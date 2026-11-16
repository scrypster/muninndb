package storage

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/scrypster/muninndb/internal/provenance"
)

// TestProvenanceWorker_DrainRacingSubmitNeverPanics reproduces the ClearVault
// panic ("sync: WaitGroup is reused before previous Wait has returned",
// provenance_worker.go Drain <- vault_lifecycle.go ClearVault) without timing.
//
// The interleaving is the one a vault clear racing metadata writes produces:
// Drain is parked waiting for an in-flight append; that append finishes, and
// before the drainer has woken a new write Submits. With a sync.WaitGroup the
// counter goes 1 -> 0 (waking the waiter) -> 1 (the new Add) and the waking
// Wait sees a non-zero state and panics.
//
// synctest makes the setup exact: synctest.Wait returns only once the worker
// is parked inside the job (a channel created in the bubble) AND Drain is
// parked in its wait, so the drainer is provably registered before the job is
// released. The new Submit runs on the worker goroutine immediately after the
// job is marked complete, before the drainer can run.
//
// The second half pins the contract the fix must keep: a job submitted BEFORE
// Drain began has been appended by the time Drain returns (ClearVault depends
// on that to range-delete it), and Drain does not wait for a job submitted
// after it began.
func TestProvenanceWorker_DrainRacingSubmitNeverPanics(t *testing.T) {
	ws := [8]byte{0x51, 0xa1, 0x00, 0x00, 0x00, 0x00, 0x00, 0x07}
	before := NewULID()
	after := NewULID()
	entry := provenance.ProvenanceEntry{
		Timestamp: time.Unix(1_700_000_000, 0),
		Source:    provenance.SourceInferred,
		AgentID:   "test:trail-crew",
		Operation: "update-meta",
	}

	synctest.Test(t, func(t *testing.T) {
		// An in-memory appender: a Pebble Sync write inside the bubble would
		// associate Pebble's own WaitGroups with it.
		rec := &recordingAppender{}
		w := newProvenanceWorker(rec)
		defer w.Close()

		release := make(chan struct{})
		releaseAfter := make(chan struct{})
		var parked atomic.Bool
		beforeAppend := func(job provenanceJob) {
			if job.id == before && parked.CompareAndSwap(false, true) {
				<-release
			}
			if job.id == after {
				// Held until Drain has returned: a Drain that waited for a
				// job submitted AFTER it began would never return, and the
				// bubble would report the deadlock.
				<-releaseAfter
			}
		}
		w.testBeforeAppend.Store(&beforeAppend)

		var resubmitted atomic.Bool
		resubmittedCh := make(chan struct{})
		finished := func() {
			if resubmitted.CompareAndSwap(false, true) {
				w.Submit(ws, after, entry) // a write landing as the drain wakes
				close(resubmittedCh)
			}
		}
		w.testJobFinished.Store(&finished)

		w.Submit(ws, before, entry)

		drained := make(chan any, 1)
		go func() {
			defer func() { drained <- recover() }()
			w.Drain()
		}()
		synctest.Wait() // worker parked in the job, drainer parked in Drain
		if !parked.Load() {
			t.Fatal("fixture: the worker never picked up the pre-drain job")
		}
		close(release)

		if p := <-drained; p != nil {
			t.Fatalf("Drain panicked while a Submit raced it: %v", p)
		}
		if n := rec.count(before); n != 1 {
			t.Fatalf("a job submitted before Drain began was not appended when Drain returned: %d entries", n)
		}
		<-resubmittedCh // the racing Submit has been counted
		close(releaseAfter)
		w.Drain()
		if n := rec.count(after); n != 1 {
			t.Fatalf("the job submitted during the first Drain was lost: %d entries", n)
		}
	})
}

type recordingAppender struct {
	mu  sync.Mutex
	got map[ULID]int
}

func (r *recordingAppender) Append(_ context.Context, _ [8]byte, id [16]byte, _ provenance.ProvenanceEntry) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.got == nil {
		r.got = map[ULID]int{}
	}
	r.got[ULID(id)]++
	return nil
}

func (r *recordingAppender) count(id ULID) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.got[id]
}
