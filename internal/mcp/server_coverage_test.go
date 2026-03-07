package mcp

// server_coverage_test.go — additional tests for server.go, context.go,
// and convert.go to push internal/mcp coverage toward 75%.

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/scrypster/muninndb/internal/auth"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// ── handleRPC paths: initialize, notifications, ping ─────────────────────────

func TestHandleRPC_Initialize(t *testing.T) {
	srv := newTestServer()
	body := `{"jsonrpc":"2.0","method":"initialize","id":1,"params":{}}`
	w := postRPC(t, srv, body)
	if got := w.Header().Get(mcpSessionHeader); got == "" {
		t.Fatal("initialize response missing Mcp-Session-Id header")
	}
	resp := decodeResp(t, w.Body.String())
	if resp.Error != nil {
		t.Fatalf("unexpected error: %v", resp.Error)
	}
	result, ok := resp.Result.(map[string]any)
	if !ok {
		t.Fatalf("expected object result, got %T", resp.Result)
	}
	if _, ok := result["protocolVersion"]; !ok {
		t.Error("response missing 'protocolVersion'")
	}
}

func TestHandleRPC_Ping(t *testing.T) {
	srv := newTestServer()
	body := `{"jsonrpc":"2.0","method":"ping","id":1}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error != nil {
		t.Fatalf("unexpected error from ping: %v", resp.Error)
	}
}

func TestHandleRPC_ToolsList(t *testing.T) {
	srv := newTestServer()
	body := `{"jsonrpc":"2.0","method":"tools/list","id":1}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error != nil {
		t.Fatalf("unexpected error from tools/list: %v", resp.Error)
	}
	result, ok := resp.Result.(map[string]any)
	if !ok {
		t.Fatalf("expected object result, got %T", resp.Result)
	}
	if _, ok := result["tools"]; !ok {
		t.Error("tools/list response missing 'tools'")
	}
}

func TestHandleRPC_Notifications(t *testing.T) {
	// MCP Streamable HTTP: notifications/ must return 202 Accepted with no body.
	srv := newTestServer()
	body := `{"jsonrpc":"2.0","method":"notifications/initialized","id":1}`
	w := postRPC(t, srv, body)
	if w.Code != http.StatusAccepted {
		t.Errorf("notifications should return 202, got %d", w.Code)
	}
}

// ── dispatchToolCall: params nil and nil arguments ────────────────────────────

func TestDispatchToolCall_NilParams(t *testing.T) {
	// A tools/call request with no params should return -32602.
	srv := newTestServer()
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error == nil || resp.Error.Code != -32602 {
		t.Errorf("expected -32602 for nil params, got %v", resp.Error)
	}
}

func TestDispatchToolCall_NilArguments(t *testing.T) {
	// Params present but Arguments nil — should default to empty map and succeed
	// for a tool like muninn_status that has no required params beyond vault.
	srv := newTestServer()
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_status"}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	// muninn_status has no required params beyond vault (which defaults to "default").
	if resp.Error != nil {
		t.Errorf("nil arguments should default to empty map and succeed: %v", resp.Error)
	}
}

func TestDispatchToolCall_InvalidVaultFallsBackToDefault(t *testing.T) {
	// An invalid vault name in args is treated as absent and falls back to "default".
	// This exercises the vaultFromArgs invalid-name path; the call still succeeds.
	srv := newTestServer()
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_status","arguments":{"vault":"INVALID VAULT!"}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	// Should succeed because resolveVault falls back to "default" when vault is invalid.
	if resp.Error != nil {
		t.Errorf("expected fallback to default vault to succeed, got error: %v", resp.Error)
	}
}

// ── withMiddleware: rate limiter and auth ─────────────────────────────────────

func TestWithMiddleware_UnauthorizedRequest(t *testing.T) {
	// Server with a required token — a request without the Bearer token must get 401.
	srv := New(":0", &fakeEngine{}, "secret", nil)
	req := httptest.NewRequest("GET", "/mcp/tools", nil)
	// No Authorization header.
	w := httptest.NewRecorder()
	srv.srv.Handler.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 Unauthorized, got %d", w.Code)
	}
}

