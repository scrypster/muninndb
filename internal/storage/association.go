package storage

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/cockroachdb/pebble"
	"github.com/scrypster/muninndb/internal/storage/keys"
)

// encodeAssocValue serializes association metadata into the 26-byte value
// stored under 0x03/0x04 Pebble keys.
// Layout: relType(2) | confidence(4) | createdAt(8) | lastActivated(4) | peakWeight(4) | coActivationCount(4) = 26 bytes
// Old readers that only consume 18 or 22 bytes continue to work; new fields are at the tail.
func encodeAssocValue(relType RelType, confidence float32, createdAt time.Time, lastActivated int32, peakWeight float32, coActivationCount uint32) [26]byte {
	var val [26]byte
	binary.BigEndian.PutUint16(val[0:2], uint16(relType))
	binary.BigEndian.PutUint32(val[2:6], math.Float32bits(confidence))
	var nanos int64
	if !createdAt.IsZero() {
		nanos = createdAt.UnixNano()
	}
	binary.BigEndian.PutUint64(val[6:14], uint64(nanos))
	binary.BigEndian.PutUint32(val[14:18], uint32(lastActivated))
	binary.BigEndian.PutUint32(val[18:22], math.Float32bits(peakWeight))
	binary.BigEndian.PutUint32(val[22:26], coActivationCount)
	return val
}

// decodeAssocValue decodes an 18-byte (legacy), 22-byte, 26-byte, or 30-byte association value.
// Returns peakWeight=0 for legacy 18-byte values, coActivationCount=0 for pre-26-byte values.
// Returns restoredAt=0 for pre-30-byte values (never restored).
// All-zero 18-byte values treated as pre-fix legacy: confidence=1.0, rest zero.
func decodeAssocValue(val []byte) (relType RelType, confidence float32, createdAt time.Time, lastActivated int32, peakWeight float32, coActivationCount uint32, restoredAt int32) {
	if len(val) < 18 {
		return 0, 1.0, time.Time{}, 0, 0, 0, 0
	}
	// All-zero 18-byte values are pre-fix legacy (old encoder wrote blank values).
	// 22-byte and 26-byte values are from newer encoders and always carry real metadata.
	if len(val) == 18 {
		allZero := true
		for _, b := range val {
			if b != 0 {
				allZero = false
				break
			}
		}
		if allZero {
			return 0, 1.0, time.Time{}, 0, 0, 0, 0
		}
	}
	relType = RelType(binary.BigEndian.Uint16(val[0:2]))
	confidence = math.Float32frombits(binary.BigEndian.Uint32(val[2:6]))
	nanos := int64(binary.BigEndian.Uint64(val[6:14]))
	if nanos != 0 {
		createdAt = time.Unix(0, nanos)
	}
	lastActivated = int32(binary.BigEndian.Uint32(val[14:18]))
	if len(val) >= 22 {
		peakWeight = math.Float32frombits(binary.BigEndian.Uint32(val[18:22]))
	}
	if len(val) >= 26 {
		coActivationCount = binary.BigEndian.Uint32(val[22:26])
	}
	if len(val) >= 30 {
		restoredAt = int32(binary.BigEndian.Uint32(val[26:30]))
	}
	return
}

// encodeArchiveValue serializes association metadata into the 30-byte value
// stored under 0x25 archive keys.
// Layout: relType(2) | confidence(4) | createdAt(8) | lastActivated(4) | peakWeight(4) | coActivationCount(4) | restoredAt(4) = 30 bytes
func encodeArchiveValue(relType RelType, confidence float32, createdAt time.Time, lastActivated int32, peakWeight float32, coActivationCount uint32, restoredAt int32) [30]byte {
	var val [30]byte
	binary.BigEndian.PutUint16(val[0:2], uint16(relType))
	binary.BigEndian.PutUint32(val[2:6], math.Float32bits(confidence))
	var nanos int64
	if !createdAt.IsZero() {
		nanos = createdAt.UnixNano()
	}
	binary.BigEndian.PutUint64(val[6:14], uint64(nanos))
	binary.BigEndian.PutUint32(val[14:18], uint32(lastActivated))
	binary.BigEndian.PutUint32(val[18:22], math.Float32bits(peakWeight))
	binary.BigEndian.PutUint32(val[22:26], coActivationCount)
	binary.BigEndian.PutUint32(val[26:30], uint32(restoredAt))
	return val
}

// assocCacheKey returns the 24-byte cache key for a (wsPrefix, engramID) pair.
func assocCacheKey(wsPrefix [8]byte, id ULID) [24]byte {
	var k [24]byte
	copy(k[:8], wsPrefix[:])
	copy(k[8:], id[:])
	return k
}

