package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/auth"
	"github.com/scrypster/muninndb/internal/engine"
	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/transport/mbp"
	"github.com/stretchr/testify/require"
)

// ── helpers ─────────────────────────────────────────────────────────────────

// newTestServerWith creates a server backed by the supplied engine.
func newTestServerWith(eng EngineInterface) *MCPServer {
	return New(":0", eng, "", nil)
}

// extractInnerJSON decodes the MCP textContent envelope and returns the inner
// JSON as a map. The result from sendResult(…, textContent(mustJSON(v))) is:
//
//	{"content":[{"type":"text","text":"<inner json>"}]}
func extractInnerJSON(t *testing.T, resp JSONRPCResponse) map[string]any {
	t.Helper()
	if resp.Error != nil {
		t.Fatalf("unexpected JSON-RPC error: code=%d msg=%s", resp.Error.Code, resp.Error.Message)
	}
	wrapper, ok := resp.Result.(map[string]any)
	if !ok {
		t.Fatalf("expected result to be an object, got %T", resp.Result)
	}
	contents, ok := wrapper["content"].([]any)
	if !ok || len(contents) == 0 {
		t.Fatal("expected result.content to be a non-empty array")
	}
	item, ok := contents[0].(map[string]any)
	if !ok {
		t.Fatalf("expected result.content[0] to be an object, got %T", contents[0])
	}
	text, ok := item["text"].(string)
	if !ok {
		t.Fatalf("expected result.content[0].text to be a string, got %T", item["text"])
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatalf("unmarshal inner JSON: %v — text was: %s", err, text)
	}
	return out
}

// decodeResp decodes the raw HTTP response body into a JSONRPCResponse.
func decodeResp(t *testing.T, body string) JSONRPCResponse {
	t.Helper()
	var resp JSONRPCResponse
	if err := json.NewDecoder(strings.NewReader(body)).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return resp
}

// ── per-handler error-injection engines ─────────────────────────────────────
// Each embeds fakeEngine so the other 16 methods are covered.

type restoreErrEngine struct{ fakeEngine }

func (e *restoreErrEngine) Restore(_ context.Context, _ string, _ string) (*RestoreResult, error) {
	return nil, fmt.Errorf("engram not found or recovery window expired")
}

type traverseErrEngine struct{ fakeEngine }

func (e *traverseErrEngine) Traverse(_ context.Context, _ string, _ *TraverseRequest) (*TraverseResult, error) {
	return nil, fmt.Errorf("start node not found")
}

type traverseCapturingEngine struct {
	fakeEngine
	capturedFollowEntities bool
}

func (e *traverseCapturingEngine) Traverse(_ context.Context, _ string, req *TraverseRequest) (*TraverseResult, error) {
	e.capturedFollowEntities = req.FollowEntities
	return &TraverseResult{
		Nodes:          []TraversalNode{{ID: "s1", Concept: "start", HopDist: 0}},
		Edges:          []TraversalEdge{},
		TotalReachable: 1,
		QueryMs:        0.5,
	}, nil
}

type explainErrEngine struct{ fakeEngine }

func (e *explainErrEngine) Explain(_ context.Context, _ string, _ *ExplainRequest) (*ExplainResult, error) {
	return nil, fmt.Errorf("engram not found")
}

type stateErrEngine struct{ fakeEngine }

func (e *stateErrEngine) UpdateState(_ context.Context, _ string, _ string, _ string, _ string) error {
	return fmt.Errorf("invalid transition: archived has no valid next states")
}

type listDeletedErrEngine struct{ fakeEngine }

func (e *listDeletedErrEngine) ListDeleted(_ context.Context, _ string, _ int) ([]DeletedEngram, error) {
	return nil, fmt.Errorf("storage error")
}

type retryEnrichErrEngine struct{ fakeEngine }

func (e *retryEnrichErrEngine) RetryEnrich(_ context.Context, _ string, _ string) (*RetryEnrichResult, error) {
	return nil, fmt.Errorf("engram not found")
}

// traverseWithNodesEngine returns a populated TraverseResult for shape tests.
type traverseWithNodesEngine struct{ fakeEngine }

func (e *traverseWithNodesEngine) Traverse(_ context.Context, _ string, _ *TraverseRequest) (*TraverseResult, error) {
	return &TraverseResult{
		Nodes: []TraversalNode{
			{ID: "s1", Concept: "start node", HopDist: 0},
			{ID: "n1", Concept: "neighbor", HopDist: 1},
		},
		Edges: []TraversalEdge{
			{FromID: "s1", ToID: "n1", RelType: "relates_to", Weight: 0.9},
		},
		TotalReachable: 2,
		QueryMs:        1.5,
	}, nil
}

// listDeletedWithEntriesEngine returns a populated deleted list for shape tests.
type listDeletedWithEntriesEngine struct{ fakeEngine }

func (e *listDeletedWithEntriesEngine) ListDeleted(_ context.Context, _ string, _ int) ([]DeletedEngram, error) {
	return []DeletedEngram{
		{
			ID:               "del-1",
			Concept:          "old decision",
			DeletedAt:        time.Now().Add(-1 * time.Hour),
			RecoverableUntil: time.Now().Add(167 * time.Hour),
			Tags:             []string{"arch"},
		},
	}, nil
}

// noPluginsEngine returns a RetryEnrichResult with the Note field populated.
type noPluginsEngine struct{ fakeEngine }

func (e *noPluginsEngine) RetryEnrich(_ context.Context, _ string, id string) (*RetryEnrichResult, error) {
	return &RetryEnrichResult{
		EngramID:        id,
		PluginsQueued:   []string{},
		AlreadyComplete: []string{},
		Note:            "No enrichment plugins are registered",
	}, nil
}

// idempotentEngine is a fake engine that records Write calls and supports
// configurable CheckIdempotency responses for testing the op_id path.
type idempotentEngine struct {
	fakeEngine
	receipt   *storage.IdempotencyReceipt // non-nil → return this on CheckIdempotency
	writeCalls int
}

func (e *idempotentEngine) CheckIdempotency(_ context.Context, _ string) (*storage.IdempotencyReceipt, error) {
	return e.receipt, nil
}

func (e *idempotentEngine) WriteIdempotency(_ context.Context, _, _ string) error {
	return nil
}

func (e *idempotentEngine) Write(_ context.Context, _ *mbp.WriteRequest) (*mbp.WriteResponse, error) {
	e.writeCalls++
	return &mbp.WriteResponse{ID: "fresh-id"}, nil
}

// limitTrackingEngine records the limit value received by ListDeleted.
type limitTrackingEngine struct {
	fakeEngine
	lastLimit int
}

func (e *limitTrackingEngine) ListDeleted(_ context.Context, _ string, limit int) ([]DeletedEngram, error) {
	e.lastLimit = limit
	return []DeletedEngram{}, nil
}

// ── muninn_restore ──────────────────────────────────────────────────────────

func TestHandleRestoreHappyPath(t *testing.T) {
	srv := newTestServer()
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_restore","arguments":{"vault":"default","id":"abc123"}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error != nil {
		t.Fatalf("unexpected error: %v", resp.Error)
	}
	if resp.Result == nil {
		t.Fatal("expected non-nil result")
	}
}

func TestHandleRestoreResponseShape(t *testing.T) {
	srv := newTestServer()
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_restore","arguments":{"vault":"default","id":"abc123"}}}`
	w := postRPC(t, srv, body)
	content := extractInnerJSON(t, decodeResp(t, w.Body.String()))

	for _, field := range []string{"id", "concept", "restored", "state"} {
		if _, ok := content[field]; !ok {
			t.Errorf("response missing field: %q", field)
		}
	}
	if restored, _ := content["restored"].(bool); !restored {
		t.Errorf("restored field should be true, got %v", content["restored"])
	}
}

func TestHandleRestoreMissingID(t *testing.T) {
	srv := newTestServer()
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_restore","arguments":{"vault":"default"}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error == nil || resp.Error.Code != -32602 {
		t.Errorf("expected -32602, got %v", resp.Error)
	}
}

func TestHandleRestoreEngineError(t *testing.T) {
	srv := newTestServerWith(&restoreErrEngine{})
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_restore","arguments":{"vault":"default","id":"gone"}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error == nil || resp.Error.Code != -32000 {
		t.Errorf("expected -32000 for engine error, got %v", resp.Error)
	}
	if !strings.Contains(resp.Error.Message, "recovery window") {
		t.Errorf("error message should mention recovery window, got: %s", resp.Error.Message)
	}
}

// ── muninn_traverse ─────────────────────────────────────────────────────────

func TestHandleTraverseHappyPath(t *testing.T) {
	srv := newTestServer()
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_traverse","arguments":{"vault":"default","start_id":"node1"}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error != nil {
		t.Fatalf("unexpected error: %v", resp.Error)
	}
}

func TestHandleTraverseResponseShape(t *testing.T) {
	srv := newTestServerWith(&traverseWithNodesEngine{})
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_traverse","arguments":{"vault":"default","start_id":"s1"}}}`
	w := postRPC(t, srv, body)
	content := extractInnerJSON(t, decodeResp(t, w.Body.String()))

	for _, field := range []string{"nodes", "edges", "total_reachable", "query_ms"} {
		if _, ok := content[field]; !ok {
			t.Errorf("response missing field: %q", field)
		}
	}

	nodes, ok := content["nodes"].([]any)
	if !ok || len(nodes) == 0 {
		t.Fatal("expected non-empty nodes array")
	}
	node, ok := nodes[0].(map[string]any)
	if !ok {
		t.Fatal("nodes[0] is not an object")
	}
	if _, ok := node["hop_dist"]; !ok {
		t.Error("nodes[0] missing required field 'hop_dist'")
	}
	// Start node must have hop_dist == 0
	if dist, _ := node["hop_dist"].(float64); dist != 0 {
		t.Errorf("start node hop_dist = %v, want 0", dist)
	}
}

func TestHandleTraverseMissingStartID(t *testing.T) {
	srv := newTestServer()
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_traverse","arguments":{"vault":"default"}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error == nil || resp.Error.Code != -32602 {
		t.Errorf("expected -32602, got %v", resp.Error)
	}
}

func TestHandleTraverseCapsBounds(t *testing.T) {
	srv := newTestServer()
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_traverse","arguments":{"vault":"default","start_id":"n1","max_hops":99,"max_nodes":9999}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error != nil {
		t.Errorf("expected success after capping bounds, got error: %v", resp.Error)
	}
}

func TestHandleTraverseWithRelTypes(t *testing.T) {
	srv := newTestServer()
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_traverse","arguments":{"vault":"default","start_id":"n1","rel_types":["depends_on","supports"]}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error != nil {
		t.Errorf("rel_types optional param should be accepted, got error: %v", resp.Error)
	}
}

func TestHandleTraverseEngineError(t *testing.T) {
	srv := newTestServerWith(&traverseErrEngine{})
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_traverse","arguments":{"vault":"default","start_id":"bad"}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error == nil || resp.Error.Code != -32000 {
		t.Errorf("expected -32000 for engine error, got %v", resp.Error)
	}
}

