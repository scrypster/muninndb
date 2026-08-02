package storage

import (
	"context"
	"fmt"

	"github.com/cockroachdb/pebble"
	"github.com/scrypster/muninndb/internal/storage/keys"
)

// WriteLastAccessEntry writes or updates the 0x22 LastAccess index entry.
// If prevMillis != 0, the previous entry is deleted first (key includes timestamp,
// so old key must be removed when time changes).
//
// #811: prefix.LastAccess (0x22) is NOT in clone.go's vaultScopedSwapPrefixes,
// so this index is empty in a cloned or merged vault and where_left_off is blind
// there. Deliberately still open — but note the ordering trap before rebuilding
// it: keys.LastAccessIndexKey encodes ^uint64(millis), so a NEGATIVE millis
// (which is what a pre-2000 LastAccess produces) inverts to a SMALL key and
// would sort FIRST — i.e. an index rebuild landing before #810's fix would put
// garbage-timestamped engrams at the head of where_left_off. Rebuild only on top
// of #810.
//
// The trap does not decay away on its own. #810's write-side fix only covers
// writers that CREATE timestamps (see normalizeEngramTimes); the six
// read-modify-write paths rewrite a pre-existing sentinel back verbatim, so a
// vault cloned before the fix keeps it until TouchAccess happens to repair each
// record individually. TouchAccess is the ONLY repair path: UpdateMetadata is a
// pass-through and three of its four callers carry a decoded LastAccess straight
// back to disk — see normalizeEngramTimes' doc for the caller table.
//
// Related, and the reason a second concern closed clean: today a cloned vault
// has NO 0x22 entries at all, so there are no orphaned index keys to reconcile.
// Adding prefix.LastAccess to vaultScopedSwapPrefixes — the obvious fix for
// #811 — changes that: the clone would then carry index keys built from the
// SOURCE's LastAccess values while the copied 0x01 records get their LastAccess
// reset to CreatedAt, so the two would disagree from the first instant. Whoever
// fixes #811 must rebuild the index from the rewritten records, not copy it.
func (ps *PebbleStore) WriteLastAccessEntry(ctx context.Context, ws [8]byte, id ULID, prevMillis, newMillis int64) error {
	batch := ps.db.NewBatch()
	defer batch.Close()

	if prevMillis != 0 {
		oldKey := keys.LastAccessIndexKey(ws, prevMillis, [16]byte(id))
		if err := batch.Delete(oldKey, nil); err != nil {
			return fmt.Errorf("last access: delete old: %w", err)
		}
	}

	newKey := keys.LastAccessIndexKey(ws, newMillis, [16]byte(id))
	if err := batch.Set(newKey, nil, nil); err != nil {
		return fmt.Errorf("last access: set new: %w", err)
	}
	return batch.Commit(pebble.NoSync)
}

// ScanLastAccessDesc scans the 0x22 index in ascending key order (= descending
// LastAccess time due to inverted millis encoding). Calls fn for each pair.
func (ps *PebbleStore) ScanLastAccessDesc(ctx context.Context, ws [8]byte, fn func(id ULID, lastAccessMillis int64) error) error {
	prefix := keys.LastAccessIndexPrefix(ws)
	upperBound := make([]byte, len(prefix))
	copy(upperBound, prefix)
	for i := len(upperBound) - 1; i >= 0; i-- {
		upperBound[i]++
		if upperBound[i] != 0 {
			break
		}
	}

	iter, err := ps.db.NewIter(&pebble.IterOptions{LowerBound: prefix, UpperBound: upperBound})
	if err != nil {
		return fmt.Errorf("scan last access: iter: %w", err)
	}
	defer iter.Close()

	for valid := iter.First(); valid; valid = iter.Next() {
		k := iter.Key()
		if len(k) != 33 {
			continue
		}
		inverted := uint64(k[9])<<56 | uint64(k[10])<<48 | uint64(k[11])<<40 |
			uint64(k[12])<<32 | uint64(k[13])<<24 | uint64(k[14])<<16 |
			uint64(k[15])<<8 | uint64(k[16])
		millis := int64(^inverted)
		var idBytes [16]byte
		copy(idBytes[:], k[17:33])
		id := ULID(idBytes)
		if err := fn(id, millis); err != nil {
			return err
		}
	}
	return nil
}

// DeleteLastAccessEntry removes the 0x22 index entry for a deleted engram.
func (ps *PebbleStore) DeleteLastAccessEntry(ctx context.Context, ws [8]byte, id ULID, lastAccessMillis int64) error {
	if lastAccessMillis == 0 {
		return nil
	}
	key := keys.LastAccessIndexKey(ws, lastAccessMillis, [16]byte(id))
	return ps.db.Delete(key, pebble.NoSync)
}
