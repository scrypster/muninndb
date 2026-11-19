package storage

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// collectEngramRels scans all relationship records sourced from engramID.
func collectEngramRels(t *testing.T, store *PebbleStore, ws [8]byte, engramID ULID) []RelationshipRecord {
	t.Helper()
	var recs []RelationshipRecord
	err := store.ScanEngramRelationships(context.Background(), ws, engramID, func(rec RelationshipRecord) error {
		recs = append(recs, rec)
		return nil
	})
	require.NoError(t, err)
	return recs
}

// TestUpsertRelationshipRecord_DistinctUnmappedPredicatesCoexist is the #894
// regression: two predicates outside the 11-entry relTypeBytes vocabulary,
// asserted from one engram about one entity pair, used to fold to the same
// 0xFF key byte — the second write silently destroyed the first. Post-fix the
// predicate component is PredicateHash(relType), so every distinct predicate
// string gets its own key.
func TestUpsertRelationshipRecord_DistinctUnmappedPredicatesCoexist(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("predicate-collision")
	engramID := NewULID()

	// "communicates_with" and "attributed_to" are both outside relTypeBytes,
	// and both appear in the enrich plugin's default prompt vocabulary.
	require.NoError(t, store.UpsertRelationshipRecord(ctx, ws, engramID, RelationshipRecord{
		FromEntity: "Aurora Platform",
		ToEntity:   "Kepler Cache",
		RelType:    "communicates_with",
		Weight:     0.9,
		Source:     "plugin:enrich",
	}))
	require.NoError(t, store.UpsertRelationshipRecord(ctx, ws, engramID, RelationshipRecord{
		FromEntity: "Aurora Platform",
		ToEntity:   "Kepler Cache",
		RelType:    "attributed_to",
		Weight:     0.7,
		Source:     "plugin:enrich",
	}))

	recs := collectEngramRels(t, store, ws, engramID)
	byType := make(map[string]RelationshipRecord, len(recs))
	for _, r := range recs {
		byType[r.RelType] = r
	}

	assert.Equal(t, 2, len(recs),
		"two distinct unmapped predicates on one entity pair must coexist, got %d record(s)", len(recs))
	if c, ok := byType["communicates_with"]; ok {
		assert.InDelta(t, 0.9, c.Weight, 0.001, "communicates_with weight must survive")
	} else {
		t.Error("communicates_with record was destroyed by the attributed_to write")
	}
	if a, ok := byType["attributed_to"]; ok {
		assert.InDelta(t, 0.7, a.Weight, 0.001, "attributed_to weight must survive")
	} else {
		t.Error("attributed_to record missing")
	}
}

// Control: mapped predicates already got distinct key bytes and must keep
// distinct keys after the switch to PredicateHash. This test must pass both
// before and after the fix — it guards against overcorrection, not the bug.
func TestUpsertRelationshipRecord_MappedPredicatesCoexist(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("predicate-collision")
	engramID := NewULID()

	require.NoError(t, store.UpsertRelationshipRecord(ctx, ws, engramID, RelationshipRecord{
		FromEntity: "Aurora Platform",
		ToEntity:   "Kepler Cache",
		RelType:    "uses",
		Weight:     0.8,
		Source:     "inline",
	}))
	require.NoError(t, store.UpsertRelationshipRecord(ctx, ws, engramID, RelationshipRecord{
		FromEntity: "Aurora Platform",
		ToEntity:   "Kepler Cache",
		RelType:    "part_of",
		Weight:     0.6,
		Source:     "inline",
	}))

	recs := collectEngramRels(t, store, ws, engramID)
	assert.Equal(t, 2, len(recs), "two mapped predicates on one entity pair must coexist")
}

// Control: re-asserting the SAME predicate must stay one record (upsert
// semantics, last write wins). Must pass both before and after the fix.
func TestUpsertRelationshipRecord_SamePredicateUpsertsInPlace(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("predicate-collision")
	engramID := NewULID()

	require.NoError(t, store.UpsertRelationshipRecord(ctx, ws, engramID, RelationshipRecord{
		FromEntity: "Aurora Platform",
		ToEntity:   "Kepler Cache",
		RelType:    "deployed_on",
		Weight:     0.5,
		Source:     "inline",
	}))
	require.NoError(t, store.UpsertRelationshipRecord(ctx, ws, engramID, RelationshipRecord{
		FromEntity: "Aurora Platform",
		ToEntity:   "Kepler Cache",
		RelType:    "deployed_on",
		Weight:     0.95,
		Source:     "plugin:enrich",
	}))

	recs := collectEngramRels(t, store, ws, engramID)
	if assert.Len(t, recs, 1, "re-asserting the same predicate must be an upsert, not a new record") {
		assert.InDelta(t, 0.95, recs[0].Weight, 0.001, "last write wins on upsert")
		assert.Equal(t, "plugin:enrich", recs[0].Source)
	}
}