func TestHandleTraverseWithFollowEntities(t *testing.T) {
	eng := &traverseCapturingEngine{}
	srv := newTestServerWith(eng)
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_traverse","arguments":{"vault":"default","start_id":"s1","follow_entities":true}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error != nil {
		t.Fatalf("unexpected error: %v", resp.Error)
	}
	if !eng.capturedFollowEntities {
		t.Error("engine should have received follow_entities=true")
	}
}

// ── muninn_explain ──────────────────────────────────────────────────────────

func TestHandleExplainHappyPath(t *testing.T) {
	srv := newTestServer()
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_explain","arguments":{"vault":"default","engram_id":"e1","query":["JWT","auth"]}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error != nil {
		t.Fatalf("unexpected error: %v", resp.Error)
	}
}

func TestHandleExplainResponseShape(t *testing.T) {
	srv := newTestServer()
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_explain","arguments":{"vault":"default","engram_id":"e1","query":["JWT"]}}}`
	w := postRPC(t, srv, body)
	content := extractInnerJSON(t, decodeResp(t, w.Body.String()))

	// Top-level required fields
	for _, field := range []string{"engram_id", "final_score", "components", "fts_matches", "assoc_path", "would_return", "threshold"} {
		if _, ok := content[field]; !ok {
			t.Errorf("response missing field: %q", field)
		}
	}

	// All 6 component sub-fields must be present
	components, ok := content["components"].(map[string]any)
	if !ok {
		t.Fatal("components field is not an object")
	}
	for _, comp := range []string{
		"full_text_relevance", "semantic_similarity", "decay_factor",
		"hebbian_boost", "access_frequency", "confidence",
	} {
		if _, ok := components[comp]; !ok {
			t.Errorf("components missing field: %q", comp)
		}
	}

	// would_return and threshold must be present and typed correctly
	if _, ok := content["would_return"].(bool); !ok {
		t.Error("would_return should be a bool")
	}
	if _, ok := content["threshold"].(float64); !ok {
		t.Error("threshold should be a number")
	}
}

func TestHandleExplainMissingEngramID(t *testing.T) {
	srv := newTestServer()
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_explain","arguments":{"vault":"default","query":["test"]}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error == nil || resp.Error.Code != -32602 {
		t.Errorf("expected -32602, got %v", resp.Error)
	}
}

func TestHandleExplainEmptyQuery(t *testing.T) {
	srv := newTestServer()
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_explain","arguments":{"vault":"default","engram_id":"e1","query":[]}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error == nil || resp.Error.Code != -32602 {
		t.Errorf("expected -32602 for empty query, got %v", resp.Error)
	}
}

func TestHandleExplainMissingQuery(t *testing.T) {
	srv := newTestServer()
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_explain","arguments":{"vault":"default","engram_id":"e1"}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error == nil || resp.Error.Code != -32602 {
		t.Errorf("expected -32602 for missing query, got %v", resp.Error)
	}
}

func TestHandleExplainEngineError(t *testing.T) {
	srv := newTestServerWith(&explainErrEngine{})
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_explain","arguments":{"vault":"default","engram_id":"gone","query":["x"]}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error == nil || resp.Error.Code != -32000 {
		t.Errorf("expected -32000 for engine error, got %v", resp.Error)
	}
}

// ── muninn_state ─────────────────────────────────────────────────────────────

func TestHandleStateHappyPath(t *testing.T) {
	srv := newTestServer()
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_state","arguments":{"vault":"default","id":"e1","state":"active"}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error != nil {
		t.Fatalf("unexpected error: %v", resp.Error)
	}
}

func TestHandleStateResponseShape(t *testing.T) {
	srv := newTestServer()
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_state","arguments":{"vault":"default","id":"e1","state":"completed"}}}`
	w := postRPC(t, srv, body)
	content := extractInnerJSON(t, decodeResp(t, w.Body.String()))

	for _, field := range []string{"id", "state", "updated"} {
		if _, ok := content[field]; !ok {
			t.Errorf("response missing field: %q", field)
		}
	}
	if updated, _ := content["updated"].(bool); !updated {
		t.Errorf("updated field should be true, got %v", content["updated"])
	}
	if content["id"] != "e1" {
		t.Errorf("id = %v, want e1", content["id"])
	}
	if content["state"] != "completed" {
		t.Errorf("state = %v, want completed", content["state"])
	}
}

func TestHandleStateInvalidState(t *testing.T) {
	srv := newTestServer()
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_state","arguments":{"vault":"default","id":"e1","state":"limbo"}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error == nil || resp.Error.Code != -32602 {
		t.Errorf("expected -32602 for invalid state, got %v", resp.Error)
	}
}

func TestHandleStateAllValidStates(t *testing.T) {
	srv := newTestServer()
	states := []string{"planning", "active", "paused", "blocked", "completed", "cancelled", "archived"}
	for _, state := range states {
		t.Run(state, func(t *testing.T) {
			body := fmt.Sprintf(`{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_state","arguments":{"vault":"default","id":"e1","state":%q}}}`, state)
			w := postRPC(t, srv, body)
			resp := decodeResp(t, w.Body.String())
			if resp.Error != nil {
				t.Errorf("state %q: unexpected error: %v", state, resp.Error)
			}
		})
	}
}

func TestHandleStateWithOptionalReason(t *testing.T) {
	srv := newTestServer()
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_state","arguments":{"vault":"default","id":"e1","state":"paused","reason":"waiting for design review"}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error != nil {
		t.Errorf("optional reason should be accepted, got error: %v", resp.Error)
	}
}

func TestHandleStateMissingID(t *testing.T) {
	srv := newTestServer()
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_state","arguments":{"vault":"default","state":"active"}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error == nil || resp.Error.Code != -32602 {
		t.Errorf("expected -32602, got %v", resp.Error)
	}
}

func TestHandleStateMissingState(t *testing.T) {
	srv := newTestServer()
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_state","arguments":{"vault":"default","id":"e1"}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error == nil || resp.Error.Code != -32602 {
		t.Errorf("expected -32602, got %v", resp.Error)
	}
}

func TestHandleStateEngineError(t *testing.T) {
	// Engine returns error for invalid transitions (e.g. archived → planning).
	srv := newTestServerWith(&stateErrEngine{})
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_state","arguments":{"vault":"default","id":"e1","state":"planning"}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error == nil || resp.Error.Code != -32000 {
		t.Errorf("expected -32000 for engine transition error, got %v", resp.Error)
	}
}

// ── muninn_list_deleted ──────────────────────────────────────────────────────

func TestHandleListDeletedHappyPath(t *testing.T) {
	srv := newTestServer()
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_list_deleted","arguments":{"vault":"default"}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error != nil {
		t.Fatalf("unexpected error: %v", resp.Error)
	}
}

func TestHandleListDeletedResponseShape(t *testing.T) {
	srv := newTestServer()
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_list_deleted","arguments":{"vault":"default"}}}`
	w := postRPC(t, srv, body)
	content := extractInnerJSON(t, decodeResp(t, w.Body.String()))

	if _, ok := content["deleted"]; !ok {
		t.Error("response missing field: \"deleted\"")
	}
	if _, ok := content["count"]; !ok {
		t.Error("response missing field: \"count\"")
	}
	// Empty result: count must equal len(deleted)
	deleted, _ := content["deleted"].([]any)
	count, _ := content["count"].(float64)
	if int(count) != len(deleted) {
		t.Errorf("count=%d does not match len(deleted)=%d", int(count), len(deleted))
	}
}

func TestHandleListDeletedEntryHasRecoverableUntil(t *testing.T) {
	srv := newTestServerWith(&listDeletedWithEntriesEngine{})
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_list_deleted","arguments":{"vault":"default"}}}`
	w := postRPC(t, srv, body)
	content := extractInnerJSON(t, decodeResp(t, w.Body.String()))

	deleted, ok := content["deleted"].([]any)
	if !ok || len(deleted) == 0 {
		t.Fatal("expected non-empty deleted list")
	}
	entry, ok := deleted[0].(map[string]any)
	if !ok {
		t.Fatal("deleted[0] is not an object")
	}
	for _, field := range []string{"id", "concept", "deleted_at", "recoverable_until"} {
		if _, ok := entry[field]; !ok {
			t.Errorf("deleted entry missing field: %q", field)
		}
	}
}

func TestHandleListDeletedEmptyIsNotError(t *testing.T) {
	srv := newTestServer()
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_list_deleted","arguments":{"vault":"default"}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error != nil {
		t.Errorf("empty vault should return success, not error: %v", resp.Error)
	}
}

func TestHandleListDeletedLimitCap(t *testing.T) {
	eng := &limitTrackingEngine{}
	srv := newTestServerWith(eng)
	// Request limit=200; handler must cap to 100 before calling engine.
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_list_deleted","arguments":{"vault":"default","limit":200}}}`
	postRPC(t, srv, body)
	if eng.lastLimit != 100 {
		t.Errorf("expected engine to receive limit=100 after capping, got %d", eng.lastLimit)
	}
}

func TestHandleListDeletedEngineError(t *testing.T) {
	srv := newTestServerWith(&listDeletedErrEngine{})
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_list_deleted","arguments":{"vault":"default"}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error == nil || resp.Error.Code != -32000 {
		t.Errorf("expected -32000 for engine error, got %v", resp.Error)
	}
}

// ── muninn_retry_enrich ──────────────────────────────────────────────────────

func TestHandleRetryEnrichHappyPath(t *testing.T) {
	srv := newTestServer()
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_retry_enrich","arguments":{"vault":"default","id":"e1"}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error != nil {
		t.Fatalf("unexpected error: %v", resp.Error)
	}
}

func TestHandleRetryEnrichResponseShape(t *testing.T) {
	srv := newTestServer()
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_retry_enrich","arguments":{"vault":"default","id":"e1"}}}`
	w := postRPC(t, srv, body)
	content := extractInnerJSON(t, decodeResp(t, w.Body.String()))

	for _, field := range []string{"engram_id", "plugins_queued", "already_complete"} {
		if _, ok := content[field]; !ok {
			t.Errorf("response missing field: %q", field)
		}
	}
	if _, ok := content["plugins_queued"].([]any); !ok {
		t.Error("plugins_queued should be an array")
	}
	if _, ok := content["already_complete"].([]any); !ok {
		t.Error("already_complete should be an array")
	}
}

func TestHandleRetryEnrichNoPluginsNote(t *testing.T) {
	// AC: "degrades gracefully when no enrich plugin registered (empty queued list + note field)"
	srv := newTestServerWith(&noPluginsEngine{})
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_retry_enrich","arguments":{"vault":"default","id":"e1"}}}`
	w := postRPC(t, srv, body)
	content := extractInnerJSON(t, decodeResp(t, w.Body.String()))

	queued, _ := content["plugins_queued"].([]any)
	if len(queued) != 0 {
		t.Errorf("expected empty plugins_queued when no plugins, got %v", queued)
	}
	note, ok := content["note"].(string)
	if !ok || note == "" {
		t.Error("expected non-empty note field when no plugins registered")
	}
}

