package storage

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestBatchCommit_OppositeOrderMutationsDoNotDeadlock pins lockMany's
// ascending stripe order. Two batches each queue a mutation of A and of B,
// in opposite orders, and commit concurrently, many times. Locking in queue
// order would let each hold one stripe and wait for the other. No
// production batch queues two mutations today; this pins the order before
// one does. The watchdog is a deadlock detector, not synchronisation: the
// test passes as soon as both loops finish.
func TestBatchCommit_OppositeOrderMutationsDoNotDeadlock(t *testing.T) {
	ps := openTestStore(t)
	ctx := context.Background()
	ws := ps.VaultPrefix("trail-order")
	var a, b ULID
	a = writeRMWFixture(t, ps, ws, "upper switchback marker")
	for {
		b = writeRMWFixture(t, ps, ws, "lower switchback marker")
		if stripeIndex(a[:]) != stripeIndex(b[:]) {
			break
		}
	}

	const rounds = 300
	run := func(first, second ULID, errs chan<- error) {
		for i := 0; i < rounds; i++ {
			batch := ps.NewBatch()
			st := []LifecycleState{StateActive, StatePaused}[i%2]
			if err := batch.UpdateEngramState(ctx, ws, first, st); err != nil {
				errs <- err
				return
			}
			if err := batch.UpdateEngramState(ctx, ws, second, st); err != nil {
				errs <- err
				return
			}
			if err := batch.Commit(); err != nil {
				errs <- err
				return
			}
		}
		errs <- nil
	}
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); run(a, b, errs) }()
	go func() { defer wg.Done(); run(b, a, errs) }()
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("two batches mutating the same two engrams in opposite order deadlocked")
	}
	for i := 0; i < 2; i++ {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
}

// TestBatch_WriteAndMutateSameEngramIsRefused: a queued mutation is applied
// at Commit to the record as STORED, so in a batch that also writes the same
// engram the mutation would silently discard that write. Both orders are
// refused at queue time.
func TestBatch_WriteAndMutateSameEngramIsRefused(t *testing.T) {
	ps := openTestStore(t)
	ctx := context.Background()
	ws := ps.VaultPrefix("trail-order")
	id := writeRMWFixture(t, ps, ws, "footbridge decking")

	// write, then mutate
	b1 := ps.NewBatch()
	defer b1.Discard()
	eng, _ := ps.GetEngramUncached(ctx, ws, id)
	eng.Content = "footbridge decking replaced with treated planks"
	if err := b1.WriteEngram(ctx, ws, eng); err != nil {
		t.Fatal(err)
	}
	err := b1.UpdateEngramState(ctx, ws, id, StatePaused)
	if err == nil || !strings.Contains(err.Error(), "same batch") {
		t.Fatalf("mutation of an engram the batch also writes: err = %v, want a same-batch refusal", err)
	}

	// mutate, then write
	b2 := ps.NewBatch()
	defer b2.Discard()
	if err := b2.UpdateEngramState(ctx, ws, id, StatePaused); err != nil {
		t.Fatal(err)
	}
	eng2, _ := ps.GetEngramUncached(ctx, ws, id)
	eng2.Content = "footbridge decking replaced again"
	err = b2.WriteEngram(ctx, ws, eng2)
	if err == nil || !strings.Contains(err.Error(), "same batch") {
		t.Fatalf("write of an engram the batch also mutates: err = %v, want a same-batch refusal", err)
	}
}
