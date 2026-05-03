package cognitive

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
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

// ConsolidationStore provides episode access and summary storage for the
// hippocampal→neocortical consolidation path during replay.
//
// TODO: Return episode ID and member IDs alongside concepts for dedup and
// RelIsPartOf linking. Current simplified interface is sufficient for the noop
// default and will be expanded when wiring the real store.
type ConsolidationStore interface {
	// GetEpisodeConcepts returns the concepts of engrams in an episode
	// identified by seedID (the first member's ULID string).
	GetEpisodeConcepts(ctx context.Context, vault string, seedID string) ([]string, error)

	// StoreConsolidation persists a summary engram and links it to episode
	// members via RelIsPartOf associations.
	StoreConsolidation(ctx context.Context, vault string, summary string, memberIDs []string) error
}

// noopConsolidationStore is a no-op implementation used when no real store is
// wired. All operations succeed without side effects.
type noopConsolidationStore struct{}

func (noopConsolidationStore) GetEpisodeConcepts(_ context.Context, _ string, _ string) ([]string, error) {
	return nil, nil
}

func (noopConsolidationStore) StoreConsolidation(_ context.Context, _ string, _ string, _ []string) error {
	return nil
}

// ReplayEngram is the minimal engram representation needed for replay.
type ReplayEngram struct {
	ID      [16]byte
	Concept string
	Content string
	Vault   string
}

// ReplayMetrics holds effectiveness counters for the replay worker.
// All fields are read via atomic loads and are safe for concurrent access.
type ReplayMetrics struct {
	CyclesCompleted   uint64        // total replay cycles run
	EngramsReplayed   uint64        // total engrams replayed across all cycles
	VaultsProcessed   uint64        // total vaults processed
	LastCycleDuration time.Duration // wall-clock duration of most recent cycle
	LastCycleAt       time.Time     // completion time of most recent cycle
	Errors            uint64        // activation errors during replay
}

// ReplayWorker periodically runs synthetic ACTIVATE calls on recent engrams
// to strengthen Hebbian associations and trigger PAS transitions. This is the
// biological analogue of hippocampal sleep replay / memory consolidation.
//
// It is NOT an event-driven Worker[T]. It follows the periodic background
// goroutine pattern (like runPruneWorker / runCoherenceFlush).
type ReplayWorker struct {
	config             ReplayConfig
	activator          ReplayActivator
	store              ReplayStore
	consolidationStore ConsolidationStore
	stopCh             chan struct{}
	done               chan struct{}

	// metrics — thread-safe via sync/atomic
	cyclesCompleted atomic.Uint64
	engramsReplayed atomic.Uint64
	vaultsProcessed atomic.Uint64
	lastCycleDur    atomic.Int64 // stored as time.Duration (int64 nanoseconds)
	lastCycleAt     atomic.Int64 // stored as UnixNano
	errors          atomic.Uint64
}

// NewReplayWorker creates a new hippocampal replay worker.
func NewReplayWorker(config ReplayConfig, activator ReplayActivator, store ReplayStore) *ReplayWorker {
	return &ReplayWorker{
		config:             config,
		activator:          activator,
		store:              store,
		consolidationStore: noopConsolidationStore{},
		stopCh:             make(chan struct{}),
		done:               make(chan struct{}),
	}
}

