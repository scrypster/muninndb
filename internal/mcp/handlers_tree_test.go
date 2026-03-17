package mcp

// handlers_tree_test.go — integration tests for the three hierarchical memory
// handlers: muninn_remember_tree, muninn_recall_tree, muninn_add_child.
//
// All tests use the same newTestServer() / postRPC() / decodeResp() /
// extractInnerJSON() helpers defined in server_test.go and handlers_test.go.

import (
	"context"
	"fmt"
	"testing"
)

// ── muninn_remember_tree ──────────────────────────────────────────────────────

// TestHandleRememberTree exercises the happy path with a 3-node tree
// (root + 2 children).
func TestHandleRememberTree(t *testing.T) {
	srv := newTestServer()
	body := `{
		"jsonrpc":"2.0","method":"tools/call","id":1,
		"params":{
			"name":"muninn_remember_tree",
			"arguments":{
				"vault":"default",
				"root":{
					"concept":"Project plan",
					"content":"Top-level project plan node",
					"children":[
						{"concept":"Phase 1","content":"Design phase"},
						{"concept":"Phase 2","content":"Implementation phase"}
					]
				}
			}
		}
	}`
	w := postRPC(t, srv, body)
	content := extractInnerJSON(t, decodeResp(t, w.Body.String()))

	rootID, ok := content["root_id"].(string)
	if !ok || rootID == "" {
		t.Errorf("expected non-empty root_id, got %v", content["root_id"])
	}

	nodeMap, ok := content["node_map"].(map[string]any)
	if !ok {
		t.Fatalf("expected node_map to be an object, got %T", content["node_map"])
	}
	// The fake engine returns an empty node_map ({}), so we only check it is present.
	_ = nodeMap
}

// TestHandleRememberTree_MissingRoot verifies that omitting the root argument
// returns error code -32602.
func TestHandleRememberTree_MissingRoot(t *testing.T) {
	srv := newTestServer()
	body := `{
		"jsonrpc":"2.0","method":"tools/call","id":1,
		"params":{
			"name":"muninn_remember_tree",
			"arguments":{"vault":"default"}
		}
	}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error == nil || resp.Error.Code != -32602 {
		t.Errorf("expected -32602, got %v", resp.Error)
	}
}

// ── muninn_recall_tree ────────────────────────────────────────────────────────

// TestHandleRecallTree stores a tree with muninn_remember_tree, extracts the
// root_id from the response, then calls muninn_recall_tree and verifies the
// returned tree root.
func TestHandleRecallTree(t *testing.T) {
	srv := newTestServer()

	// Step 1: store a small tree.
	rememberBody := `{
		"jsonrpc":"2.0","method":"tools/call","id":1,
		"params":{
			"name":"muninn_remember_tree",
			"arguments":{
				"vault":"default",
				"root":{
					"concept":"Sprint backlog",
					"content":"Current sprint tasks",
					"children":[
						{"concept":"Task A","content":"Implement login"},
						{"concept":"Task B","content":"Write tests"}
					]
				}
			}
		}
	}`
	rememberW := postRPC(t, srv, rememberBody)
	rememberContent := extractInnerJSON(t, decodeResp(t, rememberW.Body.String()))

	rootID, ok := rememberContent["root_id"].(string)
	if !ok || rootID == "" {
		t.Fatalf("remember_tree did not return a root_id: %v", rememberContent["root_id"])
	}

	// Step 2: recall the tree using the root_id.
	recallBody := `{
		"jsonrpc":"2.0","method":"tools/call","id":2,
		"params":{
			"name":"muninn_recall_tree",
			"arguments":{
				"vault":"default",
				"root_id":"` + rootID + `"
			}
		}
	}`
	recallW := postRPC(t, srv, recallBody)
	recallContent := extractInnerJSON(t, decodeResp(t, recallW.Body.String()))

	root, ok := recallContent["root"].(map[string]any)
	if !ok {
		t.Fatalf("expected root to be an object, got %T", recallContent["root"])
	}
	if root["id"] == nil || root["id"] == "" {
		t.Error("expected root.id to be non-empty")
	}
	if root["concept"] == nil {
		t.Error("expected root.concept to be present")
	}
}

// TestHandleRecallTree_MissingRootID verifies that omitting root_id returns
// error code -32602.
func TestHandleRecallTree_MissingRootID(t *testing.T) {
	srv := newTestServer()
	body := `{
		"jsonrpc":"2.0","method":"tools/call","id":1,
		"params":{
			"name":"muninn_recall_tree",
			"arguments":{"vault":"default"}
		}
	}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error == nil || resp.Error.Code != -32602 {
		t.Errorf("expected -32602, got %v", resp.Error)
	}
}

