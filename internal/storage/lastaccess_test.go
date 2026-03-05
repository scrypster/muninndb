package storage

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestPebbleStore(t *testing.T) *PebbleStore {
	t.Helper()
	return openTestStore(t)
}

func TestWriteLastAccessEntry_AndScan(t *testing.T) {
	ps := newTestPebbleStore(t)
	ctx := context.Background()
	ws := ps.VaultPrefix("test")
	id := NewULID()
	millis := int64(1000000)

	require.NoError(t, ps.WriteLastAccessEntry(ctx, ws, id, 0, millis))

	var found []ULID
	require.NoError(t, ps.ScanLastAccessDesc(ctx, ws, func(gotID ULID, _ int64) error {
		found = append(found, gotID)
		return nil
	}))
	require.Len(t, found, 1)
	assert.Equal(t, id, found[0])
}

func TestScanLastAccessDesc_DescendingOrder(t *testing.T) {
	ps := newTestPebbleStore(t)
	ctx := context.Background()
	ws := ps.VaultPrefix("test")

	id1, id2, id3 := NewULID(), NewULID(), NewULID()
	base := int64(1000000)
	require.NoError(t, ps.WriteLastAccessEntry(ctx, ws, id1, 0, base+1000)) // middle
	require.NoError(t, ps.WriteLastAccessEntry(ctx, ws, id2, 0, base+2000)) // newest
	require.NoError(t, ps.WriteLastAccessEntry(ctx, ws, id3, 0, base))      // oldest

	var order []ULID
	require.NoError(t, ps.ScanLastAccessDesc(ctx, ws, func(id ULID, _ int64) error {
		order = append(order, id)
		return nil
	}))
	require.Len(t, order, 3)
	assert.Equal(t, id2, order[0], "newest first")
	assert.Equal(t, id1, order[1])
	assert.Equal(t, id3, order[2], "oldest last")
}

func TestWriteLastAccessEntry_UpdateMovesKey(t *testing.T) {
	ps := newTestPebbleStore(t)
	ctx := context.Background()
	ws := ps.VaultPrefix("test")
	id := NewULID()

	require.NoError(t, ps.WriteLastAccessEntry(ctx, ws, id, 0, 1000))
	// Update — old key removed, new key written.
	require.NoError(t, ps.WriteLastAccessEntry(ctx, ws, id, 1000, 5000))

	var results []ULID
	var millisSeen []int64
	require.NoError(t, ps.ScanLastAccessDesc(ctx, ws, func(gotID ULID, millis int64) error {
		results = append(results, gotID)
		millisSeen = append(millisSeen, millis)
		return nil
	}))
	assert.Len(t, results, 1, "only one entry per engram")
	assert.Equal(t, int64(5000), millisSeen[0], "only new millis should appear")
}
