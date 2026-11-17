package replication

import (
	"errors"
	"fmt"
	"io"
	"log/slog"

	"github.com/cockroachdb/pebble"
	"github.com/vmihailenco/msgpack/v5"
)

// ErrApplierHalted is returned by Applier.Apply while a refusal is recorded
// (Applier.Halt). A node that was refused holds an entry its leader does not,
// and it skips every entry numbered at or below its own position, so applying
// what a leader sends it would drop that leader's entries in that range.
var ErrApplierHalted = errors.New("replication: this node was refused by its leader and applies nothing until it is re-seeded or a leader confirms its last entry")

// haltRecordVersion is the format of the stored HaltRecord.
const haltRecordVersion = 1

// HaltRecord is what a follower keeps, on disk, once a leader has refused it
// for holding an entry the leader does not (ErrFollowerAhead).
//
// It is on disk because the proof is not. The leader proved the difference by
// reading its own log entry at this node's position, and a leader prunes its
// log. From then on the same leader, asked again after either side restarts,
// can only say that it cannot compare. A node that forgot the refusal would be
// admitted on that answer and would skip the leader's entries numbered at or
// below its own position while reporting no lag.
//
// What removes it:
//   - a leader confirming that this node's last entry is the leader's own
//     (JoinResponse.Matched). "Cannot compare" is not that and removes nothing.
//   - this node becoming the leader. A leader follows nobody, and the other
//     nodes are measured against it from then on.
//   - a snapshot replacing this node's state (Applier.ResetTo).
//   - an operator replacing the store: the re-seed in docs/cluster-operations.md.
//
// Nothing else does, and there is no command that does. Removing the record
// without replacing the data is the failure it exists to prevent.
type HaltRecord struct {
	Version uint8 `msgpack:"v"`
	// LeaderID and Epoch name the leadership term that proved the difference.
	LeaderID string `msgpack:"leader"`
	Epoch    uint64 `msgpack:"epoch"`
	// LastApplied is this node's applied position when it was refused, and
	// LeaderHead is where that leader's log ended then.
	LastApplied uint64 `msgpack:"last_applied"`
	LeaderHead  uint64 `msgpack:"leader_head"`
	// Why is the proof in words, as given at the time.
	Why string `msgpack:"why"`
	// At is when the refusal was recorded, in Unix seconds.
	At int64 `msgpack:"at"`
	// UpheldBy and UpheldEpoch name the last leadership term that was asked
	// afterwards and could not confirm this node's last entry. Empty until
	// that has happened.
	UpheldBy    string `msgpack:"upheld_by,omitempty"`
	UpheldEpoch uint64 `msgpack:"upheld_epoch,omitempty"`
	// Position names what LastApplied is: positionApplyCursor, the last entry
	// this node applied. Recorded so that a later kind of refusal (one judged
	// at the head of a node's own log) is never read as this one.
	Position string `msgpack:"position,omitempty"`

	// placeholder marks a record that stands in for one this build could not
	// read (loadHalt): the read failed, or the value did not decode. It halts
	// the node like any record, but it is not a verdict anyone gave, so it is
	// never written to disk (upholdHalt). Not serialised.
	placeholder bool
}

// positionApplyCursor is HaltRecord.Position for a refusal judged at the
// node's apply cursor.
const positionApplyCursor = "apply_cursor"

// errLeading is returned by SetHalt on a node that holds the leader role. A
// refusal is a verdict on a follower's history, and a leader's history is the
// cluster's. A join answered before this node took the role can still arrive
// afterwards, and its verdict must not be recorded.
var errLeading = errors.New("applier: this node holds the leader role; a refusal is not recorded")

// unreadableHaltWhy stands in for the proof when the stored record cannot be
// decoded, for example one written by a later build.
const unreadableHaltWhy = "this node holds a record of having been refused by a leader, written in a form this build cannot read"

// text is the refusal in words: the proof, who upheld it since, and what an
// operator can do about it.
func (r HaltRecord) text() string {
	why := r.Why
	if why == "" {
		why = unreadableHaltWhy
	}
	if r.UpheldBy != "" {
		why += fmt.Sprintf(". Cortex %q (epoch %d) was asked since and could not confirm this node's last entry, so the refusal stands",
			r.UpheldBy, r.UpheldEpoch)
	}
	return why + ". " + aheadRemedy
}

// err is the error a join fails with while r stands.
func (r HaltRecord) err() error {
	return fmt.Errorf("%w: %s", ErrFollowerAhead, r.text())
}

// loadHalt reads the stored record. The key existing is the halt: a value
// that does not decode still halts the node, and so does a read that fails for
// any reason other than the key being absent. Not being able to tell is not
// evidence that the node was never refused.
func loadHalt(get func(key []byte) ([]byte, io.Closer, error)) *HaltRecord {
	val, closer, err := get(replHaltKey())
	if errors.Is(err, pebble.ErrNotFound) {
		return nil
	}
	if err != nil {
		slog.Error("applier: cannot read the stored refusal record; this node is treated as refused for the life of this process. Nothing retries the read: restart it once the store can be read",
			"err", err)
		return &HaltRecord{Why: fmt.Sprintf("this node could not read its stored refusal record (%v)", err), placeholder: true}
	}
	defer closer.Close()
	var rec HaltRecord
	if err := msgpack.Unmarshal(val, &rec); err != nil || rec.Version != haltRecordVersion {
		slog.Error("applier: this node holds a refusal record this build cannot read; it stays refused",
			"err", err, "version", rec.Version, "bytes", len(val))
		return &HaltRecord{Why: unreadableHaltWhy, placeholder: true}
	}
	return &rec
}