// ── muninn_add_child ──────────────────────────────────────────────────────────

// TestHandleAddChild stores a root engram with muninn_remember, then adds a
// child to it with muninn_add_child, and verifies the returned child_id and
// ordinal.
func TestHandleAddChild(t *testing.T) {
	srv := newTestServer()

	// Step 1: create a root engram.
	rememberBody := `{
		"jsonrpc":"2.0","method":"tools/call","id":1,
		"params":{
			"name":"muninn_remember",
			"arguments":{
				"vault":"default",
				"concept":"Root node",
				"content":"This is the root engram"
			}
		}
	}`
	rememberW := postRPC(t, srv, rememberBody)
	rememberContent := extractInnerJSON(t, decodeResp(t, rememberW.Body.String()))

	parentID, ok := rememberContent["id"].(string)
	if !ok || parentID == "" {
		t.Fatalf("muninn_remember did not return an id: %v", rememberContent["id"])
	}

	// Step 2: add a child to the root.
	addChildBody := `{
		"jsonrpc":"2.0","method":"tools/call","id":2,
		"params":{
			"name":"muninn_add_child",
			"arguments":{
				"vault":"default",
				"parent_id":"` + parentID + `",
				"concept":"Child node",
				"content":"First child of root"
			}
		}
	}`
	addChildW := postRPC(t, srv, addChildBody)
	addChildContent := extractInnerJSON(t, decodeResp(t, addChildW.Body.String()))

	childID, ok := addChildContent["child_id"].(string)
	if !ok || childID == "" {
		t.Errorf("expected non-empty child_id, got %v", addChildContent["child_id"])
	}
	ordinal, ok := addChildContent["ordinal"].(float64)
	if !ok {
		t.Errorf("expected ordinal to be a number, got %T", addChildContent["ordinal"])
	}
	if int(ordinal) != 1 {
		t.Errorf("expected ordinal == 1, got %v", ordinal)
	}
}

// TestHandleAddChild_MissingParentID verifies that omitting parent_id returns
// error code -32602.
func TestHandleAddChild_MissingParentID(t *testing.T) {
	srv := newTestServer()
	body := `{
		"jsonrpc":"2.0","method":"tools/call","id":1,
		"params":{
			"name":"muninn_add_child",
			"arguments":{
				"vault":"default",
				"concept":"Child node",
				"content":"Child content"
			}
		}
	}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error == nil || resp.Error.Code != -32602 {
		t.Errorf("expected -32602, got %v", resp.Error)
	}
}

// ── Round 2 hardening tests ───────────────────────────────────────────────────

// TestHandleRememberTree_EmptyRoot verifies that root:{} (missing concept) returns -32602.
func TestHandleRememberTree_EmptyRoot(t *testing.T) {
	srv := newTestServer()
	body := `{
		"jsonrpc":"2.0","method":"tools/call","id":1,
		"params":{
			"name":"muninn_remember_tree",
			"arguments":{
				"vault":"default",
				"root":{}
			}
		}
	}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error == nil || resp.Error.Code != -32602 {
		t.Errorf("expected -32602 for empty root concept, got: %v", resp.Error)
	}
}

// TestHandleRecallTree_EmptyRootID verifies that an explicitly empty root_id
// string returns error code -32602.
func TestHandleRecallTree_EmptyRootID(t *testing.T) {
	srv := newTestServer()
	body := `{
		"jsonrpc":"2.0","method":"tools/call","id":1,
		"params":{
			"name":"muninn_recall_tree",
			"arguments":{
				"vault":"default",
				"root_id":""
			}
		}
	}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error == nil || resp.Error.Code != -32602 {
		t.Errorf("expected -32602 for empty root_id, got %v", resp.Error)
	}
}

// TestHandleAddChild_EmptyContent verifies that an empty content string returns
// error code -32602.
func TestHandleAddChild_EmptyContent(t *testing.T) {
	srv := newTestServer()
	body := `{
		"jsonrpc":"2.0","method":"tools/call","id":1,
		"params":{
			"name":"muninn_add_child",
			"arguments":{
				"vault":"default",
				"parent_id":"some-parent-id",
				"concept":"Child concept",
				"content":""
			}
		}
	}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error == nil || resp.Error.Code != -32602 {
		t.Errorf("expected -32602 for empty content, got %v", resp.Error)
	}
}

