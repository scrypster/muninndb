package storage

import (
	"bytes"
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

	// recallLabelledRetentionDays is the retention of an event that carries
	// any child row (a label, or the read-only header that stands in for a
	// parent). Labelled data deliberately outlives unlabelled data so the
	// day-90 premise re-run of the salience design sits inside the window.
	// Cost, stated: a labelled event keeps its raw query text (Context) for
	// 180 days instead of 90. A rollback binary that predates this constant
	// prunes labelled events at 90 days — data loss, accepted and stated.
	recallLabelledRetentionDays = 180

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

// RecallPurposeLabel marks reads by muninn_feedback's label pipeline: the
// membership check that a labelled memory was among the entries an event
// served, and the rank / set size / source flags copied onto the label. It
// is the ONLY caller of this purpose (Engine.RecordFeedback); the read never
// leaves the engine — the feedback response is uniform and carries none of
// it.
const RecallPurposeLabel RecallEventPurpose = "label"

// validRecallPurposes is the closed allowlist for recall-event reads.
var validRecallPurposes = map[RecallEventPurpose]struct{}{
	RecallPurposeCalibration: {},
	RecallPurposeTrial:       {},
	RecallPurposeLabel:       {},
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
//
// Source, Abs, CM and Prior were added by the label pipeline (slice 2a). They
// are additive omitempty msgpack fields: an event written before them decodes
// with zeros, and an older binary decoding a new event ignores them (msgpack
// skips unknown map keys; pinned by TestRecallEvent_OldDecoderIgnoresNewFields).
// Because Source 0 is a legal value (admission "injected", no boosts), a
// reader must consult RecallEvent.Version before trusting it — see
// RecallEvent.EntrySource.
type RecallSurfacedEntry struct {
	ID    string  `msgpack:"id"`
	Score float32 `msgpack:"score"`
	// Source is the row's source flags (RecallSource* bits).
	Source uint8 `msgpack:"s,omitempty"`
	// Abs is the row's AbsoluteScore (the gate's evidence).
	Abs float32 `msgpack:"a,omitempty"`
	// CM is the row's ContentMatch.
	CM float32 `msgpack:"c,omitempty"`
	// Prior is the prior multiplier Raw/ContentMatch (0 when CM is 0).
	Prior float32 `msgpack:"b,omitempty"`
}

// Recall-entry source flags (RecallSurfacedEntry.Source).
const (
	// RecallSourceAdmissionMask selects bits 0-1: how the row entered the
	// candidate set (activation.Admission: 0 injected, 1 scored, 2 tag filter).
	RecallSourceAdmissionMask uint8 = 0x03
	// RecallSourceVersionHead: the row is a version head substituted for a
	// superseded member (SubstitutedFor != 0).
	RecallSourceVersionHead uint8 = 1 << 2
	// RecallSourceEntityBoost: entity boost > 0.
	RecallSourceEntityBoost uint8 = 1 << 3
	// RecallSourcePAS: PAS transition boost > 0.
	RecallSourcePAS uint8 = 1 << 4
	// RecallSourceHebbian: Hebbian boost > 0.
	RecallSourceHebbian uint8 = 1 << 5
	// RecallSourceUnknown is recorded on a label when the source flags are
	// not known: a read-only query id (no event) or an event written before
	// the flags existed.
	RecallSourceUnknown uint8 = 0xFF
)

// RecallEventVersionLabels is the RecallEvent.Version from which per-entry
// Source/Abs/CM/Prior and the event's RecallCred/RecallSess are recorded.
const RecallEventVersionLabels uint8 = 1

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
	// Version is 0 for events written before the label pipeline and
	// RecallEventVersionLabels for events that carry the fields below and the
	// per-entry source flags.
	Version uint8 `msgpack:"v,omitempty"`
	// RecallCred and RecallSess are the 8-byte recall credential and session
	// hashes (auth.QueryIDKey.CredHash / SessionHash), never the raw values.
	// Empty when unknown (no key wired, or a transport that carries no
	// session — D4).
	RecallCred []byte `msgpack:"rc,omitempty"`
	RecallSess []byte `msgpack:"rs,omitempty"`
}

// EntrySource returns entry i's source flags, or RecallSourceUnknown when the
// event predates the flags.
func (ev *RecallEvent) EntrySource(i int) uint8 {
	if ev == nil || ev.Version < RecallEventVersionLabels || i < 0 || i >= len(ev.Entries) {
		return RecallSourceUnknown
	}
	return ev.Entries[i].Source
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
		now := time.Now()
		if _, err := ps.PruneRecallEvents(ctx, wsPrefix,
			now.AddDate(0, 0, -recallEventRetentionDays),
			now.AddDate(0, 0, -recallLabelledRetentionDays)); err != nil {
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
		// Only the 25-byte parent carries a RecallEvent value. Child rows
		// (the read-only header and labels) share the prefix and must never
		// reach the RecallEvent decoder.
		if len(k) != keys.RecallEventKeyLen {
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

// CountRecallEventsSince counts the vault's recall events at or after since.
// A recall event exists only for a non-read-only recall that returned at least
// one result, and events are pruned after recallEventRetentionDays — so this is
// "recalls with results", bounded by retention, and never "all recalls".
//
// It is deliberately NOT purpose-gated: it iterates keys only (the event time
// is the ULID in the key, as in PruneRecallEvents) and never reads or decodes
// an event value, so no query context or surfaced id leaves storage. The gate
// protects the content of the instrument data; a count of keys carries none.
func (ps *PebbleStore) CountRecallEventsSince(ctx context.Context, wsPrefix [8]byte, since time.Time) (int, error) {
	lower := keys.RecallEventKey(wsPrefix, ulidTimeBound(since))
	iter, err := ps.db.NewIter(&pebble.IterOptions{
		LowerBound: lower,
		UpperBound: prefixUpperBound(keys.RecallEventPrefix(wsPrefix)),
	})
	if err != nil {
		return 0, fmt.Errorf("count recall events: iter: %w", err)
	}
	defer iter.Close()
	n := 0
	for valid := iter.First(); valid; valid = iter.Next() {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		if len(iter.Key()) == keys.RecallEventKeyLen { // parents only; child rows are not recalls
			n++
		}
	}
	return n, nil
}

// PruneRecallEvents deletes a vault's expired recall events, using the ULID
// timestamp embedded in the key — no value reads needed. Keys are grouped by
// their 25-byte event prefix (the parent plus its child rows):
//
//   - a group with NO child rows (an ordinary unlabelled event) is deleted
//     when its event time is before unlabelledBefore (90 days, as before);
//   - a group with ANY child row (a label, or the read-only header) is
//     deleted — parent and every child together — only when its event time is
//     before labelledBefore (180 days).
//
// Returns the number of event groups deleted.
//
// INHERITED GAP (D5, not fixed here): this commits WITHOUT replicateBatch, so
// in a cluster followers keep events the leader pruned. WriteRecallEvent and
// UpsertRecallLabel do replicate; only retention is node-local.
//
// Race, stated: a label upserted onto an unlabelled event in the same instant
// a prune pass decided that event's group was unlabelled-and-expired leaves
// an orphan label with no parent. It is benign — a later label lookup reports
// unknown_event, and the orphan is deleted by the 180-day rule.
func (ps *PebbleStore) PruneRecallEvents(ctx context.Context, wsPrefix [8]byte, unlabelledBefore, labelledBefore time.Time) (int, error) {
	prefix := keys.RecallEventPrefix(wsPrefix)
	// Iterate up to the LATER of the two cutoffs; groups between them are
	// kept or deleted per their children. The bound is the smallest ULID at
	// the cutoff timestamp (6-byte big-endian ms, zero entropy), so every
	// key below it — parent or child — belongs to an older event.
	later := unlabelledBefore
	if labelledBefore.After(later) {
		later = labelledBefore
	}
	upper := keys.RecallEventKey(wsPrefix, ulidTimeBound(later))
	unlabelledMs := ulid.Timestamp(unlabelledBefore)
	labelledMs := ulid.Timestamp(labelledBefore)

	iter, err := ps.db.NewIter(&pebble.IterOptions{LowerBound: prefix, UpperBound: upper})
	if err != nil {
		return 0, fmt.Errorf("prune recall events: iter: %w", err)
	}
	batch := ps.db.NewBatch()
	count := 0
	var (
		groupID   []byte   // the 16-byte event id of the current group
		groupKeys [][]byte // every key of the current group
		hasChild  bool
	)
	flush := func() error {
		if groupID == nil {
			return nil
		}
		ms := ulid.ULID(*(*[16]byte)(groupID)).Time()
		cutoff := unlabelledMs
		if hasChild {
			cutoff = labelledMs
		}
		if ms < cutoff {
			for _, k := range groupKeys {
				if err := batch.Delete(k, nil); err != nil {
					return fmt.Errorf("prune recall events: delete: %w", err)
				}
			}
			count++
		}
		groupID, groupKeys, hasChild = nil, groupKeys[:0], false
		return nil
	}
	for valid := iter.First(); valid; valid = iter.Next() {
		if err := ctx.Err(); err != nil {
			iter.Close()
			batch.Close()
			return 0, err
		}
		k := iter.Key()
		if len(k) < keys.RecallEventKeyLen {
			continue // not a recall-event shape; never ours to delete
		}
		if groupID == nil || !bytes.Equal(groupID, k[9:25]) {
			if err := flush(); err != nil {
				iter.Close()
				batch.Close()
				return 0, err
			}
			groupID = append([]byte(nil), k[9:25]...)
		}
		if len(k) != keys.RecallEventKeyLen {
			hasChild = true
		}
		groupKeys = append(groupKeys, append([]byte(nil), k...))
	}
	if err := flush(); err != nil {
		iter.Close()
		batch.Close()
		return 0, err
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

// ulidTimeBound is the smallest ULID at t's millisecond: 6-byte big-endian
// milliseconds, zero entropy.
func ulidTimeBound(t time.Time) [16]byte {
	var bound [16]byte
	binary.BigEndian.PutUint64(bound[:8], ulid.Timestamp(t)<<16) // left-align 48-bit ms into first 6 bytes
	return bound
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
