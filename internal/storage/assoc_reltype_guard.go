package storage

import (
	"errors"
	"fmt"

	"github.com/cockroachdb/pebble"
	"github.com/scrypster/muninndb/internal/storage/keys"
)

// ErrAssocRelTypeCollision reports that an association write would silently
// replace an existing edge of a DIFFERENT RelType at the same (src, weight,
// dst) — #771.
//
// The forward/reverse association keys are `prefix|ws|src|weightComplement|dst`
// (see keys.AssocFwdKey/AssocRevKey): RelType lives only in the VALUE, never in
// the key. Two edges between the same pair at the same weight therefore land on
// the identical key, and the second write overwrites the first value outright —
// with no error, no warning, ok:true. `link(contradicts, w=1.0)` followed by
// `link(supersedes, w=1.0)` between the same pair erases the contradiction
// declaration instead of resolving it, and the same collision applies to ANY
// two distinct RelTypes at a shared weight (e.g. supports silently replacing
// depends_on).
//
// See STO-15 in docs/internals/invariants.md.
var ErrAssocRelTypeCollision = errors.New("association write would replace a different rel_type at the same weight")

// checkRelTypeCollision refuses a forward-association write when a value
// already exists at the exact (ws, src, weight, dst) key and decodes a
// DIFFERENT RelType than newRelType. A missing key, a decode failure (treated
// as "nothing usable to compare against" — refusing on it would block
// legitimate writes over old or corrupt rows more aggressively than the
// silent-replacement bug it exists to catch), or an existing edge of the SAME
// RelType (an idempotent re-write, e.g. bumping confidence) is not a collision.
//
// This is the "honest first increment" named in #771: refuse loudly rather
// than replace silently. It does not give multi-RelType coexistence at a
// shared weight — that needs RelType in the key, an on-disk format change
// requiring a migration (see internal/storage/migrate/), which this increment
// deliberately defers. A caller that legitimately wants to replace an edge of
// a different RelType must pick a different weight (or delete the existing
// edge first) — there is currently no explicit "replace" affordance on the
// wire (mbp.LinkRequest carries no such flag); adding one is future work, not
// implied by this guard.
func checkRelTypeCollision(db *pebble.DB, wsPrefix [8]byte, src [16]byte, weight float32, dst [16]byte, newRelType RelType) error {
	existing, err := Get(db, keys.AssocFwdKey(wsPrefix, src, weight, dst))
	if err != nil {
		// A read fault here is not this guard's concern — the endpoint-liveness
		// guard (STO-12) and the underlying write's own error handling already
		// cover Pebble read failures on this path. Treat as "nothing to compare".
		return nil
	}
	if existing == nil {
		return nil
	}
	relType, _, _, _, _, _, _ := decodeAssocValue(existing)
	if relType == newRelType {
		return nil
	}
	return fmt.Errorf("%w: existing rel_type %v at weight %v between %s and %s, new rel_type %v",
		ErrAssocRelTypeCollision, relType, weight, ULID(src).String(), ULID(dst).String(), newRelType)
}
