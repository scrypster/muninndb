package storage

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// DecrementEntityMentionCount
// ---------------------------------------------------------------------------

func TestDecrementEntityMentionCount_Basic(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	require.NoError(t, store.UpsertEntityRecord(ctx, EntityRecord{
		Name: "Go", Type: "language", Confidence: 0.9,
	}, "test"))
	require.NoError(t, store.UpsertEntityRecord(ctx, EntityRecord{
		Name: "Go", Type: "language", Confidence: 0.9,
	}, "test"))

	got, err := store.GetEntityRecord(ctx, "Go")
	require.NoError(t, err)
	require.Equal(t, int32(2), got.MentionCount)

	require.NoError(t, store.DecrementEntityMentionCount(ctx, "Go"))

	got, err = store.GetEntityRecord(ctx, "Go")
	require.NoError(t, err)
	require.NotNil(t, got, "entity with remaining references must still exist")
	assert.Equal(t, int32(1), got.MentionCount)
}

func TestDecrementEntityMentionCount_NoopOnMissing(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	// Should not error on a non-existent entity.
	require.NoError(t, store.DecrementEntityMentionCount(ctx, "NonExistent"))
}

func TestDecrementEntityMentionCount_FloorAtZero(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	require.NoError(t, store.UpsertEntityRecord(ctx, EntityRecord{
		Name: "Rust", Type: "language", Confidence: 0.8,
	}, "test"))

	// Write a live reverse link so the entity is NOT orphaned.
	ws := store.VaultPrefix("test-vault")
	id := NewULID()
	require.NoError(t, store.WriteEntityEngramLink(ctx, ws, id, "Rust"))

	// Decrement twice — count should floor at 0, not go negative.
	require.NoError(t, store.DecrementEntityMentionCount(ctx, "Rust"))
	require.NoError(t, store.DecrementEntityMentionCount(ctx, "Rust"))

	got, err := store.GetEntityRecord(ctx, "Rust")
	require.NoError(t, err)
	require.NotNil(t, got, "entity with live reverse links must survive")
	assert.Equal(t, int32(0), got.MentionCount, "count must not go below 0")
}

func TestDecrementEntityMentionCount_DeletesOrphanedEntity(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	// Write entity with a single mention.
	require.NoError(t, store.UpsertEntityRecord(ctx, EntityRecord{
		Name: "Orphan", Type: "test", Confidence: 0.5,
	}, "test"))

	// No reverse links written — entity is orphaned once count hits 0.
	require.NoError(t, store.DecrementEntityMentionCount(ctx, "Orphan"))

	got, err := store.GetEntityRecord(ctx, "Orphan")
	require.NoError(t, err)
	assert.Nil(t, got, "orphaned entity (MentionCount=0, no 0x23 links) must be deleted")
}

func TestDecrementEntityMentionCount_PreservesEntityWithLiveLinks(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	require.NoError(t, store.UpsertEntityRecord(ctx, EntityRecord{
		Name: "Shared", Type: "test", Confidence: 0.7,
	}, "test"))

	// Write a live reverse link — entity is NOT orphaned even at count==0.
	ws := store.VaultPrefix("live-vault")
	id := NewULID()
	require.NoError(t, store.WriteEntityEngramLink(ctx, ws, id, "Shared"))

	require.NoError(t, store.DecrementEntityMentionCount(ctx, "Shared"))

	got, err := store.GetEntityRecord(ctx, "Shared")
	require.NoError(t, err)
	assert.NotNil(t, got, "entity with live 0x23 reverse links must be preserved")
}

// ---------------------------------------------------------------------------
// DecrementEntityCoOccurrence
// ---------------------------------------------------------------------------

func TestDecrementEntityCoOccurrence_Basic(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("cooccurrence-decrement")

	require.NoError(t, store.IncrementEntityCoOccurrence(ctx, ws, "Go", "PostgreSQL"))
	require.NoError(t, store.IncrementEntityCoOccurrence(ctx, ws, "Go", "PostgreSQL"))

	require.NoError(t, store.DecrementEntityCoOccurrence(ctx, ws, "Go", "PostgreSQL"))

	var pairs []struct{ count int }
	require.NoError(t, store.ScanEntityClusters(ctx, ws, 1, func(_, _ string, count int) error {
		pairs = append(pairs, struct{ count int }{count})
		return nil
	}))
	require.Len(t, pairs, 1, "pair must still exist after one decrement")
	assert.Equal(t, 1, pairs[0].count)
}

func TestDecrementEntityCoOccurrence_DeletesAtZero(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("cooccurrence-delete")

	require.NoError(t, store.IncrementEntityCoOccurrence(ctx, ws, "Redis", "Kafka"))
	require.NoError(t, store.DecrementEntityCoOccurrence(ctx, ws, "Redis", "Kafka"))

	var pairs []struct{ a, b string }
	require.NoError(t, store.ScanEntityClusters(ctx, ws, 1, func(a, b string, _ int) error {
		pairs = append(pairs, struct{ a, b string }{a, b})
		return nil
	}))
	assert.Empty(t, pairs, "0x24 key must be deleted when count reaches 0")
}

