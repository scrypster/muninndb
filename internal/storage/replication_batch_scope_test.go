package storage

import (
	"context"
	"os"
	"testing"

	"github.com/cockroachdb/pebble"
	"github.com/cockroachdb/pebble/vfs"

	"github.com/scrypster/muninndb/internal/prefix"
)

// TestReplicatedBatchesCarryNoReplicationOrIdempotencyKeys pins the boundary
// between what a Cortex ships and what it keeps to itself, in both directions.
//
// It is also the honest negative for #826. That issue attributes a Lobe's
// replication-log bloat to the applier: "an observer receives and applies the
// Cortex's own 0x19 writes" via `reprBatch.SetRepr(entry.Value)`. That is NOT
// the mechanism. `replicateBatch` captures the repr of a DATA batch, while
// `ReplicationLog.Append` commits the entry and the seq counter in a batch of
// its own, after the fact — so no replication key has ever been inside a
// shipped repr. (The two real mechanisms are the snapshot, which streamed the
// whole DB, and the Lobe's own unconditional RepLogAppend; both are fixed
// elsewhere.)
//
// The test applies a real repr the way an Applier would, and asserts the result
// contains nothing under prefix.Replication. It also asserts no idempotency
// receipt rides along: WriteIdempotency is a direct db.Set outside any
// replicated batch, so receipts are node-local by construction, and a change
// that quietly started replicating them would put a global, non-vault-scoped
// key into the stream.
func TestReplicatedBatchesCarryNoReplicationOrIdempotencyKeys(t *testing.T) {
	dir, err := os.MkdirTemp("", "muninndb-replscope-*")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	db, err := OpenPebble(dir, DefaultOptions())
	if err != nil {
		os.RemoveAll(dir)
		t.Fatalf("open pebble: %v", err)
	}

	var reprs [][]byte
	store := NewPebbleStore(db, PebbleStoreConfig{
		CacheSize: 100,
		RepLogAppend: func(op uint8, key, value []byte) error {
			reprs = append(reprs, append([]byte(nil), value...))
			return nil
		},
	})
	t.Cleanup(func() {
		store.Close()
		os.RemoveAll(dir)
	})

	ctx := context.Background()
	ws := store.VaultPrefix("replscope-vault")

	eng := &Engram{
		Content:    "a synthetic engram for the replication-scope guard",
		Confidence: 0.9,
		Tags:       []string{"kind:guard"},
	}
	if _, err := store.WriteEngram(ctx, ws, eng); err != nil {
		t.Fatalf("WriteEngram: %v", err)
	}
	if err := store.WriteIdempotency(ctx, "op-synthetic-1", "01J0000000000000000000000"); err != nil {
		t.Fatalf("WriteIdempotency: %v", err)
	}

	if len(reprs) == 0 {
		t.Fatalf("fixture: no batch was replicated — the guard would be vacuous")
	}

	// Replay the reprs into a fresh DB exactly as Applier.Apply does.
	lobe, err := pebble.Open("", &pebble.Options{FS: vfs.NewMem()})
	if err != nil {
		t.Fatalf("open lobe: %v", err)
	}
	defer lobe.Close()
	for i, repr := range reprs {
		b := lobe.NewBatch()
		if err := b.SetRepr(repr); err != nil {
			b.Close()
			t.Fatalf("SetRepr %d: %v", i, err)
		}
		if err := b.Commit(pebble.Sync); err != nil {
			b.Close()
			t.Fatalf("commit %d: %v", i, err)
		}
		b.Close()
	}

	iter, err := lobe.NewIter(nil)
	if err != nil {
		t.Fatalf("new iter: %v", err)
	}
	defer iter.Close()
	total := 0
	for valid := iter.First(); valid; valid = iter.Next() {
		total++
		switch iter.Key()[0] {
		case prefix.Replication:
			t.Errorf("a replicated batch carried a replication key % x — a Lobe would mirror the Cortex's log", iter.Key())
		case prefix.Idempotency:
			t.Errorf("a replicated batch carried an idempotency receipt % x — receipts are node-local", iter.Key())
		}
	}
	if total == 0 {
		t.Fatalf("replaying the reprs produced no keys — the guard would be vacuous")
	}
}
