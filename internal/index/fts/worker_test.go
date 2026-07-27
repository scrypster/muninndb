package fts

import (
	"runtime"
	"sync/atomic"
	"testing"
	"time"
)

// panicIndex is a stub *Index whose IndexEngram panics on the first N calls.
type panicIndex struct {
	panicCount atomic.Int64
	callCount  atomic.Int64
}

func (p *panicIndex) IndexEngram(ws [8]byte, id [16]byte, concept, createdBy, content string, tags []string) error {
	p.callCount.Add(1)
	if p.panicCount.Add(-1) >= 0 {
		panic("synthetic fts panic")
	}
	return nil
}

// TestWorkerRestartsAfterPanic verifies that a goroutine that panics during
// IndexEngram is automatically replaced so subsequent jobs are still processed.
func TestWorkerRestartsAfterPanic(t *testing.T) {
	stub := &panicIndex{}
	stub.panicCount.Store(1) // first IndexEngram call will panic

	w := newWorkerWithIndex(stub)

	// Submit the first job, which will panic during indexing.
	job := IndexJob{Concept: "test"}
	w.Submit(job)

	// Give the worker time to process the first job (and panic+restart).
	time.Sleep(300 * time.Millisecond)

	// Submit the second job after the worker has had time to restart.
	// This verifies that the restarted worker goroutine still processes jobs.
	w.Submit(job)

	// Give the worker time to process the second job.
	time.Sleep(300 * time.Millisecond)

	w.Stop()

	if calls := stub.callCount.Load(); calls < 2 {
		t.Errorf("callCount = %d, want >= 2 (worker must restart after panic)", calls)
	}
}

// countingIndex records every engram it is asked to index.
type countingIndex struct{ indexed atomic.Int64 }

func (c *countingIndex) IndexEngram(ws [8]byte, id [16]byte, concept, createdBy, content string, tags []string) error {
	c.indexed.Add(1)
	return nil
}

// TestWorkerStopDrainsJobsSubmittedBeforeStart is a regression test for jobs
// silently lost when Stop() runs before the worker goroutines are first
// scheduled.
//
// Stop() is documented to drain the queue. It set `stopped` before closing
// stopCh, and runLoop checked `!stopped` *before* ever calling run(). If the
// goroutines had not been scheduled yet, every one of them saw stopped==true,
// skipped run() entirely, and returned without draining — leaving the job in
// the channel buffer forever. The engram stayed durable but was never indexed,
// so recall could not find it.
//
// GOMAXPROCS(1) plus an immediate Stop() makes the unscheduled-goroutine window
// deterministic rather than a rare CI flake.
func TestWorkerStopDrainsJobsSubmittedBeforeStart(t *testing.T) {
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))

	for i := range 50 {
		stub := &countingIndex{}
		w := newWorkerWithIndex(stub)

		if !w.Submit(IndexJob{Concept: "indexed-before-stop"}) {
			t.Fatalf("iteration %d: Submit was rejected", i)
		}
		// No sleep on purpose: Stop() must drain regardless of whether the
		// worker goroutines have run yet.
		w.Stop()

		if got := stub.indexed.Load(); got != 1 {
			t.Fatalf("iteration %d: Stop() did not drain the queue — indexed %d jobs, want 1", i, got)
		}
	}
}
