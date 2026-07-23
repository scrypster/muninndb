package migrate

import (
	"fmt"
	"log/slog"

	"github.com/cockroachdb/pebble"
	"github.com/scrypster/muninndb/internal/prefix"
	"github.com/scrypster/muninndb/internal/storage/erf"
	"github.com/scrypster/muninndb/internal/storage/keys"
)

// BackfillConceptIndex scans existing engram records and writes the 0x2B exact-
// concept reverse index for every non-empty Concept. Existing index entries are
// overwritten with the same empty value, so the migration is idempotent.
func BackfillConceptIndex(db *pebble.DB) error {
	iter, err := db.NewIter(&pebble.IterOptions{
		LowerBound: []byte{prefix.Engram},
		UpperBound: []byte{prefix.Meta},
	})
	if err != nil {
		return fmt.Errorf("backfill concept index: new iter: %w", err)
	}
	defer iter.Close()

	const (
		batchSize    = 500
		engramKeyLen = 25 // prefix(1) + vault(8) + ULID(16)
	)
	batch := db.NewBatch()
	defer func() { _ = batch.Close() }()
	batchCount, indexed, empty := 0, 0, 0

	commit := func() error {
		if batchCount == 0 {
			return nil
		}
		if err := batch.Commit(pebble.Sync); err != nil {
			return err
		}
		batch.Close()
		batch = db.NewBatch()
		batchCount = 0
		return nil
	}

	for valid := iter.First(); valid; valid = iter.Next() {
		key := iter.Key()
		if len(key) != engramKeyLen {
			return fmt.Errorf("backfill concept index: malformed engram key length %d at %x", len(key), key)
		}
		value, err := iter.ValueAndErr()
		if err != nil {
			return fmt.Errorf("backfill concept index: read engram %x: %w", key, err)
		}
		engram, err := erf.Decode(value)
		if err != nil {
			return fmt.Errorf("backfill concept index: decode engram %x: %w", key, err)
		}
		if engram.Concept == "" {
			empty++
			continue
		}

		var vault [8]byte
		var id [16]byte
		copy(vault[:], key[1:9])
		copy(id[:], key[9:25])
		if err := batch.Set(keys.ConceptIndexKey(vault, keys.Hash(engram.Concept), id), nil, nil); err != nil {
			return fmt.Errorf("backfill concept index: set index key: %w", err)
		}
		batchCount++
		indexed++
		if batchCount >= batchSize {
			if err := commit(); err != nil {
				return fmt.Errorf("backfill concept index: commit batch: %w", err)
			}
		}
	}
	if err := iter.Error(); err != nil {
		return fmt.Errorf("backfill concept index: iter: %w", err)
	}
	if err := commit(); err != nil {
		return fmt.Errorf("backfill concept index: commit final batch: %w", err)
	}

	slog.Info("backfill concept index complete", "indexed", indexed, "empty_concepts", empty)
	return nil
}
