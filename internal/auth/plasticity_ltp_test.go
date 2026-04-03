package auth

import (
	"testing"
)

// ---------------------------------------------------------------------------
// Test: LTP parameters have correct defaults
// ---------------------------------------------------------------------------

func TestPlasticity_LTP_DefaultsNil(t *testing.T) {
	// nil config = default preset. LTP fields should have zero values
	// (disabled by default for backward compatibility).
	r := ResolvePlasticity(nil)

	if r.LTPThreshold != 0 {
		t.Errorf("LTPThreshold default: got %d, want 0 (disabled)", r.LTPThreshold)
	}
	if r.LTPWeightFloor != 0 {
		t.Errorf("LTPWeightFloor default: got %v, want 0 (disabled)", r.LTPWeightFloor)
	}
}

// ---------------------------------------------------------------------------
// Test: LTP parameters can be overridden via PlasticityConfig
// ---------------------------------------------------------------------------

func TestPlasticity_LTP_Override(t *testing.T) {
	threshold := 5
	weightFloor := float32(0.3)

	cfg := &PlasticityConfig{
		LTPThreshold:   &threshold,
		LTPWeightFloor: &weightFloor,
	}

	r := ResolvePlasticity(cfg)

	if r.LTPThreshold != 5 {
		t.Errorf("LTPThreshold override: got %d, want 5", r.LTPThreshold)
	}
	if r.LTPWeightFloor != 0.3 {
		t.Errorf("LTPWeightFloor override: got %v, want 0.3", r.LTPWeightFloor)
	}
}

// ---------------------------------------------------------------------------
// Test: LTP parameters are clamped to valid ranges
// ---------------------------------------------------------------------------

func TestPlasticity_LTP_Clamping(t *testing.T) {
	// Negative threshold should be clamped to 0
	negThreshold := -1
	cfg := &PlasticityConfig{
		LTPThreshold: &negThreshold,
	}
	r := ResolvePlasticity(cfg)
	if r.LTPThreshold < 0 {
		t.Errorf("LTPThreshold should not be negative: got %d", r.LTPThreshold)
	}

	// WeightFloor should be clamped to [0, 1]
	highFloor := float32(1.5)
	cfg4 := &PlasticityConfig{
		LTPWeightFloor: &highFloor,
	}
	r4 := ResolvePlasticity(cfg4)
	if r4.LTPWeightFloor > 1.0 {
		t.Errorf("LTPWeightFloor should be clamped to 1.0: got %v", r4.LTPWeightFloor)
	}
}

// ---------------------------------------------------------------------------
// Test: Presets do not include LTP by default (backward compatible)
// ---------------------------------------------------------------------------

func TestPlasticity_LTP_PresetsHaveZeroDefaults(t *testing.T) {
	for _, preset := range []string{"default", "reference", "scratchpad", "knowledge-graph"} {
		cfg := &PlasticityConfig{Preset: preset}
		r := ResolvePlasticity(cfg)

		if r.LTPThreshold != 0 {
			t.Errorf("preset %q: LTPThreshold should be 0, got %d", preset, r.LTPThreshold)
		}
		if r.LTPWeightFloor != 0 {
			t.Errorf("preset %q: LTPWeightFloor should be 0, got %v", preset, r.LTPWeightFloor)
		}
	}
}
