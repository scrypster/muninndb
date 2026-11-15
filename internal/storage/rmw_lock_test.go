package storage

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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

// assertRelevanceBucket checks the 0x10 index holds exactly one bucket for
// id among the candidate relevances, the one matching the record.
func assertRelevanceBucket(t *testing.T, ps *PebbleStore, ws [8]byte, id ULID, candidates ...float32) {
	t.Helper()
	got, err := ps.GetEngramUncached(context.Background(), ws, id)
	if err != nil {
		t.Fatal(err)
	}
	want := string(keys.RelevanceBucketKey(ws, got.Relevance, [16]byte(id)))
	for _, r := range candidates {
		k := keys.RelevanceBucketKey(ws, r, [16]byte(id))
		v, err := Get(ps.db, k)
		if err != nil {
			t.Fatal(err)
		}
		if has := v != nil; has != (string(k) == want) {
			t.Fatalf("0x10 bucket for relevance %.2f present=%v; the record says %.2f", r, has, got.Relevance)
		}
	}
}

// Forget vs UpdateRelevance (the decay worker): the relevance patch read the
// raw record, a forget committed, and the patch wrote the pre-forget bytes
// back — resurrecting the memory in 0x01/0x02 behind a soft-deleted 0x0B.
func TestUpdateRelevance_ConcurrentForgetStaysForgotten(t *testing.T) {
	ps := openTestStore(t)
	ctx := context.Background()
	ws := ps.VaultPrefix("trail-log")
	id := writeRMWFixture(t, ps, ws, "blowdown across the ridge spur")

	il := &interleaver{ps: ps}
	restore := il.hook(t, "UpdateRelevance", id, func() {
		if err := ps.SoftDelete(ctx, ws, id); err != nil {
			t.Errorf("SoftDelete: %v", err)
		}
	})
	if err := ps.UpdateRelevance(ctx, ws, id, 0.25, 31); err != nil {
		t.Fatalf("UpdateRelevance: %v", err)
	}
	restore()
	il.wait(t)

	assertLifecycle(t, ps, ws, id, StateSoftDeleted)
	got, _ := ps.GetEngramUncached(ctx, ws, id)
	if got.Relevance != 0.25 {
		t.Fatalf("relevance = %v, want 0.25", got.Relevance)
	}
	assertRelevanceBucket(t, ps, ws, id, 0.5, 0.25)
}

// Two relevance writers (decay and consolidation's schema pass both call
// UpdateRelevance): each took its bucket-move "old" value from a read
// outside any lock, so the loser deleted a bucket that was no longer there
// and the winner's bucket was orphaned — two 0x10 entries for one memory.
func TestUpdateRelevance_ConcurrentUpdateRelevanceKeepsOneBucket(t *testing.T) {
	ps := openTestStore(t)
	ctx := context.Background()
	ws := ps.VaultPrefix("trail-log")
	id := writeRMWFixture(t, ps, ws, "water bar rebuilt at the creek crossing")

	il := &interleaver{ps: ps}
	restore := il.hook(t, "UpdateRelevance", id, func() {
		if err := ps.UpdateRelevance(ctx, ws, id, 0.95, 30); err != nil {
			t.Errorf("inner UpdateRelevance: %v", err)
		}
	})
	if err := ps.UpdateRelevance(ctx, ws, id, 0.25, 30); err != nil {
		t.Fatalf("UpdateRelevance: %v", err)
	}
	restore()
	il.wait(t)
	assertRelevanceBucket(t, ps, ws, id, 0.5, 0.25, 0.95)
}

// Evolve (SupersedeEngram through a StoreBatch) vs UpdateTrust: the batch
// read the predecessor when the supersede was QUEUED and re-encoded that copy
// at Commit, so a trust change committed in between was reverted.
func TestSupersedeEngram_ConcurrentUpdateTrustIsKept(t *testing.T) {
	ps := openTestStore(t)
	ctx := context.Background()
	ws := ps.VaultPrefix("trail-log")
	id := writeRMWFixture(t, ps, ws, "trailhead kiosk map replaced")
	until := time.Unix(1_800_000_000, 0).UTC()

	b := ps.NewBatch()
	defer b.Discard()
	if err := b.SupersedeEngram(ctx, ws, id, until); err != nil {
		t.Fatal(err)
	}
	if err := ps.UpdateTrust(ctx, ws, id, TrustVerified); err != nil {
		t.Fatal(err)
	}
	if err := b.Commit(); err != nil {
		t.Fatal(err)
	}

	assertLifecycle(t, ps, ws, id, StateSoftDeleted)
	got, _ := ps.GetEngramUncached(ctx, ws, id)
	if got.Trust != TrustVerified {
		t.Fatalf("trust = %v, want verified: the supersede rewrote the record it read before the trust change", got.Trust)
	}
	if !got.ValidUntil.Equal(until) {
		t.Fatalf("ValidUntil = %v, want %v", got.ValidUntil, until)
	}
}

