package migrate

import (
	"fmt"
	"log/slog"

	"github.com/cockroachdb/pebble"
	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/storage/erf"
)

// BackfillRawTagRange scans all existing 0x01 (engram) records across every
// vault and writes the corresponding 0x2C ordered raw-tag-range index entries
// (S1) for any key:value tags found. New writes populate this index
// automatically (see WriteRawTagIndexEntry callers in internal/storage);
// this migration is the EAGER, one-time backfill for engrams written before
// the index existed — required so that pre-existing `due:` (and similar)
// tags become queryable via bounded range scans immediately, not lazily.
//
// The migration is idempotent: writing an already-present 0x2C key is a no-op
// (empty value, same key bytes).
//
// A tag whose value contains a 0x00 (NUL) byte would be rejected by a live
// write (WriteRawTagIndexEntry returns an error), but a NUL byte cannot
// appear in a value that was already accepted by a live write in the first
// place — so pre-existing data cannot contain one. Out of caution the
// migration still treats a rejected tag as non-fatal: it skips that single
// tag (logging it) rather than aborting the whole backfill over one engram's
// one malformed tag.
func BackfillRawTagRange(db *pebble.DB) error {
	iter, err := db.NewIter(&pebble.IterOptions{
		LowerBound: []byte{0x01},
		UpperBound: []byte{0x02},
	})
	if err != nil {
		return fmt.Errorf("backfill raw tag range: new iter: %w", err)
	}
	defer iter.Close()

	const batchSize = 500
	const engramKeyLen = 25 // 0x01(1) + ws(8) + id(16)

	batch := db.NewBatch()
	batchCount := 0
	engrams, indexKeys, skippedTags, skippedKeys := 0, 0, 0, 0

	for valid := iter.First(); valid; valid = iter.Next() {
		k := iter.Key()
		if len(k) != engramKeyLen {
			skippedKeys++
			continue
		}

		var ws [8]byte
		var id [16]byte
		copy(ws[:], k[1:9])
		copy(id[:], k[9:25])

		val, valErr := iter.ValueAndErr()
		if valErr != nil {
			skippedKeys++
			continue
		}
		buf := make([]byte, len(val))
		copy(buf, val)

		erfEng, decErr := erf.Decode(buf)
		if decErr != nil {
			// Corrupt/undecodable record — skip it, do not abort the backfill.
			skippedKeys++
			continue
		}

		for _, tag := range erfEng.Tags {
			if _, _, ok := storage.SplitRawTagKV(tag); !ok {
				continue // bare tag (no ':') — never indexed, not an error
			}
			if err := storage.WriteRawTagIndexEntry(batch, ws, tag, id); err != nil {
				slog.Warn("backfill raw tag range: skipping malformed tag",
					"ws", fmt.Sprintf("%x", ws), "id", fmt.Sprintf("%x", id), "err", err)
				skippedTags++
				continue
			}
			indexKeys++
		}
		engrams++
		batchCount++

		if batchCount >= batchSize {
			if err := batch.Commit(pebble.Sync); err != nil {
				batch.Close()
				return fmt.Errorf("backfill raw tag range: commit batch: %w", err)
			}
			batch.Close()
			batch = db.NewBatch()
			batchCount = 0
		}
	}

	if err := iter.Error(); err != nil {
		batch.Close()
		return fmt.Errorf("backfill raw tag range: iter: %w", err)
	}

	if batchCount > 0 {
		if err := batch.Commit(pebble.Sync); err != nil {
			batch.Close()
			return fmt.Errorf("backfill raw tag range: commit final batch: %w", err)
		}
	}
	batch.Close()

	slog.Info("backfill raw tag range complete",
		"engrams", engrams,
		"index_keys", indexKeys,
		"skipped_tags", skippedTags,
		"skipped_keys", skippedKeys,
	)
	return nil
}
