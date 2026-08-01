package storage

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"

	"github.com/cockroachdb/pebble"
	"github.com/scrypster/muninndb/internal/prefix"
	"github.com/scrypster/muninndb/internal/storage/keys"
)

// legacyFullWeightComplement is the 4 complement bytes a PRE-FIX weight-1.0
// association key was written at.
//
// The original WeightComplement computed uint32(weight * float32(MaxUint32)).
// float32(MaxUint32) rounds UP to 2^32, so weight exactly 1.0 landed on 2^32
// and the out-of-range uint32 conversion produced 0 (on arm64); the complement
// MaxUint32-0 is 0xFFFFFFFF — byte-for-byte the position of weight 0.0.
//
// HARDCODED deliberately, not derived from keys.WeightComplement(0). This
// constant names a fact about bytes already on disk. Deriving it from the code
// under repair would make the scan silently follow any future encoder change
// away from the keys it exists to find.
var legacyFullWeightComplement = [4]byte{0xFF, 0xFF, 0xFF, 0xFF}

// assocWeightRepairChunkSize bounds how many damaged pairs are relocated per
// Pebble batch. Each pair's fwd write, rev write and both legacy deletes ride
// the SAME batch, so a kill mid-pass can never leave a pair half-moved; the
// chunk boundary only decides how many whole pairs commit together.
const assocWeightRepairChunkSize = 1_000

// GetAssocWeightRepairMark reads the per-vault full-weight repair watermark
// (0x2E). Returns 0 if the vault has never completed a clean repair pass.
func (ps *PebbleStore) GetAssocWeightRepairMark(ctx context.Context, ws [8]byte) (uint8, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	val, closer, err := ps.db.Get(keys.AssocWeightRepairMarkKey(ws))
	if err != nil {
		if errors.Is(err, pebble.ErrNotFound) {
			return 0, nil
		}
		return 0, fmt.Errorf("get assoc weight repair mark: %w", err)
	}
	defer closer.Close()
	if len(val) == 0 {
		return 0, nil
	}
	return val[0], nil
}

// SetAssocWeightRepairMark records that a repair pass at the given version
// completed cleanly over the vault, so later boots skip the full 0x03 scan.
func (ps *PebbleStore) SetAssocWeightRepairMark(ctx context.Context, ws [8]byte, version uint8) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return ps.db.Set(keys.AssocWeightRepairMarkKey(ws), []byte{version}, pebble.NoSync)
}

