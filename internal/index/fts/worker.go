package fts

import (
	"fmt"
	"log/slog"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	workerBufSize   = 32768 // was 4096 — large enough to absorb burst at 56k writes/sec
	workerBatchSize = 64    // was 32
	workerInterval  = 100 * time.Millisecond
)

// IndexJob is a pending FTS indexing task queued from a write.
type IndexJob struct {
	WS        [8]byte
	ID        [16]byte
	Concept   string
	CreatedBy string
	Content   string
	Tags      []string
}

// indexer is the interface Worker depends on for indexing engrams.
// *Index satisfies this interface.
type indexer interface {
	IndexEngram(ws [8]byte, id [16]byte, concept, createdBy, content string, tags []string) error
}

// Worker processes FTS indexing jobs asynchronously off the write hot path.
// Jobs are distributed across NumCPU goroutines reading from a shared buffered channel.
// If the queue is full, the job is dropped and a warning is logged — the engram is
// already durably stored in Pebble; only keyword search visibility is delayed.
// Stale FTS entries for deleted engrams are harmless: Phase 6 of activation filters
// nil metadata results, so orphaned posting list entries never surface in results.
type Worker struct {
	idx    indexer
	input  chan IndexJob
	stopCh chan struct{}
	// flushNow wakes idle goroutines so Flush does not have to wait out a full
	// workerInterval tick. Buffered and best-effort: a send that would block is
	// discarded, because a goroutine that is already awake will flush anyway.
	flushNow chan struct{}
	stopped  atomic.Bool
	dropped  atomic.Int64
	// pending counts jobs accepted by Submit but not yet handed to the indexer.
	// Flush waits for it to reach zero.
	pending        atomic.Int64
	wg             sync.WaitGroup
	done           chan struct{}
	clearingVaults sync.Map // [8]byte → struct{}{}
}

// SetClearing marks or unmarks a vault as being cleared.
// While a vault is marked as clearing, incoming index jobs for that vault are
// silently dropped so that new FTS entries are not written during a vault clear
// operation.
func (w *Worker) SetClearing(ws [8]byte, clearing bool) {
	if clearing {
		w.clearingVaults.Store(ws, struct{}{})
	} else {
		w.clearingVaults.Delete(ws)
	}
}

// NewWorker creates and starts an async FTS indexing worker pool.
// Spawns NumCPU goroutines all reading from a shared 32768-entry channel.
// Call Stop() to drain and shut down on engine shutdown.
func NewWorker(idx *Index) *Worker {
	return newWorkerWithIndex(idx)
}

// newWorkerWithIndex creates and starts an async FTS indexing worker pool using
// the provided indexer. This is the real constructor logic, extracted to allow
// injection of a stub indexer in tests.
func newWorkerWithIndex(idx indexer) *Worker {
	n := runtime.NumCPU()
	w := &Worker{
		idx:      idx,
		input:    make(chan IndexJob, workerBufSize),
		stopCh:   make(chan struct{}),
		flushNow: make(chan struct{}, 1),
		done:     make(chan struct{}),
	}
	for range n {
		w.wg.Add(1)
		go w.runLoop()
	}
	return w
}

// runLoop is the per-goroutine entry point. It wraps run() in a restart loop:
// after a non-fatal panic, the goroutine re-enters run() instead of exiting.
// wg.Done() only fires when the worker is cleanly stopped.
//
// run() is entered at least once, before the stopped flag is consulted. That
// ordering is what makes Stop()'s drain guarantee hold: Stop sets `stopped`
// and only then closes stopCh, so a goroutine that had not been scheduled yet
// would — with the check at the top — observe stopped==true, skip run()
// entirely, and return without draining. Every queued job then sat in the
// channel buffer forever: durable engrams that were never indexed and so could
// not be recalled. Entering run() first costs nothing (stopCh is already
// closed, so run() takes its drain branch and returns immediately) and turns
// the guarantee into something the code actually enforces.
func (w *Worker) runLoop() {
	defer w.wg.Done()
	for {
		func() {
			defer func() {
				if r := recover(); r != nil {
					if ftsIsClosedPanic(r) || w.stopped.Load() {
						return
					}
					slog.Error("fts: worker goroutine panicked, restarting", "panic", r)
				}
			}()
			w.run()
		}()
		if w.stopped.Load() {
			return
		}
	}
}

