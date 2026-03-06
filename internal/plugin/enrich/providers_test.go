package enrich

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// --- Ollama ---

func TestOllamaProvider_Name(t *testing.T) {
	p := NewOllamaLLMProvider()
	if p.Name() != "ollama" {
		t.Fatalf("expected 'ollama', got %q", p.Name())
	}
}

func TestOllamaProvider_Complete_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/chat" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("missing content-type header")
		}

		var req ollamaChatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if req.Model != "test-model" {
			t.Errorf("expected model 'test-model', got %q", req.Model)
		}
		if len(req.Messages) != 2 {
			t.Errorf("expected 2 messages, got %d", len(req.Messages))
		}

		resp := ollamaChatResponse{
			Message: ollamaMessage{Role: "assistant", Content: "hello world"},
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	p := NewOllamaLLMProvider()
	p.baseURL = srv.URL
	p.model = "test-model"

	got, err := p.Complete(context.Background(), "system", "user")
	if err != nil {
		t.Fatalf("Complete failed: %v", err)
	}
	if got != "hello world" {
		t.Fatalf("expected 'hello world', got %q", got)
	}
}

func TestOllamaProvider_Complete_ErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("server error"))
	}))
	defer srv.Close()

	p := NewOllamaLLMProvider()
	p.baseURL = srv.URL
	p.model = "m"

	_, err := p.Complete(context.Background(), "s", "u")
	if err == nil {
		t.Fatal("expected error for 500 status")
	}
}

func TestOllamaProvider_Complete_BadJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("not json"))
	}))
	defer srv.Close()

	p := NewOllamaLLMProvider()
	p.baseURL = srv.URL
	p.model = "m"

	_, err := p.Complete(context.Background(), "s", "u")
	if err == nil {
		t.Fatal("expected error for bad JSON")
	}
}

func TestOllamaProvider_Init_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		resp := ollamaChatResponse{
			Message: ollamaMessage{Role: "assistant", Content: "OK"},
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	p := NewOllamaLLMProvider()
	err := p.Init(context.Background(), LLMProviderConfig{
		BaseURL: srv.URL,
		Model:   "test",
	})
	if err != nil {
		t.Fatalf("Init failed: %v", err)
	}
}

func TestOllamaProvider_Init_ConnectivityFail(t *testing.T) {
	p := NewOllamaLLMProvider()
	err := p.Init(context.Background(), LLMProviderConfig{
		BaseURL: "http://127.0.0.1:1", // unreachable port
		Model:   "test",
	})
	if err == nil {
		t.Fatal("expected error for unreachable host")
	}
}

func TestOllamaProvider_Close(t *testing.T) {
	p := NewOllamaLLMProvider()
	if err := p.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}
}

// --- OpenAI ---

func TestOpenAIProvider_Name(t *testing.T) {
	p := NewOpenAILLMProvider()
	if p.Name() != "openai" {
		t.Fatalf("expected 'openai', got %q", p.Name())
	}
}

func TestOpenAIProvider_Complete_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("bad auth header: %s", r.Header.Get("Authorization"))
		}

		var req openaiChatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if req.Model != "gpt-4o-mini" {
			t.Errorf("expected model 'gpt-4o-mini', got %q", req.Model)
		}

		resp := openaiChatResponse{
			Choices: []struct {
				Message openaiMessage `json:"message"`
			}{
				{Message: openaiMessage{Role: "assistant", Content: "test response"}},
			},
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	p := NewOpenAILLMProvider()
	p.baseURL = srv.URL
	p.model = "gpt-4o-mini"
	p.apiKey = "test-key"

	got, err := p.Complete(context.Background(), "system", "user")
	if err != nil {
		t.Fatalf("Complete failed: %v", err)
	}
	if got != "test response" {
		t.Fatalf("expected 'test response', got %q", got)
	}
}

func TestOpenAIProvider_Complete_ErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte("rate limited"))
	}))
	defer srv.Close()

	p := NewOpenAILLMProvider()
	p.baseURL = srv.URL
	p.model = "m"
	p.apiKey = "k"

	_, err := p.Complete(context.Background(), "s", "u")
	if err == nil {
		t.Fatal("expected error for 429 status")
	}
}

func TestOpenAIProvider_Complete_NoChoices(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		resp := openaiChatResponse{Choices: nil}
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	p := NewOpenAILLMProvider()
	p.baseURL = srv.URL
	p.model = "m"
	p.apiKey = "k"

	_, err := p.Complete(context.Background(), "s", "u")
	if err == nil {
		t.Fatal("expected error for empty choices")
	}
}

func TestOpenAIProvider_Complete_BadJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("{invalid"))
	}))
	defer srv.Close()

	p := NewOpenAILLMProvider()
	p.baseURL = srv.URL
	p.model = "m"
	p.apiKey = "k"

	_, err := p.Complete(context.Background(), "s", "u")
	if err == nil {
		t.Fatal("expected error for bad JSON")
	}
}

func TestOpenAIProvider_Init_MissingKey(t *testing.T) {
	p := NewOpenAILLMProvider()
	err := p.Init(context.Background(), LLMProviderConfig{
		BaseURL: "http://localhost",
		Model:   "m",
		APIKey:  "",
	})
	if err == nil {
		t.Fatal("expected error for missing API key")
	}
}

