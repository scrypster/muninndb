package main

import (
	"os"
	"testing"

	plugincfg "github.com/scrypster/muninndb/internal/config"
)

func TestAllAddrDefaults_UseListenHost(t *testing.T) {
	host := parseListenHost([]string{"--listen-host", "10.0.0.1"}, "")
	cases := []struct{ name, port, want string }{
		{"mbp", "8474", "10.0.0.1:8474"},
		{"rest", "8475", "10.0.0.1:8475"},
		{"mcp", "8750", "10.0.0.1:8750"},
		{"grpc", "8477", "10.0.0.1:8477"},
		{"ui", "8476", "10.0.0.1:8476"},
	}
	for _, c := range cases {
		got := host + ":" + c.port
		if got != c.want {
			t.Errorf("%s addr: got %s, want %s", c.name, got, c.want)
		}
	}
}

func TestMUNINN_UI_ADDR_EnvOverridesListenHost(t *testing.T) {
	t.Setenv("MUNINN_UI_ADDR", "192.168.1.100:9999")
	uiAddrDefault := "10.0.0.1:8476"
	if v := os.Getenv("MUNINN_UI_ADDR"); v != "" {
		uiAddrDefault = v
	}
	if uiAddrDefault != "192.168.1.100:9999" {
		t.Errorf("expected 192.168.1.100:9999, got %s", uiAddrDefault)
	}
}

