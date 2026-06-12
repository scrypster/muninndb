package engine

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

func TestEvolve_AtomicBatch_OldSoftDeletedNewReadable(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	resp, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault: "test", Concept: "Original", Content: "old content",
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	newID, err := eng.Evolve(ctx, "test", resp.ID, "new content", "update", nil, "")
	if err != nil {
		t.Fatalf("Evolve: %v", err)
	}

	ws := eng.store.ResolveVaultPrefix("test")
	oldULID, err := storage.ParseULID(resp.ID)
	if err != nil {
		t.Fatalf("ParseULID old: %v", err)
	}

	// Old engram must be soft-deleted.
	old, err := eng.store.GetEngram(ctx, ws, oldULID)
	if err != nil {
		t.Fatalf("GetEngram old: %v", err)
	}
	if old == nil {
		t.Fatal("old engram not found after Evolve")
	}
	if old.State != storage.StateSoftDeleted {
		t.Errorf("old engram state = %v, want StateSoftDeleted", old.State)
	}

	// New engram must be readable and active.
	newEng, err := eng.store.GetEngram(ctx, ws, newID)
	if err != nil {
		t.Fatalf("GetEngram new: %v", err)
	}
	if newEng == nil {
		t.Fatal("new engram not found after Evolve")
	}
	if newEng.State != storage.StateActive {
		t.Errorf("new engram state = %v, want StateActive", newEng.State)
	}

	// Verify supersedes association was written.
	assocMap, err := eng.store.GetAssociations(ctx, ws, []storage.ULID{newID}, 10)
	require.NoError(t, err)
	assocs := assocMap[newID]
	require.Len(t, assocs, 1, "supersedes association must exist")
	assert.Equal(t, oldULID, assocs[0].TargetID, "association must point to old engram")
	assert.Equal(t, storage.RelSupersedes, assocs[0].RelType, "association type must be RelSupersedes")
}

// TestEvolve_ConceptStableAcrossRepeatedEvolution verifies that the concept field
// is inherited verbatim from the predecessor on every Evolve call when no new
// concept is provided. Lineage is recorded via RelSupersedes, not by mutating
// the concept string.
func TestEvolve_ConceptStableAcrossRepeatedEvolution(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	const originalConcept = "fact: example service — canonical specs"

	resp, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault: "test", Concept: originalConcept, Content: "v1",
	})
	require.NoError(t, err)

	// First evolution with empty concept: must inherit original verbatim.
	id1, err := eng.Evolve(ctx, "test", resp.ID, "v2", "first update", nil, "")
	require.NoError(t, err)

	ws := eng.store.ResolveVaultPrefix("test")
	eng1, err := eng.store.GetEngram(ctx, ws, id1)
	require.NoError(t, err)
	require.NotNil(t, eng1)
	assert.Equal(t, originalConcept, eng1.Concept,
		"concept must be inherited verbatim from predecessor, no suffix")

	// Second evolution with empty concept: must still equal original.
	id2, err := eng.Evolve(ctx, "test", id1.String(), "v3", "second update", nil, "")
	require.NoError(t, err)

	eng2, err := eng.store.GetEngram(ctx, ws, id2)
	require.NoError(t, err)
	require.NotNil(t, eng2)
	assert.Equal(t, originalConcept, eng2.Concept,
		"concept must remain stable across repeated evolutions, no accumulating suffix")
}

// TestEvolve_ConceptRenameWhenProvided verifies that when a non-empty concept is
// supplied, the new version takes that concept rather than inheriting the predecessor's.
// This lets callers correct concepts that encode mutable state (#483).
func TestEvolve_ConceptRenameWhenProvided(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	resp, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault: "test", Concept: "answer owed to Alice on #895", Content: "v1",
	})
	require.NoError(t, err)

	newID, err := eng.Evolve(ctx, "test", resp.ID, "answer sent — closed", "paid the debt", nil, "answer to Alice on #895 — CLOSED")
	require.NoError(t, err)

	ws := eng.store.ResolveVaultPrefix("test")
	newEng, err := eng.store.GetEngram(ctx, ws, newID)
	require.NoError(t, err)
	require.NotNil(t, newEng)
	assert.Equal(t, "answer to Alice on #895 — CLOSED", newEng.Concept,
		"provided concept must be used instead of inheriting predecessor's")

	// Predecessor concept is unchanged (it just becomes soft-deleted).
	oldULID, _ := storage.ParseULID(resp.ID)
	oldEng, err := eng.store.GetEngram(ctx, ws, oldULID)
	require.NoError(t, err)
	assert.Equal(t, "answer owed to Alice on #895", oldEng.Concept,
		"predecessor concept must not be mutated")
}
