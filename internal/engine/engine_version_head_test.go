package engine

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/engine/activation"
	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// ---------------------------------------------------------------------------
// COG-28 / issue #763 — version-head substitution.
//
// These tests drive the WHOLE recall pipeline (Activate), not a hand-built
// result set: the mechanism's entire point is that the predecessor is dropped
// by phase 6's lifecycle cut BEFORE anything downstream can see it, and a
// harness that hands applyVersionHeadSubstitution a pre-made shadow would test
// the half of the change that was never broken.
//
// The engine here carries the noop embedder, so every candidate arrives through
// FTS — the lexical half of the same evidence path. That is deliberate and
// sufficient for the chain-shape, visibility and non-substitution gates, which
// are about WHICH declared chains resolve and WHO may see them, not about
// embedding quality. The issue's own reproduction — a rewrite that changed the
// VOCABULARY, where only a real embedder can produce the failure — lives in
// engine_version_head_localassets_test.go behind the localassets tag.
//
// Every fixture below is synthetic.
// ---------------------------------------------------------------------------

type versionHeadHarness struct {
	t   *testing.T
	eng *Engine
	ctx context.Context
	ws  [8]byte
}

func newVersionHeadHarness(t *testing.T) (*versionHeadHarness, func()) {
	t.Helper()
	eng, cleanup := testEnv(t)
	return &versionHeadHarness{
		t:   t,
		eng: eng,
		ctx: context.Background(),
		ws:  eng.store.ResolveVaultPrefix("default"),
	}, cleanup
}

func (h *versionHeadHarness) write(concept, content string) string {
	h.t.Helper()
	resp, err := h.eng.Write(h.ctx, &mbp.WriteRequest{Vault: "default", Concept: concept, Content: content})
	if err != nil {
		h.t.Fatalf("write %q: %v", concept, err)
	}
	return resp.ID
}

// evolve supersedes oldID with new content, exactly as muninn_evolve does:
// successor + RelSupersedes + soft-delete + closed ValidUntil, one batch.
func (h *versionHeadHarness) evolve(oldID, concept, content string) string {
	h.t.Helper()
	newID, err := h.eng.Evolve(h.ctx, "default", oldID, content, "test evolve", nil, concept)
	if err != nil {
		h.t.Fatalf("evolve %s: %v", oldID, err)
	}
	return newID.String()
}

// linkSupersedes writes a declared RelSupersedes edge without soft-deleting the
// predecessor — the manual Link(relation=supersedes) shape.
func (h *versionHeadHarness) linkSupersedes(newID, oldID string) {
	h.t.Helper()
	if _, err := h.eng.Link(h.ctx, &mbp.LinkRequest{
		Vault: "default", SourceID: newID, TargetID: oldID,
		RelType: uint16(storage.RelSupersedes), Weight: 1.0,
	}); err != nil {
		h.t.Fatalf("link supersedes %s->%s: %v", newID, oldID, err)
	}
}

func (h *versionHeadHarness) softDelete(id string) {
	h.t.Helper()
	u := h.mustParse(id)
	if err := h.eng.store.SoftDelete(h.ctx, h.ws, u); err != nil {
		h.t.Fatalf("soft-delete %s: %v", id, err)
	}
}

func (h *versionHeadHarness) mustParse(id string) storage.ULID {
	h.t.Helper()
	u, err := storage.ParseULID(id)
	if err != nil {
		h.t.Fatalf("parse %s: %v", id, err)
	}
	return u
}

// recall runs the full Activate pipeline after draining every async write-time
// worker (see docs/internals/testing-hermeticity.md) so the FTS postings the
// candidate pool depends on have actually landed.
func (h *versionHeadHarness) recall(query string, mutate ...func(*mbp.ActivateRequest)) *mbp.ActivateResponse {
	h.t.Helper()
	h.eng.waitWriteTimeIdle()
	req := &mbp.ActivateRequest{Vault: "default", Context: []string{query}, MaxResults: 10}
	for _, m := range mutate {
		m(req)
	}
	resp, err := h.eng.Activate(h.ctx, req)
	if err != nil {
		h.t.Fatalf("activate %q: %v", query, err)
	}
	return resp
}

func itemByID(resp *mbp.ActivateResponse, id string) *mbp.ActivationItem {
	for i := range resp.Activations {
		if resp.Activations[i].ID == id {
			return &resp.Activations[i]
		}
	}
	return nil
}

func ids(resp *mbp.ActivateResponse) []string {
	out := make([]string, 0, len(resp.Activations))
	for _, a := range resp.Activations {
		out = append(out, a.ID)
	}
	return out
}

// seedRetentionChain writes the predecessor and evolves it, keeping the OLD
// wording strongly lexical so the FTS pool reaches it, and giving the successor
// wording that shares nothing with the query. That is the shape of #763: the
// evidence lands on a record the lifecycle cut discards.
func seedRetentionChain(h *versionHeadHarness) (predecessor, successor string) {
	predecessor = h.write("retention policy",
		"Telemetry rows older than ninety days are purged nightly by the vacuum sweeper.")
	successor = h.evolve(predecessor, "retention policy",
		"Aged instrumentation records are compacted into cold shards after one quarter.")
	return predecessor, successor
}

const retentionQuery = "telemetry rows purged nightly by the vacuum sweeper"

// seedRetentionChainAt is seedRetentionChain with an explicit valid-time
// boundary, so an as_of query has a real instant BETWEEN the two versions to
// ask about. (A same-instant fixture cannot express "before the evolve": the
// predecessor's own ValidFrom is already now.)
func seedRetentionChainAt(h *versionHeadHarness, born, evolved time.Time) (predecessor, successor string) {
	h.t.Helper()
	vf, ca := born, born
	resp, err := h.eng.Write(h.ctx, &mbp.WriteRequest{
		Vault: "default", Concept: "retention policy",
		Content:   "Telemetry rows older than ninety days are purged nightly by the vacuum sweeper.",
		ValidFrom: &vf, CreatedAt: &ca,
	})
	if err != nil {
		h.t.Fatalf("write: %v", err)
	}
	newID, err := h.eng.EvolveAt(h.ctx, "default", resp.ID,
		"Aged instrumentation records are compacted into cold shards after one quarter.",
		"test evolve", nil, "retention policy", nil, nil, evolved)
	if err != nil {
		h.t.Fatalf("evolve: %v", err)
	}
	return resp.ID, newID.String()
}