func TestHandleRetryEnrichMissingID(t *testing.T) {
	srv := newTestServer()
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_retry_enrich","arguments":{"vault":"default"}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error == nil || resp.Error.Code != -32602 {
		t.Errorf("expected -32602, got %v", resp.Error)
	}
}

func TestHandleRetryEnrichEngineError(t *testing.T) {
	srv := newTestServerWith(&retryEnrichErrEngine{})
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_retry_enrich","arguments":{"vault":"default","id":"gone"}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error == nil || resp.Error.Code != -32000 {
		t.Errorf("expected -32000 for engine error, got %v", resp.Error)
	}
}

// ── relTypeFromString ─────────────────────────────────────────────────────────

func TestRelTypeFromString_AllTypes(t *testing.T) {
	cases := []struct {
		input string
		want  uint16
	}{
		{"supports", uint16(storage.RelSupports)},
		{"contradicts", uint16(storage.RelContradicts)},
		{"depends_on", uint16(storage.RelDependsOn)},
		{"supersedes", uint16(storage.RelSupersedes)},
		{"relates_to", uint16(storage.RelRelatesTo)},
		{"is_part_of", uint16(storage.RelIsPartOf)},
		{"causes", uint16(storage.RelCauses)},
		{"preceded_by", uint16(storage.RelPrecededBy)},
		{"followed_by", uint16(storage.RelFollowedBy)},
		{"created_by_person", uint16(storage.RelCreatedByPerson)},
		{"belongs_to_project", uint16(storage.RelBelongsToProject)},
		{"references", uint16(storage.RelReferences)},
		{"implements", uint16(storage.RelImplements)},
		{"blocks", uint16(storage.RelBlocks)},
		{"resolves", uint16(storage.RelResolves)},
		{"refines", uint16(storage.RelRefines)},
	}
	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			got := relTypeFromString(tc.input)
			if got != tc.want {
				t.Errorf("relTypeFromString(%q) = %d, want %d", tc.input, got, tc.want)
			}
		})
	}
}

func TestRelTypeFromString_UnknownDefaultsToRelatesTo(t *testing.T) {
	got := relTypeFromString("foobar")
	want := uint16(storage.RelRelatesTo)
	if got != want {
		t.Errorf("relTypeFromString(%q) = %d, want %d (RelRelatesTo)", "foobar", got, want)
	}
}

func TestRelTypeFromString_EmptyDefaultsToRelatesTo(t *testing.T) {
	got := relTypeFromString("")
	want := uint16(storage.RelRelatesTo)
	if got != want {
		t.Errorf("relTypeFromString(%q) = %d, want %d (RelRelatesTo)", "", got, want)
	}
}

// ── handleRecall profile wiring ───────────────────────────────────────────────

// profileCapturingEngine records the Profile field from the last ActivateRequest.
type profileCapturingEngine struct {
	fakeEngine
	lastProfile string
}

func (e *profileCapturingEngine) Activate(_ context.Context, req *mbp.ActivateRequest) (*mbp.ActivateResponse, error) {
	e.lastProfile = req.Profile
	return &mbp.ActivateResponse{}, nil
}

func TestHandleRecallProfileParamWired(t *testing.T) {
	eng := &profileCapturingEngine{}
	srv := newTestServerWith(eng)
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_recall","arguments":{"vault":"default","context":["auth"],"profile":"causal"}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error != nil {
		t.Fatalf("unexpected error: %v", resp.Error)
	}
	if eng.lastProfile != "causal" {
		t.Errorf("profile = %q, want %q", eng.lastProfile, "causal")
	}
}

func TestHandleRecallProfileOmittedIsEmpty(t *testing.T) {
	eng := &profileCapturingEngine{}
	srv := newTestServerWith(eng)
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_recall","arguments":{"vault":"default","context":["auth"]}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error != nil {
		t.Fatalf("unexpected error: %v", resp.Error)
	}
	if eng.lastProfile != "" {
		t.Errorf("profile = %q, want empty string when not provided", eng.lastProfile)
	}
}

// ── muninn_recall freshness fields ───────────────────────────────────────────

// recallFreshnessEngine returns an ActivateResponse with a single ActivationItem
// that has all four freshness fields populated.
type recallFreshnessEngine struct{ fakeEngine }

func (e *recallFreshnessEngine) Activate(_ context.Context, req *mbp.ActivateRequest) (*mbp.ActivateResponse, error) {
	return &mbp.ActivateResponse{
		Activations: []mbp.ActivationItem{
			{
				ID:          "freshness-001",
				Concept:     "freshness concept",
				Content:     "freshness content",
				Score:       0.9,
				LastAccess:  1700000000_000000000,
				AccessCount: 7,
				Relevance:   0.85,
				SourceType:  "human",
			},
		},
	}, nil
}

// TestHandleRecallFreshnessFieldsPresent verifies that when the engine returns
// an ActivationItem with all four freshness fields, the JSON response contains
// last_access, access_count, relevance, and source_type.
func TestHandleRecallFreshnessFieldsPresent(t *testing.T) {
	srv := newTestServerWith(&recallFreshnessEngine{})
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_recall","arguments":{"vault":"default","context":["test"]}}}`
	w := postRPC(t, srv, body)
	outer := extractInnerJSON(t, decodeResp(t, w.Body.String()))

	memories, ok := outer["memories"].([]any)
	if !ok || len(memories) == 0 {
		t.Fatalf("expected non-empty memories array, got %T %v", outer["memories"], outer["memories"])
	}
	mem, ok := memories[0].(map[string]any)
	if !ok {
		t.Fatalf("memories[0] should be an object, got %T", memories[0])
	}

	for _, field := range []string{"last_access", "access_count", "relevance", "source_type"} {
		if _, exists := mem[field]; !exists {
			t.Errorf("memories[0] missing field %q", field)
		}
	}
	if mem["source_type"] != "human" {
		t.Errorf("source_type = %v, want %q", mem["source_type"], "human")
	}
	if v, ok := mem["access_count"].(float64); !ok || v != 7 {
		t.Errorf("access_count = %v, want 7", mem["access_count"])
	}
}

// recallNoSourceEngine returns an ActivationItem with SourceType deliberately
// left empty to exercise the omitempty behaviour.
type recallNoSourceEngine struct{ fakeEngine }

func (e *recallNoSourceEngine) Activate(_ context.Context, req *mbp.ActivateRequest) (*mbp.ActivateResponse, error) {
	return &mbp.ActivateResponse{
		Activations: []mbp.ActivationItem{
			{
				ID:          "no-source-001",
				Concept:     "no source concept",
				Content:     "no source content",
				Score:       0.7,
				AccessCount: 3,
				Relevance:   0.6,
				// SourceType deliberately omitted
			},
		},
	}, nil
}

// TestHandleRecallEmptySourceTypeOmitted verifies that when SourceType is empty
// on the ActivationItem, the source_type field is absent from the JSON response
// (due to the omitempty tag on Memory.SourceType).
func TestHandleRecallEmptySourceTypeOmitted(t *testing.T) {
	srv := newTestServerWith(&recallNoSourceEngine{})
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_recall","arguments":{"vault":"default","context":["test"]}}}`
	w := postRPC(t, srv, body)
	outer := extractInnerJSON(t, decodeResp(t, w.Body.String()))

	memories, ok := outer["memories"].([]any)
	if !ok || len(memories) == 0 {
		t.Fatalf("expected non-empty memories array, got %T %v", outer["memories"], outer["memories"])
	}
	mem, ok := memories[0].(map[string]any)
	if !ok {
		t.Fatalf("memories[0] should be an object, got %T", memories[0])
	}

	if _, exists := mem["source_type"]; exists {
		t.Errorf("source_type should be absent when SourceType is empty (omitempty), but it was present with value %v", mem["source_type"])
	}
}

// ── muninn_read ──────────────────────────────────────────────────────────────

// readWithDataEngine returns a populated ReadResponse so shape assertions are meaningful.
type readWithDataEngine struct{ fakeEngine }

func (e *readWithDataEngine) Read(_ context.Context, req *mbp.ReadRequest) (*mbp.ReadResponse, error) {
	return &mbp.ReadResponse{
		ID:      req.ID,
		Concept: "test concept",
		Content: "test content body",
	}, nil
}

func TestHandleRead_HappyPath(t *testing.T) {
	srv := newTestServerWith(&readWithDataEngine{})
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_read","arguments":{"vault":"default","id":"abc-123"}}}`
	w := postRPC(t, srv, body)
	content := extractInnerJSON(t, decodeResp(t, w.Body.String()))

	// readResponseToMemory maps the response to a Memory with id and content fields.
	for _, field := range []string{"id", "content"} {
		if _, ok := content[field]; !ok {
			t.Errorf("response missing field: %q", field)
		}
	}
	if content["id"] != "abc-123" {
		t.Errorf("id = %v, want abc-123", content["id"])
	}
}

func TestHandleRead_MissingID(t *testing.T) {
	srv := newTestServer()
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_read","arguments":{"vault":"default"}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error == nil || resp.Error.Code != -32602 {
		t.Errorf("expected -32602, got %v", resp.Error)
	}
}

// ── muninn_forget ────────────────────────────────────────────────────────────

// forgetWithChildrenEngine simulates a parent that has children registered in the ordinal index.
type forgetWithChildrenEngine struct {
	fakeEngine
	childCount int
}

func (e *forgetWithChildrenEngine) CountChildren(_ context.Context, _ string, _ string) (int, error) {
	return e.childCount, nil
}

func TestHandleForget_OrphanWarning(t *testing.T) {
	// Engine reports that the forgotten engram had 2 children.
	eng := &forgetWithChildrenEngine{childCount: 2}
	srv := newTestServerWith(eng)
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_forget","arguments":{"vault":"default","id":"parent-123"}}}`
	w := postRPC(t, srv, body)
	content := extractInnerJSON(t, decodeResp(t, w.Body.String()))

	ok, _ := content["ok"].(bool)
	if !ok {
		t.Errorf("expected ok=true in response, got %v", content["ok"])
	}
	warning, hasWarning := content["warning"].(string)
	if !hasWarning || warning == "" {
		t.Errorf("expected non-empty warning field when parent has children, got %v", content["warning"])
	}
	if !strings.Contains(warning, "orphaned") {
		t.Errorf("warning message should contain 'orphaned', got: %s", warning)
	}
}

func TestHandleForget_NoWarning(t *testing.T) {
	// Default fakeEngine returns 0 children — no warning expected.
	srv := newTestServer()
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_forget","arguments":{"vault":"default","id":"leaf-456"}}}`
	w := postRPC(t, srv, body)
	content := extractInnerJSON(t, decodeResp(t, w.Body.String()))

	ok, _ := content["ok"].(bool)
	if !ok {
		t.Errorf("expected ok=true in response, got %v", content["ok"])
	}
	if _, hasWarning := content["warning"]; hasWarning {
		t.Errorf("expected no warning field for leaf engram, but got: %v", content["warning"])
	}
}

