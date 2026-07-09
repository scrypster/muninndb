package storage

import (
	"context"
	"sync"
	"testing"
	"time"
)

// TestDeleteEngramSerializesWithCompareAndSet asserts DeleteEngram takes the
// same per-engram stripe lock that CompareAndSet holds across its
// read-compare-write. Without it, a delete can commit in the middle of a
// concurrent CAS and the CAS's later write resurrects the metadata/lease the
// delete just removed. Holding the lock in the test stands in for an in-flight
// CAS: a correct DeleteEngram must block until it is released.
func TestDeleteEngramSerializesWithCompareAndSet(t *testing.T) {
	store, cleanup := newTestStoreHelper(t)
	defer cleanup()
	ctx := context.Background()
	ws := store.VaultPrefix("delete-caslock")
	id := writeLeaseTestEngram(t, store, ws)

	mu := store.casLocks.For(id[:])
	mu.Lock()

	done := make(chan error, 1)
	go func() {
		done <- store.DeleteEngram(ctx, ws, id)
	}()

	select {
	case <-done:
		mu.Unlock()
		t.Fatal("DeleteEngram completed while the engram's CAS stripe lock was held; " +
			"it does not serialize with CompareAndSet and can resurrect deleted records")
	case <-time.After(100 * time.Millisecond):
		// Expected: DeleteEngram is blocked on the stripe lock.
	}

	mu.Unlock()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("DeleteEngram: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("DeleteEngram did not complete after releasing the stripe lock")
	}

	metas, err := store.GetMetadata(ctx, ws, []ULID{id})
	if err != nil {
		t.Fatalf("GetMetadata: %v", err)
	}
	if len(metas) > 0 && metas[0] != nil {
		t.Fatalf("engram metadata still present after delete")
	}
	lease, err := store.GetLease(ctx, ws, id)
	if err != nil {
		t.Fatalf("GetLease: %v", err)
	}
	if !lease.IsZero() {
		t.Fatalf("lease sidecar resurrected after delete: %+v", lease)
	}
}

// TestDeleteEngramNoResurrectionUnderConcurrentCAS races a DeleteEngram against
// a CompareAndSet that writes a lease sidecar on the same engram. The invariant:
// once an engram's metadata is gone, no lease sidecar may remain. A resurrected
// lease after a committed delete is the bug. Run with -race for interleavings.
func TestDeleteEngramNoResurrectionUnderConcurrentCAS(t *testing.T) {
	store, cleanup := newTestStoreHelper(t)
	defer cleanup()
	ctx := context.Background()
	ws := store.VaultPrefix("delete-cas-race")

	for i := 0; i < 200; i++ {
		id := writeLeaseTestEngram(t, store, ws)
		lease := Lease{Owner: "hostA:sess", Heartbeat: time.Now().UnixNano(), TTLSeconds: 120}

		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			_ = store.DeleteEngram(ctx, ws, id)
		}()
		go func() {
			defer wg.Done()
			// Returns ErrNotFound if the delete won the race — acceptable.
			_, _ = store.CompareAndSet(ctx, ws, id, CASCondition{}, CASMutation{Lease: &lease})
		}()
		wg.Wait()

		metas, err := store.GetMetadata(ctx, ws, []ULID{id})
		if err != nil {
			t.Fatalf("iter %d GetMetadata: %v", i, err)
		}
		metaGone := len(metas) == 0 || metas[0] == nil
		gotLease, err := store.GetLease(ctx, ws, id)
		if err != nil {
			t.Fatalf("iter %d GetLease: %v", i, err)
		}
		if metaGone && !gotLease.IsZero() {
			t.Fatalf("iter %d: lease sidecar resurrected after delete removed metadata: %+v", i, gotLease)
		}
	}
}