// WriteAssociation writes forward and reverse association keys.
func (ps *PebbleStore) WriteAssociation(ctx context.Context, wsPrefix [8]byte, src, dst ULID, assoc *Association) error {
	batch := ps.db.NewBatch()
	defer batch.Close()

	// Forward association (0x03 key)
	// PeakWeight is seeded to Weight on initial write — this is the association's first peak.
	// Creation is itself a co-activation event; always seed at 1.
	// Callers should not set CoActivationCount on new associations.
	const seedCount uint32 = 1
	fwdKey := keys.AssocFwdKey(wsPrefix, [16]byte(src), assoc.Weight, [16]byte(dst))
	assocValue := encodeAssocValue(assoc.RelType, assoc.Confidence, assoc.CreatedAt, assoc.LastActivated, assoc.Weight, seedCount)
	batch.Set(fwdKey, assocValue[:], nil)

	// Reverse association (0x04 key)
	revKey := keys.AssocRevKey(wsPrefix, [16]byte(dst), assoc.Weight, [16]byte(src))
	batch.Set(revKey, assocValue[:], nil)

	// Write forward weight index (0x14 key) for O(1) GetAssocWeight lookups.
	var weightBuf [4]byte
	binary.BigEndian.PutUint32(weightBuf[:], math.Float32bits(assoc.Weight))
	batch.Set(keys.AssocWeightIndexKey(wsPrefix, [16]byte(src), [16]byte(dst)), weightBuf[:], nil)

	if err := batch.Commit(pebble.NoSync); err != nil {
		return fmt.Errorf("commit batch: %w", err)
	}

	// Invalidate source node's cached association list so BFS traversal
	// sees the new edge immediately instead of waiting for TTL expiry.
	ps.assocCache.Remove(assocCacheKey(wsPrefix, src))

	return nil
}

// GetAssociations returns forward associations for a set of source IDs.
//
// Fast path: all IDs that are cache-warm are served without touching Pebble.
// Slow path: cache-cold IDs are scanned with a SINGLE Pebble iterator using
// sorted forward seeks — O(1) iterator open + N seeks instead of N iterator opens.
// Seeks are strictly forward (IDs sorted ascending) so Pebble never seeks backward.
func (ps *PebbleStore) GetAssociations(ctx context.Context, wsPrefix [8]byte, ids []ULID, maxPerNode int) (map[ULID][]Association, error) {
	result := make(map[ULID][]Association, len(ids))

	// Phase 1: serve all cache-warm IDs without touching Pebble.
	// expirable.LRU handles TTL expiry automatically on Get.
	// Return copies of cached slices to prevent callers from mutating the cache.
	var uncached []ULID
	for _, id := range ids {
		ck := assocCacheKey(wsPrefix, id)
		if entry, ok := ps.assocCache.Get(ck); ok {
			// Determine slice length
			n := len(entry.assocs)
			if maxPerNode > 0 && n > maxPerNode {
				n = maxPerNode
			}
			// Return a copy of the slice
			result[id] = append([]Association(nil), entry.assocs[:n]...)
			continue
		}
		uncached = append(uncached, id)
	}
	if len(uncached) == 0 {
		return result, nil
	}

	// Phase 2: sort uncached IDs so all Pebble seeks are strictly forward.
	sort.Slice(uncached, func(i, j int) bool {
		return bytes.Compare(uncached[i][:], uncached[j][:]) < 0
	})

	// Phase 3: open ONE iterator covering the entire 0x03|wsPrefix range (snapshot-aware).
	lower := keys.AssocFwdRangeStart(wsPrefix)
	upper := keys.AssocFwdRangeEnd(wsPrefix) // nil means unbounded (all-0xFF workspace)
	iter, err := ps.pebbleReader(ctx).NewIter(&pebble.IterOptions{
		LowerBound: lower,
		UpperBound: upper,
	})
	if err != nil {
		return nil, fmt.Errorf("assoc iterator: %w", err)
	}
	defer iter.Close()

	for _, id := range uncached {
		prefix := keys.AssocFwdPrefixForID(wsPrefix, id) // 0x03 | ws | id (25 bytes)
		var assocs []Association

		// SeekGE positions at the first key >= prefix (strictly forward seek).
		for iter.SeekGE(prefix); iter.Valid(); iter.Next() {
			k := iter.Key()
			// Stop when we've left this srcID's prefix range.
			if len(k) < 25 || !bytes.Equal(k[:25], prefix) {
				break
			}
			if maxPerNode > 0 && len(assocs) >= maxPerNode {
				break
			}
			// Key layout: 0x03 | ws(8) | srcID(16) | weightComplement(4) | dstID(16) = 45 bytes
			if len(k) < 45 {
				continue
			}
			var targetID ULID
			copy(targetID[:], k[29:45])
			var wc [4]byte
			copy(wc[:], k[25:29])
			weight := keys.WeightFromComplement(wc)
			relType, confidence, createdAt, lastActivated, peakWeight, coActivationCount, restoredAt := decodeAssocValue(iter.Value())
			assocs = append(assocs, Association{
				TargetID:          targetID,
				Weight:            weight,
				RelType:           relType,
				Confidence:        confidence,
				CreatedAt:         createdAt,
				LastActivated:     lastActivated,
				PeakWeight:        peakWeight,
				CoActivationCount: coActivationCount,
				RestoredAt:        restoredAt,
			})
		}

		result[id] = assocs
		ps.assocCache.Add(assocCacheKey(wsPrefix, id), &assocCacheEntry{assocs: assocs})
	}

	return result, nil
}

