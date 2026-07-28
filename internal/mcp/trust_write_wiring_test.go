package mcp

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// captureTrustEngine records the Trust label the handler places on the
// downstream engine WriteRequest, for both single and batch remember. The
// engine-level gate (verified requires write/full) is proven in
// internal/engine/trust_write_test.go; this test proves the MCP handler
// actually reads the `trust` arg and forwards it, closing the surface seam.
type captureTrustEngine struct {
	fakeEngine
	gotTrust      string
	gotBatchTrust []string
}

func (e *captureTrustEngine) Write(ctx context.Context, req *mbp.WriteRequest) (*mbp.WriteResponse, error) {
	e.gotTrust = req.Trust
	return &mbp.WriteResponse{ID: "fake-id"}, nil
}

func (e *captureTrustEngine) WriteBatch(ctx context.Context, reqs []*mbp.WriteRequest) ([]*mbp.WriteResponse, []error) {
	responses := make([]*mbp.WriteResponse, len(reqs))
	errs := make([]error, len(reqs))
	for i, r := range reqs {
		e.gotBatchTrust = append(e.gotBatchTrust, r.Trust)
		responses[i] = &mbp.WriteResponse{ID: "fake-id"}
	}
	return responses, errs
}

func TestRemember_TrustArg_ForwardedToEngine(t *testing.T) {
	eng := &captureTrustEngine{}
	srv := New(":0", eng, "", nil, nil, nil)

	body := mkToolCallBody("muninn_remember", map[string]any{
		"vault": "default", "content": "verified fact", "trust": "verified",
	})
	w := doAuthenticatedPost(srv, "", body)

	var resp JSONRPCResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Error != nil {
		t.Fatalf("unexpected error: %v", resp.Error.Message)
	}
	if eng.gotTrust != "verified" {
		t.Errorf("engine received trust=%q, want verified (handler did not forward the arg)", eng.gotTrust)
	}
}

func TestRememberBatch_TrustArg_ForwardedToEngine(t *testing.T) {
	eng := &captureTrustEngine{}
	srv := New(":0", eng, "", nil, nil, nil)

	body := mkToolCallBody("muninn_remember_batch", map[string]any{
		"vault": "default",
		"memories": []any{
			map[string]any{"content": "a", "trust": "verified"},
			map[string]any{"content": "b"},
			map[string]any{"content": "c", "trust": "external"},
		},
	})
	w := doAuthenticatedPost(srv, "", body)

	var resp JSONRPCResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Error != nil {
		t.Fatalf("unexpected error: %v", resp.Error.Message)
	}
	want := []string{"verified", "", "external"}
	if len(eng.gotBatchTrust) != len(want) {
		t.Fatalf("got %d batch items, want %d", len(eng.gotBatchTrust), len(want))
	}
	for i := range want {
		if eng.gotBatchTrust[i] != want[i] {
			t.Errorf("batch item %d trust = %q, want %q", i, eng.gotBatchTrust[i], want[i])
		}
	}
}
