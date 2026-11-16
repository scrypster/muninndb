package engine

import (
	"context"
	"fmt"
	"testing"

	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// COG-29 overflow under content-first order: a returned row's unresolved
// declared contradiction may pull in ITS PARTNER beyond `limit` — only the
// partner, never the rows ranked between them, at most one per returned row,
// never chained, and at most min(limit, contradictionOverflowMax) extra rows
// in total. A partner the cap keeps out is still NAMED on the row
// (partner_in_results=false). Fixtures are invented (trail maintenance).

func coWrite(t *testing.T, eng *Engine, vault, concept, content string) string {
	t.Helper()
	w, err := eng.Write(context.Background(), &mbp.WriteRequest{Vault: vault, Concept: concept, Content: content})
	if err != nil {
		t.Fatal(err)
	}
	return w.ID
}

func coContradicts(t *testing.T, eng *Engine, vault, src, dst string) {
	t.Helper()
	if _, err := eng.Link(context.Background(), &mbp.LinkRequest{Vault: vault, SourceID: src, TargetID: dst,
		RelType: uint16(storage.RelContradicts)}); err != nil {
		t.Fatal(err)
	}
}

func coRecall(t *testing.T, eng *Engine, vault, q string, limit int) *mbp.ActivateResponse {
	t.Helper()
	resp, err := eng.Activate(context.Background(), &mbp.ActivateRequest{Vault: vault, Context: []string{q},
		MaxResults: limit, Threshold: 0.001, ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// coAssertOnlyPartners: every row past `limit` must be the declared partner
// (`with`) of a row inside the cut, and the overflow must respect the cap.
func coAssertOnlyPartners(t *testing.T, resp *mbp.ActivateResponse, limit int) {
	t.Helper()
	rows := resp.Activations
	if len(rows) <= limit {
		return
	}
	partnersOfCut := map[string]bool{}
	for _, r := range rows[:limit] {
		if c := r.UnresolvedContradiction; c != nil {
			partnersOfCut[c.With] = true
		}
	}
	extra := len(rows) - limit
	cap := limit
	if cap > contradictionOverflowMax {
		cap = contradictionOverflowMax
	}
	if extra > cap {
		t.Errorf("limit=%d returned %d rows: overflow %d exceeds the cap %d", limit, len(rows), extra, cap)
	}
	for i, r := range rows[limit:] {
		if !partnersOfCut[r.ID] {
			t.Errorf("limit=%d: overflow row %d (%s, %q) is not the declared partner of any row inside the cut", limit, limit+i, r.ID, r.Concept)
		}
	}
	if resp.Conflict == nil || resp.Conflict.AdjacencyOverflow != extra {
		t.Errorf("limit=%d: %d rows beyond the limit but adjacency_overflow=%v", limit, extra, resp.Conflict)
	}
}

// TestContradictionOverflow_PartnerAtRankFive — a disputed top row whose
// partner is the 5th match: at limit 1 the response is the row and its
// partner, not the three unrelated rows ranked between them.
func TestContradictionOverflow_PartnerAtRankFive(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	const vault = "trail-overflow"
	top := coWrite(t, eng, vault, "culvert outlet riprap drainage", "culvert outlet riprap drainage at mile two")
	coWrite(t, eng, vault, "culvert outlet riprap", "the culvert outlet riprap was reset")
	coWrite(t, eng, vault, "outlet riprap drainage", "riprap drainage at the outlet upstream")
	coWrite(t, eng, vault, "culvert drainage riprap", "culvert drainage riprap after storms")
	partner := coWrite(t, eng, vault, "drainage notes", "drainage at the culvert is fine")
	for i := 0; i < 8; i++ {
		coWrite(t, eng, vault, fmt.Sprintf("brushing log %d", i), fmt.Sprintf("crew brushed the corridor on day %d", i))
	}
	coContradicts(t, eng, vault, partner, top)
	eng.waitWriteTimeIdle()

	all := coRecall(t, eng, vault, "culvert outlet riprap drainage", 20)
	if contradictionRank(all, top) != 0 || contradictionRank(all, partner) < 3 {
		t.Fatalf("premise: top row first and the partner well below it (top %d, partner %d)",
			contradictionRank(all, top), contradictionRank(all, partner))
	}
	resp := coRecall(t, eng, vault, "culvert outlet riprap drainage", 1)
	if len(resp.Activations) != 2 || resp.Activations[0].ID != top || resp.Activations[1].ID != partner {
		ids := []string{}
		for _, a := range resp.Activations {
			ids = append(ids, a.Concept)
		}
		t.Errorf("limit=1 on a disputed top row must return the row and its partner only, got %d rows %v", len(resp.Activations), ids)
	}
	coAssertOnlyPartners(t, resp, 1)
	if c := resp.Activations[0].UnresolvedContradiction; c == nil || !c.PartnerInResults {
		t.Errorf("top row must name its partner as in results: %+v", c)
	}
}

// TestContradictionOverflow_ChainOfTen — eleven matching memories linked by
// ten contradicts edges in a chain. An overflow partner never pulls in ITS
// partner, and the total overflow is capped.
func TestContradictionOverflow_ChainOfTen(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	const vault = "trail-chain"
	ids := make([]string, 11)
	for i := range ids {
		ids[i] = coWrite(t, eng, vault, fmt.Sprintf("waterbar spacing version %d", i), fmt.Sprintf("waterbar spacing on the ridge is %d feet", 20+i))
	}
	for i := 0; i < 10; i++ {
		coContradicts(t, eng, vault, ids[i+1], ids[i])
	}
	eng.waitWriteTimeIdle()
	for _, limit := range []int{1, 3} {
		resp := coRecall(t, eng, vault, "waterbar spacing on the ridge", limit)
		coAssertOnlyPartners(t, resp, limit)
		for _, r := range resp.Activations {
			if r.UnresolvedContradiction == nil {
				t.Errorf("limit=%d: row %q in a declared chain carries no conflict annotation", limit, r.Concept)
			}
		}
	}
}
