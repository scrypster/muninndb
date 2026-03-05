package config

import (
	"encoding/json"
	"os"
	"path/filepath"
)

const pluginConfigFile = "plugin_config.json"

// PluginConfig holds persistent plugin configuration saved to disk.
// Environment variables always take precedence over values stored here.
type PluginConfig struct {
	// Embed provider settings
	EmbedProvider string `json:"embed_provider"` // "ollama", "openai", "voyage", "local", "none"
	EmbedURL      string `json:"embed_url"`      // provider URL override (ollama) or OpenAI base/provider URL override
	EmbedAPIKey   string `json:"embed_api_key"`  // API key (openai, voyage)

	// Enrich provider settings
	EnrichProvider string `json:"enrich_provider"` // "ollama", "openai", "anthropic"
	EnrichURL      string `json:"enrich_url"`      // full provider URL
	EnrichAPIKey   string `json:"enrich_api_key"`  // API key

	// Per-stage enrichment flags (nil = default true)
	EnrichEntities       *bool  `json:"enrich_entities,omitempty"`
	EnrichRelationships  *bool  `json:"enrich_relationships,omitempty"`
	EnrichClassification *bool  `json:"enrich_classification,omitempty"`
	EnrichSummary        *bool  `json:"enrich_summary,omitempty"`
	EnrichMode           string `json:"enrich_mode,omitempty"` // "full" (default) or "light"
}

// EnrichStageEnabled returns whether a given enrichment stage is enabled.
// Nil pointer fields default to true.
func (c *PluginConfig) EnrichStageEnabled(stage string) bool {
	switch stage {
	case "entities":
		return c.EnrichEntities == nil || *c.EnrichEntities
	case "relationships":
		return c.EnrichRelationships == nil || *c.EnrichRelationships
	case "classification":
		return c.EnrichClassification == nil || *c.EnrichClassification
	case "summary":
		return c.EnrichSummary == nil || *c.EnrichSummary
	default:
		return true
	}
}

// IsLightMode returns true when enrichment should use the light pipeline (summary only).
func (c *PluginConfig) IsLightMode() bool {
	return c.EnrichMode == "light"
}

// LoadPluginConfig reads plugin_config.json from dataDir.
// Returns an empty PluginConfig (not an error) if the file does not exist.
func LoadPluginConfig(dataDir string) (PluginConfig, error) {
	path := filepath.Join(dataDir, pluginConfigFile)
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return PluginConfig{}, nil
	}
	if err != nil {
		return PluginConfig{}, err
	}
	var cfg PluginConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return PluginConfig{}, err
	}
	return cfg, nil
}

// SavePluginConfig writes cfg to plugin_config.json in dataDir.
func SavePluginConfig(dataDir string, cfg PluginConfig) error {
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dataDir, pluginConfigFile), data, 0600)
}
