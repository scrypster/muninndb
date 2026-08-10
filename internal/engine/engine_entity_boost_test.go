package engine

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/auth"
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

	err = eng.store.UpsertEntityRecord(ctx, ws, storage.EntityRecord{
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

	// Apply entity boost with this test's true corpus size (3 engrams) as
	// the vault-local n. "PostgreSQL" was upserted once (df=1, n=3), so
	// idf = ln(3/1)/ln(3) = 1.0 and B's boost is the full entityBoostFactor.
	boosted := eng.applyEntityBoost(ctx, ws, 3, initialResults, &activation.ActivateRequest{Threshold: 0.05})

	// Build ID set from boosted results.
	idSet := make(map[storage.ULID]float64, len(boosted))
	for _, r := range boosted {
		idSet[r.Engram.ID] = r.Score
	}

	// Engram A must remain in results with its original score (or higher if also entity-linked to itself).
	aScore, aFound := idSet[idA]
	require.True(t, aFound, "engram A should remain in boosted results")
	require.GreaterOrEqual(t, aScore, 0.8, "engram A score should not decrease")

	// Engram B must be added with entityBoostFactor × idf (idf = 1.0 here).
	bScore, bFound := idSet[idB]
	require.True(t, bFound, "engram B should be added by entity boost")
	require.InDelta(t, entityBoostFactor*entityIDF(1, 3), bScore, 0.001, "engram B score should equal entityBoostFactor × idf")

	// The injected result must carry a full component trace (issue #569).
	for _, r := range boosted {
		if r.Engram.ID == idB {
			require.InDelta(t, bScore, r.Components.EntityBoost, 1e-9, "injected result must expose its boost in Components.EntityBoost")
			require.InDelta(t, bScore, r.Components.Final, 1e-9, "injected result Final must equal its score")
			require.Greater(t, r.Components.Confidence, 0.0, "injected result must carry engram confidence")
		}
	}

	// Engram C must NOT be in results (different entity, no entity link written).
	_, cFound := idSet[idC]
	require.False(t, cFound, "engram C (different entity) should not be in boosted results")
}

// TestEntityBoost_BelowThresholdSeedDoesNotSpread guards the S1 interaction:
// the tag-filter threshold bypass admits content-unrelated due-reminders into
// the result set with a Score below the relevance threshold. Such a result must
// NOT act as a spread-activation seed, or its entities would drag arbitrary
// entity-linked engrams into recall. Same A→B entity-link setup as the direct
// test above, but A is a below-threshold admission (Score 0.02 < threshold 0.05),
// so B must not surface.
func TestEntityBoost_BelowThresholdSeedDoesNotSpread(t *testing.T) {
	t.Parallel()
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	const vault = "boost-below-threshold-test"
	ws := eng.store.ResolveVaultPrefix(vault)

	engramA := &storage.Engram{Concept: "due-a", Content: "Send the founder agreement to the lawyer", Confidence: 0.9}
	idA, err := eng.store.WriteEngram(ctx, ws, engramA)
	require.NoError(t, err)
	err = eng.store.UpsertEntityRecord(ctx, ws, storage.EntityRecord{Name: "Lawyer", Type: "person", Source: "inline"}, "inline")
	require.NoError(t, err)
	require.NoError(t, eng.store.WriteEntityEngramLink(ctx, ws, idA, "Lawyer"))

	engramB := &storage.Engram{Concept: "unrelated-b", Content: "Lawyer notes from an unrelated matter", Confidence: 0.8}
	idB, err := eng.store.WriteEngram(ctx, ws, engramB)
	require.NoError(t, err)
	require.NoError(t, eng.store.WriteEntityEngramLink(ctx, ws, idB, "Lawyer"))

	fullA, err := eng.store.GetEngram(ctx, ws, idA)
	require.NoError(t, err)

	// A is admitted below the threshold (as the S1 tag bypass would do).
	initialResults := []activation.ScoredEngram{{Engram: fullA, Score: 0.02}}

	const threshold = 0.05
	boosted := eng.applyEntityBoost(ctx, ws, 2, initialResults, &activation.ActivateRequest{Threshold: threshold})

	idSet := make(map[storage.ULID]struct{}, len(boosted))
	for _, r := range boosted {
		idSet[r.Engram.ID] = struct{}{}
	}
	// A stays in the result (the bypass admitted it), but it must not have seeded.
	require.Contains(t, idSet, idA, "A (the tag-bypassed admission) stays in results")
	_, bFound := idSet[idB]
	require.False(t, bFound, "B must NOT be pulled in: a below-threshold admission must not seed entity boost")

	// Control: with threshold 0, A is a legitimate seed and B does surface — proves
	// the exclusion is what suppresses B, not a broken setup.
	ctrl := eng.applyEntityBoost(ctx, ws, 2, []activation.ScoredEngram{{Engram: fullA, Score: 0.02}}, &activation.ActivateRequest{Threshold: 0.0})
	ctrlSet := make(map[storage.ULID]struct{}, len(ctrl))
	for _, r := range ctrl {
		ctrlSet[r.Engram.ID] = struct{}{}
	}
	require.Contains(t, ctrlSet, idB, "control: with threshold 0, A seeds and B surfaces")
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

// writeLinkedEngram writes an engram at the store level and links it to the
// given entities. Store-level writes bypass engine counters — tests using
// this helper pass the intended vault-local n to applyEntityBoost directly.
func writeLinkedEngram(t *testing.T, eng *Engine, ws [8]byte, concept string, entities ...string) storage.ULID {
	t.Helper()
	ctx := context.Background()
	id, err := eng.store.WriteEngram(ctx, ws, &storage.Engram{
		Concept:    concept,
		Content:    concept + " content",
		Confidence: 0.9,
	})
	require.NoError(t, err)
	for _, name := range entities {
		require.NoError(t, eng.store.WriteEntityEngramLink(ctx, ws, id, name))
	}
	return id
}

// upsertEntityTimes upserts an entity record in vault ws `times` times, setting
// its vault-local MentionCount (df) to that value.
func upsertEntityTimes(t *testing.T, eng *Engine, ws [8]byte, name string, times int) {
	t.Helper()
	ctx := context.Background()
	for range times {
		require.NoError(t, eng.store.UpsertEntityRecord(ctx, ws, storage.EntityRecord{
			Name:   name,
			Type:   "concept",
			Source: "inline",
		}, "inline"))
	}
}

// TestEntityBoost_UbiquitousEntityDoesNotFlood is the core regression for
// issue #569: engrams sharing only a ubiquitous entity with the seeds must
// not be injected into results, no matter how many seeds carry that entity.
// Before the fix, each of the 5 seeds contributed a flat 0.15 per shared
// entity, so hub engrams flooded recall at 5 × 0.15 = 0.75 with empty
// score components, evicting genuine matches at truncation.
func TestEntityBoost_UbiquitousEntityDoesNotFlood(t *testing.T) {
	t.Parallel()
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	const vault = "flood-test"
	ws := eng.store.ResolveVaultPrefix(vault)

	// Entity "Everyone" is mentioned by all 12 engrams: df = n → idf = 0.
	upsertEntityTimes(t, eng, ws, "Everyone", 12)
	ids := make([]storage.ULID, 12)
	for i := range ids {
		ids[i] = writeLinkedEngram(t, eng, ws, fmt.Sprintf("hub-engram-%d", i), "Everyone")
	}
	// Five seeds — the exact shape that produced 0.75 injections pre-fix.
	results := make([]activation.ScoredEngram, 0, 5)
	for i := 0; i < 5; i++ {
		full, err := eng.store.GetEngram(ctx, ws, ids[i])
		require.NoError(t, err)
		results = append(results, activation.ScoredEngram{Engram: full, Score: 0.8 - float64(i)*0.05})
	}

	boosted := eng.applyEntityBoost(ctx, ws, 12, results, &activation.ActivateRequest{Threshold: 0.1})

	require.Len(t, boosted, 5, "no engram may be injected on ubiquitous-entity evidence alone")
	for _, r := range boosted {
		require.LessOrEqual(t, r.Components.EntityBoost, entityBoostNoiseFloor,
			"a ubiquitous entity must contribute ~0 boost")
	}
}

// TestEntityBoost_SameEntityViaMultipleSeedsCountsOnce verifies that a
// shared entity is credited to a target once, regardless of how many seeds
// carry it — the evidence is the shared entity, not the seed count.
func TestEntityBoost_SameEntityViaMultipleSeedsCountsOnce(t *testing.T) {
	t.Parallel()
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	const vault = "dedup-test"
	ws := eng.store.ResolveVaultPrefix(vault)

	// "DupEnt" is mentioned by 3 engrams (2 seeds + 1 target) in a corpus of 100.
	upsertEntityTimes(t, eng, ws, "DupEnt", 3)
	seed1 := writeLinkedEngram(t, eng, ws, "dedup-seed-1", "DupEnt")
	seed2 := writeLinkedEngram(t, eng, ws, "dedup-seed-2", "DupEnt")
	target := writeLinkedEngram(t, eng, ws, "dedup-target", "DupEnt")

	fullSeed1, err := eng.store.GetEngram(ctx, ws, seed1)
	require.NoError(t, err)
	fullSeed2, err := eng.store.GetEngram(ctx, ws, seed2)
	require.NoError(t, err)

	boosted := eng.applyEntityBoost(ctx, ws, 100, []activation.ScoredEngram{
		{Engram: fullSeed1, Score: 0.8},
		{Engram: fullSeed2, Score: 0.7},
	}, &activation.ActivateRequest{Threshold: 0.05})

	expected := entityBoostFactor * entityIDF(3, 100)
	var targetScore float64
	found := false
	for _, r := range boosted {
		if r.Engram.ID == target {
			targetScore = r.Score
			found = true
		}
	}
	require.True(t, found, "target should be injected (rare shared entity)")
	require.InDelta(t, expected, targetScore, 1e-6,
		"entity shared with two seeds must be credited once, not twice")
}

// TestEntityBoost_CapBoundsAccumulation verifies that the total boost a
// single engram accumulates across several rare shared entities is clamped
// at entityBoostCap.
func TestEntityBoost_CapBoundsAccumulation(t *testing.T) {
	t.Parallel()
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	const vault = "cap-test"
	ws := eng.store.ResolveVaultPrefix(vault)

	entities := []string{"CapE1", "CapE2", "CapE3", "CapE4"}
	for _, name := range entities {
		upsertEntityTimes(t, eng, ws, name, 2) // df=2 each — rare
	}
	seed := writeLinkedEngram(t, eng, ws, "cap-seed", entities...)
	target := writeLinkedEngram(t, eng, ws, "cap-target", entities...)

	// Sanity: uncapped accumulation would exceed the cap.
	perEntity := entityBoostFactor * entityIDF(2, 1000)
	require.Greater(t, 4*perEntity, entityBoostCap, "test setup must exceed the cap uncapped")

	fullSeed, err := eng.store.GetEngram(ctx, ws, seed)
	require.NoError(t, err)

	boosted := eng.applyEntityBoost(ctx, ws, 1000, []activation.ScoredEngram{
		{Engram: fullSeed, Score: 0.9},
	}, &activation.ActivateRequest{Threshold: 0.05})

	found := false
	for _, r := range boosted {
		if r.Engram.ID == target {
			found = true
			require.InDelta(t, entityBoostCap, r.Score, 1e-9,
				"total boost must clamp at entityBoostCap")
		}
	}
	require.True(t, found, "target should be injected")
}

// TestEntityBoost_InjectionRespectsThreshold verifies that engrams whose
// entity evidence does not clear the caller's threshold are not injected —
// a result below threshold stays below threshold regardless of how it was
// found (issue #569: the old path injected sub-threshold results).
func TestEntityBoost_InjectionRespectsThreshold(t *testing.T) {
	t.Parallel()
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	const vault = "threshold-test"
	ws := eng.store.ResolveVaultPrefix(vault)

	upsertEntityTimes(t, eng, ws, "RareThing", 2)
	seed := writeLinkedEngram(t, eng, ws, "threshold-seed", "RareThing")
	writeLinkedEngram(t, eng, ws, "threshold-target", "RareThing")

	boost := entityBoostFactor * entityIDF(2, 100)
	fullSeed, err := eng.store.GetEngram(ctx, ws, seed)
	require.NoError(t, err)

	// Threshold just above the evidence: no injection.
	high := eng.applyEntityBoost(ctx, ws, 100, []activation.ScoredEngram{
		{Engram: fullSeed, Score: 0.8},
	}, &activation.ActivateRequest{Threshold: boost + 0.01})
	require.Len(t, high, 1, "sub-threshold entity evidence must not be injected")

	// Threshold just below the evidence: injected.
	low := eng.applyEntityBoost(ctx, ws, 100, []activation.ScoredEngram{
		{Engram: fullSeed, Score: 0.8},
	}, &activation.ActivateRequest{Threshold: boost - 0.01})
	require.Len(t, low, 2, "entity evidence clearing the threshold is injected")
}

// TestEntityBoost_InjectionRespectsMetaFilters verifies that engrams injected
// on entity evidence are gated on the caller's meta filters. The pipeline
// applies filters in phase 6; the boost pass appends afterwards, so without
// this gate a tags_all query returns records that do not carry the required
// tags (reported in production on #569: 44 records returned, 4 matching).
func TestEntityBoost_InjectionRespectsMetaFilters(t *testing.T) {
	t.Parallel()
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	const vault = "meta-filter-test"
	ws := eng.store.ResolveVaultPrefix(vault)

	upsertEntityTimes(t, eng, ws, "FilterEnt", 3)

	seedID, err := eng.store.WriteEngram(ctx, ws, &storage.Engram{
		Concept:    "filter-seed",
		Content:    "filter-seed content",
		Confidence: 0.9,
		Tags:       []string{"wanted"},
	})
	require.NoError(t, err)
	require.NoError(t, eng.store.WriteEntityEngramLink(ctx, ws, seedID, "FilterEnt"))

	tagged, err := eng.store.WriteEngram(ctx, ws, &storage.Engram{
		Concept:    "filter-tagged-target",
		Content:    "tagged target content",
		Confidence: 0.9,
		Tags:       []string{"wanted"},
	})
	require.NoError(t, err)
	require.NoError(t, eng.store.WriteEntityEngramLink(ctx, ws, tagged, "FilterEnt"))

	untagged, err := eng.store.WriteEngram(ctx, ws, &storage.Engram{
		Concept:    "filter-untagged-target",
		Content:    "untagged target content",
		Confidence: 0.9,
	})
	require.NoError(t, err)
	require.NoError(t, eng.store.WriteEntityEngramLink(ctx, ws, untagged, "FilterEnt"))

	fullSeed, err := eng.store.GetEngram(ctx, ws, seedID)
	require.NoError(t, err)

	filters := []activation.Filter{{Field: "tags_all", Value: []string{"wanted"}}}
	boosted := eng.applyEntityBoost(ctx, ws, 100, []activation.ScoredEngram{
		{Engram: fullSeed, Score: 0.8},
	}, &activation.ActivateRequest{Threshold: 0.05, Filters: filters})

	ids := make(map[storage.ULID]bool, len(boosted))
	for _, r := range boosted {
		ids[r.Engram.ID] = true
	}
	require.True(t, ids[tagged], "entity-linked engram carrying the required tag must be injected")
	require.False(t, ids[untagged], "entity-linked engram without the required tag must not be injected")
}

// TestEntityBoost_TotalFoundCoversFilteredResults is the fingerprint
// regression from the production report on #569: with meta filters set, the
// boost pass appended unfiltered records after TotalFound was computed, so
// any response with total < len(activations) is the bug firing (observed:
// total=4 with 44 records returned). After the fix, every returned record
// passes the filter and TotalFound covers the returned set.
func TestEntityBoost_TotalFoundCoversFilteredResults(t *testing.T) {
	t.Parallel()
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	const vault = "fingerprint-test"

	// One engram matches both the query and the tag filter.
	respWanted, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:    vault,
		Concept:  "wanted flow record",
		Content:  "PostgreSQL flow record with the wanted tag",
		Tags:     []string{"record_kind=flow"},
		Entities: []mbp.InlineEntity{{Name: "PostgreSQL", Type: "database"}},
	})
	require.NoError(t, err)

	// Entity-linked engrams WITHOUT the tag: phase 6 filters them out, so
	// pre-fix they re-entered via injection, past the filter and past
	// TotalFound.
	untaggedIDs := make(map[string]bool, 6)
	for i := range 6 {
		resp, err := eng.Write(ctx, &mbp.WriteRequest{
			Vault:    vault,
			Concept:  fmt.Sprintf("untagged sibling %d", i),
			Content:  fmt.Sprintf("PostgreSQL sibling record %d without the tag", i),
			Entities: []mbp.InlineEntity{{Name: "PostgreSQL", Type: "database"}},
		})
		require.NoError(t, err)
		untaggedIDs[resp.ID] = true
	}

	// Filler engrams without the entity keep "PostgreSQL" rare enough that
	// its idf — and thus the injection evidence — stays above threshold
	// (df == n would zero the boost and the test would pass vacuously).
	for i := range 10 {
		_, err := eng.Write(ctx, &mbp.WriteRequest{
			Vault:   vault,
			Concept: fmt.Sprintf("filler %d", i),
			Content: fmt.Sprintf("Redis cache entry %d for session data", i),
		})
		require.NoError(t, err)
	}

	awaitFTS(t, eng)

	resp, err := eng.Activate(ctx, &mbp.ActivateRequest{
		Vault:      vault,
		Context:    []string{"PostgreSQL flow record"},
		MaxResults: 50,
		Threshold:  0.01,
		Filters:    []mbp.Filter{{Field: "tags_all", Value: []string{"record_kind=flow"}}},
	})
	require.NoError(t, err)

	// No unfiltered record may be returned.
	foundWanted := false
	for _, item := range resp.Activations {
		require.False(t, untaggedIDs[item.ID],
			"engram %s (no required tag) leaked into tag-filtered results via entity boost", item.ID)
		if item.ID == respWanted.ID {
			foundWanted = true
		}
	}
	require.True(t, foundWanted, "the tagged engram must be returned")

	// The fingerprint: any response with total < len(activations) is the bug.
	require.GreaterOrEqual(t, resp.TotalFound, len(resp.Activations),
		"TotalFound must cover the returned set (fingerprint: total < len(activations))")

	// Control from the production report: a filter matching nothing returns
	// nothing. Note this passes even pre-fix (no survivors → no seeds → the
	// boost early-returns), which is why it cannot serve as the regression
	// test on its own.
	empty, err := eng.Activate(ctx, &mbp.ActivateRequest{
		Vault:      vault,
		Context:    []string{"PostgreSQL flow record"},
		MaxResults: 50,
		Threshold:  0.01,
		Filters:    []mbp.Filter{{Field: "tags_all", Value: []string{"no-such-tag"}}},
	})
	require.NoError(t, err)
	require.Empty(t, empty.Activations, "filter matching nothing must return nothing")
}

