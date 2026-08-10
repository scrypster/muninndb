package engine

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/engine/activation"
	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// supersedeTestHarness seeds engrams and returns helpers for building recall
// result sets and asserting supersession ranking behaviour.
type supersedeTestHarness struct {
	t   *testing.T
	eng *Engine
	ctx context.Context
	ws  [8]byte
}

func newSupersedeHarness(t *testing.T) (*supersedeTestHarness, func()) {
	eng, cleanup := testEnv(t)
	return &supersedeTestHarness{
		t:   t,
		eng: eng,
		ctx: context.Background(),
		ws:  eng.store.ResolveVaultPrefix("default"),
	}, cleanup
}

func (h *supersedeTestHarness) write(concept, content string) string {
	h.t.Helper()
	resp, err := h.eng.Write(h.ctx, &mbp.WriteRequest{Vault: "default", Concept: concept, Content: content})
	if err != nil {
		h.t.Fatalf("Write %q: %v", concept, err)
	}
	return resp.ID
}

// supersede links newID --RelSupersedes--> oldID (new replaces old).
func (h *supersedeTestHarness) supersede(newID, oldID string) {
	h.t.Helper()
	if _, err := h.eng.Link(h.ctx, &mbp.LinkRequest{
		Vault: "default", SourceID: newID, TargetID: oldID,
		RelType: uint16(storage.RelSupersedes), Weight: 1.0,
	}); err != nil {
		h.t.Fatalf("Link supersedes %s->%s: %v", newID, oldID, err)
	}
}

func (h *supersedeTestHarness) softDelete(id string) {
	h.t.Helper()
	u, err := storage.ParseULID(id)
	if err != nil {
		h.t.Fatalf("parse %s: %v", id, err)
	}
	if err := h.eng.store.SoftDelete(h.ctx, h.ws, u); err != nil {
		h.t.Fatalf("soft-delete %s: %v", id, err)
	}
}

// scored builds a result set from (id, score) pairs.
func (h *supersedeTestHarness) scored(pairs ...any) []activation.ScoredEngram {
	h.t.Helper()
	if len(pairs)%2 != 0 {
		h.t.Fatalf("scored: need id,score pairs")
	}
	out := make([]activation.ScoredEngram, 0, len(pairs)/2)
	for i := 0; i < len(pairs); i += 2 {
		id := pairs[i].(string)
		score := pairs[i+1].(float64)
		u, err := storage.ParseULID(id)
		if err != nil {
			h.t.Fatalf("parse %s: %v", id, err)
		}
		eng, err := h.eng.store.GetEngram(h.ctx, h.ws, u)
		if err != nil || eng == nil {
			h.t.Fatalf("GetEngram %s: %v", id, err)
		}
		out = append(out, activation.ScoredEngram{Engram: eng, Score: score})
	}
	return out
}

// apply runs supersession under a default (unrestricted) request. Tests that
// exercise the visibility gate build their own request via applyReq.
func (h *supersedeTestHarness) apply(results []activation.ScoredEngram) []activation.ScoredEngram {
	out, _ := h.eng.applySupersession(h.ctx, h.ws, results, &activation.ActivateRequest{}, time.Now())
	return out
}

func (h *supersedeTestHarness) applyReq(results []activation.ScoredEngram, req *activation.ActivateRequest) ([]activation.ScoredEngram, int) {
	return h.eng.applySupersession(h.ctx, h.ws, results, req, time.Now())
}

// rankOf returns the 0-based rank of id in results (-1 if absent).
func rankOf(results []activation.ScoredEngram, id string) int {
	for i, r := range results {
		if r.Engram.ID.String() == id {
			return i
		}
	}
	return -1
}

func scoreOf(results []activation.ScoredEngram, id string) (float64, bool) {
	for _, r := range results {
		if r.Engram.ID.String() == id {
			return r.Score, true
		}
	}
	return 0, false
}

// TestApplySupersession_PromotesCurrentOverStale is the headline case: the stale
// fact scored higher (matched the query better) but the current fact must lead.
func TestApplySupersession_PromotesCurrentOverStale(t *testing.T) {
	h, cleanup := newSupersedeHarness(t)
	defer cleanup()

	oldID := h.write("Runway was 8 months in May", "runway 8mo")
	newID := h.write("Bridge raise extended runway to 11 months", "runway 11mo")
	h.supersede(newID, oldID)

	// Stale outranks current in the raw pool (the proven-on-labs situation).
	got := h.apply(h.scored(oldID, 1.15, newID, 0.92))

	if rankOf(got, newID) >= rankOf(got, oldID) {
		t.Fatalf("current fact must outrank stale: new rank %d, old rank %d", rankOf(got, newID), rankOf(got, oldID))
	}
	ns, _ := scoreOf(got, newID)
	os, _ := scoreOf(got, oldID)
	if ns < 1.15 {
		t.Errorf("head should inherit the topic's earned score >=1.15, got %v", ns)
	}
	if os >= ns {
		t.Errorf("stale score %v must sit below head %v", os, ns)
	}
	// Never hidden.
	if rankOf(got, oldID) < 0 {
		t.Error("stale fact must remain visible, not be dropped")
	}
}