func TestCORSOriginsResolution(t *testing.T) {
	cases := []struct {
		input string
		want  []string
	}{
		{"http://flag.local", []string{"http://flag.local"}},
		{"http://env.local", []string{"http://env.local"}},
		{"http://a.com,http://b.com", []string{"http://a.com", "http://b.com"}},
		{"", nil},
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

func TestBuildDaemonArgs_CORSFlagBeatsEnv(t *testing.T) {
	osArgs := []string{"--cors-origins=http://flag.local"}
	corsOriginsEnv := "http://env.local"
	got := buildDaemonArgs("/tmp/data", false, osArgs, "", corsOriginsEnv)

	foundFlag := false
	foundEnv := false
	for _, arg := range got {
		if arg == "http://flag.local" {
			foundFlag = true
		}
		if arg == "http://env.local" {
			foundEnv = true
		}
	}
	if !foundFlag {
		t.Errorf("expected http://flag.local in args %v", got)
	}
	if foundEnv {
		t.Errorf("expected http://env.local to be absent from args %v", got)
	}
}

// TestBuildDaemonArgs_PortFlagsForwarded is a regression test for GitHub
// issue #100: --mbp-addr, --rest-addr, --grpc-addr, --mcp-addr were silently
// ignored because buildDaemonArgs never forwarded them to the daemon process.
func TestBuildDaemonArgs_PortFlagsForwarded(t *testing.T) {
	cases := []struct {
		flag string
		val  string
	}{
		{"--rest-addr", "127.0.0.1:8485"},
		{"--mbp-addr", "127.0.0.1:8494"},
		{"--grpc-addr", "127.0.0.1:8497"},
		{"--mcp-addr", "127.0.0.1:8760"},
		{"--ui-addr", "127.0.0.1:8486"},
	}
	for _, tc := range cases {
		osArgs := []string{tc.flag, tc.val}
		got := buildDaemonArgs("/tmp/data", false, osArgs, "", "")

		foundFlag := false
		foundVal := false
		for i, arg := range got {
			if arg == tc.flag {
				foundFlag = true
				if i+1 < len(got) && got[i+1] == tc.val {
					foundVal = true
				}
			}
		}
		if !foundFlag || !foundVal {
			t.Errorf("%s %s not forwarded to daemon: got %v", tc.flag, tc.val, got)
		}
	}
}

// TestBuildDaemonArgs_PortFlagsEqForm verifies forwarding for --flag=value syntax.
func TestBuildDaemonArgs_PortFlagsEqForm(t *testing.T) {
	osArgs := []string{"--rest-addr=127.0.0.1:8485"}
	got := buildDaemonArgs("/tmp/data", false, osArgs, "", "")

	found := false
	for i, arg := range got {
		if arg == "--rest-addr" && i+1 < len(got) && got[i+1] == "127.0.0.1:8485" {
			found = true
		}
	}
	if !found {
		t.Errorf("--rest-addr=127.0.0.1:8485 not forwarded: got %v", got)
	}
}

// TestBuildDaemonArgs_DefaultAddrNotForwarded ensures that omitting per-service
// address flags produces no --*-addr entries in daemon args (they are redundant
// since the daemon uses the same defaults).
func TestBuildDaemonArgs_DefaultAddrNotForwarded(t *testing.T) {
	got := buildDaemonArgs("/tmp/data", false, []string{}, "", "")
	for _, arg := range got {
		for _, flag := range []string{"--rest-addr", "--mbp-addr", "--grpc-addr", "--mcp-addr", "--ui-addr"} {
			if arg == flag {
				t.Errorf("unexpected %s in daemon args when not explicitly set: %v", flag, got)
			}
		}
	}
}

// TestParseExplicitFlag covers both --flag value and --flag=value forms.
func TestParseExplicitFlag(t *testing.T) {
	cases := []struct {
		args []string
		name string
		want string
	}{
		{[]string{"--rest-addr", "10.0.0.1:8485"}, "rest-addr", "10.0.0.1:8485"},
		{[]string{"--rest-addr=10.0.0.1:8485"}, "rest-addr", "10.0.0.1:8485"},
		{[]string{"-rest-addr", "10.0.0.1:8485"}, "rest-addr", "10.0.0.1:8485"},
		{[]string{"-rest-addr=10.0.0.1:8485"}, "rest-addr", "10.0.0.1:8485"},
		{[]string{"--other", "val"}, "rest-addr", ""},
		{[]string{}, "rest-addr", ""},
		{[]string{"--rest-addr"}, "rest-addr", ""}, // flag at end of args with no value
	}
	for _, tc := range cases {
		got := parseExplicitFlag(tc.name, tc.args)
		if got != tc.want {
			t.Errorf("parseExplicitFlag(%q, %v) = %q, want %q", tc.name, tc.args, got, tc.want)
		}
	}
}

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

func TestResolveEmbedInfo_EnvOpenAIWithURLOverride(t *testing.T) {
	clearEmbedEnv(t)
	t.Setenv("MUNINN_OPENAI_KEY", "sk-test-key")
	t.Setenv("MUNINN_OPENAI_URL", "openai://text-embedding-3-large?base_url=http://localhost:8080/v1")

	info := resolveEmbedInfo(plugincfg.PluginConfig{})
	if info.Provider != "openai" {
		t.Errorf("expected provider=openai, got %q", info.Provider)
	}
	if info.Model != "text-embedding-3-large" {
		t.Errorf("expected model=text-embedding-3-large, got %q", info.Model)
	}
}

func TestResolveEmbedInfo_EnvOpenAIInvalidURLSkipsOpenAI(t *testing.T) {
	clearEmbedEnv(t)
	t.Setenv("MUNINN_OPENAI_KEY", "sk-test-key")
	t.Setenv("MUNINN_OPENAI_URL", "ftp://localhost:8080")
	t.Setenv("MUNINN_LOCAL_EMBED", "0")

	info := resolveEmbedInfo(plugincfg.PluginConfig{})
	if info.Provider != "none" {
		t.Errorf("expected provider=none, got %q", info.Provider)
	}
	if info.Model != "" {
		t.Errorf("expected model=\"\", got %q", info.Model)
	}
}

func TestResolveEmbedInfo_EnvOpenAIInvalidURLFallsThroughToNextProvider(t *testing.T) {
	clearEmbedEnv(t)
	t.Setenv("MUNINN_OPENAI_KEY", "sk-test-key")
	t.Setenv("MUNINN_OPENAI_URL", "ftp://localhost:8080")
	t.Setenv("MUNINN_VOYAGE_KEY", "voy-test-key")

	info := resolveEmbedInfo(plugincfg.PluginConfig{})
	if info.Provider != "voyage" {
		t.Errorf("expected provider=voyage, got %q", info.Provider)
	}
	if info.Model != "voyage-3" {
		t.Errorf("expected model=voyage-3, got %q", info.Model)
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

func TestResolveEmbedInfo_ConfigOpenAIWithURLOverride(t *testing.T) {
	clearEmbedEnv(t)

	cfg := plugincfg.PluginConfig{
		EmbedProvider: "openai",
		EmbedURL:      "openai://text-embedding-3-large?base_url=http://localhost:8080/v1",
	}
	info := resolveEmbedInfo(cfg)
	if info.Provider != "openai" {
		t.Errorf("expected provider=openai, got %q", info.Provider)
	}
	if info.Model != "text-embedding-3-large" {
		t.Errorf("expected model=text-embedding-3-large, got %q", info.Model)
	}
}

func TestResolveEmbedInfo_ConfigOpenAIInvalidURLSkipsOpenAI(t *testing.T) {
	clearEmbedEnv(t)
	t.Setenv("MUNINN_LOCAL_EMBED", "0")

	cfg := plugincfg.PluginConfig{
		EmbedProvider: "openai",
		EmbedURL:      "ftp://localhost:8080",
	}
	info := resolveEmbedInfo(cfg)
	if info.Provider != "none" {
		t.Errorf("expected provider=none, got %q", info.Provider)
	}
	if info.Model != "" {
		t.Errorf("expected model=\"\", got %q", info.Model)
	}
}

func TestResolveEmbedInfo_InvalidEnvOpenAIURLSkipsSavedConfigOpenAI(t *testing.T) {
	clearEmbedEnv(t)
	t.Setenv("MUNINN_OPENAI_URL", "ftp://localhost:8080")
	t.Setenv("MUNINN_LOCAL_EMBED", "0")

	cfg := plugincfg.PluginConfig{
		EmbedProvider: "openai",
		EmbedURL:      "openai://text-embedding-3-large?base_url=http://localhost:8080/v1",
	}
	info := resolveEmbedInfo(cfg)
	if info.Provider != "none" {
		t.Errorf("expected provider=none, got %q", info.Provider)
	}
	if info.Model != "" {
		t.Errorf("expected model=\"\", got %q", info.Model)
	}
}

func TestResolveOpenAIEmbedProviderURL(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    string
		wantErr bool
	}{
		{
			name: "default_when_empty",
			raw:  "",
			want: defaultOpenAIEmbedProviderURL,
		},
		{
			name: "provider_url_passthrough",
			raw:  "openai://text-embedding-3-small?base_url=http://localhost:8080/v1",
			want: "openai://text-embedding-3-small?base_url=http://localhost:8080/v1",
		},
		{
			name: "base_url_converted",
			raw:  "https://gateway.example.com/openai/v1",
			want: "openai://text-embedding-3-small?base_url=https%3A%2F%2Fgateway.example.com%2Fopenai%2Fv1",
		},
		{
			name:    "invalid_url_rejected",
			raw:     "ftp://localhost:8080",
			wantErr: true,
		},
	}

	for _, tc := range tests {
		got, err := resolveOpenAIEmbedProviderURL(tc.raw)
		if tc.wantErr {
			if err == nil {
				t.Errorf("%s: expected error, got nil", tc.name)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: unexpected error: %v", tc.name, err)
			continue
		}
		if got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestInjectOpenAIBaseURL(t *testing.T) {
	tests := []struct {
		name          string
		enrichURL     string
		openAIOverride string
		want          string
	}{
		{
			name:          "plain http override injected",
			enrichURL:     "openai://qwen3",
			openAIOverride: "https://api.infomaniak.com/2/ai/103246/openai/v1",
			want:          "openai://qwen3?base_url=https%3A%2F%2Fapi.infomaniak.com%2F2%2Fai%2F103246%2Fopenai%2Fv1",
		},
		{
			name:          "openai:// override with base_url param — extracts base_url",
			enrichURL:     "openai://qwen3",
			openAIOverride: "openai://text-embedding-3-small?base_url=http://localhost:8080/v1",
			want:          "openai://qwen3?base_url=http%3A%2F%2Flocalhost%3A8080%2Fv1",
		},
		{
			name:          "openai:// override without base_url — no injection (default api.openai.com)",
			enrichURL:     "openai://qwen3",
			openAIOverride: "openai://text-embedding-3-small",
			want:          "openai://qwen3",
		},
		{
			name:          "enrich URL already has base_url — not overridden",
			enrichURL:     "openai://qwen3?base_url=http://other-host:9000/v1",
			openAIOverride: "https://api.infomaniak.com/v1",
			want:          "openai://qwen3?base_url=http://other-host:9000/v1",
		},
		{
			name:          "empty override — no-op",
			enrichURL:     "openai://qwen3",
			openAIOverride: "",
			want:          "openai://qwen3",
		},
		{
			name:          "non-openai enrich URL — not touched",
			enrichURL:     "anthropic://claude-3-haiku",
			openAIOverride: "https://api.infomaniak.com/v1",
			want:          "anthropic://claude-3-haiku",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := injectOpenAIBaseURL(tc.enrichURL, tc.openAIOverride)
			if got != tc.want {
				t.Errorf("injectOpenAIBaseURL(%q, %q)\n  got  %q\n  want %q", tc.enrichURL, tc.openAIOverride, got, tc.want)
			}
		})
	}
}

func TestResolveOpenAIEmbedProviderURL_CaseInsensitiveScheme(t *testing.T) {
	// URI schemes are case-insensitive per RFC 3986 — OPENAI:// should work like openai://
	got, err := resolveOpenAIEmbedProviderURL("OPENAI://text-embedding-3-small?base_url=http://localhost:8080/v1")
	if err != nil {
		t.Fatalf("unexpected error for uppercase scheme: %v", err)
	}
	if got == "" {
		t.Error("expected non-empty provider URL")
	}
}

func TestResolveEmbedInfo_OpenAIURLWithoutKey(t *testing.T) {
	// MUNINN_OPENAI_URL without MUNINN_OPENAI_KEY should not activate the OpenAI embedder.
	clearEmbedEnv(t)
	t.Setenv("MUNINN_OPENAI_URL", "http://localhost:8080/v1")
	t.Setenv("MUNINN_LOCAL_EMBED", "0")

	info := resolveEmbedInfo(plugincfg.PluginConfig{})
	if info.Provider == "openai" {
		t.Errorf("OPENAI_URL without OPENAI_KEY should not activate openai, got provider=%q", info.Provider)
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

func TestParseListenHost_Default(t *testing.T) {
	got := parseListenHost([]string{}, "")
	if got != "127.0.0.1" {
		t.Errorf("expected 127.0.0.1, got %q", got)
	}
}

func TestParseListenHost_EnvOverride(t *testing.T) {
	got := parseListenHost([]string{}, "10.0.0.1")
	if got != "10.0.0.1" {
		t.Errorf("expected 10.0.0.1, got %q", got)
	}
}

func TestParseListenHost_ArgOverridesEnv(t *testing.T) {
	got := parseListenHost([]string{"--listen-host", "0.0.0.0"}, "10.0.0.1")
	if got != "0.0.0.0" {
		t.Errorf("expected 0.0.0.0, got %q", got)
	}
}

func TestParseListenHost_EqualsSyntax(t *testing.T) {
	got := parseListenHost([]string{"--listen-host=192.168.1.5"}, "")
	if got != "192.168.1.5" {
		t.Errorf("expected 192.168.1.5, got %q", got)
	}
}

func TestParseListenHost_SingleDashEqualsSyntax(t *testing.T) {
	got := parseListenHost([]string{"-listen-host=172.16.0.1"}, "")
	if got != "172.16.0.1" {
		t.Errorf("expected 172.16.0.1, got %q", got)
	}
}

func TestParseListenHost_SingleDashSpaceSyntax(t *testing.T) {
	got := parseListenHost([]string{"-listen-host", "10.10.10.10"}, "")
	if got != "10.10.10.10" {
		t.Errorf("expected 10.10.10.10, got %q", got)
	}
}

// TestListenHostFlag_OverridesAddrDefaults confirms that when --listen-host is
// set, the mcp-addr default is built from that host.
func TestListenHostFlag_OverridesAddrDefaults(t *testing.T) {
	host := parseListenHost([]string{"--listen-host", "10.0.0.1"}, "")
	if host != "10.0.0.1" {
		t.Fatalf("expected 10.0.0.1, got %s", host)
	}
	gotAddr := host + ":" + defaultMCPPort
	if gotAddr != "10.0.0.1:8750" {
		t.Fatalf("expected 10.0.0.1:8750, got %s", gotAddr)
	}
}

// TestListenHostFlag_ExplicitAddrOverrides confirms that an explicit --mcp-addr
// takes precedence over the --listen-host default. This is handled naturally by
// flag.Parse() since the flag default is set to listenHost+port and an explicit
// --mcp-addr value overwrites it. The test verifies the pre-scan does not
// interfere with other args.
func TestListenHostFlag_ExplicitAddrOverrides(t *testing.T) {
	// Even if listen-host is 0.0.0.0, parseListenHost only affects the
	// default value; flag.Parse() will use the explicitly-supplied --mcp-addr.
	// Here we just verify parseListenHost doesn't accidentally consume the
	// mcp-addr value.
	host := parseListenHost([]string{"--listen-host", "0.0.0.0", "--mcp-addr", "127.0.0.1:" + defaultMCPPort}, "")
	if host != "0.0.0.0" {
		t.Errorf("expected listen-host=0.0.0.0, got %q", host)
	}
	// The explicit mcp-addr would be handled by flag.Parse(); we can only test
	// that the listen-host pre-scan correctly picks up 0.0.0.0 here.
}

// clearEmbedEnv unsets all embed-related env vars for a clean test.
func clearEmbedEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"MUNINN_OLLAMA_URL", "MUNINN_OPENAI_KEY", "MUNINN_OPENAI_URL", "MUNINN_VOYAGE_KEY",
		"MUNINN_COHERE_KEY", "MUNINN_GOOGLE_KEY", "MUNINN_JINA_KEY",
		"MUNINN_MISTRAL_KEY", "MUNINN_LOCAL_EMBED",
	} {
		t.Setenv(key, "")
		os.Unsetenv(key)
	}
	t.Setenv("MUNINN_LOCAL_EMBED", "0")
}
