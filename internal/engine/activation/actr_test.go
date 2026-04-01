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

// expectedBaseLevel returns the B(M) value that computeACTR will produce
// for the given parameters, including the bLevelCap. Tests should use this
// instead of inlining the formula so they stay in sync with production code.
func expectedBaseLevel(n, ageDays, d float64) float64 {
	const ageFloorDays = 1.0 / (24.0 * 60.0)
	effectiveAge := math.Max(ageDays, ageFloorDays)
	bl := math.Log(n) - d*math.Log(effectiveAge/n)
	bLevelCap := math.Log(math.Exp(actrDenominator) - 1)
	if bl > bLevelCap {
		bl = bLevelCap
	}
	return bl
}

// expectedACTRRaw computes the expected Raw score from first principles.
// Callers should pass a baseLevel already produced by expectedBaseLevel so
// the cap is applied consistently.
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
	n := 2.0 // AccessCount(1) + 1
	// Use expectedBaseLevel so the 1-day offset + cap are applied consistently.
	baseLevel := expectedBaseLevel(n, 1.0/(24.0*60.0), 0.5)
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
	// n=1, ageDays=30: B(M) = ln(1) - 0.5*ln(30) ≈ -1.70 — well below cap.
	baseLevel := expectedBaseLevel(n, 30.0, 0.5)
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
	baseLevel := expectedBaseLevel(n, 30.0, 0.5)
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
	// With 100 accesses at 7 days the uncapped B(M) ≈ 5.88 — well above bLevelCap.
	// expectedBaseLevel applies the cap so wantRaw reflects what computeACTR returns.
	baseLevel := expectedBaseLevel(n, 7.0, 0.5)
	wantRaw := expectedACTRRaw(contentMatch, baseLevel, 4.0, 0.0)

	assertNear(t, "Raw", sc.Raw, wantRaw, 1e-6)
	// Confirm the cap fired: uncapped value >> bLevelCap.
	uncappedBL := math.Log(n) - 0.5*math.Log(7.0/n)
	bLevelCap := math.Log(math.Exp(actrDenominator) - 1)
	if uncappedBL <= bLevelCap {
		t.Errorf("test setup: uncappedBL=%.4f must exceed bLevelCap=%.4f", uncappedBL, bLevelCap)
	}
	if baseLevel != bLevelCap {
		t.Errorf("baseLevel=%.6f, want exactly bLevelCap=%.6f", baseLevel, bLevelCap)
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

	// Hebbian=1.0 pushes totalActivation well above bLevelCap — raw still exceeds 1.0.
	// This is the case the two-pass safety net handles (Hebbian-boosted engrams).
	sc := computeACTR(1.0, 10.0, 1.0, 0.0, eng, 0, now, w)

	contentMatch := 0.35*1.0 + 0.25*math.Tanh(10.0)
	n := 51.0
	// baseLevel is capped; Hebbian lifts totalActivation far above the cap.
	baseLevel := expectedBaseLevel(n, 1.0/(24.0*60.0), 0.5)
	totalActivation := baseLevel + 4.0*1.0
	expectedRaw := contentMatch * softplus(totalActivation) / actrDenominator
	if expectedRaw <= 1.0 {
		t.Fatalf("expectedRaw=%.4f, test requires > 1.0 — check Hebbian/AccessCount", expectedRaw)
	}
	if sc.Raw <= 1.0 {
		t.Errorf("Raw=%.4f, expected > 1.0 (Hebbian should push past saturation)", sc.Raw)
	}
	assertNear(t, "Raw (Hebbian-boosted)", sc.Raw, expectedRaw, 1e-9)
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
	// n=1 with 1-day offset: B(M) = ln(1) - 0.5*ln(1) = 0 — not capped.
	baseLevel := expectedBaseLevel(n, 1.0/(24.0*60.0), 0.5)
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
	// n=4, ageDays=10: B(M)=ln(4)-0.8*ln(10/4)≈0.65 — not capped.
	baseLevel := expectedBaseLevel(n, 10.0, 0.8)
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

// TestComputeACTR_FreshVault_NoDivergence verifies that the root-cause fix
// (bmAgeOffset + bLevelCap) prevents B(M) from saturating for fresh engrams
// with zero Hebbian boost. Before the fix, any engram with AccessCount ≥ 1
// and sub-minute age would produce raw > 1.0 due to B(M) ≈ 4–7.
//
// Post-fix: base-level alone cannot push raw above contentMatch.
func TestComputeACTR_FreshVault_NoDivergence(t *testing.T) {
	now := time.Now()
	w := actrDefaultWeights()

	cases := []struct {
		name        string
		accessCount int
		vectorScore float64
	}{
		{"n1", 0, 0.8},
		{"n5", 4, 0.8},
		{"n20", 19, 0.8},
		{"n100", 99, 0.8},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			eng := &storage.Engram{
				Confidence:  1.0,
				Stability:   30.0,
				AccessCount: uint32(tc.accessCount),
				LastAccess:  now,
			}
			sc := computeACTR(tc.vectorScore, 0.0, 0.0, 0.0, eng, 0, now, w)
			contentMatch := w.SemanticSimilarity * tc.vectorScore
			if sc.Raw > contentMatch+1e-9 {
				t.Errorf("Raw=%.6f exceeds contentMatch=%.6f — base-level saturated past content gate",
					sc.Raw, contentMatch)
			}
		})
	}
}