// TestApplySupersession_InjectsAbsentHead proves the fix REORDERS *and* INJECTS:
// when the query matched only the stale phrasing, the current fact is pulled into
// the results so recall does not silently truncate the topic. (Fable's added case.)
func TestApplySupersession_InjectsAbsentHead(t *testing.T) {
	h, cleanup := newSupersedeHarness(t)
	defer cleanup()

	oldID := h.write("Runway was 8 months in May", "runway 8mo")
	newID := h.write("Bridge raise extended runway to 11 months", "runway 11mo")
	h.supersede(newID, oldID)

	// Only the stale fact is in the candidate pool.
	got := h.apply(h.scored(oldID, 1.15))

	if rankOf(got, newID) < 0 {
		t.Fatal("current fact must be INJECTED when the query missed it")
	}
	if rankOf(got, newID) >= rankOf(got, oldID) {
		t.Errorf("injected head must outrank stale: new %d, old %d", rankOf(got, newID), rankOf(got, oldID))
	}
	ns, _ := scoreOf(got, newID)
	if ns < 1.15 {
		t.Errorf("injected head should carry the stale fact's earned score, got %v", ns)
	}
}

// TestApplySupersession_ChainResolvesToHead: A<-B<-C, only the head C should win;
// intermediates that surfaced are demoted below it. (chain rule)
func TestApplySupersession_ChainResolvesToHead(t *testing.T) {
	h, cleanup := newSupersedeHarness(t)
	defer cleanup()

	a := h.write("v1", "first")
	b := h.write("v2", "second")
	c := h.write("v3", "third (current)")
	h.supersede(b, a) // b supersedes a
	h.supersede(c, b) // c supersedes b

	got := h.apply(h.scored(a, 1.10, b, 0.70))

	if rankOf(got, c) < 0 {
		t.Fatal("chain head C must be present (injected)")
	}
	if rankOf(got, c) >= rankOf(got, a) || rankOf(got, c) >= rankOf(got, b) {
		t.Errorf("head C must outrank both A and B: C %d, A %d, B %d", rankOf(got, c), rankOf(got, a), rankOf(got, b))
	}
}

// TestApplySupersession_VoidedWhenSupersederDeleted: if the only superseder is
// soft-deleted, the supersession is void and the fact is NOT demoted.
func TestApplySupersession_VoidedWhenSupersederDeleted(t *testing.T) {
	h, cleanup := newSupersedeHarness(t)
	defer cleanup()

	oldID := h.write("still current", "content")
	newID := h.write("retracted replacement", "content2")
	h.supersede(newID, oldID)
	h.softDelete(newID)

	got := h.apply(h.scored(oldID, 1.15))

	os, _ := scoreOf(got, oldID)
	if os != 1.15 {
		t.Errorf("fact with a soft-deleted superseder must keep its score 1.15, got %v", os)
	}
	if rankOf(got, newID) >= 0 {
		t.Error("a soft-deleted superseder must not be injected")
	}
}

// TestApplySupersession_CycleLeavesUnchanged: a supersedes cycle (A<->B) is
// unresolvable; scores must be left untouched (degrade loudly via WARN, not
// guess a head). (Fable's added case.)
func TestApplySupersession_CycleLeavesUnchanged(t *testing.T) {
	h, cleanup := newSupersedeHarness(t)
	defer cleanup()

	a := h.write("A", "a")
	b := h.write("B", "b")
	h.supersede(a, b) // a supersedes b
	h.supersede(b, a) // b supersedes a  -> cycle

	got := h.apply(h.scored(a, 1.00, b, 0.90))
	as, _ := scoreOf(got, a)
	bs, _ := scoreOf(got, b)
	if as != 1.00 || bs != 0.90 {
		t.Errorf("cycle must leave scores unchanged, got a=%v b=%v", as, bs)
	}
}

// TestApplySupersession_UnrelatedUnaffected: results matching no supersession are
// returned untouched (ordering invariant among non-superseded results).
func TestApplySupersession_UnrelatedUnaffected(t *testing.T) {
	h, cleanup := newSupersedeHarness(t)
	defer cleanup()

	x := h.write("unrelated one", "x")
	y := h.write("unrelated two", "y")

	got := h.apply(h.scored(x, 0.80, y, 0.60))
	xs, _ := scoreOf(got, x)
	ys, _ := scoreOf(got, y)
	if xs != 0.80 || ys != 0.60 || len(got) != 2 {
		t.Errorf("non-superseded results must be unchanged, got x=%v y=%v len=%d", xs, ys, len(got))
	}
}

// TestApplySupersession_AnnotatesFields checks the Increment-2 payload: a demoted
// stale fact carries SupersededBy (immediate superseder) and CurrentVersion (chain
// head). For a depth-1 supersession they are equal; in a chain they differ.
func TestApplySupersession_AnnotatesFields(t *testing.T) {
	h, cleanup := newSupersedeHarness(t)
	defer cleanup()

	// Depth-1: old superseded directly by new.
	oldID := h.write("old", "o")
	newID := h.write("new", "n")
	h.supersede(newID, oldID)

	got := h.apply(h.scored(oldID, 1.15, newID, 0.90))
	for _, r := range got {
		if r.Engram.ID.String() == oldID {
			if r.SupersededBy.String() != newID {
				t.Errorf("depth-1 SupersededBy = %s, want %s", r.SupersededBy, newID)
			}
			if r.CurrentVersion.String() != newID {
				t.Errorf("depth-1 CurrentVersion = %s, want %s", r.CurrentVersion, newID)
			}
		}
	}

	// Chain A<-B<-C: A's immediate superseder is B, its current version is C.
	a := h.write("a", "a")
	b := h.write("b", "b")
	c := h.write("c", "c")
	h.supersede(b, a)
	h.supersede(c, b)
	got2 := h.apply(h.scored(a, 1.15))
	for _, r := range got2 {
		if r.Engram.ID.String() == a {
			if r.SupersededBy.String() != b {
				t.Errorf("chain SupersededBy = %s, want immediate %s", r.SupersededBy, b)
			}
			if r.CurrentVersion.String() != c {
				t.Errorf("chain CurrentVersion = %s, want head %s", r.CurrentVersion, c)
			}
		}
	}
}

