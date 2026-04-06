package mcp

// auth_mk_test.go — tests for mk_ vault API key authentication on the MCP endpoint.
//
// These tests cover:
//   - mk_ token accepted when apiKeyStore is provided and token is valid
//   - mk_ token rejected when apiKeyStore returns error (expired/revoked)
//   - mk_ token rejected when no apiKeyStore is configured (nil)
//   - static mdb_ token still works (backward compat)
//   - static mdb_ token has full access to all vaults (no pinning)
//   - vault pinning: mk_ key scoped to vault A cannot write to vault B
//   - vault pinning: mk_ key scoped to vault A can write to vault A
//   - observe-mode key blocked from mutating tools
//   - observe-mode key allowed on read tools
//   - write-mode key blocked from read tools
//   - write-mode key allowed on mutating tools
//   - full-mode key allowed on all tools
//   - MCP endpoint returns 401 for missing/invalid token
//   - SSE stream stores full AuthContext (vault+mode preserved)

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/scrypster/muninndb/internal/auth"
)

// --- Mock apiKeyValidator ---

// mockKeyStore is a simple in-memory apiKeyValidator for tests.
type mockKeyStore struct {
	keys map[string]auth.APIKey // token → APIKey
}

func newMockKeyStore(keys ...auth.APIKey) *mockKeyStore {
	m := &mockKeyStore{keys: make(map[string]auth.APIKey)}
	for _, k := range keys {
		m.keys["mk_"+k.ID] = k
	}
	return m
}

func (m *mockKeyStore) ValidateAPIKey(token string) (auth.APIKey, error) {
	k, ok := m.keys[token]
	if !ok {
		return auth.APIKey{}, fmt.Errorf("key not found or expired")
	}
	return k, nil
}

// --- authFromRequest unit tests ---

func TestAuthFromRequest_MkToken_ValidFullMode(t *testing.T) {
	store := newMockKeyStore(auth.APIKey{
		ID:    "abc123",
		Vault: "project-x",
		Mode:  auth.ModeFull,
	})
	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	req.Header.Set("Authorization", "Bearer mk_abc123")

	a := authFromRequest(req, "mdb_statictoken", store)

	if !a.Authorized {
		t.Fatal("expected Authorized=true for valid mk_ key")
	}
	if !a.IsAPIKey {
		t.Error("expected IsAPIKey=true")
	}
	if a.Vault != "project-x" {
		t.Errorf("expected Vault=project-x, got %q", a.Vault)
	}
	if a.Mode != auth.ModeFull {
		t.Errorf("expected Mode=full, got %q", a.Mode)
	}
	if a.Token != "mk_abc123" {
		t.Errorf("expected Token=mk_abc123, got %q", a.Token)
	}
}

func TestAuthFromRequest_MkToken_ObserveMode(t *testing.T) {
	store := newMockKeyStore(auth.APIKey{
		ID:    "obs999",
		Vault: "analytics",
		Mode:  auth.ModeObserve,
	})
	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	req.Header.Set("Authorization", "Bearer mk_obs999")

	a := authFromRequest(req, "mdb_x", store)

	if !a.Authorized {
		t.Fatal("expected Authorized=true")
	}
	if a.Mode != auth.ModeObserve {
		t.Errorf("expected observe mode, got %q", a.Mode)
	}
}

func TestAuthFromRequest_MkToken_WriteMode(t *testing.T) {
	store := newMockKeyStore(auth.APIKey{
		ID:    "wrt777",
		Vault: "ingest",
		Mode:  auth.ModeWrite,
	})
	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	req.Header.Set("Authorization", "Bearer mk_wrt777")

	a := authFromRequest(req, "mdb_x", store)

	if a.Mode != auth.ModeWrite {
		t.Errorf("expected write mode, got %q", a.Mode)
	}
}

func TestAuthFromRequest_MkToken_Expired(t *testing.T) {
	store := &mockKeyStore{keys: map[string]auth.APIKey{}} // empty store = all keys invalid
	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	req.Header.Set("Authorization", "Bearer mk_expired")

	a := authFromRequest(req, "mdb_x", store)

	if a.Authorized {
		t.Error("expected Authorized=false for expired/revoked key")
	}
}

