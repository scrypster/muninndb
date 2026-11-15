package hnsw

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cockroachdb/pebble"
)

// DimMismatchError reports a vector whose dimension differs from the dimension
// already established by a vault's existing vectors. Mixing dimensions inside
// one vault silently splits it into mutually invisible embedding spaces
// (issue #582): CosineSimilarity scores length-mismatched pairs 0, so a
// mismatched vector would be stored but never surface in semantic search.
type DimMismatchError struct {
	Got  int // dimension of the offending vector
	Want int // dimension established by the vault's existing vectors
}

func (e *DimMismatchError) Error() string {
	return fmt.Sprintf("embedding dimension %d does not match vault dimension %d — run `muninn vault reembed <vault>` after changing embedding models", e.Got, e.Want)
}

// CheckDim is the single definition of the vault-dimension invariant: a vault
// whose dimension is established (want > 0) only accepts vectors of exactly
// that dimension; an empty vault (want == 0) accepts any — the first insert
// establishes it.
func CheckDim(want, got int) error {
	if want > 0 && got != want {
		return &DimMismatchError{Got: got, Want: want}
	}
	return nil
}

// Registry is a multi-vault HNSW index registry.
// It lazily creates and caches one *Index per vault workspace prefix.
// It implements both activation.HNSWIndex and trigger.HNSWIndex (both have the
// same Search signature: Search(ctx, ws [8]byte, vec []float32, topK int) ([]ScoredID, error)).
type Registry struct {
	mu             sync.RWMutex
	indexes        map[[8]byte]*Index
	db             *pebble.DB
	efConstruction int // 0 → use package default (200)
	efSearch       int // 0 → use package default (50)

	// Memory thresholds (bytes). Zero means no limit / no warning.
	warnThresholdBytes int64 // emit slog.Warn when total vector bytes exceed this
	maxBytes           int64 // skip HNSW insert when total vector bytes exceed this

	// throttle state for periodic memory warnings (one warn per 60 s max)
	lastWarnNano atomic.Int64 // Unix nano of last slog.Warn emission
	hardLimitHit atomic.Bool  // true after the first hard-limit hit (changes log level)
	lastHardNano atomic.Int64 // Unix nano of last hard-limit log emission

	// loadErrHook is a test-only seam: when non-nil, it is installed on every
	// lazily-created Index so LoadFromPebble fails deterministically, exercising
	// the no-cache-on-error path in getOrCreate. Always nil in production.
	loadErrHook func() error

	// Background graph repair (repair.go). repairSlot admits ONE vault's
	// repair at a time across the registry, so peak extra memory is one
	// vault's scratch graph however many vaults need repair at start-up.
	// closed (guarded by mu) stops new repairs once Close has begun.
	repairSlot chan struct{}
	repairOnce sync.Once
	closed     bool

	// repairHook is a test-only seam called at a repair's named stages, with
	// the vault being repaired. Always nil in production.
	repairHook func(ws [8]byte, stage string)
}

// warnThrottleInterval is the minimum gap between repeated memory-warning log lines.
const warnThrottleInterval = 60 * time.Second

// NewRegistry creates a new Registry backed by the provided Pebble database.
func NewRegistry(db *pebble.DB) *Registry {
	return &Registry{
		indexes: make(map[[8]byte]*Index),
		db:      db,
	}
}

// NewRegistryWithEfConstruction creates a Registry where each lazily-created
// Index will use the given efConstruction instead of the package default (200).
// Use a lower value (e.g., 50) for bulk eval loading to trade build quality for
// speed. For query-time recall tuning, prefer NewRegistryWithParams and set efSearch.
func NewRegistryWithEfConstruction(db *pebble.DB, efConstruction int) *Registry {
	return &Registry{
		indexes:        make(map[[8]byte]*Index),
		db:             db,
		efConstruction: efConstruction,
	}
}

// NewRegistryWithParams creates a Registry with explicit efConstruction and efSearch.
// Use for eval configurations where both build and query beam widths need tuning
// (e.g., efC=200, efSearch=200 for large-corpus eval to maximize recall).
func NewRegistryWithParams(db *pebble.DB, efConstruction, efSearch int) *Registry {
	return &Registry{
		indexes:        make(map[[8]byte]*Index),
		db:             db,
		efConstruction: efConstruction,
		efSearch:       efSearch,
	}
}

// SetWarnThresholdBytes configures the soft warning threshold.
// When total in-memory vector bytes exceed this value, a throttled slog.Warn
// is emitted (at most once per 60 s). Zero disables the threshold.
func (r *Registry) SetWarnThresholdBytes(n int64) {
	r.warnThresholdBytes = n
}

