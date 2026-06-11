package hnsw

import (
	"errors"
	"sync/atomic"
	"testing"

	"github.com/cockroachdb/pebble"
)

// TestGetOrCreate_FailedLoadNotCached verifies that when LoadFromPebble errors,
// getOrCreate does NOT pin the empty index in r.indexes — so the next access
// retries the load instead of serving a permanently empty cached index for the
// rest of the process lifetime (issue #499, defect 2).
func TestGetOrCreate_FailedLoadNotCached(t *testing.T) {
	db, err := pebble.Open(t.TempDir(), &pebble.Options{})
	if err != nil {
		t.Fatalf("pebble.Open: %v", err)
	}
	defer db.Close()

	reg := NewRegistry(db)
	// Force every lazily-created Index's load to fail, simulating a transient
	// Pebble iterator error.
	var loadCalls atomic.Int64
	reg.loadErrHook = func() error {
		loadCalls.Add(1)
		return errors.New("simulated transient load error")
	}

	var ws [8]byte
	copy(ws[:], []byte("FAILLOAD"))

	// First access: load fails. The returned index degrades gracefully
	// (non-nil, empty) but must NOT be cached.
	idx1 := reg.getOrCreate(ws)
	if idx1 == nil {
		t.Fatal("getOrCreate returned nil on failed load; expected a usable empty index")
	}
	if cached := reg.get(ws); cached != nil {
		t.Fatalf("failed-load index was cached in r.indexes; load will never be retried")
	}

	// Second access: must retry the load (i.e. not be served from a cached
	// empty index). While the load keeps failing, the index must not be pinned.
	idx2 := reg.getOrCreate(ws)
	if idx2 == nil {
		t.Fatal("second getOrCreate returned nil; expected retry to produce a usable empty index")
	}
	if cached := reg.get(ws); cached != nil {
		t.Fatalf("failed-load index was cached on retry; load will never be retried")
	}

	// The load must have been attempted on BOTH accesses — proving the failed
	// load is retried rather than served from a cached empty index.
	if got := loadCalls.Load(); got != 2 {
		t.Fatalf("expected 2 load attempts (retry on each access), got %d", got)
	}
}

// TestGetOrCreate_SuccessfulLoadCached verifies the happy path still caches the
// index so repeated accesses reuse it.
func TestGetOrCreate_SuccessfulLoadCached(t *testing.T) {
	db, err := pebble.Open(t.TempDir(), &pebble.Options{})
	if err != nil {
		t.Fatalf("pebble.Open: %v", err)
	}
	defer db.Close()

	reg := NewRegistry(db)
	var ws [8]byte
	copy(ws[:], []byte("OKLOADED"))

	idx1 := reg.getOrCreate(ws)
	if idx1 == nil {
		t.Fatal("getOrCreate returned nil on successful load")
	}
	if cached := reg.get(ws); cached == nil {
		t.Fatal("successful-load index was not cached")
	}
	if idx2 := reg.getOrCreate(ws); idx2 != idx1 {
		t.Fatal("successful load not served from cache on second access")
	}
}
