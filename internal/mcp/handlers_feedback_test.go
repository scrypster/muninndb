package mcp

import (
	"context"
	"encoding/json"
	"testing"
)

type feedbackEngine struct {
	fakeEngine
	gotVault  string
	gotUseful *bool
}

func (e *feedbackEngine) RecordFeedback(_ context.Context, vault, _ string, useful bool) error {
	e.gotVault, e.gotUseful = vault, &useful
	return nil
}

// TestHandleFeedback_HappyPath: the legacy form (engram_id, no query_id)
// still works, passes useful through unchanged, and answers with the uniform
// shape — plus a form-dependent (never outcome-dependent) note that it
// recorded no label.
func TestHandleFeedback_HappyPath(t *testing.T) {
	fe := &feedbackEngine{}
	srv := New(":0", fe, "", nil, nil, nil)
	w := postRPC(t, srv, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"muninn_feedback","arguments":{"vault":"default","engram_id":"01ARZ3NDEKTSV4RRFFQ69G5FAV","useful":false}}}`)
	if w.Code != 200 {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	resp := decodeResp(t, w.Body.String())
	if resp.Error != nil {
		t.Fatalf("unexpected error: %v", resp.Error)
	}
	content := extractInnerJSON(t, resp)
	if ok, _ := content["ok"].(bool); !ok {
		t.Errorf("feedback response ok = %v, want true", content["ok"])
	}
	if content["received_ids"] != float64(1) {
		t.Errorf("received_ids = %v, want 1", content["received_ids"])
	}
	if fe.gotUseful == nil || *fe.gotUseful != false {
		t.Errorf("engine got useful=%v, want false", fe.gotUseful)
	}
}

func TestHandleFeedback_DefaultVault(t *testing.T) {
	// Vault is optional — omitting it defaults to "default".
	fe := &feedbackEngine{}
	srv := New(":0", fe, "", nil, nil, nil)
	w := postRPC(t, srv, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"muninn_feedback","arguments":{"engram_id":"01ARZ3NDEKTSV4RRFFQ69G5FAV","useful":true}}}`)
	resp := decodeResp(t, w.Body.String())
	if resp.Error != nil {
		t.Fatalf("unexpected error: %v", resp.Error)
	}
	if fe.gotVault != "default" {
		t.Errorf("engine got vault %q, want default", fe.gotVault)
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
