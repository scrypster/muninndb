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

// TestWrite_InlineEntityRelationships_DistinctUnmappedPredicatesCoexist is the
// #894 engine-level regression: two inline relationships whose predicates are
// outside the old relTypeBytes vocabulary, asserted about the same entity pair,
// used to fold to the same 0x21 key — the second write silently destroyed the
// first. ExportGraph must show both typed edges.
func TestWrite_InlineEntityRelationships_DistinctUnmappedPredicatesCoexist(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	_, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:   "rel-collision-test",
		Concept: "platform talks to cache",
		Content: "The Aurora Platform communicates with the Kepler Cache for lookups.",
		Entities: []mbp.InlineEntity{
			{Name: "Aurora Platform", Type: "platform"},
			{Name: "Kepler Cache", Type: "cache"},
		},
		EntityRelationships: []mbp.InlineEntityRelationship{
			{FromEntity: "Aurora Platform", ToEntity: "Kepler Cache", RelType: "communicates_with", Weight: 0.9},
			{FromEntity: "Aurora Platform", ToEntity: "Kepler Cache", RelType: "attributed_to", Weight: 0.7},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	g, err := eng.ExportGraph(ctx, "rel-collision-test", false)
	if err != nil {
		t.Fatal(err)
	}

	// Both typed edges must survive (the pair also yields a co_occurs_with edge,
	// which is unrelated to the collision). Collect the two predicates we wrote.
	typed := map[string]float32{}
	for _, edge := range g.Edges {
		if edge.From == "Aurora Platform" && edge.To == "Kepler Cache" &&
			(edge.RelType == "communicates_with" || edge.RelType == "attributed_to") {
			typed[edge.RelType] = edge.Weight
		}
	}
	if len(typed) != 2 {
		t.Fatalf("want both typed edges (communicates_with, attributed_to) between the pair, got %d: %+v", len(typed), g.Edges)
	}
	if w, ok := typed["communicates_with"]; !ok || w < 0.89 || w > 0.91 {
		t.Errorf("communicates_with edge weight = %v (ok=%v), want 0.9", w, ok)
	}
	if w, ok := typed["attributed_to"]; !ok || w < 0.69 || w > 0.71 {
		t.Errorf("attributed_to edge weight = %v (ok=%v), want 0.7", w, ok)
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
