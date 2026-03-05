package auth

import "testing"

func TestResolvePlasticity_NilUsesDefault(t *testing.T) {
	r := ResolvePlasticity(nil)
	if r.HopDepth != 2 {
		t.Errorf("want HopDepth=2, got %d", r.HopDepth)
	}
	if !r.HebbianEnabled {
		t.Error("want HebbianEnabled=true")
	}
	if !r.TemporalEnabled {
		t.Error("want TemporalEnabled=true")
	}
}

func TestResolvePlasticity_ScratchpadPreset(t *testing.T) {
	r := ResolvePlasticity(&PlasticityConfig{Preset: "scratchpad"})
	if r.HopDepth != 0 {
		t.Errorf("scratchpad HopDepth want 0, got %d", r.HopDepth)
	}
	if r.HebbianEnabled {
		t.Error("scratchpad: want HebbianEnabled=false")
	}
	if r.TemporalHalflife != 7 {
		t.Errorf("scratchpad TemporalHalflife want 7, got %f", r.TemporalHalflife)
	}
}

func TestResolvePlasticity_ReferencePreset(t *testing.T) {
	r := ResolvePlasticity(&PlasticityConfig{Preset: "reference"})
	if r.TemporalEnabled {
		t.Error("reference: want TemporalEnabled=false")
	}
	if r.RelevanceFloor != 1.0 {
		t.Errorf("reference RelevanceFloor want 1.0, got %f", r.RelevanceFloor)
	}
}

func TestResolvePlasticity_KnowledgeGraphPreset(t *testing.T) {
	r := ResolvePlasticity(&PlasticityConfig{Preset: "knowledge-graph"})
	if r.HopDepth != 4 {
		t.Errorf("knowledge-graph HopDepth want 4, got %d", r.HopDepth)
	}
}

func TestResolvePlasticity_PointerOverride(t *testing.T) {
	hd := 5
	r := ResolvePlasticity(&PlasticityConfig{
		Preset:   "default",
		HopDepth: &hd,
	})
	if r.HopDepth != 5 {
		t.Errorf("override HopDepth want 5, got %d", r.HopDepth)
	}
	if !r.HebbianEnabled {
		t.Error("want HebbianEnabled=true (from default)")
	}
}

func TestResolvePlasticity_BoolOverride(t *testing.T) {
	f := false
	r := ResolvePlasticity(&PlasticityConfig{
		Preset:         "default",
		HebbianEnabled: &f,
	})
	if r.HebbianEnabled {
		t.Error("explicit false override should set HebbianEnabled=false")
	}
}

func TestResolvePlasticity_InvalidPresetFallsToDefault(t *testing.T) {
	r := ResolvePlasticity(&PlasticityConfig{Preset: "bogus"})
	if r.HopDepth != 2 {
		t.Errorf("invalid preset should fall to default, want HopDepth=2, got %d", r.HopDepth)
	}
}

func TestValidPlasticityPreset(t *testing.T) {
	if !ValidPlasticityPreset("default")         { t.Error("default should be valid") }
	if !ValidPlasticityPreset("reference")       { t.Error("reference should be valid") }
	if !ValidPlasticityPreset("scratchpad")      { t.Error("scratchpad should be valid") }
	if !ValidPlasticityPreset("knowledge-graph") { t.Error("knowledge-graph should be valid") }
	if ValidPlasticityPreset("bogus")            { t.Error("bogus should not be valid") }
}

func TestPlasticityConfig_TraversalProfileField(t *testing.T) {
	profile := "causal"
	cfg := &PlasticityConfig{TraversalProfile: &profile}
	r := ResolvePlasticity(cfg)
	if r.TraversalProfile != "causal" {
		t.Errorf("expected TraversalProfile 'causal', got %q", r.TraversalProfile)
	}
}

func TestPlasticityConfig_NoTraversalProfileDefaultsEmpty(t *testing.T) {
	r := ResolvePlasticity(nil)
	if r.TraversalProfile != "" {
		t.Errorf("nil config should resolve TraversalProfile to empty string, got %q", r.TraversalProfile)
	}
}

