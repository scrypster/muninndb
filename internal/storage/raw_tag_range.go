package storage

import (
	"context"
	"fmt"
	"strings"

	"github.com/cockroachdb/pebble"
	"github.com/scrypster/muninndb/internal/storage/keys"
)

// SplitRawTagKV splits a tag on its FIRST ':' into (tagKey, value). Only tags
// containing ':' get a raw-tag-range index entry — this gates the index to
// key:value shaped tags (e.g. "due:2026-07-27") to bound write-amplification;
// bare tags (e.g. "important") never get a 0x2B entry.
func SplitRawTagKV(tag string) (tagKey, value string, ok bool) {
	idx := strings.IndexByte(tag, ':')
	if idx < 0 {
		return "", "", false
	}
	return tag[:idx], tag[idx+1:], true
}

// WriteRawTagIndexEntry queues a single 0x2B raw-tag-range index entry for tag
// on id into batch. Tags without a ':' are silently skipped (not indexed).
// Returns an error — and queues nothing — if the tag's value contains a 0x00
// (NUL) byte, since 0x00 is the reserved separator between value and id in the
// 0x2B key layout; a value containing it would corrupt range-scan ordering.
//
// Exported so internal/storage/migrate's eager backfill migration can reuse
// the exact same encode-and-validate logic used by every live write path.
func WriteRawTagIndexEntry(batch *pebble.Batch, ws [8]byte, tag string, id [16]byte) error {
	tagKey, value, ok := SplitRawTagKV(tag)
	if !ok {
		return nil
	}
	if strings.IndexByte(value, 0x00) >= 0 {
		return fmt.Errorf("raw tag index: tag value contains a NUL byte, rejected: %q", tag)
	}
	k := keys.RawTagRangeKey(ws, keys.Hash(tagKey), []byte(value), id)
	return batch.Set(k, nil, nil)
}

// DeleteRawTagIndexEntry queues the deletion of tag's 0x2B raw-tag-range index
// entry for id from batch, mirroring WriteRawTagIndexEntry's key derivation.
// Tags without a ':' were never indexed and are silently skipped. Unlike the
// write path this does not reject NUL-containing values — a value that was
// never written (because the write was rejected) has no entry to delete, and
// a value that WAS written could not have contained a NUL, so this is a no-op
// in that case regardless.
func DeleteRawTagIndexEntry(batch *pebble.Batch, ws [8]byte, tag string, id [16]byte) {
	tagKey, value, ok := SplitRawTagKV(tag)
	if !ok {
		return
	}
	if strings.IndexByte(value, 0x00) >= 0 {
		return
	}
	k := keys.RawTagRangeKey(ws, keys.Hash(tagKey), []byte(value), id)
	batch.Delete(k, nil)
}

// ScanRawTagRange scans the 0x2B raw-tag-range index for tagKey within
// [lower, upper) — bounds produced by keys.RawTagRangeBound (optionally
// combined via keys.CombineRawTagRangeBounds for a two-sided range) — and
// returns the matching engram IDs. Ascending order; if limit > 0 the scan
// stops after collecting limit IDs.
//
// The index keys on Hash(tagKey) (4 bytes), so a hash collision between two
// distinct tag keys can let a foreign tag's values interleave into the scanned
// range; callers must re-check the real tag_prefix condition downstream
// (e.g. activation's passesMetaFilter) — this method seeds candidates, it does
// not itself guarantee exactness.
func (ps *PebbleStore) ScanRawTagRange(ctx context.Context, wsPrefix [8]byte, tagKey string, lower, upper []byte, limit int) ([]ULID, error) {
	iter, err := ps.pebbleReader(ctx).NewIter(&pebble.IterOptions{
		LowerBound: lower,
		UpperBound: upper,
	})
	if err != nil {
		return nil, fmt.Errorf("scan raw tag range: %w", err)
	}
	defer iter.Close()

	const idLen = 16
	var ids []ULID
	for valid := iter.First(); valid; valid = iter.Next() {
		if limit > 0 && len(ids) >= limit {
			break
		}
		k := iter.Key()
		if len(k) < idLen {
			continue
		}
		var id ULID
		copy(id[:], k[len(k)-idLen:])
		ids = append(ids, id)
	}
	if err := iter.Error(); err != nil {
		return nil, fmt.Errorf("scan raw tag range: iter: %w", err)
	}
	return ids, nil
}
