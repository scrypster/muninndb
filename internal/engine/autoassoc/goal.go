// Package autoassoc provides write-time automatic association creation.
package autoassoc

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/scrypster/muninndb/internal/index/hnsw"
	"github.com/scrypster/muninndb/internal/storage"
)

const (
	goalBufSize     = 512
	goalTopK        = 5
	goalMinSim      = float32(0.6)
	goalAssocWeight = float32(0.4)
	goalJobTimeout  = 5 * time.Second
	maxGoalLinks    = 20
)

// GoalJob is a pending goal→neighbor linking task.
type GoalJob struct {
	WS        [8]byte
	ID        [16]byte
	Embedding []float32
}

// GoalStore is the storage interface needed by GoalLinkWorker.
type GoalStore interface {
	WriteAssociation(ctx context.Context, wsPrefix [8]byte, sourceID, targetID storage.ULID, assoc *storage.Association) error
}

// GoalHNSW is the HNSW search interface needed by GoalLinkWorker.
type GoalHNSW interface {
	Search(ctx context.Context, ws [8]byte, vec []float32, topK int) ([]hnsw.ScoredID, error)
}

// GoalLinkWorker auto-links goal engrams to semantically related engrams at write time.
// For each new TypeGoal engram, it queries HNSW for topK=5 neighbors with
// cosine similarity >= 0.6 and creates RelSupports associations.
type GoalLinkWorker struct {
	jobs  chan GoalJob
	store GoalStore
	hnsw  GoalHNSW
	wg    sync.WaitGroup
	// jobWG tracks in-flight+queued jobs; see Worker.jobWG in autoassoc.go
	// for the same pattern. Lets WaitIdle() block deterministically.
	jobWG   sync.WaitGroup
	stopCtx context.Context
}

// NewGoalLinkWorker creates a new GoalLinkWorker and starts a single worker goroutine.
// Call Stop() to drain the queue and shut down cleanly.
func NewGoalLinkWorker(ctx context.Context, store GoalStore, hnswIdx GoalHNSW) *GoalLinkWorker {
	w := &GoalLinkWorker{
		jobs:    make(chan GoalJob, goalBufSize),
		store:   store,
		hnsw:    hnswIdx,
		stopCtx: ctx,
	}
	w.wg.Add(1)
	go w.run()
	return w
}

// EnqueueGoalJob submits a job. If the queue is full, the job is dropped silently.
func (w *GoalLinkWorker) EnqueueGoalJob(job GoalJob) {
	w.jobWG.Add(1) // Add BEFORE the send so a concurrent WaitIdle() can't observe a false-idle gap.
	select {
	case w.jobs <- job:
	default:
		w.jobWG.Done() // never queued — nothing to wait for
		slog.Warn("goal_link: job queue full, dropping", "id", storage.ULID(job.ID).String())
	}
}

// WaitIdle blocks until every job enqueued so far has finished processing.
// Test-only synchronization helper; see Worker.WaitIdle in autoassoc.go.
func (w *GoalLinkWorker) WaitIdle() {
	w.jobWG.Wait()
}

// Stop drains the queue and waits for the worker to finish.
func (w *GoalLinkWorker) Stop() {
	close(w.jobs)
	w.wg.Wait()
}

func (w *GoalLinkWorker) run() {
	defer w.wg.Done()
	for job := range w.jobs {
		w.process(job)
	}
}

// process handles a single job. jobWG.Done() is deferred here (rather than
// called as a plain statement in run() after process returns) so it still
// fires if process ever panics — otherwise a panicking job would leak an
// un-Done() jobWG entry and hang a subsequent WaitIdle() forever. This does
// not add a recover(): a panic still propagates and crashes the process, it
// only makes the bookkeeping robust if one unwinds through here.
func (w *GoalLinkWorker) process(job GoalJob) {
	defer w.jobWG.Done()
	if w.hnsw == nil {
		slog.Warn("goal_link: hnsw index not initialized, skipping", "id", storage.ULID(job.ID).String())
		return
	}

	ctx, cancel := context.WithTimeout(w.stopCtx, goalJobTimeout)
	defer cancel()

	neighbors, err := w.hnsw.Search(ctx, job.WS, job.Embedding, goalTopK)
	if err != nil {
		slog.Warn("goal_link: hnsw search failed", "id", storage.ULID(job.ID).String(), "err", err)
		return
	}

	srcID := storage.ULID(job.ID)
	linked := 0
	for _, n := range neighbors {
		if linked >= maxGoalLinks {
			break
		}
		if float32(n.Score) < goalMinSim {
			continue
		}
		if n.ID == job.ID {
			continue // skip self
		}
		dstID := storage.ULID(n.ID)
		assoc := &storage.Association{
			TargetID:   dstID,
			RelType:    storage.RelSupports,
			Weight:     goalAssocWeight,
			Confidence: 1.0,
			CreatedAt:  time.Now(),
		}
		if err := w.store.WriteAssociation(ctx, job.WS, srcID, dstID, assoc); err != nil {
			slog.Warn("goal_link: write assoc failed", "src", srcID.String(), "dst", dstID.String(), "err", err)
		} else {
			linked++
		}
	}
}
