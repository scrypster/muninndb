package mcp

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cockroachdb/pebble"
	"github.com/cockroachdb/pebble/vfs"
	"github.com/scrypster/muninndb/internal/auth"
)

// newWorkflowTestStore builds an *auth.Store over an in-memory Pebble DB for
// create-workflow-vault tests. The store satisfies both apiKeyValidator and
// capabilityValidator (and is the concrete type the handler needs for
// SetVaultConfig + GenerateCapability).
func newWorkflowTestStore(t *testing.T) *auth.Store {
	t.Helper()
	db, err := pebble.Open("", &pebble.Options{FS: vfs.NewMem()})
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return auth.NewStore(db)
}

// decodeRPCResult decodes a successful JSON-RPC response's result map.
func decodeRPCResult(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var resp JSONRPCResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v\nbody: %s", err, w.Body.String())
	}
	if resp.Error != nil {
		t.Fatalf("unexpected error: code=%d msg=%s", resp.Error.Code, resp.Error.Message)
	}
	m, ok := resp.Result.(map[string]any)
	if !ok {
		t.Fatalf("result is not a map: %T", resp.Result)
	}
	return m
}

// assertRPCError asserts the response is a JSON-RPC error with wantCode and a
// message containing wantMsgContains. A non-empty wantMsgContains distinguishes
// a guard-layer rejection from an auth-layer "unauthorized" — critical for the
// recursion-guard proof.
func assertRPCError(t *testing.T, w *httptest.ResponseRecorder, wantCode int, wantMsgContains string) {
	t.Helper()
	var resp JSONRPCResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v\nbody: %s", err, w.Body.String())
	}
	if resp.Error == nil {
		t.Fatalf("expected error code %d, got success result: %v", wantCode, resp.Result)
	}
	if resp.Error.Code != wantCode {
		t.Errorf("error code = %d, want %d (msg: %s)", resp.Error.Code, wantCode, resp.Error.Message)
	}
	if wantMsgContains != "" && !strings.Contains(resp.Error.Message, wantMsgContains) {
		t.Errorf("error message %q does not contain %q", resp.Error.Message, wantMsgContains)
	}
}

// TestCreateWorkflowVault_OptInOff verifies the tool is disabled when
// MUNINN_AGENT_VAULT_CREATE is unset (secure-by-default). Even a full-mode mk_
// key caller is rejected with -32001 "disabled".
func TestCreateWorkflowVault_OptInOff(t *testing.T) {
	store := newWorkflowTestStore(t)
	mkToken, _, err := store.GenerateAPIKey("admin", "admin", auth.ModeFull, nil)
	if err != nil {
		t.Fatal(err)
	}
	srv := New(":0", &fakeEngine{}, "", store, store, nil)
	srv.agentVaultCreate = false // opt-in OFF (also the default)

	body := mkToolCallBody("muninn_create_workflow_vault", map[string]any{})
	w := doAuthenticatedPost(srv, mkToken, body)
	assertRPCError(t, w, -32001, "disabled")
}

// TestCreateWorkflowVault_RecursionGuard_CapCallerRejected is the CRITICAL
// security test for RFC #597. A legitimate cap_ capability token — exactly the
// kind the tool itself mints — authenticates successfully (IsCapability=true,
// IsAPIKey=false) yet MUST be rejected by the recursion guard. This proves the
// structural fix: no capability can mint further vaults/capabilities, because
// the guard gates on IsAPIKey, which capabilities never satisfy.
//
// The assertion on the guard's specific message ("full-mode mk_ key") — rather
// than a generic "unauthorized" — proves the cap_ token passed authentication
// and was rejected by the privileged-tool guard, not the auth layer.
func TestCreateWorkflowVault_RecursionGuard_CapCallerRejected(t *testing.T) {
	store := newWorkflowTestStore(t)
	// Mint a real, valid, full-mode cap_ token against an existing workflow vault.
	exp := time.Now().Add(time.Hour)
	capToken, _, err := store.GenerateCapability("wf-existing", "worker", auth.ModeFull, "workflow_vault", &exp)
	if err != nil {
		t.Fatal(err)
	}

	srv := New(":0", &fakeEngine{}, "", store, store, nil)
	srv.agentVaultCreate = true // opt-in ON so the ONLY gate is the IsAPIKey check

	body := mkToolCallBody("muninn_create_workflow_vault", map[string]any{})
	w := doAuthenticatedPost(srv, capToken, body)
	assertRPCError(t, w, -32001, "full-mode mk_ key")
}