func TestPlasticityConfig_TraversalProfilePresetOverride(t *testing.T) {
	// Setting TraversalProfile as override works regardless of preset
	profile := "adversarial"
	cfg := &PlasticityConfig{
		Preset:           "knowledge-graph",
		TraversalProfile: &profile,
	}
	r := ResolvePlasticity(cfg)
	if r.TraversalProfile != "adversarial" {
		t.Errorf("expected 'adversarial', got %q", r.TraversalProfile)
	}
}

func TestPlasticityConfig_NilTraversalProfileIsEmpty(t *testing.T) {
	// When PlasticityConfig exists but TraversalProfile is not set, it should be empty (use inference)
	cfg := &PlasticityConfig{Preset: "default"}
	r := ResolvePlasticity(cfg)
	if r.TraversalProfile != "" {
		t.Errorf("unset TraversalProfile should resolve to empty string, got %q", r.TraversalProfile)
	}
}

func TestPAS_DefaultPreset(t *testing.T) {
	r := ResolvePlasticity(nil)
	if !r.PredictiveActivation {
		t.Error("default: want PredictiveActivation=true")
	}
	if r.PASMaxInjections != 5 {
		t.Errorf("default: want PASMaxInjections=5, got %d", r.PASMaxInjections)
	}
}

func TestPAS_ScratchpadPreset(t *testing.T) {
	r := ResolvePlasticity(&PlasticityConfig{Preset: "scratchpad"})
	if r.PredictiveActivation {
		t.Error("scratchpad: want PredictiveActivation=false")
	}
	if r.PASMaxInjections != 0 {
		t.Errorf("scratchpad: want PASMaxInjections=0, got %d", r.PASMaxInjections)
	}
}

func TestPAS_ReferencePreset(t *testing.T) {
	r := ResolvePlasticity(&PlasticityConfig{Preset: "reference"})
	if !r.PredictiveActivation {
		t.Error("reference: want PredictiveActivation=true")
	}
	if r.PASMaxInjections != 5 {
		t.Errorf("reference: want PASMaxInjections=5, got %d", r.PASMaxInjections)
	}
}

func TestPAS_KnowledgeGraphPreset(t *testing.T) {
	r := ResolvePlasticity(&PlasticityConfig{Preset: "knowledge-graph"})
	if !r.PredictiveActivation {
		t.Error("knowledge-graph: want PredictiveActivation=true")
	}
	if r.PASMaxInjections != 5 {
		t.Errorf("knowledge-graph: want PASMaxInjections=5, got %d", r.PASMaxInjections)
	}
}

func TestPAS_OverrideDisable(t *testing.T) {
	f := false
	r := ResolvePlasticity(&PlasticityConfig{
		PredictiveActivation: &f,
	})
	if r.PredictiveActivation {
		t.Error("override false should disable PredictiveActivation")
	}
}

func TestPAS_OverrideMaxInjections(t *testing.T) {
	v := 3
	r := ResolvePlasticity(&PlasticityConfig{
		PASMaxInjections: &v,
	})
	if r.PASMaxInjections != 3 {
		t.Errorf("override want PASMaxInjections=3, got %d", r.PASMaxInjections)
	}
}

func TestPAS_MaxInjectionsClampedLow(t *testing.T) {
	v := -5
	r := ResolvePlasticity(&PlasticityConfig{
		PASMaxInjections: &v,
	})
	if r.PASMaxInjections != 0 {
		t.Errorf("negative should clamp to 0, got %d", r.PASMaxInjections)
	}
}

func TestPAS_MaxInjectionsClampedHigh(t *testing.T) {
	v := 99
	r := ResolvePlasticity(&PlasticityConfig{
		PASMaxInjections: &v,
	})
	if r.PASMaxInjections != 10 {
		t.Errorf("above 10 should clamp to 10, got %d", r.PASMaxInjections)
	}
}

