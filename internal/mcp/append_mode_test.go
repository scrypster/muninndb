package mcp

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/scrypster/muninndb/internal/auth"
)

// TestAppendMode_DispatchGate proves the MCP dispatch gate for append-mode
// credentials (the flush write credential): destructive/modifying mutating
// tools are refused, while read tools and additive (create-new) tools reach the
// handler. This is the first of the two enforcement layers; the engine
// (TestAppendMode_RefusesEvolveAndForget) is the transport-agnostic backstop.
func TestAppendMode_DispatchGate(t *testing.T) {
	forbidden := []string{"muninn_forget", "muninn_evolve", "muninn_trust", "muninn_merge_entity", "muninn_link"}
	allowed := []string{"muninn_remember", "muninn_remember_batch", "muninn_recall", "muninn_read", "muninn_where_left_off"}

	for _, tool := range forbidden {
		t.Run("forbidden/"+tool, func(t *testing.T) {
			store := newMockKeyStore(auth.APIKey{ID: "app", Vault: "walled", Mode: auth.ModeAppend})
			srv := newAuthTestServer(store)
			w := doAuthenticatedPost(srv, "mk_app", mkToolCallBody(tool, map[string]any{"vault": "walled"}))
			var resp JSONRPCResponse
			if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if resp.Error == nil {
				t.Fatalf("%s: expected forbidden error for append-mode key, got success", tool)
			}
			if !strings.Contains(resp.Error.Message, "forbidden") || !strings.Contains(resp.Error.Message, "append") {
				t.Errorf("%s: want an append-mode forbidden error, got: %s", tool, resp.Error.Message)
			}
		})
	}

	for _, tool := range allowed {
		t.Run("allowed/"+tool, func(t *testing.T) {
			store := newMockKeyStore(auth.APIKey{ID: "app", Vault: "walled", Mode: auth.ModeAppend})
			srv := newAuthTestServer(store)
			w := doAuthenticatedPost(srv, "mk_app", mkToolCallBody(tool, map[string]any{"vault": "walled", "content": "x", "context": "x", "id": "01ARZ3NDEKTSV4RRFFQ69G5FAV"}))
			var resp JSONRPCResponse
			if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
				t.Fatalf("decode: %v", err)
			}
			// The dispatch gate must NOT reject with the append-mode forbidden
			// error. (A downstream handler/validation error is fine — we only
			// assert the mode gate let it through.)
			if resp.Error != nil && strings.Contains(resp.Error.Message, "append-mode key cannot call") {
				t.Errorf("%s: append mode wrongly blocked an allowed tool: %s", tool, resp.Error.Message)
			}
		})
	}
}