// SetMaxBytes configures the hard OOM backstop.
// When total in-memory vector bytes already meet or exceed this limit, the
// Insert call skips adding the vector to the in-memory graph (the vector is
// still stored in Pebble, so the write succeeds and FTS remains intact).
// Zero disables the hard limit.
func (r *Registry) SetMaxBytes(n int64) {
	r.maxBytes = n
}

// getOrCreate returns the per-vault Index, creating it lazily if it doesn't exist.
// On creation it calls LoadFromPebble to restore any previously persisted nodes.
func (r *Registry) getOrCreate(ws [8]byte) *Index {
	// Fast path: read lock
	r.mu.RLock()
	idx, ok := r.indexes[ws]
	r.mu.RUnlock()
	if ok {
		return idx
	}

	// Slow path: create under write lock
	r.mu.Lock()
	defer r.mu.Unlock()

	// Double-check after acquiring write lock
	if idx, ok = r.indexes[ws]; ok {
		return idx
	}

	if r.efConstruction > 0 || r.efSearch > 0 {
		idx = NewWithParams(r.db, ws, r.efConstruction, r.efSearch)
	} else {
		idx = New(r.db, ws)
	}
	idx.loadErrHook = r.loadErrHook // nil in production
	// Load any previously persisted nodes. On failure, do NOT cache the index:
	// caching a known-empty index would pin it for the rest of the process
	// lifetime (the fast path always returns the cached value), turning a
	// transient load error into permanent silent recall loss. Returning the
	// uncached empty index lets this request degrade gracefully while the next
	// access retries the load. (issue #499)
	plan, err := idx.load()
	if err != nil {
		slog.Error("hnsw: failed to load graph from pebble; not caching index (load will be retried on next access)", "vault", ws, "error", err)
		// Record the failure so Insert refuses writes instead of treating the
		// vault as empty (Dim()==0 would let a mismatched vector through and
		// recreate the #582 split). Reads keep their degraded-empty behavior.
		idx.loadErr = err
		return idx
	}
	r.indexes[ws] = idx
	// A graph that needs repair is served AS LOADED while the repair runs in
	// the background: the repair must never run here, under the
	// registry-wide write lock, where it would stall the first access to
	// every other vault for its whole duration.
	if plan.kind != repairNone {
		r.startRepair(ws, idx, plan)
	}
	return idx
}

// startRepair launches idx's repair in the background. Caller holds r.mu.
func (r *Registry) startRepair(ws [8]byte, idx *Index, plan repairPlan) {
	if r.closed {
		slog.Warn("hnsw: graph needs repair but the registry is closing; serving the loaded graph", "vault", ws, "kind", plan.kind.String())
		return
	}
	r.repairOnce.Do(func() { r.repairSlot = make(chan struct{}, 1) })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	idx.repair.mu.Lock()
	idx.repair.cancel, idx.repair.done = cancel, done
	idx.repair.mu.Unlock()
	idx.setRepairStatus(func(s *RebuildStatus) {
		*s = RebuildStatus{State: "queued", Kind: plan.kind.String(), Vectors: plan.vectors,
			ClosedNodes: plan.closed, UnreachableNodes: plan.unreachable}
	})
	var hook func(string)
	if h := r.repairHook; h != nil {
		hook = func(stage string) { h(ws, stage) }
	}
	slot := r.repairSlot
	go func() {
		defer close(done)
		defer cancel()
		select {
		case slot <- struct{}{}:
		case <-ctx.Done():
			idx.setRepairStatus(func(s *RebuildStatus) { s.State = "cancelled" })
			return
		}
		defer func() { <-slot }()
		idx.runRepair(ctx, plan, hook)
	}()
}

// WaitRepairs blocks until every loaded vault's background graph repair has
// finished (or been cancelled). Test and measurement seam; production never
// needs to wait — searches are served throughout.
func (r *Registry) WaitRepairs() {
	for _, idx := range r.snapshotIndexes() {
		idx.waitRepair()
	}
}

// Close cancels any background graph repair (a cancelled repair writes
// nothing; the next start assesses the graph again), waits for it to stop,
// and waits for in-flight neighbour-list writes. Call before closing the
// Pebble database. Searches and inserts keep working afterwards; no new
// repair starts.
func (r *Registry) Close() {
	r.mu.Lock()
	r.closed = true
	r.mu.Unlock()
	for _, idx := range r.snapshotIndexes() {
		idx.stopRepair()
		idx.Close()
	}
}

// RebuildStatus returns the vault's graph-repair status when its index is
// loaded and a repair has been needed since; nil otherwise. Never loads.
func (r *Registry) RebuildStatus(ws [8]byte) *RebuildStatus {
	if idx := r.get(ws); idx != nil {
		return idx.RebuildStatus()
	}
	return nil
}

