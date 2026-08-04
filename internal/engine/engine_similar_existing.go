package engine

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/scrypster/muninndb/internal/engine/activation"
	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// #712 remainder — COG-34.
//
// THE MECHANISM. On a successful, non-duplicate single `remember`, run the
// shipped recall pipeline (the SAME Engine.Activate every muninn_recall call
// uses — no new scoring code) against the vault, using the just-written
// engram's own text, and return the pre-existing memories recall itself
// calls a STRONG-band match as an advisory. It writes nothing, links
// nothing, decides nothing (COG-34's own text, mirroring COG-11/COG-29/
// COG-30's "pure annotation" shape).
//
// THE BAR IS COG-30'S "strong" BAND, END TO END. ZERO new constants: no
// score threshold, no cosine cutoff, no corpus-tuned number lives in this
// file. The vault's OWN resolved calibration (COG-6 default gate + resolved
// content-channel ceiling) decides "strong" exactly the way it decides every
// recall row's band — principle #11 satisfied by reuse.
//
// SCOPE (see design record .claude/deep-review/2026-08-03-712-currency-
// design-v2.md, §4.5): single muninn_remember (MCP) and MBP Write only.
// remember_batch / remember_tree are DEFERRED — a per-item self-query would
// N-times a bulk import's cost, and the guide already names bulk pipelines
// as the evolve case. Backfill, a per-vault plasticity toggle, and REST/
// gRPC/SDK surfacing beyond the automatic REST type-alias inheritance are
// also deferred (obligation #3: an unpopulated field on a partial schema is
// the silently-wrong class — gRPC/SDKs get nothing rather than a stub).
// Auto-linking is permanently refused (COG-25's impossibility result).
//
// NAMED RESIDUAL (found during the build, not in the design): this is a
// real Activate() call, so — like any recall, not new to this mechanism —
// it warms the process-local L1 read cache (internal/storage/cache.go) for
// every candidate it SCORES, not just ones it emits. Nothing PERSISTS
// (RED-5 proves zero store writes) and COG-12's AccessCount is untouched,
// but a subsequent REAL recall within the cache's lifetime may see an old,
// dormant candidate as "just accessed" purely because a write's self-query
// looked at it. Before this mechanism, Write() never triggered a recall, so
// a write-only sequence was cache-cold for its own engrams by construction;
// a test or vault whose premise depends on that (e.g.
// engine_clone_weighted_sum_test.go's aged/never-accessed control) must
// account for it. See COG-34 in docs/internals/invariants.md for the full
// writeup; not fixed here because doing so needs a cache-bypass read path
// threaded through activation's engram loader — new scoring-adjacent code
// the design deliberately avoids.

// similarExistingSelfQueryDeadline bounds the self-query's own Activate()
// call with an ABSOLUTE ceiling, applied regardless of whether the caller's
// ctx carries a deadline at all (F3, 712-currency fix round). REST/MBP
// writes commonly carry no deadline, and activation's own EmbedBudgetFraction
// mechanism (engine/activation/engine.go, embedBudgetContext) is a
// passthrough with nothing to protect when ctx has no deadline — so a
// wedged/slow embed backend previously stalled the WRITE response
// unboundedly (measured: a 3s embedder hang produced a 3.01s write).
//
// This is not a new tuned constant: it is the design record's OWN
// pre-registered SHIP/KILL bar for this mechanism's write-added latency —
// v2 §5.3's dispositioned outcome table, "p95 latency (both scales): SHIP
// ≤ 100 ms, KILL > 100 ms, for this increment as designed (default-on)" —
// applied at runtime as the self-query's own deadline rather than left as a
// measurement-time-only threshold. (The fix-round brief that named this
// requirement cited it as "v2 §6"; §6 is "Top risks" in the current
// document — the number itself lives in §5.3's table. Cited correctly here
// so a future reader does not go looking in the wrong section.)
//
// context.WithTimeout(ctx, similarExistingSelfQueryDeadline) already gives
// "min(caller-derived, bound)" for free: Go's context package resolves a
// nested WithTimeout to whichever of the parent's existing deadline and the
// new one is EARLIER, so a caller ctx with a tighter deadline than 100ms is
// left alone, and a caller ctx with no deadline (or a looser one) is capped
// here. With F7's embedding-reuse fix, the expected path does not spend
// anywhere near this budget; this is the backstop for the genuinely slow
// case (cold model, contention, a hung backend) — never the primary
// defense.
const similarExistingSelfQueryDeadline = 100 * time.Millisecond