// TestApplySupersession_StaleNeverLeapfrogsUnrelated is the refute-pass bug #1
// guard: a barely-matching stale near-duplicate must NEVER be lifted above a
// genuinely-relevant unrelated result. Head H scores high on its own merits
// (2.0); stale S (superseded by H) barely matched (0.30); unrelated Z earned
// 1.00. Demote-only means S stays at 0.30, below Z — the head, not the stale
// fact, is what gets promoted.
func TestApplySupersession_StaleNeverLeapfrogsUnrelated(t *testing.T) {
	h, cleanup := newSupersedeHarness(t)
	defer cleanup()

	staleID := h.write("stale near-duplicate", "old")
	headID := h.write("current fact, strong on its own", "new")
	unrelID := h.write("unrelated but genuinely relevant", "z")
	h.supersede(headID, staleID)

	got := h.apply(h.scored(headID, 2.0, unrelID, 1.0, staleID, 0.30))

	ss, _ := scoreOf(got, staleID)
	if ss > 0.30 {
		t.Errorf("stale fact must never be promoted above its earned 0.30, got %v", ss)
	}
	if rankOf(got, unrelID) >= rankOf(got, staleID) {
		t.Errorf("unrelated genuine match (rank %d) must outrank the stale near-dup (rank %d)",
			rankOf(got, unrelID), rankOf(got, staleID))
	}
	if rankOf(got, headID) != 0 {
		t.Errorf("head must lead at rank 0, got %d", rankOf(got, headID))
	}
}

// TestApplySupersession_OrderIndependent is the refute-pass bug #2 guard: the
// same input in two orders must produce identical scores (two-phase scoring, no
// read-after-mutate). Chain A(1.10)<-B(0.05)<-C(head, absent).
func TestApplySupersession_OrderIndependent(t *testing.T) {
	h, cleanup := newSupersedeHarness(t)
	defer cleanup()

	a := h.write("v1 strong", "a")
	b := h.write("v2 weak", "b")
	c := h.write("v3 head", "c")
	h.supersede(b, a)
	h.supersede(c, b)

	g1 := h.apply(h.scored(a, 1.10, b, 0.05))
	g2 := h.apply(h.scored(b, 0.05, a, 1.10))

	for _, id := range []string{a, b, c} {
		s1, _ := scoreOf(g1, id)
		s2, _ := scoreOf(g2, id)
		if s1 != s2 {
			t.Errorf("score for %s is order-dependent: %v vs %v", id, s1, s2)
		}
	}
	// The weak intermediate B must stay at its earned 0.05 (demote-only), not be
	// pinned to head−ε.
	bs, _ := scoreOf(g1, b)
	if bs != 0.05 {
		t.Errorf("weak intermediate must keep earned 0.05 (demote-only), got %v", bs)
	}
}

// TestApplySupersession_DiamondLeavesUnchanged: an engram with two active
// superseders is ambiguous — WARN and leave un-demoted (mirror the cycle path),
// never silently pick one.
func TestApplySupersession_DiamondLeavesUnchanged(t *testing.T) {
	h, cleanup := newSupersedeHarness(t)
	defer cleanup()

	x := h.write("contested", "x")
	b := h.write("superseder one", "b")
	c := h.write("superseder two", "c")
	h.supersede(b, x)
	h.supersede(c, x) // x now has two active superseders

	got := h.apply(h.scored(x, 1.00))
	xs, _ := scoreOf(got, x)
	if xs != 1.00 {
		t.Errorf("ambiguous (multi-superseder) engram must be left unchanged, got %v", xs)
	}
}

// TestApplySupersession_SelfLoop: supersede(a,a) is a self-cycle; must be treated
// as unresolvable, scores untouched.
func TestApplySupersession_SelfLoop(t *testing.T) {
	h, cleanup := newSupersedeHarness(t)
	defer cleanup()

	a := h.write("self", "a")
	h.supersede(a, a)

	got := h.apply(h.scored(a, 1.00))
	as, _ := scoreOf(got, a)
	if as != 1.00 {
		t.Errorf("self-loop must leave score unchanged, got %v", as)
	}
}

// TestApplySupersession_ManyReverseEdges: a stale fact with many (>16) unrelated
// reverse edges plus its RelSupersedes edge must still be detected as superseded
// (the reverse-scan cap must not hide it).
func TestApplySupersession_ManyReverseEdges(t *testing.T) {
	h, cleanup := newSupersedeHarness(t)
	defer cleanup()

	oldID := h.write("stale with many refs", "old")
	newID := h.write("current", "new")
	// 20 other engrams each point at oldID with a non-supersedes relation.
	for i := 0; i < 20; i++ {
		other := h.write("ref", "ref content")
		if _, err := h.eng.Link(h.ctx, &mbp.LinkRequest{
			Vault: "default", SourceID: other, TargetID: oldID,
			RelType: uint16(storage.RelRelatesTo), Weight: 0.5,
		}); err != nil {
			t.Fatalf("link ref: %v", err)
		}
	}
	h.supersede(newID, oldID)

	got := h.apply(h.scored(oldID, 1.15))
	if rankOf(got, newID) < 0 {
		t.Fatal("supersedes edge missed behind >16 other reverse edges — head not injected")
	}
	if rankOf(got, newID) >= rankOf(got, oldID) {
		t.Errorf("head must outrank stale despite many reverse edges")
	}
}

