package storage

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cockroachdb/pebble"
	lru "github.com/hashicorp/golang-lru/v2"
	"github.com/hashicorp/golang-lru/v2/expirable"
	"github.com/scrypster/muninndb/internal/provenance"
	"github.com/scrypster/muninndb/internal/scoring"
	"github.com/scrypster/muninndb/internal/storage/erf"
	"github.com/scrypster/muninndb/internal/storage/keys"
	"github.com/scrypster/muninndb/internal/wal"
)

// ensure atomic is used (atomic.Int64 is referenced via vaultCounter)
var _ = atomic.Int64{}

// PebbleStoreConfig holds creation-time options for PebbleStore.
type PebbleStoreConfig struct {
	// CacheSize is the maximum number of entries in the in-memory L1 cache. 0 means no caching.
	CacheSize int
	// NoSyncEngrams switches WriteEngram from pebble.Sync to pebble.NoSync.
	// When true, the existing walSyncer provides durability within 10ms.
	// Default false preserves the previous per-write fsync behavior.
	NoSyncEngrams bool
}

// PebbleStore is the concrete Pebble-backed implementation of EngineStore.
type PebbleStore struct {
	db            *pebble.DB
	cache         *L1Cache
	mol           *wal.MOL
	gc            *wal.GroupCommitter
	noSyncEngrams bool
	vaultCounters sync.Map          // [8]byte -> *vaultCounter
	provenance    *provenance.Store // Provenance chain for tracking engram creation/updates
	scoringStore  *scoring.Store   // Per-vault learnable scoring weights
	walSync       *walSyncer        // Periodic WAL fsync — covers all pebble.NoSync writes
	counterFlush  *counterCoalescer // Coalesces vault count Pebble writes (100ms timer)
	provWork      *provenanceWorker // NumCPU goroutines for provenance appends
	// assocCache: [24]byte (wsPrefix[8]+engramID[16]) → *assocCacheEntry
	// Caches forward association lists to avoid repeated Pebble SSTable scans on hot engrams.
	// Invalidated on any WriteAssociation or UpdateAssociation for that engram.
	// Bounded to 500_000 entries with 2s TTL; expirable.LRU handles expiry automatically.
	assocCache *expirable.LRU[[24]byte, *assocCacheEntry]
	// metaCache: [16]byte (engramID) → *EngramMeta
	// Caches metadata for hot read-path engrams so GetMetadata never goes to Pebble twice.
	// Populated by GetMetadata on first Pebble read. Invalidated by UpdateMetadata/WriteEngram.
	// Bounded to 100_000 entries.
	metaCache *lru.Cache[[16]byte, *EngramMeta]
	// vaultPrefixCache: vault name (string) → [8]byte workspace prefix
	// Eliminates the Pebble.Get in ResolveVaultPrefix on every write/activation.
	// Bounded to 10_000 entries.
	vaultPrefixCache *lru.Cache[string, [8]byte]
	// vaultNameWritten: [8]byte → struct{} — tracks vaults whose name has been persisted.
	// Eliminates the Pebble.Get existence check in WriteVaultName on every write.
	vaultNameWritten sync.Map
	// recentActiveCache: [8]byte (wsPrefix) → *recentActiveCacheEntry
	// Caches RecentActive results per vault with a 100ms TTL to avoid repeated SSTable scans.
	recentActiveCache sync.Map
	// transCache is the tiered PAS transition cache (in-memory hot + Pebble warm).
	// All transition reads/writes go through this layer; Pebble is only hit on
	// cold-start loads and periodic flushes.
	transCache *TransitionCache
	closeOnce   sync.Once
	// entityLocks and coOccurrenceLocks use fixed-size striped mutex arrays instead of
	// sync.Map to bound memory growth. sync.Map grows unbounded (one entry per unique key
	// ever seen); stripedMutex uses a constant 256 × sizeof(sync.Mutex) ≈ 6 KB.
	entityLocks       stripedMutex // prevents TOCTOU in UpsertEntityRecord
	coOccurrenceLocks stripedMutex // prevents TOCTOU in IncrementEntityCoOccurrence
	// archiveBloom is an in-memory Bloom filter over src engram IDs that have
	// archived associations in the 0x25 namespace. Gates the 0x25 prefix scan
	// during BFS traversal: if the filter says "no," skip the scan entirely.
	// Rebuilt on startup and after GC runs via RebuildArchiveBloom.
	archiveBloom *archiveBloom
}