func TestAuthFromRequest_MkToken_NilStore(t *testing.T) {
	// When no apiKeyStore is configured, mk_ tokens must be rejected.
	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	req.Header.Set("Authorization", "Bearer mk_somekey")

	a := authFromRequest(req, "mdb_static", nil)

	if a.Authorized {
		t.Error("expected Authorized=false when apiKeyStore is nil")
	}
}

func TestAuthFromRequest_StaticToken_BackwardCompat(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	req.Header.Set("Authorization", "Bearer mdb_mytoken")

	a := authFromRequest(req, "mdb_mytoken", nil)

	if !a.Authorized {
		t.Fatal("static mdb_ token must still work")
	}
	if a.IsAPIKey {
		t.Error("static token should not set IsAPIKey=true")
	}
	if a.Vault != "" {
		t.Error("static token should not pin a vault")
	}
}

func TestAuthFromRequest_StaticToken_WrongValue(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	req.Header.Set("Authorization", "Bearer mdb_wrong")

	a := authFromRequest(req, "mdb_correct", nil)

	if a.Authorized {
		t.Error("wrong static token must be rejected")
	}
}

func TestAuthFromRequest_NoToken(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)

	a := authFromRequest(req, "mdb_x", nil)

	if a.Authorized {
		t.Error("missing token must be rejected")
	}
}

func TestAuthFromRequest_EmptyRequiredToken(t *testing.T) {
	// Server started with no token = open access.
	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)

	a := authFromRequest(req, "", nil)

	if !a.Authorized {
		t.Error("no required token = should always authorize")
	}
}

// TestAuthFromRequest_OpenServer_ValidMkKey verifies that a valid mk_ key is
// fully enforced (vault pinned) even when no static token is configured.
// This is the regression test for the open-server vault isolation bypass.
func TestAuthFromRequest_OpenServer_ValidMkKey(t *testing.T) {
	store := newMockKeyStore(auth.APIKey{
		ID:    "vaultkey1",
		Vault: "personal",
		Mode:  "full",
	})
	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	req.Header.Set("Authorization", "Bearer mk_vaultkey1")

	a := authFromRequest(req, "", store) // no static token = open-server mode

	if !a.Authorized {
		t.Fatal("valid mk_ key must be authorized in open-server mode")
	}
	if !a.IsAPIKey {
		t.Error("expected IsAPIKey=true")
	}
	if a.Vault != "personal" {
		t.Errorf("expected vault=personal, got %q", a.Vault)
	}
}

// TestAuthFromRequest_OpenServer_InvalidMkKey verifies that an invalid mk_ key
// is rejected (not silently upgraded to open access) in open-server mode.
func TestAuthFromRequest_OpenServer_InvalidMkKey(t *testing.T) {
	store := newMockKeyStore() // empty — no valid keys
	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	req.Header.Set("Authorization", "Bearer mk_badkey")

	a := authFromRequest(req, "", store) // no static token = open-server mode

	if a.Authorized {
		t.Error("invalid mk_ key must not fall through to open access")
	}
}

// TestAuthFromRequest_OpenServer_NoKey verifies that a request with no token
// still gets open access when no static token is configured and no mk_ key is
// presented (plain open-server mode, unchanged behaviour).
func TestAuthFromRequest_OpenServer_NoKey(t *testing.T) {
	store := newMockKeyStore()
	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)

	a := authFromRequest(req, "", store)

	if !a.Authorized {
		t.Error("no key presented in open-server mode must still authorize")
	}
	if a.IsAPIKey {
		t.Error("expected IsAPIKey=false for unauthenticated open-server request")
	}
}

// --- resolveVault unit tests ---

func TestResolveVault_PinnedVault_ArgAbsent(t *testing.T) {
	vault, errMsg := resolveVault("my-vault", map[string]any{})
	if errMsg != "" {
		t.Fatalf("unexpected error: %s", errMsg)
	}
	if vault != "my-vault" {
		t.Errorf("expected my-vault, got %q", vault)
	}
}

func TestResolveVault_PinnedVault_ArgMatchesPinned(t *testing.T) {
	vault, errMsg := resolveVault("my-vault", map[string]any{"vault": "my-vault"})
	if errMsg != "" || vault != "my-vault" {
		t.Errorf("got vault=%q err=%q", vault, errMsg)
	}
}

