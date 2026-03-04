package engine

import (
	"context"
	"encoding/binary"
	"fmt"
	"log/slog"
	"math"
	"math/rand"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/scrypster/muninndb/internal/auth"
	"github.com/scrypster/muninndb/internal/brief"
	"github.com/scrypster/muninndb/internal/cognitive"
	"github.com/scrypster/muninndb/internal/engine/activation"
	"github.com/scrypster/muninndb/internal/engine/autoassoc"
	enginebrief "github.com/scrypster/muninndb/internal/engine/brief"
	"github.com/scrypster/muninndb/internal/engine/coherence"
	"github.com/scrypster/muninndb/internal/engine/novelty"
	"github.com/scrypster/muninndb/internal/engine/trigger"
	"github.com/scrypster/muninndb/internal/engine/vaultjob"
	"github.com/scrypster/muninndb/internal/index/fts"
	"github.com/scrypster/muninndb/internal/index/hnsw"
	"github.com/scrypster/muninndb/internal/metrics"
	"github.com/scrypster/muninndb/internal/metrics/latency"
	"github.com/scrypster/muninndb/internal/plugin"
	"github.com/scrypster/muninndb/internal/provenance"
	"github.com/scrypster/muninndb/internal/scoring"
	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// CognitiveForwarder is implemented by ClusterCoordinator on Lobe nodes.
// Using an interface avoids an import cycle between engine and replication.
type CognitiveForwarder interface {
	ForwardCognitiveEffects(effect mbp.CognitiveSideEffect)
}

// Engine is the cognitive database engine implementing mbp.EngineAPI.
type Engine struct {
	store            *storage.PebbleStore
	authStore        *auth.Store // nil = use Plasticity defaults (e.g. in tests)
	fts              *fts.Index
	ftsWorker        *fts.Worker  // async FTS indexing — decoupled from write hot path
	activation       *activation.ActivationEngine
	triggers         *trigger.TriggerSystem
	engramCount      atomic.Int64
	// ──────────────────────────────────────────────────────────────────
	// Cognitive Worker Subsystem
	// ──────────────────────────────────────────────────────────────────
	//
	// Four background workers drive the cognitive pipeline:
	//
	//   HebbianWorker          – Hebbian learning: strengthens association
	//                            weights between co-activated engrams.
	//   Worker[ContradictItem] – Contradiction detection: flags engrams
	//                            whose claims conflict with existing knowledge.
	//   Worker[ConfidenceUpdate] – Confidence decay: adjusts confidence
	//                              scores over time based on access patterns.
	//   TransitionWorker       – PAS state transitions: moves engrams
	//                            through lifecycle states (active → stable
	//                            → archived) based on scoring signals.
	//
	// Hot-Swap Design
	//
	// cogMu is an RWMutex that guards all four worker pointers. It exists
	// because worker assignment changes at runtime during cluster role
	// transitions. Three operations interact with it:
	//
	//   cogWorkers()             – acquires RLock, snapshots all four
	//                              pointers, releases lock, returns the
	//                              snapshot. Safe to use after unlock even
	//                              if a concurrent hot-swap occurs.
	//   SetCognitiveWorkers()    – called on Cortex promotion (Raft leader
	//                              election → OnBecameCortex). Acquires
	//                              write lock, sets all workers, releases.
	//   ClearCognitiveWorkers()  – called on Lobe demotion (OnBecameLobe).
	//                              Sets all four pointers to nil under
	//                              write lock. Lobe nodes perform no local
	//                              cognitive processing; effects are
	//                              forwarded to the Cortex.
	//
	// Callback Wiring Invariant
	//
	// HebbianWorker.OnWeightUpdate is always set BEFORE the worker pointer
	// is published. In NewEngine, the callback is wired at construction
	// time. In SetCognitiveWorkers, it is wired before the write lock is
	// released. This guarantees no concurrent goroutine ever reads a
	// HebbianWorker reference that lacks its callback.
	//
	// Lifecycle Ownership
	//
	// The Engine reads worker pointers but does not own their goroutine
	// lifecycle. In cluster mode, ClusterCoordinator creates and stops
	// workers; Engine only exposes them. In standalone mode, server.go
	// creates workers and passes them to NewEngine. transitionWorker is
	// set separately via SetTransitionWorker() because it follows a
	// different wiring sequence during cluster promotion.
	//
	// Shutdown: Engine.Stop() does NOT stop cognitive workers. Cluster
	// code (or server.go in standalone) is responsible for calling
	// hebbianWorker.Stop(), etc. before the process exits.
	// ──────────────────────────────────────────────────────────────────
	cogMu              sync.RWMutex
	hebbianWorker      *cognitive.HebbianWorker
	contradictWorker   *cognitive.Worker[cognitive.ContradictItem]
	confidenceWorker   *cognitive.Worker[cognitive.ConfidenceUpdate]
	transitionWorker   *cognitive.TransitionWorker
	activity         *cognitive.ActivityTracker
	embedder activation.Embedder // optional embedder for embedding-based brief scoring
	// Feature subsystems (all optional, nil-safe)
	autoAssoc      *autoassoc.Worker    // write-time automatic tag-based associations
	neighborWorker *autoassoc.NeighborWorker // semantic neighbor auto-linking
	goalLinkWorker *autoassoc.GoalLinkWorker  // goal-aware semantic auto-linking
	noveltyDet           *novelty.Detector // write-time near-duplicate detection
	noveltyJobs          chan noveltyJob    // async novelty work queue
	noveltyDone          chan struct{}      // signals novelty worker shutdown
	pruneDone            chan struct{}      // signals prune worker shutdown
	idempotencySweepDone chan struct{}      // signals idempotency sweep worker shutdown
	coherence   *coherence.Registry // per-vault incremental coherence counters
	scoring     *scoring.Store      // per-vault learnable scoring weights
	prov        *provenance.Store   // audit trail per-engram

	// Fix 5: coherence persistence lifecycle
	coherenceFlushStop chan struct{}
	coherenceFlushDone chan struct{}

	// coordinator forwards cognitive side effects to the Cortex on Lobe nodes.
	// nil on standalone / Cortex nodes (workers handle effects locally).
	coordinator   CognitiveForwarder
	coordinatorID string // this node's ID, used as OriginNodeID in CognitiveSideEffect

	// onWrite is an optional callback invoked after every successful Write.
	// Used to notify background processors (e.g. embed retroactive worker) of new data.
	// Stored as atomic.Value to allow safe concurrent reads without a mutex.
	onWrite atomic.Value // stores func()

	// latencyTracker records per-vault per-operation latency samples.
	// nil-safe — callers check before recording.
	latencyTracker *latency.Tracker

	// retroProcessors holds references to background processors (embed, enrich)
	// so Observability() can report their stats. Set via SetRetroactiveProcessors.
	retroProcessors []*plugin.RetroactiveProcessor

	// enrichPlugin is the optional EnrichPlugin used by ReplayEnrichment.
	// Set via SetEnrichPlugin after construction.
	enrichPlugin plugin.EnrichPlugin

	// noveltyJobsDropped counts novelty jobs silently dropped because the channel was full.
	noveltyJobsDropped atomic.Int64

	// queryCounter generates fast query IDs without crypto/rand syscall overhead.
	queryCounter atomic.Uint64
	// stopOnce ensures Stop() is idempotent even if called multiple times.
	stopOnce sync.Once

	// Vault lifecycle fields
	vaultOpsMu   sync.Mutex     // guards name reservation in StartClone/StartMerge
	jobManager   *vaultjob.Manager  // tracks async clone/merge jobs
	stopCtx      context.Context    // cancelled on Stop() to signal goroutines
	stopCancel   context.CancelFunc
	hnswRegistry *hnsw.Registry     // per-vault HNSW indexes (shared with activation)

	// vaultMu provides per-vault mutual exclusion for destructive vault operations
	// (PruneVault, ReindexFTSVault, ClearVault). Maps string vault name → *sync.Mutex.
	vaultMu sync.Map

	// NOTE: childMu grows by one entry per unique parent ULID ever passed to
	// AddChild. This is bounded by the number of distinct tree nodes in the
	// database — it cannot grow faster than the corpus itself — so no eviction
	// is needed.
	childMu sync.Map
}

// SetOnWrite registers a callback invoked after every successful Write.
// Intended for wiring background processors that need to react to new data.
// Safe to call concurrently with Write.
func (e *Engine) SetOnWrite(fn func()) {
	e.onWrite.Store(fn)
}

// SetLatencyTracker configures the per-vault latency tracker.
// Must be called before the engine starts serving requests (not safe for concurrent use with Write/Activate/Read).
func (e *Engine) SetLatencyTracker(t *latency.Tracker) {
	e.latencyTracker = t
}

// SetRetroactiveProcessors registers background processors for observability.
// Must be called before the engine starts serving requests (not safe for concurrent use with Observability).
func (e *Engine) SetRetroactiveProcessors(procs ...*plugin.RetroactiveProcessor) {
	e.retroProcessors = procs
}

// GetProcessorStats returns stats for all registered retroactive processors.
func (e *Engine) GetProcessorStats() []plugin.RetroactiveStats {
	stats := make([]plugin.RetroactiveStats, 0, len(e.retroProcessors))
	for _, p := range e.retroProcessors {
		if p != nil {
			stats = append(stats, p.Stats())
		}
	}
	return stats
}

// GetEnrichmentMode returns a human-readable string describing the active enrichment setup.
// Returns "none" when no processors are configured, "plugin:<name>" when an enrich plugin
// is active, or "inline" when only embed processors are registered.
func (e *Engine) GetEnrichmentMode() string {
	if len(e.retroProcessors) == 0 {
		return "none"
	}
	for _, p := range e.retroProcessors {
		if p == nil {
			continue
		}
		stats := p.Stats()
		if p.Mode() == "enrich" && stats.PluginName != "" && stats.Status != "stopped" {
			return "plugin:" + stats.PluginName
		}
	}
	return "inline"
}

// LatencyTracker returns the latency tracker (may be nil).
func (e *Engine) LatencyTracker() *latency.Tracker {
	return e.latencyTracker
}

// fastQueryID returns a unique query identifier without crypto/rand overhead.
func (e *Engine) fastQueryID() string {
	n := e.queryCounter.Add(1)
	return fmt.Sprintf("q-%016x", n)
}

// getVaultMutex returns the per-vault mutex for name, creating it if needed.
// Used to serialize destructive vault operations (PruneVault, ReindexFTSVault,
// ClearVault) so concurrent calls on the same vault cannot corrupt state.
func (e *Engine) getVaultMutex(name string) *sync.Mutex {
	mu, _ := e.vaultMu.LoadOrStore(name, &sync.Mutex{})
	return mu.(*sync.Mutex)
}

// getChildMutex returns the per-parent mutex for parentID, creating it if needed.
// Used to serialize the read-modify-write ordinal assignment in AddChild (append mode)
// so concurrent appends to the same parent cannot produce duplicate ordinals.
func (e *Engine) getChildMutex(parentID string) *sync.Mutex {
	v, _ := e.childMu.LoadOrStore(parentID, &sync.Mutex{})
	return v.(*sync.Mutex)
}

// noveltyJob is the unit of work for the async novelty worker.
type noveltyJob struct {
	wsPrefix  [8]byte
	id        storage.ULID
	vaultID   uint32
	concept   string
	content   string
	vaultName string // for coherence label
}

// NewEngine creates a new Engine.
func NewEngine(
	store *storage.PebbleStore,
	authStore *auth.Store,
	ftsIdx *fts.Index,
	act *activation.ActivationEngine,
	trig *trigger.TriggerSystem,
	hebbianWorker *cognitive.HebbianWorker,
	contradictWorker *cognitive.Worker[cognitive.ContradictItem],
	confidenceWorker *cognitive.Worker[cognitive.ConfidenceUpdate],
	embedder activation.Embedder,
	hnswRegistry *hnsw.Registry,
) *Engine {
	stopCtx, stopCancel := context.WithCancel(context.Background())
	e := &Engine{
		store:            store,
		authStore:        authStore,
		fts:              ftsIdx,
		activation:       act,
		triggers:         trig,
		hebbianWorker:    hebbianWorker,
		contradictWorker: contradictWorker,
		confidenceWorker: confidenceWorker,
		activity:         cognitive.NewActivityTracker(),
		embedder:         embedder,
		autoAssoc:        autoassoc.New(stopCtx, store, ftsIdx),
		neighborWorker:  autoassoc.NewNeighborWorker(stopCtx, store, hnswRegistry),
		goalLinkWorker:  autoassoc.NewGoalLinkWorker(stopCtx, store, hnswRegistry),
		noveltyDet:       novelty.New(),
		noveltyJobs:      make(chan noveltyJob, 256),
		noveltyDone:      make(chan struct{}),
		pruneDone:        make(chan struct{}),
		coherence:        coherence.NewRegistry(),
		scoring:          scoring.NewStore(store.GetDB()),
		prov:             provenance.NewStore(store.GetDB()),
		stopCtx:          stopCtx,
		stopCancel:       stopCancel,
		hnswRegistry:     hnswRegistry,
		jobManager:       vaultjob.NewManager(),
	}
	// Start async novelty worker to decouple O(N) Jaccard scan from write hot path.
	go e.runNoveltyWorker()
	// Start async FTS worker to decouple indexing from the write hot path.
	// The engram is already durable in Pebble before this worker runs —
	// it only controls keyword search visibility (eventual, ~100ms lag).
	if ftsIdx != nil {
		e.ftsWorker = fts.NewWorker(ftsIdx)
	}
	// Seed in-memory counter from persistent storage so Stat() is accurate after restart.
	if count, err := store.CountEngrams(context.Background()); err == nil {
		e.engramCount.Store(count)
	}
	// Backfill vault name meta keys for any vaults written before this feature existed.
	_ = store.BackfillVaultNames()

	// T1: Wire cognitive callbacks to trigger system.
	// Initialization order note: HebbianWorker's background goroutine is already running
	// at this point (it auto-starts inside NewHebbianWorkerWithDB). Setting OnWeightUpdate
	// here is safe because NewEngine has not returned yet — no caller can enqueue a
	// CoActivationEvent through the engine API before this assignment completes.
	// The dynamic Cortex-promotion path (SetCognitiveWorkers) sets the callback before
	// publishing the worker reference, giving the same guarantee for hot-swap scenarios.
	// For an even stronger guarantee, pass the callback directly to NewHebbianWorkerWithDB
	// so it is set before the goroutine starts.
	if e.hebbianWorker != nil && e.triggers != nil {
		e.hebbianWorker.OnWeightUpdate = func(ws [8]byte, id [16]byte, field string, old, new float64) {
			vaultID := wsVaultID(ws)
			e.triggers.NotifyCognitive(vaultID, storage.ULID(id), field, float32(old), float32(new))
		}
	}
	// Fix 5: Load persisted coherence counters for known vaults.
	if e.coherence != nil {
		vaultNames, _ := store.ListVaultNames()
		for _, name := range vaultNames {
			prefix := store.ResolveVaultPrefix(name)
			data, ok, err := store.ReadCoherence(prefix)
			if err != nil {
				slog.Warn("engine: failed to load coherence counters", "vault", name, "error", err)
				continue
			}
			if ok {
				e.coherence.RestoreVault(name, data)
			}
		}
		// Start periodic coherence flush goroutine.
		e.coherenceFlushStop = make(chan struct{})
		e.coherenceFlushDone = make(chan struct{})
		go e.runCoherenceFlush()
	}

	// Start periodic vault pruning sweep (runs every 60s with ±5s jitter).
	// Only active for vaults with MaxEngrams > 0 or RetentionDays > 0.
	go e.runPruneWorker()

	// Start daily idempotency receipt sweep — purges Pebble 0x19 entries
	// older than 30 days to prevent unbounded disk growth.
	e.idempotencySweepDone = make(chan struct{})
	go e.runIdempotencySweep()

	return e
}

// runCoherenceFlush periodically persists coherence counters to Pebble.
// Runs until coherenceFlushStop is closed, then does a final flush.
func (e *Engine) runCoherenceFlush() {
	defer close(e.coherenceFlushDone)
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			e.flushCoherence()
		case <-e.coherenceFlushStop:
			e.flushCoherence() // final flush on shutdown
			return
		}
	}
}

