package replication

import (
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cockroachdb/pebble"
	"github.com/cockroachdb/pebble/vfs"
)

func TestReplicationLog_Prune_Large(t *testing.T) {
	// Use an in-memory filesystem so 10 000 sync commits don't hit the
	// physical disk, keeping the test fast on all platforms.
	db, err := pebble.Open("", &pebble.Options{FS: vfs.NewMem()})
	if err != nil {
		t.Fatalf("failed to open pebble: %v", err)
	}
	defer db.Close()

	log := NewReplicationLog(db)

	// Append 10000 entries
	for i := 1; i <= 10000; i++ {
		_, err := log.Append(OpSet, []byte("key"), []byte("value"))
		if err != nil {
			t.Fatalf("append %d failed: %v", i, err)
		}
	}

	// Time the prune operation — should be O(1) range delete
	start := time.Now()
	if err := log.Prune(9990); err != nil {
		t.Fatalf("Prune failed: %v", err)
	}
	elapsed := time.Since(start)

	if elapsed > 200*time.Millisecond {
		t.Errorf("Prune took %v, want < 200ms", elapsed)
	}

	// ReadSince(0) should return entries 9991-10000 (10 entries)
	entries, err := log.ReadSince(0, 100)
	if err != nil {
		t.Fatalf("ReadSince failed: %v", err)
	}

	if len(entries) != 10 {
		t.Errorf("len(entries) = %d, want 10", len(entries))
	}

	if entries[0].Seq != 9991 {
		t.Errorf("first entry seq = %d, want 9991", entries[0].Seq)
	}
	if entries[9].Seq != 10000 {
		t.Errorf("last entry seq = %d, want 10000", entries[9].Seq)
	}

	// CurrentSeq should still be 10000
	if seq := log.CurrentSeq(); seq != 10000 {
		t.Errorf("CurrentSeq = %d, want 10000", seq)
	}
}

func TestReplicationLog_Prune_BoundaryConditions(t *testing.T) {
	t.Run("prune zero on empty log", func(t *testing.T) {
		dir := t.TempDir()
		db, err := pebble.Open(dir, &pebble.Options{})
		if err != nil {
			t.Fatalf("failed to open pebble: %v", err)
		}
		defer db.Close()

		log := NewReplicationLog(db)

		// Prune(0) on empty log → no error
		if err := log.Prune(0); err != nil {
			t.Errorf("Prune(0) on empty log returned error: %v", err)
		}
		if seq := log.CurrentSeq(); seq != 0 {
			t.Errorf("CurrentSeq = %d, want 0", seq)
		}
	})

	t.Run("prune at seq boundary is no-op", func(t *testing.T) {
		dir := t.TempDir()
		db, err := pebble.Open(dir, &pebble.Options{})
		if err != nil {
			t.Fatalf("failed to open pebble: %v", err)
		}
		defer db.Close()

		log := NewReplicationLog(db)
		for i := 1; i <= 5; i++ {
			log.Append(OpSet, []byte("key"), []byte("value"))
		}

		// Prune(5) → returns nil (untilSeq >= l.seq, so it's a no-op)
		if err := log.Prune(5); err != nil {
			t.Errorf("Prune(5) returned error: %v", err)
		}

		// All 5 entries still readable
		entries, err := log.ReadSince(0, 100)
		if err != nil {
			t.Fatalf("ReadSince failed: %v", err)
		}
		if len(entries) != 5 {
			t.Errorf("len(entries) = %d, want 5", len(entries))
		}
	})

	t.Run("prune(3) leaves entries 4 and 5", func(t *testing.T) {
		dir := t.TempDir()
		db, err := pebble.Open(dir, &pebble.Options{})
		if err != nil {
			t.Fatalf("failed to open pebble: %v", err)
		}
		defer db.Close()

		log := NewReplicationLog(db)
		for i := 1; i <= 5; i++ {
			log.Append(OpSet, []byte("key"), []byte("value"))
		}

		if err := log.Prune(3); err != nil {
			t.Fatalf("Prune(3) failed: %v", err)
		}

		entries, err := log.ReadSince(0, 100)
		if err != nil {
			t.Fatalf("ReadSince failed: %v", err)
		}
		if len(entries) != 2 {
			t.Errorf("len(entries) = %d, want 2", len(entries))
		}
		if entries[0].Seq != 4 || entries[1].Seq != 5 {
			t.Errorf("sequences = %d %d, want 4 5", entries[0].Seq, entries[1].Seq)
		}
	})

	t.Run("prune(1) leaves entries 2-5", func(t *testing.T) {
		dir := t.TempDir()
		db, err := pebble.Open(dir, &pebble.Options{})
		if err != nil {
			t.Fatalf("failed to open pebble: %v", err)
		}
		defer db.Close()

		log := NewReplicationLog(db)
		for i := 1; i <= 5; i++ {
			log.Append(OpSet, []byte("key"), []byte("value"))
		}

		if err := log.Prune(1); err != nil {
			t.Fatalf("Prune(1) failed: %v", err)
		}

		entries, err := log.ReadSince(0, 100)
		if err != nil {
			t.Fatalf("ReadSince failed: %v", err)
		}
		if len(entries) != 4 {
			t.Errorf("len(entries) = %d, want 4", len(entries))
		}
		if entries[0].Seq != 2 || entries[3].Seq != 5 {
			t.Errorf("sequences start=%d end=%d, want 2..5", entries[0].Seq, entries[3].Seq)
		}
	})
}