// TestCreateWorkflowVault_NonFullKeyRejected verifies that a write-mode mk_
// key — which passes mode enforcement (the tool is mutating) — is still
// rejected by the recursion guard because it requires full-mode specifically.
func TestCreateWorkflowVault_NonFullKeyRejected(t *testing.T) {
	store := newWorkflowTestStore(t)
	wrToken, _, err := store.GenerateAPIKey("admin", "writer", auth.ModeWrite, nil)
	if err != nil {
		t.Fatal(err)
	}
	srv := New(":0", &fakeEngine{}, "", store, store, nil)
	srv.agentVaultCreate = true

	body := mkToolCallBody("muninn_create_workflow_vault", map[string]any{})
	w := doAuthenticatedPost(srv, wrToken, body)
	assertRPCError(t, w, -32001, "full-mode mk_ key")
}

// TestCreateWorkflowVault_HappyPath verifies the end-to-end flow: a full-mode
// mk_ key creates a named workflow vault, the vault is configured with the
// working preset + multi_user, and the returned cap_ token validates as
// full-mode against the new vault.
func TestCreateWorkflowVault_HappyPath(t *testing.T) {
	store := newWorkflowTestStore(t)
	mkToken, _, err := store.GenerateAPIKey("admin", "admin", auth.ModeFull, nil)
	if err != nil {
		t.Fatal(err)
	}
	srv := New(":0", &fakeEngine{}, "", store, store, nil)
	srv.agentVaultCreate = true
	t.Setenv("MUNINN_WORKFLOW_CAP_TTL_HOURS", "") // no env override

	body := mkToolCallBody("muninn_create_workflow_vault", map[string]any{"name": "wf-happy"})
	w := doAuthenticatedPost(srv, mkToken, body)
	result := decodeRPCResult(t, w)

	if result["vault"] != "wf-happy" {
		t.Errorf("vault = %v, want wf-happy", result["vault"])
	}
	capSecret, _ := result["capability_secret"].(string)
	if capSecret == "" {
		t.Fatal("capability_secret empty — must be shown once")
	}
	if !strings.HasPrefix(capSecret, "cap_") {
		t.Errorf("capability_secret = %q, want cap_ prefix", capSecret)
	}

	// The minted token validates against the same store (full-mode, new vault).
	cap, err := store.ValidateCapability(capSecret)
	if err != nil {
		t.Fatalf("minted cap_ token failed validation: %v", err)
	}
	if cap.Vault != "wf-happy" {
		t.Errorf("cap vault = %s, want wf-happy", cap.Vault)
	}
	if cap.Mode != auth.ModeFull {
		t.Errorf("cap mode = %s, want %s", cap.Mode, auth.ModeFull)
	}

	// Vault config: working preset + multi_user enabled.
	cfg, err := store.GetVaultConfig("wf-happy")
	if err != nil {
		t.Fatalf("get vault config: %v", err)
	}
	if cfg.Plasticity == nil || cfg.Plasticity.Preset != "working" {
		t.Errorf("preset = %v, want working", cfg.Plasticity)
	}
	if cfg.Plasticity == nil || cfg.Plasticity.MultiUser == nil || !*cfg.Plasticity.MultiUser {
		t.Errorf("multi_user not enabled: %+v", cfg.Plasticity)
	}
}