// --- GATE 1 (lexical arm): the substitution happens at all -----------------
//
// Query wording matches the PREDECESSOR only. The predecessor must be absent
// (it is soft-deleted; substitution never resurrects it) and the successor
// present, annotated with the evidence that admitted it.
func TestVersionHead_PredecessorWordingReturnsHead(t *testing.T) {
	h, cleanup := newVersionHeadHarness(t)
	defer cleanup()

	predecessor, successor := seedRetentionChain(h)
	resp := h.recall(retentionQuery)

	if itemByID(resp, predecessor) != nil {
		t.Fatalf("the superseded predecessor was RETURNED (%v) — substitution must redirect the evidence, never resurrect the stale row", ids(resp))
	}
	head := itemByID(resp, successor)
	if head == nil {
		t.Fatalf("#763 NOT FIXED: query %q matched only the predecessor's wording and the current version was not returned. got=%v abstained=%v/%q",
			retentionQuery, ids(resp), resp.Abstained, resp.AbstainedReason)
	}
	if head.SubstitutedFor != predecessor {
		t.Errorf("substituted_for = %q, want the predecessor %q — a substituted row MUST say whose match admitted it", head.SubstitutedFor, predecessor)
	}
	if head.SubstitutionBasis == nil {
		t.Fatalf("substitution_basis is nil on a substituted row — the score components shown are the predecessor's measurements and must be attributed")
	}
	// The basis is the quantity the caller's threshold was compared against;
	// the engine's ACT-R default is 0.1 (COG-6 coerce).
	if got := float64(head.SubstitutionBasis.AbsoluteScore); got < 0.1 {
		t.Errorf("substitution_basis.absolute_score = %.4f, below the effective threshold 0.10 — a substitution may only be admitted by evidence that cleared the bar", got)
	}
	if head.ChainTruncated {
		t.Errorf("chain_truncated set on a two-member chain")
	}
}

// --- GATE 2: historical queries never substitute --------------------------
//
// Belt AND braces, verified as such: shadowsEnabled refuses these requests
// outright, and independently the lifecycle cut ADMITS a superseded predecessor
// under as_of / include_invalid, so it never becomes a refusal for the shadow
// path to catch. Deleting the explicit guard does not change this test's
// outcome — that is the finding, not a gap, and it is recorded rather than
// dressed up as a RED.
func TestVersionHead_HistoricalQueriesDoNotSubstitute(t *testing.T) {
	h, cleanup := newVersionHeadHarness(t)
	defer cleanup()

	// Backdated only far enough to give as_of a real instant BETWEEN the two
	// versions: the ACT-R prior decays with age, so a heavily aged fixture
	// would drop the predecessor under the relevance bar and the test would
	// pass for the wrong reason.
	born := time.Now().Add(-40 * time.Minute)
	evolved := time.Now().Add(-20 * time.Minute)
	predecessor, successor := seedRetentionChainAt(h, born, evolved)

	// as_of BEFORE the evolve: the predecessor IS the right answer, and the
	// successor did not exist in that view. Substituting it would answer a
	// different question than the one asked.
	asOf := time.Now().Add(-30 * time.Minute)
	resp := h.recall(retentionQuery, func(r *mbp.ActivateRequest) { r.AsOf = &asOf })
	if itemByID(resp, predecessor) == nil {
		t.Errorf("as_of before the evolve did not return the predecessor: got %v", ids(resp))
	}
	if it := itemByID(resp, successor); it != nil {
		t.Errorf("as_of before the evolve returned the SUCCESSOR (substituted_for=%q) — under as_of the successor is legitimately hidden", it.SubstitutedFor)
	}

	// include_invalid: history mode. The predecessor comes back annotated
	// expired; the successor may come back on its own merit, but never as a
	// substitution.
	resp = h.recall(retentionQuery, func(r *mbp.ActivateRequest) { r.IncludeInvalid = true })
	if itemByID(resp, predecessor) == nil {
		t.Errorf("include_invalid did not return the expired predecessor: got %v", ids(resp))
	}
	for _, a := range resp.Activations {
		if a.SubstitutedFor != "" {
			t.Errorf("include_invalid produced a substitution (%s substituted_for %s) — historical queries collect no shadows", a.ID, a.SubstitutedFor)
		}
	}
}

// --- GATE 3: chain shapes -------------------------------------------------

func TestVersionHead_MultiHopResolvesToDeepestHead(t *testing.T) {
	h, cleanup := newVersionHeadHarness(t)
	defer cleanup()

	a := h.write("retention policy", "Telemetry rows older than ninety days are purged nightly by the vacuum sweeper.")
	b := h.evolve(a, "retention policy", "Aged instrumentation records are compacted into cold shards after one quarter.")
	c := h.evolve(b, "retention policy", "Stale measurement archives migrate to glacier storage each fiscal period.")

	resp := h.recall(retentionQuery)
	head := itemByID(resp, c)
	if head == nil {
		t.Fatalf("A->B->C: the chain head C was not returned: got %v", ids(resp))
	}
	if itemByID(resp, b) != nil {
		t.Errorf("A->B->C: the intermediate B was returned — only the deepest view-valid node is the head")
	}
	if head.SubstitutedFor != a {
		t.Errorf("substituted_for = %q, want A (%q) — the annotation names the EVIDENCE SOURCE, not the intermediate", head.SubstitutedFor, a)
	}
}

func TestVersionHead_ForkRefusesAndAbstainsAmbiguous(t *testing.T) {
	h, cleanup := newVersionHeadHarness(t)
	defer cleanup()

	a := h.write("retention policy", "Telemetry rows older than ninety days are purged nightly by the vacuum sweeper.")
	// Two independent successors of A: a genuine fork. Written as bare engrams
	// plus declared edges so both stay active (evolve would serialize them).
	b := h.write("retention policy alpha", "Aged instrumentation records compact into cold shards after one quarter.")
	c := h.write("retention policy beta", "Stale measurement archives migrate to glacier storage each fiscal period.")
	h.linkSupersedes(b, a)
	h.linkSupersedes(c, a)
	h.softDelete(a) // evolve's other half: the predecessor is hidden from the present

	resp := h.recall(retentionQuery)
	for _, it := range resp.Activations {
		if it.SubstitutedFor != "" {
			t.Fatalf("a FORKED chain produced a substitution (%s substituted_for %s) — recall must refuse to choose a branch", it.ID, it.SubstitutedFor)
		}
	}
	if len(resp.Activations) == 0 && resp.AbstainedReason != activation.AbstainAmbiguousVersion {
		t.Errorf("abstained_reason = %q, want %q — an empty response over a forked chain must say WHY",
			resp.AbstainedReason, activation.AbstainAmbiguousVersion)
	}
}