func TestDecrementEntityCoOccurrence_SymmetricWithIncrement(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("cooccurrence-symmetric")

	// Increment with (B, A) order.
	require.NoError(t, store.IncrementEntityCoOccurrence(ctx, ws, "PostgreSQL", "Go"))
	require.NoError(t, store.IncrementEntityCoOccurrence(ctx, ws, "PostgreSQL", "Go"))

	// Decrement with (A, B) order — must hit the same canonical key.
	require.NoError(t, store.DecrementEntityCoOccurrence(ctx, ws, "Go", "PostgreSQL"))

	var pairs []struct{ count int }
	require.NoError(t, store.ScanEntityClusters(ctx, ws, 1, func(_, _ string, count int) error {
		pairs = append(pairs, struct{ count int }{count})
		return nil
	}))
	require.Len(t, pairs, 1)
	assert.Equal(t, 1, pairs[0].count, "decrement must use same canonical key regardless of argument order")
}

func TestDecrementEntityCoOccurrence_NoopOnMissing(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("cooccurrence-noop")

	// Should not error if the pair doesn't exist.
	require.NoError(t, store.DecrementEntityCoOccurrence(ctx, ws, "X", "Y"))
}

// ---------------------------------------------------------------------------
// DeleteEngram — entity graph cleanup integration
// ---------------------------------------------------------------------------

func TestDeleteEngram_CleansEntityLinks(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("delete-entity-links")

	// Write engram with entity links.
	eng := makeTestEngram("entity cleanup test")
	_, err := store.WriteEngram(ctx, ws, eng)
	require.NoError(t, err)

	require.NoError(t, store.UpsertEntityRecord(ctx, EntityRecord{
		Name: "PostgreSQL", Type: "database", Confidence: 0.9,
	}, "test"))
	require.NoError(t, store.WriteEntityEngramLink(ctx, ws, eng.ID, "PostgreSQL"))

	// Verify link exists.
	var before []ULID
	require.NoError(t, store.ScanEntityEngrams(ctx, "PostgreSQL", func(_ [8]byte, id ULID) error {
		before = append(before, id)
		return nil
	}))
	require.Len(t, before, 1)

	// Hard delete.
	require.NoError(t, store.DeleteEngram(ctx, ws, eng.ID))

	// 0x23 reverse link must be gone.
	var after []ULID
	require.NoError(t, store.ScanEntityEngrams(ctx, "PostgreSQL", func(_ [8]byte, id ULID) error {
		after = append(after, id)
		return nil
	}))
	assert.Empty(t, after, "0x23 reverse link must be removed after DeleteEngram")

	// 0x20 forward link must be gone.
	var entities []string
	require.NoError(t, store.ScanEngramEntities(ctx, ws, eng.ID, func(name string) error {
		entities = append(entities, name)
		return nil
	}))
	assert.Empty(t, entities, "0x20 forward link must be removed after DeleteEngram")
}

func TestDeleteEngram_DecrementsAndDeletesOrphanedEntity(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("delete-orphan-entity")

	eng := makeTestEngram("orphan entity test")
	_, err := store.WriteEngram(ctx, ws, eng)
	require.NoError(t, err)

	require.NoError(t, store.UpsertEntityRecord(ctx, EntityRecord{
		Name: "Orphan", Type: "test", Confidence: 0.5,
	}, "test"))
	require.NoError(t, store.WriteEntityEngramLink(ctx, ws, eng.ID, "Orphan"))

	require.NoError(t, store.DeleteEngram(ctx, ws, eng.ID))

	got, err := store.GetEntityRecord(ctx, "Orphan")
	require.NoError(t, err)
	assert.Nil(t, got, "entity with no remaining references must be deleted after hard delete")
}

func TestDeleteEngram_PreservesEntityReferencedByOtherEngram(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("delete-shared-entity")

	eng1 := makeTestEngram("first engram")
	eng2 := makeTestEngram("second engram")
	_, err := store.WriteEngram(ctx, ws, eng1)
	require.NoError(t, err)
	_, err = store.WriteEngram(ctx, ws, eng2)
	require.NoError(t, err)

	require.NoError(t, store.UpsertEntityRecord(ctx, EntityRecord{
		Name: "Shared", Type: "test", Confidence: 0.8,
	}, "test"))
	require.NoError(t, store.UpsertEntityRecord(ctx, EntityRecord{
		Name: "Shared", Type: "test", Confidence: 0.8,
	}, "test"))
	require.NoError(t, store.WriteEntityEngramLink(ctx, ws, eng1.ID, "Shared"))
	require.NoError(t, store.WriteEntityEngramLink(ctx, ws, eng2.ID, "Shared"))

	// Delete only the first engram.
	require.NoError(t, store.DeleteEngram(ctx, ws, eng1.ID))

	got, err := store.GetEntityRecord(ctx, "Shared")
	require.NoError(t, err)
	assert.NotNil(t, got, "entity referenced by a surviving engram must not be deleted")
}