// RepairLegacyFullWeightAssocKeys relocates pre-fix weight-1.0 association keys
// from the weight-0.0 key position they were written at to the true 1.0
// position, and reports how many pairs it moved.
//
// # What is being repaired
//
// A pre-fix full-confidence edge (decide's evidence links, an explicit 1.0
// link, LTP saturation) sits at complement 0xFFFFFFFF and therefore READS as
// weight 0. Its 0x14 weight index, which always stored raw float bits and was
// never affected by the encoder bug, still says 1.0. That disagreement — key
// position 0.0, index exactly 1.0 — is the disambiguator, and it is what makes
// the repair unambiguous rather than a guess.
//
// # The rule
//
// For every 0x03 key at complement 0xFFFFFFFF, read the pair's 0x14 index:
//
//   - index == exactly 1.0 → pre-fix full-weight edge. Rewrite fwd (0x03) and
//     rev (0x04) at the 1.0 position carrying the value bytes VERBATIM, delete
//     the legacy-position keys. The 0x14 index is left untouched: it was always
//     right, and rewriting it would be the one way this pass could lose data.
//   - anything else → left alone. It is a genuine weight-0-ish edge, or an
//     orphan some other mechanism owns. The repair never guesses.
//
// # Repair window (this is why the pass runs early)
//
// A decay pass over an unrepaired vault destroys the disambiguator, per the
// #756 correction. Decay reads oldW=0 from the key and then either
//
//   - deletes the edge outright, index included (legacy 18-byte values, whose
//     peakWeight decodes as 0 → bootstrap peak 0 → dynamicFloor 0 → remove), or
//   - clamps it to peak*0.05, REWRITING 0x14 to 0.05 and moving the keys
//     (modern values, whose peakWeight is 1.0).
//
// After either, the edge is byte-indistinguishable from an honestly-earned
// 0.05 edge (or gone). Those edges are NOT counted as skipped or unrepairable
// below, because they cannot be identified at all — claiming a count for them
// would be inventing one.
//
// # Multi-key pairs
//
// WriteAssociation performs no cleanup on rewrite, so one pair can hold several
// 0x03 keys at different weights. The scan is therefore key-driven, not
// pair-driven, and never assumes a pair has exactly one key. Two consequences,
// both deliberate:
//
//   - If a pair ALREADY has a key at the true 1.0 position, that key wins: the
//     legacy key is deleted and the surviving key's value bytes are left alone.
//     Overwriting it with the legacy value would roll back whatever wrote the
//     correctly-placed one.
//   - Keys the pair holds at OTHER weights are not touched. They are pre-existing
//     duplicates from WriteAssociation's no-cleanup rewrite, not damage this bug
//     caused, and inventing a garbage collector for them here would let a repair
//     delete edges on evidence it never checked.
//
// # Atomicity, funding, replication
//
// The iterator's snapshot is fixed at creation, so chunked commits are safe:
// every legacy key is visited exactly once and the relocated keys this pass
// writes (complement 0x00000000, which sorts BEFORE the legacy position) are
// invisible to it. Work is bounded per batch by assocWeightRepairChunkSize and
// the whole pass is one-shot per vault behind the 0x2E watermark.
//
// Each chunk ships through replicateBatch like every other association write.
// A follower that already ran its own repair tolerates receiving the leader's
// batch: the fwd/rev Sets are byte-identical to what it already wrote, and the
// legacy-position Deletes are no-ops on keys it already removed. Replication is
// what heals a follower still running a PRE-repair binary during a rolling
// upgrade — the case a purely local backfill (the #681 evolve repair's choice)
// would leave broken.
func (ps *PebbleStore) RepairLegacyFullWeightAssocKeys(ctx context.Context, wsPrefix [8]byte) (int, error) {
	scanPrefix := make([]byte, 9)
	scanPrefix[0] = prefix.AssocFwd
	copy(scanPrefix[1:9], wsPrefix[:])

	iter, err := PrefixIterator(ps.db, scanPrefix)
	if err != nil {
		return 0, fmt.Errorf("assoc weight repair: prefix iterator: %w", err)
	}
	defer iter.Close()

	type damaged struct {
		src [16]byte
		dst [16]byte
		val []byte
	}

	repaired := 0
	chunk := make([]damaged, 0, assocWeightRepairChunkSize)

	flushChunk := func() error {
		if len(chunk) == 0 {
			return nil
		}
		batch := ps.db.NewBatch()
		defer batch.Close()
		for _, d := range chunk {
			fwdCorrect := keys.AssocFwdKey(wsPrefix, d.src, 1.0, d.dst)
			revCorrect := keys.AssocRevKey(wsPrefix, d.dst, 1.0, d.src)
			// Only fill the true position if it is empty. A key already there
			// was written by the post-fix encoder and is newer than this one.
			if exists, err := ps.keyExists(fwdCorrect); err != nil {
				return err
			} else if !exists {
				_ = batch.Set(fwdCorrect, d.val, nil)
			}
			if exists, err := ps.keyExists(revCorrect); err != nil {
				return err
			} else if !exists {
				_ = batch.Set(revCorrect, d.val, nil)
			}
			// Legacy position goes, in the same batch, either way.
			_ = batch.Delete(legacyFullWeightFwdKey(wsPrefix, d.src, d.dst), nil)
			_ = batch.Delete(legacyFullWeightRevKey(wsPrefix, d.dst, d.src), nil)
		}
		if err := batch.Commit(pebble.NoSync); err != nil {
			return fmt.Errorf("assoc weight repair: chunk commit: %w", err)
		}
		ps.replicateBatch(batch)
		for _, d := range chunk {
			ps.assocCache.Remove(assocCacheKey(wsPrefix, ULID(d.src)))
		}
		chunk = chunk[:0]
		return nil
	}

	for iter.First(); iter.Valid(); iter.Next() {
		if err := ctx.Err(); err != nil {
			return repaired, err
		}
		key := iter.Key()
		if len(key) < 45 {
			continue
		}
		if !bytes.Equal(key[25:29], legacyFullWeightComplement[:]) {
			continue
		}
		var src, dst [16]byte
		copy(src[:], key[9:25])
		copy(dst[:], key[29:45])

		// The 0x14 index is the disambiguator. Exactly 1.0 and nothing else:
		// a genuinely-decayed edge's index reads its decayed value, and a
		// near-1.0 index is a decayed edge, not a pre-fix one.
		indexed, err := ps.rawAssocWeightIndex(wsPrefix, src, dst)
		if err != nil {
			return repaired, err
		}
		if indexed != 1.0 {
			continue
		}

		chunk = append(chunk, damaged{src: src, dst: dst, val: append([]byte(nil), iter.Value()...)})
		repaired++
		if len(chunk) >= assocWeightRepairChunkSize {
			if err := flushChunk(); err != nil {
				return repaired, err
			}
		}
	}
	if err := iter.Error(); err != nil {
		return repaired, fmt.Errorf("assoc weight repair: scan: %w", err)
	}
	if err := flushChunk(); err != nil {
		return repaired, err
	}
	return repaired, nil
}

