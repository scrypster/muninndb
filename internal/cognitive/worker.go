package cognitive

import (
	"context"
	"sync/atomic"
	"time"

	"golang.org/x/sync/errgroup"
)

// WorkerState represents the dormancy state of a worker.
type WorkerState int32

const (
	WorkerStateActive  WorkerState = 0
	WorkerStateIdle    WorkerState = 1
	WorkerStateDormant WorkerState = 2
)

const (
	defaultIdleThreshold    = 5 * time.Minute
	defaultDormantThreshold = 30 * time.Minute
	defaultIdlePoll         = 60 * time.Second
	defaultDormantPoll      = 5 * time.Minute
)

// WorkerStats holds telemetry counters.
type WorkerStats struct {
	Processed     uint64        `json:"processed"`
	Batches       uint64        `json:"batches"`
	Errors        uint64        `json:"errors"`
	Dropped       uint64        `json:"dropped"`
	LastRun       int64         `json:"lastRun"` // Unix nanoseconds
	State         WorkerState   `json:"state"`
	EffectiveWait time.Duration `json:"effectiveWait"` // current actual tick interval
}

// Worker is the generic goroutine lifecycle for cognitive workers.
type Worker[T any] struct {
	input     chan T
	process   func(ctx context.Context, batch []T) error
	batchSize int
	maxWait   atomic.Int64
	baseMaxWait time.Duration // original maxWait, never changes

	adaptiveScaling bool

	processed atomic.Uint64
	batches   atomic.Uint64
	errors    atomic.Uint64
	lastRun   atomic.Int64
	dropped   atomic.Uint64

	state            atomic.Int32
	lastItem         atomic.Int64 // unix nanos of last submitted item
	idleThreshold    time.Duration
	dormantThreshold time.Duration
	idlePoll         time.Duration // ticker interval when idle (overridable for tests)
	dormantPoll      time.Duration // ticker interval when dormant (overridable for tests)
}

// NewWorker creates a new generic Worker.
func NewWorker[T any](bufSize, batchSize int, maxWait time.Duration, process func(ctx context.Context, batch []T) error) *Worker[T] {
	w := &Worker[T]{
		input:            make(chan T, bufSize),
		process:          process,
		batchSize:        batchSize,
		baseMaxWait:      maxWait,
		idleThreshold:    defaultIdleThreshold,
		dormantThreshold: defaultDormantThreshold,
		idlePoll:         defaultIdlePoll,
		dormantPoll:      defaultDormantPoll,
	}
	w.maxWait.Store(int64(maxWait))
	return w
}

// EnableAdaptiveScaling enables queue-pressure-based interval auto-tuning.
// Call after NewWorker, before Run.
func (w *Worker[T]) EnableAdaptiveScaling() {
	w.adaptiveScaling = true
}

// SetThresholds overrides the default idle/dormant thresholds. Used in tests.
// Poll intervals are derived from the thresholds so state re-evaluation happens
// fast enough for the test to observe transitions without waiting minutes.
func (w *Worker[T]) SetThresholds(idle, dormant time.Duration) {
	w.idleThreshold = idle
	w.dormantThreshold = dormant
	// idle poll: fire quickly enough to catch the dormant transition
	idlePoll := idle / 5
	if idlePoll < time.Millisecond {
		idlePoll = time.Millisecond
	}
	w.idlePoll = idlePoll
	// dormant poll: fire quickly enough to observe dormant-check in tests
	dormantPoll := dormant / 5
	if dormantPoll < time.Millisecond {
		dormantPoll = time.Millisecond
	}
	w.dormantPoll = dormantPoll
}

// Submit enqueues an item for processing. Never blocks (drops if full).
func (w *Worker[T]) Submit(item T) bool {
	select {
	case w.input <- item:
		w.lastItem.Store(time.Now().UnixNano())
		return true
	default:
		w.dropped.Add(1)
		return false // channel full, drop
	}
}

// SubmitBatch enqueues multiple items at once. Never blocks; drops items when the
// channel is full. Reduces per-item channel overhead compared to calling Submit in a loop.
func (w *Worker[T]) SubmitBatch(items []T) {
	now := time.Now().UnixNano()
	for _, item := range items {
		select {
		case w.input <- item:
		default:
			w.dropped.Add(1)
		}
	}
	if len(items) > 0 {
		w.lastItem.Store(now)
	}
}