// assocCacheEntry holds a cached association list.
// TTL is enforced by the expirable.LRU cache (2s); no per-entry expiry field needed.
type assocCacheEntry struct {
	assocs []Association
}

// assocCacheTTL is how long association lists are cached.
// Stale weights are acceptable for BFS traversal in the activation path.
// Longer TTL = fewer Pebble reads; associations change slowly via Hebbian updates.
const assocCacheTTL = 2 * time.Second

// recentActiveCacheEntry is a TTL-cached result for RecentActive.
type recentActiveCacheEntry struct {
	ids     []ULID
	expires int64 // unix nanoseconds
}

// vaultCounter tracks the engram count for a vault.
// sync.Once ensures the counter is seeded from Pebble exactly once per vault per process lifetime.
type vaultCounter struct {
	once  sync.Once
	count atomic.Int64
}

// getOrInitCounter returns the vault counter, initializing it from Pebble on first access.
func (ps *PebbleStore) getOrInitCounter(ctx context.Context, wsPrefix [8]byte) *vaultCounter {
	if v, ok := ps.vaultCounters.Load(wsPrefix); ok {
		return v.(*vaultCounter)
	}
	vc := &vaultCounter{}
	actual, _ := ps.vaultCounters.LoadOrStore(wsPrefix, vc)
	loaded := actual.(*vaultCounter)
	loaded.once.Do(func() {
		// Try to read persisted count from Pebble
		countKey := keys.VaultCountKey(wsPrefix)
		val, err := Get(ps.db, countKey)
		if err == nil && len(val) == 8 {
			n := int64(binary.BigEndian.Uint64(val))
			loaded.count.Store(n)
			return
		}
		// Fall back to per-vault scan (one-time cost on first startup)
		n, _ := ps.countEngramsForVault(ctx, wsPrefix)
		loaded.count.Store(n)
		// Persist so next startup avoids the scan
		buf := make([]byte, 8)
		binary.BigEndian.PutUint64(buf, uint64(n))
		_ = ps.db.Set(countKey, buf, pebble.NoSync)
	})
	return loaded
}

// countEngramsForVault scans the 0x01 prefix for a single vault.
func (ps *PebbleStore) countEngramsForVault(ctx context.Context, wsPrefix [8]byte) (int64, error) {
	lower := keys.EngramKey(wsPrefix, [16]byte{})
	upperWS := wsPrefix
	for i := 7; i >= 0; i-- {
		upperWS[i]++
		if upperWS[i] != 0 {
			break
		}
	}
	upper := make([]byte, 1+8)
	upper[0] = 0x01
	copy(upper[1:9], upperWS[:])
	iter, err := ps.db.NewIter(&pebble.IterOptions{LowerBound: lower, UpperBound: upper})
	if err != nil {
		return 0, err
	}
	defer iter.Close()
	var count int64
	for valid := iter.First(); valid; valid = iter.Next() {
		if len(iter.Key()) >= 25 {
			count++
		}
	}
	if err := iter.Error(); err != nil {
		return 0, fmt.Errorf("count engrams scan: %w", err)
	}
	return count, nil
}

// GetVaultCount returns the current engram count for a vault.
func (ps *PebbleStore) GetVaultCount(ctx context.Context, wsPrefix [8]byte) int64 {
	return ps.getOrInitCounter(ctx, wsPrefix).count.Load()
}

