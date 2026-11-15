package storage

import (
	"context"

	"github.com/cockroachdb/pebble"
)

type snapshotCtxKey struct{}

// ContextWithSnapshot returns a context carrying a Pebble snapshot.
// PebbleStore read methods (GetEngrams, GetMetadata, GetAssociations,
// RecentActive, EngramIDsByCreatedRange) use this snapshot for Pebble
// reads instead of the live DB, providing point-in-time consistency
// across the full activation pipeline.
func ContextWithSnapshot(ctx context.Context, snap *pebble.Snapshot) context.Context {
	return context.WithValue(ctx, snapshotCtxKey{}, snap)
}

// pebbleReader returns the snapshot from ctx if present, otherwise the live DB.
func (ps *PebbleStore) pebbleReader(ctx context.Context) pebble.Reader {
	if snap, ok := ctx.Value(snapshotCtxKey{}).(*pebble.Snapshot); ok && snap != nil {
		return snap
	}
	return ps.db
}

// NewSnapshot creates a Pebble snapshot for point-in-time reads.
// Caller must close the returned snapshot when done.
func (ps *PebbleStore) NewSnapshot() *pebble.Snapshot {
	return ps.db.NewSnapshot()
}

// NewSnapshotContext pins a Pebble snapshot and returns a ctx carrying it,
// plus the snapshot itself (the caller must Close it). This is the read
// pipeline's entry point for point-in-time reads; prefer it over the
// NewSnapshot + ContextWithSnapshot pair.
func (ps *PebbleStore) NewSnapshotContext(ctx context.Context) (context.Context, *pebble.Snapshot) {
	snap := ps.db.NewSnapshot()
	return ContextWithSnapshot(ctx, snap), snap
}
