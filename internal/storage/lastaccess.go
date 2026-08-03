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
// #811 (closed): prefix.LastAccess (0x22) is NOT in clone.go's
// vaultScopedSwapPrefixes, so CloneVaultData/MergeVaultData never carry it —
// that part is unchanged, deliberately: copying the SOURCE's index keys would
// disagree with the copied 0x01 records' rewritten LastAccess (clone resets
// AccessCount but carries LastAccess forward as of #810/#835; a copied key
// built from the pre-copy value would then point at the wrong millis for a
// merge, or be simply wrong after a clone that also rewrote CreatedAt via
// normalizeEngramTimes). Instead, `Engine.reindexVault`
// (internal/engine/engine_clone.go) — already run after both operations to
// rebuild FTS/HNSW — rebuilds this index too, calling WriteLastAccessEntry
// with prevMillis=0 for every scanned engram's REWRITTEN LastAccess. That
// ordering is why the ordering trap this comment used to warn about never
// materializes: keys.LastAccessIndexKey encodes ^uint64(millis), so a
// NEGATIVE millis (a pre-2000 LastAccess, #810's ERF zero-time sentinel)
// inverts to a SMALL key and sorts FIRST — but #810/#835 landed first, so by
// the time reindexVault runs, every engram's LastAccess is real, non-sentinel
// data, not the 1754 ghost.
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
