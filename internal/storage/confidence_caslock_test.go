package storage

import (
	"context"
	"sync"
	"testing"
	"time"
)

// TestUpdateConfidenceWithContradiction_SerializesWithCAS asserts
// UpdateConfidenceWithContradiction takes the same per-engram stripe lock that
// CompareAndSet and DeleteEngram hold across their read-modify-write. Without
// it, the function's GetEngram → batch.Commit RMW can interleave with a
// concurrent CAS or delete: a CAS that loses the race can have its state change
// clobbered when UpdateConfidenceWithContradiction writes the whole engram
// back, and the function can commit after a DeleteEngram — resurrecting a
// record the caller believes is gone. Holding the lock in the test stands in
// for an in-flight CAS: a correct UpdateConfidenceWithContradiction must block
// until it is released. Mirrors TestDeleteEngramSerializesWithCompareAndSet.
func TestUpdateConfidenceWithContradiction_SerializesWithCAS(t *testing.T) {
	store, cleanup := newTestStoreHelper(t)
	defer cleanup()
	ctx := context.Background()
	ws := store.VaultPrefix("conf-caslock")
	id := writeLeaseTestEngram(t, store, ws)

	mu := store.casLocks.For(id[:])
	mu.Lock()

	done := make(chan error, 1)
	go func() {
		// Delta-based API (#559): method now returns (prior, newConf, err).
		_, _, err := store.UpdateConfidenceWithContradiction(ctx, ws, id, 0.5, id, false)
		done <- err
	}()

	select {
	case <-done:
		mu.Unlock()
		t.Fatal("UpdateConfidenceWithContradiction completed while the engram's CAS stripe lock was held; " +
			"it does not serialize with CompareAndSet/DeleteEngram and can resurrect deleted records")
	case <-time.After(100 * time.Millisecond):
		// Expected: blocked on the stripe lock.
	}

	mu.Unlock()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("UpdateConfidenceWithContradiction: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("UpdateConfidenceWithContradiction did not complete after releasing the stripe lock")
	}
}

// TestUpdateConfidenceWithContradiction_NoLostUpdateUnderConcurrentDeltas is the
// lost-update test the NOTE below used to document as absent. N concurrent
// +delta calls on the same id must each observe the prior writer's commit and
// accumulate — the per-engram stripe lock acquired inside this method now spans
// the read+add+clamp+commit, so the calls serialize.
//
// Before #559's delta-based refactor, Engine.AdjustConfidence did its confidence
// read OUTSIDE this storage method (via GetConfidence) and passed an ABSOLUTE
// target; the stripe lock acquired here could not span that external read, so
// 50 concurrent +0.01 calls landed at ≈0.02 (last absolute-writer wins). With
// the read+add+clamp moved inside the locked method, the 50 deltas accumulate
// to ≈0.5. Run with -race -count=N to confirm stability.
func TestUpdateConfidenceWithContradiction_NoLostUpdateUnderConcurrentDeltas(t *testing.T) {
	store, cleanup := newTestStoreHelper(t)
	defer cleanup()
	ctx := context.Background()
	ws := store.VaultPrefix("conf-lostupdate")
	id := writeLeaseTestEngram(t, store, ws)

	// Pin the seed at exactly 0.0 so the expected accumulation is N*delta.
	if err := store.UpdateConfidence(ctx, ws, id, 0.0); err != nil {
		t.Fatalf("seed UpdateConfidence: %v", err)
	}

	const N = 50
	const delta = float32(0.01)
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			// hasContra=false: bare delta path, no marker work, no other-id
			// needed — isolates the confidence RMW.
			_, _, _ = store.UpdateConfidenceWithContradiction(ctx, ws, id, delta, id, false)
		}()
	}
	wg.Wait()

	got, err := store.GetConfidence(ctx, ws, id)
	if err != nil {
		t.Fatalf("GetConfidence: %v", err)
	}
	// Expected ≈ 50 × 0.01 = 0.5. The lower bound 0.45 EXCLUDES the broken-
	// behaviour result (~0.02 = a single delta landing, last-writer-wins) with
	// headroom for float32 accumulation noise. The upper bound guards against
	// a runaway clamp bug. This assertion is the load-bearing one: if it fails
	// low, deltas were lost.
	const want = float32(0.5)
	if got < 0.45 || got > want+0.01 {
		t.Fatalf("lost-update: final confidence = %v, want ≈ %v (50 × +0.01); "+
			"< 0.45 means deltas were lost (pre-fix behaviour landed ≈ 0.02)", got, want)
	}
}

