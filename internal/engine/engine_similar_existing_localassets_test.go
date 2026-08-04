//go:build localassets

package engine

import (
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/engine/activation"
	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// ---------------------------------------------------------------------------
// COG-34 (#712 remainder) — RED-1..4 plus the 4.2-A live/AsOf-replay parity
// pin, against the REAL bundled bge-small-en-v1.5 embedder. A fake or noop
// embedder cannot reach the "strong" band at all here — testEnv's noop
// embedder has no registered model name, so COG-30 reports
// no_model_baseline (response-wide uncalibrated) on every self-query
// regardless of content, and "strong" is structurally unreachable. That is
// why these live in the localassets-gated file rather than the cheap one
// (RED-5, which does not need a band, stays there).
//
// Every timestamp pair below is staggered >= 2h (v2's RED-control fix: a
// shared CreatedAt silences everything via the temporal floor regardless of
// mechanism state, which proves nothing — see the design record's
// discussion of v1's vacuous controls).
//
// Every string is synthetic, invented for this test file only.
// ---------------------------------------------------------------------------

// writeEmbeddedAt is writeEmbedded with an explicit backdated CreatedAt, so
// candidates can be staggered relative to the (unwritten, in-memory) query
// engram each RED control probes with.
func (h *realEmbedHarness) writeEmbeddedAt(concept, content string, createdAt time.Time) string {
	h.t.Helper()
	vec, err := h.svc.Embed(h.ctx, []string{concept + " " + content})
	if err != nil {
		h.t.Fatalf("embed: %v", err)
	}
	resp, err := h.eng.Write(h.ctx, &mbp.WriteRequest{
		Vault: "default", Concept: concept, Content: content, Embedding: vec, CreatedAt: &createdAt,
	})
	if err != nil {
		h.t.Fatalf("write: %v", err)
	}
	return resp.ID
}

func containsSimilarID(items []mbp.SimilarExisting, id string) bool {
	for _, it := range items {
		if it.ID == id {
			return true
		}
	}
	return false
}

// TestSimilarExisting_RED1_MechanismToggle is RED-1: with the advisory hook
// disabled, a staggered 2-generation pair emits no block; enabled, it emits
// the predecessor. Calling similarExisting directly (not through Write) on
// the SAME in-memory query engram before/after the toggle isolates exactly
// the mechanism-off/on variable — nothing else about the request changes.
func TestSimilarExisting_RED1_MechanismToggle(t *testing.T) {
	h, cleanup := newRealEmbedHarness(t)
	defer cleanup()
	ws := h.ws
	ctx := h.ctx

	const concept = "harborlight distributor rate sheet"
	const content = "The harborlight distributor rate sheet lists unit price, minimum order quantity, and freight terms for tier B."
	predecessor := h.writeEmbeddedAt(concept, content, time.Now().Add(-3*time.Hour))
	h.eng.waitWriteTimeIdle()

	// The query engram is never itself written — its text is IDENTICAL to
	// the predecessor's, which is what pushes the self-query's cosine to
	// near-1.0 and the absolute score into the strong band (the same shape
	// TestRelevanceBand_StrongMatch_NoHint uses: query == stored content).
	probe := &storage.Engram{Concept: concept, Content: content, CreatedAt: time.Now()}
	probeID := storage.NewULID()

	restore := similarExistingHookEnabled
	defer func() { similarExistingHookEnabled = restore }()

	similarExistingHookEnabled = false
	off := h.eng.similarExisting(ctx, ws, "default", probeID, probe)
	if len(off.Items) != 0 || off.OmittedBasis != "" {
		t.Fatalf("mechanism-off arm: got items=%v basis=%q, want a fully empty result", off.Items, off.OmittedBasis)
	}

	similarExistingHookEnabled = true
	on := h.eng.similarExisting(ctx, ws, "default", probeID, probe)
	t.Logf("mechanism-on arm: items=%+v exclusions=%v", on.Items, on.exclusions)
	if !containsSimilarID(on.Items, predecessor) {
		t.Fatalf("mechanism-on arm: predecessor %s not in block %v (exclusions=%v)", predecessor, on.Items, on.exclusions)
	}
	if on.Items[0].RelevanceBand != activation.RelevanceStrong {
		t.Errorf("emitted row band = %q, want strong", on.Items[0].RelevanceBand)
	}
}

// TestSimilarExisting_RED2_BandIsLoadBearing is RED-2: a topically-adjacent
// rival that reaches the top-5 self-query pool but bands below strong is
// NOT emitted, and the harness asserts the RECORDED exclusion reason is
// "band" — not "temporal_floor", not "declared_edge".
func TestSimilarExisting_RED2_BandIsLoadBearing(t *testing.T) {
	h, cleanup := newRealEmbedHarness(t)
	defer cleanup()
	ws := h.ws
	ctx := h.ctx

	const concept = "greenridge backup retention policy"
	const goldContent = "Nightly greenridge backups are written to cold storage and retained for ninety days."
	candidate := h.writeEmbeddedAt(concept, goldContent, time.Now().Add(-3*time.Hour))
	h.eng.waitWriteTimeIdle()

	// A REWORDED probe — same topic, different wording, the exact shape
	// TestRelevanceBand_ModerateMatch_NoHint already measures as landing
	// below strong (real evidence, not near-verbatim).
	probe := &storage.Engram{
		Concept:   "backup schedule question",
		Content:   "how are the greenridge backups stored and for how long are they kept?",
		CreatedAt: time.Now(),
	}
	probeID := storage.NewULID()

	adv := h.eng.similarExisting(ctx, ws, "default", probeID, probe)
	t.Logf("items=%+v exclusions=%v", adv.Items, adv.exclusions)

	if containsSimilarID(adv.Items, candidate) {
		t.Fatalf("RED-2 PREMISE FAILED: the reworded probe reached strong band and the candidate was emitted — this fixture cannot show the bar excluding a sub-strong row; reword it weaker")
	}
	reason, seen := adv.exclusions[candidate]
	if !seen {
		t.Fatalf("candidate %s did not even reach the self-query's top-5 pool (exclusions=%v) — cannot assert an exclusion reason", candidate, adv.exclusions)
	}
	if reason != "band" {
		t.Errorf("excluded for reason %q, want %q (temporal floor and declared-edge must not be why this candidate was excluded)", reason, "band")
	}
}

// TestSimilarExisting_RED3_FloorIsNotTheSilencer is RED-3: several
// adjacent-topic probes emit no block against a corpus backdated >= 2h, and
// at least one candidate is proven to have PASSED the temporal floor and
// failed only on band — i.e. the silence is not an artifact of the floor
// (v1's vacuous control: a shared CreatedAt silenced everything regardless
// of mechanism state). Reduced from v1's "8 adjacent-topic writes" to 3,
// documented here rather than silently: the assertion shape (silence, with
// the recorded reason proven to be band, not floor) is identical at any N,
// and CI cost is the reason for the reduction.
func TestSimilarExisting_RED3_FloorIsNotTheSilencer(t *testing.T) {
	h, cleanup := newRealEmbedHarness(t)
	defer cleanup()
	ws := h.ws
	ctx := h.ctx

	backdated := time.Now().Add(-3 * time.Hour)
	gold := map[string]string{
		"aurora deploy pipeline": h.writeEmbeddedAt("aurora deploy pipeline",
			"The aurora deploy pipeline runs lint, then unit tests, then a canary rollout to five percent of traffic before full release.",
			backdated),
		"aurora on-call rotation": h.writeEmbeddedAt("aurora on-call rotation",
			"The aurora on-call rotation is weekly and hands over every Monday at ten in the ops channel.",
			backdated),
		"aurora database backups": h.writeEmbeddedAt("aurora database backups",
			"Nightly aurora database backups are written to cold storage and retained for ninety days.",
			backdated),
	}
	h.eng.waitWriteTimeIdle()

	probes := []*storage.Engram{
		{Concept: "deploy question", Content: "what testing happens before an aurora canary rollout ships to production traffic?", CreatedAt: time.Now()},
		{Concept: "oncall question", Content: "who is on call for aurora this week and when does the handover happen?", CreatedAt: time.Now()},
		{Concept: "backup question", Content: "which storage tier do nightly backups land on and what is the retention policy?", CreatedAt: time.Now()},
	}

	sawBandExclusion := false
	for i, probe := range probes {
		adv := h.eng.similarExisting(ctx, ws, "default", storage.NewULID(), probe)
		t.Logf("probe[%d] items=%+v exclusions=%v", i, adv.Items, adv.exclusions)
		if len(adv.Items) != 0 {
			t.Errorf("probe[%d]: expected no block on a reworded adjacent-topic probe, got %v", i, adv.Items)
		}
		for candID, reason := range adv.exclusions {
			if reason == "temporal_floor" {
				t.Errorf("probe[%d]: candidate %s excluded by temporal_floor — the >= 2h stagger did not clear it, so this fixture cannot isolate the band as the silencer", i, candID)
			}
			if reason == "band" {
				sawBandExclusion = true
			}
		}
	}
	if !sawBandExclusion {
		names := make([]string, 0, len(gold))
		for k := range gold {
			names = append(names, k)
		}
		t.Fatalf("no candidate was ever recorded as excluded by band across %d probes (candidates=%v) — RED-3 needs at least one proven temporal-floor-cleared, band-only exclusion", len(probes), names)
	}
}

// TestSimilarExisting_RED4_VisibilityGate is RED-4: a lease-held candidate
// that would otherwise band strong is not named while COG-22's visibility
// gate is intact; removing it (by flipping IncludeLeased true, the same
// real request field a caller uses to opt out of lease visibility) leaks
// it. There is no separate gate implementation in this mechanism to bypass
// directly — visibility is inherited from the shipped Activate() pipeline,
// which is exactly the property this test proves is load-bearing.
func TestSimilarExisting_RED4_VisibilityGate(t *testing.T) {
	h, cleanup := newRealEmbedHarness(t)
	defer cleanup()
	ws := h.ws
	ctx := h.ctx

	const concept = "cinderfall vendor contract terms"
	const content = "The cinderfall vendor contract terms specify net-45 payment and a two percent early-payment discount."
	leased := h.writeEmbeddedAt(concept, content, time.Now().Add(-3*time.Hour))
	h.eng.waitWriteTimeIdle()

	if _, err := h.eng.Claim(ctx, "default", leased, "someone-elses-worker", 3600); err != nil {
		t.Fatalf("Claim: %v", err)
	}

	probe := &storage.Engram{Concept: concept, Content: content, CreatedAt: time.Now()}

	hidden := h.eng.similarExisting(ctx, ws, "default", storage.NewULID(), probe)
	t.Logf("gate intact: items=%+v exclusions=%v", hidden.Items, hidden.exclusions)
	if containsSimilarID(hidden.Items, leased) {
		t.Fatalf("lease-held candidate %s leaked through the block with the visibility gate intact", leased)
	}

	restore := buildSimilarExistingRequestFn
	buildSimilarExistingRequestFn = func(vault string, eng *storage.Engram) *mbp.ActivateRequest {
		req := restore(vault, eng)
		req.IncludeLeased = true // sabotage: the documented, real bypass of the lease predicate
		return req
	}
	leaked := h.eng.similarExisting(ctx, ws, "default", storage.NewULID(), probe)
	buildSimilarExistingRequestFn = restore
	t.Logf("gate bypassed: items=%+v exclusions=%v", leaked.Items, leaked.exclusions)
	if !containsSimilarID(leaked.Items, leased) {
		t.Fatalf("RED-4 did not go red: expected the lease-held candidate to leak once IncludeLeased was forced true, got %v (exclusions=%v)", leaked.Items, leaked.exclusions)
	}
}

// TestSimilarExisting_LiveAsOfReplayParity is the 4.2-A parity pin: a live
// self-query (block captured at the moment S was actually written) and the
// design record's §5.2.1 AsOf-replay measurement method, applied to the
// SAME synthetic generation pair after the supersession is declared, must
// produce the same block membership. Per §4.2-B, S itself is structurally
// invisible in the replay (S.ValidFrom > AsOf), so this test — like the
// design's own replay — does not exercise the self-echo guard; that is
// pinned separately (RED-1's probe never being emitted against itself is
// implicit in every RED test above, since the query engram is never
// written and so never appears in its own candidate pool by construction).
func TestSimilarExisting_LiveAsOfReplayParity(t *testing.T) {
	h, cleanup := newRealEmbedHarness(t)
	defer cleanup()
	ws := h.ws
	ctx := h.ctx

	const concept = "driftwood quarterly rate sheet"
	const predecessorContent = "The driftwood quarterly rate sheet lists unit price, minimum order quantity, and freight terms for tier B."
	predecessorID := h.writeEmbeddedAt(concept, predecessorContent, time.Now().Add(-3*time.Hour))
	h.eng.waitWriteTimeIdle()

	// S: an ordinary remember (never Evolve) so the LIVE hook actually runs
	// and the write response carries a real captured block — exactly #712's
	// "rival copy" shape, wording near-identical to the predecessor's.
	successorContent := predecessorContent + " Effective the following quarter."
	vec, err := h.svc.Embed(ctx, []string{concept + " " + successorContent})
	if err != nil {
		t.Fatalf("embed: %v", err)
	}
	sResp, err := h.eng.Write(ctx, &mbp.WriteRequest{
		Vault: "default", Concept: concept, Content: successorContent, Embedding: vec,
	})
	if err != nil {
		t.Fatalf("write successor: %v", err)
	}
	successorID := sResp.ID
	liveItems := sResp.SimilarExisting
	t.Logf("live block at write time: %+v", liveItems)
	if !containsSimilarID(liveItems, predecessorID) {
		t.Fatalf("PREMISE FAILED: the live self-query at S's own write time did not name the predecessor (got %v) — cannot run the parity pin without a real live block to compare against", liveItems)
	}

	// Declare the supersession via Link (one of §5.2.1's two edge sources),
	// deliberately NOT Evolve, so S's own Write() stays the plain-remember
	// path whose live block is already captured above.
	sID, err := storage.ParseULID(successorID)
	if err != nil {
		t.Fatalf("parse successor id: %v", err)
	}
	pID, err := storage.ParseULID(predecessorID)
	if err != nil {
		t.Fatalf("parse predecessor id: %v", err)
	}
	if _, err := h.eng.Link(ctx, &mbp.LinkRequest{
		Vault: "default", SourceID: successorID, TargetID: predecessorID, RelType: uint16(storage.RelSupersedes),
	}); err != nil {
		t.Fatalf("Link(supersedes): %v", err)
	}

	// §5.2.1: AsOf = min(S.CreatedAt, P.ValidUntil) - 1s, replayed through
	// the IDENTICAL self-query request the live hook builds (no divergence
	// in any field other than AsOf, per §5.5's MEASUREMENT-INVALID rule).
	sEng, err := h.eng.store.GetEngram(ctx, ws, sID)
	if err != nil {
		t.Fatalf("GetEngram(S): %v", err)
	}
	pEng, err := h.eng.store.GetEngram(ctx, ws, pID)
	if err != nil {
		t.Fatalf("GetEngram(P): %v", err)
	}
	if pEng.ValidUntil.IsZero() {
		t.Fatalf("PREMISE FAILED: Link(supersedes) did not close the predecessor's ValidUntil")
	}
	asOfBasis := sEng.CreatedAt
	if pEng.ValidUntil.Before(asOfBasis) {
		asOfBasis = pEng.ValidUntil
	}
	asOfInstant := asOfBasis.Add(-time.Second)

	replayReq := buildSimilarExistingRequestFn("default", sEng)
	replayReq.AsOf = &asOfInstant
	replayResp, err := h.eng.Activate(ctx, replayReq)
	if err != nil {
		t.Fatalf("AsOf replay Activate: %v", err)
	}
	replayAdv := h.eng.filterSimilarExisting(replayResp, sID, sEng)
	t.Logf("AsOf replay block: %+v (exclusions=%v)", replayAdv.Items, replayAdv.exclusions)

	liveSet := map[string]bool{}
	for _, it := range liveItems {
		liveSet[it.ID] = true
	}
	replaySet := map[string]bool{}
	for _, it := range replayAdv.Items {
		replaySet[it.ID] = true
	}
	if len(liveSet) != len(replaySet) {
		t.Fatalf("block membership mismatch: live=%v replay=%v", liveItems, replayAdv.Items)
	}
	for id := range liveSet {
		if !replaySet[id] {
			t.Errorf("live named %s, replay did not: live=%v replay=%v", id, liveItems, replayAdv.Items)
		}
	}
	for id := range replaySet {
		if !liveSet[id] {
			t.Errorf("replay named %s, live did not: live=%v replay=%v", id, liveItems, replayAdv.Items)
		}
	}
}
