package cognitive

import (
	"context"
	"fmt"
	"math"
	"testing"
)

// ---------------------------------------------------------------------------
// Benchmark 1: JaccardSimilarity across set sizes and overlap ratios
// ---------------------------------------------------------------------------

func BenchmarkJaccardSimilarity(b *testing.B) {
	sizes := []int{10, 100, 1000}
	overlaps := []struct {
		name  string
		ratio float64
	}{
		{"0pct", 0.0},
		{"25pct", 0.25},
		{"50pct", 0.50},
		{"75pct", 0.75},
		{"100pct", 1.0},
	}

	for _, size := range sizes {
		for _, ov := range overlaps {
			name := fmt.Sprintf("size=%d/overlap=%s", size, ov.name)
			b.Run(name, func(b *testing.B) {
				setA, sliceB := buildJaccardInputs(size, ov.ratio)
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					JaccardSimilarity(setA, sliceB)
				}
			})
		}
	}
}

// buildJaccardInputs creates a set A of the given size and a slice B of the
// same size with the specified overlap ratio. Elements are deterministic
// strings so benchmarks are reproducible.
func buildJaccardInputs(size int, overlapRatio float64) (map[string]struct{}, []string) {
	shared := int(math.Round(float64(size) * overlapRatio))

	setA := make(map[string]struct{}, size)
	for i := 0; i < size; i++ {
		setA[fmt.Sprintf("a-%d", i)] = struct{}{}
	}

	sliceB := make([]string, 0, size)
	// Shared elements use the same keys as setA.
	for i := 0; i < shared; i++ {
		sliceB = append(sliceB, fmt.Sprintf("a-%d", i))
	}
	// Remaining elements are unique to B.
	for i := shared; i < size; i++ {
		sliceB = append(sliceB, fmt.Sprintf("b-%d", i))
	}
	return setA, sliceB
}

// ---------------------------------------------------------------------------
// Benchmark 2: ScoreSeparation with varying candidate counts
// ---------------------------------------------------------------------------

func BenchmarkScoreSeparation(b *testing.B) {
	candidateCounts := []int{10, 50, 100}
	entitiesPerEngram := 5

	for _, n := range candidateCounts {
		name := fmt.Sprintf("candidates=%d", n)
		b.Run(name, func(b *testing.B) {
			store, candidates := buildScoringInputs(n, entitiesPerEngram)
			scorer := NewSeparationScorer(store, SeparationConfig{
				RepulsionAlpha:    0.3,
				ContextMismatchFn: "entity",
			})
			queryEntities := []string{"q-0", "q-1", "q-2", "q-3", "q-4"}
			ws := [8]byte{}
			ctx := context.Background()

			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_, _ = scorer.ScoreSeparation(ctx, ws, queryEntities, candidates)
			}
		})
	}
}

// buildScoringInputs creates a mock store and candidate ID list for
// benchmarking ScoreSeparation.
func buildScoringInputs(n, entitiesPerEngram int) (*mockSeparationStore, [][16]byte) {
	entities := make(map[[16]byte][]string, n)
	candidates := make([][16]byte, n)
	for i := 0; i < n; i++ {
		var cid [16]byte
		cid[0] = byte(i >> 8)
		cid[1] = byte(i)
		candidates[i] = cid

		ents := make([]string, entitiesPerEngram)
		for j := 0; j < entitiesPerEngram; j++ {
			ents[j] = fmt.Sprintf("e-%d-%d", i, j)
		}
		entities[cid] = ents
	}
	return &mockSeparationStore{entities: entities}, candidates
}

// ---------------------------------------------------------------------------
// Benchmark 3: Quality metrics — verify cross-context penalty effectiveness
//
// NOTE: This tests the scoring layer (multiplier values), not end-to-end
// retrieval quality. The ratio assertions compare score multipliers produced
// by the SeparationScorer, which is a different metric than the retrieval-level
// avg(cross-context distance) / avg(within-context distance) described in
// issue #19. A retrieval-level A/B test requires a full engine with embeddings
// and is beyond unit-test scope.
// ---------------------------------------------------------------------------

