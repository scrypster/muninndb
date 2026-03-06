package enrich

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// OpenAILLMProvider is an HTTP client for OpenAI's /v1/chat/completions endpoint.
type OpenAILLMProvider struct {
	client  *http.Client
	baseURL string
	model   string
	apiKey  string
}

// openaiChatRequest is the request structure for OpenAI chat API.
type openaiChatRequest struct {
	Model            string               `json:"model"`
	Messages         []openaiMessage      `json:"messages"`
	Temperature      float32              `json:"temperature"`
	MaxTokens        int                  `json:"max_tokens,omitempty"`
	ResponseFormat   *openaiResponseFormat `json:"response_format,omitempty"`
}

// openaiMessage is a message in the OpenAI chat API.
type openaiMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// openaiResponseFormat specifies JSON response format for OpenAI.
type openaiResponseFormat struct {
	Type string `json:"type"`
}

// openaiChatResponse is the response structure from OpenAI chat API.
type openaiChatResponse struct {
	Choices []struct {
		Message openaiMessage `json:"message"`
	} `json:"choices"`
}

// NewOpenAILLMProvider creates a new OpenAI provider.
func NewOpenAILLMProvider() *OpenAILLMProvider {
	return &OpenAILLMProvider{
		client: &http.Client{
			Timeout: 300 * time.Second,
		},
	}
}

// Name returns the provider name.
func (p *OpenAILLMProvider) Name() string {
	return "openai"
}

// Init initializes the provider and validates connectivity.
func (p *OpenAILLMProvider) Init(ctx context.Context, cfg LLMProviderConfig) error {
	p.baseURL = cfg.BaseURL
	p.model = cfg.Model
	p.apiKey = cfg.APIKey

	if p.apiKey == "" {
		return fmt.Errorf("openai provider requires API key")
	}

	// Send a probe completion request to validate connectivity.
	// The user message must contain the word "json" because Complete always
	// sets response_format:json_object — OpenAI rejects requests where none
	// of the messages mention json when that format is requested.
	_, err := p.Complete(ctx, "You are a connectivity probe. Respond with valid JSON only.", `{"ok":true}`)
	if err != nil {
		return fmt.Errorf("openai connectivity check failed: %w", err)
	}

	return nil
}

// Complete sends a chat completion request to OpenAI.
func (p *OpenAILLMProvider) Complete(ctx context.Context, system, user string) (string, error) {
	req := openaiChatRequest{
		Model:       p.model,
		Temperature: 0.0,
		MaxTokens:   1024,
		Messages: []openaiMessage{
			{Role: "system", Content: system},
			{Role: "user", Content: user},
		},
		ResponseFormat: &openaiResponseFormat{
			Type: "json_object",
		},
	}

	body, err := json.Marshal(req)
	if err != nil {
		return "", fmt.Errorf("failed to marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(
		ctx,
		"POST",
		p.baseURL+"/v1/chat/completions",
		bytes.NewReader(body),
	)
	if err != nil {
		return "", fmt.Errorf("failed to create request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("openai returned status %d: %s", resp.StatusCode, string(bodyBytes))
	}

	var chatResp openaiChatResponse
	if err := json.NewDecoder(resp.Body).Decode(&chatResp); err != nil {
		return "", fmt.Errorf("failed to parse response: %w", err)
	}

	if len(chatResp.Choices) == 0 {
		return "", fmt.Errorf("openai response has no choices")
	}

	return chatResp.Choices[0].Message.Content, nil
}

// Close releases HTTP connections.
func (p *OpenAILLMProvider) Close() error {
	p.client.CloseIdleConnections()
	return nil
}
