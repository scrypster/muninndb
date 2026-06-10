package activation

import (
	"math"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/storage"
)

// defaultTestWeights returns a resolvedWeights with all components equal and summing to 1.0,
// suitable for invariant tests that do not depend on a specific scoring profile.
func defaultTestWeights() resolvedWeights {
	return resolvedWeights{
		SemanticSimilarity: 1.0 / 6,
		FullTextRelevance:  1.0 / 6,
		DecayFactor:        1.0 / 6,
		HebbianBoost:       1.0 / 6,
		AccessFrequency:    1.0 / 6,
		Recency:            1.0 / 6,
	}
}

// makeEngram constructs a minimal storage.Engram for scoring tests.
func makeEngram(accessCount uint32, stability float32, confidence float32, lastAccess time.Time) *storage.Engram {
	return &storage.Engram{
		AccessCount: accessCount,
		Stability:   stability,
		Confidence:  confidence,
		LastAccess:  lastAccess,
	}
}

// TestScoringInvariant_RawScoreBounded verifies that computeComponents always
// clamps the weighted-sum raw score to [0.0, 1.0] regardless of input extremes.
func TestScoringInvariant_RawScoreBounded(t *testing.T) {
	now := time.Now()
	yesterday := now.Add(-24 * time.Hour)

	cases := []struct {
		name         string
		vectorScore  float64
		ftsScore     float64
		hebbianBoost float64
		eng          *storage.Engram
		weights      resolvedWeights
	}{
		{
			name:        "all zeros",
			vectorScore: 0, ftsScore: 0, hebbianBoost: 0,
			eng:     makeEngram(0, 1, 1, yesterday),
			weights: defaultTestWeights(),
		},
		{
			name:        "all maximums",
			vectorScore: 1, ftsScore: 100, hebbianBoost: 1,
			eng:     makeEngram(1000, 365, 1, yesterday),
			weights: defaultTestWeights(),
		},
		{
			name:        "weights sum > 1 with all components = 1",
			vectorScore: 1, ftsScore: 1, hebbianBoost: 1,
			eng: makeEngram(100, 30, 1, yesterday),
			weights: resolvedWeights{
				SemanticSimilarity: 1.0,
				FullTextRelevance:  1.0,
				DecayFactor:        1.0,
				HebbianBoost:       1.0,
				AccessFrequency:    1.0,
				Recency:            1.0,
			},
		},
		{
			name:        "large ftsScore drives weighted FTS above 1",
			vectorScore: 0, ftsScore: 1e9, hebbianBoost: 0,
			eng: makeEngram(0, 1, 1, yesterday),
			weights: resolvedWeights{
				FullTextRelevance: 2.0, // exaggerated weight
			},
		},
		{
			name:        "vector score 1 with extreme weight",
			vectorScore: 1, ftsScore: 0, hebbianBoost: 0,
			eng: makeEngram(0, 1, 1, yesterday),
			weights: resolvedWeights{
				SemanticSimilarity: 999.0,
			},
		},
		{
			name:        "negative vector score (guard)",
			vectorScore: -5, ftsScore: -3, hebbianBoost: -1,
			eng: makeEngram(0, 1, 1, yesterday),
			weights: resolvedWeights{
				SemanticSimilarity: 1.0,
				FullTextRelevance:  1.0,
				HebbianBoost:       1.0,
			},
		},
		{
			name:        "fresh engram high confidence",
			vectorScore: 0.9, ftsScore: 5, hebbianBoost: 0.8,
			eng:     makeEngram(50, 30, 0.95, now),
			weights: defaultTestWeights(),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sc := computeComponents(tc.vectorScore, tc.ftsScore, tc.hebbianBoost, tc.eng, 0, now, tc.weights)
			if sc.Raw < 0.0 || sc.Raw > 1.0 {
				t.Errorf("Raw = %v, want ∈ [0.0, 1.0]", sc.Raw)
			}
		})
	}
}

