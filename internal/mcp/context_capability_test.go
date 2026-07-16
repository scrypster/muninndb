package mcp

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/auth"
)

// stubCapStore satisfies capabilityValidator without a live Pebble store.
type stubCapStore struct {
	cap auth.Capability
	err error
}

func (s stubCapStore) ValidateCapability(token string) (auth.Capability, error) {
	return s.cap, s.err
}

func TestAuthFromRequest_CapToken(t *testing.T) {
	exp := time.Now().Add(time.Hour)
	stub := stubCapStore{cap: auth.Capability{Vault: "wf-x", Mode: auth.ModeFull, ExpiresAt: &exp}}
	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	req.Header.Set("Authorization", "Bearer cap_abc")
	a := authFromRequest(req, "", nil, stub)
	if !a.Authorized || !a.IsCapability || a.IsAPIKey || a.Vault != "wf-x" || a.Mode != auth.ModeFull {
		t.Errorf("cap auth resolved wrong: %+v", a)
	}
}

func TestAuthFromRequest_InvalidCapFailClosed(t *testing.T) {
	stub := stubCapStore{err: errors.New("nope")}
	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	req.Header.Set("Authorization", "Bearer cap_abc")
	a := authFromRequest(req, "", nil, stub)
	if a.Authorized {
		t.Error("invalid cap_ token must not fall through to open-server mode")
	}
}

// TestDispatch_ObserveModeCapability_BlocksMutatingTool mirrors
// TestDispatch_ObserveMode_BlocksMutatingTool (the mk_ variant in
// auth_mk_test.go) but authenticates with a cap_ capability token
// (IsCapability=true, IsAPIKey=false). It pins down that mode enforcement in
// dispatchToolCall fires for capability bearers, not just mk_ API keys.
func TestDispatch_ObserveModeCapability_BlocksMutatingTool(t *testing.T) {
	exp := time.Now().Add(time.Hour)
	stub := stubCapStore{cap: auth.Capability{
		Vault:     "wf-x",
		Mode:      auth.ModeObserve,
		ExpiresAt: &exp,
	}}
	eng := &fakeEngine{}
	srv := New(":0", eng, "", nil, stub, nil)

	body := mkToolCallBody("muninn_remember", map[string]any{"vault": "wf-x", "concept": "x", "content": "y"})
	w := doAuthenticatedPost(srv, "cap_obs-block", body)

	var resp JSONRPCResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if resp.Error == nil {
		t.Fatal("expected error response for observe-mode capability calling mutating tool")
	}
	if !strings.Contains(resp.Error.Message, "forbidden") {
		t.Errorf("expected 'forbidden' in error, got: %s", resp.Error.Message)
	}
}
