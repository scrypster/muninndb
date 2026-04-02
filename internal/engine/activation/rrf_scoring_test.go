package activation

import (
	"math"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/storage"
)

// ---------------------------------------------------------------------------
// Tests for RRF scoring fusion — the alternative to ACT-R/CGDN/weighted-sum
// in Phase 6. These tests verify the computeRRFScore function and its
// integration into the phase6Score scoring path.
// ---------------------------------------------------------------------------

// TestRRFScore_CorrectForKnownRankings verifies the RRF formula produces
// correct scores for known input rankings.
// RRF score = Σ 1/(k + rank_i(d)), ranks are 1-indexed from phase3RRF.
func TestRRFScore_CorrectForKnownRankings(t *testing.T) {
	now := time.Now()
	eng := makeEngram(10, 30, 1.0, now.Add(-1*time.Hour))

	// A candidate ranked #1 in FTS (k=60) and #1 in HNSW (k=40):
	// RRF = 1/(60+1) + 1/(40+1) = 1/61 + 1/41 ≈ 0.01639 + 0.02439 ≈ 0.04079
	candidate := fusedCandidate{
		rrfScore:    1.0/(rrfK_FTS+1) + 1.0/(rrfK_HNSW+1),
		ftsScore:    5.0,
		vectorScore: 0.8,
	}

	score := computeRRFScore(candidate.rrfScore, candidate.hebbianBoost, candidate.transitionBoost, eng)
	// Score should equal rrfScore * confidence
	expected := candidate.rrfScore * float64(eng.Confidence)
	if math.Abs(score-expected) > 1e-9 {
		t.Errorf("RRF score = %v, want %v", score, expected)
	}
}

// TestRRFScore_ScaleInvariant verifies that RRF scoring is scale-invariant:
// documents with the same ranks but different raw score magnitudes get the
// same RRF score, since RRF depends only on rank position.
func TestRRFScore_ScaleInvariant(t *testing.T) {
	// Two candidate sets with different score magnitudes but same rank ordering.
	// After phase3RRF, the rrfScore depends only on rank position.
	setsSmall := &candidateSets{
		fts: []ScoredID{
			{ID: storage.ULID{1}, Score: 0.5},
			{ID: storage.ULID{2}, Score: 0.3},
		},
		vector: []ScoredID{
			{ID: storage.ULID{1}, Score: 0.4},
			{ID: storage.ULID{2}, Score: 0.2},
		},
	}
	setsLarge := &candidateSets{
		fts: []ScoredID{
			{ID: storage.ULID{1}, Score: 500.0},
			{ID: storage.ULID{2}, Score: 300.0},
		},
		vector: []ScoredID{
			{ID: storage.ULID{1}, Score: 0.99},
			{ID: storage.ULID{2}, Score: 0.88},
		},
	}

	fusedSmall := phase3RRF(setsSmall)
	fusedLarge := phase3RRF(setsLarge)

	// Both should produce the same RRF scores since rank order is identical.
	if len(fusedSmall) != len(fusedLarge) {
		t.Fatalf("fused lengths differ: %d vs %d", len(fusedSmall), len(fusedLarge))
	}

	// Find the candidate with ID {1} in each result.
	var rrfSmall, rrfLarge float64
	for _, c := range fusedSmall {
		if c.id == (storage.ULID{1}) {
			rrfSmall = c.rrfScore
		}
	}
	for _, c := range fusedLarge {
		if c.id == (storage.ULID{1}) {
			rrfLarge = c.rrfScore
		}
	}

	if math.Abs(rrfSmall-rrfLarge) > 1e-12 {
		t.Errorf("RRF not scale-invariant: small=%v large=%v", rrfSmall, rrfLarge)
	}
}

// TestRRFScore_MultiSignalHigherThanSingle verifies that a document appearing
// in multiple retrieval signals scores higher than one in only a single signal.
func TestRRFScore_MultiSignalHigherThanSingle(t *testing.T) {
	multiID := storage.ULID{1}
	singleID := storage.ULID{2}

	sets := &candidateSets{
		fts: []ScoredID{
			{ID: multiID, Score: 3.0},
			{ID: singleID, Score: 5.0}, // higher raw score, but only in FTS
		},
		vector: []ScoredID{
			{ID: multiID, Score: 0.9},
		},
	}

	fused := phase3RRF(sets)

	var multiRRF, singleRRF float64
	for _, c := range fused {
		if c.id == multiID {
			multiRRF = c.rrfScore
		}
		if c.id == singleID {
			singleRRF = c.rrfScore
		}
	}

	if multiRRF <= singleRRF {
		t.Errorf("multi-signal candidate RRF (%v) should be > single-signal (%v)", multiRRF, singleRRF)
	}
}