// TestCreateWorkflowVault_TTLHonored verifies the ttl_hours arg flows through
// to the minted capability's ExpiresAt. With ttl_hours=1 and no env override,
// the cap expires ~1h from now and validates successfully (not yet expired).
func TestCreateWorkflowVault_TTLHonored(t *testing.T) {
	store := newWorkflowTestStore(t)
	mkToken, _, err := store.GenerateAPIKey("admin", "admin", auth.ModeFull, nil)
	if err != nil {
		t.Fatal(err)
	}
	srv := New(":0", &fakeEngine{}, "", store, store, nil)
	srv.agentVaultCreate = true
	t.Setenv("MUNINN_WORKFLOW_CAP_TTL_HOURS", "") // arg, not env, drives the TTL

	before := time.Now()
	body := mkToolCallBody("muninn_create_workflow_vault", map[string]any{
		"name":      "wf-ttl",
		"ttl_hours": float64(1), // JSON numbers arrive as float64
	})
	w := doAuthenticatedPost(srv, mkToken, body)
	result := decodeRPCResult(t, w)

	capSecret, _ := result["capability_secret"].(string)
	cap, err := store.ValidateCapability(capSecret)
	if err != nil {
		t.Fatalf("validate cap: %v", err)
	}
	if cap.ExpiresAt == nil {
		t.Fatal("minted cap has nil ExpiresAt — TTL not applied")
	}
	want := before.Add(time.Hour)
	got := *cap.ExpiresAt
	if tol := 5 * time.Minute; got.Sub(want).Abs() > tol {
		t.Errorf("ttl expiry = %v, want ~%v (tol %v)", got, want, tol)
	}
}

// TestCreateWorkflowVault_AutoName verifies that omitting "name" auto-generates
// a wf-<8hex> vault name and the flow still succeeds.
func TestCreateWorkflowVault_AutoName(t *testing.T) {
	store := newWorkflowTestStore(t)
	mkToken, _, err := store.GenerateAPIKey("admin", "admin", auth.ModeFull, nil)
	if err != nil {
		t.Fatal(err)
	}
	srv := New(":0", &fakeEngine{}, "", store, store, nil)
	srv.agentVaultCreate = true
	t.Setenv("MUNINN_WORKFLOW_CAP_TTL_HOURS", "")

	body := mkToolCallBody("muninn_create_workflow_vault", map[string]any{})
	w := doAuthenticatedPost(srv, mkToken, body)
	result := decodeRPCResult(t, w)

	name, _ := result["vault"].(string)
	if !strings.HasPrefix(name, "wf-") || len(name) != len("wf-")+8 {
		t.Errorf("auto-name = %q, want wf-<8hex>", name)
	}
}

// TestCreateWorkflowVault_Namespace_RejectsOperatorVault (RedTeam CRITICAL #1):
// a caller-supplied name without the wf- prefix must be rejected even when
// otherwise well-formed. This proves the structural anti-clobber guard: a
// full-mode key scoped to one vault cannot pass name="default" or
// name="production" to overwrite an operator vault's config.
func TestCreateWorkflowVault_Namespace_RejectsOperatorVault(t *testing.T) {
	store := newWorkflowTestStore(t)
	mkToken, _, err := store.GenerateAPIKey("admin", "admin", auth.ModeFull, nil)
	if err != nil {
		t.Fatal(err)
	}
	srv := New(":0", &fakeEngine{}, "", store, store, nil)
	srv.agentVaultCreate = true

	cases := []struct {
		name  string
		vault string
	}{
		{"default", "default"},
		{"production", "production"},
		{"missing-prefix", "my-vault"},
		{"prefix-only", "wf-"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := mkToolCallBody("muninn_create_workflow_vault", map[string]any{"name": tc.vault})
			w := doAuthenticatedPost(srv, mkToken, body)
			assertRPCError(t, w, -32602, "wf-")
		})
	}
}