// TestEntityBoost_RarityIsVaultLocal is the two-vaults-one-engine regression
// for the review on #570: entityIDF's n must be the recalled vault's engram
// count, not the deployment-global counter. With a global n, an entity that
// is locally ubiquitous (mentioned by every engram in its vault — exactly
// what should score idf = 0) regains a positive idf as unrelated vaults are
// added to the same server, re-opening the #569 flood gated by deployment
// size: idf(6, 6) = 0 but idf(6, 30) ≈ 0.47, which boosts hub siblings past
// a 0.05 threshold. The suite missed this because every prior test pinned n
// to a single test vault's own size.
func TestEntityBoost_RarityIsVaultLocal(t *testing.T) {
	t.Parallel()
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	const vaultA = "vault-local-a"
	const vaultB = "vault-local-b"

	// Vault A holds 6 engrams. "AlphaHub" is on ALL of them — locally
	// ubiquitous, unique to this vault (df = 6 = vault size → idf must be 0).
	// "AlphaRare" is on 2 — locally rare (idf(2,6) ≈ 0.61 → boost ≈ 0.09).
	seed, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:   vaultA,
		Concept: "primary database choice",
		Content: "We use PostgreSQL as the primary relational database for transactional workloads",
		Entities: []mbp.InlineEntity{
			{Name: "AlphaHub", Type: "concept"},
			{Name: "AlphaRare", Type: "concept"},
		},
	})
	require.NoError(t, err)

	hubOnly := make(map[string]bool, 4)
	for i := range 4 {
		resp, err := eng.Write(ctx, &mbp.WriteRequest{
			Vault:    vaultA,
			Concept:  fmt.Sprintf("hub sibling %d", i),
			Content:  fmt.Sprintf("moss inventory shelf audit note %d", i),
			Entities: []mbp.InlineEntity{{Name: "AlphaHub", Type: "concept"}},
		})
		require.NoError(t, err)
		hubOnly[resp.ID] = true
	}

	rare, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:   vaultA,
		Concept: "rare sibling",
		Content: "quiet bench ledger of appeals heard in whispers",
		Entities: []mbp.InlineEntity{
			{Name: "AlphaHub", Type: "concept"},
			{Name: "AlphaRare", Type: "concept"},
		},
	})
	require.NoError(t, err)

	// Vault B: unrelated engrams sharing the engine. Under a global n these
	// inflate every idf computed for vault A recalls; under a vault-local n
	// they must change nothing about vault A's scoring.
	for i := range 24 {
		_, err := eng.Write(ctx, &mbp.WriteRequest{
			Vault:   vaultB,
			Concept: fmt.Sprintf("unrelated %d", i),
			Content: fmt.Sprintf("orchard weather almanac entry %d", i),
		})
		require.NoError(t, err)
	}

	awaitFTS(t, eng)

	resp, err := eng.Activate(ctx, &mbp.ActivateRequest{
		Vault:      vaultA,
		Context:    []string{"primary relational database workloads"},
		MaxResults: 20,
		Threshold:  0.05,
	})
	require.NoError(t, err)

	ids := make(map[string]bool, len(resp.Activations))
	for _, item := range resp.Activations {
		ids[item.ID] = true
	}
	require.True(t, ids[seed.ID], "the FTS-matching seed must be returned")
	require.True(t, ids[rare.ID],
		"a LOCALLY rare shared entity must still inject its sibling (idf(2,6) ≈ 0.61)")
	for id := range hubOnly {
		require.False(t, ids[id],
			"a LOCALLY ubiquitous entity (df = vault size) must inject nothing, "+
				"no matter how many engrams other vaults hold")
	}
}

