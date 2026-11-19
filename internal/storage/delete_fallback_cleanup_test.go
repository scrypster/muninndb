package storage

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/scrypster/muninndb/internal/storage/keys"
)

// TestDeleteEngram_FallbackCleansRelationshipAndEntityIndex pins that the
// key-only fallback DeleteEngram takes when GetEngram cannot read the record
// still removes the relationship row (0x21) and both relationship-entity index
// entries (0x26). Those are derivable from the engram ID alone, so the fallback
// has no excuse to leave them: they are orphaned forever, and a later engram
// reusing the same entity names inherits stale index rows.
//
// RED before the fix: the fallback deleted only the three primary keys and
// returned, leaving 0x21 = 1 and 0x26 = 2.
func TestDeleteEngram_FallbackCleansRelationshipAndEntityIndex(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("fallback-probe")

	eng := makeTestEngram("fallback probe")
	_, err := store.WriteEngram(ctx, ws, eng)
	require.NoError(t, err)
	require.NoError(t, store.UpsertRelationshipRecord(ctx, ws, eng.ID, RelationshipRecord{
		FromEntity: "payment-service", ToEntity: "PostgreSQL",
		RelType: "uses", Weight: 0.9, Source: "test",
	}))

	require.Equal(t, 2, countRelEntityIndexKeys(t, store, ws), "0x26 before")
	require.Equal(t, 1, countRawRelationshipKeys(t, store, ws), "0x21 before")

	// Force the fallback: remove the 0x01 record so GetEngram cannot read it.
	require.NoError(t, store.db.Delete(keys.EngramKey(ws, [16]byte(eng.ID)), nil))

	require.NoError(t, store.DeleteEngram(ctx, ws, eng.ID))

	require.Equal(t, 0, countRelEntityIndexKeys(t, store, ws), "0x26 must be cleaned even on the fallback path")
	require.Equal(t, 0, countRawRelationshipKeys(t, store, ws), "0x21 must be cleaned even on the fallback path")
}