// associationsForOne scans forward-assoc keys for a single source ID.
// Checks the in-memory assocCache first; falls back to Pebble on miss.
// Extracted to ensure iter.Close() is deferred at function scope, not inside
// the calling loop (which would defer until the outer function returned).
// Returns a copy of cached slices to prevent callers from mutating the cache.
func (ps *PebbleStore) associationsForOne(wsPrefix [8]byte, id ULID, maxPerNode int) ([]Association, error) {
	// Fast path: check in-memory cache.
	// expirable.LRU handles TTL expiry automatically on Get.
	// Return a copy to prevent caller mutation.
	ck := assocCacheKey(wsPrefix, id)
	if entry, ok := ps.assocCache.Get(ck); ok {
		n := len(entry.assocs)
		if maxPerNode > 0 && n > maxPerNode {
			n = maxPerNode
		}
		return append([]Association(nil), entry.assocs[:n]...), nil
	}

	// Build prefix: 0x03 | wsPrefix | id
	prefix := keys.AssocFwdKey(wsPrefix, [16]byte(id), 1.0, [16]byte{})
	prefix = prefix[0 : 1+8+16] // trim to just the prefix portion

	iter, err := PrefixIterator(ps.db, prefix)
	if err != nil {
		return nil, fmt.Errorf("prefix iterator: %w", err)
	}
	defer iter.Close()

	var assocs []Association
	for iter.First(); iter.Valid(); iter.Next() {
		if maxPerNode > 0 && len(assocs) >= maxPerNode {
			break
		}
		// Key format: 0x03 | wsPrefix(8) | srcID(16) | weightComplement(4) | dstID(16)
		// dstID starts at offset 29.
		key := iter.Key()
		if len(key) < 45 {
			continue
		}
		var targetID ULID
		copy(targetID[:], key[29:45])
		wc := [4]byte{}
		copy(wc[:], key[25:29])
		weight := keys.WeightFromComplement(wc)

		// Decode value bytes: rel_type, confidence, timestamps, peakWeight
		val := iter.Value()
		relType, confidence, createdAt, lastActivated, peakWeight, coActivationCount, restoredAt := decodeAssocValue(val)

		assocs = append(assocs, Association{
			TargetID:          targetID,
			Weight:            weight,
			RelType:           relType,
			Confidence:        confidence,
			CreatedAt:         createdAt,
			LastActivated:     lastActivated,
			PeakWeight:        peakWeight,
			CoActivationCount: coActivationCount,
			RestoredAt:        restoredAt,
		})
	}
	// Populate cache — expirable.LRU enforces the TTL automatically.
	ps.assocCache.Add(ck, &assocCacheEntry{assocs: assocs})
	return assocs, nil
}

// GetAssocWeight reads the weight of a forward association for pair (a,b).
// Uses the 0x14 weight index for O(1) lookup.
// Returns 0.0 if no association exists.
func (ps *PebbleStore) GetAssocWeight(ctx context.Context, wsPrefix [8]byte, a, b ULID) (float32, error) {
	key := keys.AssocWeightIndexKey(wsPrefix, [16]byte(a), [16]byte(b))
	val, err := Get(ps.db, key)
	if err != nil || val == nil || len(val) < 4 {
		return 0.0, nil
	}
	return math.Float32frombits(binary.BigEndian.Uint32(val[:4])), nil
}

// getAssocValue reads the decoded association metadata for pair (a→b).
// Uses knownWeight to construct the 0x03 key. Returns zero values if no
// association exists or the key cannot be read.
func (ps *PebbleStore) getAssocValue(wsPrefix [8]byte, a, b ULID, knownWeight float32) (relType RelType, confidence float32, createdAt time.Time, lastActivated int32, peakWeight float32, coActivationCount uint32) {
	if knownWeight <= 0 {
		return 0, 1.0, time.Time{}, 0, 0, 0
	}
	fwdKey := keys.AssocFwdKey(wsPrefix, [16]byte(a), knownWeight, [16]byte(b))
	val, err := Get(ps.db, fwdKey)
	if err != nil || val == nil {
		return 0, 1.0, time.Time{}, 0, 0, 0
	}
	relType, confidence, createdAt, lastActivated, peakWeight, coActivationCount, _ = decodeAssocValue(val)
	return
}