// flushCoherence serializes all vault coherence counters to Pebble.
func (e *Engine) flushCoherence() {
	if e.coherence == nil {
		return
	}
	for name, data := range e.coherence.SerializeAll() {
		prefix := e.store.ResolveVaultPrefix(name)
		if err := e.store.WriteCoherence(prefix, data); err != nil {
			slog.Warn("engine: failed to flush coherence", "vault", name, "error", err)
		}
	}
}

// Stop gracefully shuts down all background workers.
// Idempotent: safe to call multiple times. Must be called before the process
// exits (or the Pebble DB is closed) to ensure in-flight jobs complete.
func (e *Engine) Stop() {
	e.stopOnce.Do(func() {
		// Cancel the engine lifecycle context to signal running goroutines (clone/merge).
		if e.stopCancel != nil {
			e.stopCancel()
		}
		if e.autoAssoc != nil {
			e.autoAssoc.Stop()
			if e.neighborWorker != nil {
				e.neighborWorker.Stop()
			}
		}
		if e.goalLinkWorker != nil {
			e.goalLinkWorker.Stop()
		}
		// Wait for the prune worker to exit after stopCancel() signalled it.
		if e.pruneDone != nil {
			select {
			case <-e.pruneDone:
			case <-time.After(5 * time.Second):
				slog.Warn("engine: prune worker did not exit within 5s")
			}
		}
		// Stop async novelty worker.
		// Do NOT close noveltyJobs — Write() guards with stopCtx.Done() but
		// closing a channel concurrently with a pending select-send is still
		// a data race detected by -race. Signal shutdown via the cancelled
		// context instead and wait for the worker to drain and exit.
		if e.noveltyDone != nil {
			select {
			case <-e.noveltyDone:
			case <-time.After(5 * time.Second):
				slog.Warn("engine: novelty worker did not exit within 5s")
			}
		}
		// Drain the FTS worker — flushes any queued indexing jobs before exit.
		if e.ftsWorker != nil {
			e.ftsWorker.Stop()
		}
		// Close the activation engine's drainLog goroutine. Must happen after FTS
		// worker stops (writes may reference activation) but before other cleanup.
		if e.activation != nil {
			e.activation.Close()
		}
		// Fix 5: Stop coherence flush goroutine and wait for final flush.
		if e.coherenceFlushStop != nil {
			close(e.coherenceFlushStop)
			select {
			case <-e.coherenceFlushDone:
			case <-time.After(5 * time.Second):
				slog.Warn("engine: coherence flush worker did not exit within 5s")
			}
		}
		// Stop the vault job GC goroutine.
		if e.jobManager != nil {
			e.jobManager.Close()
		}
		// Wait for idempotency sweep worker to exit.
		if e.idempotencySweepDone != nil {
			select {
			case <-e.idempotencySweepDone:
			case <-time.After(5 * time.Second):
				slog.Warn("engine: idempotency sweep worker did not exit within 5s")
			}
		}
	})
}

// SetCoordinator wires the Lobe's ClusterCoordinator so Activate() can forward
// cognitive side effects to the Cortex. nodeID is stamped as OriginNodeID in
// every CognitiveSideEffect. Call this after both Engine and ClusterCoordinator
// are constructed.
func (e *Engine) SetCoordinator(coord CognitiveForwarder, nodeID string) {
	e.coordinator = coord
	e.coordinatorID = nodeID
}

// generateEmbeddingBrief generates a brief using embedding-based sentence scoring.
// Returns a list of BriefSentences sorted by cosine similarity to the context embedding.
// Falls back to empty list if embedding fails or no sentences meet the threshold.
func (e *Engine) generateEmbeddingBrief(ctx context.Context, items []mbp.ActivationItem, contextEmbedding []float32) []mbp.BriefSentence {
	if e.embedder == nil || len(contextEmbedding) == 0 || len(items) == 0 {
		return nil
	}

	// Create the brief scorer with the embedder adapter
	adapter := newEmbedderAdapter(e.embedder)
	scorer := &brief.Scorer{
		Model:        adapter,
		Threshold:    0.72, // reasonable default for cosine similarity
		MaxSentences: 3,    // return up to 3 sentences
		MaxSentLen:   512,  // truncate long sentences
	}

	// Combine all content from activation items
	var allSentences []mbp.BriefSentence
	for _, item := range items {
		scored, err := scorer.Score(ctx, item.Content, contextEmbedding)
		if err != nil {
			continue // skip this item on error
		}

		for _, s := range scored {
			allSentences = append(allSentences, mbp.BriefSentence{
				EngramID: item.ID,
				Text:     s.Text,
				Score:    float64(s.Score),
			})
		}
	}

	// Sort by score descending and cap at MaxSentences
	sort.Slice(allSentences, func(i, j int) bool {
		return allSentences[i].Score > allSentences[j].Score
	})

	maxN := 5 // return up to 5 top sentences across all engrams
	if maxN > len(allSentences) {
		maxN = len(allSentences)
	}

	if maxN > 0 {
		return allSentences[:maxN]
	}
	return nil
}

// Store returns the underlying PebbleStore, used by the MCP adapter for direct storage access.
func (e *Engine) Store() *storage.PebbleStore {
	return e.store
}

// CountEmbedded returns the count of engrams that have had embeddings generated
// (i.e. the DigestEmbed flag is set). Returns -1 on error.
func (e *Engine) CountEmbedded(ctx context.Context) int64 {
	const DigestEmbed uint8 = 0x02
	count, err := e.store.CountWithFlag(ctx, DigestEmbed)
	if err != nil {
		return -1
	}
	return count
}

// ActivityTracker returns the vault-level activity tracker.
func (e *Engine) ActivityTracker() *cognitive.ActivityTracker {
	return e.activity
}

// Hello implements mbp.EngineAPI.Hello.
func (e *Engine) Hello(ctx context.Context, req *mbp.HelloRequest) (*mbp.HelloResponse, error) {
	if req.Version != "1" {
		return nil, fmt.Errorf("unsupported version: %s", req.Version)
	}

	// Register the vault name so it appears in ListVaults even before the
	// first engram is written (idempotent, cheap).
	vaultName := req.Vault
	if vaultName == "" {
		vaultName = "default"
	}
	wsPrefix := e.store.ResolveVaultPrefix(vaultName)
	if err := e.store.WriteVaultName(wsPrefix, vaultName); err != nil {
		slog.Warn("engine: Hello: failed to persist vault name", "vault", vaultName, "err", err)
	}

	return &mbp.HelloResponse{
		ServerVersion: "1.0.0",
		SessionID:     uuid.New().String(),
		VaultID:       req.Vault,
		Capabilities:  []string{"compression"},
		Limits: mbp.Limits{
			MaxResults:   100,
			MaxHopDepth:  5,
			MaxRate:      1000,
			MaxPayloadMB: 64,
		},
	}, nil
}

