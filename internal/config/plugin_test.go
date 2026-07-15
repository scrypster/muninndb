package config

import (
	"os"
	"path/filepath"
	"testing"
)

func boolPtr(b bool) *bool { return &b }

func TestPluginConfig_UserModelKeysRoundTrip(t *testing.T) {
	dir := t.TempDir()
	in := PluginConfig{
		EmbedProvider:      "local",
		EmbedModelPath:     "/opt/models/multilingual-e5-small/model_quantized.onnx",
		EmbedTokenizerPath: "/opt/models/multilingual-e5-small/tokenizer.json",
		EmbedPooling:       "mean",
		EmbedMaxTokens:     512,
		EmbedQueryPrefix:   "query: ",
		EmbedPassagePrefix: "passage: ",
	}
	if err := SavePluginConfig(dir, in); err != nil {
		t.Fatalf("SavePluginConfig: %v", err)
	}
	out, err := LoadPluginConfig(dir)
	if err != nil {
		t.Fatalf("LoadPluginConfig: %v", err)
	}
	if out != in {
		t.Fatalf("round-trip mismatch:\n got %+v\nwant %+v", out, in)
	}
}

// TestPluginConfig_OldFileWithoutUserModelKeys pins backward compatibility:
// a config written before the user-model keys existed loads with zero values.
func TestPluginConfig_OldFileWithoutUserModelKeys(t *testing.T) {
	dir := t.TempDir()
	old := []byte(`{"embed_provider":"ollama","embed_url":"ollama://localhost:11434/some-model"}`)
	if err := os.WriteFile(filepath.Join(dir, "plugin_config.json"), old, 0o600); err != nil {
		t.Fatalf("write old config: %v", err)
	}
	out, err := LoadPluginConfig(dir)
	if err != nil {
		t.Fatalf("LoadPluginConfig: %v", err)
	}
	if out.EmbedProvider != "ollama" {
		t.Errorf("EmbedProvider = %q, want ollama", out.EmbedProvider)
	}
	if out.EmbedModelPath != "" || out.EmbedTokenizerPath != "" || out.EmbedPooling != "" ||
		out.EmbedMaxTokens != 0 || out.EmbedQueryPrefix != "" || out.EmbedPassagePrefix != "" {
		t.Errorf("user-model keys must be zero for an old config file, got %+v", out)
	}
}

func TestEnrichStageEnabled_DefaultsTrue(t *testing.T) {
	cfg := &PluginConfig{}
	for _, stage := range []string{"entities", "relationships", "classification", "summary"} {
		if !cfg.EnrichStageEnabled(stage) {
			t.Errorf("stage %q should default to enabled", stage)
		}
	}
}

func TestEnrichStageEnabled_DisableEntities(t *testing.T) {
	cfg := &PluginConfig{EnrichEntities: boolPtr(false)}
	if cfg.EnrichStageEnabled("entities") {
		t.Error("entities should be disabled")
	}
	if !cfg.EnrichStageEnabled("relationships") {
		t.Error("relationships should still be enabled")
	}
}

func TestEnrichStageEnabled_DisableRelationships(t *testing.T) {
	cfg := &PluginConfig{EnrichRelationships: boolPtr(false)}
	if cfg.EnrichStageEnabled("relationships") {
		t.Error("relationships should be disabled")
	}
	if !cfg.EnrichStageEnabled("entities") {
		t.Error("entities should still be enabled")
	}
}

func TestEnrichStageEnabled_DisableClassification(t *testing.T) {
	cfg := &PluginConfig{EnrichClassification: boolPtr(false)}
	if cfg.EnrichStageEnabled("classification") {
		t.Error("classification should be disabled")
	}
}

func TestEnrichStageEnabled_DisableSummary(t *testing.T) {
	cfg := &PluginConfig{EnrichSummary: boolPtr(false)}
	if cfg.EnrichStageEnabled("summary") {
		t.Error("summary should be disabled")
	}
}

func TestEnrichStageEnabled_UnknownStage(t *testing.T) {
	cfg := &PluginConfig{}
	if !cfg.EnrichStageEnabled("unknown") {
		t.Error("unknown stages should default to enabled")
	}
}

func TestIsLightMode(t *testing.T) {
	tests := []struct {
		mode string
		want bool
	}{
		{"light", true},
		{"full", false},
		{"", false},
		{"LIGHT", false},
	}
	for _, tt := range tests {
		cfg := &PluginConfig{EnrichMode: tt.mode}
		if cfg.IsLightMode() != tt.want {
			t.Errorf("IsLightMode() for mode %q = %v, want %v", tt.mode, cfg.IsLightMode(), tt.want)
		}
	}
}
