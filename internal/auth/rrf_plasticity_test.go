package auth

import "testing"

// ---------------------------------------------------------------------------
// Tests for ScoringFusion and RRF_K plasticity parameters
// ---------------------------------------------------------------------------

func TestScoringFusion_DefaultEmpty(t *testing.T) {
	r := ResolvePlasticity(nil)
	if r.ScoringFusion != "" {
		t.Errorf("default ScoringFusion should be empty (ACT-R), got %q", r.ScoringFusion)
	}
}

func TestScoringFusion_AllPresetsDefaultEmpty(t *testing.T) {
	presets := []string{"default", "reference", "scratchpad", "knowledge-graph"}
	for _, name := range presets {
		r := ResolvePlasticity(&PlasticityConfig{Preset: name})
		if r.ScoringFusion != "" {
			t.Errorf("preset %q: ScoringFusion should be empty, got %q", name, r.ScoringFusion)
		}
	}
}

func TestScoringFusion_OverrideRRF(t *testing.T) {
	mode := "rrf"
	r := ResolvePlasticity(&PlasticityConfig{ScoringFusion: &mode})
	if r.ScoringFusion != "rrf" {
		t.Errorf("override: want ScoringFusion=rrf, got %q", r.ScoringFusion)
	}
}

func TestScoringFusion_OverrideWeightedSum(t *testing.T) {
	mode := "weighted_sum"
	r := ResolvePlasticity(&PlasticityConfig{ScoringFusion: &mode})
	if r.ScoringFusion != "weighted_sum" {
		t.Errorf("override: want ScoringFusion=weighted_sum, got %q", r.ScoringFusion)
	}
}

func TestScoringFusion_InvalidFallsToEmpty(t *testing.T) {
	mode := "invalid-scoring"
	r := ResolvePlasticity(&PlasticityConfig{ScoringFusion: &mode})
	if r.ScoringFusion != "" {
		t.Errorf("invalid mode should fall back to empty, got %q", r.ScoringFusion)
	}
}

func TestRRFK_DefaultIs60(t *testing.T) {
	r := ResolvePlasticity(nil)
	if r.RRF_K != 60 {
		t.Errorf("default RRF_K should be 60, got %d", r.RRF_K)
	}
}

func TestRRFK_AllPresetsDefault60(t *testing.T) {
	presets := []string{"default", "reference", "scratchpad", "knowledge-graph"}
	for _, name := range presets {
		r := ResolvePlasticity(&PlasticityConfig{Preset: name})
		if r.RRF_K != 60 {
			t.Errorf("preset %q: RRF_K should be 60, got %d", name, r.RRF_K)
		}
	}
}

func TestRRFK_Override(t *testing.T) {
	k := 40
	r := ResolvePlasticity(&PlasticityConfig{RRF_K: &k})
	if r.RRF_K != 40 {
		t.Errorf("override: want RRF_K=40, got %d", r.RRF_K)
	}
}

func TestRRFK_ClampedLow(t *testing.T) {
	k := 0
	r := ResolvePlasticity(&PlasticityConfig{RRF_K: &k})
	if r.RRF_K != 1 {
		t.Errorf("zero should clamp to 1, got %d", r.RRF_K)
	}
}

func TestRRFK_ClampedHigh(t *testing.T) {
	k := 10000
	r := ResolvePlasticity(&PlasticityConfig{RRF_K: &k})
	if r.RRF_K != 1000 {
		t.Errorf("above 1000 should clamp to 1000, got %d", r.RRF_K)
	}
}

func TestValidScoringFusion(t *testing.T) {
	valid := []string{"", "rrf", "weighted_sum"}
	for _, mode := range valid {
		if !ValidScoringFusion(mode) {
			t.Errorf("ValidScoringFusion(%q) = false, want true", mode)
		}
	}

	invalid := []string{"actr", "cgdn", "RANDOM", "RRF"}
	for _, mode := range invalid {
		if ValidScoringFusion(mode) {
			t.Errorf("ValidScoringFusion(%q) = true, want false", mode)
		}
	}
}
