package embed

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/scrypster/muninndb/internal/plugin"
)

func TestNewEmbedService_Ollama(t *testing.T) {
	es, err := NewEmbedService("ollama://localhost:11434/nomic-embed-text")
	if err != nil {
		t.Fatalf("NewEmbedService failed: %v", err)
	}

	if es.name != "embed-ollama" {
		t.Errorf("expected name embed-ollama, got %s", es.name)
	}

	if es.provCfg.Model != "nomic-embed-text" {
		t.Errorf("expected model nomic-embed-text, got %s", es.provCfg.Model)
	}
}

func TestNewEmbedService_OpenAI(t *testing.T) {
	es, err := NewEmbedService("openai://text-embedding-3-small")
	if err != nil {
		t.Fatalf("NewEmbedService failed: %v", err)
	}

	if es.name != "embed-openai" {
		t.Errorf("expected name embed-openai, got %s", es.name)
	}

	if es.provCfg.Model != "text-embedding-3-small" {
		t.Errorf("expected model text-embedding-3-small, got %s", es.provCfg.Model)
	}
}

func TestNewEmbedService_Voyage(t *testing.T) {
	es, err := NewEmbedService("voyage://voyage-3")
	if err != nil {
		t.Fatalf("NewEmbedService failed: %v", err)
	}

	if es.name != "embed-voyage" {
		t.Errorf("expected name embed-voyage, got %s", es.name)
	}

	if es.provCfg.Model != "voyage-3" {
		t.Errorf("expected model voyage-3, got %s", es.provCfg.Model)
	}
}

func TestNewEmbedService_InvalidScheme(t *testing.T) {
	_, err := NewEmbedService("unknown://model")
	if err == nil {
		t.Fatal("expected error for invalid scheme")
	}
}

func TestEmbedServiceName(t *testing.T) {
	es, _ := NewEmbedService("ollama://localhost:11434/model")
	if es.Name() != "embed-ollama" {
		t.Errorf("expected embed-ollama, got %s", es.Name())
	}
}

func TestEmbedServiceTier(t *testing.T) {
	es, _ := NewEmbedService("ollama://localhost:11434/model")
	if es.Tier() != plugin.TierEmbed {
		t.Errorf("expected TierEmbed, got %v", es.Tier())
	}
}

func TestEmbedServiceDimension(t *testing.T) {
	es := &EmbedService{dim: 1536}
	if es.Dimension() != 1536 {
		t.Errorf("expected dimension 1536, got %d", es.Dimension())
	}
}

func TestEmbedService_OllamaInit_Success(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.Method == "POST" && r.URL.Path == "/api/embeddings" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"embedding": [0.1, 0.2, 0.3, 0.4]}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	es, _ := NewEmbedService("ollama://" + server.Listener.Addr().String() + "/test-model")

	cfg := plugin.PluginConfig{
		ProviderURL: "ollama://" + server.Listener.Addr().String() + "/test-model",
		APIKey:      "",
		Options:     map[string]string{},
	}

	err := es.Init(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Init failed: %v", err)
	}

	if es.Dimension() != 4 {
		t.Errorf("expected dimension 4, got %d", es.Dimension())
	}
}

func TestEmbedService_OllamaInit_Unreachable(t *testing.T) {
	es, _ := NewEmbedService("ollama://localhost:54321/test-model")

	cfg := plugin.PluginConfig{
		ProviderURL: "ollama://localhost:54321/test-model",
	}

	err := es.Init(context.Background(), cfg)
	if err == nil {
		t.Fatal("expected error for unreachable Ollama")
	}
}

func TestEmbedService_EmbedAfterInit(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.Method == "POST" && r.URL.Path == "/api/embeddings" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"embedding": [0.1, 0.2]}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	es, _ := NewEmbedService("ollama://" + server.Listener.Addr().String() + "/test-model")

	cfg := plugin.PluginConfig{
		ProviderURL: "ollama://" + server.Listener.Addr().String() + "/test-model",
	}

	es.Init(context.Background(), cfg)

	result, err := es.Embed(context.Background(), []string{"hello world"})
	if err != nil {
		t.Fatalf("Embed failed: %v", err)
	}

	expectedLen := 2 // 1 text * 2 dimension
	if len(result) != expectedLen {
		t.Errorf("expected %d embeddings, got %d", expectedLen, len(result))
	}
}

func TestEmbedService_Close(t *testing.T) {
	es, _ := NewEmbedService("ollama://localhost:11434/model")

	err := es.Close()
	if err != nil {
		t.Errorf("Close failed: %v", err)
	}

	// Second close should be no-op
	err = es.Close()
	if err != nil {
		t.Errorf("second Close failed: %v", err)
	}
}

func TestEmbedService_Embed_NotInitialized(t *testing.T) {
	es, _ := NewEmbedService("ollama://localhost:11434/model")

	_, err := es.Embed(context.Background(), []string{"hello"})
	if err == nil {
		t.Fatal("expected error for not-initialized service")
	}
}

func TestEmbedService_Embed_AfterClose(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.Method == "POST" && r.URL.Path == "/api/embeddings" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"embedding": [0.1, 0.2]}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	es, _ := NewEmbedService("ollama://" + server.Listener.Addr().String() + "/test-model")
	cfg := plugin.PluginConfig{
		ProviderURL: "ollama://" + server.Listener.Addr().String() + "/test-model",
	}
	es.Init(context.Background(), cfg)
	es.Close()

	_, err := es.Embed(context.Background(), []string{"hello"})
	if err == nil {
		t.Fatal("expected error for closed service")
	}
}

func TestEmbedService_Close_WithProviderError(t *testing.T) {
	es := &EmbedService{
		provider: &failingCloseProvider{},
	}

	err := es.Close()
	if err == nil {
		t.Fatal("expected error from provider close")
	}
}

