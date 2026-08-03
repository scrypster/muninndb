package storage

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/cockroachdb/pebble"
	"github.com/scrypster/muninndb/internal/prefix"
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
//
// Refuses with ErrDanglingEndpoint when either endpoint has no 0x01 engram
// record (STO-12). This is the choke point for the whole writer set —
// autoassoc, the neighbor and goal-link workers, the refines writer, the
// plugin store adapter, inline caller relationships and muninn_link all reach
// the 0x03/0x04/0x14 keyspace through here. Guarding those sites individually
// would be cosmetic: the next worker added would reintroduce the class.
func (ps *PebbleStore) WriteAssociation(ctx context.Context, wsPrefix [8]byte, src, dst ULID, assoc *Association) error {
	if err := ps.checkEndpointsLive(wsPrefix, [16]byte(src), [16]byte(dst)); err != nil {
		return err
	}
	// #771: refuse rather than silently replace a different RelType edge
	// collision at the same (src, weight, dst) key.
	if err := checkRelTypeCollision(ps.db, wsPrefix, [16]byte(src), assoc.Weight, [16]byte(dst), assoc.RelType); err != nil {
		return err
	}
	return ps.writeAssociationUnguarded(ctx, wsPrefix, src, dst, assoc)
}

// writeAssociationUnguarded is WriteAssociation's body without the STO-12
// endpoint check. Split out so the guard's cost can be measured directly
// (BenchmarkWriteAssociation{With,No}Guard); no production caller uses it.
func (ps *PebbleStore) writeAssociationUnguarded(ctx context.Context, wsPrefix [8]byte, src, dst ULID, assoc *Association) error {
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
	ps.replicateBatch(batch)

	// Invalidate the source node's cached FORWARD list and the destination
	// node's cached REVERSE list, so both endpoints see the new edge
	// immediately instead of waiting for TTL expiry. The reverse eviction is
	// keyed on dst because that is the 0x04 key's first ULID (COG-31).
	ps.assocCache.Remove(assocCacheKey(wsPrefix, src))
	ps.revAssocCache.Remove(assocCacheKey(wsPrefix, dst))

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
			// A truncated entry can only answer a caller asking for no more
			// than it holds; otherwise re-scan rather than silently under-serve
			// (#820). Same rule as revAssocCache's hit path.
			if !entry.truncated || maxPerNode > 0 && maxPerNode <= n {
				if maxPerNode > 0 && n > maxPerNode {
					n = maxPerNode
				}
				// Return a copy of the slice
				result[id] = append([]Association(nil), entry.assocs[:n]...)
				continue
			}
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
	rawIter, err := ps.pebbleReader(ctx).NewIter(&pebble.IterOptions{
		LowerBound: lower,
		UpperBound: upper,
	})
	if err != nil {
		return nil, fmt.Errorf("assoc iterator: %w", err)
	}
	iter := ps.scanIter(lower, rawIter)
	defer iter.Close()

	for _, id := range uncached {
		prefix := keys.AssocFwdPrefixForID(wsPrefix, id) // 0x03 | ws | id (25 bytes)
		var assocs []Association
		truncated := false

		// SeekGE positions at the first key >= prefix (strictly forward seek).
		for iter.SeekGE(prefix); iter.Valid(); iter.Next() {
			k := iter.Key()
			// Stop when we've left this srcID's prefix range.
			if len(k) < 25 || !bytes.Equal(k[:25], prefix) {
				break
			}
			if maxPerNode > 0 && len(assocs) >= maxPerNode {
				truncated = true
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

		// #808: FAIL CLOSED on a mid-scan iterator failure, and do it BEFORE
		// anything is returned or cached.
		//
		// This is the opposite call from the STO-12 endpoint-liveness guard
		// (#803), which fails OPEN, and the difference is the loss direction.
		// #803 is a WRITE guard: refusing on a read fault deletes a live edge
		// that cannot be recovered. This is a READ: refusing costs the caller
		// an error it can retry or report, and destroys nothing. Meanwhile
		// proceeding does not stay local — the truncated list is written into
		// assocCache and served for the 2s TTL to readers that never saw a
		// fault, and "zero edges" is read as "not in a declared chain" by
		// currencyInDeclaredChain, which is how a scan fault turns into a false
		// `possibly_superseded_by` advisory (COG-25).
		//
		// One iterator serves every uncached id, so a failure anywhere is
		// terminal for the whole call.
		if err := iter.Error(); err != nil {
			return nil, fmt.Errorf("assoc scan: %w", err)
		}

		ps.assocCache.Add(assocCacheKey(wsPrefix, id), &assocCacheEntry{assocs: assocs, truncated: truncated})
		// COPY (#820). `assocs` is now the cache entry's backing array; the hit
		// path above copies for the same reason, and this function's own doc
		// comment promises it. Returning `assocs` published a live view of the
		// cache entry to the first caller after every miss — safe only while no
		// caller mutates and no caller forwards it, which is a property of
		// today's callers, not of this function. One allocation per cold miss,
		// against a Pebble scan.
		result[id] = append([]Association(nil), assocs...)
	}

	return result, nil
}

// GetRankingNeighbors returns, for each id, the top-maxPerNode association
// edges by weight over the UNION of:
//
//   - every 0x03 forward edge FROM id (all relation types — identical to
//     GetAssociations), and
//   - every 0x04 reverse edge INTO id whose RelType.BidirectionalForRanking()
//     is true (RelCoActivated, RelRelatesTo, RelContradicts, and the
//     user-defined range).
//
// RANKING AND TRAVERSAL ONLY — COG-31. This function deliberately LOSES the
// direction an edge was written in: a reverse edge is returned with the
// far endpoint in TargetID, indistinguishable from a forward one. A writer or
// a direction-presenting surface that consumes it will manufacture facts
// ("the OLD version supersedes the NEW one"). Use GetAssociations for those.
//
// The forward half delegates to GetAssociations, so it keeps the assocCache
// and its 2s TTL byte-identically. The reverse half has its own cache of the
// same shape (revAssocCache, keyed on the DESTINATION); the merged list is
// never cached.
//
// Both halves now fail alike on a mid-scan iterator failure: each checks
// iter.Error() and propagates, before anything is cached (#808). They used to
// differ — the forward half reported a truncated scan as a short-but-successful
// list — and that difference was never intentional design.
//
// Both 0x03 and 0x04 order by weightComplement in the same key position, so
// both streams arrive weight-DESCENDING and the cap is an exact top-N via a
// two-pointer merge — never a concatenate-then-truncate, which could fill the
// cap with floor-weight forward edges while discarding a far heavier reverse
// one. Duplicates (the same partner reachable both ways) are collapsed to the
// larger weight BEFORE the cap, so phase4HebbianBoost cannot double-count a
// pair.
func (ps *PebbleStore) GetRankingNeighbors(ctx context.Context, wsPrefix [8]byte, ids []ULID, maxPerNode int) (map[ULID][]Association, error) {
	// Both input streams are capped at maxPerNode, not read uncapped. That is
	// exact, not an approximation: each stream is weight-descending and its
	// targets are distinct, so the top-maxPerNode of the merged, deduplicated
	// union is always a subset of the union of the two capped streams. Reading
	// the forward half UNCAPPED would also change what GetAssociations stores
	// in assocCache (it caches the list it built UNDER the cap), leaking a
	// behaviour change into every other GetAssociations consumer for the 2s
	// TTL — the exact non-interference this increment must not spend.
	fwd, err := ps.GetAssociations(ctx, wsPrefix, ids, maxPerNode)
	if err != nil {
		return nil, err
	}

	rev, err := ps.rankingReverseEdges(ctx, wsPrefix, ids, maxPerNode)
	if err != nil {
		return nil, err
	}

	result := make(map[ULID][]Association, len(ids))
	for _, id := range ids {
		result[id] = mergeRankingNeighbors(fwd[id], rev[id], maxPerNode)
	}
	return result, nil
}

// rankingReverseEdges scans the 0x04 reverse index for every id with a SINGLE
// iterator and sorted forward seeks, mirroring GetAssociations' shape.
// Reusing GetReverseAssociations per id would open one iterator per id, and
// BFS passes up to 20 ids per level.
//
// Returned Associations carry the SOURCE engram in TargetID (the engram that
// points at id) and are weight-descending, matching the forward stream.
// Edges whose RelType is not BidirectionalForRanking are skipped.
// maxPerNode <= 0 means uncapped.
//
// Results are cached in revAssocCache, and BOTH the hit and the miss path
// return a COPY: the returned slices never alias a cache entry's backing
// array, so a caller may mutate or retain what it is given. GetRankingNeighbors
// offers the same guarantee for the MERGED list — see mergeRankingNeighbors,
// whose forward half needs its own copy because GetAssociations does not offer
// it (#820).
func (ps *PebbleStore) rankingReverseEdges(ctx context.Context, wsPrefix [8]byte, ids []ULID, maxPerNode int) (map[ULID][]Association, error) {
	if len(ids) == 0 {
		return nil, nil
	}

	// scanCap bounds the scan and what gets cached, independent of the
	// caller's cap, so one cached entry serves every ranking caller.
	scanCap := revAssocScanCap
	if maxPerNode <= 0 || maxPerNode > scanCap {
		scanCap = maxPerNode // 0 (uncapped) or an unusually large request
	}

	out := make(map[ULID][]Association, len(ids))

	// Phase 1: serve cache-warm ids without touching Pebble, mirroring
	// GetAssociations' forward fast path (2s TTL, same size, same key shape).
	var uncached []ULID
	for _, id := range ids {
		if _, dup := out[id]; dup {
			continue // duplicate id in the input
		}
		ck := assocCacheKey(wsPrefix, id)
		if entry, ok := ps.revAssocCache.Get(ck); ok {
			n := len(entry.assocs)
			// A truncated entry can only answer a caller asking for no more
			// than it holds; otherwise re-scan rather than silently under-serve.
			if !entry.truncated || maxPerNode > 0 && maxPerNode <= n {
				if maxPerNode > 0 && n > maxPerNode {
					n = maxPerNode
				}
				out[id] = append([]Association(nil), entry.assocs[:n]...)
				continue
			}
		}
		out[id] = nil // claim the slot so a duplicate id is not scanned twice
		uncached = append(uncached, id)
	}
	if len(uncached) == 0 {
		return out, nil
	}

	sorted := make([]ULID, len(uncached))
	copy(sorted, uncached)
	sort.Slice(sorted, func(i, j int) bool {
		return bytes.Compare(sorted[i][:], sorted[j][:]) < 0
	})

	iter, err := ps.pebbleReader(ctx).NewIter(&pebble.IterOptions{
		LowerBound: keys.AssocRevRangeStart(wsPrefix),
		UpperBound: keys.AssocRevRangeEnd(wsPrefix),
	})
	if err != nil {
		return nil, fmt.Errorf("ranking reverse iterator: %w", err)
	}
	defer iter.Close()

	for _, id := range sorted {
		idPrefix := keys.AssocRevPrefixForID(wsPrefix, [16]byte(id)) // 0x04 | ws | dst (25 bytes)
		var assocs []Association
		truncated := false

		for iter.SeekGE(idPrefix); iter.Valid(); iter.Next() {
			k := iter.Key()
			if len(k) < 25 || !bytes.Equal(k[:25], idPrefix) {
				break
			}
			// Accepted edges arrive weight-descending, so stopping at the cap
			// keeps the heaviest ones. Filtered-out (directional) edges
			// deliberately do not consume a slot — which means this loop is
			// O(inbound degree), NOT O(scanCap), for a directional hub. That
			// cost is measured and the trade is argued at revAssocScanCap;
			// read it before turning this into a scanned-key budget.
			if scanCap > 0 && len(assocs) >= scanCap {
				truncated = true
				break
			}
			// Key layout: 0x04 | ws(8) | dstID(16) | weightComplement(4) | srcID(16) = 45 bytes
			if len(k) < 45 {
				continue
			}
			var srcID ULID
			copy(srcID[:], k[29:45])
			// No explicit self-edge skip: a self-loop writes fwd(a,a) and
			// rev(a,a) with the same weight, and mergeRankingNeighbors'
			// TargetID dedup already collapses the two copies into one. An
			// extra `srcID == id` guard here would be dead code, and dead code
			// that looks like a safety property is worse than none — pinned by
			// TestGetRankingNeighbors_SelfEdge.
			relType, confidence, createdAt, lastActivated, peakWeight, coActivationCount, restoredAt := decodeAssocValue(iter.Value())
			if !relType.BidirectionalForRanking() {
				continue
			}
			var wc [4]byte
			copy(wc[:], k[25:29])
			assocs = append(assocs, Association{
				TargetID:          srcID,
				Weight:            keys.WeightFromComplement(wc),
				RelType:           relType,
				Confidence:        confidence,
				CreatedAt:         createdAt,
				LastActivated:     lastActivated,
				PeakWeight:        peakWeight,
				CoActivationCount: coActivationCount,
				RestoredAt:        restoredAt,
			})
		}
		if err := iter.Error(); err != nil {
			return nil, fmt.Errorf("ranking reverse scan: %w", err)
		}
		ps.revAssocCache.Add(assocCacheKey(wsPrefix, id), &revAssocCacheEntry{assocs: assocs, truncated: truncated})
		n := len(assocs)
		if maxPerNode > 0 && n > maxPerNode {
			n = maxPerNode
		}
		// COPY. `assocs` is now the cache entry's backing array, and the
		// caller owns what it is handed — the hit path above copies for the
		// same reason. Returning `assocs` here would publish the cache's array
		// for the rest of the 2s TTL; it is safe only while no caller mutates
		// and no caller forwards it, which is a property of today's callers,
		// not of this function. Defending that at the merge instead would put
		// the invariant somewhere the caller cannot see it. Pinned by
		// TestRankingReverseEdges_MissPathDoesNotAliasTheCache.
		out[id] = append([]Association(nil), assocs[:n]...)
	}
	return out, nil
}

// mergeRankingNeighbors merges two weight-DESCENDING association streams into
// one, deduplicating by TargetID (keeping the larger weight) and then taking
// the top maxPerNode. maxPerNode <= 0 means uncapped.
//
// Dedup happens BEFORE the cap, so a pair reachable in both directions
// consumes exactly one cap slot and contributes its weight exactly once.
//
// The returned slice is ALWAYS freshly allocated, including on the no-reverse-
// edges shortcut, which used to return `fwd` as it stood. `fwd` comes from
// GetAssociations, whose miss path used to return the slice it had just put in
// assocCache (#820, now fixed at the source) — so that shortcut published a
// live cache entry to the caller for the 2s TTL, but only for nodes with no
// inbound symmetric edges. An ownership contract that holds for some nodes and
// not others, decided by data the caller cannot see, is the trap; the copy here
// (<= maxPerNode entries: 20 in phase4, 10 in BFS) is kept even now that the
// source is fixed, so this function's contract does not depend on its
// caller's. Pinned by TestGetRankingNeighbors_NoReverseEdgesDoesNotAliasTheCache.
func mergeRankingNeighbors(fwd, rev []Association, maxPerNode int) []Association {
	if len(rev) == 0 {
		if maxPerNode > 0 && len(fwd) > maxPerNode {
			return append([]Association(nil), fwd[:maxPerNode]...)
		}
		if len(fwd) == 0 {
			return nil
		}
		return append([]Association(nil), fwd...)
	}

	capHint := len(fwd) + len(rev)
	if maxPerNode > 0 && capHint > maxPerNode {
		capHint = maxPerNode
	}
	merged := make([]Association, 0, capHint)

	i, j := 0, 0
	for i < len(fwd) || j < len(rev) {
		if maxPerNode > 0 && len(merged) >= maxPerNode {
			break
		}
		var next Association
		// Both streams are weight-descending; take the heavier head. Ties go to
		// the forward stream so a reverse edge never displaces an equal-weight
		// forward edge from the cap.
		if j >= len(rev) || (i < len(fwd) && fwd[i].Weight >= rev[j].Weight) {
			next = fwd[i]
			i++
		} else {
			next = rev[j]
			j++
		}
		// Dedup by linear scan, not a map. This runs once per CANDIDATE on
		// every recall (up to 50 per phase4HebbianBoost call), and the merged
		// list is bounded by maxPerNode — 20 in phase4, 10 in BFS. Allocating
		// a map per candidate measured as the single largest cost in the
		// merge; a 16-byte compare over <=20 entries is cheaper and allocates
		// nothing. The heavier copy was emitted first, so a duplicate is
		// always equal-or-lighter and is simply dropped.
		dup := false
		for k := range merged {
			if merged[k].TargetID == next.TargetID {
				dup = true
				break
			}
		}
		if dup {
			continue
		}
		merged = append(merged, next)
	}
	return merged
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
		// A truncated entry can only answer a caller asking for no more than it
		// holds; otherwise fall through and re-scan (#820).
		if !entry.truncated || maxPerNode > 0 && maxPerNode <= n {
			if maxPerNode > 0 && n > maxPerNode {
				n = maxPerNode
			}
			return append([]Association(nil), entry.assocs[:n]...), nil
		}
	}

	// Build prefix: 0x03 | wsPrefix | id
	prefix := keys.AssocFwdKey(wsPrefix, [16]byte(id), 1.0, [16]byte{})
	prefix = prefix[0 : 1+8+16] // trim to just the prefix portion

	rawIter, err := PrefixIterator(ps.db, prefix)
	if err != nil {
		return nil, fmt.Errorf("prefix iterator: %w", err)
	}
	iter := ps.scanIter(prefix, rawIter)
	defer iter.Close()

	var assocs []Association
	truncated := false
	for iter.First(); iter.Valid(); iter.Next() {
		if maxPerNode > 0 && len(assocs) >= maxPerNode {
			truncated = true
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
	// #808: check the scan BEFORE anything is cached or returned. See the same
	// guard in GetAssociations for the fail-closed reasoning.
	if err := iter.Error(); err != nil {
		return nil, fmt.Errorf("associationsForOne scan: %w", err)
	}
	// Populate cache — expirable.LRU enforces the TTL automatically.
	ps.assocCache.Add(ck, &assocCacheEntry{assocs: assocs, truncated: truncated})
	return assocs, nil
}

// GetAssocWeight reads the weight of a forward association for pair (a,b).
// Uses the 0x14 weight index for O(1) lookup.
//
// Returns (0, nil) when no association exists — and (0, err) when the read
// FAILED. The two are not the same fact and must not be conflated: weight 0
// means "no such edge" to every caller, so a swallowed read error makes the
// Hebbian worker re-seed a strong edge at the 0.01 cold-start weight and makes
// dream consolidation infer a fresh A→C over one that already exists.
//
// Three outcomes, not two. A MISSING key is absence. A key holding fewer than
// 4 bytes is neither absence nor a disk fault but a corrupt record: every
// writer of the 0x14 index in this package writes exactly 4 bytes
// (binary.BigEndian.PutUint32 of the float bits), so a shorter one cannot be
// something this code wrote. Reporting it as absence is the same laundering
// one layer down — UpdateAssocWeight then reads oldWeight 0, skips the delete
// of the live forward/reverse keys, and writes a SECOND edge for the pair with
// a reset relType and peakWeight, which GetAssociations, BFS traversal and
// DecayAssocWeights all treat as an independent relation.
//
// rawAssocWeightIndex (assoc_weight_repair.go) deliberately keeps the LENIENT
// policy for the same record: it is a whole-vault repair scan whose job is to
// make progress across damage, not a read-modify-write on one pair.
func (ps *PebbleStore) GetAssocWeight(ctx context.Context, wsPrefix [8]byte, a, b ULID) (float32, error) {
	key := keys.AssocWeightIndexKey(wsPrefix, [16]byte(a), [16]byte(b))
	val, err := ps.pointGet(key)
	if err != nil {
		return 0.0, fmt.Errorf("read assoc weight index: %w", err)
	}
	if val == nil {
		return 0.0, nil // absent
	}
	if len(val) < 4 {
		return 0.0, fmt.Errorf("read assoc weight index: corrupt record for pair %s->%s: %d bytes, want 4",
			a.String(), b.String(), len(val))
	}
	return math.Float32frombits(binary.BigEndian.Uint32(val[:4])), nil
}

// getAssocValueFull reads all 7 decoded fields for pair (a→b), including restoredAt.
// Uses GetAssocWeight to determine the current weight, then reads the forward key.
//
// Absence and failure are distinct returns. Absence is (zero fields with
// confidence=1.0, nil) — the caller may safely treat the pair as new. Failure
// is (zero fields, err) and the caller MUST NOT write those zero fields back:
// re-encoding them overwrites a live edge's relType (reclassifying a
// directional relation as RelType 0, the Hebbian co-activation type),
// createdAt, and peakWeight — which anchors the COG-27 decay ceiling
// (ceiling = peakWeight * 2^(-dt/halfLife)), so a reset peak collapses that
// edge's ceiling on the next decay pass.
func (ps *PebbleStore) getAssocValueFull(ctx context.Context, wsPrefix [8]byte, a, b ULID) (RelType, float32, time.Time, int32, float32, uint32, int32, error) {
	w, err := ps.GetAssocWeight(ctx, wsPrefix, a, b)
	if err != nil {
		return 0, 1.0, time.Time{}, 0, 0, 0, 0, err
	}
	if w <= 0 {
		return 0, 1.0, time.Time{}, 0, 0, 0, 0, nil
	}
	fwdKey := keys.AssocFwdKey(wsPrefix, [16]byte(a), w, [16]byte(b))
	val, err := ps.pointGet(fwdKey)
	if err != nil {
		return 0, 1.0, time.Time{}, 0, 0, 0, 0, fmt.Errorf("read assoc metadata: %w", err)
	}
	// The 0x14 index says w > 0 but the 0x03 key at w is MISSING: an orphaned
	// index entry. Absence, deliberately, and not a fourth outcome — every
	// reader of associations scans the 0x03 namespace, so an index entry with no
	// 0x03 key behind it is invisible to all of them. Re-creating the edge from
	// this write is repair, not damage: there is no live metadata to overwrite.
	if val == nil {
		return 0, 1.0, time.Time{}, 0, 0, 0, 0, nil
	}
	relType, confidence, createdAt, lastActivated, peakWeight, coActivationCount, restoredAt := decodeAssocValue(val)
	return relType, confidence, createdAt, lastActivated, peakWeight, coActivationCount, restoredAt, nil
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

// deleteLegacyFullWeightKeys removes the PRE-FIX key locations for a
// full-weight edge. The old WeightComplement overflowed for weight exactly 1.0
// and produced the byte pattern of weight 0.0, so every pre-fix 1.0-weight
// association lives at the weight-0.0 key position. A post-fix update that
// deletes only the CORRECT position would leave that stale key behind — a
// duplicate edge that still reads as weight 0. Deleting the 0.0 position for a
// pair whose true weight is 1.0 is safe: the 0x14 weight index is one value
// per pair, so a pair cannot legitimately hold both a 1.0 and a 0.0 edge.
func deleteLegacyFullWeightKeys(batch *pebble.Batch, wsPrefix [8]byte, a, b [16]byte, trueWeight float32) {
	if trueWeight != 1.0 {
		return
	}
	_ = batch.Delete(keys.AssocFwdKey(wsPrefix, a, 0.0, b), nil)
	_ = batch.Delete(keys.AssocRevKey(wsPrefix, b, 0.0, a), nil)
}

// assocMonotonicAnchor returns the lastActivated stamp to persist: never
// earlier than the one already on disk.
//
// lastActivated is COG-27's decay input — `ceiling = peakWeight *
// 2^(-(now - lastActivated)/H)` — so moving it BACKWARDS collapses a live
// edge's ceiling on the very next pass, and COG-27's never-raise guarantee
// makes that collapse IRREVERSIBLE: a corrected stamp cannot restore the
// weight, only re-learning can.
//
// The stamp is not purely local. Since #779, AssocWeightUpdate.LastActivatedAt
// reaches disk, and the replication coordinator builds its CoActivationEvent
// from `time.Unix(0, effect.Timestamp)` — the REMOTE PEER'S CLOCK, verbatim.
// The Hebbian worker takes the per-pair max WITHIN one batch, which is
// order-independent and correct, but nothing compared against what was already
// stored. A lagging or skewed peer, or a cog-forward backlog delivered after a
// partition heals, could therefore rewrite another node's decay anchor.
//
// max() is the same shape peakWeight already has in both writers (principle #3:
// make the bad state unrepresentable rather than policy-checked), and it is
// what makes COG-27's "convergent under replication" claim true again — max,
// like min, is commutative and associative, so nodes converge on the anchor
// regardless of delivery order.
//
// A zero `stored` is "unknown, not the Unix epoch" (COG-27) and cannot pin
// anything forward. A FUTURE incoming stamp is deliberately NOT clamped down:
// that is the opposite direction (an inflated anchor suppresses decay), it is a
// separate question, and clamping would break the offline replay driver's
// virtual clock.
func assocMonotonicAnchor(stored, incoming int32) int32 {
	if stored > incoming {
		return stored
	}
	return incoming
}

// UpdateAssocWeight updates one association's weight, preserving the edge's
// existing metadata.
//
// STO-12: this is a CREATOR, not only an updater — exactly the same shape as
// UpdateAssocWeightBatch. A read ABSENCE is a normal fact that returns a nil
// error and a zero tuple, so a pair naming two ULIDs with no engram and no edge
// falls straight through both reads below and Sets a brand-new 0x03/0x04/0x14
// row, returning nil. Its live callers are consolidation's transitive-inference
// phase and the Hebbian store adapter, neither of which re-checks its endpoints.
// Unlike the batch form there is no per-pair skip channel to report through:
// one call, one pair, so a dead endpoint is simply the call's error, and both
// callers already log-and-continue on one.
func (ps *PebbleStore) UpdateAssocWeight(ctx context.Context, wsPrefix [8]byte, a, b ULID, weight float32, countDelta uint32) error {
	if err := ps.checkEndpointsLive(wsPrefix, [16]byte(a), [16]byte(b)); err != nil {
		return err
	}

	batch := ps.db.NewBatch()
	defer batch.Close()

	// Read existing metadata (all 7 fields) before deleting old keys.
	// getAssocValueFull does its own GetAssocWeight internally; capture oldWeight
	// separately so we can delete the old fwd/rev keys.
	// A failed read is NOT an empty edge: refuse the update rather than
	// re-encoding fabricated defaults over live metadata (see getAssocValueFull).
	oldWeight, err := ps.GetAssocWeight(ctx, wsPrefix, a, b)
	if err != nil {
		return fmt.Errorf("update assoc weight: %w", err)
	}
	relType, confidence, createdAt, existingLastActivated, existingPeak, existingCoAct, existingRestoredAt, err := ps.getAssocValueFull(ctx, wsPrefix, a, b)
	if err != nil {
		return fmt.Errorf("update assoc weight: %w", err)
	}

	if oldWeight > 0 {
		batch.Delete(keys.AssocFwdKey(wsPrefix, [16]byte(a), oldWeight, [16]byte(b)), nil)
		batch.Delete(keys.AssocRevKey(wsPrefix, [16]byte(b), oldWeight, [16]byte(a)), nil)
		deleteLegacyFullWeightKeys(batch, wsPrefix, [16]byte(a), [16]byte(b), oldWeight)
	}

	// Preserve existing metadata; set lastActivated = now (Hebbian update = activation).
	// PeakWeight is monotonically non-decreasing: max(existingPeak, newWeight).
	// So is lastActivated, and for the same reason — see assocMonotonicAnchor.
	now := assocMonotonicAnchor(existingLastActivated, int32(time.Now().Unix()))
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
	ps.replicateBatch(batch)

	ps.assocCache.Remove(assocCacheKey(wsPrefix, a))
	ps.revAssocCache.Remove(assocCacheKey(wsPrefix, b))
	return nil
}

// AssocBatchSkipError reports that UpdateAssocWeightBatch committed every
// update whose existing metadata it could read, and SKIPPED the ones it could
// not. It is returned alongside a successful commit — the batch that landed is
// durable — so a caller must treat it as "partially applied", not "failed".
//
// Skipped holds indices into the updates slice passed to the call, ascending.
// The Hebbian worker uses them to suppress the OnWeightUpdate callbacks for
// transitions that were never persisted; the in-tree adapters build their
// storage.AssocWeightUpdate slice positionally, so the indices carry across.
type AssocBatchSkipError struct {
	Skipped []int
	Errs    []error
	Applied int
}

// SkippedUpdates returns the indices of the updates that were not applied.
// Declared as a method so callers in packages that must not import storage can
// reach it through a one-method interface.
func (e *AssocBatchSkipError) SkippedUpdates() []int { return e.Skipped }

func (e *AssocBatchSkipError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "update assoc weight batch: %d of %d update(s) skipped, %d applied",
		len(e.Skipped), len(e.Skipped)+e.Applied, e.Applied)
	// Skipped and Errs are appended in lockstep by UpdateAssocWeightBatch, so
	// they are the same length. The guard is for a hand-constructed value (a
	// test double, a future caller): formatting an error must never panic.
	for i, err := range e.Errs {
		if i > 0 {
			b.WriteString(";")
		}
		if i < len(e.Skipped) {
			fmt.Fprintf(&b, " [%d] %v", e.Skipped[i], err)
		} else {
			fmt.Fprintf(&b, " [?] %v", err)
		}
	}
	return b.String()
}

// Unwrap exposes the per-pair causes so errors.Is reaches the underlying
// Pebble failure.
func (e *AssocBatchSkipError) Unwrap() []error { return e.Errs }

// UpdateAssocWeightBatch updates multiple association weights in a single Pebble
// batch. Every update whose existing metadata could be read is committed
// atomically — the batch is all-or-nothing about what it WRITES, so no pair is
// ever left half-updated and no fabricated tuple is ever persisted.
// Existing metadata (relType, confidence, createdAt) is preserved per-pair;
// lastActivated is set to update.LastActivatedAt when non-zero, else to now
// (Hebbian update = activation). Zero-value = the pre-#779 behaviour, so
// production is byte-identical unless a caller opts in.
//
// # An unreadable pair is skipped, not fatal to the batch
//
// A failed read is NOT an empty edge, so a pair whose metadata cannot be read
// is left exactly as it is on disk and reported. It does not abort the other
// updates, and this is a deliberate reversal of the first version of this fix,
// which aborted the whole batch on the reasoning that "a Pebble point-read
// failure here is an I/O or lifecycle fault that the subsequent batch.Commit
// would hit anyway". That reasoning is false. Measured on Pebble v1.1.5 with 8
// bytes flipped in one .sst and the DB reopened, a point read of the corrupted
// key returns "pebble/table: invalid table ... (checksum mismatch)" while a
// batch.Commit immediately afterwards returns nil: a corrupt block is a
// PERMANENT, KEY-SCOPED fault on a fully writable database.
//
// That makes the blast radius of aborting large and standing. The Hebbian
// worker aggregates up to 50 co-activation events spanning MULTIPLE VAULTS into
// one call, and a frequently co-activated pair recurs in flush window after
// flush window — so one unreadable edge would cost every innocent vault in
// every window its learning, indefinitely. Skipping costs that one pair's
// update and keeps the rest (principle #2: degrade loudly-but-gracefully).
//
// The report is the other half. Returning nil here would be the same
// silently-wrong class one layer up, so a partial application returns
// *AssocBatchSkipError naming each skipped pair and its cause, with the indices
// the caller needs to know which of its updates did not land.
func (ps *PebbleStore) UpdateAssocWeightBatch(ctx context.Context, updates []AssocWeightUpdate) error {
	batch := ps.db.NewBatch()
	defer batch.Close()

	wallNow := int32(time.Now().Unix())
	var skipped []int
	var skipErrs []error
	applied := 0

	for i, update := range updates {
		// STO-12. This method DOES create edges out of nothing, which is not
		// obvious and was measured rather than assumed: #809 made a read
		// FAILURE skip the pair, but read ABSENCE is a normal fact that
		// returns a nil error and a zero tuple, so a pair naming two ULIDs
		// with no engram and no edge falls straight through both reads and
		// Sets a brand-new 0x03/0x04/0x14 row (probe: weight 0.42 written,
		// UpdateAssocWeightBatch returned nil). That is also the shape of the
		// DeleteEngram-vs-Hebbian-flush race: the cascade removes the edge,
		// this batch re-Sets it.
		//
		// Reported through the same skip channel as an unreadable pair —
		// silence would be the silently-wrong class one layer up.
		if err := ps.checkEndpointsLive(update.WS, update.Src, update.Dst); err != nil {
			skipped = append(skipped, i)
			skipErrs = append(skipErrs, fmt.Errorf("pair %s->%s: %w",
				ULID(update.Src).String(), ULID(update.Dst).String(), err))
			continue
		}

		// A caller may carry its own activation timestamp (the replay driver
		// does); zero means "use the wall clock", the pre-#779 behaviour.
		now := wallNow
		if update.LastActivatedAt != 0 {
			now = update.LastActivatedAt
		}
		oldWeight, err := ps.GetAssocWeight(ctx, update.WS, update.Src, update.Dst)
		if err != nil {
			skipped = append(skipped, i)
			skipErrs = append(skipErrs, fmt.Errorf("pair %s->%s: %w",
				ULID(update.Src).String(), ULID(update.Dst).String(), err))
			continue
		}
		relType, confidence, createdAt, existingLastActivated, existingPeak, existingCoAct, existingRestoredAt, err := ps.getAssocValueFull(ctx, update.WS, ULID(update.Src), ULID(update.Dst))
		if err != nil {
			skipped = append(skipped, i)
			skipErrs = append(skipErrs, fmt.Errorf("pair %s->%s: %w",
				ULID(update.Src).String(), ULID(update.Dst).String(), err))
			continue
		}
		applied++

		// The decay anchor is monotonic: a late-arriving or remotely-produced
		// stamp may not move it back. See assocMonotonicAnchor.
		now = assocMonotonicAnchor(existingLastActivated, now)

		if oldWeight > 0 {
			batch.Delete(keys.AssocFwdKey(update.WS, update.Src, oldWeight, update.Dst), nil)
			batch.Delete(keys.AssocRevKey(update.WS, update.Dst, oldWeight, update.Src), nil)
			deleteLegacyFullWeightKeys(batch, update.WS, update.Src, update.Dst, oldWeight)
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
	ps.replicateBatch(batch)

	// Invalidate the forward cache for every updated source and the reverse
	// cache for every updated destination.
	// Deduplicate to avoid redundant removals when an engram appears in the
	// same role more than once. Skipped pairs are invalidated too: dropping a
	// cache entry can only cost a re-read, and the cached list for a pair whose
	// read just failed is exactly the entry least worth trusting.
	//
	// The two dedup sets are SEPARATE, and that is the whole point: the caches
	// are keyed alike but hold different things, so one shared set makes an
	// engram appearing in BOTH roles inside one batch lose its second eviction
	// — whichever cache is reached second keeps serving pre-batch weights for
	// the rest of the 2s TTL. That shape is the common case, not the corner
	// one: HebbianWorker.processBatch emits every C(n,2) pair of a co-activated
	// set, so for any three co-activated engrams X<Y<Z one batch carries (X,Y)
	// and (Y,Z) and Y is both a Dst and a Src. Pinned from both sides by
	// TestUpdateAssocWeightBatch_ForwardCacheEvictedWhenEngramIsAlsoDst and
	// TestUpdateAssocWeightBatch_ReverseCacheEvictedWhenEngramIsAlsoSrc.
	seenFwd := make(map[[24]byte]struct{}, len(updates))
	seenRev := make(map[[24]byte]struct{}, len(updates))
	for _, update := range updates {
		ck := assocCacheKey(update.WS, update.Src)
		if _, ok := seenFwd[ck]; !ok {
			seenFwd[ck] = struct{}{}
			ps.assocCache.Remove(ck)
		}
		rk := assocCacheKey(update.WS, update.Dst)
		if _, ok := seenRev[rk]; !ok {
			seenRev[rk] = struct{}{}
			ps.revAssocCache.Remove(rk)
		}
	}
	if len(skipped) > 0 {
		return &AssocBatchSkipError{Skipped: skipped, Errs: skipErrs, Applied: applied}
	}
	return nil
}

const assocDecayChunkSize = 10_000

// assocDecayWriteEpsilon is a write-avoidance quantum on the [0,1] weight
// scale, NOT a behavioural threshold. An edge whose computed ceiling is within
// epsilon of its stored weight is left untouched this pass.
//
// The skip's cost is a BOUNDED LAG, not accumulated error: the stored weight
// may sit up to epsilon above the true ceiling between writes, but the
// residual never grows with the number of skipped passes, because the ceiling
// is absolute rather than compounded — every pass recomputes it from
// lastActivated regardless of how many passes were skipped. Under the old
// per-pass multiplier the same skip would have permanently forgiven a pass's
// worth of decay. Measured by TestDecayAssoc_EpsilonSkipLagIsBounded: gap
// <= epsilon after both 1,329 and 39,876 skipped passes.
const assocDecayWriteEpsilon = 0.001

// DecayAssocWeights applies time-normalized decay to every association under
// wsPrefix. Returns count of deleted entries.
//
// The mechanism (COG-27) is a peak-anchored elapsed-time ceiling:
//
//	dt      = max(0, now - lastActivated)
//	ceiling = max(peakWeight*0.05, peakWeight * 2^(-dt/halfLife))
//	w_new   = min(w_old, ceiling)
//
// Both inputs already live in the 26-byte association value, so there is no
// value-format change and no new Pebble prefix.
//
// Two properties follow, and they are the entire point of the shape:
//
//   - Cadence-independence. With no intervening activation the ceiling is
//     monotone decreasing in t, so the running minimum over ANY grid of
//     evaluation times equals the ceiling at the final time. Sixty one-minute
//     passes and one sixty-minute pass are bit-identical. Before this, the unit
//     of decay was "one prune-worker tick" — an interval owned by an unrelated
//     engram sweep — which made the configured rate a function of the worker's
//     cadence and ground every edge to the floor within the hour (#762).
//   - It never raises a weight. Decay only ever clamps downward, so a weight
//     written below the peak curve by another writer (the dream engine's
//     inferred weights) is never silently undone, and edges already sitting at
//     the #762 floor are not resurrected for free.
//
// When archiveThreshold > 0 and an edge hits the dynamic floor AND its
// consolidation score (peakWeight * coActivationCount / daysSinceLastActivated)
// exceeds archiveThreshold, the edge is moved to the 0x25 archive namespace
// instead of being clamped.
//
// Processes in chunks of assocDecayChunkSize to bound memory usage.
// The Pebble iterator sees a consistent snapshot (created before any mutations),
// so chunked commits are safe: each original key is visited exactly once.
func (ps *PebbleStore) DecayAssocWeights(ctx context.Context, wsPrefix [8]byte, halfLife time.Duration, minWeight float32, archiveThreshold float64) (int, error) {
	if halfLife <= 0 {
		return 0, fmt.Errorf("decay assoc: halfLife must be positive, got %v", halfLife)
	}
	// One clock read for the whole pass: every edge is evaluated against the
	// same instant, so a long scan cannot decay its tail more than its head.
	now := ps.now()
	halfLifeSeconds := halfLife.Seconds()
	// Build scan prefix: 0x03 | wsPrefix (9 bytes).
	scanPrefix := make([]byte, 9)
	scanPrefix[0] = prefix.AssocFwd
	copy(scanPrefix[1:9], wsPrefix[:])

	// The iterator snapshot is fixed at creation time — mutations committed in
	// intermediate batches are invisible to it, making chunked processing safe.
	iter, err := PrefixIterator(ps.db, scanPrefix)
	if err != nil {
		return 0, fmt.Errorf("prefix iterator: %w", err)
	}
	defer iter.Close()

	type assocEntry struct {
		src     [16]byte
		dst     [16]byte
		oldW    float32
		newW    float32
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
			// e.oldW is decoded from the scanned KEY, so this delete depends on
			// encode(decode(k)) == k for encoder-produced complements — pinned
			// exhaustively (all float32 in [0,1]) by the byte-compat tests in
			// storage/keys. If that identity ever breaks, this delete misses and
			// decay grows duplicate keys unboundedly.
			_ = batch.Delete(keys.AssocFwdKey(wsPrefix, e.src, e.oldW, e.dst), nil)
			_ = batch.Delete(keys.AssocRevKey(wsPrefix, e.dst, e.oldW, e.src), nil)
			deleteLegacyFullWeightKeys(batch, wsPrefix, e.src, e.dst, e.oldW)
			if e.archive {
				// Move to 0x25 archive namespace. Write archive value, delete live
				// weight index; fwd/rev keys already deleted above.
				archVal := encodeArchiveValue(e.relType, e.confidence, e.createdAt, e.lastActivated, e.peakWeight, e.coActivationCount, 0)
				_ = batch.Set(keys.ArchiveAssocKey(wsPrefix, e.src, e.dst), archVal[:], nil)
				_ = batch.Delete(keys.AssocWeightIndexKey(wsPrefix, e.src, e.dst), nil)
				ps.AddToArchiveBloom(e.src)
				// #806: a symmetric relation asserts the same fact from either
				// endpoint (RelType.IsSymmetric, COG-31 — "safe for a WRITER... to
				// consult"), but 0x25 has no reverse index, so an edge archived
				// only under its src is invisible to RestoreArchivedEdges called
				// on its dst — exactly the endpoint a recent session is most
				// likely to touch, since Hebbian canonicalization always makes
				// the newer engram the dst. Mirror the row under the swapped key
				// so the SAME src-prefix scan RestoreArchivedEdges already runs
				// finds it from either side, with no format change and no new
				// prefix. A directional (non-symmetric) edge is never mirrored:
				// doing so would let a restore MINT a live edge in the wrong
				// direction from the reverse endpoint, which COG-31 forbids for
				// any surface, writer included.
				if e.relType.IsSymmetric() {
					_ = batch.Set(keys.ArchiveAssocKey(wsPrefix, e.dst, e.src), archVal[:], nil)
					ps.AddToArchiveBloom(e.dst)
				}
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
		ps.replicateBatch(batch)
		// #818: every entry in this chunk relocated, archived or deleted a
		// 0x03 row (keyed on src) and its 0x04 mirror (keyed on dst), so both
		// caches can be holding pre-decay weights. Evicted POST-COMMIT, per
		// chunk, so a reader that misses immediately after re-scans the state
		// this batch just committed. Weights, not membership, are usually what
		// goes stale here — and weight is what ranking orders on.
		for _, e := range chunk {
			ps.assocCache.Remove(assocCacheKey(wsPrefix, ULID(e.src)))
			ps.revAssocCache.Remove(assocCacheKey(wsPrefix, ULID(e.dst)))
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

		var src, dst [16]byte
		copy(src[:], key[9:25])
		var wc [4]byte
		copy(wc[:], key[25:29])
		copy(dst[:], key[29:45])

		oldW := keys.WeightFromComplement(wc)

		// Bootstrap legacy peakWeight from current weight (pre-upgrade associations have peakWeight=0).
		// This runs before decay so oldW is the pre-decay weight — a good conservative peak estimate.
		if peakWeight == 0 {
			peakWeight = oldW
		}

		// Anchor the elapsed-time clock. lastActivated == 0 means "unknown", NOT
		// "the Unix epoch" — reading it as an epoch timestamp would hand the
		// formula a 56-year elapsed time and floor every legacy edge on the
		// first pass. Fall back to createdAt; if that is unknown too, adopt the
		// edge by stamping lastActivated = now and leaving its weight alone.
		adopt := false
		anchor := lastActivated
		if anchor == 0 {
			if !createdAt.IsZero() {
				anchor = int32(createdAt.Unix())
			} else {
				adopt = true
			}
		}

		e := assocEntry{
			src: src, dst: dst, oldW: oldW, newW: oldW,
			relType: relType, confidence: confidence,
			createdAt: createdAt, lastActivated: lastActivated,
			peakWeight: peakWeight, coActivationCount: coActivationCount,
		}

		if adopt {
			// Stamp the anchor so this edge decays normally from now on; weight
			// untouched this pass. Written unconditionally (no epsilon skip) —
			// the point of the pass is the stamp, not the weight.
			e.lastActivated = int32(now.Unix())
			chunk = append(chunk, e)
			if len(chunk) >= assocDecayChunkSize {
				if err := flushChunk(); err != nil {
					return removed, err
				}
			}
			continue
		}

		// Negative elapsed (backwards clock skew, or a future lastActivated
		// stamp) clamps to zero: never decay, and never boost either.
		elapsed := now.Sub(time.Unix(int64(anchor), 0))
		if elapsed < 0 {
			elapsed = 0
		}

		ceiling := float64(peakWeight) * math.Exp2(-elapsed.Seconds()/halfLifeSeconds)

		// drop is how far this pass would move the weight DOWN. This guard
		// does two jobs:
		//
		//   drop <= 0      the ceiling sits at or above the stored weight, so
		//                  there is nothing to do.
		//   0 < drop < eps bounded-lag write-avoidance. The ceiling is
		//                  absolute, so skipping the write forgives no decay —
		//                  the next pass recomputes from the same anchor.
		//                  Collapses steady-state decay write volume by ~99%.
		//
		// This guard alone is NOT what makes "decay never raises a weight"
		// structural: the floor branch below ASSIGNS e.newW = dynamicFloor,
		// which can exceed the stored weight. The post-floor newW >= oldW
		// guard before append is the second half of the invariant — together
		// they leave no path that can write an increase (principle #3).
		drop := oldW - float32(ceiling)
		if float64(drop) < assocDecayWriteEpsilon {
			continue
		}
		newW := oldW - drop
		e.newW = newW

		if newW < minWeight {
			dynamicFloor := peakWeight * 0.05
			if dynamicFloor > 0 && archiveThreshold > 0 {
				// Compute consolidation score to decide archive vs clamp.
				daysSince := now.Sub(time.Unix(int64(anchor), 0)).Hours() / 24.0
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

		// Second half of the never-raises invariant, and the churn guard. The
		// floor branch above ASSIGNS e.newW = dynamicFloor, which the drop
		// guard cannot see: a stored weight below the floor (dream-engine
		// inferred weights write through UpdateAssocWeight with a monotone
		// peak) would be RAISED to it, and an edge already at the floor would
		// be rewritten — 5 keys plus a replicated batch — every pass, forever.
		// Placement is load-bearing: this must sit AFTER the floor/archive
		// block (the epsilon guard runs before e.newW can be floored) and must
		// exclude archive promotion and the dynamicFloor==0 delete path, which
		// act without lowering the weight.
		if !e.archive && !e.remove && e.newW >= e.oldW {
			continue
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

// GetReverseAssociations returns all associations that TARGET id by scanning
// the 0x04 reverse index. The returned Association.TargetID is the SOURCE
// engram (the engram that points TO id). Results are capped at maxPerNode entries.
// Reverse key layout: 0x04 | ws(8) | dstID(16) | weightComplement(4) | srcID(16) = 45 bytes
func (ps *PebbleStore) GetReverseAssociations(ctx context.Context, wsPrefix [8]byte, id ULID, maxPerNode int) ([]Association, error) {
	prefix := keys.AssocRevPrefixForID(wsPrefix, [16]byte(id))
	iter, err := PrefixIterator(ps.db, prefix)
	if err != nil {
		return nil, fmt.Errorf("GetReverseAssociations prefix iter: %w", err)
	}
	defer iter.Close()

	var results []Association
	for iter.First(); iter.Valid() && (maxPerNode <= 0 || len(results) < maxPerNode); iter.Next() {
		k := iter.Key()
		if len(k) < 45 {
			continue
		}
		var srcID ULID
		copy(srcID[:], k[29:45])

		var wc [4]byte
		copy(wc[:], k[25:29])
		weight := keys.WeightFromComplement(wc)

		relType, confidence, createdAt, lastActivated, peakWeight, coActivationCount, restoredAt := decodeAssocValue(iter.Value())

		results = append(results, Association{
			TargetID:          srcID,
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
	if err := iter.Error(); err != nil {
		return nil, fmt.Errorf("GetReverseAssociations scan: %w", err)
	}
	return results, nil
}

// contradictionValueLen is the current 0x0A value length: the partner ULID
// followed by the detection time in Unix nanoseconds.
// Layout: partner(16) | detectedAtUnixNano(8).
//
// Values written before the timestamp existed are a bare 16-byte partner ULID.
// They decode to a ZERO detectedAt, which every reader must render as "unknown"
// — never as a real instant and never as the Go zero time dressed up as one
// (CLAUDE.md §2.1: a plausible wrong value is worse than an admitted gap).
const contradictionValueLen = 16 + 8

// encodeContradictionValue builds the 0x0A value for a marker.
func encodeContradictionValue(partner [16]byte, detectedAt time.Time) []byte {
	val := make([]byte, contradictionValueLen)
	copy(val[:16], partner[:])
	if !detectedAt.IsZero() {
		binary.BigEndian.PutUint64(val[16:24], uint64(detectedAt.UnixNano()))
	}
	return val
}

// decodeContradictionValue reads a 0x0A value. ok is false when the value is
// too short to even name the partner. detectedAt is the zero time for legacy
// 16-byte values, meaning "flagged, detection time not recorded".
func decodeContradictionValue(val []byte) (partner ULID, detectedAt time.Time, ok bool) {
	if len(val) < 16 {
		return partner, time.Time{}, false
	}
	copy(partner[:], val[:16])
	if len(val) >= contradictionValueLen {
		if nanos := int64(binary.BigEndian.Uint64(val[16:24])); nanos != 0 {
			detectedAt = time.Unix(0, nanos)
		}
	}
	return partner, detectedAt, true
}

// readContradictionMarker reads the 0x0A marker at key and returns the moment
// the contradiction FIRST became known.
//
// Three outcomes, not two — the same policy GetAssocWeight states one seam
// over. A MISSING key is absence: (zero, false, nil). A key holding fewer than
// 16 bytes cannot even name the partner, so it is not something any writer in
// this package produced; that is corruption, and reporting it as absence is the
// same laundering one layer down. Anything else is a read FAILURE.
//
// The two callers take deliberately OPPOSITE actions on that failure, because
// their loss directions are opposite. See each call site.
func (ps *PebbleStore) readContradictionMarker(key []byte) (detectedAt time.Time, exists bool, err error) {
	val, err := ps.pointGet(key)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("read contradiction marker: %w", err)
	}
	if val == nil {
		return time.Time{}, false, nil
	}
	_, prior, ok := decodeContradictionValue(val)
	if !ok {
		return time.Time{}, false, fmt.Errorf("read contradiction marker: corrupt record: %d bytes, want at least 16", len(val))
	}
	return prior, true, nil
}

// FlagContradiction writes the 0x0A contradiction key for pair (a,b).
//
// The marker's value carries the moment the contradiction FIRST became known.
// The detector re-writes the marker on every observation of the same edge, so
// a re-flag deliberately preserves the original timestamp: detected_at answers
// "when did this vault learn these two memories disagree?", not "when did the
// batch worker last look at it".
func (ps *PebbleStore) FlagContradiction(ctx context.Context, wsPrefix [8]byte, a, b ULID) (bool, error) {
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
	contraKeyRev := keys.ContradictionKey(wsPrefix, 0, 0, bBytes)

	// Was this pair already flagged? The 0x0A marker is the durable record that
	// this contradiction is ALREADY KNOWN, and it is what makes the confidence
	// penalty idempotent: one declared contradiction is one fact, however many
	// times a worker re-observes the edge. Re-penalising compounds a single
	// Bayesian update (1.0 -> 0.975 -> 0.797 -> 0.313 -> 0.0709) until BOTH
	// memories — including the true one — fall out of recall entirely.
	//
	// FAILS CLOSED (#804), and that is the opposite call from the STO-12
	// endpoint-liveness guard (#803), deliberately. Both are read faults; the
	// loss directions are mirror images:
	//
	//   #803  refusing an association write on a read fault DESTROYS live data
	//         (a real edge that can never be recovered), while proceeding costs
	//         one repairable dangling row. So it fails OPEN, loudly.
	//   here  proceeding on a read fault reports the pair NEWLY flagged and
	//         restamps detectedAt with `now`. The first drives a repeat
	//         confidence penalty on BOTH engrams, and BayesianUpdate is not
	//         invertible (COG-23 residual) — unrecoverable. Refusing costs at
	//         most one deferred penalty, which the next detection re-applies.
	//
	// So the read error propagates and NOTHING is written. The only production
	// caller is ContradictWorker.processBatch, which already treats an error as
	// "newness unknown, do not penalise" and continues — so this cannot fail a
	// recall or a user-facing path on an otherwise-harmless corrupt block. The
	// (false, err) return keeps that suppression true even for a caller that
	// ignores the error.
	newlyFlagged := true
	detectedAt := time.Now()
	prior, exists, rerr := ps.readContradictionMarker(contraKey)
	if rerr != nil {
		return false, fmt.Errorf("flag contradiction: %w", rerr)
	}
	if exists {
		newlyFlagged = false
		// Carry the prior stamp forward verbatim — INCLUDING a zero one from a
		// legacy marker. Re-stamping an already-known contradiction with "now"
		// would invent a detection time that is off by however long the marker
		// has existed, which is exactly the plausible-wrong-value failure this
		// change exists to remove.
		detectedAt = prior
	}

	batch.Set(contraKey, encodeContradictionValue(bBytes, detectedAt), nil)
	// Also write reverse for quick lookup
	batch.Set(contraKeyRev, encodeContradictionValue(aBytes, detectedAt), nil)

	if err := batch.Commit(pebble.NoSync); err != nil {
		return false, fmt.Errorf("commit batch: %w", err)
	}
	ps.replicateBatch(batch)

	return newlyFlagged, nil
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
	ps.replicateBatch(batch)
	return nil
}

// ContradictionRecord is one deduplicated contradiction between engrams A and
// B, in canonical (A < B) order.
//
// DetectedAt is when the detector flagged the pair; it is the zero time for
// legacy markers written before the timestamp existed, and callers must render
// that as "unknown" rather than as an instant.
//
// DeclaredAt is when an explicit "contradicts" association between the two was
// written. It is zero when the pair was found by the detector without an
// explicit link.
type ContradictionRecord struct {
	A          ULID
	B          ULID
	DetectedAt time.Time
	DeclaredAt time.Time
}

// GetContradictions returns all contradiction pairs in the vault by scanning the 0x0A prefix.
// Each pair (a, b) is stored twice (forward and reverse), so we deduplicate by canonical ordering.
func (ps *PebbleStore) GetContradictions(ctx context.Context, wsPrefix [8]byte) ([][2]ULID, error) {
	recs, err := ps.GetContradictionRecords(ctx, wsPrefix)
	if err != nil {
		return nil, err
	}
	pairs := make([][2]ULID, 0, len(recs))
	for _, r := range recs {
		pairs = append(pairs, [2]ULID{r.A, r.B})
	}
	if len(pairs) == 0 {
		return nil, nil
	}
	return pairs, nil
}

// HasContradictionMarkers reports whether the vault has ANY 0x0A contradiction
// marker, using a single bounded iterator seek rather than the full scan
// GetContradictionRecords pays.
//
// It exists for the recall hot path: COG-29's contradiction-honesty phase must
// be free on the overwhelming majority of vaults, which have no contradictions
// at all, and a per-query prefix scan would not be. A true answer only means
// "run the phase"; the phase itself decides what is actually unresolved.
func (ps *PebbleStore) HasContradictionMarkers(ctx context.Context, wsPrefix [8]byte) (bool, error) {
	// keys.PrefixUpperBound, never a naive last-byte increment: the prefix ends
	// in the vault's 8th SipHash byte, and ~1/256 vaults hash to a prefix ending
	// in 0xFF, where `upper[last]++` wraps to 0x00 and yields an upper bound
	// BELOW the lower bound. The scan then returns empty — silently, forever —
	// which would disable COG-29 contradiction honesty for those vaults.
	lower := keys.ContradictionKeyPrefix(wsPrefix)
	upper := keys.PrefixUpperBound(lower)

	iter, err := ps.db.NewIter(&pebble.IterOptions{LowerBound: lower, UpperBound: upper})
	if err != nil {
		return false, err
	}
	defer iter.Close()
	found := iter.First()
	if err := iter.Error(); err != nil {
		return false, fmt.Errorf("HasContradictionMarkers: %w", err)
	}
	return found, nil
}

// GetContradictionRecords returns every flagged contradiction in the vault by
// scanning the 0x0A prefix, carrying each marker's detection time.
// The key structure is: 0x0A | wsPrefix(8) | conceptHash(4) | relType(2) | id(16) = 31 bytes.
// The value is partner ULID(16) | detectedAtUnixNano(8) — 16 bytes for legacy markers.
func (ps *PebbleStore) GetContradictionRecords(ctx context.Context, wsPrefix [8]byte) ([]ContradictionRecord, error) {
	// keys.PrefixUpperBound — see HasContradictionMarkers: a naive last-byte
	// increment wraps for the ~1/256 vaults whose prefix ends in 0xFF and makes
	// this scan silently return nothing.
	lower := keys.ContradictionKeyPrefix(wsPrefix)
	upper := keys.PrefixUpperBound(lower)

	iter, err := ps.db.NewIter(&pebble.IterOptions{LowerBound: lower, UpperBound: upper})
	if err != nil {
		return nil, err
	}
	defer iter.Close()

	// Key layout: 0x0A(1) | ws(8) | conceptHash(4) | relType(2) | id(16) = 31 bytes total
	const keyLen = 1 + 8 + 4 + 2 + 16
	const idOffset = 1 + 8 + 4 + 2 // offset where the 16-byte id starts

	seen := make(map[[32]byte]int)
	var recs []ContradictionRecord
	for valid := iter.First(); valid; valid = iter.Next() {
		k := iter.Key()
		if len(k) < keyLen {
			continue
		}
		b, detectedAt, ok := decodeContradictionValue(iter.Value())
		if !ok {
			continue
		}
		var a ULID
		copy(a[:], k[idOffset:idOffset+16])

		// Canonicalize: always put smaller first to deduplicate
		if CompareULIDs(a, b) > 0 {
			a, b = b, a
		}
		var dedupeKey [32]byte
		copy(dedupeKey[:16], a[:])
		copy(dedupeKey[16:], b[:])
		if idx, dup := seen[dedupeKey]; dup {
			// The forward and reverse markers are written together with the same
			// stamp; if one direction predates the timestamp, prefer the known one.
			if recs[idx].DetectedAt.IsZero() && !detectedAt.IsZero() {
				recs[idx].DetectedAt = detectedAt
			}
			continue
		}
		seen[dedupeKey] = len(recs)
		recs = append(recs, ContradictionRecord{A: a, B: b, DetectedAt: detectedAt})
	}
	if err := iter.Error(); err != nil {
		return nil, fmt.Errorf("GetContradictionRecords scan: %w", err)
	}
	return recs, nil
}

// DeclaredContradictionScan is the result of looking for explicit "contradicts"
// associations. Complete reports whether the whole association keyspace was
// examined: when it is false the caller MUST NOT claim there are no pending
// contradictions, only that none were found in what it scanned.
type DeclaredContradictionScan struct {
	Records  []ContradictionRecord
	Scanned  int
	Complete bool
}

// DefaultDeclaredContradictionScanCap bounds the forward-association scan that
// finds declared-but-undetected contradictions. Association values carry the
// relation type, so there is no prefix that isolates "contradicts" edges — the
// scan is O(edges) and has to be capped. The cap is generous because the scan
// is value-decode-only (no point lookups, no engram reads) and this surface is
// a deliberate, low-frequency call, not part of recall.
const DefaultDeclaredContradictionScanCap = 500_000

// DeclaredContradictions returns pairs joined by an explicit "contradicts"
// association, deduplicated in canonical order, keeping the EARLIEST
// declaration time when both directions were linked.
//
// This exists because an explicit link is durable the moment Link returns while
// the 0x0A marker is written by a batch worker up to 30s later. Without this
// scan the read surface reported an empty list in that window, which is
// indistinguishable from "this vault has no contradictions" — reporting an
// unknown state as a known one.
//
// maxScan <= 0 uses DefaultDeclaredContradictionScanCap.
func (ps *PebbleStore) DeclaredContradictions(ctx context.Context, wsPrefix [8]byte, maxScan int) (DeclaredContradictionScan, error) {
	if maxScan <= 0 {
		maxScan = DefaultDeclaredContradictionScanCap
	}
	var out DeclaredContradictionScan

	lower := make([]byte, 9)
	lower[0] = prefix.AssocFwd
	copy(lower[1:9], wsPrefix[:])
	// keys.PrefixUpperBound — see HasContradictionMarkers: `upper[8]++` wraps
	// for the ~1/256 vaults whose 8-byte prefix ends in 0xFF.
	upper := keys.PrefixUpperBound(lower)

	iter, err := ps.db.NewIter(&pebble.IterOptions{LowerBound: lower, UpperBound: upper})
	if err != nil {
		return out, err
	}
	defer iter.Close()

	// Forward assoc key: 0x03(1) | ws(8) | src(16) | weightComplement(4) | dst(16)
	const assocKeyLen = 1 + 8 + 16 + 4 + 16
	seen := make(map[[32]byte]int)

	out.Complete = true
	for valid := iter.First(); valid; valid = iter.Next() {
		if out.Scanned >= maxScan {
			out.Complete = false
			break
		}
		out.Scanned++
		k := iter.Key()
		if len(k) < assocKeyLen {
			continue
		}
		relType, _, createdAt, _, _, _, _ := decodeAssocValue(iter.Value())
		if relType != RelContradicts {
			continue
		}
		var a, b ULID
		copy(a[:], k[9:25])
		copy(b[:], k[29:45])
		if CompareULIDs(a, b) > 0 {
			a, b = b, a
		}
		var dedupeKey [32]byte
		copy(dedupeKey[:16], a[:])
		copy(dedupeKey[16:], b[:])
		if idx, dup := seen[dedupeKey]; dup {
			if !createdAt.IsZero() && (out.Records[idx].DeclaredAt.IsZero() || createdAt.Before(out.Records[idx].DeclaredAt)) {
				out.Records[idx].DeclaredAt = createdAt
			}
			continue
		}
		seen[dedupeKey] = len(out.Records)
		out.Records = append(out.Records, ContradictionRecord{A: a, B: b, DeclaredAt: createdAt})
	}
	if err := iter.Error(); err != nil {
		return out, fmt.Errorf("DeclaredContradictions scan: %w", err)
	}
	return out, nil
}
