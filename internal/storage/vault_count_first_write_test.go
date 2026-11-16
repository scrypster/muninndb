package storage

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/scrypster/muninndb/internal/storage/keys"
)

// A vault's engram count is seeded from a scan the first time anything touches
// the vault in a process. Every path that counts a write used to seed it AFTER
// committing that write, so the scan already saw the new engrams and the
// following Add counted them a second time: one high after a single write,
// 2k after a first batch of k. The count is persisted and trusted on restart,
// so the inflation was permanent, and PruneVault (excess = count - MaxEngrams)
// deleted real memories to pay for it. These tests pin every counting path.

func vcEngram(i int) *Engram {
	return &Engram{Concept: fmt.Sprintf("apiary note %d", i), Content: fmt.Sprintf("hive %d was inspected and the brood looked even", i)}
}

func vcWS(name string) [8]byte {
	var ws [8]byte
	copy(ws[:], name)
	return ws
}

func TestVaultCount_FirstSingleWritesNotInflated(t *testing.T) {
	ps := openTestStore(t)
	ctx := context.Background()
	ws := vcWS("vc-single")
	for i := 0; i < 3; i++ {
		if _, err := ps.WriteEngram(ctx, ws, vcEngram(i)); err != nil {
			t.Fatal(err)
		}
	}
	if got := ps.GetVaultCount(ctx, ws); got != 3 {
		t.Fatalf("GetVaultCount = %d after 3 writes into a fresh vault, want 3", got)
	}
}

func TestVaultCount_FirstBatchNotInflated(t *testing.T) {
	ps := openTestStore(t)
	ctx := context.Background()
	ws := vcWS("vc-batch")
	items := make([]EngramBatchItem, 5)
	for i := range items {
		items[i] = EngramBatchItem{WSPrefix: ws, Engram: vcEngram(i)}
	}
	_, errs := ps.WriteEngramBatch(ctx, items)
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := ps.GetVaultCount(ctx, ws); got != 5 {
		t.Fatalf("GetVaultCount = %d after a first batch of 5, want 5", got)
	}
}

func TestVaultCount_FirstStoreBatchNotInflated(t *testing.T) {
	ps := openTestStore(t)
	ctx := context.Background()
	ws := vcWS("vc-storebatch")
	b := ps.NewBatch()
	for i := 0; i < 4; i++ {
		if err := b.WriteEngram(ctx, ws, vcEngram(i)); err != nil {
			t.Fatal(err)
		}
	}
	if err := b.Commit(); err != nil {
		t.Fatal(err)
	}
	b.Discard()
	if got := ps.GetVaultCount(ctx, ws); got != 4 {
		t.Fatalf("GetVaultCount = %d after a first StoreBatch of 4, want 4", got)
	}
}

// A delete as the first touch in a process (the count key absent, as for a
// vault written before 0x15 existed) is the mirror image: seeding after the
// delete commits leaves the scan already one short, and the decrement takes
// another.
func TestVaultCount_FirstTouchIsDeleteNotDeflated(t *testing.T) {
	ps := openTestStore(t)
	ctx := context.Background()
	ws := vcWS("vc-delete")
	var ids []ULID
	for i := 0; i < 3; i++ {
		id, err := ps.WriteEngram(ctx, ws, vcEngram(i))
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	// A new process over a vault with no persisted count.
	ps.FlushCounters()
	ps.vaultCounters.Delete(ws)
	ps.counterFlush.Delete(ws)
	if err := ps.db.Delete(keys.VaultCountKey(ws), nil); err != nil {
		t.Fatal(err)
	}
	if err := ps.DeleteEngram(ctx, ws, ids[0]); err != nil {
		t.Fatal(err)
	}
	if got := ps.GetVaultCount(ctx, ws); got != 2 {
		t.Fatalf("GetVaultCount = %d after deleting 1 of 3 as the first touch, want 2", got)
	}
}

// Concurrent first writes to one fresh vault, single and batched, must count
// each engram exactly once. Seeding before the commit is what makes this hold:
// sync.Once runs one scan, and no writer can commit until its own seed call
// has returned, so the scan never sees an engram that will be added again.
func TestVaultCount_ConcurrentFirstWritesCountEachOnce(t *testing.T) {
	ps := openTestStore(t)
	ctx := context.Background()
	const vaults, singles, batches, perBatch = 20, 6, 3, 4
	for v := 0; v < vaults; v++ {
		ws := vcWS(fmt.Sprintf("vcc%05d", v)) // 8 bytes: a distinct prefix per vault
		var wg sync.WaitGroup
		start := make(chan struct{})
		errc := make(chan error, singles+batches)
		for g := 0; g < singles; g++ {
			wg.Add(1)
			go func(g int) {
				defer wg.Done()
				<-start
				if _, err := ps.WriteEngram(ctx, ws, vcEngram(g)); err != nil {
					errc <- err
				}
			}(g)
		}
		for g := 0; g < batches; g++ {
			wg.Add(1)
			go func(g int) {
				defer wg.Done()
				<-start
				items := make([]EngramBatchItem, perBatch)
				for i := range items {
					items[i] = EngramBatchItem{WSPrefix: ws, Engram: vcEngram(100 + g*perBatch + i)}
				}
				_, errs := ps.WriteEngramBatch(ctx, items)
				for _, err := range errs {
					if err != nil {
						errc <- err
					}
				}
			}(g)
		}
		close(start)
		wg.Wait()
		close(errc)
		for err := range errc {
			t.Fatal(err)
		}
		want := int64(singles + batches*perBatch)
		if got := ps.GetVaultCount(ctx, ws); got != want {
			t.Fatalf("vault %d: GetVaultCount = %d after %d concurrent first writes, want %d", v, got, want, want)
		}
		if scanned, _ := ps.countEngramsForVault(ctx, ws); scanned != want {
			t.Fatalf("vault %d: fixture: %d engrams stored, want %d", v, scanned, want)
		}
	}
}