// TestUpdateConfidenceWithContradiction_NoResurrectionUnderConcurrentDelete
// races UpdateConfidenceWithContradiction against DeleteEngram on the same id,
// mirroring TestDeleteEngramNoResurrectionUnderConcurrentCAS. The invariant:
// once DeleteEngram returns, the engram MUST be gone — no concurrent RMW may
// resurrect it by committing its engram/metadata keys after the delete's batch.
// Without the per-engram lock on UpdateConfidenceWithContradiction, the
// function's GetEngram read can land before DeleteEngram's commit and its
// batch.Set(0x01|0x02) + Commit can land after — leaving the deleted engram
// readable again (the #594 resurrection class). Run with -race for
// interleavings; the 200-trial loop is the same cadence the delete-path test
// uses to surface the ~few/200 resurrection rate.
func TestUpdateConfidenceWithContradiction_NoResurrectionUnderConcurrentDelete(t *testing.T) {
	store, cleanup := newTestStoreHelper(t)
	defer cleanup()
	ctx := context.Background()
	ws := store.VaultPrefix("conf-resurrect")

	for i := 0; i < 200; i++ {
		id := writeLeaseTestEngram(t, store, ws)

		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			_ = store.DeleteEngram(ctx, ws, id)
		}()
		go func() {
			defer wg.Done()
			// Delta-based API (#559): returns (prior, newConf, err).
			// Returns ErrNotFound if the delete won the race — acceptable.
			_, _, _ = store.UpdateConfidenceWithContradiction(ctx, ws, id, 0.5, id, false)
		}()
		wg.Wait()

		// After both return the engram MUST NOT be readable. A non-nil engram
		// means UpdateConfidenceWithContradiction committed its 0x01|0x02
		// write AFTER DeleteEngram's batch and resurrected the record.
		eng, err := store.GetEngram(ctx, ws, id)
		if err == nil && eng != nil {
			t.Fatalf("iter %d: engram resurrected after DeleteEngram returned: %+v", i, eng)
		}
	}
}

// TestUpdateConfidence_NoResurrectionUnderConcurrentDelete is the ABSOLUTE-setter
// analogue of the delta-path test above. PebbleStore.UpdateConfidence (the
// absolute set, reachable from Engine.AdjustConfidence via the ConfidenceWorker
// when hasContra=true) is also a GetEngram → batch.Set(0x01|0x02) → Commit
// read-modify-write, so it can resurrect a concurrently deleted engram in exactly
// the same way (#626). The invariant is identical: once DeleteEngram returns, the
// engram MUST NOT be readable — GetEngram reads the 0x01/0x02 keys, so a non-nil
// result means UpdateConfidence committed its payload write after the delete's
// batch, restoring the record without its 0x0B state index. Without the
// per-engram stripe lock on UpdateConfidence this fails ~1/500 under -race; run
// with -race -count=3 to surface it reliably. Mirrors the delta-path test's
// 200-trial cadence.
func TestUpdateConfidence_NoResurrectionUnderConcurrentDelete(t *testing.T) {
	store, cleanup := newTestStoreHelper(t)
	defer cleanup()
	ctx := context.Background()
	ws := store.VaultPrefix("conf-abs-resurrect")

	for i := 0; i < 200; i++ {
		id := writeLeaseTestEngram(t, store, ws)

		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			_ = store.DeleteEngram(ctx, ws, id)
		}()
		go func() {
			defer wg.Done()
			// Absolute set. Returns ErrNotFound if the delete won the race —
			// acceptable; the invariant is only about post-delete resurrection.
			_ = store.UpdateConfidence(ctx, ws, id, 0.5)
		}()
		wg.Wait()

		// After both return the engram MUST NOT be readable. GetEngram reads the
		// 0x01/0x02 keys; a non-nil engram means UpdateConfidence committed its
		// payload write AFTER DeleteEngram's batch and resurrected the record.
		eng, err := store.GetEngram(ctx, ws, id)
		if err == nil && eng != nil {
			t.Fatalf("iter %d: engram resurrected after DeleteEngram returned via absolute UpdateConfidence: %+v", i, eng)
		}
	}
}
