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

type CohereProvider struct {
	client  *http.Client
	baseURL string
	model   string
	apiKey  string
}

type cohereEmbedRequest struct {
	Texts          []string `json:"texts"`
	Model          string   `json:"model"`
	InputType      string   `json:"input_type"`
	EmbeddingTypes []string `json:"embedding_types"`
}

type cohereEmbedResponse struct {
	Embeddings struct {
		Float [][]float32 `json:"float"`
	} `json:"embeddings"`
}

func (p *CohereProvider) Name() string {
	return "cohere"
}

func (p *CohereProvider) Init(ctx context.Context, cfg ProviderHTTPConfig) (int, error) {
	p.baseURL = cfg.BaseURL
	p.model = cfg.Model
	p.apiKey = cfg.APIKey

	if p.apiKey == "" {
		return 0, fmt.Errorf("API authentication failed — check MUNINNDB_EMBED_API_KEY")
	}

	transport := &http.Transport{
		MaxIdleConns:        10,
		MaxIdleConnsPerHost: 10,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
	}
	p.client = &http.Client{
		Timeout:   10 * time.Second,
		Transport: plugin.WrapTransport(transport),
	}

	probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	reqBody, _ := json.Marshal(cohereEmbedRequest{
		Texts:          []string{"dimension detection probe"},
		Model:          p.model,
		InputType:      "search_document",
		EmbeddingTypes: []string{"float"},
	})

	req, err := http.NewRequestWithContext(probeCtx, "POST",
		p.baseURL+"/v2/embed", bytes.NewReader(reqBody))
	if err != nil {
		return 0, fmt.Errorf("cannot create embed request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", p.apiKey))

	resp, err := p.client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("API authentication failed — check MUNINNDB_EMBED_API_KEY (%w)", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return 0, fmt.Errorf("API authentication failed — check MUNINNDB_EMBED_API_KEY (status %d: %s)",
			resp.StatusCode, string(bodyBytes))
	}

	var cohereResp cohereEmbedResponse
	if err := json.NewDecoder(resp.Body).Decode(&cohereResp); err != nil {
		return 0, fmt.Errorf("failed to decode Cohere response: %w", err)
	}

	if len(cohereResp.Embeddings.Float) == 0 {
		return 0, fmt.Errorf("Cohere returned no embeddings")
	}

	dim := len(cohereResp.Embeddings.Float[0])
	if dim == 0 {
		return 0, fmt.Errorf("Cohere returned empty embedding")
	}

	slog.Info("Cohere dimension probe successful", "dimension", dim)

	return dim, nil
}

func (p *CohereProvider) EmbedBatch(ctx context.Context, texts []string) ([]float32, error) {
	reqBody, _ := json.Marshal(cohereEmbedRequest{
		Texts:          texts,
		Model:          p.model,
		InputType:      "search_document",
		EmbeddingTypes: []string{"float"},
	})

	req, err := http.NewRequestWithContext(ctx, "POST",
		p.baseURL+"/v2/embed", bytes.NewReader(reqBody))
	if err != nil {
		return nil, fmt.Errorf("cohere embed: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", p.apiKey))

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cohere embed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("Cohere returned status %d: %s", resp.StatusCode, string(bodyBytes))
	}

	var cohereResp cohereEmbedResponse
	if err := json.NewDecoder(resp.Body).Decode(&cohereResp); err != nil {
		return nil, fmt.Errorf("cohere decode: %w", err)
	}

	result := make([]float32, 0)
	for _, emb := range cohereResp.Embeddings.Float {
		result = append(result, emb...)
	}

	return result, nil
}

func (p *CohereProvider) MaxBatchSize() int {
	return 96
}

func (p *CohereProvider) Close() error {
	if p.client != nil {
		p.client.CloseIdleConnections()
	}
	return nil
}
