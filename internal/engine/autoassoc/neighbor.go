// Package autoassoc provides write-time automatic association creation.
package autoassoc

import (
	"context"
	"log/slog"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/scrypster/muninndb/internal/index/hnsw"
	"github.com/scrypster/muninndb/internal/storage"
)

const (
	neighborBufSize    = 4096
	neighborBatchSize  = 32
	neighborTopK       = 10
	neighborMinSim     = float32(0.7)
	neighborMaxWeight  = float32(0.5)
	neighborJobTimeout = 5 * time.Second
)

// NeighborJob is a pending semantic neighbor linking task.
type NeighborJob struct {
	WS        [8]byte
	ID        [16]byte
	Embedding []float32
}

// NeighborStore is the storage interface needed by NeighborWorker.
type NeighborStore interface {
	WriteAssociation(ctx context.Context, wsPrefix [8]byte, sourceID, targetID storage.ULID, assoc *storage.Association) error
}

// NeighborHNSW is the HNSW search interface needed by NeighborWorker.
type NeighborHNSW interface {
	Search(ctx context.Context, ws [8]byte, vec []float32, topK int) ([]hnsw.ScoredID, error)
}

// NeighborMetrics are the runtime counters for the neighbor worker pool.
type NeighborMetrics struct {
	Enqueued  atomic.Int64
	Completed atomic.Int64
	Dropped   atomic.Int64
	Errors    atomic.Int64
}

// NeighborWorker auto-links semantically similar engrams at write time.
// It queries HNSW for the top-10 nearest neighbors of a new embedding,
// then creates RelRelatesTo associations for neighbors with cosine similarity > 0.7.
type NeighborWorker struct {
	jobs    chan NeighborJob
	store   NeighborStore
	hnsw    NeighborHNSW
	metrics *NeighborMetrics
	wg      sync.WaitGroup
	// jobWG tracks in-flight+queued jobs; see Worker.jobWG in autoassoc.go
	// for the same pattern. Lets WaitIdle() block deterministically.
	jobWG   sync.WaitGroup
	stopCtx context.Context
}

// NewNeighborWorker creates a new NeighborWorker and starts worker goroutines.
// Call Stop() to drain the queue and shut down cleanly.
func NewNeighborWorker(ctx context.Context, store NeighborStore, hnsw NeighborHNSW) *NeighborWorker {
	numWorkers := runtime.NumCPU()
	w := &NeighborWorker{
		jobs:    make(chan NeighborJob, neighborBufSize),
		store:   store,
		hnsw:    hnsw,
		metrics: &NeighborMetrics{},
		stopCtx: ctx,
	}
	for i := 0; i < numWorkers; i++ {
		w.wg.Add(1)
		go w.run()
	}
	return w
}

// EnqueueNeighborJob submits a job to the worker pool. If the queue is full,
// the job is dropped (non-blocking) and the Dropped counter is incremented.
// Drops are logged at powers of 2.
func (w *NeighborWorker) EnqueueNeighborJob(job NeighborJob) {
	w.jobWG.Add(1) // Add BEFORE the send so a concurrent WaitIdle() can't observe a false-idle gap.
	select {
	case w.jobs <- job:
		w.metrics.Enqueued.Add(1)
	default:
		w.jobWG.Done() // never queued — nothing to wait for
		dropped := w.metrics.Dropped.Add(1)
		// Log at powers of 2: 1, 2, 4, 8, 16, ...
		if dropped&(dropped-1) == 0 {
			slog.Warn("neighbor worker queue full, dropping job", "dropped", dropped)
		}
	}
}

// WaitIdle blocks until every job enqueued so far has finished processing.
// Test-only synchronization helper; see Worker.WaitIdle in autoassoc.go.
func (w *NeighborWorker) WaitIdle() {
	w.jobWG.Wait()
}

