package activation

import (
	"math"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/storage"
)

func actrDefaultWeights() resolvedWeights {
	return resolvedWeights{
		SemanticSimilarity: 0.35,
		FullTextRelevance:  0.25,
		UseACTR:            true,
		ACTRDecay:          0.5,
		ACTRHebScale:       4.0,
	}
}

func assertNear(t *testing.T, name string, got, want, tol float64) {
	t.Helper()
	if math.Abs(got-want) > tol {
		t.Errorf("%s = %.10f, want %.10f (±%v, delta=%.2e)", name, got, want, tol, math.Abs(got-want))
	}
}

// expectedACTRRaw computes the expected Raw score from first principles.
// Note: no upper clamp — computeACTR now returns the unclamped value so that
// the ACT-R scoring path can apply per-query normalization (issue #331 fix).
func expectedACTRRaw(contentMatch, baseLevel, hebScale, hebbianBoost float64) float64 {
	totalActivation := baseLevel + hebScale*hebbianBoost
	contextualPrior := softplus(totalActivation)
	raw := contentMatch * contextualPrior / actrDenominator
	if raw < 0.0 {
		return 0.0
	}
	return raw
}

func TestComputeACTR_FreshEngram(t *testing.T) {
	now := time.Now()
	eng := &storage.Engram{
		Confidence:  1.0,
		Stability:   30.0,
		AccessCount: 1,
		LastAccess:  now,
	}
	w := actrDefaultWeights()

	sc := computeACTR(0.9, 2.0, 0.0, 0.0, eng, 0, now, w)

	contentMatch := 0.35*0.9 + 0.25*math.Tanh(2.0)
	n := 2.0       // AccessCount(1) + 1
	ageDays := 1.0 / (24.0 * 60.0) // floor: 1 minute
	baseLevel := math.Log(n) - 0.5*math.Log(ageDays/n)
	wantRaw := expectedACTRRaw(contentMatch, baseLevel, 4.0, 0.0)

	assertNear(t, "Raw", sc.Raw, wantRaw, 1e-6)
	assertNear(t, "Final", sc.Final, wantRaw, 1e-6) // Confidence=1.0
	assertNear(t, "SemanticSimilarity", sc.SemanticSimilarity, 0.9, 1e-9)
	assertNear(t, "FullTextRelevance", sc.FullTextRelevance, math.Tanh(2.0), 1e-9)
	if sc.Raw < 0.3 {
		t.Errorf("Fresh engram Raw=%.4f, expected > 0.3", sc.Raw)
	}
}

func TestComputeACTR_OldEngram_NoHebbian(t *testing.T) {
	now := time.Now()
	eng := &storage.Engram{
		Confidence:  1.0,
		Stability:   30.0,
		AccessCount: 0,
		LastAccess:  now.Add(-30 * 24 * time.Hour),
	}
	w := actrDefaultWeights()

	sc := computeACTR(0.7, 1.0, 0.0, 0.0, eng, 0, now, w)

	contentMatch := 0.35*0.7 + 0.25*math.Tanh(1.0)
	n := 1.0
	ageDays := 30.0
	baseLevel := math.Log(n) - 0.5*math.Log(ageDays/n) // ln(1) - 0.5*ln(30) ≈ -1.70
	wantRaw := expectedACTRRaw(contentMatch, baseLevel, 4.0, 0.0)

	assertNear(t, "Raw", sc.Raw, wantRaw, 1e-6)
	if sc.Raw > 0.1 {
		t.Errorf("Old engram (no Hebbian) Raw=%.4f, expected suppressed (< 0.1)", sc.Raw)
	}
}

func TestComputeACTR_OldEngram_WithHebbian(t *testing.T) {
	now := time.Now()
	eng := &storage.Engram{
		Confidence:  1.0,
		Stability:   30.0,
		AccessCount: 0,
		LastAccess:  now.Add(-30 * 24 * time.Hour),
	}
	w := actrDefaultWeights()

	sc := computeACTR(0.7, 1.0, 0.8, 0.0, eng, 0, now, w)

	contentMatch := 0.35*0.7 + 0.25*math.Tanh(1.0)
	n := 1.0
	ageDays := 30.0
	baseLevel := math.Log(n) - 0.5*math.Log(ageDays/n)
	wantRaw := expectedACTRRaw(contentMatch, baseLevel, 4.0, 0.8)

	assertNear(t, "Raw", sc.Raw, wantRaw, 1e-6)

	// Hebbian must rescue the memory: compare with no-Hebbian case.
	scNoHeb := computeACTR(0.7, 1.0, 0.0, 0.0, eng, 0, now, w)
	if sc.Raw <= scNoHeb.Raw*2 {
		t.Errorf("Hebbian rescue too weak: with=%.4f, without=%.4f, ratio=%.1fx",
			sc.Raw, scNoHeb.Raw, sc.Raw/scNoHeb.Raw)
	}
}

