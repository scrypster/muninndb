package storage

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// UpsertRelationshipRecord — 0x26 index writes
// ---------------------------------------------------------------------------

func TestUpsertRelationshipRecord_WritesRelEntityIndex(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("entity-test")
	ws = store.VaultPrefix("rel-index-write")

	eng := makeTestEngram("relationship index write test")
	_, err := store.WriteEngram(ctx, ws, eng)
	require.NoError(t, err)

	require.NoError(t, store.UpsertRelationshipRecord(ctx, ws, eng.ID, RelationshipRecord{
		FromEntity: "payment-service",
		ToEntity:   "PostgreSQL",
		RelType:    "uses",
		Weight:     0.9,
		Source:     "test",
	}))

	// ScanEntityRelationships must find the record via the 0x26 index for fromEntity.
	var fromRels []RelationshipRecord
	require.NoError(t, store.ScanEntityRelationships(ctx, ws, "payment-service",
		func(r RelationshipRecord) error {
			fromRels = append(fromRels, r)
			return nil
		}))
	require.Len(t, fromRels, 1, "0x26 index must route fromEntity query to the record")
	assert.Equal(t, "payment-service", fromRels[0].FromEntity)
	assert.Equal(t, "PostgreSQL", fromRels[0].ToEntity)

	// ScanEntityRelationships must also find the record via the 0x26 index for toEntity.
	var toRels []RelationshipRecord
	require.NoError(t, store.ScanEntityRelationships(ctx, ws, "PostgreSQL",
		func(r RelationshipRecord) error {
			toRels = append(toRels, r)
			return nil
		}))
	require.Len(t, toRels, 1, "0x26 index must route toEntity query to the record")
	assert.Equal(t, "PostgreSQL", toRels[0].ToEntity)
}

// ---------------------------------------------------------------------------
// RelinkRelationshipEntity
// ---------------------------------------------------------------------------

// TestRelinkRelationshipEntity_UpdatesFromAndToEntries verifies that after calling
// RelinkRelationshipEntity(oldName, newName):
//   - ScanEntityRelationships(oldName) returns 0 records
//   - ScanEntityRelationships(newName) returns all records that previously referenced oldName
//   - The record values themselves contain newName (not oldName)
func TestRelinkRelationshipEntity_UpdatesFromAndToEntries(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("entity-test")
	ws = store.VaultPrefix("relink-rel-entity")

	eng := makeTestEngram("relink relationship test")
	_, err := store.WriteEngram(ctx, ws, eng)
	require.NoError(t, err)

	// Write a relationship: "Postgre SQL" uses "PostgreSQL".
	require.NoError(t, store.UpsertRelationshipRecord(ctx, ws, eng.ID, RelationshipRecord{
		FromEntity: "Postgre SQL",
		ToEntity:   "PostgreSQL",
		RelType:    "uses",
		Weight:     0.9,
		Source:     "test",
	}))

	// Before relink: ScanEntityRelationships("Postgre SQL") finds 1 record.
	var before []RelationshipRecord
	require.NoError(t, store.ScanEntityRelationships(ctx, ws, "Postgre SQL",
		func(r RelationshipRecord) error {
			before = append(before, r)
			return nil
		}))
	require.Len(t, before, 1, "must find record before relink")

	// Relink "Postgre SQL" → "PostgreSQL" in all relationship records.
	require.NoError(t, store.RelinkRelationshipEntity(ctx, ws, "Postgre SQL", "PostgreSQL"))

	// After relink: ScanEntityRelationships("Postgre SQL") must return nothing.
	var afterOld []RelationshipRecord
	require.NoError(t, store.ScanEntityRelationships(ctx, ws, "Postgre SQL",
		func(r RelationshipRecord) error {
			afterOld = append(afterOld, r)
			return nil
		}))
	assert.Empty(t, afterOld, "ScanEntityRelationships for old name must return nothing after relink")

	// ScanEntityRelationships("PostgreSQL") must find 1 record where both sides are "PostgreSQL".
	var afterNew []RelationshipRecord
	require.NoError(t, store.ScanEntityRelationships(ctx, ws, "PostgreSQL",
		func(r RelationshipRecord) error {
			afterNew = append(afterNew, r)
			return nil
		}))
	require.Len(t, afterNew, 1, "ScanEntityRelationships for new name must return 1 record")
	assert.Equal(t, "PostgreSQL", afterNew[0].FromEntity, "FromEntity must be updated to new name")
	assert.Equal(t, "PostgreSQL", afterNew[0].ToEntity, "ToEntity must remain PostgreSQL")
}

