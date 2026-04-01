package engine

import (
	"context"
	"testing"

	"github.com/scrypster/muninndb/internal/storage"
	"github.com/stretchr/testify/require"
)

// TestFindByEntity_ExcludesArchived verifies that archived engrams do not appear
// in FindByEntity results.
func TestFindByEntity_ExcludesArchived(t *testing.T) {
	t.Parallel()
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	const vault = "find-by-entity-archived"
	ws := eng.store.ResolveVaultPrefix(vault)

	// Write two engrams and link both to the same entity.
	engA := &storage.Engram{Concept: "active-engram", Content: "This one stays active"}
	idA, err := eng.store.WriteEngram(ctx, ws, engA)
	require.NoError(t, err)

	engB := &storage.Engram{Concept: "archived-engram", Content: "This one gets archived"}
	idB, err := eng.store.WriteEngram(ctx, ws, engB)
	require.NoError(t, err)

	err = eng.store.UpsertEntityRecord(ctx, storage.EntityRecord{
		Name:   "SharedEntity",
		Type:   "concept",
		Source: "inline",
	}, "inline")
	require.NoError(t, err)
	err = eng.store.WriteEntityEngramLink(ctx, ws, idA, "SharedEntity")
	require.NoError(t, err)
	err = eng.store.WriteEntityEngramLink(ctx, ws, idB, "SharedEntity")
	require.NoError(t, err)

	// Archive engram B.
	err = eng.UpdateLifecycleState(ctx, vault, idB.String(), "archived")
	require.NoError(t, err)

	// FindByEntity must return only the active engram.
	results, err := eng.FindByEntity(ctx, vault, "SharedEntity", 50)
	require.NoError(t, err)

	var foundActive, foundArchived bool
	for _, r := range results {
		if r.ID == idA {
			foundActive = true
		}
		if r.ID == idB {
			foundArchived = true
		}
	}
	require.True(t, foundActive, "active engram A should appear in FindByEntity results")
	require.False(t, foundArchived, "archived engram B should NOT appear in FindByEntity results")
}

// TestFindByEntity_ExcludesSoftDeleted verifies that soft-deleted engrams do not
// appear in FindByEntity results.
func TestFindByEntity_ExcludesSoftDeleted(t *testing.T) {
	t.Parallel()
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	const vault = "find-by-entity-softdeleted"
	ws := eng.store.ResolveVaultPrefix(vault)

	engA := &storage.Engram{Concept: "active-engram", Content: "This one stays active"}
	idA, err := eng.store.WriteEngram(ctx, ws, engA)
	require.NoError(t, err)

	engB := &storage.Engram{Concept: "deleted-engram", Content: "This one gets deleted"}
	idB, err := eng.store.WriteEngram(ctx, ws, engB)
	require.NoError(t, err)

	err = eng.store.UpsertEntityRecord(ctx, storage.EntityRecord{
		Name:   "SharedEntity2",
		Type:   "concept",
		Source: "inline",
	}, "inline")
	require.NoError(t, err)
	err = eng.store.WriteEntityEngramLink(ctx, ws, idA, "SharedEntity2")
	require.NoError(t, err)
	err = eng.store.WriteEntityEngramLink(ctx, ws, idB, "SharedEntity2")
	require.NoError(t, err)

	err = eng.store.SoftDelete(ctx, ws, idB)
	require.NoError(t, err)

	results, err := eng.FindByEntity(ctx, vault, "SharedEntity2", 50)
	require.NoError(t, err)

	var foundActive, foundDeleted bool
	for _, r := range results {
		if r.ID == idA {
			foundActive = true
		}
		if r.ID == idB {
			foundDeleted = true
		}
	}
	require.True(t, foundActive, "active engram A should appear in FindByEntity results")
	require.False(t, foundDeleted, "soft-deleted engram B should NOT appear in FindByEntity results")
}