// getAssocValueFull reads all 7 decoded fields for pair (a→b), including restoredAt.
// Uses GetAssocWeight to determine the current weight, then reads the forward key.
// Returns zero values (with confidence=1.0) if no association exists.
func (ps *PebbleStore) getAssocValueFull(wsPrefix [8]byte, a, b ULID) (RelType, float32, time.Time, int32, float32, uint32, int32) {
	w, _ := ps.GetAssocWeight(context.Background(), wsPrefix, a, b)
	if w <= 0 {
		return 0, 1.0, time.Time{}, 0, 0, 0, 0
	}
	fwdKey := keys.AssocFwdKey(wsPrefix, [16]byte(a), w, [16]byte(b))
	val, err := Get(ps.db, fwdKey)
	if err != nil || val == nil {
		return 0, 1.0, time.Time{}, 0, 0, 0, 0
	}
	return decodeAssocValue(val)
}

// UpdateAssocWeight writes/updates the 0x03 and 0x04 association keys for pair (a,b).
// It reads the current weight first and deletes the old keys before writing new
// ones, preventing stale duplicate entries from accumulating in the key space.
// Existing metadata (relType, confidence, createdAt) is preserved; lastActivated
// is set to now (Hebbian update = activation).
// countDelta is added to the existing CoActivationCount (saturating at MaxUint32).
// If the edge was previously restored (restoredAt != 0), restoredAt is cleared once
// the edge re-establishes itself: 3+ co-activations post-restore OR weight exceeds
// restoreWeight * 1.5 (where restoreWeight = existingPeak * 0.25).
func (ps *PebbleStore) UpdateAssocWeight(ctx context.Context, wsPrefix [8]byte, a, b ULID, weight float32, countDelta uint32) error {
	batch := ps.db.NewBatch()
	defer batch.Close()

	// Read existing metadata (all 7 fields) before deleting old keys.
	// getAssocValueFull does its own GetAssocWeight internally; capture oldWeight
	// separately so we can delete the old fwd/rev keys.
	oldWeight, _ := ps.GetAssocWeight(ctx, wsPrefix, a, b)
	relType, confidence, createdAt, _, existingPeak, existingCoAct, existingRestoredAt := ps.getAssocValueFull(wsPrefix, a, b)

	if oldWeight > 0 {
		batch.Delete(keys.AssocFwdKey(wsPrefix, [16]byte(a), oldWeight, [16]byte(b)), nil)
		batch.Delete(keys.AssocRevKey(wsPrefix, [16]byte(b), oldWeight, [16]byte(a)), nil)
	}

	// Preserve existing metadata; set lastActivated = now (Hebbian update = activation).
	// PeakWeight is monotonically non-decreasing: max(existingPeak, newWeight).
	now := int32(time.Now().Unix())
	newPeak := existingPeak
	if weight > newPeak {
		newPeak = weight
	}
	// Accumulate CoActivationCount with saturation at MaxUint32.
	newCoAct := existingCoAct
	if countDelta > 0 {
		if newCoAct+countDelta < newCoAct {
			newCoAct = ^uint32(0) // saturate at MaxUint32
		} else {
			newCoAct += countDelta
		}
	}

	// Clear restoredAt if the edge has re-established itself post-restore.
	// Clearing conditions (either is sufficient):
	//   - coActivationCount has reached 3+ (incremental activations accumulate across calls), OR
	//   - weight exceeds restoreWeight * 1.5 (restoreWeight ≈ existingPeak * 0.25).
	// We use an absolute coActivationCount threshold of 3 because the count at restore time
	// is not separately tracked; newly restored edges start at the archive's count value and
	// each UpdateAssocWeight call increments it, so the threshold is met after enough calls.
	outRestoredAt := existingRestoredAt
	if outRestoredAt != 0 {
		restoreWeight := existingPeak * 0.25
		if newCoAct >= existingCoAct+3 || newCoAct >= 3 || weight > restoreWeight*1.5 {
			outRestoredAt = 0
		}
	}

	// Encode: use 30-byte archive format when the edge has (or had) a restoredAt,
	// to avoid silently dropping that field. Use compact 26-byte format otherwise.
	if existingRestoredAt != 0 || outRestoredAt != 0 {
		v := encodeArchiveValue(relType, confidence, createdAt, now, newPeak, newCoAct, outRestoredAt)
		batch.Set(keys.AssocFwdKey(wsPrefix, [16]byte(a), weight, [16]byte(b)), v[:], nil)
		batch.Set(keys.AssocRevKey(wsPrefix, [16]byte(b), weight, [16]byte(a)), v[:], nil)
	} else {
		v := encodeAssocValue(relType, confidence, createdAt, now, newPeak, newCoAct)
		batch.Set(keys.AssocFwdKey(wsPrefix, [16]byte(a), weight, [16]byte(b)), v[:], nil)
		batch.Set(keys.AssocRevKey(wsPrefix, [16]byte(b), weight, [16]byte(a)), v[:], nil)
	}

	// Update weight index.
	var wiBuf [4]byte
	binary.BigEndian.PutUint32(wiBuf[:], math.Float32bits(weight))
	batch.Set(keys.AssocWeightIndexKey(wsPrefix, [16]byte(a), [16]byte(b)), wiBuf[:], nil)

	if err := batch.Commit(pebble.NoSync); err != nil {
		return fmt.Errorf("commit batch: %w", err)
	}

	ps.assocCache.Remove(assocCacheKey(wsPrefix, a))
	return nil
}