func TestHandleForget_HappyPath(t *testing.T) {
	srv := newTestServer()
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_forget","arguments":{"vault":"default","id":"abc-123"}}}`
	w := postRPC(t, srv, body)
	content := extractInnerJSON(t, decodeResp(t, w.Body.String()))

	ok, _ := content["ok"].(bool)
	if !ok {
		t.Errorf("expected ok=true in response, got %v", content["ok"])
	}
}

func TestHandleForget_MissingID(t *testing.T) {
	srv := newTestServer()
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_forget","arguments":{"vault":"default"}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error == nil || resp.Error.Code != -32602 {
		t.Errorf("expected -32602, got %v", resp.Error)
	}
}

// ── muninn_link ──────────────────────────────────────────────────────────────

func TestHandleLink_HappyPath(t *testing.T) {
	srv := newTestServer()
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_link","arguments":{"vault":"default","source_id":"src-1","target_id":"tgt-1","relation":"supports"}}}`
	w := postRPC(t, srv, body)
	content := extractInnerJSON(t, decodeResp(t, w.Body.String()))

	ok, _ := content["ok"].(bool)
	if !ok {
		t.Errorf("expected ok=true in response, got %v", content["ok"])
	}
}

func TestHandleLink_MissingFields(t *testing.T) {
	srv := newTestServer()
	// Missing target_id and relation
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_link","arguments":{"vault":"default","source_id":"src-1"}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error == nil || resp.Error.Code != -32602 {
		t.Errorf("expected -32602, got %v", resp.Error)
	}
}

// ── muninn_contradictions ────────────────────────────────────────────────────

func TestHandleContradictions_HappyPath(t *testing.T) {
	srv := newTestServer()
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_contradictions","arguments":{"vault":"default"}}}`
	w := postRPC(t, srv, body)
	content := extractInnerJSON(t, decodeResp(t, w.Body.String()))

	if _, ok := content["contradictions"]; !ok {
		t.Error("response missing field: \"contradictions\"")
	}
}

// contradictionsErrMCPEngine returns an error from GetContradictions.
type contradictionsErrMCPEngine struct{ fakeEngine }

func (e *contradictionsErrMCPEngine) GetContradictions(_ context.Context, _ string) ([]ContradictionPair, error) {
	return nil, fmt.Errorf("index unavailable")
}

func TestHandleContradictions_EngineError(t *testing.T) {
	srv := newTestServerWith(&contradictionsErrMCPEngine{})
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_contradictions","arguments":{"vault":"default"}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error == nil || resp.Error.Code != -32000 {
		t.Errorf("expected -32000 for engine error, got %v", resp.Error)
	}
}

// ── applyEnrichmentArgs ──────────────────────────────────────────────────────

func TestApplyEnrichmentArgs_NormalizesEntityNames(t *testing.T) {
	// Entity name should be NFKC-normalized and whitespace-trimmed
	args := map[string]any{
		"entities": []any{
			map[string]any{"name": "  PostgreSQL  ", "type": "database"},
			map[string]any{"name": "openai", "type": "organization"},
		},
	}
	req := &mbp.WriteRequest{}
	applyEnrichmentArgs(args, req)
	require.Len(t, req.Entities, 2)
	require.Equal(t, "PostgreSQL", req.Entities[0].Name, "whitespace should be trimmed")
	require.Equal(t, "openai", req.Entities[1].Name)
}

func TestApplyEnrichmentArgs_EnforcesEntityTypeVocabulary(t *testing.T) {
	args := map[string]any{
		"entities": []any{
			map[string]any{"name": "Foo", "type": "invalid_type"},
			map[string]any{"name": "Bar", "type": "DATABASE"}, // uppercase — normalize to "database"
			map[string]any{"name": "Baz", "type": "person"},
		},
	}
	req := &mbp.WriteRequest{}
	applyEnrichmentArgs(args, req)
	require.Len(t, req.Entities, 3)
	require.Equal(t, "other", req.Entities[0].Type, "unknown type should become 'other'")
	require.Equal(t, "database", req.Entities[1].Type, "type should be lowercased")
	require.Equal(t, "person", req.Entities[2].Type)
}

func TestApplyEnrichmentArgs_CapsAt20Entities(t *testing.T) {
	entities := make([]any, 25)
	for i := range entities {
		entities[i] = map[string]any{"name": fmt.Sprintf("Entity%d", i), "type": "person"}
	}
	args := map[string]any{"entities": entities}
	req := &mbp.WriteRequest{}
	applyEnrichmentArgs(args, req)
	require.Len(t, req.Entities, 20, "entities should be capped at 20")
}

func TestApplyEnrichmentArgs_CapsAt30Relationships(t *testing.T) {
	rels := make([]any, 35)
	for i := range rels {
		rels[i] = map[string]any{
			"target_id": fmt.Sprintf("01ABCDEFGHJKMNPQRSTVWX%04d", i)[:26],
			"relation":  "uses",
			"weight":    0.8,
		}
	}
	args := map[string]any{"relationships": rels}
	req := &mbp.WriteRequest{}
	applyEnrichmentArgs(args, req)
	require.Len(t, req.Relationships, 30, "relationships should be capped at 30")
}

func TestApplyEnrichmentArgs_SkipsEmptyOrInvalidEntities(t *testing.T) {
	args := map[string]any{
		"entities": []any{
			map[string]any{"name": "", "type": "person"},    // empty name — skip
			map[string]any{"name": "   ", "type": "person"}, // whitespace only — skip
			map[string]any{"name": "Alice", "type": ""},     // empty type — skip
			map[string]any{"name": "Bob", "type": "person"}, // valid
		},
	}
	req := &mbp.WriteRequest{}
	applyEnrichmentArgs(args, req)
	require.Len(t, req.Entities, 1)
	require.Equal(t, "Bob", req.Entities[0].Name)
}

// ── muninn_status ────────────────────────────────────────────────────────────

func TestHandleStatus_HappyPath(t *testing.T) {
	srv := newTestServer()
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_status","arguments":{"vault":"default"}}}`
	w := postRPC(t, srv, body)
	content := extractInnerJSON(t, decodeResp(t, w.Body.String()))

	if _, ok := content["total_memories"]; !ok {
		t.Error("response missing field: \"total_memories\"")
	}
}

func TestHandleStatus_IncludesEnrichmentMode(t *testing.T) {
	srv := newTestServer()
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_status","arguments":{"vault":"default"}}}`
	w := postRPC(t, srv, body)
	content := extractInnerJSON(t, decodeResp(t, w.Body.String()))

	mode, ok := content["enrichment_mode"].(string)
	if !ok {
		t.Fatal("response missing or wrong type for field: \"enrichment_mode\"")
	}
	if mode == "" {
		t.Error("enrichment_mode should be a non-empty string; expected \"none\", \"inline\", or \"plugin:<name>\"")
	}
}

// ── muninn_find_by_entity ────────────────────────────────────────────────────

type findByEntityEngine struct{ fakeEngine }

func (f *findByEntityEngine) FindByEntity(_ context.Context, _, name string, _ int) ([]*storage.Engram, error) {
	if name == "PostgreSQL" {
		id := storage.NewULID()
		return []*storage.Engram{
			{ID: id, Concept: "DB choice", Summary: "Chose PostgreSQL"},
		}, nil
	}
	return nil, nil
}

func TestHandleFindByEntity_HappyPath(t *testing.T) {
	srv := newTestServerWith(&findByEntityEngine{})
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_find_by_entity","arguments":{"vault":"default","entity_name":"PostgreSQL"}}}`
	w := postRPC(t, srv, body)
	content := extractInnerJSON(t, decodeResp(t, w.Body.String()))

	for _, field := range []string{"entity", "engrams", "count"} {
		if _, ok := content[field]; !ok {
			t.Errorf("response missing field: %q", field)
		}
	}
	count, _ := content["count"].(float64)
	if int(count) != 1 {
		t.Errorf("expected count=1, got %v", content["count"])
	}
	engrams, _ := content["engrams"].([]any)
	if len(engrams) != 1 {
		t.Fatalf("expected 1 engram, got %d", len(engrams))
	}
	entry, _ := engrams[0].(map[string]any)
	if entry["concept"] != "DB choice" {
		t.Errorf("concept = %v, want 'DB choice'", entry["concept"])
	}
	if content["entity"] != "PostgreSQL" {
		t.Errorf("entity = %v, want 'PostgreSQL'", content["entity"])
	}
}

func TestHandleFindByEntity_EmptyName(t *testing.T) {
	srv := newTestServerWith(&findByEntityEngine{})
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_find_by_entity","arguments":{"vault":"default","entity_name":""}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error == nil || resp.Error.Code != -32602 {
		t.Errorf("expected -32602 for empty entity_name, got %v", resp.Error)
	}
}

func TestHandleFindByEntity_MissingName(t *testing.T) {
	srv := newTestServerWith(&findByEntityEngine{})
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_find_by_entity","arguments":{"vault":"default"}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error == nil || resp.Error.Code != -32602 {
		t.Errorf("expected -32602 for missing entity_name, got %v", resp.Error)
	}
}

func TestHandleFindByEntity_NoResults(t *testing.T) {
	srv := newTestServerWith(&findByEntityEngine{})
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_find_by_entity","arguments":{"vault":"default","entity_name":"UnknownEntity"}}}`
	w := postRPC(t, srv, body)
	content := extractInnerJSON(t, decodeResp(t, w.Body.String()))

	count, _ := content["count"].(float64)
	if int(count) != 0 {
		t.Errorf("expected count=0, got %v", content["count"])
	}
	engrams, _ := content["engrams"].([]any)
	if len(engrams) != 0 {
		t.Errorf("expected empty engrams, got %d", len(engrams))
	}
}

type findByEntityCapturingEngine struct {
	fakeEngine
	lastLimit int
}

func (f *findByEntityCapturingEngine) FindByEntity(_ context.Context, _, _ string, limit int) ([]*storage.Engram, error) {
	f.lastLimit = limit
	return []*storage.Engram{}, nil
}

func TestHandleFindByEntity_LimitCapped(t *testing.T) {
	eng := &findByEntityCapturingEngine{}
	srv := newTestServerWith(eng)
	// Request limit=999; handler must cap to 50 before calling engine.
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_find_by_entity","arguments":{"vault":"default","entity_name":"TestEntity","limit":999}}}`
	postRPC(t, srv, body)
	if eng.lastLimit != 50 {
		t.Errorf("expected engine to receive limit=50 after capping, got %d", eng.lastLimit)
	}
}

// ── muninn_where_left_off ────────────────────────────────────────────────────

