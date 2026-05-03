package mcp

// handlers_coverage_test.go — additional tests to push internal/mcp coverage
// from 60.2% toward 75%. Targets: handleRemember, handleRecall,
// handleRememberBatch, handleConsolidate, handleEvolve, handleSession,
// handleGuide, handleRead, handleForget, handleLink error paths.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// ── engine stubs ──────────────────────────────────────────────────────────────

// writeErrEngine returns an error from Write.
type writeErrEngine struct{ fakeEngine }

func (e *writeErrEngine) Write(_ context.Context, _ *mbp.WriteRequest) (*mbp.WriteResponse, error) {
	return nil, fmt.Errorf("storage write failed")
}

// activateErrEngine returns an error from Activate.
type activateErrEngine struct{ fakeEngine }

func (e *activateErrEngine) Activate(_ context.Context, _ *mbp.ActivateRequest) (*mbp.ActivateResponse, error) {
	return nil, fmt.Errorf("recall storage error")
}

// readErrEngine returns an error from Read.
type readErrEngine struct{ fakeEngine }

func (e *readErrEngine) Read(_ context.Context, _ *mbp.ReadRequest) (*mbp.ReadResponse, error) {
	return nil, fmt.Errorf("engram not found")
}

// forgetErrEngine returns an error from Forget.
type forgetErrEngine struct{ fakeEngine }

func (e *forgetErrEngine) Forget(_ context.Context, _ *mbp.ForgetRequest) (*mbp.ForgetResponse, error) {
	return nil, fmt.Errorf("forget storage error")
}

// linkErrEngine returns an error from Link.
type linkErrEngine struct{ fakeEngine }

func (e *linkErrEngine) Link(_ context.Context, _ *mbp.LinkRequest) (*mbp.LinkResponse, error) {
	return nil, fmt.Errorf("link storage error")
}

// evolveErrEngine returns an error from Evolve.
type evolveErrEngine struct{ fakeEngine }

func (e *evolveErrEngine) Evolve(_ context.Context, _, _, _, _ string, _ []float32) (*WriteResult, error) {
	return nil, fmt.Errorf("evolve storage error")
}

// updateContentErrEngine returns an error from UpdateContent.
type updateContentErrEngine struct{ fakeEngine }

func (e *updateContentErrEngine) UpdateContent(_ context.Context, _, _, _, _ string, _ float32) (*WriteResult, error) {
	return nil, fmt.Errorf("update content storage error")
}

// consolidateErrEngine returns an error from Consolidate.
type consolidateErrEngine struct{ fakeEngine }

func (e *consolidateErrEngine) Consolidate(_ context.Context, _ string, _ []string, _ string) (*ConsolidateResult, error) {
	return nil, fmt.Errorf("consolidate storage error")
}

// sessionErrEngine returns an error from Session.
type sessionErrEngine struct{ fakeEngine }

func (e *sessionErrEngine) Session(_ context.Context, _ string, _ time.Time) (*SessionSummary, error) {
	return nil, fmt.Errorf("session storage error")
}

// statErrEngine returns an error from Stat (used by handleGuide / handleStatus).
type statErrEngine struct{ fakeEngine }

func (e *statErrEngine) Stat(_ context.Context, _ *mbp.StatRequest) (*mbp.StatResponse, error) {
	return nil, fmt.Errorf("stat unavailable")
}

// writeBatchPartialErrEngine has one failing item in WriteBatch.
type writeBatchPartialErrEngine struct{ fakeEngine }

func (e *writeBatchPartialErrEngine) WriteBatch(_ context.Context, reqs []*mbp.WriteRequest) ([]*mbp.WriteResponse, []error) {
	responses := make([]*mbp.WriteResponse, len(reqs))
	errs := make([]error, len(reqs))
	for i := range reqs {
		if i == 1 {
			errs[i] = fmt.Errorf("item %d write failed", i)
		} else {
			responses[i] = &mbp.WriteResponse{ID: fmt.Sprintf("batch-id-%d", i)}
		}
	}
	return responses, errs
}

// ── handleRemember additional paths ───────────────────────────────────────────

func TestHandleRemember_EngineError(t *testing.T) {
	srv := newTestServerWith(&writeErrEngine{})
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_remember","arguments":{"vault":"default","content":"test"}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error == nil || resp.Error.Code != -32000 {
		t.Errorf("expected -32000 for engine error, got %v", resp.Error)
	}
}