func TestResolveVault_PinnedVault_ArgDiffers_Error(t *testing.T) {
	_, errMsg := resolveVault("my-vault", map[string]any{"vault": "other-vault"})
	if errMsg == "" {
		t.Fatal("expected vault mismatch error")
	}
	if !strings.Contains(errMsg, "vault mismatch") {
		t.Errorf("error should say 'vault mismatch', got: %s", errMsg)
	}
	// Security: error message should NOT leak the pinned vault name.
	if strings.Contains(errMsg, "my-vault") {
		t.Errorf("error should not leak pinned vault name, got: %s", errMsg)
	}
}

func TestResolveVault_NoPinned_ExplicitArg(t *testing.T) {
	vault, errMsg := resolveVault("", map[string]any{"vault": "custom"})
	if errMsg != "" || vault != "custom" {
		t.Errorf("got vault=%q err=%q", vault, errMsg)
	}
}

func TestResolveVault_NoPinned_NoArg_DefaultsToDefault(t *testing.T) {
	vault, errMsg := resolveVault("", map[string]any{})
	if errMsg != "" || vault != "default" {
		t.Errorf("got vault=%q err=%q", vault, errMsg)
	}
}

// --- isMutatingTool unit tests ---

func TestIsMutatingTool_MutatingSet(t *testing.T) {
	mutating := []string{
		"muninn_remember", "muninn_remember_batch", "muninn_remember_tree",
		"muninn_add_child", "muninn_forget", "muninn_link", "muninn_evolve",
		"muninn_consolidate", "muninn_decide", "muninn_restore",
		"muninn_retry_enrich", "muninn_entity_state", "muninn_entity_state_batch",
		"muninn_merge_entity", "muninn_replay_enrichment", "muninn_feedback",
	}
	for _, name := range mutating {
		if !isMutatingTool(name) {
			t.Errorf("isMutatingTool(%q) should be true", name)
		}
	}
}

func TestIsMutatingTool_ReadSet(t *testing.T) {
	readonly := []string{
		"muninn_recall", "muninn_read", "muninn_status", "muninn_session",
		"muninn_contradictions", "muninn_traverse", "muninn_explain",
		"muninn_state", "muninn_list_deleted", "muninn_guide",
		"muninn_where_left_off", "muninn_recall_tree",
		"muninn_find_by_entity", "muninn_entity_clusters", "muninn_export_graph",
		"muninn_similar_entities", "muninn_entity_timeline", "muninn_provenance",
		"muninn_entity", "muninn_entities",
	}
	for _, name := range readonly {
		if isMutatingTool(name) {
			t.Errorf("isMutatingTool(%q) should be false", name)
		}
	}
}

// --- Integration: dispatchToolCall mode enforcement via HTTP ---

func mkToolCallBody(toolName string, args map[string]any) []byte {
	if args == nil {
		args = map[string]any{"vault": "test-vault"}
	}
	req := map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      toolName,
			"arguments": args,
		},
	}
	b, _ := json.Marshal(req)
	return b
}

// testServer wraps MCPServer with a mock engine and key store for integration tests.
func newAuthTestServer(keyStore apiKeyValidator) *MCPServer {
	eng := &fakeEngine{}
	return New(":0", eng, "mdb_static", keyStore, nil)
}

func doAuthenticatedPost(srv *MCPServer, token string, body []byte) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleRPC(w, r.WithContext(contextWithAuth(r.Context(), authFromRequest(r, srv.token, srv.authKeys))))
	return w
}

func TestDispatch_ObserveMode_BlocksMutatingTool(t *testing.T) {
	store := newMockKeyStore(auth.APIKey{
		ID:    "obs001",
		Vault: "walled",
		Mode:  auth.ModeObserve,
	})
	srv := newAuthTestServer(store)
	body := mkToolCallBody("muninn_remember", map[string]any{"vault": "walled", "concept": "x", "content": "y"})

	w := doAuthenticatedPost(srv, "mk_obs001", body)

	var resp JSONRPCResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if resp.Error == nil {
		t.Fatal("expected error response for observe-mode key calling mutating tool")
	}
	if !strings.Contains(resp.Error.Message, "forbidden") {
		t.Errorf("expected 'forbidden' in error, got: %s", resp.Error.Message)
	}
}