func TestBehaviorMode_DefaultPreset(t *testing.T) {
	r := ResolvePlasticity(nil)
	if r.BehaviorMode != "autonomous" {
		t.Errorf("default: want BehaviorMode=autonomous, got %q", r.BehaviorMode)
	}
}

func TestBehaviorMode_ScratchpadPreset(t *testing.T) {
	r := ResolvePlasticity(&PlasticityConfig{Preset: "scratchpad"})
	if r.BehaviorMode != "selective" {
		t.Errorf("scratchpad: want BehaviorMode=selective, got %q", r.BehaviorMode)
	}
}

func TestBehaviorMode_Override(t *testing.T) {
	mode := "prompted"
	r := ResolvePlasticity(&PlasticityConfig{BehaviorMode: &mode})
	if r.BehaviorMode != "prompted" {
		t.Errorf("override: want BehaviorMode=prompted, got %q", r.BehaviorMode)
	}
}

func TestBehaviorMode_InvalidFallsToAutonomous(t *testing.T) {
	mode := "invalid-mode"
	r := ResolvePlasticity(&PlasticityConfig{BehaviorMode: &mode})
	if r.BehaviorMode != "autonomous" {
		t.Errorf("invalid mode should fall back to autonomous, got %q", r.BehaviorMode)
	}
}

func TestBehaviorMode_CustomWithInstructions(t *testing.T) {
	mode := "custom"
	instr := "Remember only code patterns."
	r := ResolvePlasticity(&PlasticityConfig{
		BehaviorMode:         &mode,
		BehaviorInstructions: &instr,
	})
	if r.BehaviorMode != "custom" {
		t.Errorf("want BehaviorMode=custom, got %q", r.BehaviorMode)
	}
	if r.BehaviorInstructions != "Remember only code patterns." {
		t.Errorf("want BehaviorInstructions=%q, got %q", instr, r.BehaviorInstructions)
	}
}

func TestBehaviorMode_AllPresetsHaveMode(t *testing.T) {
	presets := []string{"default", "reference", "scratchpad", "knowledge-graph"}
	for _, preset := range presets {
		t.Run(preset, func(t *testing.T) {
			r := ResolvePlasticity(&PlasticityConfig{Preset: preset})
			if r.BehaviorMode == "" {
				t.Errorf("preset %q: BehaviorMode should not be empty", preset)
			}
		})
	}
}

// InlineEnrichment tests

func TestInlineEnrichment_DefaultPreset(t *testing.T) {
	r := ResolvePlasticity(nil)
	if r.InlineEnrichment != "caller_preferred" {
		t.Errorf("default: want InlineEnrichment=caller_preferred, got %q", r.InlineEnrichment)
	}
}

func TestInlineEnrichment_AllPresetsHaveValue(t *testing.T) {
	presets := []string{"default", "reference", "scratchpad", "knowledge-graph"}
	for _, preset := range presets {
		t.Run(preset, func(t *testing.T) {
			r := ResolvePlasticity(&PlasticityConfig{Preset: preset})
			if r.InlineEnrichment == "" {
				t.Errorf("preset %q: InlineEnrichment should not be empty", preset)
			}
		})
	}
}

func TestInlineEnrichment_Override(t *testing.T) {
	mode := "caller_only"
	r := ResolvePlasticity(&PlasticityConfig{InlineEnrichment: &mode})
	if r.InlineEnrichment != "caller_only" {
		t.Errorf("override: want InlineEnrichment=caller_only, got %q", r.InlineEnrichment)
	}
}

func TestInlineEnrichment_BackgroundOnly(t *testing.T) {
	mode := "background_only"
	r := ResolvePlasticity(&PlasticityConfig{InlineEnrichment: &mode})
	if r.InlineEnrichment != "background_only" {
		t.Errorf("want background_only, got %q", r.InlineEnrichment)
	}
}

