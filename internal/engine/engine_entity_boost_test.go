package engine

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/engine/activation"
	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/transport/mbp"
	"github.com/stretchr/testify/assert"
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
	awaitFTS(t, eng)

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
	boosted := eng.applyEntityBoost(ctx, ws, initialResults, nil, false)

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

// TestEntityBoost_MaxResultsRespectedAfterBoost verifies that max_results is
// enforced even when the entity boost phase appends additional engrams beyond
// the limit. Regression test for issue #171.
func TestEntityBoost_MaxResultsRespectedAfterBoost(t *testing.T) {
	t.Parallel()
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	const vault = "max-results-test"

	// Write one strong-match engram tagged with entity "PostgreSQL".
	_, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:   vault,
		Concept: "primary database",
		Content: "PostgreSQL primary relational database for transactional workloads",
		Entities: []mbp.InlineEntity{
			{Name: "PostgreSQL", Type: "database"},
		},
	})
	require.NoError(t, err)

	// Write many additional entity-linked engrams; the entity boost phase may
	// append these to results after the BFS limit has been applied.
	for i := range 8 {
		_, err := eng.Write(ctx, &mbp.WriteRequest{
			Vault:   vault,
			Concept: "related config",
			Content: fmt.Sprintf("PostgreSQL related engram %d configuration details", i),
			Entities: []mbp.InlineEntity{
				{Name: "PostgreSQL", Type: "database"},
			},
		})
		require.NoError(t, err)
	}

	const maxResults = 3
	resp, err := eng.Activate(ctx, &mbp.ActivateRequest{
		Vault:      vault,
		Context:    []string{"PostgreSQL database"},
		MaxResults: maxResults,
	})
	require.NoError(t, err)
	require.LessOrEqual(t, len(resp.Activations), maxResults,
		"expected at most %d activations after entity boost, got %d", maxResults, len(resp.Activations))

	// Verify descending score order — entity boost re-sorts, truncation must preserve it.
	for i := 1; i < len(resp.Activations); i++ {
		if resp.Activations[i].Score > resp.Activations[i-1].Score {
			t.Errorf("activations not sorted descending at index %d: %.3f > %.3f",
				i, resp.Activations[i].Score, resp.Activations[i-1].Score)
		}
	}
}

// TestEntityBoost_InjectedResultsRespectMetaFilters pins issue #654: engrams
// injected by the entity-boost pass have never been through phase 6, so the
// request's meta filters must be applied to them here. Before the fix, a recall
// scoped by tags_all or created_after returned entity-linked engrams matching
// neither constraint.
//
// The two halves are asserted separately on purpose. Filtering the injection
// path is the fix; filtering engrams already in results would be a regression,
// since those passed phase 6 on the way in and dropping them would silently
// shrink legitimate result sets.
func TestEntityBoost_InjectedResultsRespectMetaFilters(t *testing.T) {
	t.Parallel()
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	const vault = "boost-filter-test"
	ws := eng.store.ResolveVaultPrefix(vault)

	require.NoError(t, eng.store.UpsertEntityRecord(ctx, storage.EntityRecord{
		Name: "Shared", Type: "concept", Source: "inline",
	}, "inline"))

	// Seed: in the result set already, carries the tag.
	seed := &storage.Engram{Concept: "seed", Content: "seed content", Confidence: 0.9, Tags: []string{"keep"}}
	idSeed, err := eng.store.WriteEngram(ctx, ws, seed)
	require.NoError(t, err)
	require.NoError(t, eng.store.WriteEntityEngramLink(ctx, ws, idSeed, "Shared"))

	// Matching: entity-linked and carries the tag. Must still be injected —
	// the fix must not suppress legitimate injections.
	match := &storage.Engram{Concept: "match", Content: "tagged neighbour", Confidence: 0.8, Tags: []string{"keep"}}
	idMatch, err := eng.store.WriteEngram(ctx, ws, match)
	require.NoError(t, err)
	require.NoError(t, eng.store.WriteEntityEngramLink(ctx, ws, idMatch, "Shared"))

	// Violating: entity-linked but does NOT carry the tag. Must not appear.
	violating := &storage.Engram{Concept: "violating", Content: "untagged neighbour", Confidence: 0.8, Tags: []string{"other"}}
	idViolating, err := eng.store.WriteEngram(ctx, ws, violating)
	require.NoError(t, err)
	require.NoError(t, eng.store.WriteEntityEngramLink(ctx, ws, idViolating, "Shared"))

	fullSeed, err := eng.store.GetEngram(ctx, ws, idSeed)
	require.NoError(t, err)
	require.NotNil(t, fullSeed)

	filters := []activation.Filter{{Field: "tags_all", Op: "eq", Value: []string{"keep"}}}
	boosted := eng.applyEntityBoost(ctx, ws,
		[]activation.ScoredEngram{{Engram: fullSeed, Score: 0.8}}, filters, false)

	got := make(map[storage.ULID]float64, len(boosted))
	for _, r := range boosted {
		got[r.Engram.ID] = r.Score
	}

	_, violatingFound := got[idViolating]
	assert.False(t, violatingFound,
		"engram carrying none of the requested tags must not be injected by entity boost (#654)")

	_, matchFound := got[idMatch]
	assert.True(t, matchFound,
		"entity-linked engram that satisfies the filter must still be injected")

	_, seedFound := got[idSeed]
	assert.True(t, seedFound,
		"engrams already in the result set passed phase 6 and must not be re-filtered out")
}

