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

type JinaProvider struct {
	client  *http.Client
	baseURL string
	model   string
	apiKey  string
}

type jinaEmbedRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

type jinaEmbedData struct {
	Embedding []float32 `json:"embedding"`
	Index     int       `json:"index"`
}

type jinaEmbedResponse struct {
	Data []jinaEmbedData `json:"data"`
}

func (p *JinaProvider) Name() string {
	return "jina"
}

func (p *JinaProvider) Init(ctx context.Context, cfg ProviderHTTPConfig) (int, error) {
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

	reqBody, _ := json.Marshal(jinaEmbedRequest{
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

	var jinaResp jinaEmbedResponse
	if err := json.NewDecoder(resp.Body).Decode(&jinaResp); err != nil {
		return 0, fmt.Errorf("failed to decode Jina response: %w", err)
	}

	if len(jinaResp.Data) == 0 {
		return 0, fmt.Errorf("Jina returned no embeddings")
	}

	dim := len(jinaResp.Data[0].Embedding)
	if dim == 0 {
		return 0, fmt.Errorf("Jina returned empty embedding")
	}

	slog.Info("Jina dimension probe successful", "dimension", dim)

	return dim, nil
}

func (p *JinaProvider) EmbedBatch(ctx context.Context, texts []string) ([]float32, error) {
	reqBody, _ := json.Marshal(jinaEmbedRequest{
		Model: p.model,
		Input: texts,
	})

	req, err := http.NewRequestWithContext(ctx, "POST",
		p.baseURL+"/v1/embeddings", bytes.NewReader(reqBody))
	if err != nil {
		return nil, fmt.Errorf("jina embed: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", p.apiKey))

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("jina embed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("Jina returned status %d: %s", resp.StatusCode, string(bodyBytes))
	}

	var jinaResp jinaEmbedResponse
	if err := json.NewDecoder(resp.Body).Decode(&jinaResp); err != nil {
		return nil, fmt.Errorf("jina decode: %w", err)
	}

	sort.Slice(jinaResp.Data, func(i, j int) bool {
		return jinaResp.Data[i].Index < jinaResp.Data[j].Index
	})
	result := make([]float32, 0)
	for _, data := range jinaResp.Data {
		result = append(result, data.Embedding...)
	}

	return result, nil
}

func (p *JinaProvider) MaxBatchSize() int {
	return 2048
}

func (p *JinaProvider) Close() error {
	if p.client != nil {
		p.client.CloseIdleConnections()
	}
	return nil
}