// Write implements mbp.EngineAPI.Write.
func (e *Engine) Write(ctx context.Context, req *mbp.WriteRequest) (*mbp.WriteResponse, error) {
	writeStart := time.Now()
	wsPrefix := e.store.ResolveVaultPrefix(req.Vault)
	e.activity.Record(wsPrefix)

	// Resolve inline enrichment mode for this vault.
	vaultName := req.Vault
	if vaultName == "" {
		vaultName = "default"
	}
	resolved := e.resolveVaultPlasticity(vaultName)
	inlineMode := resolved.InlineEnrichment

	// Determine which caller-provided enrichment fields to use based on mode.
	callerSummary := ""
	var callerEntities []mbp.InlineEntity
	var callerRelationships []mbp.InlineRelationship

	switch inlineMode {
	case "caller_only", "caller_preferred":
		callerSummary = req.Summary
		callerEntities = req.Entities
		callerRelationships = req.Relationships
	case "disabled", "background_only":
		// Ignore caller enrichment data.
	}

	// Build storage.Engram from request
	eng := &storage.Engram{
		Concept:    req.Concept,
		Content:    req.Content,
		Tags:       req.Tags,
		Confidence: req.Confidence,
		Stability:  req.Stability,
		Embedding:  req.Embedding,
		MemoryType: storage.MemoryType(req.MemoryType),
		TypeLabel:  req.TypeLabel,
	}

	// Apply caller-provided summary directly to the engram.
	if callerSummary != "" {
		eng.Summary = callerSummary
	}

	// Set custom CreatedAt if provided
	if req.CreatedAt != nil {
		eng.CreatedAt = *req.CreatedAt
	}

	// Convert associations
	assocs := make([]storage.Association, len(req.Associations))
	for i, a := range req.Associations {
		targetID, err := storage.ParseULID(a.TargetID)
		if err != nil {
			return nil, fmt.Errorf("parse target id: %w", err)
		}
		assocs[i] = storage.Association{
			TargetID:      targetID,
			RelType:       storage.RelType(a.RelType),
			Weight:        a.Weight,
			Confidence:    a.Confidence,
			CreatedAt:     time.Unix(0, a.CreatedAt),
			LastActivated: a.LastActivated,
		}
	}
	eng.Associations = assocs

	// Write to store
	id, err := e.store.WriteEngram(ctx, wsPrefix, eng)
	if err != nil {
		return nil, fmt.Errorf("write engram: %w", err)
	}

	// Store caller-provided inline entities in the entity table (not as KeyPoints).
	if len(callerEntities) > 0 {
		ws, _ := e.store.FindVaultPrefix(id)
		var linkedEntityNames []string
		for _, ent := range callerEntities {
			record := storage.EntityRecord{
				Name:       ent.Name,
				Type:       ent.Type,
				Confidence: 1.0,
			}
			if err := e.store.UpsertEntityRecord(ctx, record, "inline"); err != nil {
				slog.Warn("engine: failed to store inline entity", "name", ent.Name, "err", err)
				continue
			}
			if err := e.store.WriteEntityEngramLink(ctx, ws, id, ent.Name); err != nil {
				slog.Warn("engine: failed to link inline entity", "name", ent.Name, "err", err)
				continue
			}
			linkedEntityNames = append(linkedEntityNames, ent.Name)
		}
		// Write co-occurrence pairs for entities co-appearing in this engram.
		for i := 0; i < len(linkedEntityNames); i++ {
			for j := i + 1; j < len(linkedEntityNames); j++ {
				if err := e.store.IncrementEntityCoOccurrence(ctx, ws, linkedEntityNames[i], linkedEntityNames[j]); err != nil {
					slog.Warn("engine: failed to increment co-occurrence", "vault", req.Vault, "engram", id.String(), "entity_a", linkedEntityNames[i], "entity_b", linkedEntityNames[j], "err", err)
				}
				if err := e.store.UpsertRelationshipRecord(ctx, ws, id, storage.RelationshipRecord{
					FromEntity: linkedEntityNames[i],
					ToEntity:   linkedEntityNames[j],
					RelType:    "co_occurs_with",
					Weight:     0.3,
					Source:     "co-occurrence",
				}); err != nil {
					slog.Warn("engine: failed to upsert co_occurs_with relationship", "vault", req.Vault, "engram", id.String(), "entity_a", linkedEntityNames[i], "entity_b", linkedEntityNames[j], "err", err)
				}
			}
		}
		// Mark entities as caller-provided so the retroactive processor skips extraction.
		existing, _ := e.store.GetDigestFlags(ctx, plugin.ULID(id))
		_ = e.store.SetDigestFlag(ctx, id, existing|plugin.DigestEntities)
	}

	// Create associations from caller-provided relationships (after engram is stored).
	for _, rel := range callerRelationships {
		targetULID, parseErr := storage.ParseULID(rel.TargetID)
		if parseErr != nil {
			slog.Warn("engine: inline relationship has invalid target_id", "target_id", rel.TargetID, "error", parseErr)
			continue
		}
		relAssoc := &storage.Association{
			TargetID:   targetULID,
			RelType:    storage.RelType(relTypeFromString(rel.Relation)),
			Weight:     rel.Weight,
			Confidence: 1.0,
			CreatedAt:  time.Now(),
		}
		if writeErr := e.store.WriteAssociation(ctx, wsPrefix, id, targetULID, relAssoc); writeErr != nil {
			slog.Warn("engine: failed to write inline relationship", "target_id", rel.TargetID, "error", writeErr)
		}
	}

	// Store caller-provided entity-to-entity relationships in the 0x21 relationship index.
	if len(req.EntityRelationships) > 0 {
		wsER, _ := e.store.FindVaultPrefix(id)
		for _, er := range req.EntityRelationships {
			if er.FromEntity == "" || er.ToEntity == "" || er.RelType == "" {
				continue
			}
			weight := er.Weight
			if weight <= 0 {
				weight = 0.9
			}
			if err := e.store.UpsertRelationshipRecord(ctx, wsER, id, storage.RelationshipRecord{
				FromEntity: er.FromEntity,
				ToEntity:   er.ToEntity,
				RelType:    er.RelType,
				Weight:     weight,
				Source:     "inline",
			}); err != nil {
				slog.Warn("engine: failed to store entity relationship", "vault", req.Vault, "engram", id.String(), "from", er.FromEntity, "to", er.ToEntity, "rel_type", er.RelType, "err", err)
			}
		}
	}

	// Determine if we should skip background enrichment.
	// caller_only: skip if any caller data was provided
	// caller_preferred: the retroactive processor checks per-field (handled there)
	// disabled: skip entirely (no enrichment at all)
	callerProvidedAny := callerSummary != "" || len(callerEntities) > 0
	skipBackgroundEnrich := (inlineMode == "caller_only" && callerProvidedAny) || inlineMode == "disabled"

	// If we should skip background enrichment, set the DigestEnrich flag now
	// so the retroactive processor skips this engram.
	if skipBackgroundEnrich {
		if flagErr := e.store.SetDigestFlag(ctx, id, plugin.DigestEnrich); flagErr != nil {
			slog.Warn("engine: failed to set enrich digest flag for inline enrichment", "id", id.String(), "error", flagErr)
		}
	}

	// Persist vault name for discovery (idempotent, cheap)
	if err := e.store.WriteVaultName(wsPrefix, vaultName); err != nil {
		slog.Warn("engine: failed to persist vault name", "vault", vaultName, "err", err)
	}

	// Submit to async FTS worker — decoupled from write hot path.
	// Engram is already durable; FTS visibility follows within ~100ms.
	if e.ftsWorker != nil {
		e.ftsWorker.Submit(fts.IndexJob{
			WS:        wsPrefix,
			ID:        [16]byte(id),
			Concept:   eng.Concept,
			CreatedBy: eng.CreatedBy,
			Content:   eng.Content,
			Tags:      eng.Tags,
		})
	}

	// Submit to contradiction worker for post-write analysis.
	_, contraW, _ := e.cogWorkers()
	if contraW != nil {
		contraAssocs := make([]cognitive.ContradictAssoc, len(eng.Associations))
		for i, assoc := range eng.Associations {
			contraAssocs[i] = cognitive.ContradictAssoc{
				EngramID:   [16]byte(id),
				TargetID:   assoc.TargetID,
				TargetHash: hashString(assoc.TargetID.String()),
				RelType:    uint16(assoc.RelType),
			}
		}
		contraW.Submit(cognitive.ContradictItem{
			WS:           wsPrefix,
			EngramID:     [16]byte(id),
			ConceptHash:  hashString(eng.Concept),
			Associations: contraAssocs,
			OnFound: func(ev cognitive.ContradictionEvent) {
				if e.triggers != nil {
					e.triggers.NotifyContradiction(wsVaultID(wsPrefix), storage.ULID(ev.EngramA), storage.ULID(ev.EngramB), ev.Severity, "semantic")
				}
				_, _, cw := e.cogWorkers()
				if cw != nil {
					cw.Submit(cognitive.ConfidenceUpdate{
						WS:       wsPrefix,
						EngramID: ev.EngramA,
						Evidence: cognitive.EvidenceContradiction,
						Source:   "contradiction_detected",
					})
					cw.Submit(cognitive.ConfidenceUpdate{
						WS:       wsPrefix,
						EngramID: ev.EngramB,
						Evidence: cognitive.EvidenceContradiction,
						Source:   "contradiction_detected",
					})
				}
			},
		})
	}

	e.engramCount.Add(1)

	// Update coherence counters for the new engram (starts as an orphan).
	if e.coherence != nil {
		e.coherence.GetOrCreate(vaultName).RecordWrite(eng.Confidence)
	}

	// Write-time novelty detection: enqueue async — O(N) Jaccard scan runs off the hot path.
	if e.noveltyDet != nil {
		job := noveltyJob{
			wsPrefix:  wsPrefix,
			id:        id,
			vaultID:   wsVaultID(wsPrefix),
			concept:   eng.Concept,
			content:   eng.Content,
			vaultName: vaultName,
		}
		select {
		case <-e.stopCtx.Done():
			// Engine shutting down — skip novelty detection to avoid send on closed channel.
		case e.noveltyJobs <- job:
		default:
			// Queue full — drop novelty check rather than block write path.
			e.noveltyJobsDropped.Add(1)
			metrics.NoveltyDropsTotal.Inc()
		}
	}

	// Write-time auto-association: find engrams with overlapping tags.
	if e.autoAssoc != nil && len(eng.Tags) > 0 {
		e.autoAssoc.Enqueue(autoassoc.Job{
			WSPrefix: wsPrefix,
			NewID:    id,
			Tags:     eng.Tags,
		})
	}


	// Write-time semantic neighbor linking: find semantically similar engrams via HNSW.
	if e.neighborWorker != nil && len(eng.Embedding) > 0 {
		e.neighborWorker.EnqueueNeighborJob(autoassoc.NeighborJob{
			WS:        wsPrefix,
			ID:        [16]byte(id),
			Embedding: eng.Embedding,
		})
	}

	// Fire goal auto-linking if this is a goal-type engram with an embedding.
	if e.goalLinkWorker != nil && eng.MemoryType == storage.TypeGoal && len(eng.Embedding) > 0 {
		goalEmb := append([]float32(nil), eng.Embedding...)
		e.goalLinkWorker.EnqueueGoalJob(autoassoc.GoalJob{
			WS:        wsPrefix,
			ID:        [16]byte(id),
			Embedding: goalEmb,
		})
	}

	// Notify trigger system after the write is committed and counted.
	// We copy the engram so the trigger worker goroutine has safe read-only access
	// to the struct after Write() returns and the caller's stack frame is potentially
	// reused. A shallow copy is safe because the trigger worker never mutates fields.
	if e.triggers != nil {
		vaultID := wsVaultID(wsPrefix)
		engCopy := *eng // struct copy; deep-copy slices so trigger worker has independent data
		engCopy.Tags = append([]string(nil), eng.Tags...)
		engCopy.Associations = append([]storage.Association(nil), eng.Associations...)
		if eng.Embedding != nil {
			engCopy.Embedding = append([]float32(nil), eng.Embedding...)
		}
		e.triggers.NotifyWrite(vaultID, &engCopy, true)
	}

	// Notify background processors (e.g. embed worker) of the new engram.
	if fn, ok := e.onWrite.Load().(func()); ok && fn != nil {
		fn()
	}

	metrics.EngineWritesTotal.Inc()

	d := time.Since(writeStart)
	if e.latencyTracker != nil {
		e.latencyTracker.Record(wsPrefix, "write", d)
	}
	metrics.WriteDuration.WithLabelValues(vaultName).Observe(d.Seconds())

	return &mbp.WriteResponse{
		ID:        id.String(),
		CreatedAt: time.Now().UnixNano(),
	}, nil
}

// MaxBatchSize is the maximum number of items allowed in a single WriteBatch call.
const MaxBatchSize = 50

// ErrBatchTooLarge is returned when a WriteBatch call exceeds MaxBatchSize items.
var ErrBatchTooLarge = fmt.Errorf("batch size exceeds maximum of %d", MaxBatchSize)

// preparedBatchItem holds all data needed for one item in a WriteBatch —
// the prepared engram plus post-commit metadata. This avoids re-deriving
// vault prefixes and enrichment fields after the batch commits.
type preparedBatchItem struct {
	wsPrefix                [8]byte
	vaultName               string
	eng                     *storage.Engram
	inlineMode              string
	callerSummary           string
	callerEntities          []mbp.InlineEntity
	callerRelationships     []mbp.InlineRelationship
	callerEntityRelationships []mbp.InlineEntityRelationship
	skipBackgroundEnrich    bool
}

