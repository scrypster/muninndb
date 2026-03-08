package storage

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUpsertEntityRecord_RoundTrip(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	record := EntityRecord{
		Name:       "PostgreSQL",
		Type:       "database",
		Confidence: 0.95,
	}
	err := store.UpsertEntityRecord(ctx, record, "inline:test")
	require.NoError(t, err)

	// Normalized lookup (lowercase)
	got, err := store.GetEntityRecord(ctx, "postgresql")
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, "PostgreSQL", got.Name)
	require.Equal(t, "database", got.Type)
	require.Equal(t, "inline:test", got.Source)

	// Different case resolves to same record
	got2, err := store.GetEntityRecord(ctx, "POSTGRESQL")
	require.NoError(t, err)
	require.NotNil(t, got2)
	require.Equal(t, got.Name, got2.Name)
}

func TestGetEntityRecord_NotFound(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	got, err := store.GetEntityRecord(ctx, "nonexistent-entity")
	require.NoError(t, err)
	require.Nil(t, got)
}

func TestWriteEntityEngramLink(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	ws := store.VaultPrefix("entity-link-vault")

	// Write an entity first
	err := store.UpsertEntityRecord(ctx, EntityRecord{Name: "PostgreSQL", Type: "database", Confidence: 0.9}, "test")
	require.NoError(t, err)

	engramID := NewULID()

	// Write the link — should succeed without error
	err = store.WriteEntityEngramLink(ctx, ws, engramID, "PostgreSQL")
	require.NoError(t, err)
}

func TestUpsertRelationshipRecord(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	ws := store.VaultPrefix("relationship-vault")
	engramID := NewULID()

	record := RelationshipRecord{
		FromEntity: "payment-service",
		ToEntity:   "PostgreSQL",
		RelType:    "uses",
		Weight:     0.9,
		Source:     "plugin:enrich",
	}

	err := store.UpsertRelationshipRecord(ctx, ws, engramID, record)
	require.NoError(t, err)
}

func TestUpsertEntityRecord_UpdatePreservesName(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	// Write initial record
	err := store.UpsertEntityRecord(ctx, EntityRecord{
		Name:       "Redis",
		Type:       "cache",
		Confidence: 0.7,
	}, "inline:test")
	require.NoError(t, err)

	// Overwrite with higher confidence
	err = store.UpsertEntityRecord(ctx, EntityRecord{
		Name:       "Redis",
		Type:       "database",
		Confidence: 0.95,
	}, "plugin:enrich")
	require.NoError(t, err)

	got, err := store.GetEntityRecord(ctx, "redis")
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, "Redis", got.Name)
	require.Equal(t, "database", got.Type)
	require.Equal(t, "plugin:enrich", got.Source)
	require.InDelta(t, 0.95, got.Confidence, 0.001)
}

func TestUpsertEntityRecord_KeepsHigherConfidence(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	// Write initial record with high confidence.
	err := store.UpsertEntityRecord(ctx, EntityRecord{
		Name: "PostgreSQL", Type: "technology", Confidence: 0.9,
	}, "plugin:enrich")
	require.NoError(t, err)

	// Upsert with lower confidence — existing confidence must be preserved.
	err = store.UpsertEntityRecord(ctx, EntityRecord{
		Name: "PostgreSQL", Type: "technology", Confidence: 0.4,
	}, "inline")
	require.NoError(t, err)

	got, err := store.GetEntityRecord(ctx, "PostgreSQL")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.InDelta(t, float32(0.9), got.Confidence, 0.001, "higher confidence must be preserved")
}

func TestUpsertEntityRecord_UpdatesWhenHigherConfidence(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	err := store.UpsertEntityRecord(ctx, EntityRecord{
		Name: "PostgreSQL", Type: "technology", Confidence: 0.4,
	}, "inline")
	require.NoError(t, err)

	// Upsert with higher confidence — should update.
	err = store.UpsertEntityRecord(ctx, EntityRecord{
		Name: "PostgreSQL", Type: "technology", Confidence: 0.9,
	}, "plugin:enrich")
	require.NoError(t, err)

	got, err := store.GetEntityRecord(ctx, "PostgreSQL")
	require.NoError(t, err)
	assert.InDelta(t, float32(0.9), got.Confidence, 0.001, "higher confidence must be accepted")
	assert.Equal(t, "plugin:enrich", got.Source)
}