// --- Visibility-gate tests: an absent head the caller's request refuses must
// void the WHOLE substitution (no injection, no demotion, no annotation), and
// an admitted injection must be counted. Each guard test fails with the gate
// check in applySupersession reverted (RED-verified). ---

// requireAbstained asserts the atomic-abstention contract: head absent, stale
// at its earned score, no supersession annotation, nothing counted.
func requireAbstained(t *testing.T, got []activation.ScoredEngram, injected int, staleID, headID string, earned float64) {
	t.Helper()
	if rankOf(got, headID) != -1 {
		t.Fatalf("refused head %s was injected", headID)
	}
	if injected != 0 {
		t.Errorf("injected = %d, want 0 on abstention", injected)
	}
	s, ok := scoreOf(got, staleID)
	if !ok {
		t.Fatalf("stale %s missing from results", staleID)
	}
	if s != earned {
		t.Errorf("stale score = %v, want earned %v (no demotion on abstention)", s, earned)
	}
	for _, r := range got {
		if r.Engram.ID.String() == staleID {
			if r.SupersededBy != (storage.ULID{}) || r.CurrentVersion != (storage.ULID{}) {
				t.Errorf("stale row annotated (SupersededBy=%s CurrentVersion=%s) — leaks the refused head",
					r.SupersededBy, r.CurrentVersion)
			}
		}
	}
}

// TestApplySupersession_HeadBlockedByMetaFilter_AbstainsAtomically: the stale
// fact matches the caller's tag filter; the head does not. Injecting the head
// would violate the filter (#654's class); demoting or annotating the stale
// without it would half-apply the substitution.
func TestApplySupersession_HeadBlockedByMetaFilter_AbstainsAtomically(t *testing.T) {
	h, cleanup := newSupersedeHarness(t)
	defer cleanup()

	staleID, err := h.eng.store.WriteEngram(h.ctx, h.ws, &storage.Engram{
		Concept: "stale-tagged", Content: "stale fact", Confidence: 0.9,
		Tags: []string{"wanted"},
	})
	if err != nil {
		t.Fatalf("write stale: %v", err)
	}
	headID := h.write("head-untagged", "current fact")
	h.supersede(headID, staleID.String())

	req := &activation.ActivateRequest{
		Filters: []activation.Filter{{Field: "tags_all", Value: []string{"wanted"}}},
	}
	got, injected := h.applyReq(h.scored(staleID.String(), 0.9), req)
	requireAbstained(t, got, injected, staleID.String(), headID, 0.9)

	// Positive control: a head that satisfies the same filter substitutes in
	// full — the abstention above is the gate's doing, not a broken chain.
	stale2, err := h.eng.store.WriteEngram(h.ctx, h.ws, &storage.Engram{
		Concept: "stale-tagged-2", Content: "stale fact 2", Confidence: 0.9,
		Tags: []string{"wanted"},
	})
	if err != nil {
		t.Fatalf("write stale2: %v", err)
	}
	head2, err := h.eng.store.WriteEngram(h.ctx, h.ws, &storage.Engram{
		Concept: "head-tagged-2", Content: "current fact 2", Confidence: 0.9,
		Tags: []string{"wanted"},
	})
	if err != nil {
		t.Fatalf("write head2: %v", err)
	}
	h.supersede(head2.String(), stale2.String())
	got, injected = h.applyReq(h.scored(stale2.String(), 0.9), req)
	if rankOf(got, head2.String()) == -1 || injected != 1 {
		t.Fatalf("control: filter-satisfying head must inject (rank=%d injected=%d)", rankOf(got, head2.String()), injected)
	}
}

// TestApplySupersession_HeadUntrusted_AbstainsAtomically: under
// ExcludeUntrusted, a TrustUntrusted head must not enter — and must not
// demote the trusted stale it would have replaced.
func TestApplySupersession_HeadUntrusted_AbstainsAtomically(t *testing.T) {
	h, cleanup := newSupersedeHarness(t)
	defer cleanup()

	staleID := h.write("stale-trusted", "stale fact")
	headID := h.write("head-untrusted", "current fact")
	h.supersede(headID, staleID)
	headULID, err := storage.ParseULID(headID)
	if err != nil {
		t.Fatalf("parse head: %v", err)
	}
	if err := h.eng.store.UpdateTrust(h.ctx, h.ws, headULID, storage.TrustUntrusted); err != nil {
		t.Fatalf("mark head untrusted: %v", err)
	}

	req := &activation.ActivateRequest{ExcludeUntrusted: true}
	got, injected := h.applyReq(h.scored(staleID, 0.9), req)
	requireAbstained(t, got, injected, staleID, headID, 0.9)

	// Positive control: without ExcludeUntrusted the SAME untrusted head
	// substitutes in full — the abstention above is the trust gate's doing.
	got, injected = h.applyReq(h.scored(staleID, 0.9), &activation.ActivateRequest{})
	if rankOf(got, headID) == -1 || injected != 1 {
		t.Fatalf("control: head must inject without ExcludeUntrusted (rank=%d injected=%d)", rankOf(got, headID), injected)
	}
}