// TestRelinkRelationshipEntity_ToEntitySide verifies the case where oldName appears
// as toEntity (not fromEntity).
func TestRelinkRelationshipEntity_ToEntitySide(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("entity-test")
	ws = store.VaultPrefix("relink-rel-to")

	eng := makeTestEngram("relink to-entity test")
	_, err := store.WriteEngram(ctx, ws, eng)
	require.NoError(t, err)

	require.NoError(t, store.UpsertRelationshipRecord(ctx, ws, eng.ID, RelationshipRecord{
		FromEntity: "payment-service",
		ToEntity:   "Postgre SQL",
		RelType:    "uses",
		Weight:     0.8,
		Source:     "test",
	}))

	require.NoError(t, store.RelinkRelationshipEntity(ctx, ws, "Postgre SQL", "PostgreSQL"))

	var rels []RelationshipRecord
	require.NoError(t, store.ScanEntityRelationships(ctx, ws, "PostgreSQL",
		func(r RelationshipRecord) error {
			rels = append(rels, r)
			return nil
		}))
	require.Len(t, rels, 1)
	assert.Equal(t, "payment-service", rels[0].FromEntity)
	assert.Equal(t, "PostgreSQL", rels[0].ToEntity, "ToEntity must be renamed")
}

// TestRelinkRelationshipEntity_NoopOnMissing verifies that calling RelinkRelationshipEntity
// for an entity that has no relationships returns nil without error.
func TestRelinkRelationshipEntity_NoopOnMissing(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("entity-test")
	ws = store.VaultPrefix("relink-rel-noop")
	require.NoError(t, store.RelinkRelationshipEntity(ctx, ws, "NonExistent", "Target"))
}

// TestRelinkRelationshipEntity_IdempotentOnRepeat verifies that calling RelinkRelationshipEntity
// twice produces the same correct final state.
func TestRelinkRelationshipEntity_IdempotentOnRepeat(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("entity-test")
	ws = store.VaultPrefix("relink-rel-idempotent")

	eng := makeTestEngram("idempotent relink rel")
	_, err := store.WriteEngram(ctx, ws, eng)
	require.NoError(t, err)

	require.NoError(t, store.UpsertRelationshipRecord(ctx, ws, eng.ID, RelationshipRecord{
		FromEntity: "Postgre SQL",
		ToEntity:   "Redis",
		RelType:    "uses",
		Weight:     0.7,
		Source:     "test",
	}))

	require.NoError(t, store.RelinkRelationshipEntity(ctx, ws, "Postgre SQL", "PostgreSQL"))
	// Second call — must not error; old-hash entries are already gone, new-hash entries idempotently set.
	require.NoError(t, store.RelinkRelationshipEntity(ctx, ws, "Postgre SQL", "PostgreSQL"))

	var rels []RelationshipRecord
	require.NoError(t, store.ScanEntityRelationships(ctx, ws, "PostgreSQL",
		func(r RelationshipRecord) error {
			rels = append(rels, r)
			return nil
		}))
	require.Len(t, rels, 1)
	assert.Equal(t, "PostgreSQL", rels[0].FromEntity)
}

// ---------------------------------------------------------------------------
// ScanEntityRelationships — filtering
// ---------------------------------------------------------------------------