func (r *Registry) snapshotIndexes() []*Index {
	r.mu.RLock()
	defer r.mu.RUnlock()
	idxs := make([]*Index, 0, len(r.indexes))
	for _, idx := range r.indexes {
		idxs = append(idxs, idx)
	}
	return idxs
}

// Search implements activation.HNSWIndex and trigger.HNSWIndex.
// It delegates to the per-vault Index.
// A query vector whose dimension differs from the vault's established
// dimension returns a *DimMismatchError instead of silently scoring 0 against
// every node (issue #582); callers already degrade to FTS on search errors.
func (r *Registry) Search(ctx context.Context, ws [8]byte, vec []float32, topK int) ([]ScoredID, error) {
	idx := r.getOrCreate(ws)
	if err := CheckDim(idx.Dim(), len(vec)); err != nil {
		return nil, err
	}
	return idx.Search(ctx, vec, topK)
}

// SearchLive is Search with retired nodes routed through but never counted as
// results; see Index.SearchLive. Same dimension guard as Search.
func (r *Registry) SearchLive(ctx context.Context, ws [8]byte, vec []float32, topK int, retired func([16]byte) bool, shadowBudget int) (live, shadows []ScoredID, stats LiveSearchStats, err error) {
	idx := r.getOrCreate(ws)
	if err := CheckDim(idx.Dim(), len(vec)); err != nil {
		return nil, nil, stats, err
	}
	return idx.SearchLive(ctx, vec, topK, retired, shadowBudget)
}

// TotalVectors returns the total number of indexed vectors across all vaults.
func (r *Registry) TotalVectors() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	total := 0
	for _, idx := range r.indexes {
		total += idx.Len()
	}
	return total
}

// VaultVectors returns the number of indexed vectors for a single vault.
// Uses getOrCreate to ensure the index is loaded from Pebble if it hasn't
// been accessed since startup.
func (r *Registry) VaultVectors(ws [8]byte) int {
	return r.getOrCreate(ws).Len()
}

// VaultEmbedDim returns the vector dimension for the given vault's HNSW index.
// Returns 0 if the vault has no indexed vectors yet (dimension not yet established).
func (r *Registry) VaultEmbedDim(ws [8]byte) int {
	return r.getOrCreate(ws).Dim()
}

// CachedVaultDim returns the vault's vector dimension when its index is
// already in memory, and 0 otherwise — unlike VaultEmbedDim it never triggers
// a graph load, so callers that only need a cheap dimension peek (e.g. the
// write-side dimension guard) don't pull a whole vault into memory just to
// refuse a vector.
func (r *Registry) CachedVaultDim(ws [8]byte) int {
	if idx := r.get(ws); idx != nil {
		return idx.Dim()
	}
	return 0
}

// TotalVectorBytes returns the total in-memory vector size across all vaults.
func (r *Registry) TotalVectorBytes() int64 {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var total int64
	for _, idx := range r.indexes {
		total += idx.VectorBytes()
	}
	return total
}

// VaultVectorBytes returns the in-memory vector size for a single vault.
func (r *Registry) VaultVectorBytes(ws [8]byte) int64 {
	return r.getOrCreate(ws).VectorBytes()
}

// ResetVault drops the in-memory HNSW index for the given vault workspace prefix.
// Called by ClearVault and DeleteVault to evict stale index state after the
// underlying storage has been wiped. The next Insert or Search call will
// recreate the index lazily (empty, since Pebble data is gone).
//
// A background repair of the dropped index is cancelled and waited for, so it
// cannot write the vault's old lists back after the reset.
func (r *Registry) ResetVault(ws [8]byte) {
	r.mu.Lock()
	idx := r.indexes[ws]
	delete(r.indexes, ws)
	r.mu.Unlock()
	if idx != nil {
		idx.stopRepair()
	}
}

// WaitPersisted blocks until every loaded index has finished persisting its
// in-flight neighbour-list writes (Index.Close for each), background graph
// repairs included (each finishes; none is cancelled). A test that closes
// the store and re-opens it — a restart — must call it first, or the graph
// it reloads can be missing the last asynchronous writes.
func (r *Registry) WaitPersisted() {
	for _, idx := range r.snapshotIndexes() {
		idx.waitRepair()
		idx.Close()
	}
}

// get returns the per-vault Index if it exists, or nil if it has not been created yet.
// Unlike getOrCreate, this does not lazily create a new index.
func (r *Registry) get(ws [8]byte) *Index {
	r.mu.RLock()
	idx := r.indexes[ws]
	r.mu.RUnlock()
	return idx
}