func TestWithMiddleware_AuthorizedRequest(t *testing.T) {
	// Correct Bearer token must succeed.
	srv := New(":0", &fakeEngine{}, "secret", nil)
	req := httptest.NewRequest("GET", "/mcp/tools", nil)
	req.Header.Set("Authorization", "Bearer secret")
	w := httptest.NewRecorder()
	srv.srv.Handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("expected 200 OK with correct token, got %d", w.Code)
	}
}

func TestWithMiddleware_ContentLengthTooLarge(t *testing.T) {
	srv := newTestServer()
	req := httptest.NewRequest("GET", "/mcp/tools", nil)
	req.ContentLength = 2 << 20 // 2 MiB > 1 MiB limit
	w := httptest.NewRecorder()
	srv.srv.Handler.ServeHTTP(w, req)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("expected 413 for oversized Content-Length header, got %d", w.Code)
	}
}

// ── handleStreamablePost: auth failure ───────────────────────────────────────

func TestHandleStreamablePost_Unauthorized(t *testing.T) {
	srv := New(":0", &fakeEngine{}, "secret", nil)
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_status","arguments":{"vault":"default"}}}`
	req := httptest.NewRequest("POST", "/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	// Wrong token.
	req.Header.Set("Authorization", "Bearer wrong")
	w := httptest.NewRecorder()
	srv.srv.Handler.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 for wrong token, got %d", w.Code)
	}
}

// ── context.go: isValidVaultName edge cases ───────────────────────────────────

func TestIsValidVaultName_TooLong(t *testing.T) {
	// 65-char lowercase vault name must be invalid.
	name := strings.Repeat("a", 65)
	if isValidVaultName(name) {
		t.Error("expected isValidVaultName to return false for 65-char name")
	}
}

func TestIsValidVaultName_MaxLength(t *testing.T) {
	// 64-char lowercase vault name must be valid.
	name := strings.Repeat("a", 64)
	if !isValidVaultName(name) {
		t.Error("expected isValidVaultName to return true for 64-char name")
	}
}

func TestIsValidVaultName_Empty(t *testing.T) {
	if isValidVaultName("") {
		t.Error("expected isValidVaultName to return false for empty string")
	}
}

func TestIsValidVaultName_Uppercase(t *testing.T) {
	if isValidVaultName("MyVault") {
		t.Error("expected isValidVaultName to return false for uppercase chars")
	}
}

func TestIsValidVaultName_ValidChars(t *testing.T) {
	cases := []string{"default", "my-vault", "vault_1", "a0-b_c"}
	for _, name := range cases {
		if !isValidVaultName(name) {
			t.Errorf("expected isValidVaultName to return true for %q", name)
		}
	}
}

func TestVaultFromArgs_NonStringType(t *testing.T) {
	// vault value that is not a string should return ("", false).
	args := map[string]any{"vault": 42}
	v, ok := vaultFromArgs(args)
	if ok || v != "" {
		t.Errorf("expected ('', false) for non-string vault, got (%q, %v)", v, ok)
	}
}

func TestVaultFromArgs_EmptyString(t *testing.T) {
	args := map[string]any{"vault": ""}
	v, ok := vaultFromArgs(args)
	if ok || v != "" {
		t.Errorf("expected ('', false) for empty vault string, got (%q, %v)", v, ok)
	}
}

func TestVaultFromArgs_InvalidName(t *testing.T) {
	// Vault name with invalid characters returns ("", false).
	args := map[string]any{"vault": "INVALID!"}
	v, ok := vaultFromArgs(args)
	if ok || v != "" {
		t.Errorf("expected ('', false) for invalid vault name, got (%q, %v)", v, ok)
	}
}

// ── resolveVault with session ─────────────────────────────────────────────────

func TestResolveVault_SessionPinned_ArgAbsent(t *testing.T) {
	sess := &mcpSession{vault: "work"}
	vault, errMsg := resolveVault(sess, map[string]any{})
	if errMsg != "" {
		t.Errorf("expected no error, got: %s", errMsg)
	}
	if vault != "work" {
		t.Errorf("expected session vault 'work', got %q", vault)
	}
}