func TestSeparationScorerQualityMetrics(t *testing.T) {
	// Two distinct entity clusters ("projects").
	projectA := []string{"postgres", "redis", "auth-service"}
	projectB := []string{"kafka", "spark", "data-pipeline"}

	// Engrams with deterministic IDs.
	// IDs 1-3: Project A only
	// IDs 4-6: Project B only
	// IDs 7-8: Mixed (one entity from each project)
	store := &mockSeparationStore{
		entities: map[[16]byte][]string{
			id(1): projectA,
			id(2): {"postgres", "redis"},
			id(3): {"auth-service", "redis"},
			id(4): projectB,
			id(5): {"kafka", "spark"},
			id(6): {"data-pipeline", "spark"},
			id(7): {"postgres", "kafka"},        // mixed
			id(8): {"auth-service", "spark"},     // mixed
		},
	}

	scorer := NewSeparationScorer(store, SeparationConfig{
		RepulsionAlpha:    0.3,
		ContextMismatchFn: "entity",
	})

	// Query with Project A context.
	queryEntities := projectA
	candidates := [][16]byte{id(1), id(2), id(3), id(4), id(5), id(6), id(7), id(8)}
	ws := [8]byte{}

	mults, err := scorer.ScoreSeparation(context.Background(), ws, queryEntities, candidates)
	if err != nil {
		t.Fatalf("ScoreSeparation error: %v", err)
	}

	// Categorise multipliers.
	sameContext := mults[0:3]   // ids 1-3: Project A
	crossContext := mults[3:6]  // ids 4-6: Project B
	mixed := mults[6:8]        // ids 7-8: mixed

	avgSame := avg(sameContext)
	avgCross := avg(crossContext)
	avgMixed := avg(mixed)

	t.Logf("Same-context multipliers:  %v  avg=%.4f", sameContext, avgSame)
	t.Logf("Cross-context multipliers: %v  avg=%.4f", crossContext, avgCross)
	t.Logf("Mixed multipliers:         %v  avg=%.4f", mixed, avgMixed)

	// Verify: same-context candidates get multiplier >= 0.9.
	for i, m := range sameContext {
		if m < 0.9 {
			t.Errorf("same-context candidate %d: multiplier %.4f < 0.9", i+1, m)
		}
	}

	// Verify: cross-context candidates get multiplier < 0.8.
	for i, m := range crossContext {
		if m >= 0.8 {
			t.Errorf("cross-context candidate %d: multiplier %.4f >= 0.8", i+4, m)
		}
	}

	// Verify: mixed candidates get intermediate multiplier (between avgCross and avgSame).
	for i, m := range mixed {
		if m <= avgCross || m >= avgSame {
			t.Errorf("mixed candidate %d: multiplier %.4f not between cross avg (%.4f) and same avg (%.4f)",
				i+7, m, avgCross, avgSame)
		}
	}

	// Compute separation ratio: avg(cross) / avg(same). Should be < 1.0.
	separationRatio := avgCross / avgSame
	t.Logf("Separation ratio (cross/same): %.4f", separationRatio)
	if separationRatio >= 1.0 {
		t.Errorf("separation ratio %.4f >= 1.0; cross-context should be penalised more", separationRatio)
	}
}

// ---------------------------------------------------------------------------
// Benchmark 4: Alpha effect — separation ratio scales with alpha
//
// This tests the scorer multiplier ratio, not retrieval precision/recall.
// End-to-end retrieval benchmarks require a live engine with embeddings.
// ---------------------------------------------------------------------------

func TestSeparationAlphaEffect(t *testing.T) {
	// Same two-project scenario.
	projectA := []string{"postgres", "redis", "auth-service"}
	projectB := []string{"kafka", "spark", "data-pipeline"}

	store := &mockSeparationStore{
		entities: map[[16]byte][]string{
			id(1): projectA,
			id(2): {"postgres", "redis"},
			id(3): {"auth-service", "redis"},
			id(4): projectB,
			id(5): {"kafka", "spark"},
			id(6): {"data-pipeline", "spark"},
		},
	}

	alphas := []float64{0.0, 0.1, 0.3, 0.5, 0.9}
	queryEntities := projectA
	candidates := [][16]byte{id(1), id(2), id(3), id(4), id(5), id(6)}
	ws := [8]byte{}

	var prevRatio float64
	for i, alpha := range alphas {
		scorer := NewSeparationScorer(store, SeparationConfig{
			RepulsionAlpha:    alpha,
			ContextMismatchFn: "entity",
		})

		mults, err := scorer.ScoreSeparation(context.Background(), ws, queryEntities, candidates)
		if err != nil {
			t.Fatalf("alpha=%.1f: ScoreSeparation error: %v", alpha, err)
		}

		sameContext := mults[0:3]
		crossContext := mults[3:6]

		avgSame := avg(sameContext)
		avgCross := avg(crossContext)

		ratio := avgCross / avgSame
		t.Logf("alpha=%.1f  avgSame=%.4f  avgCross=%.4f  ratio=%.4f", alpha, avgSame, avgCross, ratio)

		// alpha=0 should produce ratio=1.0 (no separation).
		if alpha == 0.0 {
			if math.Abs(ratio-1.0) > 1e-9 {
				t.Errorf("alpha=0: ratio=%.4f, want 1.0 (no separation)", ratio)
			}
		}

		// For alpha > 0, ratio should be < 1.0.
		if alpha > 0 && ratio >= 1.0 {
			t.Errorf("alpha=%.1f: ratio=%.4f >= 1.0; expected cross-context penalty", alpha, ratio)
		}

		// Ratio should decrease (or stay equal) as alpha increases.
		if i > 0 && ratio > prevRatio+1e-9 {
			t.Errorf("alpha=%.1f: ratio=%.4f > previous ratio=%.4f; separation should increase with alpha",
				alpha, ratio, prevRatio)
		}
		prevRatio = ratio
	}
}

// avg returns the arithmetic mean of a float64 slice.
func avg(vals []float64) float64 {
	if len(vals) == 0 {
		return 0
	}
	sum := 0.0
	for _, v := range vals {
		sum += v
	}
	return sum / float64(len(vals))
}
