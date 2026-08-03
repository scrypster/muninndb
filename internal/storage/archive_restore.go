package storage

import (
	"bytes"
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

	// STO-12: if the SOURCE engram is gone, nothing under this prefix can ever
	// be restored. Reap the whole prefix rather than scanning it on every
	// recall forever. (DeleteEngram now cascades 0x25, so this only fires for
	// archive rows stranded by a pre-fix hard delete.)
	if !ps.engramExists(ws, srcID) {
		return nil, ps.reapArchivedEdgesFrom(ws, srcID)
	}

	prefix := keys.ArchiveAssocPrefixForID(ws, srcID)

	// STO-11. This bound is DELIBERATELY TIGHT and stays hand-rolled.
	//
	// History, because the comment would otherwise read as superstition: until
	// #816, keys.PrefixUpperBound incremented the first sub-0xFF byte from the
	// right and returned WITHOUT clearing the trailing 0xFF bytes, so for a
	// 25-byte 0x25|ws|src prefix whose last byte was 0xFF it admitted keys
	// belonging to the NEXT source id. The loop below increments from the right
	// and ZEROES every byte it wraps, which is why it was correct when the
	// shared helper was not. #816 made the helper carry-and-truncate, so the two
	// now agree (the helper is marginally tighter — it drops the zeroed tail
	// rather than writing it).
	//
	// It stays hand-rolled anyway. This loop has NO per-key prefix check, so its
	// bound is its ONLY protection, and it is the one scan here that both
	// deletes and MINTS. A bound whose correctness is visible in the ten lines
	// above the loop it protects is worth more here than consistency with the
	// four guarded call sites — those degrade to an extra comparison if a helper
	// regresses; this one fabricates edges.
	//
	// This is the FIFTH scan over the 0x25|ws|src prefix and the only one held
	// inside its keyspace by its bound alone: the candidate loop below has no
	// bytes.Equal(k[:25], prefix) break, unlike the four sites listed in the
	// STO-11 census. A loose bound here does not merely over-delete — the restore
	// loop MINTS live 0x03/0x04/0x14 rows, so it would fabricate a victim →
	// neighbour's-dst edge that never existed and stamp restoredAt on it, which
	// permanently exempts it from GCArchivedEdges. The engramExists(ws, c.dst)
	// guard below does NOT catch that: the neighbour's dst is a real live engram.
	// The liveness predicate and the bound are orthogonal.
	//
	// Pinned by TestSTO11_RestoreArchivedEdgesCandidateScanStaysInsideItsOwnPrefix.
	//
	// A prefix check is not folded into the len(k) < 41 || len(v) < 26 skip
	// below on purpose: len(v) < 26 is a value-shaped condition that must stay a
	// `continue`, while a prefix mismatch must `break` — sharing one branch would
	// walk the whole widened band on every recall.
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
		// STO-12: never restore an edge whose target engram was hard-deleted.
		// Restoring it recreates a dangling 0x03/0x04/0x14 row AND stamps
		// restoredAt, which permanently exempts the row from GCArchivedEdges
		// (it requires restoredAt == 0) — so this path does not merely leak,
		// it makes the leak un-collectable. Drop the archive row instead: with
		// its target's 0x01 record gone the edge can never become valid again.
		if !ps.engramExists(ws, c.dst) {
			_ = batch.Delete(keys.ArchiveAssocKey(ws, srcID, c.dst), nil)
			continue
		}

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
	ps.replicateBatch(batch)

	// Invalidate assocCache for src and all restored dst nodes.
	ps.assocCache.Remove(assocCacheKey(ws, ULID(srcID)))
	ps.revAssocCache.Remove(assocCacheKey(ws, ULID(srcID)))
	for _, dst := range restoredDsts {
		ps.assocCache.Remove(assocCacheKey(ws, ULID(dst)))
		ps.revAssocCache.Remove(assocCacheKey(ws, ULID(dst)))
	}

	return restoredDsts, nil
}