func TestDispatch_ObserveMode_AllowsReadTool(t *testing.T) {
	store := newMockKeyStore(auth.APIKey{
		ID:    "obs002",
		Vault: "walled",
		Mode:  auth.ModeObserve,
	})
	srv := newAuthTestServer(store)
	body := mkToolCallBody("muninn_recall", map[string]any{"vault": "walled", "context": "test"})

	w := doAuthenticatedPost(srv, "mk_obs002", body)

	var resp JSONRPCResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	// fakeEngine returns an empty result — no error expected from mode enforcement
	if resp.Error != nil && strings.Contains(resp.Error.Message, "forbidden") {
		t.Errorf("observe-mode key should be allowed to call read tools, got: %s", resp.Error.Message)
	}
}

func TestDispatch_WriteMode_BlocksReadTool(t *testing.T) {
	store := newMockKeyStore(auth.APIKey{
		ID:    "wrt001",
		Vault: "ingest",
		Mode:  auth.ModeWrite,
	})
	srv := newAuthTestServer(store)
	body := mkToolCallBody("muninn_recall", map[string]any{"vault": "ingest", "context": "test"})

	w := doAuthenticatedPost(srv, "mk_wrt001", body)

	var resp JSONRPCResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if resp.Error == nil {
		t.Fatal("expected error response for write-only key calling read tool")
	}
	if !strings.Contains(resp.Error.Message, "forbidden") {
		t.Errorf("expected 'forbidden' in error, got: %s", resp.Error.Message)
	}
}

func TestDispatch_WriteMode_AllowsMutatingTool(t *testing.T) {
	store := newMockKeyStore(auth.APIKey{
		ID:    "wrt002",
		Vault: "ingest",
		Mode:  auth.ModeWrite,
	})
	srv := newAuthTestServer(store)
	body := mkToolCallBody("muninn_remember", map[string]any{"vault": "ingest", "concept": "x", "content": "y"})

	w := doAuthenticatedPost(srv, "mk_wrt002", body)

	var resp JSONRPCResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if resp.Error != nil && strings.Contains(resp.Error.Message, "forbidden") {
		t.Errorf("write-mode key should be allowed to call mutating tools, got: %s", resp.Error.Message)
	}
}

func TestDispatch_FullMode_AllowsAll(t *testing.T) {
	store := newMockKeyStore(auth.APIKey{
		ID:    "full001",
		Vault: "all",
		Mode:  auth.ModeFull,
	})
	srv := newAuthTestServer(store)

	for _, tool := range []string{"muninn_recall", "muninn_remember"} {
		body := mkToolCallBody(tool, map[string]any{"vault": "all", "context": "x", "concept": "x", "content": "y"})
		w := doAuthenticatedPost(srv, "mk_full001", body)

		var resp JSONRPCResponse
		if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
			t.Fatalf("decode error for tool %s: %v", tool, err)
		}
		if resp.Error != nil && strings.Contains(resp.Error.Message, "forbidden") {
			t.Errorf("full-mode key blocked from %s: %s", tool, resp.Error.Message)
		}
	}
}

func TestDispatch_VaultMismatch_Rejected(t *testing.T) {
	store := newMockKeyStore(auth.APIKey{
		ID:    "pin001",
		Vault: "vault-a",
		Mode:  auth.ModeFull,
	})
	srv := newAuthTestServer(store)
	// Key is pinned to vault-a, but tool call specifies vault-b
	body := mkToolCallBody("muninn_remember", map[string]any{
		"vault":   "vault-b",
		"concept": "x",
		"content": "y",
	})

	w := doAuthenticatedPost(srv, "mk_pin001", body)

	var resp JSONRPCResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if resp.Error == nil {
		t.Fatal("expected error for cross-vault write attempt")
	}
	if !strings.Contains(resp.Error.Message, "vault mismatch") {
		t.Errorf("expected 'vault mismatch' in error, got: %s", resp.Error.Message)
	}
	// Security: pinned vault name should NOT appear in the error.
	if strings.Contains(resp.Error.Message, "vault-a") {
		t.Errorf("error message should not leak pinned vault name, got: %s", resp.Error.Message)
	}
}

