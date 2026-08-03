package engine

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/transport/mbp"
	"github.com/stretchr/testify/require"
)

// writeEntityRelationship writes a memory with two entities and a relationship
// so that the 0x21 relationship index is populated.
func writeEntityRelationship(t *testing.T, eng *Engine, vault, fromName, fromType, toName, toType, relType string) {
	t.Helper()
	ctx := context.Background()
	_, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:   vault,
		Content: fromName + " " + relType + " " + toName,
		Concept: fromName + " → " + toName,
		Entities: []mbp.InlineEntity{
			{Name: fromName, Type: fromType},
			{Name: toName, Type: toType},
		},
		Relationships: []mbp.InlineRelationship{},
	})
	require.NoError(t, err)

	// Also upsert the relationship record directly so the 0x21 index is populated.
	ws := eng.store.ResolveVaultPrefix(vault)
	// Create a placeholder ULID for the relationship entry.
	id := storage.ULID{}
	err = eng.store.UpsertRelationshipRecord(ctx, ws, id, storage.RelationshipRecord{
		FromEntity: fromName,
		ToEntity:   toName,
		RelType:    relType,
		Weight:     0.8,
		Source:     "test",
	})
	require.NoError(t, err)

	// Upsert entity records so GetEntityRecord works.
	err = eng.store.UpsertEntityRecord(ctx, ws, storage.EntityRecord{
		Name: fromName, Type: fromType, Confidence: 1.0,
	}, "test")
	require.NoError(t, err)
	err = eng.store.UpsertEntityRecord(ctx, ws, storage.EntityRecord{
		Name: toName, Type: toType, Confidence: 1.0,
	}, "test")
	require.NoError(t, err)
}

func TestExportGraph_JSONLDFormat(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()

	writeEntityRelationship(t, eng, "default", "PostgreSQL", "database", "Redis", "cache", "manages")

	g, err := eng.ExportGraph(context.Background(), "default", true)
	require.NoError(t, err)
	require.NotNil(t, g)

	// Should have 2 nodes (PostgreSQL, Redis) and 2 edges:
	// one "manages" edge from the explicit UpsertRelationshipRecord call,
	// and one "co_occurs_with" edge auto-populated at write time.
	require.Len(t, g.Edges, 2, "expected 2 edges (manages + co_occurs_with)")
	require.Len(t, g.Nodes, 2, "expected 2 nodes")

	jsonLD, err := FormatGraphJSONLD(g)
	require.NoError(t, err)
	require.NotEmpty(t, jsonLD)

	// Parse as JSON and verify @context and @graph.
	var doc map[string]any
	require.NoError(t, json.Unmarshal([]byte(jsonLD), &doc))

	ctx, ok := doc["@context"].(map[string]any)
	require.True(t, ok, "expected @context to be a map")
	require.Equal(t, "https://schema.org/", ctx["@vocab"])
	require.Equal(t, "https://muninndb.io/ontology#", ctx["muninn"])

	graph, ok := doc["@graph"].([]any)
	require.True(t, ok, "expected @graph to be an array")
	// 2 entity nodes + 1 relationship node
	require.GreaterOrEqual(t, len(graph), 2, "expected at least 2 entries in @graph")

	// Find the "manages" relationship entry (there may also be a co_occurs_with edge).
	foundManages := false
	for _, item := range graph {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if m["@type"] == "muninn:Relationship" && m["muninn:relType"] == "manages" {
			foundManages = true
		}
	}
	require.True(t, foundManages, "expected a muninn:Relationship with relType 'manages' in @graph")
}

func TestExportGraph_GraphMLFormat(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()

	writeEntityRelationship(t, eng, "default", "PostgreSQL", "database", "Redis", "cache", "uses")

	g, err := eng.ExportGraph(context.Background(), "default", false)
	require.NoError(t, err)
	require.NotNil(t, g)

	graphML, err := FormatGraphGraphML(g)
	require.NoError(t, err)
	require.NotEmpty(t, graphML)

	// Verify XML preamble and structure.
	require.True(t, strings.HasPrefix(graphML, `<?xml version="1.0" encoding="UTF-8"?>`), "expected XML declaration")
	require.Contains(t, graphML, `<graphml`)
	require.Contains(t, graphML, `<graph`)

	// Should contain both entity names.
	require.Contains(t, graphML, "PostgreSQL")
	require.Contains(t, graphML, "Redis")

	// Should contain the edge.
	require.Contains(t, graphML, `<edge`)
	require.Contains(t, graphML, "uses")
}

