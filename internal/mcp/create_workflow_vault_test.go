package mcp

import (
	"encoding/json"
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