func TestInlineEnrichment_Disabled(t *testing.T) {
	mode := "disabled"
	r := ResolvePlasticity(&PlasticityConfig{InlineEnrichment: &mode})
	if r.InlineEnrichment != "disabled" {
		t.Errorf("want disabled, got %q", r.InlineEnrichment)
	}
}

func TestInlineEnrichment_InvalidFallsToCallerPreferred(t *testing.T) {
	mode := "invalid-mode"
	r := ResolvePlasticity(&PlasticityConfig{InlineEnrichment: &mode})
	if r.InlineEnrichment != "caller_preferred" {
		t.Errorf("invalid mode should fall back to caller_preferred, got %q", r.InlineEnrichment)
	}
}

func TestInlineEnrichment_CallerPreferred(t *testing.T) {
	mode := "caller_preferred"
	r := ResolvePlasticity(&PlasticityConfig{InlineEnrichment: &mode})
	if r.InlineEnrichment != "caller_preferred" {
		t.Errorf("want caller_preferred, got %q", r.InlineEnrichment)
	}
}

func TestEnrichmentEnabled_DefaultTrue(t *testing.T) {
	r := ResolvePlasticity(nil)
	if !r.EnrichmentEnabled {
		t.Error("EnrichmentEnabled should default to true")
	}
}

func TestEnrichmentEnabled_ExplicitFalse(t *testing.T) {
	f := false
	r := ResolvePlasticity(&PlasticityConfig{EnrichmentEnabled: &f})
	if r.EnrichmentEnabled {
		t.Error("EnrichmentEnabled should be false when explicitly set")
	}
}

func TestEnrichmentEnabled_ExplicitTrue(t *testing.T) {
	tr := true
	r := ResolvePlasticity(&PlasticityConfig{EnrichmentEnabled: &tr})
	if !r.EnrichmentEnabled {
		t.Error("EnrichmentEnabled should be true when explicitly set")
	}
}

func TestEnrichmentEnabled_AllPresetsDefaultTrue(t *testing.T) {
	presets := []string{"default", "reference", "scratchpad", "knowledge-graph"}
	for _, name := range presets {
		r := ResolvePlasticity(&PlasticityConfig{Preset: name})
		if !r.EnrichmentEnabled {
			t.Errorf("preset %q: EnrichmentEnabled should default to true", name)
		}
	}
}

// --- RecallMode tests ---

func TestValidRecallMode_AllValid(t *testing.T) {
	for _, mode := range []string{"semantic", "recent", "balanced", "deep"} {
		if !ValidRecallMode(mode) {
			t.Errorf("ValidRecallMode(%q) = false, want true", mode)
		}
	}
}

func TestValidRecallMode_Invalid(t *testing.T) {
	for _, mode := range []string{"turbo", "fast", "actr", "cgdn", "", "SEMANTIC"} {
		if ValidRecallMode(mode) {
			t.Errorf("ValidRecallMode(%q) = true, want false", mode)
		}
	}
}

func TestRecallMode_AllPresetsDefaultBalanced(t *testing.T) {
	presets := []string{"default", "reference", "scratchpad", "knowledge-graph"}
	for _, name := range presets {
		r := ResolvePlasticity(&PlasticityConfig{Preset: name})
		if r.RecallMode != "balanced" {
			t.Errorf("preset %q: RecallMode = %q, want %q", name, r.RecallMode, "balanced")
		}
	}
}

func TestRecallMode_NilConfigDefaultsBalanced(t *testing.T) {
	r := ResolvePlasticity(nil)
	if r.RecallMode != "balanced" {
		t.Errorf("nil config: RecallMode = %q, want %q", r.RecallMode, "balanced")
	}
}

func TestRecallMode_Override(t *testing.T) {
	mode := "semantic"
	r := ResolvePlasticity(&PlasticityConfig{RecallMode: &mode})
	if r.RecallMode != "semantic" {
		t.Errorf("RecallMode = %q, want %q", r.RecallMode, "semantic")
	}
}

