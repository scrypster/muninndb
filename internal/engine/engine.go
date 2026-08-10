package engine

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/fnv"
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
	embedpkg "github.com/scrypster/muninndb/internal/plugin/embed"
	"github.com/scrypster/muninndb/internal/provenance"
	"github.com/scrypster/muninndb/internal/scoring"
	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// dedupReinforceCap is the minimum interval between content-hash dedup
// reinforcements (#682) for a single engram. It applies ONLY to the
// content-hash/dedup "re-experienced" channel (Write and WriteBatch), never to
// explicit read-by-id (engine.Read) — five explicit reads of the same engram
// must still land AccessCount at 5. Without this cap, re-submitting identical
// content in a tight loop (e.g. a flaky retry) would runaway-inflate
// AccessCount in a way no human/agent "access" actually occurred.
const dedupReinforceCap = 24 * time.Hour

// CognitiveForwarder is implemented by ClusterCoordinator on Lobe nodes.
// Using an interface avoids an import cycle between engine and replication.
type CognitiveForwarder interface {
	ForwardCognitiveEffects(effect mbp.CognitiveSideEffect)
}

// Engine is the cognitive database engine implementing mbp.EngineAPI.
type Engine struct {
	store       *storage.PebbleStore
	authStore   *auth.Store // nil = use Plasticity defaults (e.g. in tests)
	fts         *fts.Index
	ftsWorker   *fts.Worker // async FTS indexing — decoupled from write hot path
	activation  *activation.ActivationEngine
	triggers    *trigger.TriggerSystem
	engramCount atomic.Int64
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
	cogMu            sync.RWMutex
	hebbianWorker    *cognitive.HebbianWorker
	contradictWorker *cognitive.Worker[cognitive.ContradictItem]
	confidenceWorker *cognitive.Worker[cognitive.ConfidenceUpdate]
	transitionWorker *cognitive.TransitionWorker
	activity         *cognitive.ActivityTracker
	embedder         activation.Embedder // optional embedder for embedding-based brief scoring
	// embedModelName is the resolved model identifier for embedder (COG-26,
	// see EngineConfig.EmbedModelName). Empty = unknown → identity transform.
	embedModelName string
	// warnedUnknownEmbedModels dedupes the COG-26 "no calibration on record"
	// WARN so a busy vault doesn't spam logs once per query.
	warnedUnknownEmbedModels sync.Map // model string -> struct{}{}
	// Feature subsystems (all optional, nil-safe)
	autoAssoc            *autoassoc.Worker         // write-time automatic tag-based associations
	neighborWorker       *autoassoc.NeighborWorker // semantic neighbor auto-linking
	goalLinkWorker       *autoassoc.GoalLinkWorker // goal-aware semantic auto-linking
	noveltyDet           *novelty.Detector         // write-time near-duplicate detection
	noveltyJobs          chan noveltyJob           // async novelty work queue
	noveltyDone          chan struct{}             // signals novelty worker shutdown
	pruneDone            chan struct{}             // signals prune worker shutdown
	idempotencySweepDone chan struct{}             // signals idempotency sweep worker shutdown
	archiveGCDone        chan struct{}             // signals archive GC worker shutdown
	evolveRepairDone     chan struct{}             // signals evolve entity-link repair completion
	evolveRepairDelay    time.Duration             // startup delay before the repair pass
	// assocWeightRepairDone is closed ONLY when the one-shot pre-fix
	// full-weight association-key repair (#756) completes cleanly.
	// runPruneWorker gates assoc decay on it — decay destroys the repair's
	// disambiguator, so a failed pass parks decay instead of opening the gate.
	assocWeightRepairDone chan struct{}
	// assocWeightRepairExited is closed unconditionally when that goroutine
	// returns — cleanly, in error, on panic, or on shutdown. Stop() waits on
	// THIS one, so a parked gate can never hang shutdown or leak the goroutine.
	assocWeightRepairExited chan struct{}
	assocWeightRepairDelay  time.Duration // startup delay before that pass
	// decayAssocWeightsFn, when non-nil, replaces the store call decayAllVaults
	// makes. Test-only seam: it is what lets a unit test prove the repair gate
	// actually suppresses decay calls rather than merely existing in the source.
	decayAssocWeightsFn func(ctx context.Context, ws [8]byte, halfLife time.Duration, minWeight float32, archiveThreshold float64) (int, error)
	// assocDecayWarned dedupes the per-vault association-decay config WARNs so
	// a ~60s sweep cannot turn a one-line diagnostic into a log flood.
	// vault name (string) -> struct{}.
	assocDecayWarned sync.Map
	// assocDecayLegacyChecked dedupes the legacy-factor CHECK (a per-vault
	// GetVaultConfig read), separately from the WARN itself: vaults that never
	// need the WARN must not pay a config read every ~60s sweep pass.
	// vault name (string) -> struct{}.
	assocDecayLegacyChecked sync.Map
	// repairAssocWeightsFn, when non-nil, replaces the per-vault store call the
	// repair pass makes. Test-only seam, and the only way to exercise the
	// failure policy: a real forced storage error is not reachable from a test
	// without closing the store out from under the engine.
	repairAssocWeightsFn func(ctx context.Context, ws [8]byte) (int, error)
	coherence            *coherence.Registry // per-vault incremental coherence counters
	scoring              *scoring.Store      // per-vault learnable scoring weights
	prov                 *provenance.Store   // audit trail per-engram

	// Fix 5: coherence persistence lifecycle
	coherenceFlushStop chan struct{}
	coherenceFlushDone chan struct{}

	// coordinator forwards cognitive side effects to the Cortex on Lobe nodes.
	// nil on standalone / Cortex nodes (workers handle effects locally).
	coordinator   CognitiveForwarder
	writeGate     WriteGate // nil outside cluster mode; see single_writer.go (#596)
	coordinatorID string    // this node's ID, used as OriginNodeID in CognitiveSideEffect

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

	// replayEnrichTimeout, when positive, caps each per-engram LLM enrichment
	// call during ReplayEnrichment. Decouples the per-request timeout from the
	// MCP request context deadline. Set via SetReplayEnrichTimeout.
	// Useful for Ollama cold-start scenarios (MUNINN_ENRICH_TIMEOUT env var).
	replayEnrichTimeout time.Duration

	// replayFailMu guards replayFailCounts for atomic read-modify-write.
	// A plain mutex + map is used instead of sync.Map to avoid the TOCTOU
	// race inherent in Load-then-Store on sync.Map.
	replayFailMu sync.Mutex
	// replayFailCounts tracks how many times each engram has consecutively
	// failed enrichment during replay calls in this server session.
	// After maxReplayFails failures, the engram is skipped by subsequent
	// ReplayEnrichment calls. Keys are storage.ULID; values are int.
	// Resets on server restart. Cleared per-engram by ResetReplayFailCount.
	replayFailCounts map[storage.ULID]int

	// contradictionsDeclared is the per-vault ([8]byte ws prefix -> struct{})
	// in-process record that THIS process has written a RelContradicts edge in
	// that vault. It closes COG-29's fast-path gate over the window between an
	// explicit muninn_link(contradicts) and the batch detector's 0x0A marker,
	// so a single-binary deployment honors a declaration on the very next
	// query instead of up to one worker interval later. Resets on restart —
	// by then the marker exists.
	contradictionsDeclared sync.Map

	// contradictionProbeClean memoises the once-per-process declared-edge scan
	// in vaultMayHaveContradictions ([8]byte ws prefix -> struct{}): presence
	// means "this vault was scanned to completion and had NO declared
	// contradicts edge". It closes the hole where a process restarts between
	// muninn_link(contradicts) and the batch worker's 0x0A flush — the marker
	// was never written and the in-process flag is gone, so without a re-probe
	// recall would silently stop honoring the declaration forever. Cleared by
	// noteContradictionDeclared.
	contradictionProbeClean sync.Map

	// declaredScanCache memoises the COG-29 debt readout's declared-edge scan
	// per vault ([8]byte ws prefix -> *declaredScanCacheEntry), validated
	// against the store's RelContradicts write counter. ONLY the scan is
	// cached; resolution is re-derived on every call. See
	// declaredContradictionsCached for why that split is load-bearing.
	declaredScanCache sync.Map
	// declaredScanRuns counts actual executions of that scan, so the gate and
	// the cache — both of which change only I/O — can be pinned by a test.
	declaredScanRuns atomic.Int64
	// replicaProbe, when non-nil, reports whether this node is POSITIVELY a
	// cluster follower (Lobe/Observer). The debt scan cache is bypassed on a
	// follower because its invalidation signal — the store's RelContradicts
	// write counter — never moves for replicated writes: replication.Applier
	// commits raw Pebble batches below the store (#869 is the same class), so
	// a follower's cache would serve a stale scan forever while the leader
	// declares. nil (standalone) and false (leader / RoleUnknown) keep the
	// cache; the asymmetry matches replication.LocalAppendFunc's fail-open
	// reasoning. Set once at server wiring, before any traffic.
	replicaProbe func() bool

	// mergeMu serialises concurrent MergeEntity calls that touch the same entities.
	// Uses a dedicated stripe array separate from the storage-layer entity locks to
	// avoid reentrancy deadlock (UpsertEntityRecord acquires storage stripes internally).
	// See merge_guard.go for the full concurrency contract.
	mergeMu mergeGuard

	// noveltyJobsDropped counts novelty jobs silently dropped because the channel was full.
	noveltyJobsDropped atomic.Int64

	// queryCounter generates fast query IDs without crypto/rand syscall overhead.
	queryCounter atomic.Uint64
	// stopOnce ensures Stop() is idempotent even if called multiple times.
	stopOnce sync.Once

	// Vault lifecycle fields
	vaultOpsMu sync.Mutex        // guards name reservation in StartClone/StartMerge
	jobManager *vaultjob.Manager // tracks async clone/merge jobs
	stopCtx    context.Context   // cancelled on Stop() to signal goroutines
	stopCancel context.CancelFunc

	// Goroutine lifecycle tracking — see spawnFireAndForget and spawnJob.
	// Per-request fire-and-forget goroutines (Read, RecordAccess).
	fireAndForgetWG      sync.WaitGroup
	fireAndForgetStopped atomic.Bool
	// Long-running vault job goroutines (clone, merge, reembed, import).
	jobWG      sync.WaitGroup
	jobStopped atomic.Bool
	// Synchronous vault-operation entrypoints (clone/import/export setup, vault
	// maintenance, graph export, pruning) can still touch Pebble before they hand
	// work off to a tracked background goroutine, or without spawning one at all.
	// Fence and drain them separately so Stop() does not return while one of
	// these direct Pebble-touching paths is still racing DB teardown.
	vaultOpMu      sync.RWMutex
	vaultOpWG      sync.WaitGroup
	vaultOpStopped atomic.Bool

	hnswRegistry *hnsw.Registry // per-vault HNSW indexes (shared with activation)

	// vaultMu provides per-vault mutual exclusion for destructive vault operations
	// (PruneVault, ReindexFTSVault, ClearVault). Maps string vault name → *sync.Mutex.
	vaultMu sync.Map

	// NOTE: childMu grows by one entry per unique parent ULID ever passed to
	// AddChild. This is bounded by the number of distinct tree nodes in the
	// database — it cannot grow faster than the corpus itself — so no eviction
	// is needed.
	childMu sync.Map

	// contentHashLocks serialises the GetContentHash → WriteEngram → PutContentHash
	// sequence per (vault, content-hash) stripe, preventing TOCTOU duplicates under
	// concurrent REST writes. Uses contentHashStripes FNV-32a stripes for constant memory overhead.
	contentHashLocks [contentHashStripes]sync.Mutex

	// upsertKeyLocks serialises the GetUpsertKey → dispatch → write sequence per
	// (vault, upsert-key) stripe, preventing two concurrent upserts on the same
	// idempotent_id from both observing a miss and creating two engrams. Mirrors
	// contentHashLocks; the storage layer trusts the caller holds this lock.
	upsertKeyLocks [upsertKeyStripes]sync.Mutex

	// idempotencyLocks provides per-op_id mutexes to prevent TOCTOU races in the
	// idempotency check → write → store-receipt window. Mirrors the pattern used by
	// MCPServer.idempotencyLocks but lives on the engine so gRPC and REST callers
	// get the same protection.
	idempotencyLocks sync.Map

	// dedupReinforceTimes tracks, per engram ID, the last time the content-hash
	// dedup channel (#682) reinforced it via TouchAccess — keyed separately
	// from Engram.LastAccess because LastAccess is set to "now" by the ORIGINAL
	// write too, so gating on LastAccess directly would make the very first
	// duplicate-content submission (the common case: a retry moments after the
	// original write) look like it's already "within the cap window" and
	// wrongly skip reinforcement. This map remembers only dedup-channel
	// touches, so the 1/day cap (dedupReinforceCap) applies to repeated
	// dedup hits, not to the first one. In-memory/process-local — a soft
	// anti-runaway cap, not a security invariant — same pattern as
	// idempotencyLocks above; bounded by the number of distinct engrams that
	// have ever received a duplicate-content write.
	dedupReinforceTimes sync.Map
}

const contentHashStripes = 256

// upsertKeyStripes is the stripe count for upsertKeyLocks (mirrors contentHashStripes).
const upsertKeyStripes = 256

// getIdempotencyLock returns (or lazily creates) a per-op_id mutex. Prevents TOCTOU
// races in the check → write → store-receipt window for concurrent calls sharing an op_id.
func (e *Engine) getIdempotencyLock(opID string) *sync.Mutex {
	v, _ := e.idempotencyLocks.LoadOrStore(opID, &sync.Mutex{})
	return v.(*sync.Mutex)
}

// shouldDedupReinforce reports whether id is due for a content-hash dedup
// reinforcement (#682). This is a SOFT rate cap, not a strict mutex: the
// first-ever hit for an id is claimed atomically (LoadOrStore), but the
// cap-window-elapsed path below is check-then-Store, so two callers that race
// past the window for the same id can both reinforce once. That is acceptable —
// the cap only bounds runaway reinforcement from repeated duplicate writes to
// roughly one per window; an occasional extra bump under a rare exact race does
// not distort decay. If it ever needs to be strict, switch the elapsed path to
// CompareAndSwap on a comparable stored value.
func (e *Engine) shouldDedupReinforce(id storage.ULID) bool {
	now := time.Now()
	v, loaded := e.dedupReinforceTimes.LoadOrStore(id, now)
	if !loaded {
		return true // first time this id has hit the dedup channel
	}
	last := v.(time.Time)
	if now.Sub(last) < dedupReinforceCap {
		return false
	}
	// Cap window elapsed — claim it for this call (soft: see doc comment).
	e.dedupReinforceTimes.Store(id, now)
	return true
}

// contentHashLock returns the stripe mutex for the given (vault prefix, content hash) pair.
// Uses FNV-32a to spread locks across contentHashStripes stripes.
func (e *Engine) contentHashLock(wsPrefix [8]byte, hash [32]byte) *sync.Mutex {
	h := fnv.New32a()
	h.Write(wsPrefix[:])
	h.Write(hash[:])
	return &e.contentHashLocks[h.Sum32()%contentHashStripes]
}

// upsertKeyLock returns the stripe mutex for the given (vault prefix, upsert-key
// hash) pair. Mirrors contentHashLock — FNV-32a over wsPrefix + sha256(key).
func (e *Engine) upsertKeyLock(wsPrefix [8]byte, keyHash [32]byte) *sync.Mutex {
	h := fnv.New32a()
	h.Write(wsPrefix[:])
	h.Write(keyHash[:])
	return &e.upsertKeyLocks[h.Sum32()%upsertKeyStripes]
}

// ctxKeySkipContentDedup marks a Write as identity-addressed rather than
// content-addressed. Only Engine.upsertCreate sets it (#556).
//
// Upsert identity is the caller's key, NOT the content. Two distinct upsert
// keys whose documents happen to carry byte-identical text are TWO documents,
// and collapsing them onto one engram makes them share fate: when one key's
// document later changes, the shared engram is superseded and soft-deleted out
// from under the other key, which loses its content from default recall until
// its own next sync and then comes back under a NEW ULID — destroying the
// stable identity the feature exists to provide. It also stamps a FALSE
// lineage claim, that document A was superseded by document B's revision.
// Duplicate text across chunks (boilerplate, licence blocks, repeated
// headings) is ordinary in the re-ingest corpora upsert targets, so this is a
// routine case, not a corner one.
type ctxKeySkipContentDedup struct{}

func withSkipContentDedup(ctx context.Context) context.Context {
	return context.WithValue(ctx, ctxKeySkipContentDedup{}, true)
}

