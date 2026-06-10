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
type L1Cache struct {
	data    sync.Map // string (vaultHex+":"+ULID) -> *cacheEntry
	count   atomic.Int64
	maxSize int
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

// Set stores an engram in the cache under the given vault prefix.
func (c *L1Cache) Set(ws [8]byte, id ULID, eng *Engram) {
	entry := &cacheEntry{
		eng: eng,
	}
	entry.lastAccess.Store(time.Now().UnixNano())

	c.data.Store(cacheKeyFor(ws, id), entry)
	newCount := c.count.Add(1)

	// Trigger eviction if needed
	if newCount > int64(c.maxSize) {
		c.evict()
	}
}

// Delete removes an engram from the cache for the given vault.
// Uses LoadAndDelete to avoid decrementing the counter when the key was already
// absent (e.g. due to concurrent eviction or a prior DeleteByVault call).
func (c *L1Cache) Delete(ws [8]byte, id ULID) {
	if _, loaded := c.data.LoadAndDelete(cacheKeyFor(ws, id)); loaded {
		c.count.Add(-1)
	}
}

// Len returns the approximate number of entries in the cache.
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

// evict removes one entry using approximate LRU.
// Scans 64 random entries, evicts the one with oldest lastAccess.
func (c *L1Cache) evict() {
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
		}
	}
}