func TestRecallMode_InvalidOverrideKeepsPreset(t *testing.T) {
	mode := "turbo"
	r := ResolvePlasticity(&PlasticityConfig{RecallMode: &mode})
	if r.RecallMode != "balanced" {
		t.Errorf("invalid override: RecallMode = %q, want %q (preset default)", r.RecallMode, "balanced")
	}
}

func TestRecallMode_AllFourModesOverride(t *testing.T) {
	for _, mode := range []string{"semantic", "recent", "balanced", "deep"} {
		m := mode
		r := ResolvePlasticity(&PlasticityConfig{RecallMode: &m})
		if r.RecallMode != mode {
			t.Errorf("override %q: RecallMode = %q", mode, r.RecallMode)
		}
	}
}

func TestLookupRecallMode_AllKnown(t *testing.T) {
	for _, name := range []string{"semantic", "recent", "balanced", "deep"} {
		p, err := LookupRecallMode(name)
		if err != nil {
			t.Errorf("LookupRecallMode(%q): unexpected error: %v", name, err)
		}
		_ = p
	}
}

func TestLookupRecallMode_Unknown(t *testing.T) {
	_, err := LookupRecallMode("turbo")
	if err == nil {
		t.Error("LookupRecallMode(turbo): expected error, got nil")
	}
}

func TestLookupRecallMode_SemanticValues(t *testing.T) {
	p, err := LookupRecallMode("semantic")
	if err != nil {
		t.Fatalf("LookupRecallMode(semantic): %v", err)
	}
	if p.Threshold != 0.3 {
		t.Errorf("semantic Threshold = %v, want 0.3", p.Threshold)
	}
	if p.SemanticSimilarity != 0.8 {
		t.Errorf("semantic SemanticSimilarity = %v, want 0.8", p.SemanticSimilarity)
	}
	if !p.DisableACTR {
		t.Error("semantic DisableACTR should be true")
	}
}

func TestLookupRecallMode_DeepValues(t *testing.T) {
	p, err := LookupRecallMode("deep")
	if err != nil {
		t.Fatalf("LookupRecallMode(deep): %v", err)
	}
	if p.MaxHops != 4 {
		t.Errorf("deep MaxHops = %d, want 4", p.MaxHops)
	}
	if p.Threshold != 0.1 {
		t.Errorf("deep Threshold = %v, want 0.1", p.Threshold)
	}
}

func TestLookupRecallMode_RecentValues(t *testing.T) {
	p, err := LookupRecallMode("recent")
	if err != nil {
		t.Fatalf("LookupRecallMode(recent): %v", err)
	}
	if p.Recency != 0.7 {
		t.Errorf("recent Recency = %v, want 0.7", p.Recency)
	}
	if p.MaxHops != 1 {
		t.Errorf("recent MaxHops = %d, want 1", p.MaxHops)
	}
}

func TestLookupRecallMode_BalancedIsZero(t *testing.T) {
	p, err := LookupRecallMode("balanced")
	if err != nil {
		t.Fatalf("LookupRecallMode(balanced): %v", err)
	}
	if p.MaxHops != 0 || p.Threshold != 0 || p.SemanticSimilarity != 0 || p.Recency != 0 {
		t.Errorf("balanced should be all zero values, got %+v", p)
	}
}

func TestPlasticityConfig_ArchiveThreshold_Default(t *testing.T) {
	r := ResolvePlasticity(&PlasticityConfig{Preset: "default"})
	if r.ArchiveThreshold != 0.05 {
		t.Errorf("default ArchiveThreshold: got %v, want 0.05", r.ArchiveThreshold)
	}
}

func TestPlasticityConfig_ArchiveThreshold_Override(t *testing.T) {
	val := 0.10
	r := ResolvePlasticity(&PlasticityConfig{ArchiveThreshold: &val})
	if r.ArchiveThreshold != 0.10 {
		t.Errorf("overridden ArchiveThreshold: got %v, want 0.10", r.ArchiveThreshold)
	}
}