func TestComputeACTR_HighAccessCount(t *testing.T) {
	now := time.Now()
	eng := &storage.Engram{
		Confidence:  1.0,
		Stability:   30.0,
		AccessCount: 100,
		LastAccess:  now.Add(-7 * 24 * time.Hour),
	}
	w := actrDefaultWeights()

	sc := computeACTR(0.7, 1.5, 0.0, 0.0, eng, 0, now, w)

	contentMatch := 0.35*0.7 + 0.25*math.Tanh(1.5)
	n := 101.0
	ageDays := 7.0
	baseLevel := math.Log(n) - 0.5*math.Log(ageDays/n)
	wantRaw := expectedACTRRaw(contentMatch, baseLevel, 4.0, 0.0)

	assertNear(t, "Raw", sc.Raw, wantRaw, 1e-6)
	// Base level should be very high with 100 accesses.
	if baseLevel < 3.0 {
		t.Errorf("baseLevel=%.4f, expected > 3.0 for 100 accesses at 7 days", baseLevel)
	}
}

func TestComputeACTR_ZeroContentMatch(t *testing.T) {
	now := time.Now()
	eng := &storage.Engram{
		Confidence:  1.0,
		Stability:   30.0,
		AccessCount: 50,
		LastAccess:  now,
	}
	w := actrDefaultWeights()

	// High activation but zero content relevance.
	sc := computeACTR(0, 0, 0.9, 0.0, eng, 0, now, w)

	if sc.Raw != 0.0 {
		t.Errorf("Zero content match: Raw=%v, want exactly 0.0", sc.Raw)
	}
	if sc.Final != 0.0 {
		t.Errorf("Zero content match: Final=%v, want exactly 0.0", sc.Final)
	}
}

func TestComputeACTR_ConfidenceMultiplication(t *testing.T) {
	now := time.Now()
	eng := &storage.Engram{
		Confidence:  0.5,
		Stability:   30.0,
		AccessCount: 5,
		LastAccess:  now.Add(-2 * 24 * time.Hour),
	}
	w := actrDefaultWeights()

	sc := computeACTR(0.8, 1.0, 0.0, 0.0, eng, 0, now, w)

	assertNear(t, "Confidence", sc.Confidence, 0.5, 1e-9)
	assertNear(t, "Final", sc.Final, sc.Raw*0.5, 1e-9)
	if sc.Raw <= 0 {
		t.Fatal("Raw must be positive for this test to be meaningful")
	}
}

func TestComputeACTR_ScoreClamping(t *testing.T) {
	// computeACTR no longer applies an upper clamp — clamping to [0,1] is the
	// caller's responsibility after per-query normalization (issue #331 fix).
	// Verify that the raw value above 1.0 is returned unchanged.
	now := time.Now()
	eng := &storage.Engram{
		Confidence:  1.0,
		Stability:   30.0,
		AccessCount: 50,
		LastAccess:  now,
	}
	w := actrDefaultWeights()

	// Perfect match + very high FTS + Hebbian + many accesses → exceeds 1.0.
	sc := computeACTR(1.0, 10.0, 1.0, 0.0, eng, 0, now, w)

	// Compute expected unclamped value.
	contentMatch := 0.35*1.0 + 0.25*math.Tanh(10.0)
	n := 51.0
	ageDays := 1.0 / (24.0 * 60.0) // floor: 1 minute
	baseLevel := math.Log(n) - 0.5*math.Log(ageDays/n)
	totalActivation := baseLevel + 4.0*1.0
	unclamped := contentMatch * softplus(totalActivation) / actrDenominator
	if unclamped <= 1.0 {
		t.Fatalf("unclamped=%.4f, test requires > 1.0 to be meaningful", unclamped)
	}
	// computeACTR must now return the unclamped value — the caller normalises.
	if sc.Raw <= 1.0 {
		t.Errorf("Raw=%.4f, expected > 1.0 (no longer clamped in computeACTR)", sc.Raw)
	}
	assertNear(t, "Raw (unclamped)", sc.Raw, unclamped, 1e-9)
}

