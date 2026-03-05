package storage

import (
	"context"
	"encoding/binary"
	"math"
	"sort"
	"time"

	"github.com/cockroachdb/pebble"

	"github.com/scrypster/muninndb/internal/storage/keys"
)

const restoreTopN = 10
const restoreWeightFactor float32 = 0.25

type restoredEdge struct {
	dst                [16]byte
	relType            RelType
	confidence         float32
	createdAt          time.Time
	lastActivated      int32
	peakWeight         float32
	coActivationCount  uint32
	restoreWeight      float32
	consolidationScore float64
}

// RestoreArchivedEdges scans the 0x25 archive prefix for archived edges from srcID,
// selects the top maxN by consolidation score, restores them to the live index
// at peakWeight * 0.25, stamps restoredAt = now on the live write, and removes
// them from the archive. Returns the restored dst IDs.
func (ps *PebbleStore) RestoreArchivedEdges(ctx context.Context, ws [8]byte, srcID [16]byte, maxN int) ([][16]byte, error) {
	if maxN <= 0 || maxN > restoreTopN {
		maxN = restoreTopN
	}

	prefix := keys.ArchiveAssocPrefixForID(ws, srcID)

	// Upper bound: increment the last byte of the prefix to bound the scan.
	upperBound := make([]byte, len(prefix))
	copy(upperBound, prefix)
	for i := len(upperBound) - 1; i >= 0; i-- {
		upperBound[i]++
		if upperBound[i] != 0 {
			break
		}
	}

	iterOpts := &pebble.IterOptions{
		LowerBound: prefix,
		UpperBound: upperBound,
	}
	iter, err := ps.db.NewIter(iterOpts)
	if err != nil {
		return nil, err
	}
	defer iter.Close()

	var candidates []restoredEdge
	for valid := iter.First(); valid; valid = iter.Next() {
		k := iter.Key()
		v := iter.Value()
		// Archive key: 0x25 | ws(8) | src(16) | dst(16) = 41 bytes
		if len(k) < 41 || len(v) < 26 {
			continue
		}

		var dstID [16]byte
		copy(dstID[:], k[25:41])

		relType, confidence, createdAt, lastActivated, peakWeight, coActivationCount, _ := decodeAssocValue(v)

		daysSince := time.Since(time.Unix(int64(lastActivated), 0)).Hours() / 24
		if daysSince < 1 {
			daysSince = 1
		}
		score := (float64(peakWeight) * float64(coActivationCount)) / daysSince

		restoreWeight := peakWeight * restoreWeightFactor

		candidates = append(candidates, restoredEdge{
			dst:                dstID,
			relType:            relType,
			confidence:         confidence,
			createdAt:          createdAt,
			lastActivated:      lastActivated,
			peakWeight:         peakWeight,
			coActivationCount:  coActivationCount,
			restoreWeight:      restoreWeight,
			consolidationScore: score,
		})
	}
	if err := iter.Error(); err != nil {
		return nil, err
	}

	// Sort by consolidation score descending, take top maxN.
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].consolidationScore > candidates[j].consolidationScore
	})
	if maxN > 0 && len(candidates) > maxN {
		candidates = candidates[:maxN]
	}

	if len(candidates) == 0 {
		return nil, nil
	}

	now := int32(time.Now().Unix())

	batch := ps.db.NewBatch()
	defer batch.Close()

	var restoredDsts [][16]byte
	for _, c := range candidates {
		restoreW := c.restoreWeight

		// Encode the live value using the 30-byte archive format so that restoredAt is stamped.
		// decodeAssocValue handles both 26-byte and 30-byte values via a length check.
		liveVal := encodeArchiveValue(c.relType, c.confidence, c.createdAt, c.lastActivated, c.peakWeight, c.coActivationCount, now)

		// Write to 0x03 (forward key) — weight is embedded in the key.
		fwdKey := keys.AssocFwdKey(ws, srcID, restoreW, c.dst)
		_ = batch.Set(fwdKey, liveVal[:], nil)

		// Write to 0x04 (reverse key).
		revKey := keys.AssocRevKey(ws, c.dst, restoreW, srcID)
		_ = batch.Set(revKey, liveVal[:], nil)

		// Write to 0x14 (weight index) — stores the plain float32 weight for O(1) lookups.
		wKey := keys.AssocWeightIndexKey(ws, srcID, c.dst)
		var wBuf [4]byte
		binary.BigEndian.PutUint32(wBuf[:], math.Float32bits(restoreW))
		_ = batch.Set(wKey, wBuf[:], nil)

		// Delete from 0x25 archive.
		archKey := keys.ArchiveAssocKey(ws, srcID, c.dst)
		_ = batch.Delete(archKey, nil)

		restoredDsts = append(restoredDsts, c.dst)
	}

	if err := batch.Commit(pebble.NoSync); err != nil {
		return nil, err
	}

	// Invalidate assocCache for src and all restored dst nodes.
	ps.assocCache.Remove(assocCacheKey(ws, ULID(srcID)))
	for _, dst := range restoredDsts {
		ps.assocCache.Remove(assocCacheKey(ws, ULID(dst)))
	}

	return restoredDsts, nil
}

// RestoreArchivedEdgesTransitive restores archived edges for src (top-N),
// then for each directly restored neighbor, restores their top-M archived edges
// (depth-2 lazy transitive restore).
func (ps *PebbleStore) RestoreArchivedEdgesTransitive(ctx context.Context, wsPrefix [8]byte, src ULID, maxDirect int, maxTransitive int) ([]ULID, error) {
	directRestored, err := ps.RestoreArchivedEdges(ctx, wsPrefix, src, maxDirect)
	if err != nil {
		return nil, err
	}

	var allRestored []ULID
	for _, dst := range directRestored {
		allRestored = append(allRestored, ULID(dst))
	}

	// Depth-2: for each directly restored neighbor, restore their top-M.
	for _, neighbor := range directRestored {
		if !ps.archiveBloom.MayContain(neighbor) {
			continue
		}
		transitiveRestored, err := ps.RestoreArchivedEdges(ctx, wsPrefix, neighbor, maxTransitive)
		if err != nil {
			continue // best-effort for transitive restore
		}
		for _, dst := range transitiveRestored {
			allRestored = append(allRestored, ULID(dst))
		}
	}

	return allRestored, nil
}