func TestExportGraph_DeduplicatesEdgesByTriple(t *testing.T) {
	// Two relationship records with same (from, to, relType) but different weights.
	// Export should return one edge with the higher weight.
	eng, cleanup := testEnv(t)
	defer cleanup()

	ctx := context.Background()
	ws := eng.store.ResolveVaultPrefix("dedup-vault")
	var id1, id2 storage.ULID
	id1[0] = 1
	id2[0] = 2

	_ = eng.store.UpsertRelationshipRecord(ctx, ws, id1, storage.RelationshipRecord{
		FromEntity: "A", ToEntity: "B", RelType: "co_occurs_with", Weight: 0.3, Source: "co-occurrence",
	})
	_ = eng.store.UpsertRelationshipRecord(ctx, ws, id2, storage.RelationshipRecord{
		FromEntity: "A", ToEntity: "B", RelType: "co_occurs_with", Weight: 0.4, Source: "co-occurrence",
	})

	g, err := eng.ExportGraph(ctx, "dedup-vault", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(g.Edges) != 1 {
		t.Fatalf("want 1 deduplicated edge, got %d", len(g.Edges))
	}
	if g.Edges[0].Weight != 0.4 {
		t.Fatalf("want max weight 0.4, got %f", g.Edges[0].Weight)
	}
}

func TestExportGraph_CoOccursWithAutoPopulated(t *testing.T) {
	// Write an engram with two inline entities.
	// ExportGraph should return a co_occurs_with edge without any explicit UpsertRelationshipRecord call.
	eng, cleanup := testEnv(t)
	defer cleanup()

	ctx := context.Background()
	_, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:   "cooccur-test",
		Concept: "test",
		Content: "PostgreSQL uses Redis for caching session data.",
		Entities: []mbp.InlineEntity{
			{Name: "PostgreSQL", Type: "database"},
			{Name: "Redis", Type: "database"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	g, err := eng.ExportGraph(ctx, "cooccur-test", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(g.Edges) != 1 {
		t.Fatalf("want 1 co_occurs_with edge, got %d", len(g.Edges))
	}
	if g.Edges[0].RelType != "co_occurs_with" {
		t.Fatalf("want co_occurs_with, got %s", g.Edges[0].RelType)
	}
}

func TestExportGraph_EmptyVault(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()

	g, err := eng.ExportGraph(context.Background(), "empty-vault", false)
	require.NoError(t, err)
	require.NotNil(t, g)
	require.Empty(t, g.Nodes, "expected no nodes for empty vault")
	require.Empty(t, g.Edges, "expected no edges for empty vault")

	// JSON-LD export of empty graph should still be valid JSON.
	jsonLD, err := FormatGraphJSONLD(g)
	require.NoError(t, err)
	var doc map[string]any
	require.NoError(t, json.Unmarshal([]byte(jsonLD), &doc))
	graph, _ := doc["@graph"].([]any)
	require.Empty(t, graph)

	// GraphML export of empty graph should still be valid XML.
	graphML, err := FormatGraphGraphML(g)
	require.NoError(t, err)
	require.Contains(t, graphML, `<graphml`)
}

func TestExportGraph_DeterministicOrder(t *testing.T) {
	// Write multiple entities and relationships, call ExportGraph twice,
	// and assert that node and edge ordering is identical both times.
	eng, cleanup := testEnv(t)
	defer cleanup()

	ctx := context.Background()
	vault := "deterministic-test"

	// Write several engrams with different entities to create a diverse graph.
	entities := []struct {
		from     string
		fromType string
		to       string
		toType   string
	}{
		{"PostgreSQL", "database", "Redis", "cache"},
		{"Redis", "cache", "Python", "language"},
		{"Python", "language", "PostgreSQL", "database"},
		{"Kubernetes", "orchestrator", "Docker", "container"},
		{"Docker", "container", "Linux", "os"},
	}

	for _, e := range entities {
		ws := eng.store.ResolveVaultPrefix(vault)
		id := storage.ULID{}
		// Write some relationship records to populate the graph.
		_ = eng.store.UpsertRelationshipRecord(ctx, ws, id, storage.RelationshipRecord{
			FromEntity: e.from,
			ToEntity:   e.to,
			RelType:    "integrates_with",
			Weight:     0.8,
			Source:     "test",
		})
		// Also upsert entity records.
		_ = eng.store.UpsertEntityRecord(ctx, ws, storage.EntityRecord{
			Name: e.from, Type: e.fromType, Confidence: 1.0,
		}, "test")
		_ = eng.store.UpsertEntityRecord(ctx, ws, storage.EntityRecord{
			Name: e.to, Type: e.toType, Confidence: 1.0,
		}, "test")
	}

	// Export graph twice and compare results.
	g1, err := eng.ExportGraph(ctx, vault, true)
	require.NoError(t, err)

	g2, err := eng.ExportGraph(ctx, vault, true)
	require.NoError(t, err)

	// Nodes and edges should be in the same order both times.
	require.Equal(t, len(g1.Nodes), len(g2.Nodes), "node count mismatch")
	require.Equal(t, len(g1.Edges), len(g2.Edges), "edge count mismatch")

	for i := range g1.Nodes {
		require.Equal(t, g1.Nodes[i].ID, g2.Nodes[i].ID, "node ID mismatch at index %d", i)
		require.Equal(t, g1.Nodes[i].Type, g2.Nodes[i].Type, "node type mismatch at index %d", i)
	}

	for i := range g1.Edges {
		require.Equal(t, g1.Edges[i].From, g2.Edges[i].From, "edge From mismatch at index %d", i)
		require.Equal(t, g1.Edges[i].To, g2.Edges[i].To, "edge To mismatch at index %d", i)
		require.Equal(t, g1.Edges[i].RelType, g2.Edges[i].RelType, "edge RelType mismatch at index %d", i)
		require.Equal(t, g1.Edges[i].Weight, g2.Edges[i].Weight, "edge Weight mismatch at index %d", i)
	}

	// Verify nodes are sorted by ID.
	for i := 1; i < len(g1.Nodes); i++ {
		require.Less(t, g1.Nodes[i-1].ID, g1.Nodes[i].ID, "nodes not sorted by ID at index %d", i)
	}

	// Verify edges are sorted by (From, To, RelType).
	for i := 1; i < len(g1.Edges); i++ {
		prev := g1.Edges[i-1]
		curr := g1.Edges[i]
		if prev.From == curr.From {
			if prev.To == curr.To {
				require.Less(t, prev.RelType, curr.RelType, "edges not sorted by RelType at index %d", i)
			} else {
				require.Less(t, prev.To, curr.To, "edges not sorted by To at index %d", i)
			}
		} else {
			require.Less(t, prev.From, curr.From, "edges not sorted by From at index %d", i)
		}
	}
}
