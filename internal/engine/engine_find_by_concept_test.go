package engine

import (
	"context"
	"testing"

	"github.com/scrypster/muninndb/internal/storage"
	"github.com/stretchr/testify/require"
)

// TestFindByConcept_HappyPath verifies that an engram written with a concept
// is retrievable by exact concept match.
func TestFindByConcept_HappyPath(t *testing.T) {
	t.Parallel()
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	const vault = "find-by-concept-happy"
	const concept = "obs:Higley:higley_snapshot:2026-05-25"
	ws := eng.store.ResolveVaultPrefix(vault)

	written := &storage.Engram{Concept: concept, Content: `{"trucks_shipped_ytd":0}`}
	id, err := eng.store.WriteEngram(ctx, ws, written)
	require.NoError(t, err)

	results, err := eng.FindByConcept(ctx, vault, concept, 1)
	require.NoError(t, err)
	require.Len(t, results, 1, "should find exactly the one engram we wrote")
	require.Equal(t, id, results[0].ID, "returned ID must match the write")
	require.Equal(t, concept, results[0].Concept, "returned Concept must match the query")
}

// TestFindByConcept_ExcludesArchived verifies that archived engrams do not
// appear in FindByConcept results.
func TestFindByConcept_ExcludesArchived(t *testing.T) {
	t.Parallel()
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	const vault = "find-by-concept-archived"
	const concept = "obs:Test:snapshot:2026-05-25"
	ws := eng.store.ResolveVaultPrefix(vault)

	idArchived, err := eng.store.WriteEngram(ctx, ws, &storage.Engram{Concept: concept, Content: "v1"})
	require.NoError(t, err)
	// Write a second engram with the same concept that stays active
	idActive, err := eng.store.WriteEngram(ctx, ws, &storage.Engram{Concept: concept, Content: "v2"})
	require.NoError(t, err)

	// Archive the first one
	err = eng.UpdateLifecycleState(ctx, vault, idArchived.String(), "archived")
	require.NoError(t, err)

	results, err := eng.FindByConcept(ctx, vault, concept, 50)
	require.NoError(t, err)

	var foundActive, foundArchived bool
	for _, r := range results {
		if r.ID == idActive {
			foundActive = true
		}
		if r.ID == idArchived {
			foundArchived = true
		}
	}
	require.True(t, foundActive, "active engram should appear in results")
	require.False(t, foundArchived, "archived engram must NOT appear in results")
}

// TestFindByConcept_ExcludesSoftDeleted verifies that soft-deleted engrams do
// not appear in FindByConcept results.
func TestFindByConcept_ExcludesSoftDeleted(t *testing.T) {
	t.Parallel()
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	const vault = "find-by-concept-softdeleted"
	const concept = "obs:Test:soft:2026-05-25"
	ws := eng.store.ResolveVaultPrefix(vault)

	idDeleted, err := eng.store.WriteEngram(ctx, ws, &storage.Engram{Concept: concept, Content: "v1"})
	require.NoError(t, err)
	idActive, err := eng.store.WriteEngram(ctx, ws, &storage.Engram{Concept: concept, Content: "v2"})
	require.NoError(t, err)

	err = eng.store.SoftDelete(ctx, ws, idDeleted)
	require.NoError(t, err)

	results, err := eng.FindByConcept(ctx, vault, concept, 50)
	require.NoError(t, err)

	var foundActive, foundDeleted bool
	for _, r := range results {
		if r.ID == idActive {
			foundActive = true
		}
		if r.ID == idDeleted {
			foundDeleted = true
		}
	}
	require.True(t, foundActive, "active engram should appear in results")
	require.False(t, foundDeleted, "soft-deleted engram must NOT appear in results")
}

// TestFindByConcept_MultipleEngramsSameConcept verifies that multiple engrams
// sharing a concept are all returned, sorted newest-first by ULID, capped at
// limit.
func TestFindByConcept_MultipleEngramsSameConcept(t *testing.T) {
	t.Parallel()
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	const vault = "find-by-concept-multi"
	const concept = "obs:Test:multi:2026-05-25"
	ws := eng.store.ResolveVaultPrefix(vault)

	id1, err := eng.store.WriteEngram(ctx, ws, &storage.Engram{Concept: concept, Content: "v1"})
	require.NoError(t, err)
	id2, err := eng.store.WriteEngram(ctx, ws, &storage.Engram{Concept: concept, Content: "v2"})
	require.NoError(t, err)
	id3, err := eng.store.WriteEngram(ctx, ws, &storage.Engram{Concept: concept, Content: "v3"})
	require.NoError(t, err)

	// Limit=2 should return the 2 newest (id3 + id2), not id1.
	results, err := eng.FindByConcept(ctx, vault, concept, 2)
	require.NoError(t, err)
	require.Len(t, results, 2, "limit should cap to 2")
	require.Equal(t, id3, results[0].ID, "newest should come first")
	require.Equal(t, id2, results[1].ID, "second-newest should come second")
	require.NotEqual(t, id1, results[0].ID)
	require.NotEqual(t, id1, results[1].ID)

	// Limit=50 should return all 3.
	results, err = eng.FindByConcept(ctx, vault, concept, 50)
	require.NoError(t, err)
	require.Len(t, results, 3, "all three should be returned with limit=50")
}

