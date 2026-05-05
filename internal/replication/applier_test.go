package replication

import (
	"encoding/binary"
	"testing"

	"github.com/cockroachdb/pebble"
)

func TestApplier_FreshStart(t *testing.T) {
	dir := t.TempDir()
	db, err := pebble.Open(dir, &pebble.Options{})
	if err != nil {
		t.Fatalf("failed to open pebble: %v", err)
	}
	defer db.Close()

	applier := NewApplier(db)
	if applier.LastApplied() != 0 {
		t.Errorf("LastApplied() = %d, want 0 on fresh start", applier.LastApplied())
	}
}

func TestApplier_PersistsLastApplied(t *testing.T) {
	dir := t.TempDir()

	// Phase 1: apply entries 1, 2, 3
	db, err := pebble.Open(dir, &pebble.Options{})
	if err != nil {
		t.Fatalf("failed to open pebble: %v", err)
	}

	applier := NewApplier(db)
	for i := 1; i <= 3; i++ {
		entry := ReplicationEntry{
			Seq:   uint64(i),
			Op:    OpSet,
			Key:   []byte("key"),
			Value: []byte("value"),
		}
		if err := applier.Apply(entry); err != nil {
			t.Fatalf("Apply(%d) failed: %v", i, err)
		}
	}

	if applier.LastApplied() != 3 {
		t.Errorf("LastApplied() = %d, want 3", applier.LastApplied())
	}

	db.Close()

	// Phase 2: reopen and verify lastApplied was persisted
	db, err = pebble.Open(dir, &pebble.Options{})
	if err != nil {
		t.Fatalf("failed to reopen pebble: %v", err)
	}
	defer db.Close()

	applier2 := NewApplier(db)
	if applier2.LastApplied() != 3 {
		t.Errorf("after restart, LastApplied() = %d, want 3", applier2.LastApplied())
	}
}

func TestApplier_ResumeAfterRestart(t *testing.T) {
	dir := t.TempDir()

	// Phase 1: apply entries 1, 2, 3
	db, err := pebble.Open(dir, &pebble.Options{})
	if err != nil {
		t.Fatalf("failed to open pebble: %v", err)
	}

	applier := NewApplier(db)
	for i := 1; i <= 3; i++ {
		entry := ReplicationEntry{
			Seq:   uint64(i),
			Op:    OpSet,
			Key:   []byte("k"),
			Value: []byte("v"),
		}
		if err := applier.Apply(entry); err != nil {
			t.Fatalf("Apply(%d) failed: %v", i, err)
		}
	}
	db.Close()

	// Phase 2: reopen
	db, err = pebble.Open(dir, &pebble.Options{})
	if err != nil {
		t.Fatalf("failed to reopen pebble: %v", err)
	}
	defer db.Close()

	applier2 := NewApplier(db)

	// Re-apply entry 2 — should be skipped (idempotent)
	stale := ReplicationEntry{Seq: 2, Op: OpSet, Key: []byte("stale"), Value: []byte("ignored")}
	if err := applier2.Apply(stale); err != nil {
		t.Fatalf("Apply(stale) failed: %v", err)
	}
	if applier2.LastApplied() != 3 {
		t.Errorf("LastApplied() = %d after stale apply, want 3", applier2.LastApplied())
	}

	// Apply entry 4 — should succeed
	entry4 := ReplicationEntry{Seq: 4, Op: OpSet, Key: []byte("key4"), Value: []byte("val4")}
	if err := applier2.Apply(entry4); err != nil {
		t.Fatalf("Apply(4) failed: %v", err)
	}
	if applier2.LastApplied() != 4 {
		t.Errorf("LastApplied() = %d, want 4", applier2.LastApplied())
	}

	// Verify key4 exists in Pebble
	val, closer, err := db.Get([]byte("key4"))
	if err != nil {
		t.Fatalf("key4 not found in Pebble: %v", err)
	}
	if closer != nil {
		closer.Close()
	}
	if string(val) != "val4" {
		t.Errorf("key4 value = %q, want %q", string(val), "val4")
	}
}