// TestEntityBoost_InjectedResultsRespectTimeFilters covers the same bypass on
// created_after, the form in which it was originally observed on a live daemon:
// a recall bounded to the current day returned entity-linked engrams from two
// weeks earlier, each scored purely by entity boost with no content score.
func TestEntityBoost_InjectedResultsRespectTimeFilters(t *testing.T) {
	t.Parallel()
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	const vault = "boost-time-filter-test"
	ws := eng.store.ResolveVaultPrefix(vault)

	require.NoError(t, eng.store.UpsertEntityRecord(ctx, storage.EntityRecord{
		Name: "Timed", Type: "concept", Source: "inline",
	}, "inline"))

	now := time.Now()
	cutoff := now.Add(-24 * time.Hour)

	seed := &storage.Engram{Concept: "seed", Content: "recent seed", Confidence: 0.9, CreatedAt: now}
	idSeed, err := eng.store.WriteEngram(ctx, ws, seed)
	require.NoError(t, err)
	require.NoError(t, eng.store.WriteEntityEngramLink(ctx, ws, idSeed, "Timed"))

	old := &storage.Engram{
		Concept: "old", Content: "two weeks old", Confidence: 0.8,
		CreatedAt: now.Add(-14 * 24 * time.Hour),
	}
	idOld, err := eng.store.WriteEngram(ctx, ws, old)
	require.NoError(t, err)
	require.NoError(t, eng.store.WriteEntityEngramLink(ctx, ws, idOld, "Timed"))

	fullSeed, err := eng.store.GetEngram(ctx, ws, idSeed)
	require.NoError(t, err)
	require.NotNil(t, fullSeed)

	filters := []activation.Filter{{Field: "created_after", Op: "gt", Value: cutoff}}
	boosted := eng.applyEntityBoost(ctx, ws,
		[]activation.ScoredEngram{{Engram: fullSeed, Score: 0.8}}, filters, false)

	for _, r := range boosted {
		if r.Engram.ID == idOld {
			t.Fatalf("engram created %s violates the created_after bound %s but was injected by entity boost (#654)",
				r.Engram.CreatedAt, cutoff)
		}
	}
}

// TestEntityBoost_InjectedResultsRespectExcludeUntrusted pins the trust half of
// the same bypass. ExcludeUntrusted is a vault operator's standing posture, not
// a per-call convenience — the plasticity config documents untrusted engrams as
// filtered from ACTIVATE results, and the MCP tool surface advertises that to
// end users. Phase 6 honors it; the injection path did not.
//
// The check mirrors phase 6 exactly: only TrustUntrusted is excluded.
// TrustUnset is the zero-value backward-compat alias for TrustInferred and must
// still be injected, which the third assertion pins.
func TestEntityBoost_InjectedResultsRespectExcludeUntrusted(t *testing.T) {
	t.Parallel()
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	const vault = "boost-trust-test"
	ws := eng.store.ResolveVaultPrefix(vault)

	require.NoError(t, eng.store.UpsertEntityRecord(ctx, storage.EntityRecord{
		Name: "Trusted", Type: "concept", Source: "inline",
	}, "inline"))

	seed := &storage.Engram{Concept: "seed", Content: "seed", Confidence: 0.9, Trust: storage.TrustInferred}
	idSeed, err := eng.store.WriteEngram(ctx, ws, seed)
	require.NoError(t, err)
	require.NoError(t, eng.store.WriteEntityEngramLink(ctx, ws, idSeed, "Trusted"))

	untrusted := &storage.Engram{Concept: "untrusted", Content: "untrusted neighbour", Confidence: 0.8, Trust: storage.TrustUntrusted}
	idUntrusted, err := eng.store.WriteEngram(ctx, ws, untrusted)
	require.NoError(t, err)
	require.NoError(t, eng.store.WriteEntityEngramLink(ctx, ws, idUntrusted, "Trusted"))

	unset := &storage.Engram{Concept: "unset", Content: "unset-trust neighbour", Confidence: 0.8, Trust: storage.TrustUnset}
	idUnset, err := eng.store.WriteEngram(ctx, ws, unset)
	require.NoError(t, err)
	require.NoError(t, eng.store.WriteEntityEngramLink(ctx, ws, idUnset, "Trusted"))

	fullSeed, err := eng.store.GetEngram(ctx, ws, idSeed)
	require.NoError(t, err)
	require.NotNil(t, fullSeed)

	// Guard: the fixture really is untrusted, so a failure indicts the boost
	// pass rather than the write path.
	predUntrusted, err := eng.store.GetEngram(ctx, ws, idUntrusted)
	require.NoError(t, err)
	require.Equal(t, storage.TrustUntrusted, predUntrusted.Trust, "precondition: fixture is untrusted")

	boosted := eng.applyEntityBoost(ctx, ws,
		[]activation.ScoredEngram{{Engram: fullSeed, Score: 0.8}}, nil, true)

	got := make(map[storage.ULID]struct{}, len(boosted))
	for _, r := range boosted {
		got[r.Engram.ID] = struct{}{}
	}

	_, untrustedFound := got[idUntrusted]
	assert.False(t, untrustedFound,
		"TrustUntrusted engram must not be injected when ExcludeUntrusted is set (#654)")

	_, unsetFound := got[idUnset]
	assert.True(t, unsetFound,
		"TrustUnset is the backward-compat alias for TrustInferred and must still be injected, as in phase 6")

	// With the posture off, the untrusted engram is injected normally.
	boostedOff := eng.applyEntityBoost(ctx, ws,
		[]activation.ScoredEngram{{Engram: fullSeed, Score: 0.8}}, nil, false)
	offFound := false
	for _, r := range boostedOff {
		if r.Engram.ID == idUntrusted {
			offFound = true
		}
	}
	assert.True(t, offFound,
		"with ExcludeUntrusted unset the untrusted engram must still be injected")
}

