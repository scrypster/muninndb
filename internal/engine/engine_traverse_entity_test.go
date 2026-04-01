package engine

import (
	"context"
	"testing"

	"github.com/scrypster/muninndb/internal/storage"
	"github.com/stretchr/testify/require"
)

// TestTraverse_FollowEntities_False verifies that with follow_entities=false, engrams
// reachable only via a shared entity link (but no direct association) are NOT returned.
func TestTraverse_FollowEntities_False(t *testing.T) {
	t.Parallel()
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	const vault = "traverse-entity-false"
	ws := eng.store.ResolveVaultPrefix(vault)

	// Write engram A and link it to entity "PostgreSQL".
	engramA := &storage.Engram{
		Concept: "engram-A",
		Content: "PostgreSQL primary database",
	}
	idA, err := eng.store.WriteEngram(ctx, ws, engramA)
	require.NoError(t, err)

	err = eng.store.UpsertEntityRecord(ctx, storage.EntityRecord{
		Name:   "PostgreSQL",
		Type:   "database",
		Source: "inline",
	}, "inline")
	require.NoError(t, err)
	err = eng.store.WriteEntityEngramLink(ctx, ws, idA, "PostgreSQL")
	require.NoError(t, err)

	// Write engram B — no entity links, no association to A.
	engramB := &storage.Engram{
		Concept: "engram-B",
		Content: "Unrelated memory with no links",
	}
	_, err = eng.store.WriteEngram(ctx, ws, engramB)
	require.NoError(t, err)

	// Write engram C — also linked to "PostgreSQL" but NOT directly associated with A.
	engramC := &storage.Engram{
		Concept: "engram-C",
		Content: "PostgreSQL replica configuration",
	}
	idC, err := eng.store.WriteEngram(ctx, ws, engramC)
	require.NoError(t, err)
	err = eng.store.WriteEntityEngramLink(ctx, ws, idC, "PostgreSQL")
	require.NoError(t, err)

	// Traverse from A with follow_entities=false and max_depth=2.
	// C is only reachable via entity link — must NOT appear.
	nodes, _, err := eng.Traverse(ctx, vault, idA.String(), 2, 100, false)
	require.NoError(t, err)

	foundC := false
	for _, n := range nodes {
		if n.ID == idC {
			foundC = true
		}
	}
	require.False(t, foundC, "engram C should NOT be reachable when follow_entities=false (no direct association)")
}

// TestTraverse_FollowEntities_True verifies that with follow_entities=true, engrams
// reachable only via a shared entity link are returned.
func TestTraverse_FollowEntities_True(t *testing.T) {
	t.Parallel()
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	const vault = "traverse-entity-true"
	ws := eng.store.ResolveVaultPrefix(vault)

	// Write engram A and link it to entity "PostgreSQL".
	engramA := &storage.Engram{
		Concept: "engram-A",
		Content: "PostgreSQL primary database",
	}
	idA, err := eng.store.WriteEngram(ctx, ws, engramA)
	require.NoError(t, err)

	err = eng.store.UpsertEntityRecord(ctx, storage.EntityRecord{
		Name:   "PostgreSQL",
		Type:   "database",
		Source: "inline",
	}, "inline")
	require.NoError(t, err)
	err = eng.store.WriteEntityEngramLink(ctx, ws, idA, "PostgreSQL")
	require.NoError(t, err)

	// Write engram B — direct association from A to B (not via entity).
	engramB := &storage.Engram{
		Concept: "engram-B",
		Content: "Direct neighbour of A",
	}
	idB, err := eng.store.WriteEngram(ctx, ws, engramB)
	require.NoError(t, err)

	assoc := &storage.Association{
		TargetID:   idB,
		RelType:    storage.RelRelatesTo,
		Weight:     0.9,
		Confidence: 1.0,
	}
	err = eng.store.WriteAssociation(ctx, ws, idA, idB, assoc)
	require.NoError(t, err)

	// Write engram C — also linked to "PostgreSQL" but NO direct association with A.
	engramC := &storage.Engram{
		Concept: "engram-C",
		Content: "PostgreSQL replica configuration",
	}
	idC, err := eng.store.WriteEngram(ctx, ws, engramC)
	require.NoError(t, err)
	err = eng.store.WriteEntityEngramLink(ctx, ws, idC, "PostgreSQL")
	require.NoError(t, err)

	// Traverse from A with follow_entities=true and max_depth=2.
	// C must be reachable via A → "PostgreSQL" ← C entity hop.
	nodes, edges, err := eng.Traverse(ctx, vault, idA.String(), 2, 100, true)
	require.NoError(t, err)

	idSet := make(map[storage.ULID]bool, len(nodes))
	for _, n := range nodes {
		idSet[n.ID] = true
	}

	// A must be in results (it is the start node).
	require.True(t, idSet[idA], "start engram A must be in traversal results")

	// B must be in results (direct association edge).
	require.True(t, idSet[idB], "engram B must be in results (direct association from A)")

	// C must be in results (entity hop via 'PostgreSQL').
	require.True(t, idSet[idC], "engram C must be reachable via entity hop through 'PostgreSQL'")

	// Verify there is an entity-hop edge A→C with entityHopWeight.
	foundEntityEdge := false
	for _, e := range edges {
		if e.From == idA && e.To == idC && e.Weight == entityHopWeight {
			foundEntityEdge = true
		}
	}
	require.True(t, foundEntityEdge, "expected entity-hop edge A→C with weight %v", entityHopWeight)
}

