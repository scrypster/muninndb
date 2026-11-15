package storage

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/cockroachdb/pebble"
	"github.com/vmihailenco/msgpack/v5"

	"github.com/scrypster/muninndb/internal/storage/keys"
)

// Label pipeline storage (slice 2a). A label is one agent's statement that a
// memory a recall returned was (or was not) useful. It is stored as a CHILD
// row of the 0x29 recall event it refers to — no new prefix byte — so it
// inherits 0x29's deliberate lifecycle: cleared with the vault, never
// exported, never cloned or merged. See keys.RecallLabelKey for the layout.
//
// Nothing in ranking reads labels in this slice. They are recorded with
// enough context (rank, set size, source, who recalled, who labelled) that a
// later slice can correct for position and exposure bias: a field not
// recorded now cannot be recovered later.

// Label membership (RecallLabel.Membership).
const (
	// LabelMembershipVerified: the memory was checked against the served
	// entries of the persisted recall event.
	LabelMembershipVerified uint8 = 1
	// LabelMembershipToken: a read-only (ro1.) query id — no event exists;
	// membership and rank were verified against the keyed row tags the id
	// carries (auth.QueryIDKey.ReadOnlyRank). Source flags stay unknown.
	LabelMembershipToken uint8 = 2
)

// Label self-labelling (RecallLabel.Self).
const (
	LabelSelfUnknown uint8 = 0 // writer or recall session unknown
	LabelSelfSame    uint8 = 1 // the memory was written in the recall's session
	LabelSelfOther   uint8 = 2 // it was not
)

// Label channel (RecallLabel.Channel).
const (
	LabelChannelDirect  uint8 = 1
	LabelChannelHandoff uint8 = 2
)

// RecallLabel is the value of a label child row. Short msgpack keys: the row
// is ~95 bytes, ~145 with its key.
//
// Repeats of the same (event, memory, labelling credential) never create a
// new row: Polarity is last-write-wins, Count and LastAt advance, and every
// other field keeps the value recorded by the FIRST post.
type RecallLabel struct {
	Polarity   uint8  `msgpack:"p"`    // 1 useful, 0 not useful — last write wins
	Count      uint32 `msgpack:"n"`    // posts of this (E, id, lc), repeats included
	FirstAt    int64  `msgpack:"t0"`   // unix ms
	LastAt     int64  `msgpack:"t1"`   // unix ms
	Membership uint8  `msgpack:"m"`    // LabelMembership*
	Rank       uint16 `msgpack:"r"`    // 1-based served rank; 0 unknown
	SetSize    uint16 `msgpack:"k"`    // result-set size; 0 unknown
	Source     uint8  `msgpack:"s"`    // the entry's RecallSource* flags; 0xFF unknown
	RecallCred []byte `msgpack:"rc"`   // recall credential hash (8 B) or empty
	RecallSess []byte `msgpack:"rs"`   // recall session hash (8 B) or empty
	LabelSess  []byte `msgpack:"ls"`   // labelling session hash (8 B) or empty
	Self       uint8  `msgpack:"self"` // LabelSelf*
	Superseded uint8  `msgpack:"sup"`  // 1 if superseded/expired/soft-deleted when labelled
	Channel    uint8  `msgpack:"ch"`   // LabelChannel*
}

// RecallROHeader is the value of the read-only header child row: the
// stand-in for the parent event a read-only recall never writes. It is
// written by muninn_feedback, in the same batch as the first label for that
// id — never by the recall.
type RecallROHeader struct {
	Unverified uint8  `msgpack:"u"`  // always 1: no event — membership came from the id's row tags, source flags unknown
	RecallCred []byte `msgpack:"rc"` // authenticated inside the ro1. id
	RecallSess []byte `msgpack:"rs"`
	At         int64  `msgpack:"at"` // unix ms, from the id's ULID time
}