// UpdateAssocWeightBatch atomically updates multiple association weights in a single batch.
// All updates are committed atomically — either all succeed or none do.
// Existing metadata (relType, confidence, createdAt) is preserved per-pair;
// lastActivated is set to now (Hebbian update = activation).
func (ps *PebbleStore) UpdateAssocWeightBatch(ctx context.Context, updates []AssocWeightUpdate) error {
	batch := ps.db.NewBatch()
	defer batch.Close()

	now := int32(time.Now().Unix())

	for _, update := range updates {
		oldWeight, _ := ps.GetAssocWeight(ctx, update.WS, update.Src, update.Dst)
		relType, confidence, createdAt, _, existingPeak, existingCoAct, existingRestoredAt := ps.getAssocValueFull(update.WS, ULID(update.Src), ULID(update.Dst))

		if oldWeight > 0 {
			batch.Delete(keys.AssocFwdKey(update.WS, update.Src, oldWeight, update.Dst), nil)
			batch.Delete(keys.AssocRevKey(update.WS, update.Dst, oldWeight, update.Src), nil)
		}

		// PeakWeight is monotonically non-decreasing: max(existingPeak, newWeight).
		newPeak := existingPeak
		if update.Weight > newPeak {
			newPeak = update.Weight
		}
		// Accumulate CoActivationCount with saturation at MaxUint32.
		newCoAct := existingCoAct
		if update.CountDelta > 0 {
			if newCoAct+update.CountDelta < newCoAct {
				newCoAct = ^uint32(0) // saturate at MaxUint32
			} else {
				newCoAct += update.CountDelta
			}
		}

		// Clear restoredAt if the edge has re-established itself post-restore.
		// Same conditions as UpdateAssocWeight: absolute coActivationCount >= 3
		// OR newCoAct increased by 3+ in this call OR weight > restoreWeight*1.5.
		outRestoredAt := existingRestoredAt
		if outRestoredAt != 0 {
			restoreWeight := existingPeak * 0.25
			if newCoAct >= existingCoAct+3 || newCoAct >= 3 || update.Weight > restoreWeight*1.5 {
				outRestoredAt = 0
			}
		}

		// Encode: use 30-byte archive format when the edge has (or had) a restoredAt.
		if existingRestoredAt != 0 || outRestoredAt != 0 {
			v := encodeArchiveValue(relType, confidence, createdAt, now, newPeak, newCoAct, outRestoredAt)
			batch.Set(keys.AssocFwdKey(update.WS, update.Src, update.Weight, update.Dst), v[:], nil)
			batch.Set(keys.AssocRevKey(update.WS, update.Dst, update.Weight, update.Src), v[:], nil)
		} else {
			v := encodeAssocValue(relType, confidence, createdAt, now, newPeak, newCoAct)
			batch.Set(keys.AssocFwdKey(update.WS, update.Src, update.Weight, update.Dst), v[:], nil)
			batch.Set(keys.AssocRevKey(update.WS, update.Dst, update.Weight, update.Src), v[:], nil)
		}

		// Update weight index.
		var wiBuf [4]byte
		binary.BigEndian.PutUint32(wiBuf[:], math.Float32bits(update.Weight))
		batch.Set(keys.AssocWeightIndexKey(update.WS, update.Src, update.Dst), wiBuf[:], nil)
	}

	if err := batch.Commit(pebble.NoSync); err != nil {
		return fmt.Errorf("commit batch: %w", err)
	}

	// Invalidate assoc cache for all updated source nodes.
	// Deduplicate to avoid redundant removals when a source appears multiple times.
	seen := make(map[[24]byte]struct{}, len(updates))
	for _, update := range updates {
		ck := assocCacheKey(update.WS, update.Src)
		if _, ok := seen[ck]; !ok {
			seen[ck] = struct{}{}
			ps.assocCache.Remove(ck)
		}
	}
	return nil
}