// WriteBatch writes multiple engrams in a single Pebble batch commit, then
// fans out all post-commit async work per item. This amortises the fsync cost
// across N engrams instead of paying it per-engram.
func (e *Engine) WriteBatch(ctx context.Context, reqs []*mbp.WriteRequest) ([]*mbp.WriteResponse, []error) {
	n := len(reqs)
	if n > MaxBatchSize {
		errs := make([]error, n)
		for i := range errs {
			errs[i] = ErrBatchTooLarge
		}
		return make([]*mbp.WriteResponse, n), errs
	}

	responses := make([]*mbp.WriteResponse, n)
	errs := make([]error, n)

	// Phase 1: Prepare all engrams (pure computation, no I/O).
	items := make([]storage.EngramBatchItem, n)
	prepared := make([]preparedBatchItem, n)
	validCount := 0

	for i, req := range reqs {
		wsPrefix := e.store.ResolveVaultPrefix(req.Vault)
		e.activity.Record(wsPrefix)

		vaultName := req.Vault
		if vaultName == "" {
			vaultName = "default"
		}
		resolved := e.resolveVaultPlasticity(vaultName)
		inlineMode := resolved.InlineEnrichment

		var callerSummary string
		var callerEntities []mbp.InlineEntity
		var callerRelationships []mbp.InlineRelationship

		switch inlineMode {
		case "caller_only", "caller_preferred":
			callerSummary = req.Summary
			callerEntities = req.Entities
			callerRelationships = req.Relationships
		}

		eng := &storage.Engram{
			Concept:    req.Concept,
			Content:    req.Content,
			Tags:       req.Tags,
			Confidence: req.Confidence,
			Stability:  req.Stability,
			Embedding:  req.Embedding,
			MemoryType: storage.MemoryType(req.MemoryType),
			TypeLabel:  req.TypeLabel,
		}

		if callerSummary != "" {
			eng.Summary = callerSummary
		}
		if req.CreatedAt != nil {
			eng.CreatedAt = *req.CreatedAt
		}

		assocs := make([]storage.Association, len(req.Associations))
		for j, a := range req.Associations {
			targetID, parseErr := storage.ParseULID(a.TargetID)
			if parseErr != nil {
				errs[i] = fmt.Errorf("parse target id: %w", parseErr)
				break
			}
			assocs[j] = storage.Association{
				TargetID:      targetID,
				RelType:       storage.RelType(a.RelType),
				Weight:        a.Weight,
				Confidence:    a.Confidence,
				CreatedAt:     time.Unix(0, a.CreatedAt),
				LastActivated: a.LastActivated,
			}
		}
		if errs[i] != nil {
			continue
		}
		eng.Associations = assocs

		callerProvidedAny := callerSummary != "" || len(callerEntities) > 0
		skipBG := (inlineMode == "caller_only" && callerProvidedAny) || inlineMode == "disabled"

		items[i] = storage.EngramBatchItem{WSPrefix: wsPrefix, Engram: eng}
		prepared[i] = preparedBatchItem{
			wsPrefix:                  wsPrefix,
			vaultName:                 vaultName,
			eng:                       eng,
			inlineMode:                inlineMode,
			callerSummary:             callerSummary,
			callerEntities:            callerEntities,
			callerRelationships:       callerRelationships,
			callerEntityRelationships: req.EntityRelationships,
			skipBackgroundEnrich:      skipBG,
		}
		validCount++
	}

	// Phase 2: Single Pebble batch commit for all valid engrams.
	ids, batchErrs := e.store.WriteEngramBatch(ctx, items)
	for i := range reqs {
		if errs[i] != nil {
			continue // already failed in prepare phase
		}
		if batchErrs[i] != nil {
			errs[i] = fmt.Errorf("write engram: %w", batchErrs[i])
			continue
		}
		responses[i] = &mbp.WriteResponse{
			ID:        ids[i].String(),
			CreatedAt: time.Now().UnixNano(),
		}
	}

	// Phase 3: Post-commit async work for each successfully written engram.
	for i := range reqs {
		if errs[i] != nil {
			continue
		}
		p := &prepared[i]
		id := ids[i]

		// Store caller-provided inline entities in the entity table (not as KeyPoints).
		if len(p.callerEntities) > 0 {
			ws, _ := e.store.FindVaultPrefix(id)
			var linkedEntityNames []string
			for _, ent := range p.callerEntities {
				record := storage.EntityRecord{
					Name:       ent.Name,
					Type:       ent.Type,
					Confidence: 1.0,
				}
				if err := e.store.UpsertEntityRecord(ctx, record, "inline"); err != nil {
					slog.Warn("engine: batch: failed to store inline entity", "name", ent.Name, "err", err)
					continue
				}
				if err := e.store.WriteEntityEngramLink(ctx, ws, id, ent.Name); err != nil {
					slog.Warn("engine: batch: failed to link inline entity", "name", ent.Name, "err", err)
					continue
				}
				linkedEntityNames = append(linkedEntityNames, ent.Name)
			}
			// Write co-occurrence pairs for entities co-appearing in this engram.
			for i := 0; i < len(linkedEntityNames); i++ {
				for j := i + 1; j < len(linkedEntityNames); j++ {
					if err := e.store.IncrementEntityCoOccurrence(ctx, ws, linkedEntityNames[i], linkedEntityNames[j]); err != nil {
						slog.Warn("engine: batch: failed to increment co-occurrence", "vault", p.vaultName, "engram", id.String(), "entity_a", linkedEntityNames[i], "entity_b", linkedEntityNames[j], "err", err)
					}
					if err := e.store.UpsertRelationshipRecord(ctx, ws, id, storage.RelationshipRecord{
						FromEntity: linkedEntityNames[i],
						ToEntity:   linkedEntityNames[j],
						RelType:    "co_occurs_with",
						Weight:     0.3,
						Source:     "co-occurrence",
					}); err != nil {
						slog.Warn("engine: batch: failed to upsert co_occurs_with relationship", "vault", p.vaultName, "engram", id.String(), "entity_a", linkedEntityNames[i], "entity_b", linkedEntityNames[j], "err", err)
					}
				}
			}
			// Mark entities as caller-provided so the retroactive processor skips extraction.
			existing, _ := e.store.GetDigestFlags(ctx, plugin.ULID(id))
			_ = e.store.SetDigestFlag(ctx, id, existing|plugin.DigestEntities)
		}

		for _, rel := range p.callerRelationships {
			targetULID, parseErr := storage.ParseULID(rel.TargetID)
			if parseErr != nil {
				slog.Warn("engine: batch: skipping inline relationship with invalid target_id", "target_id", rel.TargetID, "err", parseErr)
				continue
			}
			relAssoc := &storage.Association{
				TargetID:   targetULID,
				RelType:    storage.RelType(relTypeFromString(rel.Relation)),
				Weight:     rel.Weight,
				Confidence: 1.0,
				CreatedAt:  time.Now(),
			}
			if err := e.store.WriteAssociation(ctx, p.wsPrefix, id, targetULID, relAssoc); err != nil {
				slog.Warn("engine: batch: failed to write inline relationship", "target_id", rel.TargetID, "err", err)
			}
		}

		// Store caller-provided entity-to-entity relationships in the 0x21 relationship index.
		if len(p.callerEntityRelationships) > 0 {
			wsER, ok := e.store.FindVaultPrefix(id)
			if !ok {
				slog.Warn("engine: batch: failed to find vault prefix for entity relationships", "vault", p.vaultName, "engram", id.String())
			} else {
				for _, er := range p.callerEntityRelationships {
					if er.FromEntity == "" || er.ToEntity == "" || er.RelType == "" {
						continue
					}
					weight := er.Weight
					if weight <= 0 {
						weight = 0.9
					}
					if err := e.store.UpsertRelationshipRecord(ctx, wsER, id, storage.RelationshipRecord{
						FromEntity: er.FromEntity,
						ToEntity:   er.ToEntity,
						RelType:    er.RelType,
						Weight:     weight,
						Source:     "inline",
					}); err != nil {
						slog.Warn("engine: batch: failed to store entity relationship", "vault", p.vaultName, "engram", id.String(), "from", er.FromEntity, "to", er.ToEntity, "rel_type", er.RelType, "err", err)
					}
				}
			}
		}

		if p.skipBackgroundEnrich {
			_ = e.store.SetDigestFlag(ctx, id, plugin.DigestEnrich)
		}

		if err := e.store.WriteVaultName(p.wsPrefix, p.vaultName); err != nil {
			slog.Warn("engine: failed to persist vault name", "vault", p.vaultName, "err", err)
		}

		if e.ftsWorker != nil {
			if !e.ftsWorker.Submit(fts.IndexJob{
				WS:        p.wsPrefix,
				ID:        [16]byte(id),
				Concept:   p.eng.Concept,
				CreatedBy: p.eng.CreatedBy,
				Content:   p.eng.Content,
				Tags:      p.eng.Tags,
			}) {
				slog.Warn("engine: FTS index job dropped in batch write", "id", id.String())
			}
		}

		_, contraW, _ := e.cogWorkers()
		if contraW != nil {
			contraAssocs := make([]cognitive.ContradictAssoc, len(p.eng.Associations))
			for j, assoc := range p.eng.Associations {
				contraAssocs[j] = cognitive.ContradictAssoc{
					EngramID:   [16]byte(id),
					TargetID:   assoc.TargetID,
					TargetHash: hashString(assoc.TargetID.String()),
					RelType:    uint16(assoc.RelType),
				}
			}
			wsPrefix := p.wsPrefix
			contraW.Submit(cognitive.ContradictItem{
				WS:           wsPrefix,
				EngramID:     [16]byte(id),
				ConceptHash:  hashString(p.eng.Concept),
				Associations: contraAssocs,
				OnFound: func(ev cognitive.ContradictionEvent) {
					if e.triggers != nil {
						e.triggers.NotifyContradiction(wsVaultID(wsPrefix), storage.ULID(ev.EngramA), storage.ULID(ev.EngramB), ev.Severity, "semantic")
					}
					_, _, cw := e.cogWorkers()
					if cw != nil {
						cw.Submit(cognitive.ConfidenceUpdate{WS: wsPrefix, EngramID: ev.EngramA, Evidence: cognitive.EvidenceContradiction, Source: "contradiction_detected"})
						cw.Submit(cognitive.ConfidenceUpdate{WS: wsPrefix, EngramID: ev.EngramB, Evidence: cognitive.EvidenceContradiction, Source: "contradiction_detected"})
					}
				},
			})
		}

		e.engramCount.Add(1)

		if e.coherence != nil {
			e.coherence.GetOrCreate(p.vaultName).RecordWrite(p.eng.Confidence)
		}

		if e.noveltyDet != nil {
			job := noveltyJob{
				wsPrefix:  p.wsPrefix,
				id:        id,
				vaultID:   wsVaultID(p.wsPrefix),
				concept:   p.eng.Concept,
				content:   p.eng.Content,
				vaultName: p.vaultName,
			}
			select {
			case <-e.stopCtx.Done():
			case e.noveltyJobs <- job:
			default:
				e.noveltyJobsDropped.Add(1)
				metrics.NoveltyDropsTotal.Inc()
			}
		}

		if e.autoAssoc != nil && len(p.eng.Tags) > 0 {
			e.autoAssoc.Enqueue(autoassoc.Job{
				WSPrefix: p.wsPrefix,
				NewID:    id,
				Tags:     p.eng.Tags,
			})
		}

		if e.neighborWorker != nil && len(p.eng.Embedding) > 0 {
			e.neighborWorker.EnqueueNeighborJob(autoassoc.NeighborJob{
				WS:        p.wsPrefix,
				ID:        [16]byte(id),
				Embedding: p.eng.Embedding,
			})
		}

		// Fire goal auto-linking if this is a goal-type engram with an embedding.
		if e.goalLinkWorker != nil && p.eng.MemoryType == storage.TypeGoal && len(p.eng.Embedding) > 0 {
			goalEmb := append([]float32(nil), p.eng.Embedding...)
			e.goalLinkWorker.EnqueueGoalJob(autoassoc.GoalJob{
				WS:        p.wsPrefix,
				ID:        [16]byte(id),
				Embedding: goalEmb,
			})
		}

		if e.triggers != nil {
			vaultID := wsVaultID(p.wsPrefix)
			engCopy := *p.eng
			engCopy.Tags = append([]string(nil), p.eng.Tags...)
			engCopy.Associations = append([]storage.Association(nil), p.eng.Associations...)
			if p.eng.Embedding != nil {
				engCopy.Embedding = append([]float32(nil), p.eng.Embedding...)
			}
			e.triggers.NotifyWrite(vaultID, &engCopy, true)
		}

		if fn, ok := e.onWrite.Load().(func()); ok && fn != nil {
			fn()
		}

		metrics.EngineWritesTotal.Inc()
	}

	return responses, errs
}

// Read implements mbp.EngineAPI.Read.
func (e *Engine) Read(ctx context.Context, req *mbp.ReadRequest) (*mbp.ReadResponse, error) {
	readStart := time.Now()
	wsPrefix := e.store.ResolveVaultPrefix(req.Vault)

	id, err := storage.ParseULID(req.ID)
	if err != nil {
		return nil, fmt.Errorf("parse id: %w", err)
	}

	eng, err := e.store.GetEngram(ctx, wsPrefix, id)
	if err != nil {
		return nil, fmt.Errorf("get engram: %w", err)
	}

	// Fire implicit positive feedback signal asynchronously — read = accessed.
	go func() {
		signal := scoring.FeedbackSignal{
			EngramID:    [16]byte(id),
			Accessed:    true,
			ScoreVector: scoring.DefaultWeights(),
			Timestamp:   time.Now(),
		}
		e.scoring.RecordFeedback(e.stopCtx, wsPrefix, signal)
	}()

	d := time.Since(readStart)
	if e.latencyTracker != nil {
		e.latencyTracker.Record(wsPrefix, "read", d)
	}
	metrics.ReadDuration.WithLabelValues(req.Vault).Observe(d.Seconds())

	return &mbp.ReadResponse{
		ID:             eng.ID.String(),
		Concept:        eng.Concept,
		Content:        eng.Content,
		Confidence:     eng.Confidence,
		Relevance:      eng.Relevance,
		Stability:      eng.Stability,
		AccessCount:    eng.AccessCount,
		Tags:           eng.Tags,
		State:          uint8(eng.State),
		CreatedAt:      eng.CreatedAt.UnixNano(),
		UpdatedAt:      eng.UpdatedAt.UnixNano(),
		LastAccess:     eng.LastAccess.UnixNano(),
		Summary:        eng.Summary,
		KeyPoints:      eng.KeyPoints,
		MemoryType:     uint8(eng.MemoryType),
		TypeLabel:      eng.TypeLabel,
		Classification: eng.Classification,
		EmbedDim:       uint8(eng.EmbedDim),
	}, nil
}

// Activate implements mbp.EngineAPI.Activate.
func (e *Engine) Activate(ctx context.Context, req *mbp.ActivateRequest) (*mbp.ActivateResponse, error) {
	return e.activateCore(ctx, req, nil)
}

// ActivateWithStructuredFilter is like Activate but accepts a structured filter
// (e.g., *query.Filter) that implements a Match(*storage.Engram) bool interface.
// This allows the MQL executor to pass WHERE predicates for proper post-retrieval filtering.
func (e *Engine) ActivateWithStructuredFilter(ctx context.Context, req *mbp.ActivateRequest, structuredFilter interface{}) (*mbp.ActivateResponse, error) {
	return e.activateCore(ctx, req, structuredFilter)
}

