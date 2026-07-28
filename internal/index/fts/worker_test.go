package fts

import (
	"fmt"
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

// slowIndex blocks each IndexEngram call briefly so jobs are provably still in
// flight when Flush is called.
type slowIndex struct {
	indexed atomic.Int64
	delay   time.Duration
}

func (s *slowIndex) IndexEngram(ws [8]byte, id [16]byte, concept, createdBy, content string, tags []string) error {
	time.Sleep(s.delay)
	s.indexed.Add(1)
	return nil
}

// TestWorkerFlushWaitsForPendingJobs verifies Flush's contract: when it returns
// nil, every job accepted before the call has reached the indexer.
func TestWorkerFlushWaitsForPendingJobs(t *testing.T) {
	const jobs = 200
	stub := &slowIndex{delay: 50 * time.Microsecond}
	w := newWorkerWithIndex(stub)
	defer w.Stop()

	for i := range jobs {
		if !w.Submit(IndexJob{Concept: fmt.Sprintf("job-%d", i)}) {
			t.Fatalf("Submit %d rejected", i)
		}
	}

	if err := w.Flush(10 * time.Second); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if got := stub.indexed.Load(); got != jobs {
		t.Errorf("Flush returned with work outstanding: indexed %d, want %d", got, jobs)
	}
}

// Flush must not block for the full timeout once the worker is stopped — Stop
// drains synchronously, so anything still pending will never be picked up.
func TestWorkerFlushReportsAfterStop(t *testing.T) {
	w := newWorkerWithIndex(&countingIndex{})
	w.Stop()

	start := time.Now()
	err := w.Flush(5 * time.Second)
	elapsed := time.Since(start)

	// Nothing was queued, so a stopped-but-empty worker still flushes cleanly.
	if err != nil {
		t.Fatalf("Flush on a drained stopped worker should succeed, got %v", err)
	}
	if elapsed > time.Second {
		t.Errorf("Flush took %s on an empty stopped worker; should return immediately", elapsed)
	}
}

// Flush is a no-op when nothing was ever submitted.
func TestWorkerFlushNoJobs(t *testing.T) {
	w := newWorkerWithIndex(&countingIndex{})
	defer w.Stop()
	if err := w.Flush(2 * time.Second); err != nil {
		t.Fatalf("Flush with no jobs: %v", err)
	}
}
