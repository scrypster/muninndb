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

// encodeAssocValue serializes association metadata into the 18-byte value
// stored under association forward/reverse keys (0x03/0x04).
// Layout: relType(uint16 BE,2) | confidence(float32 BE,4) | createdAt(int64 nanos BE,8) | lastActivated(int32 BE,4)
func encodeAssocValue(relType RelType, confidence float32, createdAt time.Time, lastActivated int32) [18]byte {
	var val [18]byte
	binary.BigEndian.PutUint16(val[0:2], uint16(relType))
	binary.BigEndian.PutUint32(val[2:6], math.Float32bits(confidence))
	var nanos int64
	if !createdAt.IsZero() {
		nanos = createdAt.UnixNano()
	}
	binary.BigEndian.PutUint64(val[6:14], uint64(nanos))
	binary.BigEndian.PutUint32(val[14:18], uint32(lastActivated))
	return val
}

// decodeAssocValue decodes the 18-byte association value.
// All-zero values are treated as legacy (pre-BUG-2-fix): returns relType=0, confidence=1.0.
func decodeAssocValue(val []byte) (relType RelType, confidence float32, createdAt time.Time, lastActivated int32) {
	if len(val) < 18 {
		return 0, 1.0, time.Time{}, 0
	}
	allZero := true
	for _, b := range val[:18] {
		if b != 0 {
			allZero = false
			break
		}
	}
	if allZero {
		// Legacy association written before BUG-2 fix; use optimistic defaults.
		return 0, 1.0, time.Time{}, 0
	}
	relType = RelType(binary.BigEndian.Uint16(val[0:2]))
	confidence = math.Float32frombits(binary.BigEndian.Uint32(val[2:6]))
	nanos := int64(binary.BigEndian.Uint64(val[6:14]))
	if nanos != 0 {
		createdAt = time.Unix(0, nanos)
	}
	lastActivated = int32(binary.BigEndian.Uint32(val[14:18]))
	return
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
	fwdKey := keys.AssocFwdKey(wsPrefix, [16]byte(src), assoc.Weight, [16]byte(dst))
	assocValue := encodeAssocValue(assoc.RelType, assoc.Confidence, assoc.CreatedAt, assoc.LastActivated)
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
	var uncached []ULID
	for _, id := range ids {
		ck := assocCacheKey(wsPrefix, id)
		if entry, ok := ps.assocCache.Get(ck); ok {
			if maxPerNode <= 0 || len(entry.assocs) <= maxPerNode {
				result[id] = entry.assocs
			} else {
				result[id] = entry.assocs[:maxPerNode]
			}
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
			relType, confidence, createdAt, lastActivated := decodeAssocValue(iter.Value())
			assocs = append(assocs, Association{
				TargetID:      targetID,
				Weight:        weight,
				RelType:       relType,
				Confidence:    confidence,
				CreatedAt:     createdAt,
				LastActivated: lastActivated,
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
func (ps *PebbleStore) associationsForOne(wsPrefix [8]byte, id ULID, maxPerNode int) ([]Association, error) {
	// Fast path: check in-memory cache.
	// expirable.LRU handles TTL expiry automatically on Get.
	ck := assocCacheKey(wsPrefix, id)
	if entry, ok := ps.assocCache.Get(ck); ok {
		if maxPerNode <= 0 || len(entry.assocs) <= maxPerNode {
			return entry.assocs, nil
		}
		return entry.assocs[:maxPerNode], nil
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

		// Decode value bytes: rel_type, confidence, timestamps
		val := iter.Value()
		relType, confidence, createdAt, lastActivated := decodeAssocValue(val)

		assocs = append(assocs, Association{
			TargetID:      targetID,
			Weight:        weight,
			RelType:       relType,
			Confidence:    confidence,
			CreatedAt:     createdAt,
			LastActivated: lastActivated,
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

// UpdateAssocWeight writes/updates the 0x03 and 0x04 association keys for pair (a,b).
// It reads the current weight first and deletes the old keys before writing new
// ones, preventing stale duplicate entries from accumulating in the key space.
func (ps *PebbleStore) UpdateAssocWeight(ctx context.Context, wsPrefix [8]byte, a, b ULID, weight float32) error {
	batch := ps.db.NewBatch()
	defer batch.Close()

	// Delete old keys if a prior weight exists for this pair.
	if oldWeight, err := ps.GetAssocWeight(ctx, wsPrefix, a, b); err == nil && oldWeight > 0 {
		oldFwd := keys.AssocFwdKey(wsPrefix, [16]byte(a), oldWeight, [16]byte(b))
		oldRev := keys.AssocRevKey(wsPrefix, [16]byte(b), oldWeight, [16]byte(a))
		batch.Delete(oldFwd, nil)
		batch.Delete(oldRev, nil)
	}

	// Weight-only update: rel_type unknown, use confidence=1.0 optimistic default
	assocValue := encodeAssocValue(0, 1.0, time.Time{}, 0)
	batch.Set(keys.AssocFwdKey(wsPrefix, [16]byte(a), weight, [16]byte(b)), assocValue[:], nil)
	batch.Set(keys.AssocRevKey(wsPrefix, [16]byte(b), weight, [16]byte(a)), assocValue[:], nil)

	// Update weight index.
	var wiBuf [4]byte
	binary.BigEndian.PutUint32(wiBuf[:], math.Float32bits(weight))
	batch.Set(keys.AssocWeightIndexKey(wsPrefix, [16]byte(a), [16]byte(b)), wiBuf[:], nil)

	if err := batch.Commit(pebble.NoSync); err != nil {
		return fmt.Errorf("commit batch: %w", err)
	}

	// Association list is TTL-cached; stale weights are acceptable for BFS traversal.

	return nil
}

// UpdateAssocWeightBatch atomically updates multiple association weights in a single batch.
// All updates are committed atomically — either all succeed or none do.
func (ps *PebbleStore) UpdateAssocWeightBatch(ctx context.Context, updates []AssocWeightUpdate) error {
	batch := ps.db.NewBatch()
	defer batch.Close()

	assocValue := encodeAssocValue(0, 1.0, time.Time{}, 0)

	for _, update := range updates {
		// Delete old keys if a prior weight exists for this pair.
		if oldWeight, err := ps.GetAssocWeight(ctx, update.WS, update.Src, update.Dst); err == nil && oldWeight > 0 {
			oldFwd := keys.AssocFwdKey(update.WS, [16]byte(update.Src), oldWeight, [16]byte(update.Dst))
			oldRev := keys.AssocRevKey(update.WS, [16]byte(update.Dst), oldWeight, [16]byte(update.Src))
			batch.Delete(oldFwd, nil)
			batch.Delete(oldRev, nil)
		}

		// Set new forward and reverse keys
		batch.Set(keys.AssocFwdKey(update.WS, [16]byte(update.Src), update.Weight, [16]byte(update.Dst)), assocValue[:], nil)
		batch.Set(keys.AssocRevKey(update.WS, [16]byte(update.Dst), update.Weight, [16]byte(update.Src)), assocValue[:], nil)

		// Update weight index.
		var wiBuf [4]byte
		binary.BigEndian.PutUint32(wiBuf[:], math.Float32bits(update.Weight))
		batch.Set(keys.AssocWeightIndexKey(update.WS, [16]byte(update.Src), [16]byte(update.Dst)), wiBuf[:], nil)
	}

	if err := batch.Commit(pebble.NoSync); err != nil {
		return fmt.Errorf("commit batch: %w", err)
	}

	return nil
}

const assocDecayChunkSize = 10_000

// DecayAssocWeights multiplies all association weights for wsPrefix by decayFactor,
// deleting entries that fall below minWeight. Returns count of deleted entries.
//
// Processes in chunks of assocDecayChunkSize to bound memory usage.
// The Pebble iterator sees a consistent snapshot (created before any mutations),
// so chunked commits are safe: each original key is visited exactly once.
func (ps *PebbleStore) DecayAssocWeights(ctx context.Context, wsPrefix [8]byte, decayFactor float64, minWeight float32) (int, error) {
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
		remove bool
	}

	removed := 0
	chunk := make([]assocEntry, 0, assocDecayChunkSize)

	flushChunk := func() error {
		if len(chunk) == 0 {
			return nil
		}
		batch := ps.db.NewBatch()
		defer batch.Close()
		newAssocValue := encodeAssocValue(0, 1.0, time.Time{}, 0)
		for _, e := range chunk {
			_ = batch.Delete(keys.AssocFwdKey(wsPrefix, e.src, e.oldW, e.dst), nil)
			_ = batch.Delete(keys.AssocRevKey(wsPrefix, e.dst, e.oldW, e.src), nil)
			if !e.remove {
				_ = batch.Set(keys.AssocFwdKey(wsPrefix, e.src, e.newW, e.dst), newAssocValue[:], nil)
				_ = batch.Set(keys.AssocRevKey(wsPrefix, e.dst, e.newW, e.src), newAssocValue[:], nil)
				// Add weight index update:
				var wiBuf [4]byte
				binary.BigEndian.PutUint32(wiBuf[:], math.Float32bits(e.newW))
				_ = batch.Set(keys.AssocWeightIndexKey(wsPrefix, e.src, e.dst), wiBuf[:], nil)
			} else {
				// Add weight index deletion:
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
		var src, dst [16]byte
		copy(src[:], key[9:25])
		var wc [4]byte
		copy(wc[:], key[25:29])
		copy(dst[:], key[29:45])

		oldW := keys.WeightFromComplement(wc)
		newW := float32(float64(oldW) * decayFactor)

		e := assocEntry{src: src, dst: dst, oldW: oldW, newW: newW}
		if newW < minWeight {
			e.remove = true
			removed++
		}
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
