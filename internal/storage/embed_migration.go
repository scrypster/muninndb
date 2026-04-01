package storage

import (
	"context"
	"errors"
	"fmt"

	"github.com/cockroachdb/pebble"
	"github.com/scrypster/muninndb/internal/storage/keys"
)

// ClearEmbedFlagsForVault clears the DigestEmbed (0x02) and DigestEmbedFailed
// (0x80) flags on every engram's 0x11 digest record within the given vault, and
// range-deletes all 0x18 (embedding) keys for the vault. This causes the
// RetroactiveProcessor to re-embed every engram on its next scan cycle,
// including engrams that previously failed to embed.
//
// Engrams that have no existing digest record are written a zero record so they
// are explicitly tracked and eligible for re-embedding.
//
// Returns the number of digest records that were written (created or updated).
func (ps *PebbleStore) ClearEmbedFlagsForVault(ctx context.Context, ws [8]byte) (int64, error) {
	const DigestEmbed uint8 = 0x02
	const DigestEmbedFailed uint8 = 0x80
	const embedMask uint8 = DigestEmbed | DigestEmbedFailed

	wsPlus, err := keys.IncrementWSPrefix(ws)
	if err != nil {
		return 0, fmt.Errorf("clear embed flags: increment ws: %w", err)
	}

	// Step 1: Range-delete all 0x18 embedding keys for this vault.
	lo := make([]byte, 9)
	lo[0] = 0x18
	copy(lo[1:], ws[:])
	hi := make([]byte, 9)
	hi[0] = 0x18
	copy(hi[1:], wsPlus[:])

	if err := ps.db.DeleteRange(lo, hi, pebble.Sync); err != nil {
		return 0, fmt.Errorf("clear embed flags: delete embedding keys: %w", err)
	}

	// Step 2: Scan all 0x01 (engram) keys for this vault. For each, read the 0x11
	// digest flag, clear bit 0x02, and write back. Skip engrams where the bit is
	// already cleared.
	engramLo := make([]byte, 9)
	engramLo[0] = 0x01
	copy(engramLo[1:], ws[:])
	engramHi := make([]byte, 9)
	engramHi[0] = 0x01
	copy(engramHi[1:], wsPlus[:])

	iter, err := ps.db.NewIter(&pebble.IterOptions{
		LowerBound: engramLo,
		UpperBound: engramHi,
	})
	if err != nil {
		return 0, fmt.Errorf("clear embed flags: create iter: %w", err)
	}
	defer iter.Close()

	var cleared int64
	batch := ps.db.NewBatch()
	defer batch.Close()

	for valid := iter.First(); valid; valid = iter.Next() {
		if ctx.Err() != nil {
			return cleared, ctx.Err()
		}

		k := iter.Key()
		if len(k) < 25 {
			continue
		}
		var id [16]byte
		copy(id[:], k[9:25])

		raw, err := ps.getDigestFlagsRaw(id)
		noRecord := errors.Is(err, pebble.ErrNotFound)
		if err != nil && !noRecord {
			return cleared, fmt.Errorf("clear embed flags: get digest: %w", err)
		}
		if noRecord {
			raw = 0
		}

		// If an existing record already has both flags clear, there is nothing to
		// do — skip it. But if there is NO record at all (noRecord), we must fall
		// through and write the zero record so the RetroactiveProcessor explicitly
		// sees this engram as pending embedding (fixes the silent skip introduced
		// by the Bug 3 guard: raw=0 caused raw&embedMask==0 to be true, meaning
		// the zero write was never reached for freshly imported engrams).
		if !noRecord && raw&embedMask == 0 {
			continue
		}

		raw &^= embedMask
		flagKey := keys.DigestFlagsKey(id)
		if err := batch.Set(flagKey, []byte{raw}, nil); err != nil {
			return cleared, fmt.Errorf("clear embed flags: batch set: %w", err)
		}
		cleared++

		// Flush in micro-batches to avoid unbounded memory use.
		if cleared%1000 == 0 {
			if err := batch.Commit(pebble.NoSync); err != nil {
				return cleared, fmt.Errorf("clear embed flags: commit batch: %w", err)
			}
			batch.Close()
			batch = ps.db.NewBatch()
		}
	}

	if err := iter.Error(); err != nil {
		return cleared, fmt.Errorf("clear embed flags: iter error: %w", err)
	}

	// Final flush.
	if err := batch.Commit(pebble.Sync); err != nil {
		return cleared, fmt.Errorf("clear embed flags: final commit: %w", err)
	}

	return cleared, nil
}

// ClearHNSWForVault range-deletes all 0x07 (HNSW node) keys for the given vault.
func (ps *PebbleStore) ClearHNSWForVault(ws [8]byte) error {
	wsPlus, err := keys.IncrementWSPrefix(ws)
	if err != nil {
		return fmt.Errorf("clear hnsw: increment ws: %w", err)
	}

	lo := make([]byte, 9)
	lo[0] = 0x07
	copy(lo[1:], ws[:])
	hi := make([]byte, 9)
	hi[0] = 0x07
	copy(hi[1:], wsPlus[:])

	if err := ps.db.DeleteRange(lo, hi, pebble.Sync); err != nil {
		return fmt.Errorf("clear hnsw: delete range: %w", err)
	}
	return nil
}

// GetEmbedModel reads the vault-level embed model marker (0x1D key).
// Returns empty string if not set.
func (ps *PebbleStore) GetEmbedModel(ws [8]byte) (string, error) {
	key := keys.EmbedModelKey(ws)
	val, closer, err := ps.db.Get(key)
	if err == pebble.ErrNotFound {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("get embed model: %w", err)
	}
	defer closer.Close()
	return string(val), nil
}

// SetEmbedModel writes the vault-level embed model marker (0x1D key).
// Pass empty string to clear it.
func (ps *PebbleStore) SetEmbedModel(ws [8]byte, model string) error {
	key := keys.EmbedModelKey(ws)
	if model == "" {
		return ps.db.Delete(key, pebble.Sync)
	}
	return ps.db.Set(key, []byte(model), pebble.Sync)
}
