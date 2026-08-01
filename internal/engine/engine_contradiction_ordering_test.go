package engine

import (
	"context"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/engine/activation"
	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// contradictionRows writes concept/content pairs into a vault and returns them
// as a score-descending synthetic result set, so a COG-29 ordering property can
// be pinned on an EXACT score shape rather than on whatever a no-op embedder
// happens to produce. Everything below the score assignment is real: real
// engrams, real 0x03/0x04 edges, the real phase.
func contradictionRows(t *testing.T, eng *Engine, vault string, specs []struct {
	concept, content string
	score            float64
}) ([]activation.ScoredEngram, []string) {
	t.Helper()
	ctx := context.Background()
	ids := make([]storage.ULID, 0, len(specs))
	strIDs := make([]string, 0, len(specs))
	for _, s := range specs {
		w, err := eng.Write(ctx, &mbp.WriteRequest{Vault: vault, Concept: s.concept, Content: s.content})
		if err != nil {
			t.Fatal(err)
		}
		id, err := storage.ParseULID(w.ID)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
		strIDs = append(strIDs, w.ID)
	}
	eng.waitWriteTimeIdle()

	ws := eng.store.ResolveVaultPrefix(vault)
	engrams, err := eng.store.GetEngrams(ctx, ws, ids)
	if err != nil {
		t.Fatal(err)
	}
	rows := make([]activation.ScoredEngram, 0, len(specs))
	for i, eg := range engrams {
		if eg == nil {
			t.Fatalf("engram %d missing", i)
		}
		rows = append(rows, activation.ScoredEngram{Engram: eg, Score: specs[i].score})
	}
	return rows, strIDs
}

func linkContradicts(t *testing.T, eng *Engine, vault, src, dst string) {
	t.Helper()
	if _, err := eng.Link(context.Background(), &mbp.LinkRequest{Vault: vault,
		SourceID: src, TargetID: dst, RelType: uint16(storage.RelContradicts)}); err != nil {
		t.Fatal(err)
	}
	eng.waitWriteTimeIdle()
}

// TestContradictionHonesty_DominantAnswerStaysRankZero is the RED for the
// gather-to-lowest adjacency rule.
//
// Corpus shape: a DOMINANT conflicted row (0.8587) whose declared partner is a
// weak straggler at the bottom of the response (0.0183 — 47x lower), with
// ordinary unrelated rows in between. Gathering the cluster down to the rank of
// its lowest member moved the best answer from rank 0 to rank 6, behind five
// rows it outscores by 2-8x, and left the response NOT score-descending. That
// is a worse lie than the one the phase exists to fix: an agent reading the
// first result now gets a 0.5 row presented as the answer over a 0.77 row.
//
// The rule that holds instead: demote, then RE-SORT by score, stably. Adjacency
// is whatever the scores produce.
func TestContradictionHonesty_DominantAnswerStaysRankZero(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()

	rows, ids := contradictionRows(t, eng, "dom", []struct {
		concept, content string
		score            float64
	}{
		{"request timeout limit", "the request timeout limit is 180ms", 0.8587},
		{"deploy runbook", "the deploy runbook lives in ops/deploy.md", 0.5000},
		{"oncall rotation", "oncall rotates weekly on tuesday", 0.4000},
		{"retry budget", "the retry budget is three attempts", 0.3000},
		{"backoff policy", "backoff is exponential with jitter", 0.2000},
		{"log retention", "logs are retained for thirty days", 0.1000},
		{"timeout aside", "an unrelated note that mentions timeout once", 0.0183},
	})
	// The dominant row (index 0) and the weak straggler (index 6) are the pair.
	linkContradicts(t, eng, "dom", ids[6], ids[0])

	req := &activation.ActivateRequest{MaxResults: 10}
	now := time.Now()
	out, block, _ := eng.applyContradictionHonesty(context.Background(),
		eng.store.ResolveVaultPrefix("dom"), rows, req, newVisibilityGate(req, now), now)

	if block == nil {
		t.Fatal("precondition: the declared conflict must be reported")
	}
	if len(out) != len(rows) {
		t.Fatalf("row count changed: %d -> %d", len(rows), len(out))
	}

	// (1) The dominant answer keeps rank 0 — demoted and annotated, not buried.
	if got := out[0].Engram.ID.String(); got != ids[0] {
		var order []string
		for i := range out {
			order = append(order, out[i].Engram.ID.String())
		}
		t.Errorf("rank 0 is %s, want the dominant conflicted row %s; order = %v", got, ids[0], order)
	}
	if out[0].UnresolvedContradiction == nil {
		t.Error("the top row is disputed and carries no annotation")
	}
	if want := 0.8587 * (1 - contradictionDemote); out[0].Score >= 0.8587 || out[0].Score != want {
		t.Errorf("top row score = %.6f, want the demoted %.6f", out[0].Score, want)
	}

	// (2) The response is strictly score-descending. A recall response that is
	// not sorted by its own scores is uninterpretable to any caller.
	for i := 1; i < len(out); i++ {
		if out[i].Score > out[i-1].Score {
			t.Errorf("response is not score-descending at rank %d: %.6f > %.6f",
				i, out[i].Score, out[i-1].Score)
		}
	}

	// (3) Demote-only still holds: no unrelated row moved down, and no row rose.
	for i := 1; i <= 5; i++ {
		if out[i].Engram.ID.String() != ids[i] {
			t.Errorf("rank %d is %s, want the untouched unrelated row %s",
				i, out[i].Engram.ID.String(), ids[i])
		}
	}
}

// TestContradictionHonesty_AdjacencyOverflowUsesPostSortRanks pins the trap the
// re-sort introduces: keepAtLeast and adjacency_overflow are computed from
// cluster member POSITIONS, and the demote itself can push a member below rows
// that used to score under it, so those positions MOVE. Computing them against
// the pre-sort ranks reports an overflow that does not describe the response
// the caller actually receives — and cuts the conflict in half while claiming
// it did not.
//
// Corpus: a dominant conflicted row (0.90) and a marginal partner (0.50) that
// the 10% demote drops below two unrelated rows at 0.48 and 0.47.
func TestContradictionHonesty_AdjacencyOverflowUsesPostSortRanks(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()

	rows, ids := contradictionRows(t, eng, "ovf", []struct {
		concept, content string
		score            float64
	}{
		{"request timeout limit", "the request timeout limit is 180ms", 0.90},
		{"request timeout limit revised", "the request timeout limit is 320ms", 0.50},
		{"deploy runbook", "the deploy runbook lives in ops/deploy.md", 0.48},
		{"oncall rotation", "oncall rotates weekly on tuesday", 0.47},
	})
	linkContradicts(t, eng, "ovf", ids[1], ids[0])

	req := &activation.ActivateRequest{MaxResults: 1}
	now := time.Now()
	out, block, keep := eng.applyContradictionHonesty(context.Background(),
		eng.store.ResolveVaultPrefix("ovf"), rows, req, newVisibilityGate(req, now), now)
	if block == nil {
		t.Fatal("precondition: the declared conflict must be reported")
	}

	rank := func(id string) int {
		for i := range out {
			if out[i].Engram.ID.String() == id {
				return i
			}
		}
		t.Fatalf("row %s missing from the response", id)
		return -1
	}
	a, b := rank(ids[0]), rank(ids[1])
	if a != 0 || b != 3 {
		t.Fatalf("precondition: want the demoted partner pushed to rank 3, got a=%d b=%d", a, b)
	}
	if keep != 4 {
		t.Errorf("keepAtLeast = %d, want 4 — the partner's POST-SORT rank + 1. Computed against pre-sort ranks this is 2, and the caller then receives one side of a conflict alone", keep)
	}
	if block.AdjacencyOverflow != keep-req.MaxResults {
		t.Errorf("adjacency_overflow = %d, want %d", block.AdjacencyOverflow, keep-req.MaxResults)
	}
}

// TestContradictionHonesty_PartnerChoiceIsDeterministic is the RED for the
// map-ordered partner pick. A row that is an endpoint of TWO declared
// contradicts edges got its `with` and `partner_in_results` from whichever edge
// came first in the collected slice — and that slice is built by ranging the
// map GetAssociations returns, so two identical queries against an unchanged
// vault disagreed about which memory disputes the answer.
//
// Selection must be total: prefer a partner that is itself in the result set,
// then the lowest partner ULID.
func TestContradictionHonesty_PartnerChoiceIsDeterministic(t *testing.T) {
	t.Run("two in-set rivals: lowest ULID wins", func(t *testing.T) {
		eng, cleanup := testEnv(t)
		defer cleanup()

		rows, ids := contradictionRows(t, eng, "twoin", []struct {
			concept, content string
			score            float64
		}{
			{"request timeout limit", "the request timeout limit is 180ms", 0.80},
			{"request timeout limit revised", "the request timeout limit is 320ms", 0.60},
			{"request timeout limit restated", "the request timeout limit is 90ms", 0.55},
		})
		// Both rivals are the SOURCE of their edge, so both land in the forward
		// association map that collectContradictionEdges ranges.
		linkContradicts(t, eng, "twoin", ids[1], ids[0])
		linkContradicts(t, eng, "twoin", ids[2], ids[0])

		// ULIDs are monotonic, so ids[1] was written first and sorts lowest.
		want := ids[1]
		if ids[2] < want {
			want = ids[2]
		}

		ws := eng.store.ResolveVaultPrefix("twoin")
		seen := map[string]int{}
		const runs = 40
		for i := 0; i < runs; i++ {
			req := &activation.ActivateRequest{MaxResults: 10}
			now := time.Now()
			in := append([]activation.ScoredEngram(nil), rows...)
			out, block, _ := eng.applyContradictionHonesty(context.Background(), ws, in, req,
				newVisibilityGate(req, now), now)
			if block == nil {
				t.Fatal("precondition: the declared conflicts must be reported")
			}
			var c *activation.ContradictionConflict
			for j := range out {
				if out[j].Engram.ID.String() == ids[0] {
					c = out[j].UnresolvedContradiction
				}
			}
			if c == nil {
				t.Fatal("the subject row carries no annotation")
			}
			seen[c.With.String()]++
			if !c.PartnerInResults {
				t.Errorf("run %d: partner_in_results=false, but both rivals are in the result set", i)
			}
		}
		if len(seen) != 1 {
			t.Errorf("partner choice is nondeterministic across %d identical calls: %v", runs, seen)
		}
		if seen[want] != runs {
			t.Errorf("annotation names %v; the total rule is lowest partner ULID = %s", seen, want)
		}
	})

	t.Run("an in-set rival beats an out-of-set one", func(t *testing.T) {
		eng, cleanup := testEnv(t)
		defer cleanup()

		rows, ids := contradictionRows(t, eng, "mixed", []struct {
			concept, content string
			score            float64
		}{
			{"request timeout limit", "the request timeout limit is 180ms", 0.80},
			{"request timeout limit revised", "the request timeout limit is 320ms", 0.60},
			{"lunar chronology", "the first crewed lunar landing happened in 1969", 0.40},
		})
		linkContradicts(t, eng, "mixed", ids[1], ids[0])
		linkContradicts(t, eng, "mixed", ids[2], ids[0])

		// Only the first two rows are candidates: ids[2] is a live, nameable
		// partner that did not match the query.
		req := &activation.ActivateRequest{MaxResults: 10}
		now := time.Now()
		out, block, _ := eng.applyContradictionHonesty(context.Background(),
			eng.store.ResolveVaultPrefix("mixed"), rows[:2], req, newVisibilityGate(req, now), now)
		if block == nil {
			t.Fatal("precondition: the declared conflicts must be reported")
		}
		var c *activation.ContradictionConflict
		for j := range out {
			if out[j].Engram.ID.String() == ids[0] {
				c = out[j].UnresolvedContradiction
			}
		}
		if c == nil {
			t.Fatal("the subject row carries no annotation")
		}
		if c.With.String() != ids[1] || !c.PartnerInResults {
			t.Errorf("annotation names %s (partner_in_results=%v); the in-set rival %s must win",
				c.With, c.PartnerInResults, ids[1])
		}
	})
}
