package mcp

import (
	"context"
	"encoding/json"
	"fmt"
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
	resp := decodeResp(t, w.Body.String())
	if resp.Error != nil {
		t.Fatalf("unexpected error: %v", resp.Error)
	}
	content := extractInnerJSON(t, resp)
	// The response must echo the requested engram ID.
	if content["id"] != "01HXYZ" {
		t.Errorf("provenance response id = %v, want 01HXYZ", content["id"])
	}
	// The entries array must be present and non-empty (provenanceEngine returns 1 entry).
	entries, ok := content["entries"].([]any)
	if !ok {
		t.Fatalf("provenance entries should be an array, got %T", content["entries"])
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 provenance entry, got %d", len(entries))
	}
	entry, ok := entries[0].(map[string]any)
	if !ok {
		t.Fatalf("entries[0] should be an object, got %T", entries[0])
	}
	if entry["source"] != "human" {
		t.Errorf("entries[0].source = %v, want human", entry["source"])
	}
	if entry["operation"] != "write" {
		t.Errorf("entries[0].operation = %v, want write", entry["operation"])
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

type provenanceErrEngine struct{ fakeEngine }

func (e *provenanceErrEngine) GetProvenance(_ context.Context, _, _ string) ([]ProvenanceEntry, error) {
	return nil, fmt.Errorf("engram not found")
}

func TestHandleProvenance_EngineError(t *testing.T) {
	srv := New(":0", &provenanceErrEngine{}, "", nil)
	w := postRPC(t, srv, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"muninn_provenance","arguments":{"vault":"default","id":"01HXYZ"}}}`)
	var resp JSONRPCResponse
	json.NewDecoder(w.Body).Decode(&resp)
	if resp.Error == nil {
		t.Fatal("expected error for engine failure")
	}
}