type failingCloseProvider struct {
	MockProvider
}

func (f *failingCloseProvider) Close() error {
	return fmt.Errorf("provider close error")
}

func TestEmbedService_NewEmbedService_Local(t *testing.T) {
	es, err := NewEmbedService("local://all-MiniLM-L6-v2")
	if err != nil {
		t.Fatalf("NewEmbedService failed: %v", err)
	}
	if es.name != "embed-local" {
		t.Errorf("expected name embed-local, got %s", es.name)
	}
}

func TestEmbedService_CreateRateLimiter_OpenAI(t *testing.T) {
	es := &EmbedService{}
	limiter := es.createRateLimiter(plugin.SchemeOpenAI, nil)
	if limiter == nil {
		t.Fatal("expected non-nil limiter for OpenAI")
	}
	if limiter.rate != 50.0 {
		t.Errorf("expected OpenAI rate 50.0, got %f", limiter.rate)
	}
}

func TestEmbedService_CreateRateLimiter_Voyage(t *testing.T) {
	es := &EmbedService{}
	limiter := es.createRateLimiter(plugin.SchemeVoyage, nil)
	if limiter == nil {
		t.Fatal("expected non-nil limiter for Voyage")
	}
	if limiter.rate != 5.0 {
		t.Errorf("expected Voyage rate 5.0, got %f", limiter.rate)
	}
}

func TestEmbedService_CreateRateLimiter_Ollama(t *testing.T) {
	es := &EmbedService{}
	limiter := es.createRateLimiter(plugin.SchemeOllama, nil)
	if limiter != nil {
		t.Fatal("expected nil limiter for Ollama")
	}
}

func TestEmbedService_CreateRateLimiter_DefaultScheme(t *testing.T) {
	es := &EmbedService{}
	limiter := es.createRateLimiter(plugin.SchemeAnthropic, nil)
	if limiter == nil {
		t.Fatal("expected non-nil limiter for unknown scheme")
	}
	if limiter.rate != 10.0 {
		t.Errorf("expected default rate 10.0, got %f", limiter.rate)
	}
}

func TestEmbedService_OpenAI_Init_Success(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" && r.URL.Path == "/v1/embeddings" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"data": [{"embedding": [0.1, 0.2, 0.3], "index": 0}]}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	es, _ := NewEmbedService("openai://text-embedding-3-small")
	es.provCfg.BaseURL = "http://" + server.Listener.Addr().String()

	cfg := plugin.PluginConfig{
		APIKey:  "test-key",
		Options: map[string]string{},
	}

	err := es.Init(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Init failed: %v", err)
	}

	if es.Dimension() != 3 {
		t.Errorf("expected dimension 3, got %d", es.Dimension())
	}

	if es.limiter == nil {
		t.Error("expected non-nil rate limiter for OpenAI")
	}
}

func TestEmbedService_Voyage_Init_Success(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" && r.URL.Path == "/v1/embeddings" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"data": [{"embedding": [0.1, 0.2, 0.3], "index": 0}]}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	es, _ := NewEmbedService("voyage://voyage-3")
	es.provCfg.BaseURL = "http://" + server.Listener.Addr().String()

	cfg := plugin.PluginConfig{
		APIKey:  "test-key",
		Options: map[string]string{},
	}

	err := es.Init(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Init failed: %v", err)
	}

	if es.Dimension() != 3 {
		t.Errorf("expected dimension 3, got %d", es.Dimension())
	}

	if es.limiter == nil {
		t.Error("expected non-nil rate limiter for Voyage")
	}
}

// mockHardwareProvider satisfies both Provider and plugin.HardwareAwarePlugin.
type mockHardwareProvider struct {
	accelerated bool
	dims        int
}

func (m *mockHardwareProvider) Name() string { return "mock-hw" }
func (m *mockHardwareProvider) Init(_ context.Context, _ ProviderHTTPConfig) (int, error) {
	return m.dims, nil
}
func (m *mockHardwareProvider) EmbedBatch(_ context.Context, texts []string) ([]float32, error) {
	return make([]float32, len(texts)*m.dims), nil
}
func (m *mockHardwareProvider) MaxBatchSize() int         { return 1 }
func (m *mockHardwareProvider) Close() error              { return nil }
func (m *mockHardwareProvider) HardwareAccelerated() bool { return m.accelerated }

func TestEmbedService_HardwareAccelerated_Delegates(t *testing.T) {
	svc := &EmbedService{provider: &mockHardwareProvider{accelerated: true, dims: 2}}
	if !svc.HardwareAccelerated() {
		t.Error("expected HardwareAccelerated()=true when inner provider reports GPU")
	}
}

// simpleProvider implements only Provider, not HardwareAwarePlugin.
type simpleProvider struct{}

func (s *simpleProvider) Name() string { return "simple" }
func (s *simpleProvider) Init(_ context.Context, _ ProviderHTTPConfig) (int, error) {
	return 2, nil
}
func (s *simpleProvider) EmbedBatch(_ context.Context, texts []string) ([]float32, error) {
	return make([]float32, len(texts)*2), nil
}
func (s *simpleProvider) MaxBatchSize() int { return 1 }
func (s *simpleProvider) Close() error      { return nil }

func TestEmbedService_HardwareAccelerated_NotSupported(t *testing.T) {
	// Use a provider that does NOT implement plugin.HardwareAwarePlugin
	svc := &EmbedService{provider: &simpleProvider{}}
	if svc.HardwareAccelerated() {
		t.Error("expected HardwareAccelerated()=false when provider does not implement HardwareAwarePlugin")
	}
}