// legacyFullWeightFwdKey builds the 0x03 key a pre-fix weight-1.0 edge was
// written at, from the hardcoded legacy complement rather than the encoder.
func legacyFullWeightFwdKey(ws [8]byte, src, dst [16]byte) []byte {
	key := make([]byte, 1+8+16+4+16)
	key[0] = prefix.AssocFwd
	copy(key[1:9], ws[:])
	copy(key[9:25], src[:])
	copy(key[25:29], legacyFullWeightComplement[:])
	copy(key[29:45], dst[:])
	return key
}

// legacyFullWeightRevKey builds the matching 0x04 key.
func legacyFullWeightRevKey(ws [8]byte, dst, src [16]byte) []byte {
	key := make([]byte, 1+8+16+4+16)
	key[0] = prefix.AssocRev
	copy(key[1:9], ws[:])
	copy(key[9:25], dst[:])
	copy(key[25:29], legacyFullWeightComplement[:])
	copy(key[29:45], src[:])
	return key
}

// rawAssocWeightIndex reads the 0x14 weight index for a pair, distinguishing a
// missing/short record (returns 0, the value GetAssocWeight also reports) from
// a read error, which is propagated rather than silently read as 0 — a swallowed
// error here would look exactly like "not a pre-fix edge" and skip real damage.
func (ps *PebbleStore) rawAssocWeightIndex(ws [8]byte, a, b [16]byte) (float32, error) {
	val, closer, err := ps.db.Get(keys.AssocWeightIndexKey(ws, a, b))
	if err != nil {
		if errors.Is(err, pebble.ErrNotFound) {
			return 0, nil
		}
		return 0, fmt.Errorf("assoc weight repair: read weight index: %w", err)
	}
	defer closer.Close()
	if len(val) < 4 {
		return 0, nil
	}
	return math.Float32frombits(binary.BigEndian.Uint32(val[:4])), nil
}

// keyExists reports whether a key is present, propagating real read errors.
func (ps *PebbleStore) keyExists(key []byte) (bool, error) {
	_, closer, err := ps.db.Get(key)
	if err != nil {
		if errors.Is(err, pebble.ErrNotFound) {
			return false, nil
		}
		return false, fmt.Errorf("assoc weight repair: probe key: %w", err)
	}
	_ = closer.Close()
	return true, nil
}
