package provenance

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync/atomic"

	"github.com/cockroachdb/pebble"
	"github.com/scrypster/muninndb/internal/storage/keys"
)

// globalProvenanceSeq is a process-wide monotonic counter used as a tiebreaker
// suffix in ProvenanceSuffixKey. This ensures uniqueness even when two goroutines
// call Append for the same engram within the same nanosecond (which is common on
// Darwin where time.Now() has only microsecond resolution).
var globalProvenanceSeq atomic.Uint32

// Store is the Pebble-backed provenance log.
type Store struct {
	db *pebble.DB
}

// NewStore creates a new provenance Store backed by a Pebble database.
func NewStore(db *pebble.DB) *Store {
	return &Store{
		db: db,
	}
}

// Append adds a new provenance entry under a unique per-entry key.
// The key encodes: prefix | engram_id | timestamp_ns | global_seq.
// No read is required — this is a single O(1) Pebble Set, safe for concurrent
// goroutines appending to the same or different engram IDs simultaneously.
func (s *Store) Append(ctx context.Context, ws [8]byte, id [16]byte, entry ProvenanceEntry) error {
	ts := uint64(entry.Timestamp.UnixNano())
	seq := globalProvenanceSeq.Add(1)
	key := keys.ProvenanceSuffixKey(ws, id, ts, seq)

	data, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	return s.db.Set(key, data, pebble.Sync)
}

// updateOperations are the operations that change an engram that already
// exists. Every other operation is one that brought an engram into being
// (create, evolve, and whatever is added later).
var updateOperations = map[string]struct{}{
	"update-meta":       {},
	"update-relevance":  {},
	"update-trust":      {},
	"stamp-valid-until": {},
}

// IsUpdateOperation reports whether op records a change to an engram that
// already existed, as opposed to the write that created it.
func IsUpdateOperation(op string) bool {
	_, ok := updateOperations[op]
	return ok
}

// Origin returns the entry that brought the engram into being: the earliest
// entry whose operation is not an update. ok is false when there is no such
// entry (no provenance at all, or only updates, as for a record older than its
// audit trail); nothing is then known about where the engram came from.
//
// Entries are keyed by timestamp and the creating entry is stamped with the
// engram's CreatedAt, so it is normally the first key and the scan stops
// there: one read, however often the engram has been touched since. A corrupt
// entry is skipped, as in Get.
func (s *Store) Origin(ctx context.Context, ws [8]byte, id [16]byte) (ProvenanceEntry, bool, error) {
	iter, err := s.db.NewIter(&pebble.IterOptions{
		LowerBound: keys.ProvenanceKey(ws, id),
		UpperBound: keys.ProvenanceKeyUpperBound(ws, id),
	})
	if err != nil {
		return ProvenanceEntry{}, false, err
	}
	defer iter.Close()

	for iter.First(); iter.Valid(); iter.Next() {
		if err := ctx.Err(); err != nil {
			return ProvenanceEntry{}, false, err
		}
		var e ProvenanceEntry
		if err := json.Unmarshal(iter.Value(), &e); err != nil {
			slog.Warn("provenance: skipping corrupt entry", "err", err)
			continue
		}
		if !IsUpdateOperation(e.Operation) {
			return e, true, nil
		}
	}
	if err := iter.Error(); err != nil {
		return ProvenanceEntry{}, false, err
	}
	return ProvenanceEntry{}, false, nil
}

// Get returns all provenance entries for an engram via a prefix-range scan,
// in chronological order (guaranteed by BigEndian timestamp in the key).
// Returns an empty slice (not error) if no entries exist.
func (s *Store) Get(ctx context.Context, ws [8]byte, id [16]byte) ([]ProvenanceEntry, error) {
	lower := keys.ProvenanceKey(ws, id)
	upper := keys.ProvenanceKeyUpperBound(ws, id)

	iter, err := s.db.NewIter(&pebble.IterOptions{
		LowerBound: lower,
		UpperBound: upper, // nil means unbounded; handled by ProvenanceKeyUpperBound
	})
	if err != nil {
		return nil, err
	}
	defer iter.Close()

	entries := make([]ProvenanceEntry, 0)
	for iter.First(); iter.Valid(); iter.Next() {
		var e ProvenanceEntry
		if err := json.Unmarshal(iter.Value(), &e); err != nil {
			slog.Warn("provenance: skipping corrupt entry", "err", err)
			continue
		}
		entries = append(entries, e)
	}
	if err := iter.Error(); err != nil {
		return nil, err
	}
	return entries, nil
}
