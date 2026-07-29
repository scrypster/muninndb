package engine

import (
	"context"
	"time"

	"github.com/scrypster/muninndb/internal/engine/activation"
	"github.com/scrypster/muninndb/internal/storage"
)

// visibilityGate renders the caller's complete result-eligibility decision for
// post-pipeline injections. Phase 6 applies the visibility contract to every
// scored candidate; anything appended to the result set afterwards must satisfy
// the same contract. The drift record shows what happens when each injector
// re-implements fragments of it instead — #654 (meta filters), the #570 review
// rounds (trust, then lease and valid-time): the contract lives partly in
// Filters and partly in request fields a predicate-over-filters cannot see, so
// any injector plumbed field-by-field is wrong by default the day a new field
// is added.
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
// set. It mirrors phase 6's exclusion conditions in phase-6 order — state,
// meta filters, trust, foreign live lease, valid-time — delegating to the
// same predicates phase 6 uses so the two paths cannot drift apart.
//
// A lease read error fails CLOSED (the candidate is refused): phase 6 fails
// the whole request on the same fault, and silently admitting a
// possibly-checked-out engram is worse than dropping an optional injection.
// A missing lease record is not an error on either path.
func (g *visibilityGate) Admits(ctx context.Context, store *storage.PebbleStore, ws [8]byte, eng *storage.Engram) bool {
	if eng == nil {
		return false
	}
	if eng.State == storage.StateSoftDeleted || eng.State == storage.StateArchived {
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
	// Valid-time gate (COG-19).
	return activation.PassesValidity(eng, g.req.AsOf, g.req.IncludeInvalid, g.now)
}