const assocDecayChunkSize = 10_000

// assocDecayGraceWindow is the minimum time since lastActivated before an
// association is eligible for weight decay. Edges activated within this window
// are skipped on the decay pass, protecting recently-used associations from
// being penalized by the next scheduled consolidation run.
// TODO: make this configurable per vault via PlasticityConfig.
const assocDecayGraceWindow = 5 * time.Minute

// DecayAssocWeights multiplies all association weights for wsPrefix by decayFactor,
// deleting entries that fall below minWeight. Returns count of deleted entries.
//
// When archiveThreshold > 0 and an edge hits the dynamic floor AND its
// consolidation score (peakWeight * coActivationCount / daysSinceLastActivated)
// exceeds archiveThreshold, the edge is moved to the 0x25 archive namespace
// instead of being clamped.
//
// Processes in chunks of assocDecayChunkSize to bound memory usage.
// The Pebble iterator sees a consistent snapshot (created before any mutations),
// so chunked commits are safe: each original key is visited exactly once.
func (ps *PebbleStore) DecayAssocWeights(ctx context.Context, wsPrefix [8]byte, decayFactor float64, minWeight float32, archiveThreshold float64) (int, error) {
	// Build scan prefix: 0x03 | wsPrefix (9 bytes).
	scanPrefix := make([]byte, 9)
	scanPrefix[0] = 0x03
	copy(scanPrefix[1:9], wsPrefix[:])

	// The iterator snapshot is fixed at creation time — mutations committed in
	// intermediate batches are invisible to it, making chunked processing safe.
	iter, err := PrefixIterator(ps.db, scanPrefix)
	if err != nil {
		return 0, fmt.Errorf("prefix iterator: %w", err)
	}
	defer iter.Close()

	type assocEntry struct {
		src    [16]byte
		dst    [16]byte
		oldW   float32
		newW   float32
		remove  bool
		archive bool
		// Preserved from existing Pebble value:
		relType           RelType
		confidence        float32
		createdAt         time.Time
		lastActivated     int32
		peakWeight        float32 // historical max Weight
		coActivationCount uint32
	}

	removed := 0
	chunk := make([]assocEntry, 0, assocDecayChunkSize)

	flushChunk := func() error {
		if len(chunk) == 0 {
			return nil
		}
		batch := ps.db.NewBatch()
		defer batch.Close()
		for _, e := range chunk {
			_ = batch.Delete(keys.AssocFwdKey(wsPrefix, e.src, e.oldW, e.dst), nil)
			_ = batch.Delete(keys.AssocRevKey(wsPrefix, e.dst, e.oldW, e.src), nil)
			if e.archive {
				// Move to 0x25 archive namespace. Write archive value, delete live
				// weight index; fwd/rev keys already deleted above.
				archVal := encodeArchiveValue(e.relType, e.confidence, e.createdAt, e.lastActivated, e.peakWeight, e.coActivationCount, 0)
				_ = batch.Set(keys.ArchiveAssocKey(wsPrefix, e.src, e.dst), archVal[:], nil)
				_ = batch.Delete(keys.AssocWeightIndexKey(wsPrefix, e.src, e.dst), nil)
				ps.AddToArchiveBloom(e.src)
			} else if !e.remove {
				// Preserve existing metadata. Do NOT update lastActivated here —
				// decay is a background process, not a user activation.
				// Keep peak up to date (shouldn't change during decay, but guard).
				peak := e.peakWeight
				if e.newW > peak {
					peak = e.newW
				}
				val := encodeAssocValue(e.relType, e.confidence, e.createdAt, e.lastActivated, peak, e.coActivationCount)
				_ = batch.Set(keys.AssocFwdKey(wsPrefix, e.src, e.newW, e.dst), val[:], nil)
				_ = batch.Set(keys.AssocRevKey(wsPrefix, e.dst, e.newW, e.src), val[:], nil)
				var wiBuf [4]byte
				binary.BigEndian.PutUint32(wiBuf[:], math.Float32bits(e.newW))
				_ = batch.Set(keys.AssocWeightIndexKey(wsPrefix, e.src, e.dst), wiBuf[:], nil)
			} else {
				_ = batch.Delete(keys.AssocWeightIndexKey(wsPrefix, e.src, e.dst), nil)
			}
		}
		if err := batch.Commit(pebble.NoSync); err != nil {
			return fmt.Errorf("decay assoc chunk commit: %w", err)
		}
		chunk = chunk[:0]
		return nil
	}

	for iter.First(); iter.Valid(); iter.Next() {
		key := iter.Key()
		if len(key) < 45 {
			continue
		}

		// Decode existing metadata from the value bytes before extracting key fields.
		relType, confidence, createdAt, lastActivated, peakWeight, coActivationCount, _ := decodeAssocValue(iter.Value())

		// Recency skip: associations activated within the grace window are not decayed.
		// Window must be > a few seconds (to protect edges just activated) but
		// < 30 minutes (so edges activated 30 min ago are still eligible for decay).
		if lastActivated > 0 {
			activatedAt := time.Unix(int64(lastActivated), 0)
			if time.Since(activatedAt) < assocDecayGraceWindow {
				continue // skip — recently used, leave key untouched
			}
		}

		var src, dst [16]byte
		copy(src[:], key[9:25])
		var wc [4]byte
		copy(wc[:], key[25:29])
		copy(dst[:], key[29:45])

		oldW := keys.WeightFromComplement(wc)
		newW := float32(float64(oldW) * decayFactor)

		// Bootstrap legacy peakWeight from current weight (pre-upgrade associations have peakWeight=0).
		// This runs before decay so oldW is the pre-decay weight — a good conservative peak estimate.
		if peakWeight == 0 {
			peakWeight = oldW
		}

		e := assocEntry{
			src: src, dst: dst, oldW: oldW, newW: newW,
			relType: relType, confidence: confidence,
			createdAt: createdAt, lastActivated: lastActivated,
			peakWeight: peakWeight, coActivationCount: coActivationCount,
		}
		if newW < minWeight {
			dynamicFloor := peakWeight * 0.05
			if dynamicFloor > 0 && archiveThreshold > 0 {
				// Compute consolidation score to decide archive vs clamp.
				daysSince := time.Since(time.Unix(int64(lastActivated), 0)).Hours() / 24.0
				if daysSince < 1.0 {
					daysSince = 1.0
				}
				consolidationScore := (float64(peakWeight) * float64(coActivationCount)) / daysSince
				if consolidationScore > archiveThreshold {
					// Strong historical association: archive instead of clamp.
					e.archive = true
				} else {
					// Earned association: clamp to floor instead of deleting.
					e.newW = dynamicFloor
				}
			} else if dynamicFloor > 0 {
				// Earned association: clamp to floor instead of deleting.
				e.newW = dynamicFloor
				// e.remove stays false
			} else {
				// No peak (guard; shouldn't reach here after bootstrap).
				e.remove = true
				removed++
			}
		} // else: newW >= minWeight, standard keep
		chunk = append(chunk, e)

		if len(chunk) >= assocDecayChunkSize {
			if err := flushChunk(); err != nil {
				return removed, err
			}
		}
	}
	if err := iter.Error(); err != nil {
		return 0, fmt.Errorf("decay assoc scan: %w", err)
	}
	if err := flushChunk(); err != nil {
		return removed, err
	}
	return removed, nil
}