func TestVersionHead_RetractedSuccessorAbstainsSupersededOnly(t *testing.T) {
	h, cleanup := newVersionHeadHarness(t)
	defer cleanup()

	predecessor, successor := seedRetentionChain(h)
	// The successor is forgotten after the evolve: the chain has no current
	// member. We must NOT resurrect the predecessor — "there is no current
	// version of this" is a real fact about the world.
	if _, err := h.eng.Forget(h.ctx, &mbp.ForgetRequest{Vault: "default", ID: successor}); err != nil {
		t.Fatalf("forget successor: %v", err)
	}

	resp := h.recall(retentionQuery)
	if itemByID(resp, predecessor) != nil {
		t.Errorf("the predecessor was resurrected after its successor was retracted: %v", ids(resp))
	}
	if itemByID(resp, successor) != nil {
		t.Errorf("the retracted successor was returned: %v", ids(resp))
	}
	if len(resp.Activations) == 0 && resp.AbstainedReason != activation.AbstainSupersededOnly {
		t.Errorf("abstained_reason = %q, want %q", resp.AbstainedReason, activation.AbstainSupersededOnly)
	}
}

// evolveChain evolves cur n times through distinct wordings, none of which
// shares vocabulary with retentionQuery.
func evolveChain(h *versionHeadHarness, cur string, n int) string {
	h.t.Helper()
	wordings := []string{
		"Aged instrumentation records compact into cold shards.",
		"Stale measurement archives migrate to glacier storage.",
		"Historic signal captures roll into frozen partitions.",
		"Dormant probe outputs consolidate into sealed volumes.",
		"Expired sensor traces fold into archival buckets.",
		"Legacy metric dumps collapse into dormant tiers.",
		"Obsolete diagnostic streams settle into deep vaults.",
		"Retired observation logs anchor into permanent stores.",
		"Superannuated readings rest in the final repository.",
	}
	if n > len(wordings) {
		h.t.Fatalf("evolveChain: only %d wordings available", len(wordings))
	}
	for i := 0; i < n; i++ {
		cur = h.evolve(cur, "retention policy", wordings[i])
	}
	return cur
}

func substitutedItem(resp *mbp.ActivateResponse) *mbp.ActivationItem {
	for i := range resp.Activations {
		if resp.Activations[i].SubstitutedFor != "" {
			return &resp.Activations[i]
		}
	}
	return nil
}

// A chain whose head sits at exactly the depth cap still substitutes — a
// cap-abstain would regress every long legacy chain — but must SAY the answer
// may not be the terminus, because on this path the injected head is the only
// row the caller sees.
func TestVersionHead_CapDepthChainSubstitutesAndReportsTruncation(t *testing.T) {
	h, cleanup := newVersionHeadHarness(t)
	defer cleanup()

	a := h.write("retention policy", "Telemetry rows older than ninety days are purged nightly by the vacuum sweeper.")
	head := evolveChain(h, a, supersessionMaxDepth)

	resp := h.recall(retentionQuery)
	sub := substitutedItem(resp)
	if sub == nil {
		t.Fatalf("a %d-hop chain produced no substitution at all: got %v", supersessionMaxDepth, ids(resp))
	}
	if sub.ID != head {
		t.Errorf("substituted row = %s, want the chain head %s", sub.ID, head)
	}
	if !sub.ChainTruncated {
		t.Errorf("chain_truncated is false on a chain reaching the depth cap (%d) — the walk had no iteration left to prove this node terminal, and over-warning is the safe direction", supersessionMaxDepth)
	}
}

// PAST the cap, an EVOLVE chain has no reachable current version at all: every
// intermediate an evolve leaves behind is soft-deleted and therefore unnameable
// under default recall, so the only visible member is the terminus — and it is
// out of reach. The honest answer is a LOUD abstention, not an invisible
// intermediate presented as current and not silence.
func TestVersionHead_OverlongEvolveChainAbstainsLoudly(t *testing.T) {
	h, cleanup := newVersionHeadHarness(t)
	defer cleanup()

	a := h.write("retention policy", "Telemetry rows older than ninety days are purged nightly by the vacuum sweeper.")
	evolveChain(h, a, supersessionMaxDepth+1)

	resp := h.recall(retentionQuery)
	if sub := substitutedItem(resp); sub != nil {
		t.Fatalf("a chain past the depth cap substituted %s — no node within the cap is nameable on an evolve chain", sub.ID)
	}
	if len(resp.Activations) == 0 && resp.AbstainedReason != activation.AbstainSupersededOnly {
		t.Errorf("abstained_reason = %q, want %q — an unreachable chain head must be named, not fallen silent about",
			resp.AbstainedReason, activation.AbstainSupersededOnly)
	}
}

// #794: the SAME overlong-evolve-chain abstention as
// TestVersionHead_OverlongEvolveChainAbstainsLoudly computes `truncated`
// inside resolveSupersessionHead's depth-cap loop, but the abstain return at
// the bottom of that function dropped it on the floor — so the refusal was
// reported identically whether the chain genuinely had no current version or
// merely ran the depth cap out before it could tell. Pin that the WARN fires
// on the refusal path too (previously it fired only on walk.OK), matching the
// already-tested OK-but-truncated substitution path
// (TestVersionHead_CapDepthChainSubstitutesAndReportsTruncation).
func TestVersionHead_OverlongEvolveChainAbstainWarnsOnTruncation(t *testing.T) {
	h, cleanup := newVersionHeadHarness(t)
	defer cleanup()

	buf := captureWarn(t)

	a := h.write("retention policy", "Telemetry rows older than ninety days are purged nightly by the vacuum sweeper.")
	evolveChain(h, a, supersessionMaxDepth+1)

	resp := h.recall(retentionQuery)
	if sub := substitutedItem(resp); sub != nil {
		t.Fatalf("a chain past the depth cap substituted %s — no node within the cap is nameable on an evolve chain", sub.ID)
	}

	if !strings.Contains(buf.String(), "chain depth cap") {
		t.Errorf("expected a chain-depth-cap WARN on the abstain path, got log: %q", buf.String())
	}
}

