package mcp

import (
	"context"
	"encoding/json"
	"testing"
)

type feedbackEngine struct{ fakeEngine }

func (e *feedbackEngine) RecordFeedback(_ context.Context, _, _ string, _ bool) error {
	return nil
}

func TestHandleFeedback_HappyPath(t *testing.T) {
	srv := New(":0", &feedbackEngine{}, "", nil)
	w := postRPC(t, srv, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"muninn_feedback","arguments":{"vault":"default","engram_id":"01HXYZ","useful":false}}}`)
	if w.Code != 200 {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp JSONRPCResponse
	json.NewDecoder(w.Body).Decode(&resp)
	if resp.Error != nil {
		t.Fatalf("unexpected error: %v", resp.Error)
	}
}

func TestHandleFeedback_DefaultVault(t *testing.T) {
	// Vault is optional — omitting it defaults to "default".
	srv := New(":0", &feedbackEngine{}, "", nil)
	w := postRPC(t, srv, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"muninn_feedback","arguments":{"engram_id":"01HXYZ"}}}`)
	if w.Code != 200 {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp JSONRPCResponse
	json.NewDecoder(w.Body).Decode(&resp)
	if resp.Error != nil {
		t.Fatalf("unexpected error: %v", resp.Error)
	}
}

func TestHandleFeedback_MissingEngramID(t *testing.T) {
	srv := newTestServer()
	w := postRPC(t, srv, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"muninn_feedback","arguments":{"vault":"default"}}}`)
	var resp JSONRPCResponse
	json.NewDecoder(w.Body).Decode(&resp)
	if resp.Error == nil {
		t.Fatal("expected error for missing engram_id")
	}
}
