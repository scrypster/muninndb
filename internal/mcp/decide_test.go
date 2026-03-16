package mcp

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// ── per-handler engines for decide tests ────────────────────────────────────

// decideErrEngine returns an error from Decide.
type decideErrEngine struct{ fakeEngine }

func (e *decideErrEngine) Decide(_ context.Context, _, _, _ string, _, _ []string) (*WriteResult, error) {
	return nil, fmt.Errorf("decide storage error")
}

// ── muninn_decide ─────────────────────────────────────────────────────────────

func TestHandleDecide_HappyPath(t *testing.T) {
	srv := newTestServer()
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_decide","arguments":{"vault":"default","decision":"Use PostgreSQL for the primary store","rationale":"It handles our write throughput and has good ecosystem support"}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error != nil {
		t.Fatalf("unexpected error: code=%d msg=%s", resp.Error.Code, resp.Error.Message)
	}
	if resp.Result == nil {
		t.Fatal("expected non-nil result")
	}
	// The result must contain an id field.
	content := extractInnerJSON(t, resp)
	if _, ok := content["id"]; !ok {
		t.Error("response missing field: \"id\"")
	}
	if content["id"] == "" {
		t.Error("id field should be non-empty")
	}
}

func TestHandleDecide_MissingDecision(t *testing.T) {
	srv := newTestServer()
	// Omit "decision" — only rationale provided.
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_decide","arguments":{"vault":"default","rationale":"Because it is the best choice"}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error == nil {
		t.Fatal("expected error for missing decision, got nil")
	}
	if resp.Error.Code != -32602 {
		t.Errorf("expected error code -32602 (invalid params), got %d", resp.Error.Code)
	}
	if !strings.Contains(resp.Error.Message, "decision") && !strings.Contains(resp.Error.Message, "rationale") {
		t.Errorf("error message should mention required params, got: %s", resp.Error.Message)
	}
}

func TestHandleDecide_MissingRationale(t *testing.T) {
	srv := newTestServer()
	// Omit "rationale" — only decision provided.
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_decide","arguments":{"vault":"default","decision":"Switch to Redis for caching"}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error == nil {
		t.Fatal("expected error for missing rationale, got nil")
	}
	if resp.Error.Code != -32602 {
		t.Errorf("expected error code -32602 (invalid params), got %d", resp.Error.Code)
	}
}

func TestHandleDecide_EngineError(t *testing.T) {
	srv := newTestServerWith(&decideErrEngine{})
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_decide","arguments":{"vault":"default","decision":"Deploy to prod","rationale":"Tests pass"}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error == nil {
		t.Fatal("expected error from engine, got nil")
	}
	if resp.Error.Code != -32000 {
		t.Errorf("expected error code -32000 (tool error), got %d", resp.Error.Code)
	}
}

func TestHandleDecide_WithOptionalFields(t *testing.T) {
	srv := newTestServer()
	// Include optional alternatives and evidence_ids.
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_decide","arguments":{"vault":"default","decision":"Use gRPC for internal APIs","rationale":"Better performance and strong typing","alternatives":["REST","GraphQL"],"evidence_ids":["e1","e2"]}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error != nil {
		t.Fatalf("unexpected error with optional fields: code=%d msg=%s", resp.Error.Code, resp.Error.Message)
	}
}

// ── authFromRequest tests ─────────────────────────────────────────────────────

func TestAuthFromRequest_ValidToken(t *testing.T) {
	const requiredToken = "secret-token"
	req, _ := http.NewRequest(http.MethodPost, "/mcp", nil)
	req.Header.Set("Authorization", "Bearer "+requiredToken)

	ctx := authFromRequest(req, requiredToken, nil)
	if !ctx.Authorized {
		t.Error("expected Authorized=true for matching token")
	}
	if ctx.Token != requiredToken {
		t.Errorf("expected Token=%q, got %q", requiredToken, ctx.Token)
	}
}

func TestAuthFromRequest_MissingHeader(t *testing.T) {
	const requiredToken = "secret-token"
	req, _ := http.NewRequest(http.MethodPost, "/mcp", nil)
	// No Authorization header.

	ctx := authFromRequest(req, requiredToken, nil)
	if ctx.Authorized {
		t.Error("expected Authorized=false when Authorization header is absent")
	}
}

func TestAuthFromRequest_WrongToken(t *testing.T) {
	const requiredToken = "secret-token"
	req, _ := http.NewRequest(http.MethodPost, "/mcp", nil)
	req.Header.Set("Authorization", "Bearer wrong-token")

	ctx := authFromRequest(req, requiredToken, nil)
	if ctx.Authorized {
		t.Error("expected Authorized=false for wrong token")
	}
}

func TestAuthFromRequest_NoTokenRequired(t *testing.T) {
	// When requiredToken is empty, all requests should be authorized.
	req, _ := http.NewRequest(http.MethodPost, "/mcp", nil)
	// No Authorization header.

	ctx := authFromRequest(req, "", nil)
	if !ctx.Authorized {
		t.Error("expected Authorized=true when no token is required")
	}
}

func TestAuthFromRequest_MalformedHeader(t *testing.T) {
	const requiredToken = "secret-token"
	req, _ := http.NewRequest(http.MethodPost, "/mcp", nil)
	// Missing "Bearer " prefix.
	req.Header.Set("Authorization", "Token secret-token")

	ctx := authFromRequest(req, requiredToken, nil)
	if ctx.Authorized {
		t.Error("expected Authorized=false when Authorization header lacks 'Bearer ' prefix")
	}
}
