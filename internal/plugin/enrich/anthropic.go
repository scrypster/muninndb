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

// AnthropicLLMProvider is an HTTP client for Anthropic's /v1/messages endpoint.
type AnthropicLLMProvider struct {
	client  *http.Client
	baseURL string
	model   string
	apiKey  string
}

// anthropicMessagesRequest is the request structure for Anthropic messages API.
type anthropicMessagesRequest struct {
	Model       string               `json:"model"`
	MaxTokens   int                  `json:"max_tokens"`
	System      string               `json:"system"`
	Messages    []anthropicMessage   `json:"messages"`
}

// anthropicMessage is a message in the Anthropic messages API.
type anthropicMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// anthropicMessagesResponse is the response structure from Anthropic messages API.
type anthropicMessagesResponse struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
}

// NewAnthropicLLMProvider creates a new Anthropic provider.
func NewAnthropicLLMProvider() *AnthropicLLMProvider {
	return &AnthropicLLMProvider{
		client: &http.Client{
			Timeout:   300 * time.Second,
			Transport: plugin.WrapTransport(nil),
		},
	}
}

// Name returns the provider name.
func (p *AnthropicLLMProvider) Name() string {
	return "anthropic"
}

// Init initializes the provider and validates connectivity.
func (p *AnthropicLLMProvider) Init(ctx context.Context, cfg LLMProviderConfig) error {
	p.baseURL = cfg.BaseURL
	p.model = cfg.Model
	p.apiKey = cfg.APIKey

	if p.apiKey == "" {
		return fmt.Errorf("anthropic provider requires API key")
	}

	// Send a probe completion request to validate connectivity
	_, err := p.Complete(ctx, "You are a helpful assistant.", "Say 'OK' only.")
	if err != nil {
		return fmt.Errorf("anthropic connectivity check failed: %w", err)
	}

	return nil
}

// Complete sends a messages request to Anthropic.
func (p *AnthropicLLMProvider) Complete(ctx context.Context, system, user string) (string, error) {
	req := anthropicMessagesRequest{
		Model:     p.model,
		MaxTokens: 1024,
		System:    system,
		Messages: []anthropicMessage{
			{Role: "user", Content: user},
		},
	}

	body, err := json.Marshal(req)
	if err != nil {
		return "", fmt.Errorf("failed to marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(
		ctx,
		"POST",
		p.baseURL+"/v1/messages",
		bytes.NewReader(body),
	)
	if err != nil {
		return "", fmt.Errorf("failed to create request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-api-key", p.apiKey)
	httpReq.Header.Set("anthropic-version", "2023-06-01")

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("anthropic returned status %d: %s", resp.StatusCode, string(bodyBytes))
	}

	var messagesResp anthropicMessagesResponse
	if err := json.NewDecoder(resp.Body).Decode(&messagesResp); err != nil {
		return "", fmt.Errorf("failed to parse response: %w", err)
	}

	if len(messagesResp.Content) == 0 {
		return "", fmt.Errorf("anthropic response has no content")
	}

	// Return the first text block
	for _, block := range messagesResp.Content {
		if block.Type == "text" {
			return block.Text, nil
		}
	}

	return "", fmt.Errorf("anthropic response has no text blocks")
}

// Close releases HTTP connections.
func (p *AnthropicLLMProvider) Close() error {
	p.client.CloseIdleConnections()
	return nil
}