func TestDispatch_StaticToken_FullAccess_AnyVault(t *testing.T) {
	srv := newAuthTestServer(nil)
	// Static token has no vault pinning — can write to any vault
	for _, vault := range []string{"vault-a", "vault-b", "default"} {
		body := mkToolCallBody("muninn_remember", map[string]any{
			"vault":   vault,
			"concept": "x",
			"content": "y",
		})
		r := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(body))
		r.Header.Set("Authorization", "Bearer mdb_static")
		a := authFromRequest(r, srv.token, srv.authKeys)
		w := httptest.NewRecorder()
		srv.handleRPC(w, r.WithContext(contextWithAuth(r.Context(), a)))

		var resp JSONRPCResponse
		if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
			t.Fatalf("decode error for vault %s: %v", vault, err)
		}
		if resp.Error != nil && strings.Contains(resp.Error.Message, "vault mismatch") {
			t.Errorf("static token should not be vault-pinned, got error for vault %s: %s", vault, resp.Error.Message)
		}
	}
}

func TestEndpoint_Returns401_MissingToken(t *testing.T) {
	srv := newAuthTestServer(nil)
	body := mkToolCallBody("muninn_recall", map[string]any{"context": "test"})

	r := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(body))
	// No Authorization header
	w := httptest.NewRecorder()
	srv.handleStreamablePost(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", w.Code)
	}
}

func TestEndpoint_Returns401_WrongToken(t *testing.T) {
	srv := newAuthTestServer(nil)
	body := mkToolCallBody("muninn_recall", map[string]any{"context": "test"})

	r := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(body))
	r.Header.Set("Authorization", "Bearer wrong_token")
	w := httptest.NewRecorder()
	srv.handleStreamablePost(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", w.Code)
	}
}

// TestSSESession_StoresFullAuthContext verifies that the SSE session stores the
// full AuthContext (including vault and mode) when established via an mk_ key.
func TestSSESession_StoresFullAuthContext(t *testing.T) {
	store := newMockKeyStore(auth.APIKey{
		ID:    "sse001",
		Vault: "sse-vault",
		Mode:  auth.ModeObserve,
	})
	srv := New(":0", &fakeEngine{}, "mdb_static", store, nil)

	// Manually insert a session as handleSSE would after auth
	a := authFromRequest(func() *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/mcp", nil)
		r.Header.Set("Authorization", "Bearer mk_sse001")
		return r
	}(), "mdb_static", store)

	srv.sseSessionsMu.Lock()
	srv.sseSessions["test-session"] = &sseSession{
		ch:   make(chan []byte, 4),
		auth: a,
	}
	srv.sseSessionsMu.Unlock()

	srv.sseSessionsMu.RLock()
	sess := srv.sseSessions["test-session"]
	srv.sseSessionsMu.RUnlock()

	if sess.auth.Vault != "sse-vault" {
		t.Errorf("expected Vault=sse-vault in stored session, got %q", sess.auth.Vault)
	}
	if sess.auth.Mode != auth.ModeObserve {
		t.Errorf("expected Mode=observe in stored session, got %q", sess.auth.Mode)
	}
	if !sess.auth.IsAPIKey {
		t.Error("expected IsAPIKey=true in stored session")
	}
}

// ═══════════════════════════════════════════════════════════════════════════════
// Security hardening tests — added during enterprise security review.
// ═══════════════════════════════════════════════════════════════════════════════

// --- Tool classification completeness ---

// TestToolClassification_CoversAllRegisteredHandlers ensures that every tool in
// the dispatchToolCall handler map is classified as either mutating or read-only.
// This test FAILS if a new tool is added to server.go but not added to
// isMutatingTool or isReadOnlyTool — a critical safety net against drift.
func TestToolClassification_CoversAllRegisteredHandlers(t *testing.T) {
	for _, name := range registeredToolNames() {
		mutating := isMutatingTool(name)
		readonly := isReadOnlyTool(name)

		if !mutating && !readonly {
			t.Errorf("tool %q is registered but not classified in isMutatingTool or isReadOnlyTool — "+
				"mode enforcement will block it for observe/write keys (fail-closed)", name)
		}
		if mutating && readonly {
			t.Errorf("tool %q is classified as BOTH mutating AND read-only — must be exactly one", name)
		}
	}
}