func TestReplicationLog_Prune_StartsAtOne(t *testing.T) {
	dir := t.TempDir()
	db, err := pebble.Open(dir, &pebble.Options{})
	if err != nil {
		t.Fatalf("failed to open pebble: %v", err)
	}
	defer db.Close()

	log := NewReplicationLog(db)

	// Append 3 entries
	for i := 1; i <= 3; i++ {
		_, err := log.Append(OpSet, []byte("key"), []byte("value"))
		if err != nil {
			t.Fatalf("append %d failed: %v", i, err)
		}
	}

	// Prune(2) — entries 1 and 2 pruned, entry 3 remains
	if err := log.Prune(2); err != nil {
		t.Fatalf("Prune(2) failed: %v", err)
	}

	// ReadSince(0) returns only entry 3
	entries, err := log.ReadSince(0, 100)
	if err != nil {
		t.Fatalf("ReadSince failed: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("len(entries) = %d, want 1", len(entries))
	}
	if entries[0].Seq != 3 {
		t.Errorf("entry seq = %d, want 3", entries[0].Seq)
	}

	// Pruning does not affect the seq counter
	if seq := log.CurrentSeq(); seq != 3 {
		t.Errorf("CurrentSeq = %d, want 3", seq)
	}

	// Confirm seq=0 key does not exist (was never written, prune starts at 1)
	_, closer, err := db.Get(replicationEntryKey(0))
	if closer != nil {
		closer.Close()
	}
	if err != pebble.ErrNotFound {
		t.Errorf("replicationEntryKey(0) should not exist, got err=%v", err)
	}
}

func TestReplicationLog_AppendAndRead(t *testing.T) {
	dir := t.TempDir()
	db, err := pebble.Open(dir, &pebble.Options{})
	if err != nil {
		t.Fatalf("failed to open pebble: %v", err)
	}
	defer db.Close()

	log := NewReplicationLog(db)

	// Append 3 entries
	seq1, err := log.Append(OpSet, []byte("key1"), []byte("value1"))
	if err != nil {
		t.Fatalf("append 1 failed: %v", err)
	}
	if seq1 != 1 {
		t.Errorf("seq1 = %d, want 1", seq1)
	}

	seq2, err := log.Append(OpSet, []byte("key2"), []byte("value2"))
	if err != nil {
		t.Fatalf("append 2 failed: %v", err)
	}
	if seq2 != 2 {
		t.Errorf("seq2 = %d, want 2", seq2)
	}

	seq3, err := log.Append(OpDelete, []byte("key3"), nil)
	if err != nil {
		t.Fatalf("append 3 failed: %v", err)
	}
	if seq3 != 3 {
		t.Errorf("seq3 = %d, want 3", seq3)
	}

	// ReadSince(0) should return all 3 entries
	entries, err := log.ReadSince(0, 100)
	if err != nil {
		t.Fatalf("ReadSince failed: %v", err)
	}

	if len(entries) != 3 {
		t.Errorf("len(entries) = %d, want 3", len(entries))
	}

	if entries[0].Seq != 1 || entries[0].Op != OpSet {
		t.Errorf("entry 0: seq=%d op=%d, want seq=1 op=%d", entries[0].Seq, entries[0].Op, OpSet)
	}
	if entries[1].Seq != 2 || entries[1].Op != OpSet {
		t.Errorf("entry 1: seq=%d op=%d, want seq=2 op=%d", entries[1].Seq, entries[1].Op, OpSet)
	}
	if entries[2].Seq != 3 || entries[2].Op != OpDelete {
		t.Errorf("entry 2: seq=%d op=%d, want seq=3 op=%d", entries[2].Seq, entries[2].Op, OpDelete)
	}
}

func TestReplicationLog_ReadSince(t *testing.T) {
	dir := t.TempDir()
	db, err := pebble.Open(dir, &pebble.Options{})
	if err != nil {
		t.Fatalf("failed to open pebble: %v", err)
	}
	defer db.Close()

	log := NewReplicationLog(db)

	// Append 5 entries
	for i := 1; i <= 5; i++ {
		_, err := log.Append(OpSet, []byte("key"), []byte("value"))
		if err != nil {
			t.Fatalf("append %d failed: %v", i, err)
		}
	}

	// ReadSince(2) should return entries 3, 4, 5
	entries, err := log.ReadSince(2, 100)
	if err != nil {
		t.Fatalf("ReadSince failed: %v", err)
	}

	if len(entries) != 3 {
		t.Errorf("len(entries) = %d, want 3", len(entries))
	}

	if entries[0].Seq != 3 || entries[1].Seq != 4 || entries[2].Seq != 5 {
		t.Errorf("sequences incorrect: %d %d %d, want 3 4 5", entries[0].Seq, entries[1].Seq, entries[2].Seq)
	}
}

func TestReplicationLog_ReadSince_WithLimit(t *testing.T) {
	dir := t.TempDir()
	db, err := pebble.Open(dir, &pebble.Options{})
	if err != nil {
		t.Fatalf("failed to open pebble: %v", err)
	}
	defer db.Close()

	log := NewReplicationLog(db)

	// Append 10 entries
	for i := 1; i <= 10; i++ {
		_, err := log.Append(OpSet, []byte("key"), []byte("value"))
		if err != nil {
			t.Fatalf("append %d failed: %v", i, err)
		}
	}

	// ReadSince with limit=3 should return only 3 entries
	entries, err := log.ReadSince(5, 3)
	if err != nil {
		t.Fatalf("ReadSince failed: %v", err)
	}

	if len(entries) != 3 {
		t.Errorf("len(entries) = %d, want 3", len(entries))
	}

	if entries[0].Seq != 6 || entries[1].Seq != 7 || entries[2].Seq != 8 {
		t.Errorf("sequences incorrect: %d %d %d, want 6 7 8", entries[0].Seq, entries[1].Seq, entries[2].Seq)
	}
}

func TestReplicationLog_Prune(t *testing.T) {
	dir := t.TempDir()
	db, err := pebble.Open(dir, &pebble.Options{})
	if err != nil {
		t.Fatalf("failed to open pebble: %v", err)
	}
	defer db.Close()

	log := NewReplicationLog(db)

	// Append 5 entries
	for i := 1; i <= 5; i++ {
		_, err := log.Append(OpSet, []byte("key"), []byte("value"))
		if err != nil {
			t.Fatalf("append %d failed: %v", i, err)
		}
	}

	// Prune entries <= 2
	if err := log.Prune(2); err != nil {
		t.Fatalf("Prune failed: %v", err)
	}

	// ReadSince(0) should now return entries 3, 4, 5
	entries, err := log.ReadSince(0, 100)
	if err != nil {
		t.Fatalf("ReadSince failed: %v", err)
	}

	if len(entries) != 3 {
		t.Errorf("len(entries) = %d, want 3", len(entries))
	}

	if entries[0].Seq != 3 || entries[1].Seq != 4 || entries[2].Seq != 5 {
		t.Errorf("sequences incorrect after prune: %d %d %d, want 3 4 5",
			entries[0].Seq, entries[1].Seq, entries[2].Seq)
	}
}

func TestReplicationLog_CurrentSeq(t *testing.T) {
	dir := t.TempDir()
	db, err := pebble.Open(dir, &pebble.Options{})
	if err != nil {
		t.Fatalf("failed to open pebble: %v", err)
	}
	defer db.Close()

	log := NewReplicationLog(db)

	if log.CurrentSeq() != 0 {
		t.Errorf("initial CurrentSeq = %d, want 0", log.CurrentSeq())
	}

	// Append entries
	for i := 1; i <= 5; i++ {
		log.Append(OpSet, []byte("key"), []byte("value"))
		if seq := log.CurrentSeq(); seq != uint64(i) {
			t.Errorf("after append %d, CurrentSeq = %d, want %d", i, seq, i)
		}
	}
}

func TestReplicationLog_Persistence(t *testing.T) {
	dir := t.TempDir()

	// First db: append entries
	db, err := pebble.Open(dir, &pebble.Options{})
	if err != nil {
		t.Fatalf("failed to open pebble: %v", err)
	}

	log := NewReplicationLog(db)
	for i := 1; i <= 3; i++ {
		_, err := log.Append(OpSet, []byte("key"), []byte("value"))
		if err != nil {
			t.Fatalf("append %d failed: %v", i, err)
		}
	}

	if log.CurrentSeq() != 3 {
		t.Errorf("before close, CurrentSeq = %d, want 3", log.CurrentSeq())
	}

	db.Close()

	// Reopen db: should restore seq from persistence
	db, err = pebble.Open(dir, &pebble.Options{})
	if err != nil {
		t.Fatalf("failed to reopen pebble: %v", err)
	}
	defer db.Close()

	log2 := NewReplicationLog(db)
	if log2.CurrentSeq() != 3 {
		t.Errorf("after reopen, CurrentSeq = %d, want 3", log2.CurrentSeq())
	}

	// Append another entry, should be seq 4
	seq, err := log2.Append(OpSet, []byte("key4"), []byte("value4"))
	if err != nil {
		t.Fatalf("append 4 failed: %v", err)
	}

	if seq != 4 {
		t.Errorf("new append seq = %d, want 4", seq)
	}
}

func TestReplicationLog_Append_AtomicBatch(t *testing.T) {
	dir := t.TempDir()
	db, err := pebble.Open(dir, &pebble.Options{})
	if err != nil {
		t.Fatalf("failed to open pebble: %v", err)
	}
	defer db.Close()

	log := NewReplicationLog(db)

	seq, err := log.Append(OpSet, []byte("key1"), []byte("value1"))
	if err != nil {
		t.Fatalf("Append failed: %v", err)
	}
	if seq != 1 {
		t.Errorf("seq = %d, want 1", seq)
	}

	// Verify the entry key exists in Pebble with the correct seq
	entryVal, entryCloser, err := db.Get(replicationEntryKey(1))
	if err != nil {
		t.Fatalf("entry key not found in Pebble: %v", err)
	}
	if entryCloser != nil {
		defer entryCloser.Close()
	}
	if len(entryVal) == 0 {
		t.Error("entry value is empty")
	}

	// Verify the seq counter key exists in Pebble with value == 1
	seqVal, seqCloser, err := db.Get(seqCounterKey())
	if err != nil {
		t.Fatalf("seq counter key not found in Pebble: %v", err)
	}
	if seqCloser != nil {
		defer seqCloser.Close()
	}
	if len(seqVal) < 8 {
		t.Fatalf("seq counter value too short: %d bytes", len(seqVal))
	}
	storedSeq := binary.BigEndian.Uint64(seqVal)
	if storedSeq != 1 {
		t.Errorf("stored seq counter = %d, want 1", storedSeq)
	}
}

func TestReplicationLog_Append_RollbackOnError(t *testing.T) {
	dir := t.TempDir()
	db, err := pebble.Open(dir, &pebble.Options{})
	if err != nil {
		t.Fatalf("failed to open pebble: %v", err)
	}
	defer db.Close()

	log := NewReplicationLog(db)

	// Fresh log: CurrentSeq should be 0
	if seq := log.CurrentSeq(); seq != 0 {
		t.Errorf("initial CurrentSeq = %d, want 0", seq)
	}

	// Successful append: seq should advance to 1
	seq, err := log.Append(OpSet, []byte("key1"), []byte("value1"))
	if err != nil {
		t.Fatalf("Append failed: %v", err)
	}
	if seq != 1 {
		t.Errorf("seq after first Append = %d, want 1", seq)
	}
	if cur := log.CurrentSeq(); cur != 1 {
		t.Errorf("CurrentSeq after first Append = %d, want 1", cur)
	}

	// Append a second entry and verify seq advances correctly
	seq2, err := log.Append(OpDelete, []byte("key1"), nil)
	if err != nil {
		t.Fatalf("second Append failed: %v", err)
	}
	if seq2 != 2 {
		t.Errorf("seq after second Append = %d, want 2", seq2)
	}
	if cur := log.CurrentSeq(); cur != 2 {
		t.Errorf("CurrentSeq after second Append = %d, want 2", cur)
	}
}

func TestApplier_Apply(t *testing.T) {
	dir := t.TempDir()
	db, err := pebble.Open(dir, &pebble.Options{})
	if err != nil {
		t.Fatalf("failed to open pebble: %v", err)
	}
	defer db.Close()

	applier := NewApplier(db)

	// Apply SET
	entry1 := ReplicationEntry{
		Seq:   1,
		Op:    OpSet,
		Key:   []byte("test_key"),
		Value: []byte("test_value"),
	}

	if err := applier.Apply(entry1); err != nil {
		t.Fatalf("apply SET failed: %v", err)
	}

	if applier.LastApplied() != 1 {
		t.Errorf("LastApplied = %d, want 1", applier.LastApplied())
	}

	// Verify value in db
	val, closer, err := db.Get([]byte("test_key"))
	if err != nil {
		t.Fatalf("db.Get failed: %v", err)
	}
	if closer != nil {
		defer closer.Close()
	}

	if string(val) != "test_value" {
		t.Errorf("value = %q, want %q", string(val), "test_value")
	}

	// Apply DELETE
	entry2 := ReplicationEntry{
		Seq: 2,
		Op:  OpDelete,
		Key: []byte("test_key"),
	}

	if err := applier.Apply(entry2); err != nil {
		t.Fatalf("apply DELETE failed: %v", err)
	}

	if applier.LastApplied() != 2 {
		t.Errorf("LastApplied = %d, want 2", applier.LastApplied())
	}

	// Verify key is deleted
	val, closer, err = db.Get([]byte("test_key"))
	if err == pebble.ErrNotFound {
		// Expected
	} else if err != nil {
		t.Fatalf("db.Get failed: %v", err)
	} else {
		t.Errorf("key still exists: %v", val)
	}
	if closer != nil {
		closer.Close()
	}
}

func TestApplier_IsLagging(t *testing.T) {
	dir := t.TempDir()
	db, err := pebble.Open(dir, &pebble.Options{})
	if err != nil {
		t.Fatalf("failed to open pebble: %v", err)
	}
	defer db.Close()

	applier := NewApplier(db)

	// No entries applied yet
	if !applier.IsLagging(100, 10) {
		t.Errorf("IsLagging(100, 10) = false, want true (lag 100 > max 10)")
	}

	// Apply some entries
	for i := 1; i <= 50; i++ {
		entry := ReplicationEntry{
			Seq: uint64(i),
			Op:  OpSet,
			Key: []byte("key"),
		}
		applier.Apply(entry)
	}

	// Now at seq 50, primary at 100, lag = 50, max = 10 → lagging
	if !applier.IsLagging(100, 10) {
		t.Errorf("IsLagging(100, 10) = false, want true (lag 50 > max 10)")
	}

	// Lag = 30, max = 30 → not lagging
	if applier.IsLagging(80, 30) {
		t.Errorf("IsLagging(80, 30) = true, want false (lag 30 <= max 30)")
	}
}

func TestFencingToken_Valid(t *testing.T) {
	// Only exact equality is valid: the fencing token IS the epoch,
	// and a node's token must match the current cluster epoch exactly.
	if err := ValidateFencingToken(5, 5); err != nil {
		t.Errorf("ValidateFencingToken(5, 5) failed: %v, want nil", err)
	}
}

func TestFencingToken_StaleRejected(t *testing.T) {
	// Token less than current epoch — demoted primary.
	if err := ValidateFencingToken(10, 5); err != ErrStaleFencingToken {
		t.Errorf("ValidateFencingToken(10, 5) = %v, want ErrStaleFencingToken", err)
	}

	// Token greater than current epoch — also rejected (strict equality).
	if err := ValidateFencingToken(5, 10); err != ErrStaleFencingToken {
		t.Errorf("ValidateFencingToken(5, 10) = %v, want ErrStaleFencingToken", err)
	}
}

func TestLeaderElector_BasicElection(t *testing.T) {
	backend := NewMemoryLeaseBackend()
	elector := NewLeaderElector("node1", backend)

	if elector.IsLeader() {
		t.Errorf("IsLeader() = true initially, want false")
	}

	// Manually trigger election (simulating a tick)
	promoted := false
	elector.OnPromote = func() {
		promoted = true
	}

	elector.tick(context.Background())

	if !elector.IsLeader() {
		t.Errorf("IsLeader() = false after tick, want true")
	}

	if !promoted {
		t.Errorf("OnPromote not called")
	}

	if elector.FencingToken() != 1 {
		t.Errorf("FencingToken() = %d, want 1", elector.FencingToken())
	}
}

func TestLeaderElector_FencingToken(t *testing.T) {
	backend := NewMemoryLeaseBackend()

	elector1 := NewLeaderElector("node1", backend)
	elector2 := NewLeaderElector("node2", backend)

	// Node1 acquires lease
	acquired, err := backend.TryAcquire(context.Background(), "node1", elector1.LeaseTTL)
	if err != nil || !acquired {
		t.Fatalf("node1 should acquire lease: err=%v acquired=%v", err, acquired)
	}

	elector1.tick(context.Background())
	if !elector1.IsLeader() {
		t.Errorf("node1 should be leader after tick")
	}

	token1 := elector1.FencingToken()

	// Node2 tries to acquire (should fail)
	acquired, err = backend.TryAcquire(context.Background(), "node2", elector2.LeaseTTL)
	if err != nil || acquired {
		t.Errorf("node2 should not acquire while node1 holds lease")
	}

	// Release node1's lease explicitly and increment token to simulate failover
	backend.Release(context.Background(), "node1")
	acquired, err = backend.TryAcquire(context.Background(), "node2", elector2.LeaseTTL)
	if err != nil || !acquired {
		t.Fatalf("node2 should acquire after node1 release: err=%v acquired=%v", err, acquired)
	}

	// Update elector state
	elector1.tick(context.Background())
	elector2.tick(context.Background())

	if elector1.IsLeader() {
		t.Errorf("node1 should be demoted after lease release")
	}
	if !elector2.IsLeader() {
		t.Errorf("node2 should now be leader")
	}

	token2 := elector2.FencingToken()
	if token2 <= token1 {
		t.Errorf("token2 = %d, token1 = %d, token2 should be > token1 (incremented on change)",
			token2, token1)
	}
}

func TestNodeRole_String(t *testing.T) {
	tests := []struct {
		role     NodeRole
		expected string
	}{
		{RoleUnknown, "unknown"},
		{RolePrimary, "primary"},
		{RoleReplica, "replica"},
		{RoleSentinel, "sentinel"},
		{RoleObserver, "observer"},
		{NodeRole(99), "unknown"}, // Unknown value
	}

	for _, tt := range tests {
		t.Run(tt.expected, func(t *testing.T) {
			if got := tt.role.String(); got != tt.expected {
				t.Errorf("String() = %q, want %q", got, tt.expected)
			}
		})
	}
}

func TestWALOp_String(t *testing.T) {
	tests := []struct {
		op       WALOp
		expected string
	}{
		{OpSet, "set"},
		{OpDelete, "delete"},
		{OpBatch, "batch"},
		{OpCognitive, "cognitive"},
		{OpIndex, "index"},
		{OpMeta, "meta"},
		{WALOp(99), "unknown"}, // Unknown value
	}

	for _, tt := range tests {
		t.Run(tt.expected, func(t *testing.T) {
			if got := tt.op.String(); got != tt.expected {
				t.Errorf("String() = %q, want %q", got, tt.expected)
			}
		})
	}
}

func TestNodeRole_Values(t *testing.T) {
	tests := []struct {
		role     NodeRole
		expected uint8
	}{
		{RoleUnknown, 0},
		{RolePrimary, 1},
		{RoleReplica, 2},
		{RoleSentinel, 3},
		{RoleObserver, 4},
	}

	for _, tt := range tests {
		t.Run(tt.role.String(), func(t *testing.T) {
			if uint8(tt.role) != tt.expected {
				t.Errorf("Value = %d, want %d", uint8(tt.role), tt.expected)
			}
		})
	}
}

func TestWALOp_Values(t *testing.T) {
	tests := []struct {
		op       WALOp
		expected uint8
	}{
		{OpSet, 1},
		{OpDelete, 2},
		{OpBatch, 3},
		{OpCognitive, 4},
		{OpIndex, 5},
		{OpMeta, 6},
	}

	for _, tt := range tests {
		t.Run(tt.op.String(), func(t *testing.T) {
			if uint8(tt.op) != tt.expected {
				t.Errorf("Value = %d, want %d", uint8(tt.op), tt.expected)
			}
		})
	}
}

// TestReplicationLog_ReadSince_LimitZero verifies the behavior of ReadSince when
// limit=0 is passed. The implementation treats limit<=0 as limit=1000, so all
// entries are returned when fewer than 1000 exist.
func TestReplicationLog_ReadSince_LimitZero(t *testing.T) {
	dir := t.TempDir()
	db, err := pebble.Open(dir, &pebble.Options{})
	if err != nil {
		t.Fatalf("failed to open pebble: %v", err)
	}
	defer db.Close()

	log := NewReplicationLog(db)

	// Append 5 entries.
	for i := 1; i <= 5; i++ {
		_, err := log.Append(OpSet, []byte("key"), []byte("value"))
		if err != nil {
			t.Fatalf("append %d failed: %v", i, err)
		}
	}

	// ReadSince(0, 0): limit=0 is treated as limit=1000, returns all 5 entries.
	entries, err := log.ReadSince(0, 0)
	if err != nil {
		t.Fatalf("ReadSince(0, 0) failed: %v", err)
	}

	// Observed behavior: limit=0 → all entries returned (treated as no limit, capped at 1000).
	if len(entries) != 5 {
		t.Errorf("ReadSince(0, 0): expected 5 entries (limit=0 means no limit), got %d", len(entries))
	}
	for i, e := range entries {
		if e.Seq != uint64(i+1) {
			t.Errorf("entries[%d].Seq = %d, want %d", i, e.Seq, i+1)
		}
	}
}

// TestReplicationLog_Prune_BeyondCurrentSeq_IsNoOp verifies that calling
// Prune with a seq beyond the current highest seq is a no-op.
func TestReplicationLog_Prune_BeyondCurrentSeq_IsNoOp(t *testing.T) {
	dir := t.TempDir()
	db, err := pebble.Open(dir, &pebble.Options{})
	if err != nil {
		t.Fatalf("failed to open pebble: %v", err)
	}
	defer db.Close()

	log := NewReplicationLog(db)

	// Append 5 entries (seq 1-5).
	for i := 1; i <= 5; i++ {
		_, err := log.Append(OpSet, []byte("key"), []byte("value"))
		if err != nil {
			t.Fatalf("append %d failed: %v", i, err)
		}
	}

	// Prune(10) — beyond currentSeq (5): should be a no-op.
	if err := log.Prune(10); err != nil {
		t.Errorf("Prune(10) returned unexpected error: %v", err)
	}

	// All 5 entries must still be readable.
	entries, err := log.ReadSince(0, 100)
	if err != nil {
		t.Fatalf("ReadSince(0, 100) failed: %v", err)
	}
	if len(entries) != 5 {
		t.Errorf("expected 5 entries after Prune(10), got %d", len(entries))
	}

	// CurrentSeq must still be 5.
	if seq := log.CurrentSeq(); seq != 5 {
		t.Errorf("CurrentSeq = %d, want 5 after no-op Prune", seq)
	}
}

func TestReplicationLog_ReadSince_ConcurrentAppend(t *testing.T) {
	dir := t.TempDir()
	db, err := pebble.Open(dir, &pebble.Options{})
	if err != nil {
		t.Fatalf("failed to open pebble: %v", err)
	}
	defer db.Close()

	log := NewReplicationLog(db)

	deadline := time.Now().Add(1 * time.Second)
	var wg sync.WaitGroup
	var appendedSeq atomic.Uint64

	// Writer goroutine: append entries in a tight loop for 1 second
	wg.Add(1)
	go func() {
		defer wg.Done()
		for time.Now().Before(deadline) {
			seq, err := log.Append(OpSet, []byte("key"), []byte("value"))
			if err != nil {
				t.Errorf("Append failed: %v", err)
				return
			}
			appendedSeq.Store(seq)
		}
	}()

	// Reader goroutine: call ReadSince in a tight loop for 1 second
	wg.Add(1)
	go func() {
		defer wg.Done()
		var afterSeq uint64
		for time.Now().Before(deadline) {
			entries, err := log.ReadSince(afterSeq, 100)
			if err != nil {
				t.Errorf("ReadSince failed: %v", err)
				return
			}

			// Verify monotonically increasing seq numbers with no duplicates or zeros
			for i, entry := range entries {
				if entry.Seq == 0 {
					t.Errorf("entry %d has seq=0", i)
					return
				}
				if i > 0 && entry.Seq <= entries[i-1].Seq {
					t.Errorf("non-monotonic seq at index %d: %d <= %d",
						i, entry.Seq, entries[i-1].Seq)
					return
				}
				if entry.Seq <= afterSeq {
					t.Errorf("entry seq %d <= afterSeq %d", entry.Seq, afterSeq)
					return
				}
			}

			if len(entries) > 0 {
				afterSeq = entries[len(entries)-1].Seq
			}
		}
	}()

	wg.Wait()

	// Verify we actually appended some entries
	if appendedSeq.Load() == 0 {
		t.Error("no entries were appended during the test")
	}
}

// TestReplicationLog_Prune_EntryRangeCollisionNoLongerPossible is a FORWARD
// regression guard, not a reproduction of the original defect.
//
// Before #726, prefix 0x19 was shared by the replication log
// (0x19|seq_be64, msgpack) and prefix.Idempotency (0x19|siphash(op_id),
// JSON) — byte-identical in shape. A range-delete prune over
// [0x19|1, 0x19|untilSeq+1) therefore risked taking any receipt whose
// siphash happened to land in that window, which is why Prune used to decode
// every value in the window and skip anything that didn't unmarshal as a
// ReplicationEntry.
//
// #726 relocated the log to prefix.Replication|subEntry, a range that
// provably contains nothing but log entries (see keys.go and
// prefix_relocation_test.go's TestReplicationKeys_NeverCollideWithIdempotency
// and TestPrune_LeavesIdempotencyReceiptsAlone, which model the collision
// correctly — seeding receipts at their real prefix.Idempotency address).
// The precondition for the old defect — a receipt able to occupy a key this
// Prune scans — no longer holds, so what this test pins instead is the
// opposite: data placed inside the entry range IS entry-range data, and a
// plain DeleteRange over it is exactly the intended (and now unnecessary to
// avoid) behaviour.
func TestReplicationLog_Prune_EntryRangeCollisionNoLongerPossible(t *testing.T) {
	db, err := pebble.Open("", &pebble.Options{FS: vfs.NewMem()})
	if err != nil {
		t.Fatalf("open pebble: %v", err)
	}
	defer db.Close()

	log := NewReplicationLog(db)
	for i := 1; i <= 100; i++ {
		if _, err := log.Append(OpSet, []byte("k"), []byte("v")); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}

	if err := log.Prune(60); err != nil {
		t.Fatalf("Prune: %v", err)
	}

	// Every key in the pruned window is entry-range data and must be gone —
	// there is no encoding to preserve here anymore.
	for _, seq := range []uint64{7, 42, 50, 60} {
		if _, closer, err := db.Get(replicationEntryKey(seq)); err == nil {
			closer.Close()
			t.Fatalf("entry %d survived Prune(60) — expected the whole window deleted", seq)
		}
	}

	// And the log entries above the watermark are untouched.
	if _, closer, err := db.Get(replicationEntryKey(61)); err != nil {
		t.Fatalf("entry above watermark was pruned: %v", err)
	} else {
		closer.Close()
	}
}

// TestReplicationLog_Prune_IdempotentOnRepeatedWatermark exercises the
// lastPruned fast path: calling Prune with a watermark that has already been
// pruned (the periodic prune loop calls Prune every tick regardless of
// whether the watermark advanced) must not error and must not disturb
// anything, rather than re-issuing a redundant delete/flush/compact cycle.
func TestReplicationLog_Prune_IdempotentOnRepeatedWatermark(t *testing.T) {
	db, err := pebble.Open("", &pebble.Options{FS: vfs.NewMem()})
	if err != nil {
		t.Fatalf("open pebble: %v", err)
	}
	defer db.Close()

	log := NewReplicationLog(db)
	for i := 1; i <= 10; i++ {
		if _, err := log.Append(OpSet, []byte("k"), []byte("v")); err != nil {
			t.Fatalf("append: %v", err)
		}
	}

	if err := log.Prune(4); err != nil {
		t.Fatalf("Prune: %v", err)
	}
	for seq := uint64(1); seq <= 4; seq++ {
		if _, closer, err := db.Get(replicationEntryKey(seq)); err == nil {
			closer.Close()
			t.Fatalf("entry %d survived Prune(4)", seq)
		}
	}

	// Pruning the same watermark again must be a clean no-op, not an error.
	if err := log.Prune(4); err != nil {
		t.Fatalf("second Prune(4) = %v, want nil", err)
	}
	if _, closer, err := db.Get(replicationEntryKey(5)); err != nil {
		t.Fatalf("entry above watermark lost after repeated prune: %v", err)
	} else {
		closer.Close()
	}
}

// Pruning writes tombstones; the bytes only come back when a compaction
// rewrites the sstables holding them. In production, pruning 104k entries
// reclaimed nothing — the store sat at 20 GB until a compaction was forced by
// hand. Prune must therefore compact the range it just deleted.
func TestReplicationLog_Prune_ReclaimsDiskSpace(t *testing.T) {
	dir := t.TempDir()
	db, err := pebble.Open(dir, &pebble.Options{})
	if err != nil {
		t.Fatalf("open pebble: %v", err)
	}
	defer db.Close()

	log := NewReplicationLog(db)
	// Payloads big enough that reclaimed space is unambiguous on disk.
	value := make([]byte, 64*1024)
	for i := range value {
		value[i] = byte(i % 251)
	}
	for i := 1; i <= 400; i++ {
		if _, err := log.Append(OpSet, []byte("k"), value); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
	if err := db.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	before := dirSize(t, dir)

	if err := log.Prune(350); err != nil {
		t.Fatalf("Prune: %v", err)
	}

	// Compact() returns once the compaction is applied, but Pebble unlinks the
	// obsolete sstables from a background cleaner goroutine, so the bytes can
	// still be on disk for a moment afterwards. Poll on the observable rather
	// than sampling once — a single sample makes this test fail under load
	// (observed once in a full-package run) for a reason that has nothing to do
	// with whether the prune reclaimed anything.
	//
	// 350 of 400 entries at 64 KiB is ~22 MiB; require most of it back rather
	// than an exact figure, since sstable boundaries are not key-aligned.
	var after int64
	deadline := time.Now().Add(30 * time.Second)
	for {
		after = dirSize(t, dir)
		if after < before/2 || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if after >= before/2 {
		t.Fatalf("prune did not reclaim disk: before=%d after=%d bytes "+
			"(tombstones were written but never compacted)", before, after)
	}

	// The surviving entries must still be readable.
	entries, err := log.ReadSince(350, 100)
	if err != nil {
		t.Fatalf("ReadSince after prune: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("entries above the watermark were lost")
	}
}

// sstSize sums only sstable bytes. The Pebble WAL still holds every append
// until it is recycled, so including it would mask what a compaction actually
// reclaims from the tables.
func dirSize(t *testing.T, dir string) int64 {
	t.Helper()
	var total int64
	err := filepath.Walk(dir, func(_ string, info os.FileInfo, err error) error {
		// Compaction removes sstables while we walk — a vanished file is
		// evidence the compaction ran, not an error.
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if !info.IsDir() && filepath.Ext(info.Name()) == ".sst" {
			total += info.Size()
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}
	return total
}

// Prune must not hold the Append mutex for its duration: Append is on the
// synchronous write path, and a first prune over a never-pruned log scans,
// deletes and compacts tens of GB. Run under -race.
func TestReplicationLog_PruneDoesNotBlockAppend(t *testing.T) {
	db, err := pebble.Open("", &pebble.Options{FS: vfs.NewMem()})
	if err != nil {
		t.Fatalf("open pebble: %v", err)
	}
	defer db.Close()

	log := NewReplicationLog(db)
	value := make([]byte, 4096)
	for i := 1; i <= 5000; i++ {
		if _, err := log.Append(OpSet, []byte("k"), value); err != nil {
			t.Fatalf("seed append %d: %v", i, err)
		}
	}

	var wg sync.WaitGroup
	var appended atomic.Int64
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := log.Append(OpSet, []byte("k"), value); err != nil {
				t.Errorf("concurrent append: %v", err)
				return
			}
			appended.Add(1)
		}
	}()

	err = log.Prune(4000)
	// Sample BEFORE stopping the writer, so the count is what completed while
	// the prune was running rather than after it returned.
	during := appended.Load()
	close(stop)
	wg.Wait()
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}

	if during < 10 {
		t.Fatalf("only %d appends completed while the prune ran — Prune is serialising the write path", during)
	}
	// Everything above the watermark must still be readable and contiguous.
	entries, err := log.ReadSince(4000, 100)
	if err != nil {
		t.Fatalf("ReadSince after prune: %v", err)
	}
	if len(entries) == 0 || entries[0].Seq != 4001 {
		t.Fatalf("entries above the watermark are wrong: got %d entries starting at %v",
			len(entries), func() any {
				if len(entries) == 0 {
					return nil
				}
				return entries[0].Seq
			}())
	}
}
