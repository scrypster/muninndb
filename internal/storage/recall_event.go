package storage

import (
	"context"
	"encoding/binary"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/cockroachdb/pebble"
	"github.com/oklog/ulid/v2"
	"github.com/vmihailenco/msgpack/v5"

	"github.com/scrypster/muninndb/internal/storage/keys"
)

const (
	// recallEventRetentionDays is how long persisted recall events are kept.
	// Pinned rather than configurable: calibration ground truth wants weeks
	// to months of events, and the records are small (one per recall).
	recallEventRetentionDays = 90

	// recallEventPruneInterval amortizes retention pruning: every Nth
	// WriteRecallEvent per process also prunes the writing vault's expired
	// events. Keeps retention maintenance inside the write path — no
	// background job or external hook required.
	recallEventPruneInterval = 256
)

// recallEventWriteSeq drives the amortized prune schedule.
var recallEventWriteSeq atomic.Uint64

// RecallEventPurpose declares why a reader is accessing recall events.
// Recall events are calibration instrument data, not a memory surface:
// nothing may recall FROM them, and every read path is purpose-gated
// against a closed allowlist by construction — an unknown purpose is
// refused loudly, not logged quietly. Widening the allowlist requires
// editing validRecallPurposes, so any change to who may read this data is
// a reviewable diff rather than a remembered promise (issue #573).
type RecallEventPurpose string

// RecallPurposeCalibration marks reads by scoring-calibration tooling —
// fitting weights against recorded ground truth.
const RecallPurposeCalibration RecallEventPurpose = "calibration"

// RecallPurposeTrial marks reads by the offline cognition trial: the
// build-tagged harness that puts the cognitive layer (Hebbian co-activation,
// PAS predictive activation, the ACT-R base-level prior) on trial against
// real recorded queries, and the replay driver that rebuilds the association
// graph from recorded co-activation sets. It is a SEPARATE purpose from
// calibration on purpose — the gate exists so that reads of instrument data
// are deliberate and named, and reusing "calibration" for a different
// analysis would make the audit log lie about who read what.
const RecallPurposeTrial RecallEventPurpose = "cognition-trial"

// validRecallPurposes is the closed allowlist for recall-event reads.
var validRecallPurposes = map[RecallEventPurpose]struct{}{
	RecallPurposeCalibration: {},
	RecallPurposeTrial:       {},
}

// checkRecallPurpose refuses reads whose purpose is not in the allowlist.
func checkRecallPurpose(action string, purpose RecallEventPurpose) error {
	if _, ok := validRecallPurposes[purpose]; !ok {
		slog.Error("storage: recall-event read refused — purpose not in allowlist",
			"action", action, "purpose", string(purpose))
		return fmt.Errorf("%s: recall events are purpose-gated instrument data; purpose %q is not in the allowlist", action, purpose)
	}
	return nil
}

// RecallSurfacedEntry is one engram surfaced by a recall, with the final
// score the caller saw (post entity-boost, post truncation).
type RecallSurfacedEntry struct {
	ID    string  `msgpack:"id"`
	Score float32 `msgpack:"score"`
}

