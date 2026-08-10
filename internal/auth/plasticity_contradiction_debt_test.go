package auth

import (
	"encoding/json"
	"reflect"
	"testing"
)

// The per-vault contradiction-debt readout switch is NOT preset-varying and
// resolves default-TRUE. These are the #713-shaped pins: an absent config must
// resolve byte-identically to today's resolution PLUS the new default-true
// field, and no preset may move it.

func TestResolvePlasticity_ContradictionDebt_DefaultsTrueAndIsNotPresetVarying(t *testing.T) {
	if !ResolvePlasticity(nil).ContradictionDebt {
		t.Error("nil config: ContradictionDebt = false, want true (resolved default)")
	}
	for preset := range plasticityPresets {
		got := ResolvePlasticity(&PlasticityConfig{Preset: preset})
		if !got.ContradictionDebt {
			t.Errorf("preset %q: ContradictionDebt = false, want true — the field is not preset-varying", preset)
		}
	}
}

func TestResolvePlasticity_ContradictionDebt_PerVaultOverride(t *testing.T) {
	off := false
	if ResolvePlasticity(&PlasticityConfig{ContradictionDebt: &off}).ContradictionDebt {
		t.Error("explicit false was not honored")
	}
	on := true
	if !ResolvePlasticity(&PlasticityConfig{Preset: "reference", ContradictionDebt: &on}).ContradictionDebt {
		t.Error("explicit true was not honored")
	}
}

// TestResolvePlasticity_ContradictionDebt_NilConfigIsTodayPlusTheNewField is the
// #713 pin: adding this knob must not move ANY other resolved value. The
// expectation is built from the resolution of an empty (non-nil) config, which
// takes the same preset path, so the only permitted difference between "no
// config at all" and "an empty config" is nothing — and both must carry
// contradiction_debt:true.
func TestResolvePlasticity_ContradictionDebt_NilConfigIsTodayPlusTheNewField(t *testing.T) {
	fromNil := ResolvePlasticity(nil)
	fromEmpty := ResolvePlasticity(&PlasticityConfig{})
	if !reflect.DeepEqual(fromNil, fromEmpty) {
		t.Fatalf("nil config resolves differently from an empty config:\n nil   = %+v\n empty = %+v", fromNil, fromEmpty)
	}

	// And the JSON the console/API sees carries the field, set true, so an
	// operator reading the resolved config can tell the readout is on rather
	// than having to infer it from an absent key.
	raw, err := json.Marshal(fromNil)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if v, ok := m["contradiction_debt"]; !ok || v != true {
		t.Errorf("resolved JSON contradiction_debt = %v (present=%v), want true", v, ok)
	}
}