// TestApplySupersession_HeadUnderForeignLease_AbstainsAtomically: a head
// checked out by another agent is invisible to this caller (#548). Annotating
// the stale row with the hidden head's ID would leak the exact existence the
// lease hides, so the whole substitution abstains.
func TestApplySupersession_HeadUnderForeignLease_AbstainsAtomically(t *testing.T) {
	h, cleanup := newSupersedeHarness(t)
	defer cleanup()

	staleID := h.write("stale-lease", "stale fact")
	headID := h.write("head-leased", "current fact")
	h.supersede(headID, staleID)

	claimRes, err := h.eng.Claim(h.ctx, "default", headID, "other-agent", 600)
	if err != nil {
		t.Fatalf("claim head: %v", err)
	}
	if claimRes.Status != LeaseAcquired {
		t.Fatalf("claim status = %v, want LeaseAcquired", claimRes.Status)
	}

	req := &activation.ActivateRequest{CallerOwner: "me"}
	got, injected := h.applyReq(h.scored(staleID, 0.9), req)
	requireAbstained(t, got, injected, staleID, headID, 0.9)

	// IncludeLeased (admin/debugging) re-admits the head: full substitution.
	got, injected = h.applyReq(h.scored(staleID, 0.9), &activation.ActivateRequest{
		CallerOwner: "me", IncludeLeased: true,
	})
	if rankOf(got, headID) == -1 {
		t.Fatal("IncludeLeased must disable lease-based hiding for injected heads")
	}
	if injected != 1 {
		t.Errorf("injected = %d, want 1 under IncludeLeased", injected)
	}
}

// TestApplySupersession_LeaseReadErrorFailsClosed: a lease-read fault must
// refuse the injection (and the substitution with it), mirroring the boost
// path and phase 6 — never silently admit a possibly-checked-out engram.
func TestApplySupersession_LeaseReadErrorFailsClosed(t *testing.T) {
	h, cleanup := newSupersedeHarness(t)
	defer cleanup()

	staleID := h.write("stale-leaseerr", "stale fact")
	headID := h.write("head-leaseerr", "current fact")
	h.supersede(headID, staleID)

	// Fault-inject a lease-read error for the head only; every other id
	// (including calls from concurrently-running tests) is unaffected.
	faultyID, err := storage.ParseULID(headID)
	if err != nil {
		t.Fatalf("parse head: %v", err)
	}
	orig := getLeaseForInjection
	getLeaseForInjection = func(ctx context.Context, s *storage.PebbleStore, wsPrefix [8]byte, id storage.ULID) (storage.Lease, error) {
		if id == faultyID {
			return storage.Lease{}, fmt.Errorf("simulated lease read failure")
		}
		return orig(ctx, s, wsPrefix, id)
	}
	defer func() { getLeaseForInjection = orig }()

	got, injected := h.applyReq(h.scored(staleID, 0.9), &activation.ActivateRequest{})
	requireAbstained(t, got, injected, staleID, headID, 0.9)
}

// TestApplySupersession_HeadAfterAsOf_AbstainsAtomically: at the caller's
// chosen instant the "stale" fact WAS the truth — a head that only became
// valid later must not be injected, and the stale fact must keep the rank it
// earned, unannotated. (The head is valid NOW; only the as-of view refuses it.)
func TestApplySupersession_HeadAfterAsOf_AbstainsAtomically(t *testing.T) {
	h, cleanup := newSupersedeHarness(t)
	defer cleanup()

	past := time.Now().Add(-2 * time.Hour)
	staleULID, err := h.eng.store.WriteEngram(h.ctx, h.ws, &storage.Engram{
		Concept: "stale-asof", Content: "stale fact", Confidence: 0.9,
		ValidFrom: past,
	})
	if err != nil {
		t.Fatalf("write stale: %v", err)
	}
	staleID := staleULID.String()
	headID := h.write("head-asof", "current fact") // ValidFrom defaults to now
	h.supersede(headID, staleID)

	// Sanity: with no AsOf the substitution applies (the head is visible now).
	got, injected := h.applyReq(h.scored(staleID, 0.9), &activation.ActivateRequest{})
	if rankOf(got, headID) == -1 || injected != 1 {
		t.Fatalf("precondition: head must inject under a plain request (rank=%d injected=%d)",
			rankOf(got, headID), injected)
	}

	// As of one hour ago: the head's ValidFrom is in the query's future.
	asOf := time.Now().Add(-1 * time.Hour)
	got, injected = h.applyReq(h.scored(staleID, 0.9), &activation.ActivateRequest{AsOf: &asOf})
	requireAbstained(t, got, injected, staleID, headID, 0.9)
}

// TestApplySupersession_AdmittedHeadCounts: the plain path reports its
// injection so the caller can keep TotalFound honest (closing the counting
// gap the #570 review scoped as a follow-up).
func TestApplySupersession_AdmittedHeadCounts(t *testing.T) {
	h, cleanup := newSupersedeHarness(t)
	defer cleanup()

	staleID := h.write("stale-count", "stale fact")
	headID := h.write("head-count", "current fact")
	h.supersede(headID, staleID)

	got, injected := h.applyReq(h.scored(staleID, 0.9), &activation.ActivateRequest{})
	if injected != 1 {
		t.Errorf("injected = %d, want 1", injected)
	}
	if rankOf(got, headID) != 0 {
		t.Errorf("admitted head must lead (rank %d)", rankOf(got, headID))
	}

	// Promotion of an in-pool head is not an injection.
	got, injected = h.applyReq(h.scored(staleID, 0.9, headID, 0.5), &activation.ActivateRequest{})
	if injected != 0 {
		t.Errorf("injected = %d, want 0 when the head was already in the pool", injected)
	}
	if rankOf(got, headID) != 0 {
		t.Errorf("promoted head must lead (rank %d)", rankOf(got, headID))
	}
}