// --- GATE 4: visibility (COG-22 parity) -----------------------------------

func TestVersionHead_HeadUnderForeignLeaseIsNotInjected(t *testing.T) {
	h, cleanup := newVersionHeadHarness(t)
	defer cleanup()

	predecessor, successor := seedRetentionChain(h)
	if _, err := h.eng.Claim(h.ctx, "default", successor, "someone-else", 300); err != nil {
		t.Fatalf("claim successor: %v", err)
	}

	resp := h.recall(retentionQuery)
	for _, it := range resp.Activations {
		if it.ID == successor || it.SubstitutedFor != "" {
			t.Fatalf("a head hidden by a live foreign lease was injected (%s substituted_for %s) — #548: the ID is the existence", it.ID, it.SubstitutedFor)
		}
	}
	if itemByID(resp, predecessor) != nil {
		t.Errorf("the predecessor leaked into the results: %v", ids(resp))
	}

	// CONTROL: the same query, with the caller owning the lease, DOES
	// substitute. Without this the assertion above would also pass if the
	// mechanism were simply broken.
	resp = h.recall(retentionQuery, func(r *mbp.ActivateRequest) { r.CallerOwner = "someone-else" })
	if it := itemByID(resp, successor); it == nil || it.SubstitutedFor != predecessor {
		t.Errorf("control: the lease OWNER did not get the substitution (got %v) — the lease guard above proves nothing if the mechanism never fires here", ids(resp))
	}
}

func TestVersionHead_PredecessorUnderForeignLeaseIsNotEvenAShadow(t *testing.T) {
	h, cleanup := newVersionHeadHarness(t)
	defer cleanup()

	predecessor, successor := seedRetentionChain(h)
	// Claim the (soft-deleted) predecessor for someone else. A candidate the
	// caller may not see does not get to speak through a proxy.
	//
	// TWO independent barriers enforce this, and both are deliberate: phase 6
	// refuses the lease-hidden engram before it can become a shadow, and the
	// injection site independently refuses to NAME it (NameableAsLineage
	// relaxes only the lifecycle predicate — the lease is untouched). Flipping
	// phase 6's predicate order alone therefore does not make this test fail;
	// removing NameableAsLineage's lease check would. Recorded rather than
	// dressed up as a single-point RED.
	if _, err := h.eng.Claim(h.ctx, "default", predecessor, "someone-else", 300); err != nil {
		t.Fatalf("claim predecessor: %v", err)
	}

	resp := h.recall(retentionQuery)
	if it := itemByID(resp, successor); it != nil && it.SubstitutedFor != "" {
		t.Fatalf("a predecessor hidden by a live foreign lease still admitted a substitution (%s) — phase 6 must refuse it as a shadow", it.SubstitutedFor)
	}
}

func TestVersionHead_ExcludedTagPredecessorIsNotAShadow(t *testing.T) {
	h, cleanup := newVersionHeadHarness(t)
	defer cleanup()

	resp0, err := h.eng.Write(h.ctx, &mbp.WriteRequest{
		Vault: "default", Concept: "retention policy",
		Content: "Telemetry rows older than ninety days are purged nightly by the vacuum sweeper.",
		Tags:    []string{"scratch"},
	})
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	successor := h.evolve(resp0.ID, "retention policy", "Aged instrumentation records compact into cold shards after one quarter.")

	// ExcludeTags rides the ActivateRequest via plasticity; drive it directly.
	h.eng.waitWriteTimeIdle()
	out, _, _ := h.activateWith(&activation.ActivateRequest{
		VaultPrefix: h.ws, Context: []string{retentionQuery}, MaxResults: 10, Threshold: 0.1,
		ExcludeTags: []string{"scratch"},
	})
	for _, s := range out {
		if (s.SubstitutedFor != storage.ULID{}) {
			t.Fatalf("a predecessor carrying a standing exclude-tag admitted a substitution to %s — the tag exclusion is not relaxed for shadows", successor)
		}
	}
}

// activateWith runs activation + the substitution phase directly, for the knobs
// that ride ActivateRequest rather than mbp.ActivateRequest.
func (h *versionHeadHarness) activateWith(req *activation.ActivateRequest) ([]activation.ScoredEngram, int, []substitutionBlock) {
	h.t.Helper()
	if req.Weights == nil {
		req.Weights = &activation.Weights{SemanticSimilarity: 0.6, FullTextRelevance: 0.4, UseACTR: true}
	}
	res, err := h.eng.activation.Run(h.ctx, req)
	if err != nil {
		h.t.Fatalf("activation run: %v", err)
	}
	now := time.Now()
	gate := newVisibilityGate(req, now)
	return h.eng.applyVersionHeadSubstitution(h.ctx, h.ws, res.Activations, res.ShadowMatches, req, gate, map[storage.ULID]bool{})
}

// --- Non-substitution paths -----------------------------------------------

// Explain drives activation at threshold -1 ("gate nothing"). At that bar every
// superseded predecessor in the pool "clears" it, so substitution would fire on
// every chain and Explain would report a would_return recall never produces.
//
// Asserted on the SHADOW SET, not on an injection count: at threshold -1 the
// successor is usually already retrieved on its own merit, so "injected == 0"
// would be produced equally by a working guard and by no guard at all. The
// shadow set is what the guard actually controls. (RED-verified: replacing the
// guard with `return true` makes this fail.)
//
// The tag-pool bypass is pinned one layer down, in
// activation.TestCollectShadowMatches_TagPoolBypassIsNotApplied: reaching it
// through the full pipeline is not possible today because tag seeding never
// surfaces a soft-deleted engram, so an end-to-end assertion here would be
// vacuous — it would pass with the guard deleted.
func TestVersionHead_ExplainThresholdBypassDisablesSubstitution(t *testing.T) {
	h, cleanup := newVersionHeadHarness(t)
	defer cleanup()

	seedRetentionChain(h)
	h.eng.waitWriteTimeIdle()
	res, err := h.eng.activation.Run(h.ctx, &activation.ActivateRequest{
		VaultPrefix: h.ws, Context: []string{retentionQuery}, MaxResults: 10, Threshold: -1,
		Weights: &activation.Weights{SemanticSimilarity: 0.6, FullTextRelevance: 0.4, UseACTR: true},
	})
	if err != nil {
		t.Fatalf("activation run: %v", err)
	}
	if len(res.ShadowMatches) != 0 {
		t.Fatalf("Explain's threshold=-1 diagnostic bypass collected %d shadow(s) — at that bar EVERY superseded "+
			"predecessor clears, so every chain would substitute and explain would report a would_return recall never produces",
			len(res.ShadowMatches))
	}
}