func TestApplier_AtomicApplyAndPersist(t *testing.T) {
	dir := t.TempDir()
	db, err := pebble.Open(dir, &pebble.Options{})
	if err != nil {
		t.Fatalf("failed to open pebble: %v", err)
	}
	defer db.Close()

	applier := NewApplier(db)

	entry := ReplicationEntry{
		Seq:   1,
		Op:    OpSet,
		Key:   []byte("atomic_key"),
		Value: []byte("atomic_value"),
	}
	if err := applier.Apply(entry); err != nil {
		t.Fatalf("Apply failed: %v", err)
	}

	// Read the data key
	val, closer, err := db.Get([]byte("atomic_key"))
	if err != nil {
		t.Fatalf("data key not found in Pebble: %v", err)
	}
	if closer != nil {
		closer.Close()
	}
	if string(val) != "atomic_value" {
		t.Errorf("data value = %q, want %q", string(val), "atomic_value")
	}

	// Read the lastApplied key
	seqVal, seqCloser, err := db.Get(lastAppliedKey())
	if err != nil {
		t.Fatalf("lastAppliedKey not found in Pebble: %v", err)
	}
	if seqCloser != nil {
		seqCloser.Close()
	}
	if len(seqVal) < 8 {
		t.Fatalf("lastApplied value too short: %d bytes", len(seqVal))
	}
	stored := binary.BigEndian.Uint64(seqVal)
	if stored != 1 {
		t.Errorf("stored lastApplied = %d, want 1", stored)
	}
}

func TestApplier_Apply_AllOpTypes(t *testing.T) {
	dir := t.TempDir()
	db, err := pebble.Open(dir, &pebble.Options{})
	if err != nil {
		t.Fatalf("failed to open pebble: %v", err)
	}
	defer db.Close()

	applier := NewApplier(db)

	// Build a valid pebble batch repr for OpBatch.
	// OpBatch.Value must be a pebble batch repr (not a raw key/value string).
	batchSrc := db.NewBatch()
	batchSrc.Set([]byte("batch_key"), []byte("batch_value"), nil)
	rawRepr := batchSrc.Repr()
	batchRepr := make([]byte, len(rawRepr))
	copy(batchRepr, rawRepr)
	batchSrc.Close()

	tests := []struct {
		name    string
		op      WALOp
		seq     uint64
		key     string
		val     string
		reprVal []byte // non-nil overrides val; used for OpBatch
	}{
		{"OpSet", OpSet, 1, "set_key", "set_value", nil},
		{"OpDelete", OpDelete, 2, "delete_key", "delete_value", nil},
		// OpBatch: Value is a pebble batch repr; the contained key is "batch_key".
		{"OpBatch", OpBatch, 3, "batch_key", "batch_value", batchRepr},
		{"OpCognitive", OpCognitive, 4, "cognitive_key", "cognitive_value", nil},
		{"OpIndex", OpIndex, 5, "index_key", "index_value", nil},
		{"OpMeta", OpMeta, 6, "meta_key", "meta_value", nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			entryVal := []byte(tt.val)
			if tt.reprVal != nil {
				entryVal = tt.reprVal
			}
			entry := ReplicationEntry{
				Seq:   tt.seq,
				Op:    tt.op,
				Key:   []byte(tt.key),
				Value: entryVal,
			}
			if err := applier.Apply(entry); err != nil {
				t.Fatalf("Apply failed: %v", err)
			}

			// For OpDelete, verify key was deleted
			if tt.op == OpDelete {
				_, _, err := db.Get([]byte(tt.key))
				if err != pebble.ErrNotFound {
					t.Errorf("expected key to be deleted, but found or got error: %v", err)
				}
			} else {
				// For all other ops, verify the key is readable with the expected value.
				val, closer, err := db.Get([]byte(tt.key))
				if err != nil {
					t.Fatalf("key %q not found in Pebble: %v", tt.key, err)
				}
				if closer != nil {
					closer.Close()
				}
				if string(val) != tt.val {
					t.Errorf("key %q value = %q, want %q", tt.key, string(val), tt.val)
				}
			}

			// Verify lastApplied was updated
			if applier.LastApplied() != tt.seq {
				t.Errorf("LastApplied = %d, want %d", applier.LastApplied(), tt.seq)
			}
		})
	}
}