func TestHandleRemember_LongContentHint(t *testing.T) {
	// Content >500 chars should trigger a Hint field in the response.
	srv := newTestServer()
	longContent := strings.Repeat("x", 501)
	body := fmt.Sprintf(`{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_remember","arguments":{"vault":"default","content":%q}}}`, longContent)
	w := postRPC(t, srv, body)
	content := extractInnerJSON(t, decodeResp(t, w.Body.String()))
	if _, ok := content["hint"]; !ok {
		t.Error("expected 'hint' field for content > 500 chars")
	}
}

func TestHandleRemember_ShortContentNoHint(t *testing.T) {
	srv := newTestServer()
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_remember","arguments":{"vault":"default","content":"short"}}}`
	w := postRPC(t, srv, body)
	content := extractInnerJSON(t, decodeResp(t, w.Body.String()))
	if _, ok := content["hint"]; ok {
		t.Error("should not have 'hint' field for short content")
	}
}

func TestHandleRemember_ConfidenceClampedBelow(t *testing.T) {
	// confidence < 0 should be clamped to 0, not error.
	srv := newTestServer()
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_remember","arguments":{"vault":"default","content":"test","confidence":-0.5}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error != nil {
		t.Errorf("expected success when confidence < 0 is clamped, got error: %v", resp.Error)
	}
}

func TestHandleRemember_ConfidenceClampedAbove(t *testing.T) {
	// confidence > 1 should be clamped to 1, not error.
	srv := newTestServer()
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_remember","arguments":{"vault":"default","content":"test","confidence":1.5}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error != nil {
		t.Errorf("expected success when confidence > 1 is clamped, got error: %v", resp.Error)
	}
}

func TestHandleRemember_InvalidCreatedAt(t *testing.T) {
	srv := newTestServer()
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_remember","arguments":{"vault":"default","content":"test","created_at":"not-a-date"}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error == nil || resp.Error.Code != -32602 {
		t.Errorf("expected -32602 for invalid created_at, got %v", resp.Error)
	}
}

func TestHandleRemember_ValidCreatedAt(t *testing.T) {
	srv := newTestServer()
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_remember","arguments":{"vault":"default","content":"test","created_at":"2026-01-15T09:00:00Z"}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error != nil {
		t.Errorf("expected success with valid created_at, got error: %v", resp.Error)
	}
}

func TestHandleRemember_WithType(t *testing.T) {
	srv := newTestServer()
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_remember","arguments":{"vault":"default","content":"test","type":"decision"}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error != nil {
		t.Errorf("expected success with type param, got error: %v", resp.Error)
	}
}

func TestHandleRemember_WithTypeLabel(t *testing.T) {
	srv := newTestServer()
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_remember","arguments":{"vault":"default","content":"test","type":"custom_type","type_label":"My Custom Label"}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error != nil {
		t.Errorf("expected success with type_label param, got error: %v", resp.Error)
	}
}

func TestHandleRemember_TagsTruncatedAt50(t *testing.T) {
	// 60 tags should be silently truncated to 50.
	tags := make([]string, 60)
	for i := range tags {
		tags[i] = fmt.Sprintf("tag%d", i)
	}
	tagsJSON, _ := json.Marshal(tags)
	body := fmt.Sprintf(`{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_remember","arguments":{"vault":"default","content":"test","tags":%s}}}`, string(tagsJSON))
	srv := newTestServer()
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error != nil {
		t.Errorf("expected success with 60 tags (truncated to 50), got error: %v", resp.Error)
	}
}

// ── handleRecall additional paths ─────────────────────────────────────────────

func TestHandleRecall_MissingContext(t *testing.T) {
	srv := newTestServer()
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_recall","arguments":{"vault":"default"}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error == nil || resp.Error.Code != -32602 {
		t.Errorf("expected -32602 for missing context, got %v", resp.Error)
	}
}

func TestHandleRecall_EmptyContextArray(t *testing.T) {
	srv := newTestServer()
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_recall","arguments":{"vault":"default","context":[]}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error == nil || resp.Error.Code != -32602 {
		t.Errorf("expected -32602 for empty context array, got %v", resp.Error)
	}
}