// RRF finals are rank-based with a ~0.001 coerced default, so "cleared the bar"
// carries almost no information. Skipped for the same calibration reason
// COG-18's R1 amendment skips the entity boost under RRF.
//
// TWO independent barriers produce this and the test deliberately does not
// distinguish them: shadowsEnabled refuses the mode, AND the RRF scoring block
// has no shadow pass for a candidate to reach. Deleting either alone leaves the
// property intact, so this is stated as an observable rather than RED-checked
// against one implementation detail.
func TestVersionHead_RRFFusionCollectsNoShadows(t *testing.T) {
	h, cleanup := newVersionHeadHarness(t)
	defer cleanup()

	seedRetentionChain(h)
	h.eng.waitWriteTimeIdle()
	res, err := h.eng.activation.Run(h.ctx, &activation.ActivateRequest{
		VaultPrefix: h.ws, Context: []string{retentionQuery}, MaxResults: 10, Threshold: 0.001,
		Weights: &activation.Weights{SemanticSimilarity: 0.6, FullTextRelevance: 0.4, UseRRFFusion: true, DisableACTR: true},
	})
	if err != nil {
		t.Fatalf("activation run: %v", err)
	}
	if len(res.ShadowMatches) != 0 {
		t.Fatalf("RRF fusion produced %d shadow match(es) — the RRF path is skipped deliberately (design §4.4)", len(res.ShadowMatches))
	}
}

// A plain forget leaves ValidUntil OPEN: that is trash, not history, and must
// never speak for a chain head.
func TestVersionHead_PlainForgetIsNotAShadow(t *testing.T) {
	h, cleanup := newVersionHeadHarness(t)
	defer cleanup()

	// Soft-deleted with NO ValidUntil stamp — a plain muninn_forget. No
	// supersedes edge is written, because Link(relation=supersedes) CLOSES the
	// target's validity window and would manufacture the very signature this
	// test asserts is absent.
	a := h.write("retention policy", "Telemetry rows older than ninety days are purged nightly by the vacuum sweeper.")
	h.softDelete(a)

	h.eng.waitWriteTimeIdle()
	res, err := h.eng.activation.Run(h.ctx, &activation.ActivateRequest{
		VaultPrefix: h.ws, Context: []string{retentionQuery}, MaxResults: 10, Threshold: 0.1,
		Weights: &activation.Weights{SemanticSimilarity: 0.6, FullTextRelevance: 0.4, UseACTR: true},
	})
	if err != nil {
		t.Fatalf("activation run: %v", err)
	}
	for _, sh := range res.ShadowMatches {
		if sh.Engram.ID == h.mustParse(a) {
			t.Fatalf("a plainly-forgotten engram (open ValidUntil) became a shadow — that is trash, not history")
		}
	}
}

// --- Normalization leakage (risk 2) ---------------------------------------
//
// If a shadow reached maxRaw / sigma / denom, EVERY score in EVERY query with a
// superseded candidate in the pool would shift. This is the detector, and it is
// an exact assertion rather than an eyeball.
//
// The two arms hold the CORPUS FIXED and vary only whether the superseded
// predecessor participates as a shadow — arm A excludes it outright with a
// standing exclude-tag (it is neither live nor shadow), arm B lets it become a
// shadow. Comparing against a run over a smaller corpus would prove nothing:
// FTS coverage is IDF-weighted against the whole index (COG-24/#711), so adding
// any engram legitimately moves every ftsScore, and that movement would mask or
// counterfeit a leak.
func TestVersionHead_ShadowsDoNotEnterPerQueryNormalization(t *testing.T) {
	h, cleanup := newVersionHeadHarness(t)
	defer cleanup()

	// Neighbours that are returned on their own merit — the rows a leak would
	// visibly rescale.
	if _, err := h.eng.Write(h.ctx, &mbp.WriteRequest{Vault: "default", Concept: "vacuum sweeper schedule",
		Content: "The vacuum sweeper runs nightly against telemetry rows in every shard."}); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := h.eng.Write(h.ctx, &mbp.WriteRequest{Vault: "default", Concept: "purge audit trail",
		Content: "Every nightly purge of telemetry rows writes an audit trail entry."}); err != nil {
		t.Fatalf("write: %v", err)
	}
	// The superseded predecessor, tagged so one arm can exclude it.
	pred, err := h.eng.Write(h.ctx, &mbp.WriteRequest{Vault: "default", Concept: "retention policy",
		Content: "Telemetry rows older than ninety days are purged nightly by the vacuum sweeper.",
		Tags:    []string{"legacy"}})
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := h.eng.Evolve(h.ctx, "default", pred.ID,
		"Aged instrumentation records are compacted into cold shards after one quarter.",
		"test evolve", nil, "retention policy"); err != nil {
		t.Fatalf("evolve: %v", err)
	}
	h.eng.waitWriteTimeIdle()

	run := func(excludeTags []string) *activation.ActivateResult {
		t.Helper()
		res, err := h.eng.activation.Run(h.ctx, &activation.ActivateRequest{
			VaultPrefix: h.ws, Context: []string{retentionQuery}, MaxResults: 10, Threshold: 0.1,
			ExcludeTags: excludeTags,
			Weights:     &activation.Weights{SemanticSimilarity: 0.6, FullTextRelevance: 0.4, UseACTR: true},
		})
		if err != nil {
			t.Fatalf("activation run: %v", err)
		}
		return res
	}

	without := run([]string{"legacy"}) // predecessor excluded entirely
	with := run(nil)                   // predecessor participates as a shadow

	if len(with.ShadowMatches) == 0 {
		t.Fatalf("no shadow was collected — this test proves nothing without one")
	}
	if len(without.ShadowMatches) != 0 {
		t.Fatalf("a standing exclude-tag did not keep the predecessor out of the shadow set")
	}
	if len(without.Activations) == 0 {
		t.Fatalf("baseline arm returned nothing — the fixture cannot detect a shift")
	}

	baseline := map[storage.ULID]activation.ScoreComponents{}
	for _, s := range without.Activations {
		baseline[s.Engram.ID] = s.Components
	}
	compared := 0
	for _, s := range with.Activations {
		b, ok := baseline[s.Engram.ID]
		if !ok {
			continue
		}
		compared++
		if s.Components.Raw != b.Raw || s.Components.Final != b.Final || s.Components.AbsoluteScore != b.AbsoluteScore {
			t.Errorf("SHADOW LEAKED INTO NORMALIZATION for %s: raw %.9f->%.9f final %.9f->%.9f absolute %.9f->%.9f. "+
				"A superseded predecessor must not rescale the live result set (design §4.2, risk 2).",
				s.Engram.ID, b.Raw, s.Components.Raw, b.Final, s.Components.Final, b.AbsoluteScore, s.Components.AbsoluteScore)
		}
	}
	if compared == 0 {
		t.Fatalf("no live row was common to both arms — nothing was actually compared")
	}
}