func TestComputeACTR_ZeroLastAccess(t *testing.T) {
	now := time.Now()
	eng := &storage.Engram{
		Confidence:  1.0,
		Stability:   30.0,
		AccessCount: 0,
		LastAccess:  time.Time{}, // zero value
	}
	w := actrDefaultWeights()

	sc := computeACTR(0.8, 1.0, 0.0, 0.0, eng, 0, now, w)

	// Zero LastAccess → treated as "just now" → ageDays = ageFloor (1 minute)
	contentMatch := 0.35*0.8 + 0.25*math.Tanh(1.0)
	n := 1.0
	ageDays := 1.0 / (24.0 * 60.0) // floor: 1 minute
	baseLevel := math.Log(n) - 0.5*math.Log(ageDays/n)
	wantRaw := expectedACTRRaw(contentMatch, baseLevel, 4.0, 0.0)

	assertNear(t, "Raw", sc.Raw, wantRaw, 1e-6)
}

func TestComputeACTR_PreY2KLastAccess(t *testing.T) {
	now := time.Now()
	preY2K := time.Date(1999, 1, 1, 0, 0, 0, 0, time.UTC)
	eng := &storage.Engram{
		Confidence:  1.0,
		Stability:   30.0,
		AccessCount: 0,
		LastAccess:  preY2K,
	}
	w := actrDefaultWeights()

	sc := computeACTR(0.8, 1.0, 0.0, 0.0, eng, 0, now, w)

	// Pre-Y2K → treated as "just now", identical to zero-time case.
	engZero := &storage.Engram{
		Confidence:  1.0,
		Stability:   30.0,
		AccessCount: 0,
		LastAccess:  time.Time{},
	}
	scZero := computeACTR(0.8, 1.0, 0.0, 0.0, engZero, 0, now, w)

	assertNear(t, "Raw (PreY2K == ZeroTime)", sc.Raw, scZero.Raw, 1e-9)
	assertNear(t, "Final (PreY2K == ZeroTime)", sc.Final, scZero.Final, 1e-9)
}

func TestComputeACTR_CustomDecayAndHebScale(t *testing.T) {
	now := time.Now()
	eng := &storage.Engram{
		Confidence:  1.0,
		Stability:   30.0,
		AccessCount: 3,
		LastAccess:  now.Add(-10 * 24 * time.Hour),
	}

	hebbianBoost := 0.6

	// Custom parameters: steeper decay, weaker Hebbian scaling.
	w := actrDefaultWeights()
	w.ACTRDecay = 0.8
	w.ACTRHebScale = 2.0

	sc := computeACTR(0.7, 1.0, hebbianBoost, 0.0, eng, 0, now, w)

	contentMatch := 0.35*0.7 + 0.25*math.Tanh(1.0)
	n := 4.0
	ageDays := 10.0
	baseLevel := math.Log(n) - 0.8*math.Log(ageDays/n)
	wantRaw := expectedACTRRaw(contentMatch, baseLevel, 2.0, hebbianBoost)

	assertNear(t, "Raw", sc.Raw, wantRaw, 1e-6)

	// Must differ from default parameters.
	wDef := actrDefaultWeights()
	scDef := computeACTR(0.7, 1.0, hebbianBoost, 0.0, eng, 0, now, wDef)
	if math.Abs(sc.Raw-scDef.Raw) < 1e-6 {
		t.Error("Custom params should produce different Raw than defaults")
	}
}

func TestSoftplus_NumericalStability(t *testing.T) {
	t.Run("large_positive_returns_x", func(t *testing.T) {
		for _, x := range []float64{21, 50, 100, 1000} {
			got := softplus(x)
			if got != x {
				t.Errorf("softplus(%v) = %v, want %v (large x fast path)", x, got, x)
			}
		}
	})

	t.Run("boundary_20_uses_log_path", func(t *testing.T) {
		got := softplus(20)
		want := math.Log1p(math.Exp(20))
		assertNear(t, "softplus(20)", got, want, 1e-6)
	})

	t.Run("boundary_above_20_returns_x", func(t *testing.T) {
		got := softplus(20.0001)
		if got != 20.0001 {
			t.Errorf("softplus(20.0001) = %v, want 20.0001", got)
		}
	})

	t.Run("large_negative_near_zero", func(t *testing.T) {
		got := softplus(-21)
		if got <= 0 {
			t.Errorf("softplus(-21) = %v, expected > 0 (softplus is always positive)", got)
		}
		if got > 1e-8 {
			t.Errorf("softplus(-21) = %v, expected near 0", got)
		}

		got50 := softplus(-50)
		if got50 <= 0 || got50 > 1e-20 {
			t.Errorf("softplus(-50) = %v, expected in (0, 1e-20)", got50)
		}
	})

	t.Run("zero_equals_ln2", func(t *testing.T) {
		assertNear(t, "softplus(0)", softplus(0), math.Ln2, 1e-15)
	})

	t.Run("monotonically_increasing", func(t *testing.T) {
		prev := softplus(-10)
		for _, x := range []float64{-5, -1, 0, 1, 5, 10, 25} {
			cur := softplus(x)
			if cur <= prev {
				t.Errorf("softplus(%v)=%v <= softplus(prev)=%v, not monotonic", x, cur, prev)
			}
			prev = cur
		}
	})
}