// Submit enqueues an FTS index job. Non-blocking — drops and warns if queue is full.
// Returns true if the job was accepted, false if dropped (including after Stop).
func (w *Worker) Submit(job IndexJob) bool {
	if w.stopped.Load() {
		return false
	}
	select {
	case w.input <- job:
		w.pending.Add(1)
		return true
	default:
		n := w.dropped.Add(1)
		if n&(n-1) == 0 {
			slog.Warn("fts: worker queue full, index jobs dropped", "total_dropped", n)
		}
		return false
	}
}

// Stop drains the queue and shuts down all worker goroutines. Blocks until complete.
func (w *Worker) Stop() {
	w.stopped.Store(true)
	close(w.stopCh)
	w.wg.Wait()
	close(w.done)
}

// Dropped returns the cumulative number of jobs dropped due to queue pressure.
func (w *Worker) Dropped() int64 {
	return w.dropped.Load()
}

// Flush blocks until every job accepted by Submit before this call has been
// handed to the indexer, or until timeout elapses. It does not stop the worker.
//
// Use it when a caller needs FTS visibility to be guaranteed rather than
// eventual — tests asserting on recall results, and any admin path that must
// not report success while writes are still unsearchable. It is not needed on
// the normal write path, which is deliberately decoupled from indexing.
//
// Jobs rejected by Submit (queue full, or worker stopped) were never counted,
// so Flush makes no promise about them; check the Submit return value or
// Dropped() for that.
func (w *Worker) Flush(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if w.pending.Load() == 0 {
			return nil
		}
		if w.stopped.Load() {
			// Stop() drains synchronously, so anything still pending here is
			// never going to be picked up. Report rather than spin to timeout.
			return fmt.Errorf("fts: flush abandoned, worker stopped with %d jobs pending", w.pending.Load())
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("fts: flush timed out after %s with %d jobs pending", timeout, w.pending.Load())
		}
		// Nudge an idle goroutine rather than waiting out the workerInterval
		// tick. Best-effort: if the buffer is full a wake is already queued.
		select {
		case w.flushNow <- struct{}{}:
		default:
		}
		time.Sleep(200 * time.Microsecond)
	}
}

func (w *Worker) run() {
	ticker := time.NewTicker(workerInterval)
	defer ticker.Stop()

	batch := make([]IndexJob, 0, workerBatchSize)

	flush := func() {
		if len(batch) == 0 {
			return
		}
		w.flush(batch)
		batch = batch[:0]
	}

	for {
		select {
		case job := <-w.input:
			batch = append(batch, job)
			if len(batch) >= workerBatchSize {
				flush()
			}
		case <-w.stopCh:
			// Drain remaining items from the input channel before exiting.
			for {
				select {
				case job := <-w.input:
					batch = append(batch, job)
				default:
					flush()
					return
				}
			}
		case <-w.flushNow:
			flush()
		case <-ticker.C:
			flush()
		}
	}
}

func (w *Worker) flush(jobs []IndexJob) {
	// Each job decrements pending exactly once, and only after it is fully dealt
	// with — indexed, failed, or skipped for a clearing vault. Decrementing on
	// entry instead would let pending reach zero while IndexEngram was still
	// running for the last batch, so Flush would return before the write was
	// actually searchable.
	done := 0
	defer func() {
		// IndexEngram panicking aborts the batch (the goroutine then restarts,
		// see runLoop). Release the counter for the jobs we never reached, or
		// Flush would wait out its timeout on work that will never happen.
		if rem := len(jobs) - done; rem > 0 {
			w.pending.Add(int64(-rem))
		}
	}()

	for _, job := range jobs {
		if _, dropping := w.clearingVaults.Load(job.WS); !dropping {
			if err := w.idx.IndexEngram(job.WS, job.ID, job.Concept, job.CreatedBy, job.Content, job.Tags); err != nil {
				slog.Warn("fts: worker failed to index engram",
					"engram_id", job.ID,
					"err", err,
				)
			}
		}
		done++
		w.pending.Add(-1)
	}
}

// ftsIsClosedPanic reports whether a recovered panic value represents a
// closed-DB condition from Pebble. Inlined here to avoid an import cycle
// with the storage package. Must stay in sync with storage.IsClosedPanic.
func ftsIsClosedPanic(r any) bool {
	s := fmt.Sprintf("%v", r)
	return strings.Contains(s, "pebble: closed") ||
		strings.Contains(s, "pebble/record: closed")
}