// GetConceptAssociations returns up to maxN neighbor IDs for spreading activation.
func (ps *PebbleStore) GetConceptAssociations(ctx context.Context, wsPrefix [8]byte, id ULID, maxN int) ([]ULID, error) {
	// Build prefix: 0x03 | wsPrefix | id
	prefix := keys.AssocFwdKey(wsPrefix, [16]byte(id), 1.0, [16]byte{})
	prefix = prefix[0 : 1+8+16] // just keep the prefix part

	// Create iterator with prefix
	iter, err := PrefixIterator(ps.db, prefix)
	if err != nil {
		return nil, fmt.Errorf("prefix iterator: %w", err)
	}
	defer iter.Close()

	neighbors := []ULID{}
	count := 0

	for iter.First(); iter.Valid() && count < maxN; iter.Next() {
		// Extract TargetID from key bytes
		// Key format: 0x03 | wsPrefix(8) | srcID(16) | weightComplement(4) | dstID(16)
		// TargetID is at offset 29
		key := iter.Key()
		if len(key) >= 45 {
			var targetID ULID
			copy(targetID[:], key[29:45])
			neighbors = append(neighbors, targetID)
			count++
		}
	}

	return neighbors, nil
}

// GetChildrenByParent returns the IDs of all engrams that have a RelIsPartOf
// association targeting parentID. Scans the 0x04 reverse index and filters
// by RelType in the value bytes.
func (ps *PebbleStore) GetChildrenByParent(ctx context.Context, wsPrefix [8]byte, parentID ULID) ([]ULID, error) {
	prefix := keys.AssocRevPrefixForID(wsPrefix, [16]byte(parentID))
	iter, err := PrefixIterator(ps.db, prefix)
	if err != nil {
		return nil, fmt.Errorf("GetChildrenByParent prefix iter: %w", err)
	}
	defer iter.Close()

	// Reverse key: 0x04 | ws(8) | dstID(16) | weightComplement(4) | srcID(16) = 45 bytes
	// srcID (the child) is at offset 29.
	var children []ULID
	for iter.First(); iter.Valid(); iter.Next() {
		k := iter.Key()
		if len(k) < 45 {
			continue
		}
		val := iter.Value()
		relType, _, _, _, _, _, _ := decodeAssocValue(val)
		if relType != RelIsPartOf {
			continue
		}
		var childID ULID
		copy(childID[:], k[29:45])
		children = append(children, childID)
	}
	if err := iter.Error(); err != nil {
		return nil, fmt.Errorf("GetChildrenByParent scan: %w", err)
	}
	return children, nil
}

