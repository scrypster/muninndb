package replication_test

// External test package: the #869 regression spans the replication/storage
// seam, and storage is imported by this package's own dependency chain
// (replication ← cognitive ← storage), so the test can only exist outside
// both. That mirrors the production shape anyway — the two halves of the fix
// are joined by a callback in cmd/muninn/server.go, not by an import.

import (
	"context"
	"testing"

	"github.com/scrypster/muninndb/internal/replication"
	"github.com/scrypster/muninndb/internal/storage"
)

// The #869 regression, end to end.
//
// A follower that has RECALLED an engram holds it in the L1 cache. A
// replicated mutation of that engram (a soft-delete here — the privacy-worst
// instance, but the same path carries evolve supersession, trust changes, tag
// updates and restore) is committed by the applier straight to Pebble, which
// the cache shadows. Without per-key invalidation the follower keeps serving
// the cached copy as active until the process restarts — and because every
// hit refreshes recency, the hottest memories are exactly the ones that never
// heal.
//
// The wiring below is the same shape cmd/muninn/server.go uses: the primary
// store's RepLogAppend hook captures what the storage layer would ship, the
// follower's applier applies it, and SetInvalidate feeds applied keys back
// into the follower store. The test fails without the applier's invalidation
// calls (RED-checked by reverting them): the step-3 read then returns the
// warmed StateActive copy instead of the applied StateSoftDeleted.
func TestReplicatedSoftDelete_InvalidatesFollowerCache(t *testing.T) {
	ctx := context.Background()

	// Primary: a store whose replication hook captures every shipped entry.
	pdb, err := storage.OpenPebble(t.TempDir(), storage.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	var shipped []replication.ReplicationEntry
	var seq uint64
	primary := storage.NewPebbleStore(pdb, storage.PebbleStoreConfig{
		CacheSize: 1000,
		RepLogAppend: func(op uint8, key, value []byte) error {
			seq++
			shipped = append(shipped, replication.ReplicationEntry{
				Seq: seq,
				Op:  replication.WALOp(op),
				Key: append([]byte(nil), key...),
				// Repr() buffers are reused after Close; the real stream copies
				// via msgpack marshalling, so the capture copies too.
				Value: append([]byte(nil), value...),
			})
			return nil
		},
	})
	defer primary.Close()

	// Follower: its own Pebble, its own store, an applier on the same DB,
	// wired exactly as server.go wires them.
	fdb, err := storage.OpenPebble(t.TempDir(), storage.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	follower := storage.NewPebbleStore(fdb, storage.PebbleStoreConfig{CacheSize: 1000})
	defer follower.Close()
	applier := replication.NewApplier(fdb)
	applier.SetInvalidate(follower.InvalidateReplicatedKey)

	drain := func(stage string) {
		t.Helper()
		if len(shipped) == 0 {
			t.Fatalf("%s: primary shipped no replication entries — capture hook not firing", stage)
		}
		for _, e := range shipped {
			if err := applier.Apply(e); err != nil {
				t.Fatalf("%s: apply seq %d: %v", stage, e.Seq, err)
			}
		}
		shipped = shipped[:0]
	}

	var ws [8]byte

	// 1. Create on the primary; stream to the follower.
	id, err := primary.WriteEngram(ctx, ws, &storage.Engram{
		Concept: "operator secret",
		Content: "a memory the operator will explicitly forget",
	})
	if err != nil {
		t.Fatal(err)
	}
	drain("create")

	// 2. Recall on the follower — the read that loads the L1 cache and arms
	// the bug. Asserting state here also proves the create replicated.
	warmed, err := follower.GetEngram(ctx, ws, id)
	if err != nil {
		t.Fatalf("follower read after create: %v", err)
	}
	if warmed.State != storage.StateActive {
		t.Fatalf("follower state after create = %v, want StateActive", warmed.State)
	}

	// 3. Forget on the primary; stream the deletion; recall again WITHOUT any
	// restart. The follower must observe the deletion through its warm cache.
	if err := primary.SoftDelete(ctx, ws, id); err != nil {
		t.Fatal(err)
	}
	drain("soft-delete")

	got, err := follower.GetEngram(ctx, ws, id)
	if err != nil {
		t.Fatalf("follower read after replicated delete: %v", err)
	}
	if got.State != storage.StateSoftDeleted {
		t.Fatalf("follower state after replicated soft-delete = %v, want StateSoftDeleted — "+
			"the warmed cache entry survived the applied mutation (#869)", got.State)
	}
}