func TestDeleteEngram_CleansRelationshipRecords(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("delete-relationships")

	eng := makeTestEngram("relationship cleanup test")
	_, err := store.WriteEngram(ctx, ws, eng)
	require.NoError(t, err)

	require.NoError(t, store.UpsertRelationshipRecord(ctx, ws, eng.ID, RelationshipRecord{
		FromEntity: "payment-service",
		ToEntity:   "PostgreSQL",
		RelType:    "uses",
		Weight:     0.9,
		Source:     "test",
	}))

	// Verify relationship exists before delete.
	var before []RelationshipRecord
	require.NoError(t, store.ScanRelationships(ctx, ws, func(r RelationshipRecord) error {
		before = append(before, r)
		return nil
	}))
	require.Len(t, before, 1)

	require.NoError(t, store.DeleteEngram(ctx, ws, eng.ID))

	// 0x21 relationship record must be gone.
	var after []RelationshipRecord
	require.NoError(t, store.ScanRelationships(ctx, ws, func(r RelationshipRecord) error {
		after = append(after, r)
		return nil
	}))
	assert.Empty(t, after, "0x21 relationship record must be removed after DeleteEngram")
}

func TestDeleteEngram_CleansCoOccurrence(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("delete-cooccurrence")

	eng := makeTestEngram("co-occurrence cleanup test")
	_, err := store.WriteEngram(ctx, ws, eng)
	require.NoError(t, err)

	// Write entity links and co-occurrence for the engram.
	for _, name := range []string{"Go", "PostgreSQL"} {
		require.NoError(t, store.UpsertEntityRecord(ctx, EntityRecord{
			Name: name, Type: "technology", Confidence: 0.8,
		}, "test"))
		require.NoError(t, store.WriteEntityEngramLink(ctx, ws, eng.ID, name))
	}
	require.NoError(t, store.IncrementEntityCoOccurrence(ctx, ws, "Go", "PostgreSQL"))

	require.NoError(t, store.DeleteEngram(ctx, ws, eng.ID))

	// Co-occurrence pair must be gone.
	var pairs []struct{ a, b string }
	require.NoError(t, store.ScanEntityClusters(ctx, ws, 1, func(a, b string, _ int) error {
		pairs = append(pairs, struct{ a, b string }{a, b})
		return nil
	}))
	assert.Empty(t, pairs, "0x24 co-occurrence key must be removed after DeleteEngram")
}

func TestSoftDelete_PreservesEntityLinks(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("softdelete-preserve-links")

	eng := makeTestEngram("soft delete entity preservation")
	_, err := store.WriteEngram(ctx, ws, eng)
	require.NoError(t, err)

	require.NoError(t, store.UpsertEntityRecord(ctx, EntityRecord{
		Name: "SoftEntity", Type: "test", Confidence: 0.7,
	}, "test"))
	require.NoError(t, store.WriteEntityEngramLink(ctx, ws, eng.ID, "SoftEntity"))

	// Soft delete.
	require.NoError(t, store.SoftDelete(ctx, ws, eng.ID))

	// Entity links must survive — Restore depends on them.
	var found []ULID
	require.NoError(t, store.ScanEntityEngrams(ctx, "SoftEntity", func(_ [8]byte, id ULID) error {
		found = append(found, id)
		return nil
	}))
	assert.Len(t, found, 1, "0x23 reverse link must survive SoftDelete (needed for Restore)")

	got, err := store.GetEntityRecord(ctx, "SoftEntity")
	require.NoError(t, err)
	assert.NotNil(t, got, "entity record must survive SoftDelete")
}

// ---------------------------------------------------------------------------
// UpsertEntityRecord — MergedInto unmerge fix
// ---------------------------------------------------------------------------

func TestUpsertEntityRecord_UnmergeToActive(t *testing.T) {
	store := newTestPebbleStore(t)
	ctx := context.Background()

	// Write a merged entity.
	require.NoError(t, store.UpsertEntityRecord(ctx, EntityRecord{
		Name: "OldFoo", Type: "test", Confidence: 0.8,
		State: "merged", MergedInto: "CanonicalFoo",
	}, "test"))

	// Transition back to active — must succeed (was previously broken).
	require.NoError(t, store.UpsertEntityRecord(ctx, EntityRecord{
		Name: "OldFoo", Type: "test", Confidence: 0.8,
		State: "active",
	}, "test"), "transitioning merged entity back to active must succeed")

	got, err := store.GetEntityRecord(ctx, "OldFoo")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "active", got.State)
	assert.Empty(t, got.MergedInto, "MergedInto must be cleared when transitioning to active")
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func makeTestEngram(content string) *Engram {
	return &Engram{
		ID:      NewULID(),
		Concept: "test",
		Content: content,
	}
}