func skipContentDedup(ctx context.Context) bool {
	v, _ := ctx.Value(ctxKeySkipContentDedup{}).(bool)
	return v
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

// ReevaluatePushOnEmbed re-evaluates push subscriptions for an engram whose
// embedding just finished computing asynchronously (#512). It resolves the
// engram's vault and forwards to the trigger system, which pushes the engram to
// any subscription it now matches semantically but was not already delivered to
// at write time. Wired to the embed retroactive processor's OnEmbed callback.
func (e *Engine) ReevaluatePushOnEmbed(eng *storage.Engram, vec []float32) {
	if e.triggers == nil || eng == nil || len(vec) == 0 {
		return
	}
	ws, ok := e.store.FindVaultPrefix(eng.ID)
	if !ok {
		return
	}
	e.triggers.NotifyEmbed(wsVaultID(ws), eng, vec)
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

// EmbedStats returns the current stats for the first embed retroactive processor.
// Returns a zero-value RetroactiveStats when no embed processor is registered.
func (e *Engine) EmbedStats() plugin.RetroactiveStats {
	for _, p := range e.retroProcessors {
		if p != nil && p.Mode() == "embed" {
			return p.Stats()
		}
	}
	return plugin.RetroactiveStats{}
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
func NewEngine(cfg EngineConfig) *Engine {
	store := cfg.Store
	stopCtx, stopCancel := context.WithCancel(context.Background())
	e := &Engine{
		store:            store,
		authStore:        cfg.AuthStore,
		fts:              cfg.FTSIndex,
		activation:       cfg.ActivationEngine,
		triggers:         cfg.TriggerSystem,
		hebbianWorker:    cfg.HebbianWorker,
		contradictWorker: cfg.ContradictWorker,
		confidenceWorker: cfg.ConfidenceWorker,
		activity:         cognitive.NewActivityTracker(),
		embedder:         cfg.Embedder,
		embedModelName:   cfg.EmbedModelName,
		autoAssoc:        autoassoc.New(stopCtx, store, cfg.FTSIndex),
		neighborWorker:   autoassoc.NewNeighborWorker(stopCtx, store, cfg.HNSWRegistry),
		goalLinkWorker:   autoassoc.NewGoalLinkWorker(stopCtx, store, cfg.HNSWRegistry),
		noveltyDet:       novelty.New(),
		noveltyJobs:      make(chan noveltyJob, 256),
		noveltyDone:      make(chan struct{}),
		pruneDone:        make(chan struct{}),
		coherence:        coherence.NewRegistry(),
		scoring:          store.ScoringStore(),
		prov:             store.ProvenanceStore(),
		stopCtx:          stopCtx,
		stopCancel:       stopCancel,
		hnswRegistry:     cfg.HNSWRegistry,
		jobManager:       vaultjob.NewManager(),
		replayFailCounts: make(map[storage.ULID]int),
	}
	// Start async novelty worker to decouple O(N) Jaccard scan from write hot path.
	// engine:spawn-ok — tracked by noveltyDone channel, drained in Stop()
	go e.runNoveltyWorker()
	// Start async FTS worker to decouple indexing from the write hot path.
	// The engram is already durable in Pebble before this worker runs —
	// it only controls keyword search visibility (eventual, ~100ms lag).
	if cfg.FTSIndex != nil {
		e.ftsWorker = fts.NewWorker(cfg.FTSIndex)
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
			e.triggers.NotifyCognitive(vaultID, ws, storage.ULID(id), field, float32(old), float32(new))
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
		// engine:spawn-ok — tracked by coherenceFlushDone channel, drained in Stop()
		go e.runCoherenceFlush()
	}

	// Start periodic vault pruning sweep (runs every 60s with ±5s jitter).
	// Only active for vaults with MaxEngrams > 0 or RetentionDays > 0.
	// engine:spawn-ok — tracked by pruneDone channel, drained in Stop()
	go e.runPruneWorker()

	// Start daily idempotency receipt sweep — purges Pebble 0x19 entries
	// older than 30 days to prevent unbounded disk growth.
	e.idempotencySweepDone = make(chan struct{})
	// engine:spawn-ok — tracked by idempotencySweepDone channel, drained in Stop()
	go e.runIdempotencySweep()

	// Start weekly archive GC sweep — true-prunes 0x25 edges that have been
	// dormant for 3+ years, had low peak weight, and were never restored.
	e.archiveGCDone = make(chan struct{})
	// engine:spawn-ok — tracked by archiveGCDone channel, drained in Stop()
	go e.runArchiveGCWorker()

	// One-shot startup repair (after a delay) for supersede-successors
	// stripped of their entity links by the pre-fix Evolve path (#622).
	e.evolveRepairDelay = defaultEvolveRepairDelay()
	if cfg.EvolveRepairDelay != nil {
		e.evolveRepairDelay = *cfg.EvolveRepairDelay
	}
	e.evolveRepairDone = make(chan struct{})
	// engine:spawn-ok — tracked by evolveRepairDone channel, drained in Stop()
	go e.runEvolveEntityLinkRepair()

	// One-shot startup repair for pre-fix full-weight association keys, which
	// the overflowed WeightComplement wrote at the weight-0.0 position (#756).
	// Must precede the first assoc-decay pass; runPruneWorker gates on
	// assocWeightRepairDone to guarantee that, not merely hope for it.
	e.assocWeightRepairDelay = defaultAssocWeightRepairDelay()
	if cfg.AssocWeightRepairDelay != nil {
		e.assocWeightRepairDelay = *cfg.AssocWeightRepairDelay
	}
	e.assocWeightRepairDone = make(chan struct{})
	e.assocWeightRepairExited = make(chan struct{})
	// engine:spawn-ok — tracked by assocWeightRepairExited channel, drained in Stop()
	go e.runLegacyFullWeightAssocRepair()

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
	defer func() {
		if r := recover(); r != nil {
			if storage.IsClosedPanic(r) {
				return
			}
			slog.Error("engine: coherence flush panicked", "panic", r)
		}
	}()
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
		// Fence new goroutine spawns immediately. Must happen after stopCancel()
		// so goroutines see a cancelled context, and before wg.Wait() calls below.
		e.fireAndForgetStopped.Store(true)
		e.jobStopped.Store(true)
		e.vaultOpMu.Lock()
		e.vaultOpStopped.Store(true)
		e.vaultOpMu.Unlock()

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
		// Drain synchronous vault-operation setup paths before waiting on jobWG or
		// closing the job manager. These entrypoints can still create/fail jobs and
		// touch Pebble while Stop() is in progress.
		vaultOpDone := make(chan struct{})
		// engine:spawn-ok — local inline drain helper; closes vaultOpDone after vaultOpWG drains
		go func() { e.vaultOpWG.Wait(); close(vaultOpDone) }()
		select {
		case <-vaultOpDone:
		case <-time.After(30 * time.Second):
			slog.Warn("engine: vault operation setup paths did not drain within 30s")
		}
		// Drain job goroutines BEFORE closing the job manager. Job goroutines
		// call jobManager.Fail() / jobManager.Complete() — closing the manager
		// first would race with those calls.
		jobDone := make(chan struct{})
		// engine:spawn-ok — local inline drain helper; closes jobDone and exits immediately after WG drains
		go func() { e.jobWG.Wait(); close(jobDone) }()
		select {
		case <-jobDone:
		case <-time.After(30 * time.Second):
			slog.Warn("engine: job goroutines did not drain within 30s")
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
		// Wait for archive GC worker to exit.
		if e.archiveGCDone != nil {
			select {
			case <-e.archiveGCDone:
			case <-time.After(5 * time.Second):
				slog.Warn("engine: archive GC worker did not exit within 5s")
			}
		}

		// Wait for the evolve entity-link repair pass to exit.
		if e.evolveRepairDone != nil {
			select {
			case <-e.evolveRepairDone:
			case <-time.After(5 * time.Second):
				slog.Warn("engine: evolve entity-link repair did not exit within 5s")
			}
		}

		// Wait for the full-weight association repair pass to exit. This waits
		// on the EXITED channel, not the gate channel: the gate stays shut on a
		// non-clean pass, and shutdown must never depend on repair success.
		if e.assocWeightRepairExited != nil {
			select {
			case <-e.assocWeightRepairExited:
			case <-time.After(5 * time.Second):
				slog.Warn("engine: legacy full-weight association repair did not exit within 5s")
			}
		}

		// Drain fire-and-forget goroutines last — they write to Pebble via the
		// scoring store. Must complete before store.Close() (called by the caller
		// immediately after Stop() returns).
		ffDone := make(chan struct{})
		// engine:spawn-ok — local inline drain helper; closes ffDone and exits immediately after WG drains
		go func() { e.fireAndForgetWG.Wait(); close(ffDone) }()
		select {
		case <-ffDone:
		case <-time.After(5 * time.Second):
			slog.Warn("engine: fire-and-forget goroutines did not drain within 5s")
		}
	})
}

// spawnFireAndForget tracks fn in the engine's fire-and-forget WaitGroup and
// launches it as a goroutine. Returns false without spawning if the engine is
// shutting down. Callers may silently skip work on false — fire-and-forget
// tasks are best-effort by definition.
//
// Uses the add-before-check pattern: Add(1) happens before the stopped check,
// and Stop() sets stopped before calling Wait(). This eliminates the TOCTOU
// race in naive check-then-add WaitGroup usage.
//
// MUST be used instead of bare `go` for all per-request goroutines.
func (e *Engine) spawnFireAndForget(fn func()) bool {
	e.fireAndForgetWG.Add(1) // Add FIRST — visible to wg.Wait() in Stop()
	if e.fireAndForgetStopped.Load() {
		e.fireAndForgetWG.Done() // Undo — engine is shutting down
		return false
	}
	// engine:spawn-ok — tracked by fireAndForgetWG, drained in Stop() before store.Close()
	go func() {
		defer e.fireAndForgetWG.Done()
		fn()
	}()
	return true
}

// waitFireAndForgetIdle blocks until every fire-and-forget goroutine spawned
// so far (RecordFeedback's scoring/TouchAccess writes, Read's reinforcement
// signal) has finished. Test-only synchronization helper, mirroring
// autoassoc.Worker.WaitIdle: production callers never await this —
// fire-and-forget work is deliberately unawaited on the request path.
// Safe to call only when the caller knows no *other* concurrent request on
// this Engine is still spawning fire-and-forget work, since the WaitGroup is
// shared across all callers of spawnFireAndForget.
func (e *Engine) waitFireAndForgetIdle() {
	e.fireAndForgetWG.Wait()
}

// spawnJob tracks fn in the engine's job WaitGroup and launches it as a
// goroutine. Returns false without spawning if the engine is shutting down.
// Callers MUST fail the associated job immediately when false is returned.
//
// MUST be used instead of bare `go` for all vault job goroutines.
func (e *Engine) spawnJob(fn func()) bool {
	e.jobWG.Add(1) // Add FIRST — visible to wg.Wait() in Stop()
	if e.jobStopped.Load() {
		e.jobWG.Done() // Undo — engine is shutting down
		return false
	}
	// engine:spawn-ok — tracked by jobWG, drained in Stop() before jobManager.Close()
	go func() {
		defer e.jobWG.Done()
		fn()
	}()
	return true
}

// waitWriteTimeIdle blocks until every write-time async worker has finished
// the work enqueued so far: the three association workers (autoAssoc,
// neighborWorker, goalLinkWorker) AND the FTS indexer (ftsWorker), all of
// which run off the Write() hot path (see the Intend/Write enqueue sites) so
// normal callers never await them — recall tolerates their eventual
// consistency by design. It also drains the activation engine's async log
// drainer (see below) — not a write-time worker, but folded in here so the
// harness has a single drain call to make between scripted steps.
//
// Test-only synchronization helper: the prospective acceptance harness arms
// intentions via Intend() (which calls Write()) and then immediately runs
// scripted Activate() calls that depend both on associations these workers
// create (RelSupports for the BFS pool) AND on the intention being FTS-indexed
// (the only recall path with the harness's noop zero-vector embeddings). Async
// FTS indexing is decoupled from the write hot path (ftsWorker), so without
// flushing it a scripted call can run before its intention is searchable and
// the intention silently "does not fire" — the residual harness flake under
// -race/CPU contention. Draining all of it makes the harness observe steady
// state before asserting recall. Production scheduling/dispatch is unchanged;
// this only adds the ability to await it.
//
// activation.WaitLogIdle (drainLog/logCh): Run() submits each activation's
// result set to a buffered channel that a single goroutine drains into
// assocLog, which phase4HebbianBoost reads on the NEXT call to boost
// candidates associated with something recently activated. The drainer's
// eventual consistency ("~1ms lag, half-life 3600s — irrelevant", per its
// doc comment) is production-safe but breaks a scripted back-to-back
// harness: call N's log entry can still be in flight when call N+1 runs
// phase4HebbianBoost, so the same candidate nondeterministically scores with
// or without that boost — flipping which of two near-tied candidates ranks
// first (and, downstream, whether NoticesForRecall sees the corroborator or
// the intention's own engram at rank 0). Refs #722.
func (e *Engine) waitWriteTimeIdle() {
	e.WaitWriteTimeIdle()
}

// WaitWriteTimeIdle is the exported test seam over waitWriteTimeIdle, for
// tests OUTSIDE this package (e.g. internal/mcp) that seed through the real
// engine and must not poll wall-clock deadlines for async index visibility
// (#722 doctrine; the SetFlushInterval precedent). Production code has no
// reason to call it — recall is eventually consistent by design.
func (e *Engine) WaitWriteTimeIdle() {
	if e.autoAssoc != nil {
		e.autoAssoc.WaitIdle()
	}
	if e.neighborWorker != nil {
		e.neighborWorker.WaitIdle()
	}
	if e.goalLinkWorker != nil {
		e.goalLinkWorker.WaitIdle()
	}
	if e.ftsWorker != nil {
		// Best-effort in a test helper; the timeout is generous enough that it
		// never trips for the handful of intentions the harness arms.
		_ = e.ftsWorker.Flush(10 * time.Second)
	}
	if e.activation != nil {
		e.activation.WaitLogIdle()
	}
}

// resetActivationLogForVault clears the activation engine's recorded recent-
// activations for vault (see activation.ActivationLog.ResetVault). Test-only:
// callers MUST have already called waitWriteTimeIdle (or otherwise know no
// Activate() on this vault has an entry still in flight to the log drainer),
// or the reset can be immediately undone by a late-arriving drain.
func (e *Engine) resetActivationLogForVault(vault string) {
	if e.activation == nil {
		return
	}
	ws := e.store.ResolveVaultPrefix(vault)
	e.activation.ResetLog(wsVaultID(ws))
}

// beginVaultOp tracks synchronous vault-operation setup work that can still
// touch Pebble before handing off to a tracked background job. Returns false if
// the engine is shutting down and the caller should fail fast.
func (e *Engine) beginVaultOp() bool {
	e.vaultOpMu.RLock()
	if e.vaultOpStopped.Load() || (e.stopCtx != nil && e.stopCtx.Err() != nil) {
		e.vaultOpMu.RUnlock()
		return false
	}
	e.vaultOpWG.Add(1)
	e.vaultOpMu.RUnlock()
	return true
}

func (e *Engine) endVaultOp() {
	e.vaultOpWG.Done()
}

// vaultOpContext returns a context that is cancelled when either the caller's
// context is done or the engine begins shutting down. Long-running synchronous
// vault operations should use this so Stop() can actively interrupt them rather
// than only waiting for the fixed vaultOpWG drain timeout.
func (e *Engine) vaultOpContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx == nil {
		if e.stopCtx != nil {
			return e.stopCtx, func() {}
		}
		return context.Background(), func() {}
	}
	if e.stopCtx == nil || ctx == e.stopCtx {
		return ctx, func() {}
	}

	opCtx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(e.stopCtx, cancel)
	return opCtx, func() {
		stop()
		cancel()
	}
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

// Store returns the underlying PebbleStore.
// Prefer the typed engine methods (GetEngram, UpdateTags, CheckIdempotency, WriteIdempotency)
// over direct Store() access. Store() is retained for HNSW plugin wiring that
// requires the raw store adapter.
func (e *Engine) Store() *storage.PebbleStore {
	return e.store
}

// GetEngram fetches a single engram by vault and ULID.
func (e *Engine) GetEngram(ctx context.Context, vault string, id storage.ULID) (*storage.Engram, error) {
	wsPrefix := e.store.ResolveVaultPrefix(vault)
	return e.store.GetEngram(ctx, wsPrefix, id)
}

// GetVaultEmbedDim returns the embedding vector dimension currently in use by vault.
// Derived from the HNSW index — returns 0 if no embeddings have been stored yet.
func (e *Engine) GetVaultEmbedDim(_ context.Context, vault string) int {
	ws := e.store.ResolveVaultPrefix(vault)
	return e.hnswRegistry.VaultEmbedDim(ws)
}

// validateClientEmbeddingDim refuses a caller-supplied embedding whose
// dimension differs from the vault's established dimension (issue #582),
// BEFORE anything is persisted — the MCP layer already enforces this with
// -32602; checking here extends the same contract to every other transport
// (MBP, gRPC, SDKs, embedded library). The dimension peek never loads a
// graph: cached index first, one Pebble seek otherwise. Registry.Insert
// remains the atomic authority; this check exists so a refused vector is
// never persisted first.
func (e *Engine) validateClientEmbeddingDim(wsPrefix [8]byte, vec []float32) error {
	if len(vec) == 0 || e.hnswRegistry == nil {
		return nil
	}
	want := e.hnswRegistry.CachedVaultDim(wsPrefix)
	if want == 0 && e.store != nil {
		want, _ = e.store.VaultEmbedDimOnDisk(wsPrefix)
	}
	if err := hnsw.CheckDim(want, len(vec)); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidRequest, err)
	}
	return nil
}

// UpdateTags replaces the tags on an engram, keeping the FTS posting lists in
// sync with the new tag set.
//
// The reindex is not housekeeping — it is the difference between correct and
// silently-wrong recall. Tags are tokenized into the BM25 posting lists under
// FieldTags (fts.Index.IndexEngram), while store.UpdateTags only rewrites
// 0x02/0x03/0x0C/0x2C. Stale 0x0C/0x2C tag-index entries are harmless because
// activation.PassesMetaFilter re-checks tags_all/tags_any/tag_prefix against
// eng.Tags before a candidate survives; FTS has no such rescue, because
// ftsScore feeds the RRF/ACT-R blend directly. Without this, a retagged engram
// scores on a tag it no longer has and cannot score on the tag it just gained
// (#720, TestUpdateTags_ReindexesFTS).
//
// The reindex goes through fts.ReindexEngram, SYNCHRONOUSLY, and that is
// deliberate on both counts.
//
// One call, because delete-then-add is not statistics-neutral: fts.IndexEngram
// increments the per-term document frequency df_t for every term of the document
// (and bumps TotalEngrams) while fts.DeleteEngram decrements neither, so pairing
// them made every retag decay the engram's own score for a query on its own
// unchanged content — measured at 82.6% over 100 retags against a 10.0% drop for
// an unretagged control in the same vault. ReindexEngram moves df_t only for
// terms whose membership actually changed and never touches the global stats;
// see its doc comment for the arithmetic.
//
// Synchronously, because this is the one FTS path that DELETES before it adds.
// Every other ftsWorker.Submit site only ever adds postings, so a job dropped
// under queue pressure costs visibility of new content. Here a dropped job — a
// full queue, a worker stopped mid-shutdown, a crash in the ~100ms window —
// would leave the engram with NO postings at all, invisible to keyword search
// under its concept and content, not just its tags, until someone ran
// reindex-fts. That is strictly worse than the stale-tag bug the reindex
// replaces. The delete half was already synchronous and a retag is rare, so
// there is nothing to buy by keeping half of it on the worker.
//
// TRASH is de-indexed; HISTORY is reindexed. The drop is gated on
// activation.CarriesSupersessionSignature, not on State alone. Only a plain
// soft-delete — muninn_forget without not_true_since, which leaves ValidUntil
// OPEN — gets its postings dropped: that record is trash, Forget already
// de-indexed it at delete time, and re-adding it would put a discarded document
// back where it burns candidate seed slots.
//
// Everything else is reindexed, including a CLOSED-stamp soft-delete and an
// archived record. An earlier revision dropped postings for StateSoftDeleted/
// StateArchived unconditionally, reasoning that phase 6 filters both before
// returning candidates. That premise is false in exactly the case that matters:
// activation.PassesLifecycle ADMITS a soft-deleted engram carrying a closed
// ValidUntil under as_of or include_invalid — precisely the evolve/supersedes
// signature — and Evolve deliberately does NOT delete its predecessor's postings
// the way Forget does, because those postings are what a query phrased in the
// OLD wording still matches (COG-28 then resolves that evidence forward to the
// chain head). So retagging an evolve predecessor permanently destroyed the
// keyword path to a superseded fact that as_of is contracted to still reach,
// recoverable only by reindex-fts (#720 review, finding 1: one hit before the
// retag, zero after). Archived is reindexed for the adjacent reason — nothing
// de-indexes on archival, so dropping here would make a metadata edit the only
// thing that ever removes those postings, and archival is a storage-tier
// eviction rather than a statement about truth.
//
// The delete side must be keyed on the OLD document — read here BEFORE
// store.UpdateTags overwrites it, since the terms to remove are derived from the
// tags handed in. Errors are logged, never returned, matching Write/Evolve/
// Forget: the record is already durably retagged, so a failed index write must
// not turn a committed retag into a caller-visible failure.
func (e *Engine) UpdateTags(ctx context.Context, vault string, id storage.ULID, tags []string) error {
	if err := e.refuseWrite(ctx); err != nil {
		return err
	}
	wsPrefix := e.store.ResolveVaultPrefix(vault)

	// Serialize the WHOLE read-prev → write → reindex sequence under the same
	// per-engram stripe lock the storage write takes, via the unlocked inner
	// variant (casLocks is not reentrant). Storage being atomic is not enough:
	// the FTS delta is derived from the tag set read BEFORE the write, so two
	// concurrent retags of one engram could interleave read/write/reindex and
	// strand the loser's postings while double-decrementing df_t for the terms
	// both passes believed they removed — and because df_t is corpus-wide, that
	// moved an UNINVOLVED third engram's full-text score by 6.5%. Measured at 36
	// of 40 trials with no artificial delay (#720 review, finding 3). Two agents
	// retagging the same memory is ordinary cooperative behaviour, not an
	// adversarial edge, so documenting the race was not sufficient.
	unlock := e.store.LockEngram(id)
	defer unlock()

	// Capture the OLD tags (and the text fields the postings were built from)
	// before the write; this read is what makes the FTS delete exact.
	//
	// The snapshot must be a COPY, not the returned pointer: GetEngram hands
	// back the L1 cache's live entry (DomainCache.Get does not clone), and
	// store.UpdateTags reassigns Tags on that very object — so holding the
	// pointer and reading prev.Tags after the write yields the NEW tags and
	// deletes the wrong postings, which looks like a fix while leaving the bug
	// in place.
	prev, prevErr := e.store.GetEngram(ctx, wsPrefix, id)
	var prevDoc fts.Document
	if prevErr == nil && prev != nil {
		prevDoc = fts.Document{
			Concept:   prev.Concept,
			CreatedBy: prev.CreatedBy,
			Content:   prev.Content,
			Tags:      append([]string(nil), prev.Tags...),
		}
	}

	if err := e.store.UpdateTagsLocked(ctx, wsPrefix, id, tags); err != nil {
		return err
	}

	if e.fts == nil {
		return nil
	}
	if prevErr != nil || prev == nil {
		slog.Warn("engine: update_tags: pre-read failed, FTS tag postings left stale until reindex-fts",
			"id", id.String(), "err", prevErr)
		return nil
	}

	// Authoritative post-write record. prev is a pre-write snapshot; the stripe
	// lock held above means no concurrent writer can have changed the lifecycle
	// under us, but store.UpdateTagsLocked re-caches the committed record so this
	// read is cheap and is the one that reflects the commit.
	cur := prev
	if committed, err := e.store.GetEngram(ctx, wsPrefix, id); err == nil && committed != nil {
		cur = committed
	}
	if cur.State == storage.StateSoftDeleted && !activation.CarriesSupersessionSignature(cur) {
		// Trash, not history: an open ValidUntil means muninn_forget threw this
		// record away rather than superseding it, and Forget already de-indexed
		// it. Drop whatever postings remain and write nothing back, so a retag
		// cannot put a discarded document into the candidate pool.
		if err := e.fts.DeleteEngram(wsPrefix, [16]byte(id),
			prevDoc.Concept, prevDoc.CreatedBy, prevDoc.Content, prevDoc.Tags); err != nil {
			slog.Warn("engine: update_tags: fts delete failed", "id", id.String(), "err", err)
		}
		return nil
	}

	// The text fields are unchanged by a retag; only Tags differ between the two
	// documents, which is exactly what makes ReindexEngram's DF bookkeeping a
	// no-op for every term the engram keeps.
	next := prevDoc
	next.Tags = tags
	if err := e.fts.ReindexEngram(wsPrefix, [16]byte(id), prevDoc, next); err != nil {
		slog.Warn("engine: update_tags: fts reindex failed", "id", id.String(), "err", err)
	}
	return nil
}

// CheckIdempotency looks up an op_id receipt. Returns nil, nil if not found.
func (e *Engine) CheckIdempotency(ctx context.Context, opID string) (*storage.IdempotencyReceipt, error) {
	return e.store.CheckIdempotency(ctx, opID)
}

// WriteIdempotency stores an idempotency receipt (op_id → engramID).
func (e *Engine) WriteIdempotency(ctx context.Context, opID, engramID string) error {
	if err := e.refuseNonLeaderWrite(); err != nil {
		return err
	}
	return e.store.WriteIdempotency(ctx, opID, engramID)
}

