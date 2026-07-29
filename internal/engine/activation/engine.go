package activation

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/errgroup"

	hnswpkg "github.com/scrypster/muninndb/internal/index/hnsw"
	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/storage/keys"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// DefaultWeights for composite scoring.
type DefaultWeights struct {
	SemanticSimilarity float32
	FullTextRelevance  float32
	DecayFactor        float32
	HebbianBoost       float32
	AccessFrequency    float32
	Recency            float32
	// ACT-R mode: when true, the engine uses ACT-R base-level + Hebbian scoring
	// instead of the additive weighted sum. This is the recommended production default.
	// See computeACTR() for the formula (Anderson 1993).
	UseACTR      bool
	ACTRDecay    float32 // power-law exponent d (default 0.5)
	ACTRHebScale float32 // Hebbian amplifier inside softplus (default 4.0)
}

// Weights is an optional client override.
type Weights struct {
	SemanticSimilarity float32
	FullTextRelevance  float32
	DecayFactor        float32
	HebbianBoost       float32
	AccessFrequency    float32
	Recency            float32
	// CGDN mode: set UseCGDN=true to enable Cognitive-Gated Divisive Normalization.
	UseCGDN   bool
	CGDNAlpha float32 // Ebbinghaus gate exponent (0 → default 1.5)
	CGDNBeta  float32 // Hebbian gate exponent (0 → default 0.5)
	CGDNPower float32 // divisive normalization power (0 → default 2.0)
	// ACT-R mode: set UseACTR=true to enable ACT-R base-level + Hebbian scoring.
	// This is the recommended mode: resolves decay-vs-Hebbian tension, deterministic,
	// total recall (no stored state mutation), grounded in 30+ years of cognitive science.
	UseACTR      bool
	ACTRDecay    float32 // power-law decay exponent d (0 → default 0.5)
	ACTRHebScale float32 // Hebbian scaling inside softplus (0 → default 4.0)
	DisableACTR  bool    // when true, force legacy weighted-sum scoring (overrides UseACTR)
	// RRF fusion mode: when true, use Phase 3 RRF scores directly as the scoring
	// basis in Phase 6, bypassing ACT-R/CGDN/weighted-sum recomputation.
	// Rank-based and scale-invariant (Cormack et al. 2009). Cognitive boosts
	// (Hebbian, PAS transition, confidence) are applied after fusion.
	UseRRFFusion bool
}

type resolvedWeights struct {
	SemanticSimilarity float64
	FullTextRelevance  float64
	DecayFactor        float64
	HebbianBoost       float64
	AccessFrequency    float64
	Recency            float64

	// CGDN: Cognitive-Gated Divisive Normalization (Carandini & Heeger 2012).
	// When UseCGDN=true, replaces the additive weighted sum with:
	//   g(d) = Ebbinghaus^Alpha * max(Hebbian, ε)^Beta  [cognitive gate]
	//   a(d) = (w_vec*semantic + w_fts*fts) * g(d)       [gated content]
	//   R(d) = a(d)^Power / (σ^Power + Σ a(j)^Power)     [divisive normalization]
	// This replicates lateral inhibition in hippocampal retrieval networks where
	// PFC inhibitory control (the gate) suppresses contextually stale candidates
	// so that FTS/semantic signal cannot override temporal decay.
	UseCGDN   bool
	CGDNAlpha float64 // exponent on Ebbinghaus decay in the gate (default 1.5)
	CGDNBeta  float64 // exponent on Hebbian boost in the gate (default 0.5)
	CGDNPower float64 // exponent n in divisive normalization (default 2.0)

	// ACT-R: Adaptive Control of Thought-Rational scoring (Anderson, 1993).
	// When UseACTR=true, replaces the additive weighted sum with:
	//   B(M) = ln(n+1) - d * ln(max(ageDays,ageFloor) / (n+1))  [base-level activation]
	//   Score = ContentMatch × softplus(B(M) + scale×Hebbian) × Confidence
	//
	// This resolves the decay-vs-Hebbian tension: both are ADDITIVE inside softplus.
	// Fresh memories: high B(M) → high score; Old memories + Hebbian link: low B(M)
	// rescued by Hebbian → moderate score; Old memories no link: low B(M) → suppressed.
	// TOTAL RECALL: no background worker degrades stored state. Time is query-time only.
	UseACTR      bool
	ACTRDecay    float64 // power-law decay exponent d (default 0.5 per Anderson 1993)
	ACTRHebScale float64 // Hebbian scaling factor inside softplus (default 4.0)

	// RRF fusion: when UseRRFFusion=true, Phase 6 uses the Phase 3 RRF score
	// directly as the scoring basis. Cognitive boosts (Hebbian, transition,
	// confidence) are applied multiplicatively after fusion.
	// This is rank-based and scale-invariant — robust to score scale mismatches.
	UseRRFFusion bool
}

// Filter is a query filter applied in Phase 6.
type Filter struct {
	Field string
	Op    string
	Value interface{}
}

// ScoredID is a search result from an index.
type ScoredID struct {
	ID    storage.ULID
	Score float64
}

// ScoreComponents breaks down how a score was computed.
type ScoreComponents struct {
	SemanticSimilarity float64
	// FullTextRelevance is an absolute, query-calibrated IDF-weighted coverage
	// score in [0,1] straight from fts.Index.Search (COG-24, issue #711) — the
	// fraction of the query's IDF mass this engram covers. It is NOT a
	// normalized raw BM25 score; corpus-absent query terms are charged the
	// corpus's maximum-rarity IDF, so a query with no real overlap scores low
	// here regardless of what else matched.
	FullTextRelevance float64
	DecayFactor       float64
	HebbianBoost      float64
	TransitionBoost   float64
	// EntityBoost is the post-pipeline spread-activation adjustment added by
	// the entity boost phase (rarity-weighted, capped). Zero when the result
	// received no entity boost; for engrams injected by that phase it equals
	// the full Score (issue #569).
	EntityBoost     float64
	AccessFrequency float64
	Recency         float64
	Confidence      float64
	Raw             float64
	Final           float64
}

// ScoredEngram is one activation result.
type ScoredEngram struct {
	Engram      *storage.Engram
	Score       float64
	Components  ScoreComponents
	Why         string
	HopPath     []storage.ULID
	HopConcepts []string
	Dormant     bool

	// SupersededBy / CurrentVersion are set by the engine's supersedes-aware
	// recall phase (applySupersession) on a result that is superseded by a newer
	// active engram. SupersededBy is the immediate superseder; CurrentVersion is
	// the chain head (the fact to consult now). Both zero when not superseded.
	// They let recall surface "this is stale — current is X" without the caller
	// opting into annotations or making a second call.
	SupersededBy   storage.ULID
	CurrentVersion storage.ULID
}

// EngramFilter is a post-retrieval predicate applied as the final activation step.
// Implemented by *query.Filter; any caller can implement this for custom filtering.
type EngramFilter interface {
	Match(*storage.Engram) bool
}

// ActivateRequest is the internal activation request form.
type ActivateRequest struct {
	VaultID          uint32
	VaultPrefix      [8]byte // if set, used directly instead of VaultID
	Context          []string
	Embedding        []float32
	Threshold        float64
	MaxResults       int
	HopDepth         int
	IncludeWhy       bool
	Weights          *Weights
	Filters          []Filter
	ReadOnly         bool         // when true, skip all write side-effects (observe mode)
	Profile          string       // traversal profile override: "default"|"causal"|"confirmatory"|"adversarial"|"structural"
	VaultDefault     string       // vault Plasticity default profile (set by engine.go, not by callers)
	StructuredFilter EngramFilter // applied as final post-retrieval predicate
	// CandidatesPerIndex overrides the per-index candidate pool size for phase2.
	// Zero means fall back to 30.
	CandidatesPerIndex int
	// PAS: Predictive Activation Signal — sequential transition tracking.
	PASEnabled       bool // when true, inject transition candidates in Phase 2
	PASMaxInjections int  // max transition candidates to inject (0 = default 5)
	// ExcludeUntrusted: when true, engrams with TrustUntrusted (0x04) are silently
	// excluded from activation results. Set by the engine from vault PlasticityConfig.
	ExcludeUntrusted bool
	// CallerOwner is the ownership-lease identity of the recall caller. Engrams
	// held by a live lease owned by someone else are hidden (work-queue checkout),
	// unless IncludeLeased is set. Empty means the caller owns no leases.
	CallerOwner string
	// IncludeLeased disables lease-based visibility filtering (admin/debugging).
	IncludeLeased bool
	// AsOf, when set, gates results by the full valid-time interval check
	// [ValidFrom, ValidUntil) at T ("what was true at T"). Nil = the default
	// gate: drop only engrams whose closed ValidUntil <= now; a future
	// ValidFrom is deliberately NOT hidden (hiding a just-stored future fact
	// until a clock ticks kills trust).
	AsOf *time.Time
	// IncludeInvalid disables the valid-time gate entirely (show history).
	IncludeInvalid bool
}

// PassesValidity is the valid-time recall gate (COG-19: default recall never
// returns an engram whose ValidUntil <= now). Validity is a HARD filter —
// cognition ranks only survivors. Shared by phase-6 scoring and the engine's
// final post-entity-boost gate so injected candidates cannot bypass it.
func PassesValidity(eng *storage.Engram, asOf *time.Time, includeInvalid bool, now time.Time) bool {
	if includeInvalid {
		return true
	}
	if asOf != nil {
		return eng.ValidAt(*asOf)
	}
	return !eng.IsExpired(now)
}

// ActivateResult is what the transport layer serializes and returns.
type ActivateResult struct {
	QueryID       string
	Activations   []ScoredEngram
	TotalFound    int
	LatencyMs     float64
	ProfileUsed   string        // resolved traversal profile name (e.g. "default", "causal")
	RestoredEdges []mbp.EdgeRef // edges lazily restored from archive during Phase 4.75
}

// ActivateResponseFrame is one streaming frame of results.
type ActivateResponseFrame struct {
	QueryID     string
	TotalFound  int
	LatencyMs   float64
	Activations []ScoredEngram
	Frame       int
	TotalFrames int
}

