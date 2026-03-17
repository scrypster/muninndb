package engine

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/cognitive"
	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/transport/mbp"
	"github.com/stretchr/testify/require"
)

// TestHebbian_WeightStrengthenAfterCoActivation is a cognitive package integration
// test that verifies the Hebbian learning cycle updates association weights in
// real storage after a co-activation event is submitted and flushed.
func TestHebbian_WeightStrengthenAfterCoActivation(t *testing.T) {
	t.Parallel()

	dir, err := os.MkdirTemp("", "muninndb-hebbian-test-*")
	require.NoError(t, err)
	defer os.RemoveAll(dir)

	db, err := storage.OpenPebble(dir, storage.DefaultOptions())
	require.NoError(t, err)

	store := storage.NewPebbleStore(db, storage.PebbleStoreConfig{CacheSize: 100})
	defer store.Close()

	ctx := context.Background()

	// Create a HebbianWorker backed by real storage via the adapter.
	hebbAdapter := cognitive.NewHebbianStoreAdapter(store)
	hw := cognitive.NewHebbianWorker(hebbAdapter)

	// Set up vault workspace and two engram IDs.
	ws := store.VaultPrefix("test-hebbian")

	var idA, idB storage.ULID
	idA[0] = 0x01
	idA[1] = 0x02
	idB[0] = 0x0A
	idB[1] = 0x0B

	// Write a forward association with an initial weight of 0.1.
	err = store.WriteAssociation(ctx, ws, idA, idB, &storage.Association{
		TargetID:   idB,
		RelType:    storage.RelSupports,
		Weight:     0.1,
		Confidence: 1.0,
		CreatedAt:  time.Now(),
	})
	require.NoError(t, err)

	// Read back initial weight to confirm it was stored.
	// GetAssocWeight uses canonical (sorted) pair ordering; returns 0 if not indexed.
	initialWeight, err := store.GetAssocWeight(ctx, ws, idA, idB)
	require.NoError(t, err)
	// If the initial write didn't populate the weight index, the weight may be zero — that's ok
	// because Hebbian cold-starts at 0.01.

	// Submit a co-activation event with both engrams at high score.
	hw.Submit(cognitive.CoActivationEvent{
		WS: ws,
		At: time.Now(),
		Engrams: []cognitive.CoActivatedEngram{
			{ID: [16]byte(idA), Score: 0.9},
			{ID: [16]byte(idB), Score: 0.9},
		},
	})

	// Stop flushes and drains all pending items before returning.
	hw.Stop()

	// Read the updated weight from storage.
	newWeight, err := store.GetAssocWeight(ctx, ws, idA, idB)
	require.NoError(t, err)

	// The Hebbian rule strengthens from cold-start (0.01) or from initialWeight.
	// Either way, newWeight must be greater than the effective starting weight.
	effectiveStart := initialWeight
	if effectiveStart <= 0 {
		effectiveStart = 0.01 // HebbianWorker cold-start seed
	}

	require.Greater(t, newWeight, effectiveStart,
		"Hebbian learning should have increased the association weight (initial=%.4f, new=%.4f)",
		effectiveStart, newWeight)
}

// TestEntityCoOccurrence_PopulatesRelationshipGraph verifies that writing an
// engram with two inline entities automatically creates a co_occurs_with
// relationship record in the vault-scoped relationship index.
func TestEntityCoOccurrence_PopulatesRelationshipGraph(t *testing.T) {
	t.Parallel()

	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	resp, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:   "entity-cooccur-test",
		Concept: "database systems",
		Content: "PostgreSQL and Redis are used together",
		Entities: []mbp.InlineEntity{
			{Name: "PostgreSQL", Type: "database"},
			{Name: "Redis", Type: "cache"},
		},
	})
	require.NoError(t, err)
	require.NotEmpty(t, resp.ID)

	// Scan vault-scoped relationship records for the co_occurs_with entry.
	ws := eng.store.ResolveVaultPrefix("entity-cooccur-test")
	var found bool
	_ = eng.store.ScanRelationships(ctx, ws, func(r storage.RelationshipRecord) error {
		if r.RelType == "co_occurs_with" &&
			((r.FromEntity == "PostgreSQL" && r.ToEntity == "Redis") ||
				(r.FromEntity == "Redis" && r.ToEntity == "PostgreSQL")) {
			found = true
		}
		return nil
	})

	require.True(t, found,
		"co_occurs_with relationship should be auto-populated for co-occurring entities")
}

