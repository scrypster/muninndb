package main

import (
	"os"
	"testing"

	plugincfg "github.com/scrypster/muninndb/internal/config"
)

func TestResolveEmbedInfo_EnvOllama(t *testing.T) {
	clearEmbedEnv(t)
	t.Setenv("MUNINN_OLLAMA_URL", "ollama://localhost:11434/nomic-embed-text")

	info := resolveEmbedInfo(plugincfg.PluginConfig{})
	if info.Provider != "ollama" {
		t.Errorf("expected provider=ollama, got %q", info.Provider)
	}
	if info.Model != "nomic-embed-text" {
		t.Errorf("expected model=nomic-embed-text, got %q", info.Model)
	}
}

func TestResolveEmbedInfo_EnvOllamaInvalidURL(t *testing.T) {
	clearEmbedEnv(t)
	t.Setenv("MUNINN_OLLAMA_URL", "not-a-valid-url")

	info := resolveEmbedInfo(plugincfg.PluginConfig{})
	if info.Provider != "ollama" {
		t.Errorf("expected provider=ollama, got %q", info.Provider)
	}
}

func TestResolveEmbedInfo_EnvOpenAI(t *testing.T) {
	clearEmbedEnv(t)
	t.Setenv("MUNINN_OPENAI_KEY", "sk-test-key")

	info := resolveEmbedInfo(plugincfg.PluginConfig{})
	if info.Provider != "openai" {
		t.Errorf("expected provider=openai, got %q", info.Provider)
	}
	if info.Model != "text-embedding-3-small" {
		t.Errorf("expected model=text-embedding-3-small, got %q", info.Model)
	}
}

func TestResolveEmbedInfo_EnvVoyage(t *testing.T) {
	clearEmbedEnv(t)
	t.Setenv("MUNINN_VOYAGE_KEY", "voy-test-key")

	info := resolveEmbedInfo(plugincfg.PluginConfig{})
	if info.Provider != "voyage" {
		t.Errorf("expected provider=voyage, got %q", info.Provider)
	}
	if info.Model != "voyage-3" {
		t.Errorf("expected model=voyage-3, got %q", info.Model)
	}
}

func TestResolveEmbedInfo_EnvCohere(t *testing.T) {
	clearEmbedEnv(t)
	t.Setenv("MUNINN_COHERE_KEY", "cohere-test-key")

	info := resolveEmbedInfo(plugincfg.PluginConfig{})
	if info.Provider != "cohere" {
		t.Errorf("expected provider=cohere, got %q", info.Provider)
	}
	if info.Model != "embed-v4" {
		t.Errorf("expected model=embed-v4, got %q", info.Model)
	}
}

func TestResolveEmbedInfo_EnvGoogle(t *testing.T) {
	clearEmbedEnv(t)
	t.Setenv("MUNINN_GOOGLE_KEY", "google-test-key")

	info := resolveEmbedInfo(plugincfg.PluginConfig{})
	if info.Provider != "google" {
		t.Errorf("expected provider=google, got %q", info.Provider)
	}
	if info.Model != "text-embedding-004" {
		t.Errorf("expected model=text-embedding-004, got %q", info.Model)
	}
}

func TestResolveEmbedInfo_EnvJina(t *testing.T) {
	clearEmbedEnv(t)
	t.Setenv("MUNINN_JINA_KEY", "jina-test-key")

	info := resolveEmbedInfo(plugincfg.PluginConfig{})
	if info.Provider != "jina" {
		t.Errorf("expected provider=jina, got %q", info.Provider)
	}
	if info.Model != "jina-embeddings-v3" {
		t.Errorf("expected model=jina-embeddings-v3, got %q", info.Model)
	}
}

func TestResolveEmbedInfo_EnvMistral(t *testing.T) {
	clearEmbedEnv(t)
	t.Setenv("MUNINN_MISTRAL_KEY", "mistral-test-key")

	info := resolveEmbedInfo(plugincfg.PluginConfig{})
	if info.Provider != "mistral" {
		t.Errorf("expected provider=mistral, got %q", info.Provider)
	}
	if info.Model != "mistral-embed" {
		t.Errorf("expected model=mistral-embed, got %q", info.Model)
	}
}

func TestResolveEmbedInfo_ConfigFallback(t *testing.T) {
	clearEmbedEnv(t)

	cases := []struct {
		provider string
		wantProv string
		wantMod  string
	}{
		{"openai", "openai", "text-embedding-3-small"},
		{"voyage", "voyage", "voyage-3"},
		{"cohere", "cohere", "embed-v4"},
		{"google", "google", "text-embedding-004"},
		{"jina", "jina", "jina-embeddings-v3"},
		{"mistral", "mistral", "mistral-embed"},
		{"none", "none", ""},
	}
	for _, tc := range cases {
		cfg := plugincfg.PluginConfig{EmbedProvider: tc.provider}
		info := resolveEmbedInfo(cfg)
		if info.Provider != tc.wantProv {
			t.Errorf("config provider=%q: got provider=%q, want %q", tc.provider, info.Provider, tc.wantProv)
		}
		if info.Model != tc.wantMod {
			t.Errorf("config provider=%q: got model=%q, want %q", tc.provider, info.Model, tc.wantMod)
		}
	}
}

