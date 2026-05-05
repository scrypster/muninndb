package storage_test

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/cockroachdb/pebble"
	"github.com/scrypster/muninndb/internal/storage"
)

func openRepHookDB(t *testing.T) (*pebble.DB, func()) {
	t.Helper()
	dir := t.TempDir()
	db, err := pebble.Open(dir, &pebble.Options{})
	if err != nil {
		t.Fatalf("open pebble: %v", err)
	}
	return db, func() { _ = db.Close() }
}

// TestRepLogHook_NilCallback verifies WriteEngram does not panic when
// RepLogAppend is nil (non-cluster deployments).
func TestRepLogHook_NilCallback(t *testing.T) {
	db, cleanup := openRepHookDB(t)
	defer cleanup()

	store := storage.NewPebbleStore(db, storage.PebbleStoreConfig{})
	ws := store.VaultPrefix("nil-hook-vault")
	ctx := context.Background()

	if _, err := store.WriteEngram(ctx, ws, &storage.Engram{
		Concept: "no callback",
		Content: "should not panic",
	}); err != nil {
		t.Fatalf("WriteEngram with nil callback: %v", err)
	}
}

// TestRepLogHook_WriteEngram fires once per WriteEngram call.
func TestRepLogHook_WriteEngram(t *testing.T) {
	db, cleanup := openRepHookDB(t)
	defer cleanup()

	var callCount atomic.Int32
	store := storage.NewPebbleStore(db, storage.PebbleStoreConfig{
		RepLogAppend: func(op uint8, key, value []byte) error {
			if op == 3 {
				callCount.Add(1)
			}
			return nil
		},
	})

	ws := store.VaultPrefix("write-vault")
	ctx := context.Background()

	if _, err := store.WriteEngram(ctx, ws, &storage.Engram{
		Concept: "test",
		Content: "body",
	}); err != nil {
		t.Fatalf("WriteEngram: %v", err)
	}

	if got := callCount.Load(); got != 1 {
		t.Errorf("WriteEngram: RepLogAppend called %d times, want 1", got)
	}
}

// TestRepLogHook_EndToEnd verifies that the batch repr captured via RepLogAppend
// can be applied to a second Pebble instance (simulating what Applier.Apply does
// for OpBatch). This is the direct regression test for issue #409.
func TestRepLogHook_EndToEnd(t *testing.T) {
	db, cleanup := openRepHookDB(t)
	defer cleanup()

	dir2 := t.TempDir()
	db2, err := pebble.Open(dir2, &pebble.Options{})
	if err != nil {
		t.Fatalf("open replica pebble: %v", err)
	}
	defer db2.Close()

	var capturedRepr []byte
	store := storage.NewPebbleStore(db, storage.PebbleStoreConfig{
		RepLogAppend: func(op uint8, key, value []byte) error {
			if op == 3 && capturedRepr == nil {
				capturedRepr = append([]byte(nil), value...)
			}
			return nil
		},
	})

	ws := store.VaultPrefix("e2e-vault")
	ctx := context.Background()

	if _, err := store.WriteEngram(ctx, ws, &storage.Engram{
		Concept: "end-to-end",
		Content: "replica should receive this",
	}); err != nil {
		t.Fatalf("WriteEngram: %v", err)
	}

	if len(capturedRepr) == 0 {
		t.Fatal("no batch repr captured by RepLogAppend")
	}

	// Apply to replica DB — exactly what Applier.Apply does for OpBatch.
	replicaBatch := db2.NewBatch()
	defer replicaBatch.Close()
	if err := replicaBatch.SetRepr(capturedRepr); err != nil {
		t.Fatalf("SetRepr on replica: %v", err)
	}
	if err := replicaBatch.Commit(pebble.NoSync); err != nil {
		t.Fatalf("replica batch commit: %v", err)
	}

	// Confirm replica has the 0x01-prefixed engram key.
	iter, err := db2.NewIter(&pebble.IterOptions{
		LowerBound: []byte{0x01},
		UpperBound: []byte{0x02},
	})
	if err != nil {
		t.Fatalf("NewIter: %v", err)
	}
	defer iter.Close()

	if !iter.First() {
		t.Fatal("replica DB has no 0x01 engram keys after applying batch repr")
	}
}