// NewPebbleStore creates a new PebbleStore wrapping a Pebble database and L1 cache.
func NewPebbleStore(db *pebble.DB, cfg PebbleStoreConfig) *PebbleStore {
	prov := provenance.NewStore(db)
	metaCache, _ := lru.New[[16]byte, *EngramMeta](100_000)
	vaultPrefixCache, _ := lru.New[string, [8]byte](10_000)
	assocCache := expirable.NewLRU[[24]byte, *assocCacheEntry](500_000, nil, 2*time.Second)
	ps := &PebbleStore{
		db:               db,
		cache:            NewL1Cache(cfg.CacheSize),
		provenance:       prov,
		scoringStore:     scoring.NewStore(db),
		noSyncEngrams:    cfg.NoSyncEngrams,
		metaCache:        metaCache,
		vaultPrefixCache: vaultPrefixCache,
		assocCache:       assocCache,
	}
	ps.walSync = newWALSyncer(db)
	ps.counterFlush = newCounterCoalescer(db)
	ps.provWork = newProvenanceWorker(prov)
	ps.transCache = NewTransitionCache(ps)
	ps.archiveBloom = ps.RebuildArchiveBloom()
	return ps
}

// CacheLen returns the number of entries in the L1 cache.
func (ps *PebbleStore) CacheLen() int {
	return ps.cache.Len()
}

// SetWAL sets the MOL and GroupCommitter for the PebbleStore.
// After this is called, WriteEngram will append entries to the WAL asynchronously.
func (ps *PebbleStore) SetWAL(mol *wal.MOL, gc *wal.GroupCommitter) {
	ps.mol = mol
	ps.gc = gc
}

// VaultPrefix computes the 8-byte SipHash prefix for a vault name.
func (ps *PebbleStore) VaultPrefix(vault string) [8]byte {
	return keys.VaultPrefix(vault)
}