func TestHandleRecall_ContextWrongType(t *testing.T) {
	// A non-string, non-array context (e.g. a number) should return a clear type error.
	srv := newTestServer()
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_recall","arguments":{"vault":"default","context":42}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error == nil || resp.Error.Code != -32602 {
		t.Errorf("expected -32602 for wrong context type, got %v", resp.Error)
	}
	if resp.Error != nil && !strings.Contains(resp.Error.Message, "got") {
		t.Errorf("expected error message to mention actual type, got: %q", resp.Error.Message)
	}
}

func TestHandleRecall_ContextBareString(t *testing.T) {
	// A bare string should be coerced into a single-element array and succeed.
	srv := newTestServer()
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_recall","arguments":{"vault":"default","context":"some query"}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error != nil {
		t.Errorf("expected bare string context to be coerced and succeed, got error: %v", resp.Error)
	}
}

func TestHandleRecall_ContextAllNonStrings(t *testing.T) {
	// An array containing only non-string elements should return -32602.
	srv := newTestServer()
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_recall","arguments":{"vault":"default","context":[123,true]}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error == nil || resp.Error.Code != -32602 {
		t.Errorf("expected -32602 for all-non-string context array, got %v", resp.Error)
	}
}

func TestHandleRecall_ThresholdClampedBelow(t *testing.T) {
	// threshold < 0 should be clamped to 0, not error.
	srv := newTestServer()
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_recall","arguments":{"vault":"default","context":["test"],"threshold":-0.5}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error != nil {
		t.Errorf("expected success with threshold < 0 clamped, got error: %v", resp.Error)
	}
}

func TestHandleRecall_ThresholdClampedAbove(t *testing.T) {
	// threshold > 1 should be clamped to 1, not error.
	srv := newTestServer()
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_recall","arguments":{"vault":"default","context":["test"],"threshold":2.0}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error != nil {
		t.Errorf("expected success with threshold > 1 clamped, got error: %v", resp.Error)
	}
}

func TestHandleRecall_LimitClampedBelow(t *testing.T) {
	// limit < 1 should become 1.
	srv := newTestServer()
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_recall","arguments":{"vault":"default","context":["test"],"limit":0}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error != nil {
		t.Errorf("expected success with limit=0 clamped to 1, got error: %v", resp.Error)
	}
}

func TestHandleRecall_LimitClampedAbove(t *testing.T) {
	// limit > 100 should become 100.
	srv := newTestServer()
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_recall","arguments":{"vault":"default","context":["test"],"limit":999}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error != nil {
		t.Errorf("expected success with limit=999 clamped to 100, got error: %v", resp.Error)
	}
}

func TestHandleRecall_SemanticMode(t *testing.T) {
	srv := newTestServer()
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_recall","arguments":{"vault":"default","context":["test"],"mode":"semantic"}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error != nil {
		t.Errorf("expected success with mode=semantic, got error: %v", resp.Error)
	}
}

func TestHandleRecall_RecentMode(t *testing.T) {
	srv := newTestServer()
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_recall","arguments":{"vault":"default","context":["test"],"mode":"recent"}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error != nil {
		t.Errorf("expected success with mode=recent, got error: %v", resp.Error)
	}
}

func TestHandleRecall_DeepMode(t *testing.T) {
	srv := newTestServer()
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_recall","arguments":{"vault":"default","context":["test"],"mode":"deep"}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error != nil {
		t.Errorf("expected success with mode=deep, got error: %v", resp.Error)
	}
}

func TestHandleRecall_InvalidMode(t *testing.T) {
	srv := newTestServer()
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_recall","arguments":{"vault":"default","context":["test"],"mode":"turbo"}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error == nil || resp.Error.Code != -32602 {
		t.Errorf("expected -32602 for invalid mode, got %v", resp.Error)
	}
}

func TestHandleRecall_WithSinceFilter(t *testing.T) {
	srv := newTestServer()
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_recall","arguments":{"vault":"default","context":["test"],"since":"2026-01-01T00:00:00Z"}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error != nil {
		t.Errorf("expected success with since filter, got error: %v", resp.Error)
	}
}

func TestHandleRecall_InvalidSinceFilter(t *testing.T) {
	srv := newTestServer()
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_recall","arguments":{"vault":"default","context":["test"],"since":"not-a-date"}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error == nil || resp.Error.Code != -32602 {
		t.Errorf("expected -32602 for invalid since, got %v", resp.Error)
	}
}

