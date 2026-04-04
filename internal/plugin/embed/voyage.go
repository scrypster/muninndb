package embed

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"time"

	"github.com/scrypster/muninndb/internal/plugin"
)

type VoyageProvider struct {
	client  *http.Client
	baseURL string
	model   string
	apiKey  string
}

type voyageEmbedRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

type voyageEmbedData struct {
	Embedding []float32 `json:"embedding"`
	Index     int       `json:"index"`
}

type voyageEmbedResponse struct {
	Data []voyageEmbedData `json:"data"`
}

func (p *VoyageProvider) Name() string {
	return "voyage"
}

func (p *VoyageProvider) Init(ctx context.Context, cfg ProviderHTTPConfig) (int, error) {
	p.baseURL = cfg.BaseURL
	p.model = cfg.Model
	p.apiKey = cfg.APIKey

	if p.apiKey == "" {
		return 0, fmt.Errorf("API authentication failed — check MUNINNDB_EMBED_API_KEY")
	}

	// Create HTTP client with 10s timeout (cloud)
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

	// Embed probe text to detect dimension
	probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	reqBody, _ := json.Marshal(voyageEmbedRequest{
		Model: p.model,
		Input: []string{"dimension detection probe"},
	})

	req, err := http.NewRequestWithContext(probeCtx, "POST",
		p.baseURL+"/v1/embeddings", bytes.NewReader(reqBody))
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

	var voyageResp voyageEmbedResponse
	if err := json.NewDecoder(resp.Body).Decode(&voyageResp); err != nil {
		return 0, fmt.Errorf("failed to decode Voyage response: %w", err)
	}

	if len(voyageResp.Data) == 0 {
		return 0, fmt.Errorf("Voyage returned no embeddings")
	}

	dim := len(voyageResp.Data[0].Embedding)
	if dim == 0 {
		return 0, fmt.Errorf("Voyage returned empty embedding")
	}

	slog.Info("Voyage dimension probe successful", "dimension", dim)

	return dim, nil
}

func (p *VoyageProvider) EmbedBatch(ctx context.Context, texts []string) ([]float32, error) {
	reqBody, _ := json.Marshal(voyageEmbedRequest{
		Model: p.model,
		Input: texts,
	})

	req, err := http.NewRequestWithContext(ctx, "POST",
		p.baseURL+"/v1/embeddings", bytes.NewReader(reqBody))
	if err != nil {
		return nil, fmt.Errorf("voyage embed: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", p.apiKey))

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("voyage embed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("Voyage returned status %d: %s", resp.StatusCode, string(bodyBytes))
	}

	var voyageResp voyageEmbedResponse
	if err := json.NewDecoder(resp.Body).Decode(&voyageResp); err != nil {
		return nil, fmt.Errorf("voyage decode: %w", err)
	}

	// Sort by Index field to handle out-of-order API responses (allowed by spec).
	sort.Slice(voyageResp.Data, func(i, j int) bool {
		return voyageResp.Data[i].Index < voyageResp.Data[j].Index
	})
	result := make([]float32, 0)
	for _, data := range voyageResp.Data {
		result = append(result, data.Embedding...)
	}

	return result, nil
}

func (p *VoyageProvider) MaxBatchSize() int {
	// Voyage supports batch embedding
	return 128
}

func (p *VoyageProvider) Close() error {
	if p.client != nil {
		p.client.CloseIdleConnections()
	}
	return nil
}
