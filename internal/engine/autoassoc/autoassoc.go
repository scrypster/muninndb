// Package autoassoc provides write-time automatic association creation.
// When a new engram is written, this worker finds existing engrams that
// share tags with the new engram and creates RELATES_TO associations
// between them. This runs asynchronously in a bounded worker pool so
// it never blocks the write path.
package autoassoc

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/scrypster/muninndb/internal/index/fts"
	"github.com/scrypster/muninndb/internal/storage"
)

const (
	// JobBufSize is the capacity of the work queue channel.
	JobBufSize = 1024
	// NumWorkers is the number of goroutines processing jobs.
	NumWorkers = 4
	// MaxTagQueries is the maximum number of tags queried per write.
	// We take the first MaxTagQueries tags (by position, not IDF, to keep it simple).
	MaxTagQueries = 5
	// MaxAssociations is the maximum number of auto-associations per write.
	MaxAssociations = 10
	// AssocWeight is the link weight for automatically created associations.
	AssocWeight = float32(0.3)
	// JobTimeout is the per-job context timeout.
	JobTimeout = 5 * time.Second
)

// Job is a unit of work for the auto-association worker.
type Job struct {
	WSPrefix [8]byte
	NewID    storage.ULID
	Tags     []string
}

// Metrics are the runtime counters for the worker pool.
type Metrics struct {
	Enqueued  atomic.Int64
	Completed atomic.Int64
	Dropped   atomic.Int64
	Errors    atomic.Int64
}

// Store is the subset of storage.Store used by the worker.
type Store interface {
	WriteAssociation(ctx context.Context, wsPrefix [8]byte, sourceID, targetID storage.ULID, assoc *storage.Association) error
}

// FTSIndex is the subset of the FTS index used by the worker.
type FTSIndex interface {
	Search(ctx context.Context, ws [8]byte, query string, topK int) ([]fts.ScoredID, error)
}

// Worker is the auto-association worker pool.
// It is safe for concurrent use.
type Worker struct {
	jobs    chan Job
	store   Store
	fts     FTSIndex
	metrics *Metrics
	wg      sync.WaitGroup
	// jobWG tracks in-flight+queued jobs (Add on successful Enqueue, Done
	// after processJob returns). Separate from wg (which tracks the fixed
	// pool of NumWorkers goroutines). Lets WaitIdle() block until the queue
	// has been fully drained without stopping the worker pool.
	jobWG   sync.WaitGroup
	stopCtx context.Context
}

// New creates a new Worker and starts NumWorkers goroutines.
// Call Stop() to drain the queue and shut down cleanly.
func New(ctx context.Context, store Store, fts FTSIndex) *Worker {
	w := &Worker{
		jobs:    make(chan Job, JobBufSize),
		store:   store,
		fts:     fts,
		metrics: &Metrics{},
		stopCtx: ctx,
	}
	for i := 0; i < NumWorkers; i++ {
		w.wg.Add(1)
		go w.run()
	}
	return w
}

// Enqueue submits a job to the worker pool. If the queue is full, the job
// is dropped (non-blocking) and the Dropped counter is incremented.
func (w *Worker) Enqueue(job Job) {
	w.jobWG.Add(1) // Add BEFORE the send so a concurrent WaitIdle() can't observe a false-idle gap.
	select {
	case w.jobs <- job:
		w.metrics.Enqueued.Add(1)
	default:
		w.jobWG.Done() // never queued — nothing to wait for
		w.metrics.Dropped.Add(1)
	}
}

// WaitIdle blocks until every job enqueued so far has finished processing
// (completed or errored). Test-only synchronization helper: production
// callers never await this — write-time association is fire-and-forget by
// design. It does not stop or otherwise alter the worker pool's scheduling.
func (w *Worker) WaitIdle() {
	w.jobWG.Wait()
}

// Stop drains all pending jobs and waits for in-flight work to complete.
// After Stop returns, no new jobs should be enqueued.
func (w *Worker) Stop() {
	close(w.jobs)
	w.wg.Wait()
}

// GetMetrics returns a snapshot of the current counters.
func (w *Worker) GetMetrics() (enqueued, completed, dropped, errors int64) {
	return w.metrics.Enqueued.Load(),
		w.metrics.Completed.Load(),
		w.metrics.Dropped.Load(),
		w.metrics.Errors.Load()
}

// run is the worker loop. Processes jobs until the channel is closed.
func (w *Worker) run() {
	defer w.wg.Done()
	for job := range w.jobs {
		w.runJob(job)
	}
}

// runJob processes a single job. Done() and cancel() are deferred (rather
// than called as plain statements after processJob returns) so they still
// fire if processJob ever panics — otherwise a panicking job would leak an
// un-Done() jobWG entry and hang a subsequent WaitIdle() forever. This does
// not add a recover(): a panic still propagates and crashes the process,
// it only makes the bookkeeping robust if one unwinds through here.
func (w *Worker) runJob(job Job) {
	defer w.jobWG.Done()
	ctx, cancel := context.WithTimeout(w.stopCtx, JobTimeout)
	defer cancel()
	if err := w.processJob(ctx, job); err != nil {
		w.metrics.Errors.Add(1)
		slog.Warn("autoassoc job failed", "err", err)
	} else {
		w.metrics.Completed.Add(1)
	}
}

// processJob executes a single auto-association job.
// For each of the first MaxTagQueries tags, it searches FTS for engrams
// sharing that tag. Candidates are deduplicated and limited to MaxAssociations.
// A RELATES_TO link (weight=0.3) is created for each candidate.
func (w *Worker) processJob(ctx context.Context, job Job) error {
	if len(job.Tags) == 0 {
		return nil
	}

	// Limit tags queried
	tags := job.Tags
	if len(tags) > MaxTagQueries {
		tags = tags[:MaxTagQueries]
	}

	// Collect unique candidate IDs from all tag queries, excluding the new engram itself.
	seen := make(map[storage.ULID]bool, MaxAssociations*2)
	seen[job.NewID] = true // always exclude self

	var candidates []storage.ULID

	for _, tag := range tags {
		if len(candidates) >= MaxAssociations {
			break
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}

		results, err := w.fts.Search(ctx, job.WSPrefix, tag, MaxAssociations)
		if err != nil {
			// Non-fatal: FTS search failure for one tag shouldn't abort the job
			slog.Debug("autoassoc FTS search error", "tag", tag, "err", err)
			continue
		}

		for _, r := range results {
			id := storage.ULID(r.ID)
			if !seen[id] {
				seen[id] = true
				candidates = append(candidates, id)
				if len(candidates) >= MaxAssociations {
					break
				}
			}
		}
	}

	if len(candidates) == 0 {
		return nil
	}

	// Create RELATES_TO associations from the new engram to each candidate.
	for _, targetID := range candidates {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		assoc := &storage.Association{
			TargetID:   targetID,
			RelType:    storage.RelRelatesTo,
			Weight:     AssocWeight,
			Confidence: 1.0,
			CreatedAt:  time.Now(),
		}
		if err := w.store.WriteAssociation(ctx, job.WSPrefix, job.NewID, targetID, assoc); err != nil {
			slog.Debug("autoassoc write association failed", "err", err)
			// Non-fatal: continue with remaining candidates
		}
	}

	return nil
}