// similarExistingBasisSelfQueryDeadlineExceeded is the OmittedBasis value
// set when the self-query's OWN bounded ctx (similarExistingSelfQueryDeadline)
// expired before Activate() could return a usable response — distinct from
// the existing relevance-band-basis values (no_model_baseline etc.), which
// mean "this vault's calibration cannot express confidence"; this one means
// "the self-query ran out of its own time budget", a runtime condition, not
// a calibration one. Named rather than silent per principle #2: an omitted
// advisory must always say why.
const similarExistingBasisSelfQueryDeadlineExceeded = "self_query_deadline_exceeded"

// similarExistingHookEnabled gates the write-time similar_existing advisory.
// Production code must NEVER reassign this var — it exists only so RED-1
// (mechanism-off) can prove the advisory is not vacuous, mirroring
// visibility_gate.go's getLeaseForInjection save/restore pattern.
var similarExistingHookEnabled = true

// similarExistingResponseWideBases are the relevance_band_basis values
// applyRelevanceBands (engine_relevance.go) applies UNIFORMLY to every row
// in a response — the "this response's numbers are uncalibrated" statement,
// as opposed to a per-row admission fact (tag_filter_bypass, not_scored).
// Only a response-wide basis means the self-query itself could not be
// banded; a per-row one just means that particular candidate wasn't
// measured, which is not a statement about the vault's calibration.
var similarExistingResponseWideBases = map[string]bool{
	activation.BasisNoModelBaseline:       true,
	activation.BasisSemanticFloorDisabled: true,
	activation.BasisSemanticDegraded:      true,
	activation.BasisRRFFusion:             true,
	activation.BasisWeightedSumFusion:     true,
	// F5 (712-currency fix round): missing from the original allowlist. A
	// vault with no content channel at all (COG-30's engine_relevance.go)
	// is exactly as uncalibrated as no_model_baseline/semantic_floor_disabled
	// — the self-query cannot express "strong" for it either.
	activation.BasisNoContentChannel: true,
}

// similarExistingAdvisory is the result of one write's COG-34 self-query.
// Exactly one of Items and OmittedBasis is meaningful at a time: a non-empty
// OmittedBasis means the self-query's own calibration could not express
// confidence (the whole block is omitted, loudly, rather than silently
// empty); otherwise Items holds whatever survived the strong-band /
// temporal-floor / declared-edge / self-echo filters (may be empty — a
// genuine "no strong rivals" is silent by design, distinct from "couldn't
// measure").
//
// exclusions is populated ONLY for tests (RED-2/RED-3): it names, per
// candidate ID that reached the top-5 self-query pool but was NOT emitted,
// which single-reason gate excluded it — "band" | "temporal_floor" |
// "declared_edge" | "self". It costs nothing on the write path since
// production never reads it, and it exists so RED-2/RED-3 can assert the
// RECORDED reason for silence rather than merely its absence.
type similarExistingAdvisory struct {
	Items        []mbp.SimilarExisting
	OmittedBasis string
	exclusions   map[string]string
}

// buildSimilarExistingRequestDefault constructs the self-query exactly as
// §4.2 step 1 registers it: Context = [concept + " " + content],
// MaxResults = 5, read_only:true (COG-11 — the self-query must have zero
// write side effects; #846 made that contract real, and this call is the
// reason it must stay real). Factored out from the call site so the AsOf
// replay parity pin (4.2-A) can construct the byte-identical request save
// for AsOf, per §5.5's MEASUREMENT-INVALID rule ("the replay request
// diverges from the live hook's request in any field other than AsOf").
//
// F7 (712-currency fix round): when eng.Embedding is already populated —
// caller-supplied on the write, or (for a later AsOf replay) computed by
// the background embed processor since — it is threaded straight into the
// request's own Embedding field, which activation's phase1 already treats
// as a precomputed query vector and skips re-embedding for (engine.go's
// phase1, `if len(req.Embedding) > 0`). Without this, EVERY successful
// remember paid a second full embedder inference for text the write path
// had just embedded (or was handed) moments earlier — measured at ~100ms
// p95, over the v2 §6 pre-registered ≤100ms bar (F7 in the fix round's
// adjudication). This is not a new field or a new code path: it is the
// query-embedding seam Activate already has, applied to the one caller who
// happens to hold the answer already. When eng.Embedding is empty (the
// common case: a fresh write with no client-supplied vector, whose
// embedding is still pending the async retroactive processor), there is
// nothing to reuse and the self-query embeds fresh exactly as before —
// F3's registered latency ceiling is the backstop for that path, not this
// one. Data availability, not code path, differs between live and replay —
// the same "scoring state is present-day, not rewound" residual the design
// record already names for existing candidates (§5.2.1), extended to the
// self-query's own vector.
func buildSimilarExistingRequestDefault(vaultName string, eng *storage.Engram) *mbp.ActivateRequest {
	req := &mbp.ActivateRequest{
		Vault:      vaultName,
		Context:    []string{eng.Concept + " " + eng.Content},
		MaxResults: 5,
		ReadOnly:   true,
		// filterSimilarExisting reads only resp.Activations — it never reads
		// BriefSentences. Left at the default ("auto"), activateCore's
		// embedding-based brief path (engine.go, `generateEmbeddingBrief`)
		// triggers whenever req.Embedding is non-empty and issues its OWN
		// per-sentence embedder calls — which the F7 fix above would have
		// newly turned on for every self-query with a reusable vector,
		// re-introducing the exact duplicate-embed cost F7 removes, just
		// relocated and multiplied by candidate count. "extractive" forces
		// the heuristic (embedder-free) brief computation regardless of
		// Embedding, for work nothing ever reads.
		BriefMode: "extractive",
	}
	if len(eng.Embedding) > 0 {
		req.Embedding = eng.Embedding
	}
	return req
}

