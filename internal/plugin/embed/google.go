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

type GoogleProvider struct {
	client  *http.Client
	baseURL string
	model   string
	apiKey  string
}

type googleEmbedContentRequest struct {
	Content googleContent `json:"content"`
}

type googleContent struct {
	Parts []googlePart `json:"parts"`
}

type googlePart struct {
	Text string `json:"text"`
}

type googleEmbedContentResponse struct {
	Embedding struct {
		Values []float32 `json:"values"`
	} `json:"embedding"`
}

type googleBatchEmbedRequest struct {
	Requests []googleBatchEmbedItem `json:"requests"`
}

type googleBatchEmbedItem struct {
	Model   string        `json:"model"`
	Content googleContent `json:"content"`
}

type googleBatchEmbedResponse struct {
	Embeddings []struct {
		Values []float32 `json:"values"`
	} `json:"embeddings"`
}

func (p *GoogleProvider) Name() string {
	return "google"
}

func (p *GoogleProvider) Init(ctx context.Context, cfg ProviderHTTPConfig) (int, error) {
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

	reqBody, _ := json.Marshal(googleEmbedContentRequest{
		Content: googleContent{
			Parts: []googlePart{{Text: "dimension detection probe"}},
		},
	})

	url := fmt.Sprintf("%s/v1beta/models/%s:embedContent?key=%s", p.baseURL, p.model, p.apiKey)
	req, err := http.NewRequestWithContext(probeCtx, "POST", url, bytes.NewReader(reqBody))
	if err != nil {
		return 0, fmt.Errorf("cannot create embed request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

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

	var googleResp googleEmbedContentResponse
	if err := json.NewDecoder(resp.Body).Decode(&googleResp); err != nil {
		return 0, fmt.Errorf("failed to decode Google response: %w", err)
	}

	dim := len(googleResp.Embedding.Values)
	if dim == 0 {
		return 0, fmt.Errorf("Google returned empty embedding")
	}

	slog.Info("Google dimension probe successful", "dimension", dim)

	return dim, nil
}

func (p *GoogleProvider) EmbedBatch(ctx context.Context, texts []string) ([]float32, error) {
	requests := make([]googleBatchEmbedItem, len(texts))
	for i, text := range texts {
		requests[i] = googleBatchEmbedItem{
			Model:   "models/" + p.model,
			Content: googleContent{Parts: []googlePart{{Text: text}}},
		}
	}

	reqBody, _ := json.Marshal(googleBatchEmbedRequest{Requests: requests})

	url := fmt.Sprintf("%s/v1beta/models/%s:batchEmbedContents?key=%s", p.baseURL, p.model, p.apiKey)
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(reqBody))
	if err != nil {
		return nil, fmt.Errorf("google embed: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("google embed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("Google returned status %d: %s", resp.StatusCode, string(bodyBytes))
	}

	var googleResp googleBatchEmbedResponse
	if err := json.NewDecoder(resp.Body).Decode(&googleResp); err != nil {
		return nil, fmt.Errorf("google decode: %w", err)
	}

	result := make([]float32, 0)
	for _, emb := range googleResp.Embeddings {
		result = append(result, emb.Values...)
	}

	return result, nil
}

func (p *GoogleProvider) MaxBatchSize() int {
	return 100
}

func (p *GoogleProvider) Close() error {
	if p.client != nil {
		p.client.CloseIdleConnections()
	}
	return nil
}