// TestApplier_OpBatch verifies that the Applier correctly applies a batch repr,
// replicating all key-value pairs atomically. Regression test for issue #409.
func TestApplier_OpBatch(t *testing.T) {
	dir := t.TempDir()
	db, err := pebble.Open(dir, &pebble.Options{})
	if err != nil {
		t.Fatalf("open pebble: %v", err)
	}
	defer db.Close()

	applier := NewApplier(db)

	// Build a batch with two Set operations and capture its repr.
	srcBatch := db.NewBatch()
	key1 := []byte{0x01, 0xAA}
	val1 := []byte("value-one")
	key2 := []byte{0x01, 0xBB}
	val2 := []byte("value-two")
	srcBatch.Set(key1, val1, nil)
	srcBatch.Set(key2, val2, nil)
	// Copy the repr before closing: pebble's batch pool reuses the underlying
	// slice, so Close() zeroes the count field in-place (Reset() truncates to
	// batchHeaderLen and zeros all bytes). The copy is an independent buffer.
	rawRepr := srcBatch.Repr()
	repr := make([]byte, len(rawRepr))
	copy(repr, rawRepr)
	srcBatch.Close()

	entry := ReplicationEntry{
		Seq:   1,
		Op:    OpBatch,
		Key:   nil,
		Value: repr,
	}

	if err := applier.Apply(entry); err != nil {
		t.Fatalf("Apply OpBatch: %v", err)
	}

	for _, kv := range []struct{ k, v []byte }{{key1, val1}, {key2, val2}} {
		got, closer, err := db.Get(kv.k)
		if err != nil {
			t.Errorf("Get %x: %v", kv.k, err)
			continue
		}
		if string(got) != string(kv.v) {
			t.Errorf("Get %x = %q, want %q", kv.k, got, kv.v)
		}
		closer.Close()
	}

	if got := applier.LastApplied(); got != 1 {
		t.Errorf("LastApplied = %d, want 1", got)
	}
}

func TestApplier_Apply_AllOpTypes_Idempotent(t *testing.T) {
	dir := t.TempDir()
	db, err := pebble.Open(dir, &pebble.Options{})
	if err != nil {
		t.Fatalf("failed to open pebble: %v", err)
	}
	defer db.Close()

	applier := NewApplier(db)

	// Apply an OpCognitive entry
	entry := ReplicationEntry{
		Seq:   5,
		Op:    OpCognitive,
		Key:   []byte("cognitive_key"),
		Value: []byte("cognitive_value"),
	}
	if err := applier.Apply(entry); err != nil {
		t.Fatalf("first Apply failed: %v", err)
	}

	if applier.LastApplied() != 5 {
		t.Errorf("LastApplied = %d after first apply, want 5", applier.LastApplied())
	}

	// Re-apply the same entry — should be skipped (idempotent)
	if err := applier.Apply(entry); err != nil {
		t.Fatalf("second Apply failed: %v", err)
	}

	if applier.LastApplied() != 5 {
		t.Errorf("LastApplied = %d after re-apply, want 5", applier.LastApplied())
	}

	// Verify data is still there
	val, closer, err := db.Get([]byte("cognitive_key"))
	if err != nil {
		t.Fatalf("cognitive_key not found: %v", err)
	}
	if closer != nil {
		closer.Close()
	}
	if string(val) != "cognitive_value" {
		t.Errorf("cognitive_key value = %q, want %q", string(val), "cognitive_value")
	}
}
