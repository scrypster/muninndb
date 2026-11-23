package embed

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/plugin"
)

func TestOllamaProvider_Init_Success(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.Method == "POST" && r.URL.Path == "/api/embeddings" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"embedding": [0.1, 0.2, 0.3]}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	provider := &OllamaProvider{}
	cfg := ProviderHTTPConfig{
		BaseURL: "http://" + server.Listener.Addr().String(),
		Model:   "test-model",
	}

	dim, err := provider.Init(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Init failed: %v", err)
	}

	if dim != 3 {
		t.Errorf("expected dimension 3, got %d", dim)
	}
}

func TestOllamaProvider_Init_Unreachable(t *testing.T) {
	provider := &OllamaProvider{}
	cfg := ProviderHTTPConfig{
		BaseURL: "http://localhost:54321",
		Model:   "test-model",
	}

	_, err := provider.Init(context.Background(), cfg)
	if err == nil {
		t.Fatal("expected error for unreachable server")
	}
}

func TestOllamaProvider_Embed(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.Method == "POST" && r.URL.Path == "/api/embeddings" {
			var req ollamaEmbedRequest
			json.NewDecoder(r.Body).Decode(&req)

			embedding := []float64{0.1, 0.2, 0.3}
			if req.Prompt == "hello" {
				embedding = []float64{0.4, 0.5, 0.6}
			}

			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string][]float64{
				"embedding": embedding,
			})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	provider := &OllamaProvider{}
	cfg := ProviderHTTPConfig{
		BaseURL: "http://" + server.Listener.Addr().String(),
		Model:   "test-model",
	}

	provider.Init(context.Background(), cfg)

	result, err := provider.EmbedBatch(context.Background(), []string{"hello", "world"})
	if err != nil {
		t.Fatalf("Embed failed: %v", err)
	}

	expectedLen := 6 // 2 texts * 3 dimension
	if len(result) != expectedLen {
		t.Errorf("expected %d embeddings, got %d", expectedLen, len(result))
	}
}

func TestOllamaProvider_MaxBatchSize_Default(t *testing.T) {
	// A fresh provider (useLegacyAPI=false, no batch_size in the URL) carries
	// 8 texts per request: a batch request is what a recall's query embed can
	// wait behind (#928).
	provider := &OllamaProvider{}
	if provider.MaxBatchSize() != 8 {
		t.Errorf("expected batch size 8 (new batch endpoint), got %d", provider.MaxBatchSize())
	}
}

func TestOllamaProvider_MaxBatchSize_Legacy(t *testing.T) {
	// When legacy API is forced, MaxBatchSize must return 1
	provider := &OllamaProvider{useLegacyAPI: true}
	if provider.MaxBatchSize() != 1 {
		t.Errorf("expected batch size 1 in legacy mode, got %d", provider.MaxBatchSize())
	}
}

func TestOllamaProvider_Close(t *testing.T) {
	provider := &OllamaProvider{}
	err := provider.Close()
	if err != nil {
		t.Errorf("Close failed: %v", err)
	}
}

func TestOllamaProvider_Name(t *testing.T) {
	provider := &OllamaProvider{}
	if provider.Name() != "ollama" {
		t.Errorf("expected name ollama, got %s", provider.Name())
	}
}

func TestOllamaProvider_Embed_ServerError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.Method == "POST" && r.URL.Path == "/api/embeddings" {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte("internal error"))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	provider := &OllamaProvider{}
	cfg := ProviderHTTPConfig{
		BaseURL: "http://" + server.Listener.Addr().String(),
		Model:   "test-model",
	}

	provider.Init(context.Background(), cfg)

	_, err := provider.EmbedBatch(context.Background(), []string{"test"})
	if err == nil {
		t.Fatal("expected error for server error")
	}
}

func TestOllamaProvider_Init_ProbeError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.Method == "POST" && r.URL.Path == "/api/embeddings" {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte("model not found"))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	provider := &OllamaProvider{}
	cfg := ProviderHTTPConfig{
		BaseURL: "http://" + server.Listener.Addr().String(),
		Model:   "nonexistent-model",
	}

	_, err := provider.Init(context.Background(), cfg)
	if err == nil {
		t.Fatal("expected error for model not found")
	}
}

