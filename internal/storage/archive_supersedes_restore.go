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

// GetSupersedesRestoreMark reads the per-vault watermark (0x31) of the
// one-time restore of archived supersedes edges. Returns 0 if the restore has
// never completed over the vault.
func (ps *PebbleStore) GetSupersedesRestoreMark(ctx context.Context, ws [8]byte) (uint8, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	v, err := Get(ps.db, keys.SupersedesRestoreMarkKey(ws))
	if err != nil {
		return 0, fmt.Errorf("read supersedes restore mark: %w", err)
	}
	if len(v) == 0 {
		return 0, nil
	}
	return v[0], nil
}

// SetSupersedesRestoreMark records that the restore at the given version
// completed over the vault. Local to the node that ran it, like the 0x2E
// mark: only the leader runs the restore, and a node that becomes leader
// later runs it over its own copy, which its replicated batches have kept
// equal to the leader's.
func (ps *PebbleStore) SetSupersedesRestoreMark(ctx context.Context, ws [8]byte, version uint8) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return ps.db.Set(keys.SupersedesRestoreMarkKey(ws), []byte{version}, pebble.NoSync)
}

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
	// SupersedesAlreadyLive: the same source holds a live supersedes edge on
	// the target already (it was linked again after the archive), so the
	// declaration is in effect. The row is removed and nothing is written:
	// restoring it would put a second live key for one declaration. holders
	// names any other successor the target has (an existing fork, reported,
	// never resolved here).
	SupersedesAlreadyLive
)

// RestoreArchivedSupersedes puts one archived supersedes edge back, at the
// weight it was declared with (its peak), when nothing else has taken its
// target since. When the same source is live on the target already, the row
// is removed and nothing is written (SupersedesAlreadyLive). Otherwise it
// changes nothing and says why; holders names the
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
	alreadyLive := false
	for _, s := range succ {
		if s == src {
			alreadyLive = true
		} else {
			holders = append(holders, s)
		}
	}
	if alreadyLive {
		batch := ps.db.NewBatch()
		defer batch.Close()
		_ = batch.Delete(archKey, nil)
		testAssocGuardPoint("supersedes-restore", ws, src, dst)
		if err := ps.commitBatch(batch, pebble.NoSync); err != nil {
			return 0, nil, fmt.Errorf("drop archived supersedes row: %w", err)
		}
		ps.replicateBatch(batch)
		return SupersedesAlreadyLive, holders, nil
	}
	if len(holders) > 0 {
		return SupersedesLeftTargetHeld, holders, nil
	}
	w := peak
	if checkRelTypeCollision(ps.db, ws, [16]byte(src), w, [16]byte(dst), RelSupersedes) != nil {
		return SupersedesLeftCollision, nil, nil
	}

	live := encodeArchiveValue(RelSupersedes, confidence, createdAt, lastActivated, peak, coAct, int32(time.Now().Unix()))
	testAssocGuardPoint("supersedes-restore", ws, src, dst)
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
