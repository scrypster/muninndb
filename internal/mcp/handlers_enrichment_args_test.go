package mcp

import (
	"strings"
	"testing"

	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// TestApplyEnrichmentArgs_PlainStringEntityIsSkipped tests that a plain string
// entity is silently skipped (not stored as a valid entity).
//
// Before the fix: this passes but no warning is surfaced to the caller.
// After the fix: this still passes, but a malformed count is returned.
func TestApplyEnrichmentArgs_PlainStringEntityIsSkipped(t *testing.T) {
	args := map[string]any{
		"entities": []any{"PostgreSQL"}, // plain string, not map[string]any{"name":..., "type":...}
	}
	req := &mbp.WriteRequest{}
	applyEnrichmentArgs(args, req)
	if len(req.Entities) != 0 {
		t.Errorf("expected 0 entities (plain string should be skipped), got %d", len(req.Entities))
	}
}

func TestApplyEnrichmentArgs_PlainStringEntityMalformedCount(t *testing.T) {
	args := map[string]any{
		"entities": []any{
			"PostgreSQL", // malformed: plain string
			map[string]any{"name": "Go", "type": "language"}, // valid
		},
	}
	req := &mbp.WriteRequest{}
	malformed := applyEnrichmentArgs(args, req)
	if malformed != 1 {
		t.Errorf("expected malformedCount=1, got %d", malformed)
	}
	if len(req.Entities) != 1 {
		t.Errorf("expected 1 valid entity, got %d", len(req.Entities))
	}
}

// TestApplyEnrichmentArgs_BatchMalformedEntityWarning tests that the batch
// remember handler surfaces a per-item hint when entities are malformed
// (plain strings instead of {"name":"...","type":"..."} objects).
func TestApplyEnrichmentArgs_BatchMalformedEntityWarning(t *testing.T) {
	srv := newTestServer()
	// Item 0: has a malformed entity (plain string) — should get a hint.
	// Item 1: has a valid entity — should have no hint.
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_remember_batch","arguments":{"vault":"default","memories":[{"content":"first memory","entities":["PostgreSQL"]},{"content":"second memory","entities":[{"name":"Go","type":"language"}]}]}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error != nil {
		t.Fatalf("unexpected RPC error: %v", resp.Error)
	}
	content := extractInnerJSON(t, resp)
	results, ok := content["results"].([]any)
	if !ok {
		t.Fatal("expected results array in response")
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}

	// Item 0 should have a hint about malformed entities.
	item0, ok := results[0].(map[string]any)
	if !ok {
		t.Fatal("results[0] is not an object")
	}
	hint0, _ := item0["hint"].(string)
	if !strings.Contains(hint0, "malformed") {
		t.Errorf("results[0].hint should mention 'malformed', got: %q", hint0)
	}

	// Item 1 should have no hint (valid entities).
	item1, ok := results[1].(map[string]any)
	if !ok {
		t.Fatal("results[1] is not an object")
	}
	if hint1, exists := item1["hint"]; exists && hint1 != "" {
		t.Errorf("results[1].hint should be absent or empty for well-formed entities, got: %q", hint1)
	}
}
