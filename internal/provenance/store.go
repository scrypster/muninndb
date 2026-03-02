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
