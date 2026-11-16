package storage

import (
	"context"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// A ReadOnly-marked GetEngram/GetEngrams call must not stamp the L1 cache's
// recency timestamp (EngramLastAccessNs) — "a scoring pass is not a user
// access". Both directions matter and are pinned separately: read-only
// scoring must NOT freshen, and a real (non-suppressed) read still MUST
// freshen — otherwise the fix could have been "never stamp at all", which
// would silently break genuine recency-based recall for every vault.
// ---------------------------------------------------------------------------

func TestGetEngram_NoAccessCacheStampContext_DoesNotFreshenRecency(t *testing.T) {
	dir := t.TempDir()
	db, err := OpenPebble(dir, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	store := NewPebbleStore(db, PebbleStoreConfig{CacheSize: 1000})
	defer store.Close()

	var ws [8]byte
	eng := &Engram{Concept: "probe", Content: "probe content", Confidence: 1.0, Stability: 30}
	id, err := store.WriteEngram(context.Background(), ws, eng)
	if err != nil {
		t.Fatalf("WriteEngram: %v", err)
	}

	// A suppressed-ctx read must leave the cache recency timestamp at 0
	// (not cached, in the recency sense) even after reading the engram
	// successfully.
	suppressedCtx := ContextWithNoAccessCacheStamp(context.Background())
	if _, err := store.GetEngram(suppressedCtx, ws, id); err != nil {
		t.Fatalf("GetEngram (suppressed): %v", err)
	}
	if ns := store.EngramLastAccessNs(ws, id); ns != 0 {
		t.Fatalf("violated: a suppressed-ctx GetEngram stamped cache recency (EngramLastAccessNs=%d), want 0", ns)
	}

	// The SAME engram, read WITHOUT suppression, must freshen recency —
	// proving the fix did not just turn off stamping altogether.
	before := time.Now().UnixNano()
	if _, err := store.GetEngram(context.Background(), ws, id); err != nil {
		t.Fatalf("GetEngram (real read): %v", err)
	}
	ns := store.EngramLastAccessNs(ws, id)
	if ns < before {
		t.Fatalf("a real (non-suppressed) GetEngram must still stamp cache recency: EngramLastAccessNs=%d, want >= %d", ns, before)
	}
}

func TestGetEngrams_NoAccessCacheStampContext_DoesNotFreshenRecency(t *testing.T) {
	dir := t.TempDir()
	db, err := OpenPebble(dir, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	store := NewPebbleStore(db, PebbleStoreConfig{CacheSize: 1000})
	defer store.Close()

	var ws [8]byte
	eng := &Engram{Concept: "probe batch", Content: "probe batch content", Confidence: 1.0, Stability: 30}
	id, err := store.WriteEngram(context.Background(), ws, eng)
	if err != nil {
		t.Fatalf("WriteEngram: %v", err)
	}

	suppressedCtx := ContextWithNoAccessCacheStamp(context.Background())
	if _, err := store.GetEngrams(suppressedCtx, ws, []ULID{id}); err != nil {
		t.Fatalf("GetEngrams (suppressed): %v", err)
	}
	if ns := store.EngramLastAccessNs(ws, id); ns != 0 {
		t.Fatalf("violated: a suppressed-ctx GetEngrams stamped cache recency (EngramLastAccessNs=%d), want 0", ns)
	}

	before := time.Now().UnixNano()
	if _, err := store.GetEngrams(context.Background(), ws, []ULID{id}); err != nil {
		t.Fatalf("GetEngrams (real read): %v", err)
	}
	ns := store.EngramLastAccessNs(ws, id)
	if ns < before {
		t.Fatalf("a real (non-suppressed) GetEngrams must still stamp cache recency: EngramLastAccessNs=%d, want >= %d", ns, before)
	}
}

// A suppressed (read-only) cache fill must not EVICT either. Eviction removes
// the sampled entry with the oldest lastAccess — i.e. a warm entry whose stamp
// is a live ACT-R recency input for the next recall. If a read-only recall's
// cold-candidate fill pushes the cache over capacity and evicts one, the next
// identical recall falls back to that engram's persisted LastAccess and scores
// it differently: the read changed what the following read sees. On a full
// cache the suppressed fill is skipped instead (the engram is still returned;
// it is simply not cached, exactly as if it had been evicted later).
//
// The cache must be larger than evict()'s 64-entry sample: with a smaller one
// the sample always includes the zero-stamped newcomer, which is then the
// victim and the bug hides. With 512 entries each fill samples the newcomer
// with probability ~1/8, so 40 fills evicting no stamped entry by luck is
// ~1e-36 — the RED is deterministic in practice.
func TestL1Cache_SetNoStamp_OnFullCacheDoesNotEvictStampedEntries(t *testing.T) {
	const size = 512
	c := NewL1Cache(size)
	var ws [8]byte
	stamped := make([]ULID, size)
	for i := range stamped {
		stamped[i] = ULID{0x01, byte(i >> 8), byte(i)}
		c.Set(ws, stamped[i], &Engram{ID: stamped[i]})
	}
	before := make([]int64, size)
	for i, id := range stamped {
		before[i] = c.LastAccessNs(ws, id)
		if before[i] == 0 {
			t.Fatalf("fixture: entry %d not stamped", i)
		}
	}

	for i := 0; i < 40; i++ {
		cold := ULID{0x02, byte(i)}
		c.SetNoStamp(ws, cold, &Engram{ID: cold})
	}

	for i, id := range stamped {
		if got := c.LastAccessNs(ws, id); got != before[i] {
			t.Fatalf("a suppressed SetNoStamp on a full cache evicted stamped entry %d (lastAccess %d -> %d): "+
				"a read-only fill changed the recency the next recall scores with", i, before[i], got)
		}
	}
	if n := c.Len(); n > size {
		t.Fatalf("cache over capacity after suppressed fill: Len=%d, max %d", n, size)
	}
}