// RecallEventEntries fetches several recall events in one call, for the
// label pipeline's membership check. Absent ids are absent from the map.
// One INFO line per call (not per event, unlike GetRecallEvent).
func (ps *PebbleStore) RecallEventEntries(ctx context.Context, wsPrefix [8]byte, ids []ULID, purpose RecallEventPurpose) (map[ULID]*RecallEvent, error) {
	if err := checkRecallPurpose("recall event entries", purpose); err != nil {
		return nil, err
	}
	slog.Info("storage: recall event read", "action", "entries", "purpose", string(purpose), "events", len(ids))
	out := make(map[ULID]*RecallEvent, len(ids))
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if _, seen := out[id]; seen {
			continue
		}
		val, closer, err := ps.db.Get(keys.RecallEventKey(wsPrefix, [16]byte(id)))
		if err == pebble.ErrNotFound {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("recall event entries: %w", err)
		}
		var ev RecallEvent
		uerr := msgpack.Unmarshal(val, &ev)
		closer.Close()
		if uerr != nil {
			return nil, fmt.Errorf("recall event unmarshal (%s): %w", id.String(), uerr)
		}
		out[id] = &ev
	}
	return out, nil
}

// UpsertRecallLabel records one label under (eventID, engramID, lc).
//
// Under casLocks.For(labelKey) it reads the existing row: absent → fresh is
// stored with Count 1 and FirstAt = LastAt = fresh.LastAt (first = true);
// present → Polarity := fresh.Polarity, Count++, LastAt := fresh.LastAt, every
// other field kept. When header is non-nil and no header row exists yet, the
// header is written in the SAME batch. The batch is replicated (SEC-13); the
// caller is responsible for the leader gate.
//
// The lock is the label key's stripe only. Callers must not hold an engram
// stripe across this call (stripes are shared with engram CAS locks).
func (ps *PebbleStore) UpsertRecallLabel(ctx context.Context, wsPrefix [8]byte, eventID, engramID ULID, lc [8]byte, fresh RecallLabel, header *RecallROHeader) (stored RecallLabel, first bool, err error) {
	if err := ctx.Err(); err != nil {
		return RecallLabel{}, false, err
	}
	key := keys.RecallLabelKey(wsPrefix, [16]byte(eventID), [16]byte(engramID), lc)
	mu := ps.casLocks.For(key)
	mu.Lock()
	defer mu.Unlock()

	existing, found, err := ps.getRecallLabelRaw(key)
	if err != nil {
		return RecallLabel{}, false, err
	}
	if found {
		stored = existing
		stored.Polarity = fresh.Polarity
		stored.Count++
		stored.LastAt = fresh.LastAt
	} else {
		stored = fresh
		stored.Count = 1
		stored.FirstAt = fresh.LastAt
		first = true
	}
	val, err := msgpack.Marshal(&stored)
	if err != nil {
		return RecallLabel{}, false, fmt.Errorf("recall label marshal: %w", err)
	}
	batch := ps.db.NewBatch()
	defer batch.Close()
	if header != nil {
		hk := keys.RecallROHeaderKey(wsPrefix, [16]byte(eventID))
		_, closer, gerr := ps.db.Get(hk)
		switch {
		case gerr == pebble.ErrNotFound:
			hv, merr := msgpack.Marshal(header)
			if merr != nil {
				return RecallLabel{}, false, fmt.Errorf("recall ro header marshal: %w", merr)
			}
			if err := batch.Set(hk, hv, nil); err != nil {
				return RecallLabel{}, false, fmt.Errorf("recall ro header: %w", err)
			}
		case gerr != nil:
			return RecallLabel{}, false, fmt.Errorf("recall ro header read: %w", gerr)
		default:
			closer.Close()
		}
	}
	if err := batch.Set(key, val, nil); err != nil {
		return RecallLabel{}, false, fmt.Errorf("recall label: %w", err)
	}
	if err := batch.Commit(pebble.NoSync); err != nil {
		return RecallLabel{}, false, fmt.Errorf("recall label commit: %w", err)
	}
	ps.replicateBatch(batch)
	return stored, first, nil
}

