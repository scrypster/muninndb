package embed

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/scrypster/muninndb/internal/plugin"
)

// OllamaProvider implements Provider for local Ollama instances.
// It probes /api/embed (batch endpoint) on Init; falls back to legacy
// /api/embeddings (single-text) when /api/embed returns 404.
type OllamaProvider struct {
	client       *http.Client
	baseURL      string
	model        string
	useLegacyAPI bool // true when Ollama version lacks /api/embed
	hasGPU       bool // true when /api/ps reports size_vram > 0
}

// Legacy single-text structs (for /api/embeddings)
type ollamaEmbedRequest struct {
	Model  string `json:"model"`
	Prompt string `json:"prompt"`
}

type ollamaEmbedResponse struct {
	Embedding []float64 `json:"embedding"`
}

// Batch structs (for /api/embed)
type ollamaBatchEmbedRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

type ollamaBatchEmbedResponse struct {
	Embeddings [][]float64 `json:"embeddings"`
}

// Hardware detection struct (for /api/ps)
type ollamaPSModel struct {
	Name     string `json:"name"`
	SizeVRAM int64  `json:"size_vram"`
}

type ollamaPSResponse struct {
	Models []ollamaPSModel `json:"models"`
}

type ollamaShowRequest struct {
	Name string `json:"name"`
}

type ollamaShowResponse struct {
	ModelInfo map[string]any `json:"model_info"`
}

func (p *OllamaProvider) Name() string {
	return "ollama"
}

func (p *OllamaProvider) Init(ctx context.Context, cfg ProviderHTTPConfig) (int, error) {
	p.baseURL = cfg.BaseURL
	p.model = cfg.Model

	transport := &http.Transport{
		MaxIdleConns:        10,
		MaxIdleConnsPerHost: 10,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
	}
	// No client-level Timeout: all requests carry per-request context deadlines
	// (set in probeEmbedEndpoint, embedBatchNew, etc.). A global Timeout would
	// override context deadlines and silently kill large batch requests.
	p.client = &http.Client{Transport: plugin.WrapTransport(transport)}

	// Probe connectivity with root GET
	probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(probeCtx, "GET", p.baseURL, nil)
	if err != nil {
		return 0, fmt.Errorf("cannot connect to Ollama at %s — is it running? (%w)", p.baseURL, err)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("cannot connect to Ollama at %s — is it running? (%w)", p.baseURL, err)
	}
	resp.Body.Close()

	// Probe model context length (best-effort, warns on small windows)
	p.probeContextLength(ctx)

	// Probe embed endpoint. Tries /api/embed (batch) first; falls back to
	// /api/embeddings (legacy single-text) on 404.
	dim, err := p.probeEmbedEndpoint(ctx)
	if err != nil {
		return 0, err
	}

	// Hardware detection — Ollama loads models lazily; model is guaranteed
	// loaded at this point because probeEmbedEndpoint embedded a text.
	p.detectHardware(ctx)

	return dim, nil
}

// probeEmbedEndpoint tries POST /api/embed with a single-element input array.
// On 404, sets useLegacyAPI=true and falls back to probing /api/embeddings.
func (p *OllamaProvider) probeEmbedEndpoint(ctx context.Context) (int, error) {
	embedCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	body, _ := json.Marshal(ollamaBatchEmbedRequest{
		Model: p.model,
		Input: []string{"dimension detection probe"},
	})
	req, err := http.NewRequestWithContext(embedCtx, "POST", p.baseURL+"/api/embed", bytes.NewReader(body))
	if err != nil {
		return 0, fmt.Errorf("cannot create embed request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := p.client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("cannot connect to Ollama at %s — is it running? (%w)", p.baseURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		// Old Ollama — use legacy single-text endpoint
		p.useLegacyAPI = true
		return p.probeLegacyEmbedEndpoint(ctx)
	}

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return 0, fmt.Errorf("Ollama returned status %d: %s", resp.StatusCode, string(bodyBytes))
	}

	var batchResp ollamaBatchEmbedResponse
	if err := json.NewDecoder(resp.Body).Decode(&batchResp); err != nil {
		return 0, fmt.Errorf("failed to decode Ollama response: %w", err)
	}

	if len(batchResp.Embeddings) == 0 || len(batchResp.Embeddings[0]) == 0 {
		return 0, fmt.Errorf("Ollama returned empty embedding")
	}

	dim := len(batchResp.Embeddings[0])
	slog.Info("Ollama dimension probe successful", "dimension", dim, "endpoint", "/api/embed")
	return dim, nil
}

