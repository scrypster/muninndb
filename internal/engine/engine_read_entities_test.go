package engine

import (
	"context"
	"testing"

	"github.com/scrypster/muninndb/internal/transport/mbp"
	"github.com/stretchr/testify/require"
)

// TestRead_IncludesEntities verifies that Read returns linked entities with their types.
func TestRead_IncludesEntities(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	writeResp, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:   "default",
		Concept: "test entity read",
		Content: "Alice and Bob are colleagues.",
		Entities: []mbp.InlineEntity{
			{Name: "Alice", Type: "person"},
			{Name: "Bob", Type: "person"},
		},
	})
	require.NoError(t, err)

	readResp, err := eng.Read(ctx, &mbp.ReadRequest{Vault: "default", ID: writeResp.ID})
	require.NoError(t, err)
	require.Len(t, readResp.Entities, 2)

	names := make(map[string]string, len(readResp.Entities))
	for _, e := range readResp.Entities {
		names[e.Name] = e.Type
	}
	require.Equal(t, "person", names["Alice"])
	require.Equal(t, "person", names["Bob"])
}

// TestRead_IncludesEntityRelationships verifies that Read returns entity-to-entity
// relationships sourced from this engram.
func TestRead_IncludesEntityRelationships(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	writeResp, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:   "default",
		Concept: "test rel read",
		Content: "Alice manages Bob.",
		Entities: []mbp.InlineEntity{
			{Name: "Alice", Type: "person"},
			{Name: "Bob", Type: "person"},
		},
		EntityRelationships: []mbp.InlineEntityRelationship{
			{FromEntity: "Alice", ToEntity: "Bob", RelType: "manages", Weight: 1.0},
		},
	})
	require.NoError(t, err)

	readResp, err := eng.Read(ctx, &mbp.ReadRequest{Vault: "default", ID: writeResp.ID})
	require.NoError(t, err)
	// co_occurs_with is filtered from the read response; only caller-provided relationships returned.
	require.Len(t, readResp.EntityRelationships, 1)
	rel := readResp.EntityRelationships[0]
	require.Equal(t, "Alice", rel.FromEntity)
	require.Equal(t, "Bob", rel.ToEntity)
	require.Equal(t, "manages", rel.RelType)
}

// TestRead_NoEntitiesReturnsEmptySlices verifies that an engram written without
// entities/relationships returns nil (omitted) slices — not empty arrays.
func TestRead_NoEntitiesReturnsEmptySlices(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	writeResp, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:   "default",
		Concept: "plain engram",
		Content: "No entities here.",
	})
	require.NoError(t, err)

	readResp, err := eng.Read(ctx, &mbp.ReadRequest{Vault: "default", ID: writeResp.ID})
	require.NoError(t, err)
	require.Empty(t, readResp.Entities)
	require.Empty(t, readResp.EntityRelationships)
}
