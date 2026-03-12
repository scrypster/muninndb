package rest

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cockroachdb/pebble"
	"github.com/cockroachdb/pebble/vfs"
	"github.com/scrypster/muninndb/internal/auth"
	"github.com/scrypster/muninndb/internal/config"
	"github.com/scrypster/muninndb/internal/plugin"
	"github.com/scrypster/muninndb/internal/replication"
)

func newTestAuthStore(t *testing.T) *auth.Store {
	t.Helper()
	db, err := pebble.Open("", &pebble.Options{FS: vfs.NewMem()})
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return auth.NewStore(db)
}

func newTestServer(t *testing.T, store *auth.Store) *Server {
	t.Helper()
	return NewServer("localhost:0", &MockEngine{}, store, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)
}

// TestCreateAPIKey tests POST /api/admin/keys
func TestCreateAPIKey(t *testing.T) {
	store := newTestAuthStore(t)
	srv := newTestServer(t, store)

	body, _ := json.Marshal(map[string]string{
		"vault": "default",
		"label": "test-agent",
		"mode":  "full",
	})
	req := httptest.NewRequest("POST", "/api/admin/keys", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	token, ok := resp["token"].(string)
	if !ok || token == "" {
		t.Fatal("expected non-empty token in response")
	}
	if len(token) < 10 {
		t.Fatalf("token looks too short: %q", token)
	}
	if _, ok := resp["key"]; !ok {
		t.Fatal("expected key metadata in response")
	}
}

// TestCreateAPIKey_WriteModeAccepted tests that "write" mode is accepted.
func TestCreateAPIKey_WriteModeAccepted(t *testing.T) {
	store := newTestAuthStore(t)
	srv := newTestServer(t, store)

	body, _ := json.Marshal(map[string]string{"vault": "default", "label": "bot", "mode": "write"})
	req := httptest.NewRequest("POST", "/api/admin/keys", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Errorf("expected 201 for write mode, got %d: %s", w.Code, w.Body.String())
	}
}

// TestCreateAPIKeyInvalidMode tests that an invalid mode is rejected
func TestCreateAPIKeyInvalidMode(t *testing.T) {
	store := newTestAuthStore(t)
	srv := newTestServer(t, store)

	body, _ := json.Marshal(map[string]string{
		"vault": "default",
		"label": "bad-key",
		"mode":  "superuser",
	})
	req := httptest.NewRequest("POST", "/api/admin/keys", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid mode, got %d", w.Code)
	}
}

// TestCreateAPIKey_DefaultsToFullMode tests that omitting mode defaults to "full".
func TestCreateAPIKey_DefaultsToFullMode(t *testing.T) {
	store := newTestAuthStore(t)
	srv := newTestServer(t, store)

	body, _ := json.Marshal(map[string]string{"vault": "default", "label": "default-mode-key"})
	req := httptest.NewRequest("POST", "/api/admin/keys", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201 when mode is omitted, got %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Key struct {
			Mode string `json:"mode"`
		} `json:"key"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Key.Mode != "full" {
		t.Errorf("expected default mode 'full', got %q", resp.Key.Mode)
	}
}

// TestListAPIKeys tests GET /api/admin/keys?vault=default
func TestListAPIKeys(t *testing.T) {
	store := newTestAuthStore(t)
	srv := newTestServer(t, store)

	// Create a key first
	body, _ := json.Marshal(map[string]string{"vault": "default", "label": "x", "mode": "observe"})
	createReq := httptest.NewRequest("POST", "/api/admin/keys", bytes.NewReader(body))
	createReq.Header.Set("Content-Type", "application/json")
	createW := httptest.NewRecorder()
	srv.mux.ServeHTTP(createW, createReq)
	if createW.Code != http.StatusCreated {
		t.Fatalf("setup: create key failed with %d", createW.Code)
	}

	req := httptest.NewRequest("GET", "/api/admin/keys?vault=default", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	keys, ok := resp["keys"].([]interface{})
	if !ok {
		t.Fatal("expected 'keys' array in response")
	}
	if len(keys) != 1 {
		t.Fatalf("expected 1 key, got %d", len(keys))
	}
}

// TestRevokeAPIKey tests DELETE /api/admin/keys/{id}
func TestRevokeAPIKey(t *testing.T) {
	store := newTestAuthStore(t)
	srv := newTestServer(t, store)

	// Create a key
	body, _ := json.Marshal(map[string]string{"vault": "default", "label": "to-revoke", "mode": "full"})
	createReq := httptest.NewRequest("POST", "/api/admin/keys", bytes.NewReader(body))
	createReq.Header.Set("Content-Type", "application/json")
	createW := httptest.NewRecorder()
	srv.mux.ServeHTTP(createW, createReq)
	if createW.Code != http.StatusCreated {
		t.Fatalf("setup: create key failed: %s", createW.Body.String())
	}

	var createResp map[string]interface{}
	json.NewDecoder(createW.Body).Decode(&createResp)
	keyMeta := createResp["key"].(map[string]interface{})
	keyID := keyMeta["id"].(string)

	req := httptest.NewRequest("DELETE", "/api/admin/keys/"+keyID+"?vault=default", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]interface{}
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["revoked"] != keyID {
		t.Fatalf("expected revoked=%q, got %v", keyID, resp["revoked"])
	}
}

// TestSetVaultConfig tests PUT /api/admin/vaults/config
func TestSetVaultConfig(t *testing.T) {
	store := newTestAuthStore(t)
	srv := newTestServer(t, store)

	body, _ := json.Marshal(map[string]interface{}{
		"name":   "myvault",
		"public": false,
	})
	req := httptest.NewRequest("PUT", "/api/admin/vaults/config", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]interface{}
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["name"] != "myvault" {
		t.Fatalf("expected name=myvault, got %v", resp["name"])
	}
	if resp["public"] != false {
		t.Fatalf("expected public=false, got %v", resp["public"])
	}
}

// TestChangeAdminPassword tests PUT /api/admin/password
func TestChangeAdminPasswordEndpoint(t *testing.T) {
	store := newTestAuthStore(t)
	srv := newTestServer(t, store)

	// Seed a root admin
	if err := store.CreateAdmin("root", "oldpass"); err != nil {
		t.Fatalf("setup: %v", err)
	}

	body, _ := json.Marshal(map[string]string{
		"username":     "root",
		"new_password": "newpass123",
	})
	req := httptest.NewRequest("PUT", "/api/admin/password", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// Old password should no longer work
	if err := store.ValidateAdmin("root", "oldpass"); err == nil {
		t.Fatal("old password should be invalid after change")
	}
	// New password should work
	if err := store.ValidateAdmin("root", "newpass123"); err != nil {
		t.Fatalf("new password should validate: %v", err)
	}
}

// TestMCPInfo tests GET /api/admin/mcp-info
func TestMCPInfo(t *testing.T) {
	store := newTestAuthStore(t)
	srv := NewServer("localhost:0", &MockEngine{}, store, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil, MCPInfo{
		Addr:     ":8750",
		HasToken: true,
	})

	req := httptest.NewRequest("GET", "/api/admin/mcp-info", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp MCPInfoResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.URL != "http://127.0.0.1:8750/mcp" {
		t.Errorf("unexpected URL: %q", resp.URL)
	}
	if !resp.TokenConfigured {
		t.Error("expected token_configured=true")
	}
}

// TestMCPInfo_NoToken verifies token_configured=false when no token.
func TestMCPInfo_NoToken(t *testing.T) {
	store := newTestAuthStore(t)
	srv := NewServer("localhost:0", &MockEngine{}, store, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil, MCPInfo{
		Addr:     ":8750",
		HasToken: false,
	})

	req := httptest.NewRequest("GET", "/api/admin/mcp-info", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	var resp MCPInfoResponse
	json.NewDecoder(w.Body).Decode(&resp)
	if resp.TokenConfigured {
		t.Error("expected token_configured=false when no token")
	}
}

// TestMCPInfo_CustomPort verifies custom port is reflected in URL.
func TestMCPInfo_CustomPort(t *testing.T) {
	store := newTestAuthStore(t)
	srv := NewServer("localhost:0", &MockEngine{}, store, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil, MCPInfo{
		Addr:     ":9999",
		HasToken: false,
	})

	req := httptest.NewRequest("GET", "/api/admin/mcp-info", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	var resp MCPInfoResponse
	json.NewDecoder(w.Body).Decode(&resp)
	if resp.URL != "http://127.0.0.1:9999/mcp" {
		t.Errorf("unexpected URL for custom port: %q", resp.URL)
	}
}

// TestChangeAdminPasswordEmptyRejected tests that empty new_password is rejected
func TestChangeAdminPasswordEmptyRejected(t *testing.T) {
	store := newTestAuthStore(t)
	srv := newTestServer(t, store)

	body, _ := json.Marshal(map[string]string{
		"username":     "root",
		"new_password": "",
	})
	req := httptest.NewRequest("PUT", "/api/admin/password", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for empty password, got %d", w.Code)
	}
}

// TestHandlePlugins_NilRegistry verifies empty array returned when no registry.
func TestHandlePlugins_NilRegistry(t *testing.T) {
	store := newTestAuthStore(t)
	srv := newTestServer(t, store)

	req := httptest.NewRequest("GET", "/api/admin/plugins", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp []interface{}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp) != 0 {
		t.Fatalf("expected empty array, got %d items", len(resp))
	}
}

// TestHandlePlugins_EmptyRegistry verifies empty array returned for non-nil empty registry.
func TestHandlePlugins_EmptyRegistry(t *testing.T) {
	store := newTestAuthStore(t)
	reg := plugin.NewRegistry()
	srv := NewServer("localhost:0", &MockEngine{}, store, nil, nil, EmbedInfo{}, EnrichInfo{}, reg, "", nil, MCPInfo{})

	req := httptest.NewRequest("GET", "/api/admin/plugins", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp []interface{}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp) != 0 {
		t.Fatalf("expected empty array for empty registry, got %d items", len(resp))
	}
}

// newTestCoordinator creates a minimal ClusterCoordinator backed by an in-memory pebble DB.
func newTestCoordinator(t *testing.T) *replication.ClusterCoordinator {
	t.Helper()
	return newTestCoordinatorWithSecret(t, "")
}

// newTestCoordinatorWithSecret creates a minimal ClusterCoordinator with an optional
// cluster secret. Pass a non-empty secret to test cluster auth middleware behaviour.
func newTestCoordinatorWithSecret(t *testing.T, secret string) *replication.ClusterCoordinator {
	t.Helper()
	db, err := pebble.Open("", &pebble.Options{FS: vfs.NewMem()})
	if err != nil {
		t.Fatalf("open pebble: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	repLog := replication.NewReplicationLog(db)
	applier := replication.NewApplier(db)
	epochStore, err := replication.NewEpochStore(db)
	if err != nil {
		t.Fatalf("new epoch store: %v", err)
	}
	cfg := &config.ClusterConfig{
		Enabled:       true,
		NodeID:        "test-node",
		BindAddr:      "127.0.0.1:0",
		Role:          "primary",
		LeaseTTL:      10,
		HeartbeatMS:   1000,
		ClusterSecret: secret,
	}
	coord := replication.NewClusterCoordinator(cfg, repLog, applier, epochStore)
	t.Cleanup(func() { coord.Stop() })
	return coord
}

// TestReplicationStatus_ClusterDisabled verifies {"enabled":false} when no coordinator.
func TestReplicationStatus_ClusterDisabled(t *testing.T) {
	srv := newTestServer(t, nil)

	req := httptest.NewRequest("GET", "/v1/replication/status", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if enabled, _ := resp["enabled"].(bool); enabled {
		t.Fatal("expected enabled=false when cluster disabled")
	}
}

// TestReplicationStatus_WithCoordinator verifies real data is returned.
func TestReplicationStatus_WithCoordinator(t *testing.T) {
	const testSecret = "test-cluster-secret"
	srv := newTestServer(t, nil)
	coord := newTestCoordinatorWithSecret(t, testSecret)
	srv.SetCoordinator(coord)

	req := httptest.NewRequest("GET", "/v1/replication/status", nil)
	req.Header.Set("Authorization", "Bearer "+testSecret)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if enabled, _ := resp["enabled"].(bool); !enabled {
		t.Fatal("expected enabled=true when coordinator set")
	}
	if _, ok := resp["role"]; !ok {
		t.Error("expected role field in response")
	}
	if _, ok := resp["epoch"]; !ok {
		t.Error("expected epoch field in response")
	}
	if _, ok := resp["node_id"]; !ok {
		t.Error("expected node_id field in response")
	}
}

// TestReplicationLag_ClusterDisabled verifies 503 when no coordinator.
func TestReplicationLag_ClusterDisabled(t *testing.T) {
	srv := newTestServer(t, nil)

	req := httptest.NewRequest("GET", "/v1/replication/lag", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d: %s", w.Code, w.Body.String())
	}
}

// TestReplicationLag_WithCoordinator verifies lag data is returned.
func TestReplicationLag_WithCoordinator(t *testing.T) {
	const testSecret = "test-cluster-secret"
	srv := newTestServer(t, nil)
	coord := newTestCoordinatorWithSecret(t, testSecret)
	srv.SetCoordinator(coord)

	req := httptest.NewRequest("GET", "/v1/replication/lag", nil)
	req.Header.Set("Authorization", "Bearer "+testSecret)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := resp["lag"]; !ok {
		t.Error("expected lag field in response")
	}
	if _, ok := resp["role"]; !ok {
		t.Error("expected role field in response")
	}
}

// TestReplicationPromote_ClusterDisabled verifies 503 when no coordinator.
func TestReplicationPromote_ClusterDisabled(t *testing.T) {
	srv := newTestServer(t, nil)

	req := httptest.NewRequest("POST", "/v1/replication/promote", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d: %s", w.Code, w.Body.String())
	}
}

// TestReplicationPromote_WithCoordinator verifies election is triggered.
func TestReplicationPromote_WithCoordinator(t *testing.T) {
	const testSecret = "test-cluster-secret"
	srv := newTestServer(t, nil)
	coord := newTestCoordinatorWithSecret(t, testSecret)
	srv.SetCoordinator(coord)

	req := httptest.NewRequest("POST", "/v1/replication/promote", nil)
	req.Header.Set("Authorization", "Bearer "+testSecret)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	// Single-node cluster wins immediately — triggered=true
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if triggered, _ := resp["triggered"].(bool); !triggered {
		t.Error("expected triggered=true in response")
	}
}

// ---------------------------------------------------------------------------
// canonicalize() tests
// ---------------------------------------------------------------------------

func TestCanonicalize(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"datadog-clone", "datadogclone"},
		{"FOO_BAR", "foobar"},
		{"my-vault-2", "myvault2"},
		{"hello world", "helloworld"},
		{"café", "caf"},
		{"vault.name", "vaultname"},
		{"123-abc", "123abc"},
		{"", ""},
		{"---", ""},
		{"HELLO", "hello"},
		{"a1b2c3", "a1b2c3"},
	}
	for _, tc := range cases {
		got := canonicalize(tc.input)
		if got != tc.want {
			t.Errorf("canonicalize(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
// handleSetVaultConfig — collision detection tests
// ---------------------------------------------------------------------------

// newCollisionTestServer builds a Server pre-seeded with existingVaultNames in the
// auth store so that collectVaultNames detects near-duplicate names.
func newCollisionTestServer(t *testing.T, existingVaultNames []string) (*Server, *auth.Store) {
	t.Helper()
	store := newTestAuthStore(t)
	// Pre-populate the existing vault names into the auth store so
	// collectVaultNames picks them up via ListVaultConfigs.
	for _, name := range existingVaultNames {
		if err := store.SetVaultConfig(auth.VaultConfig{Name: name}); err != nil {
			t.Fatalf("seed vault %q: %v", name, err)
		}
	}
	srv := newTestServer(t, store)
	return srv, store
}

func TestHandleSetVaultConfig_DuplicateDetection(t *testing.T) {
	// "datadog-clone" already exists; creating "datadogclone" should yield 409.
	srv, _ := newCollisionTestServer(t, []string{"datadog-clone"})

	body, _ := json.Marshal(map[string]interface{}{"name": "datadogclone"})
	req := httptest.NewRequest("PUT", "/api/admin/vaults/config", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409 for collision, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]string
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp["code"] != "VAULT_NAME_COLLISION" {
		t.Errorf("expected code=VAULT_NAME_COLLISION, got %q", resp["code"])
	}
	if resp["conflict"] != "datadog-clone" {
		t.Errorf("expected conflict=datadog-clone, got %q", resp["conflict"])
	}
	if resp["normalized"] != "datadogclone" {
		t.Errorf("expected normalized=datadogclone, got %q", resp["normalized"])
	}
}

func TestHandleSetVaultConfig_ForceOverride(t *testing.T) {
	// Same scenario with ?force=true — should succeed (200 OK).
	srv, _ := newCollisionTestServer(t, []string{"datadog-clone"})

	body, _ := json.Marshal(map[string]interface{}{"name": "datadogclone"})
	req := httptest.NewRequest("PUT", "/api/admin/vaults/config?force=true", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 with force=true, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleSetVaultConfig_ForcePreservesAuth(t *testing.T) {
	// force=true bypasses collision check but NOT auth.
	// When the server has a session secret, unauthenticated requests return 401.
	store := newTestAuthStore(t)
	if err := store.SetVaultConfig(auth.VaultConfig{Name: "datadog-clone"}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	secret := []byte("test-secret")
	srv := NewServer("localhost:0", &MockEngine{}, store, secret, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	body, _ := json.Marshal(map[string]interface{}{"name": "datadogclone"})
	req := httptest.NewRequest("PUT", "/api/admin/vaults/config?force=true", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	// No session cookie — should be rejected by admin auth middleware.
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without auth (force=true must not bypass auth), got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleSetVaultConfig_ExactNameIsUpdate(t *testing.T) {
	// Creating/updating a vault with the exact same name as an existing vault
	// is NOT a collision — it's an update/overwrite and should return 200.
	srv, _ := newCollisionTestServer(t, []string{"myvault"})

	body, _ := json.Marshal(map[string]interface{}{"name": "myvault", "public": true})
	req := httptest.NewRequest("PUT", "/api/admin/vaults/config", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for exact name update (not a collision), got %d: %s", w.Code, w.Body.String())
	}
}

// ---------------------------------------------------------------------------
// handleRenameVault — collision detection test
// ---------------------------------------------------------------------------

func TestHandleRenameVault_DuplicateDetection(t *testing.T) {
	// Vault "my-vault" exists. Rename "source" to "myvault" → canonical collision → 409.
	store := newTestAuthStore(t)
	// Seed "my-vault" into the auth store so collectVaultNames finds it.
	if err := store.SetVaultConfig(auth.VaultConfig{Name: "my-vault"}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	srv := newTestServer(t, store)

	body, _ := json.Marshal(map[string]string{"new_name": "myvault"})
	req := httptest.NewRequest("POST", "/api/admin/vaults/source/rename", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409 for rename collision, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]string
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp["code"] != "VAULT_NAME_COLLISION" {
		t.Errorf("expected code=VAULT_NAME_COLLISION, got %q", resp["code"])
	}
	if resp["conflict"] != "my-vault" {
		t.Errorf("expected conflict=my-vault, got %q", resp["conflict"])
	}
}

// TestEntityGraph_EmptyVaultDefaultsToDefault verifies that omitting the vault
// query parameter defaults to the "default" vault.
func TestEntityGraph_EmptyVaultDefaultsToDefault(t *testing.T) {
	srv := newTestServer(t, newTestAuthStore(t))

	req := httptest.NewRequest("GET", "/api/admin/entity-graph", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 with no vault param, got %d: %s", w.Code, w.Body.String())
	}
}

// TestEntityGraph returns nodes/edges from the mock engine.
func TestEntityGraph(t *testing.T) {
	srv := newTestServer(t, newTestAuthStore(t))

	req := httptest.NewRequest("GET", "/api/admin/entity-graph?vault=default", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp EntityGraphResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// MockEngine.ExportGraph returns empty graph — just verify the shape.
	if resp.Nodes == nil {
		t.Error("expected non-nil nodes slice")
	}
	if resp.Edges == nil {
		t.Error("expected non-nil edges slice")
	}
}

// TestEntityGraph_InvalidVault returns 400 for an invalid vault name.
func TestEntityGraph_InvalidVault(t *testing.T) {
	srv := newTestServer(t, newTestAuthStore(t))

	req := httptest.NewRequest("GET", "/api/admin/entity-graph?vault=../../etc/passwd", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid vault, got %d: %s", w.Code, w.Body.String())
	}
}

// TestEntityGraph_RequiresAdminAuth verifies 401 is returned when auth is
// configured and no session cookie is present.
func TestEntityGraph_RequiresAdminAuth(t *testing.T) {
	store := newTestAuthStore(t)
	secret := []byte("test-session-secret-32bytes-ok!!")
	srv := NewServer("localhost:0", &MockEngine{}, store, secret, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	req := httptest.NewRequest("GET", "/api/admin/entity-graph?vault=default", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without session cookie, got %d: %s", w.Code, w.Body.String())
	}
}

// TestEntityGraph_AllowedWithValidSession verifies 200 with a valid session cookie.
func TestEntityGraph_AllowedWithValidSession(t *testing.T) {
	store := newTestAuthStore(t)
	secret := []byte("test-session-secret-32bytes-ok!!")
	srv := NewServer("localhost:0", &MockEngine{}, store, secret, nil, EmbedInfo{}, EnrichInfo{}, nil, "", nil)

	token, err := auth.NewSessionToken("admin", secret)
	if err != nil {
		t.Fatalf("NewSessionToken: %v", err)
	}

	req := httptest.NewRequest("GET", "/api/admin/entity-graph?vault=default", nil)
	req.AddCookie(&http.Cookie{Name: "muninn_session", Value: token})
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 with valid session cookie, got %d: %s", w.Code, w.Body.String())
	}
	var resp EntityGraphResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Nodes == nil || resp.Edges == nil {
		t.Error("expected non-nil nodes and edges")
	}
}

// mockEngineWithStats embeds MockEngine but allows controlling EmbedStats and CountEmbedded.
type mockEngineWithStats struct {
	MockEngine
	embedStats    plugin.RetroactiveStats
	embeddedCount int64
	totalCount    int64
}

func (m *mockEngineWithStats) EmbedStats() plugin.RetroactiveStats {
	return m.embedStats
}

func (m *mockEngineWithStats) CountEmbedded(ctx context.Context) int64 {
	return m.embeddedCount
}

func (m *mockEngineWithStats) Stat(ctx context.Context, req *StatRequest) (*StatResponse, error) {
	return &StatResponse{
		EngramCount:  m.totalCount,
		VaultCount:   1,
		StorageBytes: 1024,
	}, nil
}

// TestHandleEmbedStatus_IncludesRateAndETA verifies that rate_per_sec and eta_seconds
// are populated from EmbedStats when the server is actively indexing.
func TestHandleEmbedStatus_IncludesRateAndETA(t *testing.T) {
	eng := &mockEngineWithStats{
		embedStats: plugin.RetroactiveStats{
			RatePerSec: 1.5,
			ETASeconds: 120,
		},
		embeddedCount: 50,  // less than total → indexing=true
		totalCount:    100,
	}
	srv := NewServer("localhost:0", eng, nil, nil, nil, EmbedInfo{Provider: "ollama", Model: "nomic"}, EnrichInfo{}, nil, "", nil)

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
	if !resp.Indexing {
		t.Error("expected indexing=true")
	}
	if resp.RatePerSec != 1.5 {
		t.Errorf("expected rate_per_sec=1.5, got %v", resp.RatePerSec)
	}
	if resp.ETASeconds != 120 {
		t.Errorf("expected eta_seconds=120, got %v", resp.ETASeconds)
	}
}

// TestHandleEmbedStatus_ZeroRateWhenIdle verifies that rate_per_sec and eta_seconds
// are 0 when the server is not actively indexing (all engrams already embedded).
func TestHandleEmbedStatus_ZeroRateWhenIdle(t *testing.T) {
	eng := &mockEngineWithStats{
		embedStats: plugin.RetroactiveStats{
			RatePerSec: 5.0,
			ETASeconds: 999,
		},
		embeddedCount: 100,  // equal to total → indexing=false
		totalCount:    100,
	}
	srv := NewServer("localhost:0", eng, nil, nil, nil, EmbedInfo{Provider: "ollama", Model: "nomic"}, EnrichInfo{}, nil, "", nil)

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
	if resp.Indexing {
		t.Error("expected indexing=false when all engrams are embedded")
	}
	if resp.RatePerSec != 0 {
		t.Errorf("expected rate_per_sec=0 when idle, got %v", resp.RatePerSec)
	}
	if resp.ETASeconds != 0 {
		t.Errorf("expected eta_seconds=0 when idle, got %v", resp.ETASeconds)
	}
}

// TestHandleEmbedStatus_HardwareAccelerated verifies that hardware_accelerated
// is reflected correctly in the response when set on the server.
func TestHandleEmbedStatus_HardwareAccelerated(t *testing.T) {
	trueVal := true
	eng := &mockEngineWithStats{
		embeddedCount: 100,
		totalCount:    100,
	}
	srv := NewServer("localhost:0", eng, nil, nil, nil,
		EmbedInfo{Provider: "ollama", Model: "nomic", HardwareAccelerated: &trueVal},
		EnrichInfo{}, nil, "", nil)

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
	if resp.HardwareAccelerated == nil {
		t.Fatal("expected hardware_accelerated to be non-nil")
	}
	if !*resp.HardwareAccelerated {
		t.Error("expected hardware_accelerated=true")
	}
}
