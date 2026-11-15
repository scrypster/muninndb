package storage

import (
	"encoding/hex"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// L1Cache is a hot cache for recently accessed engrams.
// Cache keys are scoped by vault prefix so different vaults cannot
// serve each other's engrams even when ULIDs overlap in memory.
// Uses sync.Map for lock-free concurrent reads (the common case).
//
// Fills are guarded (cache_guard.go): a value is installed only through fill,
// with the token its reader took before reading the store, so a value read
// before a committed write can never outlive that write's invalidation.
//
// count is exactly the number of live entries at quiescence: it moves only
// when a key goes absent→present (a Swap that loaded nothing) or
// present→absent (a LoadAndDelete/CompareAndDelete that removed something).
type L1Cache struct {
	data    sync.Map // string (vaultHex+":"+ULID) -> *cacheEntry
	count   atomic.Int64
	maxSize int
	guard   fillGuard
}

// cacheKeyFor returns the composite cache key for a (vault, id) pair.
// Format: 16-char hex vault prefix + ":" + ULID string = 17+26 = 43 chars.
func cacheKeyFor(ws [8]byte, id ULID) string {
	return hex.EncodeToString(ws[:]) + ":" + id.String()
}

// cacheEntry wraps an engram with access tracking.
type cacheEntry struct {
	eng        *Engram
	lastAccess atomic.Int64 // Unix nanoseconds
	// token is the fill token this entry was installed under: every write
	// whose bump is <= token is reflected in eng. A value DERIVED from this
	// entry (GetMetadata's metaCache promotion) is filled with this token.
	token uint64
}

// NewL1Cache creates a new L1 cache with the specified max size.
func NewL1Cache(maxSize int) *L1Cache {
	if maxSize <= 0 {
		maxSize = 10000 // default
	}
	return &L1Cache{
		maxSize: maxSize,
	}
}

// getEntry is Get returning the entry (for its token); stamp as Get does.
func (c *L1Cache) getEntry(ws [8]byte, id ULID, stamp bool) (*cacheEntry, bool) {
	val, ok := c.data.Load(cacheKeyFor(ws, id))
	if !ok {
		return nil, false
	}
	entry := val.(*cacheEntry)
	if stamp {
		entry.lastAccess.Store(time.Now().UnixNano())
	}
	return entry, true
}

// fill installs eng for (ws, id) iff no write to id has been bumped past tok
// (R3), then re-checks and withdraws its own entry if one has (R4). See
// fillGuard for the proof. stamp=false is the read-only path: lastAccess
// stays 0 and a full cache skips the fill instead of evicting.
func (c *L1Cache) fill(ws [8]byte, id ULID, eng *Engram, tok uint64, stamp bool) {
	if !c.guard.engramClean(id, tok) {
		return
	}
	if !stamp && c.count.Load() >= int64(c.maxSize) {
		return
	}
	entry := &cacheEntry{eng: eng, token: tok}
	if stamp {
		entry.lastAccess.Store(time.Now().UnixNano())
	}
	key := cacheKeyFor(ws, id)
	if _, loaded := c.data.Swap(key, entry); !loaded {
		if n := c.count.Add(1); stamp && n > int64(c.maxSize) {
			// Evict until back at capacity. One eviction per fill (the old
			// shape) never recovered from concurrent evictions that picked
			// the same victim — with an exact counter that overshoot would
			// persist. Stops if a pass removes nothing (another goroutine is
			// evicting the same sample).
			for c.count.Load() > int64(c.maxSize) && c.evict() {
			}
		}
	}
	if !c.guard.engramClean(id, tok) {
		if c.data.CompareAndDelete(key, entry) {
			c.count.Add(-1)
		}
	}
}

// Invalidate is W2+W3 for one engram: bump its stripe, then drop the entry.
// Writers reach it through PebbleStore.commitBatch, never directly.
func (c *L1Cache) Invalidate(ws [8]byte, id ULID) {
	c.guard.bumpEngram(id)
	c.Delete(ws, id)
}

// Get retrieves an engram from the cache.
// Returns (engram, found). The vault prefix is included in the lookup
// so engrams from different vaults are never served to each other.
//
// IMMUTABILITY CONTRACT: the returned *Engram is shared — the same pointer may
// be handed to concurrent readers (and is also what GetEngrams returns). Treat
// it as read-only. Callers that need to change an engram must deep-copy first
// (as the write path does before invoking triggers) or go through an Update*
// method, which persists the change and invalidates this entry. Mutating the
// returned struct in place is a data race against concurrent recalls.
func (c *L1Cache) Get(ws [8]byte, id ULID) (*Engram, bool) {
	val, ok := c.data.Load(cacheKeyFor(ws, id))
	if !ok {
		return nil, false
	}

	entry := val.(*cacheEntry)
	entry.lastAccess.Store(time.Now().UnixNano())
	return entry.eng, true
}

// Set stores an engram in the cache under the given vault prefix, asserting
// that eng is at least as new as every write committed so far (it fills with
// the current token). Production read paths never call it — they fill with
// the token taken BEFORE their store read (fill); TestNoUnguardedCacheFills
// pins that. Tests and callers holding a just-computed value use it.
func (c *L1Cache) Set(ws [8]byte, id ULID, eng *Engram) {
	c.fill(ws, id, eng, c.guard.token(), true)
}

// GetNoStamp is Get without the recency side effect: it returns the cached
// engram (if present) WITHOUT updating lastAccess. A scoring pass is not a
// user access — a ReadOnly Activate SCORES candidates it never emits, and
// LastAccessNs (below) feeds real ACT-R recency scoring in a LATER,
// unrelated recall. Stamping on every scoring pass made an aged, genuinely-
// never-accessed memory look "just accessed" to that later call purely
// because an earlier read-only pass glanced at it. Same immutability
// contract as Get.
func (c *L1Cache) GetNoStamp(ws [8]byte, id ULID) (*Engram, bool) {
	val, ok := c.data.Load(cacheKeyFor(ws, id))
	if !ok {
		return nil, false
	}
	return val.(*cacheEntry).eng, true
}

// SetNoStamp is Set without the recency side effect: the entry is cached
// (so a same-call re-read is still served without a Pebble round-trip) but
// its lastAccess is left at the zero value, so LastAccessNs reports 0 (not
// cached, in the recency sense) until a REAL access — one that goes through
// Get/Set — stamps it. See GetNoStamp's doc for why.
//
// It also never EVICTS: on a full cache the fill is skipped. Eviction removes
// a warm entry, and a warm entry's lastAccess is a live recency SCORING input
// (EngramLastAccessNs) for the next recall — so a suppressed read that evicted
// would still change what the following identical read scores. Skipping costs
// only a Pebble re-read of this engram later, the same as if it had been
// evicted. The capacity check is approximate under concurrency, like Set's.
//
// Same freshness assertion as Set; production uses fill.
func (c *L1Cache) SetNoStamp(ws [8]byte, id ULID, eng *Engram) {
	c.fill(ws, id, eng, c.guard.token(), false)
}

// Delete removes an engram from the cache for the given vault.
// Uses LoadAndDelete to avoid decrementing the counter when the key was already
// absent (e.g. due to concurrent eviction or a prior DeleteByVault call).
func (c *L1Cache) Delete(ws [8]byte, id ULID) {
	if _, loaded := c.data.LoadAndDelete(cacheKeyFor(ws, id)); loaded {
		c.count.Add(-1)
	}
}

// Len returns the number of entries in the cache (exact at quiescence).
func (c *L1Cache) Len() int {
	return int(c.count.Load())
}

// LastAccessNs returns the nanosecond Unix timestamp of the last cache access
// for the given (vault, engram) pair. Returns 0 if the engram is not cached.
func (c *L1Cache) LastAccessNs(ws [8]byte, id ULID) int64 {
	val, ok := c.data.Load(cacheKeyFor(ws, id))
	if !ok {
		return 0
	}
	return val.(*cacheEntry).lastAccess.Load()
}

// DeleteByVault removes all cache entries for the given vault workspace prefix.
// It is safe for concurrent use: LoadAndDelete is used atomically so that only
// the goroutine that successfully removes an entry decrements the counter,
// preventing count drift when concurrent deletes race on the same key.
func (c *L1Cache) DeleteByVault(ws [8]byte) {
	prefix := hex.EncodeToString(ws[:]) + ":"
	c.data.Range(func(k, _ any) bool {
		if strings.HasPrefix(k.(string), prefix) {
			if _, loaded := c.data.LoadAndDelete(k); loaded {
				c.count.Add(-1)
			}
		}
		return true
	})
}

// InvalidateVault is DeleteByVault for a committed range write: every stripe
// is bumped first, so a fill in flight from before the write is rejected.
func (c *L1Cache) InvalidateVault(ws [8]byte) {
	c.guard.bumpAll()
	c.DeleteByVault(ws)
}

// InvalidateAll drops every entry after bumping every stripe.
func (c *L1Cache) InvalidateAll() {
	c.guard.bumpAll()
	c.data.Range(func(k, _ any) bool {
		if _, loaded := c.data.LoadAndDelete(k); loaded {
			c.count.Add(-1)
		}
		return true
	})
}

// evict removes one entry using approximate LRU.
// Scans 64 random entries, evicts the one with oldest lastAccess.
func (c *L1Cache) evict() bool {
	const sampleSize = 64

	var oldest *cacheEntry
	var oldestID string
	var oldestTime int64 = time.Now().UnixNano()

	count := 0
	c.data.Range(func(key, val any) bool {
		if count >= sampleSize {
			return false
		}
		count++

		entry := val.(*cacheEntry)
		lastAccess := entry.lastAccess.Load()
		if lastAccess < oldestTime {
			oldestTime = lastAccess
			oldest = entry
			oldestID = key.(string)
		}

		return true
	})

	if oldest != nil && oldestID != "" {
		// LoadAndDelete is atomic: only the goroutine that successfully removes
		// the entry decrements the counter, preventing count drift when two
		// concurrent evictions pick the same victim.
		if _, loaded := c.data.LoadAndDelete(oldestID); loaded {
			c.count.Add(-1)
			return true
		}
	}
	return false
}
