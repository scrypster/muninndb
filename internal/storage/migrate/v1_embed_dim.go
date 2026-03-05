package migrate

import (
	"fmt"
	"log/slog"

	"github.com/cockroachdb/pebble"
	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/storage/erf"
	"github.com/scrypster/muninndb/internal/storage/keys"
	"github.com/scrypster/muninndb/internal/types"
)

// BackfillEmbedDim scans all embedding keys (0x18 prefix), derives the vector
// dimension from the stored value length, and patches EmbedDim in the
// corresponding ERF record (0x01) and meta key (0x02).
//
// It is idempotent: records that already have EmbedDim != 0 are skipped.
func BackfillEmbedDim(db *pebble.DB) error {
	iter, err := db.NewIter(&pebble.IterOptions{
		LowerBound: []byte{0x18},
		UpperBound: []byte{0x19},
	})
	if err != nil {
		return fmt.Errorf("backfill embed dim: new iter: %w", err)
	}
	defer iter.Close()

	const batchSize = 100

	patched, skipped := 0, 0

	batch := db.NewBatch()
	batchCount := 0

	for valid := iter.First(); valid; valid = iter.Next() {
		embedKey := iter.Key()
		// Embedding key: 0x18 | wsPrefix(8) | ulid(16) = 25 bytes total.
		if len(embedKey) != 25 {
			skipped++
			continue
		}

		embedVal, err := iter.ValueAndErr()
		if err != nil || len(embedVal) <= 8 {
			skipped++
			continue
		}

		vecLen := len(embedVal) - 8
		dim := storage.DimFromLen(vecLen)
		if dim == types.EmbedNone {
			skipped++
			continue
		}

		var wsPrefix [8]byte
		var id [16]byte
		copy(wsPrefix[:], embedKey[1:9])
		copy(id[:], embedKey[9:25])

		erfKey := keys.EngramKey(wsPrefix, id)
		val, closer, err := db.Get(erfKey)
		if err != nil {
			// ERF record gone (deleted engram); nothing to patch.
			skipped++
			continue
		}
		buf := make([]byte, len(val))
		copy(buf, val)
		closer.Close()

		// Skip if EmbedDim is already set (idempotent).
		if len(buf) > erf.OffsetEmbedDim && buf[erf.OffsetEmbedDim] != 0 {
			skipped++
			continue
		}

		if err := erf.PatchEmbedDim(buf, uint8(dim)); err != nil {
			batch.Close()
			return fmt.Errorf("backfill embed dim: patch ERF for %x/%x: %w", wsPrefix, id, err)
		}

		// Also patch the 0x02 meta key which holds a prefix of the ERF record.
		metaKey := keys.MetaKey(wsPrefix, id)
		batch.Set(erfKey, buf, nil)
		batch.Set(metaKey, erf.MetaKeySlice(buf), nil)
		batchCount++
		patched++

		if batchCount >= batchSize {
			if err := batch.Commit(pebble.Sync); err != nil {
				batch.Close()
				return fmt.Errorf("backfill embed dim: commit batch: %w", err)
			}
			batch.Close()
			batch = db.NewBatch()
			batchCount = 0
		}
	}

	if err := iter.Error(); err != nil {
		batch.Close()
		return fmt.Errorf("backfill embed dim: iter: %w", err)
	}

	if batchCount > 0 {
		if err := batch.Commit(pebble.Sync); err != nil {
			batch.Close()
			return fmt.Errorf("backfill embed dim: commit final batch: %w", err)
		}
	}
	batch.Close()

	slog.Info("backfill embed dim complete", "patched", patched, "skipped", skipped)
	return nil
}