// TestRRFScore_K60CorrectValues verifies the RRF formula with the standard
// k=60 constant produces known expected values.
func TestRRFScore_K60CorrectValues(t *testing.T) {
	// With k=60 (rrfK_FTS), rank 1 → 1/(60+1) = 1/61
	// rank 2 → 1/(60+2) = 1/62
	// rank 3 → 1/(60+3) = 1/63
	sets := &candidateSets{
		fts: []ScoredID{
			{ID: storage.ULID{1}, Score: 10},
			{ID: storage.ULID{2}, Score: 5},
			{ID: storage.ULID{3}, Score: 1},
		},
	}

	fused := phase3RRF(sets)

	expected := map[storage.ULID]float64{
		{1}: 1.0 / 61.0,
		{2}: 1.0 / 62.0,
		{3}: 1.0 / 63.0,
	}

	for _, c := range fused {
		exp, ok := expected[c.id]
		if !ok {
			continue
		}
		if math.Abs(c.rrfScore-exp) > 1e-12 {
			t.Errorf("ID %v: RRF score = %v, want %v", c.id, c.rrfScore, exp)
		}
	}
}

// TestRRFScore_MissingSignalContributesZero verifies that a document not present
// in a signal's result list contributes 0 to RRF from that signal.
func TestRRFScore_MissingSignalContributesZero(t *testing.T) {
	// docA only in FTS, not in HNSW.
	// Its RRF should equal only the FTS contribution.
	docA := storage.ULID{1}
	docB := storage.ULID{2}

	sets := &candidateSets{
		fts: []ScoredID{
			{ID: docA, Score: 5.0},
		},
		vector: []ScoredID{
			{ID: docB, Score: 0.9},
		},
	}

	fused := phase3RRF(sets)

	var docARRF float64
	for _, c := range fused {
		if c.id == docA {
			docARRF = c.rrfScore
		}
	}

	// docA should only have FTS contribution: 1/(60+1)
	expectedFTS := 1.0 / (rrfK_FTS + 1.0)
	if math.Abs(docARRF-expectedFTS) > 1e-12 {
		t.Errorf("docA RRF = %v, want %v (FTS-only, no HNSW contribution)", docARRF, expectedFTS)
	}
}

// TestRRFScore_DefaultConfigUsesACTR verifies that the default configuration
// (no scoring_fusion override) still uses ACT-R scoring, not RRF.
// This ensures backward compatibility.
func TestRRFScore_DefaultConfigUsesACTR(t *testing.T) {
	w := resolveWeights(nil, DefaultWeights{
		SemanticSimilarity: 0.35,
		FullTextRelevance:  0.25,
		DecayFactor:        0.20,
		HebbianBoost:       0.10,
		AccessFrequency:    0.05,
		Recency:            0.05,
	})

	// Default should use ACT-R, not RRF fusion scoring
	if !w.UseACTR {
		t.Error("default config should use ACT-R scoring")
	}
	if w.UseRRFFusion {
		t.Error("default config should NOT use RRF fusion scoring")
	}
}

// TestRRFScore_HebbianBoostApplied verifies that Hebbian boost is applied
// after RRF fusion when using RRF scoring mode.
func TestRRFScore_HebbianBoostApplied(t *testing.T) {
	now := time.Now()
	eng := makeEngram(10, 30, 1.0, now.Add(-1*time.Hour))

	candidate := fusedCandidate{
		rrfScore:     0.04, // typical RRF score
		hebbianBoost: 0.8,
	}
	candidateNoHeb := fusedCandidate{
		rrfScore:     0.04,
		hebbianBoost: 0.0,
	}

	scoreWithHeb := computeRRFScore(candidate.rrfScore, candidate.hebbianBoost, candidate.transitionBoost, eng)
	scoreNoHeb := computeRRFScore(candidateNoHeb.rrfScore, candidateNoHeb.hebbianBoost, candidateNoHeb.transitionBoost, eng)

	if scoreWithHeb <= scoreNoHeb {
		t.Errorf("Hebbian boost should increase RRF score: with=%v without=%v", scoreWithHeb, scoreNoHeb)
	}
}

