package activation

import (
	"context"
	"math/rand"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/storage"
)

// ---------------------------------------------------------------------------
// TestPhase6Score_TieBreakDeterministic (#698)
//
// sort.Slice is NOT a stable sort, and until this fix the final-ordering
// comparators in phase6Score compared ONLY on `final` score with no secondary
// key. When many candidates share the same final score -- routine under
// weighted-sum blend-cap saturation (0.400) and in cold vaults where every
// candidate has zero access history -- the order among tied candidates (and
// therefore which ones survive a small MaxResults cutoff) tracked the input
// slice order rather than being a deterministic function of the candidate
// set itself.
//
// This test builds a candidate set where every candidate scores identically
// under all four scoring paths (RRF, CGDN, ACT-R, legacy weighted-sum), feeds
// phase6Score the SAME set in many different (shuffled) input orders, and
// asserts the surviving top-N ordering is always identical -- and equal to
// ascending-ULID order, per the documented tie-break rule.
//
// RED (pre-fix): output order tracks input order, so different shuffles of
// the same tied candidate set produce different top-N results/orderings.
// GREEN (post-fix): output order is independent of input order.
// ---------------------------------------------------------------------------

func buildTiedCandidates(t *testing.T, store *internalStubStore, n int) []fusedCandidate {
	t.Helper()
	created := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	fused := make([]fusedCandidate, 0, n)
	for i := 0; i < n; i++ {
		eng := &storage.Engram{
			Concept:     "tied engram",
			Content:     "identical content for every tied candidate",
			Confidence:  1.0,
			Stability:   30.0,
			Relevance:   0.8,
			State:       storage.StateActive,
			CreatedAt:   created,
			LastAccess:  created,
			AccessCount: 3,
		}
		// Force a fresh random ID (not time-derived) so ties aren't
		// accidentally broken by monotonic ULID generation order.
		eng.ID = storage.NewULID()
		store.addEngram(eng)
		fused = append(fused, fusedCandidate{
			id:          eng.ID,
			rrfScore:    0.5,
			ftsScore:    1.0,
			vectorScore: 0.8,
		})
	}
	return fused
}

func shuffledCopy(seed int64, in []fusedCandidate) []fusedCandidate {
	out := make([]fusedCandidate, len(in))
	copy(out, in)
	r := rand.New(rand.NewSource(seed))
	r.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
	return out
}

func ascendingIDOrder(fused []fusedCandidate) []storage.ULID {
	ids := make([]storage.ULID, len(fused))
	for i, c := range fused {
		ids[i] = c.id
	}
	// simple insertion sort ascending by ULID bytes
	for i := 1; i < len(ids); i++ {
		for j := i; j > 0 && ulidLess(ids[j], ids[j-1]); j-- {
			ids[j], ids[j-1] = ids[j-1], ids[j]
		}
	}
	return ids
}

func ulidLess(a, b storage.ULID) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}

func runPhase6ScoreTieBreak(t *testing.T, weights *Weights) {
	t.Helper()

	const n = 12
	const maxResults = 5
	const runs = 50

	store := newInternalStubStore()
	e := newTestActivationEngine(store)
	defer e.Close()

	base := buildTiedCandidates(t, store, n)
	wantOrder := ascendingIDOrder(base)[:maxResults]

	var firstOrder []storage.ULID
	for run := 0; run < runs; run++ {
		fused := shuffledCopy(int64(run), base)
		p1 := &phase1Result{queryStr: "test"}
		result, err := e.phase6Score(context.Background(), &ActivateRequest{
			MaxResults: maxResults,
			Threshold:  0.0,
			Weights:    weights,
		}, [8]byte{}, fused, nil, p1)
		if err != nil {
			t.Fatalf("run %d: phase6Score: %v", run, err)
		}
		if len(result.Activations) != maxResults {
			t.Fatalf("run %d: got %d activations, want %d", run, len(result.Activations), maxResults)
		}
		gotOrder := make([]storage.ULID, len(result.Activations))
		for i, a := range result.Activations {
			gotOrder[i] = a.Engram.ID
		}

		if run == 0 {
			firstOrder = gotOrder
			continue
		}
		if !ulidSliceEqual(gotOrder, firstOrder) {
			t.Fatalf("run %d: top-%d order changed across input shuffles (nondeterministic tie-break)\n  run 0: %v\n  run %d: %v",
				run, maxResults, ulidStrings(firstOrder), run, ulidStrings(gotOrder))
		}
	}

	if !ulidSliceEqual(firstOrder, wantOrder) {
		t.Fatalf("stable order did not match ascending-ULID tie-break rule\n  got:  %v\n  want: %v",
			ulidStrings(firstOrder), ulidStrings(wantOrder))
	}
}

func ulidSliceEqual(a, b []storage.ULID) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func ulidStrings(ids []storage.ULID) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = id.String()
	}
	return out
}

func TestPhase6Score_TieBreakDeterministic_Legacy(t *testing.T) {
	runPhase6ScoreTieBreak(t, &Weights{
		DisableACTR:        true,
		SemanticSimilarity: 0.35,
		FullTextRelevance:  0.25,
		DecayFactor:        0.20,
		HebbianBoost:       0.10,
		AccessFrequency:    0.05,
		Recency:            0.05,
	})
}

func TestPhase6Score_TieBreakDeterministic_ACTR(t *testing.T) {
	runPhase6ScoreTieBreak(t, &Weights{UseACTR: true})
}

func TestPhase6Score_TieBreakDeterministic_CGDN(t *testing.T) {
	runPhase6ScoreTieBreak(t, &Weights{
		UseCGDN:            true,
		SemanticSimilarity: 0.5,
		FullTextRelevance:  0.3,
	})
}

func TestPhase6Score_TieBreakDeterministic_RRF(t *testing.T) {
	runPhase6ScoreTieBreak(t, &Weights{UseRRFFusion: true})
}