// TestEntityBoost_InjectionRespectsExcludeUntrusted verifies that engrams
// injected on entity evidence honor the caller's ExcludeUntrusted, mirroring
// phase 6's hard trust filter. ExcludeUntrusted rides the request bool rather
// than Filters, so the meta-filter gate cannot cover it: pre-fix, a vault
// excluding flagged-unreliable memory got it re-injected through any shared
// rare entity. Injection-side only — engrams already in the result set
// cleared phase 6's own trust filter.
func TestEntityBoost_InjectionRespectsExcludeUntrusted(t *testing.T) {
	t.Parallel()
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	const vault = "trust-gate-test"
	ws := eng.store.ResolveVaultPrefix(vault)

	upsertEntityTimes(t, eng, ws, "TrustEnt", 2)

	seedID, err := eng.store.WriteEngram(ctx, ws, &storage.Engram{
		Concept:    "trust-seed",
		Content:    "trusted seed content",
		Confidence: 0.9,
	})
	require.NoError(t, err)
	require.NoError(t, eng.store.WriteEntityEngramLink(ctx, ws, seedID, "TrustEnt"))

	untrusted, err := eng.store.WriteEngram(ctx, ws, &storage.Engram{
		Concept:    "trust-untrusted-target",
		Content:    "flagged unreliable content sharing the entity",
		Confidence: 0.9,
	})
	require.NoError(t, err)
	require.NoError(t, eng.store.WriteEntityEngramLink(ctx, ws, untrusted, "TrustEnt"))
	require.NoError(t, eng.store.UpdateTrust(ctx, ws, untrusted, storage.TrustUntrusted))

	fullSeed, err := eng.store.GetEngram(ctx, ws, seedID)
	require.NoError(t, err)
	seedResults := []activation.ScoredEngram{{Engram: fullSeed, Score: 0.8}}

	// With ExcludeUntrusted set, the untrusted engram must not be injected.
	gated := eng.applyEntityBoost(ctx, ws, 100, seedResults, &activation.ActivateRequest{Threshold: 0.05, ExcludeUntrusted: true})
	for _, r := range gated {
		require.NotEqual(t, untrusted, r.Engram.ID,
			"TrustUntrusted engram must not be injected when ExcludeUntrusted is set")
	}

	// Control: without the flag the same evidence injects it — proves the
	// gate is what suppresses it, not a broken setup or insufficient idf.
	open := eng.applyEntityBoost(ctx, ws, 100, seedResults, &activation.ActivateRequest{Threshold: 0.05})
	found := false
	for _, r := range open {
		if r.Engram.ID == untrusted {
			found = true
		}
	}
	require.True(t, found, "control: without ExcludeUntrusted the entity evidence injects the engram")
}