// TestHandleAddChild_EmptyConcept verifies that an empty concept string returns
// error code -32602.
func TestHandleAddChild_EmptyConcept(t *testing.T) {
	srv := newTestServer()
	body := `{
		"jsonrpc":"2.0","method":"tools/call","id":1,
		"params":{
			"name":"muninn_add_child",
			"arguments":{
				"vault":"default",
				"parent_id":"some-parent-id",
				"concept":"",
				"content":"Some content"
			}
		}
	}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error == nil || resp.Error.Code != -32602 {
		t.Errorf("expected -32602 for empty concept, got %v", resp.Error)
	}
}

// TestHandleRememberTree_InvalidRootJSON verifies that passing a non-object
// value for root (e.g., a bare string) returns error code -32602 because the
// json.Unmarshal into TreeNodeInput will fail.
func TestHandleRememberTree_InvalidRootJSON(t *testing.T) {
	srv := newTestServer()
	body := `{
		"jsonrpc":"2.0","method":"tools/call","id":1,
		"params":{
			"name":"muninn_remember_tree",
			"arguments":{
				"vault":"default",
				"root":"not-an-object"
			}
		}
	}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error == nil || resp.Error.Code != -32602 {
		t.Errorf("expected -32602 for non-object root, got %v", resp.Error)
	}
}

// fakeEngineRememberTreeErr wraps fakeEngine and overrides RememberTree to
// return an error — used to simulate a duplicate-concept rejection from the
// engine layer so we can verify the MCP handler propagates the error correctly.
type fakeEngineRememberTreeErr struct {
	fakeEngine
	rememberTreeErr error
}

func (f *fakeEngineRememberTreeErr) RememberTree(_ context.Context, req *RememberTreeRequest) (*RememberTreeResult, error) {
	return nil, f.rememberTreeErr
}