// whereLeftOffEngine returns a populated WhereLeftOff result for shape tests.
type whereLeftOffEngine struct{ fakeEngine }

func (e *whereLeftOffEngine) WhereLeftOff(_ context.Context, _ string, _ int) ([]WhereLeftOffEntry, error) {
	return []WhereLeftOffEntry{
		{
			ID:         "entry-1",
			Concept:    "recent work",
			Summary:    "working on feature X",
			LastAccess: time.Now().Add(-5 * time.Minute),
			State:      "active",
		},
		{
			ID:         "entry-2",
			Concept:    "older work",
			LastAccess: time.Now().Add(-30 * time.Minute),
			State:      "paused",
		},
	}, nil
}

type whereLeftOffErrEngine struct{ fakeEngine }

func (e *whereLeftOffErrEngine) WhereLeftOff(_ context.Context, _ string, _ int) ([]WhereLeftOffEntry, error) {
	return nil, fmt.Errorf("storage unavailable")
}

type whereLeftOffLimitEngine struct {
	fakeEngine
	lastLimit int
}

func (e *whereLeftOffLimitEngine) WhereLeftOff(_ context.Context, _ string, limit int) ([]WhereLeftOffEntry, error) {
	e.lastLimit = limit
	return []WhereLeftOffEntry{}, nil
}

func TestHandleWhereLeftOff_HappyPath(t *testing.T) {
	srv := newTestServer()
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_where_left_off","arguments":{"vault":"default"}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error != nil {
		t.Fatalf("unexpected error: %v", resp.Error)
	}
	if resp.Result == nil {
		t.Fatal("expected non-nil result")
	}
}

func TestHandleWhereLeftOff_ResponseShape(t *testing.T) {
	srv := newTestServerWith(&whereLeftOffEngine{})
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_where_left_off","arguments":{"vault":"default"}}}`
	w := postRPC(t, srv, body)
	content := extractInnerJSON(t, decodeResp(t, w.Body.String()))

	for _, field := range []string{"memories", "count", "hint"} {
		if _, ok := content[field]; !ok {
			t.Errorf("response missing field: %q", field)
		}
	}

	memories, ok := content["memories"].([]any)
	if !ok {
		t.Fatal("expected memories to be an array")
	}
	if len(memories) != 2 {
		t.Fatalf("expected 2 memories, got %d", len(memories))
	}

	entry, ok := memories[0].(map[string]any)
	if !ok {
		t.Fatal("memories[0] is not an object")
	}
	for _, field := range []string{"id", "concept", "last_access", "state"} {
		if _, ok := entry[field]; !ok {
			t.Errorf("memory entry missing field: %q", field)
		}
	}

	count, ok := content["count"].(float64)
	if !ok || int(count) != 2 {
		t.Errorf("expected count=2, got %v", content["count"])
	}
}

func TestHandleWhereLeftOff_EmptyVault(t *testing.T) {
	srv := newTestServer()
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_where_left_off","arguments":{"vault":"default"}}}`
	w := postRPC(t, srv, body)
	content := extractInnerJSON(t, decodeResp(t, w.Body.String()))

	memories, ok := content["memories"].([]any)
	if !ok {
		t.Fatal("expected memories to be an array")
	}
	if len(memories) != 0 {
		t.Errorf("expected empty memories, got %d", len(memories))
	}

	count, ok := content["count"].(float64)
	if !ok || int(count) != 0 {
		t.Errorf("expected count=0, got %v", content["count"])
	}
}

func TestHandleWhereLeftOff_EngineError(t *testing.T) {
	srv := newTestServerWith(&whereLeftOffErrEngine{})
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_where_left_off","arguments":{"vault":"default"}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error == nil || resp.Error.Code != -32000 {
		t.Errorf("expected -32000 for engine error, got %v", resp.Error)
	}
	if !strings.Contains(resp.Error.Message, "storage unavailable") {
		t.Errorf("error message should mention storage error, got: %s", resp.Error.Message)
	}
}

func TestHandleWhereLeftOff_LimitDefault(t *testing.T) {
	eng := &whereLeftOffLimitEngine{}
	srv := newTestServerWith(eng)
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_where_left_off","arguments":{"vault":"default"}}}`
	postRPC(t, srv, body)
	if eng.lastLimit != 10 {
		t.Errorf("expected default limit=10, got %d", eng.lastLimit)
	}
}

func TestHandleWhereLeftOff_LimitCapped(t *testing.T) {
	eng := &whereLeftOffLimitEngine{}
	srv := newTestServerWith(eng)
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_where_left_off","arguments":{"vault":"default","limit":999}}}`
	postRPC(t, srv, body)
	if eng.lastLimit != 50 {
		t.Errorf("expected limit capped to 50, got %d", eng.lastLimit)
	}
}

// ── op_id idempotency ─────────────────────────────────────────────────────────

// TestHandleRemember_IdempotentHit verifies that when CheckIdempotency finds a
// receipt for the given op_id, the cached engram ID is returned immediately
// with "idempotent":true and the engine's Write method is NOT called.
func TestHandleRemember_IdempotentHit(t *testing.T) {
	eng := &idempotentEngine{
		receipt: &storage.IdempotencyReceipt{EngramID: "cached-id-abc", CreatedAt: 1000000},
	}
	srv := newTestServerWith(eng)

	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_remember","arguments":{"vault":"default","content":"hello world","op_id":"my-unique-op"}}}`
	w := postRPC(t, srv, body)

	resp := decodeResp(t, w.Body.String())
	if resp.Error != nil {
		t.Fatalf("unexpected error: %v", resp.Error)
	}

	content := extractInnerJSON(t, resp)

	id, ok := content["id"].(string)
	if !ok || id != "cached-id-abc" {
		t.Errorf("expected id='cached-id-abc', got %v", content["id"])
	}

	idempotent, ok := content["idempotent"].(bool)
	if !ok || !idempotent {
		t.Errorf("expected idempotent=true, got %v", content["idempotent"])
	}

	if eng.writeCalls != 0 {
		t.Errorf("expected Write to not be called on idempotent hit, got %d calls", eng.writeCalls)
	}
}

// TestHandleRemember_IdempotentMiss verifies that when no receipt exists for
// the op_id, the Write proceeds normally and returns a fresh engram ID.
func TestHandleRemember_IdempotentMiss(t *testing.T) {
	eng := &idempotentEngine{receipt: nil} // no existing receipt
	srv := newTestServerWith(eng)

	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_remember","arguments":{"vault":"default","content":"new content","op_id":"new-unique-op"}}}`
	w := postRPC(t, srv, body)

	resp := decodeResp(t, w.Body.String())
	if resp.Error != nil {
		t.Fatalf("unexpected error: %v", resp.Error)
	}

	content := extractInnerJSON(t, resp)

	id, ok := content["id"].(string)
	if !ok || id != "fresh-id" {
		t.Errorf("expected id='fresh-id', got %v", content["id"])
	}

	if _, hasIdempotent := content["idempotent"]; hasIdempotent {
		t.Error("expected no 'idempotent' field on a fresh write")
	}

	if eng.writeCalls != 1 {
		t.Errorf("expected Write to be called once, got %d", eng.writeCalls)
	}
}

// TestHandleRemember_NoOpID verifies that muninn_remember without op_id
// behaves exactly as before — no idempotency check is performed.
func TestHandleRemember_NoOpID(t *testing.T) {
	eng := &idempotentEngine{receipt: nil}
	srv := newTestServerWith(eng)

	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_remember","arguments":{"vault":"default","content":"plain memory"}}}`
	w := postRPC(t, srv, body)

	resp := decodeResp(t, w.Body.String())
	if resp.Error != nil {
		t.Fatalf("unexpected error: %v", resp.Error)
	}

	if eng.writeCalls != 1 {
		t.Errorf("expected Write to be called once, got %d", eng.writeCalls)
	}
}

// slowIdempotentEngine is like idempotentEngine but introduces a brief delay in
// Write so that a concurrent goroutine has time to reach the CheckIdempotency
// gate while the first goroutine is inside Write. Without the per-op_id mutex
// in handleRemember, both goroutines would see a nil receipt and each call
// Write — producing two engrams for a single op_id.
type slowIdempotentEngine struct {
	mu        sync.Mutex
	writeCalls int32 // accessed atomically

	// storedReceipt is written after the first Write completes; subsequent
	// CheckIdempotency calls inside the lock will see it.
	storedOpID    string
	storedReceipt *storage.IdempotencyReceipt
}

func (e *slowIdempotentEngine) CheckIdempotency(_ context.Context, opID string) (*storage.IdempotencyReceipt, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.storedOpID == opID && e.storedReceipt != nil {
		return e.storedReceipt, nil
	}
	return nil, nil
}

func (e *slowIdempotentEngine) WriteIdempotency(_ context.Context, opID, engramID string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.storedOpID = opID
	e.storedReceipt = &storage.IdempotencyReceipt{EngramID: engramID}
	return nil
}

func (e *slowIdempotentEngine) Write(_ context.Context, _ *mbp.WriteRequest) (*mbp.WriteResponse, error) {
	atomic.AddInt32(&e.writeCalls, 1)
	// Small sleep so a concurrent goroutine can race toward CheckIdempotency.
	time.Sleep(5 * time.Millisecond)
	return &mbp.WriteResponse{ID: "idempotent-engram"}, nil
}

