package embed

import "testing"

// ---------------------------------------------------------------------------
// COG-26: NoiseBaseline registry + Rescale transform pins.
// ---------------------------------------------------------------------------

func TestNoiseBaseline_RegisteredModel(t *testing.T) {
	b, ok := NoiseBaseline("bge-small-en-v1.5")
	if !ok {
		t.Fatal("expected bge-small-en-v1.5 to be registered")
	}
	if b != 0.520 {
		t.Errorf("bge-small-en-v1.5 baseline = %v, want 0.520 (mu=0.450 + 1.3*sigma=0.054, measured — see design doc)", b)
	}
}

func TestNoiseBaseline_UnknownModel(t *testing.T) {
	if _, ok := NoiseBaseline("some-future-model-v2"); ok {
		t.Error("expected an unregistered model to report ok=false, not a guessed baseline")
	}
	if _, ok := NoiseBaseline(""); ok {
		t.Error("expected empty model string to report ok=false — never a guessed baseline for 'we don't know'")
	}
}

func TestRescale_IdentityAtZeroBaseline(t *testing.T) {
	for _, cos := range []float64{0, 0.3, 0.520, 0.7, 1.0} {
		if got := Rescale(cos, 0); got != cos {
			t.Errorf("Rescale(%v, 0) = %v, want %v (b<=0 is identity)", cos, got, cos)
		}
	}
}

func TestRescale_AtOrBelowBaselineIsZero(t *testing.T) {
	const b = 0.520
	for _, cos := range []float64{0, 0.3, 0.520} {
		if got := Rescale(cos, b); got != 0 {
			t.Errorf("Rescale(%v, %v) = %v, want 0 (at/below the noise floor contributes nothing)", cos, b, got)
		}
	}
}

func TestRescale_AboveBaselineIsPositiveAndBounded(t *testing.T) {
	const b = 0.520
	got := Rescale(0.69, b)
	if got <= 0 || got > 1 {
		t.Errorf("Rescale(0.69, %v) = %v, want in (0, 1]", b, got)
	}
	// cos=1.0 (perfect match) must rescale to exactly 1.0.
	if got := Rescale(1.0, b); got != 1.0 {
		t.Errorf("Rescale(1.0, %v) = %v, want 1.0", b, got)
	}
}

// TestRescale_Monotone pins the COG-26 claim that the rescale can never
// reorder candidates: RRF ranking (and any other consumer that sorts by the
// calibrated value) is provably unaffected by the floor.
func TestRescale_Monotone(t *testing.T) {
	const b = 0.520
	cosines := []float64{0, 0.1, 0.3, 0.45, 0.520, 0.6, 0.65, 0.69, 0.8, 1.0}
	prev := -1.0
	for _, cos := range cosines {
		v := Rescale(cos, b)
		if v < prev {
			t.Fatalf("Rescale is not monotone: cos=%v produced %v, which is less than a lower cosine's %v", cos, v, prev)
		}
		prev = v
	}
}

func TestRescale_DegenerateBaselineFallsBackToIdentity(t *testing.T) {
	// b>=1 would divide by <=0 — must never NaN/Inf, falls back to identity.
	if got := Rescale(0.7, 1.0); got != 0.7 {
		t.Errorf("Rescale(0.7, 1.0) = %v, want 0.7 (degenerate b falls back to identity, not NaN/Inf)", got)
	}
}
