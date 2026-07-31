package mcp

import (
	"strings"
	"testing"

	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// A plain string entity used to be dropped, then dropped-with-a-warning. It is
// now the MIDDLE GEAR: the name is kept and the type is resolved from the
// vault's own entity table (here: no resolver, so "other"). Four independent
// evaluators said they would not pay for typed JSON objects mid-task, and a
// measured 4.81% declaration rate agreed with them; a name with an imperfect
// type is worth far more than no entity at all.
func TestApplyEnrichmentArgs_PlainStringEntityIsKept(t *testing.T) {
	args := map[string]any{
		"entities": []any{"PostgreSQL"},
	}
	req := &mbp.WriteRequest{}
	applyEnrichmentArgs(args, req)
	if len(req.Entities) != 1 || req.Entities[0].Name != "PostgreSQL" {
		t.Fatalf("bare-string entity must be kept, got %+v", req.Entities)
	}
	if req.Entities[0].Type != "other" {
		t.Errorf("unresolvable bare name should be typed %q, got %q", "other", req.Entities[0].Type)
	}
}

func TestApplyEnrichmentArgs_BareStringIsNotMalformed(t *testing.T) {
	args := map[string]any{
		"entities": []any{
			"PostgreSQL", // middle gear, not malformed
			map[string]any{"name": "Go", "type": "language"},
			42.0, // genuinely unusable: no name anywhere in it
		},
	}
	req := &mbp.WriteRequest{}
	malformed := applyEnrichmentArgs(args, req)
	if malformed != 1 {
		t.Errorf("only the nameless item is malformed; got malformedCount=%d", malformed)
	}
	if len(req.Entities) != 2 {
		t.Errorf("expected 2 entities, got %d (%+v)", len(req.Entities), req.Entities)
	}
}

// The batch path reports the same way the single path does: a bare string is
// accepted, and the caller is told which names could not be typed.
func TestApplyEnrichmentArgs_BatchBareStringHint(t *testing.T) {
	srv := newTestServer()
	// Item 0: a bare-string entity — accepted, but reported as untypeable.
	// Item 1: a fully-typed entity — nothing to report, so no hint.
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

	// Item 0 should be told its name could not be typed — never silently.
	item0, ok := results[0].(map[string]any)
	if !ok {
		t.Fatal("results[0] is not an object")
	}
	hint0, _ := item0["hint"].(string)
	if !strings.Contains(hint0, "PostgreSQL") || !strings.Contains(hint0, "other") {
		t.Errorf("results[0].hint should name the untypeable entity and the type it got, got: %q", hint0)
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
