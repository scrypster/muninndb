package storage

import (
	"os"
	"testing"

	"github.com/cockroachdb/pebble"
)

// openTestPebble opens a Pebble DB in a temp directory and registers t.Cleanup
// to close the DB and remove the directory. Use this ONLY when you need a raw
// *pebble.DB without a PebbleStore wrapper. If you need a PebbleStore, use
// openTestStore instead — PebbleStore.Close() already calls db.Close()
// internally, so using openTestPebble alongside a PebbleStore causes a
// double-close panic ("pebble: closed").
func openTestPebble(t *testing.T) *pebble.DB {
	t.Helper()
	dir, err := os.MkdirTemp("", "muninndb-test-*")
	if err != nil {
		t.Fatalf("create temp dir: %v", err)
	}
	db, err := OpenPebble(dir, DefaultOptions())
	if err != nil {
		os.RemoveAll(dir)
		t.Fatalf("open pebble: %v", err)
	}
	t.Cleanup(func() {
		db.Close()
		os.RemoveAll(dir)
	})
	return db
}

// newFreshStore wraps an existing *pebble.DB in a new PebbleStore (cold-cache
// path) and registers t.Cleanup to drain the store's background goroutines
// without closing the shared db — the primary store owns the db lifecycle.
//
// Use this instead of "defer fresh.Close()" when creating a second PebbleStore
// from the same db: fresh.Close() calls db.Close() on the shared db, which
// causes a double-close panic when the primary store's Close() runs later.
//
// t.Cleanup ordering: newFreshStore must be called AFTER the primary store's
// cleanup is registered (e.g. via newTestStore or openTestStore) so that
// fresh goroutine drain runs FIRST (LIFO), guaranteeing no goroutine fires
// on the db after the primary store's Close() closes it.
func newFreshStore(t *testing.T, db *pebble.DB) *PebbleStore {
	t.Helper()
	store := NewPebbleStore(db, PebbleStoreConfig{CacheSize: 100})
	t.Cleanup(func() {
		// Drain all background goroutines without closing the shared db.
		// The primary PebbleStore owns db.Close().
		if store.walSync != nil {
			store.walSync.Close()
		}
		if store.counterFlush != nil {
			store.counterFlush.Close()
		}
		if store.provWork != nil {
			store.provWork.Close()
		}
		if store.transCache != nil {
			store.transCache.Close()
		}
	})
	return store
}

// openTestStore opens a Pebble DB in a temp directory, wraps it in a
// PebbleStore, and registers t.Cleanup to stop background goroutines,
// close the DB, and remove the temp dir.
//
// Do NOT use openTestPebble here: PebbleStore.Close() already calls
// db.Close() internally. A second db.Close() from openTestPebble's cleanup
// would cause pebble to panic with "pebble: closed".
func openTestStore(t *testing.T) *PebbleStore {
	t.Helper()
	dir, err := os.MkdirTemp("", "muninndb-test-*")
	if err != nil {
		t.Fatalf("create temp dir: %v", err)
	}
	db, err := OpenPebble(dir, DefaultOptions())
	if err != nil {
		os.RemoveAll(dir)
		t.Fatalf("open pebble: %v", err)
	}
	store := NewPebbleStore(db, PebbleStoreConfig{CacheSize: 100})
	// store.Close() stops all background goroutines and calls db.Close().
	// os.RemoveAll runs after the DB is fully closed.
	t.Cleanup(func() {
		store.Close()
		os.RemoveAll(dir)
	})
	return store
}
