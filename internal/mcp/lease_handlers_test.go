package mcp

import "testing"

// The fakeEngine (server_test.go) returns canned values for CompareAndSet/Claim/
// Release, so these tests exercise argument parsing, dispatch routing and
// response shaping for the three lease tools rather than engine behaviour.

func TestHandleCompareAndSetResponseShape(t *testing.T) {
	srv := newTestServer()
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_compare_and_set","arguments":{"vault":"default","id":"abc123","expect_state":"active","set_state":"completed"}}}`
	w := postRPC(t, srv, body)
	content := extractInnerJSON(t, decodeResp(t, w.Body.String()))

	if applied, _ := content["applied"].(bool); !applied {
		t.Errorf("applied should be true, got %v", content["applied"])
	}
	cur, ok := content["current"].(map[string]any)
	if !ok {
		t.Fatalf("expected current object, got %T", content["current"])
	}
	if cur["state"] != "completed" {
		t.Errorf("current.state = %v, want completed", cur["state"])
	}
}

func TestHandleCompareAndSetMissingSetState(t *testing.T) {
	srv := newTestServer()
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_compare_and_set","arguments":{"vault":"default","id":"abc123","expect_state":"active"}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error == nil || resp.Error.Code != -32602 {
		t.Errorf("expected -32602 for missing set_state, got %v", resp.Error)
	}
}

func TestHandleClaimResponseShape(t *testing.T) {
	srv := newTestServer()
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_claim","arguments":{"vault":"default","id":"abc123","owner":"host:sess","ttl_secs":60}}}`
	w := postRPC(t, srv, body)
	content := extractInnerJSON(t, decodeResp(t, w.Body.String()))

	for _, field := range []string{"id", "status", "owner", "heartbeat"} {
		if _, ok := content[field]; !ok {
			t.Errorf("response missing field: %q", field)
		}
	}
	if content["status"] != "acquired" {
		t.Errorf("status = %v, want acquired", content["status"])
	}
	if content["owner"] != "host:sess" {
		t.Errorf("owner = %v, want host:sess", content["owner"])
	}
}

func TestHandleClaimMissingTTL(t *testing.T) {
	srv := newTestServer()
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_claim","arguments":{"vault":"default","id":"abc123","owner":"host:sess"}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error == nil || resp.Error.Code != -32602 {
		t.Errorf("expected -32602 for missing ttl_secs, got %v", resp.Error)
	}
}

func TestHandleReleaseResponseShape(t *testing.T) {
	srv := newTestServer()
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_release","arguments":{"vault":"default","id":"abc123","owner":"host:sess"}}}`
	w := postRPC(t, srv, body)
	content := extractInnerJSON(t, decodeResp(t, w.Body.String()))

	if released, _ := content["released"].(bool); !released {
		t.Errorf("released should be true, got %v", content["released"])
	}
}

func TestHandleReleaseMissingOwner(t *testing.T) {
	srv := newTestServer()
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_release","arguments":{"vault":"default","id":"abc123"}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error == nil || resp.Error.Code != -32602 {
		t.Errorf("expected -32602 for missing owner, got %v", resp.Error)
	}
}
