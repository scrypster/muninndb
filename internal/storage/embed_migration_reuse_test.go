package storage

import (
	"context"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cockroachdb/pebble"
	"github.com/scrypster/muninndb/internal/storage/keys"
)

// TestClearEmbedFlagsForVault_ConcurrentWritersSurvive exercises the
// micro-batch path of ClearEmbedFlagsForVault (more than 1,000 rows) while
// other goroutines commit through Pebble's shared batch pool, as the WAL
// syncer and every other writer do in a running server.
//
// The bug it pins: the function deferred Close on its FIRST batch object and
// also closed that object in the loop, so the object went back to Pebble's
// pool a second time at return. By then the pool could have handed it to
// another writer, whose in-flight commit was reset under it and whose batch
// was then shared with the next writer. Observed as a re-embed hanging
// forever in the next synced write (SetEmbedModel), with every later write
// queued behind it.
//
// The reuse needs another goroutine to pick the object up from the same P's
// pool slot, so the test pins GOMAXPROCS to 2. It is a stress test, not a
// deterministic one: with the fix reverted it failed 3 of 12 runs on an
// M-series Mac, and an independent rerun under -race failed 2 of 12. The
// deterministic guard for the shape is TestNoDeferredCloseOfAReplacedBatch;
// this test is the end-to-end evidence that the shape was the hang.
func TestClearEmbedFlagsForVault_ConcurrentWritersSurvive(t *testing.T) {
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(2))
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("embed-flags-reuse")

	const n = 2500 // three micro-batch commits plus a final one
	items := make([]EngramBatchItem, n)
	for i := range items {
		items[i] = EngramBatchItem{WSPrefix: ws, Engram: &Engram{Concept: "trail log", Content: "cleared a blowdown past the switchback"}}
	}
	ids, errs := store.WriteEngramBatch(ctx, items)
	for i, err := range errs {
		if err != nil {
			t.Fatalf("WriteEngramBatch[%d]: %v", i, err)
		}
	}

	// Writers sharing the batch pool for the whole test.
	stop := make(chan struct{})
	var wg sync.WaitGroup
	var writes atomic.Int64
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			key := []byte{0xFE, byte(w)}
			for {
				select {
				case <-stop:
					return
				default:
				}
				// Half the writers mimic the WAL syncer (a synced LogData,
				// which holds its pooled batch through the fsync), half a
				// plain unsynced writer.
				if w%2 == 0 {
					if err := store.db.LogData(nil, pebble.Sync); err != nil {
						return
					}
				} else {
					b := store.db.NewBatch()
					_ = b.Set(key, []byte("x"), nil)
					if err := b.Commit(pebble.NoSync); err != nil {
						b.Close()
						return
					}
					b.Close()
				}
				writes.Add(1)
			}
		}(w)
	}
	defer func() {
		close(stop)
		joined := make(chan struct{})
		go func() { wg.Wait(); close(joined) }()
		select {
		case <-joined:
		case <-time.After(10 * time.Second):
			t.Errorf("a concurrent writer did not return within 10s of stop (a commit is stuck)")
		}
	}()

	setFlags := func() {
		b := store.db.NewBatch()
		defer b.Close()
		for _, id := range ids {
			_ = b.Set(keys.DigestFlagsKey(id), encodeDigestFlags(0x02), nil)
		}
		if err := b.Commit(pebble.NoSync); err != nil {
			t.Fatalf("set flags: %v", err)
		}
	}

	deadline := time.Now().Add(3 * time.Second)
	rounds := 0
	for time.Now().Before(deadline) {
		setFlags()
		done := make(chan error, 1)
		go func() {
			cleared, err := store.ClearEmbedFlagsForVault(ctx, ws)
			if err == nil && cleared != n {
				t.Errorf("round %d: cleared %d, want %d", rounds, cleared, n)
			}
			if err == nil {
				err = store.SetEmbedModel(ws, "")
			}
			done <- err
		}()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("round %d: %v", rounds, err)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("round %d: clear + synced write did not return in 10s (writers committed %d batches)", rounds, writes.Load())
		}
		rounds++
	}
	t.Logf("%d clear rounds, %d concurrent writer commits", rounds, writes.Load())
}
