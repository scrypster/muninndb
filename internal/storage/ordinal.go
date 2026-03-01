package storage

import (
	"context"
	"encoding/binary"
	"fmt"
	"sort"

	"github.com/cockroachdb/pebble"
	"github.com/scrypster/muninndb/internal/storage/keys"
)

// WriteOrdinal atomically writes the ordinal value for (parentID, childID).
func (ps *PebbleStore) WriteOrdinal(ctx context.Context, wsPrefix [8]byte, parentID, childID ULID, ordinal int32) error {
	key := keys.OrdinalKey(wsPrefix, [16]byte(parentID), [16]byte(childID))
	var val [4]byte
	binary.BigEndian.PutUint32(val[:], uint32(ordinal))
	batch := ps.db.NewBatch()
	defer batch.Close()
	batch.Set(key, val[:], nil)
	if err := batch.Commit(pebble.NoSync); err != nil {
		return fmt.Errorf("WriteOrdinal commit: %w", err)
	}
	return nil
}

// ReadOrdinal reads the ordinal for (parentID, childID).
// Returns found=false if the key does not exist.
func (ps *PebbleStore) ReadOrdinal(ctx context.Context, wsPrefix [8]byte, parentID, childID ULID) (int32, bool, error) {
	key := keys.OrdinalKey(wsPrefix, [16]byte(parentID), [16]byte(childID))
	val, err := Get(ps.db, key)
	if err != nil || val == nil || len(val) < 4 {
		return 0, false, nil
	}
	return int32(binary.BigEndian.Uint32(val[:4])), true, nil
}

// DeleteOrdinal removes the ordinal key for (parentID, childID). No-op if absent.
func (ps *PebbleStore) DeleteOrdinal(ctx context.Context, wsPrefix [8]byte, parentID, childID ULID) error {
	key := keys.OrdinalKey(wsPrefix, [16]byte(parentID), [16]byte(childID))
	batch := ps.db.NewBatch()
	defer batch.Close()
	batch.Delete(key, nil)
	if err := batch.Commit(pebble.NoSync); err != nil {
		return fmt.Errorf("DeleteOrdinal commit: %w", err)
	}
	return nil
}

// ListChildOrdinals returns all (childID, ordinal) pairs for parentID,
// sorted by ordinal ascending.
func (ps *PebbleStore) ListChildOrdinals(ctx context.Context, wsPrefix [8]byte, parentID ULID) ([]OrdinalEntry, error) {
	prefix := keys.OrdinalPrefixForParent(wsPrefix, [16]byte(parentID))
	iter, err := PrefixIterator(ps.db, prefix)
	if err != nil {
		return nil, fmt.Errorf("ListChildOrdinals iter: %w", err)
	}
	defer iter.Close()

	// Key: 0x1E | ws(8) | parentID(16) | childID(16) = 41 bytes; childID at [25:41].
	var entries []OrdinalEntry
	for iter.First(); iter.Valid(); iter.Next() {
		k := iter.Key()
		if len(k) < 41 {
			continue
		}
		var childID ULID
		copy(childID[:], k[25:41])
		val := iter.Value()
		if len(val) < 4 {
			continue
		}
		ordinal := int32(binary.BigEndian.Uint32(val[:4]))
		entries = append(entries, OrdinalEntry{ChildID: childID, Ordinal: ordinal})
	}
	if err := iter.Error(); err != nil {
		return nil, fmt.Errorf("ListChildOrdinals scan: %w", err)
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Ordinal < entries[j].Ordinal
	})
	return entries, nil
}