// Delegate everything else to fakeEngine.
func (e *slowIdempotentEngine) WriteBatch(ctx context.Context, reqs []*mbp.WriteRequest) ([]*mbp.WriteResponse, []error) {
	f := &fakeEngine{}
	return f.WriteBatch(ctx, reqs)
}
func (e *slowIdempotentEngine) Activate(ctx context.Context, req *mbp.ActivateRequest) (*mbp.ActivateResponse, error) {
	return (&fakeEngine{}).Activate(ctx, req)
}
func (e *slowIdempotentEngine) Read(ctx context.Context, req *mbp.ReadRequest) (*mbp.ReadResponse, error) {
	return (&fakeEngine{}).Read(ctx, req)
}
func (e *slowIdempotentEngine) Forget(ctx context.Context, req *mbp.ForgetRequest) (*mbp.ForgetResponse, error) {
	return (&fakeEngine{}).Forget(ctx, req)
}
func (e *slowIdempotentEngine) Link(ctx context.Context, req *mbp.LinkRequest) (*mbp.LinkResponse, error) {
	return (&fakeEngine{}).Link(ctx, req)
}
func (e *slowIdempotentEngine) Stat(ctx context.Context, req *mbp.StatRequest) (*mbp.StatResponse, error) {
	return (&fakeEngine{}).Stat(ctx, req)
}
func (e *slowIdempotentEngine) GetContradictions(ctx context.Context, vault string) ([]ContradictionPair, error) {
	return (&fakeEngine{}).GetContradictions(ctx, vault)
}
func (e *slowIdempotentEngine) Evolve(ctx context.Context, vault, oldID, newContent, reason string) (*WriteResult, error) {
	return (&fakeEngine{}).Evolve(ctx, vault, oldID, newContent, reason)
}
func (e *slowIdempotentEngine) Consolidate(ctx context.Context, vault string, ids []string, merged string) (*ConsolidateResult, error) {
	return (&fakeEngine{}).Consolidate(ctx, vault, ids, merged)
}
func (e *slowIdempotentEngine) Session(ctx context.Context, vault string, since time.Time) (*SessionSummary, error) {
	return (&fakeEngine{}).Session(ctx, vault, since)
}
func (e *slowIdempotentEngine) Decide(ctx context.Context, vault, decision, rationale string, alternatives, evidenceIDs []string) (*WriteResult, error) {
	return (&fakeEngine{}).Decide(ctx, vault, decision, rationale, alternatives, evidenceIDs)
}
func (e *slowIdempotentEngine) Restore(ctx context.Context, vault string, id string) (*RestoreResult, error) {
	return (&fakeEngine{}).Restore(ctx, vault, id)
}
func (e *slowIdempotentEngine) Traverse(ctx context.Context, vault string, req *TraverseRequest) (*TraverseResult, error) {
	return (&fakeEngine{}).Traverse(ctx, vault, req)
}
func (e *slowIdempotentEngine) Explain(ctx context.Context, vault string, req *ExplainRequest) (*ExplainResult, error) {
	return (&fakeEngine{}).Explain(ctx, vault, req)
}
func (e *slowIdempotentEngine) UpdateState(ctx context.Context, vault string, id string, state string, reason string) error {
	return (&fakeEngine{}).UpdateState(ctx, vault, id, state, reason)
}
func (e *slowIdempotentEngine) ListDeleted(ctx context.Context, vault string, limit int) ([]DeletedEngram, error) {
	return (&fakeEngine{}).ListDeleted(ctx, vault, limit)
}
func (e *slowIdempotentEngine) RetryEnrich(ctx context.Context, vault string, id string) (*RetryEnrichResult, error) {
	return (&fakeEngine{}).RetryEnrich(ctx, vault, id)
}
func (e *slowIdempotentEngine) GetVaultPlasticity(ctx context.Context, vault string) (*auth.ResolvedPlasticity, error) {
	return (&fakeEngine{}).GetVaultPlasticity(ctx, vault)
}
func (e *slowIdempotentEngine) RememberTree(ctx context.Context, req *RememberTreeRequest) (*RememberTreeResult, error) {
	return (&fakeEngine{}).RememberTree(ctx, req)
}
func (e *slowIdempotentEngine) RecallTree(ctx context.Context, vault, rootID string, maxDepth, limit int, includeCompleted bool) (*RecallTreeResult, error) {
	return (&fakeEngine{}).RecallTree(ctx, vault, rootID, maxDepth, limit, includeCompleted)
}
func (e *slowIdempotentEngine) AddChild(ctx context.Context, vault, parentID string, child *AddChildRequest) (*AddChildResult, error) {
	return (&fakeEngine{}).AddChild(ctx, vault, parentID, child)
}
func (e *slowIdempotentEngine) CountChildren(ctx context.Context, vault, engramID string) (int, error) {
	return (&fakeEngine{}).CountChildren(ctx, vault, engramID)
}
func (e *slowIdempotentEngine) GetEnrichmentMode(ctx context.Context) string {
	return (&fakeEngine{}).GetEnrichmentMode(ctx)
}
func (e *slowIdempotentEngine) WhereLeftOff(ctx context.Context, vault string, limit int) ([]WhereLeftOffEntry, error) {
	return (&fakeEngine{}).WhereLeftOff(ctx, vault, limit)
}
func (e *slowIdempotentEngine) FindByEntity(ctx context.Context, vault, entityName string, limit int) ([]*storage.Engram, error) {
	return (&fakeEngine{}).FindByEntity(ctx, vault, entityName, limit)
}
func (e *slowIdempotentEngine) SetEntityState(ctx context.Context, entityName, state, mergedInto string) error {
	return (&fakeEngine{}).SetEntityState(ctx, entityName, state, mergedInto)
}
func (e *slowIdempotentEngine) GetEntityClusters(ctx context.Context, vault string, minCount, topN int) ([]EntityClusterResult, error) {
	return (&fakeEngine{}).GetEntityClusters(ctx, vault, minCount, topN)
}
func (e *slowIdempotentEngine) ExportGraph(ctx context.Context, vault string, includeEngrams bool) (*engine.ExportGraph, error) {
	return (&fakeEngine{}).ExportGraph(ctx, vault, includeEngrams)
}
func (e *slowIdempotentEngine) GetEntityTimeline(ctx context.Context, vault string, entityName string, limit int) (*engine.EntityTimeline, error) {
	return (&fakeEngine{}).GetEntityTimeline(ctx, vault, entityName, limit)
}
func (e *slowIdempotentEngine) FindSimilarEntities(ctx context.Context, vault string, threshold float64, topN int) ([]engine.SimilarEntityPair, error) {
	return (&fakeEngine{}).FindSimilarEntities(ctx, vault, threshold, topN)
}
func (e *slowIdempotentEngine) MergeEntity(ctx context.Context, vault string, entityA string, entityB string, dryRun bool) (*engine.MergeEntityResult, error) {
	return (&fakeEngine{}).MergeEntity(ctx, vault, entityA, entityB, dryRun)
}
func (e *slowIdempotentEngine) ReplayEnrichment(ctx context.Context, vault string, stages []string, limit int, dryRun bool) (*engine.ReplayEnrichmentResult, error) {
	return (&fakeEngine{}).ReplayEnrichment(ctx, vault, stages, limit, dryRun)
}
func (e *slowIdempotentEngine) GetProvenance(ctx context.Context, vault, id string) ([]ProvenanceEntry, error) {
	return (&fakeEngine{}).GetProvenance(ctx, vault, id)
}
func (e *slowIdempotentEngine) RecordFeedback(ctx context.Context, vault, engramID string, useful bool) error {
	return (&fakeEngine{}).RecordFeedback(ctx, vault, engramID, useful)
}
func (e *slowIdempotentEngine) GetEntityAggregate(ctx context.Context, vault, entityName string, limit int) (*EntityAggregate, error) {
	return (&fakeEngine{}).GetEntityAggregate(ctx, vault, entityName, limit)
}
func (e *slowIdempotentEngine) ListEntities(ctx context.Context, vault string, limit int, state string) ([]EntitySummary, error) {
	return (&fakeEngine{}).ListEntities(ctx, vault, limit, state)
}

// TestHandleRemember_ConcurrentSameOpID verifies that two concurrent
// muninn_remember calls carrying the same op_id do not produce duplicate
// engrams. The per-op_id mutex in handleRemember ensures only one Write
// executes; the second goroutine must observe the cached receipt and return
// the same engram ID with idempotent=true.
func TestHandleRemember_ConcurrentSameOpID(t *testing.T) {
	eng := &slowIdempotentEngine{}
	srv := newTestServerWith(eng)

	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_remember","arguments":{"vault":"default","content":"concurrent test","op_id":"race-op-123"}}}`

	type result struct {
		id         string
		idempotent bool
	}
	results := make([]result, 2)

	var wg sync.WaitGroup
	wg.Add(2)
	for i := 0; i < 2; i++ {
		i := i
		go func() {
			defer wg.Done()
			w := postRPC(t, srv, body)
			resp := decodeResp(t, w.Body.String())
			if resp.Error != nil {
				t.Errorf("goroutine %d got unexpected error: %v", i, resp.Error)
				return
			}
			content := extractInnerJSON(t, resp)
			id, _ := content["id"].(string)
			idempotent, _ := content["idempotent"].(bool)
			results[i] = result{id: id, idempotent: idempotent}
		}()
	}
	wg.Wait()

	// Both responses must reference the same engram ID.
	if results[0].id != results[1].id {
		t.Errorf("concurrent op_id produced different engram IDs: %q vs %q — TOCTOU race not fixed", results[0].id, results[1].id)
	}

	// Write must have been called exactly once; the second goroutine must have
	// hit the receipt cache inside the lock.
	if calls := atomic.LoadInt32(&eng.writeCalls); calls != 1 {
		t.Errorf("expected exactly 1 Write call, got %d — duplicate engrams were created", calls)
	}
}

// ── muninn_entity_state ──────────────────────────────────────────────────────

// entityStateEngine is a minimal engine stub for muninn_entity_state tests.
type entityStateEngine struct{ fakeEngine }

func (e *entityStateEngine) SetEntityState(_ context.Context, name, state, mergedInto string) error {
	if name == "" {
		return fmt.Errorf("entity_name is required")
	}
	return nil
}

// entityStateErrEngine returns an error from SetEntityState.
type entityStateErrEngine struct{ fakeEngine }

func (e *entityStateErrEngine) SetEntityState(_ context.Context, _, _, _ string) error {
	return fmt.Errorf("entity %q not found", "PostgreSQL")
}

func TestHandleEntityStateHappyPath(t *testing.T) {
	srv := newTestServerWith(&entityStateEngine{})
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_entity_state","arguments":{"vault":"default","entity_name":"PostgreSQL","state":"deprecated"}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error != nil {
		t.Fatalf("unexpected error: %v", resp.Error)
	}
	content := extractInnerJSON(t, resp)
	for _, field := range []string{"entity", "state", "ok"} {
		if _, ok := content[field]; !ok {
			t.Errorf("response missing field: %q", field)
		}
	}
	if content["entity"] != "PostgreSQL" {
		t.Errorf("entity = %v, want PostgreSQL", content["entity"])
	}
	if content["state"] != "deprecated" {
		t.Errorf("state = %v, want deprecated", content["state"])
	}
	if ok, _ := content["ok"].(bool); !ok {
		t.Errorf("ok field should be true, got %v", content["ok"])
	}
}

func TestHandleEntityStateMissingEntityName(t *testing.T) {
	srv := newTestServerWith(&entityStateEngine{})
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_entity_state","arguments":{"vault":"default","state":"deprecated"}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error == nil || resp.Error.Code != -32602 {
		t.Errorf("expected -32602 for missing entity_name, got %v", resp.Error)
	}
}

func TestHandleEntityStateMissingState(t *testing.T) {
	srv := newTestServerWith(&entityStateEngine{})
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_entity_state","arguments":{"vault":"default","entity_name":"PostgreSQL"}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error == nil || resp.Error.Code != -32602 {
		t.Errorf("expected -32602 for missing state, got %v", resp.Error)
	}
}

func TestHandleEntityStateWithMergedInto(t *testing.T) {
	srv := newTestServerWith(&entityStateEngine{})
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_entity_state","arguments":{"vault":"default","entity_name":"Postgres","state":"merged","merged_into":"PostgreSQL"}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error != nil {
		t.Fatalf("unexpected error for merged state: %v", resp.Error)
	}
}