// Halt returns the recorded refusal, if this node holds one. Thread-safe.
func (a *Applier) Halt() (HaltRecord, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.halt == nil {
		return HaltRecord{}, false
	}
	return *a.halt, true
}

// SetHalt records rec, in memory and on disk, replacing any earlier record.
//
// Memory is set first and stays set if the write fails: the node is refused
// for the life of this process either way, and the error tells the caller that
// a restart would lose it.
func (a *Applier) SetHalt(rec HaltRecord) error {
	rec.Version = haltRecordVersion
	val, err := msgpack.Marshal(rec)
	if err != nil {
		return fmt.Errorf("applier: encode refusal record: %w", err)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.leading {
		return errLeading
	}
	// A refusal from a term at or below one this node has led was answered
	// before this node took the role, and arrived after it gave it up again.
	// The node's history since then is the history of its own term: the
	// verdict is about a node that no longer exists.
	if rec.Epoch != 0 && rec.Epoch <= a.ledEpoch {
		return fmt.Errorf("%w: the refusal is from epoch %d and this node has led epoch %d since", errLeading, rec.Epoch, a.ledEpoch)
	}
	a.halt = &rec
	if err := a.db.Set(replHaltKey(), val, pebble.Sync); err != nil {
		return fmt.Errorf("applier: persist refusal record: %w", err)
	}
	return nil
}

// upholdHalt notes, in the record that stands, the leadership term that was
// asked since and could not confirm this node's last entry. It is one step
// under the lock: a record removed in the meantime (this node took the role,
// or a snapshot replaced its state) is not written back. It returns the record
// as it now stands, and false when there is none.
func (a *Applier) upholdHalt(by string, epoch uint64) (HaltRecord, bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.halt == nil || a.leading {
		return HaltRecord{}, false, nil
	}
	// A record that stands in for one this build could not read is not
	// written: it is not a verdict, and writing it would turn a read that
	// failed once into a refusal that outlasts a restart on a healthy disk.
	if a.halt.placeholder || (a.halt.UpheldBy == by && a.halt.UpheldEpoch == epoch) {
		return *a.halt, true, nil
	}
	rec := *a.halt
	rec.UpheldBy, rec.UpheldEpoch = by, epoch
	rec.Version = haltRecordVersion
	val, err := msgpack.Marshal(rec)
	if err != nil {
		return *a.halt, true, fmt.Errorf("applier: encode refusal record: %w", err)
	}
	a.halt = &rec
	if err := a.db.Set(replHaltKey(), val, pebble.Sync); err != nil {
		return rec, true, fmt.Errorf("applier: persist refusal record: %w", err)
	}
	return rec, true, nil
}

// takeLead marks this applier as the leader's, removes any refusal record, and
// then runs fn with the apply cursor, as HoldCursor does: all one step under
// the lock. fn is what flips the role, so by the time anything can see this
// node as the leader its record is already gone from disk and from memory.
// Refusing SetHalt from here on is what keeps a join that was answered before
// this node took the role from recording its refusal on a leader
// (TestPromotionDuringARefusedJoinLeavesNoRecord).
//
// It returns the record that was removed, if there was one. A failure to
// remove it from disk is returned with the record and leaves it in memory too,
// so that the two do not disagree. fn runs either way: the promotion is not
// undone over a failed delete.
func (a *Applier) takeLead(epoch uint64, fn func(lastApplied uint64)) (was HaltRecord, removed bool, err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.leading = true
	if epoch > a.ledEpoch {
		a.ledEpoch = epoch
	}
	if a.halt != nil {
		was = *a.halt
		if derr := a.db.Delete(replHaltKey(), pebble.Sync); derr != nil {
			err = fmt.Errorf("applier: remove refusal record: %w", derr)
		} else {
			a.halt, removed = nil, true
		}
	}
	fn(a.lastApplied)
	return was, removed, err
}

// stepDown marks this applier as a follower's again. Called when the node
// gives up the leader role.
func (a *Applier) stepDown() {
	a.mu.Lock()
	a.leading = false
	a.mu.Unlock()
}

// ClearHalt removes the recorded refusal and reports whether there was one.
//
// The stored record goes first. If that fails the node stays refused in memory
// as well, so the two cannot disagree in the direction that matters: a node
// that follows now and finds itself refused again after a restart.
func (a *Applier) ClearHalt() (was HaltRecord, cleared bool, err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.halt == nil {
		return HaltRecord{}, false, nil
	}
	if err := a.db.Delete(replHaltKey(), pebble.Sync); err != nil {
		return *a.halt, false, fmt.Errorf("applier: remove refusal record: %w", err)
	}
	was = *a.halt
	a.halt = nil
	return was, true, nil
}
