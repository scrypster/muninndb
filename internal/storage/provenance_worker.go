package storage

import (
	"context"
	"log/slog"
	"runtime"
	"sync"
	"sync/atomic"

	"github.com/scrypster/muninndb/internal/provenance"
)

const provenanceChanDepth = 32768 // was 8192 — absorbs burst at 63k writes/sec

type provenanceJob struct {
	wsPrefix [8]byte
	id       ULID
	entry    provenance.ProvenanceEntry
	epoch    uint64 // drain epoch the job was submitted in (drainTracker)
}

// drainTracker counts in-flight jobs per drain EPOCH, so Drain waits for
// exactly the jobs submitted before it began and nothing after.
//
// It replaces a sync.WaitGroup, which is not a valid tool here: Submit runs
// on every metadata write while ClearVault calls Drain, and a WaitGroup must
// not be Added to while a Wait is returning. When the count went 1 -> 0 (the
// waiter is woken) and a write's Submit Added before the waiter ran, Wait
// panicked with "sync: WaitGroup is reused before previous Wait has
// returned" — a vault clear racing writes crashed the process
// (TestProvenanceWorker_DrainRacingSubmitNeverPanics). A WaitGroup Wait also
// only returns when the count happens to touch zero, so under a steady write
// load a clear could wait on writes that began after it.
//
// Drain closes the current epoch and waits until every epoch at or below it
// has no outstanding job; Submits that arrive meanwhile land in the next
// epoch and are not waited for.
type drainTracker struct {
	mu    sync.Mutex
	cond  sync.Cond
	epoch uint64
	open  map[uint64]int64
}

func newDrainTracker() *drainTracker {
	d := &drainTracker{open: make(map[uint64]int64, 2)}
	d.cond.L = &d.mu
	return d
}

// begin records a job in the current epoch and returns that epoch.
func (d *drainTracker) begin() uint64 {
	d.mu.Lock()
	e := d.epoch
	d.open[e]++
	d.mu.Unlock()
	return e
}

// end marks one job of epoch e complete.
func (d *drainTracker) end(e uint64) {
	d.mu.Lock()
	if d.open[e]--; d.open[e] <= 0 {
		delete(d.open, e)
		if e < d.epoch { // a closed epoch emptied: some Drain may be waiting
			d.cond.Broadcast()
		}
	}
	d.mu.Unlock()
}

// wait closes the current epoch and blocks until it and every earlier epoch
// are empty.
func (d *drainTracker) wait() {
	d.mu.Lock()
	target := d.epoch
	d.epoch++
	for d.outstandingThrough(target) {
		d.cond.Wait()
	}
	d.mu.Unlock()
}

func (d *drainTracker) outstandingThrough(target uint64) bool {
	for e, n := range d.open {
		if e <= target && n > 0 {
			return true
		}
	}
	return false
}

// provenanceWorker fans out provenance appends across NumCPU goroutines.
// Each Append in provenance.Store is now a single O(1) Pebble Set (no
// read-modify-write), so concurrent goroutines are safe to run in parallel
// without per-ID coordination. The global atomic sequence in the key format
// ensures uniqueness even under sub-nanosecond timestamp collisions.
// provenanceAppender is the one method the worker needs from
// *provenance.Store; an interface so a test can run the worker without a
// Pebble write under it.
type provenanceAppender interface {
	Append(ctx context.Context, ws [8]byte, id [16]byte, entry provenance.ProvenanceEntry) error
}

type provenanceWorker struct {
	store    provenanceAppender
	pending  chan provenanceJob
	dropped  atomic.Int64
	wg       sync.WaitGroup // worker goroutines; Added only at construction
	inFlight *drainTracker  // jobs submitted and not yet appended, per drain epoch
	done     chan struct{}

	// TEST-ONLY seams (nil in production; one atomic load per job each).
	// testBeforeAppend runs on the worker goroutine before a job is appended;
	// testJobFinished runs right after the job is counted as complete.
	testBeforeAppend atomic.Pointer[func(provenanceJob)]
	testJobFinished  atomic.Pointer[func()]
}

func newProvenanceWorker(store provenanceAppender) *provenanceWorker {
	n := runtime.NumCPU()
	w := &provenanceWorker{
		store:    store,
		pending:  make(chan provenanceJob, provenanceChanDepth),
		inFlight: newDrainTracker(),
		done:     make(chan struct{}),
	}
	w.wg.Add(n)
	for range n {
		go func() {
			defer w.wg.Done()
			w.run()
		}()
	}
	return w
}

// Submit enqueues a provenance append. Non-blocking — drops and warns if full.
// Recovers from "send on closed channel" during graceful shutdown (e.g., when
// the decay worker processes a final batch after Close() has been called).
// The job is counted (inFlight.begin) before the channel send so a Drain that
// begins after this Submit can never miss it between enqueue and processing.
func (w *provenanceWorker) Submit(wsPrefix [8]byte, id ULID, entry provenance.ProvenanceEntry) {
	epoch := w.inFlight.begin()
	sent := false
	func() {
		defer func() { recover() }() // safe against send-on-closed-channel during shutdown
		select {
		case w.pending <- provenanceJob{wsPrefix: wsPrefix, id: id, entry: entry, epoch: epoch}:
			sent = true
		default:
		}
	}()
	if !sent {
		w.inFlight.end(epoch) // not enqueued — release immediately
		n := w.dropped.Add(1)
		if n&(n-1) == 0 { // log at powers of 2 to rate-limit slog I/O
			slog.Warn("storage: provenance worker full, entries dropped", "total_dropped", n)
		}
	}
}

// safeAppend calls store.Append but recovers from panics (e.g., "pebble: closed"
// during test teardown when db.Close() races with the worker).
func (w *provenanceWorker) safeAppend(ctx context.Context, job provenanceJob) {
	defer func() { recover() }()
	if err := w.store.Append(ctx, job.wsPrefix, [16]byte(job.id), job.entry); err != nil {
		slog.Warn("storage: provenance append failed", "engram_id", job.id, "err", err)
	}
}

// run drains the shared channel until it is closed (on Stop). Submit counted
// the job before it entered the channel; end() here after it is appended.
func (w *provenanceWorker) run() {
	ctx := context.Background()
	for job := range w.pending {
		if h := w.testBeforeAppend.Load(); h != nil {
			(*h)(job)
		}
		w.safeAppend(ctx, job)
		w.inFlight.end(job.epoch)
		if h := w.testJobFinished.Load(); h != nil {
			(*h)()
		}
	}
}

// Drain waits until every provenance job submitted before it was called has
// been appended. Jobs submitted while it waits are not waited for. Used by
// ClearVault so that no 0x16 key for a write that committed and submitted
// before the clear lands after the range tombstones. Safe to call
// concurrently with Submit and with other Drains.
//
// Residual (not closed here): a writer submits provenance AFTER its commit,
// so a write that commits just before a clear but submits just after the
// Drain still appends its 0x16 entry after the tombstones.
func (w *provenanceWorker) Drain() {
	w.inFlight.wait()
}

// Close drains the queue and waits for all worker goroutines to exit.
// Must be called before db.Close().
func (w *provenanceWorker) Close() {
	close(w.pending)
	w.wg.Wait()
	close(w.done)
}