// annotationOf returns (SupersededBy, CurrentVersion) for id in results.
func annotationOf(t *testing.T, results []activation.ScoredEngram, id string) (string, string) {
	t.Helper()
	for _, r := range results {
		if r.Engram.ID.String() == id {
			return r.SupersededBy.String(), r.CurrentVersion.String()
		}
	}
	t.Fatalf("%s missing from results", id)
	return "", ""
}

// TestApplySupersession_HiddenIntermediateNeverNamed: chain A<-B<-C with B
// under a live foreign lease and C visible. The substitution proceeds to the
// visible head C, but A's annotation must name C — never lease-hidden B,
// whose ULID is exactly the existence #548 hides. (Adversarial-review
// finding 1; RED against the endpoint-only gate.)
func TestApplySupersession_HiddenIntermediateNeverNamed(t *testing.T) {
	h, cleanup := newSupersedeHarness(t)
	defer cleanup()

	a := h.write("chain-a", "v1")
	b := h.write("chain-b", "v2")
	c := h.write("chain-c", "v3")
	h.supersede(b, a)
	h.supersede(c, b)

	claimRes, err := h.eng.Claim(h.ctx, "default", b, "other-agent", 600)
	if err != nil || claimRes.Status != LeaseAcquired {
		t.Fatalf("claim b: %v %v", err, claimRes.Status)
	}

	got, injected := h.applyReq(h.scored(a, 0.9), &activation.ActivateRequest{CallerOwner: "me"})
	if rankOf(got, c) == -1 {
		t.Fatal("visible head C must still be injected past the hidden intermediate")
	}
	if injected != 1 {
		t.Errorf("injected = %d, want 1 (C)", injected)
	}
	if rankOf(got, b) != -1 {
		t.Error("lease-hidden B must not appear in results")
	}
	sb, cv := annotationOf(t, got, a)
	if sb == b || cv == b {
		t.Errorf("annotation names lease-hidden B (SupersededBy=%s CurrentVersion=%s) — #548 leak", sb, cv)
	}
	if sb != c || cv != c {
		t.Errorf("annotation should name visible C (SupersededBy=%s CurrentVersion=%s, want %s)", sb, cv, c)
	}
}

// TestApplySupersession_HiddenTailStopsAtVisibleIntermediate: chain A<-B<-C
// with C under a live foreign lease and B visible in the pool.
// (Adversarial-review finding 2; RED against endpoint-only abstention.)
//
// Two views, two correct answers. B carries a stamped ValidUntil (the
// supersede Link stamps its target), so under the DEFAULT view B is expired
// lineage and cannot be the current head — with C hidden there is nothing
// view-valid to substitute toward, and the whole substitution abstains (in
// full recall the final COG-19 cut then sweeps the expired rows; a caller
// who may not see C gets no half-answer built on it). Under a show-history
// view (IncludeInvalid), expired B IS presentable: the effective head under
// the caller's view is B — A demotes below it, and C leaks nowhere.
func TestApplySupersession_HiddenTailStopsAtVisibleIntermediate(t *testing.T) {
	h, cleanup := newSupersedeHarness(t)
	defer cleanup()

	a := h.write("tail-a", "v1")
	b := h.write("tail-b", "v2")
	c := h.write("tail-c", "v3")
	h.supersede(b, a)
	h.supersede(c, b)

	claimRes, err := h.eng.Claim(h.ctx, "default", c, "other-agent", 600)
	if err != nil || claimRes.Status != LeaseAcquired {
		t.Fatalf("claim c: %v %v", err, claimRes.Status)
	}

	// Default view: B is expired lineage, C is hidden → atomic abstention.
	got, injected := h.applyReq(h.scored(a, 0.9, b, 0.5), &activation.ActivateRequest{CallerOwner: "me"})
	if injected != 0 {
		t.Errorf("default view: injected = %d, want 0", injected)
	}
	if rankOf(got, c) != -1 {
		t.Error("default view: lease-hidden C must not appear in results")
	}
	if s, _ := scoreOf(got, a); s != 0.9 {
		t.Errorf("default view: A must keep its earned score (got %v)", s)
	}
	sb, cv := annotationOf(t, got, a)
	if sb != "" && sb != (storage.ULID{}).String() {
		t.Errorf("default view: A must carry no annotation (SupersededBy=%s)", sb)
	}
	_ = cv

	// Show-history view: expired B is presentable — the head under this view.
	got, injected = h.applyReq(h.scored(a, 0.9, b, 0.5), &activation.ActivateRequest{
		CallerOwner: "me", IncludeInvalid: true,
	})
	if injected != 0 {
		t.Errorf("history view: injected = %d, want 0 (B already in pool)", injected)
	}
	if rankOf(got, c) != -1 {
		t.Error("history view: lease-hidden C must not appear in results")
	}
	if rankOf(got, b) >= rankOf(got, a) {
		t.Errorf("history view: visible successor B must outrank stale A (B %d, A %d)", rankOf(got, b), rankOf(got, a))
	}
	sb, cv = annotationOf(t, got, a)
	if sb != b || cv != b {
		t.Errorf("history view: A's annotation must name B (SupersededBy=%s CurrentVersion=%s)", sb, cv)
	}
	if sb == c || cv == c {
		t.Errorf("history view: annotation names lease-hidden C — leak")
	}
}