// --- Head already present (§6.6) ------------------------------------------
//
// The boundary this pins: a head whose OWN match is at least as strong as the
// predecessor's shadow is left completely untouched — own score, no
// annotation. (The other side of the boundary — own match WEAKER than the
// shadow, score raised — must carry attribution, and is pinned by
// TestVersionHead_HeadInPoolScoreRaiseIsAttributed.)
func TestVersionHead_HeadAlreadyInResultsIsNotAnnotated(t *testing.T) {
	h, cleanup := newVersionHeadHarness(t)
	defer cleanup()

	// The SUCCESSOR carries the query's near-verbatim wording; the predecessor
	// is a weaker subset. The successor therefore outscores the shadow, the
	// raise never fires, and nothing about the row is substituted.
	predecessor := h.write("retention policy", "Telemetry rows are purged nightly.")
	successor := h.evolve(predecessor, "retention policy", "Telemetry rows are purged nightly by the vacuum sweeper, quarterly cadence.")

	resp := h.recall(retentionQuery)
	head := itemByID(resp, successor)
	if head == nil {
		t.Fatalf("the successor was not returned at all: %v", ids(resp))
	}
	if head.SubstitutedFor != "" {
		t.Errorf("substituted_for = %q on a row whose OWN match was the strongest evidence — nothing was substituted and no score was raised", head.SubstitutedFor)
	}
	if head.Score > head.ScoreComponents.Final+1e-6 {
		t.Errorf("score %.6f exceeds the row's own final %.6f — the raise fired, so this fixture no longer tests the no-raise side of the boundary", head.Score, head.ScoreComponents.Final)
	}
}

// --- GATE 8a: the embed-lag one-liner --------------------------------------
//
// EvolveAt was the one write path that never woke the retroactive embed
// processor, so on an idle vault a freshly evolved memory could wait out the
// processor's 3-minute idle backoff before becoming semantically retrievable.
func TestEvolve_NotifiesEmbedProcessor(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	var calls int
	done := make(chan struct{}, 8)
	eng.SetOnWrite(func() {
		calls++
		select {
		case done <- struct{}{}:
		default:
		}
	})

	resp, err := eng.Write(ctx, &mbp.WriteRequest{Vault: "default", Concept: "c", Content: "original content"})
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	<-done
	afterWrite := calls

	if _, err := eng.Evolve(ctx, "default", resp.ID, "revised content", "because", nil, ""); err != nil {
		t.Fatalf("evolve: %v", err)
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
	}
	if calls <= afterWrite {
		t.Fatalf("EvolveAt did not invoke the onWrite hook (%d calls before, %d after) — the retroactive embed processor is never woken, "+
			"so on an idle vault the successor waits out a 3-minute backoff before it is semantically retrievable (#763, design §5.6)", afterWrite, calls)
	}
}

// --- Abstention recompute (refute finding 1) --------------------------------
//
// Phase 6 renders its abstention verdict over ITS OWN set — and in the #763
// shape that set is empty (the only admission-worthy evidence was the refused
// predecessor), so it says abstained=true. The substitution phase then fills
// the response with the injected head. The wire contract (mbp/types.go:
// "Empty iff Abstained is false") binds the FINAL response, so Abstained must
// be recomputed after every phase that can add or remove rows — NOT cleared
// inside applyVersionHeadSubstitution, because the COG-19 gate and the
// MaxResults re-truncation run after it and can empty the set again.
func TestVersionHead_SubstitutionClearsAbstention(t *testing.T) {
	h, cleanup := newVersionHeadHarness(t)
	defer cleanup()

	seedRetentionChain(h)
	resp := h.recall(retentionQuery)

	if len(resp.Activations) == 0 {
		t.Fatalf("fixture failure: substitution did not fire at all (abstained=%v/%q) — this test needs a filled response",
			resp.Abstained, resp.AbstainedReason)
	}
	if resp.Abstained {
		t.Errorf("CONTRACT BREAK: abstained=true alongside %d returned result(s) — \"Empty iff Abstained is false\" "+
			"(mbp/types.go) holds on REST/gRPC/MBP/SDK, and a caller honoring the flag discards a real answer",
			len(resp.Activations))
	}
	if resp.AbstainedReason != "" {
		t.Errorf("abstained_reason = %q on a non-empty response, want empty", resp.AbstainedReason)
	}
}

// --- Head-in-pool score raise is attributed (refute finding 3) --------------
//
// When the head is ALREADY in the result pool on its own (weak) merit and the
// predecessor's evidence is stronger, the raise branch lifts Score to the
// predecessor's Final — a number that is NOT the row's own measurement. COG-28
// says attribution is never optional on a row whose displayed score is not its
// own aboutness, so the raise must carry substituted_for/substitution_basis
// exactly as an injection does. Without it the caller sees score=0.9 beside
// components.final=0.2, unexplained.
func TestVersionHead_HeadInPoolScoreRaiseIsAttributed(t *testing.T) {
	h, cleanup := newVersionHeadHarness(t)
	defer cleanup()

	// The successor must SELF-RETRIEVE (clear the threshold on its own wording)
	// but score BELOW the predecessor's shadow: it shares a few query terms,
	// while the predecessor is the query's near-verbatim source.
	predecessor := h.write("retention policy",
		"Telemetry rows older than ninety days are purged nightly by the vacuum sweeper.")
	successor := h.evolve(predecessor, "retention policy",
		"Telemetry rows are purged nightly by the compaction sweeper into cold shard archives.")

	resp := h.recall(retentionQuery)
	head := itemByID(resp, successor)
	if head == nil {
		t.Fatalf("the head was not returned at all: %v", ids(resp))
	}
	// Fixture validity: the raise branch must actually have fired — the
	// displayed score exceeds the row's own measured Final. If this trips,
	// reshape the wordings; the assertions below prove nothing without it.
	if !(head.Score > head.ScoreComponents.Final+1e-6) {
		t.Fatalf("fixture failure: score %.6f did not rise above the row's own final %.6f — the raise branch was not reached",
			head.Score, head.ScoreComponents.Final)
	}
	if head.SubstitutedFor != predecessor {
		t.Errorf("substituted_for = %q, want %q — a raised score is the PREDECESSOR's measurement and must be attributed",
			head.SubstitutedFor, predecessor)
	}
	if head.SubstitutionBasis == nil {
		t.Errorf("substitution_basis is nil on a raised row — the basis names the predecessor evidence that produced the displayed score")
	}
}

