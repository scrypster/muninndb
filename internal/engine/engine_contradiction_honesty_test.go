package engine

import (
	"context"
	"testing"
	"time"

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

	// (ii) neither side is presented as the answer.
	for _, it := range resp.Activations {
		if (it.ID == a || it.ID == b) && float64(it.Score) > contradictionScoreCeiling {
			t.Errorf("row %s scored %.4f, above the %.2f ceiling — a disputed fact must never be presented at ~1.0",
				it.ID, it.Score, contradictionScoreCeiling)
		}
	}

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
// restated, and it is why COG-29 gathers a cluster to its LOWEST-ranked
// member rather than its highest.
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
// with the score capped and the response-level block present. The
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
	if float64(only.Score) > contradictionScoreCeiling {
		t.Errorf("row scored %.4f, above the ceiling", only.Score)
	}
	if resp.Conflict == nil {
		t.Error("response carries no conflict block")
	}
}