func TestComputeACTR_DecayFactorReportedCorrectly(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name      string
		ageDays   float64
		stability float32
	}{
		{"fresh_s30", 0, 30.0},
		{"1day_s30", 1, 30.0},
		{"7days_s30", 7, 30.0},
		{"30days_s14", 30, 14.0},
		{"100days_s30", 100, 30.0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lastAccess := now
			if tt.ageDays > 0 {
				lastAccess = now.Add(-time.Duration(tt.ageDays*24) * time.Hour)
			}
			eng := &storage.Engram{
				Confidence:  1.0,
				Stability:   tt.stability,
				AccessCount: 1,
				LastAccess:  lastAccess,
			}
			w := actrDefaultWeights()

			sc := computeACTR(0.5, 0.5, 0.0, 0.0, eng, 0, now, w)

			effectiveAgeDays := math.Max(now.Sub(lastAccess).Hours()/24.0, 1.0/(24.0*60.0))
			wantDecay := math.Max(0.05, math.Exp(-effectiveAgeDays/float64(tt.stability)))

			assertNear(t, "DecayFactor", sc.DecayFactor, wantDecay, 1e-6)
		})
	}
}

// TestACTR_PerQueryNormalization_RankingPreserved verifies that per-query
// normalization restores relative ranking when multiple fresh engrams saturate
// above 1.0. Before the fix (issue #331), the hard clamp at 1.0 collapsed all
// saturated scores to the same value — ranking became arbitrary tie-breaking.
//
// Uses AccessCount=5 (n=6) with LastAccess=now to produce a high base-level
// activation (B(M) ≈ 6.3), which pushes raw above 1.0 for both engrams even
// though their content match scores differ — matching the small-vault scenario
// reported in the issue.
func TestACTR_PerQueryNormalization_RankingPreserved(t *testing.T) {
	now := time.Now()
	w := actrDefaultWeights()

	// Both engrams have high base-level activation (AccessCount=5, just accessed)
	// so both produce raw > 1.0. They differ only in semantic relevance.
	engHigh := &storage.Engram{Confidence: 1.0, Stability: 30.0, AccessCount: 5, LastAccess: now}
	engLow := &storage.Engram{Confidence: 1.0, Stability: 30.0, AccessCount: 5, LastAccess: now}

	// vectorScore=0.9 vs 0.4; same ftsScore — only content match differs.
	scHigh := computeACTR(0.9, 1.0, 0.0, 0.0, engHigh, 0, now, w)
	scLow := computeACTR(0.4, 1.0, 0.0, 0.0, engLow, 0, now, w)

	// Precondition: both raw scores must exceed 1.0 (saturation zone).
	if scHigh.Raw <= 1.0 {
		t.Fatalf("precondition: scHigh.Raw=%.4f must exceed 1.0 (check AccessCount/LastAccess)", scHigh.Raw)
	}
	if scLow.Raw <= 1.0 {
		t.Fatalf("precondition: scLow.Raw=%.4f must exceed 1.0 (check AccessCount/LastAccess)", scLow.Raw)
	}

	// Simulate per-query normalization (mirrors the ACT-R scoring path in engine.go).
	maxRaw := math.Max(scHigh.Raw, scLow.Raw)
	normHigh := math.Min(scHigh.Raw/maxRaw, 1.0)
	normLow := math.Min(scLow.Raw/maxRaw, 1.0)

	// After normalization the higher-content-match engram must outscore the lower one.
	if normHigh <= normLow {
		t.Errorf("normHigh=%.4f <= normLow=%.4f: high-relevance engram must rank above low-relevance", normHigh, normLow)
	}
	// The top scorer anchors at exactly 1.0.
	if math.Abs(normHigh-1.0) > 1e-9 {
		t.Errorf("top normalized score=%.10f, want 1.0", normHigh)
	}
	// Scores must not be identical (the old saturation bug: both clamped to 1.0).
	if normHigh == normLow {
		t.Error("normalized scores are identical — saturation not resolved")
	}
}