func TestOpenAIProvider_Init_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		resp := openaiChatResponse{
			Choices: []struct {
				Message openaiMessage `json:"message"`
			}{
				{Message: openaiMessage{Role: "assistant", Content: "OK"}},
			},
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	p := NewOpenAILLMProvider()
	err := p.Init(context.Background(), LLMProviderConfig{
		BaseURL: srv.URL,
		Model:   "test",
		APIKey:  "key",
	})
	if err != nil {
		t.Fatalf("Init failed: %v", err)
	}
}

// TestOpenAIProvider_Init_ProbeContainsJSON is a regression test for the bug
// where the connectivity probe sent "Say 'OK' only." without the word "json",
// causing OpenAI to reject the request with HTTP 400 when response_format is
// json_object. Asserts that all probe messages contain "json".
func TestOpenAIProvider_Init_ProbeContainsJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req openaiChatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("failed to decode request: %v", err)
		}
		// OpenAI requires at least one message to contain the word "json"
		// when response_format=json_object is set.
		hasJSON := false
		for _, msg := range req.Messages {
			if strings.Contains(strings.ToLower(msg.Content), "json") {
				hasJSON = true
				break
			}
		}
		if !hasJSON {
			t.Errorf("no probe message contains 'json' — OpenAI rejects response_format=json_object when 'json' does not appear in any message")
		}
		resp := openaiChatResponse{
			Choices: []struct {
				Message openaiMessage `json:"message"`
			}{
				{Message: openaiMessage{Role: "assistant", Content: `{"ok":true}`}},
			},
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	p := NewOpenAILLMProvider()
	if err := p.Init(context.Background(), LLMProviderConfig{
		BaseURL: srv.URL,
		Model:   "test",
		APIKey:  "key",
	}); err != nil {
		t.Fatalf("Init failed: %v", err)
	}
}

func TestOpenAIProvider_Close(t *testing.T) {
	p := NewOpenAILLMProvider()
	if err := p.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}
}

// --- Anthropic ---

func TestAnthropicProvider_Name(t *testing.T) {
	p := NewAnthropicLLMProvider()
	if p.Name() != "anthropic" {
		t.Fatalf("expected 'anthropic', got %q", p.Name())
	}
}

func TestAnthropicProvider_Complete_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.Header.Get("x-api-key") != "test-key" {
			t.Errorf("bad x-api-key header: %s", r.Header.Get("x-api-key"))
		}
		if r.Header.Get("anthropic-version") != "2023-06-01" {
			t.Errorf("missing anthropic-version header")
		}

		var req anthropicMessagesRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if req.System == "" {
			t.Error("expected non-empty system prompt")
		}

		resp := anthropicMessagesResponse{
			Content: []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			}{
				{Type: "text", Text: "anthropic response"},
			},
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	p := NewAnthropicLLMProvider()
	p.baseURL = srv.URL
	p.model = "claude-haiku"
	p.apiKey = "test-key"

	got, err := p.Complete(context.Background(), "system prompt", "user msg")
	if err != nil {
		t.Fatalf("Complete failed: %v", err)
	}
	if got != "anthropic response" {
		t.Fatalf("expected 'anthropic response', got %q", got)
	}
}

func TestAnthropicProvider_Complete_ErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte("forbidden"))
	}))
	defer srv.Close()

	p := NewAnthropicLLMProvider()
	p.baseURL = srv.URL
	p.model = "m"
	p.apiKey = "k"

	_, err := p.Complete(context.Background(), "s", "u")
	if err == nil {
		t.Fatal("expected error for 403 status")
	}
}

func TestAnthropicProvider_Complete_NoContent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		resp := anthropicMessagesResponse{Content: nil}
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	p := NewAnthropicLLMProvider()
	p.baseURL = srv.URL
	p.model = "m"
	p.apiKey = "k"

	_, err := p.Complete(context.Background(), "s", "u")
	if err == nil {
		t.Fatal("expected error for no content")
	}
}

func TestAnthropicProvider_Complete_NoTextBlocks(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		resp := anthropicMessagesResponse{
			Content: []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			}{
				{Type: "image", Text: ""},
			},
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	p := NewAnthropicLLMProvider()
	p.baseURL = srv.URL
	p.model = "m"
	p.apiKey = "k"

	_, err := p.Complete(context.Background(), "s", "u")
	if err == nil {
		t.Fatal("expected error for no text blocks")
	}
}

func TestAnthropicProvider_Complete_BadJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("}{bad"))
	}))
	defer srv.Close()

	p := NewAnthropicLLMProvider()
	p.baseURL = srv.URL
	p.model = "m"
	p.apiKey = "k"

	_, err := p.Complete(context.Background(), "s", "u")
	if err == nil {
		t.Fatal("expected error for bad JSON")
	}
}

func TestAnthropicProvider_Init_MissingKey(t *testing.T) {
	p := NewAnthropicLLMProvider()
	err := p.Init(context.Background(), LLMProviderConfig{
		BaseURL: "http://localhost",
		Model:   "m",
		APIKey:  "",
	})
	if err == nil {
		t.Fatal("expected error for missing API key")
	}
}

func TestAnthropicProvider_Init_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		resp := anthropicMessagesResponse{
			Content: []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			}{
				{Type: "text", Text: "OK"},
			},
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	p := NewAnthropicLLMProvider()
	err := p.Init(context.Background(), LLMProviderConfig{
		BaseURL: srv.URL,
		Model:   "test",
		APIKey:  "key",
	})
	if err != nil {
		t.Fatalf("Init failed: %v", err)
	}
}

func TestAnthropicProvider_Close(t *testing.T) {
	p := NewAnthropicLLMProvider()
	if err := p.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}
}
