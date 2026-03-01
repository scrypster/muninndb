package mcp

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

type provenanceEngine struct{ fakeEngine }

func (e *provenanceEngine) GetProvenance(_ context.Context, _, _ string) ([]ProvenanceEntry, error) {
	return []ProvenanceEntry{
		{Timestamp: time.Unix(1700000000, 0).UTC().Format(time.RFC3339), Source: "human", AgentID: "", Operation: "write", Note: "initial"},
	}, nil
}

func TestHandleProvenance_HappyPath(t *testing.T) {
	srv := New(":0", &provenanceEngine{}, "", nil)
	w := postRPC(t, srv, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"muninn_provenance","arguments":{"vault":"default","id":"01HXYZ"}}}`)
	if w.Code != 200 {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp JSONRPCResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if resp.Error != nil {
		t.Fatalf("unexpected error: %v", resp.Error)
	}
}

func TestHandleProvenance_MissingVault(t *testing.T) {
	// When vault arg is absent, resolveVault injects "default" — no error expected.
	srv := newTestServer()
	w := postRPC(t, srv, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"muninn_provenance","arguments":{"id":"01HXYZ"}}}`)
	var resp JSONRPCResponse
	json.NewDecoder(w.Body).Decode(&resp)
	// fakeEngine.GetProvenance returns empty slice — should succeed
	if resp.Error != nil {
		t.Fatalf("expected success with default vault injection, got error: %v", resp.Error)
	}
}

func TestHandleProvenance_MissingID(t *testing.T) {
	srv := newTestServer()
	w := postRPC(t, srv, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"muninn_provenance","arguments":{"vault":"default"}}}`)
	var resp JSONRPCResponse
	json.NewDecoder(w.Body).Decode(&resp)
	if resp.Error == nil {
		t.Fatal("expected error for missing id")
	}
}
