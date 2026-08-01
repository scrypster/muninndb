package auth

import (
	"reflect"
	"testing"
)

// The per-vault exclude-tags knob (#713) is not preset-varying and is
// default-empty / opt-in: a nil or empty config must resolve to a nil list so
// recall behaves byte-identically to before. Resolution also normalizes the
// list (blank entries dropped, duplicates collapsed) and never mutates a
// preset. All fixtures use invented synthetic tag names.

func TestResolvePlasticity_ExcludeTags_DefaultNil(t *testing.T) {
	// No config at all: identity — no exclusion.
	if got := ResolvePlasticity(nil).ExcludeTags; got != nil {
		t.Errorf("nil config: ExcludeTags = %v, want nil (default-empty / opt-in)", got)
	}
	// Empty config with an unrelated preset: still no exclusion.
	if got := ResolvePlasticity(&PlasticityConfig{Preset: "reference"}).ExcludeTags; got != nil {
		t.Errorf("preset-only config: ExcludeTags = %v, want nil", got)
	}
	// Explicitly empty slice resolves identically to "not set".
	if got := ResolvePlasticity(&PlasticityConfig{ExcludeTags: []string{}}).ExcludeTags; got != nil {
		t.Errorf("empty slice: ExcludeTags = %v, want nil", got)
	}
}

func TestResolvePlasticity_ExcludeTags_Passthrough(t *testing.T) {
	r := ResolvePlasticity(&PlasticityConfig{ExcludeTags: []string{"noise-tag", "fixture-log"}})
	want := []string{"noise-tag", "fixture-log"}
	if !reflect.DeepEqual(r.ExcludeTags, want) {
		t.Errorf("ExcludeTags = %v, want %v", r.ExcludeTags, want)
	}
}

func TestResolvePlasticity_ExcludeTags_Normalized(t *testing.T) {
	// Blank entries dropped, duplicates collapsed, first-seen order preserved.
	r := ResolvePlasticity(&PlasticityConfig{
		ExcludeTags: []string{"noise-tag", "  ", "fixture-log", "noise-tag", " template-x "},
	})
	want := []string{"noise-tag", "fixture-log", "template-x"}
	if !reflect.DeepEqual(r.ExcludeTags, want) {
		t.Errorf("normalized ExcludeTags = %v, want %v", r.ExcludeTags, want)
	}
}

func TestResolvePlasticity_ExcludeTags_AllBlankResolvesNil(t *testing.T) {
	// A config of only-blank tags must resolve to nil — byte-identical to "no
	// config", so it cannot silently diverge from the default recall path.
	if got := ResolvePlasticity(&PlasticityConfig{ExcludeTags: []string{"", "   "}}).ExcludeTags; got != nil {
		t.Errorf("all-blank config: ExcludeTags = %v, want nil", got)
	}
}
