package engine

import (
	"testing"

	"github.com/scrypster/muninndb/internal/auth"
)

// ---------------------------------------------------------------------------
// COG-26: resolveSemanticBaseline resolution-order pins.
//
// These are unit pins on the resolution policy itself (registry hit, unknown
// model -> identity+WARN dedup, plasticity override precedence and range
// validation) — the end-to-end RED/GREEN behavior against a real bge-small
// embedder lives in internal/engine/activation/semantic_abstention_test.go.
// ---------------------------------------------------------------------------

func TestResolveSemanticBaseline_RegistryHit(t *testing.T) {
	eng, _, _, cleanup := testEnvWithAuth(t)
	defer cleanup()
	eng.embedModelName = "bge-small-en-v1.5"

	ws := eng.store.ResolveVaultPrefix("v-registry-hit")
	b := eng.resolveSemanticBaseline("v-registry-hit", ws, auth.ResolvePlasticity(nil))
	if b != 0.520 {
		t.Errorf("resolveSemanticBaseline for bge-small-en-v1.5 = %v, want 0.520 (the registered constant)", b)
	}
}

func TestResolveSemanticBaseline_UnknownModel_IdentityFallback(t *testing.T) {
	eng, _, _, cleanup := testEnvWithAuth(t)
	defer cleanup()
	eng.embedModelName = "some-future-embedder-v3"

	ws := eng.store.ResolveVaultPrefix("v-unknown-model")
	b := eng.resolveSemanticBaseline("v-unknown-model", ws, auth.ResolvePlasticity(nil))
	if b != 0 {
		t.Errorf("resolveSemanticBaseline for an unregistered model = %v, want 0 (identity transform, never a guessed floor)", b)
	}
}

func TestResolveSemanticBaseline_EmptyModel_IdentityFallback(t *testing.T) {
	// No process-wide EmbedModelName wired (embedded/library use, or a
	// pre-registry-population vault) — the "we don't know what embedded
	// this" case must never guess a floor.
	eng, _, _, cleanup := testEnvWithAuth(t)
	defer cleanup()

	ws := eng.store.ResolveVaultPrefix("v-empty-model")
	b := eng.resolveSemanticBaseline("v-empty-model", ws, auth.ResolvePlasticity(nil))
	if b != 0 {
		t.Errorf("resolveSemanticBaseline with no model recorded = %v, want 0 (identity transform)", b)
	}
}

func TestResolveSemanticBaseline_UnknownModel_WarnsOncePerModel(t *testing.T) {
	eng, _, _, cleanup := testEnvWithAuth(t)
	defer cleanup()
	eng.embedModelName = "warn-dedup-probe-model"

	ws := eng.store.ResolveVaultPrefix("v-warn-dedup")
	resolved := auth.ResolvePlasticity(nil)
	for i := 0; i < 5; i++ {
		eng.resolveSemanticBaseline("v-warn-dedup", ws, resolved)
	}
	if _, warned := eng.warnedUnknownEmbedModels.Load("warn-dedup-probe-model"); !warned {
		t.Error("expected the unknown model to be recorded in warnedUnknownEmbedModels after resolution")
	}
	// Dedup is a log-spam guard, not independently observable via the return
	// value beyond "still resolves to identity every call" — covered above.
}

func TestResolveSemanticBaseline_PlasticityOverride_TakesPrecedence(t *testing.T) {
	eng, _, _, cleanup := testEnvWithAuth(t)
	defer cleanup()
	// Even with a registered model, an explicit override wins.
	eng.embedModelName = "bge-small-en-v1.5"

	override := 0.3
	resolved := auth.ResolvePlasticity(&auth.PlasticityConfig{SemanticFloor: &override})
	ws := eng.store.ResolveVaultPrefix("v-override")
	b := eng.resolveSemanticBaseline("v-override", ws, resolved)
	if b != 0.3 {
		t.Errorf("resolveSemanticBaseline with SemanticFloor override = %v, want 0.3 (explicit override, ignoring the registry)", b)
	}
}

func TestResolveSemanticBaseline_PlasticityOverride_ZeroDisablesFloor(t *testing.T) {
	eng, _, _, cleanup := testEnvWithAuth(t)
	defer cleanup()
	eng.embedModelName = "bge-small-en-v1.5"

	zero := 0.0
	resolved := auth.ResolvePlasticity(&auth.PlasticityConfig{SemanticFloor: &zero})
	ws := eng.store.ResolveVaultPrefix("v-override-zero")
	b := eng.resolveSemanticBaseline("v-override-zero", ws, resolved)
	if b != 0 {
		t.Errorf("resolveSemanticBaseline with explicit SemanticFloor=0 = %v, want 0 (operator-disabled floor)", b)
	}
}

func TestResolveSemanticBaseline_PlasticityOverride_OutOfRangeRejectedFallsBackToRegistry(t *testing.T) {
	eng, _, _, cleanup := testEnvWithAuth(t)
	defer cleanup()
	eng.embedModelName = "bge-small-en-v1.5"

	tooHigh := 1.5
	resolved := auth.ResolvePlasticity(&auth.PlasticityConfig{SemanticFloor: &tooHigh})
	ws := eng.store.ResolveVaultPrefix("v-override-bad")
	b := eng.resolveSemanticBaseline("v-override-bad", ws, resolved)
	if b != 0.520 {
		t.Errorf("resolveSemanticBaseline with out-of-range SemanticFloor=1.5 = %v, want 0.520 "+
			"(rejected override must fall back to the registry, never silently zero every match)", b)
	}
}

// semanticFloorExplicitlyDisabled feeds the relevance-band phase's
// semantic_floor_disabled basis (G6 refute of #773, finding 3): true ONLY for
// an explicit `semantic_floor: 0`. An unset override, a positive override,
// and an out-of-range override (which resolveSemanticBaseline rejects and
// falls through to the registry) must all be false — those vaults either have
// a real baseline or a genuinely missing one, and naming the wrong cause is
// the defect this exists to remove.
func TestSemanticFloorExplicitlyDisabled(t *testing.T) {
	zero, positive, tooHigh := 0.0, 0.3, 1.5
	tests := []struct {
		name string
		cfg  *auth.PlasticityConfig
		want bool
	}{
		{"no override", nil, false},
		{"explicit zero disables", &auth.PlasticityConfig{SemanticFloor: &zero}, true},
		{"positive override is a floor, not a disable", &auth.PlasticityConfig{SemanticFloor: &positive}, false},
		{"out-of-range override is rejected, not a disable", &auth.PlasticityConfig{SemanticFloor: &tooHigh}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := semanticFloorExplicitlyDisabled(auth.ResolvePlasticity(tt.cfg)); got != tt.want {
				t.Errorf("semanticFloorExplicitlyDisabled = %v, want %v", got, tt.want)
			}
		})
	}
}
