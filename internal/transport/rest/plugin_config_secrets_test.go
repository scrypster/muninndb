package rest

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/scrypster/muninndb/internal/config"
)

// The admin plugin-config endpoint must never return stored LLM provider API
// keys in cleartext, and saving a config whose key fields still carry the
// masked value (i.e. the admin did not retype the key) must preserve the real
// stored key rather than overwrite it with the mask.

func TestMaskSecret(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", ""},
		{"abcd", "••••••••"}, // too short to reveal any suffix
		{"sk-1234567890", "••••••••7890"},
		{"voy-abcdEFGH", "••••••••EFGH"},
	}
	for _, c := range cases {
		if got := maskSecret(c.in); got != c.want {
			t.Errorf("maskSecret(%q) = %q, want %q", c.in, got, c.want)
		}
		if c.in != "" && strings.Contains(maskSecret(c.in), c.in) {
			t.Errorf("maskSecret(%q) leaks the full secret", c.in)
		}
	}
}

func TestHandleGetPluginConfig_MasksAPIKeys(t *testing.T) {
	dir := t.TempDir()
	const embedKey = "voy-secret-embed-key-1234"
	const enrichKey = "sk-secret-enrich-key-abcd"
	if err := config.SavePluginConfig(dir, config.PluginConfig{
		EmbedProvider: "voyage", EmbedAPIKey: embedKey,
		EnrichProvider: "openai", EnrichAPIKey: enrichKey,
	}); err != nil {
		t.Fatalf("save config: %v", err)
	}

	srv := NewServer("localhost:0", &MockEngine{}, newTestAuthStore(t), nil, nil, EmbedInfo{}, EnrichInfo{}, nil, dir, nil)
	req := httptest.NewRequest("GET", "/api/admin/plugin-config", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if strings.Contains(body, embedKey) || strings.Contains(body, enrichKey) {
		t.Fatalf("response leaks a cleartext API key:\n%s", body)
	}

	var resp config.PluginConfig
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.EmbedAPIKey != maskSecret(embedKey) {
		t.Errorf("embed key = %q, want masked %q", resp.EmbedAPIKey, maskSecret(embedKey))
	}
	if resp.EnrichAPIKey != maskSecret(enrichKey) {
		t.Errorf("enrich key = %q, want masked %q", resp.EnrichAPIKey, maskSecret(enrichKey))
	}
}

func TestHandlePutPluginConfig_PreservesMaskedKeys(t *testing.T) {
	dir := t.TempDir()
	const embedKey = "voy-secret-embed-key-1234"
	const enrichKey = "sk-secret-enrich-key-abcd"
	if err := config.SavePluginConfig(dir, config.PluginConfig{
		EmbedProvider: "voyage", EmbedAPIKey: embedKey,
		EnrichProvider: "openai", EnrichAPIKey: enrichKey,
	}); err != nil {
		t.Fatalf("save config: %v", err)
	}

	srv := NewServer("localhost:0", &MockEngine{}, newTestAuthStore(t), nil, nil, EmbedInfo{}, EnrichInfo{}, nil, dir, nil)

	// Simulate the UI saving the form back unchanged: the key fields carry the
	// masked values that GET returned, and the admin changed only the provider.
	body, _ := json.Marshal(config.PluginConfig{
		EmbedProvider: "voyage", EmbedAPIKey: maskSecret(embedKey),
		EnrichProvider: "anthropic", EnrichAPIKey: maskSecret(enrichKey),
	})
	req := httptest.NewRequest("PUT", "/api/admin/plugin-config", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// The stored config must retain the real keys, not the masks.
	stored, err := config.LoadPluginConfig(dir)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if stored.EmbedAPIKey != embedKey {
		t.Errorf("embed key was clobbered: got %q, want %q", stored.EmbedAPIKey, embedKey)
	}
	if stored.EnrichAPIKey != enrichKey {
		t.Errorf("enrich key was clobbered: got %q, want %q", stored.EnrichAPIKey, enrichKey)
	}
	if stored.EnrichProvider != "anthropic" {
		t.Errorf("provider not updated: got %q", stored.EnrichProvider)
	}
	// The PUT response must also be masked.
	if strings.Contains(w.Body.String(), embedKey) || strings.Contains(w.Body.String(), enrichKey) {
		t.Errorf("PUT response leaks a cleartext key:\n%s", w.Body.String())
	}
}

func TestHandlePutPluginConfig_UpdatesNewKey(t *testing.T) {
	dir := t.TempDir()
	if err := config.SavePluginConfig(dir, config.PluginConfig{
		EmbedProvider: "voyage", EmbedAPIKey: "old-embed-key-9999",
	}); err != nil {
		t.Fatalf("save config: %v", err)
	}

	srv := NewServer("localhost:0", &MockEngine{}, newTestAuthStore(t), nil, nil, EmbedInfo{}, EnrichInfo{}, nil, dir, nil)

	const newKey = "voy-brand-new-key-7777"
	body, _ := json.Marshal(config.PluginConfig{EmbedProvider: "voyage", EmbedAPIKey: newKey})
	req := httptest.NewRequest("PUT", "/api/admin/plugin-config", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	stored, err := config.LoadPluginConfig(dir)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if stored.EmbedAPIKey != newKey {
		t.Errorf("new key not saved: got %q, want %q", stored.EmbedAPIKey, newKey)
	}
}

func TestHandlePutPluginConfig_ClearsEmptyKey(t *testing.T) {
	dir := t.TempDir()
	if err := config.SavePluginConfig(dir, config.PluginConfig{
		EmbedProvider: "voyage", EmbedAPIKey: "old-embed-key-9999",
	}); err != nil {
		t.Fatalf("save config: %v", err)
	}

	srv := NewServer("localhost:0", &MockEngine{}, newTestAuthStore(t), nil, nil, EmbedInfo{}, EnrichInfo{}, nil, dir, nil)

	// An explicitly empty key clears it (distinct from sending the mask back).
	body, _ := json.Marshal(config.PluginConfig{EmbedProvider: "local", EmbedAPIKey: ""})
	req := httptest.NewRequest("PUT", "/api/admin/plugin-config", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	stored, err := config.LoadPluginConfig(dir)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if stored.EmbedAPIKey != "" {
		t.Errorf("key should be cleared, got %q", stored.EmbedAPIKey)
	}
}