// TestEntityBoost_ActivateWiresExcludeUntrustedIntoBoost drives the real
// Activate path: a vault whose PlasticityConfig sets ExcludeUntrusted must not
// see a TrustUntrusted engram re-enter through entity-boost injection. The
// direct-call test above proves applyEntityBoost gates correctly; this one
// proves activateCore actually hands the resolved flag to it, so a refactor of
// the call site cannot silently reopen the hole while CI stays green. An
// identical vault without the flag is the positive control: the same engram
// arrives there ONLY via injection (its content shares no stems with the
// query, so the FTS/vector pipeline cannot carry it — verified by disabling
// the boost pass, which fails exactly the control leg), so a boost pass that
// never ran cannot produce a false green.
func TestEntityBoost_ActivateWiresExcludeUntrustedIntoBoost(t *testing.T) {
	eng, as, _, cleanup := testEnvWithAuth(t)
	defer cleanup()
	ctx := context.Background()

	tr := true
	require.NoError(t, as.SetVaultConfig(auth.VaultConfig{
		Name:       "trust-wire-gated",
		Public:     true,
		Plasticity: &auth.PlasticityConfig{ExcludeUntrusted: &tr},
	}))

	for _, vault := range []string{"trust-wire-gated", "trust-wire-open"} {
		// Entity names are per-vault: MentionCount is store-global, so a name
		// shared across the two test vaults would double df and sink the boost
		// below threshold.
		entName := "WireTrustEnt-" + vault
		// Seed: matches the query, carries the shared entity.
		_, err := eng.Write(ctx, &mbp.WriteRequest{
			Vault:    vault,
			Concept:  "primary database choice",
			Content:  "We use PostgreSQL as the primary relational database for transactional workloads",
			Entities: []mbp.InlineEntity{{Name: entName, Type: "concept"}},
		})
		require.NoError(t, err)

		// Untrusted target: content-unrelated to the query, reachable only
		// via entity-boost injection.
		resp, err := eng.Write(ctx, &mbp.WriteRequest{
			Vault:    vault,
			Concept:  "flagged note",
			Content:  "moss shelf almanac entry, whisper ledger of appeals",
			Entities: []mbp.InlineEntity{{Name: entName, Type: "concept"}},
		})
		require.NoError(t, err)
		require.NoError(t, eng.SetTrust(ctx, vault, resp.ID, "untrusted"))

		// Filler keeps the entity rare (df=2 against n=7 per vault).
		for i := range 5 {
			_, err := eng.Write(ctx, &mbp.WriteRequest{
				Vault:   vault,
				Concept: fmt.Sprintf("filler %d", i),
				Content: fmt.Sprintf("Redis cache entry %d for session data", i),
			})
			require.NoError(t, err)
		}

		awaitFTS(t, eng)

		act, err := eng.Activate(ctx, &mbp.ActivateRequest{
			Vault:      vault,
			Context:    []string{"primary relational database workloads"},
			MaxResults: 20,
			Threshold:  0.05,
		})
		require.NoError(t, err)

		found := false
		for _, item := range act.Activations {
			if item.ID == resp.ID {
				found = true
			}
		}
		if vault == "trust-wire-gated" {
			require.False(t, found,
				"ExcludeUntrusted vault must not receive the untrusted engram via entity-boost injection")
		} else {
			require.True(t, found,
				"control vault without ExcludeUntrusted must receive the same engram by injection")
		}
	}
}