// ActivationStore is the storage interface required by the activation engine.
type ActivationStore interface {
	GetMetadata(ctx context.Context, wsPrefix [8]byte, ids []storage.ULID) ([]*storage.EngramMeta, error)
	GetEngrams(ctx context.Context, wsPrefix [8]byte, ids []storage.ULID) ([]*storage.Engram, error)
	// GetLeases batch-reads ownership leases, one per id in order (zero Lease for
	// unleased engrams). Used for work-queue recall visibility filtering.
	GetLeases(ctx context.Context, wsPrefix [8]byte, ids []storage.ULID) ([]storage.Lease, error)
	GetAssociations(ctx context.Context, wsPrefix [8]byte, ids []storage.ULID, maxPerNode int) (map[storage.ULID][]storage.Association, error)
	RecentActive(ctx context.Context, wsPrefix [8]byte, topK int) ([]storage.ULID, error)
	VaultPrefix(vault string) [8]byte
	// EngramLastAccessNs returns the nanosecond timestamp of the last cache access for id.
	// Returns 0 if not in cache; callers fall back to eng.LastAccess.
	EngramLastAccessNs(wsPrefix [8]byte, id storage.ULID) int64
	EngramIDsByCreatedRange(ctx context.Context, wsPrefix [8]byte, since, until time.Time, limit int) ([]storage.ULID, error)
	// ListByTagInRange returns engram IDs carrying tag, created within [since, until],
	// newest-first (see storage.PebbleStore.ListByTagInRange). Used for tag-scoped
	// candidate seeding so tag-filtered recall does not miss engrams absent from the
	// FTS/HNSW/decay pools.
	ListByTagInRange(ctx context.Context, wsPrefix [8]byte, tag string, since, until time.Time, limit int) ([]storage.ULID, error)
	// ListByTagsAllInRange returns engram IDs carrying EVERY tag, created within
	// [since, until], newest-first, via a K-way stream intersection so that limit
	// bounds the intersection OUTPUT and never truncates a per-tag input window
	// (see storage.PebbleStore.ListByTagsAllInRange). Used for tags_all seeding.
	ListByTagsAllInRange(ctx context.Context, wsPrefix [8]byte, tags []string, since, until time.Time, limit int) ([]storage.ULID, error)
	// ScanRawTagRange scans the S1 ordered raw-tag index (0x2B) for tagKey
	// within [lower, upper) — see storage.PebbleStore.ScanRawTagRange and
	// keys.RawTagRangeBound. Used to SEED candidates for tag_prefix filters
	// (e.g. due:<=today) so a bounded range query does not depend on the
	// engram already being a candidate from FTS/HNSW/decay/tags_all/tags_any.
	ScanRawTagRange(ctx context.Context, wsPrefix [8]byte, tagKey string, lower, upper []byte, limit int) ([]storage.ULID, error)
	// RestoreArchivedEdgesTransitive lazily restores archived edges for src and
	// its direct neighbors, returning the set of restored destination IDs.
	RestoreArchivedEdgesTransitive(ctx context.Context, wsPrefix [8]byte, src storage.ULID, maxDirect int, maxTransitive int) ([]storage.ULID, error)
	// ArchiveBloomMayContain returns true if src may have archived edges
	// (Bloom filter probabilistic check; false positives possible, no false negatives).
	ArchiveBloomMayContain(id [16]byte) bool
}

// FTSIndex is the full-text search interface.
type FTSIndex interface {
	Search(ctx context.Context, ws [8]byte, query string, topK int) ([]ScoredID, error)
}

// HNSWIndex is the vector search interface.
type HNSWIndex interface {
	Search(ctx context.Context, ws [8]byte, vec []float32, topK int) ([]ScoredID, error)
}

// Embedder converts text to a vector embedding.
type Embedder interface {
	Embed(ctx context.Context, texts []string) ([]float32, error)
	Tokenize(text string) []string
}

// PASTransitionStore reads transition targets for PAS candidate injection.
type PASTransitionStore interface {
	GetTopTransitions(ctx context.Context, ws [8]byte, srcID [16]byte, topK int) ([]storage.TransitionTarget, error)
}

// logItem is a queued activation log entry for the async drainer.
// activations is the already-allocated result slice — the drainer extracts
// ids and scores off the hot path, keeping Run() allocation-free for logging.
type logItem struct {
	vaultID     uint32
	activations []ScoredEngram
}

// ActivationEngine is the main ACTIVATE pipeline orchestrator.
type ActivationEngine struct {
	store      ActivationStore
	fts        FTSIndex
	hnsw       HNSWIndex
	embedder   Embedder
	assocLog   *ActivationLog
	weights    DefaultWeights
	transStore PASTransitionStore // optional, nil = PAS disabled at engine level
	// logCh is a buffered channel for async activation log entries.
	// A single drainer goroutine owns all writes to assocLog, eliminating
	// Lock() contention against Phase 4's concurrent RLock() calls.
	logCh     chan logItem
	logDone   chan struct{}
	closeOnce sync.Once
}

// New creates a new ActivationEngine.
func New(store ActivationStore, fts FTSIndex, hnsw HNSWIndex, embedder Embedder) *ActivationEngine {
	// DefaultWeights are only used when resolveWeights gets req.Weights == nil (e.g. tests).
	// All production scoring goes through ACT-R; decay path is kept in code but not reachable.
	w := DefaultWeights{
		SemanticSimilarity: 0.35,
		FullTextRelevance:  0.25,
		DecayFactor:        0.20,
		HebbianBoost:       0.10,
		AccessFrequency:    0.05,
		Recency:            0.05,
	}
	// When HNSW is unavailable, semantic similarity is always 0.
	// Redistribute that 0.35 budget to active components so the score
	// range isn't compressed by 35% of dead weight.
	if hnsw == nil {
		scale := float32(1.0 / 0.65)
		w.SemanticSimilarity = 0
		w.FullTextRelevance = 0.25 * scale // ≈ 0.385
		w.DecayFactor = 0.20 * scale       // ≈ 0.308
		w.HebbianBoost = 0.10 * scale      // ≈ 0.154
		w.AccessFrequency = 0.05 * scale   // ≈ 0.077
		w.Recency = 0.05 * scale           // ≈ 0.077
	}
	e := &ActivationEngine{
		store:    store,
		fts:      fts,
		hnsw:     hnsw,
		embedder: embedder,
		assocLog: &ActivationLog{},
		weights:  w,
		logCh:    make(chan logItem, 4096),
		logDone:  make(chan struct{}),
	}
	go e.drainLog()
	return e
}

// drainLog is the single goroutine that writes to assocLog.
// Serializes all activation log writes, eliminating Lock contention against
// Phase 4's concurrent RecentForVault RLock calls. Eventual consistency:
// the log may lag by ~1ms but Hebbian decay half-life is 3600s — irrelevant.
func (e *ActivationEngine) drainLog() {
	defer close(e.logDone)
	for item := range e.logCh {
		// Extract ids/scores in the drainer goroutine — off the hot path.
		ids := make([]storage.ULID, len(item.activations))
		scores := make([]float64, len(item.activations))
		for i, a := range item.activations {
			ids[i] = a.Engram.ID
			scores[i] = a.Score
		}
		e.assocLog.Record(LogEntry{
			VaultID:   item.vaultID,
			At:        time.Now(),
			EngramIDs: ids,
			Scores:    scores,
		})
	}
}

// SetTransitionStore sets the PAS transition store for candidate injection.
// Must be called before any activations. Pass nil to disable PAS at engine level.
func (e *ActivationEngine) SetTransitionStore(ts PASTransitionStore) {
	e.transStore = ts
}

// AssocLog returns the activation log for reading previous activations.
// Used by engine.go to determine previous activation results for transition recording.
func (e *ActivationEngine) AssocLog() *ActivationLog {
	return e.assocLog
}

// Close shuts down the async activation log drainer. Idempotent: safe to call
// multiple times (e.g. from both test cleanup and Engine.Stop).
func (e *ActivationEngine) Close() {
	e.closeOnce.Do(func() {
		close(e.logCh)
		<-e.logDone
	})
}

// CalcCandidatesPerIndex returns the per-index candidate pool size for phase2
// based on vault size. For small vaults (≤1000 items) returns N to scan
// everything — 1000 × 384 cosine comparisons is negligible.
// For larger vaults: clamp(sqrt(vaultSize), 30, 200).
// Called by engine.go before constructing ActivateRequest.
func CalcCandidatesPerIndex(vaultSize int64) int {
	if vaultSize <= 0 {
		return 30
	}
	if vaultSize <= 1000 {
		return int(vaultSize)
	}
	c := int(math.Sqrt(float64(vaultSize)))
	if c < 30 {
		return 30
	}
	if c > 200 {
		return 200
	}
	return c
}

const minFloor = float32(0.05)
const frameSize = 100

// Run executes the 6-phase ACTIVATE pipeline.
func (e *ActivationEngine) Run(ctx context.Context, req *ActivateRequest) (*ActivateResult, error) {
	start := time.Now()

	if req.MaxResults <= 0 {
		req.MaxResults = 10
	}
	w := resolveWeights(req.Weights, e.weights)
	if req.Threshold <= 0 {
		// No explicit threshold: pick a mode-appropriate default.
		// RRF scores are rank-based, typically in [0, 0.05] -- far lower than ACT-R.
		if w.UseRRFFusion {
			req.Threshold = 0.001
		} else {
			req.Threshold = 0.05
		}
	}

	// Phase 1: embed + tokenize
	p1, err := e.phase1(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("activation phase1: %w", err)
	}

	// Phase 2: parallel candidate retrieval
	ws := req.VaultPrefix
	if ws == ([8]byte{}) {
		ws = e.vaultWorkspace(req.VaultID)
	}
	sets, err := e.phase2(ctx, req, p1, ws)
	if err != nil {
		return nil, fmt.Errorf("activation phase2: %w", err)
	}

	// Phase 3: RRF fusion
	fused := phase3RRF(sets)

	// Phase 4: Hebbian boost (always sequential — fast, in-memory ring buffer read).
	e.phase4HebbianBoost(ctx, ws, req.VaultID, fused)

	// Phase 4.5: PAS transition boost — applies to candidates already in the fused list.
	if req.PASEnabled {
		e.phase4_5TransitionBoost(ctx, ws, req.VaultID, fused)
	}

	// Phase 4.75: Lazy archive restore — check Bloom filter, restore dormant edges.
	restoredEdges := e.phase4_75ArchiveRestore(ctx, ws, fused)

	// Resolve traversal profile for Phase 5 and for audit logging.
	// Always resolved so ProfileUsed is set on every activation, regardless of HopDepth.
	profileName, profile := resolveProfile(req)

	// Phase 5: BFS traversal — run sequentially after Phase 4.
	// Goroutine spawn overhead (~3-5µs) is not worth it for the common case where
	// the corpus has no associations (empty GetAssociations returns immediately from cache).
	// The early-exit in phase5Traverse handles the no-association case efficiently.
	var traversed []traversedCandidate
	if req.HopDepth > 0 {
		// Check deadline before starting BFS — skip traversal if already expired.
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		traversed = e.phase5Traverse(ctx, req, ws, profile, fused)
	}

	// Phase 6: final scoring, filter, response
	result, err := e.phase6Score(ctx, req, ws, fused, traversed, p1)
	if err != nil {
		return nil, fmt.Errorf("activation phase6: %w", err)
	}

	result.LatencyMs = float64(time.Since(start).Microseconds()) / 1000.0
	result.ProfileUsed = profileName
	result.RestoredEdges = restoredEdges

	slog.Info("activation complete", "profile", profileName, "results", len(result.Activations), "elapsed_ms", result.LatencyMs)

	// Submit activation log entry to the async drainer — zero hot-path allocations.
	// The drainer extracts ids/scores off the critical path.
	// Non-blocking: drops if channel full (Hebbian half-life=3600s, 1ms lag is negligible).
	if !req.ReadOnly && len(result.Activations) > 0 {
		select {
		case e.logCh <- logItem{vaultID: req.VaultID, activations: result.Activations}:
			// Yield to allow the drainer goroutine to process immediately.
			// Cost: ~1-5ns (no syscall). Ensures test consistency and reduces
			// drainer queue depth in production under bursty load.
			runtime.Gosched()
		default: // channel full — drop; eventual consistency accepted
		}
	}

	return result, nil
}

func (e *ActivationEngine) vaultWorkspace(vaultID uint32) [8]byte {
	var ws [8]byte
	ws[0] = byte(vaultID >> 24)
	ws[1] = byte(vaultID >> 16)
	ws[2] = byte(vaultID >> 8)
	ws[3] = byte(vaultID)
	return ws
}

// phase1 embeds context and tokenizes query.
type phase1Result struct {
	embedding []float32
	tokens    []string
	queryStr  string
}