// Run starts the worker loop. Blocks until ctx is done.
func (w *Worker[T]) Run(ctx context.Context) error {
	batch := make([]T, 0, w.batchSize)
	ticker := time.NewTicker(time.Duration(w.maxWait.Load()))
	defer ticker.Stop()

	flush := func(flushCtx context.Context) {
		if len(batch) == 0 {
			return
		}
		w.lastRun.Store(time.Now().UnixNano())
		if err := w.process(flushCtx, batch); err != nil {
			w.errors.Add(1)
		} else {
			w.processed.Add(uint64(len(batch)))
			w.batches.Add(1)
		}
		batch = batch[:0]
	}

	for {
		// Update state based on time since last item.
		lastItem := w.lastItem.Load()
		var idleFor time.Duration
		if lastItem == 0 {
			idleFor = w.dormantThreshold + 1 // never seen anything → treat as dormant
		} else {
			idleFor = time.Since(time.Unix(0, lastItem))
		}

		switch {
		case idleFor >= w.dormantThreshold:
			if WorkerState(w.state.Load()) != WorkerStateDormant {
				w.state.Store(int32(WorkerStateDormant))
				ticker.Reset(w.dormantPoll)
			}
		case idleFor >= w.idleThreshold:
			if WorkerState(w.state.Load()) != WorkerStateIdle {
				w.state.Store(int32(WorkerStateIdle))
				ticker.Reset(w.idlePoll)
			}
		default:
			if WorkerState(w.state.Load()) != WorkerStateActive {
				w.state.Store(int32(WorkerStateActive))
				ticker.Reset(time.Duration(w.maxWait.Load()))
			}
		}

		select {
		case <-ctx.Done():
			// Drain any items still in the channel into the pending batch before
			// flushing, so no submitted work is silently dropped on shutdown.
			for {
				select {
				case item, ok := <-w.input:
					if !ok {
						goto shutdownFlush
					}
					batch = append(batch, item)
				default:
					goto shutdownFlush
				}
			}
		shutdownFlush:
			// Use a fresh context for the final flush so store writes succeed
			// even though the caller context has already been cancelled.
			shutCtx, shutCancel := context.WithTimeout(context.Background(), 3*time.Second)
			flush(shutCtx)
			shutCancel()
			return ctx.Err()

		case item, ok := <-w.input:
			if !ok {
				shutCtx, shutCancel := context.WithTimeout(context.Background(), 3*time.Second)
				flush(shutCtx)
				shutCancel()
				return nil
			}
			w.lastItem.Store(time.Now().UnixNano())
			w.state.Store(int32(WorkerStateActive))
			ticker.Reset(time.Duration(w.maxWait.Load()))
			batch = append(batch, item)
			if len(batch) >= w.batchSize {
				flush(ctx)
			}

		case <-ticker.C:
			// Measure pressure before flush: items in channel + items
			// already drained into the pending batch this tick cycle.
			queueLen := len(w.input) + len(batch)
			flush(ctx)
			if w.adaptiveScaling && WorkerState(w.state.Load()) == WorkerStateActive {
				pressure := float64(queueLen) / float64(cap(w.input))
				switch {
				case pressure > 0.75:
					// Queue backing up — tighten interval, minimum 500ms
					curWait := time.Duration(w.maxWait.Load())
					newWait := curWait / 2
					if newWait < 500*time.Millisecond {
						newWait = 500 * time.Millisecond
					}
					if newWait != curWait {
						w.maxWait.Store(int64(newWait))
						ticker.Reset(newWait)
					}
				case pressure < 0.10 && time.Duration(w.maxWait.Load()) < w.baseMaxWait:
					// Queue draining — relax toward original
					curWait := time.Duration(w.maxWait.Load())
					newWait := curWait * 2
					if newWait > w.baseMaxWait {
						newWait = w.baseMaxWait
					}
					if newWait != curWait {
						w.maxWait.Store(int64(newWait))
						ticker.Reset(newWait)
					}
				}
			}
		}
	}
}

// Stats returns current telemetry.
func (w *Worker[T]) Stats() WorkerStats {
	return WorkerStats{
		Processed:     w.processed.Load(),
		Batches:       w.batches.Load(),
		Errors:        w.errors.Load(),
		Dropped:       w.dropped.Load(),
		LastRun:       w.lastRun.Load(),
		State:         WorkerState(w.state.Load()),
		EffectiveWait: time.Duration(w.maxWait.Load()),
	}
}

// EngineWorkerStats holds the combined statistics for all cognitive workers.
type EngineWorkerStats struct {
	Hebbian    WorkerStats   `json:"hebbian"`
	Contradict WorkerStats   `json:"contradict"`
	Confidence WorkerStats   `json:"confidence"`
	Episode    WorkerStats   `json:"episode"`
	Replay     ReplayMetrics `json:"replay"`
}

// StartAll runs multiple workers via errgroup. Returns first error.
func StartAll(ctx context.Context, workers []interface{ Run(context.Context) error }) error {
	g, gctx := errgroup.WithContext(ctx)
	for _, w := range workers {
		w := w // capture
		g.Go(func() error {
			return w.Run(gctx)
		})
	}
	return g.Wait()
}
