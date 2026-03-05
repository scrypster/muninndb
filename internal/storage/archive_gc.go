package storage

import (
	"context"
	"fmt"
	"time"

	"github.com/cockroachdb/pebble"
	"github.com/scrypster/muninndb/internal/storage/keys"
)

// GCArchivedEdges scans all archived edges for the given vault and deletes
// those that meet ALL four conditions:
//   - peakWeight < 0.15
//   - coActivationCount < 3
//   - daysSinceLastActivation > 1095 (3 years)
//   - restoredAt == 0 (never restored and reestablished)
//
// Returns the number of pruned entries.
func (ps *PebbleStore) GCArchivedEdges(ctx context.Context, wsPrefix [8]byte) (int, error) {
	lower := keys.ArchiveAssocRangeStart(wsPrefix)
	upper := keys.ArchiveAssocRangeEnd(wsPrefix)

	iter, err := ps.db.NewIter(&pebble.IterOptions{
		LowerBound: lower,
		UpperBound: upper,
	})
	if err != nil {
		return 0, fmt.Errorf("archive GC iterator: %w", err)
	}
	defer iter.Close()

	type gcEntry struct {
		key []byte
	}
	var toDelete []gcEntry

	const daysThreshold = 1095 // 3 years

	for iter.First(); iter.Valid(); iter.Next() {
		if ctx.Err() != nil {
			break
		}
		val := iter.Value()
		_, _, _, lastActivated, peakWeight, coActivationCount, restoredAt := decodeAssocValue(val)

		if peakWeight >= 0.15 {
			continue
		}
		if coActivationCount >= 3 {
			continue
		}
		if restoredAt != 0 {
			continue
		}

		daysSinceLastAct := float64(0)
		if lastActivated > 0 {
			daysSinceLastAct = time.Since(time.Unix(int64(lastActivated), 0)).Hours() / 24
		}
		if daysSinceLastAct <= float64(daysThreshold) {
			continue
		}

		// All four conditions met — schedule for deletion.
		keyCopy := make([]byte, len(iter.Key()))
		copy(keyCopy, iter.Key())
		toDelete = append(toDelete, gcEntry{key: keyCopy})
	}
	if err := iter.Error(); err != nil {
		return 0, fmt.Errorf("archive GC scan: %w", err)
	}

	if len(toDelete) == 0 {
		return 0, nil
	}

	batch := ps.db.NewBatch()
	defer batch.Close()

	for _, e := range toDelete {
		batch.Delete(e.key, nil)
	}

	if err := batch.Commit(pebble.NoSync); err != nil {
		return 0, fmt.Errorf("archive GC commit: %w", err)
	}

	// Rebuild the Bloom filter so pruned entries are no longer tested positively
	// (standard Bloom filters do not support deletion; rebuild compacts the filter).
	ps.archiveBloom = ps.RebuildArchiveBloom()

	return len(toDelete), nil
}
