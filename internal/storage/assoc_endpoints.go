package storage

import (
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/cockroachdb/pebble"

	"github.com/scrypster/muninndb/internal/storage/keys"
)

// guardReadFaultLogInterval rate-limits the WARN emitted when the endpoint
// guard's liveness read fails. A corrupt block is a PERMANENT, key-scoped fault
// (measured in #809), so the same key recurs in flush window after flush window
// — unbounded logging would bury the first report, which is the useful one.
const guardReadFaultLogInterval = 30 * time.Second

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
// FAILS OPEN, BUT NOT SILENTLY. Any error that is not "not found" reports true
// — and is counted and logged. The guard protects an integrity invariant, not a
// security boundary (principle #4: fail closed on auth, fail open on
// presentation) — and refusing writes on a transient read fault would silently
// stop association learning across every vault. A missed refusal costs one
// repairable row; a false refusal costs a real edge that can never be recovered.
//
// The report is the other half, and matches the standard #809 set with
// AssocBatchSkipError: a bare fail-open would revert to pre-#803 behaviour
// under a Pebble read fault with NOTHING in the logs, which is the
// silently-wrong class this whole invariant exists to close. The WARN is
// rate-limited (a corrupt block is a permanent, key-scoped fault, so it
// recurs); GuardReadFaults is not, so the true rate is always observable.
func (ps *PebbleStore) engramExists(ws [8]byte, id [16]byte) bool {
	key := keys.EngramKey(ws, id)
	// The readFault seam is consulted directly rather than by routing through
	// pointGet: pointGet copies the value out, and the whole point of this
	// predicate is that it never touches the value (a 16 KB engram benchmarks
	// no slower than a 1-byte one because Pebble hands back a reference into
	// the cached block). Test-only; nil in production.
	if ps.readFault != nil {
		if err := ps.readFault(key); err != nil {
			ps.reportGuardReadFault(ws, id, err)
			return true // fail open — see doc comment
		}
	}
	_, closer, err := ps.db.Get(key)
	if err != nil {
		if errors.Is(err, pebble.ErrNotFound) {
			return false
		}
		ps.reportGuardReadFault(ws, id, err)
		return true // fail open — see doc comment
	}
	_ = closer.Close()
	return true
}

// reportGuardReadFault counts every endpoint-liveness read fault and logs at
// most one WARN per guardReadFaultLogInterval.
func (ps *PebbleStore) reportGuardReadFault(ws [8]byte, id [16]byte, err error) {
	n := ps.guardReadFaults.Add(1)
	now := time.Now().UnixNano()
	last := ps.guardReadFaultLoggedAt.Load()
	if now-last < int64(guardReadFaultLogInterval) && last != 0 {
		return
	}
	if !ps.guardReadFaultLoggedAt.CompareAndSwap(last, now) {
		return // another goroutine is logging this window
	}
	slog.Warn("storage: STO-12 endpoint liveness read failed; guard is failing OPEN for this write",
		"engram", ULID(id).String(), "vault_prefix", fmt.Sprintf("%x", ws),
		"total_faults", n, "err", err)
}

// GuardReadFaults returns the number of times the STO-12 endpoint-liveness read
// failed and the guard therefore failed open. A non-zero value means the
// invariant was not enforced for that many writes, so dangling rows may exist
// even though every writer is guarded.
func (ps *PebbleStore) GuardReadFaults() uint64 { return ps.guardReadFaults.Load() }

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

// checkInlineAssocTargets is the STO-12 guard for the three INLINE-Associations
// writers — PebbleStore.WriteEngram, PebbleStore.WriteEngramBatch and
// pebbleStoreBatch.WriteEngram — each of which Sets 0x03/0x04/0x14 straight
// from eng.Associations without ever calling WriteAssociation.
//
// That path is not an internal one: mbp.WriteRequest.Associations reaches it
// from REST (rest.WriteRequest is a type alias of mbp.WriteRequest), gRPC, MBP
// and the embedded library, so an ordinary client holding an ID across a hard
// delete lands three dangling rows with a nil error. Guarding only
// WriteAssociation left that wide open.
//
// The SOURCE endpoint is never read. The engram carrying these associations is
// the source of all of them, and its own 0x01 key is Set in the same
// uncommitted batch as the edges — so it is live by construction, and a
// DB-read predicate would reject every engram that has ever carried an inline
// association. sameWrite widens that to the other engrams the same atomic write
// makes live (siblings in a WriteEngramBatch call, items already queued in a
// StoreBatch); nil means "no siblings".
func (ps *PebbleStore) checkInlineAssocTargets(ws [8]byte, self [16]byte, assocs []Association, sameWrite func(id [16]byte) bool) error {
	for _, a := range assocs {
		dst := [16]byte(a.TargetID)
		if dst == self {
			continue
		}
		if sameWrite != nil && sameWrite(dst) {
			continue
		}
		if !ps.engramExists(ws, dst) {
			return fmt.Errorf("%w: inline association target %s", ErrDanglingEndpoint, ULID(dst).String())
		}
	}
	return nil
}