// reapArchivedEdgesFrom deletes every 0x25 archive row sourced from srcID.
// Used when srcID has no 0x01 record: none of those edges can ever be restored.
//
// # Replication (obligation 11): a new DESTRUCTIVE write on the recall path
//
// RestoreArchivedEdges has always replicated its restore batch, so this is not
// a new replicated write on the read path — but it is the first DESTRUCTIVE
// one, and a follower reaches it independently of the leader. On a follower,
// recall locally deletes 0x25 rows the leader may still hold, until the leader
// recalls the same source and ships the same deletion.
//
// Judged not a correctness problem, on two grounds. First, the rows are
// PROVABLY unrestorable: RestoreArchivedEdges is the only reader of a 0x25
// prefix, it requires the source's 0x01 record, and that record is gone — so
// the divergence is between "absent" and "present but unreadable by anything",
// which no query can distinguish. Second, it is self-limiting: one commit per
// source, after which the prefix is empty and every later call finds n == 0 and
// commits nothing. It cannot oscillate and it cannot amplify.
//
// What it is NOT is a claim about the replication model in general. If a
// follower's local deletes are supposed to be impossible rather than merely
// harmless, this is the call site to change, and the fix is to gate the reap on
// leadership and let followers skip (paying the rescan) rather than to make the
// reap conditional on some weaker signal. Stated here so that review is a
// question about one function, not an archaeology exercise.
func (ps *PebbleStore) reapArchivedEdgesFrom(ws [8]byte, srcID [16]byte) error {
	prefix := keys.ArchiveAssocPrefixForID(ws, srcID)
	iter, err := ps.db.NewIter(&pebble.IterOptions{
		LowerBound: prefix,
		UpperBound: keys.PrefixUpperBound(prefix),
	})
	if err != nil {
		return err
	}
	batch := ps.db.NewBatch()
	defer batch.Close()
	n := 0
	for valid := iter.First(); valid; valid = iter.Next() {
		k := iter.Key()
		// STO-11. keys.PrefixUpperBound used to be LOOSE — it incremented the
		// first sub-0xFF byte from the right and returned without clearing the
		// trailing 0xFF bytes, so for a 25-byte kind|ws|src prefix whose last
		// byte was 0xFF the bound admitted keys belonging to the NEXT source id.
		// #816 made it carry-and-truncate. This loop deletes what the iterator
		// returns, and it is reachable from the ordinary recall read path, so
		// the explicit prefix check STAYS as belt and braces: one comparison per
		// key, and it is what stops an ordinary recall of one hard-deleted
		// source from deleting a LIVE engram's archive rows no matter what a
		// helper in another package does next.
		//
		// Reachability of the old looseness, on the same terms as the other
		// three guarded sites: it was STRUCTURAL HYGIENE, never a live
		// data-loss report. ~1 in 256 was the rate at which the BOUND WAS LOOSE,
		// not the rate at which anything was lost — landing inside the widened
		// band additionally required a second source sharing the victim's first
		// 14 ID bytes (the full 48-bit ULID millisecond AND 8 of the 10 entropy
		// bytes), i.e. ~2^-64 on top of a same-millisecond collision. Any future
		// non-ULID id tail would collapse that 64-bit gap to zero.
		//
		// Pinned by TestSTO11_EveryDestructivePrefixScanStaysInsideItsOwnPrefix.
		//
		// break, not continue: the iterator starts at exactly this prefix and
		// returns keys in order, so the first key that does not carry it is
		// already past it — a key shorter than 25 bytes can only sort at or
		// above the prefix by differing (greater) within its own length.
		if len(k) < 25 || !bytes.Equal(k[:25], prefix) {
			break
		}
		_ = batch.Delete(append([]byte{}, k...), nil)
		n++
	}
	_ = iter.Close()
	if n == 0 {
		return nil
	}
	if err := batch.Commit(pebble.NoSync); err != nil {
		return err
	}
	ps.replicateBatch(batch)
	ps.assocCache.Remove(assocCacheKey(ws, ULID(srcID)))
	return nil
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