func TestHandleRecall_WithBeforeFilter(t *testing.T) {
	srv := newTestServer()
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_recall","arguments":{"vault":"default","context":["test"],"before":"2026-02-01T00:00:00Z"}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error != nil {
		t.Errorf("expected success with before filter, got error: %v", resp.Error)
	}
}

func TestHandleRecall_InvalidBeforeFilter(t *testing.T) {
	srv := newTestServer()
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_recall","arguments":{"vault":"default","context":["test"],"before":"bad-date"}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error == nil || resp.Error.Code != -32602 {
		t.Errorf("expected -32602 for invalid before, got %v", resp.Error)
	}
}

func TestHandleRecall_EngineError(t *testing.T) {
	srv := newTestServerWith(&activateErrEngine{})
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_recall","arguments":{"vault":"default","context":["test"]}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error == nil || resp.Error.Code != -32000 {
		t.Errorf("expected -32000 for engine error, got %v", resp.Error)
	}
}

// modePresetThresholdCapturingEngine records the Threshold from the ActivateRequest.
type modePresetThresholdCapturingEngine struct {
	fakeEngine
	lastThreshold float32
}

func (e *modePresetThresholdCapturingEngine) Activate(_ context.Context, req *mbp.ActivateRequest) (*mbp.ActivateResponse, error) {
	e.lastThreshold = req.Threshold
	return &mbp.ActivateResponse{}, nil
}

func TestHandleRecall_ModePresetAppliedWhenNoExplicitThreshold(t *testing.T) {
	// The "semantic" mode preset has Threshold=0.3. Without an explicit threshold
	// param, the preset value must be used.
	eng := &modePresetThresholdCapturingEngine{}
	srv := newTestServerWith(eng)
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_recall","arguments":{"vault":"default","context":["test"],"mode":"semantic"}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error != nil {
		t.Fatalf("unexpected error: %v", resp.Error)
	}
	if eng.lastThreshold != 0.3 {
		t.Errorf("mode preset threshold = %v, want 0.3", eng.lastThreshold)
	}
}

func TestHandleRecall_ExplicitThresholdWinsOverModePreset(t *testing.T) {
	// An explicit threshold=0.7 must override the semantic preset (0.3).
	eng := &modePresetThresholdCapturingEngine{}
	srv := newTestServerWith(eng)
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_recall","arguments":{"vault":"default","context":["test"],"mode":"semantic","threshold":0.7}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error != nil {
		t.Fatalf("unexpected error: %v", resp.Error)
	}
	if eng.lastThreshold != 0.7 {
		t.Errorf("explicit threshold should win: got %v, want 0.7", eng.lastThreshold)
	}
}

// ── handleRememberBatch additional paths ──────────────────────────────────────

func TestHandleRememberBatch_NotAnObject(t *testing.T) {
	// A memory entry that is not an object should cause -32602.
	srv := newTestServer()
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_remember_batch","arguments":{"vault":"default","memories":["not an object"]}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error == nil || resp.Error.Code != -32602 {
		t.Errorf("expected -32602 for non-object memory entry, got %v", resp.Error)
	}
}

func TestHandleRememberBatch_InvalidCreatedAt(t *testing.T) {
	srv := newTestServer()
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_remember_batch","arguments":{"vault":"default","memories":[{"content":"test","created_at":"not-a-date"}]}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error == nil || resp.Error.Code != -32602 {
		t.Errorf("expected -32602 for invalid created_at in batch item, got %v", resp.Error)
	}
}

func TestHandleRememberBatch_PartialEngineError_ReturnsResults(t *testing.T) {
	// When some items fail, the batch still returns a result (not an RPC error).
	// Failed items have status="error" in results[].
	srv := newTestServerWith(&writeBatchPartialErrEngine{})
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_remember_batch","arguments":{"vault":"default","memories":[{"content":"first"},{"content":"second"},{"content":"third"}]}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error != nil {
		t.Fatalf("batch with partial errors should not return RPC error, got: %v", resp.Error)
	}
	content := extractInnerJSON(t, resp)
	results, ok := content["results"].([]any)
	if !ok {
		t.Fatal("expected results array in response")
	}
	if len(results) != 3 {
		t.Fatalf("expected 3 results, got %d", len(results))
	}
	// Item 1 (index 1) should have status="error"
	item1, ok := results[1].(map[string]any)
	if !ok {
		t.Fatal("results[1] is not an object")
	}
	if item1["status"] != "error" {
		t.Errorf("results[1].status = %v, want 'error'", item1["status"])
	}
	// Items 0 and 2 should be ok
	for _, idx := range []int{0, 2} {
		item, ok := results[idx].(map[string]any)
		if !ok {
			t.Fatalf("results[%d] is not an object", idx)
		}
		if item["status"] != "ok" {
			t.Errorf("results[%d].status = %v, want 'ok'", idx, item["status"])
		}
	}
}