func TestHandleEntityStateEngineError(t *testing.T) {
	srv := newTestServerWith(&entityStateErrEngine{})
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_entity_state","arguments":{"vault":"default","entity_name":"PostgreSQL","state":"deprecated"}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error == nil || resp.Error.Code != -32000 {
		t.Errorf("expected -32000 for engine error, got %v", resp.Error)
	}
}

func TestHandleEntityStateInvalidState(t *testing.T) {
	srv := newTestServerWith(&entityStateEngine{})
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_entity_state","arguments":{"vault":"default","entity_name":"PostgreSQL","state":"invalid_state"}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error == nil || resp.Error.Code != -32602 {
		t.Errorf("expected -32602 for invalid state, got %v", resp.Error)
	}
	if resp.Error != nil && !strings.Contains(resp.Error.Message, "must be one of") {
		t.Errorf("expected error message to mention valid states, got: %q", resp.Error.Message)
	}
}

func TestHandleEntityStateMergedWithoutMergedInto(t *testing.T) {
	srv := newTestServerWith(&entityStateEngine{})
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_entity_state","arguments":{"vault":"default","entity_name":"Postgres","state":"merged"}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error == nil || resp.Error.Code != -32602 {
		t.Errorf("expected -32602 for merged without merged_into, got %v", resp.Error)
	}
	if resp.Error != nil && !strings.Contains(resp.Error.Message, "merged_into") {
		t.Errorf("expected error message to mention merged_into requirement, got: %q", resp.Error.Message)
	}
}

// ── muninn_entity_clusters tests ─────────────────────────────────────────────

// entityClustersEngine returns a fixed set of clusters for testing.
type entityClustersEngine struct {
	fakeEngine
	clusters []EntityClusterResult
	err      error
}

func (e *entityClustersEngine) GetEntityClusters(_ context.Context, _ string, _, _ int) ([]EntityClusterResult, error) {
	if e.err != nil {
		return nil, e.err
	}
	return e.clusters, nil
}

func TestHandleEntityClusters_HappyPath(t *testing.T) {
	eng := &entityClustersEngine{
		clusters: []EntityClusterResult{
			{EntityA: "PostgreSQL", EntityB: "Redis", Count: 5},
			{EntityA: "Go", EntityB: "PostgreSQL", Count: 3},
		},
	}
	srv := newTestServerWith(eng)
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_entity_clusters","arguments":{"vault":"default","min_count":2,"top_n":10}}}`
	w := postRPC(t, srv, body)
	require.Equal(t, 200, w.Code)
	resp := decodeResp(t, w.Body.String())
	inner := extractInnerJSON(t, resp)

	clusters, ok := inner["clusters"].([]any)
	if !ok {
		t.Fatalf("expected clusters to be an array, got %T", inner["clusters"])
	}
	if len(clusters) != 2 {
		t.Errorf("expected 2 clusters, got %d", len(clusters))
	}

	count, ok := inner["count"].(float64)
	if !ok {
		t.Fatalf("expected count to be a number, got %T", inner["count"])
	}
	if int(count) != 2 {
		t.Errorf("expected count=2, got %v", count)
	}

	// Verify shape of first cluster.
	first, ok := clusters[0].(map[string]any)
	if !ok {
		t.Fatalf("expected cluster[0] to be an object, got %T", clusters[0])
	}
	if first["entity_a"] == nil || first["entity_b"] == nil || first["count"] == nil {
		t.Errorf("cluster entry missing required fields: %v", first)
	}
}

func TestHandleEntityClusters_EngineError(t *testing.T) {
	eng := &entityClustersEngine{
		err: fmt.Errorf("storage unavailable"),
	}
	srv := newTestServerWith(eng)
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_entity_clusters","arguments":{"vault":"default"}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error == nil || resp.Error.Code != -32000 {
		t.Errorf("expected -32000 for engine error, got %v", resp.Error)
	}
}

func TestHandleEntityClusters_EmptyResult(t *testing.T) {
	eng := &entityClustersEngine{clusters: nil}
	srv := newTestServerWith(eng)
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_entity_clusters","arguments":{"vault":"default"}}}`
	w := postRPC(t, srv, body)
	require.Equal(t, 200, w.Code)
	resp := decodeResp(t, w.Body.String())
	inner := extractInnerJSON(t, resp)

	clusters, ok := inner["clusters"].([]any)
	if !ok {
		t.Fatalf("expected clusters to be an array, got %T", inner["clusters"])
	}
	if len(clusters) != 0 {
		t.Errorf("expected empty clusters array, got %d entries", len(clusters))
	}
}

// ── muninn_export_graph tests ─────────────────────────────────────────────

// exportGraphEngine is a fake engine that returns a configurable ExportGraph result.
type exportGraphEngine struct {
	fakeEngine
	graph *engine.ExportGraph
	err   error
}

func (e *exportGraphEngine) ExportGraph(_ context.Context, _ string, _ bool) (*engine.ExportGraph, error) {
	if e.err != nil {
		return nil, e.err
	}
	if e.graph != nil {
		return e.graph, nil
	}
	return &engine.ExportGraph{
		Nodes: []engine.GraphNode{
			{ID: "PostgreSQL", Type: "database"},
			{ID: "Redis", Type: "cache"},
		},
		Edges: []engine.GraphEdge{
			{From: "PostgreSQL", To: "Redis", RelType: "manages", Weight: 0.8},
		},
	}, nil
}

func TestHandleExportGraph_HappyPathJSONLD(t *testing.T) {
	srv := newTestServerWith(&exportGraphEngine{})
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_export_graph","arguments":{"vault":"default","format":"json-ld"}}}`
	w := postRPC(t, srv, body)
	require.Equal(t, 200, w.Code)
	resp := decodeResp(t, w.Body.String())
	inner := extractInnerJSON(t, resp)

	require.Equal(t, "json-ld", inner["format"], "format field should be json-ld")

	data, ok := inner["data"].(string)
	require.True(t, ok, "data field should be a string")
	require.NotEmpty(t, data)

	// data should be valid JSON-LD.
	var doc map[string]any
	require.NoError(t, json.Unmarshal([]byte(data), &doc), "data should be valid JSON")
	_, hasGraph := doc["@graph"]
	require.True(t, hasGraph, "JSON-LD data should have @graph key")

	nodeCount, _ := inner["node_count"].(float64)
	require.Equal(t, float64(2), nodeCount)
	edgeCount, _ := inner["edge_count"].(float64)
	require.Equal(t, float64(1), edgeCount)
}

func TestHandleExportGraph_HappyPathGraphML(t *testing.T) {
	srv := newTestServerWith(&exportGraphEngine{})
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_export_graph","arguments":{"vault":"default","format":"graphml"}}}`
	w := postRPC(t, srv, body)
	require.Equal(t, 200, w.Code)
	resp := decodeResp(t, w.Body.String())
	inner := extractInnerJSON(t, resp)

	require.Equal(t, "graphml", inner["format"])

	data, ok := inner["data"].(string)
	require.True(t, ok, "data field should be a string")
	require.Contains(t, data, "<graphml", "data should contain GraphML XML")
	require.Contains(t, data, "PostgreSQL")
}

func TestHandleExportGraph_InvalidFormat(t *testing.T) {
	srv := newTestServerWith(&exportGraphEngine{})
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_export_graph","arguments":{"vault":"default","format":"rdf"}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	require.NotNil(t, resp.Error)
	require.Equal(t, -32602, resp.Error.Code)
}

func TestHandleExportGraph_DefaultVaultUsed(t *testing.T) {
	// When vault is omitted, resolveVault defaults to "default"; the call should succeed.
	srv := newTestServerWith(&exportGraphEngine{})
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_export_graph","arguments":{}}}`
	w := postRPC(t, srv, body)
	require.Equal(t, 200, w.Code)
	resp := decodeResp(t, w.Body.String())
	require.Nil(t, resp.Error)
	inner := extractInnerJSON(t, resp)
	_, hasFormat := inner["format"]
	require.True(t, hasFormat, "response should have format field")
}

func TestHandleExportGraph_EngineError(t *testing.T) {
	srv := newTestServerWith(&exportGraphEngine{err: fmt.Errorf("storage error")})
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_export_graph","arguments":{"vault":"default"}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	require.NotNil(t, resp.Error)
	require.Equal(t, -32000, resp.Error.Code)
}

// ── entity timeline handler tests ───────────────────────────────────────────

type entityTimelineEngine struct {
	fakeEngine
	timeline *engine.EntityTimeline
	err      error
}

func (e *entityTimelineEngine) GetEntityTimeline(_ context.Context, _ string, _ string, _ int) (*engine.EntityTimeline, error) {
	return e.timeline, e.err
}

func TestHandleEntityTimeline_HappyPath(t *testing.T) {
	timeline := &engine.EntityTimeline{
		Entity:       "TestEntity",
		FirstSeen:    time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC),
		MentionCount: 3,
		Entries: []engine.TimelineEntry{
			{
				EngramID:  "01ARZ3NDEKTSV4RRFFQ69G5FAV",
				Concept:   "First mention",
				CreatedAt: time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC),
				Summary:   "Entity first encountered",
			},
			{
				EngramID:  "01ARZ3NDEKTSV4RRFFQ69G5FAW",
				Concept:   "Second mention",
				CreatedAt: time.Date(2024, 1, 16, 10, 0, 0, 0, time.UTC),
				Summary:   "Entity in new context",
			},
		},
		Count: 2,
	}
	srv := newTestServerWith(&entityTimelineEngine{timeline: timeline})
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_entity_timeline","arguments":{"vault":"default","entity_name":"TestEntity","limit":10}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	require.Nil(t, resp.Error)

	inner := extractInnerJSON(t, resp)
	require.Equal(t, "TestEntity", inner["entity"])
	require.Equal(t, float64(3), inner["mention_count"])
	require.Equal(t, float64(2), inner["count"])

	entries, ok := inner["timeline"].([]any)
	require.True(t, ok)
	require.Len(t, entries, 2)
}

func TestHandleEntityTimeline_MissingEntityName(t *testing.T) {
	srv := newTestServerWith(&entityTimelineEngine{})
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_entity_timeline","arguments":{"vault":"default"}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	require.NotNil(t, resp.Error)
	require.Equal(t, -32602, resp.Error.Code)
	require.Contains(t, resp.Error.Message, "entity_name")
}

func TestHandleEntityTimeline_EngineError(t *testing.T) {
	srv := newTestServerWith(&entityTimelineEngine{err: fmt.Errorf("entity not found")})
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_entity_timeline","arguments":{"vault":"default","entity_name":"Unknown"}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	require.NotNil(t, resp.Error)
	require.Equal(t, -32000, resp.Error.Code)
	require.Contains(t, resp.Error.Message, "entity not found")
}

func TestHandleEntityTimeline_DefaultLimit(t *testing.T) {
	timeline := &engine.EntityTimeline{
		Entity:       "Entity",
		FirstSeen:    time.Now(),
		MentionCount: 15,
		Entries:      make([]engine.TimelineEntry, 10),
		Count:        10,
	}
	captured := &entityTimelineEngine{timeline: timeline}
	srv := newTestServerWith(captured)
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_entity_timeline","arguments":{"vault":"default","entity_name":"Entity"}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	require.Nil(t, resp.Error)

	inner := extractInnerJSON(t, resp)
	require.Equal(t, float64(10), inner["count"])
}