// TestComputeACTR_FreshVault_Differentiated verifies that two fresh engrams
// with different content-match scores produce different Raw values.
// This was the observable symptom of issue #331: all engrams collapsed to the
// same clamped score, destroying ranking in new vaults.
func TestComputeACTR_FreshVault_Differentiated(t *testing.T) {
	now := time.Now()
	w := actrDefaultWeights()

	// Both fresh, high access count — the scenario that triggered #331.
	engHigh := &storage.Engram{Confidence: 1.0, Stability: 30.0, AccessCount: 5, LastAccess: now}
	engLow := &storage.Engram{Confidence: 1.0, Stability: 30.0, AccessCount: 5, LastAccess: now}

	scHigh := computeACTR(0.9, 1.0, 0.0, 0.0, engHigh, 0, now, w)
	scLow := computeACTR(0.4, 1.0, 0.0, 0.0, engLow, 0, now, w)

	if scHigh.Raw <= scLow.Raw {
		t.Errorf("scHigh.Raw=%.6f <= scLow.Raw=%.6f: higher content match must rank above lower",
			scHigh.Raw, scLow.Raw)
	}
	// Neither score should saturate without Hebbian boost.
	contentHigh := w.SemanticSimilarity*0.9 + w.FullTextRelevance*math.Tanh(1.0)
	contentLow := w.SemanticSimilarity*0.4 + w.FullTextRelevance*math.Tanh(1.0)
	if scHigh.Raw > contentHigh+1e-9 {
		t.Errorf("scHigh.Raw=%.6f exceeded contentMatch=%.6f — saturation not resolved", scHigh.Raw, contentHigh)
	}
	if scLow.Raw > contentLow+1e-9 {
		t.Errorf("scLow.Raw=%.6f exceeded contentMatch=%.6f — saturation not resolved", scLow.Raw, contentLow)
	}
}

// TestComputeACTR_TwoPassSafetyNet_HebbianCanStillSaturate verifies that the
// two-pass per-query normalization safety net is still needed and functional.
// After the root-cause fix, base-level alone cannot saturate; but a strong
// Hebbian boost legitimately pushes totalActivation well past the cap, so
// raw > 1.0 is still possible and the safety net must handle it.
func TestComputeACTR_TwoPassSafetyNet_HebbianCanStillSaturate(t *testing.T) {
	now := time.Now()
	w := actrDefaultWeights()

	eng := &storage.Engram{Confidence: 1.0, Stability: 30.0, AccessCount: 5, LastAccess: now}

	// Strong Hebbian (1.0) with scale 4.0 adds 4.0 to totalActivation.
	scNoHeb := computeACTR(0.9, 1.0, 0.0, 0.0, eng, 0, now, w)
	scHeb := computeACTR(0.9, 1.0, 1.0, 0.0, eng, 0, now, w)

	if scNoHeb.Raw > 1.0 {
		t.Errorf("zero-Hebbian Raw=%.6f must not exceed 1.0 after root-cause fix", scNoHeb.Raw)
	}
	if scHeb.Raw <= 1.0 {
		t.Errorf("strong-Hebbian Raw=%.6f must exceed 1.0 (two-pass safety net required)", scHeb.Raw)
	}
}
