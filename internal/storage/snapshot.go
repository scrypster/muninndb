package storage

import (
	"context"

	"github.com/cockroachdb/pebble"
)

type snapshotCtxKey struct{}

// snapshotView is the ctx value for a pinned snapshot. fillToken is the
// cache fill token taken BEFORE the snapshot was created (cache_guard.go,
// R1) — the freshness lower bound for every value read through it. A view
// built by ContextWithSnapshot has no such bound (noFillToken): its reads are
// served but never cached.
type snapshotView struct {
	snap      *pebble.Snapshot
	fillToken uint64
}

// ContextWithSnapshot returns a context carrying a Pebble snapshot.
// PebbleStore read methods (GetEngrams, GetMetadata, GetAssociations,
// RecentActive, EngramIDsByCreatedRange) use this snapshot for Pebble
// reads instead of the live DB, providing point-in-time consistency
// across the full activation pipeline.
//
// Reads through a snapshot attached this way do NOT fill the caches: the
// store cannot know which writes the snapshot predates. Use
// PebbleStore.NewSnapshotContext, which records that bound.
func ContextWithSnapshot(ctx context.Context, snap *pebble.Snapshot) context.Context {
	return context.WithValue(ctx, snapshotCtxKey{}, &snapshotView{snap: snap, fillToken: noFillToken})
}

func snapshotFromContext(ctx context.Context) *snapshotView {
	if v, ok := ctx.Value(snapshotCtxKey{}).(*snapshotView); ok && v != nil && v.snap != nil {
		return v
	}
	return nil
}

// pebbleReader returns the snapshot from ctx if present, otherwise the live DB.
func (ps *PebbleStore) pebbleReader(ctx context.Context) pebble.Reader {
	if v := snapshotFromContext(ctx); v != nil {
		return v.snap
	}
	return ps.db
}

// readFillToken is R1 for a read about to go through pebbleReader(ctx): the
// snapshot's recorded token, or — for a live read — the current token. It
// MUST be called before the Get/NewIter it guards.
func (ps *PebbleStore) readFillToken(ctx context.Context) uint64 {
	if v := snapshotFromContext(ctx); v != nil {
		return v.fillToken
	}
	return ps.cache.guard.token()
}

// NewSnapshot creates a Pebble snapshot for point-in-time reads.
// Caller must close the returned snapshot when done.
func (ps *PebbleStore) NewSnapshot() *pebble.Snapshot {
	return ps.db.NewSnapshot()
}

// NewSnapshotContext pins a Pebble snapshot and returns a ctx carrying it,
// plus the snapshot itself (the caller must Close it). This is the read
// pipeline's entry point for point-in-time reads: the fill token is taken
// BEFORE the snapshot is created, so values read through it may be cached
// and a write committed after the pin can never be overwritten by them.
func (ps *PebbleStore) NewSnapshotContext(ctx context.Context) (context.Context, *pebble.Snapshot) {
	tok := ps.cache.guard.token()
	testSnapshotPinPoint() // test seam: a write landing between token and pin
	snap := ps.db.NewSnapshot()
	return context.WithValue(ctx, snapshotCtxKey{}, &snapshotView{snap: snap, fillToken: tok}), snap
}
