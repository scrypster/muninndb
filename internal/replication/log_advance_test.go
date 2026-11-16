package replication

import (
	"fmt"
	"testing"
)

// The two primitives a promotion is built from: ReplicationLog.AdvanceTo and
// Applier.HoldCursor. promotion_sequence_test.go covers how the coordinator
// uses them.

// fillLog appends n entries to log, numbered from wherever it stands.
func fillLog(t *testing.T, log *ReplicationLog, n int) {
	t.Helper()
	for i := 1; i <= n; i++ {
		if _, err := log.Append(OpSet, []byte(fmt.Sprintf("probe/fill/%04d", i)), []byte("invented")); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
}

// TestReplicationLog_AdvanceTo covers the log primitive on its own.
func TestReplicationLog_AdvanceTo(t *testing.T) {
	t.Run("raises an empty log and the next append follows it", func(t *testing.T) {
		db := newTestDB(t)
		log := NewReplicationLog(db)
		prev, err := log.AdvanceTo(40)
		if err != nil || prev != 0 {
			t.Fatalf("AdvanceTo(40) = (%d, %v), want (0, nil)", prev, err)
		}
		if got := log.CurrentSeq(); got != 40 {
			t.Fatalf("CurrentSeq = %d, want 40", got)
		}
		seq, err := log.Append(OpSet, []byte("probe/k"), []byte("invented"))
		if err != nil || seq != 41 {
			t.Fatalf("Append after AdvanceTo(40) = (%d, %v), want (41, nil)", seq, err)
		}
	})

	t.Run("is on disk before any append", func(t *testing.T) {
		db := newTestDB(t)
		if _, err := NewReplicationLog(db).AdvanceTo(40); err != nil {
			t.Fatalf("AdvanceTo: %v", err)
		}
		if got := NewReplicationLog(db).CurrentSeq(); got != 40 {
			t.Fatalf("a second log over the same store reads head %d, want 40", got)
		}
	})

	t.Run("never lowers the counter", func(t *testing.T) {
		db := newTestDB(t)
		log := NewReplicationLog(db)
		fillLog(t, log, 10)
		for _, target := range []uint64{0, 3, 10} {
			prev, err := log.AdvanceTo(target)
			if err != nil || prev != 10 {
				t.Fatalf("AdvanceTo(%d) = (%d, %v), want (10, nil)", target, prev, err)
			}
			if got := log.CurrentSeq(); got != 10 {
				t.Fatalf("CurrentSeq after AdvanceTo(%d) = %d, want 10", target, got)
			}
		}
	})

	t.Run("leaves the earlier entries readable and writes none of its own", func(t *testing.T) {
		db := newTestDB(t)
		log := NewReplicationLog(db)
		fillLog(t, log, 3)
		if _, err := log.AdvanceTo(40); err != nil {
			t.Fatalf("AdvanceTo: %v", err)
		}
		if _, err := log.Append(OpSet, []byte("probe/after"), []byte("invented")); err != nil {
			t.Fatalf("Append: %v", err)
		}
		if got, want := fmt.Sprint(logSeqs(t, log, 0)), "[1 2 3 41]"; got != want {
			t.Fatalf("entries in the log = %s, want %s: nothing may be written into the advanced range", got, want)
		}
	})

	t.Run("an advance requested while the counter was unreadable is applied by the first successful read", func(t *testing.T) {
		// A store read cannot be made to fail here without fault injection, so
		// the failing branch of AdvanceTo itself is not exercised. This sets the
		// one thing that branch leaves behind — the floor — and checks every
		// way the counter is then first read.
		for name, read := range map[string]func(*ReplicationLog) uint64{
			"CurrentSeq": func(l *ReplicationLog) uint64 { return l.CurrentSeq() },
			"Append": func(l *ReplicationLog) uint64 {
				seq, _ := l.Append(OpSet, []byte("probe/k"), []byte("invented"))
				return seq - 1
			},
		} {
			db := newTestDB(t)
			stale := NewReplicationLog(db)
			fillLog(t, stale, 5) // the value on disk from before the promotion

			log := NewReplicationLog(db)
			log.floor = 40
			if got := read(log); got != 40 {
				t.Errorf("%s: counter initialized to %d with a pending advance to 40 and 5 on disk, want 40", name, got)
			}
		}
	})
}

// TestHoldCursor_ExcludesApply pins the mechanism the test above relies on:
// HoldCursor runs its function with the applier locked, and reports the cursor
// as of that moment.
func TestHoldCursor_ExcludesApply(t *testing.T) {
	coord, _ := newTestCoordinator(t, "replica")
	applyThrough(t, coord, 0, 7)

	called := false
	coord.applier.HoldCursor(func(lastApplied uint64) {
		called = true
		if lastApplied != 7 {
			t.Errorf("HoldCursor reported %d, want 7", lastApplied)
		}
		if coord.applier.mu.TryLock() {
			coord.applier.mu.Unlock()
			t.Error("the applier's lock is free inside HoldCursor: an Apply could run while the cursor is held")
		}
	})
	if !called {
		t.Fatal("HoldCursor did not call its function")
	}
	// And it lets go afterwards.
	applyThrough(t, coord, 7, 8)
	if got := coord.applier.LastApplied(); got != 8 {
		t.Errorf("LastApplied after HoldCursor returned = %d, want 8", got)
	}
}
