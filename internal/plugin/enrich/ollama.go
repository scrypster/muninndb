package enrich

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/scrypster/muninndb/internal/plugin"
)

// OllamaLLMProvider is an HTTP client for Ollama's /api/chat endpoint.
type OllamaLLMProvider struct {
	client  *http.Client
	baseURL string
	model   string
}

// ollamaChatRequest is the request structure for Ollama chat API.
type ollamaChatRequest struct {
	Model    string          `json:"model"`
	Messages []ollamaMessage `json:"messages"`
	Stream   bool            `json:"stream"`
	Options  ollamaOptions   `json:"options,omitempty"`
}

// ollamaMessage is a message in the Ollama chat API.
type ollamaMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// ollamaOptions contains request options for Ollama.
type ollamaOptions struct {
	Temperature float32 `json:"temperature,omitempty"`
}

// ollamaChatResponse is the response structure from Ollama chat API.
type ollamaChatResponse struct {
	Message ollamaMessage `json:"message"`
}

// NewOllamaLLMProvider creates a new Ollama provider.
func NewOllamaLLMProvider() *OllamaLLMProvider {
	return &OllamaLLMProvider{
		client: &http.Client{
			Timeout:   300 * time.Second,
			Transport: plugin.WrapTransport(nil),
		},
	}
}

// Name returns the provider name.
func (p *OllamaLLMProvider) Name() string {
	return "ollama"
}

// Init initializes the provider and validates connectivity.
func (p *OllamaLLMProvider) Init(ctx context.Context, cfg LLMProviderConfig) error {
	p.baseURL = cfg.BaseURL
	p.model = cfg.Model

	// Send a probe completion request to validate connectivity
	_, err := p.Complete(ctx, "You are a helpful assistant.", "Say 'OK' only.")
	if err != nil {
		return fmt.Errorf("ollama connectivity check failed: %w", err)
	}

	return nil
}

// Complete sends a chat completion request to Ollama.
func (p *OllamaLLMProvider) Complete(ctx context.Context, system, user string) (string, error) {
	req := ollamaChatRequest{
		Model:   p.model,
		Stream:  false,
		Messages: []ollamaMessage{
			{Role: "system", Content: system},
			{Role: "user", Content: user},
		},
		Options: ollamaOptions{
			Temperature: 0.0,
		},
	}

	body, err := json.Marshal(req)
	if err != nil {
		return "", fmt.Errorf("failed to marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(
		ctx,
		"POST",
		p.baseURL+"/api/chat",
		bytes.NewReader(body),
	)
	if err != nil {
		return "", fmt.Errorf("failed to create request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("ollama returned status %d: %s", resp.StatusCode, string(bodyBytes))
	}

	var chatResp ollamaChatResponse
	if err := json.NewDecoder(resp.Body).Decode(&chatResp); err != nil {
		return "", fmt.Errorf("failed to parse response: %w", err)
	}

	return chatResp.Message.Content, nil
}

// Close releases HTTP connections.
func (p *OllamaLLMProvider) Close() error {
	p.client.CloseIdleConnections()
	return nil
}
