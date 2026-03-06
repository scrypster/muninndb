package plugin

import (
	"testing"
)

func TestParseOllamaURL(t *testing.T) {
	config, err := ParseProviderURL("ollama://localhost:11434/nomic-embed-text")
	if err != nil {
		t.Fatalf("failed to parse ollama URL: %v", err)
	}

	if config.Scheme != SchemeOllama {
		t.Errorf("expected scheme 'ollama', got %q", config.Scheme)
	}
	if config.Host != "localhost" {
		t.Errorf("expected host 'localhost', got %q", config.Host)
	}
	if config.Port != 11434 {
		t.Errorf("expected port 11434, got %d", config.Port)
	}
	if config.Model != "nomic-embed-text" {
		t.Errorf("expected model 'nomic-embed-text', got %q", config.Model)
	}
	if config.BaseURL != "http://localhost:11434" {
		t.Errorf("expected BaseURL 'http://localhost:11434', got %q", config.BaseURL)
	}
}

func TestParseOpenAIURL(t *testing.T) {
	config, err := ParseProviderURL("openai://text-embedding-3-small")
	if err != nil {
		t.Fatalf("failed to parse openai URL: %v", err)
	}

	if config.Scheme != SchemeOpenAI {
		t.Errorf("expected scheme 'openai', got %q", config.Scheme)
	}
	if config.Host != "api.openai.com" {
		t.Errorf("expected host 'api.openai.com', got %q", config.Host)
	}
	if config.Port != 443 {
		t.Errorf("expected port 443, got %d", config.Port)
	}
	if config.Model != "text-embedding-3-small" {
		t.Errorf("expected model 'text-embedding-3-small', got %q", config.Model)
	}
	if config.BaseURL != "https://api.openai.com" {
		t.Errorf("expected BaseURL 'https://api.openai.com', got %q", config.BaseURL)
	}
}

func TestParseOpenAIURL_CustomBaseURL(t *testing.T) {
	config, err := ParseProviderURL("openai://text-embedding-3-small?base_url=http://localhost:8080/v1")
	if err != nil {
		t.Fatalf("failed to parse openai URL with custom base_url: %v", err)
	}
	if config.Host != "localhost" {
		t.Errorf("expected host 'localhost', got %q", config.Host)
	}
	if config.Port != 8080 {
		t.Errorf("expected port 8080, got %d", config.Port)
	}
	if config.BaseURL != "http://localhost:8080" {
		t.Errorf("expected BaseURL 'http://localhost:8080', got %q", config.BaseURL)
	}
}

func TestParseOpenAIURL_CustomBaseURLPathPrefix(t *testing.T) {
	config, err := ParseProviderURL("openai://text-embedding-3-small?base_url=https://gateway.example.com/openai/v1")
	if err != nil {
		t.Fatalf("failed to parse openai URL with path-prefixed base_url: %v", err)
	}
	if config.Host != "gateway.example.com" {
		t.Errorf("expected host 'gateway.example.com', got %q", config.Host)
	}
	if config.Port != 443 {
		t.Errorf("expected port 443, got %d", config.Port)
	}
	if config.BaseURL != "https://gateway.example.com/openai" {
		t.Errorf("expected BaseURL 'https://gateway.example.com/openai', got %q", config.BaseURL)
	}
}

func TestParseOpenAIURL_InvalidBaseURL(t *testing.T) {
	tests := []string{
		"openai://text-embedding-3-small?base_url=ftp://localhost:8080",
		"openai://text-embedding-3-small?base_url=https://",
		"openai://text-embedding-3-small?base_url=://not-a-url",
		"openai://text-embedding-3-small?base_url=http://localhost:0",
		"openai://text-embedding-3-small?base_url=http://localhost:65536",
		// query string in base_url rejected to avoid silent config mismatch
		"openai://text-embedding-3-small?base_url=http%3A%2F%2Flocalhost%3A8080%2Fv1%3Fkey%3Dfoo",
	}
	for _, providerURL := range tests {
		if _, err := ParseProviderURL(providerURL); err == nil {
			t.Errorf("expected parse error for provider URL %q", providerURL)
		}
	}
}

func TestParseAnthropicURL(t *testing.T) {
	config, err := ParseProviderURL("anthropic://claude-haiku")
	if err != nil {
		t.Fatalf("failed to parse anthropic URL: %v", err)
	}

	if config.Scheme != SchemeAnthropic {
		t.Errorf("expected scheme 'anthropic', got %q", config.Scheme)
	}
	if config.Host != "api.anthropic.com" {
		t.Errorf("expected host 'api.anthropic.com', got %q", config.Host)
	}
	if config.Port != 443 {
		t.Errorf("expected port 443, got %d", config.Port)
	}
	if config.Model != "claude-haiku" {
		t.Errorf("expected model 'claude-haiku', got %q", config.Model)
	}
	if config.BaseURL != "https://api.anthropic.com" {
		t.Errorf("expected BaseURL 'https://api.anthropic.com', got %q", config.BaseURL)
	}
}

func TestParseVoyageURL(t *testing.T) {
	config, err := ParseProviderURL("voyage://voyage-3")
	if err != nil {
		t.Fatalf("failed to parse voyage URL: %v", err)
	}

	if config.Scheme != SchemeVoyage {
		t.Errorf("expected scheme 'voyage', got %q", config.Scheme)
	}
	if config.Host != "api.voyageai.com" {
		t.Errorf("expected host 'api.voyageai.com', got %q", config.Host)
	}
	if config.Port != 443 {
		t.Errorf("expected port 443, got %d", config.Port)
	}
	if config.Model != "voyage-3" {
		t.Errorf("expected model 'voyage-3', got %q", config.Model)
	}
	if config.BaseURL != "https://api.voyageai.com" {
		t.Errorf("expected BaseURL 'https://api.voyageai.com', got %q", config.BaseURL)
	}
}

func TestParseInvalidScheme(t *testing.T) {
	_, err := ParseProviderURL("unknown://localhost:5000/model")
	if err == nil {
		t.Error("should return error for unknown scheme")
	}
}

func TestParseMalformedURL(t *testing.T) {
	tests := []string{
		"",                    // empty
		"not-a-url",           // no scheme
		"openai://",           // missing model
		"ollama://localhost/", // missing port
		"ollama://localhost/", // missing port
	}

	for _, url := range tests {
		_, err := ParseProviderURL(url)
		if err == nil {
			t.Errorf("should return error for malformed URL: %q", url)
		}
	}
}
