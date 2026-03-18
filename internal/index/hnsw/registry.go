package hnsw

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cockroachdb/pebble"
)

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
	lastWarnNano  atomic.Int64 // Unix nano of last slog.Warn emission
	hardLimitHit  atomic.Bool  // true after the first hard-limit hit (changes log level)
	lastHardNano  atomic.Int64 // Unix nano of last hard-limit log emission
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
	// Load any previously persisted nodes; log errors — empty index is still usable.
	if err := idx.LoadFromPebble(); err != nil {
		slog.Error("hnsw: failed to load graph from pebble", "vault", ws, "error", err)
	}
	r.indexes[ws] = idx
	return idx
}

// Search implements activation.HNSWIndex and trigger.HNSWIndex.
// It delegates to the per-vault Index.
func (r *Registry) Search(ctx context.Context, ws [8]byte, vec []float32, topK int) ([]ScoredID, error) {
	idx := r.getOrCreate(ws)
	return idx.Search(ctx, vec, topK)
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
func (r *Registry) ResetVault(ws [8]byte) {
	r.mu.Lock()
	delete(r.indexes, ws)
	r.mu.Unlock()
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