func (e *ActivationEngine) phase1(ctx context.Context, req *ActivateRequest) (*phase1Result, error) {
	result := &phase1Result{}
	result.queryStr = strings.Join(req.Context, " ")

	if e.embedder != nil {
		result.tokens = e.embedder.Tokenize(result.queryStr)
	}

	if len(req.Embedding) > 0 {
		result.embedding = req.Embedding
		return result, nil
	}

	// Only compute embedding if HNSW is available — the embedding is used
	// exclusively for vector search in phase2.  When HNSW is nil (common in
	// benchmarks and lightweight deployments), this avoids the hashEmbedder
	// CPU cost entirely (~13% of activation CPU).
	if e.embedder != nil && e.hnsw != nil {
		vec, err := e.embedder.Embed(ctx, req.Context)
		if err != nil {
			// Embedding backend unreachable (e.g. connection refused on the
			// embedding endpoint). Degrade to BM25+decay recall instead of
			// aborting: FTS still returns useful results, and phase2 already
			// takes the FTS-only path when len(embedding)==0.
			slog.Warn("activation: embed backend unreachable, degrading to BM25-only recall",
				"vault", req.VaultID, "error", err)
			return result, nil
		}
		// Embed returns a flat len(texts)*dim slice — each phrase's vector
		// concatenated. A multi-phrase context must be pooled back into a single
		// dim-sized query vector; feeding the raw N*dim slice to the dim-sized
		// HNSW index makes CosineSimilarity's length guard return 0 for every
		// node, silently zeroing the vector signal for any 2+ phrase context (#498).
		if n := len(req.Context); n > 1 && len(vec) > 0 && len(vec)%n == 0 {
			vec = meanPoolEmbeddings(vec, n)
		}
		result.embedding = vec
	}
	return result, nil
}

// meanPoolEmbeddings averages n equal-length vectors concatenated in flat
// (dim = len(flat)/n) and L2-normalizes the result into a single dim-sized
// query vector. Callers must ensure len(flat) is a positive multiple of n.
func meanPoolEmbeddings(flat []float32, n int) []float32 {
	dim := len(flat) / n
	pooled := make([]float32, dim)
	for p := 0; p < n; p++ {
		base := p * dim
		for i := 0; i < dim; i++ {
			pooled[i] += flat[base+i]
		}
	}
	var norm float64
	for i := range pooled {
		pooled[i] /= float32(n)
		norm += float64(pooled[i]) * float64(pooled[i])
	}
	if norm > 0 {
		inv := float32(1.0 / math.Sqrt(norm))
		for i := range pooled {
			pooled[i] *= inv
		}
	}
	return pooled
}

// phase2 retrieves candidates from FTS, HNSW, and decay pool in parallel.
type candidateSets struct {
	fts        []ScoredID
	vector     []ScoredID
	decay      []storage.ULID
	time       []storage.ULID // from time-bounded range scan when since/before filters present
	tag        []storage.ULID // from tag-index scan when tags_all/tags_any filters present
	transition []storage.ULID // PAS: transition-predicted candidates from previous activation
}

// extractTimeBounds extracts since/before time bounds from filter list.
// Returns (since time.Time, before time.Time, hasTimeBounds bool).
// If a bound is not present, it defaults to zero value.
func extractTimeBounds(filters []Filter) (time.Time, time.Time, bool) {
	var since, before time.Time
	hasBounds := false

	for _, f := range filters {
		if f.Field == "created_after" {
			if t, ok := f.Value.(time.Time); ok {
				since = t
				hasBounds = true
			}
		} else if f.Field == "created_before" {
			if t, ok := f.Value.(time.Time); ok {
				before = t
				hasBounds = true
			}
		}
	}

	return since, before, hasBounds
}

// extractTagFilters extracts tags_all/tags_any tag lists, and raw tag_prefix
// (prefix, op, bound) triples, from the filter list. tags_all/tags_any values
// are coerced with asStringSlice; tag_prefix values are coerced with asPair —
// both match PassesMetaFilter's own interpretation of these fields.
func extractTagFilters(filters []Filter) (tagsAll, tagsAny []string, tagPrefix []tagPrefixFilter) {
	for _, f := range filters {
		switch f.Field {
		case "tags_all":
			tagsAll = append(tagsAll, asStringSlice(f.Value)...)
		case "tags_any":
			tagsAny = append(tagsAny, asStringSlice(f.Value)...)
		case "tag_prefix":
			if pb := asPair(f.Value); pb != nil {
				tagPrefix = append(tagPrefix, tagPrefixFilter{Prefix: pb[0], Op: f.Op, Bound: pb[1]})
			}
		}
	}
	return tagsAll, tagsAny, tagPrefix
}

// tagPrefixFilter is a decoded tag_prefix filter: engrams whose tag begins
// with Prefix (e.g. "due:") are compared, after stripping Prefix, against
// Bound per Op (lte/gte/lt/gt/eq) — see PassesMetaFilter's "tag_prefix" case,
// which this mirrors for candidate SEEDING via the S1 raw-tag-range index.
type tagPrefixFilter struct {
	Prefix string
	Op     string
	Bound  string
}

// seedTagCandidates queries the tag index for the requested tags and returns a
// deduped candidate set within [since, until], newest-first per tag.
//
//   - tags_all: a K-way stream intersection over the tag index
//     (ListByTagsAllInRange), so limit bounds the intersection OUTPUT and never
//     truncates a per-tag input window — a true positive is found even when every
//     per-tag newest-first window is full of single-tag decoys.
//   - tags_any: union of the per-tag newest-first scans (ListByTagInRange).
//   - both present: union of the tags_all intersection and the tags_any union.
//
// The tag index stores Hash(tag) (4-byte), so a seeded ID can be a hash-collision
// false positive; PassesMetaFilter in phase 6 remains the correctness gate.
//
// tagPrefix additionally seeds from the S1 ordered raw-tag-range index (0x2B):
// for each distinct Prefix among tagPrefix, every filter sharing that prefix is
// combined into a single bounded range scan (AND semantics — e.g. a gte and an
// lte filter on the same prefix narrow to one bounded range), unioned into the
// same seed set as tags_all/tags_any. This is what makes range-filtered recall
// (e.g. tag_filter{prefix:"due:", lte:today}) SEED candidates instead of only
// being checked post-hoc in phase 6's PassesMetaFilter.
func (e *ActivationEngine) seedTagCandidates(ctx context.Context, ws [8]byte, tagsAll, tagsAny []string, tagPrefix []tagPrefixFilter, since, until time.Time, limit int) []storage.ULID {
	seen := make(map[storage.ULID]struct{})
	var seed []storage.ULID
	add := func(ids []storage.ULID) {
		for _, id := range ids {
			if _, ok := seen[id]; ok {
				continue
			}
			seen[id] = struct{}{}
			seed = append(seed, id)
		}
	}

	if len(tagsAll) > 0 {
		// ListByTagsAllInRange collapses duplicate tags itself, so tagsAll needs no
		// dedupe here.
		if ids, err := e.store.ListByTagsAllInRange(ctx, ws, tagsAll, since, until, limit); err == nil {
			add(ids)
		}
	}

	// Dedupe tags_any so a repeated tag does not trigger a redundant index scan.
	seenTag := make(map[string]struct{}, len(tagsAny))
	for _, tag := range tagsAny {
		if _, ok := seenTag[tag]; ok {
			continue
		}
		seenTag[tag] = struct{}{}
		if ids, err := e.store.ListByTagInRange(ctx, ws, tag, since, until, limit); err == nil {
			add(ids)
		}
	}

	// Group tag_prefix filters by Prefix (e.g. "due:") so multiple filters on
	// the same prefix (gte + lte) combine into one bounded range scan instead
	// of two independent (and semantically wrong, if ORed) scans.
	if len(tagPrefix) > 0 {
		byPrefix := make(map[string][]tagPrefixFilter, len(tagPrefix))
		order := make([]string, 0, len(tagPrefix))
		for _, tpf := range tagPrefix {
			if _, ok := byPrefix[tpf.Prefix]; !ok {
				order = append(order, tpf.Prefix)
			}
			byPrefix[tpf.Prefix] = append(byPrefix[tpf.Prefix], tpf)
		}
		for _, prefix := range order {
			tagKey := strings.TrimSuffix(prefix, ":")
			if tagKey == "" {
				continue
			}
			tagKeyHash := keys.Hash(tagKey)
			var lower, upper []byte
			for i, tpf := range byPrefix[prefix] {
				lo, hi := keys.RawTagRangeBound(ws, tagKeyHash, tpf.Op, []byte(tpf.Bound))
				if i == 0 {
					lower, upper = lo, hi
				} else {
					lower, upper = keys.CombineRawTagRangeBounds(lower, upper, lo, hi)
				}
			}
			if ids, err := e.store.ScanRawTagRange(ctx, ws, tagKey, lower, upper, limit); err == nil {
				add(ids)
			}
		}
	}

	return seed
}

func (e *ActivationEngine) phase2(ctx context.Context, req *ActivateRequest, p1 *phase1Result, ws [8]byte) (*candidateSets, error) {
	var sets candidateSets
	k := req.CandidatesPerIndex
	if k <= 0 {
		k = 30
	}

	// Extract time bounds from filters for Phase 3: time-bounded candidate injection.
	since, before, hasTimeBounds := extractTimeBounds(req.Filters)

	// Extract tag filters for tag-scoped candidate seeding. The tag scans reuse the
	// time window (zero since → epoch; zero before → now) so tag-filtered recall
	// combined with explicit time bounds respects both via the ULID-ordered index.
	tagsAll, tagsAny, tagPrefix := extractTagFilters(req.Filters)
	hasTagFilters := len(tagsAll) > 0 || len(tagsAny) > 0 || len(tagPrefix) > 0
	tagSince := since
	if tagSince.IsZero() {
		tagSince = time.Unix(0, 0)
	}
	tagUntil := before
	if tagUntil.IsZero() {
		tagUntil = time.Now()
	}

	// Fast path: when HNSW is nil, there is nothing to parallelize.
	// FTS and RecentActive are both in-memory with sub-10µs latency.
	// Eliminating the errgroup saves goroutine spawn + context derivation overhead
	// (~3-5µs per activation at 12+ concurrent goroutines).
	if e.hnsw == nil || len(p1.embedding) == 0 {
		if e.fts != nil {
			results, err := e.fts.Search(ctx, ws, p1.queryStr, k)
			if err != nil {
				slog.Warn("activation: fts search degraded", "vault", req.VaultID, "error", err)
			}
			sets.fts = results
		}
		ids, _ := e.store.RecentActive(ctx, ws, k)
		sets.decay = ids

		// Phase 3: Time-bounded candidate injection
		if hasTimeBounds {
			if before.IsZero() {
				before = time.Now()
			}
			ids, _ := e.store.EngramIDsByCreatedRange(ctx, ws, since, before, k*3)
			sets.time = ids
		}

		// Tag-scoped candidate seeding (fast path).
		if hasTagFilters {
			sets.tag = e.seedTagCandidates(ctx, ws, tagsAll, tagsAny, tagPrefix, tagSince, tagUntil, k*3)
		}

		// PAS: transition candidate retrieval (fast path)
		if req.PASEnabled && e.transStore != nil {
			sets.transition = e.getTransitionCandidates(ctx, ws, req.VaultID, req.PASMaxInjections)
		}

		return &sets, nil
	}

	// Full parallel path: FTS + HNSW + decay + time-bounded scan run concurrently.
	g, gctx := errgroup.WithContext(ctx)

	g.Go(func() error {
		if e.fts == nil {
			return nil
		}
		results, err := e.fts.Search(gctx, ws, p1.queryStr, k)
		if err != nil {
			slog.Warn("activation: fts search degraded", "vault", req.VaultID, "error", err)
			// continue with empty FTS results
			return nil
		}
		sets.fts = results
		return nil
	})

	g.Go(func() error {
		results, err := e.hnsw.Search(gctx, ws, p1.embedding, k)
		if err != nil {
			var dimErr *hnswpkg.DimMismatchError
			if errors.As(err, &dimErr) {
				// The active embedder's dimension does not match this vault's
				// existing vectors (#582). FTS results below still apply — same
				// degrade-not-abort contract as an unreachable embed backend (#578).
				slog.Warn("activation: query embedding dimension does not match vault vectors — recall degraded to BM25-only; run `muninn vault reembed` after changing embedding models",
					"vault", req.VaultID, "query_dim", dimErr.Got, "vault_dim", dimErr.Want)
			} else {
				slog.Warn("activation: hnsw search degraded", "vault", req.VaultID, "err", err)
			}
			return nil
		}
		sets.vector = results
		return nil
	})

	g.Go(func() error {
		ids, err := e.store.RecentActive(gctx, ws, k)
		if err != nil {
			return nil
		}
		sets.decay = ids
		return nil
	})

	// Phase 3: Time-bounded candidate injection (parallel with other indices)
	if hasTimeBounds {
		g.Go(func() error {
			// Default before to now if not specified
			before_ts := before
			if before_ts.IsZero() {
				before_ts = time.Now()
			}
			ids, err := e.store.EngramIDsByCreatedRange(gctx, ws, since, before_ts, k*3)
			if err != nil {
				return nil
			}
			sets.time = ids
			return nil
		})
	}

	// Tag-scoped candidate seeding (parallel with other indices)
	if hasTagFilters {
		g.Go(func() error {
			sets.tag = e.seedTagCandidates(gctx, ws, tagsAll, tagsAny, tagPrefix, tagSince, tagUntil, k*3)
			return nil
		})
	}

	// PAS: transition candidate retrieval (parallel with other indices)
	if req.PASEnabled && e.transStore != nil {
		g.Go(func() error {
			sets.transition = e.getTransitionCandidates(gctx, ws, req.VaultID, req.PASMaxInjections)
			return nil
		})
	}

	if err := g.Wait(); err != nil {
		return nil, err
	}
	return &sets, nil
}

