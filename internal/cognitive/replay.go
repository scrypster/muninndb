package cognitive

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// ReplayActivator is the engine method the worker calls for synthetic activation.
// Implementations should route through the normal ACTIVATE pipeline so that
// Hebbian learning, PAS transitions, and contradiction detection all fire.
type ReplayActivator interface {
	SyntheticActivate(ctx context.Context, vault string, queryContext string) error
}

// ReplayStore provides read access to vaults and recent engrams for replay.
type ReplayStore interface {
	ListVaults(ctx context.Context) ([]string, error)
	RecentEngrams(ctx context.Context, vault string, limit int) ([]ReplayEngram, error)
}

// ReplayEngram is the minimal engram representation needed for replay.
type ReplayEngram struct {
	ID      [16]byte
	Concept string
	Content string
	Vault   string
}

// ReplayWorker periodically runs synthetic ACTIVATE calls on recent engrams
// to strengthen Hebbian associations and trigger PAS transitions. This is the
// biological analogue of hippocampal sleep replay / memory consolidation.
//
// It is NOT an event-driven Worker[T]. It follows the periodic background
// goroutine pattern (like runPruneWorker / runCoherenceFlush).
type ReplayWorker struct {
	config    ReplayConfig
	activator ReplayActivator
	store     ReplayStore
	stopCh    chan struct{}
	done      chan struct{}
}

// NewReplayWorker creates a new hippocampal replay worker.
func NewReplayWorker(config ReplayConfig, activator ReplayActivator, store ReplayStore) *ReplayWorker {
	return &ReplayWorker{
		config:    config,
		activator: activator,
		store:     store,
		stopCh:    make(chan struct{}),
		done:      make(chan struct{}),
	}
}

// Run starts the periodic replay loop. Blocks until the stop channel is
// closed or the context is cancelled.
func (rw *ReplayWorker) Run(ctx context.Context) {
	defer close(rw.done)
	ticker := time.NewTicker(rw.config.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			rw.replayCycle(ctx)
		case <-rw.stopCh:
			return
		case <-ctx.Done():
			return
		}
	}
}

// Stop signals the replay worker to shut down.
func (rw *ReplayWorker) Stop() {
	select {
	case <-rw.stopCh:
		// already stopped
	default:
		close(rw.stopCh)
	}
}

// Done returns a channel that is closed when the worker has exited.
func (rw *ReplayWorker) Done() <-chan struct{} {
	return rw.done
}

// replayCycle runs one full replay pass across all vaults.
func (rw *ReplayWorker) replayCycle(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("replay: cycle panicked", "panic", r)
		}
	}()

	vaults, err := rw.store.ListVaults(ctx)
	if err != nil {
		slog.Warn("replay: failed to list vaults", "error", err)
		return
	}

	slog.Info("replay: starting consolidation cycle",
		"vaults", len(vaults),
		"max_engrams_per_vault", rw.config.MaxEngrams,
		"learning_rate", rw.config.LearningRate)

	var totalActivated int
	for _, vault := range vaults {
		if ctx.Err() != nil {
			return
		}

		engrams, err := rw.store.RecentEngrams(ctx, vault, rw.config.MaxEngrams)
		if err != nil {
			slog.Warn("replay: failed to get recent engrams",
				"vault", vault, "error", err)
			continue
		}
		if len(engrams) == 0 {
			continue
		}

		slog.Debug("replay: replaying vault",
			"vault", vault, "engrams", len(engrams))

		for _, eg := range engrams {
			if ctx.Err() != nil {
				return
			}

			// Build a synthetic activation context from the engram's concept and content.
			// Truncate content to avoid excessively long queries.
			queryCtx := eg.Concept
			if eg.Content != "" {
				snippet := eg.Content
				if len(snippet) > 200 {
					snippet = snippet[:200]
				}
				queryCtx = fmt.Sprintf("%s: %s", eg.Concept, snippet)
			}

			if err := rw.activator.SyntheticActivate(ctx, vault, queryCtx); err != nil {
				slog.Debug("replay: synthetic activate failed",
					"vault", vault,
					"engram", fmt.Sprintf("%x", eg.ID),
					"error", err)
				continue
			}
			totalActivated++
		}
	}

	slog.Info("replay: consolidation cycle complete",
		"total_activated", totalActivated)
}
