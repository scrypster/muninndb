package migrate

import (
	"fmt"
	"log/slog"

	"github.com/cockroachdb/pebble"
	"github.com/scrypster/muninndb/internal/storage/erf"
	"github.com/scrypster/muninndb/internal/storage/keys"
	"github.com/scrypster/muninndb/internal/types"
)

// embedDimFromLen maps a quantized embedding value length (number of int8 vector bytes)
// to an EmbedDimension constant. The embedding value format is:
// [8 bytes quantize params] + [N bytes quantized int8], so N = len(val) - 8.
func embedDimFromLen(n int) types.EmbedDimension {
	switch n {
	case 384:
		return types.Embed384
	case 768:
		return types.Embed768
	case 1536:
		return types.Embed1536
	case 3072:
		return types.Embed3072
	default:
		if n > 0 {
			return types.EmbedOther
		}
		return types.EmbedNone
	}
}

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

	patched, skipped := 0, 0

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
		dim := embedDimFromLen(vecLen)
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
			return fmt.Errorf("backfill embed dim: patch ERF for %x/%x: %w", wsPrefix, id, err)
		}

		batch := db.NewBatch()
		batch.Set(erfKey, buf, nil)
		// Also patch the 0x02 meta key which holds a prefix of the ERF record.
		metaKey := keys.MetaKey(wsPrefix, id)
		batch.Set(metaKey, erf.MetaKeySlice(buf), nil)
		if err := batch.Commit(pebble.Sync); err != nil {
			batch.Close()
			return fmt.Errorf("backfill embed dim: commit for %x/%x: %w", wsPrefix, id, err)
		}
		batch.Close()
		patched++
	}

	if err := iter.Error(); err != nil {
		return fmt.Errorf("backfill embed dim: iter: %w", err)
	}

	slog.Info("backfill embed dim complete", "patched", patched, "skipped", skipped)
	return nil
}
