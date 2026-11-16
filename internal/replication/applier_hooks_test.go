package replication

import (
	"testing"

	"github.com/cockroachdb/pebble"
	"github.com/cockroachdb/pebble/vfs"
)

// The applier hands every committed batch to onCommitted and calls onReset
// when a snapshot rebases the cursor — the store's derived retired set depends
// on both (storage/retired.go).
func TestApplier_CommitHooks(t *testing.T) {
	db, err := pebble.Open("applier-hooks", &pebble.Options{FS: vfs.NewMem()})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	a := NewApplier(db)
	var reprs [][]byte
	resets := 0
	a.SetCommitHooks(func(r []byte) { reprs = append(reprs, r) }, func() { resets++ })

	if err := a.Apply(ReplicationEntry{Seq: 1, Op: OpSet, Key: []byte("k1"), Value: []byte("v")}); err != nil {
		t.Fatal(err)
	}
	b := db.NewBatch()
	_ = b.Set([]byte("k2"), []byte("v"), nil)
	if err := a.Apply(ReplicationEntry{Seq: 2, Op: OpBatch, Value: b.Repr()}); err != nil {
		t.Fatal(err)
	}
	b.Close()
	if len(reprs) != 2 {
		t.Fatalf("onCommitted called %d times for 2 applied entries", len(reprs))
	}
	if err := a.ResetTo(2); err != nil { // same seq: the cursor does not move, the state was still replaced
		t.Fatal(err)
	}
	if resets != 1 {
		t.Fatalf("onReset called %d times after a resnapshot rebase, want 1", resets)
	}
}

// A commit that fails must still reach onCommitted: it may have landed, and
// the consumers (the store's retired set and caches) re-derive from Pebble
// rather than trusting the batch, so being told is always safe and not being
// told is fail-open.
func TestApplier_CommitHooksRunWhenCommitFails(t *testing.T) {
	fs := vfs.NewMem()
	db, err := pebble.Open("applier-ro", &pebble.Options{FS: fs})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	ro, err := pebble.Open("applier-ro", &pebble.Options{FS: fs, ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	a := NewApplier(ro)
	calls := 0
	a.SetCommitHooks(func([]byte) { calls++ }, nil)

	if err := a.Apply(ReplicationEntry{Seq: 1, Op: OpSet, Key: []byte("k1"), Value: []byte("v")}); err == nil {
		t.Fatal("expected the commit on a read-only DB to fail")
	}
	b := ro.NewBatch()
	_ = b.Set([]byte("k2"), []byte("v"), nil)
	repr := append([]byte(nil), b.Repr()...)
	b.Close()
	if err := a.Apply(ReplicationEntry{Seq: 2, Op: OpBatch, Value: repr}); err == nil {
		t.Fatal("expected the batch commit on a read-only DB to fail")
	}
	if calls != 2 {
		t.Fatalf("onCommitted ran %d times for 2 failed commits, want 2", calls)
	}
}