// WriteEngram atomically writes the full engram record and metadata-only copy in a single Pebble batch.
// Also writes association forward/reverse keys and secondary index entries.
func (ps *PebbleStore) WriteEngram(ctx context.Context, wsPrefix [8]byte, eng *Engram) (ULID, error) {
	if eng.ID == (ULID{}) {
		if !eng.CreatedAt.IsZero() {
			eng.ID = NewULIDWithTime(eng.CreatedAt)
		} else {
			eng.ID = NewULID()
		}
	}
	if eng.State == 0 {
		eng.State = StateActive
	}
	if eng.Confidence == 0 {
		eng.Confidence = 1.0
	}
	if eng.Stability == 0 {
		eng.Stability = 30.0
	}
	if eng.CreatedAt.IsZero() {
		eng.CreatedAt = time.Now()
	}
	if eng.UpdatedAt.IsZero() {
		eng.UpdatedAt = eng.CreatedAt
	}
	if eng.LastAccess.IsZero() {
		eng.LastAccess = eng.CreatedAt
	}

	erfEng := toERFEngram(eng)
	erfBytes, err := erf.EncodeV2(erfEng)
	if err != nil {
		return ULID{}, fmt.Errorf("encode engram: %w", err)
	}

	batch := ps.db.NewBatch()
	defer batch.Close()

	// 0x01: full engram record
	batch.Set(keys.EngramKey(wsPrefix, [16]byte(eng.ID)), erfBytes, nil)

	// 0x02: metadata-only slim form (MetaKeySize bytes of the ERF header)
	batch.Set(keys.MetaKey(wsPrefix, [16]byte(eng.ID)), erf.MetaKeySlice(erfBytes), nil)

	// 0x18: standalone embedding key (ERF v2 — embedding not inline)
	if len(eng.Embedding) > 0 {
		params, quantized := erf.Quantize(eng.Embedding)
		paramsBuf := erf.EncodeQuantizeParams(params)
		embedBytes := make([]byte, 8+len(quantized))
		copy(embedBytes[:8], paramsBuf[:])
		for i, v := range quantized {
			embedBytes[8+i] = byte(v)
		}
		batch.Set(keys.EmbeddingKey(wsPrefix, [16]byte(eng.ID)), embedBytes, nil)
	}

	// 0x03/0x04/weight-index: association keys
	for _, assoc := range eng.Associations {
		// Seed PeakWeight from Weight if not set (legacy or newly created associations).
		peak := assoc.PeakWeight
		if peak == 0 {
			peak = assoc.Weight
		}
		av := encodeAssocValue(assoc.RelType, assoc.Confidence, assoc.CreatedAt, assoc.LastActivated, peak, assoc.CoActivationCount)
		batch.Set(keys.AssocFwdKey(wsPrefix, [16]byte(eng.ID), assoc.Weight, [16]byte(assoc.TargetID)), av[:], nil)
		batch.Set(keys.AssocRevKey(wsPrefix, [16]byte(assoc.TargetID), assoc.Weight, [16]byte(eng.ID)), av[:], nil)
		var wiBuf [4]byte
		binary.BigEndian.PutUint32(wiBuf[:], math.Float32bits(assoc.Weight))
		batch.Set(keys.AssocWeightIndexKey(wsPrefix, [16]byte(eng.ID), [16]byte(assoc.TargetID)), wiBuf[:], nil)
	}

	// 0x0B: state index
	batch.Set(keys.StateIndexKey(wsPrefix, uint8(eng.State), [16]byte(eng.ID)), []byte{}, nil)

	// 0x0C: tag indexes
	for _, tag := range eng.Tags {
		batch.Set(keys.TagIndexKey(wsPrefix, keys.Hash(tag), [16]byte(eng.ID)), []byte{}, nil)
	}

	// 0x0D: creator index
	batch.Set(keys.CreatorIndexKey(wsPrefix, keys.Hash(eng.CreatedBy), [16]byte(eng.ID)), []byte{}, nil)

	// 0x10: relevance bucket key
	batch.Set(keys.RelevanceBucketKey(wsPrefix, eng.Relevance, [16]byte(eng.ID)), []byte{}, nil)

	// 0x22: LastAccess index — seed with LastAccess (= CreatedAt for new engrams).
	laMillis := eng.LastAccess.UnixMilli()
	laKey := keys.LastAccessIndexKey(wsPrefix, laMillis, [16]byte(eng.ID))
	batch.Set(laKey, nil, nil)

	// Commit — default: one fsync per user-submitted engram (pebble.Sync).
	// User content is the irreplaceable asset; immediate durability is the
	// correct tradeoff for a write-light memory store.
	// When noSyncEngrams=true, walSyncer provides WAL durability within 10ms,
	// batching fsyncs across all concurrent NoSync writes at lower I/O cost.
	syncOption := pebble.Sync
	if ps.noSyncEngrams {
		syncOption = pebble.NoSync
	}
	if err := batch.Commit(syncOption); err != nil {
		return ULID{}, fmt.Errorf("commit batch: %w", err)
	}

	// NOTE: Intentionally NOT caching on write to avoid flooding L1 cache.

	// Vault counter: in-memory atomic; coalescer persists every 100ms.
	// Load-or-init before the Add so we hold a reference to the live counter.
	vc := ps.getOrInitCounter(ctx, wsPrefix)
	newCount := vc.count.Add(1)
	// Only submit if vc is still the current counter for this vault.
	// ClearVault may have evicted the counter between Add and Submit; in that
	// case submitting would re-seed the coalescer with a stale count.
	if ps.counterFlush != nil {
		if current, ok := ps.vaultCounters.Load(wsPrefix); ok && current.(*vaultCounter) == vc {
			ps.counterFlush.Submit(wsPrefix, newCount)
		}
	}

	// WAL MOL entry (async, non-blocking).
	if ps.gc != nil {
		idBytes := [16]byte(eng.ID)
		vaultID := binary.BigEndian.Uint32(wsPrefix[:4])
		wal.AppendAsync(ps.gc, &wal.MOLEntry{
			OpType:  wal.OpEngramWrite,
			VaultID: vaultID,
			Payload: idBytes[:],
		})
	}

	// Provenance (async, non-blocking channel send).
	if ps.provWork != nil {
		ps.provWork.Submit(wsPrefix, eng.ID, provenance.ProvenanceEntry{
			Timestamp: eng.CreatedAt,
			Source:    provenance.SourceHuman,
			AgentID:   eng.CreatedBy,
			Operation: "create",
		})
	}

	return eng.ID, nil
}

