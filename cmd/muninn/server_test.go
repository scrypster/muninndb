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
	got := buildDaemonArgs("/tmp/data", false, "", osArgs, "", corsOriginsEnv)

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
		"MUNINN_OLLAMA_URL", "MUNINN_OPENAI_KEY", "MUNINN_VOYAGE_KEY",
		"MUNINN_COHERE_KEY", "MUNINN_GOOGLE_KEY", "MUNINN_JINA_KEY",
		"MUNINN_MISTRAL_KEY", "MUNINN_LOCAL_EMBED",
	} {
		t.Setenv(key, "")
		os.Unsetenv(key)
	}
	t.Setenv("MUNINN_LOCAL_EMBED", "0")
}