// TestEntityBoost_InjectionRespectsLeaseVisibility verifies that injections
// honor the work-queue lease contract (#548), mirroring phase 6: an engram
// checked out under a live foreign lease must not be re-injected through a
// shared rare entity; the caller's own lease and an expired foreign lease
// must not hide it.
func TestEntityBoost_InjectionRespectsLeaseVisibility(t *testing.T) {
	t.Parallel()
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	const vault = "lease-gate-test"
	ws := eng.store.ResolveVaultPrefix(vault)

	upsertEntityTimes(t, eng, ws, "LeaseEnt", 2)

	seedID, err := eng.store.WriteEngram(ctx, ws, &storage.Engram{
		Concept:    "lease-seed",
		Content:    "seed content",
		Confidence: 0.9,
	})
	require.NoError(t, err)
	require.NoError(t, eng.store.WriteEntityEngramLink(ctx, ws, seedID, "LeaseEnt"))

	target, err := eng.store.WriteEngram(ctx, ws, &storage.Engram{
		Concept:    "lease-target",
		Content:    "work item content sharing the entity",
		Confidence: 0.9,
	})
	require.NoError(t, err)
	require.NoError(t, eng.store.WriteEntityEngramLink(ctx, ws, target, "LeaseEnt"))

	fullSeed, err := eng.store.GetEngram(ctx, ws, seedID)
	require.NoError(t, err)
	seedResults := []activation.ScoredEngram{{Engram: fullSeed, Score: 0.8}}
	inSet := func(rs []activation.ScoredEngram) bool {
		for _, r := range rs {
			if r.Engram.ID == target {
				return true
			}
		}
		return false
	}

	// Live foreign lease: hidden.
	claimRes, err := eng.Claim(ctx, vault, target.String(), "other-agent", 600)
	require.NoError(t, err)
	require.Equal(t, LeaseAcquired, claimRes.Status)
	require.False(t, inSet(eng.applyEntityBoost(ctx, ws, 100, seedResults,
		&activation.ActivateRequest{Threshold: 0.05, CallerOwner: "me"})),
		"engram under a live foreign lease must not be injected")

	// IncludeLeased opt-out: visible even under the live foreign lease.
	require.True(t, inSet(eng.applyEntityBoost(ctx, ws, 100, seedResults,
		&activation.ActivateRequest{Threshold: 0.05, CallerOwner: "me", IncludeLeased: true})),
		"IncludeLeased must disable lease-based hiding")

	// The lease owner's own recall: visible.
	require.True(t, inSet(eng.applyEntityBoost(ctx, ws, 100, seedResults,
		&activation.ActivateRequest{Threshold: 0.05, CallerOwner: "other-agent"})),
		"the lease owner's own lease must not hide the injection")

	// Released: visible again (Live() staleness itself is pinned by the lease tests).
	_, err = eng.Release(ctx, vault, target.String(), "other-agent")
	require.NoError(t, err)
	require.True(t, inSet(eng.applyEntityBoost(ctx, ws, 100, seedResults,
		&activation.ActivateRequest{Threshold: 0.05, CallerOwner: "me"})),
		"a released lease must not hide the injection")
}

