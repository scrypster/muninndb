package auth

import "testing"

// ---------------------------------------------------------------------------
// Tests for ScoringFusion plasticity parameter
// ---------------------------------------------------------------------------

func TestScoringFusion_DefaultEmpty(t *testing.T) {
	r := ResolvePlasticity(nil)
	if r.ScoringFusion != "rrf" {
		t.Errorf("default ScoringFusion should be rrf, got %q", r.ScoringFusion)
	}
}

func TestScoringFusion_AllPresetsDefaultEmpty(t *testing.T) {
	// Only "default" preset now uses RRF; other presets still default to ACT-R ("").
	presets := map[string]string{
		"default":          "rrf",
		"reference":        "",
		"scratchpad":       "",
		"knowledge-graph":  "",
	}
	for name, want := range presets {
		r := ResolvePlasticity(&PlasticityConfig{Preset: name})
		if r.ScoringFusion != want {
			t.Errorf("preset %q: ScoringFusion want %q, got %q", name, want, r.ScoringFusion)
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