func (ps *PebbleStore) getRecallLabelRaw(key []byte) (RecallLabel, bool, error) {
	val, closer, err := ps.db.Get(key)
	if err == pebble.ErrNotFound {
		return RecallLabel{}, false, nil
	}
	if err != nil {
		return RecallLabel{}, false, fmt.Errorf("recall label read: %w", err)
	}
	defer closer.Close()
	var l RecallLabel
	if err := msgpack.Unmarshal(val, &l); err != nil {
		return RecallLabel{}, false, fmt.Errorf("recall label unmarshal: %w", err)
	}
	return l, true, nil
}

// GetRecallLabel fetches one label row. Purpose-gated like every other read
// of 0x29 instrument data.
func (ps *PebbleStore) GetRecallLabel(ctx context.Context, wsPrefix [8]byte, eventID, engramID ULID, lc [8]byte, purpose RecallEventPurpose) (*RecallLabel, error) {
	if err := checkRecallPurpose("get recall label", purpose); err != nil {
		return nil, err
	}
	l, found, err := ps.getRecallLabelRaw(keys.RecallLabelKey(wsPrefix, [16]byte(eventID), [16]byte(engramID), lc))
	if err != nil || !found {
		return nil, err
	}
	return &l, nil
}

// GetRecallROHeader fetches a read-only header row. Returns nil, nil when
// absent.
func (ps *PebbleStore) GetRecallROHeader(ctx context.Context, wsPrefix [8]byte, eventID ULID, purpose RecallEventPurpose) (*RecallROHeader, error) {
	if err := checkRecallPurpose("get recall ro header", purpose); err != nil {
		return nil, err
	}
	val, closer, err := ps.db.Get(keys.RecallROHeaderKey(wsPrefix, [16]byte(eventID)))
	if err == pebble.ErrNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get recall ro header: %w", err)
	}
	defer closer.Close()
	var h RecallROHeader
	if err := msgpack.Unmarshal(val, &h); err != nil {
		return nil, fmt.Errorf("recall ro header unmarshal: %w", err)
	}
	return &h, nil
}

// RecallLabelRow is one label as returned by ScanRecallLabels.
type RecallLabelRow struct {
	EventID   ULID
	EngramID  ULID
	LabelCred [8]byte
	Label     RecallLabel
}

// ScanRecallLabels iterates every label child row of a vault in event order.
// Purpose-gated and logged.
func (ps *PebbleStore) ScanRecallLabels(ctx context.Context, wsPrefix [8]byte, purpose RecallEventPurpose, fn func(RecallLabelRow) error) error {
	if err := checkRecallPurpose("scan recall labels", purpose); err != nil {
		return err
	}
	slog.Info("storage: recall label read", "action", "scan", "purpose", string(purpose))
	prefix := keys.RecallEventPrefix(wsPrefix)
	iter, err := ps.db.NewIter(&pebble.IterOptions{LowerBound: prefix, UpperBound: prefixUpperBound(prefix)})
	if err != nil {
		return fmt.Errorf("scan recall labels: iter: %w", err)
	}
	defer iter.Close()
	for valid := iter.First(); valid; valid = iter.Next() {
		if err := ctx.Err(); err != nil {
			return err
		}
		k := iter.Key()
		if len(k) != keys.RecallLabelKeyLen || k[25] != keys.RecallChildLabel {
			continue
		}
		var row RecallLabelRow
		copy(row.EventID[:], k[9:25])
		copy(row.EngramID[:], k[26:42])
		copy(row.LabelCred[:], k[42:50])
		if err := msgpack.Unmarshal(iter.Value(), &row.Label); err != nil {
			return fmt.Errorf("recall label unmarshal (%s): %w", row.EventID.String(), err)
		}
		if err := fn(row); err != nil {
			return err
		}
	}
	return nil
}

