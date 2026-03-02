package mcp

import (
	"context"
	"encoding/json"
	"testing"
)

type entityAggEngine struct{ fakeEngine }

func (e *entityAggEngine) GetEntityAggregate(_ context.Context, _, _ string, _ int) (*EntityAggregate, error) {
	return &EntityAggregate{
		Name:          "PostgreSQL",
		Type:          "database",
		Confidence:    0.9,
		MentionCount:  3,
		State:         "active",
		Engrams:       []EntityEngramSummary{},
		Relationships: []EntityRelSummary{},
		CoOccurring:   []EntityCoOccurrence{},
	}, nil
}

func (e *entityAggEngine) ListEntities(_ context.Context, _ string, _ int, _ string) ([]EntitySummary, error) {
	return []EntitySummary{
		{Name: "PostgreSQL", Type: "database", MentionCount: 5, State: "active"},
	}, nil
}

func TestHandleEntity_HappyPath(t *testing.T) {
	srv := New(":0", &entityAggEngine{}, "", nil)
	w := postRPC(t, srv, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"muninn_entity","arguments":{"vault":"default","name":"PostgreSQL"}}}`)
	if w.Code != 200 {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	resp := decodeResp(t, w.Body.String())
	if resp.Error != nil {
		t.Fatalf("unexpected error: %v", resp.Error)
	}
	content := extractInnerJSON(t, resp)
	if content["name"] != "PostgreSQL" {
		t.Errorf("entity name = %v, want PostgreSQL", content["name"])
	}
	if content["type"] != "database" {
		t.Errorf("entity type = %v, want database", content["type"])
	}
	if content["mention_count"] == nil {
		t.Error("entity response should have a mention_count field")
	}
	if mentionCount, ok := content["mention_count"].(float64); !ok || mentionCount != 3 {
		t.Errorf("entity mention_count = %v, want 3", content["mention_count"])
	}
	if content["state"] != "active" {
		t.Errorf("entity state = %v, want active", content["state"])
	}
}

func TestHandleEntity_MissingName(t *testing.T) {
	srv := newTestServer()
	w := postRPC(t, srv, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"muninn_entity","arguments":{"vault":"default"}}}`)
	var resp JSONRPCResponse
	json.NewDecoder(w.Body).Decode(&resp)
	if resp.Error == nil {
		t.Fatal("expected error for missing name")
	}
}

func TestHandleEntities_HappyPath(t *testing.T) {
	srv := New(":0", &entityAggEngine{}, "", nil)
	w := postRPC(t, srv, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"muninn_entities","arguments":{"vault":"default"}}}`)
	if w.Code != 200 {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	resp := decodeResp(t, w.Body.String())
	if resp.Error != nil {
		t.Fatalf("unexpected error: %v", resp.Error)
	}
	content := extractInnerJSON(t, resp)
	if content["count"] == nil {
		t.Error("entities response should have a count field")
	}
	if count, ok := content["count"].(float64); !ok || count != 1 {
		t.Errorf("entities count = %v, want 1", content["count"])
	}
	entities, ok := content["entities"].([]any)
	if !ok {
		t.Fatalf("entities field should be an array, got %T", content["entities"])
	}
	if len(entities) != 1 {
		t.Fatalf("expected 1 entity, got %d", len(entities))
	}
	first, ok := entities[0].(map[string]any)
	if !ok {
		t.Fatalf("entities[0] should be an object, got %T", entities[0])
	}
	if first["name"] != "PostgreSQL" {
		t.Errorf("entities[0].name = %v, want PostgreSQL", first["name"])
	}
}

func TestHandleEntities_NoVaultDefaultsToDefault(t *testing.T) {
	// When vault is omitted, the server defaults to "default" — no error expected.
	srv := New(":0", &entityAggEngine{}, "", nil)
	w := postRPC(t, srv, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"muninn_entities","arguments":{}}}`)
	if w.Code != 200 {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	resp := decodeResp(t, w.Body.String())
	if resp.Error != nil {
		t.Fatalf("unexpected error: %v", resp.Error)
	}
	// Verify that the response is a valid entities list (not an empty or malformed result).
	content := extractInnerJSON(t, resp)
	if _, ok := content["entities"]; !ok {
		t.Error("entities response should have an 'entities' field even when vault is omitted")
	}
	if _, ok := content["count"]; !ok {
		t.Error("entities response should have a 'count' field even when vault is omitted")
	}
}