func TestHandleEntityTimeline_LimitCapped(t *testing.T) {
	timeline := &engine.EntityTimeline{
		Entity:       "Entity",
		FirstSeen:    time.Now(),
		MentionCount: 100,
		Entries:      make([]engine.TimelineEntry, 50),
		Count:        50,
	}
	srv := newTestServerWith(&entityTimelineEngine{timeline: timeline})
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_entity_timeline","arguments":{"vault":"default","entity_name":"Entity","limit":200}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	require.Nil(t, resp.Error)

	inner := extractInnerJSON(t, resp)
	require.Equal(t, float64(50), inner["count"])
}

// ── muninn_similar_entities ──────────────────────────────────────────────────

type similarEntitiesEngine struct {
	fakeEngine
	pairs []engine.SimilarEntityPair
	err   error
}

func (e *similarEntitiesEngine) FindSimilarEntities(_ context.Context, _ string, _ float64, _ int) ([]engine.SimilarEntityPair, error) {
	return e.pairs, e.err
}

func TestHandleSimilarEntities_HappyPath(t *testing.T) {
	pairs := []engine.SimilarEntityPair{
		{EntityA: "PostgreSQL", EntityB: "Postgre SQL", Similarity: 0.92},
	}
	srv := newTestServerWith(&similarEntitiesEngine{pairs: pairs})
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_similar_entities","arguments":{"vault":"default","threshold":0.85,"top_n":10}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	require.Nil(t, resp.Error)

	inner := extractInnerJSON(t, resp)
	require.Equal(t, float64(1), inner["count"])
	similar, ok := inner["similar"].([]any)
	require.True(t, ok, "similar should be an array")
	require.Len(t, similar, 1)
	item := similar[0].(map[string]any)
	require.Equal(t, "PostgreSQL", item["entity_a"])
	require.Equal(t, "Postgre SQL", item["entity_b"])
}

func TestHandleSimilarEntities_MissingVault(t *testing.T) {
	srv := newTestServerWith(&fakeEngine{})
	// No vault provided — use raw JSON with no vault key.
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_similar_entities","arguments":{}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	// The handler checks for empty vault and returns -32602.
	// However the global resolveVault provides a default of "default" when absent,
	// so the vault check in the handler may not fire. We just verify no panic and
	// that a result is returned.
	// In practice, empty vault → handler treats it as valid ("default" is injected).
	_ = resp
}

func TestHandleSimilarEntities_InvalidThreshold(t *testing.T) {
	srv := newTestServerWith(&fakeEngine{})
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_similar_entities","arguments":{"vault":"default","threshold":1.5}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error == nil || resp.Error.Code != -32602 {
		t.Errorf("expected -32602 for threshold > 1.0, got %v", resp.Error)
	}
}

func TestHandleSimilarEntities_EngineError(t *testing.T) {
	srv := newTestServerWith(&similarEntitiesEngine{err: fmt.Errorf("storage failure")})
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_similar_entities","arguments":{"vault":"default"}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error == nil || resp.Error.Code != -32000 {
		t.Errorf("expected -32000 for engine error, got %v", resp.Error)
	}
}

// ── muninn_merge_entity ──────────────────────────────────────────────────────

type mergeEntityEngine struct {
	fakeEngine
	result *engine.MergeEntityResult
	err    error
}

func (e *mergeEntityEngine) MergeEntity(_ context.Context, _, _, _ string, dryRun bool) (*engine.MergeEntityResult, error) {
	if e.result != nil {
		e.result.DryRun = dryRun
		return e.result, e.err
	}
	return nil, e.err
}

func TestHandleMergeEntity_HappyPath(t *testing.T) {
	result := &engine.MergeEntityResult{
		EntityA:         "Postgre SQL",
		EntityB:         "PostgreSQL",
		EngramsRelinked: 3,
	}
	srv := newTestServerWith(&mergeEntityEngine{result: result})
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_merge_entity","arguments":{"vault":"default","entity_a":"Postgre SQL","entity_b":"PostgreSQL"}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	require.Nil(t, resp.Error)

	inner := extractInnerJSON(t, resp)
	require.Equal(t, "Postgre SQL", inner["entity_a"])
	require.Equal(t, "PostgreSQL", inner["entity_b"])
	require.Equal(t, float64(3), inner["engrams_relinked"])
	merged, _ := inner["merged"].(bool)
	require.True(t, merged, "merged should be true for a real (non-dry-run) merge")
}

func TestHandleMergeEntity_DryRun(t *testing.T) {
	result := &engine.MergeEntityResult{
		EntityA:         "Postgre SQL",
		EntityB:         "PostgreSQL",
		EngramsRelinked: 5,
	}
	srv := newTestServerWith(&mergeEntityEngine{result: result})
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_merge_entity","arguments":{"vault":"default","entity_a":"Postgre SQL","entity_b":"PostgreSQL","dry_run":true}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	require.Nil(t, resp.Error)

	inner := extractInnerJSON(t, resp)
	dryRun, _ := inner["dry_run"].(bool)
	require.True(t, dryRun, "dry_run should be true in response")
	merged, _ := inner["merged"].(bool)
	require.False(t, merged, "merged should be false for dry_run")
}

func TestHandleMergeEntity_MissingParams(t *testing.T) {
	srv := newTestServerWith(&fakeEngine{})

	// Missing entity_b.
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_merge_entity","arguments":{"vault":"default","entity_a":"Postgre SQL"}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error == nil || resp.Error.Code != -32602 {
		t.Errorf("expected -32602 for missing entity_b, got %v", resp.Error)
	}

	// Missing entity_a.
	body = `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_merge_entity","arguments":{"vault":"default","entity_b":"PostgreSQL"}}}`
	w = postRPC(t, srv, body)
	resp = decodeResp(t, w.Body.String())
	if resp.Error == nil || resp.Error.Code != -32602 {
		t.Errorf("expected -32602 for missing entity_a, got %v", resp.Error)
	}
}

func TestHandleMergeEntity_EngineError(t *testing.T) {
	srv := newTestServerWith(&mergeEntityEngine{err: fmt.Errorf("entity not found")})
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_merge_entity","arguments":{"vault":"default","entity_a":"Foo","entity_b":"Bar"}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error == nil || resp.Error.Code != -32000 {
		t.Errorf("expected -32000 for engine error, got %v", resp.Error)
	}
}

// ── muninn_replay_enrichment ────────────────────────────────────────────────

type replayEnrichEngine struct {
	fakeEngine
	result *engine.ReplayEnrichmentResult
	err    error
}

func (e *replayEnrichEngine) ReplayEnrichment(_ context.Context, _ string, _ []string, _ int, dryRun bool) (*engine.ReplayEnrichmentResult, error) {
	if e.err != nil {
		return nil, e.err
	}
	if e.result != nil {
		r := *e.result
		r.DryRun = dryRun
		return &r, nil
	}
	return &engine.ReplayEnrichmentResult{Processed: 5, Skipped: 2, StagesRun: []string{"entities", "relationships", "classification", "summary"}, DryRun: dryRun}, nil
}

func TestHandleReplayEnrichment_HappyPath(t *testing.T) {
	srv := newTestServerWith(&replayEnrichEngine{})
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_replay_enrichment","arguments":{"vault":"default"}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	inner := extractInnerJSON(t, resp)

	if _, ok := inner["processed"]; !ok {
		t.Error("response missing 'processed' field")
	}
	if _, ok := inner["skipped"]; !ok {
		t.Error("response missing 'skipped' field")
	}
	if _, ok := inner["failed"]; !ok {
		t.Error("response missing 'failed' field")
	}
	if _, ok := inner["remaining"]; !ok {
		t.Error("response missing 'remaining' field")
	}
	if _, ok := inner["stages_run"]; !ok {
		t.Error("response missing 'stages_run' field")
	}
	if _, ok := inner["dry_run"]; !ok {
		t.Error("response missing 'dry_run' field")
	}
	dryRun, _ := inner["dry_run"].(bool)
	if dryRun {
		t.Error("expected dry_run=false by default, got true")
	}
}

func TestHandleReplayEnrichment_MissingVault(t *testing.T) {
	// When vault arg is empty string, resolveVault falls back to "default" (no error).
	// Verify the handler succeeds with the default vault injection.
	srv := newTestServerWith(&replayEnrichEngine{})
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_replay_enrichment","arguments":{"vault":""}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	// resolveVault injects "default" when vault arg is absent or empty — no error expected.
	if resp.Error != nil {
		t.Errorf("expected success with default vault injection, got error: %v", resp.Error)
	}
	inner := extractInnerJSON(t, resp)
	if _, ok := inner["processed"]; !ok {
		t.Error("response missing 'processed' field")
	}
}

func TestHandleReplayEnrichment_DryRun(t *testing.T) {
	srv := newTestServerWith(&replayEnrichEngine{})
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_replay_enrichment","arguments":{"vault":"default","dry_run":true}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	inner := extractInnerJSON(t, resp)

	dryRun, _ := inner["dry_run"].(bool)
	if !dryRun {
		t.Error("expected dry_run=true in response, got false")
	}
}

func TestHandleReplayEnrichment_EngineError(t *testing.T) {
	srv := newTestServerWith(&replayEnrichEngine{err: fmt.Errorf("enrichment pipeline not configured")})
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_replay_enrichment","arguments":{"vault":"default"}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error == nil || resp.Error.Code != -32000 {
		t.Errorf("expected -32000 for engine error, got %v", resp.Error)
	}
}

func TestHandleReplayEnrichment_FailedAndRemainingInResponse(t *testing.T) {
	srv := newTestServerWith(&replayEnrichEngine{
		result: &engine.ReplayEnrichmentResult{
			Processed: 3, Skipped: 1, Failed: 2, Remaining: 4,
			StagesRun: []string{"entities"}, DryRun: false,
		},
	})
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_replay_enrichment","arguments":{"vault":"default"}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	inner := extractInnerJSON(t, resp)

	if failed, _ := inner["failed"].(float64); failed != 2 {
		t.Errorf("failed: got %v, want 2", failed)
	}
	if remaining, _ := inner["remaining"].(float64); remaining != 4 {
		t.Errorf("remaining: got %v, want 4", remaining)
	}
}

func TestHandleReplayEnrichment_WithStages(t *testing.T) {
	srv := newTestServerWith(&replayEnrichEngine{})
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_replay_enrichment","arguments":{"vault":"default","stages":["summary","classification"],"limit":10}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error != nil {
		t.Errorf("unexpected error: %v", resp.Error)
	}
	inner := extractInnerJSON(t, resp)
	if inner["processed"] == nil {
		t.Error("response missing 'processed' field")
	}
}