// TestEntityBoost_ActivateWiresRequestFiltersIntoBoost drives the real Activate
// entry point rather than applyEntityBoost directly. The direct-call tests
// above prove the function filters correctly; this one proves activateCore
// actually passes the request's filters into it — un-wiring the call site to
// (nil, false) leaves every direct-call test green and turns only this red, so
// a later refactor of activateCore cannot silently reopen #654.
//
// Filter values are typed the way activateCore's verbatim conversion delivers
// them to passesMetaFilter: created_after must be a time.Time — with a string
// the type assertion fails open and the negative assertion would pass for the
// wrong reason. The positive assertions guard the same trap from the other
// side: a filter that is silently ignored (or an injection pass that never
// ran) cannot produce a false green, because the satisfying neighbour must
// still arrive via the boost path.
func TestEntityBoost_ActivateWiresRequestFiltersIntoBoost(t *testing.T) {
	t.Parallel()
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	const vault = "boost-wiring-test"
	ws := eng.store.ResolveVaultPrefix(vault)

	now := time.Now()
	cutoff := now.Add(-24 * time.Hour)

	// Seed: strong FTS match for the query, entity-linked, inside the window.
	respSeed, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:   vault,
		Concept: "primary database choice",
		Content: "We use PostgreSQL as the primary relational database for all transactional workloads",
		Entities: []mbp.InlineEntity{
			{Name: "PostgreSQL", Type: "database"},
		},
	})
	require.NoError(t, err)

	// Recent neighbour: entity-linked, content deliberately unrelated to the
	// query so it can only arrive via the boost path. Inside the window.
	recent := &storage.Engram{
		Concept: "recent neighbour", Content: "streaming replication failover runbook",
		Confidence: 0.8, CreatedAt: now.Add(-time.Hour),
	}
	idRecent, err := eng.store.WriteEngram(ctx, ws, recent)
	require.NoError(t, err)
	require.NoError(t, eng.store.WriteEntityEngramLink(ctx, ws, idRecent, "PostgreSQL"))

	// Old neighbour: entity-linked, outside the window. Before the wiring fix
	// this came back on a created_after-bounded recall, scored purely by boost.
	old := &storage.Engram{
		Concept: "old neighbour", Content: "legacy connection pool sizing notes",
		Confidence: 0.8, CreatedAt: now.Add(-14 * 24 * time.Hour),
	}
	idOld, err := eng.store.WriteEngram(ctx, ws, old)
	require.NoError(t, err)
	require.NoError(t, eng.store.WriteEntityEngramLink(ctx, ws, idOld, "PostgreSQL"))

	awaitFTS(t, eng)

	resp, err := eng.Activate(ctx, &mbp.ActivateRequest{
		Vault:      vault,
		Context:    []string{"primary relational database"},
		MaxResults: 20,
		Threshold:  0.01,
		Filters: []mbp.Filter{
			{Field: "created_after", Op: "gt", Value: cutoff},
		},
	})
	require.NoError(t, err)

	got := make(map[string]struct{}, len(resp.Activations))
	for _, item := range resp.Activations {
		got[item.ID] = struct{}{}
	}

	_, seedFound := got[respSeed.ID]
	require.True(t, seedFound, "seed satisfies the filter and matches the query; its absence means the request itself failed, not the boost pass")

	_, recentFound := got[idRecent.String()]
	assert.True(t, recentFound,
		"filter-satisfying entity neighbour must be injected — proves the boost pass ran and the filter was actually evaluated, not ignored")

	_, oldFound := got[idOld.String()]
	assert.False(t, oldFound,
		"engram outside the created_after bound must not be injected by entity boost on a real Activate request (#654)")
}