// EngramBatchItem pairs a vault workspace prefix with the engram to write.
type EngramBatchItem struct {
	WSPrefix [8]byte
	Engram   *Engram
}

// WriteEngramBatch atomically writes multiple engrams in a single Pebble batch
// commit. This amortises the fsync cost across N engrams instead of paying it
// per-engram, which is the dominant I/O bottleneck on write-heavy workloads.
//
// Each engram gets its own ULID, defaults, ERF encoding, and index keys — the
// only difference from N × WriteEngram is the single commit at the end.
// Post-commit work (vault counters, WAL/MOL, provenance) runs per-item after
// the batch commits.
//
// Returns a slice of (ULID, error) per item. If the batch commit itself fails,
// all items receive the commit error.
func (ps *PebbleStore) WriteEngramBatch(ctx context.Context, items []EngramBatchItem) ([]ULID, []error) {
	n := len(items)
	ids := make([]ULID, n)
	errs := make([]error, n)

	if n == 0 {
		return ids, errs
	}

	batch := ps.db.NewBatch()
	defer batch.Close()

	for i := range items {
		eng := items[i].Engram
		ws := items[i].WSPrefix

		if eng.ID == (ULID{}) {
			if !eng.CreatedAt.IsZero() {
				eng.ID = NewULIDWithTime(eng.CreatedAt)
			} else {
				eng.ID = NewULID()
			}
		}
		if eng.State == 0 {
			eng.State = StateActive
		}
		if eng.Confidence == 0 {
			eng.Confidence = 1.0
		}
		if eng.Stability == 0 {
			eng.Stability = 30.0
		}
		if eng.CreatedAt.IsZero() {
			eng.CreatedAt = time.Now()
		}
		if eng.UpdatedAt.IsZero() {
			eng.UpdatedAt = eng.CreatedAt
		}
		if eng.LastAccess.IsZero() {
			eng.LastAccess = eng.CreatedAt
		}

		erfEng := toERFEngram(eng)
		erfBytes, encErr := erf.EncodeV2(erfEng)
		if encErr != nil {
			errs[i] = fmt.Errorf("encode engram: %w", encErr)
			continue
		}

		id16 := [16]byte(eng.ID)
		batch.Set(keys.EngramKey(ws, id16), erfBytes, nil)
		batch.Set(keys.MetaKey(ws, id16), erf.MetaKeySlice(erfBytes), nil)

		if len(eng.Embedding) > 0 {
			params, quantized := erf.Quantize(eng.Embedding)
			paramsBuf := erf.EncodeQuantizeParams(params)
			embedBytes := make([]byte, 8+len(quantized))
			copy(embedBytes[:8], paramsBuf[:])
			for j, v := range quantized {
				embedBytes[8+j] = byte(v)
			}
			batch.Set(keys.EmbeddingKey(ws, id16), embedBytes, nil)
		}

		for _, assoc := range eng.Associations {
			// Seed PeakWeight from Weight if not set (legacy or newly created associations).
			peak := assoc.PeakWeight
			if peak == 0 {
				peak = assoc.Weight
			}
			av := encodeAssocValue(assoc.RelType, assoc.Confidence, assoc.CreatedAt, assoc.LastActivated, peak, assoc.CoActivationCount)
			batch.Set(keys.AssocFwdKey(ws, id16, assoc.Weight, [16]byte(assoc.TargetID)), av[:], nil)
			batch.Set(keys.AssocRevKey(ws, [16]byte(assoc.TargetID), assoc.Weight, id16), av[:], nil)
			var wiBuf [4]byte
			binary.BigEndian.PutUint32(wiBuf[:], math.Float32bits(assoc.Weight))
			batch.Set(keys.AssocWeightIndexKey(ws, id16, [16]byte(assoc.TargetID)), wiBuf[:], nil)
		}

		batch.Set(keys.StateIndexKey(ws, uint8(eng.State), id16), []byte{}, nil)
		for _, tag := range eng.Tags {
			batch.Set(keys.TagIndexKey(ws, keys.Hash(tag), id16), []byte{}, nil)
		}
		batch.Set(keys.CreatorIndexKey(ws, keys.Hash(eng.CreatedBy), id16), []byte{}, nil)
		batch.Set(keys.RelevanceBucketKey(ws, eng.Relevance, id16), []byte{}, nil)

		// 0x22: LastAccess index — seed with LastAccess (= CreatedAt for new engrams).
		laMillis := eng.LastAccess.UnixMilli()
		laKey := keys.LastAccessIndexKey(ws, laMillis, id16)
		if err := batch.Set(laKey, nil, nil); err != nil {
			errs[i] = fmt.Errorf("write engram batch: last access index: %w", err)
			continue
		}

		ids[i] = eng.ID
	}

	syncOption := pebble.Sync
	if ps.noSyncEngrams {
		syncOption = pebble.NoSync
	}
	if commitErr := batch.Commit(syncOption); commitErr != nil {
		for i := range errs {
			if errs[i] == nil {
				errs[i] = fmt.Errorf("commit batch: %w", commitErr)
			}
		}
		return ids, errs
	}

	// Post-commit: vault counters, WAL/MOL, provenance — per item.
	for i := range items {
		if errs[i] != nil {
			continue
		}
		eng := items[i].Engram
		ws := items[i].WSPrefix

		vc := ps.getOrInitCounter(ctx, ws)
		newCount := vc.count.Add(1)
		if ps.counterFlush != nil {
			if current, ok := ps.vaultCounters.Load(ws); ok && current.(*vaultCounter) == vc {
				ps.counterFlush.Submit(ws, newCount)
			}
		}

		if ps.gc != nil {
			idBytes := [16]byte(eng.ID)
			vaultID := binary.BigEndian.Uint32(ws[:4])
			wal.AppendAsync(ps.gc, &wal.MOLEntry{
				OpType:  wal.OpEngramWrite,
				VaultID: vaultID,
				Payload: idBytes[:],
			})
		}

		if ps.provWork != nil {
			ps.provWork.Submit(ws, eng.ID, provenance.ProvenanceEntry{
				Timestamp: eng.CreatedAt,
				Source:    provenance.SourceHuman,
				AgentID:   eng.CreatedBy,
				Operation: "create",
			})
		}
	}

	return ids, errs
}

