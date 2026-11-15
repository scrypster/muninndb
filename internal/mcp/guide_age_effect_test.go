package mcp

import (
	"strings"
	"testing"

	"github.com/scrypster/muninndb/internal/auth"
)

// TestGuide_AgeEffectLineDerivedFromScoring: the guide states what age does to
// recall, derived from the vault's effective scoring fusion — never from a
// decay setting scoring does not read. Under ACT-R (every shipped preset) age
// and access history change neither findability nor absolute score, so the line is the
// same for every preset's decay exponent; weighted_sum keeps its age terms and
// says so with the resolved weights; rrf has none. The writer-declared
// valid_until is named as the way to retire a memory, since age no longer does.
func TestGuide_AgeEffectLineDerivedFromScoring(t *testing.T) {
	stats := engineStats{EngramCount: 3}
	gen := func(cfg *auth.PlasticityConfig) string {
		return generateGuide("railway", auth.ResolvePlasticity(cfg), stats, "")
	}
	ws, rrf := "weighted_sum", "rrf"
	actr := gen(nil)
	if !strings.Contains(actr, "- Age effect on recall: none") {
		t.Fatalf("ACT-R guide lacks the derived age line:\n%s", actr)
	}
	if !strings.Contains(actr, "valid_until") {
		t.Errorf("guide must name valid_until as the way to retire a memory")
	}
	// The decay exponent is inert under ACT-R: changing it must not change
	// a byte of the guide (it would be describing config nothing reads).
	for _, preset := range []string{"reference", "scratchpad", "knowledge-graph"} {
		r := auth.ResolvePlasticity(nil)
		r.ACTRDecay = auth.ResolvePlasticity(&auth.PlasticityConfig{Preset: preset}).ACTRDecay
		if got := generateGuide("railway", r, stats, ""); got != actr {
			t.Errorf("guide changes with %s's actr_decay, which ACT-R recall scoring does not read", preset)
		}
	}
	w := gen(&auth.PlasticityConfig{ScoringFusion: &ws})
	if !strings.Contains(w, "- Age effect on recall: older memories score lower") {
		t.Errorf("weighted_sum guide must say age lowers scores:\n%s", w)
	}
	if r := gen(&auth.PlasticityConfig{ScoringFusion: &rrf}); !strings.Contains(r, "- Age effect on recall: none") {
		t.Errorf("rrf guide must say age has no effect:\n%s", r)
	}
}
