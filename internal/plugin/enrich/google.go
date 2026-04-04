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

// GoogleLLMProvider is an HTTP client for Google's Gemini generateContent endpoint.
type GoogleLLMProvider struct {
	client  *http.Client
	baseURL string
	model   string
	apiKey  string
}

// googleGenerateRequest is the request structure for Gemini generateContent.
type googleGenerateRequest struct {
	Contents          []googleContent       `json:"contents"`
	SystemInstruction *googleSystemContent  `json:"systemInstruction,omitempty"`
	GenerationConfig  googleGenerationConfig `json:"generationConfig"`
}

type googleContent struct {
	Role  string       `json:"role"`
	Parts []googlePart `json:"parts"`
}

type googleSystemContent struct {
	Parts []googlePart `json:"parts"`
}

type googlePart struct {
	Text string `json:"text"`
}

type googleGenerationConfig struct {
	Temperature      float32 `json:"temperature"`
	MaxOutputTokens  int     `json:"maxOutputTokens"`
	ResponseMimeType string  `json:"responseMimeType"`
}

// googleGenerateResponse is the response structure from Gemini generateContent.
type googleGenerateResponse struct {
	Candidates []struct {
		Content struct {
			Parts []googlePart `json:"parts"`
		} `json:"content"`
	} `json:"candidates"`
}

// NewGoogleLLMProvider creates a new Google Gemini provider.
func NewGoogleLLMProvider() *GoogleLLMProvider {
	return &GoogleLLMProvider{
		client: &http.Client{
			Timeout:   300 * time.Second,
			Transport: plugin.WrapTransport(nil),
		},
	}
}

// Name returns the provider name.
func (p *GoogleLLMProvider) Name() string {
	return "google"
}

// Init initializes the provider and validates connectivity.
func (p *GoogleLLMProvider) Init(ctx context.Context, cfg LLMProviderConfig) error {
	p.baseURL = cfg.BaseURL
	p.model = cfg.Model
	p.apiKey = cfg.APIKey

	if p.apiKey == "" {
		return fmt.Errorf("google provider requires API key")
	}

	// Send a probe completion request to validate connectivity.
	// The system prompt explicitly mentions "json" to be consistent with the
	// OpenAI provider pattern — defensively guards against providers that
	// reject JSON output mode without a json keyword in the prompt.
	_, err := p.Complete(ctx, "You are a connectivity probe. Respond with valid JSON only.", `{"ok":true}`)
	if err != nil {
		return fmt.Errorf("google connectivity check failed: %w", err)
	}

	return nil
}

// Complete sends a generateContent request to the Gemini API.
func (p *GoogleLLMProvider) Complete(ctx context.Context, system, user string) (string, error) {
	req := googleGenerateRequest{
		Contents: []googleContent{
			{Role: "user", Parts: []googlePart{{Text: user}}},
		},
		SystemInstruction: &googleSystemContent{
			Parts: []googlePart{{Text: system}},
		},
		GenerationConfig: googleGenerationConfig{
			Temperature:      0.0,
			MaxOutputTokens:  1024,
			ResponseMimeType: "application/json",
		},
	}

	body, err := json.Marshal(req)
	if err != nil {
		return "", fmt.Errorf("failed to marshal request: %w", err)
	}

	url := fmt.Sprintf("%s/v1beta/models/%s:generateContent", p.baseURL, p.model)
	httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("failed to create request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	// Google uses x-goog-api-key, not Authorization: Bearer.
	httpReq.Header.Set("x-goog-api-key", p.apiKey)

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("google returned status %d: %s", resp.StatusCode, string(bodyBytes))
	}

	var genResp googleGenerateResponse
	if err := json.NewDecoder(resp.Body).Decode(&genResp); err != nil {
		return "", fmt.Errorf("failed to parse response: %w", err)
	}

	if len(genResp.Candidates) == 0 || len(genResp.Candidates[0].Content.Parts) == 0 {
		return "", fmt.Errorf("google response has no candidates")
	}

	return genResp.Candidates[0].Content.Parts[0].Text, nil
}

// Close releases HTTP connections.
func (p *GoogleLLMProvider) Close() error {
	p.client.CloseIdleConnections()
	return nil
}