// fusedCandidate is a candidate after RRF fusion.
type fusedCandidate struct {
	id              storage.ULID
	rrfScore        float64
	ftsScore        float64
	vectorScore     float64
	inDecayPool     bool
	inTagPool       bool
	hebbianBoost    float64
	transitionBoost float64
}

const (
	rrfK_HNSW       = 40.0
	rrfK_FTS        = 60.0
	rrfK_Transition = 50.0 // PAS: between HNSW and FTS, strong but not dominant
	rrfK_Decay      = 120.0
	rrfK_Time       = 100.0 // time-bounded range scan; lower than decay to deprioritize vs semantic relevance
	rrfK_Tag        = 100.0 // tag-index scan; same tier as time — the post-filter is the correctness gate
)

// phase3RRF merges candidate lists via Reciprocal Rank Fusion.
// Uses index-into-slice instead of map-of-pointers to reduce heap allocations.
func phase3RRF(sets *candidateSets) []fusedCandidate {
	totalCap := len(sets.fts) + len(sets.vector) + len(sets.decay) + len(sets.time) + len(sets.tag) + len(sets.transition)
	result := make([]fusedCandidate, 0, totalCap)
	index := make(map[storage.ULID]int, totalCap)

	getOrCreate := func(id storage.ULID) *fusedCandidate {
		if idx, ok := index[id]; ok {
			return &result[idx]
		}
		idx := len(result)
		result = append(result, fusedCandidate{id: id})
		index[id] = idx
		return &result[idx]
	}

	for rank, s := range sets.fts {
		c := getOrCreate(s.ID)
		c.rrfScore += 1.0 / (rrfK_FTS + float64(rank+1))
		c.ftsScore = s.Score
	}

	for rank, s := range sets.vector {
		c := getOrCreate(s.ID)
		c.rrfScore += 1.0 / (rrfK_HNSW + float64(rank+1))
		c.vectorScore = s.Score
	}

	for rank, id := range sets.decay {
		c := getOrCreate(id)
		c.rrfScore += 1.0 / (rrfK_Decay + float64(rank+1))
		c.inDecayPool = true
	}

	// Phase 3: time-bounded candidate injection via RRF
	for rank, id := range sets.time {
		c := getOrCreate(id)
		c.rrfScore += 1.0 / (rrfK_Time + float64(rank+1))
	}

	// Tag-scoped candidate injection via RRF
	for rank, id := range sets.tag {
		c := getOrCreate(id)
		c.rrfScore += 1.0 / (rrfK_Tag + float64(rank+1))
		c.inTagPool = true
	}

	// PAS: transition-predicted candidate injection via RRF
	for rank, id := range sets.transition {
		c := getOrCreate(id)
		c.rrfScore += 1.0 / (rrfK_Transition + float64(rank+1))
	}

	sort.Slice(result, func(i, j int) bool {
		return result[i].rrfScore > result[j].rrfScore
	})
	return result
}

// phase4HebbianBoost applies Hebbian association boost to candidates.
// vaultID is used to scope the activation log to the current vault, preventing
// Hebbian boosts from other vaults from bleeding into this vault's results.
func (e *ActivationEngine) phase4HebbianBoost(ctx context.Context, ws [8]byte, vaultID uint32, candidates []fusedCandidate) {
	recent := e.assocLog.RecentForVault(vaultID, 50)
	if len(recent) == 0 {
		return
	}

	now := time.Now().Unix()
	recentWeights := make(map[storage.ULID]float64, len(recent))
	const halfLife = 3600.0
	for _, entry := range recent {
		age := float64(now - entry.At.Unix())
		if age < 0 { // clock skew: activation timestamped in the future
			age = 0
		}
		recencyW := math.Exp(-age / halfLife)
		for _, id := range entry.EngramIDs {
			if w, ok := recentWeights[id]; !ok || recencyW > w {
				recentWeights[id] = recencyW
			}
		}
	}

	if len(recentWeights) == 0 {
		return
	}

	ids := make([]storage.ULID, len(candidates))
	for i, c := range candidates {
		ids[i] = c.id
	}

	// Cap to top-50 candidates to bound GetAssociations work per activation cycle.
	if len(ids) > 50 {
		ids = ids[:50]
	}

	assocMap, err := e.store.GetAssociations(ctx, ws, ids, 20)
	if err != nil {
		return
	}

	for i := range candidates {
		assocs := assocMap[candidates[i].id]
		var boost float64
		for _, a := range assocs {
			if rw, ok := recentWeights[a.TargetID]; ok {
				boost += float64(a.Weight) * rw
			}
		}
		if boost > 1.0 {
			boost = 1.0
		}
		candidates[i].hebbianBoost = boost
	}
}

// getTransitionCandidates retrieves PAS candidate IDs from the transition table.
// Looks at the most recent activation for this vault and finds transition targets
// for each result engram from that activation. Returns deduplicated IDs capped
// at maxInjections.
func (e *ActivationEngine) getTransitionCandidates(ctx context.Context, ws [8]byte, vaultID uint32, maxInjections int) []storage.ULID {
	if maxInjections <= 0 {
		maxInjections = 5
	}

	recent := e.assocLog.RecentForVault(vaultID, 1)
	if len(recent) == 0 {
		return nil
	}

	seen := make(map[storage.ULID]struct{})
	var candidates []storage.ULID

	for _, id := range recent[0].EngramIDs {
		targets, err := e.transStore.GetTopTransitions(ctx, ws, [16]byte(id), maxInjections)
		if err != nil {
			slog.Warn("PAS: transition candidate retrieval degraded", "error", err)
			continue
		}
		for _, t := range targets {
			tid := storage.ULID(t.ID)
			if _, dup := seen[tid]; dup {
				continue
			}
			seen[tid] = struct{}{}
			candidates = append(candidates, tid)
			if len(candidates) >= maxInjections {
				return candidates
			}
		}
	}
	return candidates
}

// phase4_5TransitionBoost applies PAS transition boost to candidates.
// For each candidate already in the fused list, checks if it's a transition target
// from the previous activation. If so, sets transitionBoost = normalized count.
func (e *ActivationEngine) phase4_5TransitionBoost(ctx context.Context, ws [8]byte, vaultID uint32, candidates []fusedCandidate) {
	if e.transStore == nil {
		return
	}

	recent := e.assocLog.RecentForVault(vaultID, 1)
	if len(recent) == 0 {
		return
	}

	// Collect all transition targets from each previous result engram.
	transTargets := make(map[storage.ULID]uint32)
	var globalMax uint32

	for _, id := range recent[0].EngramIDs {
		targets, err := e.transStore.GetTopTransitions(ctx, ws, [16]byte(id), 20)
		if err != nil {
			slog.Warn("phase4.5: transition store read degraded", "error", err)
			continue
		}
		for _, t := range targets {
			tid := storage.ULID(t.ID)
			if existing, ok := transTargets[tid]; ok {
				if t.Count > existing {
					transTargets[tid] = t.Count
				}
			} else {
				transTargets[tid] = t.Count
			}
			if t.Count > globalMax {
				globalMax = t.Count
			}
		}
	}

	if len(transTargets) == 0 || globalMax == 0 {
		return
	}

	for i := range candidates {
		if count, ok := transTargets[candidates[i].id]; ok {
			boost := float64(count) / float64(globalMax)
			if boost > 1.0 {
				boost = 1.0
			}
			candidates[i].transitionBoost = boost
		}
	}
}

// traversedCandidate is one node discovered via BFS.
type traversedCandidate struct {
	id         storage.ULID
	propagated float64
	hopPath    []storage.ULID
	relType    uint16
}

// resolveProfile implements the C-B-A traversal profile resolution chain:
//  1. Explicit per-request profile override (A) — if valid, use it.
//  2. Auto-inferred from context phrases (C) — if score >= 2, use inferred.
//  3. Vault Plasticity default (B) — if set, use it.
//  4. Hardcoded "default" profile.
//
// Returns both the resolved profile name and the profile pointer. Never returns nil.
func resolveProfile(req *ActivateRequest) (string, *TraversalProfile) {
	name := strings.ToLower(strings.TrimSpace(req.Profile))
	if name != "" && ValidProfileName(name) {
		return name, GetProfile(name)
	}
	inferredName := InferProfile(req.Context, req.VaultDefault)
	return inferredName, GetProfile(inferredName)
}

// phase4_75ArchiveRestore checks the Bloom filter for archived edges among
// the fused candidate IDs and lazily restores them before BFS traversal.
// False positives from the Bloom filter trigger a cheap storage scan that
// returns immediately when no archive keys are found; false negatives are
// impossible, so no archived edges are silently skipped.
// Returns the set of edges that were restored (src→dst pairs) for forwarding
// to the Cortex via CognitiveForwarder.
func (e *ActivationEngine) phase4_75ArchiveRestore(ctx context.Context, ws [8]byte, candidates []fusedCandidate) []mbp.EdgeRef {
	var restoredEdges []mbp.EdgeRef
	for _, c := range candidates {
		if !e.store.ArchiveBloomMayContain([16]byte(c.id)) {
			continue
		}
		// Restore top-10 direct + top-5 transitive neighbors.
		restored, err := e.store.RestoreArchivedEdgesTransitive(ctx, ws, c.id, 10, 5)
		if err != nil || len(restored) == 0 {
			continue
		}
		for _, dst := range restored {
			restoredEdges = append(restoredEdges, mbp.EdgeRef{
				Src: [16]byte(c.id),
				Dst: [16]byte(dst),
			})
		}
	}
	return restoredEdges
}

