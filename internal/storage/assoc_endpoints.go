package storage

import (
	"errors"
	"fmt"

	"github.com/cockroachdb/pebble"

	"github.com/scrypster/muninndb/internal/storage/keys"
)

// ErrDanglingEndpoint reports that an association write was refused because one
// of its endpoints has no 0x01 engram record — the engram was hard-deleted (or
// never existed), so the edge would point at nothing.
//
// See STO-12 in docs/internals/invariants.md.
var ErrDanglingEndpoint = errors.New("association endpoint has no engram record")

// engramExists reports whether the authoritative 0x01 record for id is present.
//
// This is the ONLY correct liveness predicate for an association endpoint, and
// it is deliberately EXISTENCE, not lifecycle state:
//
//   - StateSoftDeleted (0x7F) and StateArchived (0x06) engrams KEEP their 0x01
//     record (SoftDelete batch.Sets the same key with a new state). They are
//     restorable, and their edges must survive with them. A state-aware check
//     here would silently destroy every edge of a restorable engram — the one
//     way to turn this guard into data loss.
//   - Only DeleteEngram (hard delete) removes 0x01. Only then is an edge to
//     this ID unrecoverable garbage.
//
// Cost: a single Pebble point read, closed immediately without touching the
// value. Measured at 0.55–1.1 µs, INDEPENDENT of engram size — Pebble hands
// back a reference into the cached block, so a 16 KB engram benchmarks no
// slower than a 1-byte one, and faster than the bounded-iterator alternative
// (1.14–1.36 µs). See assoc_endpoints_bench_test.go.
//
// FAILS OPEN. Any error that is not "not found" reports true. The guard
// protects an integrity invariant, not a security boundary (principle #4:
// fail closed on auth, fail open on presentation) — and refusing writes on a
// transient read fault would silently stop association learning across every
// vault. A missed refusal costs one repairable row; a false refusal costs a
// real edge that can never be recovered.
func (ps *PebbleStore) engramExists(ws [8]byte, id [16]byte) bool {
	_, closer, err := ps.db.Get(keys.EngramKey(ws, id))
	if err != nil {
		return !errors.Is(err, pebble.ErrNotFound) // fail open — see doc comment
	}
	_ = closer.Close()
	return true
}

// checkEndpointsLive returns ErrDanglingEndpoint if either endpoint of an
// association has no 0x01 engram record.
func (ps *PebbleStore) checkEndpointsLive(ws [8]byte, src, dst [16]byte) error {
	if !ps.engramExists(ws, src) {
		return fmt.Errorf("%w: source %s", ErrDanglingEndpoint, ULID(src).String())
	}
	if !ps.engramExists(ws, dst) {
		return fmt.Errorf("%w: target %s", ErrDanglingEndpoint, ULID(dst).String())
	}
	return nil
}
