package plugin

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
)

// ---------------------------------------------------------------------------
// Mock plugins for health checker tests — named with "health" prefix to
// avoid conflict with mocks in registry_test.go.
// ---------------------------------------------------------------------------

type healthMockEmbed struct {
	name      string
	dim       int
	embedErr  error
	callCount atomic.Int32
}

func (m *healthMockEmbed) Name() string                                          { return m.name }
func (m *healthMockEmbed) Tier() PluginTier                                      { return TierEmbed }
func (m *healthMockEmbed) Init(_ context.Context, _ PluginConfig) error          { return nil }
func (m *healthMockEmbed) Close() error                                          { return nil }
func (m *healthMockEmbed) Dimension() int                                        { return m.dim }
func (m *healthMockEmbed) MaxBatchSize() int                                     { return 32 }
func (m *healthMockEmbed) Embed(_ context.Context, texts []string) ([]float32, error) {
	m.callCount.Add(1)
	if m.embedErr != nil {
		return nil, m.embedErr
	}
	out := make([]float32, len(texts)*m.dim)
	return out, nil
}

type healthMockEnrich struct {
	name      string
	enrichErr error
	callCount atomic.Int32
}

func (m *healthMockEnrich) Name() string                                         { return m.name }
func (m *healthMockEnrich) Tier() PluginTier                                     { return TierEnrich }
func (m *healthMockEnrich) Init(_ context.Context, _ PluginConfig) error         { return nil }
func (m *healthMockEnrich) Close() error                                         { return nil }
func (m *healthMockEnrich) Enrich(_ context.Context, _ *Engram) (*EnrichmentResult, error) {
	m.callCount.Add(1)
	if m.enrichErr != nil {
		return nil, m.enrichErr
	}
	return &EnrichmentResult{}, nil
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestHealthChecker_StartStop(t *testing.T) {
	reg := NewRegistry()
	hc := NewHealthChecker(reg)

	ctx, cancel := context.WithCancel(context.Background())
	hc.Start(ctx)
	cancel()
	hc.Stop()
}

func TestHealthChecker_HealthyEmbed(t *testing.T) {
	reg := NewRegistry()
	embed := &healthMockEmbed{name: "test-embed", dim: 384}
	if err := reg.Register(embed); err != nil {
		t.Fatal(err)
	}

	hc := &PluginHealthChecker{
		registry: reg,
		stopCh:   make(chan struct{}),
	}

	err := hc.probeEmbed(context.Background(), embed)
	if err != nil {
		t.Errorf("healthy embed probe should not error: %v", err)
	}
	if embed.callCount.Load() != 1 {
		t.Errorf("expected 1 embed call, got %d", embed.callCount.Load())
	}
}

func TestHealthChecker_UnhealthyEmbed(t *testing.T) {
	reg := NewRegistry()
	embed := &healthMockEmbed{name: "failing-embed", dim: 384, embedErr: errors.New("connection refused")}
	if err := reg.Register(embed); err != nil {
		t.Fatal(err)
	}

	hc := &PluginHealthChecker{
		registry: reg,
		stopCh:   make(chan struct{}),
	}

	err := hc.probeEmbed(context.Background(), embed)
	if err == nil {
		t.Error("unhealthy embed probe should return error")
	}
}

func TestHealthChecker_HealthyEnrich(t *testing.T) {
	reg := NewRegistry()
	enrich := &healthMockEnrich{name: "test-enrich"}
	if err := reg.Register(enrich); err != nil {
		t.Fatal(err)
	}

	hc := &PluginHealthChecker{
		registry: reg,
		stopCh:   make(chan struct{}),
	}

	err := hc.probeEnrich(context.Background(), enrich)
	if err != nil {
		t.Errorf("healthy enrich probe should not error: %v", err)
	}
}

func TestHealthChecker_UnhealthyEnrich(t *testing.T) {
	reg := NewRegistry()
	enrich := &healthMockEnrich{name: "failing-enrich", enrichErr: errors.New("timeout")}
	if err := reg.Register(enrich); err != nil {
		t.Fatal(err)
	}

	hc := &PluginHealthChecker{
		registry: reg,
		stopCh:   make(chan struct{}),
	}

	err := hc.probeEnrich(context.Background(), enrich)
	if err == nil {
		t.Error("unhealthy enrich probe should return error")
	}
}

func TestRegistry_GetEnrich(t *testing.T) {
	reg := NewRegistry()
	enrich := &healthMockEnrich{name: "test-enrich"}
	if err := reg.Register(enrich); err != nil {
		t.Fatal(err)
	}

	got := reg.GetEnrich()
	if got == nil {
		t.Fatal("expected non-nil enrich plugin")
	}
	if got.Name() != "test-enrich" {
		t.Errorf("got name %q, want %q", got.Name(), "test-enrich")
	}
}

func TestRegistry_HasEnrich(t *testing.T) {
	reg := NewRegistry()
	if reg.HasEnrich() {
		t.Error("should not have enrich before registration")
	}

	enrich := &healthMockEnrich{name: "test-enrich"}
	reg.Register(enrich)

	if !reg.HasEnrich() {
		t.Error("should have enrich after registration")
	}
}

func TestRegistry_SetUnhealthy(t *testing.T) {
	reg := NewRegistry()
	embed := &healthMockEmbed{name: "test-embed", dim: 384}
	reg.Register(embed)

	if !reg.HasEmbed() {
		t.Fatal("should have embed")
	}

	reg.SetUnhealthy("test-embed", errors.New("test error"))

	reg.mu.RLock()
	isHealthy := reg.healthy["test-embed"]
	reg.mu.RUnlock()
	if isHealthy {
		t.Error("plugin should be marked unhealthy")
	}
}

func TestRegistry_SetHealthyRecovers(t *testing.T) {
	reg := NewRegistry()
	embed := &healthMockEmbed{name: "test-embed", dim: 384}
	reg.Register(embed)

	reg.SetUnhealthy("test-embed", errors.New("down"))
	reg.SetHealthy("test-embed", true)

	reg.mu.RLock()
	isHealthy := reg.healthy["test-embed"]
	reg.mu.RUnlock()
	if !isHealthy {
		t.Error("plugin should be marked healthy after recovery")
	}
}

func TestRegistry_ConcurrentAccess(t *testing.T) {
	reg := NewRegistry()
	embed := &healthMockEmbed{name: "concurrent-embed", dim: 384}
	reg.Register(embed)

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			reg.HasEmbed()
			reg.HasEnrich()
			reg.GetEmbed()
			reg.GetEnrich()
		}()
	}
	wg.Wait()
}