// --- Cycle pinning (refute finding 6) ---------------------------------------
//
// A declared A<->B supersession loop must neither substitute nor hang, and an
// empty response over it abstains superseded_only. DECISION, recorded here so
// the fold is deliberate rather than accidental: a cycle does NOT get its own
// wire reason. What the caller can act on is identical to the retracted/
// unreachable cases — "your evidence is in a declared chain with no reachable
// current head; read the predecessor" — and a third value would expand the
// wire contract for a pathological authoring error the WARN log already names
// precisely. If cycles ever need programmatic discrimination, that is the
// moment to add the value (and the mbp/types.go doc line with it).
func TestVersionHead_SupersessionCycleAbstainsSupersededOnly(t *testing.T) {
	h, cleanup := newVersionHeadHarness(t)
	defer cleanup()

	a := h.write("retention policy", "Telemetry rows older than ninety days are purged nightly by the vacuum sweeper.")
	b := h.write("retention policy v2", "Aged instrumentation records compact into cold shards after one quarter.")
	h.linkSupersedes(b, a) // closes A's validity: A carries the shadow signature
	h.linkSupersedes(a, b) // and the loop back closes B's — a genuine declared cycle

	resp := h.recall(retentionQuery)
	for _, it := range resp.Activations {
		if it.SubstitutedFor != "" {
			t.Fatalf("a CYCLIC chain produced a substitution (%s substituted_for %s) — there is no head to resolve to", it.ID, it.SubstitutedFor)
		}
	}
	if len(resp.Activations) == 0 {
		if !resp.Abstained || resp.AbstainedReason != activation.AbstainSupersededOnly {
			t.Errorf("abstained=%v reason=%q, want true/%q — a cycle folds into superseded_only (see the decision above)",
				resp.Abstained, resp.AbstainedReason, activation.AbstainSupersededOnly)
		}
	}
}

// --- Normalization leakage, saturating arm (refute finding 4) ---------------
//
// The non-saturating detector above cannot catch a real maxRaw leak: the ACT-R
// rescale is a NO-OP unless maxRaw > 1.0, and every row in that fixture sits
// below 1.0, so a shadow contributing to maxRaw changes nothing observable.
// This arm makes the shadow HOT — a strong Hebbian boost (edge to a recently
// activated neighbour) pushes its unscaled Raw past 1.0 and past every live
// row — so a leak into maxRaw would rescale every live score and redden the
// exact same equality assertion. RED-verified by the faithful perturbation
// (folding shadow Raw into the pass-1 maxRaw): this test fails under it, the
// non-saturating one does not.
func TestVersionHead_ShadowsDoNotEnterPerQueryNormalization_Saturating(t *testing.T) {
	h, cleanup := newVersionHeadHarness(t)
	defer cleanup()

	// Live neighbours a leak would visibly rescale.
	n1, err := h.eng.Write(h.ctx, &mbp.WriteRequest{Vault: "default", Concept: "vacuum sweeper schedule",
		Content: "The vacuum sweeper runs nightly against telemetry rows in every shard."})
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := h.eng.Write(h.ctx, &mbp.WriteRequest{Vault: "default", Concept: "purge audit trail",
		Content: "Every nightly purge of telemetry rows writes an audit trail entry."}); err != nil {
		t.Fatalf("write: %v", err)
	}
	// The predecessor, tagged so one arm can exclude it, with a full-weight
	// association to n1 — the Hebbian source of its heat.
	pred, err := h.eng.Write(h.ctx, &mbp.WriteRequest{Vault: "default", Concept: "retention policy",
		Content: "Telemetry rows older than ninety days are purged nightly by the vacuum sweeper.",
		Tags:    []string{"legacy"}})
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := h.eng.Link(h.ctx, &mbp.LinkRequest{
		Vault: "default", SourceID: pred.ID, TargetID: n1.ID,
		RelType: uint16(storage.RelRelatesTo), Weight: 1.0,
	}); err != nil {
		t.Fatalf("link: %v", err)
	}
	if _, err := h.eng.Evolve(h.ctx, "default", pred.ID,
		"Aged instrumentation records are compacted into cold shards after one quarter.",
		"test evolve", nil, "retention policy"); err != nil {
		t.Fatalf("evolve: %v", err)
	}
	// Prime the activation log AFTER the evolve, with the neighbours only: the
	// predecessor is already a shadow here and shadows are never logged, so no
	// LIVE row acquires a Hebbian boost (their assoc target — the predecessor —
	// is absent from the recency window) and both arms stay deterministic.
	// Heat n1 by RECORDING its activation directly. Two recall-based priming
	// shapes failed on CI while passing locally (real-embedder cosine drifts
	// across ONNX platforms; then the measured FTS-only arms themselves
	// returned zero live rows on linux/amd64), because a recall-shaped prime
	// couples this fixture to every platform-sensitive stage of the pipeline
	// when all it needs is one entry in the recency structure phase 4 reads.
	// ActivationLog.Record is the exact structure the async drainer writes,
	// keyed by the SAME VaultID the measured runs below pass — deterministic
	// on every platform, no drain to race.
	n1ULID, err := storage.ParseULID(n1.ID)
	if err != nil {
		t.Fatalf("parse n1: %v", err)
	}
	h.eng.activation.AssocLog().Record(activation.LogEntry{
		VaultID: wsVaultID(h.ws), At: time.Now(),
		EngramIDs: []storage.ULID{n1ULID}, Scores: []float64{1.0},
	})
	h.eng.waitWriteTimeIdle()

	// FTS-only weights: under the noop embedder ContentMatch is capped at the
	// FTS weight, and at the default 0.4 even the full 3.24x Hebbian prior
	// cannot push Raw past 1.0 (0.4 x 3.24 x coverage < 1.3 only at coverage
	// ~1.0, and typical coverage lands lower). At weight 1.0 the near-verbatim
	// predecessor saturates decisively. Same weights in both arms, so the
	// baseline comparison is untouched.
	run := func(excludeTags []string) *activation.ActivateResult {
		t.Helper()
		res, err := h.eng.activation.Run(h.ctx, &activation.ActivateRequest{
			VaultID:     wsVaultID(h.ws),
			VaultPrefix: h.ws, Context: []string{retentionQuery}, MaxResults: 10, Threshold: 0.1,
			ExcludeTags: excludeTags,
			Weights:     &activation.Weights{SemanticSimilarity: 0.0, FullTextRelevance: 1.0, UseACTR: true},
		})
		if err != nil {
			t.Fatalf("activation run: %v", err)
		}
		return res
	}

	without := run([]string{"legacy"})
	with := run(nil)

	if len(with.ShadowMatches) == 0 {
		t.Fatalf("no shadow was collected — this test proves nothing without one")
	}
	// Saturation proof, pinned on the OBSERVABLE: with no leak the live pool
	// stays below 1.0, so scale is 1.0 and the shadow's reported Raw is
	// min(unscaledRaw, 1.0) — it reads exactly 1.0 iff the shadow actually
	// saturated. The Hebbian boost is the mechanism (softplus(1.489 + 4x1.0)
	// / 1.693 ≈ 3.24x on near-verbatim content); both are asserted so a decay
	// of either drains this test's power loudly instead of silently.
	sh := with.ShadowMatches[0]
	if sh.Components.HebbianBoost < 0.5 {
		t.Fatalf("fixture failure: shadow HebbianBoost = %.4f, want >= 0.5 — the shadow is not hot, its Raw cannot exceed 1.0, and the rescale stays a no-op", sh.Components.HebbianBoost)
	}
	if sh.Components.Raw < 1.0 {
		t.Fatalf("fixture failure: shadow Raw = %.4f, want 1.0 (the clamp of a saturated unscaled raw) — with an unsaturated shadow the rescale is a no-op and a maxRaw leak is undetectable", sh.Components.Raw)
	}
	if len(without.Activations) == 0 {
		t.Fatalf("baseline arm returned nothing — the fixture cannot detect a shift")
	}

	baseline := map[storage.ULID]activation.ScoreComponents{}
	for _, s := range without.Activations {
		baseline[s.Engram.ID] = s.Components
	}
	compared := 0
	for _, s := range with.Activations {
		b, ok := baseline[s.Engram.ID]
		if !ok {
			continue
		}
		compared++
		if s.Components.Raw != b.Raw || s.Components.Final != b.Final || s.Components.AbsoluteScore != b.AbsoluteScore {
			t.Errorf("SHADOW LEAKED INTO NORMALIZATION for %s: raw %.9f->%.9f final %.9f->%.9f absolute %.9f->%.9f. "+
				"A HOT superseded predecessor rescaled the live result set (design §4.2, risk 2 — saturating arm).",
				s.Engram.ID, b.Raw, s.Components.Raw, b.Final, s.Components.Final, b.AbsoluteScore, s.Components.AbsoluteScore)
		}
	}
	if compared == 0 {
		t.Fatalf("no live row was common to both arms — nothing was actually compared")
	}
}