// TestRRFScore_ConfidenceModulates verifies that confidence modulates the
// final RRF score.
func TestRRFScore_ConfidenceModulates(t *testing.T) {
	now := time.Now()
	highConf := makeEngram(10, 30, 1.0, now.Add(-1*time.Hour))
	lowConf := makeEngram(10, 30, 0.3, now.Add(-1*time.Hour))

	candidate := fusedCandidate{
		rrfScore: 0.04,
	}

	scoreHigh := computeRRFScore(candidate.rrfScore, candidate.hebbianBoost, candidate.transitionBoost, highConf)
	scoreLow := computeRRFScore(candidate.rrfScore, candidate.hebbianBoost, candidate.transitionBoost, lowConf)

	if scoreHigh <= scoreLow {
		t.Errorf("higher confidence should give higher score: high=%v low=%v", scoreHigh, scoreLow)
	}
}

// TestRRFScore_TransitionBoostApplied verifies that transition boost from PAS
// is applied in RRF scoring mode.
func TestRRFScore_TransitionBoostApplied(t *testing.T) {
	now := time.Now()
	eng := makeEngram(10, 30, 1.0, now.Add(-1*time.Hour))

	withTrans := fusedCandidate{
		rrfScore:        0.04,
		transitionBoost: 0.5,
	}
	noTrans := fusedCandidate{
		rrfScore:        0.04,
		transitionBoost: 0.0,
	}

	scoreWith := computeRRFScore(withTrans.rrfScore, withTrans.hebbianBoost, withTrans.transitionBoost, eng)
	scoreNo := computeRRFScore(noTrans.rrfScore, noTrans.hebbianBoost, noTrans.transitionBoost, eng)

	if scoreWith <= scoreNo {
		t.Errorf("transition boost should increase RRF score: with=%v without=%v", scoreWith, scoreNo)
	}
}

// TestRRFScore_NonNegative verifies that RRF scores are always non-negative.
func TestRRFScore_NonNegative(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name string
		eng  *storage.Engram
		cand fusedCandidate
	}{
		{
			"zero everything",
			makeEngram(0, 1, 0, now),
			fusedCandidate{rrfScore: 0},
		},
		{
			"zero rrfScore positive confidence",
			makeEngram(0, 1, 1.0, now),
			fusedCandidate{rrfScore: 0},
		},
		{
			"normal values",
			makeEngram(10, 30, 0.8, now.Add(-48*time.Hour)),
			fusedCandidate{rrfScore: 0.04, hebbianBoost: 0.5, transitionBoost: 0.3},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			score := computeRRFScore(tc.cand.rrfScore, tc.cand.hebbianBoost, tc.cand.transitionBoost, tc.eng)
			if score < 0 {
				t.Errorf("RRF score = %v, want >= 0", score)
			}
		})
	}
}

// TestResolveWeights_UseRRFFusion verifies that the UseRRFFusion flag is
// correctly propagated through resolveWeights when set on the Weights struct.
func TestResolveWeights_UseRRFFusion(t *testing.T) {
	t.Run("explicit_rrf", func(t *testing.T) {
		w := resolveWeights(&Weights{
			UseRRFFusion: true,
			DisableACTR:  true,
		}, DefaultWeights{})
		if !w.UseRRFFusion {
			t.Error("expected UseRRFFusion=true")
		}
		if w.UseACTR {
			t.Error("expected UseACTR=false when DisableACTR=true")
		}
	})

	t.Run("default_no_rrf", func(t *testing.T) {
		w := resolveWeights(nil, DefaultWeights{})
		if w.UseRRFFusion {
			t.Error("default config should not use RRF fusion")
		}
	})

	t.Run("rrf_and_cgdn_both_set", func(t *testing.T) {
		w := resolveWeights(&Weights{
			UseRRFFusion: true,
			UseCGDN:      true,
			DisableACTR:  true,
		}, DefaultWeights{})
		// resolveWeights propagates both flags; the guard in phase6Score
		// clears UseCGDN at runtime. Verify both are set here (the guard
		// is tested in the integration test TestRRF_CGDNConflict_RRFTakesPrecedence).
		if !w.UseRRFFusion {
			t.Error("expected UseRRFFusion=true")
		}
		if !w.UseCGDN {
			t.Error("expected UseCGDN=true from resolveWeights (guard is in phase6Score)")
		}
	})
}