// TestTraverse_FollowEntities_CrossVaultSkip verifies that entity hops do not
// cross vault boundaries.
func TestTraverse_FollowEntities_CrossVaultSkip(t *testing.T) {
	t.Parallel()
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	const vaultA = "traverse-entity-cv-a"
	const vaultB = "traverse-entity-cv-b"
	wsA := eng.store.ResolveVaultPrefix(vaultA)
	wsB := eng.store.ResolveVaultPrefix(vaultB)

	// Write engram A in vaultA linked to entity "SharedEntity".
	engramA := &storage.Engram{Concept: "cv-A", Content: "in vault A"}
	idA, err := eng.store.WriteEngram(ctx, wsA, engramA)
	require.NoError(t, err)

	err = eng.store.UpsertEntityRecord(ctx, storage.EntityRecord{
		Name: "SharedEntity", Type: "concept", Source: "inline",
	}, "inline")
	require.NoError(t, err)
	err = eng.store.WriteEntityEngramLink(ctx, wsA, idA, "SharedEntity")
	require.NoError(t, err)

	// Write engram X in vaultB also linked to "SharedEntity".
	engramX := &storage.Engram{Concept: "cv-X", Content: "in vault B"}
	idX, err := eng.store.WriteEngram(ctx, wsB, engramX)
	require.NoError(t, err)
	err = eng.store.WriteEntityEngramLink(ctx, wsB, idX, "SharedEntity")
	require.NoError(t, err)

	// Traverse from A in vaultA with follow_entities=true.
	// X is in vaultB — must NOT appear.
	nodes, _, err := eng.Traverse(ctx, vaultA, idA.String(), 2, 100, true)
	require.NoError(t, err)

	for _, n := range nodes {
		require.NotEqual(t, idX, n.ID, "cross-vault engram X must not appear in traversal of vaultA")
	}
}

// TestTraverse_FollowEntities_VisitedSetPreventsInfiniteLoop verifies that
// a cycle via entity links does not cause infinite looping.
func TestTraverse_FollowEntities_VisitedSetPreventsInfiniteLoop(t *testing.T) {
	t.Parallel()
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	const vault = "traverse-entity-cycle"
	ws := eng.store.ResolveVaultPrefix(vault)

	err := eng.store.UpsertEntityRecord(ctx, storage.EntityRecord{
		Name: "CycleEntity", Type: "concept", Source: "inline",
	}, "inline")
	require.NoError(t, err)

	// Write three engrams all sharing "CycleEntity" — forms a dense entity cluster.
	var ids [3]storage.ULID
	for i := range ids {
		eng_ := &storage.Engram{Concept: "cycle-node", Content: "cycle content"}
		ids[i], err = eng.store.WriteEngram(ctx, ws, eng_)
		require.NoError(t, err)
		err = eng.store.WriteEntityEngramLink(ctx, ws, ids[i], "CycleEntity")
		require.NoError(t, err)
	}

	// This must complete without hanging or stack overflow.
	nodes, _, err := eng.Traverse(ctx, vault, ids[0].String(), 3, 100, true)
	require.NoError(t, err)

	// All three nodes should be reachable via entity hops.
	idSet := make(map[storage.ULID]bool)
	for _, n := range nodes {
		idSet[n.ID] = true
	}
	for _, id := range ids {
		require.True(t, idSet[id], "all cycle-entity-linked engrams should be reachable")
	}
}