// CountEmbedded returns the count of engrams that have had embeddings generated
// (i.e. the DigestEmbed flag is set). Returns -1 on error.
//
// vault == "" counts across the whole store (the historical, instance-wide
// behavior). A non-empty vault scopes the count to that vault's own engrams
// (#802) — the 0x11 DigestFlags keyspace itself is deliberately global (see
// docs/internals/keyspace-registry.md), so the vault-scoped path is a
// different, more expensive query: scan the vault's engram IDs and look up
// each one's flags, rather than scanning DigestFlags directly.
func (e *Engine) CountEmbedded(ctx context.Context, vault string) int64 {
	const DigestEmbed uint16 = 0x02
	if vault == "" {
		count, err := e.store.CountWithFlag(ctx, DigestEmbed)
		if err != nil {
			return -1
		}
		return count
	}
	wsPrefix := e.store.ResolveVaultPrefix(vault)
	count, err := e.store.CountEmbeddedInVault(ctx, wsPrefix, DigestEmbed)
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

// resolveTrust validates a caller-supplied trust label and enforces the S8/D4
// credential gate: the elevated "verified" level (human-confirmed / admin-
// certified) may only be set by a write or full credential, never by an
// observe credential. An empty label defaults to TrustInferred — the level for
// all AI-generated content — preserving the pre-S8 behaviour for callers that
// do not pass trust. source_type is provenance-derived and never a write
// argument, so trust is the sole discriminator the write path exposes.
func resolveTrust(ctx context.Context, label string) (storage.TrustLevel, error) {
	if label == "" {
		return storage.TrustInferred, nil
	}
	level, err := storage.ParseTrustLevel(label)
	if err != nil {
		return 0, fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}
	if level == storage.TrustVerified {
		mode, _ := ctx.Value(auth.ContextMode).(string)
		if mode != auth.ModeFull && mode != auth.ModeWrite {
			return 0, fmt.Errorf("%w: trust=verified requires a write or full credential", ErrInvalidRequest)
		}
	}
	return level, nil
}

// importanceFromRequest resolves an optional caller-asserted importance into
// the stored representation: nil → 0 (unset — the use-time type-table default
// applies at read/decay/prune time). An explicit value is clamped to [0,1],
// and an explicit 0.0 (or negative) is quantized to 0.01 so the stored 0
// keeps meaning "unset" — behaviorally identical, one fewer flag byte.
func importanceFromRequest(p *float32) float32 {
	if p == nil {
		return 0
	}
	v := *p
	if v > 1 {
		v = 1
	}
	if v <= 0 {
		v = 0.01
	}
	return v
}

// Write implements mbp.EngineAPI.Write.
func (e *Engine) Write(ctx context.Context, req *mbp.WriteRequest) (*mbp.WriteResponse, error) {
	// Cluster single-writer gate (#596). Additive methods have no append gate to
	// hang this off, so they call it directly.
	if err := e.refuseNonLeaderWrite(); err != nil {
		return nil, err
	}
	writeStart := time.Now()
	wsPrefix := e.store.ResolveVaultPrefix(req.Vault)
	e.activity.Record(wsPrefix)

	// ── Upsert mode: key-pinned create-or-evolve (#556) ─────────────────
	// When upsert_mode is set, the durable 0x30 forward index (keyed by
	// idempotent_id) decides create-vs-merge — not the content-hash dedup below.
	// The whole path returns from writeUpsert, which delegates to the default
	// create path or to EvolveAt (which re-points the index in its own batch).
	if req.UpsertMode {
		return e.writeUpsert(ctx, wsPrefix, req)
	}

	// ── Idempotency check: op_id dedup (must run before content-hash dedup) ──
	// If the caller set idempotent_id, return the existing engram ID immediately.
	// This runs first so that a re-submission with changed content still returns
	// the original — correct idempotency semantics regardless of content drift.
	if req.IdempotentID != "" {
		mu := e.getIdempotencyLock(req.IdempotentID)
		mu.Lock()
		defer mu.Unlock()
		if receipt, err := e.store.CheckIdempotency(ctx, req.IdempotentID); err == nil && receipt != nil {
			return &mbp.WriteResponse{ID: receipt.EngramID, Hint: "idempotent"}, nil
		}
	}

	// ── Content-hash dedup: O(1) exact-duplicate check ──────────────
	// Locked per (vault, content-hash) stripe to prevent TOCTOU duplicates under
	// concurrent REST writes: two goroutines with identical content must not both
	// pass GetContentHash before either calls PutContentHash.
	// unlockContentHash is idempotent — safe to call from any return path and
	// from defer, eliminating the risk of a lock leak on future code changes.
	contentHash := storage.ContentHash(req.Content)
	contentHashMu := e.contentHashLock(wsPrefix, contentHash)
	contentHashMu.Lock()
	var contentHashUnlocked bool
	unlockContentHash := func() {
		if !contentHashUnlocked {
			contentHashUnlocked = true
			contentHashMu.Unlock()
		}
	}
	defer unlockContentHash()
	if existingID, err := e.store.GetContentHash(ctx, wsPrefix, contentHash); err == nil && existingID != (storage.ULID{}) && !skipContentDedup(ctx) {
		// A mapping exists — verify the engram is still live (not soft-deleted)
		// AND still current on the valid-time axis. Re-remembering content
		// identical to an EXPIRED engram must NOT reinforce the expired record:
		// two same-content facts with disjoint validity windows are two facts,
		// so we fall through to write a new engram (PutContentHash below then
		// repoints the hash at the new, current one).
		if existingEng, err := e.store.GetEngram(ctx, wsPrefix, existingID); err == nil &&
			existingEng.State != storage.StateSoftDeleted && !existingEng.IsExpired(time.Now()) {
			// Reinforce: increment access count and update LastAccess to signal
			// that this content is being re-experienced. Release the stripe
			// lock before TouchAccess — the dedup decision is already made and
			// the per-engram reinforcement below has its own lock (#682).
			//
			// 1/day cap (dedup channel ONLY — explicit read-by-id is uncapped,
			// see engine.Read): re-submitting identical content in a tight loop
			// must not runaway-inflate AccessCount the way N distinct reads-by-id
			// legitimately do. shouldDedupReinforce (not LastAccess age) is the
			// gate — see its doc comment for why LastAccess alone can't tell "the
			// first duplicate right after the original write" apart from "already
			// reinforced recently".
			unlockContentHash()
			if e.shouldDedupReinforce(existingID) {
				_ = e.store.TouchAccess(ctx, wsPrefix, existingID)
			}
			return &mbp.WriteResponse{
				ID:        existingID.String(),
				CreatedAt: existingEng.CreatedAt.UnixNano(),
				Hint:      "duplicate_content",
			}, nil
		}
		// Engram was soft-deleted, expired, or not found — remove the stale hash
		// mapping and proceed to write a fresh engram.
		_ = e.store.DeleteContentHash(ctx, wsPrefix, contentHash)
	}

	// Resolve inline enrichment mode for this vault.
	vaultName := req.Vault
	if vaultName == "" {
		vaultName = "default"
	}
	resolved := e.ResolveVaultPlasticity(vaultName)
	inlineMode := resolved.InlineEnrichment

	// Determine which caller-provided enrichment fields to use based on mode.
	callerSummary := ""
	var callerEntities []mbp.InlineEntity
	var callerRelationships []mbp.InlineRelationship

	switch inlineMode {
	case "background_only":
		// background_only: background LLM runs and wins; ignore any caller-provided fields.
	default:
		// "caller_only", "caller_preferred", "disabled", and unknown modes:
		// always store non-empty caller-provided fields.
		callerSummary = req.Summary
		callerEntities = req.Entities
		callerRelationships = req.Relationships
	}

	// Resolve trust (S8): default inferred; "verified" gated to write/full creds.
	trust, trustErr := resolveTrust(ctx, req.Trust)
	if trustErr != nil {
		return nil, trustErr
	}

	// Build storage.Engram from request
	eng := &storage.Engram{
		Concept:    req.Concept,
		Content:    req.Content,
		Tags:       req.Tags,
		Confidence: req.Confidence,
		Stability:  req.Stability,
		Importance: importanceFromRequest(req.Importance),
		Embedding:  req.Embedding,
		MemoryType: storage.MemoryType(req.MemoryType),
		TypeLabel:  req.TypeLabel,
		Trust:      trust,
	}

	// Apply caller-provided summary directly to the engram.
	if callerSummary != "" {
		eng.Summary = callerSummary
	}

	// Set custom CreatedAt if provided; validate bounds to prevent provenance
	// falsification and ULID overflow.
	if req.CreatedAt != nil {
		if err := validateCreatedAt(*req.CreatedAt); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrInvalidRequest, err)
		}
		eng.CreatedAt = *req.CreatedAt
	}

	// Valid-time bounds (half-open [valid_from, valid_until)). ValidFrom
	// defaults to CreatedAt at decode time, so leaving it unset is free.
	if err := applyValidity(eng, req.ValidFrom, req.ValidUntil); err != nil {
		return nil, err
	}

	// Convert associations
	assocs := make([]storage.Association, 0, len(req.Associations)+len(callerRelationships))
	for _, a := range req.Associations {
		targetID, err := storage.ParseULID(a.TargetID)
		if err != nil {
			return nil, fmt.Errorf("%w: association target_id %q: %v", ErrInvalidID, a.TargetID, err)
		}
		assocs = append(assocs, storage.Association{
			TargetID:      targetID,
			RelType:       storage.RelType(a.RelType),
			Weight:        a.Weight,
			Confidence:    a.Confidence,
			CreatedAt:     time.Unix(0, a.CreatedAt),
			LastActivated: a.LastActivated,
		})
	}
	// relationships[] (mbp.WriteRequest.Relationships) is the OTHER inline
	// edge field on a write, and until #817 a dangling target on it was
	// refused differently from associations[] for the identical defect — a
	// target naming a hard-deleted engram. associations[] failed the whole
	// write (ErrDanglingEndpoint -> ErrInvalidID, 400) because it reaches
	// checkInlineAssocTargets atomically through eng.Associations;
	// relationships[] instead ran a POST-write loop of individual
	// WriteAssociation calls that logged a WARN on refusal and returned 200
	// regardless — silent for the MCP surface (muninn_link, muninn_remember)
	// that is the most common agent path (#817). Folding relationships into
	// this SAME eng.Associations slice, validated by the SAME atomic
	// pre-write guard, closes the asymmetry by construction instead of
	// duplicating the check or inventing a warnings field: a dangling
	// relationships[] target now fails the whole write exactly like a
	// dangling associations[] target, and a malformed target_id (previously
	// silently skipped) does too, for the same reason. See CHANGELOG.
	for _, rel := range callerRelationships {
		targetID, err := storage.ParseULID(rel.TargetID)
		if err != nil {
			return nil, fmt.Errorf("%w: relationship target_id %q: %v", ErrInvalidID, rel.TargetID, err)
		}
		assocs = append(assocs, storage.Association{
			TargetID:   targetID,
			RelType:    storage.RelType(relTypeFromString(rel.Relation)),
			Weight:     rel.Weight,
			Confidence: 1.0,
			CreatedAt:  time.Now(),
		})
	}
	eng.Associations = assocs

	// Refuse a mismatched caller-supplied embedding before anything is
	// persisted (issue #582).
	if err := e.validateClientEmbeddingDim(wsPrefix, req.Embedding); err != nil {
		return nil, err
	}

	// Write to store
	id, err := e.store.WriteEngram(ctx, wsPrefix, eng)
	if err != nil {
		// An inline association naming an engram that no longer exists is the
		// caller's mistake, not a storage fault — the same class as the
		// unparseable target_id refused a few lines above, and refused the same
		// way. Mapped to ErrInvalidID so the transports answer 400 rather than
		// 500 (STO-12).
		if errors.Is(err, storage.ErrDanglingEndpoint) {
			return nil, fmt.Errorf("%w: %v", ErrInvalidID, err)
		}
		return nil, fmt.Errorf("write engram: %w", err)
	}

	// Store content hash → engram ID mapping for future dedup lookups — unless
	// this is an identity-addressed upsert create (skipContentDedup), whose
	// identity is the caller's key, never the content. Populating the index
	// for it made the dedup bypass one-directional (#556 fix-round finding
	// 2): the LOOKUP above was suppressed for upsert creates, but leaving
	// this PutContentHash unconditional still re-pointed the shared
	// content-hash entry at the upsert-owned engram, so a later ORDINARY,
	// non-upsert Write of the same text would alias onto it via the default
	// dedup path — and then get soft-deleted out from under the plain
	// caller the next time the upsert key's document changed, with the
	// caller never told. Same fate-sharing harm the ctxKeySkipContentDedup
	// doc comment describes, just in the other direction. Consequence: an
	// upsert-created engram is now invisible to content dedup entirely (a
	// plain Write of byte-identical text mints its own engram rather than
	// reinforcing the upsert one) — correct, since an upsert engram's
	// identity is its key, not its bytes, so it was never a legitimate dedup
	// target for anything outside its own key's chain.
	if !skipContentDedup(ctx) {
		if err := e.store.PutContentHash(ctx, wsPrefix, contentHash, id); err != nil {
			slog.Warn("engine: failed to store content hash", "id", id.String(), "err", err)
		}
	}
	unlockContentHash() // release stripe lock — PutContentHash is done

	// Store idempotency receipt so future calls with the same op_id short-circuit.
	if req.IdempotentID != "" {
		if err := e.store.WriteIdempotency(ctx, req.IdempotentID, id.String()); err != nil {
			slog.Warn("engine: failed to store idempotency receipt", "op_id", req.IdempotentID, "id", id.String(), "err", err)
		}
	}

	// When the caller provided an embedding, insert into HNSW inline so the
	// vector is searchable immediately, then mark DigestEmbed so the
	// retroactive processor does not overwrite it. Insert first: if it is
	// refused (e.g. a dimension race lost against a concurrent first insert,
	// #582), the engram stays unflagged and the retroactive processor embeds
	// it with the server's own embedder instead — self-healing rather than a
	// stranded, never-searchable memory.
	if len(req.Embedding) > 0 {
		if err := e.hnswRegistry.Insert(ctx, wsPrefix, [16]byte(id), req.Embedding); err != nil {
			slog.Warn("engine: client embedding not inserted into HNSW; engram left for the retroactive processor", "id", id.String(), "err", err)
		} else {
			existing, _ := e.store.GetDigestFlags(ctx, plugin.ULID(id))
			if err := e.store.SetDigestFlag(ctx, id, existing|plugin.DigestEmbed); err != nil {
				slog.Warn("engine: failed to set DigestEmbed flag", "id", id.String(), "err", err)
			}
		}
	}

	// Store caller-provided inline entities in the entity table (not as KeyPoints).
	if len(callerEntities) > 0 {
		ws, _ := e.store.FindVaultPrefix(id)
		var linkedEntityNames []string
		for _, ent := range callerEntities {
			typ := strings.ToLower(strings.TrimSpace(ent.Type))
			if typ == "" {
				typ = "other"
			}
			record := storage.EntityRecord{
				Name:       ent.Name,
				Type:       typ,
				Confidence: 1.0,
			}
			if err := e.store.UpsertEntityRecord(ctx, ws, record, "inline"); err != nil {
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

	// callerRelationships were already converted into eng.Associations above
	// and written atomically with the engram itself, through the same
	// checkInlineAssocTargets guard associations[] uses (#817) — nothing left
	// to do here.

	// Store caller-provided entity-to-entity relationships in the 0x21 relationship index.
	wroteEntityRelationships := false
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
				continue
			}
			wroteEntityRelationships = true
		}
	}

	// Mark per-stage digest flags for inline (caller-supplied) enrichment so
	// GetEnrichmentCandidates does not report these memories as un-enriched (#500).
	// Mirror the existing DigestEntities-on-inline pattern: only set a stage's flag
	// when that stage's data is actually present from the caller.
	//   - summary supplied        -> DigestSummarized
	//   - entity relationships     -> DigestRelationships
	//   - type/classification      -> DigestClassified (req.TypeLabel is set whenever
	//                                 the caller supplies a type or type_label)
	var inlineStageFlags uint16
	if callerSummary != "" {
		inlineStageFlags |= plugin.DigestSummarized
	}
	if wroteEntityRelationships {
		inlineStageFlags |= plugin.DigestRelationships
	}
	if req.TypeLabel != "" {
		inlineStageFlags |= plugin.DigestClassified
	}
	if inlineStageFlags != 0 {
		existing, _ := e.store.GetDigestFlags(ctx, plugin.ULID(id))
		if flagErr := e.store.SetDigestFlag(ctx, id, existing|inlineStageFlags); flagErr != nil {
			slog.Warn("engine: failed to set inline enrichment stage flags", "id", id.String(), "flags", inlineStageFlags, "error", flagErr)
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
					e.triggers.NotifyContradiction(wsVaultID(wsPrefix), wsPrefix, storage.ULID(ev.EngramA), storage.ULID(ev.EngramB), ev.Severity, "relation_matrix")
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

// writeUpsert implements the upsert_mode write path (#556, Rev 2 redesign).
// The durable 0x30 forward index (keyed by sha256(idempotent_id)) maps to the
// CURRENT HEAD of the key's chain — never a fixed engram. The orchestrator
// holds the per-(vault, keyHash) stripe mutex across lookup → dispatch → write
// so concurrent upserts on the same key serialize, and the index is updated
// atomically on every path:
//
//   - MISS or stale pointer (absent / soft-deleted / hard-deleted head) → the
//     default Write path creates a fresh engram; PutUpsertKey then pins the new
//     ULID under the key.
//   - HIT + identical content → no-op (returns the existing head id).
//   - HIT + changed content → EvolveAt creates the successor, supersedes the
//     head, and re-points the 0x30 entry IN THE SAME atomic batch as the
//     successor write (the re-point is queued via StoreBatch.RepointUpsertKey
//     inside evolveAtInternal — scrypster's #1 trap: never a separate commit).
//
// This replaces the v1 in-place merge path. The review's insight: a path that
// mutates an existing engram in place has to hand-re-derive every invariant the
// create path establishes, and each pass finds a different one it forgot.
// Delegating to Write (creates) and EvolveAt (content changes) means upsert
// inherits both paths' invariant coverage for free.
//
// v1 scope retained: the delegated paths run the search-critical side effects
// (HNSW insert for caller embeddings, FTS submit, vault-name, triggers +
// counters on create). Upsert targets content-addressed re-ingest (e.g. a
// document-sync bridge), which does not carry caller-enrichment / analytics hooks —
// EvolveAt carries the predecessor's entity links forward but does not run
// contradiction/novelty/auto-associations. Hook parity is a follow-up if needed.
func (e *Engine) writeUpsert(ctx context.Context, wsPrefix [8]byte, req *mbp.WriteRequest) (*mbp.WriteResponse, error) {
	start := time.Now()

	if req.IdempotentID == "" {
		return nil, fmt.Errorf("%w: upsert_mode requires a non-empty idempotent_id", ErrInvalidRequest)
	}
	if err := e.validateClientEmbeddingDim(wsPrefix, req.Embedding); err != nil {
		return nil, err
	}

	keyHash := sha256.Sum256([]byte(req.IdempotentID))
	mu := e.upsertKeyLock(wsPrefix, keyHash)
	mu.Lock()
	defer mu.Unlock()

	vaultName := req.Vault
	if vaultName == "" {
		vaultName = "default"
	}

	// ── Dispatch on the durable 0x30 pointer ──────────────────────────────
	// upsertKeyLock serializes concurrent upserts on the same key; the default
	// Write path takes contentHashLock internally for creates, EvolveAt takes
	// its own locks for evolves — writeUpsert no longer holds contentHashLock
	// itself (the v1 merge wrote the content-hash in-batch, the Rev 2 paths
	// delegate). No path takes upsertKeyLock + contentHashLock in the other
	// order, so the one-way lock order is preserved.
	existingID, err := e.store.GetUpsertKey(ctx, wsPrefix, keyHash)
	if err != nil {
		return nil, fmt.Errorf("upsert lookup: %w", err)
	}

	// Load the head once (if the pointer is present). Absent/non-Active head →
	// treat as a stale pointer and recreate (RedTeam #556 Change-2: merging
	// into a tombstone silently loses data). GetEngram returns (nil, ErrNotFound)
	// for a hard-deleted/absent engram — the error is the signal, not nil-nil.
	var head *storage.Engram
	if existingID != (storage.ULID{}) {
		head, _ = e.store.GetEngram(ctx, wsPrefix, existingID)
	}
	// A head that is Active but valid-time-expired (COG-19 not_true_since
	// invalidation stamps ValidUntil without soft-deleting — State stays
	// Active) is invisible to default recall exactly like a soft-deleted
	// head, so it must be treated as a stale pointer, not a live target for
	// the identical-content no-op below. Otherwise an agent that calls
	// muninn_forget(not_true_since=...) — the documented way to retire a
	// fact without deleting it — finds the SAME upsert key permanently stuck
	// returning "upsert-identical" on every re-sync of unchanged content,
	// recoverable only by the caller editing the text. Mirrors the sibling
	// content-hash-dedup predicate at the default Write path (this file,
	// ~line 1401): State != SoftDeleted && !IsExpired(now). Create, not
	// evolve-from-the-expired-head: an EvolveAt here would supersede an
	// engram that valid-time semantics already say is not the fact's current
	// state, manufacturing a supersession edge from a fact that was declared
	// no-longer-true, and would inherit ValidFrom/entity links across an
	// invalidation the caller explicitly asserted — a plain create is the
	// same shape as the ordinary stale-pointer branches just above it.
	if head == nil || head.State != storage.StateActive || head.IsExpired(time.Now()) {
		return e.upsertCreate(ctx, wsPrefix, vaultName, req, keyHash, start)
	}

	// HIT + Active → compare content hash to decide no-op vs evolve.
	if storage.ContentHash(head.Content) == storage.ContentHash(req.Content) {
		// Identical content: no-op. Cheap fast path — no write, no evolve, no
		// index churn. Return the existing head id with the identical hint so
		// callers can tell nothing changed.
		metrics.EngineWritesTotal.Inc()
		d := time.Since(start)
		if e.latencyTracker != nil {
			e.latencyTracker.Record(wsPrefix, "write", d)
		}
		metrics.WriteDuration.WithLabelValues(vaultName).Observe(d.Seconds())
		return &mbp.WriteResponse{
			ID:        existingID.String(),
			CreatedAt: time.Now().UnixNano(),
			Hint:      "upsert-identical",
		}, nil
	}

	// Changed content: EvolveAt creates the successor, supersedes the head, and
	// re-points the 0x30 pointer in one atomic batch (the re-point is queued
	// via StoreBatch.RepointUpsertKey inside evolveAtInternal). EvolveAt
	// inherits Concept/Tags/MemoryType/TypeLabel from the predecessor when
	// concept == ""; pass the caller's concept only if set, embedding when
	// supplied, importance when asserted. EffectiveAt defaults to now inside
	// EvolveAt. Trust is set to TrustInferred internally by EvolveAt — the
	// existing behavior, which upsert inherits.
	concept := req.Concept // "" → EvolveAt inherits the predecessor's concept
	newID, evErr := e.evolveAtInternal(
		ctx,
		req.Vault,
		existingID.String(),
		req.Content,
		"upsert_mode: content changed",
		req.Embedding,
		concept,
		nil, // entities: upsert targets content-addressed re-ingest — no inline entities
		req.Importance,
		time.Time{},
		&keyHash,
	)
	if evErr != nil {
		return nil, fmt.Errorf("upsert evolve: %w", evErr)
	}

	if fn, ok := e.onWrite.Load().(func()); ok && fn != nil {
		fn()
	}
	metrics.EngineWritesTotal.Inc()
	d := time.Since(start)
	if e.latencyTracker != nil {
		e.latencyTracker.Record(wsPrefix, "write", d)
	}
	metrics.WriteDuration.WithLabelValues(vaultName).Observe(d.Seconds())

	return &mbp.WriteResponse{
		ID:        newID.String(),
		CreatedAt: time.Now().UnixNano(),
		Hint:      "upsert-evolved",
	}, nil
}

// upsertCreate is the writeUpsert helper for the miss / stale-pointer branch:
// delegate to the default Write path (which runs content-hash dedup, ResolveTrust,
// importanceFromRequest, hooks, and counters), then pin the freshly created
// ULID under the upsert key via PutUpsertKey. Returns the Write response with
// Hint="upsert-created".
//
// The caller MUST hold the upsertKeyLock(wsPrefix, keyHash) stripe. The default
// Write path is entered with UpsertMode=false (we're already inside the upsert
// orchestrator; re-entering it would loop) and IdempotentID="" (upsert tracks
// via the durable 0x30 index, not the legacy receipt — a receipt would point
// the second caller back at the first's engram and bypass the head/evolve
// dispatch).
func (e *Engine) upsertCreate(ctx context.Context, wsPrefix [8]byte, vaultName string, req *mbp.WriteRequest, keyHash [32]byte, start time.Time) (*mbp.WriteResponse, error) {
	createReq := *req
	createReq.UpsertMode = false
	createReq.IdempotentID = ""
	// Identity-addressed create: bypass content-hash dedup so two distinct
	// upsert keys with byte-identical text stay two engrams (see
	// ctxKeySkipContentDedup).
	resp, err := e.Write(withSkipContentDedup(ctx), &createReq)
	if err != nil {
		return nil, fmt.Errorf("upsert create: %w", err)
	}
	parsedID, pErr := storage.ParseULID(resp.ID)
	if pErr != nil {
		return nil, fmt.Errorf("upsert create: parse id: %w", pErr)
	}
	// Pin the freshly created engram under the upsert key. Non-batched — the
	// engram has already committed under the default Write path's own batch,
	// which this call cannot join (Write owns and commits its own batch
	// internally). A crash in the sub-millisecond window between that commit
	// and this PutUpsertKey call self-heals the POINTER ONLY: the next
	// upsert on this key finds no 0x30 entry, treats it as a miss, and
	// creates again. It does NOT heal the orphan — the first engram this
	// call created is left Active with content-hash dedup bypassed
	// (ctxKeySkipContentDedup) and nothing pointing at it, so nothing will
	// ever supersede or reap it. That is structural while create delegates
	// to Write, which owns its own batch — fixing it would mean folding the
	// pointer write into the SAME batch as the engram write, which the
	// current delegation-to-Write design doesn't do.
	if err := e.store.PutUpsertKey(ctx, wsPrefix, keyHash, parsedID); err != nil {
		return nil, fmt.Errorf("upsert create: pin key: %w", err)
	}
	resp.Hint = "upsert-created"

	metrics.EngineWritesTotal.Inc()
	d := time.Since(start)
	if e.latencyTracker != nil {
		e.latencyTracker.Record(wsPrefix, "write", d)
	}
	metrics.WriteDuration.WithLabelValues(vaultName).Observe(d.Seconds())
	return resp, nil
}

// MaxBatchSize is the maximum number of items allowed in a single WriteBatch call.
const MaxBatchSize = 50

// ErrBatchTooLarge is returned when a WriteBatch call exceeds MaxBatchSize items.
var ErrBatchTooLarge = fmt.Errorf("batch size exceeds maximum of %d", MaxBatchSize)

// preparedBatchItem holds all data needed for one item in a WriteBatch —
// the prepared engram plus post-commit metadata. This avoids re-deriving
// vault prefixes and enrichment fields after the batch commits.
type preparedBatchItem struct {
	wsPrefix                  [8]byte
	vaultName                 string
	eng                       *storage.Engram
	inlineMode                string
	callerSummary             string
	callerEntities            []mbp.InlineEntity
	callerEntityRelationships []mbp.InlineEntityRelationship
	skipBackgroundEnrich      bool
	contentHash               [32]byte // SHA-256 of content for dedup
}

// WriteBatch writes multiple engrams in a single Pebble batch commit, then
// fans out all post-commit async work per item. This amortises the fsync cost
// across N engrams instead of paying it per-engram.
func (e *Engine) WriteBatch(ctx context.Context, reqs []*mbp.WriteRequest) ([]*mbp.WriteResponse, []error) {
	n := len(reqs)
	// Cluster single-writer gate (#596): refuse the whole batch, per item, so a
	// caller reading errs[i] sees the same reason for every element.
	if err := e.refuseNonLeaderWrite(); err != nil {
		errs := make([]error, n)
		for i := range errs {
			errs[i] = err
		}
		return make([]*mbp.WriteResponse, n), errs
	}
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

	// Track idempotency keys seen within this batch to prevent intra-batch duplicates.
	// If two items share an op_id, the second is recorded here and resolved after Phase 2
	// using the first item's committed ID. Cross-request TOCTOU is bounded to concurrent
	// batches — the same known limitation as content-hash intra-batch dedup above.
	seenOpIDs := make(map[string]int)      // op_id → original index of first occurrence
	pendingDuplicates := make(map[int]int) // duplicate item index → first-occurrence index

	for i, req := range reqs {
		wsPrefix := e.store.ResolveVaultPrefix(req.Vault)
		e.activity.Record(wsPrefix)

		// ── Upsert mode: route through the per-item upsert path (#556) ──
		// writeUpsert holds its own upsertKeyLock and delegates to Write or
		// EvolveAt. Per-item — no cross-item atomicity,
		// matching WriteBatch's existing per-item contract. Intra-batch same-key
		// items dedup naturally: B sees A's just-committed forward-index entry.
		if req.UpsertMode {
			resp, err := e.writeUpsert(ctx, wsPrefix, req)
			if err != nil {
				errs[i] = err
			} else {
				responses[i] = resp
			}
			continue
		}

		// ── Idempotency check (must precede content-hash dedup) ──────
		if req.IdempotentID != "" {
			// Check store for a receipt from a prior request.
			if receipt, err := e.store.CheckIdempotency(ctx, req.IdempotentID); err == nil && receipt != nil {
				responses[i] = &mbp.WriteResponse{ID: receipt.EngramID, Hint: "idempotent"}
				continue
			}
			// Check for intra-batch duplicate: if this op_id was already queued in
			// this batch, set a placeholder response now (to exclude the item from
			// filteredItems / WriteEngramBatch) and record it for resolution after
			// Phase 2 when the first occurrence's committed ID is known.
			if firstIdx, seen := seenOpIDs[req.IdempotentID]; seen {
				pendingDuplicates[i] = firstIdx
				responses[i] = &mbp.WriteResponse{Hint: "pending_duplicate"}
				continue
			}
			seenOpIDs[req.IdempotentID] = i
		}

		// ── Content-hash dedup: same O(1) check as single Write ──────
		// NOTE: Intra-batch dedup is not performed — items within the same batch
		// cannot see each other's hashes during Phase 1. This is a known limitation.
		contentHash := storage.ContentHash(req.Content)
		if existingID, err := e.store.GetContentHash(ctx, wsPrefix, contentHash); err == nil && existingID != (storage.ULID{}) {
			// Same live-AND-current check as the single-Write path: an EXPIRED
			// engram (closed ValidUntil <= now) must not be reinforced — write a
			// new engram with its own validity window instead.
			if existingEng, err := e.store.GetEngram(ctx, wsPrefix, existingID); err == nil &&
				existingEng.State != storage.StateSoftDeleted && !existingEng.IsExpired(time.Now()) {
				// Reinforce via TouchAccess (#682) — same locked primitive and
				// same 1/day dedup-channel cap as the single-Write path above;
				// this unlocked GetEngram→UpdateMetadata pair was the identical
				// STO-2 race, just in the batch path.
				if e.shouldDedupReinforce(existingID) {
					_ = e.store.TouchAccess(ctx, wsPrefix, existingID)
				}
				responses[i] = &mbp.WriteResponse{
					ID:        existingID.String(),
					CreatedAt: existingEng.CreatedAt.UnixNano(),
					Hint:      "duplicate_content",
				}
				continue
			}
			// Engram was soft-deleted or not found — remove stale hash mapping and proceed.
			_ = e.store.DeleteContentHash(ctx, wsPrefix, contentHash)
		}

		vaultName := req.Vault
		if vaultName == "" {
			vaultName = "default"
		}
		resolved := e.ResolveVaultPlasticity(vaultName)
		inlineMode := resolved.InlineEnrichment

		var callerSummary string
		var callerEntities []mbp.InlineEntity
		var callerRelationships []mbp.InlineRelationship

		switch inlineMode {
		case "background_only":
			// background_only: background LLM runs and wins; ignore any caller-provided fields.
		default:
			// "caller_only", "caller_preferred", "disabled", and unknown modes:
			// always store non-empty caller-provided fields.
			callerSummary = req.Summary
			callerEntities = req.Entities
			callerRelationships = req.Relationships
		}

		// Resolve trust (S8): default inferred; "verified" gated to write/full
		// creds. A bad/ungated trust fails only this item, not the whole batch.
		trust, trustErr := resolveTrust(ctx, req.Trust)
		if trustErr != nil {
			errs[i] = trustErr
			continue
		}

		eng := &storage.Engram{
			Concept:    req.Concept,
			Content:    req.Content,
			Tags:       req.Tags,
			Confidence: req.Confidence,
			Stability:  req.Stability,
			Importance: importanceFromRequest(req.Importance),
			Embedding:  req.Embedding,
			MemoryType: storage.MemoryType(req.MemoryType),
			TypeLabel:  req.TypeLabel,
			Trust:      trust,
		}

		if callerSummary != "" {
			eng.Summary = callerSummary
		}
		if req.CreatedAt != nil {
			if validateErr := validateCreatedAt(*req.CreatedAt); validateErr != nil {
				errs[i] = fmt.Errorf("%w: %v", ErrInvalidRequest, validateErr)
				continue
			}
			eng.CreatedAt = *req.CreatedAt
		}
		if validityErr := applyValidity(eng, req.ValidFrom, req.ValidUntil); validityErr != nil {
			errs[i] = validityErr
			continue
		}

		assocs := make([]storage.Association, 0, len(req.Associations)+len(callerRelationships))
		for _, a := range req.Associations {
			targetID, parseErr := storage.ParseULID(a.TargetID)
			if parseErr != nil {
				errs[i] = fmt.Errorf("parse target id: %w", parseErr)
				break
			}
			assocs = append(assocs, storage.Association{
				TargetID:      targetID,
				RelType:       storage.RelType(a.RelType),
				Weight:        a.Weight,
				Confidence:    a.Confidence,
				CreatedAt:     time.Unix(0, a.CreatedAt),
				LastActivated: a.LastActivated,
			})
		}
		if errs[i] != nil {
			continue
		}
		// relationships[] folded into the same eng.Associations slice as
		// associations[] — see the single-write path's comment (#817). A
		// dangling or malformed relationship target now fails only this
		// item's write, the same as a dangling associations[] target above,
		// instead of silently succeeding with the edge dropped.
		for _, rel := range callerRelationships {
			targetID, parseErr := storage.ParseULID(rel.TargetID)
			if parseErr != nil {
				errs[i] = fmt.Errorf("%w: relationship target_id %q: %v", ErrInvalidID, rel.TargetID, parseErr)
				break
			}
			assocs = append(assocs, storage.Association{
				TargetID:   targetID,
				RelType:    storage.RelType(relTypeFromString(rel.Relation)),
				Weight:     rel.Weight,
				Confidence: 1.0,
				CreatedAt:  time.Now(),
			})
		}
		if errs[i] != nil {
			continue
		}
		eng.Associations = assocs

		callerProvidedAny := callerSummary != "" || len(callerEntities) > 0
		skipBG := (inlineMode == "caller_only" && callerProvidedAny) || inlineMode == "disabled"

		// Refuse a mismatched caller-supplied embedding before anything is
		// persisted (issue #582).
		if err := e.validateClientEmbeddingDim(wsPrefix, req.Embedding); err != nil {
			errs[i] = err
			continue
		}

		items[i] = storage.EngramBatchItem{WSPrefix: wsPrefix, Engram: eng}
		prepared[i] = preparedBatchItem{
			wsPrefix:                  wsPrefix,
			vaultName:                 vaultName,
			eng:                       eng,
			inlineMode:                inlineMode,
			callerSummary:             callerSummary,
			callerEntities:            callerEntities,
			callerEntityRelationships: req.EntityRelationships,
			skipBackgroundEnrich:      skipBG,
			contentHash:               contentHash,
		}
		validCount++
	}

	// Phase 2: Single Pebble batch commit for all valid (non-dedup'd) engrams.
	// Build a filtered items slice excluding dedup'd and errored items so
	// WriteEngramBatch never sees nil Engram pointers.
	filteredItems := make([]storage.EngramBatchItem, 0, validCount)
	filteredIdx := make([]int, 0, validCount) // maps filtered position → original index
	for i := range reqs {
		if responses[i] != nil || errs[i] != nil {
			continue // dedup'd or errored in Phase 1
		}
		filteredIdx = append(filteredIdx, i)
		filteredItems = append(filteredItems, items[i])
	}
	batchIDs, batchErrs := e.store.WriteEngramBatch(ctx, filteredItems)
	ids := make([]storage.ULID, n)
	for fi, origIdx := range filteredIdx {
		if batchErrs[fi] != nil {
			// STO-12, same mapping as Engine.Write: a dangling inline
			// association target is a bad request, and it fails only its own
			// item — the rest of the batch is unaffected.
			if errors.Is(batchErrs[fi], storage.ErrDanglingEndpoint) {
				errs[origIdx] = fmt.Errorf("%w: %v", ErrInvalidID, batchErrs[fi])
			} else {
				errs[origIdx] = fmt.Errorf("write engram: %w", batchErrs[fi])
			}
			continue
		}
		ids[origIdx] = batchIDs[fi]
		responses[origIdx] = &mbp.WriteResponse{
			ID:        batchIDs[fi].String(),
			CreatedAt: time.Now().UnixNano(),
		}
		// Store content hash → engram ID mapping for future dedup lookups.
		if err := e.store.PutContentHash(ctx, prepared[origIdx].wsPrefix, prepared[origIdx].contentHash, batchIDs[fi]); err != nil {
			slog.Warn("engine: batch: failed to store content hash", "id", batchIDs[fi].String(), "err", err)
		}
		// Store idempotency receipt if caller provided an op_id.
		if reqs[origIdx].IdempotentID != "" {
			if err := e.store.WriteIdempotency(ctx, reqs[origIdx].IdempotentID, batchIDs[fi].String()); err != nil {
				slog.Warn("engine: batch: failed to store idempotency receipt", "op_id", reqs[origIdx].IdempotentID, "id", batchIDs[fi].String(), "err", err)
			}
		}
	}

	// Resolve intra-batch duplicates: fill in the committed ID from the first occurrence.
	for dupIdx, firstIdx := range pendingDuplicates {
		if responses[firstIdx] != nil {
			responses[dupIdx] = &mbp.WriteResponse{ID: responses[firstIdx].ID, Hint: "idempotent"}
		} else {
			// First occurrence failed to write; propagate its error.
			errs[dupIdx] = errs[firstIdx]
		}
	}

	// Phase 3: Post-commit async work for each successfully written engram.
	for i := range reqs {
		if errs[i] != nil || responses[i] == nil {
			continue
		}
		// Skip dedup'd items (they have no new engram to process).
		if responses[i].Hint == "duplicate_content" || responses[i].Hint == "idempotent" {
			continue
		}
		// Skip upsert-routed items — writeUpsert already committed them and ran
		// their own hooks; prepared[i]/ids[i] were never populated for these.
		// Rev 2 hints: upsert-created (miss/stale), upsert-evolved (content change),
		// upsert-identical (no-op). v1 used upsert-merged; it is no longer produced
		// but the check stays defensive against future hint names.
		if responses[i].Hint == "upsert-created" || responses[i].Hint == "upsert-evolved" ||
			responses[i].Hint == "upsert-identical" || responses[i].Hint == "upsert-merged" {
			continue
		}
		p := &prepared[i]
		id := ids[i]

		// Store caller-provided inline entities in the entity table (not as KeyPoints).
		if len(p.callerEntities) > 0 {
			ws, _ := e.store.FindVaultPrefix(id)
			var linkedEntityNames []string
			for _, ent := range p.callerEntities {
				typ := strings.ToLower(strings.TrimSpace(ent.Type))
				if typ == "" {
					typ = "other"
				}
				record := storage.EntityRecord{
					Name:       ent.Name,
					Type:       typ,
					Confidence: 1.0,
				}
				if err := e.store.UpsertEntityRecord(ctx, ws, record, "inline"); err != nil {
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

		// callerRelationships were already folded into eng.Associations in
		// Phase 1 and written atomically by WriteEngramBatch — nothing left
		// to do here (#817).

		// Store caller-provided entity-to-entity relationships in the 0x21 relationship index.
		wroteEntityRelationships := false
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
						continue
					}
					wroteEntityRelationships = true
				}
			}
		}

		// Mark per-stage digest flags for inline (caller-supplied) enrichment so
		// GetEnrichmentCandidates does not report these memories as un-enriched (#500).
		// Mirror the single-write path: only set a stage's flag when that stage's data
		// is actually present from the caller.
		var inlineStageFlags uint16
		if p.callerSummary != "" {
			inlineStageFlags |= plugin.DigestSummarized
		}
		if wroteEntityRelationships {
			inlineStageFlags |= plugin.DigestRelationships
		}
		if p.eng.TypeLabel != "" {
			inlineStageFlags |= plugin.DigestClassified
		}
		if inlineStageFlags != 0 {
			existing, _ := e.store.GetDigestFlags(ctx, plugin.ULID(id))
			if flagErr := e.store.SetDigestFlag(ctx, id, existing|inlineStageFlags); flagErr != nil {
				slog.Warn("engine: batch: failed to set inline enrichment stage flags", "id", id.String(), "flags", inlineStageFlags, "error", flagErr)
			}
		}

		// When the caller provided an embedding, insert into HNSW inline first
		// and only mark DigestEmbed on success — a refused insert (#582) leaves
		// the engram for the retroactive processor to embed server-side.
		if len(reqs[i].Embedding) > 0 {
			if err := e.hnswRegistry.Insert(ctx, p.wsPrefix, [16]byte(id), reqs[i].Embedding); err != nil {
				slog.Warn("engine: batch: client embedding not inserted into HNSW; engram left for the retroactive processor", "id", id.String(), "err", err)
			} else {
				existing, _ := e.store.GetDigestFlags(ctx, plugin.ULID(id))
				if err := e.store.SetDigestFlag(ctx, id, existing|plugin.DigestEmbed); err != nil {
					slog.Warn("engine: batch: failed to set DigestEmbed flag", "id", id.String(), "err", err)
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
						e.triggers.NotifyContradiction(wsVaultID(wsPrefix), wsPrefix, storage.ULID(ev.EngramA), storage.ULID(ev.EngramB), ev.Severity, "relation_matrix")
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
		if errors.Is(err, storage.ErrNotFound) {
			return nil, ErrEngramNotFound
		}
		return nil, fmt.Errorf("get engram: %w", err)
	}

	// S3: effective read-only = observe-mode credential OR an explicit
	// req.ReadOnly (already the effective decision computed by the
	// MCP/REST/gRPC handler). A read-only read must skip BOTH write side
	// effects below — the implicit feedback signal AND #682's reinforcement —
	// closing the brief→"verify before acting"→read→reinforce loop for
	// observe credentials.
	readOnly := auth.ObserveFromContext(ctx) || req.ReadOnly

	if !readOnly {
		// Fire implicit positive feedback signal asynchronously — read = accessed.
		// spawnFireAndForget ensures Stop() drains this goroutine before DB close.
		e.spawnFireAndForget(func() {
			signal := scoring.FeedbackSignal{
				EngramID:    [16]byte(id),
				Accessed:    true,
				ScoreVector: scoring.DefaultWeights(),
				Timestamp:   time.Now(),
			}
			e.scoring.RecordFeedback(e.stopCtx, wsPrefix, signal)
		})

		// #682: reinforce AccessCount/LastAccess for this explicit read-by-id,
		// gated on the vault's ReinforceOnRead plasticity flag (default true).
		// Fire-and-forget on e.stopCtx (not the request ctx) — mirrors the
		// feedback signal above: reinforcement is gated by engine lifecycle,
		// not request lifecycle, so a client disconnect must not abort it.
		// Uncapped (unlike the dedup channel): N explicit reads-by-id must
		// land AccessCount at N.
		if e.ResolveVaultPlasticity(req.Vault).ReinforceOnRead {
			e.spawnFireAndForget(func() {
				_ = e.store.TouchAccess(e.stopCtx, wsPrefix, id)
			})
		}
	}

	// Collect entities linked to this engram (0x20 forward index).
	var entities []mbp.InlineEntity
	_ = e.store.ScanEngramEntities(ctx, wsPrefix, id, func(name string) error {
		rec, err := e.store.GetEntityRecord(ctx, wsPrefix, name)
		if err != nil || rec == nil {
			entities = append(entities, mbp.InlineEntity{Name: name})
			return nil
		}
		entities = append(entities, mbp.InlineEntity{Name: rec.Name, Type: rec.Type})
		return nil
	})

	// Collect entity-to-entity relationships sourced from this engram (0x21 prefix).
	// co_occurs_with records are engine-generated side effects (not caller-provided) and
	// are excluded — use muninn_entity to explore co-occurrence data.
	var entityRels []mbp.InlineEntityRelationship
	_ = e.store.ScanEngramRelationships(ctx, wsPrefix, id, func(r storage.RelationshipRecord) error {
		if r.RelType == "co_occurs_with" {
			return nil
		}
		entityRels = append(entityRels, mbp.InlineEntityRelationship{
			FromEntity: r.FromEntity,
			ToEntity:   r.ToEntity,
			RelType:    r.RelType,
			Weight:     r.Weight,
		})
		return nil
	})

	d := time.Since(readStart)
	if e.latencyTracker != nil {
		e.latencyTracker.Record(wsPrefix, "read", d)
	}
	metrics.ReadDuration.WithLabelValues(req.Vault).Observe(d.Seconds())

	return &mbp.ReadResponse{
		ID:                  eng.ID.String(),
		Concept:             eng.Concept,
		Content:             eng.Content,
		Confidence:          eng.Confidence,
		Relevance:           eng.Relevance,
		Stability:           eng.Stability,
		AccessCount:         eng.AccessCount,
		Tags:                eng.Tags,
		State:               uint8(eng.State),
		CreatedAt:           eng.CreatedAt.UnixNano(),
		UpdatedAt:           eng.UpdatedAt.UnixNano(),
		LastAccess:          eng.LastAccess.UnixNano(),
		Summary:             eng.Summary,
		KeyPoints:           eng.KeyPoints,
		MemoryType:          uint8(eng.MemoryType),
		TypeLabel:           eng.TypeLabel,
		Classification:      eng.Classification,
		EmbedDim:            uint8(eng.EmbedDim),
		Trust:               uint8(eng.Trust),
		ValidFrom:           eng.EffectiveValidFrom().UnixNano(),
		ValidUntil:          validUntilNano(eng.ValidUntil),
		IsCurrent:           eng.ValidUntil.IsZero(),
		Importance:          eng.Importance,
		Entities:            entities,
		EntityRelationships: entityRels,
	}, nil
}

// validUntilNano converts an optional ValidUntil to UnixNano, keeping 0 as the
// "open window" sentinel.
func validUntilNano(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixNano()
}

// Activate implements mbp.EngineAPI.Activate.
func (e *Engine) Activate(ctx context.Context, req *mbp.ActivateRequest) (*mbp.ActivateResponse, error) {
	return e.activateCore(ctx, req, nil)
}

// ActivateWithStructuredFilter is like Activate but accepts a typed filter for
// post-retrieval predicate evaluation (e.g., *query.Filter from MQL WHERE clauses).
func (e *Engine) ActivateWithStructuredFilter(ctx context.Context, req *mbp.ActivateRequest, structuredFilter activation.EngramFilter) (*mbp.ActivateResponse, error) {
	return e.activateCore(ctx, req, structuredFilter)
}

// activateCore is the shared implementation for Activate and ActivateWithStructuredFilter.
// structuredFilter may be nil (plain Activate) or a typed predicate (structured query).
func (e *Engine) activateCore(ctx context.Context, req *mbp.ActivateRequest, structuredFilter activation.EngramFilter) (*mbp.ActivateResponse, error) {
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
	// Hebbian read side, gated symmetrically with PAS (COG-32). The same flag
	// already gates learning submission (below) and association decay
	// (decayAllVaults); before #779 the phase-4 boost ignored it.
	actReq.HebbianEnabled = resolved.HebbianEnabled
	actReq.ExcludeUntrusted = resolved.ExcludeUntrusted
	// Per-vault exclude-tags (#713): candidates carrying a configured tag are
	// dropped from recall ranking. nil/empty = no exclusion (unchanged behavior).
	actReq.ExcludeTags = resolved.ExcludeTags

	// Valid-time gate (COG-19): as_of / include_invalid flow into phase-6
	// filtering; the final gate below re-applies them to entity-boost injections.
	actReq.AsOf = req.AsOf
	actReq.IncludeInvalid = req.IncludeInvalid

	// Ownership-lease work-queue visibility (#548): hide engrams checked out by a
	// live foreign lease unless the caller opts in.
	actReq.CallerOwner = req.CallerOwner
	actReq.IncludeLeased = req.IncludeLeased

	// Set defaults
	if actReq.MaxResults == 0 {
		actReq.MaxResults = 20
	}
	// Fix 2: Default to resolved HopDepth (from Plasticity preset) BFS traversal.
	// Order matters: apply default FIRST, then check explicit opt-out.
	//
	// This default was justified as "the association graph is the primary
	// differentiator of MuninnDB". Half of that is still true and half is not
	// (#801): the graph reaches results through phase4HebbianBoost, which does
	// contribute — but phase5Traverse's hop gate has sat above the phase's own
	// score ceiling since the initial commit, so HopDepth > 0 has never produced
	// a hop on any real corpus. Measured on a 3,458-engram production vault with
	// 127,798 edges: 0 hops on 150/150 queries, and traversal strictly dominated
	// by raising CandidatesPerIndex at every budget cap. Left on rather than
	// defaulted off because it is inert, not harmful, and because HopDepth is a
	// per-vault plasticity value surfaced on six transports — disposing of it is
	// its own increment. See docs/internals/decision-record.md (#801).
	if actReq.HopDepth == 0 {
		actReq.HopDepth = resolved.HopDepth
	}
	if req.DisableHops {
		actReq.HopDepth = 0
	}

	// Fix 4: Observe mode is a pure read — skip activation log side effects.
	// S3: an explicit req.ReadOnly (already the effective value computed by the
	// MCP/REST/gRPC handler — credential-observe OR request read_only) also
	// forces this; it can only ADD read-only-ness, never remove it.
	actReq.ReadOnly = auth.ObserveFromContext(ctx) || req.ReadOnly

	// Resolve the recall-mode preset — explicit wire mode first, else the
	// vault default. The engine is the SINGLE preset decider (#704):
	// transports validate the mode name and forward it instead of stamping
	// preset values into the request, because only the engine knows the
	// effective scoring mode. An invalid mode from a raw wire caller is
	// ignored (transports fail fast; a display preference falls open, never
	// hides memory — the #604 line), and "balanced" means engine defaults.
	var modePreset *auth.RecallModePreset
	recallMode := req.Mode
	if recallMode == "" {
		recallMode = resolved.RecallMode
	}
	if recallMode != "" && recallMode != "balanced" {
		if p, mErr := auth.LookupRecallMode(recallMode); mErr == nil {
			modePreset = &p
		}
	}

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
			// The MBP/REST wire field is `omitempty` float32, so an explicit 0
			// from a caller never reaches this struct as 0 — it arrives as
			// "absent". Mapping 0 to nil here is therefore not a loss: it is
			// the only honest reading of what the wire can express. Per-vault
			// `actr_heb_scale: 0` goes through the plasticity branch below,
			// which CAN express it.
			ACTRHebScale: actrHebScalePtr(req.Weights.ACTRHebScale),
		}
	} else if modePreset != nil && req.Mode != "" && presetCarriesWeights(*modePreset) && resolved.ScoringFusion != "rrf" {
		// EXPLICIT weight-carrying mode (semantic/recent), no caller weights:
		// the preset defines the FULL weight vector, from the ZERO base — the
		// same struct these modes produced when transports stamped them as
		// caller weights. Overlaying resolved defaults instead is wrong: the
		// preset zero-value scheme cannot express semantic's implicit "decay,
		// hebbian, access, recency = 0", and under legacy (DisableACTR)
		// scoring those default weights let a fresh, recently-active engram
		// score ~0.7 with zero content match — above semantic's own 0.3
		// threshold (see presetCarriesWeights). rrf vaults take the resolved
		// branch instead: presets never change the fusion mode, and under rrf
		// the preset weights are inert.
		actReq.Weights = &activation.Weights{
			SemanticSimilarity: modePreset.SemanticSimilarity,
			FullTextRelevance:  modePreset.FullTextRelevance,
			Recency:            modePreset.Recency,
			UseACTR:            !modePreset.DisableACTR,
			DisableACTR:        modePreset.DisableACTR,
		}
	} else {
		actrDecay := float32(0.5)
		if resolved.ACTRDecay > 0 {
			actrDecay = float32(resolved.ACTRDecay)
		}
		// resolved.ACTRHebScale is ALWAYS populated by auth.ResolvePlasticity
		// (from the preset, or from an explicit override clamped to [0, 50]),
		// so there is no "unset" to fall back from. The `> 0` guard this
		// replaces was a SECOND silent substitution of the documented 0 —
		// principle #1, in the same request that the activation layer's own
		// substitution corrupted.
		actrHebScale := float32(resolved.ACTRHebScale)
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
			ACTRHebScale: &actrHebScale,
		}

		// Wire ScoringFusion from plasticity config to activation weights.
		if resolved.ScoringFusion == "rrf" {
			actReq.Weights.UseRRFFusion = true
			actReq.Weights.DisableACTR = true
			actReq.Weights.UseACTR = false
		} else if resolved.ScoringFusion == "weighted_sum" {
			actReq.Weights.DisableACTR = true
			actReq.Weights.UseACTR = false
		}
	}

	// Gate CGDN behind vault's ExperimentalCGDN flag.
	if actReq.Weights.UseCGDN && !resolved.ExperimentalCGDN {
		actReq.Weights.UseCGDN = false
	}

	// COG-26: resolve the semantic-abstention baseline b for this vault's
	// embed model, regardless of scoring mode — the noise floor is a property
	// of the embedder, not the scorer. Applies uniformly whether Weights came
	// from an explicit caller override, a recall-mode preset, or the resolved
	// default, so a caller-supplied Weights struct still gets the floor
	// (unlike SemanticSimilarity/FullTextRelevance, which an explicit caller
	// override legitimately replaces).
	actReq.Weights.SemanticBaseline = float32(e.resolveSemanticBaseline(req.Vault, wsPrefix, resolved))
	// Distinguish the two causes of a zero baseline for the relevance-band
	// phase: an operator who explicitly set `semantic_floor: 0` (documented
	// "disable the floor") must not be told their model is unregistered
	// (semantic_floor_disabled vs no_model_baseline — G6 refute of #773,
	// finding 3). Scoring math is identical either way (identity transform).
	actReq.Weights.SemanticFloorDisabled = semanticFloorExplicitlyDisabled(resolved)

	// COG-6: the effective default threshold is mode-aware and keyed on the
	// EFFECTIVE scoring mode (actReq.Weights.UseRRFFusion), decided here in one
	// place — AFTER the weights block sets UseRRFFusion — so the threshold default
	// always matches the scoring math that will actually run. (Keying on vault
	// config, resolved.ScoringFusion, diverges when a caller passes explicit
	// weights on an rrf vault: the request scores ACT-R but config says rrf.)
	// For rrf scoring, leave Threshold 0 ("unset") so activation.Run() applies its
	// rrf default (0.001, #590's mechanism); coercing to 0.1 here — an
	// ACT-R-calibrated value — made #590's fix unreachable on every production
	// transport. ACT-R/weighted_sum default behavior is unchanged.
	if actReq.Threshold == 0 && !actReq.Weights.UseRRFFusion {
		actReq.Threshold = cog6DefaultThreshold(actReq.Weights.UseRRFFusion, actReq.Weights.UseACTR)
	}

	// Apply the recall-mode preset (#704) — resolved into modePreset above the
	// weights block (the zero-base branch needs it there); runs after the
	// COG-6 coerce because its threshold decision is keyed on the effective
	// scoring mode. Semantics documented on applyRecallModePreset.
	if modePreset != nil {
		applyRecallModePreset(actReq, req, *modePreset)
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
	//
	// #598 follow-up: gate on actReq.ReadOnly (the single resolved decision —
	// observe-mode credential OR explicit req.ReadOnly, computed once above),
	// not on auth.ObserveFromContext(ctx) alone. Every post-Run write site in
	// this function must consult actReq.ReadOnly instead of re-deriving the
	// credential check directly — TestActivateCore_ObserveFromContextResolvedExactlyOnce
	// enforces there is exactly one direct call to auth.ObserveFromContext in
	// this function (the one computing actReq.ReadOnly above), so a future
	// write site that copies the old `!auth.ObserveFromContext(ctx)` pattern
	// fails the census immediately instead of silently ignoring an explicit
	// req.ReadOnly from a full-mode credential.
	var prevActivation []storage.ULID
	if resolved.PredictiveActivation && !actReq.ReadOnly {
		prevEntries := e.activation.AssocLog().RecentForVault(vaultID, 1)
		if len(prevEntries) > 0 {
			prevActivation = prevEntries[0].EngramIDs
		}
	}

	// Run activation
	result, err := e.activation.Run(ctx, actReq)
	if err != nil {
		// #606: hard-failure counter, labelled by a coarse reason so an
		// operator can separate a caller-side cancellation/timeout (expected
		// under load, not an embed/activation defect) from an actual
		// activation error.
		reason := "activation_error"
		if errors.Is(err, context.DeadlineExceeded) {
			reason = "timeout"
		} else if errors.Is(err, context.Canceled) {
			reason = "canceled"
		}
		metrics.RecallErrorsTotal.WithLabelValues(req.Vault, reason).Inc()
		return nil, fmt.Errorf("activation: %w", err)
	}
	metrics.EngineActivationsTotal.Inc()
	// #606: recall-health signal for the silent embed->BM25 fallback (#578,
	// hardened for reachability by #658). result.SemanticDegraded already
	// surfaces per-call on the MCP/MBP response (principle #2's "loud"); this
	// counter is the fleet-level counterpart — an operator can alert on
	// rate(muninndb_recall_embed_fallback_total)/rate(muninndb_activate_duration_seconds_count)
	// per vault instead of scraping logs for the WARN that fires on every
	// degraded call.
	if result.SemanticDegraded {
		metrics.RecallEmbedFallbackTotal.WithLabelValues(req.Vault).Inc()
	}

	// Entity boost phase: spread activation through shared named entities.
	// After BFS produces a scored set, any engram sharing a named entity with a
	// top-N result receives a small boost. This surfaces entity-linked engrams
	// that have no direct association edge to the query-matching engrams.
	preBoost := len(result.Activations)
	result.Activations = e.applyEntityBoost(ctx, wsPrefix, vaultSize, result.Activations, actReq)
	// Injected engrams count as found: on the boost path, total <
	// len(activations) was the #569 bypass fingerprint. Known imprecision, for
	// BOTH injectors here and below (scoped follow-up, PR #570 review): an
	// engram that scored above threshold in the pipeline but was truncated
	// past MaxResults inside Run() and then re-injected by boost or
	// supersession is counted twice.
	result.TotalFound += len(result.Activations) - preBoost

	// Supersedes-aware ranking: promote the current fact over any superseded one
	// it replaces (injecting it if the query didn't retrieve it), so recall never
	// leads with a fact it knows is stale. Runs after entity boost and BEFORE
	// truncation so an injected head is not cut. Chains resolve under the
	// caller's view through the shared visibility gate (hidden nodes are
	// traversable but unnameable; no admitted successor → the substitution
	// abstains whole), and admitted injections count as found, same rule as
	// boost. injectorNow is shared with the final COG-19 cut below so a
	// validity boundary cannot fall between the gate's admission and that cut.
	injectorNow := time.Now()
	// One visibility gate and ONE nameable cache shared by both post-pipeline
	// chain phases: they walk overlapping chains, and the gate's lease check is
	// a store read. Sharing also guarantees both resolve visibility at the same
	// instant (risk 3 of the COG-28 design).
	injectorGate := newVisibilityGate(actReq, injectorNow)
	nameableCache := make(map[storage.ULID]bool)

	// COG-28 version-head substitution (#763). Phase 6 handed us the
	// admission-worthy candidates it refused for being superseded; resolve each
	// to its DECLARED chain head and inject that head at the predecessor's
	// score, annotated substituted_for. Runs BEFORE applySupersession on the
	// same clock so an injected head is an ordinary row by the time supersedes-
	// aware ranking runs. See engine_version_head.go.
	var subInjected int
	var subBlocked []substitutionBlock
	result.Activations, subInjected, subBlocked = e.applyVersionHeadSubstitution(
		ctx, wsPrefix, result.Activations, result.ShadowMatches, actReq, injectorGate, nameableCache)
	result.TotalFound += subInjected

	var supInjected int
	result.Activations, supInjected = e.applySupersessionWithGate(ctx, wsPrefix, result.Activations, actReq, injectorGate, nameableCache)
	result.TotalFound += supInjected

	// Final valid-time gate (COG-19: default recall never returns an engram whose
	// ValidUntil <= now). Phase-6 gated scored candidates and both injectors
	// gate their entrants. For supersession this cut is defense in depth at
	// the same instant (injectorNow); it remains LOAD-BEARING for two paths:
	// boost runs on its own earlier clock, so a boost injection (or phase-6
	// survivor) whose validity boundary falls inside that window is admitted,
	// counted at line ~2340, then swept here — a documented overcount sliver —
	// and it backstops any future result-set mutation that forgets the gate.
	// Runs AFTER supersession
	// on purpose: a manual supersede's now-expired predecessor is dropped only once
	// its current head has been promoted/injected, so a query matching only the
	// stale phrasing still returns the current fact.
	// NOTE: filter into a NEW slice — the activation engine's async log-drain
	// goroutine may still be reading the backing array returned by Run(), so
	// in-place compaction (Activations[:0]) is a data race.
	gateNow := injectorNow
	kept := make([]activation.ScoredEngram, 0, len(result.Activations))
	for _, s := range result.Activations {
		if activation.PassesValidity(s.Engram, req.AsOf, req.IncludeInvalid, gateNow) {
			kept = append(kept, s)
		}
	}
	result.Activations = kept

	// Recall-time version-cluster ADVISORY annotation (#712, COG-25). Pure
	// read-path, zero writes, zero score changes: pairwise-clusters the top
	// survivors when embedding/token similarity, a shared anchor (rare entity,
	// existing RelRefines edge, or competing version-marker tags), and temporal
	// separation ALL hold, then annotates (never demotes) and — ONLY at an exact
	// score tie within a detected cluster — reorders newest-EffectiveValidFrom-
	// first. An explicit RelSupersedes on a pair always wins and suppresses the
	// heuristic for that pair (COG-25). Runs after the validity gate and before
	// final truncation so it sees exactly the survivor set truncation will cut.
	result.Activations = e.applyCurrencyAnnotation(ctx, wsPrefix, result.Activations, vaultSize, actReq.MaxResults)

	// COG-29 contradiction honesty (#764). Runs LAST of the post-pipeline
	// phases and before truncation: after the COG-19 gate so a side already
	// removed for being invalid/soft-deleted is neither resurrected nor
	// referenced (this is what makes evolve/forget stop the theater), after
	// supersession/substitution because a declared RelSupersedes between the
	// pair IS the resolution, after currency because COG-25 may reorder exact
	// ties and this phase's ordering must be the last word, and before
	// truncation so the adjacency guarantee holds.
	var conflictKeep int
	var conflictBlock *mbp.ConflictBlock
	result.Activations, conflictBlock, conflictKeep = e.applyContradictionHonesty(
		ctx, wsPrefix, result.Activations, actReq, injectorGate, gateNow)

	// Re-apply MaxResults: entity boost / supersession / the validity gate may have
	// changed the set. All re-sort or preserve score order, so truncation keeps top-K.
	// COG-29 may raise the cut by up to contradictionMaxCluster-1 so a conflict
	// cluster is never returned half-truncated; the overflow is reported in the
	// conflict block rather than applied silently.
	truncateAt := actReq.MaxResults
	if conflictKeep > truncateAt {
		truncateAt = conflictKeep
	}
	if truncateAt > 0 && len(result.Activations) > truncateAt {
		result.Activations = result.Activations[:truncateAt]
	}

	// A conflict cluster that fell entirely outside the caller's cut must not
	// be reported: the block describes what the caller RECEIVED. Prune to the
	// pairs with a surviving endpoint, and drop the block when none remain.
	conflictBlock = pruneConflictBlock(conflictBlock, result.Activations)

	// ABSTENTION IS RECOMPUTED HERE, on the FINAL set, and nowhere earlier.
	// Phase 6's verdict described ITS set, not this one: the substitution and
	// supersession injectors can FILL a response phase 6 abstained on (the
	// exact #763 shape — every live candidate below threshold, then the head
	// injected), and the COG-19 gate / MaxResults re-truncation can EMPTY a
	// response phase 6 did not abstain on. The wire contract ("Empty iff
	// Abstained is false", mbp/types.go) binds what the caller receives, so
	// the flag must be derived from what the caller receives. Deliberately
	// NOT cleared inside applyVersionHeadSubstitution: the gate and the
	// truncation run after it and can empty the set again.
	if len(result.Activations) > 0 {
		result.Abstained = false
		result.AbstainedReason = ""
		if len(subBlocked) > 0 {
			slog.Debug("recall: version-head substitution blocked on some chains", "blocked", len(subBlocked))
		}
	} else {
		result.Abstained = true
		// COG-28: an empty response that had a declared version chain in it is
		// not the same emptiness as "nothing in this vault is about that".
		// Name it, so an agent can act ("there IS a chain here — read <id>")
		// instead of being handed the generic below_threshold.
		if reason := versionHeadAbstainReason(subBlocked); reason != "" {
			result.AbstainedReason = reason
		} else if result.AbstainedReason == "" {
			// Phase 6 produced rows and the post-pipeline gates swept them
			// all: candidates cleared the bar and were then filtered — the
			// same emptiness AbstainFiltered already names.
			result.AbstainedReason = activation.AbstainFiltered
		}
	}

	// #773 score-presentation honesty. Runs LAST of the post-pipeline phases —
	// after currency, contradiction honesty, the final truncation and the
	// abstention recompute — so it bands exactly the rows the caller receives.
	// READ-ONLY: no score change, no reorder, no truncation, no removal, no
	// write. See engine_relevance.go for why the calibration gate, and not
	// actReq.Threshold, is the anchor.
	relevanceBands, relevanceBases := applyRelevanceBands(
		result.Activations,
		result.Calibration,
		cog6DefaultThreshold(actReq.Weights.UseRRFFusion, actReq.Weights.UseACTR),
		result.SemanticDegraded,
	)

	// Convert result.Activations to []mbp.ActivationItem
	items := make([]mbp.ActivationItem, len(result.Activations))
	for i, scored := range result.Activations {
		items[i] = mbp.ActivationItem{
			ID:          scored.Engram.ID.String(),
			Concept:     scored.Engram.Concept,
			Content:     scored.Engram.Content,
			Summary:     scored.Engram.Summary,
			Score:       float32(scored.Score),
			Confidence:  scored.Engram.Confidence,
			Why:         scored.Why,
			Dormant:     scored.Dormant,
			CreatedAt:   scored.Engram.CreatedAt.UnixNano(),
			LastAccess:  scored.Engram.LastAccess.UnixNano(),
			AccessCount: scored.Engram.AccessCount,
			Relevance:   scored.Engram.Relevance,
			State:       uint8(scored.Engram.State),
			Trust:       uint8(scored.Engram.Trust),
			MemoryType:  uint8(scored.Engram.MemoryType),
			TypeLabel:   scored.Engram.TypeLabel,
			Tags:        scored.Engram.Tags,
			Importance:  scored.Engram.Importance,
			// #773: the absolute relevance band and, for filter_match /
			// uncalibrated, why.
			RelevanceBand:      relevanceBands[i],
			RelevanceBandBasis: relevanceBases[i],
		}

		// Supersession annotation from the supersedes-aware ranking phase (always-on
		// when this result is superseded; empty otherwise). Zero ULID → omit.
		if (scored.CurrentVersion != storage.ULID{}) {
			items[i].CurrentVersion = scored.CurrentVersion.String()
		}
		if (scored.SupersededBy != storage.ULID{}) {
			items[i].SupersededBy = scored.SupersededBy.String()
		}
		// COG-28 substitution annotation (#763): ASSERTED, from a declared
		// RelSupersedes chain — a sibling of superseded_by/current_version
		// above, NOT part of the advisory possibly_superseded_by block below.
		// Never optional on a substituted row: the score components on such a
		// row are the PREDECESSOR's measurements, so the attribution is what
		// keeps them honest.
		if (scored.SubstitutedFor != storage.ULID{}) {
			items[i].SubstitutedFor = scored.SubstitutedFor.String()
			items[i].ChainTruncated = scored.ChainTruncated
			items[i].HeadNotIndexedYet = scored.HeadNotIndexedYet
			if b := scored.SubstitutionBasis; b != nil {
				items[i].SubstitutionBasis = &mbp.SubstitutionBasis{
					AbsoluteScore:      float32(b.AbsoluteScore),
					ContentMatch:       float32(b.ContentMatch),
					SemanticSimilarity: float32(b.SemanticSimilarity),
					FullTextRelevance:  float32(b.FullTextRelevance),
				}
			}
		}
		// COG-29 contradiction honesty (#764): ASSERTED, from a declared
		// RelContradicts edge that is still unresolved. Always-on for the same
		// reason superseded_by is — a caller must never be handed a disputed
		// fact without being told, and this row's score was demoted and capped
		// because of it.
		if c := scored.UnresolvedContradiction; c != nil {
			item := &mbp.ContradictionConflict{
				With:             c.With.String(),
				WithConcept:      c.WithConcept,
				Side:             c.Side,
				PartnerInResults: c.PartnerInResults,
				ClusterSize:      c.ClusterSize,
				ClusterTruncated: c.ClusterTruncated,
			}
			// Zero means UNKNOWN (a legacy edge with no stamp): omit it rather
			// than serialising the Go zero time as if it were an instant.
			if !c.DeclaredAt.IsZero() {
				item.DeclaredAt = c.DeclaredAt.UTC().Format(time.RFC3339)
			}
			items[i].UnresolvedContradiction = item
		}
		// Heuristic currency annotation (advisory; from applyCurrencyAnnotation).
		if (scored.PossiblySupersededBy != storage.ULID{}) {
			items[i].PossiblySupersededBy = scored.PossiblySupersededBy.String()
		}
		if scored.VersionCluster != "" {
			items[i].VersionCluster = scored.VersionCluster
			items[i].ClusterSize = scored.ClusterSize
		}
		items[i].NewestOfCluster = scored.NewestOfCluster
		// Valid-time annotations: ValidFrom only when explicitly divergent from
		// CreatedAt; ValidUntil when the window is closed; Expired when the
		// window closed at or before now (reachable only under include_invalid).
		if !scored.Engram.ValidFrom.IsZero() && !scored.Engram.ValidFrom.Equal(scored.Engram.CreatedAt) {
			items[i].ValidFrom = scored.Engram.ValidFrom.UnixNano()
		}
		if !scored.Engram.ValidUntil.IsZero() {
			items[i].ValidUntil = scored.Engram.ValidUntil.UnixNano()
		}
		items[i].Expired = scored.Engram.IsExpired(gateNow)

		items[i].ScoreComponents = mbp.ScoreComponents{
			SemanticSimilarity:    float32(scored.Components.SemanticSimilarity),
			SemanticSimilarityRaw: float32(scored.Components.SemanticSimilarityRaw),
			FullTextRelevance:     float32(scored.Components.FullTextRelevance),
			DecayFactor:           float32(scored.Components.DecayFactor),
			HebbianBoost:          float32(scored.Components.HebbianBoost),
			TransitionBoost:       float32(scored.Components.TransitionBoost),
			EntityBoost:           float32(scored.Components.EntityBoost),
			AccessFrequency:       float32(scored.Components.AccessFrequency),
			Recency:               float32(scored.Components.Recency),
			Raw:                   float32(scored.Components.Raw),
			Final:                 float32(scored.Components.Final),
			ContentMatch:          float32(scored.Components.ContentMatch),
			AbsoluteScore:         float32(scored.Components.AbsoluteScore),
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
		// engine:spawn-ok — tracked by local wg, drained by wg.Wait() a few lines below
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

	// Persist the surfaced set as a recall event (issue #573): the engrams
	// this recall actually returned (post entity-boost, post truncation),
	// keyed by a time-ordered event ULID. Skipped when actReq.ReadOnly (observe
	// mode OR an explicit req.ReadOnly, #598) — a pure read must not write the
	// signal that calibration reads. On success the event ID becomes the
	// response query_id, so a later muninn_decide can be joined offline
	// against exactly what this recall surfaced.
	queryID := e.fastQueryID()
	if len(items) > 0 && !actReq.ReadOnly {
		eventID := storage.NewULID()
		entries := make([]storage.RecallSurfacedEntry, len(items))
		for i, item := range items {
			entries[i] = storage.RecallSurfacedEntry{ID: item.ID, Score: item.Score}
		}
		ev := &storage.RecallEvent{
			Context:    req.Context,
			Threshold:  float32(actReq.Threshold),
			SurfacedAt: time.Now().UnixNano(),
			Entries:    entries,
		}
		if err := e.store.WriteRecallEvent(ctx, wsPrefix, eventID, ev); err != nil {
			// Persistence is best-effort: a failed sink write degrades
			// calibration ground truth, never the recall itself.
			slog.Warn("activate: persist recall event failed", "err", err)
		} else {
			queryID = eventID.String()
		}
	}

	// Submit co-activations to Hebbian worker. Skipped when actReq.ReadOnly
	// (observe mode OR an explicit req.ReadOnly, #598 — this was the live
	// defect: an unread caller's read_only:true still bonded every returned
	// engram pairwise) or when disabled by Plasticity.
	// On Lobe nodes (hebbianWorker == nil) collect refs for forwarding to Cortex instead.
	var lobeCoActivations []mbp.CoActivationRef
	if len(result.Activations) > 0 && !actReq.ReadOnly && resolved.HebbianEnabled {
		hebW, _, _ := e.cogWorkers()
		if hebW != nil {
			coActivatedEngrams := make([]cognitive.CoActivatedEngram, len(result.Activations))
			for i, scored := range result.Activations {
				coActivatedEngrams[i] = cognitive.CoActivatedEngram{
					ID:    scored.Engram.ID,
					Score: scored.Score,
				}
			}
			// Build per-vault LTP config from resolved plasticity.
			var ltpCfg *cognitive.LTPConfig
			if resolved.LTPThreshold > 0 {
				ltpCfg = &cognitive.LTPConfig{
					Threshold:   resolved.LTPThreshold,
					WeightFloor: resolved.LTPWeightFloor,
				}
			}
			hebW.Submit(cognitive.CoActivationEvent{
				WS:      wsPrefix,
				At:      time.Now(),
				Engrams: coActivatedEngrams,
				LTP:     ltpCfg,
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
	// with the async drainLog goroutine. prevActivation is already empty whenever
	// actReq.ReadOnly is set (see the snapshot above), but the explicit check here
	// is defense in depth: this write must not depend on an implementation detail
	// of an earlier phase to stay suppressed under #598's contract.
	if len(result.Activations) > 0 && len(prevActivation) > 0 && !actReq.ReadOnly {
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

	// Forward Lobe side effects to Cortex: co-activations and any edges restored
	// from archive during Phase 4.75. ArchivedEdges are not forwarded — the Cortex
	// runs its own decay pass and archives edges independently.
	restoredEdges := result.RestoredEdges
	if e.coordinator != nil && (len(lobeCoActivations) > 0 || len(restoredEdges) > 0) {
		effect := mbp.CognitiveSideEffect{
			QueryID:       e.fastQueryID(),
			OriginNodeID:  e.coordinatorID,
			Timestamp:     time.Now().UnixNano(),
			CoActivations: lobeCoActivations,
			RestoredEdges: restoredEdges,
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
		QueryID:          queryID,
		TotalFound:       result.TotalFound,
		Activations:      items,
		LatencyMs:        result.LatencyMs,
		Brief:            briefSentences,
		SemanticDegraded: result.SemanticDegraded,
		Abstained:        result.Abstained,
		AbstainedReason:  result.AbstainedReason,
		Conflict:         conflictBlock,
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
		WSPrefix:       wsPrefix,
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
	if err := e.refuseWrite(ctx); err != nil {
		return nil, err
	}
	wsPrefix := e.store.ResolveVaultPrefix(req.Vault)

	sourceID, err := storage.ParseULID(req.SourceID)
	if err != nil {
		return nil, fmt.Errorf("%w: source_id %q: %v", ErrInvalidID, req.SourceID, err)
	}

	targetID, err := storage.ParseULID(req.TargetID)
	if err != nil {
		return nil, fmt.Errorf("%w: target_id %q: %v", ErrInvalidID, req.TargetID, err)
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
		if meta.State == storage.StateArchived {
			return nil, ErrEngramArchived
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

	// An explicit RelSupersedes link closes the target's validity window at
	// write time (valid-time axis, COG-19: a stamp, never a delete). Skipped
	// when the window is already closed — an earlier stamp records when the
	// fact actually stopped being true and must not be moved. Best-effort:
	// the association is already committed, and the legacy read-time
	// supersession chain-walk still demotes unstamped targets.
	if storage.RelType(req.RelType) == storage.RelSupersedes {
		if _, stampErr := e.store.StampValidUntil(ctx, wsPrefix, targetID, time.Now(), true); stampErr != nil {
			slog.Warn("engine: link: failed to stamp ValidUntil on superseded target",
				"target", targetID.String(), "err", stampErr)
		}
	}

	// When a "contradicts" link is explicitly created via Link(), notify the
	// ContradictWorker so it can flag the pair and drive confidence updates.
	if storage.RelType(req.RelType) == storage.RelContradicts {
		// Tell COG-29's fast-path gate this vault has a declared contradiction
		// NOW, rather than when the batch detector's 0x0A marker lands. The
		// declaration is durable the moment the association above committed,
		// so recall must honor it on the very next query.
		e.noteContradictionDeclared(wsPrefix)
		_, linkContra, _ := e.cogWorkers()
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
						e.triggers.NotifyContradiction(wsVaultID(wsPrefix), wsPrefix, storage.ULID(ev.EngramA), storage.ULID(ev.EngramB), ev.Severity, "explicit_link")
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
		// The confidence penalty is applied ONLY by the ContradictWorker's OnFound
		// callback above, which fires exactly once per pair (the 0x0A marker makes
		// it idempotent). A second, unconditional submit used to live here to
		// avoid waiting for the batch worker — but it bypassed that idempotency
		// and applied the SAME evidence a second time. BayesianUpdate compounds
		// repeat applications, so an explicit link cost two updates instead of
		// one, and linking both directions (the natural thing for an agent to do)
		// drove BOTH engrams 1.0 -> 0.975 -> 0.797 -> 0.313 -> 0.0709. Confidence
		// multiplies into the recall score, so both memories — including the TRUE
		// one, penalised exactly as hard as the false one — silently vanished from
		// recall. Declaring a contradiction was a data-loss operation.
		//
		// The penalty is now delayed by up to one worker interval. That is the
		// correct trade: a slightly late annotation beats a promptly destroyed
		// memory.
	}

	return &mbp.LinkResponse{OK: true}, nil
}

// AdjustConfidence applies a signed delta to engram id's confidence (clamped to
// [0,1]) and, when hasContra, mirrors the internal OnFound: persists the 0x0A
// contradiction marker for (id, other) AND submits EvidenceContradiction to the
// ConfidenceWorker for both engrams. Returns the new absolute confidence.
//
// All validation precedes any write. A bare delta (hasContra=false) does NOT
// submit to the ConfidenceWorker (§D3 carve-out — processBatch would otherwise
// clobber the explicit value).
//
// Mirrors Engine.Link's vault resolution + GetMetadata existence pattern +
// cogWorkers() submit pattern. Source string is "external_contradiction" to
// distinguish external bridge signal from the internal "contradiction_detected"
// path emitted by Link/ContradictWorker.
func (e *Engine) AdjustConfidence(ctx context.Context, vault string, id storage.ULID, delta float32, other storage.ULID, hasContra bool, reason, caller string) (float32, error) {
	if err := e.refuseWrite(ctx); err != nil {
		return 0, err
	}
	wsPrefix := e.store.ResolveVaultPrefix(vault)

	if math.IsNaN(float64(delta)) || math.IsInf(float64(delta), 0) {
		return 0, fmt.Errorf("%w: delta is NaN or Inf", ErrInvalidArgument)
	}
	if hasContra && other == id {
		return 0, ErrSelfContradiction
	}

	// Existence check — mirror Engine.Link's batched GetMetadata (nil meta = NotFound).
	ids := []storage.ULID{id}
	if hasContra {
		ids = []storage.ULID{id, other}
	}
	metas, err := e.store.GetMetadata(ctx, wsPrefix, ids)
	if err != nil {
		return 0, fmt.Errorf("adjust confidence: read meta: %w", err)
	}
	for _, m := range metas {
		if m == nil {
			return 0, ErrEngramNotFound
		}
	}

	// Atomic delta write: the storage method holds the per-engram stripe lock
	// across read+add+clamp+commit, closing the lost-update race (#559). The
	// prior confidence read and the [0,1] clamp have moved INTO the locked
	// storage method (UpdateConfidenceWithContradiction) — an external unlocked
	// read here would re-open the race, so we do not call GetConfidence. The
	// returned (prior, newConf) come from inside the locked section so the
	// audit log below reports the same values that were committed.
	prior, newConf, err := e.store.UpdateConfidenceWithContradiction(ctx, wsPrefix, id, delta, other, hasContra)
	if err != nil {
		return 0, fmt.Errorf("adjust confidence: write: %w", err)
	}

	// Mirror OnFound: submit EvidenceContradiction for both engrams (only when hasContra).
	if hasContra {
		if _, _, cw := e.cogWorkers(); cw != nil {
			cw.Submit(cognitive.ConfidenceUpdate{
				WS: wsPrefix, EngramID: [16]byte(id),
				Evidence: cognitive.EvidenceContradiction, Source: "external_contradiction",
			})
			cw.Submit(cognitive.ConfidenceUpdate{
				WS: wsPrefix, EngramID: [16]byte(other),
				Evidence: cognitive.EvidenceContradiction, Source: "external_contradiction",
			})
		}
	}

	slog.Info("adjust_confidence",
		"engram_id", id.String(), "delta", delta, "prior", prior, "new", newConf,
		"contradicted_by", other.String(), "has_contra", hasContra,
		"reason", reason, "caller", caller, "vault", vault)

	return newConf, nil
}

// Forget implements mbp.EngineAPI.Forget.
func (e *Engine) Forget(ctx context.Context, req *mbp.ForgetRequest) (*mbp.ForgetResponse, error) {
	// refuseWrite, not an inline AppendFromContext check: this method used to
	// re-implement the append gate by hand, which meant it also missed the
	// cluster single-writer gate when that was added to the shared helper (#596).
	if err := e.refuseWrite(ctx); err != nil {
		return nil, err
	}
	wsPrefix := e.store.ResolveVaultPrefix(req.Vault)

	id, err := storage.ParseULID(req.ID)
	if err != nil {
		return nil, fmt.Errorf("parse id: %w", err)
	}

	// not_true_since: invalidate on the valid-time axis instead of deleting.
	// The engram stays ACTIVE with a closed validity window — default recall
	// drops it (COG-19), as_of/include_invalid still see it. This is an
	// explicit caller assertion of WHEN the fact stopped being true, so it
	// overwrites any prior stamp (onlyIfOpen=false).
	if req.NotTrueSince != nil {
		if req.Hard {
			return nil, fmt.Errorf("%w: not_true_since cannot be combined with hard=true — invalidation is a stamp, deletion is deletion", ErrInvalidRequest)
		}
		// Guard the stamp instant BEFORE writing: reject the epoch/uninitialized
		// sentinel (which would decode as "open" and silently NOT invalidate while
		// the response claims it did) and an inverted window (invalid before the
		// fact existed). Mirrors applyValidity, which the stamp path bypasses.
		eng, err := e.store.GetEngram(ctx, wsPrefix, id)
		if err != nil || eng == nil {
			return nil, ErrEngramNotFound
		}
		if err := validateStampTime(*req.NotTrueSince, eng.EffectiveValidFrom()); err != nil {
			return nil, err
		}
		if _, err := e.store.StampValidUntil(ctx, wsPrefix, id, *req.NotTrueSince, false); err != nil {
			if errors.Is(err, storage.ErrNotFound) {
				return nil, ErrEngramNotFound
			}
			return nil, fmt.Errorf("stamp not_true_since: %w", err)
		}
		return &mbp.ForgetResponse{OK: true}, nil
	}

	if req.Hard {
		// Read the engram before hard-deleting so we can clean up the
		// content-hash mapping (the record is gone after DeleteEngram).
		eng, _ := e.store.GetEngram(ctx, wsPrefix, id)

		if err := e.store.DeleteEngram(ctx, wsPrefix, id); err != nil {
			if errors.Is(err, storage.ErrNotFound) {
				return nil, ErrEngramNotFound
			}
			return nil, fmt.Errorf("hard delete: %w", err)
		}

		// Remove content-hash → engram ID mapping so the hash slot is freed
		// and the same content can be stored again.
		if eng != nil {
			contentHash := storage.ContentHash(eng.Content)
			if err := e.store.DeleteContentHash(ctx, wsPrefix, contentHash); err != nil {
				slog.Warn("engine: failed to delete content hash after hard delete", "id", req.ID, "err", err)
			}
		}

		// Remove the engram from the search indexes. Until #803 this branch
		// tombstoned HNSW but never touched FTS — only the SOFT branch below
		// called fts.DeleteEngram — so a hard-deleted engram's tag postings
		// survived, and autoassoc's tag search kept handing its ID to
		// WriteAssociation on every later write sharing a tag. See STO-12.
		e.cleanSearchIndexesAfterHardDelete(wsPrefix, [16]byte(id), eng)
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
			if errors.Is(err, storage.ErrNotFound) {
				return nil, ErrEngramNotFound
			}
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

// cleanSearchIndexesAfterHardDelete removes a hard-deleted engram from the FTS
// posting lists and tombstones it in the HNSW vector index.
//
// STO-12. storage.DeleteEngram's own cascade (0x01/0x02/0x03/0x04/0x14/0x25 and
// the secondary indexes) is complete, but the FTS and HNSW indexes live outside
// the store and every hard-delete caller must clean them itself. The two
// callers that did not — Forget(hard=true) and PruneVault — left the dead ID
// reachable from a tag search and a vector search, which the autoassoc and
// neighbor workers then turned back into association edges. `eng` may be nil
// (the record was unreadable before the delete); FTS cleanup needs its fields,
// the HNSW tombstone does not, so they are gated separately.
func (e *Engine) cleanSearchIndexesAfterHardDelete(wsPrefix [8]byte, id [16]byte, eng *storage.Engram) {
	if e.fts != nil && eng != nil {
		if err := e.fts.DeleteEngram(wsPrefix, id, eng.Concept, eng.CreatedBy, eng.Content, eng.Tags); err != nil {
			slog.Warn("engine: fts cleanup failed after hard delete",
				"id", storage.ULID(id).String(), "error", err)
		}
	}
	// Mark the node as deleted in the HNSW index so it is skipped in future
	// Search results. Memory is reclaimed on the next rebuild.
	if e.hnswRegistry != nil {
		e.hnswRegistry.TombstoneNode(wsPrefix, id)
	}
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
			e.triggers.NotifyCognitive(vaultID, ws, storage.ULID(id), field, float32(old), float32(new))
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
	stats := cognitive.EngineWorkerStats{
		Hebbian:    cognitive.DisabledWorkerStats(),
		Contradict: cognitive.DisabledWorkerStats(),
		Confidence: cognitive.DisabledWorkerStats(),
	}
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
	if err := e.refuseWrite(ctx); err != nil {
		return nil, err
	}
	ws := e.store.ResolveVaultPrefix(vault)
	ulid, err := storage.ParseULID(id)
	if err != nil {
		return nil, fmt.Errorf("parse id: %w", err)
	}
	eng, err := e.store.GetEngram(ctx, ws, ulid)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return nil, ErrEngramNotFound
		}
		return nil, fmt.Errorf("restore: %w", err)
	}
	if eng.State != storage.StateSoftDeleted && eng.State != storage.StateArchived {
		return nil, fmt.Errorf("restore: engram %s is not soft-deleted or archived (state=%d)", id, eng.State)
	}
	// Clone before the response-shaping writes below (#858, STO-20). The two
	// assignments at the end of this method exist only to make the RETURNED
	// engram reflect the restore; applied to the pointer GetEngram handed back
	// they wrote through the shared L1-cache struct instead — a race against
	// every concurrent recall, and the returned pointer was the cache's own,
	// which the caller is then free to mutate further.
	eng = eng.Clone()
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

	// Restore re-opens the validity window: without this, a restored engram
	// whose predecessor-stamp (evolve/supersedes) is in the past would be
	// invisible to default recall — restore would be a recall no-op.
	if !eng.ValidUntil.IsZero() {
		if _, err := e.store.StampValidUntil(ctx, ws, ulid, time.Time{}, false); err != nil {
			slog.Warn("engine: restore: failed to clear ValidUntil stamp", "id", id, "err", err)
		} else {
			eng.ValidUntil = time.Time{}
		}
	}

	// Re-add content-hash mapping so future writes detect this engram as a duplicate.
	contentHash := storage.ContentHash(eng.Content)
	_ = e.store.PutContentHash(ctx, ws, contentHash, ulid)

	eng.State = storage.StateActive
	return eng, nil
}

// UpdateLifecycleState transitions an engram to the named lifecycle state.
// The write goes through the stripe-locked CompareAndSet path (with no guard),
// so its read-modify-write is atomic and serializes with any concurrent
// transition on the same engram — closing the former TOCTOU.
func (e *Engine) UpdateLifecycleState(ctx context.Context, vault, id, state string) error {
	if err := e.refuseWrite(ctx); err != nil {
		return err
	}
	ws := e.store.ResolveVaultPrefix(vault)
	ulid, err := storage.ParseULID(id)
	if err != nil {
		return fmt.Errorf("parse id: %w", err)
	}
	newState, err := storage.ParseLifecycleState(state)
	if err != nil {
		return err
	}
	if _, err := e.store.CompareAndSet(ctx, ws, ulid,
		storage.CASCondition{},
		storage.CASMutation{State: &newState}); err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return fmt.Errorf("get engram: %w", err)
		}
		return err
	}
	return nil
}

// SetTrust changes the trust label of an engram identified by id (string ULID).
// trust must be one of "verified", "inferred", "external", "untrusted".
// Returns an error if the engram is not found or trust is invalid.
func (e *Engine) SetTrust(ctx context.Context, vault, id, trust string) error {
	if err := e.refuseWrite(ctx); err != nil {
		return err
	}
	level, err := storage.ParseTrustLevel(trust)
	if err != nil {
		return fmt.Errorf("parse trust: %w", err)
	}
	ws := e.store.ResolveVaultPrefix(vault)
	ulid, err := storage.ParseULID(id)
	if err != nil {
		return fmt.Errorf("parse id: %w", err)
	}
	if err := e.store.UpdateTrust(ctx, ws, ulid, level); err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return ErrEngramNotFound
		}
		return err
	}
	return nil
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
// prefix. This mirrors the convention in storage/impl.go line ~146. Used to
// route write/cognitive/contradiction events to the correct subscription
// bucket in the trigger registry (internal/engine/trigger.SubscriptionRegistry
// keys byVault by this uint32, not by the full 8-byte prefix).
//
// #696 residual: two vaults whose real 8-byte SipHash prefixes agree on the
// first 4 bytes share a registry bucket, and trigger.handleSweep then serves
// the WHOLE bucket from subs[0].WSPrefix (#692's fix), so every OTHER vault
// in that bucket gets swept against its own subscriptions but the FIRST
// subscription's store prefix — the class #692 fixed for the non-colliding
// case, reopened only for the colliding one. This is pre-existing design,
// untouched by #692/#696, and #692 strictly improved it (before that fix
// EVERY vault hit this class of bug, not just a colliding one).
//
// The collision probability was UNDER-STATED when #692 was filed ("~10⁻⁵ at
// 10k vaults" — an inherited assumption, not independently checked): the
// correct birthday-bound estimate is expected-colliding-pairs ≈
// N(N-1)/(2·2³²), giving P(at least one collision) = 1 - e^(-that) ≈ **1.16%
// at N=10,000 vaults**, not ~10⁻⁵ — that smaller figure is what the SAME
// formula gives at N≈300. The exposure is bounded (a mis-swept vault's own
// write/cognitive/contradiction pushes are unaffected — only the periodic
// sweep's store lookups inside a colliding bucket are wrong, and a fleet
// only reaches meaningful odds of a bucket actually colliding in the
// thousands-of-vaults range), but "bounded" is not "negligible" at the
// scale a multi-tenant deployment can reach. Long-term fix: key the trigger
// registry's byVault map by the full [8]byte prefix instead of this uint32,
// which erases the class rather than shrinking its odds.
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

// ConsolidateResult is returned by Engine.Consolidate.
type ConsolidateResult struct {
	MergedID storage.ULID
	Archived []string
	Warnings []string
}

// DecideResult is returned by Engine.Decide.
// Warnings lists any evidence IDs that could not be linked to the decision;
// the decision itself is always committed even when evidence linking partially fails.
type DecideResult struct {
	ID storage.ULID
	// Concept is the concept actually stored (the decision text). Returned so
	// a transport can echo what it wrote instead of an empty string — two
	// evaluators read the previous {"concept":""} response as data loss.
	Concept  string
	Warnings []string
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
// concept overrides the inherited concept label; empty string inherits verbatim (#483).
// entities, when non-empty, REPLACE the entity links otherwise carried forward
// from the predecessor — the update changed what the memory is about.
// All three writes are committed in a single atomic Pebble batch.
func (e *Engine) Evolve(ctx context.Context, vault, oldID, newContent, reason string, embedding []float32, concept string) (storage.ULID, error) {
	return e.EvolveAt(ctx, vault, oldID, newContent, reason, embedding, concept, nil, nil, time.Time{})
}

// EvolveAt is Evolve with three optional extras. entities, when non-nil,
// replaces the carried entity set (the update changed what the memory is
// about); nil carries the predecessor's entities forward as before. importance
// overrides the successor's caller-asserted importance (clamped [0,1], explicit
// 0 quantized to 0.01, same rules as Write); nil inherits the predecessor's
// explicitly asserted importance verbatim (an unset predecessor stays unset, so
// a type-derived default keeps deriving from the inherited MemoryType rather
// than being frozen into the record). effectiveAt is the application-time moment
// the new version became true — it becomes the successor's ValidFrom and the
// predecessor's ValidUntil stamp (half-open [from, until) windows meet exactly);
// the zero time defaults to now.
func (e *Engine) EvolveAt(ctx context.Context, vault, oldID, newContent, reason string, embedding []float32, concept string, entities []mbp.InlineEntity, importance *float32, effectiveAt time.Time) (storage.ULID, error) {
	return e.evolveAtInternal(ctx, vault, oldID, newContent, reason, embedding, concept, entities, importance, effectiveAt, nil)
}

// evolveAtInternal is EvolveAt with an optional upsert key hash. When non-nil,
// the 0x30 upsert-key forward index for (wsPrefix, *upsertKeyHash) is re-pointed
// to the successor ULID INSIDE the evolve's atomic batch — so a content-change
// upsert never leaves the durable pointer aimed at the soft-deleted predecessor
// (#556, scrypster's #1 trap: the re-point must be crash-atomic with the evolve,
// never a separate commit). Public Evolve/EvolveAt pass nil (unchanged behavior);
// only Engine.writeUpsert passes a non-nil pointer on the content-change path.
func (e *Engine) evolveAtInternal(ctx context.Context, vault, oldID, newContent, reason string, embedding []float32, concept string, entities []mbp.InlineEntity, importance *float32, effectiveAt time.Time, upsertKeyHash *[32]byte) (storage.ULID, error) {
	// Append-mode credentials cannot evolve (modify an existing memory). Guarding
	// EvolveAt covers Evolve too, since Evolve delegates here (#687 + valid-time).
	// Via refuseWrite so the cluster single-writer gate lands here too (#596) —
	// the previous inline AppendFromContext check silently opted out of it.
	if err := e.refuseWrite(ctx); err != nil {
		return storage.ULID{}, err
	}
	wsPrefix := e.store.ResolveVaultPrefix(vault)

	// Refuse a mismatched caller-supplied embedding before anything is
	// persisted (issue #582).
	if err := e.validateClientEmbeddingDim(wsPrefix, embedding); err != nil {
		return storage.ULID{}, err
	}

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
	// effectiveAt is the valid-time boundary between predecessor and successor:
	// old.ValidUntil = new.ValidFrom = effectiveAt (half-open windows meet
	// exactly, no overlap and no gap). Default: the evolve moment.
	if effectiveAt.IsZero() {
		effectiveAt = now
	} else if err := validateStampTime(effectiveAt, oldEng.EffectiveValidFrom()); err != nil {
		// An explicit effective_at must be a real instant after the predecessor
		// began: epoch would collapse the successor's ValidFrom to CreatedAt AND
		// leave the predecessor's ValidUntil raw-0 (= open), silently un-superseding.
		return storage.ULID{}, err
	}
	if concept == "" {
		concept = oldEng.Concept
	}
	// MemoryType and TypeLabel are inherited alongside Concept and Tags. Evolve
	// exposes no parameter for either, so leaving them unset reset the type to
	// the MemoryType zero value, Fact — a decision or procedure was silently
	// relabelled a fact on every evolve, and again at each hop down a supersede
	// chain (issue #653).
	//
	// Summary is deliberately NOT inherited. It is content-derived (see the
	// field doc on storage.Engram), and Evolve replaces the content, so the
	// predecessor's summary is false of the successor. Leaving it empty is also
	// what lets the summarize stage regenerate it: engramHasSummary and the
	// retroactive processor both treat a non-empty Summary as caller-authoritative
	// and skip the stage, which would pin the stale text permanently and leave
	// KeyPoints — produced only by that stage — empty forever.
	// Importance: explicit override wins; otherwise inherit the predecessor's
	// stored (explicit-only) value. Unset stays unset — never freeze the
	// use-time type-table default into the record.
	newImportance := oldEng.Importance
	if importance != nil {
		newImportance = importanceFromRequest(importance)
	}
	newEng := &storage.Engram{
		ID:         newULID,
		Concept:    concept,
		Content:    newContent,
		Tags:       oldEng.Tags,
		MemoryType: oldEng.MemoryType,
		TypeLabel:  oldEng.TypeLabel,
		Importance: newImportance,
		Confidence: 1.0,
		Stability:  30.0,
		State:      storage.StateActive,
		CreatedAt:  now,
		UpdatedAt:  now,
		LastAccess: now,
		Embedding:  embedding,
		Trust:      storage.TrustInferred, // all new MCP writes default to inferred
		ValidFrom:  effectiveAt,
	}

	// Carry the predecessor's entity links and relationship records forward
	// (#622): Evolve wrote no 0x20/0x23 links and no 0x21/0x26 relationship
	// records for the successor, so every evolved memory silently vanished
	// from FindByEntity and entity scoring from the moment it was evolved.
	// The link and relationship keys are queued into the same atomic batch as
	// the engram writes; the mention-count and co-occurrence ledgers are funded
	// post-commit (see below). Caller-supplied inline entities suppress the
	// carry entirely — they replace the set rather than merging with it.
	var carriedEntities []string
	var carriedRels []storage.RelationshipRecord
	if len(entities) == 0 {
		if err := e.store.ScanEngramEntities(ctx, wsPrefix, oldULID, func(name string) error {
			carriedEntities = append(carriedEntities, name)
			return nil
		}); err != nil {
			return storage.ULID{}, fmt.Errorf("evolve: scan predecessor entities: %w", err)
		}
		if err := e.store.ScanEngramRelationships(ctx, wsPrefix, oldULID, func(rec storage.RelationshipRecord) error {
			carriedRels = append(carriedRels, rec)
			return nil
		}); err != nil {
			return storage.ULID{}, fmt.Errorf("evolve: scan predecessor relationships: %w", err)
		}
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

	// The successor's provenance entry carries what changed and why, not just
	// the verb: an "evolve" entry with only a timestamp, a source and the word
	// "evolve" records the one thing the reader already knew. It is recorded on
	// the SUCCESSOR because that is the engram a reader holds after an evolve
	// (the predecessor is soft-deleted and hidden from the present), and
	// predecessor_id is the exact mirror of read's superseded_by. reason is
	// passed through verbatim — empty when the caller gave none, never
	// backfilled with a plausible-looking string.
	evolveDetails := &provenance.Details{
		PredecessorID: oldULID.String(),
		Reason:        reason,
		EffectiveAt:   &effectiveAt,
	}
	if err := batch.WriteEngramOpDetails(ctx, wsPrefix, newEng, "evolve", evolveDetails); err != nil {
		return storage.ULID{}, fmt.Errorf("evolve: batch write new engram: %w", err)
	}
	if err := batch.WriteAssociation(ctx, wsPrefix, newULID, oldULID, supersedes); err != nil {
		return storage.ULID{}, fmt.Errorf("evolve: batch write association: %w", err)
	}
	// Supersede = soft-delete (hidden from the present) + ValidUntil stamp
	// (re-opens the record for as_of time-travel only) in the same atomic
	// batch. Invalidation is always a stamp, never a delete (COG-19).
	if err := batch.SupersedeEngram(ctx, wsPrefix, oldULID, effectiveAt); err != nil {
		return storage.ULID{}, fmt.Errorf("evolve: batch supersede old: %w", err)
	}
	for _, name := range carriedEntities {
		if err := batch.WriteEntityEngramLink(ctx, wsPrefix, newULID, name); err != nil {
			return storage.ULID{}, fmt.Errorf("evolve: batch carry entity link: %w", err)
		}
	}
	for _, rec := range carriedRels {
		if err := batch.WriteRelationshipRecord(ctx, wsPrefix, newULID, rec); err != nil {
			return storage.ULID{}, fmt.Errorf("evolve: batch carry relationship: %w", err)
		}
	}
	// Upsert re-point (#556): when called from writeUpsert's content-change
	// path, move the 0x30 forward index to the successor in this same atomic
	// batch — the re-point must commit (or roll back) with the evolve itself.
	if upsertKeyHash != nil {
		batch.RepointUpsertKey(wsPrefix, *upsertKeyHash, newULID)
	}
	if err := batch.Commit(); err != nil {
		return storage.ULID{}, fmt.Errorf("evolve: batch commit: %w", err)
	}

	// Fund the ledgers for the carried links. The mention-count and
	// co-occurrence ledgers are one increment per link key created, one
	// decrement per link key destroyed: DeleteEngram decrements both
	// unconditionally for every link it removes, so a carried link with no
	// matching increment becomes an unfunded liability that the pruner cashes
	// in — hard-delete the predecessor and MentionCount under-reads while the
	// successor is still linked, and a co-occurrence pair drops to zero and
	// vanishes from ScanEntityClusters while both entities are still mentioned
	// together. Done post-commit, mirroring DeleteEngram's post-commit
	// decrements: a crash here leaves counts slightly low with the links
	// intact, the mirror image of DeleteEngram's slightly-high stale case.
	for _, name := range carriedEntities {
		if err := e.store.IncrementEntityMentionCount(ctx, wsPrefix, name); err != nil {
			slog.Warn("engine: evolve: failed to increment mention count for carried entity", "entity", name, "engram", newULID.String(), "err", err)
		}
	}
	// Cap mirrors DeleteEngram's maxCoOccurrenceEntities. Beyond the cap the
	// ledger is best-effort on both sides — DeleteEngram's first-50 comes from
	// map iteration, so the truncated subsets need not be identical — matching
	// the pre-existing behaviour for >50-entity engrams rather than fixing it.
	const maxCoOccurrenceEntities = 50
	coNames := carriedEntities
	if len(coNames) > maxCoOccurrenceEntities {
		slog.Warn("engine: evolve: engram has unusually many carried entities, co-occurrence funding capped",
			"engram", newULID.String(), "entity_count", len(carriedEntities), "cap", maxCoOccurrenceEntities)
		coNames = coNames[:maxCoOccurrenceEntities]
	}
	for i := 0; i < len(coNames); i++ {
		for j := i + 1; j < len(coNames); j++ {
			if err := e.store.IncrementEntityCoOccurrence(ctx, wsPrefix, coNames[i], coNames[j]); err != nil {
				slog.Warn("engine: evolve: failed to increment co-occurrence for carried pair", "a", coNames[i], "b", coNames[j], "engram", newULID.String(), "err", err)
			}
		}
	}

	// Caller-supplied inline entities replace the carry: the update changed what
	// the memory is about. These are genuinely new mentions, so they take the
	// same path remember's inline entities do — record upsert (which funds the
	// mention count), link, and co-occurrence bookkeeping included.
	if len(entities) > 0 {
		var linkedEntityNames []string
		for _, ent := range entities {
			typ := strings.ToLower(strings.TrimSpace(ent.Type))
			if typ == "" {
				typ = "other"
			}
			record := storage.EntityRecord{
				Name:       ent.Name,
				Type:       typ,
				Confidence: 1.0,
			}
			if err := e.store.UpsertEntityRecord(ctx, wsPrefix, record, "inline"); err != nil {
				slog.Warn("engine: evolve: failed to store inline entity", "name", ent.Name, "err", err)
				continue
			}
			if err := e.store.WriteEntityEngramLink(ctx, wsPrefix, newULID, ent.Name); err != nil {
				slog.Warn("engine: evolve: failed to link inline entity", "name", ent.Name, "err", err)
				continue
			}
			linkedEntityNames = append(linkedEntityNames, ent.Name)
		}
		for i := 0; i < len(linkedEntityNames); i++ {
			for j := i + 1; j < len(linkedEntityNames); j++ {
				if err := e.store.IncrementEntityCoOccurrence(ctx, wsPrefix, linkedEntityNames[i], linkedEntityNames[j]); err != nil {
					slog.Warn("engine: evolve: failed to increment co-occurrence", "vault", vault, "engram", newULID.String(), "entity_a", linkedEntityNames[i], "entity_b", linkedEntityNames[j], "err", err)
				}
				if err := e.store.UpsertRelationshipRecord(ctx, wsPrefix, newULID, storage.RelationshipRecord{
					FromEntity: linkedEntityNames[i],
					ToEntity:   linkedEntityNames[j],
					RelType:    "co_occurs_with",
					Weight:     0.3,
					Source:     "co-occurrence",
				}); err != nil {
					slog.Warn("engine: evolve: failed to upsert co_occurs_with relationship", "vault", vault, "engram", newULID.String(), "entity_a", linkedEntityNames[i], "entity_b", linkedEntityNames[j], "err", err)
				}
			}
		}
	}

	// Mark entity extraction complete whenever the successor carries a curated
	// set (carried or inline), so the retroactive enrichment provider does not
	// re-extract over it.
	if len(carriedEntities) > 0 || len(entities) > 0 {
		existingFlags, _ := e.store.GetDigestFlags(ctx, plugin.ULID(newULID))
		if err := e.store.SetDigestFlag(ctx, newULID, existingFlags|plugin.DigestEntities); err != nil {
			slog.Warn("engine: evolve: failed to set DigestEntities flag", "id", newULID.String(), "err", err)
		}
	}

	// ── Content-hash bookkeeping: delete old mapping, add new mapping ──
	oldHash := storage.ContentHash(oldEng.Content)
	if err := e.store.DeleteContentHash(ctx, wsPrefix, oldHash); err != nil {
		slog.Warn("engine: evolve: failed to delete old content hash", "id", oldID, "err", err)
	}
	newHash := storage.ContentHash(newContent)
	if err := e.store.PutContentHash(ctx, wsPrefix, newHash, newULID); err != nil {
		slog.Warn("engine: evolve: failed to store new content hash", "id", newULID.String(), "err", err)
	}

	// Persist vault name (idempotent).
	if err := e.store.WriteVaultName(wsPrefix, vault); err != nil {
		slog.Warn("engine: failed to persist vault name", "vault", vault, "err", err)
	}

	// When the caller provided an embedding, insert into HNSW inline first and
	// only mark DigestEmbed on success — a refused insert (#582) leaves the
	// engram for the retroactive processor to embed server-side.
	if len(embedding) > 0 {
		if err := e.hnswRegistry.Insert(ctx, wsPrefix, [16]byte(newULID), embedding); err != nil {
			slog.Warn("engine: evolve: client embedding not inserted into HNSW; engram left for the retroactive processor", "id", newULID.String(), "err", err)
		} else {
			existing, _ := e.store.GetDigestFlags(ctx, plugin.ULID(newULID))
			if err := e.store.SetDigestFlag(ctx, newULID, existing|plugin.DigestEmbed); err != nil {
				slog.Warn("engine: evolve: failed to set DigestEmbed flag", "id", newULID.String(), "err", err)
			}
		}
	}

	// Notify background processors (e.g. the retroactive embed/summarize worker)
	// that a new engram exists — UNCONDITIONALLY, exactly as Write does.
	// Without this, evolve was the ONE write path that never woke the
	// processor: the successor waited for its poll ticker, which backs off
	// geometrically to a 3-MINUTE ceiling on an idle vault, so on a quiet vault
	// a freshly evolved memory could be semantically unindexed for up to three
	// minutes after the commit. That is the largest single contributor to the
	// fresh-evolve retrieval window in #763.
	//
	// Not conditioned on whether a caller-supplied embedding was just indexed
	// inline: the same processor also runs the SUMMARIZE stage, and Evolve
	// deliberately does not inherit the predecessor's summary (it is
	// content-derived and the content just changed), so an evolved memory has
	// work waiting for the processor even when its vector is already in HNSW.
	// Matching Write's unconditional call is also the property that keeps the
	// two write paths from drifting again.
	if fn, ok := e.onWrite.Load().(func()); ok && fn != nil {
		fn()
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

// Consolidate merges multiple engrams into a single new engram and SUPERSEDES
// the originals. Returns a ConsolidateResult with the new ID, the superseded
// (Archived) IDs, and any non-fatal warnings.
//
// Consolidation is content REPLACEMENT, and it declares that the same way its
// siblings do (issue #779): each source gets a `RelSupersedes` edge from the
// merged engram plus a soft-delete and a CLOSED `ValidUntil` stamp, in one
// atomic batch per source — the exact shape `EvolveAt` writes.
//
// Before this, sources were archived with a plain soft-delete: no edge, an OPEN
// `ValidUntil`. That is the signature COG-28 reads as "trash, not history", so
// consolidation was the ONE content-replacing operation excluded from the
// mechanism built to close exactly this hole — a query phrased against a
// source's wording reached a record the lifecycle cut discards, with nothing
// declaring where the content went, and recall returned nothing about the fact
// the merge was meant to PRESERVE. Unlike the embed-lag race #779 describes
// (already closed: `Write` fires `onWrite`, #767), that hole was permanent.
//
// Two consequences of declaring it, both deliberate and both user-visible:
//
//   - Sources become time-travellable lineage. `as_of` before the merge and
//     `include_invalid` now return them, and `muninn_read` reports
//     `superseded_by` = the merged id instead of a bare soft-delete.
//   - Their FTS postings are KEPT, exactly as evolve keeps a predecessor's.
//     That is what lets the old wording reach the source and be redirected to
//     the merged memory; deleting them (what the old `Forget` path did) is
//     precisely what made the hole unreachable from the lexical side.
//
// A JOIN is not a FORK: many sources point at ONE merged engram, so each source
// still has exactly one superseder and every chain resolves.
func (e *Engine) Consolidate(ctx context.Context, vault string, ids []string, mergedContent string) (*ConsolidateResult, error) {
	if err := e.refuseWrite(ctx); err != nil {
		return nil, err
	}
	if len(ids) > 50 {
		return nil, fmt.Errorf("consolidate: too many ids (max 50, got %d)", len(ids))
	}
	wsPrefix := e.store.ResolveVaultPrefix(vault)
	mergedResp, err := e.Write(ctx, &mbp.WriteRequest{
		Vault:   vault,
		Concept: "Consolidated memory",
		Content: mergedContent,
	})
	if err != nil {
		return nil, fmt.Errorf("consolidate: write merged: %w", err)
	}
	mergedULID, err := storage.ParseULID(mergedResp.ID)
	if err != nil {
		return nil, fmt.Errorf("consolidate: parse merged id: %w", err)
	}

	// NEVER archive the merged result itself. Write is exact-content deduplicated,
	// so when mergedContent matches an input byte-for-byte — the NATURAL case,
	// because the obvious merged text for a set of near-duplicates is usually one
	// of them verbatim — Write returns THAT input's existing id rather than
	// creating a new engram. Archiving every input then soft-deletes the very
	// engram just returned as the survivor, and the fact disappears from recall
	// while the response still reports success and names the (now dead) id.
	//
	// Verified before this guard: consolidate(ids=[A,B], merged=<A's content>)
	// returned {"id": A, "archived": [A, B]} and left BOTH soft-deleted, so a
	// caller using the merge tool exactly as documented lost the memory.
	var archived []string
	var warnings []string
	// Compare PARSED ULIDs, not raw strings: ULID parsing is case-insensitive
	// and Forget accepts lowercase ids, so a lowercase input whose content
	// matched the merge text slipped past a string comparison and the survivor
	// was archived anyway — the same data loss through a one-character-class
	// gap (adversarial review of #754, finding 7).
	now := time.Now()
	for _, id := range ids {
		inputULID, parseErr := storage.ParseULID(id)
		if parseErr == nil {
			if inputULID == mergedULID {
				// This input IS the merged result (dedup hit). Keep it alive; it
				// is the consolidated memory the caller is being handed.
				continue
			}
		} else {
			// An unparseable input can be neither compared to the merged id nor
			// superseded. Say so instead of silently dropping it.
			warnings = append(warnings, fmt.Sprintf("failed to supersede %s: %v", id, parseErr))
			continue
		}

		// Supersede = declared edge + soft-delete + closed ValidUntil stamp, in
		// ONE atomic batch per source. Per-source rather than one batch for all
		// of them so a single unreadable id degrades to a warning (the previous
		// per-id Forget behaviour) instead of failing the whole merge — and a
		// crash mid-loop leaves each already-processed source fully declared,
		// never archived-without-an-edge. The remaining sources stay live beside
		// the merged engram, which is exactly what a crash between the merged
		// Write and the old archive loop already produced.
		//
		// A source that was ALREADY superseded gains a SECOND superseder, which
		// the chain walk reports as a fork and refuses to resolve — the designed
		// response to two rival replacements, and reachable only by a caller who
		// consolidates a memory they have already evolved away. Deliberately not
		// pre-screened: skipping such a source would report it as archived while
		// leaving it live and unlinked, which is the response-asserts-something-
		// untrue class the survivor guard above exists to prevent. Pinned by
		// TestConsolidate_AlreadySupersededSourceForksAndRefuses.
		supersedes := &storage.Association{
			TargetID:      inputULID,
			RelType:       storage.RelSupersedes,
			Weight:        1.0,
			Confidence:    1.0,
			CreatedAt:     now,
			LastActivated: int32(now.Unix()),
		}
		supersedeErr := func() error {
			batch := e.store.NewBatch()
			defer batch.Discard()
			if err := batch.WriteAssociation(ctx, wsPrefix, mergedULID, inputULID, supersedes); err != nil {
				return err
			}
			if err := batch.SupersedeEngram(ctx, wsPrefix, inputULID, now); err != nil {
				return err
			}
			return batch.Commit()
		}()
		if supersedeErr != nil {
			warnings = append(warnings, fmt.Sprintf("failed to supersede %s: %v", id, supersedeErr))
			continue
		}
		archived = append(archived, id)
	}

	return &ConsolidateResult{MergedID: mergedULID, Archived: archived, Warnings: warnings}, nil
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

// DailyCount holds a single day's engram creation count.
type DailyCount struct {
	Date  string
	Count int64
}

// ActivityCounts returns per-day engram counts between since and until
// (inclusive) for a vault. Days are bucketed in the timezone of the since
// argument's Location, and until is assumed to share that location; pass
// UTC-located times for UTC-day buckets.
func (e *Engine) ActivityCounts(ctx context.Context, vault string, since, until time.Time) ([]DailyCount, error) {
	ws := e.store.ResolveVaultPrefix(vault)
	counts, err := e.store.CountEngramsByDay(ctx, ws, since, until)
	if err != nil {
		return nil, err
	}
	// Build a contiguous day list so the caller always gets every day in range.
	// Iterate calendar days in the location of the since argument so the day
	// boundaries match how CountEngramsByDay bucketed the engrams. until is
	// taken to share that location (the REST handler builds both together), so
	// its own Location is intentionally not consulted. AddDate advances by a
	// calendar day (not a fixed 24h), keeping the list correct across
	// daylight-saving transitions.
	loc := since.Location()
	first := time.Date(since.Year(), since.Month(), since.Day(), 0, 0, 0, 0, loc)
	last := time.Date(until.Year(), until.Month(), until.Day(), 0, 0, 0, 0, loc)
	var result []DailyCount
	for d := first; !d.After(last); d = d.AddDate(0, 0, 1) {
		day := d.Format("2006-01-02")
		result = append(result, DailyCount{Date: day, Count: counts[day]})
	}
	return result, nil
}

// Decide records an explicit decision with rationale, alternatives, and supporting evidence.
// It returns a DecideResult containing the new engram ID and any non-fatal
// evidence-link warnings. The decision is always committed; evidence linking
// is best-effort (a bad evidence ID produces a warning, not a failure).
func (e *Engine) Decide(ctx context.Context, vault, decision, rationale string, alternatives, evidenceIDs []string) (*DecideResult, error) {
	if err := e.refuseWrite(ctx); err != nil {
		return nil, err
	}
	// The body must STATE the decision, not just the reasoning behind it.
	// Previously content was rationale(+alternatives) only, so every consumer
	// that reads the body — a recall snippet, an export, a summarizer, a human
	// — got the argument without ever being told what was decided.
	content := decision + "\n\nRationale:\n" + rationale
	if len(alternatives) > 0 {
		content += "\n---\nAlternatives:\n" + strings.Join(alternatives, "\n")
	}

	resp, err := e.Write(ctx, &mbp.WriteRequest{
		Vault:   vault,
		Concept: decision,
		Content: content,
		Tags:    []string{"decision"},
		// A decision-recording tool must produce a decision-TYPED memory.
		// Left unset, MemoryType took the uint8 zero value (fact), which
		// silently downgraded derived importance from the decision tier (0.6)
		// to the fact tier (0.4) and hid the record from every type-based
		// filter — the same silent-downgrade class as #742/#743/#745.
		MemoryType: uint8(storage.TypeDecision),
		TypeLabel:  storage.TypeDecision.String(),
	})
	if err != nil {
		return nil, fmt.Errorf("decide: write: %w", err)
	}

	var warnings []string
	for _, eid := range evidenceIDs {
		if _, err := e.Link(ctx, &mbp.LinkRequest{
			SourceID: resp.ID,
			TargetID: eid,
			RelType:  uint16(storage.RelSupports),
			Weight:   1.0,
			Vault:    vault,
		}); err != nil {
			warnings = append(warnings, fmt.Sprintf("failed to link evidence %s: %v", eid, err))
		}
	}

	decideULID, err := storage.ParseULID(resp.ID)
	if err != nil {
		return nil, fmt.Errorf("decide: parse id: %w", err)
	}
	return &DecideResult{ID: decideULID, Concept: decision, Warnings: warnings}, nil
}

// RecordAccess increments the access count and updates the last-accessed timestamp
// for the engram identified by id in the given vault.
//
// Thin wrapper over storage.TouchAccess (#682) — previously this did its own
// unlocked GetEngram→UpdateMetadata read-modify-write, which raced
// CompareAndSet/DeleteEngram on the same id (STO-2). TouchAccess holds the
// per-engram stripe lock across the whole RMW.
func (e *Engine) RecordAccess(ctx context.Context, vault, id string) error {
	if err := e.refuseWrite(ctx); err != nil {
		return err
	}
	ws := e.store.ResolveVaultPrefix(vault)
	ulid, err := storage.ParseULID(id)
	if err != nil {
		return fmt.Errorf("record_access: parse id: %w", err)
	}
	if err := e.store.TouchAccess(ctx, ws, ulid); err != nil {
		return fmt.Errorf("record_access: %w", err)
	}
	return nil
}

// ResolveVaultPlasticity returns the resolved plasticity config for a vault,
// falling back to defaults when no authStore is configured.
// VaultPlasticityConfig returns the vault's RAW plasticity config (nil when
// unset or unreadable). Callers that need resolved values want
// ResolveVaultPlasticity; this exists for the narrow case of distinguishing
// "the operator set this field explicitly" from "it came from the preset".
func (e *Engine) VaultPlasticityConfig(vaultName string) *auth.PlasticityConfig {
	if e.authStore == nil {
		return nil
	}
	vaultCfg, err := e.authStore.GetVaultConfig(vaultName)
	if err != nil {
		return nil
	}
	return vaultCfg.Plasticity
}

func (e *Engine) ResolveVaultPlasticity(vaultName string) auth.ResolvedPlasticity {
	if e.authStore != nil {
		vaultCfg, err := e.authStore.GetVaultConfig(vaultName)
		if err == nil {
			return auth.ResolvePlasticity(vaultCfg.Plasticity)
		}
	}
	return auth.ResolvePlasticity(nil)
}

// resolveSemanticBaseline resolves the COG-26 semantic-abstention baseline b
// for a vault: the anisotropy noise floor rescaled into the semantic
// relevance blend so near-baseline cosine (out-of-domain noise) contributes
// ~0 to contentMatch instead of clearing the recall threshold on noise alone.
//
// Resolution order (explicit config always wins, never silently substituted —
// #582/#585/#589):
//  1. resolved.SemanticFloorOverride (per-vault plasticity override). An
//     explicit 0 disables the floor. An out-of-range value (>=1, which would
//     zero every match) is rejected with a WARN and falls through to registry
//     resolution rather than silently abstaining everything.
//  2. The vault's recorded embed model (store.GetEmbedModel, set by a future
//     re-embed/model-tracking increment) if non-empty, else the engine's
//     process-wide configured embed model (EngineConfig.EmbedModelName,
//     resolved once at startup from the active provider).
//  3. internal/plugin/embed.NoiseBaseline(model) registry lookup.
//
// A model with no registry entry — including "" (model unknown: no per-vault
// marker set and no process-wide model recorded, e.g. embedded/library use
// without EmbedModelName wired) — resolves to 0 (identity transform) plus a
// one-time-per-model WARN: an uncalibrated model must never receive a guessed
// floor, per COG-26 and the #582/#585/#589 explicit-config rule.
func (e *Engine) resolveSemanticBaseline(vaultName string, ws [8]byte, resolved auth.ResolvedPlasticity) float64 {
	if resolved.SemanticFloorOverride != nil {
		b := *resolved.SemanticFloorOverride
		if b >= 0 && b < 1 {
			return b
		}
		slog.Warn("semantic_floor override out of range [0,1), ignoring and falling back to the embed-model registry",
			"vault", vaultName, "semantic_floor", b)
	}

	model := ""
	if e.store != nil {
		if m, err := e.store.GetEmbedModel(ws); err == nil {
			model = m
		}
	}
	if model == "" {
		model = e.embedModelName
	}

	if b, ok := embedpkg.NoiseBaseline(model); ok {
		return b
	}

	// Unknown/unregistered model: identity transform, WARN once per distinct
	// model string per process so a busy vault doesn't spam logs per query.
	if _, alreadyWarned := e.warnedUnknownEmbedModels.LoadOrStore(model, struct{}{}); !alreadyWarned {
		slog.Warn("semantic abstention floor (COG-26): no calibrated noise baseline for this embed model, using identity transform (no floor) — semantic-only nonsense may clear the recall threshold",
			"vault", vaultName, "embed_model", model)
	}
	return 0
}

// semanticFloorExplicitlyDisabled reports whether this vault's operator
// explicitly set `semantic_floor: 0` — the documented way to disable the
// COG-26 floor. Deliberately narrower than "the resolved baseline is 0":
// an unset override, a positive override, and an out-of-range override that
// resolveSemanticBaseline rejects (WARN + registry fallthrough) are all
// false. Kept beside resolveSemanticBaseline so the two read the same
// override field and cannot drift.
func semanticFloorExplicitlyDisabled(resolved auth.ResolvedPlasticity) bool {
	return resolved.SemanticFloorOverride != nil && *resolved.SemanticFloorOverride == 0
}

// PruneVault prunes a vault according to its resolved MaxEngrams and RetentionDays policy.
// It uses hard-delete (removes all secondary indexes) to ensure pruned engrams do not
// persist in the relevance bucket index and cause an infinite prune loop.
// Returns the number of engrams pruned.
func (e *Engine) PruneVault(ctx context.Context, vaultName string) (int64, error) {
	if err := e.refuseWrite(ctx); err != nil {
		return 0, err
	}
	if !e.beginVaultOp() {
		return 0, fmt.Errorf("engine is shutting down")
	}
	defer e.endVaultOp()

	opCtx, stop := e.vaultOpContext(ctx)
	defer stop()

	mu := e.getVaultMutex(vaultName)
	mu.Lock()
	defer mu.Unlock()

	resolved := e.ResolveVaultPlasticity(vaultName)
	ws := e.store.ResolveVaultPrefix(vaultName)

	var pruned int64

	// MaxEngrams: two-phase prune — use stale relevance index as pre-filter, then
	// re-rank candidates by real ACT-R base-level score to delete the worst engrams.
	if resolved.MaxEngrams > 0 {
		count := e.store.GetVaultCount(opCtx, ws)
		excess := count - int64(resolved.MaxEngrams)
		if excess > 0 {
			// Phase 1: fetch heuristic candidates from the relevance bucket index.
			// Over-fetch to compensate for index staleness (new engrams start at relevance=0).
			topK := int(excess) * 2
			var candidates []storage.ULID
			for attempt := 0; attempt < 2; attempt++ {
				ids, err := e.store.LowestRelevanceIDs(opCtx, ws, topK)
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
			//
			// COG-20: candidates with EffectiveImportance >= HighImportanceFloor
			// are exempt from this retrieval-strength prune path — an important
			// memory is never deleted just because it is cold. The exemption
			// deliberately performs NO validity check: an expired-but-important
			// fact stays findable via as_of (valid-time gives time travel;
			// importance guarantees the destination still exists). RetentionDays
			// below remains an authoritative age policy and is NOT exempt.
			type scoredCandidate struct {
				id    storage.ULID
				score float64
			}
			const actrDecay = 0.5
			now := time.Now()
			scored := make([]scoredCandidate, 0, len(candidates))
			var exempted int

			if len(candidates) > 0 {
				metas, err := e.store.GetMetadata(opCtx, ws, candidates)
				if err != nil {
					// Degrade loudly, never importance-blind: without metadata we
					// cannot honor the COG-20 exemption, so skip this MaxEngrams
					// pass entirely (the prune worker retries in ~60s) instead of
					// deleting candidates in index order.
					slog.Warn("prune vault: metadata load failed; skipping MaxEngrams prune this cycle (COG-20 exemption needs metadata)",
						"vault", vaultName, "excess", excess, "err", err)
					metas = nil
				}
				if metas != nil {
					for i, id := range candidates {
						var b float64
						if i < len(metas) && metas[i] != nil {
							m := metas[i]
							if m.EffectiveImportance() >= storage.HighImportanceFloor {
								exempted++
								continue // COG-20: never pruned by the MaxEngrams path
							}
							lastAccess := m.LastAccess
							if storage.IsUnsetTimestamp(lastAccess) {
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
			}

			// Delete the bottom `excess` by ACT-R base-level score (or all available if fewer).
			toDelete := int(excess)
			if toDelete > len(scored) {
				toDelete = len(scored)
			}
			for i := 0; i < toDelete; i++ {
				id := scored[i].id
				// Read before deleting: FTS cleanup needs the indexed fields,
				// which are gone once the 0x01 record is.
				victim, _ := e.store.GetEngram(opCtx, ws, id)
				if err := e.store.DeleteEngram(opCtx, ws, id); err != nil {
					slog.Debug("prune vault: hard-delete failed", "vault", vaultName, "id", id, "err", err)
					continue
				}
				// STO-12: the pruner reaches DeleteEngram directly and cleaned
				// neither FTS nor HNSW, so it leaked dangling edges through
				// BOTH amplifiers with no operator action at all.
				e.cleanSearchIndexesAfterHardDelete(ws, [16]byte(id), victim)
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

			// Degrade loudly, no loop (COG-15/COG-20): if importance exemptions
			// left the vault over MaxEngrams, say so — a vault marking most of
			// its memories >= HighImportanceFloor cannot shrink via this path.
			if pruned < excess && exempted > 0 {
				slog.Warn("prune vault: high-importance exemptions left vault over MaxEngrams",
					"vault", vaultName, "excess", excess, "pruned", pruned, "exempted", exempted,
					"floor", storage.HighImportanceFloor)
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
		ids, err := e.store.EngramIDsByCreatedRange(opCtx, ws, epoch, cutoff, retentionBatchSize)
		if err != nil {
			return pruned, fmt.Errorf("prune vault %s (retention scan): %w", vaultName, err)
		}
		for _, id := range ids {
			victim, _ := e.store.GetEngram(opCtx, ws, id)
			if err := e.store.DeleteEngram(opCtx, ws, id); err != nil {
				slog.Debug("prune vault: retention hard-delete failed", "vault", vaultName, "id", id, "err", err)
				continue
			}
			// STO-12: same leak as the MaxEngrams path above.
			e.cleanSearchIndexesAfterHardDelete(ws, [16]byte(id), victim)
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
// MaxEngrams, RetentionDays, and association-decay policies. Runs every 60s with ±5s jitter.
// Association decay is cadence-independent (COG-28), so this period is a
// responsiveness choice only — it is not a term in the decay rate (#762).
func (e *Engine) runPruneWorker() {
	defer close(e.pruneDone)
	defer func() {
		if r := recover(); r != nil {
			if !storage.IsClosedPanic(r) {
				slog.Error("engine: prune worker panicked", "panic", r)
			}
		}
	}()
	jitter := time.Duration(rand.Intn(10)) * time.Second
	timer := time.NewTimer(60*time.Second + jitter)
	defer timer.Stop()
	for {
		select {
		case <-timer.C:
			vaults, _ := e.ListVaults(e.stopCtx)
			for _, vaultName := range vaults {
				resolved := e.ResolveVaultPlasticity(vaultName)
				if resolved.MaxEngrams > 0 || resolved.RetentionDays > 0 {
					if _, err := e.PruneVault(e.stopCtx, vaultName); err != nil {
						slog.Debug("vault prune failed", "vault", vaultName, "err", err)
					}
				}
			}
			e.decayAllVaults(e.stopCtx, vaults)
			jitter = time.Duration(rand.Intn(10)) * time.Second
			timer.Reset(60*time.Second + jitter)
		case <-e.stopCtx.Done():
			return
		}
	}
}

// decayAllVaults runs one association-decay sweep across the given vaults,
// honouring each vault's plasticity.
//
// It is a method, not a block inside runPruneWorker, so the gate below can be
// unit-tested directly. That matters: the pre-review version of this code had
// the gate inlined in the worker loop, and DELETING it left the whole engine
// suite green — the gate was wiring nobody tested (review finding B2).
//
// The gate: association decay must never run before the one-shot full-weight
// repair has swept cleanly (#756). Post-#762 decay no longer deletes or clamps
// a weight-0 pre-fix key on its own — the drop guard skips an edge whose
// ceiling sits at or above its stored weight, and a zero weight is never below
// it. What still destroys the repair's evidence is the ADOPTION path: a pre-fix
// key whose value carries neither lastActivated nor createdAt gets stamped,
// which rewrites the fwd/rev keys and sets the 0x14 index to 0.0 — overwriting
// the weight-1.0 index entry that is the repair's only disambiguator. A pass
// that errored keeps this shut for the process lifetime (see
// runLegacyFullWeightAssocRepair's failure policy), so decay is parked rather
// than allowed to destroy repairable state.
func (e *Engine) decayAllVaults(ctx context.Context, vaults []string) {
	// #760: association decay writes are keyed by weight (AssocFwdKey/AssocRevKey
	// encode the float32 weight itself), and the decay formula (COG-28) is an
	// independent recompute from stored peakWeight/lastActivated rather than an
	// incremental step. Two nodes evaluating it a tick apart land on two
	// different float32 weights for the same edge — two different LIVE keys,
	// not one edge updated twice. Running this on every cluster node
	// (leader AND followers) therefore does not converge; it leaves stale
	// duplicate keys behind every pass. Followers already receive the leader's
	// decayed weights through the ordinary replication stream, so they must
	// not ALSO decay independently. This reuses the single-writer gate
	// (e.writeGate, #596) rather than adding a second leadership signal — the
	// same pattern the periodic replication-log prune already uses
	// (ClusterCoordinator.startPeriodicPrune: "if !c.IsLeader() { continue }").
	// A nil gate (standalone, non-cluster) decays exactly as before.
	if err := e.refuseNonLeaderWrite(); err != nil {
		slog.Debug("assoc decay skipped: not the cluster leader", "err", err)
		return
	}
	if !e.assocWeightRepairComplete() {
		slog.Debug("assoc decay deferred: full-weight repair pass has not completed cleanly")
		return
	}
	for _, vaultName := range vaults {
		resolved := e.ResolveVaultPlasticity(vaultName)
		// COG-16 unchanged: AssocDecayFactor is still the gate. It is now only
		// a switch — the rate lives in AssocHalfLifeDays (#762).
		if !resolved.HebbianEnabled || resolved.AssocDecayFactor <= 0 {
			continue
		}
		halfLife := resolved.AssocHalfLife()
		if halfLife <= 0 {
			// Decay is switched on but carries no rate. Skip rather than
			// substitute a default: an unconfigured rate is not a licence to
			// pick one (principle #1). WARN once so it is not silent — and
			// truthfully: a legacy factor >= 1 gets its own message (it is
			// NOT a reinterpretation; nothing was derived from it), distinct
			// from the generic "no rate configured" case.
			e.warnAssocDecayOnce(vaultName, func() {
				if f := e.explicitLegacyAssocFactor(vaultName); f != nil && *f >= 1 {
					slog.Warn("assoc decay enabled but legacy assoc_decay_factor >= 1 carries no rate (a per-pass multiplier of 1 never moved a weight); no half-life resolved, decay skipped for this vault",
						"vault", vaultName, "assoc_decay_factor", *f,
						"fix", "set plasticity assoc_half_life_days to enable decay, or assoc_decay_factor 0 to switch it off explicitly")
					return
				}
				slog.Warn("assoc decay enabled but no half-life resolved; decay skipped for this vault",
					"vault", vaultName, "fix", "set plasticity assoc_half_life_days")
			})
			continue
		}
		e.warnLegacyAssocDecayFactor(vaultName, resolved)
		ws := e.store.ResolveVaultPrefix(vaultName)
		removed, err := e.decayAssocWeights(ctx, ws, halfLife, resolved.AssocMinWeight, resolved.ArchiveThreshold)
		if err != nil {
			slog.Debug("assoc decay failed", "vault", vaultName, "err", err)
		} else if removed > 0 {
			slog.Info("assoc decay pruned edges", "vault", vaultName, "removed", removed)
		}
	}
}

// warnAssocDecayOnce runs fn at most once per vault for the process lifetime.
// The decay sweep runs every ~60s; a per-pass WARN would be a log flood, and a
// flood is functionally silent.
func (e *Engine) warnAssocDecayOnce(vaultName string, fn func()) {
	if _, loaded := e.assocDecayWarned.LoadOrStore(vaultName, struct{}{}); loaded {
		return
	}
	fn()
}

// explicitLegacyAssocFactor returns the vault's explicitly-configured
// assoc_decay_factor when it is the operative rate source — i.e. set, with no
// explicit half-life overriding it. Returns nil otherwise.
func (e *Engine) explicitLegacyAssocFactor(vaultName string) *float32 {
	cfg := e.VaultPlasticityConfig(vaultName)
	if cfg == nil || cfg.AssocDecayFactor == nil {
		return nil
	}
	if cfg.AssocHalfLifeDays != nil && *cfg.AssocHalfLifeDays != 0 {
		return nil // explicit half-life wins; the factor is only a switch
	}
	return cfg.AssocDecayFactor
}

// warnLegacyAssocDecayFactor emits a one-time WARN per vault when a vault's
// half-life was derived from a legacy explicit assoc_decay_factor rather than
// configured directly. Degrade loudly, never silently-wrong (principle #2):
// the factor's documented "per pass" meaning is gone, and the operator who set
// it is told exactly what it now means and which field to set instead.
func (e *Engine) warnLegacyAssocDecayFactor(vaultName string, resolved auth.ResolvedPlasticity) {
	// "Checked" is tracked separately from "warned": the sweep calls this
	// every ~60s per vault, and the config lookup below is a per-vault
	// GetVaultConfig read. One check per vault per process is enough — a
	// vault that did not need the WARN on first look never pays the read
	// again (refute non-blocking finding 7).
	if _, loaded := e.assocDecayLegacyChecked.LoadOrStore(vaultName, struct{}{}); loaded {
		return
	}
	cfg := e.VaultPlasticityConfig(vaultName)
	if cfg == nil || cfg.AssocDecayFactor == nil {
		return
	}
	if cfg.AssocHalfLifeDays != nil && *cfg.AssocHalfLifeDays != 0 {
		return // explicit half-life wins; nothing was reinterpreted
	}
	e.warnAssocDecayOnce(vaultName, func() {
		slog.Warn("legacy assoc_decay_factor reinterpreted as a per-day rate (#762); it is now only an enable/disable switch",
			"vault", vaultName,
			"assoc_decay_factor", *cfg.AssocDecayFactor,
			"derived_half_life_days", resolved.AssocHalfLifeDays,
			"fix", "set plasticity assoc_half_life_days explicitly")
	})
}

// decayAssocWeights dispatches to the store unless a test has installed a
// double via decayAssocWeightsFn.
func (e *Engine) decayAssocWeights(ctx context.Context, ws [8]byte, halfLife time.Duration, minWeight float32, archiveThreshold float64) (int, error) {
	if e.decayAssocWeightsFn != nil {
		return e.decayAssocWeightsFn(ctx, ws, halfLife, minWeight, archiveThreshold)
	}
	return e.store.DecayAssocWeights(ctx, ws, halfLife, minWeight, archiveThreshold)
}

// runIdempotencySweep is a daily background sweep that purges idempotency
// receipts (0x19 Pebble prefix) older than 30 days. It runs immediately on
// startup (to clean up existing accumulation) and then every 24 hours.
func (e *Engine) runIdempotencySweep() {
	defer close(e.idempotencySweepDone)
	defer func() {
		if r := recover(); r != nil {
			if !storage.IsClosedPanic(r) {
				slog.Error("engine: idempotency sweep panicked", "panic", r)
			}
		}
	}()
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

// runArchiveGCWorker is a weekly background sweep that true-prunes 0x25 archived
// edges meeting all four conditions: peakWeight < 0.15, coActivationCount < 3,
// daysSinceLastActivation > 1095, and restoredAt == 0 (never restored).
func (e *Engine) runArchiveGCWorker() {
	defer close(e.archiveGCDone)
	defer func() {
		if r := recover(); r != nil {
			if !storage.IsClosedPanic(r) {
				slog.Error("engine: archive GC worker panicked", "panic", r)
			}
		}
	}()
	ticker := time.NewTicker(7 * 24 * time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			vaults, _ := e.ListVaults(e.stopCtx)
			for _, vaultName := range vaults {
				ws := e.store.ResolveVaultPrefix(vaultName)
				pruned, err := e.store.GCArchivedEdges(e.stopCtx, ws)
				if err != nil {
					slog.Warn("engine: archive GC failed", "vault", vaultName, "err", err)
					continue
				}
				if pruned > 0 {
					slog.Info("engine: archive GC pruned edges", "vault", vaultName, "pruned", pruned)
				}
			}
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
	defer func() {
		if r := recover(); r != nil {
			if !storage.IsClosedPanic(r) {
				slog.Error("engine: novelty worker panicked", "panic", r)
			}
		}
	}()
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
	if err := e.refuseWrite(ctx); err != nil {
		return err
	}
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
	// spawnFireAndForget ensures Stop() drains this goroutine before DB close.
	// Uses e.stopCtx (not caller ctx) — feedback writes are gated by engine
	// lifecycle, not request lifecycle. Client disconnect must not abort them.
	e.spawnFireAndForget(func() {
		e.scoring.RecordFeedback(e.stopCtx, wsPrefix, signal)
	})

	// #682: useful=true is an explicit positive signal — reinforce
	// AccessCount/LastAccess the same as an explicit read. This is NEW wiring
	// (previously feedback only updated vault-level scoring weights, never
	// per-engram AccessCount). useful=false records negative feedback only;
	// it must not decrement or otherwise touch AccessCount. Unconditional
	// (not gated by ReinforceOnRead — that flag governs the passive
	// read-implies-access channel; an explicit "this was useful" signal is a
	// different, always-on channel) and uncapped, mirroring explicit
	// read-by-id.
	if useful {
		e.spawnFireAndForget(func() {
			_ = e.store.TouchAccess(e.stopCtx, wsPrefix, ulid)
		})
	}
	return nil
}

// actrHebScalePtr maps a wire-level ACTRHebScale to activation's optional form.
// The MBP/REST field is `omitempty`, so 0 on the wire means ABSENT, never an
// explicit zero — see the call site in Activate.
func actrHebScalePtr(v float32) *float32 {
	if v <= 0 {
		return nil
	}
	return &v
}