// phase5Traverse explores the association graph via level-by-level BFS from top candidates.
// Each BFS level issues a single batched GetAssociations call for all nodes at that depth,
// reducing Pebble iterator opens from O(nodes) to O(hops) — typically 2 calls instead of 200+.
func (e *ActivationEngine) phase5Traverse(
	ctx context.Context,
	req *ActivateRequest,
	ws [8]byte,
	profile *TraversalProfile,
	candidates []fusedCandidate,
) []traversedCandidate {
	if len(candidates) == 0 {
		return nil
	}

	const (
		hopPenalty      = 0.7
		minHopScore     = 0.05
		maxBFSNodes     = 500
		maxEdgesPerNode = 10
		maxSeeds        = 20
	)

	seedCount := maxSeeds
	if seedCount > len(candidates) {
		seedCount = len(candidates)
	}

	seen := make(map[storage.ULID]bool, len(candidates)+maxBFSNodes)
	for _, c := range candidates {
		seen[c.id] = true
	}

	type levelItem struct {
		id        storage.ULID
		baseScore float64
		hopDepth  int
		hopPath   []storage.ULID
	}

	// Seed the first level from top candidates.
	currentLevel := make([]levelItem, 0, seedCount)
	for _, seed := range candidates[:seedCount] {
		currentLevel = append(currentLevel, levelItem{
			id:        seed.id,
			baseScore: seed.rrfScore,
			hopDepth:  0,
			hopPath:   []storage.ULID{seed.id},
		})
	}

	var discovered []traversedCandidate
	expanded := 0

	for len(currentLevel) > 0 && expanded < maxBFSNodes {
		// Check context deadline at the start of each BFS level.
		// Each level issues a Pebble batch read; on large vaults with 8-hop depth
		// this can loop many times — abort early if the caller timed out.
		select {
		case <-ctx.Done():
			slog.Warn("activation: bfs truncated by context deadline",
				"vault", req.VaultID, "expanded", expanded)
			return discovered
		default:
		}

		// Collect IDs eligible for expansion at this level.
		ids := make([]storage.ULID, 0, len(currentLevel))
		eligible := currentLevel[:0:len(currentLevel)]
		eligible = eligible[:0]
		for _, item := range currentLevel {
			if item.hopDepth < req.HopDepth {
				ids = append(ids, item.id)
				eligible = append(eligible, item)
			}
		}
		if len(ids) == 0 {
			break
		}

		// One batched Pebble call for the entire level.
		assocMap, err := e.store.GetAssociations(ctx, ws, ids, maxEdgesPerNode)
		if err != nil {
			slog.Warn("activation: bfs associations error, truncating traversal",
				"vault", req.VaultID, "hop", eligible[0].hopDepth, "error", err)
			break
		}

		// Fast exit: if no associations exist at this level, deeper levels won't either.
		// Avoids a second BFS round when the corpus has no Hebbian associations yet.
		hasAny := false
		for _, a := range assocMap {
			if len(a) > 0 {
				hasAny = true
				break
			}
		}
		if !hasAny {
			break
		}

		var nextLevel []levelItem
	outer:
		for _, curr := range eligible {
			for _, assoc := range assocMap[curr.id] {
				if seen[assoc.TargetID] {
					continue
				}

				// Profile filtering: skip edges excluded by the traversal profile.
				if !profile.AllowsEdge(assoc.RelType) {
					continue
				}

				boost := float64(profile.BoostFor(assoc.RelType))
				propagated := curr.baseScore * float64(assoc.Weight) * boost * math.Pow(hopPenalty, float64(curr.hopDepth+1))
				if propagated < minHopScore {
					// With per-type boost, weight order alone doesn't guarantee score order.
					// Use continue (not break) so a later low-weight/high-boost edge isn't skipped.
					continue
				}

				seen[assoc.TargetID] = true
				expanded++

				hopPath := make([]storage.ULID, len(curr.hopPath)+1)
				copy(hopPath, curr.hopPath)
				hopPath[len(curr.hopPath)] = assoc.TargetID

				discovered = append(discovered, traversedCandidate{
					id:         assoc.TargetID,
					propagated: propagated,
					hopPath:    hopPath,
					relType:    uint16(assoc.RelType),
				})

				if curr.hopDepth+1 < req.HopDepth {
					nextLevel = append(nextLevel, levelItem{
						id:        assoc.TargetID,
						baseScore: propagated,
						hopDepth:  curr.hopDepth + 1,
						hopPath:   hopPath,
					})
				}

				if expanded >= maxBFSNodes {
					break outer
				}
			}
		}
		currentLevel = nextLevel
	}
	return discovered
}