func TestScanEntityRelationships_ReturnsOnlyEntityRelationships(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("entity-test")
	ws = store.VaultPrefix("rel-index-filter")

	eng1 := makeTestEngram("engram one")
	eng2 := makeTestEngram("engram two")
	_, err := store.WriteEngram(ctx, ws, eng1)
	require.NoError(t, err)
	_, err = store.WriteEngram(ctx, ws, eng2)
	require.NoError(t, err)

	// eng1 has payment-service → PostgreSQL
	require.NoError(t, store.UpsertRelationshipRecord(ctx, ws, eng1.ID, RelationshipRecord{
		FromEntity: "payment-service",
		ToEntity:   "PostgreSQL",
		RelType:    "uses",
		Weight:     0.9,
		Source:     "test",
	}))
	// eng2 has auth-service → Redis (unrelated to PostgreSQL)
	require.NoError(t, store.UpsertRelationshipRecord(ctx, ws, eng2.ID, RelationshipRecord{
		FromEntity: "auth-service",
		ToEntity:   "Redis",
		RelType:    "uses",
		Weight:     0.8,
		Source:     "test",
	}))

	var rels []RelationshipRecord
	require.NoError(t, store.ScanEntityRelationships(ctx, ws, "PostgreSQL",
		func(r RelationshipRecord) error {
			rels = append(rels, r)
			return nil
		}))

	require.Len(t, rels, 1, "must only return relationships involving PostgreSQL")
	assert.Equal(t, "PostgreSQL", rels[0].ToEntity)
	assert.NotEqual(t, "Redis", rels[0].ToEntity, "unrelated Redis relationship must not appear")
}

func TestScanEntityRelationships_BothDirections(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("entity-test")
	ws = store.VaultPrefix("rel-index-directions")

	eng1 := makeTestEngram("from direction")
	eng2 := makeTestEngram("to direction")
	_, err := store.WriteEngram(ctx, ws, eng1)
	require.NoError(t, err)
	_, err = store.WriteEngram(ctx, ws, eng2)
	require.NoError(t, err)

	// eng1: PostgreSQL is fromEntity
	require.NoError(t, store.UpsertRelationshipRecord(ctx, ws, eng1.ID, RelationshipRecord{
		FromEntity: "PostgreSQL",
		ToEntity:   "payment-service",
		RelType:    "used_by",
		Weight:     0.8,
		Source:     "test",
	}))
	// eng2: PostgreSQL is toEntity
	require.NoError(t, store.UpsertRelationshipRecord(ctx, ws, eng2.ID, RelationshipRecord{
		FromEntity: "auth-service",
		ToEntity:   "PostgreSQL",
		RelType:    "uses",
		Weight:     0.9,
		Source:     "test",
	}))

	var rels []RelationshipRecord
	require.NoError(t, store.ScanEntityRelationships(ctx, ws, "PostgreSQL",
		func(r RelationshipRecord) error {
			rels = append(rels, r)
			return nil
		}))

	require.Len(t, rels, 2, "must find records where entity is either from or to")
}

func TestScanEntityRelationships_NoopOnMissing(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("entity-test")
	ws = store.VaultPrefix("rel-index-noop")

	var rels []RelationshipRecord
	require.NoError(t, store.ScanEntityRelationships(ctx, ws, "NonExistent",
		func(r RelationshipRecord) error {
			rels = append(rels, r)
			return nil
		}))
	assert.Empty(t, rels, "must return empty for entity with no relationships")
}

// ---------------------------------------------------------------------------
// DeleteEngram — 0x26 index cleanup
// ---------------------------------------------------------------------------

