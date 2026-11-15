package storage

import (
	"context"
	"encoding/binary"
	"fmt"
	"sort"

	"github.com/cockroachdb/pebble"
	"github.com/scrypster/muninndb/internal/rankorder"
	"github.com/scrypster/muninndb/internal/storage/keys"
)

// TransitionUpdate represents a single transition to increment.
type TransitionUpdate struct {
	WS  [8]byte
	Src [16]byte
	Dst [16]byte
}

// TransitionSet represents an absolute transition count to write (overwrite, not increment).
type TransitionSet struct {
	WS    [8]byte
	Src   [16]byte
	Dst   [16]byte
	Count uint32
}

// TransitionTarget is one transition target with its accumulated count.
type TransitionTarget struct {
	ID    [16]byte
	Count uint32
}

// TransitionStore abstracts Pebble-backed transition persistence so the
// TransitionCache can delegate cold reads and flush writes.
type TransitionStore interface {
	IncrTransitionBatch(ctx context.Context, updates []TransitionUpdate) error
	SetTransitionBatch(ctx context.Context, sets []TransitionSet) error
	GetTopTransitions(ctx context.Context, ws [8]byte, srcID [16]byte, topK int) ([]TransitionTarget, error)
}

// IncrTransitionBatch atomically increments transition counts for multiple
// (ws, src, dst) tuples. Pre-aggregates duplicates within the batch to ensure
// correct counts when the same pair appears multiple times.
func (ps *PebbleStore) IncrTransitionBatch(ctx context.Context, updates []TransitionUpdate) error {
	if len(updates) == 0 {
		return nil
	}

	// Pre-aggregate: count how many times each unique key appears in this batch.
	type keyStr = string
	deltas := make(map[keyStr]uint32, len(updates))
	for _, u := range updates {
		k := string(keys.TransitionKey(u.WS, u.Src, u.Dst))
		deltas[k]++
	}

	batch := ps.db.NewBatch()
	defer batch.Close()

	var buf [4]byte
	for k, delta := range deltas {
		key := []byte(k)

		var current uint32
		val, closer, err := ps.db.Get(key)
		if err == nil {
			if len(val) >= 4 {
				current = binary.BigEndian.Uint32(val)
			}
			closer.Close()
		} else if err != pebble.ErrNotFound {
			return fmt.Errorf("read transition: %w", err)
		}

		current += delta
		binary.BigEndian.PutUint32(buf[:], current)
		if err := batch.Set(key, buf[:], nil); err != nil {
			return fmt.Errorf("set transition: %w", err)
		}
	}

	if err := batch.Commit(pebble.NoSync); err != nil {
		return fmt.Errorf("commit transition batch: %w", err)
	}
	return nil
}

// SetTransitionBatch writes absolute transition counts for multiple (ws, src, dst)
// tuples. Unlike IncrTransitionBatch, this overwrites existing counts rather than
// incrementing. Used by TransitionCache flush: the cache holds merged totals and
// writes them directly, avoiding the read-modify-write of IncrTransitionBatch.
func (ps *PebbleStore) SetTransitionBatch(ctx context.Context, sets []TransitionSet) error {
	if len(sets) == 0 {
		return nil
	}

	batch := ps.db.NewBatch()
	defer batch.Close()

	var buf [4]byte
	for _, s := range sets {
		key := keys.TransitionKey(s.WS, s.Src, s.Dst)
		binary.BigEndian.PutUint32(buf[:], s.Count)
		if err := batch.Set(key, buf[:], nil); err != nil {
			return fmt.Errorf("set transition: %w", err)
		}
	}

	if err := batch.Commit(pebble.NoSync); err != nil {
		return fmt.Errorf("commit transition set batch: %w", err)
	}
	return nil
}

// GetTopTransitions returns the top-K transition targets for a given source
// engram, sorted by count descending. Uses a prefix scan on the transition
// key space.
func (ps *PebbleStore) GetTopTransitions(ctx context.Context, ws [8]byte, srcID [16]byte, topK int) ([]TransitionTarget, error) {
	if topK <= 0 {
		return nil, nil
	}

	prefix := keys.TransitionPrefixForSrc(ws, srcID)
	iter, err := ps.db.NewIter(&pebble.IterOptions{
		LowerBound: prefix,
		UpperBound: incrementPrefix(prefix),
	})
	if err != nil {
		return nil, fmt.Errorf("create transition iter: %w", err)
	}
	defer iter.Close()

	var targets []TransitionTarget
	for iter.First(); iter.Valid(); iter.Next() {
		k := iter.Key()
		v := iter.Value()

		// Extract dst ID from key: prefix is 25 bytes, dst starts at byte 25
		if len(k) < 41 {
			continue
		}
		var dstID [16]byte
		copy(dstID[:], k[25:41])

		var count uint32
		if len(v) >= 4 {
			count = binary.BigEndian.Uint32(v)
		}

		targets = append(targets, TransitionTarget{ID: dstID, Count: count})
	}

	if err := iter.Error(); err != nil {
		return nil, fmt.Errorf("transition iter: %w", err)
	}

	// Equal counts break newest-first (rankorder): this list is cut at topK
	// and feeds PAS candidate injection, which precedes recall's final sort.
	sort.Slice(targets, func(i, j int) bool {
		if targets[i].Count != targets[j].Count {
			return targets[i].Count > targets[j].Count
		}
		return rankorder.Before(targets[i].ID, targets[j].ID)
	})

	if len(targets) > topK {
		targets = targets[:topK]
	}

	return targets, nil
}

// incrementPrefix returns a key that is the exclusive upper bound for a prefix scan.
func incrementPrefix(prefix []byte) []byte {
	upper := make([]byte, len(prefix))
	copy(upper, prefix)
	for i := len(upper) - 1; i >= 0; i-- {
		upper[i]++
		if upper[i] != 0 {
			return upper
		}
	}
	return append(prefix, 0)
}
