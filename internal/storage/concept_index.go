package storage

import (
	"context"
	"fmt"

	"github.com/cockroachdb/pebble"
	"github.com/scrypster/muninndb/internal/storage/keys"
)

// ScanConceptIndex scans the 0x2B concept-index keys for a single (vault,
// conceptHash) pair and calls fn for each matching engram ID. Iteration is
// pebble key-order (engram ULIDs lexicographically ascending, which is also
// time-ascending since ULIDs embed creation time).
//
// Hash collisions are NOT filtered here — callers must hydrate each engram
// and compare the full Concept string to confirm the match. This matches
// the established pattern for TagIndexKey (0x0C) / CreatorIndexKey (0x0D).
//
// Key layout: 0x2B | wsPrefix(8) | conceptHash(4) | id(16) = 29 bytes
func (ps *PebbleStore) ScanConceptIndex(ctx context.Context, wsPrefix [8]byte, conceptHash uint32, fn func(engramID ULID) error) error {
	prefix := keys.ConceptIndexPrefix(wsPrefix, conceptHash)
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
		return fmt.Errorf("scan concept index: iter: %w", err)
	}
	defer iter.Close()

	for valid := iter.First(); valid; valid = iter.Next() {
		k := iter.Key()
		if len(k) != 29 { // 1 + 8 + 4 + 16
			continue
		}
		var idBytes [16]byte
		copy(idBytes[:], k[13:29])
		id := ULID(idBytes)
		if err := fn(id); err != nil {
			return err
		}
	}
	return iter.Error()
}
