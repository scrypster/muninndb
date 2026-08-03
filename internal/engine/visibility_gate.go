package engine

import (
	"context"
	"time"

	"github.com/scrypster/muninndb/internal/engine/activation"
	"github.com/scrypster/muninndb/internal/storage"
)

// visibilityGate renders the caller's result-eligibility decision for
// post-pipeline injections: the six visibility predicates enumerated on
// Admits. Phase 6 applies the visibility contract to every scored candidate;
// anything appended to the result set afterwards must satisfy the same
// contract. The drift record shows what happens when each injector
// re-implements fragments of it instead — #654 (meta filters), the #570 review
// rounds (trust, then lease and valid-time), then the structured filter,
// missed by this gate's own first draft and caught in review: the contract
// lives partly in Filters and partly in request fields a predicate-over-
// filters cannot see, so any injector plumbed field-by-field is wrong by
// default the day a new field is added — and so is a gate that enumerates by
// memory. A new visibility field on ActivateRequest is part of THIS gate's
// contract at birth: add the predicate here, or the field is a bypass for
// every injector at once.
//
// The gate holds the resolved request and delegates each predicate to the same
// functions phase 6 calls; injectors submit candidates instead of carrying
// their own projection of the request, so a visibility field added to the
// contract is enforced here at birth rather than injector-by-injector.
//
// The gate covers visibility only. Whether an injector's evidence is strong
// enough to enter at all (entity boost's threshold comparison) and at what
// rank (supersession's corrective substitution) remain the injector's own
// semantics.
type visibilityGate struct {
	req *activation.ActivateRequest
	now time.Time
}

// getLeaseForInjection reads the lease sidecar consulted by the visibility
// gate's fail-closed guard. It defaults to the store's real GetLease; tests
// covering the fail-closed behavior on a lease-read error reassign it for the
// duration of a single test (save/restore) since the real store only errors
// on a genuine read/decode failure, never on a missing lease. Production code
// must never reassign this var.
var getLeaseForInjection = func(ctx context.Context, s *storage.PebbleStore, wsPrefix [8]byte, id storage.ULID) (storage.Lease, error) {
	return s.GetLease(ctx, wsPrefix, id)
}

func newVisibilityGate(req *activation.ActivateRequest, now time.Time) *visibilityGate {
	return &visibilityGate{req: req, now: now}
}

// Admits reports whether the caller's request permits eng to enter its result
// set. It enforces the six visibility predicates the pipeline applies —
// lifecycle state, meta filters, the ExcludeUntrusted trust filter, foreign
// live lease, valid-time, and the structured (MQL WHERE) filter — delegating
// to the same predicate functions phase 6 uses so the two paths cannot drift
// apart. The predicates are a pure conjunction, so ordering is a cost choice,
// not a semantic one: phase 6 interleaves them across its retrieval and
// scoring cuts, while the gate runs the cheap record-local checks first and
// the lease read (a store access) as late as possible.
//
// A lease read error fails CLOSED (the candidate is refused): phase 6 fails
// the whole request on the same fault, and silently admitting a
// possibly-checked-out engram is worse than dropping an optional injection.
// A missing lease record is not an error on either path.
func (g *visibilityGate) Admits(ctx context.Context, store *storage.PebbleStore, ws [8]byte, eng *storage.Engram) bool {
	return g.Nameable(ctx, store, ws, eng) && g.ValidForView(eng)
}

// Nameable holds the EXISTENCE half of the contract: lifecycle state, meta
// filters, trust, the structured filter, and lease visibility. An engram that
// fails Nameable must not surface anywhere in the response — not as a result
// row and not inside another row's annotations, because for these predicates
// the ID itself is the secret (#548: a lease-hidden work item's existence is
// exactly what the checkout hides).
//
// Validity is deliberately NOT part of Nameable: supersession stamps
// ValidUntil on every superseded engram (Link/evolve), so chain intermediates
// are validity-expired BY CONSTRUCTION, and lineage annotations exist
// precisely to name them ("this expired fact was replaced by X"). Expiry
// bounds what may be CURRENT for the view (ValidForView), not what may be
// referred to.
func (g *visibilityGate) Nameable(ctx context.Context, store *storage.PebbleStore, ws [8]byte, eng *storage.Engram) bool {
	return g.nameable(ctx, store, ws, eng, false)
}

