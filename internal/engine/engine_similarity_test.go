package engine

import (
	"context"
	"testing"

	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/transport/mbp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeEntityEngram is a test helper that writes a memory with inline entities
// and returns the engram ID.
func writeEntityEngram(t *testing.T, eng *Engine, vault, content string, entities ...mbp.InlineEntity) string {
	t.Helper()
	resp, err := eng.Write(context.Background(), &mbp.WriteRequest{
		Vault:    vault,
		Content:  content,
		Entities: entities,
	})
	require.NoError(t, err)
	return resp.ID
}

// ── trigram helpers ──────────────────────────────────────────────────────────

func TestTrigramSim_Identical(t *testing.T) {
	sim := trigramSim("PostgreSQL", "PostgreSQL")
	if sim != 1.0 {
		t.Errorf("identical strings: expected 1.0, got %f", sim)
	}
}

func TestTrigramSim_Different(t *testing.T) {
	sim := trigramSim("PostgreSQL", "MongoDB")
	if sim >= 0.5 {
		t.Errorf("very different strings: expected low similarity, got %f", sim)
	}
}

func TestTrigramSim_Similar(t *testing.T) {
	// "PostgreSQL" vs "PostgreSQL DB" share many trigrams.
	sim := trigramSim("PostgreSQL", "PostgreSQL DB")
	if sim < 0.7 {
		t.Errorf("similar strings: expected high similarity, got %f", sim)
	}
}

func TestTrigramSim_ShortString(t *testing.T) {
	// Should not panic for very short strings.
	sim := trigramSim("AB", "ABC")
	if sim < 0 || sim > 1 {
		t.Errorf("short string similarity out of range: %f", sim)
	}
}

// ── FindSimilarEntities ──────────────────────────────────────────────────────

func TestFindSimilarEntities_FindsTypo(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	// Write two memories that reference very similar entity names.
	writeEntityEngram(t, eng, "default", "PostgreSQL is the primary DB",
		mbp.InlineEntity{Name: "PostgreSQL", Type: "database"})
	writeEntityEngram(t, eng, "default", "PostgreSQL DB is used for analytics",
		mbp.InlineEntity{Name: "PostgreSQL DB", Type: "database"})

	pairs, err := eng.FindSimilarEntities(ctx, "default", 0.7, 20)
	require.NoError(t, err)

	// At threshold 0.7 the two similar names should appear.
	found := false
	for _, p := range pairs {
		if (p.EntityA == "PostgreSQL" && p.EntityB == "PostgreSQL DB") ||
			(p.EntityA == "PostgreSQL DB" && p.EntityB == "PostgreSQL") {
			found = true
			if p.Similarity < 0.7 {
				t.Errorf("similarity %f below threshold 0.7", p.Similarity)
			}
		}
	}
	if !found {
		t.Errorf("expected pair (PostgreSQL, PostgreSQL DB), got: %v", pairs)
	}
}

func TestFindSimilarEntities_NoPairsForDifferentNames(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	writeEntityEngram(t, eng, "default", "PostgreSQL is the primary DB",
		mbp.InlineEntity{Name: "PostgreSQL", Type: "database"})
	writeEntityEngram(t, eng, "default", "Kubernetes orchestrates containers",
		mbp.InlineEntity{Name: "Kubernetes", Type: "technology"})

	pairs, err := eng.FindSimilarEntities(ctx, "default", 0.85, 20)
	require.NoError(t, err)

	// "PostgreSQL" and "Kubernetes" are very different — no pairs at 0.85.
	for _, p := range pairs {
		if (p.EntityA == "PostgreSQL" && p.EntityB == "Kubernetes") ||
			(p.EntityA == "Kubernetes" && p.EntityB == "PostgreSQL") {
			t.Errorf("unexpected similar pair: %v (similarity %f)", p, p.Similarity)
		}
	}
}

func TestFindSimilarEntities_EmptyVault(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	pairs, err := eng.FindSimilarEntities(ctx, "default", 0.85, 20)
	require.NoError(t, err)
	require.Empty(t, pairs)
}

func TestFindSimilarEntities_TopNCap(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	// Write memories with entity names that share partial similarity.
	names := []string{"AlphaService", "AlphaSvc", "AlphaAPI", "AlphaApp", "AlphaDB"}
	for i, name := range names {
		writeEntityEngram(t, eng, "default", "service "+name,
			mbp.InlineEntity{Name: name, Type: "service"})
		_ = i
	}

	pairs, err := eng.FindSimilarEntities(ctx, "default", 0.0, 3)
	require.NoError(t, err)
	require.LessOrEqual(t, len(pairs), 3, "should cap at topN=3")
}

func TestFindSimilarEntities_InvalidThreshold(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	_, err := eng.FindSimilarEntities(ctx, "default", 1.5, 20)
	require.Error(t, err)
}

// ── MergeEntity ──────────────────────────────────────────────────────────────

func TestMergeEntity_DryRun(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	writeEntityEngram(t, eng, "default", "PostgreSQL is the primary DB",
		mbp.InlineEntity{Name: "PostgreSQL", Type: "database"})
	writeEntityEngram(t, eng, "default", "Postgre SQL variant",
		mbp.InlineEntity{Name: "Postgre SQL", Type: "database"})

	result, err := eng.MergeEntity(ctx, "default", "Postgre SQL", "PostgreSQL", true)
	require.NoError(t, err)
	require.True(t, result.DryRun)
	require.Equal(t, "Postgre SQL", result.EntityA)
	require.Equal(t, "PostgreSQL", result.EntityB)
	require.GreaterOrEqual(t, result.EngramsRelinked, 0)

	// Verify that entity A is NOT changed to merged (dry run).
	recA, err := eng.store.GetEntityRecord(ctx, "Postgre SQL")
	require.NoError(t, err)
	require.NotNil(t, recA)
	require.NotEqual(t, "merged", recA.State, "dry_run must not modify entity A's state")
}

func TestMergeEntity_MergesAndRelinks(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	// Write two engrams linking to entity A ("Postgre SQL").
	id1 := writeEntityEngram(t, eng, "default", "Postgre SQL is legacy name",
		mbp.InlineEntity{Name: "Postgre SQL", Type: "database"})
	id2 := writeEntityEngram(t, eng, "default", "Also Postgre SQL config",
		mbp.InlineEntity{Name: "Postgre SQL", Type: "database"})
	// Write one engram linking to entity B ("PostgreSQL").
	writeEntityEngram(t, eng, "default", "PostgreSQL is the canonical name",
		mbp.InlineEntity{Name: "PostgreSQL", Type: "database"})

	result, err := eng.MergeEntity(ctx, "default", "Postgre SQL", "PostgreSQL", false)
	require.NoError(t, err)
	require.False(t, result.DryRun)
	require.Equal(t, 2, result.EngramsRelinked, "two engrams should have been relinked")

	// Entity A should now be state=merged.
	recA, err := eng.store.GetEntityRecord(ctx, "Postgre SQL")
	require.NoError(t, err)
	require.NotNil(t, recA)
	require.Equal(t, "merged", recA.State)
	require.Equal(t, "PostgreSQL", recA.MergedInto)

	// The two engrams previously linked to A should now also link to B.
	ws := eng.store.ResolveVaultPrefix("default")
	for _, rawID := range []string{id1, id2} {
		ulid, err := storage.ParseULID(rawID)
		require.NoError(t, err)

		var foundB bool
		err = eng.store.ScanEngramEntities(ctx, ws, ulid, func(name string) error {
			if name == "PostgreSQL" {
				foundB = true
			}
			return nil
		})
		require.NoError(t, err)
		require.True(t, foundB, "engram %s should now link to PostgreSQL after merge", rawID)
	}
}

func TestMergeEntity_EntityANotFound(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	writeEntityEngram(t, eng, "default", "PostgreSQL is the canonical name",
		mbp.InlineEntity{Name: "PostgreSQL", Type: "database"})

	_, err := eng.MergeEntity(ctx, "default", "NonExistent", "PostgreSQL", false)
	require.Error(t, err)
	require.Contains(t, err.Error(), "not found")
}

func TestMergeEntity_EntityBNotFound(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	writeEntityEngram(t, eng, "default", "Postgre SQL legacy",
		mbp.InlineEntity{Name: "Postgre SQL", Type: "database"})

	_, err := eng.MergeEntity(ctx, "default", "Postgre SQL", "NonExistent", false)
	require.Error(t, err)
	require.Contains(t, err.Error(), "not found")
}

func TestMergeEntity_SameEntityRejected(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	writeEntityEngram(t, eng, "default", "PostgreSQL",
		mbp.InlineEntity{Name: "PostgreSQL", Type: "database"})

	_, err := eng.MergeEntity(ctx, "default", "PostgreSQL", "PostgreSQL", false)
	require.Error(t, err)
	require.Contains(t, err.Error(), "must be different")
}

// TestMergeEntity_RelinkRelationships verifies that after MergeEntity(A→B), relationship
// records that referenced A are updated to reference B. ScanEntityRelationships("A")
// returns nothing; ScanEntityRelationships("B") returns all previously-A relationships.
func TestMergeEntity_RelinkRelationships(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	// Write entities in separate engrams to avoid triggering the engine's
	// automatic co-occurrence relationship (which fires when two entities share an engram).
	writeEntityEngram(t, eng, "default", "Postgre SQL is a database",
		mbp.InlineEntity{Name: "Postgre SQL", Type: "database"})
	writeEntityEngram(t, eng, "default", "auth-service is a service",
		mbp.InlineEntity{Name: "auth-service", Type: "service"})
	writeEntityEngram(t, eng, "default", "PostgreSQL canonical",
		mbp.InlineEntity{Name: "PostgreSQL", Type: "database"})

	// Write a relationship: auth-service uses "Postgre SQL".
	ws := eng.store.ResolveVaultPrefix("default")
	require.NoError(t, eng.store.UpsertRelationshipRecord(ctx, ws, storage.NewULID(), storage.RelationshipRecord{
		FromEntity: "auth-service",
		ToEntity:   "Postgre SQL",
		RelType:    "uses",
		Weight:     0.9,
		Source:     "test",
	}))

	// Before merge: ScanEntityRelationships("Postgre SQL") finds 1 record.
	var before []storage.RelationshipRecord
	require.NoError(t, eng.store.ScanEntityRelationships(ctx, ws, "Postgre SQL",
		func(r storage.RelationshipRecord) error {
			before = append(before, r)
			return nil
		}))
	require.Len(t, before, 1, "must find relationship before merge")

	// Merge A → B.
	result, err := eng.MergeEntity(ctx, "default", "Postgre SQL", "PostgreSQL", false)
	require.NoError(t, err)
	require.False(t, result.DryRun)

	// After merge: "Postgre SQL" must have no relationships.
	var afterA []storage.RelationshipRecord
	require.NoError(t, eng.store.ScanEntityRelationships(ctx, ws, "Postgre SQL",
		func(r storage.RelationshipRecord) error {
			afterA = append(afterA, r)
			return nil
		}))
	assert.Empty(t, afterA, "ScanEntityRelationships(A) must be empty after merge")

	// After merge: "PostgreSQL" must now own the relationship.
	var afterB []storage.RelationshipRecord
	require.NoError(t, eng.store.ScanEntityRelationships(ctx, ws, "PostgreSQL",
		func(r storage.RelationshipRecord) error {
			afterB = append(afterB, r)
			return nil
		}))
	require.Len(t, afterB, 1, "ScanEntityRelationships(B) must find the relinked relationship")
	assert.Equal(t, "auth-service", afterB[0].FromEntity)
	assert.Equal(t, "PostgreSQL", afterB[0].ToEntity, "ToEntity must be updated to canonical name")
}

func TestMergeEntity_AlreadyMergedEntityARejected(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	writeEntityEngram(t, eng, "default", "Postgre SQL legacy",
		mbp.InlineEntity{Name: "Postgre SQL", Type: "database"})
	writeEntityEngram(t, eng, "default", "PostgreSQL canonical",
		mbp.InlineEntity{Name: "PostgreSQL", Type: "database"})
	writeEntityEngram(t, eng, "default", "PG shorthand",
		mbp.InlineEntity{Name: "PG", Type: "database"})

	// First merge: A → B succeeds.
	_, err := eng.MergeEntity(ctx, "default", "Postgre SQL", "PostgreSQL", false)
	require.NoError(t, err)

	// Second merge of the same A (now state=merged) must be rejected.
	_, err = eng.MergeEntity(ctx, "default", "Postgre SQL", "PG", false)
	require.Error(t, err)
	require.Contains(t, err.Error(), "already merged", "re-merging a merged entity must return an explicit error")
}

func TestMergeEntity_DeletesStaleLinksForMergedEntity(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	// Write two engrams linking to entity A.
	writeEntityEngram(t, eng, "default", "Postgre SQL first mention",
		mbp.InlineEntity{Name: "Postgre SQL", Type: "database"})
	writeEntityEngram(t, eng, "default", "Postgre SQL second mention",
		mbp.InlineEntity{Name: "Postgre SQL", Type: "database"})
	// Write one engram linking to entity B (the canonical).
	writeEntityEngram(t, eng, "default", "PostgreSQL is canonical",
		mbp.InlineEntity{Name: "PostgreSQL", Type: "database"})

	_, err := eng.MergeEntity(ctx, "default", "Postgre SQL", "PostgreSQL", false)
	require.NoError(t, err)

	// After merge: 0x23 reverse index for A must be empty.
	// A's two engrams were relinked to B; the stale A links must be gone.
	var staleLinks []storage.ULID
	err = eng.store.ScanEntityEngrams(ctx, "Postgre SQL", func(_ [8]byte, id storage.ULID) error {
		staleLinks = append(staleLinks, id)
		return nil
	})
	require.NoError(t, err)
	require.Empty(t, staleLinks, "merged entity A must have no remaining 0x23 reverse links after merge")

	// Entity B should have all three engrams linked (2 relinked + 1 original).
	var bLinks []storage.ULID
	err = eng.store.ScanEntityEngrams(ctx, "PostgreSQL", func(_ [8]byte, id storage.ULID) error {
		bLinks = append(bLinks, id)
		return nil
	})
	require.NoError(t, err)
	require.Len(t, bLinks, 3, "entity B must have all 3 engrams linked after merge")
}

func TestMergeEntity_DryRun_PreservesAllLinks(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	writeEntityEngram(t, eng, "default", "Postgre SQL mention",
		mbp.InlineEntity{Name: "Postgre SQL", Type: "database"})
	writeEntityEngram(t, eng, "default", "PostgreSQL canonical",
		mbp.InlineEntity{Name: "PostgreSQL", Type: "database"})

	_, err := eng.MergeEntity(ctx, "default", "Postgre SQL", "PostgreSQL", true)
	require.NoError(t, err)

	// dryRun must not modify any links — A's link must still exist.
	var aLinks []storage.ULID
	err = eng.store.ScanEntityEngrams(ctx, "Postgre SQL", func(_ [8]byte, id storage.ULID) error {
		aLinks = append(aLinks, id)
		return nil
	})
	require.NoError(t, err)
	require.Len(t, aLinks, 1, "dry run must not delete entity A's reverse links")
}

// ── FindSimilarEntities — inverted index correctness ─────────────────────────

// TestFindSimilarEntities_InvertedIndexMatchesNaive verifies that the optimised
// inverted-trigram-index implementation returns the same pairs as a naive O(n²)
// reference implementation for a representative entity set.
func TestFindSimilarEntities_InvertedIndexMatchesNaive(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	// Write a variety of entities with known similarity relationships.
	entityNames := []string{
		"PostgreSQL", "PostgreSQL DB", "Postgre SQL",
		"Redis", "Rediss", "RediSearch",
		"Kubernetes", "K8s", "KubeCtl",
		"payment-service", "payment_service", "PaymentService",
		"auth-service", "AuthService", "auth_svc",
	}
	for _, name := range entityNames {
		writeEntityEngram(t, eng, "default", "mention of "+name,
			mbp.InlineEntity{Name: name, Type: "technology"})
	}

	threshold := 0.5
	optimised, err := eng.FindSimilarEntities(ctx, "default", threshold, 1000)
	require.NoError(t, err)

	// Build reference result using naive O(n²) approach.
	ws := eng.store.ResolveVaultPrefix("default")
	var names []string
	err = eng.store.ScanVaultEntityNames(ctx, ws, func(name string) error {
		names = append(names, name)
		return nil
	})
	require.NoError(t, err)

	naivePairs := make(map[[2]string]float64)
	for i := 0; i < len(names); i++ {
		for j := i + 1; j < len(names); j++ {
			sim := trigramSim(names[i], names[j])
			if sim >= threshold {
				key := [2]string{names[i], names[j]}
				if names[i] > names[j] {
					key = [2]string{names[j], names[i]}
				}
				naivePairs[key] = sim
			}
		}
	}

	// Every naive pair must appear in the optimised result.
	optimisedPairs := make(map[[2]string]float64)
	for _, p := range optimised {
		key := [2]string{p.EntityA, p.EntityB}
		if p.EntityA > p.EntityB {
			key = [2]string{p.EntityB, p.EntityA}
		}
		optimisedPairs[key] = p.Similarity
	}

	for key := range naivePairs {
		_, found := optimisedPairs[key]
		require.True(t, found, "pair (%s, %s) found by naive but missing from optimised result",
			key[0], key[1])
	}
	require.Equal(t, len(naivePairs), len(optimisedPairs),
		"optimised and naive must return the same number of pairs")
}

// TestFindSimilarEntities_ThresholdZero_ExcludesZeroSimPairs documents and pins the
// known behaviour of the inverted-trigram-index at threshold=0.0: only pairs that
// share at least one trigram (sim > 0) are returned.  Pairs with no shared trigrams
// (sim == 0) are never candidates in the index and are therefore not returned.
// This is intentional — a sim=0 pair means "nothing in common" and is not actionable.
func TestFindSimilarEntities_ThresholdZero_ExcludesZeroSimPairs(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	// These two names share no trigrams: "aaa" vs "bbb".
	// trigramSim("aaa", "bbb") == 0.0 because their trigram sets are disjoint.
	writeEntityEngram(t, eng, "default", "mention aaa",
		mbp.InlineEntity{Name: "aaa", Type: "token"})
	writeEntityEngram(t, eng, "default", "mention bbb",
		mbp.InlineEntity{Name: "bbb", Type: "token"})

	pairs, err := eng.FindSimilarEntities(ctx, "default", 0.0, 100)
	require.NoError(t, err)

	for _, p := range pairs {
		if (p.EntityA == "aaa" && p.EntityB == "bbb") ||
			(p.EntityA == "bbb" && p.EntityB == "aaa") {
			t.Errorf("pair (aaa, bbb) with sim=0 must not appear at threshold=0.0: got similarity=%f", p.Similarity)
		}
		// Any pair that IS returned must have sim > 0 (shares at least one trigram).
		if p.Similarity <= 0 {
			t.Errorf("returned pair (%s, %s) has sim=%f; all returned pairs must have sim > 0",
				p.EntityA, p.EntityB, p.Similarity)
		}
	}
}

// ── Benchmarks ───────────────────────────────────────────────────────────────

// BenchmarkFindSimilarEntities measures the inverted-trigram-index implementation
// against a realistic entity set.  Run with -bench=. -benchtime=5s to get stable numbers.
//
// Measured baseline on Apple M4 Max (50 entities, threshold=0.5):
//
//	BenchmarkFindSimilarEntities-16  5382  606529 ns/op  (~0.6ms)
//
// If this regresses above ~5ms for 50 entities the O(n²) path has been reintroduced.
func BenchmarkFindSimilarEntities(b *testing.B) {
	eng, cleanup := testEnvTB(b)
	defer cleanup()
	ctx := context.Background()

	names := []string{
		"PostgreSQL", "PostgreSQL DB", "Postgre SQL", "PostGres", "PG",
		"Redis", "Rediss", "RediSearch", "RedisDB", "Redis Cache",
		"Kubernetes", "K8s", "KubeCtl", "KubernetesAPI", "KubeAPI",
		"payment-service", "payment_service", "PaymentService", "PaymentSvc", "pay-svc",
		"auth-service", "AuthService", "auth_svc", "AuthSvc", "authentication-service",
		"order-service", "OrderService", "order_svc", "OrderAPI", "orders-api",
		"user-service", "UserService", "user_svc", "UserAPI", "users-api",
		"notification-service", "NotificationService", "notif-svc", "NotifAPI", "push-service",
		"billing-service", "BillingService", "billing_svc", "BillingAPI", "invoice-service",
		"search-service", "SearchService", "search_svc", "SearchAPI", "ElasticSearch",
	}
	for _, name := range names {
		_, err := eng.Write(context.Background(), &mbp.WriteRequest{
			Vault:    "bench",
			Content:  "benchmark entity " + name,
			Entities: []mbp.InlineEntity{{Name: name, Type: "service"}},
		})
		if err != nil {
			b.Fatal(err)
		}
	}

	b.ResetTimer()
	for range b.N {
		_, err := eng.FindSimilarEntities(ctx, "bench", 0.5, 1000)
		if err != nil {
			b.Fatal(err)
		}
	}
}