// TestCreateWorkflowVault_RecursionGuard_ViaSSEPost (RedTeam NOTABLE #5):
// exercises the recursion guard through the SSE message path
// (handleSSEMessage → processAndPushSSE → dispatchToolCall), a different
// production dispatch route than doAuthenticatedPost (which hits handleRPC).
// A cap_ bearer opening an SSE session and then POSTing
// muninn_create_workflow_vault must be rejected by the IsAPIKey guard inside
// dispatchToolCall — proving the guard fires in real SSE routing.
func TestCreateWorkflowVault_RecursionGuard_ViaSSEPost(t *testing.T) {
	store := newWorkflowTestStore(t)
	exp := time.Now().Add(time.Hour)
	capToken, _, err := store.GenerateCapability("wf-existing", "worker", auth.ModeFull, "workflow_vault", &exp)
	if err != nil {
		t.Fatal(err)
	}

	srv := New(":0", &fakeEngine{}, "", store, store, nil)
	srv.agentVaultCreate = true

	// Simulate an SSE session opened with the cap_ token: register the session
	// the way handleSSE does, caching the cap_ auth context.
	srv.sseSessionsMu.Lock()
	srv.sseSessions["sess-cap"] = &sseSession{
		ch:   make(chan []byte, 4),
		auth: AuthContext{Token: capToken, Authorized: true, Vault: "wf-existing", Mode: auth.ModeFull, IsCapability: true},
	}
	srv.sseSessionsMu.Unlock()

	body := mkToolCallBody("muninn_create_workflow_vault", map[string]any{})
	r := httptest.NewRequest(http.MethodPost, "/mcp/message?sessionId=sess-cap", bytes.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+capToken)
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleSSEMessage(w, r)

	// The SSE POST writes the JSON-RPC response into both the POST body and the
	// SSE channel. The POST body carries the dispatch result — assert the
	// recursion guard fired with the IsAPIKey-specific message.
	var resp JSONRPCResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v\nbody: %s", err, w.Body.String())
	}
	if resp.Error == nil {
		t.Fatalf("expected recursion-guard rejection, got success result: %v", resp.Result)
	}
	if resp.Error.Code != -32001 {
		t.Errorf("error code = %d, want -32001 (msg: %s)", resp.Error.Code, resp.Error.Message)
	}
	if !strings.Contains(resp.Error.Message, "full-mode mk_ key") {
		t.Errorf("error message %q does not contain 'full-mode mk_ key' — guard may not have fired", resp.Error.Message)
	}
}

// TestSSEMessage_CapRevokedMidSession_RejectedOnPost (RedTeam CRITICAL #2 + #5):
// proves the per-POST re-validation of cached SSE credentials. Opens an SSE
// session with a valid cap_, then revokes the cap_ mid-session, then POSTs
// WITHOUT re-sending the bearer token — simulating the MCP SSE client pattern
// where the bearer is sent only on the GET (SSE open) and subsequent POSTs
// rely on the session ID. Without Fix 2 the POST passes (authFromRequest
// falls through to open-server mode because there's no static token and no
// bearer on the POST) and dispatches on the cached, now-revoked sess.auth;
// with Fix 2 the POST is rejected because ValidateCapability(sess.auth.Token)
// catches the revocation. This is the RED-sanity test for Fix 2.
func TestSSEMessage_CapRevokedMidSession_RejectedOnPost(t *testing.T) {
	store := newWorkflowTestStore(t)
	exp := time.Now().Add(time.Hour)
	capToken, cap, err := store.GenerateCapability("wf-session", "worker", auth.ModeFull, "workflow_vault", &exp)
	if err != nil {
		t.Fatal(err)
	}

	// Server has NO static token (""), only cap_ auth. This is the deployment
	// shape where the confused-deputy is exploitable without Fix 2: a POST
	// without a bearer falls through authFromRequest to open-server mode.
	srv := New(":0", &fakeEngine{}, "", store, store, nil)
	srv.agentVaultCreate = true

	// Open SSE session with the cap_ token (valid at open time).
	srv.sseSessionsMu.Lock()
	srv.sseSessions["sess-revoke"] = &sseSession{
		ch:   make(chan []byte, 4),
		auth: AuthContext{Token: capToken, Authorized: true, Vault: "wf-session", Mode: auth.ModeFull, IsCapability: true},
	}
	srv.sseSessionsMu.Unlock()

	// Revoke the capability mid-session.
	if err := store.RevokeCapability("wf-session", cap.ID); err != nil {
		t.Fatalf("revoke capability: %v", err)
	}

	// POST WITHOUT Authorization header — simulates the client pattern where
	// the bearer is sent only on the GET. The POST auth check falls through to
	// open-server mode (no static token, no bearer), so only Fix 2's
	// sess.auth re-validation catches the revoked cap_.
	body := mkToolCallBody("muninn_recall", map[string]any{"context": []string{"x"}})
	r := httptest.NewRequest(http.MethodPost, "/mcp/message?sessionId=sess-revoke", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleSSEMessage(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 after cap_ revoked mid-session (POST without bearer), got %d (body: %s)", w.Code, w.Body.String())
	}
}