// TestToolClassification_UnknownToolDefaultDeny verifies that an unknown tool
// name is classified as neither mutating nor read-only (fail-closed).
func TestToolClassification_UnknownToolDefaultDeny(t *testing.T) {
	if isMutatingTool("muninn_nonexistent") {
		t.Error("unknown tool should not be classified as mutating")
	}
	if isReadOnlyTool("muninn_nonexistent") {
		t.Error("unknown tool should not be classified as read-only")
	}
}

// --- Malformed Authorization header edge cases ---

func TestAuthFromRequest_BearerOnly_NoToken(t *testing.T) {
	// "Bearer " with trailing space but no actual token value.
	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	req.Header.Set("Authorization", "Bearer ")
	a := authFromRequest(req, "mdb_x", nil)
	if a.Authorized {
		t.Error("'Bearer ' with empty token must be rejected")
	}
}

func TestAuthFromRequest_LowercaseBearer(t *testing.T) {
	// "bearer token" (lowercase) — Go's net/http does case-sensitive header values.
	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	req.Header.Set("Authorization", "bearer mdb_x")
	a := authFromRequest(req, "mdb_x", nil)
	if a.Authorized {
		t.Error("lowercase 'bearer' prefix should be rejected (spec requires 'Bearer')")
	}
}

func TestAuthFromRequest_BasicScheme(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	req.Header.Set("Authorization", "Basic dXNlcjpwYXNz")
	a := authFromRequest(req, "mdb_x", nil)
	if a.Authorized {
		t.Error("Basic auth scheme must be rejected")
	}
}

func TestAuthFromRequest_BearerWithExtraSpaces(t *testing.T) {
	// "Bearer  tok" — double space after Bearer.
	// CutPrefix("Bearer ") leaves " tok" which is a different token.
	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	req.Header.Set("Authorization", "Bearer  mdb_x")
	a := authFromRequest(req, "mdb_x", nil)
	if a.Authorized {
		t.Error("token with leading space should not match (timing-safe compare)")
	}
}

func TestAuthFromRequest_BearerWithMultipleTokens(t *testing.T) {
	// "Bearer tok1 tok2" — extra data after the token.
	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	req.Header.Set("Authorization", "Bearer mdb_x extra_data")
	a := authFromRequest(req, "mdb_x", nil)
	if a.Authorized {
		t.Error("token with extra data after space must be rejected")
	}
}

func TestAuthFromRequest_HugeToken_Rejected(t *testing.T) {
	// A 10KB token must be rejected before constant-time compare.
	huge := "Bearer " + strings.Repeat("x", 10000)
	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	req.Header.Set("Authorization", huge)
	a := authFromRequest(req, "mdb_x", nil)
	if a.Authorized {
		t.Error("absurdly long token must be rejected")
	}
}

func TestAuthFromRequest_MkPrefix_EmptySuffix(t *testing.T) {
	// "Bearer mk_" — mk_ prefix with no actual key payload.
	store := newMockKeyStore() // empty store
	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	req.Header.Set("Authorization", "Bearer mk_")
	a := authFromRequest(req, "mdb_x", store)
	if a.Authorized {
		t.Error("'mk_' with empty suffix must be rejected")
	}
}

// --- Invalid vault name rejection (fail-closed) ---

func TestResolveVault_InvalidVaultArg_Rejected(t *testing.T) {
	// Previously fell back to "default" — now must error.
	_, errMsg := resolveVault("", map[string]any{"vault": "INVALID!"})
	if errMsg == "" {
		t.Fatal("expected error for invalid vault name, got empty errMsg")
	}
	if !strings.Contains(errMsg, "invalid vault name") {
		t.Errorf("expected 'invalid vault name' in error, got: %s", errMsg)
	}
}

func TestResolveVault_UnicodeVaultArg_Rejected(t *testing.T) {
	_, errMsg := resolveVault("", map[string]any{"vault": "vault\u200b"}) // zero-width space
	if errMsg == "" {
		t.Fatal("expected error for Unicode vault name")
	}
}

func TestResolveVault_PathTraversalVaultArg_Rejected(t *testing.T) {
	_, errMsg := resolveVault("", map[string]any{"vault": "../etc/passwd"})
	if errMsg == "" {
		t.Fatal("expected error for path traversal vault name")
	}
}

