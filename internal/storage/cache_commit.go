package storage

import (
	"bytes"
	"context"
	"sync"

	"github.com/cockroachdb/pebble"
	"github.com/scrypster/muninndb/internal/prefix"
	"github.com/scrypster/muninndb/internal/storage/erf"
	"github.com/scrypster/muninndb/internal/storage/keys"
)

// commitBatch is the ONLY way PebbleStore commits a Pebble batch. After the
// commit it walks the batch and, for every key that backs a read-through
// cache, bumps the fill guard and drops the cached entry (cache_guard.go,
// W2+W3). Putting this at the commit — rather than at each write method —
// means a write path cannot forget it: TestNoRawBatchCommitsInStorage fails
// the build of any storage file that calls Commit on a raw batch.
//
// The walk runs even when Commit returns an error: invalidating an entry is
// always safe, and a failed commit's effect on Pebble is not ours to assume.
func (ps *PebbleStore) commitBatch(b *pebble.Batch, opts *pebble.WriteOptions) error {
	err := b.Commit(opts)
	if f, ok := commitResultForTest.Load(ps); ok {
		err = f.(func(error) error)(err)
	}
	ps.invalidateCommittedRepr(b.Repr())
	return err
}

// commitResultForTest is a TEST-ONLY seam, keyed by store so parallel tests
// on other stores are untouched: an entry rewrites that store's Commit
// result, so a test can make a commit that APPLIED report an error and prove
// the invalidation above still runs. Production never stores into it.
var commitResultForTest sync.Map // *PebbleStore -> func(error) error

// InvalidateCommittedRepr applies commitBatch's invalidation to a batch that
// was committed to this store's DB by someone else — the replication applier
// on a follower, which writes the leader's batch reprs straight into Pebble.
func (ps *PebbleStore) InvalidateCommittedRepr(repr []byte) { ps.invalidateCommittedRepr(repr) }

// InvalidateAllCaches drops every read-through cache after bumping every
// guard stripe — for a wholesale replacement of the DB contents (a follower
// installing a snapshot from the leader).
func (ps *PebbleStore) InvalidateAllCaches() {
	ps.cache.InvalidateAll()
	ps.metaCache.Purge()
	ps.assocCache.Purge()
	ps.revAssocCache.Purge()
}

func (ps *PebbleStore) invalidateCommittedRepr(repr []byte) {
	if len(repr) == 0 {
		return
	}
	r, _ := pebble.ReadBatch(repr)
	for {
		kind, k, v, ok, err := r.Next()
		if err != nil {
			// A corrupt repr cannot be walked: fall back to everything.
			ps.InvalidateAllCaches()
			return
		}
		if !ok {
			return
		}
		switch kind {
		case pebble.InternalKeyKindRangeDelete:
			ps.invalidateRange(k, v)
		case pebble.InternalKeyKindLogData,
			pebble.InternalKeyKindRangeKeySet, pebble.InternalKeyKindRangeKeyUnset,
			pebble.InternalKeyKindRangeKeyDelete:
			// No point key, and no cache is derived from range keys.
		default:
			ps.invalidateKey(k)
		}
	}
}

// invalidateKey maps one written key to the cache entries derived from it.
//
//	0x01|ws|id, 0x02|ws|id            → L1 engram cache, metaCache
//	0x03|ws|src|wc|dst, 0x04|ws|dst|wc|src → assocCache(src), revAssocCache(dst)
func (ps *PebbleStore) invalidateKey(k []byte) {
	if len(k) < 25 {
		return
	}
	var ws [8]byte
	copy(ws[:], k[1:9])
	switch k[0] {
	case prefix.Engram, prefix.Meta:
		var id [16]byte
		copy(id[:], k[9:25])
		ps.cache.Invalidate(ws, ULID(id)) // bumps before it deletes
		ps.metaCache.Remove(id)
	case prefix.AssocFwd, prefix.AssocRev:
		if len(k) < 45 {
			return
		}
		var a, b [16]byte
		copy(a[:], k[9:25])
		copy(b[:], k[29:45])
		src, dst := a, b
		if k[0] == prefix.AssocRev {
			src, dst = b, a
		}
		g := &ps.cache.guard
		g.bumpAssoc(src)
		g.bumpAssoc(dst)
		ps.assocCache.Remove(assocCacheKey(ws, ULID(src)))
		ps.revAssocCache.Remove(assocCacheKey(ws, ULID(dst)))
	}
}

// invalidateRange handles a range tombstone. Only vault clears write one over
// a cache-backing prefix; they are rare, so the response is wholesale: every
// stripe is bumped and the derived caches are dropped (L1 only for the
// vault when the range is vault-scoped).
func (ps *PebbleStore) invalidateRange(start, end []byte) {
	lo, hi := []byte{prefix.Engram}, []byte{prefix.AssocRev + 1}
	if bytes.Compare(start, hi) >= 0 || (len(end) > 0 && bytes.Compare(end, lo) <= 0) {
		return // entirely outside 0x01..0x04
	}
	ps.cache.guard.bumpAll()
	if len(start) >= 9 && start[0] >= prefix.Engram && start[0] <= prefix.AssocRev {
		var ws [8]byte
		copy(ws[:], start[1:9])
		ps.cache.DeleteByVault(ws)
	} else {
		ps.cache.InvalidateAll()
	}
	ps.metaCache.Purge()
	ps.assocCache.Purge()
	ps.revAssocCache.Purge()
}

// fillMeta installs meta in metaCache under the guard (R3/R4). metaCache has
// no compare-and-delete, so R4 removes unconditionally — dropping a newer
// entry there is only a miss.
func (ps *PebbleStore) fillMeta(id [16]byte, meta *EngramMeta, tok uint64) {
	g := &ps.cache.guard
	if !g.engramClean(id, tok) {
		return
	}
	ps.metaCache.Add(id, meta)
	if !g.engramClean(id, tok) {
		ps.metaCache.Remove(id)
	}
}

func (ps *PebbleStore) fillAssoc(ws [8]byte, id ULID, e *assocCacheEntry, tok uint64) {
	g := &ps.cache.guard
	if !g.assocClean(id, tok) {
		return
	}
	ck := assocCacheKey(ws, id)
	ps.assocCache.Add(ck, e)
	if !g.assocClean(id, tok) {
		ps.assocCache.Remove(ck)
	}
}

func (ps *PebbleStore) fillRevAssoc(ws [8]byte, id ULID, e *revAssocCacheEntry, tok uint64) {
	g := &ps.cache.guard
	if !g.assocClean(id, tok) {
		return
	}
	ck := assocCacheKey(ws, id)
	ps.revAssocCache.Add(ck, e)
	if !g.assocClean(id, tok) {
		ps.revAssocCache.Remove(ck)
	}
}

// recacheCommitted re-reads an engram a writer just committed and caches it
// (stamped), the way the writers used to cache the struct they had built.
// They cannot cache that struct safely: it was derived from a read taken
// before their own bump, so no token both admits it and excludes a racing
// unlocked writer that committed after it. A fresh read under a fresh token
// is admissible by construction. Best effort: a failure just leaves a miss.
func (ps *PebbleStore) recacheCommitted(_ context.Context, ws [8]byte, id ULID) {
	tok := ps.cache.guard.token()
	raw, err := Get(ps.db, keys.EngramKey(ws, [16]byte(id)))
	if err != nil || raw == nil {
		return
	}
	dec, err := erf.Decode(raw)
	if err != nil {
		return
	}
	ps.cache.fill(ws, id, fromERFEngram(dec), tok, true)
}