// WriteCoherence persists vault coherence counters to Pebble.
// Value is 56 bytes: 7 × BigEndian int64.
func (ps *PebbleStore) WriteCoherence(vaultPrefix [8]byte, data [7]int64) error {
	buf := make([]byte, 56)
	for i, v := range data {
		binary.BigEndian.PutUint64(buf[i*8:], uint64(v))
	}
	return ps.db.Set(keys.CoherenceKey(vaultPrefix), buf, pebble.NoSync)
}

// ReadCoherence loads vault coherence counters from Pebble.
// Returns (data, true, nil) if found, (zero, false, nil) if not found.
func (ps *PebbleStore) ReadCoherence(vaultPrefix [8]byte) ([7]int64, bool, error) {
	val, closer, err := ps.db.Get(keys.CoherenceKey(vaultPrefix))
	if errors.Is(err, pebble.ErrNotFound) {
		return [7]int64{}, false, nil
	}
	if err != nil {
		return [7]int64{}, false, fmt.Errorf("read coherence: %w", err)
	}
	defer closer.Close()
	if len(val) != 56 {
		return [7]int64{}, false, fmt.Errorf("coherence: unexpected value length %d", len(val))
	}
	var data [7]int64
	for i := range data {
		data[i] = int64(binary.BigEndian.Uint64(val[i*8:]))
	}
	return data, true, nil
}