// TestApplySupersession_AsOfChainStopsAtContemporaryHead: A<-B<-C where C
// became valid only after the caller's AsOf instant and B before it. At that
// instant B WAS the head: substitution runs to B, C is neither injected nor
// named. (The time-travel half of adversarial-review finding 2.)
func TestApplySupersession_AsOfChainStopsAtContemporaryHead(t *testing.T) {
	h, cleanup := newSupersedeHarness(t)
	defer cleanup()

	twoHours := time.Now().Add(-2 * time.Hour)
	aU, err := h.eng.store.WriteEngram(h.ctx, h.ws, &storage.Engram{
		Concept: "asof-a", Content: "v1", Confidence: 0.9, ValidFrom: twoHours,
	})
	if err != nil {
		t.Fatalf("write a: %v", err)
	}
	bU, err := h.eng.store.WriteEngram(h.ctx, h.ws, &storage.Engram{
		Concept: "asof-b", Content: "v2", Confidence: 0.9, ValidFrom: twoHours,
	})
	if err != nil {
		t.Fatalf("write b: %v", err)
	}
	a, b := aU.String(), bU.String()
	c := h.write("asof-c", "v3") // ValidFrom defaults to now
	h.supersede(b, a)
	h.supersede(c, b)

	asOf := time.Now().Add(-1 * time.Hour)
	got, injected := h.applyReq(h.scored(a, 0.9), &activation.ActivateRequest{AsOf: &asOf})
	if rankOf(got, c) != -1 {
		t.Error("post-AsOf head C must not appear in an as-of view")
	}
	if rankOf(got, b) == -1 {
		t.Fatal("B was the head at the caller's instant and must be injected")
	}
	if injected != 1 {
		t.Errorf("injected = %d, want 1 (B)", injected)
	}
	if rankOf(got, b) >= rankOf(got, a) {
		t.Errorf("B must outrank A in the as-of view")
	}
	sb, cv := annotationOf(t, got, a)
	if sb != b || cv != b {
		t.Errorf("annotation must name B, the head at the caller's instant (got SupersededBy=%s CurrentVersion=%s)", sb, cv)
	}
}

// rejectULIDFilter is a minimal EngramFilter rejecting exactly one engram —
// the structured (MQL WHERE) predicate a gated injection must honor.
type rejectULIDFilter struct{ reject storage.ULID }

func (f rejectULIDFilter) Match(e *storage.Engram) bool { return e.ID != f.reject }

// TestApplySupersession_HeadBlockedByStructuredFilter_AbstainsAtomically: a
// head the caller's MQL WHERE predicate rejects must not be injected — and
// the substitution abstains whole. (Adversarial-review finding 3; RED
// against a gate without the StructuredFilter predicate.)
func TestApplySupersession_HeadBlockedByStructuredFilter_AbstainsAtomically(t *testing.T) {
	h, cleanup := newSupersedeHarness(t)
	defer cleanup()

	staleID := h.write("stale-mql", "stale fact")
	headID := h.write("head-mql", "current fact")
	h.supersede(headID, staleID)
	headULID, err := storage.ParseULID(headID)
	if err != nil {
		t.Fatalf("parse head: %v", err)
	}

	req := &activation.ActivateRequest{StructuredFilter: rejectULIDFilter{reject: headULID}}
	got, injected := h.applyReq(h.scored(staleID, 0.9), req)
	requireAbstained(t, got, injected, staleID, headID, 0.9)

	// Positive control: a predicate that accepts the head admits the full
	// substitution.
	got, injected = h.applyReq(h.scored(staleID, 0.9), &activation.ActivateRequest{
		StructuredFilter: rejectULIDFilter{}, // zero ULID rejects nothing real
	})
	if rankOf(got, headID) == -1 || injected != 1 {
		t.Fatalf("control: accepting predicate must admit the head (rank=%d injected=%d)", rankOf(got, headID), injected)
	}
}

