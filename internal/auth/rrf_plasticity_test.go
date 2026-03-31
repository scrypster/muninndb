package auth

import "testing"

// ---------------------------------------------------------------------------
// Tests for ScoringFusion plasticity parameter
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
