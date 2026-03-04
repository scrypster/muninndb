package mcp

import (
	"strings"
	"testing"
)

// TestHandleRemember_MalformedEntityStringWarning verifies that when the caller
// passes entities as plain strings instead of {"name":"...","type":"..."} objects,
// the response Hint warns about malformed entities.
//
// BUG: currently applyEnrichmentArgs silently skips plain-string entities
// (via a type-assert guard that just `continue`s) without surfacing any
// warning to the caller. This test FAILS before the fix.
func TestHandleRemember_MalformedEntityStringWarning(t *testing.T) {
	srv := newTestServer()

	// entities contains a plain string "PostgreSQL" instead of the expected
	// object {"name":"PostgreSQL","type":"database"}.  The handler should
	// accept the call (no error) but warn the caller via the Hint field.
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_remember","arguments":{
		"vault":"default",
		"content":"PostgreSQL chosen for persistence layer",
		"entities":["PostgreSQL"]
	}}}`

	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())

	// The call must succeed (no JSON-RPC error) — malformed entities are not
	// a fatal error, only a warning.
	if resp.Error != nil {
		t.Fatalf("unexpected JSON-RPC error: code=%d msg=%s", resp.Error.Code, resp.Error.Message)
	}

	inner := extractInnerJSON(t, resp)

	// The Hint field must warn the caller about the malformed entity.
	hint, _ := inner["hint"].(string)
	if !strings.Contains(strings.ToLower(hint), "malformed") && !strings.Contains(strings.ToLower(hint), "invalid") {
		t.Errorf("Hint should warn about malformed/invalid entities, got: %q", hint)
	}
}
