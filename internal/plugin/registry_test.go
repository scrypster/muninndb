package plugin

import (
	"context"
	"testing"
)

// mockPlugin is a mock plugin for testing
type mockPlugin struct {
	name   string
	tier   PluginTier
	closed bool
}

func (m *mockPlugin) Name() string                               { return m.name }
func (m *mockPlugin) Tier() PluginTier                           { return m.tier }
func (m *mockPlugin) Init(ctx context.Context, cfg PluginConfig) error { return nil }
func (m *mockPlugin) Close() error                              { m.closed = true; return nil }

// mockEmbedPlugin is a mock embed plugin
type mockEmbedPlugin struct {
	mockPlugin
}

func (m *mockEmbedPlugin) Embed(ctx context.Context, texts []string) ([]float32, error) {
	return make([]float32, len(texts)*384), nil
}
func (m *mockEmbedPlugin) Dimension() int    { return 384 }
func (m *mockEmbedPlugin) MaxBatchSize() int { return 32 }

// mockEnrichPlugin is a mock enrich plugin
type mockEnrichPlugin struct {
	mockPlugin
}

func (m *mockEnrichPlugin) Enrich(ctx context.Context, eng *Engram) (*EnrichmentResult, error) {
	return &EnrichmentResult{}, nil
}

func TestRegistryRegisterEmbed(t *testing.T) {
	r := NewRegistry()

	embed := &mockEmbedPlugin{
		mockPlugin: mockPlugin{name: "test-embed", tier: TierEmbed},
	}

	if err := r.Register(embed); err != nil {
		t.Fatalf("failed to register embed plugin: %v", err)
	}

	if !r.HasEmbed() {
		t.Error("HasEmbed() should return true after registering embed plugin")
	}

	if r.GetEmbed() == nil {
		t.Error("GetEmbed() should return the registered plugin")
	}
}

func TestRegistryDuplicateName(t *testing.T) {
	r := NewRegistry()

	p1 := &mockEmbedPlugin{
		mockPlugin: mockPlugin{name: "duplicate", tier: TierEmbed},
	}
	p2 := &mockEnrichPlugin{
		mockPlugin: mockPlugin{name: "duplicate", tier: TierEnrich},
	}

	if err := r.Register(p1); err != nil {
		t.Fatalf("failed to register first plugin: %v", err)
	}

	if err := r.Register(p2); err == nil {
		t.Error("should reject duplicate plugin name")
	}
}

func TestRegistryAtMostOneEmbed(t *testing.T) {
	r := NewRegistry()

	embed1 := &mockEmbedPlugin{
		mockPlugin: mockPlugin{name: "embed1", tier: TierEmbed},
	}
	embed2 := &mockEmbedPlugin{
		mockPlugin: mockPlugin{name: "embed2", tier: TierEmbed},
	}

	if err := r.Register(embed1); err != nil {
		t.Fatalf("failed to register first embed plugin: %v", err)
	}

	if err := r.Register(embed2); err == nil {
		t.Error("should reject second embed plugin")
	}
}

func TestRegistryUnregister(t *testing.T) {
	r := NewRegistry()

	p := &mockEmbedPlugin{
		mockPlugin: mockPlugin{name: "test", tier: TierEmbed},
	}
	if err := r.Register(p); err != nil {
		t.Fatalf("failed to register: %v", err)
	}

	if err := r.Unregister("test"); err != nil {
		t.Fatalf("failed to unregister: %v", err)
	}

	if !p.closed {
		t.Error("plugin should be closed")
	}

	// Verify it's actually removed
	if _, exists := r.plugins["test"]; exists {
		t.Error("plugin should be removed from registry")
	}
}

func TestRegistryList(t *testing.T) {
	r := NewRegistry()

	p1 := &mockEmbedPlugin{
		mockPlugin: mockPlugin{name: "plugin1", tier: TierEmbed},
	}
	p2 := &mockEnrichPlugin{
		mockPlugin: mockPlugin{name: "plugin2", tier: TierEnrich},
	}

	r.Register(p1)
	r.Register(p2)

	list := r.List()
	if len(list) != 2 {
		t.Errorf("expected 2 plugins, got %d", len(list))
	}

	// Check that both plugins are in the list
	names := make(map[string]bool)
	for _, ps := range list {
		names[ps.Name] = true
	}

	if !names["plugin1"] || !names["plugin2"] {
		t.Error("expected both plugins in list")
	}
}

func TestRegistrySetHealthy(t *testing.T) {
	r := NewRegistry()

	embed := &mockEmbedPlugin{
		mockPlugin: mockPlugin{name: "test-embed", tier: TierEmbed},
	}

	r.Register(embed)

	// Initially healthy
	if !r.HasEmbed() {
		t.Error("plugin should start healthy")
	}

	// Mark unhealthy
	r.SetHealthy("test-embed", false)
	if r.HasEmbed() {
		t.Error("HasEmbed() should return false when marked unhealthy")
	}

	// Mark healthy again
	r.SetHealthy("test-embed", true)
	if !r.HasEmbed() {
		t.Error("HasEmbed() should return true when marked healthy")
	}
}
