package storage

import (
	"testing"
	"time"

	"github.com/cockroachdb/pebble"
)

// TestWALSyncer_IdleNoSync verifies that the syncer does NOT call doSync when
// no writes have occurred. Before the adaptive fix, the syncer called doSync
// unconditionally every 10ms, producing ~1MB/s of idle disk writes.
func TestWALSyncer_IdleNoSync(t *testing.T) {
	db := openTestPebble(t)
	s := newWALSyncer(db)
	defer s.Close()

	// Wait long enough for several ticks to fire with no writes.
	time.Sleep(5 * walSyncInterval)

	got := s.syncCount.Load()
	if got != 0 {
		t.Errorf("expected 0 syncs at idle, got %d", got)
	}
}

// TestWALSyncer_SyncsAfterWrite verifies that the syncer DOES call doSync after
// a NoSync write advances the WAL byte counter.
func TestWALSyncer_SyncsAfterWrite(t *testing.T) {
	db := openTestPebble(t)
	s := newWALSyncer(db)
	defer s.Close()

	// Confirm no syncs have occurred yet.
	if got := s.syncCount.Load(); got != 0 {
		t.Fatalf("pre-write: expected 0 syncs, got %d", got)
	}

	// Perform a NoSync write to advance WAL.BytesWritten.
	if err := db.Set([]byte("test-key"), []byte("test-val"), pebble.NoSync); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Wait for the syncer to pick up the WAL change. Generous fixed bound:
	// time.Ticker guarantees a minimum tick interval, not a maximum, so a
	// starved CI runner can tick late (see #482).
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if s.syncCount.Load() > 0 {
			break
		}
		time.Sleep(walSyncInterval / 2)
	}

	if got := s.syncCount.Load(); got == 0 {
		t.Errorf("expected at least 1 sync after write, got 0")
	}
}

// TestWALSyncer_IdleThenWrite verifies the full lifecycle: idle → write → sync
// → idle again. After a sync, subsequent ticks with no new writes must not
// trigger additional syncs.
func TestWALSyncer_IdleThenWrite(t *testing.T) {
	db := openTestPebble(t)
	s := newWALSyncer(db)
	defer s.Close()

	// Phase 1: idle — no syncs expected.
	time.Sleep(5 * walSyncInterval)
	if got := s.syncCount.Load(); got != 0 {
		t.Fatalf("idle phase: expected 0 syncs, got %d", got)
	}

	// Phase 2: write — at least one sync expected.
	if err := db.Set([]byte("k1"), []byte("v1"), pebble.NoSync); err != nil {
		t.Fatalf("write: %v", err)
	}
	// Generous fixed bound — see #482 (ticker can be starved on a loaded runner).
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if s.syncCount.Load() > 0 {
			break
		}
		time.Sleep(walSyncInterval / 2)
	}
	afterWrite := s.syncCount.Load()
	if afterWrite == 0 {
		t.Fatalf("write phase: expected at least 1 sync, got 0")
	}

	// Phase 3: idle again — no additional syncs expected after the WAL is stable.
	time.Sleep(5 * walSyncInterval)
	afterIdle := s.syncCount.Load()
	if afterIdle != afterWrite {
		t.Errorf("second idle phase: sync count grew from %d to %d with no new writes", afterWrite, afterIdle)
	}
}

// TestWALSyncer_MultipleWritesBatchedIntoOneSync verifies that multiple NoSync
// writes occurring within a single tick window are covered by a single sync
// (group-commit semantics). The sync count should not exceed the number of
// tick intervals elapsed, regardless of write volume.
func TestWALSyncer_MultipleWritesBatchedIntoOneSync(t *testing.T) {
	db := openTestPebble(t)
	s := newWALSyncer(db)
	defer s.Close()

	// Write 20 keys without any delay (all within one tick window).
	for i := range 20 {
		key := []byte{byte(i)}
		if err := db.Set(key, key, pebble.NoSync); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}

	// Wait for two tick intervals — enough for exactly one or two syncs.
	time.Sleep(3 * walSyncInterval)

	got := s.syncCount.Load()
	// 20 writes should not produce 20 syncs; group-commit must batch them.
	// Allow up to 3 syncs (generous for timing jitter) but never 20.
	if got > 3 {
		t.Errorf("expected ≤3 syncs for 20 batched writes, got %d (group-commit not working)", got)
	}
	if got == 0 {
		t.Errorf("expected at least 1 sync for 20 writes, got 0")
	}
}

// TestWALSyncer_FinalSyncOnClose verifies that Close() always calls doSync
// once to flush any in-flight NoSync writes, even if the WAL counter has not
// changed since the last tick sync.
func TestWALSyncer_FinalSyncOnClose(t *testing.T) {
	db := openTestPebble(t)

	// Write before creating the syncer so lastWALBytes starts non-zero.
	if err := db.Set([]byte("pre"), []byte("write"), pebble.NoSync); err != nil {
		t.Fatalf("pre-write: %v", err)
	}

	s := newWALSyncer(db)

	// Wait for the syncer to observe the write and sync. Generous fixed bound —
	// see #482 (ticker can be starved on a loaded runner).
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if s.syncCount.Load() > 0 {
			break
		}
		time.Sleep(walSyncInterval / 2)
	}
	beforeClose := s.syncCount.Load()

	// Write again just before close — this should be covered by the final sync.
	if err := db.Set([]byte("post"), []byte("write"), pebble.NoSync); err != nil {
		t.Fatalf("post-write: %v", err)
	}

	// Close must perform the final sync without panicking.
	s.Close()

	afterClose := s.syncCount.Load()
	if afterClose <= beforeClose {
		t.Errorf("Close() did not perform final sync: count before=%d after=%d", beforeClose, afterClose)
	}
}

// TestWALSyncer_SyncCountAccurate verifies that syncCount accurately reflects
// the number of times doSync was called across a sequence of write/idle cycles.
func TestWALSyncer_SyncCountAccurate(t *testing.T) {
	db := openTestPebble(t)
	s := newWALSyncer(db)
	defer s.Close()

	for cycle := range 3 {
		// Write a key to trigger a sync.
		key := []byte{byte(cycle), 0xFF}
		if err := db.Set(key, key, pebble.NoSync); err != nil {
			t.Fatalf("cycle %d write: %v", cycle, err)
		}
		// Wait for the sync to fire. The deadline is a generous fixed wall-clock
		// bound (not a small multiple of walSyncInterval): time.Ticker guarantees
		// a *minimum* tick interval, not a maximum, so on a contended CI runner
		// the syncer goroutine can be starved well past a few intervals. A tight
		// 10×interval (100ms) deadline made this flake ("cycle 2: got 2"); 2s
		// gives a heavily loaded runner ample room while a healthy run still
		// passes in a couple of intervals.
		deadline := time.Now().Add(2 * time.Second)
		want := int64(cycle + 1)
		for time.Now().Before(deadline) {
			if s.syncCount.Load() >= want {
				break
			}
			time.Sleep(walSyncInterval / 2)
		}
		if got := s.syncCount.Load(); got < want {
			t.Errorf("cycle %d: expected syncCount ≥ %d, got %d", cycle, want, got)
		}
		// Idle between cycles — count must not grow.
		snapshot := s.syncCount.Load()
		time.Sleep(4 * walSyncInterval)
		if after := s.syncCount.Load(); after != snapshot {
			t.Errorf("cycle %d idle: sync count grew from %d to %d with no writes", cycle, snapshot, after)
		}
	}
}
