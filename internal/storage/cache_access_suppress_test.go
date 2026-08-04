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
