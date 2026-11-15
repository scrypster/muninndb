package storage

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/scrypster/muninndb/internal/storage/keys"
)

// interleaver lands a concurrent writer inside another writer's
// read-modify-write window, at the RMW seam (rmw_seam.go), deterministically.
//
// If the writer under test does NOT hold the engram's stripe lock at the seam
// (the bug), the concurrent write runs inline, to completion, between the
// read and the commit — the lost-update interleaving, every time. If it DOES
// hold it (the fix), the concurrent write is started on a goroutine; it
// blocks on the stripe and runs after the writer under test commits. wait()
// collects it. Either way the test then asserts that every field holds the
// last value its owner committed.
type interleaver struct {
	ps    *PebbleStore
	wg    sync.WaitGroup
	fired atomic.Bool
	ranIn atomic.Bool // the concurrent write ran inside the window
}

func (il *interleaver) hook(t *testing.T, op string, id ULID, concurrent func()) (restore func()) {
	t.Helper()
	return setRMWHookForTest(func(gotOp string, _ [8]byte, gotID ULID) {
		if gotOp != op || gotID != id || !il.fired.CompareAndSwap(false, true) {
			return
		}
		if !il.ps.EngramStripeHeldForTesting(id) {
			il.ranIn.Store(true)
			concurrent()
			return
		}
		il.wg.Add(1)
		go func() {
			defer il.wg.Done()
			concurrent()
		}()
	})
}

func (il *interleaver) wait(t *testing.T) {
	t.Helper()
	il.wg.Wait()
	if !il.fired.Load() {
		t.Fatal("fixture: the RMW seam never fired, so nothing was interleaved")
	}
}

func writeRMWFixture(t *testing.T, ps *PebbleStore, ws [8]byte, concept string) ULID {
	t.Helper()
	id, err := ps.WriteEngram(context.Background(), ws, &Engram{
		Concept:    concept,
		Content:    concept + " — logged by the trail crew after the spring walk",
		Confidence: 0.6,
		Relevance:  0.5,
		Stability:  30,
		Tags:       []string{"trail"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// assertLifecycle checks the record, its 0x02 slice and the 0x0B state index
// all agree on want.
func assertLifecycle(t *testing.T, ps *PebbleStore, ws [8]byte, id ULID, want LifecycleState) {
	t.Helper()
	got, err := ps.GetEngramUncached(context.Background(), ws, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != want {
		t.Fatalf("0x01 record state = %v, want %v (a lifecycle change was overwritten)", got.State, want)
	}
	metas, err := ps.GetMetadata(context.Background(), ws, []ULID{id})
	if err != nil || metas[0] == nil {
		t.Fatalf("metadata: %v", err)
	}
	if metas[0].State != want {
		t.Fatalf("0x02 metadata state = %v, want %v", metas[0].State, want)
	}
	for _, s := range []LifecycleState{StateActive, StateSoftDeleted, StateArchived} {
		v, err := Get(ps.db, keys.StateIndexKey(ws, uint8(s), [16]byte(id)))
		if err != nil {
			t.Fatal(err)
		}
		if has := v != nil; has != (s == want) {
			t.Fatalf("0x0B state index: entry for %v present=%v, want present only for %v", s, has, want)
		}
	}
}

// TestUpdateDigest_ConcurrentForgetStaysForgotten is item 2: enrichment's
// digest write read the record, a forget committed, and the digest then
// rewrote the whole record from its pre-forget copy — the memory came back
// active in 0x01/0x02 while 0x0B said soft-deleted.
func TestUpdateDigest_ConcurrentForgetStaysForgotten(t *testing.T) {
	ps := openTestStore(t)
	ctx := context.Background()
	ws := ps.VaultPrefix("trail-log")
	id := writeRMWFixture(t, ps, ws, "washout below the third switchback")

	il := &interleaver{ps: ps}
	restore := il.hook(t, "UpdateDigest", id, func() {
		if err := ps.SoftDelete(ctx, ws, id); err != nil {
			t.Errorf("SoftDelete: %v", err)
		}
	})
	if err := ps.UpdateDigest(ctx, id, "culvert needs clearing before the summer crews", nil, "", ""); err != nil {
		t.Fatalf("UpdateDigest: %v", err)
	}
	restore()
	il.wait(t)

	assertLifecycle(t, ps, ws, id, StateSoftDeleted)
	got, err := ps.GetEngramUncached(ctx, ws, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Summary != "culvert needs clearing before the summer crews" {
		t.Fatalf("the digest itself was lost: summary %q", got.Summary)
	}
}