// FlagContradiction writes the 0x0A contradiction key for pair (a,b).
func (ps *PebbleStore) FlagContradiction(ctx context.Context, wsPrefix [8]byte, a, b ULID) error {
	batch := ps.db.NewBatch()
	defer batch.Close()

	// Use a canonical ordering for the pair to ensure consistency
	// Compare a and b lexicographically
	var aBytes [16]byte = [16]byte(a)
	var bBytes [16]byte = [16]byte(b)

	if CompareULIDs(a, b) > 0 {
		aBytes, bBytes = bBytes, aBytes
	}

	// Write contradiction key using conceptHash=0 as a marker
	// The key structure is: 0x0A | wsPrefix(8) | conceptHash(4) | relType(2) | id(16)
	// We use conceptHash=0 to indicate this is a pair contradiction flag
	contraKey := keys.ContradictionKey(wsPrefix, 0, 0, aBytes)
	batch.Set(contraKey, bBytes[:], nil)

	// Also write reverse for quick lookup
	contraKeyRev := keys.ContradictionKey(wsPrefix, 0, 0, bBytes)
	batch.Set(contraKeyRev, aBytes[:], nil)

	if err := batch.Commit(pebble.NoSync); err != nil {
		return fmt.Errorf("commit batch: %w", err)
	}

	return nil
}

// ResolveContradiction deletes the contradiction marker(s) for the pair (a,b).
// Contradictions are stored bidirectionally, so both directions are removed.
func (ps *PebbleStore) ResolveContradiction(ctx context.Context, wsPrefix [8]byte, a, b ULID) error {
	batch := ps.db.NewBatch()
	defer batch.Close()

	var aBytes [16]byte = [16]byte(a)
	var bBytes [16]byte = [16]byte(b)

	// Delete both directions regardless of canonical ordering — the caller may pass
	// (a,b) or (b,a) so we always remove the marker written for each direction.
	contraKeyAB := keys.ContradictionKey(wsPrefix, 0, 0, aBytes)
	contraKeyBA := keys.ContradictionKey(wsPrefix, 0, 0, bBytes)
	batch.Delete(contraKeyAB, nil)
	batch.Delete(contraKeyBA, nil)

	if err := batch.Commit(pebble.NoSync); err != nil {
		return fmt.Errorf("resolve contradiction: %w", err)
	}
	return nil
}

// GetContradictions returns all contradiction pairs in the vault by scanning the 0x0A prefix.
// The key structure is: 0x0A | wsPrefix(8) | conceptHash(4) | relType(2) | id(16) = 31 bytes.
// The value is the partner ULID (16 bytes).
// Each pair (a, b) is stored twice (forward and reverse), so we deduplicate by canonical ordering.
func (ps *PebbleStore) GetContradictions(ctx context.Context, wsPrefix [8]byte) ([][2]ULID, error) {
	lower := keys.ContradictionKeyPrefix(wsPrefix)
	upper := make([]byte, len(lower))
	copy(upper, lower)
	// Increment last byte to form upper bound
	upper[len(upper)-1]++

	iter, err := ps.db.NewIter(&pebble.IterOptions{LowerBound: lower, UpperBound: upper})
	if err != nil {
		return nil, err
	}
	defer iter.Close()

	// Key layout: 0x0A(1) | ws(8) | conceptHash(4) | relType(2) | id(16) = 31 bytes total
	const keyLen = 1 + 8 + 4 + 2 + 16
	const idOffset = 1 + 8 + 4 + 2 // offset where the 16-byte id starts

	seen := make(map[[32]byte]bool)
	var pairs [][2]ULID
	for valid := iter.First(); valid; valid = iter.Next() {
		k := iter.Key()
		if len(k) < keyLen {
			continue
		}
		val := iter.Value()
		if len(val) < 16 {
			continue
		}
		var a ULID
		copy(a[:], k[idOffset:idOffset+16])
		var b ULID
		copy(b[:], val[:16])

		// Canonicalize: always put smaller first to deduplicate
		if CompareULIDs(a, b) > 0 {
			a, b = b, a
		}
		var dedupeKey [32]byte
		copy(dedupeKey[:16], a[:])
		copy(dedupeKey[16:], b[:])
		if !seen[dedupeKey] {
			seen[dedupeKey] = true
			pairs = append(pairs, [2]ULID{a, b})
		}
	}
	return pairs, nil
}