// TestEntityBoost_InjectionFailsClosedOnLeaseReadError verifies the pass 2b
// fail-closed guard (#570 follow-up): when the lease read itself errors
// (distinct from a missing lease, which yields a zero Lease and no error),
// the candidate must NOT be injected. The real store's GetLease only errors
// on a genuine Pebble read/decode failure — never on an absent lease record —
// so this test injects the fault via getLeaseForInjection, the seam added
// solely to make this failure mode reachable in a test.
func TestEntityBoost_InjectionFailsClosedOnLeaseReadError(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	const vault = "lease-fault-test"
	ws := eng.store.ResolveVaultPrefix(vault)

	upsertEntityTimes(t, eng, ws, "LeaseFaultEnt", 2)

	seedID, err := eng.store.WriteEngram(ctx, ws, &storage.Engram{
		Concept:    "lease-fault-seed",
		Content:    "seed content",
		Confidence: 0.9,
	})
	require.NoError(t, err)
	require.NoError(t, eng.store.WriteEntityEngramLink(ctx, ws, seedID, "LeaseFaultEnt"))

	target, err := eng.store.WriteEngram(ctx, ws, &storage.Engram{
		Concept:    "lease-fault-target",
		Content:    "work item content sharing the entity",
		Confidence: 0.9,
	})
	require.NoError(t, err)
	require.NoError(t, eng.store.WriteEntityEngramLink(ctx, ws, target, "LeaseFaultEnt"))

	fullSeed, err := eng.store.GetEngram(ctx, ws, seedID)
	require.NoError(t, err)
	seedResults := []activation.ScoredEngram{{Engram: fullSeed, Score: 0.8}}
	inSet := func(rs []activation.ScoredEngram) bool {
		for _, r := range rs {
			if r.Engram.ID == target {
				return true
			}
		}
		return false
	}

	// No lease record exists yet: the real store returns a zero Lease and no
	// error, so the candidate is injected. This is the control proving the
	// fault below — not the guard itself — is what suppresses the injection.
	require.True(t, inSet(eng.applyEntityBoost(ctx, ws, 100, seedResults,
		&activation.ActivateRequest{Threshold: 0.05, CallerOwner: "me"})),
		"control: with no lease record, the candidate is injected")

	// Fault-inject a lease-read error for this target only; every other id
	// (including calls from other concurrently-running tests) is unaffected.
	faultyID := target
	orig := getLeaseForInjection
	getLeaseForInjection = func(ctx context.Context, s *storage.PebbleStore, wsPrefix [8]byte, id storage.ULID) (storage.Lease, error) {
		if id == faultyID {
			return storage.Lease{}, fmt.Errorf("simulated lease read failure")
		}
		return orig(ctx, s, wsPrefix, id)
	}
	defer func() { getLeaseForInjection = orig }()

	require.False(t, inSet(eng.applyEntityBoost(ctx, ws, 100, seedResults,
		&activation.ActivateRequest{Threshold: 0.05, CallerOwner: "me"})),
		"a lease-read error must fail closed: the candidate must not be injected")
}