func TestHandleRememberBatch_ItemConfidenceClamped(t *testing.T) {
	// Per-item confidence bounds (< 0 and > 1) should be silently clamped.
	srv := newTestServer()
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_remember_batch","arguments":{"vault":"default","memories":[{"content":"a","confidence":-1},{"content":"b","confidence":2.5}]}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error != nil {
		t.Errorf("expected success with clamped confidence values, got error: %v", resp.Error)
	}
}

func TestHandleRememberBatch_ItemTagsTruncated(t *testing.T) {
	// Per-item tags > 50 should be silently truncated to 50.
	tags := make([]string, 60)
	for i := range tags {
		tags[i] = fmt.Sprintf("tag%d", i)
	}
	tagsJSON, _ := json.Marshal(tags)
	body := fmt.Sprintf(`{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_remember_batch","arguments":{"vault":"default","memories":[{"content":"test","tags":%s}]}}}`, string(tagsJSON))
	srv := newTestServer()
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error != nil {
		t.Errorf("expected success with 60 tags truncated to 50, got error: %v", resp.Error)
	}
}

// ── handleConsolidate additional paths ────────────────────────────────────────

func TestHandleConsolidate_HappyPath(t *testing.T) {
	srv := newTestServer()
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_consolidate","arguments":{"vault":"default","ids":["id1","id2"],"merged_content":"merged result"}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error != nil {
		t.Fatalf("unexpected error: %v", resp.Error)
	}
	content := extractInnerJSON(t, resp)
	if _, ok := content["id"]; !ok {
		t.Error("response missing field: 'id'")
	}
}

func TestHandleConsolidate_MissingIDs(t *testing.T) {
	srv := newTestServer()
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_consolidate","arguments":{"vault":"default","merged_content":"merged"}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error == nil || resp.Error.Code != -32602 {
		t.Errorf("expected -32602 for missing ids, got %v", resp.Error)
	}
}

func TestHandleConsolidate_OnlyOneValidID(t *testing.T) {
	// If the ids array has only 1 valid string (after filtering non-strings), must error.
	srv := newTestServer()
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_consolidate","arguments":{"vault":"default","ids":["only-one"],"merged_content":"merged"}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error == nil || resp.Error.Code != -32602 {
		t.Errorf("expected -32602 for <2 ids, got %v", resp.Error)
	}
}

func TestHandleConsolidate_MissingMergedContent(t *testing.T) {
	srv := newTestServer()
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_consolidate","arguments":{"vault":"default","ids":["id1","id2"]}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error == nil || resp.Error.Code != -32602 {
		t.Errorf("expected -32602 for missing merged_content, got %v", resp.Error)
	}
}

func TestHandleConsolidate_EngineError(t *testing.T) {
	srv := newTestServerWith(&consolidateErrEngine{})
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_consolidate","arguments":{"vault":"default","ids":["id1","id2"],"merged_content":"merged"}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error == nil || resp.Error.Code != -32000 {
		t.Errorf("expected -32000 for engine error, got %v", resp.Error)
	}
}

// ── handleEvolve additional paths ─────────────────────────────────────────────

func TestHandleEvolve_HappyPath(t *testing.T) {
	srv := newTestServer()
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_evolve","arguments":{"vault":"default","id":"old-id","new_content":"updated content","reason":"content was stale"}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error != nil {
		t.Fatalf("unexpected error: %v", resp.Error)
	}
	content := extractInnerJSON(t, resp)
	if _, ok := content["id"]; !ok {
		t.Error("response missing field: 'id'")
	}
}

func TestHandleEvolve_MissingReason(t *testing.T) {
	srv := newTestServer()
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_evolve","arguments":{"vault":"default","id":"old-id","new_content":"updated"}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error == nil || resp.Error.Code != -32602 {
		t.Errorf("expected -32602 for missing reason, got %v", resp.Error)
	}
}