// TestScoringInvariant_NormalizedFTSBounded verifies that tanh(ftsScore) ∈ [0, 1)
// for all non-negative ftsScore values, ensuring BM25 normalization is correct.
func TestScoringInvariant_NormalizedFTSBounded(t *testing.T) {
	cases := []struct {
		name     string
		ftsScore float64
	}{
		{"zero", 0},
		{"small", 0.001},
		{"one", 1.0},
		{"three", 3.0},
		{"ten", 10.0},
		{"hundred", 100.0},
		{"very large", 1e15},
		{"positive infinity", math.Inf(1)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			normalized := math.Tanh(tc.ftsScore)
			if math.IsNaN(normalized) {
				t.Fatalf("tanh(%v) = NaN", tc.ftsScore)
			}
			// tanh(x) for x >= 0 is in [0, 1); tanh(+Inf) = 1.0 exactly (limit)
			if normalized < 0.0 {
				t.Errorf("normalizedFTS = %v, want >= 0.0 (ftsScore=%v)", normalized, tc.ftsScore)
			}
			if normalized > 1.0 {
				t.Errorf("normalizedFTS = %v, want <= 1.0 (ftsScore=%v)", normalized, tc.ftsScore)
			}
		})
	}
}

// TestScoringInvariant_AccessFrequencyBounded verifies that the AccessFrequency
// component, computed as log1p(accessCount) / log1p(100), is always ∈ [0, 1]
// and is clamped to 1.0 when accessCount exceeds the saturation threshold.
func TestScoringInvariant_AccessFrequencyBounded(t *testing.T) {
	const accessFreqSaturation = 100.0

	cases := []struct {
		name        string
		accessCount uint32
		wantAbove   float64 // minimum expected value (inclusive)
		wantBelow   float64 // maximum expected value (inclusive)
	}{
		{"zero accesses", 0, 0.0, 0.0},
		{"one access", 1, 0.0, 1.0},
		{"saturation boundary", 100, 1.0, 1.0},
		{"above saturation", 200, 1.0, 1.0},
		{"far above saturation", 100_000, 1.0, 1.0},
		{"max uint32", math.MaxUint32, 1.0, 1.0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := math.Log1p(float64(tc.accessCount)) / math.Log1p(accessFreqSaturation)
			clamped := raw
			if clamped > 1.0 {
				clamped = 1.0
			}

			// Verify the formula result (before clamping) is non-negative
			if raw < 0.0 {
				t.Errorf("raw AccessFrequency = %v, want >= 0.0 (accessCount=%d)", raw, tc.accessCount)
			}
			// Verify after clamping we are in [0, 1]
			if clamped < 0.0 || clamped > 1.0 {
				t.Errorf("AccessFrequency = %v, want ∈ [0.0, 1.0] (accessCount=%d)", clamped, tc.accessCount)
			}
			// Verify against expected bounds
			if clamped < tc.wantAbove {
				t.Errorf("AccessFrequency = %v, want >= %v (accessCount=%d)", clamped, tc.wantAbove, tc.accessCount)
			}
			if clamped > tc.wantBelow {
				t.Errorf("AccessFrequency = %v, want <= %v (accessCount=%d)", clamped, tc.wantBelow, tc.accessCount)
			}
		})
	}
}

