package engine

import (
	"context"
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
func buildSimilarExistingRequestDefault(vaultName string, eng *storage.Engram) *mbp.ActivateRequest {
	return &mbp.ActivateRequest{
		Vault:      vaultName,
		Context:    []string{eng.Concept + " " + eng.Content},
		MaxResults: 5,
		ReadOnly:   true,
	}
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
	if e.activation == nil {
		return similarExistingAdvisory{}
	}

	start := time.Now()
	defer func() {
		if e.latencyTracker != nil {
			e.latencyTracker.Record(wsPrefix, "similar_existing", time.Since(start))
		}
	}()

	resp, err := e.Activate(ctx, buildSimilarExistingRequestFn(vaultName, eng))
	if err != nil {
		// Degrade loudly-but-gracefully (principle #2): a failed self-query
		// never fails the write. The block is simply absent; the failure is
		// logged, not swallowed.
		slog.Warn("engine: similar_existing self-query failed; advisory omitted", "id", newID.String(), "err", err)
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
	if resp == nil || len(resp.Activations) == 0 {
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