// TestEntityBoost_InjectionRespectsValidity verifies that injections honor the
// COG-19 valid-time gate: an engram whose ValidUntil has passed must not be
// injected (the final gate in activateCore would drop it anyway, but gating at
// injection keeps TotalFound honest), while IncludeInvalid restores it.
func TestEntityBoost_InjectionRespectsValidity(t *testing.T) {
	t.Parallel()
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	const vault = "validity-gate-test"
	ws := eng.store.ResolveVaultPrefix(vault)

	upsertEntityTimes(t, eng, ws, "ValidEnt", 2)

	seedID, err := eng.store.WriteEngram(ctx, ws, &storage.Engram{
		Concept:    "validity-seed",
		Content:    "seed content",
		Confidence: 0.9,
	})
	require.NoError(t, err)
	require.NoError(t, eng.store.WriteEntityEngramLink(ctx, ws, seedID, "ValidEnt"))

	expired := time.Now().Add(-time.Hour)
	target, err := eng.store.WriteEngram(ctx, ws, &storage.Engram{
		Concept:    "validity-target",
		Content:    "stale fact sharing the entity",
		Confidence: 0.9,
		ValidUntil: expired,
	})
	require.NoError(t, err)
	require.NoError(t, eng.store.WriteEntityEngramLink(ctx, ws, target, "ValidEnt"))

	fullSeed, err := eng.store.GetEngram(ctx, ws, seedID)
	require.NoError(t, err)
	seedResults := []activation.ScoredEngram{{Engram: fullSeed, Score: 0.8}}
	inSet := func(rs []activation.ScoredEngram) bool {
		for _, r := range rs {
			if r.Engram.ID == target {
				return true
			}
		}
		return false
	}

	require.False(t, inSet(eng.applyEntityBoost(ctx, ws, 100, seedResults,
		&activation.ActivateRequest{Threshold: 0.05})),
		"an expired engram must not be injected on entity evidence")

	require.True(t, inSet(eng.applyEntityBoost(ctx, ws, 100, seedResults,
		&activation.ActivateRequest{Threshold: 0.05, IncludeInvalid: true})),
		"IncludeInvalid must restore the expired injection")
}

