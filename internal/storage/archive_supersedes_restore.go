package storage

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"time"

	"github.com/cockroachdb/pebble"

	"github.com/scrypster/muninndb/internal/storage/keys"
)

// ArchivedSupersedesEdge is a supersedes edge an older binary's decay pass
// moved to the 0x25 archive: Src declared that it supersedes Dst.
type ArchivedSupersedesEdge struct {
	Src, Dst ULID
}

// ArchivedSupersedes lists the supersedes rows in a vault's 0x25 archive.
// Decay no longer archives supersedes edges, so these are rows an older binary
// left; the startup association repair restores them one by one with
// RestoreArchivedSupersedes.
func (ps *PebbleStore) ArchivedSupersedes(ctx context.Context, ws [8]byte) ([]ArchivedSupersedesEdge, error) {
	lower := keys.ArchiveAssocRangeStart(ws)
	upper := keys.ArchiveAssocRangeEnd(ws)
	iter, err := ps.db.NewIter(&pebble.IterOptions{LowerBound: lower, UpperBound: upper})
	if err != nil {
		return nil, fmt.Errorf("archived supersedes iterator: %w", err)
	}
	defer iter.Close()
	var out []ArchivedSupersedesEdge
	for iter.First(); iter.Valid(); iter.Next() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		k := iter.Key()
		// 0x25 | ws(8) | src(16) | dst(16). STO-11: the bound is the vault's
		// range, and the prefix is checked per key as well.
		if len(k) != 41 || !bytes.Equal(k[1:9], ws[:]) {
			continue
		}
		rel, _, _, _, _, _, _ := decodeAssocValue(iter.Value())
		if rel != RelSupersedes {
			continue
		}
		var e ArchivedSupersedesEdge
		copy(e.Src[:], k[9:25])
		copy(e.Dst[:], k[25:41])
		out = append(out, e)
	}
	if err := iter.Error(); err != nil {
		return nil, fmt.Errorf("archived supersedes scan: %w", err)
	}
	return out, nil
}

// SupersedesRestoreOutcome is what RestoreArchivedSupersedes did with one row.
type SupersedesRestoreOutcome int

const (
	// SupersedesRestored: the edge is live again and the row is gone.
	SupersedesRestored SupersedesRestoreOutcome = iota
	// SupersedesLeftTargetHeld: the target has another successor now. The
	// store holds two declarations for one memory; nothing was chosen, and
	// the row stays archived.
	SupersedesLeftTargetHeld
	// SupersedesLeftEndpointGone: the source or the target no longer exists.
	SupersedesLeftEndpointGone
	// SupersedesLeftCollision: a live edge of another relation holds the key
	// the edge would be restored at (STO-15). The row stays archived.
	SupersedesLeftCollision
	// SupersedesLeftNoRow: the row is no longer an archived supersedes row.
	SupersedesLeftNoRow
)

// RestoreArchivedSupersedes puts one archived supersedes edge back, at the
// weight it was declared with (its peak), when nothing else has taken its
// target since. Otherwise it changes nothing and says why; holders names the
// target's other successors when that is the reason. The caller holds the
// target's evolve lock, the lock every supersedes link of that target takes,
// so no rival can land between the check and the write. The edge goes back in
// one batch with the row's removal, and the batch is replicated as the lazy
// restore's is.
func (ps *PebbleStore) RestoreArchivedSupersedes(ctx context.Context, ws [8]byte, src, dst ULID) (outcome SupersedesRestoreOutcome, holders []ULID, err error) {
	mu := ps.assocGuardLocks.For(assocPairLockKey(ws, src, dst))
	mu.Lock()
	defer mu.Unlock()

	archKey := keys.ArchiveAssocKey(ws, [16]byte(src), [16]byte(dst))
	v, err := Get(ps.db, archKey)
	if err != nil {
		return 0, nil, fmt.Errorf("read archived supersedes row: %w", err)
	}
	if v == nil {
		return SupersedesLeftNoRow, nil, nil
	}
	rel, confidence, createdAt, lastActivated, peak, coAct, _ := decodeAssocValue(v)
	if rel != RelSupersedes {
		return SupersedesLeftNoRow, nil, nil
	}
	if !ps.engramExists(ws, [16]byte(src)) || !ps.engramExists(ws, [16]byte(dst)) {
		return SupersedesLeftEndpointGone, nil, nil
	}
	succ, err := ps.SupersedersOf(ctx, ws, dst)
	if err != nil {
		return 0, nil, fmt.Errorf("read successors of %s: %w", dst.String(), err)
	}
	for _, s := range succ {
		if s != src {
			holders = append(holders, s)
		}
	}
	if len(holders) > 0 {
		return SupersedesLeftTargetHeld, holders, nil
	}
	w := peak
	if checkRelTypeCollision(ps.db, ws, [16]byte(src), w, [16]byte(dst), RelSupersedes) != nil {
		return SupersedesLeftCollision, nil, nil
	}

	live := encodeArchiveValue(RelSupersedes, confidence, createdAt, lastActivated, peak, coAct, int32(time.Now().Unix()))
	batch := ps.db.NewBatch()
	defer batch.Close()
	_ = batch.Set(keys.AssocFwdKey(ws, [16]byte(src), w, [16]byte(dst)), live[:], nil)
	_ = batch.Set(keys.AssocRevKey(ws, [16]byte(dst), w, [16]byte(src)), live[:], nil)
	var wb [4]byte
	binary.BigEndian.PutUint32(wb[:], math.Float32bits(w))
	_ = batch.Set(keys.AssocWeightIndexKey(ws, [16]byte(src), [16]byte(dst)), wb[:], nil)
	_ = batch.Delete(archKey, nil)
	if err := ps.commitBatch(batch, pebble.NoSync); err != nil {
		return 0, nil, fmt.Errorf("restore archived supersedes edge: %w", err)
	}
	ps.replicateBatch(batch)
	ps.assocCache.Remove(assocCacheKey(ws, src))
	ps.revAssocCache.Remove(assocCacheKey(ws, src))
	ps.assocCache.Remove(assocCacheKey(ws, dst))
	ps.revAssocCache.Remove(assocCacheKey(ws, dst))
	return SupersedesRestored, nil, nil
}