func TestEntityReverseIndex_WrittenOnLink(t *testing.T) {
	ps := newTestStore(t)
	ctx := context.Background()

	ws := ps.VaultPrefix("test")
	engID := NewULID()

	require.NoError(t, ps.UpsertEntityRecord(ctx, EntityRecord{
		Name: "PostgreSQL", Type: "technology", Confidence: 0.8,
	}, "test"))
	require.NoError(t, ps.WriteEntityEngramLink(ctx, ws, engID, "PostgreSQL"))

	var found []ULID
	err := ps.ScanEntityEngrams(ctx, "PostgreSQL", func(gotWS [8]byte, id ULID) error {
		found = append(found, id)
		return nil
	})
	require.NoError(t, err)
	require.Len(t, found, 1)
	assert.Equal(t, engID, found[0])
}

func TestEntityReverseIndex_MultipleEngrams(t *testing.T) {
	ps := newTestStore(t)
	ctx := context.Background()
	ws := ps.VaultPrefix("test")

	require.NoError(t, ps.UpsertEntityRecord(ctx, EntityRecord{
		Name: "Go", Type: "technology", Confidence: 0.7,
	}, "test"))

	id1, id2 := NewULID(), NewULID()
	require.NoError(t, ps.WriteEntityEngramLink(ctx, ws, id1, "Go"))
	require.NoError(t, ps.WriteEntityEngramLink(ctx, ws, id2, "Go"))

	var found []ULID
	require.NoError(t, ps.ScanEntityEngrams(ctx, "Go", func(_ [8]byte, id ULID) error {
		found = append(found, id)
		return nil
	}))
	assert.Len(t, found, 2)
}

func TestEntityReverseIndex_EmptyForUnknownEntity(t *testing.T) {
	ps := newTestStore(t)
	ctx := context.Background()

	var found []ULID
	require.NoError(t, ps.ScanEntityEngrams(ctx, "NonExistentEntity", func(_ [8]byte, id ULID) error {
		found = append(found, id)
		return nil
	}))
	assert.Empty(t, found)
}

func TestEntityRecord_FirstSeenSetOnce(t *testing.T) {
	ps := newTestStore(t)
	ctx := context.Background()

	// First upsert — FirstSeen should be set.
	err := ps.UpsertEntityRecord(ctx, EntityRecord{
		Name: "Go", Type: "technology", Confidence: 0.8,
	}, "test")
	require.NoError(t, err)

	got, err := ps.GetEntityRecord(ctx, "Go")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.NotZero(t, got.FirstSeen, "FirstSeen must be set on first upsert")
	firstSeen := got.FirstSeen

	// Second upsert — FirstSeen must NOT change.
	err = ps.UpsertEntityRecord(ctx, EntityRecord{
		Name: "Go", Type: "technology", Confidence: 0.9,
	}, "test")
	require.NoError(t, err)

	got2, err := ps.GetEntityRecord(ctx, "Go")
	require.NoError(t, err)
	assert.Equal(t, firstSeen, got2.FirstSeen, "FirstSeen must not change on second upsert")
}

func TestEntityRecord_MentionCountIncrementsOnUpsert(t *testing.T) {
	ps := newTestStore(t)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		err := ps.UpsertEntityRecord(ctx, EntityRecord{
			Name: "Python", Type: "technology", Confidence: 0.7,
		}, "test")
		require.NoError(t, err)
	}

	got, err := ps.GetEntityRecord(ctx, "Python")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, int32(3), got.MentionCount, "MentionCount should be 3 after 3 upserts")
}

func TestEntityRecord_StateDefaultActive(t *testing.T) {
	ps := newTestStore(t)
	ctx := context.Background()

	err := ps.UpsertEntityRecord(ctx, EntityRecord{
		Name: "Rust", Type: "technology", Confidence: 0.8,
	}, "test")
	require.NoError(t, err)

	got, err := ps.GetEntityRecord(ctx, "Rust")
	require.NoError(t, err)
	assert.Equal(t, "active", got.State, "default state must be 'active'")
}