// RecallEvent is the persisted record of what one recall surfaced (issue
// #573).
//
// HONESTY CORRECTION. This comment used to claim that, together with the
// decision->evidence edges written by Decide(), the record "makes scoring
// ground truth regenerable by rule: positives = surfaced AND cited; negatives
// = surfaced minus cited, per event." That rule is ASPIRATIONAL and is NOT
// implementable from what is on disk. Four independent reasons, each verified
// against the code rather than assumed:
//
//  1. There is no join key. Activate returns query_id = eventID.String(), but
//     Decide()'s evidence_ids are MEMORY ids and no surface accepts a
//     query_id at all — the citation side never records which recall surfaced
//     the cited memory.
//  2. Neither side carries an identity. RecallEvent stores no session or agent
//     ID, and the SessionID that exists is an MBP HELLO-handshake connection ID
//     never threaded into recall, read, feedback or decide.
//  3. The residue is context-free. A citation survives only as an
//     AccessCount/LastAccess bump or a RelSupports edge from a decision
//     engram; neither carries the query. A time-window + ID-membership
//     heuristic is ambiguous by construction as soon as two recalls in a vault
//     surface overlapping sets close together, which is the normal case.
//  4. The citation side is itself damaged. Decide()'s evidence links are
//     written at weight 1.0 — exactly the class #757 wrote at the weight-0.0
//     key position — and #759's repair explicitly cannot recover or even
//     identify the ones a decay pass already touched.
//
// What the record IS good for, and what it is used for: the ranked ID list is
// bit-for-bit the co-activation set the Hebbian worker was given (same slice,
// same order, lossless to float32), and Context is real caller query text. So
// it is a training signal and a query set — not a labeled relevance set. Any
// ground truth has to be labeled separately.
//
// Reads are purpose-gated; see RecallEventPurpose.
type RecallEvent struct {
	// Context is the query context the caller passed to the recall.
	Context []string `msgpack:"context,omitempty"`
	// Threshold is the effective score threshold the recall ran with.
	Threshold float32 `msgpack:"threshold,omitempty"`
	// SurfacedAt is the event wall-clock time in Unix nanoseconds.
	SurfacedAt int64 `msgpack:"surfaced_at"`
	// Entries are the surfaced engrams in returned order.
	Entries []RecallSurfacedEntry `msgpack:"entries"`
}

// WriteRecallEvent persists ev under (wsPrefix, eventID). Event IDs are
// ULIDs, so keys within a vault sort by event time. Every
// recallEventPruneInterval-th call also prunes the vault's events older
// than the retention window.
//
// The write goes through a replicated batch like every other storage write:
// in a cluster, recalls are exactly the read traffic served by Lobe replicas,
// and an unreplicated event write would strand the calibration record on
// whichever node happened to serve the recall.
func (ps *PebbleStore) WriteRecallEvent(ctx context.Context, wsPrefix [8]byte, eventID ULID, ev *RecallEvent) error {
	if ev == nil {
		return fmt.Errorf("write recall event: nil event")
	}
	val, err := msgpack.Marshal(ev)
	if err != nil {
		return fmt.Errorf("recall event marshal: %w", err)
	}
	key := keys.RecallEventKey(wsPrefix, [16]byte(eventID))
	batch := ps.db.NewBatch()
	defer batch.Close()
	batch.Set(key, val, nil)
	if err := batch.Commit(pebble.NoSync); err != nil {
		return fmt.Errorf("write recall event: %w", err)
	}
	ps.replicateBatch(batch)
	if recallEventWriteSeq.Add(1)%recallEventPruneInterval == 0 {
		cutoff := time.Now().AddDate(0, 0, -recallEventRetentionDays)
		if _, err := ps.PruneRecallEvents(ctx, wsPrefix, cutoff); err != nil {
			return fmt.Errorf("write recall event: amortized prune: %w", err)
		}
	}
	return nil
}

// GetRecallEvent fetches one recall event. Returns nil, nil when absent.
// Reads are purpose-gated and logged — see RecallEventPurpose.
func (ps *PebbleStore) GetRecallEvent(ctx context.Context, wsPrefix [8]byte, eventID ULID, purpose RecallEventPurpose) (*RecallEvent, error) {
	if err := checkRecallPurpose("get recall event", purpose); err != nil {
		return nil, err
	}
	slog.Info("storage: recall event read", "action", "get", "purpose", string(purpose), "event_id", eventID.String())
	key := keys.RecallEventKey(wsPrefix, [16]byte(eventID))
	val, closer, err := ps.db.Get(key)
	if err == pebble.ErrNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get recall event: %w", err)
	}
	defer closer.Close()
	var ev RecallEvent
	if err := msgpack.Unmarshal(val, &ev); err != nil {
		return nil, fmt.Errorf("recall event unmarshal: %w", err)
	}
	return &ev, nil
}

