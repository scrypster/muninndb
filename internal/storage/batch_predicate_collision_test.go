package storage

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestBatchWriteRelationshipRecord_DistinctUnmappedPredicatesCoexist covers the
// batch twin of PebbleStore.UpsertRelationshipRecord on the #894 collision.
// The twin is live through Engine.evolveAtInternal's carry (engine.go) and the
// startup repair (evolve_repair.go) — both re-write an engram's relationships
// when evolving to a successor, so a fold-to-one-key there would destroy a
// carried relationship on every evolve.
func TestBatchWriteRelationshipRecord_DistinctUnmappedPredicatesCoexist(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("predicate-collision-batch")
	engramID := NewULID()

	b := store.NewBatch()
	pb, ok := b.(*pebbleStoreBatch)
	require.True(t, ok, "NewBatch must return the concrete pebbleStoreBatch")
	defer pb.Discard()

	require.NoError(t, pb.WriteRelationshipRecord(ctx, ws, engramID, RelationshipRecord{
		FromEntity: "Aurora Platform",
		ToEntity:   "Kepler Cache",
		RelType:    "communicates_with",
		Weight:     0.9,
		Source:     "plugin:enrich",
	}))
	require.NoError(t, pb.WriteRelationshipRecord(ctx, ws, engramID, RelationshipRecord{
		FromEntity: "Aurora Platform",
		ToEntity:   "Kepler Cache",
		RelType:    "attributed_to",
		Weight:     0.7,
		Source:     "plugin:enrich",
	}))
	require.NoError(t, b.Commit())

	var recs []RelationshipRecord
	require.NoError(t, store.ScanEngramRelationships(ctx, ws, engramID, func(rec RelationshipRecord) error {
		recs = append(recs, rec)
		return nil
	}))

	byType := make(map[string]RelationshipRecord, len(recs))
	for _, r := range recs {
		byType[r.RelType] = r
	}
	assert.Equal(t, 2, len(recs),
		"batch-written unmapped predicates must coexist, got %d record(s)", len(recs))
	if c, ok := byType["communicates_with"]; ok {
		assert.InDelta(t, 0.9, c.Weight, 0.001)
	} else {
		t.Error("communicates_with record was destroyed by the attributed_to batch write")
	}
	if a, ok := byType["attributed_to"]; ok {
		assert.InDelta(t, 0.7, a.Weight, 0.001)
	} else {
		t.Error("attributed_to record missing")
	}
}