func TestHandleEvolve_MissingID(t *testing.T) {
	srv := newTestServer()
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_evolve","arguments":{"vault":"default","new_content":"updated","reason":"why"}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error == nil || resp.Error.Code != -32602 {
		t.Errorf("expected -32602 for missing id, got %v", resp.Error)
	}
}

func TestHandleEvolve_EngineError(t *testing.T) {
	srv := newTestServerWith(&evolveErrEngine{})
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_evolve","arguments":{"vault":"default","id":"old-id","new_content":"updated","reason":"stale"}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error == nil || resp.Error.Code != -32000 {
		t.Errorf("expected -32000 for engine error, got %v", resp.Error)
	}
}

// ── handleUpdate paths ────────────────────────────────────────────────────────

func TestHandleUpdate_HappyPath(t *testing.T) {
	srv := newTestServer()
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_update","arguments":{"vault":"default","id":"01JTEST","new_content":"updated content","reason":"sprint11_v3_upgrade"}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error != nil {
		t.Fatalf("unexpected error: %v", resp.Error)
	}
	content := extractInnerJSON(t, resp)
	if _, ok := content["id"]; !ok {
		t.Error("response missing field: 'id'")
	}
	// Critical contract: returned ID should equal the input ID (in-place semantics).
	if got := content["id"]; got != "01JTEST" {
		t.Errorf("expected id to equal input id 01JTEST, got %v", got)
	}
}

func TestHandleUpdate_HappyPathWithConfidence(t *testing.T) {
	srv := newTestServer()
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_update","arguments":{"vault":"default","id":"01JTEST","new_content":"updated","reason":"r","new_confidence":0.85}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error != nil {
		t.Fatalf("unexpected error: %v", resp.Error)
	}
}

func TestHandleUpdate_MissingID(t *testing.T) {
	srv := newTestServer()
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_update","arguments":{"vault":"default","new_content":"updated","reason":"r"}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error == nil || resp.Error.Code != -32602 {
		t.Errorf("expected -32602 for missing id, got %v", resp.Error)
	}
}

func TestHandleUpdate_MissingContent(t *testing.T) {
	srv := newTestServer()
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_update","arguments":{"vault":"default","id":"01JTEST","reason":"r"}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error == nil || resp.Error.Code != -32602 {
		t.Errorf("expected -32602 for missing new_content, got %v", resp.Error)
	}
}

func TestHandleUpdate_MissingReason(t *testing.T) {
	srv := newTestServer()
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_update","arguments":{"vault":"default","id":"01JTEST","new_content":"updated"}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error == nil || resp.Error.Code != -32602 {
		t.Errorf("expected -32602 for missing reason, got %v", resp.Error)
	}
}

func TestHandleUpdate_BadConfidenceOutOfRange(t *testing.T) {
	srv := newTestServer()
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_update","arguments":{"vault":"default","id":"01JTEST","new_content":"x","reason":"r","new_confidence":1.5}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error == nil || resp.Error.Code != -32602 {
		t.Errorf("expected -32602 for out-of-range confidence, got %v", resp.Error)
	}
}

func TestHandleUpdate_BadConfidenceWrongType(t *testing.T) {
	srv := newTestServer()
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_update","arguments":{"vault":"default","id":"01JTEST","new_content":"x","reason":"r","new_confidence":"high"}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error == nil || resp.Error.Code != -32602 {
		t.Errorf("expected -32602 for non-numeric confidence, got %v", resp.Error)
	}
}

func TestHandleUpdate_EngineError(t *testing.T) {
	srv := newTestServerWith(&updateContentErrEngine{})
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_update","arguments":{"vault":"default","id":"01JTEST","new_content":"x","reason":"r"}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error == nil || resp.Error.Code != -32000 {
		t.Errorf("expected -32000 for engine error, got %v", resp.Error)
	}
}

// ── handleSession additional paths ────────────────────────────────────────────

func TestHandleSession_HappyPath(t *testing.T) {
	srv := newTestServer()
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_session","arguments":{"vault":"default","since":"2026-01-01T00:00:00Z"}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error != nil {
		t.Fatalf("unexpected error: %v", resp.Error)
	}
}