// TestEntityBoost_InjectionRespectsStructuredFilter: an engram the caller's
// structured (MQL WHERE) predicate rejects must not enter via boost injection.
// Phase 6 applies StructuredFilter inside Run(), BEFORE the boost pass — the
// injection side door skipped it entirely until the shared visibility gate
// picked it up (adversarial-review finding on the gate increment; behavior
// change for boost, called out in the commit).
func TestEntityBoost_InjectionRespectsStructuredFilter(t *testing.T) {
	t.Parallel()
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	const vault = "structured-filter-test"
	ws := eng.store.ResolveVaultPrefix(vault)

	upsertEntityTimes(t, eng, ws, "SFEnt", 2)

	seedID, err := eng.store.WriteEngram(ctx, ws, &storage.Engram{
		Concept:    "sf-seed",
		Content:    "seed content",
		Confidence: 0.9,
	})
	require.NoError(t, err)
	require.NoError(t, eng.store.WriteEntityEngramLink(ctx, ws, seedID, "SFEnt"))

	target, err := eng.store.WriteEngram(ctx, ws, &storage.Engram{
		Concept:    "sf-target",
		Content:    "target content sharing the entity",
		Confidence: 0.9,
	})
	require.NoError(t, err)
	require.NoError(t, eng.store.WriteEntityEngramLink(ctx, ws, target, "SFEnt"))

	fullSeed, err := eng.store.GetEngram(ctx, ws, seedID)
	require.NoError(t, err)
	seedResults := []activation.ScoredEngram{{Engram: fullSeed, Score: 0.8}}
	inSet := func(rs []activation.ScoredEngram) bool {
		for _, r := range rs {
			if r.Engram.ID == target {
				return true
			}
		}
		return false
	}

	// Control: with no structured filter the target injects.
	require.True(t, inSet(eng.applyEntityBoost(ctx, ws, 100, seedResults,
		&activation.ActivateRequest{Threshold: 0.05})),
		"control: target must inject without a structured filter")

	// A predicate rejecting the target must keep it out.
	require.False(t, inSet(eng.applyEntityBoost(ctx, ws, 100, seedResults,
		&activation.ActivateRequest{Threshold: 0.05, StructuredFilter: rejectULIDFilter{reject: target}})),
		"a structured-filter-rejected engram must not be injected")
}