func TestResolveVault_WhitespaceVaultArg_Rejected(t *testing.T) {
	_, errMsg := resolveVault("", map[string]any{"vault": "  "})
	if errMsg == "" {
		t.Fatal("expected error for whitespace vault name")
	}
}

func TestResolveVault_PinnedVault_InvalidArgIsError(t *testing.T) {
	// Even with a pinned vault, an explicitly invalid vault arg should error
	// (not silently use the pinned vault).
	_, errMsg := resolveVault("my-vault", map[string]any{"vault": "INVALID!"})
	if errMsg == "" {
		t.Fatal("expected error for invalid vault arg even with pinned vault")
	}
}

// --- Mode enforcement on ALL tools via dispatch ---

func TestDispatch_ObserveMode_AllReadToolsAllowed(t *testing.T) {
	store := newMockKeyStore(auth.APIKey{
		ID: "obs-all", Vault: "v", Mode: auth.ModeObserve,
	})
	srv := newAuthTestServer(store)

	for _, name := range registeredToolNames() {
		if !isReadOnlyTool(name) {
			continue
		}
		body := mkToolCallBody(name, map[string]any{"vault": "v", "context": "x", "id": "x", "concept": "x", "content": "y", "entity_name": "x", "query": []string{"x"}})
		w := doAuthenticatedPost(srv, "mk_obs-all", body)
		var resp JSONRPCResponse
		json.NewDecoder(w.Body).Decode(&resp)
		if resp.Error != nil && strings.Contains(resp.Error.Message, "forbidden") {
			t.Errorf("observe-mode key should be allowed to call read tool %s, got: %s", name, resp.Error.Message)
		}
	}
}

func TestDispatch_ObserveMode_AllMutatingToolsBlocked(t *testing.T) {
	store := newMockKeyStore(auth.APIKey{
		ID: "obs-block", Vault: "v", Mode: auth.ModeObserve,
	})
	srv := newAuthTestServer(store)

	for _, name := range registeredToolNames() {
		if !isMutatingTool(name) {
			continue
		}
		body := mkToolCallBody(name, map[string]any{"vault": "v", "concept": "x", "content": "y", "id": "x"})
		w := doAuthenticatedPost(srv, "mk_obs-block", body)
		var resp JSONRPCResponse
		json.NewDecoder(w.Body).Decode(&resp)
		if resp.Error == nil || !strings.Contains(resp.Error.Message, "forbidden") {
			t.Errorf("observe-mode key should be blocked from mutating tool %s", name)
		}
	}
}

func TestDispatch_WriteMode_AllMutatingToolsAllowed(t *testing.T) {
	store := newMockKeyStore(auth.APIKey{
		ID: "wrt-all", Vault: "v", Mode: auth.ModeWrite,
	})
	srv := newAuthTestServer(store)

	for _, name := range registeredToolNames() {
		if !isMutatingTool(name) {
			continue
		}
		body := mkToolCallBody(name, map[string]any{"vault": "v", "concept": "x", "content": "y", "id": "x", "ids": []string{"x"}, "merged_content": "y", "decision": "x", "rationale": "y"})
		w := doAuthenticatedPost(srv, "mk_wrt-all", body)
		var resp JSONRPCResponse
		json.NewDecoder(w.Body).Decode(&resp)
		if resp.Error != nil && strings.Contains(resp.Error.Message, "forbidden") {
			t.Errorf("write-mode key should be allowed to call mutating tool %s, got: %s", name, resp.Error.Message)
		}
	}
}

func TestDispatch_WriteMode_AllReadToolsBlocked(t *testing.T) {
	store := newMockKeyStore(auth.APIKey{
		ID: "wrt-block", Vault: "v", Mode: auth.ModeWrite,
	})
	srv := newAuthTestServer(store)

	for _, name := range registeredToolNames() {
		if !isReadOnlyTool(name) {
			continue
		}
		body := mkToolCallBody(name, map[string]any{"vault": "v", "context": "x", "id": "x"})
		w := doAuthenticatedPost(srv, "mk_wrt-block", body)
		var resp JSONRPCResponse
		json.NewDecoder(w.Body).Decode(&resp)
		if resp.Error == nil || !strings.Contains(resp.Error.Message, "forbidden") {
			t.Errorf("write-mode key should be blocked from read tool %s", name)
		}
	}
}

