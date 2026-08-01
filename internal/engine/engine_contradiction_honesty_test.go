package engine

import (
	"context"
	"fmt"
	"sort"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/engine/activation"
	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// contradictionFixture writes two rival facts about one topic and declares
// b contradicts a — the §2.1 fixture from the #764 design, and the shape both
// round-7 evaluators reported ("the request timeout limit is 180ms" vs
// "…320ms", declared, then recalled).
func contradictionFixture(t *testing.T, eng *Engine, vault string) (a, b string) {
	t.Helper()
	ctx := context.Background()
	wa, err := eng.Write(ctx, &mbp.WriteRequest{Vault: vault, Concept: "request timeout limit",
		Content: "the request timeout limit is 180ms"})
	if err != nil {
		t.Fatal(err)
	}
	wb, err := eng.Write(ctx, &mbp.WriteRequest{Vault: vault, Concept: "request timeout limit revised",
		Content: "the request timeout limit is 320ms"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := eng.Link(ctx, &mbp.LinkRequest{Vault: vault, SourceID: wb.ID, TargetID: wa.ID,
		RelType: uint16(storage.RelContradicts)}); err != nil {
		t.Fatal(err)
	}
	eng.waitWriteTimeIdle()
	return wa.ID, wb.ID
}

func recallContradiction(t *testing.T, eng *Engine, vault string, mut func(*mbp.ActivateRequest)) *mbp.ActivateResponse {
	t.Helper()
	req := &mbp.ActivateRequest{Vault: vault,
		Context: []string{"what is the request timeout limit"}, MaxResults: 5, Threshold: 0.001}
	if mut != nil {
		mut(req)
	}
	resp, err := eng.Activate(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func contradictionRank(resp *mbp.ActivateResponse, id string) int {
	for i := range resp.Activations {
		if resp.Activations[i].ID == id {
			return i
		}
	}
	return -1
}

// TestContradictionHonesty_UnresolvedDeclaredEdgeIsHonored is R3 for #764 D2.
//
// RED at bb10f30, where recall returned:
//
//	rank 0: "request timeout limit"          score=0.0935 (the OLDER side)
//	rank 1: "request timeout limit revised"  score=0.0935
//
// — both sides, tied, older first, ZERO conflict signal on either row and none
// on the response. With a real embedder that is the evaluators' "one side at
// score 1.0 with only a side-channel annotation".
func TestContradictionHonesty_UnresolvedDeclaredEdgeIsHonored(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	a, b := contradictionFixture(t, eng, "test")

	resp := recallContradiction(t, eng, "test", nil)
	ra, rb := contradictionRank(resp, a), contradictionRank(resp, b)
	if ra < 0 || rb < 0 {
		t.Fatalf("both sides of the conflict must be returned; got ranks a=%d b=%d over %d rows", ra, rb, len(resp.Activations))
	}

	// (i) both rows carry the annotation.
	for _, it := range resp.Activations {
		if it.ID != a && it.ID != b {
			continue
		}
		c := it.UnresolvedContradiction
		if c == nil {
			t.Fatalf("row %s (%q) carries no unresolved_contradiction", it.ID, it.Concept)
		}
		if (it.ID == a && c.With != b) || (it.ID == b && c.With != a) {
			t.Errorf("row %s names partner %s, want the other side of the pair", it.ID, c.With)
		}
		if !c.PartnerInResults {
			t.Errorf("row %s: partner_in_results=false, but both sides are in the response", it.ID)
		}
		if c.WithConcept == "" {
			t.Errorf("row %s: with_concept is empty; the partner is in the response and its concept is known", it.ID)
		}
	}
	if resp.Activations[ra].UnresolvedContradiction.Side != contradictionSideChallenged {
		t.Errorf("a is the TARGET of the declared edge, so its side must be %q", contradictionSideChallenged)
	}
	if resp.Activations[rb].UnresolvedContradiction.Side != contradictionSideAsserted {
		t.Errorf("b is the SOURCE of the declared edge, so its side must be %q", contradictionSideAsserted)
	}

	// (ii) neither side is presented at the score it earned. NOT asserted here
	// against an absolute constant: this test used to check
	// `score <= 0.95`, and recall's wire Score is an absolute activation that
	// is structurally an order of magnitude below that, so the assertion could
	// never fail and proved nothing. The real proof needs a control corpus and
	// lives in TestContradictionHonesty_DemoteOnly, which builds the identical
	// vault with and without the contradicts link and compares the scores.

	// (iii) the newer side ranks first, and (iv) the two are adjacent.
	if rb > ra {
		t.Errorf("older side ranked first (a=%d, b=%d); the newer EffectiveValidFrom must lead", ra, rb)
	}
	if ra-rb != 1 && rb-ra != 1 {
		t.Errorf("conflict rows are not adjacent: a=%d b=%d", ra, rb)
	}

	// (v) the response carries the block.
	if resp.Conflict == nil {
		t.Fatal("response carries no conflict block")
	}
	if !resp.Conflict.Unresolved || len(resp.Conflict.Pairs) != 1 {
		t.Fatalf("conflict block = %+v, want one unresolved pair", resp.Conflict)
	}
	if resp.Conflict.Warning == "" {
		t.Error("conflict block carries no warning naming the resolution actions")
	}
	p := resp.Conflict.Pairs[0]
	if p.Preferred != "a" || p.Basis != contradictionBasisValidFrom {
		t.Errorf("pair preferred=%q basis=%q, want a/%s (b is the edge SOURCE and the newer fact)",
			p.Preferred, p.Basis, contradictionBasisValidFrom)
	}
	// Abstention is untouched: the response is non-empty, so the "Empty iff
	// Abstained is false" contract must still hold.
	if resp.Abstained {
		t.Error("a non-empty response must never be marked abstained")
	}
}

// TestContradictionHonesty_DemoteOnly is R6, against a CONTROL rather than
// against itself: the identical corpus is built twice, once with the
// contradicts link and once without, and the two responses are compared.
//
// Two things must hold. The conflicted rows must score strictly LOWER with the
// conflict than without (the demote is real, not decorative) — RED without the
// phase. And the genuinely better unrelated match must never lose rank to
// them: a conflict can only ever push a row DOWN, never above a better
// unrelated result. That is the applySupersession demote-only precedent
// restated, and it is why COG-29 demotes scores and re-sorts rather than
// forcing cluster adjacency in either direction (gathering up lifts a member
// above a better unrelated row; gathering down buries a dominant answer).
func TestContradictionHonesty_DemoteOnly(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	// Same corpus in both vaults. Only "conflict" gets the contradicts link.
	seedUnrelated := func(vault string) string {
		w, err := eng.Write(ctx, &mbp.WriteRequest{Vault: vault, Concept: "request timeout limit policy",
			Content: "the request timeout limit policy governs what the request timeout limit may be set to"})
		if err != nil {
			t.Fatal(err)
		}
		return w.ID
	}
	controlStrong := seedUnrelated("control")
	ca, err := eng.Write(ctx, &mbp.WriteRequest{Vault: "control", Concept: "request timeout limit",
		Content: "the request timeout limit is 180ms"})
	if err != nil {
		t.Fatal(err)
	}
	cb, err := eng.Write(ctx, &mbp.WriteRequest{Vault: "control", Concept: "request timeout limit revised",
		Content: "the request timeout limit is 320ms"})
	if err != nil {
		t.Fatal(err)
	}
	eng.waitWriteTimeIdle()

	conflictStrong := seedUnrelated("conflict")
	a, b := contradictionFixture(t, eng, "conflict")

	opts := func(r *mbp.ActivateRequest) { r.MaxResults = 10 }
	control := recallContradiction(t, eng, "control", opts)
	got := recallContradiction(t, eng, "conflict", opts)

	if len(control.Activations) != len(got.Activations) {
		t.Fatalf("row count differs from the control: %d vs %d — the phase has no delete path and no inject path",
			len(control.Activations), len(got.Activations))
	}

	scoreOf := func(resp *mbp.ActivateResponse, id string) float64 {
		if i := contradictionRank(resp, id); i >= 0 {
			return float64(resp.Activations[i].Score)
		}
		t.Fatalf("row %s missing from response", id)
		return 0
	}

	// The demote fires on both sides.
	for _, pair := range [2][2]string{{ca.ID, a}, {cb.ID, b}} {
		want, have := scoreOf(control, pair[0]), scoreOf(got, pair[1])
		if have >= want {
			t.Errorf("conflicted row scored %.6f, control scored %.6f — the demote did not fire", have, want)
		}
	}

	// The unrelated row's rank never worsens, and nothing conflicted outranks
	// it while scoring below it.
	controlRank := contradictionRank(control, controlStrong)
	gotRank := contradictionRank(got, conflictStrong)
	if gotRank > controlRank {
		t.Errorf("unrelated row lost rank to the conflict: %d -> %d", controlRank, gotRank)
	}
	if scoreOf(got, conflictStrong) != scoreOf(control, controlStrong) {
		t.Errorf("unrelated row's score changed: %.6f -> %.6f; the phase must touch only cluster members",
			scoreOf(control, controlStrong), scoreOf(got, conflictStrong))
	}
	for i := range got.Activations {
		it := got.Activations[i]
		if it.ID != a && it.ID != b {
			continue
		}
		if gotRank >= 0 && i < gotRank && got.Activations[gotRank].Score > it.Score {
			t.Errorf("conflicted row %s (score %.6f) ranks above the better unrelated row (score %.6f)",
				it.ID, it.Score, got.Activations[gotRank].Score)
		}
	}
}

// TestContradictionHonesty_ResolvedBySupersedes is one arm of R5, at the recall
// layer: "I declared which one wins" IS a resolution. Asserted beats asserted.
func TestContradictionHonesty_ResolvedBySupersedes(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()
	a, b := contradictionFixture(t, eng, "test")

	if resp := recallContradiction(t, eng, "test", nil); resp.Conflict == nil {
		t.Fatal("precondition: the unresolved conflict must be reported before it is resolved")
	}
	// Distinct weight: same-weight links between the same pair share one
	// forward-association key and would REPLACE the contradicts edge rather
	// than coexisting with it — which would test the wrong thing.
	if _, err := eng.Link(ctx, &mbp.LinkRequest{Vault: "test", SourceID: b, TargetID: a, Weight: 0.9,
		RelType: uint16(storage.RelSupersedes)}); err != nil {
		t.Fatal(err)
	}
	eng.waitWriteTimeIdle()

	resp := recallContradiction(t, eng, "test", func(r *mbp.ActivateRequest) { r.IncludeInvalid = true })
	if resp.Conflict != nil {
		t.Errorf("conflict block still present after link(supersedes) resolved the pair: %+v", resp.Conflict)
	}
	for _, it := range resp.Activations {
		if it.UnresolvedContradiction != nil {
			t.Errorf("row %s still annotated unresolved_contradiction after the pair was resolved", it.ID)
		}
	}
}

// TestContradictionHonesty_AsOfBeforeDeclaration is R7. A conflict declared
// AFTER the caller's as-of instant is not part of the truth of that time: no
// block, no cap, no theater on a historical query.
func TestContradictionHonesty_AsOfBeforeDeclaration(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()

	before := time.Now()
	time.Sleep(10 * time.Millisecond)
	contradictionFixture(t, eng, "test")

	past := recallContradiction(t, eng, "test", func(r *mbp.ActivateRequest) {
		t := before
		r.AsOf = &t
		r.IncludeInvalid = true
	})
	if past.Conflict != nil {
		t.Errorf("as_of BEFORE the declaration reports a conflict that did not exist yet: %+v", past.Conflict)
	}
	for _, it := range past.Activations {
		if it.UnresolvedContradiction != nil {
			t.Errorf("row %s annotated under an as_of that predates the declaration", it.ID)
		}
	}

	future := time.Now().Add(time.Minute)
	now := recallContradiction(t, eng, "test", func(r *mbp.ActivateRequest) { r.AsOf = &future })
	if now.Conflict == nil {
		t.Error("as_of AFTER the declaration must still report the conflict")
	}
}

// TestContradictionHonesty_ObserveSafe is R8: the phase reads and annotates,
// and writes nothing (COG-11).
func TestContradictionHonesty_ObserveSafe(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()
	contradictionFixture(t, eng, "test")

	ws := eng.store.ResolveVaultPrefix("test")
	countKeys := func() (int, int) {
		t.Helper()
		recs, err := eng.store.GetContradictionRecords(ctx, ws)
		if err != nil {
			t.Fatal(err)
		}
		decl, err := eng.store.DeclaredContradictions(ctx, ws, 0)
		if err != nil {
			t.Fatal(err)
		}
		return len(recs), len(decl.Records)
	}
	m0, d0 := countKeys()
	for i := 0; i < 3; i++ {
		if resp := recallContradiction(t, eng, "test", nil); resp.Conflict == nil {
			t.Fatal("precondition: the conflict must be reported")
		}
	}
	m1, d1 := countKeys()
	if m0 != m1 || d0 != d1 {
		t.Errorf("COG-29 wrote to the store: markers %d->%d, declared edges %d->%d", m0, m1, d0, d1)
	}
}

// TestContradictionHonesty_NoContradictionsIsANoOp pins the Step-0 fast path:
// on a vault with no contradiction at all, the phase must be provably inert —
// this is what keeps the three measurement corpora byte-identical.
func TestContradictionHonesty_NoContradictionsIsANoOp(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	for _, c := range []struct{ concept, content string }{
		{"timeout", "the request timeout limit is 180ms"},
		{"retries", "the retry budget is three attempts"},
		{"backoff", "backoff is exponential with jitter"},
	} {
		if _, err := eng.Write(ctx, &mbp.WriteRequest{Vault: "clean", Concept: c.concept, Content: c.content}); err != nil {
			t.Fatal(err)
		}
	}
	eng.waitWriteTimeIdle()

	resp := recallContradiction(t, eng, "clean", nil)
	if resp.Conflict != nil {
		t.Errorf("clean vault reports a conflict: %+v", resp.Conflict)
	}
	for _, it := range resp.Activations {
		if it.UnresolvedContradiction != nil {
			t.Errorf("row %s annotated on a vault with no contradicts edge", it.ID)
		}
	}
	if ws := eng.store.ResolveVaultPrefix("clean"); eng.vaultMayHaveContradictions(ctx, ws) {
		t.Error("fast-path gate says a contradiction-free vault may have contradictions")
	}
}

// TestContradictionHonesty_OneSidedIsAnnotatedNotInjected is the design's
// fourth conflict-corpus case, and it is where COG-29 deliberately parts ways
// with COG-28.
//
// #763 INJECTS an absent chain head because a declared ordering exists: the
// head is the answer and the predecessor demonstrably is not. Here NEITHER
// side is known to be right, so injecting the partner would let a conflict
// LIFT a memory into a result set it did not earn — precisely what demote-only
// forbids — and would double the noise on every ambient query touching a
// disputed topic. The row is capped and annotated by reference instead.
func TestContradictionHonesty_OneSidedIsAnnotatedNotInjected(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	a, err := eng.Write(ctx, &mbp.WriteRequest{Vault: "test", Concept: "request timeout limit",
		Content: "the request timeout limit is 180ms"})
	if err != nil {
		t.Fatal(err)
	}
	// A live, visible partner that shares nothing with the query.
	far, err := eng.Write(ctx, &mbp.WriteRequest{Vault: "test", Concept: "lunar chronology",
		Content: "the first crewed lunar landing happened in 1969"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := eng.Link(ctx, &mbp.LinkRequest{Vault: "test", SourceID: far.ID, TargetID: a.ID,
		RelType: uint16(storage.RelContradicts)}); err != nil {
		t.Fatal(err)
	}
	eng.waitWriteTimeIdle()

	resp := recallContradiction(t, eng, "test", nil)
	if contradictionRank(resp, far.ID) >= 0 {
		t.Fatalf("the absent partner was INJECTED into a result set it did not earn: %+v", resp.Activations)
	}
	i := contradictionRank(resp, a.ID)
	if i < 0 {
		t.Fatalf("the matching side must still be returned; got %d rows", len(resp.Activations))
	}
	c := resp.Activations[i].UnresolvedContradiction
	if c == nil {
		t.Fatal("a one-sided conflict must still annotate the row that IS returned")
	}
	if c.PartnerInResults {
		t.Error("partner_in_results = true, but the partner is not in the response")
	}
	if c.With != far.ID || c.WithConcept == "" {
		t.Errorf("annotation must name the absent partner by ID and concept, got %+v", c)
	}
	if resp.Conflict == nil || len(resp.Conflict.Pairs) != 1 || resp.Conflict.Pairs[0].PartnerInResults {
		t.Errorf("conflict block must report the pair as one-sided, got %+v", resp.Conflict)
	}
}

// TestContradictionHonesty_TightLimitStillDisclosesTheConflict pins what
// actually happens when the caller's limit is too small to hold both sides.
//
// The design asked for "if any cluster member survives the MaxResults cut, all
// of them do". That guarantee did not survive contact with the code: phase 6
// truncates to MaxResults inside activation.Run (engine.go ~2323), BEFORE any
// post-pipeline phase runs, so at max_results=1 the partner never reaches
// COG-29 at all — and injecting it is exactly what §2.4 Step 7 defers, because
// neither side of an unresolved conflict is known to be right and a conflict
// must never LIFT content into a result set it did not earn.
//
// So the guarantee that holds, and that this test pins, is the one that
// actually removes the reported failure: a caller that consumes only the first
// result is still told, on that row, that it is disputed and by which memory —
// with the score demoted and the response-level block present. The
// keepAtLeast/adjacency_overflow machinery still defends the case where a
// post-pipeline injector grows the set past MaxResults and a cluster straddles
// the re-truncation boundary.
func TestContradictionHonesty_TightLimitStillDisclosesTheConflict(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	a, b := contradictionFixture(t, eng, "test")

	resp := recallContradiction(t, eng, "test", func(r *mbp.ActivateRequest) { r.MaxResults = 1 })
	if len(resp.Activations) != 1 {
		t.Fatalf("max_results=1 returned %d rows", len(resp.Activations))
	}
	only := resp.Activations[0]
	if only.ID != a && only.ID != b {
		t.Fatalf("unexpected row %s", only.ID)
	}
	c := only.UnresolvedContradiction
	if c == nil {
		t.Fatal("the single returned row carries no unresolved_contradiction — an agent consuming only the first result would be confidently misled, which is the reported failure")
	}
	if c.PartnerInResults {
		t.Error("partner_in_results = true, but the partner was cut by the limit")
	}
	want := b
	if only.ID == b {
		want = a
	}
	if c.With != want {
		t.Errorf("annotation names %s, want the partner %s", c.With, want)
	}
	if resp.Conflict == nil {
		t.Error("response carries no conflict block")
	}
}

// TestContradictionHonesty_SurvivesRestartBeforeTheMarkerFlush is the RED for
// the permanent-silence hole in COG-29's fast-path gate.
//
// muninn_link(contradicts) is durable the moment Link returns, but the 0x0A
// marker that the gate reads is written only when the batch worker FLUSHES.
// The window between the two was covered by an in-process sync.Map — which is
// exactly the state a restart destroys. Restart before the flush and the marker
// was never written and the flag is gone, so vaultMayHaveContradictions returns
// false on every subsequent query and nothing ever re-probes: recall silently
// stops honoring a correctly-declared, durably-stored contradiction FOREVER.
//
// Clearing the sync.Map with the marker absent is exactly that state.
func TestContradictionHonesty_SurvivesRestartBeforeTheMarkerFlush(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()
	contradictionFixture(t, eng, "test")

	ws := eng.store.ResolveVaultPrefix("test")
	// Precondition: the batch worker has NOT flushed, so the durable marker
	// this gate normally reads does not exist. If this ever fails the test is
	// no longer exercising the restart hole.
	has, err := eng.store.HasContradictionMarkers(ctx, ws)
	if err != nil {
		t.Fatal(err)
	}
	if has {
		t.Skip("the 0x0A marker already landed; this test needs the pre-flush window")
	}
	if resp := recallContradiction(t, eng, "test", nil); resp.Conflict == nil {
		t.Fatal("precondition: the conflict must be honored before the simulated restart")
	}

	// Simulate the restart: everything in-process is gone, the store is not.
	eng.contradictionsDeclared.Delete(ws)
	eng.contradictionProbeClean.Delete(ws)

	if !eng.vaultMayHaveContradictions(ctx, ws) {
		t.Error("after a restart with no marker written, the gate says the vault has no contradictions — honesty is now off for this vault permanently")
	}
	resp := recallContradiction(t, eng, "test", nil)
	if resp.Conflict == nil {
		t.Fatal("recall no longer honors a durably-declared contradiction after a restart that preceded the marker flush")
	}
	annotated := 0
	for _, it := range resp.Activations {
		if it.UnresolvedContradiction != nil {
			annotated++
		}
	}
	if annotated != 2 {
		t.Errorf("%d rows annotated after the simulated restart, want 2", annotated)
	}
}

// TestContradictionHonesty_ProbeIsPaidOncePerVault pins the cost side of the
// restart fix: the declared-edge scan is a bounded scan, not something recall
// may pay on every query. A contradiction-free vault runs it once and then
// answers from the memoised verdict.
func TestContradictionHonesty_ProbeIsPaidOncePerVault(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()
	if _, err := eng.Write(ctx, &mbp.WriteRequest{Vault: "clean2", Concept: "timeout",
		Content: "the request timeout limit is 180ms"}); err != nil {
		t.Fatal(err)
	}
	eng.waitWriteTimeIdle()

	ws := eng.store.ResolveVaultPrefix("clean2")
	if eng.vaultMayHaveContradictions(ctx, ws) {
		t.Fatal("a contradiction-free vault must gate the phase off")
	}
	if _, cached := eng.contradictionProbeClean.Load(ws); !cached {
		t.Error("the declared-edge probe verdict was not memoised; every recall would pay the scan")
	}
	if eng.vaultMayHaveContradictions(ctx, ws) {
		t.Error("second call changed the verdict")
	}

	// A declaration invalidates the memoised verdict at its source.
	a, _ := contradictionFixture(t, eng, "clean2")
	_ = a
	if _, cached := eng.contradictionProbeClean.Load(ws); cached {
		t.Error("the clean verdict survived a new declaration in the same vault")
	}
	if !eng.vaultMayHaveContradictions(ctx, ws) {
		t.Error("gate is off on a vault that just had a contradiction declared")
	}
}

// TestContradictionHonesty_SupersedesEdgeAloneResolves isolates clause 3 of the
// unresolved test — "a declared RelSupersedes between the pair IS the
// resolution".
//
// TestContradictionHonesty_ResolvedBySupersedes above does NOT isolate it: the
// engine's Link(supersedes) also RETIRES the target, so clause 2 (endpoint
// liveness) clears the conflict first and that test passes with clause 3
// deleted. Here the RelSupersedes edge is written straight through the store,
// so both endpoints stay live and clause 3 is the only thing that can resolve
// the pair.
func TestContradictionHonesty_SupersedesEdgeAloneResolves(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	rows, ids := contradictionRows(t, eng, "sup", []struct {
		concept, content string
		score            float64
	}{
		{"request timeout limit", "the request timeout limit is 180ms", 0.80},
		{"request timeout limit revised", "the request timeout limit is 320ms", 0.60},
	})
	linkContradicts(t, eng, "sup", ids[1], ids[0])

	ws := eng.store.ResolveVaultPrefix("sup")
	run := func() *mbp.ConflictBlock {
		req := &activation.ActivateRequest{MaxResults: 10}
		now := time.Now()
		in := append([]activation.ScoredEngram(nil), rows...)
		_, block, _ := eng.applyContradictionHonesty(ctx, ws, in, req, newVisibilityGate(req, now), now)
		return block
	}
	if run() == nil {
		t.Fatal("precondition: the unresolved conflict must be reported first")
	}

	a, err := storage.ParseULID(ids[0])
	if err != nil {
		t.Fatal(err)
	}
	b, err := storage.ParseULID(ids[1])
	if err != nil {
		t.Fatal(err)
	}
	// Straight through the store: no retirement, no ValidUntil stamp, nothing
	// but the declared edge. Distinct weight so it gets its own forward-assoc
	// key rather than replacing the contradicts edge.
	if err := eng.store.WriteAssociation(ctx, ws, b, a, &storage.Association{
		TargetID: a, RelType: storage.RelSupersedes, Weight: 0.9, Confidence: 1, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	// Both endpoints are still live and still in the result set, so clause 2
	// cannot be what clears this.
	for i := range rows {
		if !contradictionEndpointLive(rows[i].Engram, &activation.ActivateRequest{}, time.Now()) {
			t.Fatalf("endpoint %s is no longer live — clause 2 would resolve this and the test proves nothing", ids[i])
		}
	}
	if block := run(); block != nil {
		t.Errorf("a declared RelSupersedes between the pair did not resolve the conflict: %+v", block)
	}
}

// BenchmarkRecallContradictionGate measures what COG-29 costs a recall, on a
// synthetic vault, in the two states that matter: the gate CLOSED (no
// contradiction anywhere — the overwhelming majority of vaults, which must pay
// nothing but one bounded seek plus a once-per-process declared-edge scan) and
// the gate OPEN (a declared pair present, so the phase runs its forward and
// reverse association reads over the examined window every query).
//
// It reports p50 and p99 rather than only the mean, because the concern this
// answers is tail latency: the open gate is sticky per vault, and the phase
// issues up to one forward batch read plus one bounded reverse iterator per
// examined row.
func BenchmarkRecallContradictionGate(b *testing.B) {
	for _, tc := range []struct {
		name     string
		conflict bool
	}{{"gate_closed", false}, {"gate_open", true}} {
		b.Run(tc.name, func(b *testing.B) {
			eng, cleanup := testEnv(b)
			defer cleanup()
			ctx := context.Background()
			vault := "bench-" + tc.name

			// Distinctive vocabulary per engram: only the first 10 rows are
			// about the query's topic. A corpus where every row contains
			// every query term collapses the IDF, nothing clears the
			// threshold, and the benchmark silently times the phase's
			// len(results)==0 early return instead of the phase (the G6
			// re-verification caught exactly that).
			var first, second string
			for i := 0; i < 200; i++ {
				var wreq *mbp.WriteRequest
				if i < 10 {
					wreq = &mbp.WriteRequest{Vault: vault,
						Concept: fmt.Sprintf("gateway timeout policy %d", i),
						Content: fmt.Sprintf("the request timeout limit for gateway %d is %dms", i, 100+i)}
				} else {
					wreq = &mbp.WriteRequest{Vault: vault,
						Concept: fmt.Sprintf("orchard telemetry %d", i),
						Content: fmt.Sprintf("orchard drone battery cell %d holds charge for %d minutes in frost", i, 40+i)}
				}
				w, err := eng.Write(ctx, wreq)
				if err != nil {
					b.Fatal(err)
				}
				if i == 0 {
					first = w.ID
				}
				if i == 1 {
					second = w.ID
				}
			}
			eng.waitWriteTimeIdle()
			if tc.conflict {
				if _, err := eng.Link(ctx, &mbp.LinkRequest{Vault: vault, SourceID: second,
					TargetID: first, RelType: uint16(storage.RelContradicts)}); err != nil {
					b.Fatal(err)
				}
				eng.waitWriteTimeIdle()
			}
			req := &mbp.ActivateRequest{Vault: vault,
				Context: []string{"what is the request timeout limit"}, MaxResults: 10, Threshold: 0.001}
			warm, err := eng.Activate(ctx, req) // warm caches + the once-per-vault probe
			if err != nil {
				b.Fatal(err)
			}
			// A benchmark that times an empty response measures the early
			// return, not the phase. Fail loudly instead of lying quietly.
			if len(warm.Activations) == 0 {
				b.Fatalf("benchmark query returned zero rows — the corpus no longer exercises the phase")
			}
			if tc.conflict && warm.Conflict == nil {
				b.Fatalf("gate_open arm has no conflict block — the phase is not firing on the timed path")
			}

			lat := make([]time.Duration, 0, b.N)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				t0 := time.Now()
				if _, err := eng.Activate(ctx, req); err != nil {
					b.Fatal(err)
				}
				lat = append(lat, time.Since(t0))
			}
			b.StopTimer()
			sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
			pct := func(p float64) float64 {
				if len(lat) == 0 {
					return 0
				}
				i := int(float64(len(lat)-1) * p)
				return float64(lat[i].Microseconds())
			}
			b.ReportMetric(pct(0.50), "p50-us")
			b.ReportMetric(pct(0.99), "p99-us")
		})
	}
}