// TestTraverse_FollowEntities_SoftDeletedViaEntity verifies that soft-deleted
// engrams are excluded from traversal results, even when follow_entities=true.
func TestTraverse_FollowEntities_SoftDeletedViaEntity(t *testing.T) {
	t.Parallel()
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	const vault = "traverse-entity-softdelete"
	ws := eng.store.ResolveVaultPrefix(vault)

	// Write engram A and link it to entity "TestEntity".
	engramA := &storage.Engram{
		Concept: "engram-A",
		Content: "First engram linked to TestEntity",
	}
	idA, err := eng.store.WriteEngram(ctx, ws, engramA)
	require.NoError(t, err)

	err = eng.store.UpsertEntityRecord(ctx, storage.EntityRecord{
		Name:   "TestEntity",
		Type:   "concept",
		Source: "inline",
	}, "inline")
	require.NoError(t, err)
	err = eng.store.WriteEntityEngramLink(ctx, ws, idA, "TestEntity")
	require.NoError(t, err)

	// Write engram B and link it to entity "TestEntity".
	engramB := &storage.Engram{
		Concept: "engram-B",
		Content: "Second engram linked to TestEntity",
	}
	idB, err := eng.store.WriteEngram(ctx, ws, engramB)
	require.NoError(t, err)
	err = eng.store.WriteEntityEngramLink(ctx, ws, idB, "TestEntity")
	require.NoError(t, err)

	// Soft-delete engram B.
	err = eng.store.SoftDelete(ctx, ws, idB)
	require.NoError(t, err)

	// Traverse from A with follow_entities=true and max_depth=2.
	// B is reachable via entity link, but since it's soft-deleted, it must NOT appear.
	nodes, _, err := eng.Traverse(ctx, vault, idA.String(), 2, 100, true)
	require.NoError(t, err)

	foundB := false
	for _, n := range nodes {
		if n.ID == idB {
			foundB = true
		}
	}
	require.False(t, foundB, "soft-deleted engram B should NOT appear in traversal, even via entity hop")
}


// TestTraverse_FollowEntities_ArchivedViaEntity verifies that archived engrams
// are excluded from traversal results, even when follow_entities=true.
func TestTraverse_FollowEntities_ArchivedViaEntity(t *testing.T) {
	t.Parallel()
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	const vault = "traverse-entity-archived"
	ws := eng.store.ResolveVaultPrefix(vault)

	// Write engram A and link it to entity "ArchivedEntity".
	engramA := &storage.Engram{
		Concept: "engram-A",
		Content: "First engram linked to ArchivedEntity",
	}
	idA, err := eng.store.WriteEngram(ctx, ws, engramA)
	require.NoError(t, err)

	err = eng.store.UpsertEntityRecord(ctx, storage.EntityRecord{
		Name:   "ArchivedEntity",
		Type:   "concept",
		Source: "inline",
	}, "inline")
	require.NoError(t, err)
	err = eng.store.WriteEntityEngramLink(ctx, ws, idA, "ArchivedEntity")
	require.NoError(t, err)

	// Write engram B and link it to entity "ArchivedEntity".
	engramB := &storage.Engram{
		Concept: "engram-B",
		Content: "Second engram linked to ArchivedEntity",
	}
	idB, err := eng.store.WriteEngram(ctx, ws, engramB)
	require.NoError(t, err)
	err = eng.store.WriteEntityEngramLink(ctx, ws, idB, "ArchivedEntity")
	require.NoError(t, err)

	// Archive engram B via lifecycle state transition.
	err = eng.UpdateLifecycleState(ctx, vault, idB.String(), "archived")
	require.NoError(t, err)

	// Traverse from A with follow_entities=true and max_depth=2.
	// B is reachable via entity link, but since it is archived, it must NOT appear.
	nodes, _, err := eng.Traverse(ctx, vault, idA.String(), 2, 100, true)
	require.NoError(t, err)

	foundB := false
	for _, n := range nodes {
		if n.ID == idB {
			foundB = true
		}
	}
	require.False(t, foundB, "archived engram B should NOT appear in traversal, even via entity hop")
}