// phase6Score computes final scores, applies filters, and builds the result.
func (e *ActivationEngine) phase6Score(
	ctx context.Context,
	req *ActivateRequest,
	ws [8]byte,
	fused []fusedCandidate,
	traversed []traversedCandidate,
	p1 *phase1Result,
) (*ActivateResult, error) {

	w := resolveWeights(req.Weights, e.weights)

	// Guard: RRF and CGDN are mutually exclusive scoring paths.
	// If both are enabled, RRF takes precedence (checked first below).
	// Log the conflict so operators can fix their plasticity config.
	if w.UseRRFFusion && w.UseCGDN {
		slog.Warn("scoring: both RRF and CGDN enabled -- RRF takes precedence, CGDN ignored")
		w.UseCGDN = false
	}

	type scoringCandidate struct {
		id              storage.ULID
		ftsScore        float64
		vectorScore     float64
		hebbianBoost    float64
		transitionBoost float64
		rrfScore        float64
		hopPath         []storage.ULID
		relType         uint16
		isTraversed     bool // true for BFS-only candidates; vectorScore is computed post-load
		inTagPool       bool // true for tag-seeded candidates; vectorScore is computed post-load when zero
	}

	// Deduplicate: fused candidates take priority; traversed candidates are
	// only added if their ID has not already appeared in the fused set.
	// Fused candidates are already deduplicated by RRF, so no seen-check needed for them.
	all := make([]scoringCandidate, 0, len(fused)+len(traversed))
	for _, c := range fused {
		all = append(all, scoringCandidate{
			id:              c.id,
			ftsScore:        c.ftsScore,
			vectorScore:     c.vectorScore,
			hebbianBoost:    c.hebbianBoost,
			transitionBoost: c.transitionBoost,
			rrfScore:        c.rrfScore,
			inTagPool:       c.inTagPool,
		})
	}
	// Only run dedup if there are traversed candidates to merge.
	if len(traversed) > 0 {
		seen := make(map[storage.ULID]struct{}, len(fused))
		for _, c := range fused {
			seen[c.id] = struct{}{}
		}
		for _, t := range traversed {
			if _, dup := seen[t.id]; dup {
				continue
			}
			// Route the BFS propagated score to both rrfScore (for RRF mode) and
			// hebbianBoost (for ACT-R/CGDN spreading activation).
			// rrfScore must be non-zero: RRF final = rrfScore × (1 + hebbianBoost + ...)
			// so zero rrfScore silences traversed candidates in RRF mode at any threshold > 0.
			// vectorScore is computed after engrams are loaded.
			all = append(all, scoringCandidate{
				id:           t.id,
				rrfScore:     t.propagated,
				hebbianBoost: math.Min(t.propagated, 1.0),
				hopPath:      t.hopPath,
				relType:      t.relType,
				isTraversed:  true,
			})
		}
	}

	ids := make([]storage.ULID, len(all))
	for i, c := range all {
		ids[i] = c.id
	}

	// Look up per-engram cache access time BEFORE GetEngrams to avoid
	// contamination: GetEngrams populates/touches the L1 cache (setting
	// lastAccess = now), which would make every engram appear "just accessed."
	// By reading first, only engrams recalled in a *prior* activation carry
	// a cache timestamp; cache-cold engrams return 0 so the scorer falls
	// back to eng.LastAccess (the persisted write/CreatedAt time).
	lastAccessNsByID := make(map[storage.ULID]int64, len(all))
	for _, c := range all {
		if ns := e.store.EngramLastAccessNs(ws, c.id); ns != 0 {
			lastAccessNsByID[c.id] = ns
		}
	}

	// Load full engrams for all candidates in one pass.
	// Previously this was two passes: GetMetadata (all candidates) + GetEngrams (scored subset).
	// Loading full engrams upfront eliminates the second pass entirely — engrams are already
	// in hand when building the activation result. The extra bytes per candidate (~2-8KB vs ~46B
	// for metadata-only) are worth eliminating an entire Pebble read round-trip.
	allEngrams, err := e.store.GetEngrams(ctx, ws, ids)
	if err != nil {
		return nil, fmt.Errorf("phase6 get engrams: %w", err)
	}

	// Ownership-lease work-queue visibility (#548): hide engrams checked out by a
	// live lease owned by someone other than the caller, mirroring how soft-deleted
	// engrams are excluded. Staleness is evaluated here against the server clock, so
	// an expired lease never hides anything. Skipped entirely when IncludeLeased is
	// set (admin/debug opt-out).
	leaseFilterNow := time.Now()
	var leaseByID map[storage.ULID]storage.Lease
	if !req.IncludeLeased {
		leases, err := e.store.GetLeases(ctx, ws, ids)
		if err != nil {
			return nil, fmt.Errorf("phase6 get leases: %w", err)
		}
		leaseByID = make(map[storage.ULID]storage.Lease, len(ids))
		for i, id := range ids {
			leaseByID[id] = leases[i]
		}
	}

	// Filter out soft-deleted engrams (defense-in-depth; HNSW has no delete method).
	// Also filter untrusted engrams when ExcludeUntrusted is set in the request.
	var active []*storage.Engram
	for _, eng := range allEngrams {
		if eng == nil {
			continue
		}
		if eng.State == storage.StateSoftDeleted || eng.State == storage.StateArchived {
			continue
		}
		// Hard trust filter: skip engrams with TrustUntrusted (0x04) when requested.
		// TrustUnset (0x00) is intentionally passed through — it is the zero-value
		// backward-compat alias for TrustInferred, not an "unknown" or untrusted value.
		if req.ExcludeUntrusted && eng.Trust == storage.TrustUntrusted {
			continue
		}
		// Work-queue checkout: hide engrams under a live foreign lease.
		if !req.IncludeLeased {
			if l := leaseByID[eng.ID]; l.Live(leaseFilterNow) && l.Owner != req.CallerOwner {
				continue
			}
		}
		active = append(active, eng)
	}
	allEngrams = active

	engramByID := make(map[storage.ULID]*storage.Engram, len(allEngrams))
	for _, eng := range allEngrams {
		if eng != nil {
			engramByID[eng.ID] = eng
		}
	}

	// Compute vectorScore for candidates that entered the pipeline without an HNSW
	// score now that engrams are loaded. Two cases need this:
	//   - BFS-traversed candidates (never in the HNSW pool).
	//   - Tag-seeded candidates that appear in no other pool (vectorScore == 0):
	//     without this, ACT-R/CGDN/legacy contentMatch is zero and the tag hit is
	//     threshold-dropped one layer below the seeding fix (caveat 1 of #607).
	// A non-zero vectorScore from the HNSW pool is never overwritten.
	// ftsScore is left at zero: BM25 requires corpus-level IDF statistics unavailable here.
	if len(p1.embedding) > 0 {
		for i := range all {
			needsCosine := all[i].isTraversed || (all[i].inTagPool && all[i].vectorScore == 0)
			if !needsCosine {
				continue
			}
			if eng := engramByID[all[i].id]; eng != nil && len(eng.Embedding) > 0 {
				all[i].vectorScore = float64(cosineSimilarity32(p1.embedding, eng.Embedding))
			}
		}
	}

	type scoredItem struct {
		id         storage.ULID
		final      float64
		components ScoreComponents
		hopPath    []storage.ULID
	}

	now := time.Now()
	scored := make([]scoredItem, 0, len(all))

	// RRF fusion path: use Phase 3 RRF scores directly as the final score basis.
	// Rank-based and scale-invariant (Cormack et al. 2009). Cognitive boosts
	// (Hebbian, transition, confidence) are applied after fusion.
	if w.UseRRFFusion {
		for _, c := range all {
			eng := engramByID[c.id]
			if eng == nil || !PassesMetaFilter(eng, req.Filters) || !PassesValidity(eng, req.AsOf, req.IncludeInvalid, now) {
				continue
			}
			final := computeRRFScore(c.rrfScore, c.hebbianBoost, c.transitionBoost, eng)
			// A filter defines the candidate SET; the relevance threshold only
			// RANKS within it. An explicit tag-filter match (inTagPool, already
			// verified by PassesMetaFilter above) must never be dropped for
			// scoring below threshold — otherwise "due:<=today" reminders that
			// are content-unrelated to the query silently vanish. Non-tag
			// candidates are still thresholded normally.
			if final < req.Threshold && !c.inTagPool {
				continue
			}
			// Populate ScoreComponents for observability: report the individual
			// signal scores so callers can understand the composition even though
			// the final score is rank-based. c.ftsScore is already a calibrated,
			// absolute [0,1] coverage score post-#711 — no tanh normalization.
			normalizedFTS := c.ftsScore
			scored = append(scored, scoredItem{
				id:    c.id,
				final: final,
				components: ScoreComponents{
					SemanticSimilarity: c.vectorScore,
					FullTextRelevance:  normalizedFTS,
					HebbianBoost:       c.hebbianBoost,
					TransitionBoost:    c.transitionBoost,
					Confidence:         float64(eng.Confidence),
					Raw:                c.rrfScore * (1.0 + c.hebbianBoost + c.transitionBoost),
					Final:              final,
				},
				hopPath: c.hopPath,
			})
		}
		sort.Slice(scored, func(i, j int) bool {
			if scored[i].final != scored[j].final {
				return scored[i].final > scored[j].final
			}
			return bytes.Compare(scored[i].id[:], scored[j].id[:]) < 0
		})
		goto cgdnDone
	}

	// CGDN path: two-pass scoring with divisive normalization.
	// Pass 1 computes gated activations a(d) for all candidates; Pass 2 normalizes.
	// This replicates lateral inhibition in hippocampal retrieval: cognitive state
	// multiplicatively gates content relevance, then candidates compete via division.
	if w.UseCGDN {
		type cgdnItem struct {
			c interface {
				getBase() (storage.ULID, float64, float64, float64, []storage.ULID)
			}
			eng        *storage.Engram
			activation float64
			components ScoreComponents
			hopPath    []storage.ULID
		}

		// Pass 1: compute gated activations for all valid candidates.
		type cgdnCandidate struct {
			id         storage.ULID
			activation float64
			components ScoreComponents
			hopPath    []storage.ULID
			inTagPool  bool
		}
		cgdnCands := make([]cgdnCandidate, 0, len(all))
		for _, c := range all {
			eng := engramByID[c.id]
			if eng == nil || !PassesMetaFilter(eng, req.Filters) || !PassesValidity(eng, req.AsOf, req.IncludeInvalid, now) {
				continue
			}
			// Compute component scores (reuse existing helpers for decay, FTS normalization etc.)
			comp := computeComponents(c.vectorScore, c.ftsScore, c.hebbianBoost, eng, lastAccessNsByID[c.id], now, w)
			// Gated activation: content relevance × cognitive gate
			a := computeGatedActivation(comp.SemanticSimilarity, comp.FullTextRelevance, comp.DecayFactor, comp.HebbianBoost, w)
			cgdnCands = append(cgdnCands, cgdnCandidate{
				id: c.id, activation: a, components: comp, hopPath: c.hopPath, inTagPool: c.inTagPool,
			})
		}

		if len(cgdnCands) > 0 {
			// Compute σ = median activation (self-calibrating operating point).
			acts := make([]float64, len(cgdnCands))
			for i, cc := range cgdnCands {
				acts[i] = cc.activation
			}
			sort.Float64s(acts)
			sigma := acts[len(acts)/2]
			if sigma <= 0 {
				sigma = 0.01
			}

			// Compute divisive normalization denominator: σ^n + Σ a(j)^n
			n := w.CGDNPower
			var denomSum float64
			for _, a := range acts {
				denomSum += math.Pow(a, n)
			}
			denom := math.Pow(sigma, n) + denomSum

			// Pass 2: compute R(d) = a(d)^n / denom, apply confidence, threshold.
			for _, cc := range cgdnCands {
				r := math.Pow(cc.activation, n) / denom
				final := r * cc.components.Confidence
				// Tag-filter matches bypass the relevance threshold — the filter
				// defines the set (see the RRF path above for the full rationale).
				if final < req.Threshold && !cc.inTagPool {
					continue
				}
				cc.components.Raw = r
				cc.components.Final = final
				scored = append(scored, scoredItem{
					id: cc.id, final: final, components: cc.components, hopPath: cc.hopPath,
				})
			}
		}

		sort.Slice(scored, func(i, j int) bool {
			if scored[i].final != scored[j].final {
				return scored[i].final > scored[j].final
			}
			return bytes.Compare(scored[i].id[:], scored[j].id[:]) < 0
		})
		goto cgdnDone
	}

	// ACT-R path: two-pass with per-query normalization.
	// Pass 1 collects raw scores; for fresh engrams softplus(B(M)) exceeds the
	// median-activation denominator, so raw > 1.0. The old hard clamp at 1.0
	// collapsed all saturated scores to the same value, destroying ranking in
	// new vaults (issue #331). Pass 2 rescales by the query's max raw score
	// when saturation occurred. For mature vaults where max raw ≤ 1.0 the
	// scale factor is 1.0 — behaviour is identical to the old path.
	if w.UseACTR {
		type actrCandidate struct {
			id         storage.ULID
			components ScoreComponents
			hopPath    []storage.ULID
			inTagPool  bool
		}
		actrCands := make([]actrCandidate, 0, len(all))
		maxRaw := 0.0
		for _, c := range all {
			eng := engramByID[c.id]
			if eng == nil || !PassesMetaFilter(eng, req.Filters) || !PassesValidity(eng, req.AsOf, req.IncludeInvalid, now) {
				continue
			}
			components := computeACTR(c.vectorScore, c.ftsScore, c.hebbianBoost, c.transitionBoost, eng, lastAccessNsByID[c.id], now, w, c.inTagPool)
			if components.Raw > maxRaw {
				maxRaw = components.Raw
			}
			actrCands = append(actrCands, actrCandidate{id: c.id, components: components, hopPath: c.hopPath, inTagPool: c.inTagPool})
		}
		// Rescale all raw scores by 1/maxRaw when any candidate saturated above 1.0.
		// This preserves the [0,1] contract and relative ranking without altering the
		// formula for mature vaults where scores already spread below 1.0.
		scale := 1.0
		if maxRaw > 1.0 {
			scale = 1.0 / maxRaw
		}
		for _, cc := range actrCands {
			raw := math.Min(cc.components.Raw*scale, 1.0)
			final := raw * cc.components.Confidence
			// Tag-filter matches bypass the relevance threshold — the filter
			// defines the set (see the RRF path above for the full rationale).
			if final < req.Threshold && !cc.inTagPool {
				continue
			}
			cc.components.Raw = raw
			cc.components.Final = final
			scored = append(scored, scoredItem{id: cc.id, final: final, components: cc.components, hopPath: cc.hopPath})
		}
		sort.Slice(scored, func(i, j int) bool {
			if scored[i].final != scored[j].final {
				return scored[i].final > scored[j].final
			}
			return bytes.Compare(scored[i].id[:], scored[j].id[:]) < 0
		})
		goto cgdnDone
	}

	// Legacy weighted-sum path: used when neither CGDN nor ACT-R is active (DisableACTR=true).
	for _, c := range all {
		eng := engramByID[c.id]
		if eng == nil || !PassesMetaFilter(eng, req.Filters) || !PassesValidity(eng, req.AsOf, req.IncludeInvalid, now) {
			continue
		}
		components := computeComponents(c.vectorScore, c.ftsScore, c.hebbianBoost, eng, lastAccessNsByID[c.id], now, w)
		final := components.Final
		// Tag-filter matches bypass the relevance threshold — the filter
		// defines the set (see the RRF path above for the full rationale).
		if final < req.Threshold && !c.inTagPool {
			continue
		}
		scored = append(scored, scoredItem{id: c.id, final: final, components: components, hopPath: c.hopPath})
	}
	sort.Slice(scored, func(i, j int) bool {
		if scored[i].final != scored[j].final {
			return scored[i].final > scored[j].final
		}
		return bytes.Compare(scored[i].id[:], scored[j].id[:]) < 0
	})

cgdnDone:
	totalFound := len(scored)
	if len(scored) > req.MaxResults {
		scored = scored[:req.MaxResults]
	}

	// Apply structured filter if provided (post-retrieval predicate).
	// This is applied AFTER RRF scoring and confidence checks, as the final step.
	if req.StructuredFilter != nil {
		filtered := make([]scoredItem, 0, len(scored))
		for _, s := range scored {
			eng := engramByID[s.id]
			if eng == nil {
				continue
			}
			if req.StructuredFilter.Match(eng) {
				filtered = append(filtered, s)
			}
		}
		scored = filtered
	}

	activations := make([]ScoredEngram, 0, len(scored))
	for _, s := range scored {
		eng := engramByID[s.id]
		if eng == nil {
			continue
		}
		// Build hopConcepts post-truncation: only for surviving items, saving
		// allocation for all candidates that were filtered or truncated away.
		var hopConcepts []string
		if len(s.hopPath) > 0 {
			hopConcepts = make([]string, 0, len(s.hopPath))
			for _, hopID := range s.hopPath {
				if hopEng := engramByID[hopID]; hopEng != nil {
					hopConcepts = append(hopConcepts, hopEng.Concept)
				}
			}
		}
		var why string
		if req.IncludeWhy {
			why = buildWhy(eng, s.components, s.hopPath, hopConcepts, p1.queryStr, w.UseACTR)
		}
		activations = append(activations, ScoredEngram{
			Engram:      eng,
			Score:       s.final,
			Components:  s.components,
			Why:         why,
			HopPath:     append([]storage.ULID(nil), s.hopPath...),
			HopConcepts: hopConcepts,
			Dormant:     !w.UseACTR && eng.Relevance <= minFloor*1.1,
		})
	}

	return &ActivateResult{
		QueryID:     newQueryID(),
		Activations: activations,
		TotalFound:  totalFound,
	}, nil
}