// --- Unknown mode handling ---

func TestDispatch_UnknownMode_Rejected(t *testing.T) {
	store := newMockKeyStore(auth.APIKey{
		ID: "unk001", Vault: "v", Mode: "bogus",
	})
	srv := newAuthTestServer(store)
	body := mkToolCallBody("muninn_recall", map[string]any{"vault": "v", "context": "x"})
	w := doAuthenticatedPost(srv, "mk_unk001", body)
	var resp JSONRPCResponse
	json.NewDecoder(w.Body).Decode(&resp)
	if resp.Error == nil || !strings.Contains(resp.Error.Message, "forbidden") {
		t.Error("unknown key mode should be rejected (fail-closed)")
	}
}

// --- SSE message auth re-validation ---

func TestSSEMessage_RequiresAuth_WhenServerHasToken(t *testing.T) {
	srv := New(":0", &fakeEngine{}, "mdb_secret", nil, nil)

	// Insert a fake SSE session
	srv.sseSessionsMu.Lock()
	srv.sseSessions["sess123"] = &sseSession{
		ch:   make(chan []byte, 4),
		auth: AuthContext{Token: "mdb_secret", Authorized: true},
	}
	srv.sseSessionsMu.Unlock()

	// POST to /mcp/message WITHOUT auth header
	body := mkToolCallBody("muninn_recall", map[string]any{"context": "x"})
	r := httptest.NewRequest(http.MethodPost, "/mcp/message?sessionId=sess123", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleSSEMessage(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 for SSE message without auth, got %d", w.Code)
	}
}

// --- findSSEChannelsByToken with empty token ---

func TestFindSSEChannelsByToken_EmptyToken_ReturnsNil(t *testing.T) {
	srv := New(":0", &fakeEngine{}, "", nil, nil)

	srv.sseSessionsMu.Lock()
	srv.sseSessions["s1"] = &sseSession{ch: make(chan []byte, 4), auth: AuthContext{Token: ""}}
	srv.sseSessions["s2"] = &sseSession{ch: make(chan []byte, 4), auth: AuthContext{Token: ""}}
	srv.sseSessionsMu.Unlock()

	channels := srv.findSSEChannelsByToken("")
	if len(channels) != 0 {
		t.Errorf("empty token should return no channels (prevents cross-session contamination), got %d", len(channels))
	}
}

// --- Error message consistency ---

func TestErrorMessages_NoInternalStateLeakage(t *testing.T) {
	store := newMockKeyStore(auth.APIKey{
		ID: "leak001", Vault: "secret-vault", Mode: auth.ModeObserve,
	})
	srv := newAuthTestServer(store)

	// Observe-mode blocked from mutating tool — error should not leak vault name
	body := mkToolCallBody("muninn_remember", map[string]any{"vault": "secret-vault", "concept": "x", "content": "y"})
	w := doAuthenticatedPost(srv, "mk_leak001", body)
	var resp JSONRPCResponse
	json.NewDecoder(w.Body).Decode(&resp)
	if resp.Error == nil {
		t.Fatal("expected forbidden error")
	}
	// Tool name leakage in error is acceptable (client already knows what they called).
	// Vault name leakage is NOT acceptable.
	if strings.Contains(resp.Error.Message, "secret-vault") {
		t.Errorf("error message should not leak vault name, got: %s", resp.Error.Message)
	}
}

// --- vaultFromArgs 3-return values ---

func TestVaultFromArgs_AbsentKey_Returns_FalseFalse(t *testing.T) {
	v, present, invalid := vaultFromArgs(map[string]any{"context": "x"})
	if v != "" || present || invalid {
		t.Errorf("absent vault key: expected ('', false, false), got (%q, %v, %v)", v, present, invalid)
	}
}

func TestVaultFromArgs_ValidName_Returns_TrueFalse(t *testing.T) {
	v, present, invalid := vaultFromArgs(map[string]any{"vault": "my-vault"})
	if v != "my-vault" || !present || invalid {
		t.Errorf("valid vault: expected ('my-vault', true, false), got (%q, %v, %v)", v, present, invalid)
	}
}