func TestHandleSession_MissingSince(t *testing.T) {
	srv := newTestServer()
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_session","arguments":{"vault":"default"}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error == nil || resp.Error.Code != -32602 {
		t.Errorf("expected -32602 for missing since, got %v", resp.Error)
	}
}

func TestHandleSession_EngineError(t *testing.T) {
	srv := newTestServerWith(&sessionErrEngine{})
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_session","arguments":{"vault":"default","since":"2026-01-01T00:00:00Z"}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error == nil || resp.Error.Code != -32000 {
		t.Errorf("expected -32000 for engine error, got %v", resp.Error)
	}
}

// ── handleGuide additional paths ─────────────────────────────────────────────

func TestHandleGuide_HappyPath(t *testing.T) {
	srv := newTestServer()
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_guide","arguments":{"vault":"default"}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error != nil {
		t.Fatalf("unexpected error: %v", resp.Error)
	}
}

func TestHandleGuide_StatEngineError(t *testing.T) {
	// When Stat fails, handleGuide should return -32000.
	srv := newTestServerWith(&statErrEngine{})
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_guide","arguments":{"vault":"default"}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error == nil || resp.Error.Code != -32000 {
		t.Errorf("expected -32000 for stat engine error, got %v", resp.Error)
	}
}

// ── handleStatus engine error ─────────────────────────────────────────────────

func TestHandleStatus_EngineError(t *testing.T) {
	srv := newTestServerWith(&statErrEngine{})
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_status","arguments":{"vault":"default"}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error == nil || resp.Error.Code != -32000 {
		t.Errorf("expected -32000 for engine error, got %v", resp.Error)
	}
}

// ── handleRead additional paths ───────────────────────────────────────────────

func TestHandleRead_EngineError(t *testing.T) {
	srv := newTestServerWith(&readErrEngine{})
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_read","arguments":{"vault":"default","id":"gone"}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error == nil || resp.Error.Code != -32000 {
		t.Errorf("expected -32000 for engine error, got %v", resp.Error)
	}
}

// ── handleForget additional paths ─────────────────────────────────────────────

func TestHandleForget_EngineError(t *testing.T) {
	srv := newTestServerWith(&forgetErrEngine{})
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_forget","arguments":{"vault":"default","id":"e1"}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error == nil || resp.Error.Code != -32000 {
		t.Errorf("expected -32000 for engine error, got %v", resp.Error)
	}
}

// ── handleLink additional paths ───────────────────────────────────────────────

func TestHandleLink_EngineError(t *testing.T) {
	srv := newTestServerWith(&linkErrEngine{})
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_link","arguments":{"vault":"default","source_id":"s1","target_id":"t1","relation":"supports"}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error == nil || resp.Error.Code != -32000 {
		t.Errorf("expected -32000 for engine error, got %v", resp.Error)
	}
}

func TestHandleLink_WeightClampedBelow(t *testing.T) {
	srv := newTestServer()
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_link","arguments":{"vault":"default","source_id":"s1","target_id":"t1","relation":"supports","weight":-1.0}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error != nil {
		t.Errorf("expected success with weight < 0 clamped, got error: %v", resp.Error)
	}
}

func TestHandleLink_WeightClampedAbove(t *testing.T) {
	srv := newTestServer()
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_link","arguments":{"vault":"default","source_id":"s1","target_id":"t1","relation":"supports","weight":2.0}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error != nil {
		t.Errorf("expected success with weight > 1 clamped, got error: %v", resp.Error)
	}
}

func TestHandleLink_MissingSourceID(t *testing.T) {
	srv := newTestServer()
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_link","arguments":{"vault":"default","target_id":"t1","relation":"supports"}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error == nil || resp.Error.Code != -32602 {
		t.Errorf("expected -32602 for missing source_id, got %v", resp.Error)
	}
}

// ── handleListDeleted edge cases ──────────────────────────────────────────────

func TestHandleListDeleted_LimitBelowMinimum(t *testing.T) {
	// limit=0 should still work (defaults to 20 if not provided, no minimum enforced by handler).
	// Confirm no error is returned.
	eng := &limitTrackingEngine{}
	srv := newTestServerWith(eng)
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_list_deleted","arguments":{"vault":"default","limit":0}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error != nil {
		t.Errorf("expected success for limit=0, got error: %v", resp.Error)
	}
}
