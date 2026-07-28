package storage

import (
	"context"
	"sync"
	"testing"
	"time"
)

// TestTouchAccess_ConcurrentWithCAS races PebbleStore.TouchAccess against
// CompareAndSet (active→completed) on the same engram id, under -race. Both
// take the same per-engram stripe lock (casLocks.For(id)) that
// CompareAndSet/DeleteEngram/UpdateConfidence already use, so they must
// serialize: whichever runs second observes the first's committed state.
//
// The invariant under test: after both goroutines return, the CAS's state
// transition (active→completed) MUST have applied (TouchAccess must not
// silently undo it via a stale-state UpdateMetadata write), and TouchAccess's
// AccessCount bump must not be lost (no lost update between the two RMWs).
func TestTouchAccess_ConcurrentWithCAS(t *testing.T) {
	store, cleanup := newTestStoreHelper(t)
	defer cleanup()
	ctx := context.Background()
	ws := store.VaultPrefix("touch-cas")

	for i := 0; i < 200; i++ {
		id := writeLeaseTestEngram(t, store, ws)

		active := StateActive
		completed := StateCompleted

		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, _ = store.CompareAndSet(ctx, ws, id, CASCondition{State: &active}, CASMutation{State: &completed})
		}()
		go func() {
			defer wg.Done()
			_ = store.TouchAccess(ctx, ws, id)
		}()
		wg.Wait()

		eng, err := store.GetEngram(ctx, ws, id)
		if err != nil {
			t.Fatalf("iter %d: GetEngram: %v", i, err)
		}
		if eng.State != StateCompleted {
			t.Fatalf("iter %d: final state = %v, want StateCompleted (%v); TouchAccess raced/clobbered the CAS transition",
				i, eng.State, StateCompleted)
		}
		if eng.AccessCount == 0 {
			t.Fatalf("iter %d: AccessCount = 0, want > 0; TouchAccess's bump was lost", i)
		}
	}
}

// TestTouchAccess_PreservesOtherFields verifies TouchAccess bumps ONLY
// AccessCount and LastAccess — Confidence/Relevance/Stability/State/Trust must
// be read fresh under the lock and passed through unchanged. Pins the "never
// escalates trust/confidence via reinforcement" invariant at the storage layer.
func TestTouchAccess_PreservesOtherFields(t *testing.T) {
	store, cleanup := newTestStoreHelper(t)
	defer cleanup()
	ctx := context.Background()
	ws := store.VaultPrefix("touch-preserve")
	id := writeLeaseTestEngram(t, store, ws)

	if err := store.UpdateConfidence(ctx, ws, id, 0.42); err != nil {
		t.Fatalf("UpdateConfidence: %v", err)
	}

	before, err := store.GetEngram(ctx, ws, id)
	if err != nil {
		t.Fatalf("GetEngram before: %v", err)
	}

	time.Sleep(time.Millisecond)
	if err := store.TouchAccess(ctx, ws, id); err != nil {
		t.Fatalf("TouchAccess: %v", err)
	}

	after, err := store.GetEngram(ctx, ws, id)
	if err != nil {
		t.Fatalf("GetEngram after: %v", err)
	}

	if after.AccessCount != before.AccessCount+1 {
		t.Errorf("AccessCount = %d, want %d", after.AccessCount, before.AccessCount+1)
	}
	if !after.LastAccess.After(before.LastAccess) {
		t.Errorf("LastAccess did not advance: before=%v after=%v", before.LastAccess, after.LastAccess)
	}
	if after.Confidence != before.Confidence {
		t.Errorf("Confidence changed: before=%v after=%v (TouchAccess must not touch confidence)", before.Confidence, after.Confidence)
	}
	if after.State != before.State {
		t.Errorf("State changed: before=%v after=%v (TouchAccess must not touch state)", before.State, after.State)
	}
	if after.Trust != before.Trust {
		t.Errorf("Trust changed: before=%v after=%v (TouchAccess must not touch trust)", before.Trust, after.Trust)
	}
}
