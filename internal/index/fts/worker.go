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

// Worker processes FTS indexing jobs asynchronously off the write hot path.
// Jobs are distributed across NumCPU goroutines reading from a shared buffered channel.
// If the queue is full, the job is dropped and a warning is logged — the engram is
// already durably stored in Pebble; only keyword search visibility is delayed.
// Stale FTS entries for deleted engrams are harmless: Phase 6 of activation filters
// nil metadata results, so orphaned posting list entries never surface in results.
type Worker struct {
	idx            *Index
	input          chan IndexJob
	stopCh         chan struct{}
	stopped        atomic.Bool
	dropped        atomic.Int64
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
	n := runtime.NumCPU()
	w := &Worker{
		idx:    idx,
		input:  make(chan IndexJob, workerBufSize),
		stopCh: make(chan struct{}),
		done:   make(chan struct{}),
	}
	w.wg.Add(n)
	for range n {
		go func() {
			defer w.wg.Done()
			defer func() {
				if r := recover(); r != nil {
					// Swallow closed-DB panics that surface as panics in the FTS
					// indexing path when Pebble is torn down before all goroutines exit.
					if ftsIsClosedPanic(r) || w.stopped.Load() {
						return
					}
					slog.Error("fts: worker goroutine panicked", "panic", r)
				}
			}()
			w.run()
		}()
	}
	return w
}

// Submit enqueues an FTS index job. Non-blocking — drops and warns if queue is full.
// Returns true if the job was accepted, false if dropped (including after Stop).
func (w *Worker) Submit(job IndexJob) bool {
	if w.stopped.Load() {
		return false
	}
	select {
	case w.input <- job:
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
		case <-ticker.C:
			flush()
		}
	}
}

func (w *Worker) flush(jobs []IndexJob) {
	for _, job := range jobs {
		if _, dropping := w.clearingVaults.Load(job.WS); dropping {
			continue
		}
		if err := w.idx.IndexEngram(job.WS, job.ID, job.Concept, job.CreatedBy, job.Content, job.Tags); err != nil {
			slog.Warn("fts: worker failed to index engram",
				"engram_id", job.ID,
				"err", err,
			)
		}
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