// SetConsolidationStore wires a real ConsolidationStore for episode summary
// generation during replay. Call before Run().
//
// Note: EnableConsolidation in ReplayConfig has no effect without calling
// SetConsolidationStore with a non-nil, non-noop implementation. The default
// noopConsolidationStore silently discards all consolidation operations.
func (rw *ReplayWorker) SetConsolidationStore(cs ConsolidationStore) {
	if cs != nil {
		rw.consolidationStore = cs
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

// Metrics returns a snapshot of the replay effectiveness counters.
// Safe to call concurrently while the worker is running.
func (rw *ReplayWorker) Metrics() ReplayMetrics {
	var lastAt time.Time
	if nanos := rw.lastCycleAt.Load(); nanos != 0 {
		lastAt = time.Unix(0, nanos)
	}
	return ReplayMetrics{
		CyclesCompleted:   rw.cyclesCompleted.Load(),
		EngramsReplayed:   rw.engramsReplayed.Load(),
		VaultsProcessed:   rw.vaultsProcessed.Load(),
		LastCycleDuration: time.Duration(rw.lastCycleDur.Load()),
		LastCycleAt:       lastAt,
		Errors:            rw.errors.Load(),
	}
}

// replayCycle runs one full replay pass across all vaults.
func (rw *ReplayWorker) replayCycle(ctx context.Context) {
	cycleStart := time.Now()

	defer func() {
		if r := recover(); r != nil {
			slog.Error("replay: cycle panicked", "panic", r)
		}
		// Record cycle timing regardless of success/panic.
		rw.lastCycleDur.Store(int64(time.Since(cycleStart)))
		rw.lastCycleAt.Store(time.Now().UnixNano())
		rw.cyclesCompleted.Add(1)
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

		rw.vaultsProcessed.Add(1)

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
				if len([]rune(snippet)) > 200 {
					snippet = string([]rune(snippet)[:200])
				}
				queryCtx = fmt.Sprintf("%s: %s", eg.Concept, snippet)
			}

			if err := rw.activator.SyntheticActivate(ctx, vault, queryCtx); err != nil {
				rw.errors.Add(1)
				slog.Debug("replay: synthetic activate failed",
					"vault", vault,
					"engram", fmt.Sprintf("%x", eg.ID),
					"error", err)
				continue
			}
			rw.engramsReplayed.Add(1)
			totalActivated++
		}
	}

	slog.Info("replay: activation pass complete",
		"total_activated", totalActivated)

	// Phase 2: Consolidation — generate episode summaries if enabled.
	if rw.config.EnableConsolidation {
		rw.consolidateCycle(ctx, vaults)
	}
}

// consolidateCycle runs the consolidation pass across all vaults, generating
// summary engrams for episodes that meet the minimum size threshold.
func (rw *ReplayWorker) consolidateCycle(ctx context.Context, vaults []string) {
	if _, isNoop := rw.consolidationStore.(noopConsolidationStore); isNoop {
		slog.Warn("replay: consolidation enabled but no real store wired (call SetConsolidationStore)")
		return
	}

	minSize := rw.config.ConsolidationMinSize
	if minSize <= 0 {
		minSize = 3
	}

	var totalConsolidated int
	for _, vault := range vaults {
		if ctx.Err() != nil {
			return
		}

		// Use RecentEngrams as seed IDs. Each engram might belong to an episode;
		// we deduplicate by tracking which seedIDs we've already consolidated.
		engrams, err := rw.store.RecentEngrams(ctx, vault, rw.config.MaxEngrams)
		if err != nil {
			slog.Warn("replay: consolidation: failed to get recent engrams",
				"vault", vault, "error", err)
			continue
		}

		consolidated := map[string]bool{} // track processed seedIDs
		for _, eg := range engrams {
			if ctx.Err() != nil {
				return
			}

			seedID := fmt.Sprintf("%x", eg.ID)
			if consolidated[seedID] {
				continue
			}

			concepts, err := rw.consolidationStore.GetEpisodeConcepts(ctx, vault, seedID)
			if err != nil {
				slog.Debug("replay: consolidation: get episode concepts failed",
					"vault", vault, "seed", seedID, "error", err)
				continue
			}
			if len(concepts) < minSize {
				consolidated[seedID] = true
				continue
			}

			summary := buildConsolidationSummary(concepts)
			if err := rw.consolidationStore.StoreConsolidation(ctx, vault, summary, nil); err != nil {
				slog.Warn("replay: consolidation: store failed",
					"vault", vault, "seed", seedID, "error", err)
				continue
			}

			consolidated[seedID] = true
			totalConsolidated++

			slog.Debug("replay: consolidated episode",
				"vault", vault, "seed", seedID, "members", len(concepts))
		}
	}

	if totalConsolidated > 0 {
		slog.Info("replay: consolidation pass complete",
			"total_consolidated", totalConsolidated)
	}
}

// buildConsolidationSummary generates a simple summary from episode member concepts.
// No LLM dependency — concatenates concepts with semicolons.
func buildConsolidationSummary(concepts []string) string {
	return "Episode summary: " + strings.Join(concepts, "; ")
}