// RecallDayMarker is the value of a day-marker child row: one per (memory,
// labelling credential, UTC day) that saw feedback. It is the durable answer
// to "has this credential already earned this memory's access bump today?"
// and the per-day use record a later slice counts (read-only recalls are
// cheap, so one credential can produce many distinct (query, memory) labels
// for one memory in a day; it produces ONE marker).
type RecallDayMarker struct {
	Posts    uint32 `msgpack:"n"` // feedback posts that day (labels and legacy), repeats included
	FirstAt  int64  `msgpack:"f"` // unix ms of the day's first post
	BumpedAt int64  `msgpack:"b"` // unix ms the day's access bump was granted; 0 = not granted
}

// UpsertRecallDayMarker records one feedback post for (memory, lc, day) and
// decides — durably, under the marker key's stripe lock, in one replicated
// batch — whether it earns the day's access bump: granted iff wantBump and no
// bump was granted earlier that day. Returns the stored marker, whether this
// was the day's first post, and whether the bump was granted. The caller
// performs the bump AFTER this returns (never under the lock: stripes are
// shared with engram CAS locks), so a failed bump is a lost bump, never an
// extra one.
func (ps *PebbleStore) UpsertRecallDayMarker(ctx context.Context, wsPrefix [8]byte, unixDay int64, engramID ULID, lc [8]byte, nowMs int64, wantBump bool) (m RecallDayMarker, firstToday, granted bool, err error) {
	if err := ctx.Err(); err != nil {
		return RecallDayMarker{}, false, false, err
	}
	key := keys.RecallDayMarkerKey(wsPrefix, unixDay, [16]byte(engramID), lc)
	mu := ps.casLocks.For(key)
	mu.Lock()
	defer mu.Unlock()
	val, closer, gerr := ps.db.Get(key)
	switch {
	case gerr == pebble.ErrNotFound:
		firstToday = true
		m.FirstAt = nowMs
	case gerr != nil:
		return RecallDayMarker{}, false, false, fmt.Errorf("recall day marker read: %w", gerr)
	default:
		uerr := msgpack.Unmarshal(val, &m)
		closer.Close()
		if uerr != nil {
			return RecallDayMarker{}, false, false, fmt.Errorf("recall day marker unmarshal: %w", uerr)
		}
	}
	m.Posts++
	if wantBump && m.BumpedAt == 0 {
		m.BumpedAt = nowMs
		granted = true
	}
	out, err := msgpack.Marshal(&m)
	if err != nil {
		return RecallDayMarker{}, false, false, fmt.Errorf("recall day marker marshal: %w", err)
	}
	batch := ps.db.NewBatch()
	defer batch.Close()
	if err := batch.Set(key, out, nil); err != nil {
		return RecallDayMarker{}, false, false, err
	}
	if err := batch.Commit(pebble.NoSync); err != nil {
		return RecallDayMarker{}, false, false, fmt.Errorf("recall day marker commit: %w", err)
	}
	ps.replicateBatch(batch)
	return m, firstToday, granted, nil
}

// GetRecallDayMarker fetches one day marker (purpose-gated).
func (ps *PebbleStore) GetRecallDayMarker(ctx context.Context, wsPrefix [8]byte, unixDay int64, engramID ULID, lc [8]byte, purpose RecallEventPurpose) (*RecallDayMarker, error) {
	if err := checkRecallPurpose("get recall day marker", purpose); err != nil {
		return nil, err
	}
	val, closer, err := ps.db.Get(keys.RecallDayMarkerKey(wsPrefix, unixDay, [16]byte(engramID), lc))
	if err == pebble.ErrNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer closer.Close()
	var m RecallDayMarker
	if err := msgpack.Unmarshal(val, &m); err != nil {
		return nil, err
	}
	return &m, nil
}