// computeComponents calculates all scoring components for a candidate engram.
// Accepts *storage.Engram directly — avoids a separate GetMetadata call in phase6.
// lastAccessNs is the nanosecond timestamp of last cache access (0 if not cached).
func computeComponents(vectorScore, ftsScore, hebbianBoost float64, eng *storage.Engram, lastAccessNs int64, now time.Time, w resolvedWeights) ScoreComponents {
	const accessFreqSaturation = 100.0
	const recencyHalfLifeDays = 7.0

	accessFreq := math.Log1p(float64(eng.AccessCount)) / math.Log1p(accessFreqSaturation)
	if accessFreq > 1.0 {
		accessFreq = 1.0
	}

	// Use cache lastAccess if available (reflects actual recall time); else use persisted eng.LastAccess.
	var lastAccess time.Time
	if lastAccessNs > 0 {
		lastAccess = time.Unix(0, lastAccessNs)
	} else {
		lastAccess = eng.LastAccess
	}
	daysSince := now.Sub(lastAccess).Hours() / 24.0
	// Clamp clock skew: a future LastAccess (NTP step, or a cache timestamp
	// ahead of wall clock) yields a negative daysSince, which would push recency
	// and the decay factor above 1.0. Treat it as "just accessed".
	if daysSince < 0 {
		daysSince = 0
	}
	recency := math.Exp(-daysSince * math.Log(2) / recencyHalfLifeDays)

	decayFactor := math.Max(0.05, math.Exp(-daysSince/float64(eng.Stability)))

	// ftsScore is ALREADY a calibrated, absolute [0,1] coverage score (see
	// fts.Index.Search and COG-24) — no further normalization needed. Before
	// #711 this applied math.Tanh() to squash raw unbounded BM25 into [0,1];
	// that saturated by x≈3 (real BM25 magnitudes ran 2-40), making a single
	// common-word match indistinguishable from a genuine multi-term match.
	normalizedFTS := ftsScore

	raw := w.SemanticSimilarity*vectorScore +
		w.FullTextRelevance*normalizedFTS +
		w.DecayFactor*decayFactor +
		w.HebbianBoost*hebbianBoost +
		w.AccessFrequency*accessFreq +
		w.Recency*recency

	if raw > 1.0 {
		raw = 1.0
	}
	if raw < 0.0 {
		raw = 0.0
	}

	conf := float64(eng.Confidence)

	return ScoreComponents{
		SemanticSimilarity: vectorScore,
		FullTextRelevance:  normalizedFTS, // normalized [0,1), not raw BM25
		DecayFactor:        decayFactor,
		HebbianBoost:       hebbianBoost,
		AccessFrequency:    accessFreq,
		Recency:            recency,
		Confidence:         conf,
		Raw:                raw,
		Final:              raw * conf,
	}
}

// actrDenominator is the precomputed normalization denominator used in computeACTR.
// It equals 1 + softplus(0) = 1 + ln(1 + exp(0)) = 1 + ln(2) ≈ 1.6931471805599453.
// Precomputing this constant avoids recomputing softplus(0) on every engram scored.
const actrDenominator = 1.6931471805599453

// tagMatchFloor is the minimum content-match value granted to candidates that
// matched an explicit tag filter (inTagPool=true), under ACT-R scoring (COG-5
// amendment for S1). Rationale for 0.1: genuine content matches (semantic
// cosine similarity or normalized FTS relevance) typically land in the
// 0.5-0.9 range for anything a human would call "relevant" — 0.1 sits well
// below that band, so a real content match always outranks a floored
// tag-only hit. It is comfortably above zero so that, combined with a
// non-trivial base-level/confidence, an explicit tag-filter match survives
// the default activation threshold (0.3) instead of being silently dropped.
const tagMatchFloor = 0.1

// softplus computes ln(1 + exp(x)), mapping (-inf,+inf) to (0,+inf).
// Used as the activation function in ACT-R scoring: ensures the contextual prior
// is always positive and smoothly transitions from near-zero to near-linear.
// Numerically stable: for large positive x, softplus(x) ≈ x; for large negative x, ≈ exp(x).
func softplus(x float64) float64 {
	if x > 20 {
		return x // avoid overflow: softplus(x) ≈ x for large x
	}
	return math.Log1p(math.Exp(x))
}

// cosineSimilarity32 computes cosine similarity between two float32 vectors.
// Returns 0 for empty or mismatched-length inputs.
// Uses the same unrolled 4-wide dot product as the HNSW index for consistency.
func cosineSimilarity32(a, b []float32) float32 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, na, nb float32
	i := 0
	for ; i+3 < len(a); i += 4 {
		dot += a[i]*b[i] + a[i+1]*b[i+1] + a[i+2]*b[i+2] + a[i+3]*b[i+3]
		na += a[i]*a[i] + a[i+1]*a[i+1] + a[i+2]*a[i+2] + a[i+3]*a[i+3]
		nb += b[i]*b[i] + b[i+1]*b[i+1] + b[i+2]*b[i+2] + b[i+3]*b[i+3]
	}
	for ; i < len(a); i++ {
		dot += a[i] * b[i]
		na += a[i] * a[i]
		nb += b[i] * b[i]
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (float32(math.Sqrt(float64(na))) * float32(math.Sqrt(float64(nb))))
}

// computeACTR computes the ACT-R scoring components for a candidate engram.
// Formula (Anderson 1993):
//
//	B(M) = min(ln(n+1) - d × ln(max(ageDays,ageFloor) / (n+1)), bLevelCap)  [base-level activation]
//	where bLevelCap = ln(exp(actrDenominator)-1) ≈ 1.489 is the unique value at which
//	softplus(B(M)) = actrDenominator, i.e. base-level alone would push raw = contentMatch.
//	Capping here preserves score absoluteness and threshold semantics across queries.
//	Score = ContentMatch × softplus(B(M) + scale×Hebbian) × Confidence
//
// ContentMatch gates the score: zero semantic relevance = zero score regardless of recency.
// B(M) + scale×Hebbian are additive: Hebbian can rescue old but linked memories.
// This resolves the decay-vs-Hebbian tension without two separate pathways.
func computeACTR(vectorScore, ftsScore, hebbianBoost, transitionBoost float64, eng *storage.Engram,
	lastAccessNs int64, now time.Time, w resolvedWeights, inTagPool bool) ScoreComponents {

	// Compute content relevance (same as standard path). ftsScore is already a
	// calibrated, absolute [0,1] coverage score (see fts.Index.Search, COG-24) —
	// no tanh normalization needed post-#711.
	normalizedFTS := ftsScore
	contentMatch := w.SemanticSimilarity*vectorScore + w.FullTextRelevance*normalizedFTS

	// COG-5 amendment (S1): candidates that matched an explicit tag filter
	// (inTagPool) receive a content-match floor so an explicit filter match
	// surfaces regardless of semantic/lexical overlap with the query. Without
	// this, a content-unrelated tag hit scores contentMatch=0 and is dropped
	// by the gate below even though the user explicitly asked for it via
	// tags_all/tags_any/tag_prefix (S1 seeds these into the pool, but under
	// default ACT-R scoring the gate silently discarded them). tagMatchFloor
	// (0.1) is well below typical genuine content-match scores (0.5-0.9), so
	// a real semantic/lexical match always outranks a floored tag-only hit —
	// the floor only rescues candidates that would otherwise score exactly
	// zero. RRF scoring already honors inTagPool via its own pool boost and
	// is unaffected by this change. See docs/internals/invariants.md COG-5.
	if inTagPool && contentMatch < tagMatchFloor {
		contentMatch = tagMatchFloor
	}

	// Compute ACT-R base-level activation B(M).
	// B(M) = ln(n+1) - d × ln(max(ageDays, ageFloor) / (n+1))
	// High n + low ageDays → high B (fresh, frequently accessed → strong base level)
	// Low n + high ageDays → low B (old, rarely accessed → weak base level)
	var lastAccess time.Time
	if lastAccessNs > 0 {
		lastAccess = time.Unix(0, lastAccessNs)
	} else {
		lastAccess = eng.LastAccess
	}
	// Treat zero or pre-2000 LastAccess as "just now" — these are newly written
	// engrams that have never been accessed. A fresh write = maximum recency.
	if lastAccess.IsZero() || lastAccess.Year() < 2000 {
		lastAccess = now
	}
	const ageFloorDays = 1.0 / (24.0 * 60.0) // 1 minute — sub-hour precision for intraday recall
	ageDays := math.Max(now.Sub(lastAccess).Hours()/24.0, ageFloorDays)
	n := float64(eng.AccessCount + 1) // +1 avoids ln(0) for never-accessed engrams
	d := w.ACTRDecay                  // power-law forgetting exponent (default 0.5)
	baseLevel := math.Log(n) - d*math.Log(math.Max(ageDays, ageFloorDays)/n)
	// Cap baseLevel at the derived saturation threshold: the unique B(M) where
	// softplus(B(M)) = actrDenominator, i.e. where raw = contentMatch (zero Hebbian).
	// Above this, base-level alone exceeds the content-match gate — semantically wrong.
	// Preserves score absoluteness: threshold=0.3 means the same in fresh and mature vaults.
	// Hebbian boosts may still push totalActivation above the cap — that is intentional.
	bLevelCap := math.Log(math.Exp(actrDenominator) - 1) // ≈ 1.489
	if baseLevel > bLevelCap {
		baseLevel = bLevelCap
	}

	// Total activation = base-level + scaled Hebbian boost + scaled transition boost.
	// ACTRHebScale (default 4.0) amplifies both Hebbian and transition signals so
	// they can meaningfully rescue old memories, matching Anderson's spreading activation.
	// Transition boost uses the same scale as Hebbian — both represent contextual priors.
	totalActivation := baseLevel + w.ACTRHebScale*hebbianBoost + w.ACTRHebScale*transitionBoost

	// Contextual prior: softplus maps total activation to (0, +inf).
	contextualPrior := softplus(totalActivation)

	// Final raw score: ContentMatch gates contextual prior.
	// Normalize by actrDenominator = 1 + softplus(0) ≈ 1.693 so that a median-activation
	// memory with perfect content match produces raw ≈ 1.0. The upper bound is enforced
	// after per-query normalization in the ACT-R scoring path (see caller) — not here —
	// so the caller can see true relative magnitudes before rescaling.
	raw := contentMatch * contextualPrior / actrDenominator
	if raw < 0.0 {
		raw = 0.0
	}
	conf := float64(eng.Confidence)

	return ScoreComponents{
		SemanticSimilarity: vectorScore,
		FullTextRelevance:  normalizedFTS,
		DecayFactor:        math.Max(0.05, math.Exp(-ageDays/math.Max(float64(eng.Stability), 1.0))), // kept for reporting; guard against Stability=0
		HebbianBoost:       hebbianBoost,
		TransitionBoost:    transitionBoost,
		AccessFrequency:    math.Log1p(float64(eng.AccessCount)) / math.Log1p(100),
		Recency:            math.Exp(-ageDays * math.Log(2) / 7.0),
		Confidence:         conf,
		Raw:                raw,
		Final:              raw * conf,
	}
}

// computeRRFScore computes the final score for a candidate using the Phase 3
// RRF score directly as the scoring basis (Cormack et al. 2009).
//
// Unlike ACT-R/CGDN/weighted-sum which recompute scores from individual signal
// components, RRF fusion uses the rank-based score from Phase 3 and applies
// cognitive modifiers after fusion:
//
//	raw = rrfScore × (1 + hebbianBoost + transitionBoost)
//	final = raw × confidence
//
// This is scale-invariant: documents with the same ranks but different raw score
// magnitudes produce the same RRF score. Robust to score scale mismatches between
// BM25 (unbounded), HNSW cosine similarity [0,1], and graph traversal scores.
//
// Parameters match fusedCandidate fields so the function works with both
// fusedCandidate (Phase 3 output) and scoringCandidate (Phase 6 local type).
func computeRRFScore(rrfScore, hebbianBoost, transitionBoost float64, eng *storage.Engram) float64 {
	// Cognitive boost: Hebbian and transition boosts amplify the RRF score.
	// The (1 + boost) formulation ensures zero boost = no change, and positive
	// boosts provide multiplicative amplification proportional to association strength.
	cognitiveMultiplier := 1.0 + hebbianBoost + transitionBoost
	raw := rrfScore * cognitiveMultiplier
	conf := float64(eng.Confidence)
	return raw * conf
}

