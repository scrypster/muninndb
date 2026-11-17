package storage

import (
	"context"
	"encoding/binary"
	"fmt"
	"testing"
	"time"

	"github.com/cockroachdb/pebble"

	"github.com/scrypster/muninndb/internal/storage/keys"
)

// The promotion reset runs while a promoted node holds its applier and its
// election lock, so no node in the cluster accepts a write until it returns.
// It used to delete every vault's saved count with a synced write, about 5 ms
// per vault on real disk (5.6 s at 1,000 vaults). It now writes nothing: the
// store stops trusting saved counts for the rest of the process, and every
// count is seeded from a scan on next access.

// vmStaleFollower leaves ws with a saved and live count of 8 while it holds 5,
// as a follower's applied deletes leave it.
func vmStaleFollower(t *testing.T, ps *PebbleStore, ws [8]byte) []ULID {
	t.Helper()
	var ids []ULID
	for i := 0; i < 8; i++ {
		ids = append(ids, vmWrite(t, ps, ws, "follow", i))
	}
	ps.FlushCounters()
	b := ps.db.NewBatch()
	for _, id := range ids[:3] {
		if err := b.Delete(keys.EngramKey(ws, [16]byte(id)), nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := b.Commit(pebble.Sync); err != nil {
		t.Fatal(err)
	}
	b.Close()
	if got := ps.GetVaultCount(context.Background(), ws); got != 8 {
		t.Fatalf("fixture: stale count %d, want 8", got)
	}
	return ids
}

func TestVaultCountModel_PromotionResetWritesNothing(t *testing.T) {
	ctx := context.Background()
	ps := openTestStore(t)
	ws := vcWS("vm-promo-nowrite")
	vmStaleFollower(t, ps, ws)
	ps.ResetAllVaultCounts()
	if got := ps.GetVaultCount(ctx, ws); got != 5 {
		t.Fatalf("count %d after the promotion reset, the vault holds 5", got)
	}
	val, err := Get(ps.db, keys.VaultCountKey(ws))
	if err != nil || len(val) != 8 || binary.BigEndian.Uint64(val) != 8 {
		t.Fatalf("the promotion reset wrote to disk (saved count now %v, err %v): it runs while no node accepts writes, "+
			"and a synced write per vault held a 1,000-vault cluster for 5.6 s", val, err)
	}
}

// A bound on the hold itself: 1,000 vaults, each with a saved and live count,
// on real disk. Memory operations take milliseconds. The bound is generous so
// a loaded CI runner cannot trip it, and still far below one fsync per vault.
func TestVaultCountModel_PromotionResetOfManyVaultsIsFast(t *testing.T) {
	ctx := context.Background()
	dir := vmDir(t)
	ps := vmOpen(t, dir)
	defer ps.Close()
	const n = 1000
	items := make([]EngramBatchItem, n)
	vaults := make([][8]byte, n)
	for i := range items {
		vaults[i] = ps.VaultPrefix(fmt.Sprintf("vm-many-%04d", i))
		items[i] = EngramBatchItem{WSPrefix: vaults[i], Engram: &Engram{Concept: fmt.Sprintf("milepost %d", i), Content: "a milepost on the towpath"}}
	}
	if _, errs := ps.WriteEngramBatch(ctx, items); errs != nil {
		for _, e := range errs {
			if e != nil {
				t.Fatal(e)
			}
		}
	}
	ps.FlushCounters()
	t0 := time.Now()
	ps.ResetAllVaultCounts()
	held := time.Since(t0)
	t.Logf("promotion reset of %d vaults took %v", n, held)
	if held > 1500*time.Millisecond {
		t.Fatalf("promotion reset of %d vaults took %v: no node accepts writes for that long", n, held)
	}
	for _, ws := range vaults[:10] {
		if got := ps.GetVaultCount(ctx, ws); got != 1 {
			t.Fatalf("count %d after the reset, the vault holds 1", got)
		}
	}
}

// After promotion, an entry a deposed leader still streams is applied without
// moving the new leader's count. In this mode a foreign commit drops the
// counter of every vault it touches, so the next read rescans. A follower
// never enters the mode and pays nothing.
func TestVaultCountModel_LateForeignCommitAfterPromotionIsRecounted(t *testing.T) {
	ctx := context.Background()
	ps := openTestStore(t)
	ws := vcWS("vm-late-entry")
	var ids []ULID
	for i := 0; i < 7; i++ {
		ids = append(ids, vmWrite(t, ps, ws, "late", i))
	}
	ps.ResetAllVaultCounts()
	if got := ps.GetVaultCount(ctx, ws); got != 7 { // a recall reads the vault size
		t.Fatalf("fixture: count %d, want 7", got)
	}
	b := ps.db.NewBatch() // the late deletes, applied by the applier
	for _, id := range ids[:2] {
		if err := b.Delete(keys.EngramKey(ws, [16]byte(id)), nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := b.Commit(pebble.Sync); err != nil {
		t.Fatal(err)
	}
	ps.InvalidateCommittedRepr(b.Repr())
	ps.NoteCommittedRepr(b.Repr())
	b.Close()
	if got := ps.GetVaultCount(ctx, ws); got != 5 {
		t.Fatalf("count %d after 2 late deletes, the vault holds 5: the pruner would delete 2 real memories", got)
	}
}