// ScanRecallEvents iterates all recall events in a vault in event-time order
// (ULID key order). fn may return a non-nil error to stop the scan; that
// error is returned to the caller.
// Reads are purpose-gated and logged — see RecallEventPurpose.
func (ps *PebbleStore) ScanRecallEvents(ctx context.Context, wsPrefix [8]byte, purpose RecallEventPurpose, fn func(eventID ULID, ev *RecallEvent) error) error {
	if err := checkRecallPurpose("scan recall events", purpose); err != nil {
		return err
	}
	slog.Info("storage: recall event read", "action", "scan", "purpose", string(purpose))
	prefix := keys.RecallEventPrefix(wsPrefix)
	iter, err := ps.db.NewIter(&pebble.IterOptions{
		LowerBound: prefix,
		UpperBound: prefixUpperBound(prefix),
	})
	if err != nil {
		return fmt.Errorf("scan recall events: iter: %w", err)
	}
	defer iter.Close()

	for valid := iter.First(); valid; valid = iter.Next() {
		if err := ctx.Err(); err != nil {
			return err
		}
		k := iter.Key()
		if len(k) != 25 { // 1 + 8 + 16
			continue
		}
		var id ULID
		copy(id[:], k[9:25])
		var ev RecallEvent
		if err := msgpack.Unmarshal(iter.Value(), &ev); err != nil {
			return fmt.Errorf("recall event unmarshal (%s): %w", id.String(), err)
		}
		if err := fn(id, &ev); err != nil {
			return err
		}
	}
	return nil
}

// PruneRecallEvents deletes all recall events in a vault whose event time is
// before the cutoff, using the ULID timestamp embedded in the key — no value
// reads needed. Returns the number of events deleted.
func (ps *PebbleStore) PruneRecallEvents(ctx context.Context, wsPrefix [8]byte, before time.Time) (int, error) {
	prefix := keys.RecallEventPrefix(wsPrefix)
	// Smallest possible ULID at the cutoff timestamp: 6-byte big-endian
	// milliseconds, zero entropy. Every key below it is older than cutoff.
	var bound [16]byte
	ms := ulid.Timestamp(before)
	binary.BigEndian.PutUint64(bound[:8], ms<<16) // left-align 48-bit ms into first 6 bytes
	upper := keys.RecallEventKey(wsPrefix, bound)

	iter, err := ps.db.NewIter(&pebble.IterOptions{LowerBound: prefix, UpperBound: upper})
	if err != nil {
		return 0, fmt.Errorf("prune recall events: iter: %w", err)
	}
	batch := ps.db.NewBatch()
	count := 0
	for valid := iter.First(); valid; valid = iter.Next() {
		if err := ctx.Err(); err != nil {
			iter.Close()
			batch.Close()
			return 0, err
		}
		if err := batch.Delete(iter.Key(), nil); err != nil {
			iter.Close()
			batch.Close()
			return 0, fmt.Errorf("prune recall events: delete: %w", err)
		}
		count++
	}
	iter.Close()
	if count == 0 {
		batch.Close()
		return 0, nil
	}
	if err := batch.Commit(pebble.NoSync); err != nil {
		return 0, fmt.Errorf("prune recall events: commit: %w", err)
	}
	slog.Info("storage: recall events pruned", "count", count)
	return count, nil
}

// prefixUpperBound returns the smallest key greater than every key with the
// given prefix, for use as an exclusive iterator upper bound.
func prefixUpperBound(prefix []byte) []byte {
	upper := make([]byte, len(prefix))
	copy(upper, prefix)
	for i := len(upper) - 1; i >= 0; i-- {
		upper[i]++
		if upper[i] != 0 {
			return upper[:i+1]
		}
	}
	return nil // prefix was all 0xFF — no upper bound
}