// --- GATE 7: topically-adjacent memories produce no substitutions ----------
//
// This is the false-positive case COG-28 is most exposed to (design risk 1):
// muninn_evolve is sometimes used to REPLACE a memory with something
// substantially different rather than to revise it, and the head is then
// substituted for a query about the old subject, at the old subject's score.
// Declared-only confines the blast radius to author assertions and the
// annotation makes it inspectable, but the exposure is real and must be
// measured rather than argued.
//
// The corpus is the one behind TestCurrencyPrecision_AdjacentTopics_NoBroadHints
// (engine_currency_precision_test.go): two synthetic memories about genuinely
// different things that share an entity, a vocabulary register and a subject
// prefix. One is evolved; queries phrased for the OTHER must produce no
// substitution at all.
func TestVersionHead_AdjacentTopicsProduceNoSubstitution(t *testing.T) {
	h, cleanup := newVersionHeadHarness(t)
	defer cleanup()

	authID := h.write("quillstone auth tokens",
		"Quillstone issues scoped API tokens for the standard plan and rotates them every ninety days.")
	billingID := h.write("quillstone billing plan",
		"Quillstone bills the standard plan monthly and prorates upgrades every thirty days.")
	// Evolve the billing memory. Its predecessor's wording stays lexically
	// adjacent to the auth memory (shared subject, shared "standard plan",
	// shared cadence phrasing), which is exactly the confusable shape.
	billingHead := h.evolve(billingID, "quillstone billing plan",
		"Quillstone charges each workspace a flat quarterly retainer settled at renewal.")

	// Queries about the ADJACENT topic. None of them is about the billing
	// chain, so none of them may pull its head in.
	adjacent := []string{
		"how long do quillstone api tokens last before rotation",
		"scoped api tokens issued for the quillstone standard plan",
		"quillstone token rotation every ninety days",
	}
	for _, q := range adjacent {
		resp := h.recall(q)
		if sub := substitutedItem(resp); sub != nil {
			t.Errorf("ADJACENT-TOPIC FALSE SUBSTITUTION on %q: injected %s (substituted_for %s, score %.4f). "+
				"The lever if this ever fires is raising the shadow's admission bar — NEVER a similarity check "+
				"between predecessor and successor, which would smuggle inference into the declared channel.",
				q, sub.ID, sub.SubstitutedFor, sub.Score)
		}
	}

	// CONTROL: a query phrased for the billing predecessor DOES substitute.
	// Without it, the three assertions above would also pass on a build where
	// substitution never fires at all.
	resp := h.recall("does quillstone prorate upgrades monthly on the standard plan")
	sub := substitutedItem(resp)
	if sub == nil || sub.ID != billingHead {
		t.Fatalf("control: a query in the billing predecessor's own wording did not substitute to the head %s (got %v) — "+
			"the adjacent-topic assertions above prove nothing on a build where nothing substitutes", billingHead, ids(resp))
	}
	if itemByID(resp, authID) != nil {
		t.Logf("note: the adjacent auth memory was co-retrieved on the control query (expected; it shares vocabulary)")
	}
}