func TestResolveVault_SessionPinned_ArgMatches(t *testing.T) {
	sess := &mcpSession{vault: "work"}
	vault, errMsg := resolveVault(sess, map[string]any{"vault": "work"})
	if errMsg != "" {
		t.Errorf("expected no error, got: %s", errMsg)
	}
	if vault != "work" {
		t.Errorf("expected session vault 'work', got %q", vault)
	}
}

func TestResolveVault_SessionPinned_ArgMismatch(t *testing.T) {
	sess := &mcpSession{vault: "work"}
	_, errMsg := resolveVault(sess, map[string]any{"vault": "personal"})
	if errMsg == "" {
		t.Error("expected vault mismatch error, got empty")
	}
	if !strings.Contains(errMsg, "vault mismatch") {
		t.Errorf("error should mention 'vault mismatch', got: %s", errMsg)
	}
}

func TestResolveVault_NoSession_NoArg(t *testing.T) {
	vault, errMsg := resolveVault(nil, map[string]any{})
	if errMsg != "" {
		t.Errorf("expected no error, got: %s", errMsg)
	}
	if vault != "default" {
		t.Errorf("expected 'default', got %q", vault)
	}
}

// ── convert.go: readResponseToMemory long content ─────────────────────────────

// TestReadResponseToMemory_LongContent verifies that muninn_read returns the
// full content without truncation — issue #112 behavior change.
func TestReadResponseToMemory_LongContent(t *testing.T) {
	longContent := strings.Repeat("x", 501)
	resp := &mbp.ReadResponse{
		ID:      "test-id",
		Content: longContent,
	}
	mem := readResponseToMemory(resp)
	if len(mem.Content) != 501 {
		t.Errorf("readResponseToMemory should return full content: got len=%d, want 501", len(mem.Content))
	}
	if strings.HasSuffix(mem.Content, "...") {
		t.Error("readResponseToMemory must not truncate content (issue #112)")
	}
}

// ── handleGuide: GetVaultPlasticity fallback ──────────────────────────────────

// plasticityErrEngine returns an error from GetVaultPlasticity but success from Stat.
type plasticityErrEngine struct{ fakeEngine }

func (e *plasticityErrEngine) GetVaultPlasticity(_ context.Context, _ string) (*auth.ResolvedPlasticity, error) {
	return nil, fmt.Errorf("plasticity config unavailable")
}

func TestHandleGuide_PlasticityFallback(t *testing.T) {
	// When GetVaultPlasticity fails, handleGuide falls back to defaults and still succeeds.
	srv := newTestServerWith(&plasticityErrEngine{})
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_guide","arguments":{"vault":"default"}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error != nil {
		t.Fatalf("expected success on plasticity fallback, got error: %v", resp.Error)
	}
}

// ── handleListDeleted: nil-to-empty slice normalization ───────────────────────

// nilDeletedEngine returns (nil, nil) from ListDeleted.
type nilDeletedEngine struct{ fakeEngine }

func (e *nilDeletedEngine) ListDeleted(_ context.Context, _ string, _ int) ([]DeletedEngram, error) {
	return nil, nil
}

func TestHandleListDeleted_NilSliceNormalized(t *testing.T) {
	// Engine returning nil deleted list must result in response with empty array, not null.
	srv := newTestServerWith(&nilDeletedEngine{})
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_list_deleted","arguments":{"vault":"default"}}}`
	w := postRPC(t, srv, body)
	content := extractInnerJSON(t, decodeResp(t, w.Body.String()))

	deleted, ok := content["deleted"].([]any)
	if !ok {
		t.Fatal("expected 'deleted' to be a JSON array")
	}
	if len(deleted) != 0 {
		t.Errorf("expected empty deleted array, got %d items", len(deleted))
	}
	count, _ := content["count"].(float64)
	if int(count) != 0 {
		t.Errorf("expected count=0, got %v", count)
	}
}

// ── sessionFromRequest ─────────────────────────────────────────────────────────

// fakeSessionStore is a minimal sessionStore for testing sessionFromRequest.
type fakeSessionStore struct {
	sessions map[string]*mcpSession
}

func newFakeSessionStore() *fakeSessionStore {
	return &fakeSessionStore{sessions: make(map[string]*mcpSession)}
}