// computeGatedActivation computes the raw gated activation a(d) for CGDN.
//
// Formula (Hebbian-Rescue CGDN):
//
//	rescue(d) = max(0, hebbianBoost - ε) * λ
//	g(d)      = clamp(decayFactor^α + rescue(d), 0, 1)
//	a(d)      = (w_semantic*vectorScore + w_fts*normalizedFTS) * g(d)
//
// The Hebbian rescue term (additive, not multiplicative) is the key.
// Multiplicative gating `decay × hebbian` suppresses Hebbian-linked old memories
// because decay dominates. The additive rescue replicates hippocampal CA3 pattern
// completion where Hebbian activation partially RESTORES decayed memories:
//
//	Fresh, no Hebbian link:  g ≈ 0.85^1.5 + 0 ≈ 0.78  (high gate — surfaces)
//	Stale, no Hebbian link:  g ≈ 0.05^1.5 + 0 ≈ 0.01  (near-zero — suppressed)
//	Stale, Hebbian link 0.5: g ≈ 0.05^1.5 + 0.5*λ ≈ 0.01 + 0.40 = 0.41 (rescued!)
//	Stale, no Hebbian link:  g ≈ 0.05^1.5 + 0 ≈ 0.01  (still suppressed)
//
// This creates a 41x advantage for the Hebbian-linked stale vs unlinked stale,
// replicating retrieval-induced forgetting counteraction (Anderson & Bjork 1994)
// and memory reconsolidation (Nader et al. 2000).
func computeGatedActivation(vectorScore, normalizedFTS, decayFactor, hebbianBoost float64, w resolvedWeights) float64 {
	const (
		epsilon      = 0.01 // Hebbian floor — prevents zero rescue for unlinked engrams
		rescueLambda = 0.8  // Hebbian rescue strength — how much Hebbian can restore decay
	)
	rescue := math.Max(0, hebbianBoost-epsilon) * rescueLambda
	gate := math.Pow(decayFactor, w.CGDNAlpha) + rescue
	if gate > 1.0 {
		gate = 1.0
	}
	contentRelevance := w.SemanticSimilarity*vectorScore + w.FullTextRelevance*normalizedFTS
	return contentRelevance * gate
}

// PassesMetaFilter evaluates filter predicates against a full engram.
// Accepts *storage.Engram directly — avoids a separate GetMetadata call in phase6.
func PassesMetaFilter(eng *storage.Engram, filters []Filter) bool {
	for _, f := range filters {
		switch f.Field {
		case "state":
			if s, ok := f.Value.(storage.LifecycleState); ok {
				switch f.Op {
				case "eq":
					if eng.State != s {
						return false
					}
				case "neq":
					if eng.State == s {
						return false
					}
				}
			}
		case "created_after":
			if t, ok := f.Value.(time.Time); ok {
				if !eng.CreatedAt.After(t) {
					return false
				}
			}
		case "created_before":
			if t, ok := f.Value.(time.Time); ok {
				if !eng.CreatedAt.Before(t) {
					return false
				}
			}
		case "tags_all":
			// All listed tags must be present on the engram (AND).
			if want := asStringSlice(f.Value); len(want) > 0 {
				set := tagSet(eng.Tags)
				for _, w := range want {
					if _, ok := set[w]; !ok {
						return false
					}
				}
			}
		case "tags_any":
			// At least one listed tag must be present (OR).
			if want := asStringSlice(f.Value); len(want) > 0 {
				set := tagSet(eng.Tags)
				matched := false
				for _, w := range want {
					if _, ok := set[w]; ok {
						matched = true
						break
					}
				}
				if !matched {
					return false
				}
			}
		case "tag_prefix":
			// Value is [prefix, bound]; for each engram tag beginning with
			// prefix, compare the remainder lexically against bound per Op
			// (lte/gte/lt/gt/eq). Matches if ANY such tag satisfies the bound.
			// String comparison suffices for ISO dates and other sortable
			// key:value tag conventions.
			if pb := asPair(f.Value); pb != nil {
				prefix, bound := pb[0], pb[1]
				matched := false
				for _, tag := range eng.Tags {
					if !strings.HasPrefix(tag, prefix) {
						continue
					}
					v := tag[len(prefix):]
					switch f.Op {
					case "lte":
						matched = v <= bound
					case "gte":
						matched = v >= bound
					case "lt":
						matched = v < bound
					case "gt":
						matched = v > bound
					case "eq", "":
						matched = v == bound
					}
					if matched {
						break
					}
				}
				if !matched {
					return false
				}
			}
		}
	}
	return true
}

// tagSet builds a lookup set from an engram's tags.
func tagSet(tags []string) map[string]struct{} {
	set := make(map[string]struct{}, len(tags))
	for _, t := range tags {
		set[t] = struct{}{}
	}
	return set
}

// asStringSlice coerces a filter Value to []string, accepting both the
// in-process []string and a msgpack-decoded []interface{} of strings.
func asStringSlice(v interface{}) []string {
	switch s := v.(type) {
	case []string:
		return s
	case []interface{}:
		out := make([]string, 0, len(s))
		for _, e := range s {
			if str, ok := e.(string); ok {
				out = append(out, str)
			}
		}
		return out
	}
	return nil
}

// asPair coerces a tag_prefix Value to a [prefix, bound] pair, accepting the
// in-process [2]string and a msgpack-decoded []interface{}/[]string of two.
func asPair(v interface{}) *[2]string {
	switch p := v.(type) {
	case [2]string:
		return &p
	case []string:
		if len(p) == 2 {
			return &[2]string{p[0], p[1]}
		}
	case []interface{}:
		if len(p) == 2 {
			a, ok1 := p[0].(string)
			b, ok2 := p[1].(string)
			if ok1 && ok2 {
				return &[2]string{a, b}
			}
		}
	}
	return nil
}

func resolveWeights(req *Weights, def DefaultWeights) resolvedWeights {
	if req == nil {
		// No weights provided (e.g. tests): use ACT-R with defaults. Decay path is not reachable for now.
		return resolvedWeights{
			SemanticSimilarity: float64(def.SemanticSimilarity),
			FullTextRelevance:  float64(def.FullTextRelevance),
			DecayFactor:        float64(def.DecayFactor),
			HebbianBoost:       float64(def.HebbianBoost),
			AccessFrequency:    float64(def.AccessFrequency),
			Recency:            float64(def.Recency),
			UseACTR:            true, // default path always uses ACT-R
			ACTRDecay:          0.5,
			ACTRHebScale:       4.0,
		}
	}
	rw := resolvedWeights{
		SemanticSimilarity: float64(req.SemanticSimilarity),
		FullTextRelevance:  float64(req.FullTextRelevance),
		DecayFactor:        float64(req.DecayFactor),
		HebbianBoost:       float64(req.HebbianBoost),
		AccessFrequency:    float64(req.AccessFrequency),
		Recency:            float64(req.Recency),
		UseCGDN:            req.UseCGDN,
		UseACTR:            !req.DisableACTR,
		UseRRFFusion:       req.UseRRFFusion,
	}
	// Apply CGDN defaults when enabled.
	if req.UseCGDN {
		rw.CGDNAlpha = 1.5
		if req.CGDNAlpha > 0 {
			rw.CGDNAlpha = float64(req.CGDNAlpha)
		}
		rw.CGDNBeta = 0.5
		if req.CGDNBeta > 0 {
			rw.CGDNBeta = float64(req.CGDNBeta)
		}
		rw.CGDNPower = 2.0
		if req.CGDNPower > 0 {
			rw.CGDNPower = float64(req.CGDNPower)
		}
	}
	// ACT-R params (defaults applied; only used when UseACTR=true).
	rw.ACTRDecay = 0.5
	if req.ACTRDecay > 0 {
		rw.ACTRDecay = float64(req.ACTRDecay)
	}
	rw.ACTRHebScale = 4.0
	if req.ACTRHebScale > 0 {
		rw.ACTRHebScale = float64(req.ACTRHebScale)
	}
	return rw
}

func buildWhy(eng *storage.Engram, c ScoreComponents, hopPath []storage.ULID, hopConcepts []string, queryStr string, useACTR bool) string {
	var parts []string

	signals := map[string]float64{
		"semantic": c.SemanticSimilarity,
		"fts":      c.FullTextRelevance,
		"decay":    c.DecayFactor,
		"hebbian":  c.HebbianBoost,
	}
	best := ""
	bestVal := 0.0
	for k, v := range signals {
		if v > bestVal {
			bestVal = v
			best = k
		}
	}

	switch best {
	case "semantic":
		parts = append(parts, fmt.Sprintf("high semantic similarity (%.0f%%) to context", c.SemanticSimilarity*100))
	case "fts":
		q := queryStr
		if len(q) > 40 {
			q = q[:40] + "..."
		}
		parts = append(parts, fmt.Sprintf("strong full-text match (%.0f%%) to \"%s\"", c.FullTextRelevance*100, q))
	case "decay":
		parts = append(parts, "frequently accessed recently, high decay relevance")
	case "hebbian":
		parts = append(parts, "strongly associated with recently activated engrams")
	}

	if len(hopPath) > 1 {
		if len(hopConcepts) > 0 {
			// Build: "reached via: [concept A] → [concept B]"
			hops := make([]string, len(hopConcepts))
			for i, concept := range hopConcepts {
				hops[i] = "[" + concept + "]"
			}
			parts = append(parts, "reached via: "+strings.Join(hops, " → "))
		} else {
			parts = append(parts, fmt.Sprintf("reached via %d association hop(s)", len(hopPath)-1))
		}
	}

	if c.Confidence < 0.5 {
		parts = append(parts, fmt.Sprintf("confidence is low (%.0f%%)", c.Confidence*100))
	}

	if !useACTR && eng.Relevance <= minFloor*1.1 {
		parts = append(parts, "dormant (low decay relevance)")
	}

	return strings.Join(parts, "; ")
}

// queryIDSeq is a process-wide monotonic counter for query IDs.
// Replaces crypto/rand — the result is used for tracing only, not security.
var queryIDSeq atomic.Uint64

func newQueryID() string {
	return fmt.Sprintf("q-%016x", queryIDSeq.Add(1))
}

// Stream sends result frames to the provided send function.
func (e *ActivationEngine) Stream(
	ctx context.Context,
	result *ActivateResult,
	send func(frame *ActivateResponseFrame) error,
) error {
	activations := result.Activations
	totalFrames := (len(activations) + frameSize - 1) / frameSize
	if totalFrames == 0 {
		totalFrames = 1
	}

	for frame := 0; frame < totalFrames; frame++ {
		lo := frame * frameSize
		hi := lo + frameSize
		if hi > len(activations) {
			hi = len(activations)
		}

		f := &ActivateResponseFrame{
			QueryID:     result.QueryID,
			TotalFound:  result.TotalFound,
			LatencyMs:   result.LatencyMs,
			Activations: activations[lo:hi],
			Frame:       frame + 1,
			TotalFrames: totalFrames,
		}

		if err := send(f); err != nil {
			return fmt.Errorf("stream frame %d: %w", frame, err)
		}
	}
	return nil
}