// activateCore is the shared implementation for Activate and ActivateWithStructuredFilter.
// structuredFilter may be nil (plain Activate) or a non-nil filter value (structured query).
func (e *Engine) activateCore(ctx context.Context, req *mbp.ActivateRequest, structuredFilter interface{}) (*mbp.ActivateResponse, error) {
	// Pin a Pebble snapshot so all read phases see a consistent point-in-time view.
	snap := e.store.NewSnapshot()
	defer snap.Close()
	ctx = storage.ContextWithSnapshot(ctx, snap)
	activateStart := time.Now()

	// Resolve per-vault Plasticity config. nil authStore means use defaults (tests, bench).
	var resolved auth.ResolvedPlasticity
	if e.authStore != nil {
		vaultCfg, err := e.authStore.GetVaultConfig(req.Vault)
		if err == nil {
			resolved = auth.ResolvePlasticity(vaultCfg.Plasticity)
		} else {
			slog.Warn("plasticity: failed to read vault config, using defaults",
				"vault", req.Vault, "err", err)
			resolved = auth.ResolvePlasticity(nil)
		}
	} else {
		resolved = auth.ResolvePlasticity(nil)
	}

	// Build activation.ActivateRequest
	wsPrefix := e.store.ResolveVaultPrefix(req.Vault)
	e.activity.Record(wsPrefix)
	vaultSize := e.store.GetVaultCount(ctx, wsPrefix)
	vaultID := wsVaultID(wsPrefix)
	actReq := &activation.ActivateRequest{
		VaultID:            vaultID,
		VaultPrefix:        wsPrefix,
		Context:            req.Context,
		Embedding:          req.Embedding,
		Threshold:          float64(req.Threshold),
		MaxResults:         req.MaxResults,
		HopDepth:           req.MaxHops,
		IncludeWhy:         req.IncludeWhy,
		VaultDefault:       resolved.TraversalProfile,
		Profile:            req.Profile,
		StructuredFilter:   structuredFilter,
		CandidatesPerIndex: activation.CalcCandidatesPerIndex(vaultSize),
	}

	// PAS: Predictive Activation Signal config from vault Plasticity.
	actReq.PASEnabled = resolved.PredictiveActivation
	actReq.PASMaxInjections = resolved.PASMaxInjections

	// Set defaults
	if actReq.MaxResults == 0 {
		actReq.MaxResults = 20
	}
	if actReq.Threshold == 0 {
		actReq.Threshold = 0.1
	}

	// Fix 2: Default to resolved HopDepth (from Plasticity preset) BFS traversal.
	// The association graph is the primary differentiator of MuninnDB — it should
	// be active by default. Order matters: apply default FIRST, then check explicit opt-out.
	if actReq.HopDepth == 0 {
		actReq.HopDepth = resolved.HopDepth
	}
	if req.DisableHops {
		actReq.HopDepth = 0
	}

	// Fix 4: Observe mode is a pure read — skip activation log side effects.
	actReq.ReadOnly = auth.ObserveFromContext(ctx)

	// Convert weights if provided; otherwise apply preset weights from Plasticity config.
	// All scoring goes through ACT-R; legacy temporal path is kept in code but not reachable for now.
	if req.Weights != nil {
		actReq.Weights = &activation.Weights{
			SemanticSimilarity: req.Weights.SemanticSimilarity,
			FullTextRelevance:  req.Weights.FullTextRelevance,
			DecayFactor:        req.Weights.DecayFactor,
			HebbianBoost:       req.Weights.HebbianBoost,
			AccessFrequency:    req.Weights.AccessFrequency,
			Recency:            req.Weights.Recency,
			UseCGDN:            req.Weights.UseCGDN,
			CGDNAlpha:          req.Weights.CGDNAlpha,
			CGDNBeta:           req.Weights.CGDNBeta,
			CGDNPower:          req.Weights.CGDNPower,
			UseACTR:            !req.Weights.DisableACTR,
			DisableACTR:        req.Weights.DisableACTR,
			ACTRDecay:          req.Weights.ACTRDecay,
			ACTRHebScale:       req.Weights.ACTRHebScale,
		}
	} else {
		actrDecay := float32(0.5)
		if resolved.ACTRDecay > 0 {
			actrDecay = float32(resolved.ACTRDecay)
		}
		actrHebScale := float32(4.0)
		if resolved.ACTRHebScale > 0 {
			actrHebScale = float32(resolved.ACTRHebScale)
		}
		actReq.Weights = &activation.Weights{
			// ACT-R ContentMatch gate: 60% semantic, 40% FTS — proven optimal.
			// Plasticity's SemanticWeight/FTSWeight were calibrated for the old 6-component
			// additive formula and produce different semantics in ACT-R mode. Use hardcoded
			// proven values to guarantee deterministic results.
			SemanticSimilarity: 0.6,
			FullTextRelevance:  0.4,
			// Legacy fields kept for backward compat with non-ACT-R scoring paths
			DecayFactor:     float32(resolved.TemporalWeight),
			HebbianBoost:    float32(resolved.HebbianWeight),
			Recency:         float32(resolved.RecencyWeight),
			AccessFrequency: 0.05,
			// ACT-R cognitive parameters (from Plasticity presets)
			UseACTR:      true,
			ACTRDecay:    actrDecay,
			ACTRHebScale: actrHebScale,
		}
	}

	// Gate CGDN behind vault's ExperimentalCGDN flag.
	if actReq.Weights.UseCGDN && !resolved.ExperimentalCGDN {
		actReq.Weights.UseCGDN = false
	}

	// Apply vault default recall mode when no explicit mode was set by the caller.
	// When a caller explicitly sets Mode on the request, the REST handler or MCP handler
	// already applied the preset; the engine only applies the vault default when Mode is empty.
	if req.Mode == "" && resolved.RecallMode != "" && resolved.RecallMode != "balanced" {
		preset, mErr := auth.LookupRecallMode(resolved.RecallMode)
		if mErr == nil {
			if preset.Threshold > 0 && req.Threshold == 0 {
				actReq.Threshold = float64(preset.Threshold)
			}
			if preset.MaxHops > 0 && req.MaxHops == 0 {
				actReq.HopDepth = preset.MaxHops
			}
			if preset.SemanticSimilarity > 0 || preset.FullTextRelevance > 0 || preset.Recency > 0 || preset.DisableACTR {
				w := actReq.Weights
				if w != nil {
					if preset.SemanticSimilarity > 0 && w.SemanticSimilarity == 0 {
						w.SemanticSimilarity = preset.SemanticSimilarity
					}
					if preset.FullTextRelevance > 0 && w.FullTextRelevance == 0 {
						w.FullTextRelevance = preset.FullTextRelevance
					}
					if preset.Recency > 0 && w.Recency == 0 {
						w.Recency = preset.Recency
					}
					if preset.DisableACTR {
						w.DisableACTR = true
						w.UseACTR = false
					}
				}
			}
		}
	}

	// Convert filters if provided
	if len(req.Filters) > 0 {
		actReq.Filters = make([]activation.Filter, len(req.Filters))
		for i, f := range req.Filters {
			actReq.Filters[i] = activation.Filter{
				Field: f.Field,
				Op:    f.Op,
				Value: f.Value,
			}
		}
	}

	// Snapshot the previous activation BEFORE running the current one.
	// The activation log is updated asynchronously by Run()'s drainLog goroutine,
	// so capturing it here guarantees we see the correct "previous" entry.
	var prevActivation []storage.ULID
	if resolved.PredictiveActivation && !auth.ObserveFromContext(ctx) {
		prevEntries := e.activation.AssocLog().RecentForVault(vaultID, 1)
		if len(prevEntries) > 0 {
			prevActivation = prevEntries[0].EngramIDs
		}
	}

	// Run activation
	result, err := e.activation.Run(ctx, actReq)
	if err != nil {
		return nil, fmt.Errorf("activation: %w", err)
	}
	metrics.EngineActivationsTotal.Inc()

	// Entity boost phase: spread activation through shared named entities.
	// After BFS produces a scored set, any engram sharing a named entity with a
	// top-N result receives a small boost. This surfaces entity-linked engrams
	// that have no direct association edge to the query-matching engrams.
	result.Activations = e.applyEntityBoost(ctx, wsPrefix, result.Activations)

	// Convert result.Activations to []mbp.ActivationItem
	items := make([]mbp.ActivationItem, len(result.Activations))
	for i, scored := range result.Activations {
		items[i] = mbp.ActivationItem{
			ID:          scored.Engram.ID.String(),
			Concept:     scored.Engram.Concept,
			Content:     scored.Engram.Content,
			Score:       float32(scored.Score),
			Confidence:  scored.Engram.Confidence,
			Why:         scored.Why,
			Dormant:     scored.Dormant,
			LastAccess:  scored.Engram.LastAccess.UnixNano(),
			AccessCount: scored.Engram.AccessCount,
			Relevance:   scored.Engram.Relevance,
		}

		items[i].ScoreComponents = mbp.ScoreComponents{
			SemanticSimilarity: float32(scored.Components.SemanticSimilarity),
			FullTextRelevance:  float32(scored.Components.FullTextRelevance),
			DecayFactor:        float32(scored.Components.DecayFactor),
			HebbianBoost:       float32(scored.Components.HebbianBoost),
			TransitionBoost:    float32(scored.Components.TransitionBoost),
			AccessFrequency:    float32(scored.Components.AccessFrequency),
			Recency:            float32(scored.Components.Recency),
			Raw:                float32(scored.Components.Raw),
			Final:              float32(scored.Components.Final),
		}

		// Add hop path if present
		if len(scored.HopPath) > 0 {
			items[i].HopPath = make([]string, len(scored.HopPath))
			for j, hop := range scored.HopPath {
				items[i].HopPath[j] = hop.String()
			}
		}
	}

	// Parallel provenance fetch — each goroutine owns its index, no mutex needed.
	var wg sync.WaitGroup
	for i, scored := range result.Activations {
		if err := ctx.Err(); err != nil {
			break
		}
		wg.Add(1)
		go func(idx int, id [16]byte) {
			defer wg.Done()
			entries, err := e.prov.Get(ctx, wsPrefix, id)
			if err != nil {
				slog.Warn("activate: provenance lookup failed", "id", id, "err", err)
				return
			}
			if len(entries) > 0 {
				items[idx].SourceType = sourceTypeString(entries[len(entries)-1].Source)
			}
		}(i, [16]byte(scored.Engram.ID))
	}
	wg.Wait()

	// Submit co-activations to Hebbian worker (skipped in observe mode or when disabled by Plasticity).
	// On Lobe nodes (hebbianWorker == nil) collect refs for forwarding to Cortex instead.
	var lobeCoActivations []mbp.CoActivationRef
	if len(result.Activations) > 0 && !auth.ObserveFromContext(ctx) && resolved.HebbianEnabled {
		hebW, _, _ := e.cogWorkers()
		if hebW != nil {
			coActivatedEngrams := make([]cognitive.CoActivatedEngram, len(result.Activations))
			for i, scored := range result.Activations {
				coActivatedEngrams[i] = cognitive.CoActivatedEngram{
					ID:    scored.Engram.ID,
					Score: scored.Score,
				}
			}
			hebW.Submit(cognitive.CoActivationEvent{
				WS:      wsPrefix,
				At:      time.Now(),
				Engrams: coActivatedEngrams,
			})
		} else if e.coordinator != nil {
			lobeCoActivations = make([]mbp.CoActivationRef, len(result.Activations))
			for i, scored := range result.Activations {
				lobeCoActivations[i] = mbp.CoActivationRef{
					ID:    [16]byte(scored.Engram.ID),
					Score: scored.Score,
				}
			}
		}
	}

	// PAS: Record sequential transitions (previous activation → current activation).
	// Uses the prevActivation snapshot captured before Run() to avoid race conditions
	// with the async drainLog goroutine.
	if len(result.Activations) > 0 && len(prevActivation) > 0 {
		e.cogMu.RLock()
		tw := e.transitionWorker
		e.cogMu.RUnlock()
		if tw != nil {
			prevEngrams := make([]cognitive.TransitionEngram, len(prevActivation))
			for i, id := range prevActivation {
				prevEngrams[i] = cognitive.TransitionEngram{ID: [16]byte(id)}
			}
			currEngrams := make([]cognitive.TransitionEngram, len(result.Activations))
			for i, scored := range result.Activations {
				currEngrams[i] = cognitive.TransitionEngram{ID: [16]byte(scored.Engram.ID)}
			}
			tw.Submit(cognitive.TransitionEvent{
				WS:       wsPrefix,
				Previous: prevEngrams,
				Current:  currEngrams,
			})
		}
	}

	// Note: co-activation is NOT fed to the confidence worker. Co-activation is
	// evidence of relevance, not truth. Boosting confidence on every retrieval
	// creates a feedback loop that inflates confidence for active engrams regardless
	// of their actual reliability. Only user_confirmed/rejected and
	// contradiction_detected drive Bayesian confidence updates.

	// Forward collected Lobe side effects to the Cortex asynchronously.
	// Only fires when workers are nil (Lobe mode) and coordinator is wired.
	if e.coordinator != nil && len(lobeCoActivations) > 0 {
		effect := mbp.CognitiveSideEffect{
			QueryID:       e.fastQueryID(),
			OriginNodeID:  e.coordinatorID,
			Timestamp:     time.Now().UnixNano(),
			CoActivations: lobeCoActivations,
		}
		e.coordinator.ForwardCognitiveEffects(effect)
	}

	// Build activation brief (extractive summarization, LLM-free).
	// BriefMode: "" or "auto" → embedding-based if embedder available, else fallback to heuristic;
	//            "extractive" → always heuristic-based; "llm" → skip (LLM not wired here yet).
	var briefSentences []mbp.BriefSentence
	briefMode := req.BriefMode
	if briefMode == "" {
		briefMode = "auto"
	}
	if briefMode == "extractive" || briefMode == "auto" {
		// Try embedding-based brief if available and we have context embedding
		if briefMode == "auto" && e.embedder != nil && len(req.Embedding) > 0 {
			// Embedding-based approach: score sentences by cosine similarity to context embedding
			briefSentences = e.generateEmbeddingBrief(ctx, items, req.Embedding)
		}

		// Fallback to heuristic approach if embedding-based didn't produce results
		if len(briefSentences) == 0 {
			engContents := make([]enginebrief.EngramContent, 0, len(items))
			for _, item := range items {
				engContents = append(engContents, enginebrief.EngramContent{
					ID:      item.ID,
					Content: item.Content,
				})
			}
			sentences := enginebrief.Compute(engContents, req.Context)
			if len(sentences) > 0 {
				briefSentences = make([]mbp.BriefSentence, len(sentences))
				for i, s := range sentences {
					briefSentences[i] = mbp.BriefSentence{
						EngramID: s.EngramID,
						Text:     s.Text,
						Score:    s.Score,
					}
				}
			}
		}
	}

	d := time.Since(activateStart)
	if e.latencyTracker != nil {
		e.latencyTracker.Record(wsPrefix, "activate", d)
	}
	metrics.ActivateDuration.WithLabelValues(req.Vault).Observe(d.Seconds())

	return &mbp.ActivateResponse{
		QueryID:     e.fastQueryID(),
		TotalFound:  result.TotalFound,
		Activations: items,
		LatencyMs:   result.LatencyMs,
		Brief:       briefSentences,
	}, nil
}

