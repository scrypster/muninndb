package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/scrypster/muninndb/internal/auth"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// captureReadOnlyEngine records the ReadOnly field the handler sets on the
// downstream engine request, for muninn_recall (Activate) and muninn_read
// (Read).
type captureReadOnlyEngine struct {
	fakeEngine
	gotActivateReadOnly bool
	gotReadReadOnly     bool
}

func (e *captureReadOnlyEngine) Activate(ctx context.Context, req *mbp.ActivateRequest) (*mbp.ActivateResponse, error) {
	e.gotActivateReadOnly = req.ReadOnly
	return &mbp.ActivateResponse{}, nil
}

func (e *captureReadOnlyEngine) Read(ctx context.Context, req *mbp.ReadRequest) (*mbp.ReadResponse, error) {
	e.gotReadReadOnly = req.ReadOnly
	return &mbp.ReadResponse{}, nil
}

// TestReadOnly_CannotEscalate: an observe-mode mk_ credential combined with an
// EXPLICIT read_only=false must be rejected at the handler for all three S3
// tools (muninn_recall, muninn_read, muninn_where_left_off) — the request
// cannot escalate past what the credential allows.
func TestReadOnly_CannotEscalate(t *testing.T) {
	cases := []struct {
		tool string
		args map[string]any
	}{
		{"muninn_recall", map[string]any{"vault": "walled", "context": "test", "read_only": false}},
		{"muninn_read", map[string]any{"vault": "walled", "id": "01ARZ3NDEKTSV4RRFFQ69G5FAV", "read_only": false}},
		{"muninn_where_left_off", map[string]any{"vault": "walled", "read_only": false}},
	}
	for _, tc := range cases {
		t.Run(tc.tool, func(t *testing.T) {
			store := newMockKeyStore(auth.APIKey{ID: "obsro", Vault: "walled", Mode: auth.ModeObserve})
			srv := newAuthTestServer(store)
			body := mkToolCallBody(tc.tool, tc.args)

			w := doAuthenticatedPost(srv, "mk_obsro", body)

			var resp JSONRPCResponse
			if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
				t.Fatalf("decode error: %v", err)
			}
			if resp.Error == nil {
				t.Fatalf("%s: expected error response for observe credential + read_only=false, got success", tc.tool)
			}
			if !strings.Contains(resp.Error.Message, "forbidden") {
				t.Errorf("%s: expected 'forbidden' in error, got: %s", tc.tool, resp.Error.Message)
			}
		})
	}
}

// TestReadOnly_ObserveCredential_OmittedOrTrue_Allowed verifies the rejection
// in TestReadOnly_CannotEscalate is specific to an EXPLICIT false: omitting
// read_only, or passing it as true, must still work for an observe credential.
func TestReadOnly_ObserveCredential_OmittedOrTrue_Allowed(t *testing.T) {
	cases := []struct {
		name string
		args map[string]any
	}{
		{"omitted", map[string]any{"vault": "walled", "context": "test"}},
		{"explicit_true", map[string]any{"vault": "walled", "context": "test", "read_only": true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newMockKeyStore(auth.APIKey{ID: "obsok", Vault: "walled", Mode: auth.ModeObserve})
			srv := newAuthTestServer(store)
			body := mkToolCallBody("muninn_recall", tc.args)

			w := doAuthenticatedPost(srv, "mk_obsok", body)

			var resp JSONRPCResponse
			if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
				t.Fatalf("decode error: %v", err)
			}
			if resp.Error != nil {
				t.Fatalf("unexpected error: %s", resp.Error.Message)
			}
		})
	}
}

// TestRecall_ReadOnlyFlag_ReachesEngine verifies the MCP handler for
// muninn_recall actually threads the effective read_only decision through to
// mbp.ActivateRequest.ReadOnly (a full-mode credential with an explicit
// read_only=true must produce ReadOnly=true on the downstream call).
func TestRecall_ReadOnlyFlag_ReachesEngine(t *testing.T) {
	eng := &captureReadOnlyEngine{}
	srv := New(":0", eng, "", nil, nil, nil)
	body := mkToolCallBody("muninn_recall", map[string]any{"vault": "default", "context": "test", "read_only": true})

	w := doAuthenticatedPost(srv, "", body)
	var resp JSONRPCResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if resp.Error != nil {
		t.Fatalf("unexpected error: %s", resp.Error.Message)
	}
	if !eng.gotActivateReadOnly {
		t.Error("ActivateRequest.ReadOnly = false, want true (read_only:true was not threaded through)")
	}
}

// TestRead_ReadOnlyFlag_ReachesEngine is the muninn_read analogue — this is
// the critical path per S3: it closes the brief->"verify before
// acting"->read->reinforce loop.
func TestRead_ReadOnlyFlag_ReachesEngine(t *testing.T) {
	eng := &captureReadOnlyEngine{}
	srv := New(":0", eng, "", nil, nil, nil)
	body := mkToolCallBody("muninn_read", map[string]any{"vault": "default", "id": "01ARZ3NDEKTSV4RRFFQ69G5FAV", "read_only": true})

	w := doAuthenticatedPost(srv, "", body)
	var resp JSONRPCResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if resp.Error != nil {
		t.Fatalf("unexpected error: %s", resp.Error.Message)
	}
	if !eng.gotReadReadOnly {
		t.Error("ReadRequest.ReadOnly = false, want true (read_only:true was not threaded through)")
	}
}