// Checkpoint creates a Pebble checkpoint (consistent on-disk snapshot) at destDir.
func (ps *PebbleStore) Checkpoint(destDir string) error {
	return ps.db.Checkpoint(destDir)
}

// PebbleMetrics returns the raw Pebble metrics for observability and diagnostics.
func (ps *PebbleStore) PebbleMetrics() *pebble.Metrics {
	return ps.db.Metrics()
}

// ScoringStore returns the scoring.Store that manages per-vault learnable weights.
// The store is constructed once at PebbleStore creation and shared — callers must
// not close it independently.
func (ps *PebbleStore) ScoringStore() *scoring.Store {
	return ps.scoringStore
}

// ProvenanceStore returns the provenance.Store used for audit trail appends.
// The store is constructed once at PebbleStore creation and shared — callers must
// not close it independently.
func (ps *PebbleStore) ProvenanceStore() *provenance.Store {
	return ps.provenance
}

// ClearFTSKeys deletes all FTS index keys for the given vault workspace prefix via
// range tombstones. Prefixes cleared: 0x05 (posting lists), 0x06 (trigrams),
// 0x08 (FTS global stats), 0x09 (per-term stats).
func (ps *PebbleStore) ClearFTSKeys(ws, wsPlus [8]byte) error {
	ftsPrefixes := []byte{0x05, 0x06, 0x08, 0x09}
	batch := ps.db.NewBatch()
	for _, p := range ftsPrefixes {
		lo := make([]byte, 9)
		lo[0] = p
		copy(lo[1:], ws[:])
		hi := make([]byte, 9)
		hi[0] = p
		copy(hi[1:], wsPlus[:])
		if err := batch.DeleteRange(lo, hi, nil); err != nil {
			batch.Close()
			return fmt.Errorf("storage: clear FTS keys 0x%02X: %w", p, err)
		}
	}
	if err := batch.Commit(pebble.Sync); err != nil {
		batch.Close()
		return fmt.Errorf("storage: commit FTS clear batch: %w", err)
	}
	batch.Close()
	return nil
}

// SetFTSVersionMarker writes the FTS schema version marker for the given workspace.
func (ps *PebbleStore) SetFTSVersionMarker(ws [8]byte, version byte) error {
	versionKey := keys.FTSVersionKey(ws)
	if err := ps.db.Set(versionKey, []byte{version}, pebble.Sync); err != nil {
		return fmt.Errorf("storage: set FTS version marker: %w", err)
	}
	return nil
}

// FTSVersionMarker reads the FTS schema version marker for the given workspace.
// Returns the version byte, true if set, or 0, false if not yet written.
func (ps *PebbleStore) FTSVersionMarker(ws [8]byte) (byte, bool, error) {
	versionKey := keys.FTSVersionKey(ws)
	val, closer, err := ps.db.Get(versionKey)
	if errors.Is(err, pebble.ErrNotFound) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("storage: get FTS version marker: %w", err)
	}
	defer closer.Close()
	if len(val) == 0 {
		return 0, false, nil
	}
	return val[0], true, nil
}

// TransitionCache returns the tiered PAS transition cache.
// Callers should use this for all transition reads/writes instead of
// calling IncrTransitionBatch/GetTopTransitions on PebbleStore directly.
func (ps *PebbleStore) TransitionCache() *TransitionCache {
	return ps.transCache
}

// Close flushes all pending writes and closes the Pebble database. Idempotent.
func (ps *PebbleStore) Close() error {
	var closeErr error
	ps.closeOnce.Do(func() {
		if ps.transCache != nil {
			ps.transCache.Close()
		}
		if ps.counterFlush != nil {
			ps.counterFlush.Close()
		}
		if ps.provWork != nil {
			ps.provWork.Close()
		}
		if ps.walSync != nil {
			ps.walSync.Close()
		}
		closeErr = ps.db.Close()
	})
	return closeErr
}

// DiskSize returns the total on-disk size of all Pebble database files.
func (ps *PebbleStore) DiskSize() int64 {
	return int64(ps.db.Metrics().DiskSpaceUsage())
}
