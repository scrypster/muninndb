package mcp

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// captureEntityRelEngine captures WriteRequest to inspect EntityRelationships.
type captureEntityRelEngine struct {
	fakeEngine
	lastReq *mbp.WriteRequest
}

func (e *captureEntityRelEngine) Write(_ context.Context, req *mbp.WriteRequest) (*mbp.WriteResponse, error) {
	e.lastReq = req
	return &mbp.WriteResponse{ID: "01TEST"}, nil
}

func TestHandleRemember_EntityRelationships_Parsed(t *testing.T) {
	eng := &captureEntityRelEngine{}
	srv := New(":0", eng, "", nil)
	w := postRPC(t, srv, `{
        "jsonrpc":"2.0","id":1,"method":"tools/call",
        "params":{"name":"muninn_remember","arguments":{
            "vault":"default",
            "content":"PostgreSQL uses Redis for caching.",
            "entities":[{"name":"PostgreSQL","type":"database"},{"name":"Redis","type":"database"}],
            "entity_relationships":[{"from_entity":"PostgreSQL","to_entity":"Redis","rel_type":"caches_with","weight":0.9}]
        }}
    }`)
	if w.Code != 200 {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp JSONRPCResponse
	json.NewDecoder(w.Body).Decode(&resp)
	if resp.Error != nil {
		t.Fatalf("unexpected error: %v", resp.Error)
	}
	if eng.lastReq == nil {
		t.Fatal("Write was not called")
	}
	if len(eng.lastReq.EntityRelationships) != 1 {
		t.Fatalf("want 1 EntityRelationship, got %d", len(eng.lastReq.EntityRelationships))
	}
	er := eng.lastReq.EntityRelationships[0]
	if er.FromEntity != "PostgreSQL" || er.ToEntity != "Redis" || er.RelType != "caches_with" {
		t.Errorf("unexpected entity relationship: %+v", er)
	}
	if er.Weight != 0.9 {
		t.Errorf("want weight 0.9, got %f", er.Weight)
	}
}

func TestHandleRemember_EntityRelationships_DefaultWeight(t *testing.T) {
	eng := &captureEntityRelEngine{}
	srv := New(":0", eng, "", nil)
	w := postRPC(t, srv, `{
        "jsonrpc":"2.0","id":1,"method":"tools/call",
        "params":{"name":"muninn_remember","arguments":{
            "vault":"default",
            "content":"A depends on B.",
            "entity_relationships":[{"from_entity":"A","to_entity":"B","rel_type":"depends_on"}]
        }}
    }`)
	json.NewDecoder(w.Body).Decode(&struct{}{})
	if eng.lastReq == nil || len(eng.lastReq.EntityRelationships) == 0 {
		t.Fatal("EntityRelationships not populated")
	}
	if eng.lastReq.EntityRelationships[0].Weight != 0.9 {
		t.Errorf("want default weight 0.9, got %f", eng.lastReq.EntityRelationships[0].Weight)
	}
}

func TestHandleRemember_EntityRelationships_SkipsInvalid(t *testing.T) {
	eng := &captureEntityRelEngine{}
	srv := New(":0", eng, "", nil)
	w := postRPC(t, srv, `{
        "jsonrpc":"2.0","id":1,"method":"tools/call",
        "params":{"name":"muninn_remember","arguments":{
            "vault":"default",
            "content":"test.",
            "entity_relationships":[
                {"from_entity":"","to_entity":"B","rel_type":"uses"},
                {"from_entity":"A","to_entity":"","rel_type":"uses"},
                {"from_entity":"A","to_entity":"B","rel_type":""}
            ]
        }}
    }`)
	json.NewDecoder(w.Body).Decode(&struct{}{})
	if eng.lastReq != nil && len(eng.lastReq.EntityRelationships) != 0 {
		t.Fatalf("invalid relationships should be skipped, got %d", len(eng.lastReq.EntityRelationships))
	}
}