// Access bump vs StampValidUntil (Forget not_true_since, Link supersedes):
// the stamp read the raw record, TouchAccess committed AccessCount+1, and the
// stamp wrote the pre-bump bytes back.
func TestStampValidUntil_ConcurrentTouchAccessIsKept(t *testing.T) {
	ps := openTestStore(t)
	ctx := context.Background()
	ws := ps.VaultPrefix("trail-log")
	id := writeRMWFixture(t, ps, ws, "bridge decking inspected")
	before, _ := ps.GetEngramUncached(ctx, ws, id)
	until := time.Unix(1_800_000_000, 0).UTC()

	il := &interleaver{ps: ps}
	restore := il.hook(t, "StampValidUntil", id, func() {
		if err := ps.TouchAccess(ctx, ws, id); err != nil {
			t.Errorf("TouchAccess: %v", err)
		}
	})
	if _, err := ps.StampValidUntil(ctx, ws, id, until, false); err != nil {
		t.Fatal(err)
	}
	restore()
	il.wait(t)

	got, _ := ps.GetEngramUncached(ctx, ws, id)
	if got.AccessCount != before.AccessCount+1 {
		t.Fatalf("AccessCount = %d, want %d: the stamp reverted a committed access bump", got.AccessCount, before.AccessCount+1)
	}
	if !got.ValidUntil.Equal(until) {
		t.Fatalf("ValidUntil = %v, want %v", got.ValidUntil, until)
	}
}

// UpdateTrust vs UpdateConfidence: the trust patch reverted a confidence
// write committed inside its window.
func TestUpdateTrust_ConcurrentUpdateConfidenceIsKept(t *testing.T) {
	ps := openTestStore(t)
	ctx := context.Background()
	ws := ps.VaultPrefix("trail-log")
	id := writeRMWFixture(t, ps, ws, "erosion netting staked on the switchbacks")

	il := &interleaver{ps: ps}
	restore := il.hook(t, "UpdateTrust", id, func() {
		if err := ps.UpdateConfidence(ctx, ws, id, 0.9); err != nil {
			t.Errorf("UpdateConfidence: %v", err)
		}
	})
	if err := ps.UpdateTrust(ctx, ws, id, TrustExternal); err != nil {
		t.Fatal(err)
	}
	restore()
	il.wait(t)

	got, _ := ps.GetEngramUncached(ctx, ws, id)
	if got.Confidence != 0.9 || got.Trust != TrustExternal {
		t.Fatalf("confidence=%v trust=%v, want 0.9 and external", got.Confidence, got.Trust)
	}
}

// Forget vs UpdateEmbedding (retroactive embedding, the embed plugin): the
// EmbedDim patch wrote the pre-forget record back.
func TestUpdateEmbedding_ConcurrentForgetStaysForgotten(t *testing.T) {
	ps := openTestStore(t)
	ctx := context.Background()
	ws := ps.VaultPrefix("trail-log")
	id := writeRMWFixture(t, ps, ws, "drainage dip graded")

	il := &interleaver{ps: ps}
	restore := il.hook(t, "UpdateEmbedding", id, func() {
		if err := ps.SoftDelete(ctx, ws, id); err != nil {
			t.Errorf("SoftDelete: %v", err)
		}
	})
	vec := make([]float32, 384)
	for i := range vec {
		vec[i] = float32(i%7) / 7
	}
	if err := ps.UpdateEmbedding(ctx, ws, id, vec); err != nil {
		t.Fatal(err)
	}
	restore()
	il.wait(t)
	assertLifecycle(t, ps, ws, id, StateSoftDeleted)
}