// buildSimilarExistingRequestFn is a package var, not a plain function call,
// purely so tests can prove the request it builds is load-bearing rather
// than decorative: RED-4 overrides it to flip IncludeLeased true (the
// documented, real bypass of COG-22's lease predicate — the same field a
// caller uses to opt out of it) and observes a lease-held rival leak; RED-5
// overrides it to flip ReadOnly false and observes a store write that the
// production default does not produce. Production code must NEVER reassign
// this var (mirrors visibility_gate.go's getLeaseForInjection pattern).
var buildSimilarExistingRequestFn = buildSimilarExistingRequestDefault

// similarExisting computes the COG-34 advisory for the engram just written.
// wsPrefix/vaultName/newID/eng all describe the write that just committed;
// activate is the pipeline entry point to call (Engine.Activate in
// production; a test may substitute a wrapper that also snapshots the
// request for the 4.2-A parity pin).
func (e *Engine) similarExisting(ctx context.Context, wsPrefix [8]byte, vaultName string, newID storage.ULID, eng *storage.Engram) similarExistingAdvisory {
	if !similarExistingHookEnabled {
		return similarExistingAdvisory{}
	}
	// Engines built without an ActivationEngine (some minimal test/adapter
	// harnesses — e.g. gRPC's upsert-threading test — never call Activate/
	// Recall at all today) would otherwise nil-panic inside e.Activate. This
	// mirrors the existing e.activation != nil guard elsewhere in this file
	// (e.g. waitWriteTimeIdle). Degrade loudly-but-gracefully: no self-query
	// is possible, so the advisory is simply absent.
	//
	// F6 (712-currency fix round): a PRODUCTION engine (as opposed to a
	// minimal test/adapter harness) with no ActivationEngine wired is
	// misconfigured — it silently loses the documented similar_existing
	// feature on every write with no signal anywhere. Loud per claim
	// discipline: warn, don't just return.
	if e.activation == nil {
		slog.Warn("engine: similar_existing skipped — no ActivationEngine configured; the advisory is documented but unavailable on this engine", "id", newID.String())
		return similarExistingAdvisory{}
	}

	start := time.Now()
	defer func() {
		if e.latencyTracker != nil {
			e.latencyTracker.Record(wsPrefix, "similar_existing", time.Since(start))
		}
	}()

	// F3: bound the self-query with an absolute ceiling regardless of the
	// caller's own ctx (see similarExistingSelfQueryDeadline's doc for the
	// registered source and why plain WithTimeout already gives
	// min(caller-derived, bound)).
	selfQueryCtx, cancel := context.WithTimeout(ctx, similarExistingSelfQueryDeadline)
	defer cancel()

	resp, err := e.Activate(selfQueryCtx, buildSimilarExistingRequestFn(vaultName, eng))
	if err != nil {
		// Degrade loudly-but-gracefully (principle #2): a failed self-query
		// never fails the write. The block is simply absent; the failure is
		// logged, not swallowed.
		slog.Warn("engine: similar_existing self-query failed; advisory omitted", "id", newID.String(), "err", err)
		if errors.Is(err, context.DeadlineExceeded) {
			return similarExistingAdvisory{OmittedBasis: similarExistingBasisSelfQueryDeadlineExceeded}
		}
		return similarExistingAdvisory{}
	}
	return e.filterSimilarExisting(resp, newID, eng)
}