// Subscribe implements mbp.EngineAPI.Subscribe.
// Delegates to SubscribeWithDeliver with a nil deliver func (MBP/gRPC callers
// set their own deliver func after Subscribe returns via the subscription ID).
func (e *Engine) Subscribe(ctx context.Context, req *mbp.SubscribeRequest) (*mbp.SubscribeResponse, error) {
	subID, err := e.SubscribeWithDeliver(ctx, req, nil)
	if err != nil {
		return nil, err
	}
	return &mbp.SubscribeResponse{SubID: subID, Status: "active"}, nil
}

// SubscribeWithDeliver registers a subscription and immediately sets a delivery
// function that is called (in a goroutine by DeliveryRouter) on every push.
// Returns the subscription ID. Pass deliver=nil to register without a delivery
// function (useful for MBP clients that pull via the stream).
func (e *Engine) SubscribeWithDeliver(ctx context.Context, req *mbp.SubscribeRequest, deliver trigger.DeliverFunc) (string, error) {
	subID := req.SubscriptionID
	if subID == "" {
		subID = uuid.New().String()
	}

	// Resolve vault to a routing uint32 using the same BigEndian convention
	// already used in storage/impl.go (binary.BigEndian.Uint32(wsPrefix[:4])).
	// This is a compact routing key; the full 8-byte prefix is preserved in the
	// workspace prefix used for storage lookups.
	wsPrefix := e.store.ResolveVaultPrefix(req.Vault)
	vaultID := wsVaultID(wsPrefix)

	sub := &trigger.Subscription{
		ID:             subID,
		VaultID:        vaultID,
		Context:        req.Context,
		Threshold:      float64(req.Threshold),
		TTL:            time.Duration(req.TTL) * time.Second,
		RateLimit:      req.RateLimit,
		PushOnWrite:    req.PushOnWrite,
		DeltaThreshold: float64(req.DeltaThreshold),
		Deliver:        deliver,
	}

	if err := e.triggers.Subscribe(sub); err != nil {
		return "", fmt.Errorf("subscribe: %w", err)
	}
	return subID, nil
}

// Unsubscribe implements mbp.EngineAPI.Unsubscribe.
func (e *Engine) Unsubscribe(ctx context.Context, subID string) error {
	e.triggers.Unsubscribe(subID)
	return nil
}

// Link implements mbp.EngineAPI.Link.
func (e *Engine) Link(ctx context.Context, req *mbp.LinkRequest) (*mbp.LinkResponse, error) {
	wsPrefix := e.store.ResolveVaultPrefix(req.Vault)

	sourceID, err := storage.ParseULID(req.SourceID)
	if err != nil {
		return nil, fmt.Errorf("parse source id: %w", err)
	}

	targetID, err := storage.ParseULID(req.TargetID)
	if err != nil {
		return nil, fmt.Errorf("parse target id: %w", err)
	}

	// Guard: reject links to/from soft-deleted engrams. A single batched
	// GetMetadata call avoids two expensive GetEngram round-trips.
	metas, err := e.store.GetMetadata(ctx, wsPrefix, []storage.ULID{sourceID, targetID})
	if err != nil {
		return nil, fmt.Errorf("link: check endpoint states: %w", err)
	}
	for _, meta := range metas {
		if meta == nil {
			return nil, ErrEngramNotFound
		}
		if meta.State == storage.StateSoftDeleted {
			return nil, ErrEngramSoftDeleted
		}
	}

	assoc := &storage.Association{
		TargetID:   targetID,
		RelType:    storage.RelType(req.RelType),
		Weight:     req.Weight,
		Confidence: 1.0,
		CreatedAt:  time.Now(),
	}

	if err := e.store.WriteAssociation(ctx, wsPrefix, sourceID, targetID, assoc); err != nil {
		return nil, fmt.Errorf("write association: %w", err)
	}

	// When a "contradicts" link is explicitly created via Link(), notify the
	// ContradictWorker so it can flag the pair and drive confidence updates.
	if storage.RelType(req.RelType) == storage.RelContradicts {
		_, linkContra, linkConf := e.cogWorkers()
		if linkContra != nil {
			linkContra.Submit(cognitive.ContradictItem{
				WS:          wsPrefix,
				EngramID:    [16]byte(sourceID),
				ConceptHash: 0,
				Associations: []cognitive.ContradictAssoc{
					{
						EngramID:   [16]byte(sourceID),
						TargetID:   [16]byte(targetID),
						TargetHash: 0,
						RelType:    uint16(storage.RelContradicts),
					},
					{
						EngramID:   [16]byte(targetID),
						TargetID:   [16]byte(sourceID),
						TargetHash: 0,
						RelType:    uint16(storage.RelSupports),
					},
				},
				OnFound: func(ev cognitive.ContradictionEvent) {
					if e.triggers != nil {
						e.triggers.NotifyContradiction(wsVaultID(wsPrefix), storage.ULID(ev.EngramA), storage.ULID(ev.EngramB), ev.Severity, "explicit_link")
					}
					_, _, cw := e.cogWorkers()
					if cw != nil {
						cw.Submit(cognitive.ConfidenceUpdate{
							WS:       wsPrefix,
							EngramID: ev.EngramA,
							Evidence: cognitive.EvidenceContradiction,
							Source:   "contradiction_detected",
						})
						cw.Submit(cognitive.ConfidenceUpdate{
							WS:       wsPrefix,
							EngramID: ev.EngramB,
							Evidence: cognitive.EvidenceContradiction,
							Source:   "contradiction_detected",
						})
					}
				},
			})
		}
		// Also directly flag the pair and update confidence without waiting for
		// ContradictWorker's batch processing — direct link is an explicit assertion.
		if linkConf != nil {
			linkConf.Submit(cognitive.ConfidenceUpdate{
				WS:       wsPrefix,
				EngramID: [16]byte(sourceID),
				Evidence: cognitive.EvidenceContradiction,
				Source:   "contradiction_detected",
			})
			linkConf.Submit(cognitive.ConfidenceUpdate{
				WS:       wsPrefix,
				EngramID: [16]byte(targetID),
				Evidence: cognitive.EvidenceContradiction,
				Source:   "contradiction_detected",
			})
		}
	}

	return &mbp.LinkResponse{OK: true}, nil
}

// Forget implements mbp.EngineAPI.Forget.
func (e *Engine) Forget(ctx context.Context, req *mbp.ForgetRequest) (*mbp.ForgetResponse, error) {
	wsPrefix := e.store.ResolveVaultPrefix(req.Vault)

	id, err := storage.ParseULID(req.ID)
	if err != nil {
		return nil, fmt.Errorf("parse id: %w", err)
	}

	if req.Hard {
		if err := e.store.DeleteEngram(ctx, wsPrefix, id); err != nil {
			return nil, fmt.Errorf("hard delete: %w", err)
		}
		// Mark the node as deleted in the HNSW index so it is skipped in
		// future Search results. Memory is reclaimed on the next rebuild.
		if e.hnswRegistry != nil {
			e.hnswRegistry.TombstoneNode(wsPrefix, [16]byte(id))
		}
		// Decrement the global engram counter. Floor at zero to guard against
		// counter skew in crash-recovery scenarios (mirrors ClearVault's guard).
		for {
			cur := e.engramCount.Load()
			if cur <= 0 {
				break
			}
			if e.engramCount.CompareAndSwap(cur, cur-1) {
				break
			}
		}
	} else {
		// Read the engram before soft-deleting so we can clean up FTS index entries.
		eng, readErr := e.store.GetEngram(ctx, wsPrefix, id)
		if err := e.store.SoftDelete(ctx, wsPrefix, id); err != nil {
			return nil, fmt.Errorf("soft delete: %w", err)
		}
		// Remove FTS posting-list entries so soft-deleted engrams do not appear in search.
		if readErr == nil && eng != nil && e.fts != nil {
			if ftErr := e.fts.DeleteEngram(wsPrefix, [16]byte(id), eng.Concept, eng.CreatedBy, eng.Content, eng.Tags); ftErr != nil {
				slog.Warn("engine: fts cleanup failed after soft delete", "id", req.ID, "error", ftErr)
			}
		}
	}

	return &mbp.ForgetResponse{OK: true}, nil
}

// Stat implements mbp.EngineAPI.Stat.
func (e *Engine) Stat(ctx context.Context, req *mbp.StatRequest) (*mbp.StatResponse, error) {
	vaultNames, _ := e.store.ListVaultNames()

	// Count all vaults: data vaults + config-only (empty) vaults, deduplicated.
	// A vault created via `muninn vault create` only writes an auth config entry
	// until the first engram is stored. Without this merge, the dashboard would
	// show a lower vault count than `muninn vault list`.
	vaultSet := make(map[string]struct{}, len(vaultNames))
	for _, n := range vaultNames {
		vaultSet[n] = struct{}{}
	}
	if e.authStore != nil {
		if cfgs, err := e.authStore.ListVaultConfigs(); err == nil {
			for _, cfg := range cfgs {
				vaultSet[cfg.Name] = struct{}{}
			}
		}
	}
	vaultCount := len(vaultSet)
	if vaultCount == 0 {
		vaultCount = 1 // preserve the existing "minimum 1" semantics
	}

	engramCount := e.engramCount.Load()
	if req.Vault != "" {
		wsPrefix := e.store.ResolveVaultPrefix(req.Vault)
		engramCount = e.store.GetVaultCount(ctx, wsPrefix)
	}

	resp := &mbp.StatResponse{
		EngramCount:  engramCount,
		VaultCount:   vaultCount,
		StorageBytes: e.store.DiskSize(),
	}

	// Attach coherence scores for all vaults if the registry is populated.
	if e.coherence != nil {
		snapshots := e.coherence.Snapshots()
		if len(snapshots) > 0 {
			resp.CoherenceScores = make(map[string]mbp.CoherenceResult, len(snapshots))
			for _, snap := range snapshots {
				resp.CoherenceScores[snap.VaultName] = mbp.CoherenceResult{
					Score:                snap.Score,
					OrphanRatio:          snap.OrphanRatio,
					ContradictionDensity: snap.ContradictionDensity,
					DuplicationPressure:  snap.DuplicationPressure,
					TemporalVariance:     snap.TemporalVariance,
					TotalEngrams:         snap.TotalEngrams,
				}
			}
		}
	}

	return resp, nil
}

// ListVaults returns all vault names that have been written to.
func (e *Engine) ListVaults(ctx context.Context) ([]string, error) {
	return e.store.ListVaultNames()
}

