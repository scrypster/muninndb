package engine

import (
	"context"
	"testing"

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

func (h *supersedeTestHarness) apply(results []activation.ScoredEngram) []activation.ScoredEngram {
	return h.eng.applySupersession(h.ctx, h.ws, results)
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