// TestApplySupersession_InjectionCountsInTotalFound pins the CALL-SITE wire:
// an injected head must be reflected in the response's TotalFound (the
// counting gap the #570 review scoped as a follow-up). Full-pipeline test on
// purpose — the RED-verified unit test pins only applySupersession's return
// value; this one fails if the engine.go `TotalFound += supInjected` wire is
// dropped.
//
// The chain is created UNSTAMPED (raw association write, not Link) because a
// Link-stamped predecessor is validity-expired and dropped inside Run() —
// unstamped legacy chains are exactly the population read-time supersession
// serves in default view. Differential form: the same query runs before and
// after the supersede edge exists, so the head's organic retrievability is
// measured, not assumed — if the head was already in the pool the second run
// promotes (TotalFound unchanged); if it was absent the second run injects
// (TotalFound +1). Either branch is deterministic given the observed first
// run; the injection branch is the one that pins the wire.
func TestApplySupersession_InjectionCountsInTotalFound(t *testing.T) {
	t.Parallel()
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	const vault = "supersession-total-test"
	ws := eng.store.ResolveVaultPrefix(vault)

	stale, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault: vault, Concept: "runway eight months",
		Content: "zanzibar quill futon eight months of runway remain",
	})
	if err != nil {
		t.Fatalf("write stale: %v", err)
	}
	head, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault: vault, Concept: "runway eleven months",
		Content: "bridge raise extended the figure",
	})
	if err != nil {
		t.Fatalf("write head: %v", err)
	}

	awaitFTS(t, eng)

	query := &mbp.ActivateRequest{
		Vault:      vault,
		Context:    []string{"zanzibar quill futon eight months"},
		MaxResults: 50,
		// 0.10 (production default): the stale fact is a full lexical match but a
		// fresh engram, so post-ACT-R-calibration it scores ~0.24 (was tuned to 0.30
		// on an older scale); 0.10 retrieves the stale fact while still excluding the
		// head (~0.02) so head presence still proves supersession injection, not organic recall.
		Threshold: 0.10,
	}
	before, err := eng.Activate(ctx, query)
	if err != nil {
		t.Fatalf("activate (before): %v", err)
	}
	headOrganic := false
	staleFound := false
	for _, item := range before.Activations {
		if item.ID == head.ID {
			headOrganic = true
		}
		if item.ID == stale.ID {
			staleFound = true
		}
	}
	if !staleFound {
		t.Fatal("precondition: the query must retrieve the stale fact")
	}

	// Drift control: the same query again, still without the edge. If
	// repeat-query side effects (cache-touch recency, reinforcement) could
	// move TotalFound on their own, the injection branch below would be
	// vacuous — an organic +1 mimicking the wire's +1.
	control, err := eng.Activate(ctx, query)
	if err != nil {
		t.Fatalf("activate (control): %v", err)
	}
	if control.TotalFound != before.TotalFound {
		t.Fatalf("drift control failed: repeat query moved TotalFound %d -> %d without any edge",
			before.TotalFound, control.TotalFound)
	}

	// Unstamped supersede edge, as a legacy (pre-valid-time) chain would be.
	staleULID, err := storage.ParseULID(stale.ID)
	if err != nil {
		t.Fatalf("parse stale: %v", err)
	}
	headULID, err := storage.ParseULID(head.ID)
	if err != nil {
		t.Fatalf("parse head: %v", err)
	}
	if err := eng.store.WriteAssociation(ctx, ws, headULID, staleULID, &storage.Association{
		TargetID: staleULID, RelType: storage.RelSupersedes, Weight: 1.0, Confidence: 1.0,
	}); err != nil {
		t.Fatalf("write supersedes association: %v", err)
	}

	after, err := eng.Activate(ctx, query)
	if err != nil {
		t.Fatalf("activate (after): %v", err)
	}
	headAfter := false
	for _, item := range after.Activations {
		if item.ID == head.ID {
			headAfter = true
		}
	}
	if !headAfter {
		t.Fatal("head must be present after the supersede edge exists (promoted or injected)")
	}

	if headOrganic {
		// Promotion path: no injection, so no count change from this phase.
		if after.TotalFound != before.TotalFound {
			t.Errorf("promotion must not change TotalFound: before=%d after=%d", before.TotalFound, after.TotalFound)
		}
		t.Logf("head was organically retrieved; exercised the promotion branch")
	} else {
		// Injection path: the call-site wire must add exactly the head.
		if after.TotalFound != before.TotalFound+1 {
			t.Errorf("injected head must count: TotalFound before=%d after=%d, want after=before+1",
				before.TotalFound, after.TotalFound)
		}
	}
}

// TestApplySupersession_ViewFutureIntermediateNeverNamed pins ViewFuture as
// its OWN guard (review pass 2, finding 2): with a BACKDATED deeper
// successor, ValidForView alone cannot protect the annotation. Chain
// A<-B<-C where B became valid after the caller's as-of instant and C was
// backdated before it: C is the view-valid head, and without ViewFuture the
// nearest-nameable rule would stamp view-future B into SupersededBy —
// leaking the view's future through metadata. Backdated retroactive facts
// are a designed use of explicit ValidFrom, so the shape is reachable.
func TestApplySupersession_ViewFutureIntermediateNeverNamed(t *testing.T) {
	h, cleanup := newSupersedeHarness(t)
	defer cleanup()

	past := time.Now().Add(-3 * time.Hour)
	backdated := time.Now().Add(-90 * time.Minute)
	aU, err := h.eng.store.WriteEngram(h.ctx, h.ws, &storage.Engram{
		Concept: "vf-a", Content: "v1", Confidence: 0.9, ValidFrom: past,
	})
	if err != nil {
		t.Fatalf("write a: %v", err)
	}
	b := h.write("vf-b", "v2") // ValidFrom defaults to now → future of AsOf
	cU, err := h.eng.store.WriteEngram(h.ctx, h.ws, &storage.Engram{
		Concept: "vf-c", Content: "v3 backdated", Confidence: 0.9, ValidFrom: backdated,
	})
	if err != nil {
		t.Fatalf("write c: %v", err)
	}
	a, c := aU.String(), cU.String()
	h.supersede(b, a)
	h.supersede(c, b)

	asOf := time.Now().Add(-1 * time.Hour)
	got, injected := h.applyReq(h.scored(a, 0.9), &activation.ActivateRequest{AsOf: &asOf})
	if rankOf(got, c) == -1 || injected != 1 {
		t.Fatalf("backdated head C must inject (rank=%d injected=%d)", rankOf(got, c), injected)
	}
	if rankOf(got, b) != -1 {
		t.Error("view-future B must not appear in results")
	}
	sb, cv := annotationOf(t, got, a)
	if sb == b || cv == b {
		t.Errorf("annotation names view-future B (SupersededBy=%s CurrentVersion=%s) — leaks the view's future", sb, cv)
	}
	if sb != c || cv != c {
		t.Errorf("annotation must name backdated head C (SupersededBy=%s CurrentVersion=%s)", sb, cv)
	}
}