// TombstoneNode marks a node as deleted in the per-vault Index so it is skipped
// in future Search results. If the vault index has not been loaded yet this is a
// no-op — the node will never appear in results anyway.
func (r *Registry) TombstoneNode(ws [8]byte, id [16]byte) {
	if idx := r.get(ws); idx != nil {
		idx.Tombstone(id)
	}
}

// IsTombstoned reports whether the node is tombstoned in the vault's index.
// Returns false when the vault index has not been loaded — the node cannot
// appear in results from an index that does not exist, so "not tombstoned" and
// "not present" are the same answer to a caller asking about search visibility.
func (r *Registry) IsTombstoned(ws [8]byte, id [16]byte) bool {
	if idx := r.get(ws); idx != nil {
		return idx.IsTombstoned(id)
	}
	return false
}

// maybeLogMemoryPressure emits throttled warning logs when memory thresholds are
// exceeded. It returns true if the hard limit is active and the insert should be
// skipped.
//
// Design: two independent throttle clocks — one for soft-warn, one for hard-limit.
// Both use atomic CAS on Unix-nanosecond timestamps so no lock is needed.
func (r *Registry) maybeLogMemoryPressure(totalBytes int64) (skipInsert bool) {
	const mb = 1 << 20

	// Hard limit check.
	if r.maxBytes > 0 && totalBytes >= r.maxBytes {
		now := time.Now().UnixNano()
		last := r.lastHardNano.Load()
		elapsed := time.Duration(now - last)
		if elapsed >= warnThrottleInterval && r.lastHardNano.CompareAndSwap(last, now) {
			if !r.hardLimitHit.Swap(true) {
				// First time hitting the hard limit: emit slog.Error.
				slog.Error("hnsw: hard memory limit reached — skipping graph insert (embedding stored in Pebble; semantic search degraded to FTS)",
					"total_mb", totalBytes/int64(mb),
					"limit_mb", r.maxBytes/int64(mb),
				)
			} else {
				// Subsequent hits: throttled slog.Warn.
				slog.Warn("hnsw: hard memory limit still exceeded — continuing to skip graph inserts",
					"total_mb", totalBytes/int64(mb),
					"limit_mb", r.maxBytes/int64(mb),
				)
			}
		}
		return true
	}

	// Soft warn threshold.
	if r.warnThresholdBytes > 0 && totalBytes >= r.warnThresholdBytes {
		now := time.Now().UnixNano()
		last := r.lastWarnNano.Load()
		elapsed := time.Duration(now - last)
		if elapsed >= warnThrottleInterval && r.lastWarnNano.CompareAndSwap(last, now) {
			slog.Warn("hnsw: in-memory vector size exceeds warning threshold",
				"total_mb", totalBytes/int64(mb),
				"warn_threshold_mb", r.warnThresholdBytes/int64(mb),
			)
		}
	}

	return false
}

// Insert adds a vector to the appropriate per-vault Index.
// If the hard memory limit (SetMaxBytes) is set and already met, the vector is
// stored in Pebble but skipped in the in-memory graph: the write succeeds and
// FTS remains intact; only semantic (HNSW) search degrades gracefully.
func (r *Registry) Insert(ctx context.Context, ws [8]byte, id [16]byte, vec []float32) error {
	idx := r.getOrCreate(ws)

	// A vault whose graph failed to load has an UNKNOWN dimension — refuse
	// the write rather than fail open (Dim()==0 would accept anything and
	// could recreate the #582 split). The load is retried on next access, so
	// a transient read error only defers the insert.
	if err := idx.LoadError(); err != nil {
		return fmt.Errorf("hnsw: refusing insert, vault graph failed to load (retried on next access): %w", err)
	}

	// Refuse a vector whose dimension differs from the vault's established
	// dimension (issue #582) — check and first-insert establishment are
	// atomic, and both happen before StoreVector so a mismatched vector is
	// never persisted.
	if err := idx.establishDim(len(vec)); err != nil {
		return err
	}

	// Store vector first so the graph can fetch it during traversal.
	if err := idx.StoreVector(id, vec); err != nil {
		return err
	}

	// Check memory thresholds before touching the in-memory graph.
	// TotalVectorBytes acquires RLock internally; safe here.
	if r.maxBytes > 0 || r.warnThresholdBytes > 0 {
		totalBytes := r.TotalVectorBytes()
		if r.maybeLogMemoryPressure(totalBytes) {
			// Hard limit hit: skip graph insert. Vector is already in Pebble.
			return nil
		}
	}

	// If the in-memory graph insertion panics or a future error path is added,
	// clean up the orphaned vector so it is never stranded in storage unreachable
	// by graph traversal.
	insertOK := false
	defer func() {
		if !insertOK {
			_ = idx.DeleteVector(id) // cleanup orphan on Insert failure
		}
	}()
	idx.Insert(id, vec)
	insertOK = true
	return nil
}