// Stop drains all pending jobs and waits for in-flight work to complete.
// After Stop returns, no new jobs should be enqueued.
func (w *NeighborWorker) Stop() {
	close(w.jobs)
	w.wg.Wait()
}

// GetNeighborMetrics returns a snapshot of the current counters.
func (w *NeighborWorker) GetNeighborMetrics() (enqueued, completed, dropped, errors int64) {
	return w.metrics.Enqueued.Load(),
		w.metrics.Completed.Load(),
		w.metrics.Dropped.Load(),
		w.metrics.Errors.Load()
}

// run is the worker loop. Processes jobs until the channel is closed.
func (w *NeighborWorker) run() {
	defer w.wg.Done()
	for job := range w.jobs {
		w.runJob(job)
	}
}

// runJob processes a single job. Done() and cancel() are deferred (rather
// than called as plain statements after processNeighborJob returns) so they
// still fire if the job ever panics — otherwise a panicking job would leak
// an un-Done() jobWG entry and hang a subsequent WaitIdle() forever. This
// does not add a recover(): a panic still propagates and crashes the
// process, it only makes the bookkeeping robust if one unwinds through here.
func (w *NeighborWorker) runJob(job NeighborJob) {
	defer w.jobWG.Done()
	ctx, cancel := context.WithTimeout(w.stopCtx, neighborJobTimeout)
	defer cancel()
	if err := w.processNeighborJob(ctx, job); err != nil {
		w.metrics.Errors.Add(1)
		slog.Warn("neighbor worker job failed", "err", err)
	} else {
		w.metrics.Completed.Add(1)
	}
}

// processNeighborJob queries HNSW for the top-K nearest neighbors of the embedding,
// then creates RelRelatesTo associations for neighbors with similarity > neighborMinSim.
// Association weight is capped at neighborMaxWeight.
func (w *NeighborWorker) processNeighborJob(ctx context.Context, job NeighborJob) error {
	if len(job.Embedding) == 0 {
		return nil
	}

	start := time.Now()

	// Query HNSW for top-K neighbors
	results, err := w.hnsw.Search(ctx, job.WS, job.Embedding, neighborTopK)
	if err != nil {
		slog.Debug("neighbor worker HNSW search error", "err", err)
		return nil // non-fatal
	}

	if len(results) == 0 {
		return nil
	}

	var edgesCreated int
	var simSum float64

	// Create associations for neighbors meeting the similarity threshold
	for _, result := range results {
		// Skip self-links
		if result.ID == job.ID {
			continue
		}

		// Skip neighbors below similarity threshold
		if float32(result.Score) < neighborMinSim {
			continue
		}

		// Weight is similarity * 0.5, capped at neighborMaxWeight
		weight := float32(result.Score) * 0.5
		if weight > neighborMaxWeight {
			weight = neighborMaxWeight
		}

		if ctx.Err() != nil {
			return ctx.Err()
		}

		assoc := &storage.Association{
			TargetID:   storage.ULID(result.ID),
			RelType:    storage.RelRelatesTo,
			Weight:     weight,
			Confidence: 1.0,
			CreatedAt:  time.Now(),
		}

		if err := w.store.WriteAssociation(ctx, job.WS, storage.ULID(job.ID), storage.ULID(result.ID), assoc); err != nil {
			slog.Debug("neighbor worker write association failed", "err", err)
			// Non-fatal: continue with remaining neighbors
		} else {
			edgesCreated++
			simSum += float64(result.Score)
		}
	}

	dur := time.Since(start)
	if edgesCreated > 0 {
		avgSim := float32(simSum) / float32(edgesCreated)
		slog.Info("neighbor worker created edges",
			"edges", edgesCreated,
			"avg_sim", avgSim,
			"duration_ms", dur.Milliseconds(),
			"candidates", len(results),
		)
	} else {
		slog.Debug("neighbor worker processed job, no edges",
			"candidates", len(results),
			"duration_ms", dur.Milliseconds(),
		)
	}

	return nil
}