func TestDeleteEngram_CleansRelEntityIndex(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("entity-test")
	ws = store.VaultPrefix("rel-index-cleanup")

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

	// Verify 0x26 index is populated before delete.
	var before []RelationshipRecord
	require.NoError(t, store.ScanEntityRelationships(ctx, ws, "PostgreSQL",
		func(r RelationshipRecord) error {
			before = append(before, r)
			return nil
		}))
	require.Len(t, before, 1, "0x26 index must be populated before delete")

	require.NoError(t, store.DeleteEngram(ctx, ws, eng.ID))

	// After hard delete: 0x26 entries must be gone.
	var after []RelationshipRecord
	require.NoError(t, store.ScanEntityRelationships(ctx, ws, "PostgreSQL",
		func(r RelationshipRecord) error {
			after = append(after, r)
			return nil
		}))
	assert.Empty(t, after, "0x26 relationship entity index must be cleaned up after DeleteEngram")

	// Also verify from-entity side.
	var fromAfter []RelationshipRecord
	require.NoError(t, store.ScanEntityRelationships(ctx, ws, "payment-service",
		func(r RelationshipRecord) error {
			fromAfter = append(fromAfter, r)
			return nil
		}))
	assert.Empty(t, fromAfter, "0x26 from-entity index must also be cleaned up after DeleteEngram")
}

// ---------------------------------------------------------------------------
// DeleteEntityEngramLink
// ---------------------------------------------------------------------------

func TestDeleteEntityEngramLink_RemovesForwardAndReverseKeys(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("entity-test")
	ws = store.VaultPrefix("delete-entity-link")

	require.NoError(t, store.UpsertEntityRecord(ctx, ws, EntityRecord{
		Name: "PostgreSQL", Type: "database", Confidence: 0.9,
	}, "test"))

	eng := makeTestEngram("test engram")
	_, err := store.WriteEngram(ctx, ws, eng)
	require.NoError(t, err)
	require.NoError(t, store.WriteEntityEngramLink(ctx, ws, eng.ID, "PostgreSQL"))

	// Verify reverse link exists before delete.
	var before []ULID
	require.NoError(t, store.ScanEntityEngrams(ctx, "PostgreSQL", func(_ [8]byte, id ULID) error {
		before = append(before, id)
		return nil
	}))
	require.Len(t, before, 1)

	require.NoError(t, store.DeleteEntityEngramLink(ctx, ws, eng.ID, "PostgreSQL"))

	// Reverse link must be gone.
	var after []ULID
	require.NoError(t, store.ScanEntityEngrams(ctx, "PostgreSQL", func(_ [8]byte, id ULID) error {
		after = append(after, id)
		return nil
	}))
	assert.Empty(t, after, "0x23 reverse link must be removed by DeleteEntityEngramLink")

	// Forward link must also be gone.
	var entities []string
	require.NoError(t, store.ScanEngramEntities(ctx, ws, eng.ID, func(name string) error {
		entities = append(entities, name)
		return nil
	}))
	assert.Empty(t, entities, "0x20 forward link must be removed by DeleteEntityEngramLink")
}

func TestDeleteEntityEngramLink_NoopOnMissing(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("entity-test")
	ws = store.VaultPrefix("delete-entity-link-noop")

	id := NewULID()
	// Must not error on a link that doesn't exist.
	require.NoError(t, store.DeleteEntityEngramLink(ctx, ws, id, "NonExistent"))
}

// ---------------------------------------------------------------------------
// RelinkEntityEngramLink
// ---------------------------------------------------------------------------