// NameableAsLineage is Nameable with EXACTLY ONE predicate relaxed: the
// lifecycle cut is evaluated as if include_invalid were set, so a SOFT-DELETED
// record carrying a CLOSED ValidUntil — the declared-supersession signature —
// may be NAMED as lineage. StateArchived and open-stamp trash are still
// refused; the other five predicates (meta filters, trust, structured filter,
// lease, and — via Admits — validity) are untouched.
//
// This is COG-22's amendment, and it exists for exactly one caller: COG-28's
// `substituted_for` annotation, which names the predecessor whose evidence
// admitted an injected head. Three facts make that correct:
//
//  1. The predecessor's ID is ALREADY publicly derivable from the head.
//     EvolveAt writes provenance Details.PredecessorID on the successor and
//     muninn_read surfaces it, so naming it in a recall annotation leaks
//     nothing a follow-up read of the returned head does not already give.
//  2. Refusing to name it makes the row SILENTLY substituted — the failure
//     class this project ranks worst, and the whole point of the annotation.
//  3. The other predicates are not relaxed, so a predecessor under a live
//     foreign lease, untrusted under ExcludeUntrusted, or excluded by a
//     meta/structured filter is still unnameable (and is not even a shadow —
//     phase 6 refuses it upstream).
//
// It is a SEPARATE METHOD on purpose. Loosening Nameable itself would make the
// bad state representable for every other injector at once (principle #3).
func (g *visibilityGate) NameableAsLineage(ctx context.Context, store *storage.PebbleStore, ws [8]byte, eng *storage.Engram) bool {
	return g.nameable(ctx, store, ws, eng, true)
}

func (g *visibilityGate) nameable(ctx context.Context, store *storage.PebbleStore, ws [8]byte, eng *storage.Engram, asLineage bool) bool {
	if eng == nil {
		return false
	}
	// Lifecycle state, evaluated under the caller's temporal view — the same
	// predicate phase 6 applies (activation.PassesLifecycle), so the two paths
	// cannot drift. Default recall still refuses every soft-deleted engram; an
	// explicit as_of / include_invalid query still reaches a SUPERSEDED one
	// (soft-delete + closed ValidUntil), because supersession demotes a fact
	// rather than erasing it.
	includeInvalid := g.req.IncludeInvalid
	if asLineage {
		// The lineage relaxation, and its whole extent: PassesLifecycle's
		// historical branch still demands a CLOSED ValidUntil, so this admits
		// a superseded record and nothing else — never StateArchived, never a
		// plain forget's open-stamp trash.
		includeInvalid = true
	}
	if !activation.PassesLifecycle(eng, g.req.AsOf, includeInvalid) {
		return false
	}
	if !activation.PassesMetaFilter(eng, g.req.Filters) {
		return false
	}
	// Hard trust filter. ExcludeUntrusted rides the request bool, not Filters,
	// so PassesMetaFilter cannot enforce it.
	if g.req.ExcludeUntrusted && eng.Trust == storage.TrustUntrusted {
		return false
	}
	// Structured (MQL WHERE) predicate. Phase 6 applies it as the final
	// post-retrieval step inside Run(), which is BEFORE every injector runs —
	// an injection that skipped it would land ungoverned rows in a structured
	// query's results (the #654 class, MQL edition).
	if g.req.StructuredFilter != nil && !g.req.StructuredFilter.Match(eng) {
		return false
	}
	// Work-queue checkout (#548): hide engrams under a live foreign lease.
	if !g.req.IncludeLeased {
		l, err := getLeaseForInjection(ctx, store, ws, eng.ID)
		if err != nil {
			return false
		}
		if l.Live(g.now) && l.Owner != g.req.CallerOwner {
			return false
		}
	}
	return true
}

// ValidForView is the valid-time half of the contract (COG-19): whether eng
// may be presented as CURRENT under the caller's temporal view (AsOf /
// IncludeInvalid / now).
func (g *visibilityGate) ValidForView(eng *storage.Engram) bool {
	return activation.PassesValidity(eng, g.req.AsOf, g.req.IncludeInvalid, g.now)
}

// ViewFuture reports whether eng postdates the caller's as-of instant — it
// does not exist yet in that view, so it may not even be NAMED there (naming
// it would leak the view's future). Always false outside as-of views.
func (g *visibilityGate) ViewFuture(eng *storage.Engram) bool {
	if g.req.AsOf == nil || g.req.IncludeInvalid {
		return false
	}
	return eng.EffectiveValidFrom().After(*g.req.AsOf)
}