// TestFindByConcept_EmptyConcept verifies that an empty concept argument
// returns an error.
func TestFindByConcept_EmptyConcept(t *testing.T) {
	t.Parallel()
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	_, err := eng.FindByConcept(ctx, "find-by-concept-empty", "", 1)
	require.Error(t, err)
	require.Contains(t, err.Error(), "concept is required")
}

// TestFindByConcept_NoMatch verifies that a concept that was never written
// returns an empty list, not an error.
func TestFindByConcept_NoMatch(t *testing.T) {
	t.Parallel()
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	const vault = "find-by-concept-nomatch"
	// Write a different concept to confirm the vault has data.
	ws := eng.store.ResolveVaultPrefix(vault)
	_, err := eng.store.WriteEngram(ctx, ws, &storage.Engram{Concept: "different", Content: "x"})
	require.NoError(t, err)

	results, err := eng.FindByConcept(ctx, vault, "this-concept-was-never-written", 1)
	require.NoError(t, err)
	require.Empty(t, results, "unknown concept should return empty list, not error")
}

// TestFindByConcept_VaultIsolation verifies that the concept index respects
// vault boundaries — same concept written in vault A should not be returned
// when queried against vault B.
func TestFindByConcept_VaultIsolation(t *testing.T) {
	t.Parallel()
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	const vaultA = "find-by-concept-vault-a"
	const vaultB = "find-by-concept-vault-b"
	const concept = "obs:Test:isolation:2026-05-25"

	wsA := eng.store.ResolveVaultPrefix(vaultA)
	idA, err := eng.store.WriteEngram(ctx, wsA, &storage.Engram{Concept: concept, Content: "A"})
	require.NoError(t, err)

	wsB := eng.store.ResolveVaultPrefix(vaultB)
	idB, err := eng.store.WriteEngram(ctx, wsB, &storage.Engram{Concept: concept, Content: "B"})
	require.NoError(t, err)

	// Query vault A — must return only A's engram.
	resultsA, err := eng.FindByConcept(ctx, vaultA, concept, 50)
	require.NoError(t, err)
	require.Len(t, resultsA, 1, "vault A query should return exactly one engram")
	require.Equal(t, idA, resultsA[0].ID, "must be the engram written in vault A")
	require.NotEqual(t, idB, resultsA[0].ID)

	// Query vault B — must return only B's engram.
	resultsB, err := eng.FindByConcept(ctx, vaultB, concept, 50)
	require.NoError(t, err)
	require.Len(t, resultsB, 1, "vault B query should return exactly one engram")
	require.Equal(t, idB, resultsB[0].ID, "must be the engram written in vault B")
	require.NotEqual(t, idA, resultsB[0].ID)
}

// TestFindByConcept_AfterDelete verifies that hard-deleting an engram removes
// its 0x2B index entry so subsequent FindByConcept calls do not return it.
func TestFindByConcept_AfterDelete(t *testing.T) {
	t.Parallel()
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	const vault = "find-by-concept-delete"
	const concept = "obs:Test:delete:2026-05-25"
	ws := eng.store.ResolveVaultPrefix(vault)

	id, err := eng.store.WriteEngram(ctx, ws, &storage.Engram{Concept: concept, Content: "v1"})
	require.NoError(t, err)

	// Confirm it's findable.
	results, err := eng.FindByConcept(ctx, vault, concept, 1)
	require.NoError(t, err)
	require.Len(t, results, 1)

	// Hard-delete.
	err = eng.store.DeleteEngram(ctx, ws, id)
	require.NoError(t, err)

	// Must NOT be findable anymore.
	results, err = eng.FindByConcept(ctx, vault, concept, 1)
	require.NoError(t, err)
	require.Empty(t, results, "after DeleteEngram, the concept index entry must also be gone")
}