// TestHandleRememberTree_DuplicateConcept verifies that a duplicate-concept
// error from the engine is propagated by the MCP handler as an error response.
// The engine returns a semantic error, so the handler sends -32000.
// Either -32602 or -32000 is acceptable — we assert only that an error is
// present in the response.
func TestHandleRememberTree_DuplicateConcept(t *testing.T) {
	// Use a fake engine that returns a duplicate-concept error from RememberTree.
	eng := &fakeEngineRememberTreeErr{
		rememberTreeErr: fmt.Errorf("RememberTree: duplicate concept %q at depth 1", "Phase 1"),
	}
	srv := New(":0", eng, "", nil, nil)

	body := `{
		"jsonrpc":"2.0","method":"tools/call","id":1,
		"params":{
			"name":"muninn_remember_tree",
			"arguments":{
				"vault":"default",
				"root":{
					"concept":"Project Plan",
					"content":"root node",
					"children":[
						{"concept":"Phase 1","content":"first phase"},
						{"concept":"Phase 1","content":"duplicate phase — should be rejected"}
					]
				}
			}
		}
	}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error == nil {
		t.Errorf("expected an error response for duplicate concepts, got nil error")
	}
}

// TestHandleRecallTree_MaxDepthZero documents and asserts the max_depth=0
// semantics at the MCP handler layer.
//
// Current handler logic (handlers.go):
//   maxDepth := 10                                  // default when absent
//   if d, ok := args["max_depth"].(float64); ok {
//       maxDepth = int(d)                           // overrides with 0 if caller passes 0
//   }
//
// This means:
//   - absent max_depth → 10 (handler default, engine interprets 10 as "depth 10")
//   - max_depth=0      → 0  (passed to engine; engine treats 0 as unlimited)
//   - max_depth=5      → 5
//
// Passing 0 explicitly therefore means UNLIMITED depth, which is surprising
// because users might expect "0 = return root only". This test documents the
// current behavior so that any future change is deliberate.
func TestHandleRecallTree_MaxDepthZero(t *testing.T) {
	srv := newTestServer()
	body := `{
		"jsonrpc":"2.0","method":"tools/call","id":1,
		"params":{
			"name":"muninn_recall_tree",
			"arguments":{
				"vault":"default",
				"root_id":"some-root-id",
				"max_depth":0
			}
		}
	}`
	w := postRPC(t, srv, body)
	// With the fake engine, RecallTree always succeeds regardless of maxDepth.
	resp := decodeResp(t, w.Body.String())
	if resp.Error != nil {
		t.Errorf("max_depth=0 should not cause an error (0 = unlimited), got %v", resp.Error)
	}
	// Verify the result is a valid tree response (not an error).
	content := extractInnerJSON(t, resp)
	if _, ok := content["root"]; !ok {
		t.Error("expected 'root' key in recall_tree response")
	}
}

// ── Round 3 hardening tests ───────────────────────────────────────────────────

// TestHandleRecallTree_NegativeMaxDepth verifies that passing max_depth:-1 does
// not cause an error. Negative values are normalized to 0 (unlimited) at the
// handler layer before forwarding to the engine.
func TestHandleRecallTree_NegativeMaxDepth(t *testing.T) {
	srv := newTestServer()
	body := `{
		"jsonrpc":"2.0","method":"tools/call","id":1,
		"params":{
			"name":"muninn_recall_tree",
			"arguments":{
				"vault":"default",
				"root_id":"some-root-id",
				"max_depth":-1
			}
		}
	}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	// Negative max_depth is normalized to 0 (unlimited) — must not error.
	if resp.Error != nil {
		t.Errorf("max_depth=-1 should not cause an error (normalized to 0=unlimited), got %v", resp.Error)
	}
	content := extractInnerJSON(t, resp)
	if _, ok := content["root"]; !ok {
		t.Error("expected 'root' key in recall_tree response")
	}
}

// TestHandleRecallTree_LimitCap verifies that passing limit:9999 does not cause
// an error. Values above 1000 are silently capped to 1000 at the handler layer.
func TestHandleRecallTree_LimitCap(t *testing.T) {
	srv := newTestServer()
	body := `{
		"jsonrpc":"2.0","method":"tools/call","id":1,
		"params":{
			"name":"muninn_recall_tree",
			"arguments":{
				"vault":"default",
				"root_id":"some-root-id",
				"limit":9999
			}
		}
	}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	// Oversized limit is capped — must not error.
	if resp.Error != nil {
		t.Errorf("limit=9999 should not cause an error (capped to 1000), got %v", resp.Error)
	}
	content := extractInnerJSON(t, resp)
	if _, ok := content["root"]; !ok {
		t.Error("expected 'root' key in recall_tree response")
	}
}

// TestHandleRememberTree_WhitespaceConcept verifies that a root with a
// whitespace-only concept returns error code -32602.
func TestHandleRememberTree_WhitespaceConcept(t *testing.T) {
	srv := newTestServer()
	body := `{
		"jsonrpc":"2.0","method":"tools/call","id":1,
		"params":{
			"name":"muninn_remember_tree",
			"arguments":{
				"vault":"default",
				"root":{
					"concept":"   ",
					"content":"some content"
				}
			}
		}
	}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error == nil || resp.Error.Code != -32602 {
		t.Errorf("expected -32602 for whitespace-only root concept, got: %v", resp.Error)
	}
}

// TestHandleAddChild_WhitespaceConcept verifies that a whitespace-only concept
// in muninn_add_child returns error code -32602.
func TestHandleAddChild_WhitespaceConcept(t *testing.T) {
	srv := newTestServer()
	body := `{
		"jsonrpc":"2.0","method":"tools/call","id":1,
		"params":{
			"name":"muninn_add_child",
			"arguments":{
				"vault":"default",
				"parent_id":"some-parent-id",
				"concept":"   ",
				"content":"Some content"
			}
		}
	}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error == nil || resp.Error.Code != -32602 {
		t.Errorf("expected -32602 for whitespace-only concept, got %v", resp.Error)
	}
}
