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
	out, _ := h.eng.applySupersession(h.ctx, h.ws, results, &activation.ActivateRequest{})
	return out
}

func (h *supersedeTestHarness) applyReq(results []activation.ScoredEngram, req *activation.ActivateRequest) ([]activation.ScoredEngram, int) {
	return h.eng.applySupersession(h.ctx, h.ws, results, req)
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
