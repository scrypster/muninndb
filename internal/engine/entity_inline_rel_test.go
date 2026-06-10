package engine

import (
	"context"
	"testing"

	"github.com/scrypster/muninndb/internal/transport/mbp"
)

func TestWrite_InlineEntityRelationships_PopulatesGraph(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	_, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:   "rel-test",
		Concept: "PostgreSQL caches with Redis",
		Content: "Our production setup uses Redis as a caching layer in front of PostgreSQL.",
		Entities: []mbp.InlineEntity{
			{Name: "PostgreSQL", Type: "database"},
			{Name: "Redis", Type: "database"},
		},
		EntityRelationships: []mbp.InlineEntityRelationship{
			{FromEntity: "PostgreSQL", ToEntity: "Redis", RelType: "caches_with", Weight: 0.9},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	g, err := eng.ExportGraph(ctx, "rel-test", false)
	if err != nil {
		t.Fatal(err)
	}

	// Expect: 1 typed "caches_with" edge + 1 "co_occurs_with" edge = 2 deduplicated edges
	if len(g.Edges) != 2 {
		t.Fatalf("want 2 edges (caches_with + co_occurs_with), got %d", len(g.Edges))
	}

	found := false
	for _, edge := range g.Edges {
		if edge.RelType == "caches_with" && edge.From == "PostgreSQL" && edge.To == "Redis" {
			found = true
			if edge.Weight != 0.9 {
				t.Errorf("want weight 0.9, got %f", edge.Weight)
			}
		}
	}
	if !found {
		t.Fatal("caches_with edge not found in export graph")
	}
}

func TestWrite_InlineEntityRelationships_DefaultWeight(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	_, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:    "rel-weight-test",
		Concept:  "test",
		Content:  "A uses B.",
		Entities: []mbp.InlineEntity{{Name: "A", Type: "service"}, {Name: "B", Type: "service"}},
		EntityRelationships: []mbp.InlineEntityRelationship{
			{FromEntity: "A", ToEntity: "B", RelType: "uses"}, // no weight — should default to 0.9
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	g, err := eng.ExportGraph(ctx, "rel-weight-test", false)
	if err != nil {
		t.Fatal(err)
	}

	for _, edge := range g.Edges {
		if edge.RelType == "uses" && edge.Weight == 0 {
			t.Error("weight should default to 0.9, not 0")
		}
	}
}