// TestActivate_ConcurrentSafety launches 10 concurrent Activate calls against
// the same vault and verifies that no panics, races, or errors occur.
func TestActivate_ConcurrentSafety(t *testing.T) {
	t.Parallel()

	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	// Write a handful of engrams to give FTS something to search.
	for i := 0; i < 5; i++ {
		_, err := eng.Write(ctx, &mbp.WriteRequest{
			Vault:   "concurrent-test",
			Concept: fmt.Sprintf("concept %d", i),
			Content: fmt.Sprintf("content about topic number %d for searching", i),
		})
		require.NoError(t, err)
	}

	// Wait for the async FTS worker to index the written engrams.
	awaitFTS(t, eng)

	// Launch 10 concurrent Activate calls.
	var wg sync.WaitGroup
	errs := make([]error, 10)
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			_, errs[idx] = eng.Activate(ctx, &mbp.ActivateRequest{
				Vault:      "concurrent-test",
				Context:    []string{"topic content"},
				MaxResults: 5,
				Threshold:  0.01,
			})
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		require.NoError(t, err, "concurrent Activate[%d] failed", i)
	}
}

// TestWriteBatch_EntityCoOccurrenceMultipleEngrams writes two engrams (each
// with two inline entities) via WriteBatch and verifies that at least two
// co_occurs_with relationship records are written for the two entity pairs.
func TestWriteBatch_EntityCoOccurrenceMultipleEngrams(t *testing.T) {
	t.Parallel()

	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	reqs := []*mbp.WriteRequest{
		{
			Vault:   "batch-entity-test",
			Concept: "first record",
			Content: "Alice and Bob worked together on the project",
			Entities: []mbp.InlineEntity{
				{Name: "Alice", Type: "person"},
				{Name: "Bob", Type: "person"},
			},
		},
		{
			Vault:   "batch-entity-test",
			Concept: "second record",
			Content: "Carol and David led the team",
			Entities: []mbp.InlineEntity{
				{Name: "Carol", Type: "person"},
				{Name: "David", Type: "person"},
			},
		},
	}

	responses, errs := eng.WriteBatch(ctx, reqs)
	require.Len(t, responses, 2)
	require.Len(t, errs, 2)
	require.NoError(t, errs[0])
	require.NoError(t, errs[1])
	require.NotEmpty(t, responses[0].ID)
	require.NotEmpty(t, responses[1].ID)

	// Scan relationships — expect at least one co_occurs_with pair per engram.
	ws := eng.store.ResolveVaultPrefix("batch-entity-test")
	relPairs := make(map[string]bool)
	_ = eng.store.ScanRelationships(ctx, ws, func(r storage.RelationshipRecord) error {
		if r.RelType == "co_occurs_with" {
			key := r.FromEntity + "+" + r.ToEntity
			relPairs[key] = true
		}
		return nil
	})

	require.GreaterOrEqual(t, len(relPairs), 2,
		"expected at least 2 distinct co_occurs_with pairs from the two engram writes; got %d", len(relPairs))
}

// TestEvolve_ChainPreservesContent tests that Evolve creates a new engram with
// the updated content, soft-deletes the original, and the new engram is readable.
func TestEvolve_ChainPreservesContent(t *testing.T) {
	t.Parallel()

	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	// Write the original engram.
	resp, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:   "evolve-test",
		Concept: "original concept",
		Content: "Original content v1",
	})
	require.NoError(t, err)
	origID := resp.ID

	// Evolve creates a new engram and soft-deletes the old one.
	newID, err := eng.Evolve(ctx, "evolve-test", origID, "Updated content v2", "improvement", nil)
	require.NoError(t, err)
	require.NotEqual(t, storage.ULID{}, newID, "evolved ID should not be the zero value")
	require.NotEqual(t, origID, newID.String(), "evolved ID should differ from original ID")

	// The new engram should be readable with the updated content.
	readResp, err := eng.Read(ctx, &mbp.ReadRequest{
		Vault: "evolve-test",
		ID:    newID.String(),
	})
	require.NoError(t, err)
	require.Equal(t, "Updated content v2", readResp.Content,
		"new engram content should match the evolved content")

	// The original engram should be soft-deleted in storage.
	ws := eng.store.ResolveVaultPrefix("evolve-test")
	oldULID, err := storage.ParseULID(origID)
	require.NoError(t, err)

	oldEng, err := eng.store.GetEngram(ctx, ws, oldULID)
	require.NoError(t, err)
	require.NotNil(t, oldEng, "original engram should still exist in storage (soft-deleted)")
	require.Equal(t, storage.StateSoftDeleted, oldEng.State,
		"original engram should be in StateSoftDeleted state after Evolve")
}