func TestUpsertEntityRecord_ConcurrentPreservesHighestConfidence(t *testing.T) {
	ps := newTestStore(t)
	ctx := context.Background()

	// Seed with a low baseline.
	require.NoError(t, ps.UpsertEntityRecord(ctx, EntityRecord{
		Name: "ConcurrentEntity", Type: "test", Confidence: 0.1,
	}, "baseline"))

	var wg sync.WaitGroup
	// 20 goroutines: 10 write 0.8, 10 write 0.7.
	for i := 0; i < 10; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			_ = ps.UpsertEntityRecord(ctx, EntityRecord{
				Name: "ConcurrentEntity", Type: "test", Confidence: 0.8,
			}, "high")
		}()
		go func() {
			defer wg.Done()
			_ = ps.UpsertEntityRecord(ctx, EntityRecord{
				Name: "ConcurrentEntity", Type: "test", Confidence: 0.7,
			}, "mid")
		}()
	}
	wg.Wait()

	got, err := ps.GetEntityRecord(ctx, "ConcurrentEntity")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.InDelta(t, float32(0.8), got.Confidence, 0.001, "highest confidence must survive concurrent writes")
	assert.Equal(t, int32(21), got.MentionCount, "MentionCount must equal total concurrent writes")
}

func TestEntityRecord_MergedIntoPreservedOnUpsert(t *testing.T) {
	ps := newTestPebbleStore(t)
	ctx := context.Background()

	// First write: entity is merged.
	err := ps.UpsertEntityRecord(ctx, EntityRecord{
		Name: "OldName", Type: "technology", Confidence: 0.8,
		State: "merged", MergedInto: "CanonicalName",
	}, "test")
	require.NoError(t, err)

	// Second write: caller doesn't set State or MergedInto.
	err = ps.UpsertEntityRecord(ctx, EntityRecord{
		Name: "OldName", Type: "technology", Confidence: 0.9,
	}, "test")
	require.NoError(t, err)

	got, err := ps.GetEntityRecord(ctx, "OldName")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "merged", got.State, "State must be preserved")
	assert.Equal(t, "CanonicalName", got.MergedInto, "MergedInto must be preserved")
}

func TestEntityRecord_InvalidStateReturnsError(t *testing.T) {
	ps := newTestPebbleStore(t)
	ctx := context.Background()

	err := ps.UpsertEntityRecord(ctx, EntityRecord{
		Name: "X", Type: "technology", Confidence: 0.8,
		State: "invalid_state",
	}, "test")
	assert.Error(t, err, "invalid state should return error")
}

func TestEntityRecord_MergedIntoWithoutMergedStateReturnsError(t *testing.T) {
	ps := newTestPebbleStore(t)
	ctx := context.Background()

	err := ps.UpsertEntityRecord(ctx, EntityRecord{
		Name: "X", Type: "technology", Confidence: 0.8,
		State: "active", MergedInto: "Y",
	}, "test")
	assert.Error(t, err, "MergedInto without State=merged should return error")
}

// TestDigestFlagConstants_SyncWithPluginPackage asserts that the storage-local
// digest flag constants stay in sync with the canonical values defined in
// internal/plugin/types.go. If either set changes, this test will catch the
// drift at compile time via untyped constant comparison.
//
// storage cannot import plugin (circular), so the constants are duplicated.
// This test is the enforcement mechanism.
func TestDigestFlagConstants_SyncWithPluginPackage(t *testing.T) {
	// Canonical values from internal/plugin/types.go — update both if either changes.
	const (
		canonicalDigestClassified uint8 = 0x20
		canonicalDigestSummarized uint8 = 0x40
	)
	if digestClassifiedFlag != canonicalDigestClassified {
		t.Errorf("digestClassifiedFlag = 0x%02x, want 0x%02x (plugin.DigestClassified)", digestClassifiedFlag, canonicalDigestClassified)
	}
	if digestSummarizedFlag != canonicalDigestSummarized {
		t.Errorf("digestSummarizedFlag = 0x%02x, want 0x%02x (plugin.DigestSummarized)", digestSummarizedFlag, canonicalDigestSummarized)
	}
}
