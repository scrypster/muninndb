package storage

import (
	"github.com/scrypster/muninndb/internal/prefix"
)

// InvalidateReplicatedKey drops any in-memory cache entry shadowing a key that
// the replication applier just committed to Pebble (#869).
//
// On a follower, replicated writes bypass this store entirely: the applier
// holds a bare *pebble.DB and commits batch reprs straight to disk. Every read
// path here, though, is cache-first — GetEngram consults the L1 engram cache
// and GetMetadata consults metaCache before touching Pebble. So a replicated
// in-place mutation of an engram a follower had already read (soft-delete,
// evolve supersession, not_true_since invalidation, trust or tag changes,
// restore) landed in Pebble underneath a cache entry that kept serving the old
// state — and because every cache hit refreshes recency, the hotter the
// memory, the longer its mutation stayed invisible. A restart healed it only
// because the caches die with the process.
//
// This is the storage half of the fix: the applier feeds every user key it
// commits through a callback (wired in cmd/muninn/server.go), and this method
// maps those keys back to cache entries. Only two key shapes can shadow a
// cached read, and both are 25 bytes:
//
//	0x01 | ws(8) | id(16) — full engram record → L1 engram cache + metaCache
//	0x02 | ws(8) | id(16) — metadata record    → metaCache
//
// The metaCache is keyed by engram ID alone (ULIDs are globally unique), so
// the vault prefix only matters for the L1 delete. Every other prefix passes
// through untouched: association and index keys have their own caches with
// their own invalidation story (assocCache/revAssocCache carry a 2s TTL, so
// replicated association churn ages out on its own — see #818 for the known
// gaps there), and a follower applies constant Hebbian/decay traffic, so
// clearing anything more than the touched keys would make the caches useless.
//
// Safe for concurrent use: L1Cache.Delete and metaCache.Remove are both
// concurrency-safe, and the applier may call this while readers are active.
// A miss is a no-op — invalidating a key that was never cached costs a map
// lookup.
func (ps *PebbleStore) InvalidateReplicatedKey(key []byte) {
	if len(key) != 25 {
		return
	}
	switch key[0] {
	case prefix.Engram:
		var ws [8]byte
		copy(ws[:], key[1:9])
		var id [16]byte
		copy(id[:], key[9:25])
		ps.cache.Delete(ws, ULID(id))
		ps.metaCache.Remove(id)
	case prefix.Meta:
		var id [16]byte
		copy(id[:], key[9:25])
		ps.metaCache.Remove(id)
	}
}