// probeLegacyEmbedEndpoint probes POST /api/embeddings for dimension detection.
// Called when /api/embed returns 404.
func (p *OllamaProvider) probeLegacyEmbedEndpoint(ctx context.Context) (int, error) {
	embedCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	body, _ := json.Marshal(ollamaEmbedRequest{
		Model:  p.model,
		Prompt: "dimension detection probe",
	})
	req, err := http.NewRequestWithContext(embedCtx, "POST", p.baseURL+"/api/embeddings", bytes.NewReader(body))
	if err != nil {
		return 0, fmt.Errorf("cannot create embed request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := p.client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("cannot connect to Ollama at %s — is it running? (%w)", p.baseURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return 0, fmt.Errorf("Ollama returned status %d: %s", resp.StatusCode, string(bodyBytes))
	}

	var ollamaResp ollamaEmbedResponse
	if err := json.NewDecoder(resp.Body).Decode(&ollamaResp); err != nil {
		return 0, fmt.Errorf("failed to decode Ollama response: %w", err)
	}

	if len(ollamaResp.Embedding) == 0 {
		return 0, fmt.Errorf("Ollama returned empty embedding")
	}

	dim := len(ollamaResp.Embedding)
	slog.Info("Ollama dimension probe successful", "dimension", dim, "endpoint", "/api/embeddings")
	return dim, nil
}

// detectHardware calls GET /api/ps to detect GPU acceleration.
// Silently defaults to hasGPU=false on any error (best-effort).
func (p *OllamaProvider) detectHardware(ctx context.Context) {
	psCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(psCtx, "GET", p.baseURL+"/api/ps", nil)
	if err != nil {
		return
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return
	}

	var ps ollamaPSResponse
	if err := json.NewDecoder(resp.Body).Decode(&ps); err != nil {
		return
	}

	for _, m := range ps.Models {
		if m.SizeVRAM > 0 {
			p.hasGPU = true
			slog.Info("Ollama GPU detected", "model", m.Name, "size_vram", m.SizeVRAM)
			return
		}
	}
}

// HardwareAccelerated reports whether Ollama detected GPU usage on Init.
func (p *OllamaProvider) HardwareAccelerated() bool {
	return p.hasGPU
}

// EmbedBatch embeds a batch of texts. Uses the batch /api/embed endpoint
// unless useLegacyAPI is true, in which case falls back to the legacy
// per-text /api/embeddings loop.
func (p *OllamaProvider) EmbedBatch(ctx context.Context, texts []string) ([]float32, error) {
	if p.useLegacyAPI {
		return p.embedBatchLegacy(ctx, texts)
	}
	return p.embedBatchNew(ctx, texts)
}

// embedBatchNew sends all texts in one POST /api/embed request.
// Timeout scales with batch size: 30s + 10s per text.
func (p *OllamaProvider) embedBatchNew(ctx context.Context, texts []string) ([]float32, error) {
	perReqTimeout := 30*time.Second + time.Duration(len(texts))*10*time.Second
	reqCtx, cancel := context.WithTimeout(ctx, perReqTimeout)
	defer cancel()

	body, _ := json.Marshal(ollamaBatchEmbedRequest{
		Model: p.model,
		Input: texts,
	})
	req, err := http.NewRequestWithContext(reqCtx, "POST", p.baseURL+"/api/embed", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("ollama embed: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ollama embed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("Ollama returned status %d: %s", resp.StatusCode, string(bodyBytes))
	}

	var batchResp ollamaBatchEmbedResponse
	if err := json.NewDecoder(resp.Body).Decode(&batchResp); err != nil {
		return nil, fmt.Errorf("ollama decode: %w", err)
	}

	if len(batchResp.Embeddings) == 0 {
		return nil, fmt.Errorf("ollama embed: server returned empty embeddings")
	}
	if len(batchResp.Embeddings) != len(texts) {
		return nil, fmt.Errorf("ollama embed: server returned %d embeddings for %d texts", len(batchResp.Embeddings), len(texts))
	}
	// Convert float64 → float32 (Ollama returns float64)
	result := make([]float32, 0, len(texts)*len(batchResp.Embeddings[0]))
	for _, embedding := range batchResp.Embeddings {
		for _, v := range embedding {
			result = append(result, float32(v))
		}
	}
	return result, nil
}

// embedBatchLegacy posts one text at a time to /api/embeddings (legacy path).
// Each iteration uses an inline closure to ensure resp.Body.Close() fires
// per-iteration (not at function return), preventing fd accumulation.
func (p *OllamaProvider) embedBatchLegacy(ctx context.Context, texts []string) ([]float32, error) {
	result := make([]float32, 0)

	for _, text := range texts {
		if err := func() error {
			body, _ := json.Marshal(ollamaEmbedRequest{
				Model:  p.model,
				Prompt: text,
			})
			req, err := http.NewRequestWithContext(ctx, "POST",
				p.baseURL+"/api/embeddings", bytes.NewReader(body))
			if err != nil {
				return fmt.Errorf("ollama embed: %w", err)
			}
			req.Header.Set("Content-Type", "application/json")

			resp, err := p.client.Do(req)
			if err != nil {
				return fmt.Errorf("ollama embed: %w", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				bodyBytes, _ := io.ReadAll(resp.Body)
				return fmt.Errorf("Ollama returned status %d: %s", resp.StatusCode, string(bodyBytes))
			}

			var ollamaResp ollamaEmbedResponse
			if err := json.NewDecoder(resp.Body).Decode(&ollamaResp); err != nil {
				return fmt.Errorf("ollama decode: %w", err)
			}

			for _, v := range ollamaResp.Embedding {
				result = append(result, float32(v))
			}
			return nil
		}(); err != nil {
			return nil, err
		}
	}

	return result, nil
}

// MaxBatchSize returns 64 when the batch endpoint is available, 1 in legacy mode.
func (p *OllamaProvider) MaxBatchSize() int {
	if p.useLegacyAPI {
		return 1
	}
	return 64
}

// probeContextLength queries /api/show for model context window size.
// Logs a warning when context window < 2048 tokens (risks silent truncation).
func (p *OllamaProvider) probeContextLength(ctx context.Context) {
	showCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	body, _ := json.Marshal(ollamaShowRequest{Name: p.model})
	req, err := http.NewRequestWithContext(showCtx, "POST", p.baseURL+"/api/show", bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := p.client.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return
	}

	var show ollamaShowResponse
	if err := json.NewDecoder(resp.Body).Decode(&show); err != nil {
		return
	}

	const warnThreshold = 2048
	for k, v := range show.ModelInfo {
		if k != "llama.context_length" && k != "bert.context_length" &&
			k != "nomic_bert.context_length" && k != "qwen2.context_length" {
			continue
		}
		switch n := v.(type) {
		case float64:
			if int(n) < warnThreshold {
				slog.Warn("Ollama model has a small context window — long engrams may be truncated",
					"model", p.model,
					"context_length", int(n),
					"recommended_minimum", warnThreshold)
			} else {
				slog.Info("Ollama context length", "model", p.model, "context_length", int(n))
			}
		}
		return
	}
}

func (p *OllamaProvider) Close() error {
	if p.client != nil {
		p.client.CloseIdleConnections()
	}
	return nil
}
