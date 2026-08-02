package auth

import (
	"math"
	"testing"
	"time"
)

func f32(v float32) *float32 { return &v }

// TestResolvePlasticity_AssocHalfLifePresets pins the per-preset half-life
// regime chosen for #762. default/reference/working sit on 30 days, the same
// clock as their own TemporalHalflife, so engram-level and edge-level
// forgetting agree. knowledge-graph is 3x slower because structural durability
// is what that preset is for. scratchpad has no decay at all (Hebbian is off).
func TestResolvePlasticity_AssocHalfLifePresets(t *testing.T) {
	want := map[string]float32{
		"default":         30,
		"reference":       30,
		"working":         30,
		"knowledge-graph": 90,
		"scratchpad":      0,
	}
	for preset, days := range want {
		r := ResolvePlasticity(&PlasticityConfig{Preset: preset})
		if r.AssocHalfLifeDays != days {
			t.Errorf("%s: AssocHalfLifeDays = %v, want %v", preset, r.AssocHalfLifeDays, days)
		}
		// AssocDecayFactor survives as the enable/disable switch only (COG-16).
		wantEnabled := days > 0
		if got := r.AssocDecayFactor > 0; got != wantEnabled {
			t.Errorf("%s: decay switch = %v, want %v", preset, got, wantEnabled)
		}
	}
	// Every preset must be covered — a new preset without a half-life would
	// have decay enabled and no rate, and get skipped at runtime.
	if len(want) != len(plasticityPresets) {
		t.Errorf("preset count drifted: this test covers %d, table has %d", len(want), len(plasticityPresets))
	}
}

// TestResolvePlasticity_LegacyFactorConverts covers the migration story for
// #762: an operator who explicitly set assoc_decay_factor set it when the field
// was documented as a rate. It no longer is. Rather than silently keeping a
// number that never denominated anything (a pass is an interval owned by an
// unrelated worker), the factor is reinterpreted per-DAY and converted.
//
// The engine WARNs once per vault when this fires — see
// TestDecayAllVaults_LegacyFactorWarnsOncePerVault.
func TestResolvePlasticity_LegacyFactorConverts(t *testing.T) {
	cases := []struct {
		factor float32
		want   float64 // half-life in days
	}{
		{0.95, 13.5134}, // ln0.5/ln0.95
		{0.98, 34.3097},
		{0.5, 1.0},
		// factor >= 1 carries NO rate: a per-pass multiplier of 1 never moved
		// a weight, so "on but never move" must resolve to half-life 0 (the
		// skip-with-WARN path), NOT fall through to the preset's 30 days. The
		// preset default would silently substitute a rate the operator
		// explicitly declined (principle #1).
		{1.0, 0},
		{1.05, 0},
	}
	for _, tc := range cases {
		r := ResolvePlasticity(&PlasticityConfig{AssocDecayFactor: f32(tc.factor)})
		if math.Abs(float64(r.AssocHalfLifeDays)-tc.want) > 1e-3 {
			t.Errorf("factor %v: AssocHalfLifeDays = %v, want %v", tc.factor, r.AssocHalfLifeDays, tc.want)
		}
	}

	// An explicit half-life always wins over the legacy factor — the factor is
	// only a switch once the rate is stated.
	r := ResolvePlasticity(&PlasticityConfig{AssocDecayFactor: f32(0.95), AssocHalfLifeDays: f32(60)})
	if r.AssocHalfLifeDays != 60 {
		t.Errorf("explicit half-life must win over legacy factor: got %v, want 60", r.AssocHalfLifeDays)
	}

	// No explicit factor → the preset's half-life, not a conversion of the
	// preset's factor. The preset carries both fields deliberately.
	r = ResolvePlasticity(&PlasticityConfig{Preset: "default"})
	if r.AssocHalfLifeDays != 30 {
		t.Errorf("preset factor must NOT be converted: got %v, want the preset's 30", r.AssocHalfLifeDays)
	}

	// A factor of 0 disables decay and carries no rate to convert.
	r = ResolvePlasticity(&PlasticityConfig{AssocDecayFactor: f32(0)})
	if r.AssocHalfLifeDays != 30 {
		t.Errorf("disabling factor must leave the preset half-life alone: got %v", r.AssocHalfLifeDays)
	}
	if r.AssocDecayFactor != 0 {
		t.Errorf("factor 0 must resolve to 0 (decay off): got %v", r.AssocDecayFactor)
	}
}

// TestResolvePlasticity_HalfLifeClamped — COG-2: clamp, never reject.
func TestResolvePlasticity_HalfLifeClamped(t *testing.T) {
	cases := []struct{ in, want float32 }{
		{-5, 0.5},
		{0.001, 0.5},
		{0.5, 0.5},
		{30, 30},
		{3650, 3650},
		{99999, 3650},
	}
	for _, tc := range cases {
		r := ResolvePlasticity(&PlasticityConfig{AssocHalfLifeDays: f32(tc.in)})
		if r.AssocHalfLifeDays != tc.want {
			t.Errorf("AssocHalfLifeDays %v: got %v, want %v", tc.in, r.AssocHalfLifeDays, tc.want)
		}
	}
	// 0 means "unset", not "clamp to the minimum" — it falls through to the preset.
	r := ResolvePlasticity(&PlasticityConfig{AssocHalfLifeDays: f32(0)})
	if r.AssocHalfLifeDays != 30 {
		t.Errorf("zero half-life must mean unset: got %v, want the preset's 30", r.AssocHalfLifeDays)
	}
}

// TestAssocHalfLife_Duration pins the days→Duration conversion the decay call
// site uses, including the "no rate configured" case that must NOT silently
// substitute a default (principle #1).
func TestAssocHalfLife_Duration(t *testing.T) {
	if got := ResolvePlasticity(&PlasticityConfig{Preset: "default"}).AssocHalfLife(); got != 30*24*time.Hour {
		t.Errorf("default AssocHalfLife = %v, want 720h", got)
	}
	if got := ResolvePlasticity(&PlasticityConfig{Preset: "scratchpad"}).AssocHalfLife(); got != 0 {
		t.Errorf("scratchpad AssocHalfLife = %v, want 0 (no rate configured)", got)
	}
}
