package mcp

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/scrypster/muninndb/internal/auth"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// captureTrustEngine records the Trust label the handler places on the
// downstream engine WriteRequest, and the credential mode dispatchToolCall
// injected into ctx. The engine-level gate (verified requires write/full) is
// proven in internal/engine/trust_write_test.go; this test proves the MCP
// handler reads the `trust` arg and forwards it, and that an authorized
// mode-less session (static token / open-server) reaches the engine as
// ModeFull so the SEC-14 gate accepts verified on the default deployment.
type captureTrustEngine struct {
	fakeEngine
	gotTrust      string
	gotMode       string
	gotBatchTrust []string
}

func (e *captureTrustEngine) Write(ctx context.Context, req *mbp.WriteRequest) (*mbp.WriteResponse, error) {
	e.gotTrust = req.Trust
	e.gotMode, _ = ctx.Value(auth.ContextMode).(string)
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

// TestOpenServer_InjectsFullMode: on the default zero-config (open-server)
// deployment — no key auth, empty static token — an authorized session must
// reach the engine as ModeFull, not "". Before the fix, dispatchToolCall
// injected a.Mode ("") verbatim, so resolveTrust (SEC-14) rejected
// trust=verified on the most common deployment, silently killing the S8 happy
// path. This asserts the mode-less authorized session is mapped to ModeFull so
// verified is accepted.
func TestOpenServer_InjectsFullMode(t *testing.T) {
	eng := &captureTrustEngine{}
	srv := New(":0", eng, "", nil, nil, nil)

	body := mkToolCallBody("muninn_remember", map[string]any{
		"vault": "default", "content": "open-server verified", "trust": "verified",
	})
	w := doAuthenticatedPost(srv, "", body)

	var resp JSONRPCResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Error != nil {
		t.Fatalf("unexpected error: %v", resp.Error.Message)
	}
	if eng.gotMode != auth.ModeFull {
		t.Errorf("open-server session reached engine as mode %q, want %q — resolveTrust would reject verified", eng.gotMode, auth.ModeFull)
	}
}