func (s *fakeSessionStore) Get(id string) (*mcpSession, bool) {
	sess, ok := s.sessions[id]
	return sess, ok
}

func (s *fakeSessionStore) Create(_ string, _ [32]byte) (string, error) { return "", nil }
func (s *fakeSessionStore) Touch(_ string)                              {}
func (s *fakeSessionStore) MarkInitialized(_ string) error              { return nil }
func (s *fakeSessionStore) ByVault(_ string) []*mcpSession              { return nil }
func (s *fakeSessionStore) DroppedCount(_ string) int64                 { return 0 }
func (s *fakeSessionStore) Close()                                      {}

func TestSessionFromRequest_NoHeader(t *testing.T) {
	store := newFakeSessionStore()
	req, _ := http.NewRequest("POST", "/mcp", nil)
	sess, id := sessionFromRequest(req, store)
	if sess != nil || id != "" {
		t.Errorf("expected (nil, '') with no header, got (%v, %q)", sess, id)
	}
}

func TestSessionFromRequest_UnknownSessionID(t *testing.T) {
	store := newFakeSessionStore()
	req, _ := http.NewRequest("POST", "/mcp", nil)
	req.Header.Set(mcpSessionHeader, "nonexistent-id")
	sess, id := sessionFromRequest(req, store)
	if sess != nil {
		t.Error("expected nil session for unknown session ID")
	}
	if id != "nonexistent-id" {
		t.Errorf("expected session ID to be returned, got %q", id)
	}
}

func TestSessionFromRequest_ValidSession(t *testing.T) {
	store := newFakeSessionStore()
	store.sessions["test-session"] = &mcpSession{vault: "default"}
	req, _ := http.NewRequest("POST", "/mcp", nil)
	req.Header.Set(mcpSessionHeader, "test-session")
	sess, id := sessionFromRequest(req, store)
	if sess == nil {
		t.Error("expected non-nil session for known session ID")
	}
	if id != "test-session" {
		t.Errorf("expected session ID 'test-session', got %q", id)
	}
}

// ── validateSessionToken ───────────────────────────────────────────────────────

func TestValidateSessionToken_Matching(t *testing.T) {
	token := "my-secret-token"
	h := sha256.Sum256([]byte(token))
	sess := &mcpSession{tokenHash: h}
	if msg := validateSessionToken(sess, token); msg != "" {
		t.Errorf("expected empty error for matching token, got: %s", msg)
	}
}

func TestValidateSessionToken_Mismatch(t *testing.T) {
	token := "my-secret-token"
	h := sha256.Sum256([]byte(token))
	sess := &mcpSession{tokenHash: h}
	if msg := validateSessionToken(sess, "wrong-token"); msg == "" {
		t.Error("expected error for mismatched token, got empty")
	}
}

// ── applyEnrichmentArgs: relationship weight clamping ─────────────────────────

func TestApplyEnrichmentArgs_WeightClampedBelow(t *testing.T) {
	args := map[string]any{
		"relationships": []any{
			map[string]any{"target_id": "01ABC", "relation": "supports", "weight": -0.5},
		},
	}
	req := &mbp.WriteRequest{}
	applyEnrichmentArgs(args, req)
	if len(req.Relationships) != 1 {
		t.Fatalf("expected 1 relationship, got %d", len(req.Relationships))
	}
	if req.Relationships[0].Weight != 0 {
		t.Errorf("weight < 0 should be clamped to 0, got %v", req.Relationships[0].Weight)
	}
}

func TestApplyEnrichmentArgs_WeightClampedAbove(t *testing.T) {
	args := map[string]any{
		"relationships": []any{
			map[string]any{"target_id": "01ABC", "relation": "supports", "weight": 1.5},
		},
	}
	req := &mbp.WriteRequest{}
	applyEnrichmentArgs(args, req)
	if len(req.Relationships) != 1 {
		t.Fatalf("expected 1 relationship, got %d", len(req.Relationships))
	}
	if req.Relationships[0].Weight != 1 {
		t.Errorf("weight > 1 should be clamped to 1, got %v", req.Relationships[0].Weight)
	}
}