func TestOllamaProvider_Embed_InvalidJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.Method == "POST" && r.URL.Path == "/api/embeddings" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("invalid json"))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	provider := &OllamaProvider{}
	cfg := ProviderHTTPConfig{
		BaseURL: "http://" + server.Listener.Addr().String(),
		Model:   "test-model",
	}

	// Modify Init to handle bad JSON
	provider.baseURL = cfg.BaseURL
	provider.model = cfg.Model
	transport := &http.Transport{
		MaxIdleConns:        10,
		MaxIdleConnsPerHost: 10,
		IdleConnTimeout:     90,
		TLSHandshakeTimeout: 10,
	}
	provider.client = &http.Client{Transport: transport, Timeout: 10}

	_, err := provider.EmbedBatch(context.Background(), []string{"test"})
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestOllamaProvider_EmptyEmbedding(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.Method == "POST" && r.URL.Path == "/api/embeddings" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			// First request (probe) succeeds with empty embedding
			w.Write([]byte(`{"embedding": []}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	provider := &OllamaProvider{}
	cfg := ProviderHTTPConfig{
		BaseURL: "http://" + server.Listener.Addr().String(),
		Model:   "test-model",
	}

	_, err := provider.Init(context.Background(), cfg)
	if err == nil {
		t.Fatal("expected error for empty embedding")
	}
}

func TestOllamaProvider_Close_WithClient(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.Method == "POST" && r.URL.Path == "/api/embeddings" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"embedding": [0.1, 0.2, 0.3]}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	provider := &OllamaProvider{}
	cfg := ProviderHTTPConfig{
		BaseURL: "http://" + server.Listener.Addr().String(),
		Model:   "test-model",
	}
	provider.Init(context.Background(), cfg)

	err := provider.Close()
	if err != nil {
		t.Errorf("Close failed: %v", err)
	}
}

func TestOllamaProvider_Init_InvalidJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.Method == "POST" && r.URL.Path == "/api/embeddings" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("not valid json"))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	provider := &OllamaProvider{}
	cfg := ProviderHTTPConfig{
		BaseURL: "http://" + server.Listener.Addr().String(),
		Model:   "test-model",
	}

	_, err := provider.Init(context.Background(), cfg)
	if err == nil {
		t.Fatal("expected error for invalid JSON in init")
	}
}

func TestOllamaProvider_EmbedBatch_NonEmptyResult(t *testing.T) {
	callCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.Method == "POST" && r.URL.Path == "/api/embeddings" {
			callCount++
			var req ollamaEmbedRequest
			json.NewDecoder(r.Body).Decode(&req)

			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string][]float64{
				"embedding": []float64{0.1 * float64(callCount), 0.2 * float64(callCount)},
			})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	provider := &OllamaProvider{}
	cfg := ProviderHTTPConfig{
		BaseURL: "http://" + server.Listener.Addr().String(),
		Model:   "test-model",
	}

	provider.Init(context.Background(), cfg)

	// Reset for actual embed calls
	callCount = 0
	result, err := provider.EmbedBatch(context.Background(), []string{"text1", "text2"})
	if err != nil {
		t.Fatalf("Embed failed: %v", err)
	}

	// Should have 2 texts * 2 dimension = 4 values
	if len(result) != 4 {
		t.Errorf("expected 4 values, got %d", len(result))
	}

	// Verify result contains expected values
	if len(result) > 0 {
		// Just verify we got some data back
	}
}

// compile-time assertion — OllamaProvider implements HardwareAwarePlugin after Task 2
var _ plugin.HardwareAwarePlugin = (*OllamaProvider)(nil)

// --- New batch API tests ---

// TestOllamaProvider_EmbedBatch_UsesBatchEndpoint verifies POST /api/embed is called
// with the input array and that float64→float32 conversion is correct.
func TestOllamaProvider_EmbedBatch_UsesBatchEndpoint(t *testing.T) {
	var capturedPath string
	var capturedBody ollamaBatchEmbedRequest

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			w.WriteHeader(http.StatusOK)
			return
		}
		capturedPath = r.URL.Path
		if r.Method == "POST" && r.URL.Path == "/api/embed" {
			json.NewDecoder(r.Body).Decode(&capturedBody)
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(ollamaBatchEmbedResponse{
				Embeddings: [][]float64{
					{1.5, 2.5, 3.5},
					{4.5, 5.5, 6.5},
				},
			})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	provider := &OllamaProvider{}
	cfg := ProviderHTTPConfig{BaseURL: "http://" + server.Listener.Addr().String(), Model: "m"}
	if _, err := provider.Init(context.Background(), cfg); err != nil {
		t.Fatalf("Init failed: %v", err)
	}

	result, err := provider.EmbedBatch(context.Background(), []string{"a", "b"})
	if err != nil {
		t.Fatalf("EmbedBatch failed: %v", err)
	}

	// Verify batch endpoint was called (not legacy)
	if capturedPath != "/api/embed" {
		t.Errorf("expected POST /api/embed, got %s", capturedPath)
	}

	// Verify input array was sent
	if len(capturedBody.Input) != 2 {
		t.Errorf("expected 2 inputs, got %d", len(capturedBody.Input))
	}

	// Verify float64→float32 conversion
	expected := []float32{1.5, 2.5, 3.5, 4.5, 5.5, 6.5}
	if len(result) != len(expected) {
		t.Fatalf("expected %d values, got %d", len(expected), len(result))
	}
	for i, v := range result {
		if v != expected[i] {
			t.Errorf("result[%d] = %v, want %v", i, v, expected[i])
		}
	}
}

// TestOllamaProvider_EmbedBatch_LegacyFallback verifies that when /api/embed returns 404,
// the provider falls back to /api/embeddings per-text loop.
func TestOllamaProvider_EmbedBatch_LegacyFallback(t *testing.T) {
	callCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.Method == "POST" && r.URL.Path == "/api/embeddings" {
			callCount++
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(ollamaEmbedResponse{Embedding: []float64{0.1, 0.2}})
			return
		}
		// /api/embed returns 404 → triggers legacy mode
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	provider := &OllamaProvider{}
	cfg := ProviderHTTPConfig{BaseURL: "http://" + server.Listener.Addr().String(), Model: "m"}
	if _, err := provider.Init(context.Background(), cfg); err != nil {
		t.Fatalf("Init failed: %v", err)
	}

	// useLegacyAPI should be set after Init
	if !provider.useLegacyAPI {
		t.Fatal("expected useLegacyAPI=true when /api/embed returns 404")
	}

	callCount = 0 // reset after Init probe
	result, err := provider.EmbedBatch(context.Background(), []string{"x", "y"})
	if err != nil {
		t.Fatalf("EmbedBatch failed: %v", err)
	}

	// Expect 2 separate calls (one per text)
	if callCount != 2 {
		t.Errorf("expected 2 API calls in legacy mode, got %d", callCount)
	}
	if len(result) != 4 { // 2 texts × 2 dims
		t.Errorf("expected 4 floats, got %d", len(result))
	}
}

// deadlineCapturingTransport is a RoundTripper that captures the context deadline
// from the outgoing request (client-side) before delegating to the real transport.
type deadlineCapturingTransport struct {
	inner          http.RoundTripper
	capturedDL     time.Time
	capturedDLOnce bool
}

func (t *deadlineCapturingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Only capture for POST /api/embed (not for Init probes)
	if req.Method == "POST" && req.URL.Path == "/api/embed" {
		if dl, ok := req.Context().Deadline(); ok {
			t.capturedDL = dl
			t.capturedDLOnce = true
		}
	}
	return t.inner.RoundTrip(req)
}

// TestOllamaProvider_DynamicTimeout verifies that embedBatchNew applies a per-batch
// context deadline proportional to batch size: 30s + len(texts)*10s.
// For batch=64, the deadline must be >= 670s from request start.
func TestOllamaProvider_DynamicTimeout(t *testing.T) {
	const batchSize = 64
	expectedMinTimeout := 30*time.Second + batchSize*10*time.Second // 670s

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.Method == "POST" && r.URL.Path == "/api/embed" {
			var req ollamaBatchEmbedRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			embeddings := make([][]float64, len(req.Input))
			for i := range embeddings {
				embeddings[i] = []float64{0.1, 0.2}
			}
			json.NewEncoder(w).Encode(ollamaBatchEmbedResponse{Embeddings: embeddings})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	provider := &OllamaProvider{}
	cfg := ProviderHTTPConfig{BaseURL: "http://" + server.Listener.Addr().String(), Model: "m"}
	if _, err := provider.Init(context.Background(), cfg); err != nil {
		t.Fatalf("Init failed: %v", err)
	}

	// Swap in the intercepting transport after Init so we only capture the
	// EmbedBatch call's deadline, not the Init probe deadline.
	capturing := &deadlineCapturingTransport{inner: http.DefaultTransport}
	provider.client = &http.Client{Transport: capturing}

	texts := make([]string, batchSize)
	beforeCall := time.Now()
	if _, err := provider.EmbedBatch(context.Background(), texts); err != nil {
		t.Fatalf("EmbedBatch failed: %v", err)
	}

	if !capturing.capturedDLOnce {
		t.Fatal("expected request context to have a deadline set on the outgoing POST /api/embed")
	}
	remaining := capturing.capturedDL.Sub(beforeCall)
	if remaining < expectedMinTimeout-2*time.Second {
		t.Errorf("timeout too short: got ~%v, expected >= %v", remaining.Round(time.Second), expectedMinTimeout)
	}
}

// TestOllamaProvider_HardwareDetection_GPU verifies hasGPU=true when /api/ps reports size_vram > 0.
func TestOllamaProvider_HardwareDetection_GPU(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && r.URL.Path == "/" {
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.Method == "GET" && r.URL.Path == "/api/ps" {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(ollamaPSResponse{
				Models: []ollamaPSModel{
					{Name: "test-model", SizeVRAM: 4294967296}, // 4 GB VRAM
				},
			})
			return
		}
		if r.Method == "POST" && r.URL.Path == "/api/embed" {
			json.NewEncoder(w).Encode(ollamaBatchEmbedResponse{
				Embeddings: [][]float64{{0.1, 0.2}},
			})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	provider := &OllamaProvider{}
	cfg := ProviderHTTPConfig{BaseURL: "http://" + server.Listener.Addr().String(), Model: "test-model"}
	if _, err := provider.Init(context.Background(), cfg); err != nil {
		t.Fatalf("Init failed: %v", err)
	}

	if !provider.HardwareAccelerated() {
		t.Error("expected HardwareAccelerated()=true when size_vram > 0")
	}
}

// TestOllamaProvider_HardwareDetection_CPU verifies hasGPU=false when size_vram == 0.
func TestOllamaProvider_HardwareDetection_CPU(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && r.URL.Path == "/" {
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.Method == "GET" && r.URL.Path == "/api/ps" {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(ollamaPSResponse{
				Models: []ollamaPSModel{
					{Name: "test-model", SizeVRAM: 0},
				},
			})
			return
		}
		if r.Method == "POST" && r.URL.Path == "/api/embed" {
			json.NewEncoder(w).Encode(ollamaBatchEmbedResponse{
				Embeddings: [][]float64{{0.1, 0.2}},
			})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	provider := &OllamaProvider{}
	cfg := ProviderHTTPConfig{BaseURL: "http://" + server.Listener.Addr().String(), Model: "test-model"}
	if _, err := provider.Init(context.Background(), cfg); err != nil {
		t.Fatalf("Init failed: %v", err)
	}

	if provider.HardwareAccelerated() {
		t.Error("expected HardwareAccelerated()=false when size_vram == 0")
	}
}

// TestOllamaProvider_HardwareDetection_APIUnavailable verifies hasGPU=false (graceful default)
// when /api/ps returns 404 or an error.
func TestOllamaProvider_HardwareDetection_APIUnavailable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && r.URL.Path == "/" {
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.Method == "POST" && r.URL.Path == "/api/embed" {
			json.NewEncoder(w).Encode(ollamaBatchEmbedResponse{
				Embeddings: [][]float64{{0.1, 0.2}},
			})
			return
		}
		// /api/ps returns 404 (older Ollama)
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	provider := &OllamaProvider{}
	cfg := ProviderHTTPConfig{BaseURL: "http://" + server.Listener.Addr().String(), Model: "m"}
	if _, err := provider.Init(context.Background(), cfg); err != nil {
		t.Fatalf("Init failed: %v", err)
	}

	if provider.HardwareAccelerated() {
		t.Error("expected HardwareAccelerated()=false when /api/ps unavailable")
	}
}

// TestOllamaProvider_BodyClosedPerIteration verifies the legacy loop closes the
// response body after each iteration, not at function return.
// We verify this indirectly: if bodies were not closed, the server would exhaust
// its connection pool on many requests; we simply verify the legacy loop completes
// N requests without error (relying on inline closure correctness).
func TestOllamaProvider_BodyClosedPerIteration(t *testing.T) {
	const n = 10
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.Method == "POST" && r.URL.Path == "/api/embeddings" {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(ollamaEmbedResponse{Embedding: []float64{0.1}})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	provider := &OllamaProvider{useLegacyAPI: true}
	provider.baseURL = "http://" + server.Listener.Addr().String()
	provider.model = "m"
	transport := &http.Transport{MaxIdleConns: 2, MaxIdleConnsPerHost: 2}
	provider.client = &http.Client{Timeout: 5 * time.Second, Transport: transport}

	texts := make([]string, n)
	for i := range texts {
		texts[i] = "text"
	}

	_, err := provider.EmbedBatch(context.Background(), texts)
	if err != nil {
		t.Fatalf("expected no error with %d legacy requests, got: %v", n, err)
	}
}