// SetCognitiveWorkers replaces the engine's cognitive worker references.
// Used during cluster role changes: on promotion to Cortex, pass freshly
// created workers; on demotion to Lobe, call ClearCognitiveWorkers instead.
// Thread-safe: holds cogMu write lock.
func (e *Engine) SetCognitiveWorkers(
	heb *cognitive.HebbianWorker,
	contradict *cognitive.Worker[cognitive.ContradictItem],
	confidence *cognitive.Worker[cognitive.ConfidenceUpdate],
) {
	e.cogMu.Lock()
	// Wire trigger callback before publishing the worker reference, so
	// concurrent goroutines never see a worker without its callback.
	if heb != nil && e.triggers != nil {
		heb.OnWeightUpdate = func(ws [8]byte, id [16]byte, field string, old, new float64) {
			vaultID := wsVaultID(ws)
			e.triggers.NotifyCognitive(vaultID, storage.ULID(id), field, float32(old), float32(new))
		}
	}
	e.hebbianWorker = heb
	e.contradictWorker = contradict
	e.confidenceWorker = confidence
	e.cogMu.Unlock()
}

// SetTransitionWorker sets the PAS transition worker. Thread-safe.
func (e *Engine) SetTransitionWorker(tw *cognitive.TransitionWorker) {
	e.cogMu.Lock()
	e.transitionWorker = tw
	e.cogMu.Unlock()
}

// ClearCognitiveWorkers nils out the worker references so the engine
// operates in Lobe mode (no local cognitive processing).
// Thread-safe: holds cogMu write lock.
func (e *Engine) ClearCognitiveWorkers() {
	e.cogMu.Lock()
	e.hebbianWorker = nil
	e.contradictWorker = nil
	e.confidenceWorker = nil
	e.transitionWorker = nil
	e.cogMu.Unlock()
}

// cogWorkers returns thread-safe snapshots of the cognitive worker pointers.
// Safe to use after return even if ClearCognitiveWorkers runs concurrently,
// because the old worker objects remain valid (just won't receive new events).
func (e *Engine) cogWorkers() (*cognitive.HebbianWorker, *cognitive.Worker[cognitive.ContradictItem], *cognitive.Worker[cognitive.ConfidenceUpdate]) {
	e.cogMu.RLock()
	h, ct, cf := e.hebbianWorker, e.contradictWorker, e.confidenceWorker
	e.cogMu.RUnlock()
	return h, ct, cf
}

// WorkerStats returns the current statistics for all cognitive workers.
func (e *Engine) WorkerStats() cognitive.EngineWorkerStats {
	heb, contra, conf := e.cogWorkers()
	stats := cognitive.EngineWorkerStats{}
	if heb != nil {
		stats.Hebbian = heb.Stats()
	}
	if contra != nil {
		stats.Contradict = contra.Stats()
	}
	if conf != nil {
		stats.Confidence = conf.Stats()
	}
	return stats
}

// Restore un-deletes a soft-deleted engram by restoring its state to StateActive.
// Returns an error if the engram does not exist or was hard-deleted.
func (e *Engine) Restore(ctx context.Context, vault, id string) (*storage.Engram, error) {
	ws := e.store.ResolveVaultPrefix(vault)
	ulid, err := storage.ParseULID(id)
	if err != nil {
		return nil, fmt.Errorf("parse id: %w", err)
	}
	eng, err := e.store.GetEngram(ctx, ws, ulid)
	if err != nil {
		return nil, fmt.Errorf("restore: %w", err)
	}
	if eng.State != storage.StateSoftDeleted {
		return nil, fmt.Errorf("restore: engram %s is not soft-deleted (state=%d)", id, eng.State)
	}
	meta := &storage.EngramMeta{
		State:       storage.StateActive,
		Confidence:  eng.Confidence,
		Relevance:   eng.Relevance,
		Stability:   eng.Stability,
		AccessCount: eng.AccessCount,
		UpdatedAt:   time.Now(),
		LastAccess:  eng.LastAccess,
	}
	if err := e.store.UpdateMetadata(ctx, ws, ulid, meta); err != nil {
		return nil, fmt.Errorf("restore update: %w", err)
	}
	eng.State = storage.StateActive
	return eng, nil
}

// UpdateLifecycleState transitions an engram to the named lifecycle state.
func (e *Engine) UpdateLifecycleState(ctx context.Context, vault, id, state string) error {
	ws := e.store.ResolveVaultPrefix(vault)
	ulid, err := storage.ParseULID(id)
	if err != nil {
		return fmt.Errorf("parse id: %w", err)
	}
	eng, err := e.store.GetEngram(ctx, ws, ulid)
	if err != nil {
		return fmt.Errorf("get engram: %w", err)
	}
	newState, err := storage.ParseLifecycleState(state)
	if err != nil {
		return err
	}
	meta := &storage.EngramMeta{
		State:       newState,
		Confidence:  eng.Confidence,
		Relevance:   eng.Relevance,
		Stability:   eng.Stability,
		AccessCount: eng.AccessCount,
		UpdatedAt:   time.Now(),
		LastAccess:  eng.LastAccess,
	}
	return e.store.UpdateMetadata(ctx, ws, ulid, meta)
}

// ListDeleted returns soft-deleted engrams in the vault, up to limit.
func (e *Engine) ListDeleted(ctx context.Context, vault string, limit int) ([]*storage.Engram, error) {
	ws := e.store.ResolveVaultPrefix(vault)
	ids, err := e.store.ListByState(ctx, ws, storage.StateSoftDeleted, limit)
	if err != nil {
		return nil, err
	}
	return e.store.GetEngrams(ctx, ws, ids)
}

// wsVaultID extracts a uint32 routing ID from the first 4 bytes of a workspace
// prefix. This mirrors the convention in storage/impl.go line ~146 and the
// trigger system's vaultWS() function. Used to route write events to the
// correct subscription buckets in the trigger registry.
func wsVaultID(ws [8]byte) uint32 {
	return binary.BigEndian.Uint32(ws[:4])
}

// relTypeFromString converts a relation name string to a uint16 RelType value.
func relTypeFromString(rel string) uint16 {
	m := map[string]storage.RelType{
		"supports": storage.RelSupports, "contradicts": storage.RelContradicts,
		"depends_on": storage.RelDependsOn, "supersedes": storage.RelSupersedes,
		"relates_to": storage.RelRelatesTo, "is_part_of": storage.RelIsPartOf,
		"causes": storage.RelCauses, "preceded_by": storage.RelPrecededBy,
		"followed_by": storage.RelFollowedBy, "created_by_person": storage.RelCreatedByPerson,
		"belongs_to_project": storage.RelBelongsToProject, "references": storage.RelReferences,
		"implements": storage.RelImplements, "blocks": storage.RelBlocks,
		"resolves": storage.RelResolves, "refines": storage.RelRefines,
	}
	if v, ok := m[rel]; ok {
		return uint16(v)
	}
	return uint16(storage.RelRelatesTo)
}

// hashString returns a simple hash of a string for concept/target matching.
func hashString(s string) uint32 {
	h := uint32(5381)
	for _, c := range s {
		h = ((h << 5) + h) + uint32(c)
	}
	return h
}

// SessionResult holds the result of a Session query.
type SessionResult struct {
	Writes []EngineSessionEntry
	Since  time.Time
}

// EngineSessionEntry represents a single write in the session window.
type EngineSessionEntry struct {
	ID      string
	Concept string
	At      time.Time
}

// Evolve creates a new version of an existing engram and soft-deletes the old one.
// It links the new engram to the old one with RelSupersedes and returns the new ID.
// All three writes (new engram, supersedes association, old engram state) are committed
// in a single atomic Pebble batch so a crash cannot leave the store in an inconsistent state.
func (e *Engine) Evolve(ctx context.Context, vault, oldID, newContent, reason string) (storage.ULID, error) {
	wsPrefix := e.store.ResolveVaultPrefix(vault)

	// Parse the old ULID before any writes.
	oldULID, err := storage.ParseULID(oldID)
	if err != nil {
		return storage.ULID{}, fmt.Errorf("evolve: parse old id: %w", err)
	}

	// Read the old engram to inherit Concept and Tags.
	oldEng, err := e.store.GetEngram(ctx, wsPrefix, oldULID)
	if err != nil {
		return storage.ULID{}, fmt.Errorf("evolve: read old engram: %w", err)
	}
	if oldEng == nil {
		return storage.ULID{}, fmt.Errorf("evolve: engram %s not found", oldID)
	}

	// Build the new engram with a pre-assigned ULID so we can reference it in the
	// supersedes association within the same batch.
	newULID := storage.NewULID()
	now := time.Now()
	newEng := &storage.Engram{
		ID:         newULID,
		Concept:    oldEng.Concept + " (evolved)",
		Content:    newContent,
		Tags:       oldEng.Tags,
		Confidence: 1.0,
		Stability:  30.0,
		State:      storage.StateActive,
		CreatedAt:  now,
		UpdatedAt:  now,
		LastAccess: now,
	}

	// Build the supersedes association (new → old).
	supersedes := &storage.Association{
		TargetID:      oldULID,
		RelType:       storage.RelSupersedes,
		Weight:        1.0,
		Confidence:    1.0,
		CreatedAt:     now,
		LastActivated: int32(now.Unix()),
	}

	// Single atomic batch: write new engram + supersedes association + soft-delete old engram.
	batch := e.store.NewBatch()
	defer batch.Discard()

	if err := batch.WriteEngram(ctx, wsPrefix, newEng); err != nil {
		return storage.ULID{}, fmt.Errorf("evolve: batch write new engram: %w", err)
	}
	if err := batch.WriteAssociation(ctx, wsPrefix, newULID, oldULID, supersedes); err != nil {
		return storage.ULID{}, fmt.Errorf("evolve: batch write association: %w", err)
	}
	if err := batch.UpdateEngramState(ctx, wsPrefix, oldULID, storage.StateSoftDeleted); err != nil {
		return storage.ULID{}, fmt.Errorf("evolve: batch update old state: %w", err)
	}
	if err := batch.Commit(); err != nil {
		return storage.ULID{}, fmt.Errorf("evolve: batch commit: %w", err)
	}

	// Persist vault name (idempotent).
	if err := e.store.WriteVaultName(wsPrefix, vault); err != nil {
		slog.Warn("engine: failed to persist vault name", "vault", vault, "err", err)
	}

	// Submit new engram to async FTS worker.
	if e.ftsWorker != nil {
		e.ftsWorker.Submit(fts.IndexJob{
			WS:        wsPrefix,
			ID:        [16]byte(newULID),
			Concept:   newEng.Concept,
			CreatedBy: newEng.CreatedBy,
			Content:   newEng.Content,
		})
	}

	return newULID, nil
}

// Consolidate merges multiple engrams into a single new engram and archives the originals.
// Returns the new merged ID, the list of archived IDs, and any non-fatal warnings.
func (e *Engine) Consolidate(ctx context.Context, vault string, ids []string, mergedContent string) (storage.ULID, []string, []string, error) {
	if len(ids) > 50 {
		return storage.ULID{}, nil, nil, fmt.Errorf("consolidate: too many ids (max 50, got %d)", len(ids))
	}
	mergedResp, err := e.Write(ctx, &mbp.WriteRequest{
		Vault:   vault,
		Concept: "Consolidated memory",
		Content: mergedContent,
	})
	if err != nil {
		return storage.ULID{}, nil, nil, fmt.Errorf("consolidate: write merged: %w", err)
	}

	var archived []string
	var warnings []string
	for _, id := range ids {
		_, err := e.Forget(ctx, &mbp.ForgetRequest{ID: id, Hard: false, Vault: vault})
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("failed to archive %s: %v", id, err))
		} else {
			archived = append(archived, id)
		}
	}

	mergedULID, err := storage.ParseULID(mergedResp.ID)
	if err != nil {
		return storage.ULID{}, archived, warnings, fmt.Errorf("consolidate: parse merged id: %w", err)
	}
	return mergedULID, archived, warnings, nil
}

// Session returns all engrams written to the vault after the given time.
func (e *Engine) Session(ctx context.Context, vault string, since time.Time) (*SessionResult, error) {
	res, err := e.SessionPaged(ctx, vault, since, 0, 500)
	if err != nil {
		return nil, err
	}
	return &res.SessionResult, nil
}

// SessionPagedResult extends SessionResult with total count for pagination.
type SessionPagedResult struct {
	SessionResult
	Total int
}

// SessionPaged returns engrams created since the given time with offset/limit pagination.
func (e *Engine) SessionPaged(ctx context.Context, vault string, since time.Time, offset, limit int) (*SessionPagedResult, error) {
	ws := e.store.ResolveVaultPrefix(vault)
	// Fetch one extra to know if there are more pages.
	engrams, err := e.store.EngramsByCreatedSince(ctx, ws, since, offset, limit)
	if err != nil {
		return nil, err
	}
	result := &SessionPagedResult{
		SessionResult: SessionResult{Since: since},
	}
	for _, eng := range engrams {
		if eng != nil {
			result.Writes = append(result.Writes, EngineSessionEntry{
				ID:      eng.ID.String(),
				Concept: eng.Concept,
				At:      eng.CreatedAt,
			})
		}
	}
	result.Total = len(result.Writes) + offset
	return result, nil
}

