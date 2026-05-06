package fts

import (
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
