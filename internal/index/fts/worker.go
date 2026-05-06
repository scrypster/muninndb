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
	CreatedAt int64 // epoch seconds
}

// Indexer is the minimal text indexing boundary used by Worker.
type Indexer interface {
	IndexEngram(ws [8]byte, id [16]byte, concept, createdBy, content string, tags []string, createdAt int64) error
}

// indexer is the internal interface Worker depends on for indexing engrams.
// *Index satisfies this interface; adapters/search backends also implement it.
type indexer interface {
	IndexEngram(ws [8]byte, id [16]byte, concept, createdBy, content string, tags []string, createdAt int64) error
}

// Worker processes FTS indexing jobs asynchronously off the write hot path.
// Jobs are distributed across NumCPU goroutines reading from a shared buffered channel.
// If the queue is full, the job is dropped and a warning is logged — the engram is
// already durably stored in Pebble; only keyword search visibility is delayed.
// Stale FTS entries for deleted engrams are harmless: Phase 6 of activation filters
// nil metadata results, so orphaned posting list entries never surface in results.
type Worker struct {
	idx            indexer
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
func NewWorker(idx Indexer) *Worker {
	return newWorkerWithIndex(idx)
}

// newWorkerWithIndex creates and starts an async FTS indexing worker pool using
// the provided indexer. This is the real constructor logic, extracted to allow
// injection of a stub indexer in tests.
func newWorkerWithIndex(idx indexer) *Worker {
	n := runtime.NumCPU()
	w := &Worker{
		idx:    idx,
		input:  make(chan IndexJob, workerBufSize),
		stopCh: make(chan struct{}),
		done:   make(chan struct{}),
	}
	w.wg.Add(n)
	for range n {
		go w.runLoop()
	}
	return w
}

// runLoop is the per-goroutine entry point. It wraps run() in a restart loop:
// after a non-fatal panic, the goroutine re-enters run() instead of exiting.
// wg.Done() only fires when the worker is cleanly stopped.
func (w *Worker) runLoop() {
	defer w.wg.Done()
	for !w.stopped.Load() {
		func() {
			defer func() {
				if r := recover(); r != nil {
					if ftsIsClosedPanic(r) || w.stopped.Load() {
						return
					}
					// Log non-closed-DB panics and restart the goroutine.
					slog.Error("fts: worker goroutine panicked (restarting)", "panic", r)
				}
			}()
			w.run()
		}()
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
	var batch []IndexJob
	flush := func() {
		if len(batch) == 0 {
			return
		}
		for _, job := range batch {
			// Check clearingVaults purely as an optimization so that
			// IndexEngram sees a consistent state on multi-goroutine runs.
			if _, clearing := w.clearingVaults.Load(job.WS); clearing {
				continue
			}
			if err := w.idx.IndexEngram(job.WS, job.ID, job.Concept, job.CreatedBy, job.Content, job.Tags, job.CreatedAt); err != nil {
				slog.Warn("fts: IndexEngram failed", "id", fmt.Sprintf("%x", job.ID), "err", err)
			}
		}
		batch = batch[:0]
	}

	timer := time.NewTimer(workerInterval)
	defer timer.Stop()

	for {
		select {
		case job, ok := <-w.input:
			if !ok {
				flush()
				return
			}
			batch = append(batch, job)
			if len(batch) >= workerBatchSize {
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				flush()
				timer.Reset(workerInterval)
			}
		case <-timer.C:
			flush()
			timer.Reset(workerInterval)
		case <-w.stopCh:
			// Drain remaining jobs in the channel before exiting.
			for {
				select {
				case job, ok := <-w.input:
					if !ok {
						flush()
						return
					}
					batch = append(batch, job)
				default:
					flush()
					return
				}
			}
		}
	}
}

func ftsIsClosedPanic(r interface{}) bool {
	if s, ok := r.(string); ok {
		return strings.Contains(s, "closed")
	}
	return false
}