func TestResolveEmbedInfo_ConfigOllamaWithURL(t *testing.T) {
	clearEmbedEnv(t)

	cfg := plugincfg.PluginConfig{
		EmbedProvider: "ollama",
		EmbedURL:      "ollama://localhost:11434/mxbai-embed-large",
	}
	info := resolveEmbedInfo(cfg)
	if info.Provider != "ollama" {
		t.Errorf("expected provider=ollama, got %q", info.Provider)
	}
	if info.Model != "mxbai-embed-large" {
		t.Errorf("expected model=mxbai-embed-large, got %q", info.Model)
	}
}

func TestResolveEmbedInfo_EnvPriorityOverConfig(t *testing.T) {
	clearEmbedEnv(t)
	t.Setenv("MUNINN_OPENAI_KEY", "sk-override")

	cfg := plugincfg.PluginConfig{EmbedProvider: "voyage"}
	info := resolveEmbedInfo(cfg)
	if info.Provider != "openai" {
		t.Errorf("env should override config: got provider=%q, want openai", info.Provider)
	}
}

func TestParseCORSOrigins(t *testing.T) {
	cases := []struct {
		input string
		want  []string
	}{
		{"", nil},
		{"http://localhost:3000", []string{"http://localhost:3000"}},
		{"http://localhost:3000,http://example.com", []string{"http://localhost:3000", "http://example.com"}},
		{"http://localhost:3000 , http://example.com", []string{"http://localhost:3000", "http://example.com"}},
		{" , , ", nil},
	}
	for _, tc := range cases {
		got := parseCORSOrigins(tc.input)
		if len(got) != len(tc.want) {
			t.Errorf("parseCORSOrigins(%q): got %v (len %d), want %v (len %d)", tc.input, got, len(got), tc.want, len(tc.want))
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("parseCORSOrigins(%q)[%d]: got %q, want %q", tc.input, i, got[i], tc.want[i])
			}
		}
	}
}

func TestValidateServerFlags(t *testing.T) {
	cases := []struct {
		addrs   []string
		wantErr bool
	}{
		{[]string{"127.0.0.1:8474"}, false},
		{[]string{"127.0.0.1:8474", "127.0.0.1:8475", "127.0.0.1:8750"}, false},
		{[]string{":8474"}, false},
		{[]string{"0.0.0.0:1"}, false},
		{[]string{"0.0.0.0:65535"}, false},
		{[]string{"invalid-addr"}, true},
		{[]string{"127.0.0.1:0"}, true},
		{[]string{"127.0.0.1:99999"}, true},
		{[]string{"127.0.0.1:abc"}, true},
		{[]string{"127.0.0.1:8474", "bad-addr"}, true},
	}
	for _, tc := range cases {
		err := validateServerFlags(tc.addrs...)
		if tc.wantErr && err == nil {
			t.Errorf("validateServerFlags(%v): expected error, got nil", tc.addrs)
		}
		if !tc.wantErr && err != nil {
			t.Errorf("validateServerFlags(%v): unexpected error: %v", tc.addrs, err)
		}
	}
}

func TestApplyMemoryLimits_Defaults(t *testing.T) {
	t.Setenv("MUNINN_MEM_LIMIT_GB", "")
	t.Setenv("MUNINN_GC_PERCENT", "")
	os.Unsetenv("MUNINN_MEM_LIMIT_GB")
	os.Unsetenv("MUNINN_GC_PERCENT")

	applyMemoryLimits()
}

func TestApplyMemoryLimits_CustomValues(t *testing.T) {
	t.Setenv("MUNINN_MEM_LIMIT_GB", "8")
	t.Setenv("MUNINN_GC_PERCENT", "100")

	applyMemoryLimits()
}

func TestApplyMemoryLimits_InvalidValues(t *testing.T) {
	t.Setenv("MUNINN_MEM_LIMIT_GB", "not-a-number")
	t.Setenv("MUNINN_GC_PERCENT", "abc")

	applyMemoryLimits()
}

func TestApplyMemoryLimits_ZeroValues(t *testing.T) {
	t.Setenv("MUNINN_MEM_LIMIT_GB", "0")
	t.Setenv("MUNINN_GC_PERCENT", "0")

	applyMemoryLimits()
}

// clearEmbedEnv unsets all embed-related env vars for a clean test.
func clearEmbedEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"MUNINN_OLLAMA_URL", "MUNINN_OPENAI_KEY", "MUNINN_VOYAGE_KEY",
		"MUNINN_COHERE_KEY", "MUNINN_GOOGLE_KEY", "MUNINN_JINA_KEY",
		"MUNINN_MISTRAL_KEY", "MUNINN_LOCAL_EMBED",
	} {
		t.Setenv(key, "")
		os.Unsetenv(key)
	}
	t.Setenv("MUNINN_LOCAL_EMBED", "0")
}