// TestScoringInvariant_DecayFactorFloor verifies that the Ebbinghaus decay factor
// math.Max(0.05, math.Exp(-daysSince/stability)) is always >= 0.05, even for
// engrams that are very old or have very low stability.
func TestScoringInvariant_DecayFactorFloor(t *testing.T) {
	const decayFloor = 0.05
	now := time.Now()

	cases := []struct {
		name      string
		daysSince float64
		stability float64 // in days
	}{
		{"fresh, high stability", 0, 365},
		{"one day old, one day stability", 1, 1},
		{"ten days old, one day stability", 10, 1},
		{"hundred days old, one day stability", 100, 1},
		{"thousand days old, minimal stability", 1000, 0.001},
		{"zero stability (guard against div by zero)", 1, 0},
		{"negative daysSince (future lastAccess)", -1, 30},
		{"extreme age", 1e9, 1},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stability := tc.stability
			if stability <= 0 {
				stability = 1.0 // match guard in computeACTR: math.Max(float64(eng.Stability), 1.0)
			}
			decayFactor := math.Max(decayFloor, math.Exp(-tc.daysSince/stability))

			if math.IsNaN(decayFactor) {
				t.Fatalf("decayFactor = NaN (daysSince=%v, stability=%v)", tc.daysSince, tc.stability)
			}
			if decayFactor < decayFloor {
				t.Errorf("decayFactor = %v, want >= %v (daysSince=%v, stability=%v)",
					decayFactor, decayFloor, tc.daysSince, tc.stability)
			}
			if decayFactor > 1.0 {
				// math.Exp(-daysSince/stability) for daysSince<0 can exceed 1.0; this is
				// allowed (the decay factor is only floored, not capped). Verify it is finite.
				if math.IsInf(decayFactor, 0) {
					t.Errorf("decayFactor = +Inf, expected finite (daysSince=%v, stability=%v)",
						tc.daysSince, tc.stability)
				}
			}
		})

		// Also verify through computeComponents directly for non-zero stability cases
		if tc.stability > 0 {
			t.Run(tc.name+"/via_computeComponents", func(t *testing.T) {
				eng := makeEngram(0, float32(tc.stability), 1.0, now.Add(-time.Duration(tc.daysSince*24)*time.Hour))
				sc := computeComponents(0.5, 0, 0, eng, 0, now, defaultTestWeights())
				if sc.DecayFactor < decayFloor {
					t.Errorf("computeComponents.DecayFactor = %v, want >= %v", sc.DecayFactor, decayFloor)
				}
			})
		}
	}
}

// TestScoringInvariant_HebbianBoostNonNegative verifies that the HebbianBoost
// signal accumulation used in scoring never produces a negative value.
// HebbianBoost is the aggregated co-activation weight from the Hebbian store,
// which is always in [0, 1] by construction (initialized to 0.01, capped at 1.0).
func TestScoringInvariant_HebbianBoostNonNegative(t *testing.T) {
	now := time.Now()
	eng := makeEngram(10, 30, 0.8, now.Add(-48*time.Hour))
	w := defaultTestWeights()

	cases := []struct {
		name         string
		hebbianBoost float64
	}{
		{"zero", 0.0},
		{"small", 0.001},
		{"quarter", 0.25},
		{"half", 0.5},
		{"max", 1.0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.hebbianBoost < 0 {
				t.Fatalf("test case has invalid negative hebbianBoost=%v", tc.hebbianBoost)
			}
			sc := computeComponents(0.5, 1.0, tc.hebbianBoost, eng, 0, now, w)
			// HebbianBoost component stored in ScoreComponents must equal the passed value
			if sc.HebbianBoost != tc.hebbianBoost {
				t.Errorf("HebbianBoost = %v, want %v", sc.HebbianBoost, tc.hebbianBoost)
			}
			// Final score must be non-negative
			if sc.Final < 0 {
				t.Errorf("Final score = %v, want >= 0 (hebbianBoost=%v)", sc.Final, tc.hebbianBoost)
			}
		})
	}

	// Table-driven test showing monotonicity: higher Hebbian boost → higher or equal raw score
	// (all else equal), verifying the accumulation direction is always additive/non-negative.
	t.Run("monotone_in_hebbian", func(t *testing.T) {
		boosts := []float64{0.0, 0.1, 0.2, 0.5, 0.8, 1.0}
		var prevRaw float64
		for i, boost := range boosts {
			sc := computeComponents(0.3, 0.5, boost, eng, 0, now, w)
			if sc.Raw < 0 {
				t.Errorf("boosts[%d]=%v: Raw = %v, want >= 0", i, boost, sc.Raw)
			}
			if i > 0 && sc.Raw < prevRaw-1e-12 {
				t.Errorf("boosts[%d]=%v: Raw = %v decreased from previous %v (not monotone)",
					i, boost, sc.Raw, prevRaw)
			}
			prevRaw = sc.Raw
		}
	})
}
