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
		done <- store.UpdateConfidenceWithContradiction(ctx, ws, id, 0.5, id, false)
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

// NOTE on the lost-update test class (deliberately absent).
//
// The lost-update race reported by scrypster — "50 concurrent +0.01
// AdjustConfidence calls on the same id from 0.0 landed at 0.02–0.06" —
// lives at the ENGINE layer, not at this storage layer. Engine.AdjustConfidence
// does `current, _ := GetConfidence(...); newConf := current+delta;
// UpdateConfidenceWithContradiction(ctx, ws, id, newConf, ...)` — the read is
// OUTSIDE UpdateConfidenceWithContradiction, so the per-engram stripe lock
// acquired inside the storage function cannot span the read that computes the
// absolute target. Empirically confirmed: 50 concurrent "+0.01" RMW calls
// against the same id land at 0.02 WITH the storage lock and 0.04–0.07
// WITHOUT it — the storage lock cannot make this test green because the loss
// is determined entirely by the unlocked external read, not by the storage
// RMW. A meaningful lost-update test for this pattern belongs in
// internal/engine (call Engine.AdjustConfidence with delta=0.01 fifty times)
// and requires an engine-layer fix (engine holds the stripe lock across its
// read+write, OR the storage exposes an AdjustConfidence(delta) API that does
// read+add+write atomically under the lock). Both are out of scope for this
// PR, which closes the #594-class resurrection race documented below.
//
// The storage-level race the stripe lock DOES fix for this function is the
// resurrection race — a concurrent DeleteEngram committing between this
// function's GetEngram read and its batch.Commit, after which the engram-key
// Set re-creates the deleted record. That is covered by
// TestUpdateConfidenceWithContradiction_NoResurrectionUnderConcurrentDelete
// below.

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
			// Returns ErrNotFound if the delete won the race — acceptable.
			_ = store.UpdateConfidenceWithContradiction(ctx, ws, id, 0.5, id, false)
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