// Decide records an explicit decision with rationale, alternatives, and supporting evidence.
// It returns the ULID of the newly written decision engram.
func (e *Engine) Decide(ctx context.Context, vault, decision, rationale string, alternatives, evidenceIDs []string) (storage.ULID, error) {
	content := rationale
	if len(alternatives) > 0 {
		content += "\n---\nAlternatives:\n" + strings.Join(alternatives, "\n")
	}

	resp, err := e.Write(ctx, &mbp.WriteRequest{
		Vault:   vault,
		Concept: decision,
		Content: content,
		Tags:    []string{"decision"},
	})
	if err != nil {
		return storage.ULID{}, fmt.Errorf("decide: write: %w", err)
	}

	for _, eid := range evidenceIDs {
		_, _ = e.Link(ctx, &mbp.LinkRequest{
			SourceID: resp.ID,
			TargetID: eid,
			RelType:  uint16(storage.RelSupports),
			Weight:   1.0,
			Vault:    vault,
		})
	}

	decideULID, err := storage.ParseULID(resp.ID)
	if err != nil {
		return storage.ULID{}, fmt.Errorf("decide: parse id: %w", err)
	}
	return decideULID, nil
}

// RecordAccess increments the access count and updates the last-accessed timestamp
// for the engram identified by id in the given vault.
func (e *Engine) RecordAccess(ctx context.Context, vault, id string) error {
	ws := e.store.ResolveVaultPrefix(vault)
	ulid, err := storage.ParseULID(id)
	if err != nil {
		return fmt.Errorf("record_access: parse id: %w", err)
	}
	eng, err := e.store.GetEngram(ctx, ws, ulid)
	if err != nil {
		return fmt.Errorf("record_access: get engram: %w", err)
	}
	meta := &storage.EngramMeta{
		State:       eng.State,
		Confidence:  eng.Confidence,
		Relevance:   eng.Relevance,
		Stability:   eng.Stability,
		AccessCount: eng.AccessCount + 1,
		UpdatedAt:   eng.UpdatedAt,
		LastAccess:  time.Now(),
	}
	return e.store.UpdateMetadata(ctx, ws, ulid, meta)
}

// resolveVaultPlasticity returns the resolved plasticity config for a vault,
// falling back to defaults when no authStore is configured.
func (e *Engine) resolveVaultPlasticity(vaultName string) auth.ResolvedPlasticity {
	if e.authStore != nil {
		vaultCfg, err := e.authStore.GetVaultConfig(vaultName)
		if err == nil {
			return auth.ResolvePlasticity(vaultCfg.Plasticity)
		}
	}
	return auth.ResolvePlasticity(nil)
}

// ResolveVaultPlasticity returns the resolved plasticity config for a vault.
func (e *Engine) ResolveVaultPlasticity(vaultName string) auth.ResolvedPlasticity {
	return e.resolveVaultPlasticity(vaultName)
}

// PruneVault prunes a vault according to its resolved MaxEngrams and RetentionDays policy.
// It uses hard-delete (removes all secondary indexes) to ensure pruned engrams do not
// persist in the relevance bucket index and cause an infinite prune loop.
// Returns the number of engrams pruned.
func (e *Engine) PruneVault(ctx context.Context, vaultName string) (int64, error) {
	mu := e.getVaultMutex(vaultName)
	mu.Lock()
	defer mu.Unlock()

	resolved := e.resolveVaultPlasticity(vaultName)
	ws := e.store.ResolveVaultPrefix(vaultName)

	var pruned int64

	// MaxEngrams: two-phase prune — use stale relevance index as pre-filter, then
	// re-rank candidates by real ACT-R base-level score to delete the worst engrams.
	if resolved.MaxEngrams > 0 {
		count := e.store.GetVaultCount(ctx, ws)
		excess := count - int64(resolved.MaxEngrams)
		if excess > 0 {
			// Phase 1: fetch heuristic candidates from the relevance bucket index.
			// Over-fetch to compensate for index staleness (new engrams start at relevance=0).
			topK := int(excess) * 2
			var candidates []storage.ULID
			for attempt := 0; attempt < 2; attempt++ {
				ids, err := e.store.LowestRelevanceIDs(ctx, ws, topK)
				if err != nil {
					return pruned, fmt.Errorf("prune vault %s (max_engrams scan): %w", vaultName, err)
				}
				candidates = ids
				if int64(len(candidates)) >= excess {
					break
				}
				topK = int(excess) * 4
			}

			// Phase 2: load metadata and compute real ACT-R base-level score for each candidate.
			// B(M) = ln(n+1) - d * ln(max(ageDays, 0.1) / n)  where d=0.5 (standard decay).
			// Engrams with low B(M) are stale and rarely accessed — safest to delete.
			type scoredCandidate struct {
				id    storage.ULID
				score float64
			}
			const actrDecay = 0.5
			now := time.Now()
			scored := make([]scoredCandidate, 0, len(candidates))

			if len(candidates) > 0 {
				metas, err := e.store.GetMetadata(ctx, ws, candidates)
				if err != nil {
					// Fall back: delete candidates in index order without rescoring.
					metas = nil
				}
				for i, id := range candidates {
					var b float64
					if metas != nil && i < len(metas) && metas[i] != nil {
						m := metas[i]
						lastAccess := m.LastAccess
						if lastAccess.IsZero() || lastAccess.Year() < 2000 {
							lastAccess = now
						}
						ageDays := math.Max(now.Sub(lastAccess).Hours()/24.0, 0.1)
						n := float64(m.AccessCount + 1)
						b = math.Log(n) - actrDecay*math.Log(math.Max(ageDays, 0.1)/n)
					}
					scored = append(scored, scoredCandidate{id: id, score: b})
				}
				// Sort ascending: lowest base-level (worst engrams) first.
				sort.Slice(scored, func(i, j int) bool {
					return scored[i].score < scored[j].score
				})
			}

			// Delete the bottom `excess` by ACT-R base-level score (or all available if fewer).
			toDelete := int(excess)
			if toDelete > len(scored) {
				toDelete = len(scored)
			}
			for i := 0; i < toDelete; i++ {
				id := scored[i].id
				if err := e.store.DeleteEngram(ctx, ws, id); err != nil {
					slog.Debug("prune vault: hard-delete failed", "vault", vaultName, "id", id, "err", err)
					continue
				}
				// Decrement the global engram counter.
				for {
					cur := e.engramCount.Load()
					if cur <= 0 {
						break
					}
					if e.engramCount.CompareAndSwap(cur, cur-1) {
						break
					}
				}
				pruned++
			}
		}
	}

	// RetentionDays: hard-delete engrams older than the retention threshold.
	if resolved.RetentionDays > 0 {
		cutoff := time.Now().Add(-time.Duration(float64(resolved.RetentionDays) * float64(24*time.Hour)))
		// Scan 0x01 EngramKey range for engrams created before cutoff.
		// EngramIDsByCreatedRange with epoch→cutoff returns IDs of old engrams.
		const retentionBatchSize = 500
		epoch := time.Unix(0, 0)
		ids, err := e.store.EngramIDsByCreatedRange(ctx, ws, epoch, cutoff, retentionBatchSize)
		if err != nil {
			return pruned, fmt.Errorf("prune vault %s (retention scan): %w", vaultName, err)
		}
		for _, id := range ids {
			if err := e.store.DeleteEngram(ctx, ws, id); err != nil {
				slog.Debug("prune vault: retention hard-delete failed", "vault", vaultName, "id", id, "err", err)
				continue
			}
			// Decrement the global engram counter.
			for {
				cur := e.engramCount.Load()
				if cur <= 0 {
					break
				}
				if e.engramCount.CompareAndSwap(cur, cur-1) {
					break
				}
			}
			pruned++
		}
	}

	return pruned, nil
}

// runPruneWorker is a periodic background sweep that prunes vaults according to their
// MaxEngrams, RetentionDays, and AssocDecayFactor policies. Runs every 60s with ±5s jitter.
func (e *Engine) runPruneWorker() {
	defer close(e.pruneDone)
	jitter := time.Duration(rand.Intn(10)) * time.Second
	timer := time.NewTimer(60*time.Second + jitter)
	defer timer.Stop()
	for {
		select {
		case <-timer.C:
			vaults, _ := e.ListVaults(e.stopCtx)
			for _, vaultName := range vaults {
				resolved := e.resolveVaultPlasticity(vaultName)
				if resolved.MaxEngrams > 0 || resolved.RetentionDays > 0 {
					if _, err := e.PruneVault(e.stopCtx, vaultName); err != nil {
						slog.Debug("vault prune failed", "vault", vaultName, "err", err)
					}
				}
			}
			for _, vaultName := range vaults {
				resolved := e.resolveVaultPlasticity(vaultName)
				if resolved.HebbianEnabled && resolved.AssocDecayFactor > 0 {
					ws := e.store.ResolveVaultPrefix(vaultName)
					removed, err := e.store.DecayAssocWeights(e.stopCtx, ws,
						float64(resolved.AssocDecayFactor), resolved.AssocMinWeight)
					if err != nil {
						slog.Debug("assoc decay failed", "vault", vaultName, "err", err)
					} else if removed > 0 {
						slog.Info("assoc decay pruned edges", "vault", vaultName, "removed", removed)
					}
				}
			}
			jitter = time.Duration(rand.Intn(10)) * time.Second
			timer.Reset(60*time.Second + jitter)
		case <-e.stopCtx.Done():
			return
		}
	}
}

// runIdempotencySweep is a daily background sweep that purges idempotency
// receipts (0x19 Pebble prefix) older than 30 days. It runs immediately on
// startup (to clean up existing accumulation) and then every 24 hours.
func (e *Engine) runIdempotencySweep() {
	defer close(e.idempotencySweepDone)
	const retention = 30 * 24 * time.Hour
	sweep := func() {
		n, err := e.store.PurgeExpiredIdempotency(e.stopCtx, retention)
		if err != nil && e.stopCtx.Err() == nil {
			slog.Warn("engine: idempotency sweep error", "err", err)
			return
		}
		if n > 0 {
			slog.Info("engine: idempotency sweep purged entries", "count", n, "retention_days", 30)
		}
	}
	// Run immediately so stale receipts from before this deploy are cleaned up.
	sweep()
	ticker := time.NewTicker(24 * time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			sweep()
		case <-e.stopCtx.Done():
			return
		}
	}
}

// GetNoveltyDrops returns the total number of novelty jobs dropped because the channel was full.
func (e *Engine) GetNoveltyDrops() int64 {
	return e.noveltyJobsDropped.Load()
}

// runNoveltyWorker drains the noveltyJobs channel, performing O(N) Jaccard similarity
// scans and REFINES association writes entirely off the synchronous write hot path.
func (e *Engine) runNoveltyWorker() {
	defer close(e.noveltyDone)
	for {
		select {
		case <-e.stopCtx.Done():
			return
		case job, ok := <-e.noveltyJobs:
			if !ok {
				return
			}
			m := e.noveltyDet.Check(job.vaultID, job.id.String(), job.concept, job.content)
			if m == nil {
				continue
			}
			targetID, err := storage.ParseULID(m.ExistingULID)
			if err != nil {
				continue
			}
			refinesAssoc := &storage.Association{
				TargetID:   targetID,
				RelType:    storage.RelRefines,
				Weight:     float32(m.Similarity),
				Confidence: 1.0,
				CreatedAt:  time.Now(),
			}
			_ = e.store.WriteAssociation(e.stopCtx, job.wsPrefix, job.id, targetID, refinesAssoc)
			if e.coherence != nil {
				e.coherence.GetOrCreate(job.vaultName).RecordLinkCreated(true, true)
			}
		}
	}
}

// sourceTypeString converts a provenance.SourceType to its string representation
// for inclusion in activation responses.
func sourceTypeString(st provenance.SourceType) string {
	switch st {
	case provenance.SourceHuman:
		return "human"
	case provenance.SourceLLM:
		return "llm"
	case provenance.SourceDocument:
		return "document"
	case provenance.SourceInferred:
		return "inferred"
	case provenance.SourceExternal:
		return "external"
	case provenance.SourceWorkingMem:
		return "working_memory"
	case provenance.SourceSynthetic:
		return "synthetic"
	default:
		return ""
	}
}

// GetProvenance returns the ordered provenance log for an engram by ID.
func (e *Engine) GetProvenance(ctx context.Context, vault, id string) ([]provenance.ProvenanceEntry, error) {
	wsPrefix := e.store.ResolveVaultPrefix(vault)
	ulid, err := storage.ParseULID(id)
	if err != nil {
		return nil, fmt.Errorf("parse id: %w", err)
	}
	return e.prov.Get(ctx, wsPrefix, [16]byte(ulid))
}

// RecordFeedback records an explicit feedback signal for an engram.
// useful=false signals negative feedback (retrieved but not helpful);
// useful=true signals positive feedback (retrieved and helpful).
func (e *Engine) RecordFeedback(ctx context.Context, vault, engramID string, useful bool) error {
	wsPrefix := e.store.ResolveVaultPrefix(vault)
	ulid, err := storage.ParseULID(engramID)
	if err != nil {
		return fmt.Errorf("parse id: %w", err)
	}
	signal := scoring.FeedbackSignal{
		EngramID:    [16]byte(ulid),
		Accessed:    useful,
		ScoreVector: scoring.DefaultWeights(),
		Timestamp:   time.Now(),
	}
	go e.scoring.RecordFeedback(ctx, wsPrefix, signal)
	return nil
}