// filterSimilarExisting applies §4.2 steps 2-4 to a self-query response:
// the response-wide calibration gate, self-echo exclusion, the strong-band
// bar, the 1h temporal floor, and the no-declared-edge filter. Split out
// from similarExisting so the 4.2-A parity pin can apply the identical
// filtering to a replay response without re-running the timing wrapper.
func (e *Engine) filterSimilarExisting(resp *mbp.ActivateResponse, newID storage.ULID, eng *storage.Engram) similarExistingAdvisory {
	out := similarExistingAdvisory{exclusions: make(map[string]string)}
	if resp == nil {
		return out
	}
	// F5 (712-currency fix round): a ZERO-ROW response is NOT automatically
	// "genuinely nothing similar" — on an uncalibrated vault (or one whose
	// self-query embed degraded, F3), the response can legitimately come
	// back with no Activations at all, and indexing resp.Activations[0]
	// below would silently skip the calibration-gate check entirely,
	// presenting a loud "couldn't measure" condition as a quiet, confident
	// "nothing similar" absence — exactly the #582/#585/#589 doctrine
	// violation COG-34's own text forbids. resp.SemanticDegraded is the one
	// response-wide signal that survives zero rows on the wire today
	// (engine.go's activateCore sets it unconditionally, before any row is
	// built) — read it FIRST, before ever touching resp.Activations[0].
	// Residual, honestly named: the no_model_baseline / semantic_floor_disabled
	// / rrf_fusion / weighted_sum_fusion / no_content_channel bases have no
	// equivalent zero-row-survivable field on the wire response today (only
	// the per-row RelevanceBandBasis carries them, computed by
	// applyRelevanceBands, which itself never runs when len(rows)==0) — so
	// THOSE causes, if they coincide with a genuinely empty candidate set,
	// still fall through to a silent absence here. Threading them through
	// would mean a new wire field on ActivateResponse with its own
	// three-surface sync obligation (COG-30) — out of scope for this fix.
	if len(resp.Activations) == 0 {
		if resp.SemanticDegraded {
			out.OmittedBasis = activation.BasisSemanticDegraded
		}
		return out
	}

	// Response-wide calibration gate (§4.2 step 3, band-basis clause): a row
	// whose relevance_band_basis is non-empty is NOT a strong match —
	// "uncalibrated" means "not measured", never "measured and low". Because
	// applyRelevanceBands applies a response-wide basis UNIFORMLY to every
	// row (engine_relevance.go), the top row's band/basis speaks for the
	// whole response.
	if top := resp.Activations[0]; top.RelevanceBand == activation.RelevanceUncalibrated && similarExistingResponseWideBases[top.RelevanceBandBasis] {
		out.OmittedBasis = top.RelevanceBandBasis
		return out
	}

	declaredTargets := make(map[string]bool, len(eng.Associations))
	for _, a := range eng.Associations {
		if a.RelType == storage.RelSupersedes || a.RelType == storage.RelContradicts {
			declaredTargets[a.TargetID.String()] = true
		}
	}

	newIDStr := newID.String()
	floorCutoff := eng.CreatedAt.Add(-currencyTemporalFloor)

	for _, row := range resp.Activations {
		if row.ID == newIDStr {
			out.exclusions[row.ID] = "self"
			continue
		}
		if declaredTargets[row.ID] {
			out.exclusions[row.ID] = "declared_edge"
			continue
		}
		rowCreated := time.Unix(0, row.CreatedAt)
		if !rowCreated.Before(floorCutoff) {
			out.exclusions[row.ID] = "temporal_floor"
			continue
		}
		if row.RelevanceBand != activation.RelevanceStrong {
			out.exclusions[row.ID] = "band"
			continue
		}
		out.Items = append(out.Items, mbp.SimilarExisting{
			ID:            row.ID,
			Concept:       row.Concept,
			RelevanceBand: row.RelevanceBand,
			AgeDays:       eng.CreatedAt.Sub(rowCreated).Hours() / 24,
		})
	}
	return out
}

// applyToWriteResponse mirrors the advisory onto the wire response. Kept as
// a method on the advisory (rather than inline at the Write() call site) so
// the 4.2-A parity pin can compare two advisories' Items independent of how
// either got projected onto a response.
func (a similarExistingAdvisory) applyToWriteResponse(resp *mbp.WriteResponse) {
	if len(a.Items) > 0 {
		resp.SimilarExisting = a.Items
	} else if a.OmittedBasis != "" {
		resp.SimilarExistingBasis = a.OmittedBasis
	}
}
