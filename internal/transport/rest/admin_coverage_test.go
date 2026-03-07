package rest

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/scrypster/muninndb/internal/config"
)

// --- handleEmbedStatus tests ---

func TestHandleEmbedStatus(t *testing.T) {
	store := newTestAuthStore(t)
	srv := NewServer("localhost:0", &MockEngine{}, store, nil, nil, EmbedInfo{
		Provider: "openai",
		Model:    "text-embedding-3-small",
	}, EnrichInfo{}, nil, "", nil)

	req := httptest.NewRequest("GET", "/api/admin/embed/status", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp EmbedStatusResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Provider != "openai" {
		t.Errorf("expected provider=openai, got %q", resp.Provider)
	}
	if resp.Model != "text-embedding-3-small" {
		t.Errorf("expected model=text-embedding-3-small, got %q", resp.Model)
	}
	if !resp.Enabled {
		t.Error("expected enabled=true for openai provider")
	}
	if resp.TotalCount != 100 {
		t.Errorf("expected total_count=100, got %d", resp.TotalCount)
	}
}

func TestHandleEmbedStatus_NoneProvider(t *testing.T) {
	store := newTestAuthStore(t)
	srv := NewServer("localhost:0", &MockEngine{}, store, nil, nil, EmbedInfo{
		Provider: "none",
	}, EnrichInfo{}, nil, "", nil)

	req := httptest.NewRequest("GET", "/api/admin/embed/status", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp EmbedStatusResponse
	json.NewDecoder(w.Body).Decode(&resp)
	if resp.Enabled {
		t.Error("expected enabled=false for 'none' provider")
	}
}

func TestHandleEmbedStatus_EmptyProvider(t *testing.T) {
	store := newTestAuthStore(t)
	srv := NewServer("localhost:0", &MockEngine{}, store, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	req := httptest.NewRequest("GET", "/api/admin/embed/status", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var resp EmbedStatusResponse
	json.NewDecoder(w.Body).Decode(&resp)
	if resp.Enabled {
		t.Error("expected enabled=false for empty provider")
	}
}

// --- handleGetPluginConfig / handlePutPluginConfig tests ---

func TestHandleGetPluginConfig_NoDataDir(t *testing.T) {
	store := newTestAuthStore(t)
	srv := NewServer("localhost:0", &MockEngine{}, store, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	req := httptest.NewRequest("GET", "/api/admin/plugin-config", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleGetPluginConfig_WithDataDir(t *testing.T) {
	dir := t.TempDir()
	cfg := config.PluginConfig{EmbedProvider: "ollama", EmbedURL: "http://localhost:11434"}
	if err := config.SavePluginConfig(dir, cfg); err != nil {
		t.Fatalf("save config: %v", err)
	}

	store := newTestAuthStore(t)
	srv := NewServer("localhost:0", &MockEngine{}, store, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, dir, nil)

	req := httptest.NewRequest("GET", "/api/admin/plugin-config", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp config.PluginConfig
	json.NewDecoder(w.Body).Decode(&resp)
	if resp.EmbedProvider != "ollama" {
		t.Errorf("expected embed_provider=ollama, got %q", resp.EmbedProvider)
	}
}

func TestHandlePutPluginConfig_NoDataDir(t *testing.T) {
	store := newTestAuthStore(t)
	srv := NewServer("localhost:0", &MockEngine{}, store, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	body, _ := json.Marshal(config.PluginConfig{EmbedProvider: "openai"})
	req := httptest.NewRequest("PUT", "/api/admin/plugin-config", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 without data dir, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandlePutPluginConfig_Success(t *testing.T) {
	dir := t.TempDir()
	store := newTestAuthStore(t)
	srv := NewServer("localhost:0", &MockEngine{}, store, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, dir, nil)

	cfg := config.PluginConfig{
		EmbedProvider: "voyage",
		EmbedAPIKey:   "voy-test-key",
	}
	body, _ := json.Marshal(cfg)
	req := httptest.NewRequest("PUT", "/api/admin/plugin-config", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	saved, err := config.LoadPluginConfig(dir)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if saved.EmbedProvider != "voyage" {
		t.Errorf("expected saved provider=voyage, got %q", saved.EmbedProvider)
	}
}

func TestHandlePutPluginConfig_InvalidJSON(t *testing.T) {
	dir := t.TempDir()
	store := newTestAuthStore(t)
	srv := NewServer("localhost:0", &MockEngine{}, store, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, dir, nil)

	req := httptest.NewRequest("PUT", "/api/admin/plugin-config", strings.NewReader("not json"))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

// --- handleReindexFTSVault tests ---

func TestHandleReindexFTSVault_Success(t *testing.T) {
	store := newTestAuthStore(t)
	srv := newTestServer(t, store)

	req := httptest.NewRequest("POST", "/api/admin/vaults/default/reindex-fts", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]any
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["vault"] != "default" {
		t.Errorf("expected vault=default, got %v", resp["vault"])
	}
}

func TestHandleReindexFTSVault_EmptyName(t *testing.T) {
	store := newTestAuthStore(t)
	srv := newTestServer(t, store)

	req := httptest.NewRequest("POST", "/api/admin/vaults//reindex-fts", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code == http.StatusOK {
		t.Fatal("expected error for empty vault name")
	}
}

func TestHandleReindexFTSVault_InvalidVaultName(t *testing.T) {
	store := newTestAuthStore(t)
	srv := newTestServer(t, store)

	req := httptest.NewRequest("POST", "/api/admin/vaults/BAD-VAULT!/reindex-fts", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for uppercase vault name, got %d", w.Code)
	}
}

// --- handleExportVault tests ---

func TestHandleExportVault_ContentHeaders(t *testing.T) {
	store := newTestAuthStore(t)
	srv := newTestServer(t, store)

	req := httptest.NewRequest("GET", "/api/admin/vaults/default/export", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/gzip" {
		t.Errorf("expected Content-Type=application/gzip, got %q", ct)
	}
	if cd := w.Header().Get("Content-Disposition"); !strings.Contains(cd, "default.muninn") {
		t.Errorf("expected Content-Disposition with default.muninn, got %q", cd)
	}
}

func TestHandleExportVault_InvalidVaultName(t *testing.T) {
	store := newTestAuthStore(t)
	srv := newTestServer(t, store)

	req := httptest.NewRequest("GET", "/api/admin/vaults/INVALID!/export", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

// --- handleImportVault tests ---

func TestHandleImportVault_ResetMetadata(t *testing.T) {
	store := newTestAuthStore(t)
	srv := newTestServer(t, store)

	req := httptest.NewRequest("POST", "/api/admin/vaults/import?vault=test-vault&reset_metadata=true", strings.NewReader("mock-data"))
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]string
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["job_id"] == "" {
		t.Error("expected non-empty job_id")
	}
}

func TestHandleImportVault_MissingVaultParam(t *testing.T) {
	store := newTestAuthStore(t)
	srv := newTestServer(t, store)

	req := httptest.NewRequest("POST", "/api/admin/vaults/import", strings.NewReader("data"))
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for missing vault param, got %d", w.Code)
	}
}

func TestHandleImportVault_InvalidVaultName(t *testing.T) {
	store := newTestAuthStore(t)
	srv := newTestServer(t, store)

	req := httptest.NewRequest("POST", "/api/admin/vaults/import?vault=BAD!", strings.NewReader("data"))
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid vault name, got %d", w.Code)
	}
}

// --- handleDeleteVault error paths ---

func TestHandleDeleteVault_InvalidName(t *testing.T) {
	store := newTestAuthStore(t)
	srv := newTestServer(t, store)

	req := httptest.NewRequest("DELETE", "/api/admin/vaults/BAD!", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestHandleDeleteVault_DefaultWithoutHeaderCoverage(t *testing.T) {
	store := newTestAuthStore(t)
	srv := newTestServer(t, store)

	req := httptest.NewRequest("DELETE", "/api/admin/vaults/default", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409 for default vault without header, got %d", w.Code)
	}
}

// --- handleClearVault error paths ---

func TestHandleClearVault_InvalidName(t *testing.T) {
	store := newTestAuthStore(t)
	srv := newTestServer(t, store)

	req := httptest.NewRequest("POST", "/api/admin/vaults/INVALID!/clear", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestHandleClearVault_DefaultWithoutHeader(t *testing.T) {
	store := newTestAuthStore(t)
	srv := newTestServer(t, store)

	req := httptest.NewRequest("POST", "/api/admin/vaults/default/clear", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d", w.Code)
	}
}

func TestHandleClearVault_NonDefaultSuccess(t *testing.T) {
	store := newTestAuthStore(t)
	srv := newTestServer(t, store)

	req := httptest.NewRequest("POST", "/api/admin/vaults/test-vault/clear", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", w.Code, w.Body.String())
	}
}

// --- handleCloneVault error paths ---

func TestHandleCloneVault_InvalidNewName(t *testing.T) {
	store := newTestAuthStore(t)
	srv := newTestServer(t, store)

	body, _ := json.Marshal(map[string]string{"new_name": "BAD!"})
	req := httptest.NewRequest("POST", "/api/admin/vaults/source/clone", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid new_name, got %d", w.Code)
	}
}

func TestHandleCloneVault_InvalidNewNameChars(t *testing.T) {
	store := newTestAuthStore(t)
	srv := newTestServer(t, store)

	body, _ := json.Marshal(map[string]string{"new_name": "source"})
	req := httptest.NewRequest("POST", "/api/admin/vaults/source/clone", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409 for self-clone, got %d", w.Code)
	}
}

// --- handleMergeVault error paths ---

func TestHandleMergeVault_InvalidTarget(t *testing.T) {
	store := newTestAuthStore(t)
	srv := newTestServer(t, store)

	body, _ := json.Marshal(map[string]string{"target": "BAD!"})
	req := httptest.NewRequest("POST", "/api/admin/vaults/source/merge-into", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid target, got %d", w.Code)
	}
}

func TestHandleMergeVault_SameSourceAndTarget(t *testing.T) {
	store := newTestAuthStore(t)
	srv := newTestServer(t, store)

	body, _ := json.Marshal(map[string]string{"target": "same"})
	req := httptest.NewRequest("POST", "/api/admin/vaults/same/merge-into", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d", w.Code)
	}
}

// --- isValidVaultName edge cases ---

func TestIsValidVaultName(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{"", false},
		{"default", true},
		{"my-vault", true},
		{"my_vault", true},
		{"abc123", true},
		{"UPPERCASE", false},
		{"has space", false},
		{"has.dot", false},
		{"a", true},
		{strings.Repeat("a", 64), true},
		{strings.Repeat("a", 65), false},
	}
	for _, tc := range cases {
		got := isValidVaultName(tc.name)
		if got != tc.want {
			t.Errorf("isValidVaultName(%q) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// --- handleCreateAPIKey error paths ---

func TestCreateAPIKey_InvalidVaultName(t *testing.T) {
	store := newTestAuthStore(t)
	srv := newTestServer(t, store)

	body, _ := json.Marshal(map[string]string{"vault": "BAD!"})
	req := httptest.NewRequest("POST", "/api/admin/keys", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestCreateAPIKey_LabelTooLong(t *testing.T) {
	store := newTestAuthStore(t)
	srv := newTestServer(t, store)

	body, _ := json.Marshal(map[string]string{
		"vault": "default",
		"label": strings.Repeat("x", 257),
	})
	req := httptest.NewRequest("POST", "/api/admin/keys", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestCreateAPIKey_InvalidJSON(t *testing.T) {
	store := newTestAuthStore(t)
	srv := newTestServer(t, store)

	req := httptest.NewRequest("POST", "/api/admin/keys", strings.NewReader("not json"))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

// --- handleListAPIKeys error paths ---

func TestListAPIKeys_InvalidVaultName(t *testing.T) {
	store := newTestAuthStore(t)
	srv := newTestServer(t, store)

	req := httptest.NewRequest("GET", "/api/admin/keys?vault=BAD!", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

// --- handleRevokeAPIKey error paths ---

func TestRevokeAPIKey_InvalidVaultName(t *testing.T) {
	store := newTestAuthStore(t)
	srv := newTestServer(t, store)

	req := httptest.NewRequest("DELETE", "/api/admin/keys/some-id?vault=BAD!", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestRevokeAPIKey_NotFound(t *testing.T) {
	store := newTestAuthStore(t)
	srv := newTestServer(t, store)

	req := httptest.NewRequest("DELETE", "/api/admin/keys/nonexistent-id?vault=default", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

// --- handleSetVaultConfig error paths ---

func TestSetVaultConfig_InvalidJSON(t *testing.T) {
	store := newTestAuthStore(t)
	srv := newTestServer(t, store)

	req := httptest.NewRequest("PUT", "/api/admin/vaults/config", strings.NewReader("not json"))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestSetVaultConfig_InvalidVaultName(t *testing.T) {
	store := newTestAuthStore(t)
	srv := newTestServer(t, store)

	body, _ := json.Marshal(map[string]any{"name": "BAD!"})
	req := httptest.NewRequest("PUT", "/api/admin/vaults/config", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

// --- handleChangeAdminPassword error paths ---

func TestChangeAdminPassword_InvalidJSON(t *testing.T) {
	store := newTestAuthStore(t)
	srv := newTestServer(t, store)

	req := httptest.NewRequest("PUT", "/api/admin/password", strings.NewReader("not json"))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

// --- handleMCPInfo edge cases ---

func TestMCPInfo_HostSpecific(t *testing.T) {
	store := newTestAuthStore(t)
	srv := NewServer("localhost:0", &MockEngine{}, store, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil, MCPInfo{
		Addr:     "192.168.1.5:8750",
		HasToken: true,
	})

	req := httptest.NewRequest("GET", "/api/admin/mcp-info", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	var resp MCPInfoResponse
	json.NewDecoder(w.Body).Decode(&resp)
	if resp.URL != "http://192.168.1.5:8750/mcp" {
		t.Errorf("expected host-specific URL, got %q", resp.URL)
	}
}

func TestMCPInfo_EmptyAddr(t *testing.T) {
	store := newTestAuthStore(t)
	srv := NewServer("localhost:0", &MockEngine{}, store, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil, MCPInfo{
		Addr: "",
	})

	req := httptest.NewRequest("GET", "/api/admin/mcp-info", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	var resp MCPInfoResponse
	json.NewDecoder(w.Body).Decode(&resp)
	if !strings.Contains(resp.URL, "127.0.0.1") {
		t.Errorf("expected 127.0.0.1 in URL for empty addr, got %q", resp.URL)
	}
}

func TestMCPInfo_IPv6Wildcard(t *testing.T) {
	store := newTestAuthStore(t)
	srv := NewServer("localhost:0", &MockEngine{}, store, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil, MCPInfo{
		Addr: "[::]:8750",
	})

	req := httptest.NewRequest("GET", "/api/admin/mcp-info", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	var resp MCPInfoResponse
	json.NewDecoder(w.Body).Decode(&resp)
	if resp.URL != "http://127.0.0.1:8750/mcp" {
		t.Errorf("expected localhost for :: wildcard, got %q", resp.URL)
	}
}

// --- handleHello error paths ---

func TestHandleHello_InvalidJSON(t *testing.T) {
	srv := NewServer("localhost:0", &MockEngine{}, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	req := httptest.NewRequest("POST", "/api/hello", strings.NewReader("not json"))
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

// --- handleLink error paths ---

func TestHandleLink_InvalidJSON(t *testing.T) {
	srv := NewServer("localhost:0", &MockEngine{}, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	req := httptest.NewRequest("POST", "/api/link", strings.NewReader("not json"))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

// --- handleBatchCreate error paths ---

func TestBatchCreate_InvalidJSON(t *testing.T) {
	srv := NewServer("localhost:0", &MockEngine{}, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	req := httptest.NewRequest("POST", "/api/engrams/batch", strings.NewReader("not json"))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

// --- Server utility methods ---

func TestEnvIntDefault(t *testing.T) {
	key := "MUNINN_TEST_ENV_INT_DEFAULT_XYZZY"
	defer os.Unsetenv(key)

	// Unset: returns default
	os.Unsetenv(key)
	if got := envIntDefault(key, 42); got != 42 {
		t.Errorf("unset: expected 42, got %d", got)
	}

	// Valid value
	os.Setenv(key, "500")
	if got := envIntDefault(key, 42); got != 500 {
		t.Errorf("valid: expected 500, got %d", got)
	}

	// Invalid value (non-numeric)
	os.Setenv(key, "abc")
	if got := envIntDefault(key, 42); got != 42 {
		t.Errorf("non-numeric: expected default 42, got %d", got)
	}

	// Out of range (0)
	os.Setenv(key, "0")
	if got := envIntDefault(key, 42); got != 42 {
		t.Errorf("zero: expected default 42, got %d", got)
	}

	// Out of range (too large)
	os.Setenv(key, "999999")
	if got := envIntDefault(key, 42); got != 42 {
		t.Errorf("too large: expected default 42, got %d", got)
	}
}

func TestClientIP(t *testing.T) {
	cases := []struct {
		remoteAddr string
		want       string
	}{
		{"192.168.1.1:1234", "192.168.1.1"},
		{"[::1]:8080", "::1"},
		{"127.0.0.1:0", "127.0.0.1"},
		{"no-port", "no-port"},
	}
	for _, tc := range cases {
		r := &http.Request{RemoteAddr: tc.remoteAddr}
		got := clientIP(r)
		if got != tc.want {
			t.Errorf("clientIP(%q) = %q, want %q", tc.remoteAddr, got, tc.want)
		}
	}
}

// --- Server health and ready ---

func TestHealthEndpoint_Version(t *testing.T) {
	srv := NewServer("localhost:0", &MockEngine{}, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)
	srv.SetVersion("v1.2.3")

	req := httptest.NewRequest("GET", "/api/health", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	var resp HealthResponse
	json.NewDecoder(w.Body).Decode(&resp)
	if resp.Version != "v1.2.3" {
		t.Errorf("expected version=v1.2.3, got %q", resp.Version)
	}
}

func TestReadyEndpoint_NotReady(t *testing.T) {
	srv := NewServer("localhost:0", &MockEngine{}, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)
	srv.subsystemsReady.Store(false)

	req := httptest.NewRequest("GET", "/api/ready", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when not ready, got %d", w.Code)
	}
}

// --- PersistClusterDisabled with dataDir ---

func TestPersistClusterDisabled_WithDataDir(t *testing.T) {
	dir := t.TempDir()
	srv := &Server{dataDir: dir}

	if err := srv.persistClusterDisabled(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	cfg, err := config.LoadClusterConfig(dir)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if cfg.Enabled {
		t.Error("expected enabled=false after persistClusterDisabled")
	}
}

// --- countingWriter ---

func TestCountingWriter(t *testing.T) {
	w := httptest.NewRecorder()
	cw := &countingWriter{ResponseWriter: w}

	n, err := cw.Write([]byte("hello"))
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if n != 5 {
		t.Errorf("expected 5 bytes written, got %d", n)
	}
	if cw.n != 5 {
		t.Errorf("expected counter=5, got %d", cw.n)
	}

	cw.Write([]byte(" world"))
	if cw.n != 11 {
		t.Errorf("expected counter=11, got %d", cw.n)
	}
}

// --- writeError (middleware helper) ---

func TestWriteError(t *testing.T) {
	w := httptest.NewRecorder()
	writeError(w, http.StatusTooManyRequests, "rate_limit_exceeded", "too fast")

	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("expected Content-Type=application/json, got %q", ct)
	}
	var resp APIError
	json.NewDecoder(w.Body).Decode(&resp)
	if resp.Code != "rate_limit_exceeded" {
		t.Errorf("expected code=rate_limit_exceeded, got %q", resp.Code)
	}
}

// --- ApplyAndPersistSettings ---

func TestApplyAndPersistSettings_NoDataDir(t *testing.T) {
	srv := &Server{}
	if err := srv.applyAndPersistSettings(clusterSettingsRequest{}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestApplyAndPersistSettings_WithDataDir(t *testing.T) {
	dir := t.TempDir()
	cfg := config.ClusterConfig{
		Enabled:     true,
		NodeID:      "test",
		HeartbeatMS: 1000,
	}
	if err := config.SaveClusterConfig(dir, cfg); err != nil {
		t.Fatalf("save: %v", err)
	}
	srv := &Server{dataDir: dir}

	hb := 500
	err := srv.applyAndPersistSettings(clusterSettingsRequest{HeartbeatMS: &hb})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}

	saved, _ := config.LoadClusterConfig(dir)
	if saved.HeartbeatMS != 500 {
		t.Errorf("expected heartbeat=500, got %d", saved.HeartbeatMS)
	}
}

// --- enableClusterRuntime ---

func TestEnableClusterRuntime_NoFactory(t *testing.T) {
	dir := t.TempDir()
	srv := &Server{dataDir: dir}

	err := srv.enableClusterRuntime(nil, config.ClusterConfig{
		Enabled:  true,
		NodeID:   "test",
		BindAddr: ":0",
		Role:     "primary",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	saved, _ := config.LoadClusterConfig(dir)
	if !saved.Enabled {
		t.Error("expected config to be persisted as enabled")
	}
}

// --- statusRecorder ---

func TestStatusRecorder(t *testing.T) {
	w := httptest.NewRecorder()
	rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
	rec.WriteHeader(http.StatusNotFound)
	if rec.status != http.StatusNotFound {
		t.Errorf("expected status 404, got %d", rec.status)
	}
}

// --- GetPluginConfig with bad data ---

func TestHandleGetPluginConfig_CorruptFile(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "plugin_config.json"), []byte("not json"), 0600)

	store := newTestAuthStore(t)
	srv := NewServer("localhost:0", &MockEngine{}, store, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, dir, nil)

	req := httptest.NewRequest("GET", "/api/admin/plugin-config", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 for corrupt config, got %d", w.Code)
	}
}