// TestRelinkEntityEngramLink_AtomicMoveWritesBAndDeletesA verifies that a single
// RelinkEntityEngramLink call correctly writes the 0x20/0x23 links for toEntity
// and removes the 0x20/0x23 links for fromEntity — all visible as one atomic change.
func TestRelinkEntityEngramLink_AtomicMoveWritesBAndDeletesA(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("entity-test")
	ws = store.VaultPrefix("relink-entity-link")

	require.NoError(t, store.UpsertEntityRecord(ctx, ws, EntityRecord{
		Name: "Postgre SQL", Type: "database", Confidence: 0.8,
	}, "test"))
	require.NoError(t, store.UpsertEntityRecord(ctx, ws, EntityRecord{
		Name: "PostgreSQL", Type: "database", Confidence: 0.9,
	}, "test"))

	eng := makeTestEngram("relink test engram")
	_, err := store.WriteEngram(ctx, ws, eng)
	require.NoError(t, err)
	require.NoError(t, store.WriteEntityEngramLink(ctx, ws, eng.ID, "Postgre SQL"))

	// Baseline: A has one reverse link, B has none.
	var beforeA, beforeB []ULID
	require.NoError(t, store.ScanEntityEngrams(ctx, "Postgre SQL", func(_ [8]byte, id ULID) error {
		beforeA = append(beforeA, id)
		return nil
	}))
	require.NoError(t, store.ScanEntityEngrams(ctx, "PostgreSQL", func(_ [8]byte, id ULID) error {
		beforeB = append(beforeB, id)
		return nil
	}))
	require.Len(t, beforeA, 1, "entity A must have one reverse link before relink")
	require.Empty(t, beforeB, "entity B must have no reverse links before relink")

	// Relink atomically.
	require.NoError(t, store.RelinkEntityEngramLink(ctx, ws, eng.ID, "Postgre SQL", "PostgreSQL"))

	// After relink: A's 0x23 reverse link must be gone.
	var afterA []ULID
	require.NoError(t, store.ScanEntityEngrams(ctx, "Postgre SQL", func(_ [8]byte, id ULID) error {
		afterA = append(afterA, id)
		return nil
	}))
	assert.Empty(t, afterA, "entity A 0x23 reverse link must be removed by RelinkEntityEngramLink")

	// After relink: B's 0x23 reverse link must be present.
	var afterB []ULID
	require.NoError(t, store.ScanEntityEngrams(ctx, "PostgreSQL", func(_ [8]byte, id ULID) error {
		afterB = append(afterB, id)
		return nil
	}))
	require.Len(t, afterB, 1, "entity B must have one reverse link after relink")

	// After relink: forward index must show B, not A.
	var entities []string
	require.NoError(t, store.ScanEngramEntities(ctx, ws, eng.ID, func(name string) error {
		entities = append(entities, name)
		return nil
	}))
	assert.Contains(t, entities, "PostgreSQL", "0x20 forward link for B must exist after relink")
	assert.NotContains(t, entities, "Postgre SQL", "0x20 forward link for A must be gone after relink")
}

// TestRelinkEntityEngramLink_IdempotentOnRepeat verifies that calling RelinkEntityEngramLink
// twice (e.g. after a crash-restart) produces the same correct state.
func TestRelinkEntityEngramLink_IdempotentOnRepeat(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("entity-test")
	ws = store.VaultPrefix("relink-entity-link-idempotent")

	require.NoError(t, store.UpsertEntityRecord(ctx, ws, EntityRecord{
		Name: "Postgre SQL", Type: "database", Confidence: 0.8,
	}, "test"))
	require.NoError(t, store.UpsertEntityRecord(ctx, ws, EntityRecord{
		Name: "PostgreSQL", Type: "database", Confidence: 0.9,
	}, "test"))

	eng := makeTestEngram("idempotent relink engram")
	_, err := store.WriteEngram(ctx, ws, eng)
	require.NoError(t, err)
	require.NoError(t, store.WriteEntityEngramLink(ctx, ws, eng.ID, "Postgre SQL"))

	// First relink.
	require.NoError(t, store.RelinkEntityEngramLink(ctx, ws, eng.ID, "Postgre SQL", "PostgreSQL"))
	// Second relink — must not error and must leave state identical.
	require.NoError(t, store.RelinkEntityEngramLink(ctx, ws, eng.ID, "Postgre SQL", "PostgreSQL"))

	var afterA []ULID
	require.NoError(t, store.ScanEntityEngrams(ctx, "Postgre SQL", func(_ [8]byte, id ULID) error {
		afterA = append(afterA, id)
		return nil
	}))
	assert.Empty(t, afterA, "entity A must have no reverse links after repeated relink")

	var afterB []ULID
	require.NoError(t, store.ScanEntityEngrams(ctx, "PostgreSQL", func(_ [8]byte, id ULID) error {
		afterB = append(afterB, id)
		return nil
	}))
	require.Len(t, afterB, 1, "entity B must have exactly one reverse link after repeated relink")
}
