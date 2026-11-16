// Package rankorder holds recall's ONE tie rule: among entries whose ranking
// key is exactly equal, the newest ULID comes first (ULID descending — a ULID's
// leading 48 bits are its creation time in milliseconds, including a caller-
// backdated created_at).
//
// Every place recall orders or CUTS a list of equally-ranked memories uses this
// function: the FTS and HNSW top-k cuts, the RecentActive bucket cut, the
// time-bounded pool, PAS transition targets, phase-3 RRF fusion, the phase-6
// final sorts on every fusion path, the COG-28 shadow cap, and the engine's
// post-pipeline re-sorts (entity boost, supersession, version head, COG-25
// currency). The cuts run BEFORE the final sort, so a rule that differed
// between layers would let an early oldest-first cut silently hide the newest
// of a set of equals no matter what the final comparator said (honest clock,
// increment 1). One function makes the layers unable to disagree.
//
// It is a leaf package (standard library only) so storage, the indexes and the
// engine can all import it.
package rankorder

import "bytes"

// Before reports whether a orders before b when their ranking keys tie
// exactly: newest first. It is a strict weak order on distinct ids and false
// for a == b, so it composes into any sort comparator as the final key.
func Before(a, b [16]byte) bool {
	if legacyAscending() {
		return bytes.Compare(a[:], b[:]) < 0
	}
	return bytes.Compare(a[:], b[:]) > 0
}

// NewestFirst reports the direction ordered SCANS (index iterations that cut
// at a limit) must walk to honour Before: true means descending ULID order.
func NewestFirst() bool { return !legacyAscending() }
