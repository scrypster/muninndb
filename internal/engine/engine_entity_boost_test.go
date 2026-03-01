package engine

import (
	"context"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/engine/activation"
	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/transport/mbp"
	"github.com/stretchr/testify/require"
)

// TestEntityBoost_SurfacesEntityLinkedEngram verifies that the post-BFS entity
// boost phase surfaces an engram that shares a named entity with a top BFS
// result, even when no direct association edge connects them to the query.
//
// Setup:
//   - engram A: "PostgreSQL primary database" — matches query well via FTS
//   - engram B: "PostgreSQL replica configuration" — linked to entity "PostgreSQL"
//     but NOT directly associated with A, and content does not strongly match query
//   - engram C: "Redis caching layer" — linked to entity "Redis" only (control)
//
// After BFS, A should rank first. The entity boost phase should then scan A's
// entity links, find "PostgreSQL", and discover B. B must appear in the results
// with a non-zero score (entityBoostFactor = 0.15).
func TestEntityBoost_SurfacesEntityLinkedEngram(t *testing.T) {
	t.Parallel()
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	const vault = "boost-test"

	// Write engram A — strong FTS match for the query.
	respA, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:   vault,
		Concept: "primary database choice",
		Content: "We use PostgreSQL as the primary relational database for all transactional workloads",
		Entities: []mbp.InlineEntity{
			{Name: "PostgreSQL", Type: "database"},
		},
	})
	require.NoError(t, err)
	require.NotEmpty(t, respA.ID)

	// Write engram B — linked to same entity "PostgreSQL" but content is
	// deliberately different so it would not be surfaced by FTS alone.
	respB, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:   vault,
		Concept: "replica configuration",
		Content: "Read replica configuration for streaming replication failover setup",
		Entities: []mbp.InlineEntity{
			{Name: "PostgreSQL", Type: "database"},
		},
	})
	require.NoError(t, err)
	require.NotEmpty(t, respB.ID)

	// Write engram C — control: different entity, should not be entity-boosted.
	_, err = eng.Write(ctx, &mbp.WriteRequest{
		Vault:   vault,
		Concept: "caching layer",
		Content: "Redis is used as an in-memory cache for session data",
		Entities: []mbp.InlineEntity{
			{Name: "Redis", Type: "cache"},
		},
	})
	require.NoError(t, err)

	// Wait for async FTS worker to index the written engrams.
	time.Sleep(300 * time.Millisecond)

	// Query for "primary relational database" — should strongly match engram A.
	// Threshold is low to allow entity-boosted engrams through.
	resp, err := eng.Activate(ctx, &mbp.ActivateRequest{
		Vault:      vault,
		Context:    []string{"primary relational database"},
		MaxResults: 20,
		Threshold:  0.01,
	})
	require.NoError(t, err)

	// Build a map of returned IDs for easy lookup.
	idSet := make(map[string]float32, len(resp.Activations))
	for _, item := range resp.Activations {
		idSet[item.ID] = item.Score
	}

	// Engram A must be in results (strong FTS match).
	_, aFound := idSet[respA.ID]
	require.True(t, aFound, "engram A (strong FTS match) should be in results")

	// Engram B must be in results because of entity boost via "PostgreSQL".
	bScore, bFound := idSet[respB.ID]
	require.True(t, bFound, "engram B should be surfaced by entity boost (shares 'PostgreSQL' entity with top result A)")
	require.Greater(t, bScore, float32(0), "engram B score should be > 0 (boosted by entity spread activation)")
}

// TestEntityBoost_ApplyEntityBoostDirect tests the applyEntityBoost helper
// directly, bypassing the full activation pipeline. This verifies the core
// boost logic without requiring FTS indexing delay.
func TestEntityBoost_ApplyEntityBoostDirect(t *testing.T) {
	t.Parallel()
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	const vault = "boost-direct-test"
	ws := eng.store.ResolveVaultPrefix(vault)

	// Write engram A and link it to entity "PostgreSQL".
	engramA := &storage.Engram{
		Concept:    "db-a",
		Content:    "PostgreSQL is the primary database",
		Confidence: 0.9,
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

	// Write engram B — also linked to "PostgreSQL" but no BFS association from A.
	engramB := &storage.Engram{
		Concept:    "db-b",
		Content:    "Replica setup for replication",
		Confidence: 0.8,
	}
	idB, err := eng.store.WriteEngram(ctx, ws, engramB)
	require.NoError(t, err)
	err = eng.store.WriteEntityEngramLink(ctx, ws, idB, "PostgreSQL")
	require.NoError(t, err)

	// Write engram C — NOT linked to "PostgreSQL" (control).
	engramC := &storage.Engram{
		Concept:    "cache-c",
		Content:    "Redis caching layer",
		Confidence: 0.7,
	}
	idC, err := eng.store.WriteEngram(ctx, ws, engramC)
	require.NoError(t, err)

	// Re-read A so it has a non-nil Engram pointer with the correct ID set.
	fullA, err := eng.store.GetEngram(ctx, ws, idA)
	require.NoError(t, err)
	require.NotNil(t, fullA)

	// Build a synthetic BFS result containing only engram A.
	initialResults := []activation.ScoredEngram{
		{Engram: fullA, Score: 0.8},
	}

	// Apply entity boost.
	boosted := eng.applyEntityBoost(ctx, ws, initialResults)

	// Build ID set from boosted results.
	idSet := make(map[storage.ULID]float64, len(boosted))
	for _, r := range boosted {
		idSet[r.Engram.ID] = r.Score
	}

	// Engram A must remain in results with its original score (or higher if also entity-linked to itself).
	aScore, aFound := idSet[idA]
	require.True(t, aFound, "engram A should remain in boosted results")
	require.GreaterOrEqual(t, aScore, 0.8, "engram A score should not decrease")

	// Engram B must be added with entityBoostFactor score.
	bScore, bFound := idSet[idB]
	require.True(t, bFound, "engram B should be added by entity boost")
	require.InDelta(t, entityBoostFactor, bScore, 0.001, "engram B score should equal entityBoostFactor")

	// Engram C must NOT be in results (different entity, no entity link written).
	_, cFound := idSet[idC]
	require.False(t, cFound, "engram C (different entity) should not be in boosted results")
}
