package enrich

import (
	"context"
	"testing"

	"github.com/scrypster/muninndb/internal/plugin"
	"github.com/scrypster/muninndb/internal/storage"
)

// TestEnrichServiceNew_Ollama tests creating an EnrichService for Ollama.
func TestEnrichServiceNew_Ollama(t *testing.T) {
	es, err := NewEnrichService("ollama://localhost:11434/llama3.2")
	if err != nil {
		t.Fatalf("NewEnrichService failed: %v", err)
	}

	if es.Name() != "enrich-ollama" {
		t.Fatalf("Expected name 'enrich-ollama', got: %q", es.Name())
	}

	if es.Tier() != plugin.TierEnrich {
		t.Fatalf("Expected tier TierEnrich (3), got: %d", es.Tier())
	}

	if es.provCfg.Model != "llama3.2" {
		t.Fatalf("Expected model 'llama3.2', got: %q", es.provCfg.Model)
	}

	_ = es.Close()
}

// TestEnrichServiceNew_OpenAI tests creating an EnrichService for OpenAI.
func TestEnrichServiceNew_OpenAI(t *testing.T) {
	es, err := NewEnrichService("openai://gpt-4o-mini")
	if err != nil {
		t.Fatalf("NewEnrichService failed: %v", err)
	}

	if es.Name() != "enrich-openai" {
		t.Fatalf("Expected name 'enrich-openai', got: %q", es.Name())
	}

	if es.Tier() != plugin.TierEnrich {
		t.Fatalf("Expected tier TierEnrich (3), got: %d", es.Tier())
	}

	if es.provCfg.Model != "gpt-4o-mini" {
		t.Fatalf("Expected model 'gpt-4o-mini', got: %q", es.provCfg.Model)
	}

	_ = es.Close()
}

// TestEnrichServiceNew_Anthropic tests creating an EnrichService for Anthropic.
func TestEnrichServiceNew_Anthropic(t *testing.T) {
	es, err := NewEnrichService("anthropic://claude-haiku")
	if err != nil {
		t.Fatalf("NewEnrichService failed: %v", err)
	}

	if es.Name() != "enrich-anthropic" {
		t.Fatalf("Expected name 'enrich-anthropic', got: %q", es.Name())
	}

	if es.Tier() != plugin.TierEnrich {
		t.Fatalf("Expected tier TierEnrich (3), got: %d", es.Tier())
	}

	if es.provCfg.Model != "claude-haiku" {
		t.Fatalf("Expected model 'claude-haiku', got: %q", es.provCfg.Model)
	}

	_ = es.Close()
}

// TestEnrichServiceNew_InvalidScheme tests error handling for invalid schemes.
func TestEnrichServiceNew_InvalidScheme(t *testing.T) {
	_, err := NewEnrichService("invalid://localhost:11434/model")
	if err == nil {
		t.Fatalf("Expected error for invalid scheme, got nil")
	}
}

func TestEnrichService_Init(t *testing.T) {
	mock := NewMockLLMProvider()
	es := &EnrichService{
		provider: mock,
		name:     "enrich-mock",
		provCfg: &plugin.ProviderConfig{
			Scheme:  plugin.SchemeOllama,
			BaseURL: "http://localhost:11434",
			Model:   "test",
		},
	}

	err := es.Init(context.Background(), plugin.PluginConfig{})
	if err != nil {
		t.Fatalf("Init failed: %v", err)
	}
	if es.pipeline == nil {
		t.Fatal("pipeline should be initialized after Init")
	}
	if es.limiter == nil {
		t.Fatal("limiter should be initialized after Init")
	}
}

func TestEnrichService_Init_ProviderError(t *testing.T) {
	mock := NewMockLLMProvider()
	mock.customComplete = func(ctx context.Context, system, user string) (string, error) {
		return "", context.DeadlineExceeded
	}
	// Override Init to fail
	failInit := &failingInitProvider{}
	es := &EnrichService{
		provider: failInit,
		name:     "enrich-fail",
		provCfg: &plugin.ProviderConfig{
			Scheme:  plugin.SchemeOllama,
			BaseURL: "http://localhost:11434",
			Model:   "test",
		},
	}
	_ = mock

	err := es.Init(context.Background(), plugin.PluginConfig{})
	if err == nil {
		t.Fatal("expected Init to fail when provider Init fails")
	}
}

type failingInitProvider struct{}

func (f *failingInitProvider) Name() string { return "fail" }
func (f *failingInitProvider) Init(_ context.Context, _ LLMProviderConfig) error {
	return context.DeadlineExceeded
}
func (f *failingInitProvider) Complete(_ context.Context, _, _ string) (string, error) {
	return "", nil
}
func (f *failingInitProvider) Close() error { return nil }

func TestEnrichService_Enrich_NotInitialized(t *testing.T) {
	es := &EnrichService{
		provider: NewMockLLMProvider(),
		name:     "enrich-mock",
	}

	eng := &storage.Engram{ID: storage.NewULID(), Concept: "c", Content: "x"}
	_, err := es.Enrich(context.Background(), eng)
	if err == nil {
		t.Fatal("expected error when pipeline is nil")
	}
}

func TestEnrichService_Enrich_WhenClosed(t *testing.T) {
	es := &EnrichService{
		provider: NewMockLLMProvider(),
		name:     "enrich-mock",
		closed:   true,
	}

	eng := &storage.Engram{ID: storage.NewULID(), Concept: "c", Content: "x"}
	_, err := es.Enrich(context.Background(), eng)
	if err == nil {
		t.Fatal("expected error when service is closed")
	}
}

func TestEnrichService_Enrich_Success(t *testing.T) {
	mock := NewMockLLMProvider()
	limiter := NewTokenBucketLimiter(100.0, 100.0)
	pipeline := NewPipeline(mock, limiter)

	es := &EnrichService{
		provider: mock,
		pipeline: pipeline,
		limiter:  limiter,
		name:     "enrich-mock",
	}

	eng := &storage.Engram{ID: storage.NewULID(), Concept: "test", Content: "hello world"}
	result, err := es.Enrich(context.Background(), eng)
	if err != nil {
		t.Fatalf("Enrich failed: %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil result")
	}
}

func TestEnrichService_Close_Idempotent(t *testing.T) {
	es, err := NewEnrichService("ollama://localhost:11434/test")
	if err != nil {
		t.Fatalf("NewEnrichService failed: %v", err)
	}

	if err := es.Close(); err != nil {
		t.Fatalf("first Close failed: %v", err)
	}
	if err := es.Close(); err != nil {
		t.Fatalf("second Close should be idempotent: %v", err)
	}
}

func TestEnrichService_CreateRateLimiter(t *testing.T) {
	es := &EnrichService{}

	tests := []struct {
		scheme plugin.ProviderScheme
		name   string
	}{
		{plugin.SchemeOllama, "ollama"},
		{plugin.SchemeOpenAI, "openai"},
		{plugin.SchemeAnthropic, "anthropic"},
		{"unknown", "default"},
	}

	for _, tt := range tests {
		limiter := es.createRateLimiter(tt.scheme)
		if limiter == nil {
			t.Fatalf("createRateLimiter(%s) returned nil", tt.name)
		}
	}
}
