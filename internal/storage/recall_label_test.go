package storage

import (
	"bytes"
	"context"
	"sync"
	"testing"
	"time"

	"github.com/cockroachdb/pebble"
	"github.com/stretchr/testify/require"
	"github.com/vmihailenco/msgpack/v5"

	"github.com/scrypster/muninndb/internal/prefix"
	"github.com/scrypster/muninndb/internal/storage/keys"
)

// Label pipeline (slice 2a) storage tests. Every vault name and query string
// here is invented for the test.

var testLC = [8]byte{0xa4, 0x07, 1, 2, 3, 4, 5, 6}

func writeTestEvent(t *testing.T, store *PebbleStore, ws [8]byte, id ULID, entries ...ULID) {
	t.Helper()
	ev := &RecallEvent{
		Context:    []string{"trail drainage culvert spacing"},
		SurfacedAt: time.Now().UnixNano(),
		Version:    RecallEventVersionLabels,
	}
	for _, e := range entries {
		ev.Entries = append(ev.Entries, RecallSurfacedEntry{ID: e.String(), Score: 0.5, Source: 1})
	}
	require.NoError(t, store.WriteRecallEvent(context.Background(), ws, id, ev))
}

func upsertTestLabel(t *testing.T, store *PebbleStore, ws [8]byte, ev, mem ULID, lc [8]byte, header *RecallROHeader) (RecallLabel, bool) {
	t.Helper()
	l, first, err := store.UpsertRecallLabel(context.Background(), ws, ev, mem, lc,
		RecallLabel{Polarity: 1, LastAt: time.Now().UnixMilli(), Membership: LabelMembershipVerified, Channel: LabelChannelDirect}, header)
	require.NoError(t, err)
	return l, first
}

// allKeys returns every key in the store, in order, as copies.
func allKeys(t *testing.T, store *PebbleStore) [][]byte {
	t.Helper()
	iter, err := store.db.NewIter(nil)
	require.NoError(t, err)
	defer iter.Close()
	var out [][]byte
	for iter.First(); iter.Valid(); iter.Next() {
		out = append(out, append([]byte(nil), iter.Key()...))
	}
	return out
}

func vaultKeysUnder(t *testing.T, store *PebbleStore, p byte, ws [8]byte) int {
	t.Helper()
	lo := append([]byte{p}, ws[:]...)
	iter, err := store.db.NewIter(&pebble.IterOptions{LowerBound: lo, UpperBound: keys.PrefixUpperBound(lo)})
	require.NoError(t, err)
	defer iter.Close()
	n := 0
	for iter.First(); iter.Valid(); iter.Next() {
		n++
	}
	return n
}

// TestRecallLabel_KeyShapes pins the byte layout and lengths.
func TestRecallLabel_KeyShapes(t *testing.T) {
	ws := [8]byte{0x9f, 0x1c, 1, 2, 3, 4, 5, 6}
	ev, mem := NewULID(), NewULID()
	parent := keys.RecallEventKey(ws, ev)
	header := keys.RecallROHeaderKey(ws, ev)
	label := keys.RecallLabelKey(ws, ev, mem, testLC)
	require.Len(t, parent, 25)
	require.Len(t, header, 26)
	require.Len(t, label, 50)
	require.Equal(t, prefix.RecallEvent, header[0])
	require.Equal(t, parent, header[:25], "header is a child of its parent")
	require.Equal(t, parent, label[:25], "label is a child of its parent")
	require.Equal(t, byte('H'), header[25])
	require.Equal(t, byte('L'), label[25])
	require.Equal(t, mem[:], label[26:42])
	require.Equal(t, testLC[:], label[42:50])
	require.Equal(t, -1, bytes.Compare(parent, header))
	require.Equal(t, -1, bytes.Compare(header, label), "'H' sorts before every 'L' row")
	// A child never sorts past the NEXT event's parent.
	var next [16]byte
	copy(next[:], ev[:])
	next[15]++
	require.Equal(t, -1, bytes.Compare(label, keys.RecallEventKey(ws, next)))
}

// recallEventV0 is a byte-for-byte copy of the RecallEvent / entry structs as
// they were BEFORE the label pipeline — the decoder an old binary runs.
type recallSurfacedEntryV0 struct {
	ID    string  `msgpack:"id"`
	Score float32 `msgpack:"score"`
}
type recallEventV0 struct {
	Context    []string                `msgpack:"context,omitempty"`
	Threshold  float32                 `msgpack:"threshold,omitempty"`
	SurfacedAt int64                   `msgpack:"surfaced_at"`
	Entries    []recallSurfacedEntryV0 `msgpack:"entries"`
}

// TestRecallEvent_OldDecoderIgnoresNewFields settles the spec's open item:
// vmihailenco/msgpack skips unknown map keys on decode by default, so an old
// binary decodes a new event (with v/rc/rs and per-entry s/a/c/b) without
// error and with every old field intact. And the reverse: a pre-2a value
// decodes on the new binary with Version 0, so its source reads as unknown.
func TestRecallEvent_OldDecoderIgnoresNewFields(t *testing.T) {
	newEv := RecallEvent{
		Context: []string{"beehive winter feeding"}, Threshold: 0.1, SurfacedAt: 1234,
		Version: RecallEventVersionLabels, RecallCred: []byte{1, 2, 3, 4, 5, 6, 7, 8}, RecallSess: []byte{8, 7, 6, 5, 4, 3, 2, 1},
		Entries: []RecallSurfacedEntry{
			{ID: "01JC000000000000000000000A", Score: 0.7, Source: 0x2D, Abs: 0.4, CM: 0.5, Prior: 1.2},
			{ID: "01JC000000000000000000000B", Score: 0.3, Source: 0, Abs: 0.1, CM: 0.2, Prior: 0.9},
		},
	}
	raw, err := msgpack.Marshal(&newEv)
	require.NoError(t, err)

	var old recallEventV0
	require.NoError(t, msgpack.Unmarshal(raw, &old), "an old binary must decode a new event without error")
	require.Equal(t, newEv.Context, old.Context)
	require.Equal(t, newEv.Threshold, old.Threshold)
	require.Equal(t, newEv.SurfacedAt, old.SurfacedAt)
	require.Len(t, old.Entries, 2)
	require.Equal(t, newEv.Entries[0].ID, old.Entries[0].ID)
	require.Equal(t, newEv.Entries[1].Score, old.Entries[1].Score)

	// Reverse direction.
	oldRaw, err := msgpack.Marshal(&recallEventV0{SurfacedAt: 5, Entries: []recallSurfacedEntryV0{{ID: "x", Score: 0.5}}})
	require.NoError(t, err)
	var back RecallEvent
	require.NoError(t, msgpack.Unmarshal(oldRaw, &back))
	require.Equal(t, uint8(0), back.Version)
	require.Equal(t, RecallSourceUnknown, back.EntrySource(0), "a pre-2a event's source is unknown, not 'injected'")
	require.Equal(t, uint8(0x2D), newEv.EntrySource(0))
}

// TestLabels_ScansAndCountIgnoreChildRows: CountRecallEventsSince,
// ScanRecallEvents and GetRecallEvent are unchanged with 'H' and 'L' rows
// present — and the scan's decoder is never handed a child value (which would
// not be a RecallEvent). This is also the old-binary read path: those three
// functions' key-length selection is the code an old binary runs.
func TestLabels_ScansAndCountIgnoreChildRows(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.ResolveVaultPrefix("label-scan-children")

	// Distinct timestamps: two NewULID() calls in one millisecond draw fresh
	// entropy and need not sort in call order.
	e1, e2 := NewULIDWithTime(time.Now().Add(-2*time.Second)), NewULIDWithTime(time.Now().Add(-time.Second))
	m1, m2 := NewULID(), NewULID()
	writeTestEvent(t, store, ws, e1, m1, m2)
	writeTestEvent(t, store, ws, e2, m1)
	upsertTestLabel(t, store, ws, e1, m1, testLC, nil)
	upsertTestLabel(t, store, ws, e1, m2, testLC, nil)
	// A read-only id: header + label, no parent.
	ro := NewULID()
	upsertTestLabel(t, store, ws, ro, m1, testLC, &RecallROHeader{Unverified: 1, At: time.Now().UnixMilli()})

	n, err := store.CountRecallEventsSince(ctx, ws, time.Now().Add(-time.Hour))
	require.NoError(t, err)
	require.Equal(t, 2, n, "child rows are not recalls")

	var seen []ULID
	require.NoError(t, store.ScanRecallEvents(ctx, ws, RecallPurposeCalibration, func(id ULID, ev *RecallEvent) error {
		require.NotEmpty(t, ev.Entries)
		seen = append(seen, id)
		return nil
	}))
	require.Equal(t, []ULID{e1, e2}, seen)

	got, err := store.GetRecallEvent(ctx, ws, e1, RecallPurposeCalibration)
	require.NoError(t, err)
	require.Len(t, got.Entries, 2)
	none, err := store.GetRecallEvent(ctx, ws, ro, RecallPurposeCalibration)
	require.NoError(t, err)
	require.Nil(t, none, "a read-only header is not an event")

	// Old-binary decode path, verbatim: select 25-byte keys, decode as the
	// pre-2a struct. No child value may reach it, and none may fail it.
	iter, err := store.db.NewIter(&pebble.IterOptions{LowerBound: keys.RecallEventPrefix(ws), UpperBound: prefixUpperBound(keys.RecallEventPrefix(ws))})
	require.NoError(t, err)
	defer iter.Close()
	decoded, children := 0, 0
	for iter.First(); iter.Valid(); iter.Next() {
		if len(iter.Key()) != 25 {
			children++
			continue
		}
		var old recallEventV0
		require.NoError(t, msgpack.Unmarshal(iter.Value(), &old))
		decoded++
	}
	require.Equal(t, 2, decoded)
	require.Equal(t, 4, children, "two labels on e1, one header + one label on ro")

	var labels []RecallLabelRow
	require.NoError(t, store.ScanRecallLabels(ctx, ws, RecallPurposeLabel, func(r RecallLabelRow) error {
		labels = append(labels, r)
		return nil
	}))
	require.Len(t, labels, 3)
}

// TestLabels_PruneKeepsLabelledEventsTo180Days: an unlabelled event of 91
// days is gone; a labelled one is kept with its children; at 181 days,
// gone with all its children. A read-only header group (no parent) follows
// the labelled rule.
func TestLabels_PruneKeepsLabelledEventsTo180Days(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.ResolveVaultPrefix("label-prune")
	now := time.Now()
	day := func(d int) ULID { return NewULIDWithTime(now.AddDate(0, 0, -d)) }

	unl91, lab91, lab181, ro91, fresh := day(91), day(91), day(181), day(91), day(1)
	mem := NewULID()
	// Raw writes, not WriteRecallEvent: its amortized prune (every 256th
	// write, process-wide) would otherwise run the real 90/180-day rule in the
	// middle of this setup when other tests have advanced the counter — and
	// prune lab91 before its label exists.
	for _, id := range []ULID{unl91, lab91, lab181, fresh} {
		val, err := msgpack.Marshal(&RecallEvent{SurfacedAt: 1, Entries: []RecallSurfacedEntry{{ID: mem.String(), Score: 0.5}}})
		require.NoError(t, err)
		require.NoError(t, store.db.Set(keys.RecallEventKey(ws, id), val, pebble.NoSync))
	}
	upsertTestLabel(t, store, ws, lab91, mem, testLC, nil)
	upsertTestLabel(t, store, ws, lab181, mem, testLC, nil)
	upsertTestLabel(t, store, ws, ro91, mem, testLC, &RecallROHeader{Unverified: 1})

	deleted, err := store.PruneRecallEvents(ctx, ws,
		now.AddDate(0, 0, -recallEventRetentionDays), now.AddDate(0, 0, -recallLabelledRetentionDays))
	require.NoError(t, err)
	require.Equal(t, 2, deleted, "unl91 and lab181 groups")

	has := func(k []byte) bool {
		_, c, err := store.db.Get(k)
		if err == pebble.ErrNotFound {
			return false
		}
		require.NoError(t, err)
		c.Close()
		return true
	}
	require.False(t, has(keys.RecallEventKey(ws, unl91)), "unlabelled 91-day event pruned")
	require.True(t, has(keys.RecallEventKey(ws, lab91)), "labelled 91-day event kept")
	require.True(t, has(keys.RecallLabelKey(ws, lab91, mem, testLC)), "its label kept")
	require.False(t, has(keys.RecallEventKey(ws, lab181)), "labelled 181-day event pruned")
	require.False(t, has(keys.RecallLabelKey(ws, lab181, mem, testLC)), "with its label")
	require.True(t, has(keys.RecallROHeaderKey(ws, ro91)), "91-day read-only header kept")
	require.True(t, has(keys.RecallLabelKey(ws, ro91, mem, testLC)))
	require.True(t, has(keys.RecallEventKey(ws, fresh)))
}

// TestLabels_ClearVaultLeavesZeroKeys pins ClearVault BEHAVIOURALLY (the
// registry and scope guards stay green when a list omits a prefix): after a
// clear, no 0x29 key of any shape survives under the vault, and a sibling's
// labels are untouched.
func TestLabels_ClearVaultLeavesZeroKeys(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.ResolveVaultPrefix("label-clear")
	other := store.ResolveVaultPrefix("label-clear-sibling")
	mem := NewULID()
	for _, w := range [][8]byte{ws, other} {
		ev := NewULID()
		writeTestEvent(t, store, w, ev, mem)
		upsertTestLabel(t, store, w, ev, mem, testLC, nil)
		upsertTestLabel(t, store, w, NewULID(), mem, testLC, &RecallROHeader{Unverified: 1})
	}
	require.Equal(t, 4, vaultKeysUnder(t, store, prefix.RecallEvent, ws))

	_, err := store.ClearVault(ctx, ws)
	require.NoError(t, err)
	require.Zero(t, vaultKeysUnder(t, store, prefix.RecallEvent, ws), "no parent, header or label may survive a clear")
	require.Equal(t, 4, vaultKeysUnder(t, store, prefix.RecallEvent, other))
}

// TestLabels_NotExported: labels are instrument data next to raw query text
// and are not exported, the same as their events. An export → import round
// trip carries no 0x29 key of any shape.
func TestLabels_NotExported(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	src := store.ResolveVaultPrefix("label-export-src")
	mem := NewULID()
	ev := NewULID()
	writeTestEvent(t, store, src, ev, mem)
	upsertTestLabel(t, store, src, ev, mem, testLC, nil)
	upsertTestLabel(t, store, src, NewULID(), mem, testLC, &RecallROHeader{Unverified: 1})

	var buf bytes.Buffer
	_, err := store.ExportVaultData(ctx, src, "label-export-src", ExportOpts{}, &buf)
	require.NoError(t, err)

	dst := store.ResolveVaultPrefix("label-export-dst")
	_, err = store.ImportVaultData(ctx, dst, "label-export-dst", ImportOpts{}, &buf)
	require.NoError(t, err)
	require.Zero(t, vaultKeysUnder(t, store, prefix.RecallEvent, dst), "no event, header or label travels with export")
}

// TestLabels_NotClonedOrMerged: clone and merge copy no 0x29 rows.
func TestLabels_NotClonedOrMerged(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	src := store.ResolveVaultPrefix("label-clone-src")
	mem := NewULID()
	ev := NewULID()
	writeTestEvent(t, store, src, ev, mem)
	upsertTestLabel(t, store, src, ev, mem, testLC, nil)
	upsertTestLabel(t, store, src, NewULID(), mem, testLC, &RecallROHeader{Unverified: 1})

	cl := store.ResolveVaultPrefix("label-clone-dst")
	_, err := store.CloneVaultData(ctx, src, cl, nil)
	require.NoError(t, err)
	require.Zero(t, vaultKeysUnder(t, store, prefix.RecallEvent, cl), "clone copies no labels")

	mg := store.ResolveVaultPrefix("label-merge-dst")
	_, err = store.MergeVaultData(ctx, src, mg, nil)
	require.NoError(t, err)
	require.Zero(t, vaultKeysUnder(t, store, prefix.RecallEvent, mg), "merge copies no labels")
	require.Equal(t, 4, vaultKeysUnder(t, store, prefix.RecallEvent, src), "source untouched")
}

// TestRecallLabel_UpsertIdempotent: repeats never create rows; polarity is
// last-write-wins; the first post's context is kept; the header is written
// once.
func TestRecallLabel_UpsertIdempotent(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.ResolveVaultPrefix("label-idem")
	ev, mem := NewULID(), NewULID()
	before := len(allKeys(t, store))

	first, isFirst, err := store.UpsertRecallLabel(ctx, ws, ev, mem, testLC,
		RecallLabel{Polarity: 1, LastAt: 100, Rank: 3, SetSize: 7, Source: 1, Membership: LabelMembershipVerified}, &RecallROHeader{Unverified: 1, At: 99})
	require.NoError(t, err)
	require.True(t, isFirst)
	require.Equal(t, uint32(1), first.Count)
	require.Equal(t, int64(100), first.FirstAt)

	for i := 0; i < 9; i++ {
		_, isFirst, err = store.UpsertRecallLabel(ctx, ws, ev, mem, testLC,
			RecallLabel{Polarity: 1, LastAt: int64(200 + i), Rank: 9}, &RecallROHeader{Unverified: 1, At: 1})
		require.NoError(t, err)
		require.False(t, isFirst)
	}
	last, _, err := store.UpsertRecallLabel(ctx, ws, ev, mem, testLC, RecallLabel{Polarity: 0, LastAt: 999}, nil)
	require.NoError(t, err)
	require.Equal(t, uint32(11), last.Count)
	require.Equal(t, uint8(0), last.Polarity, "last write wins")
	require.Equal(t, int64(100), last.FirstAt)
	require.Equal(t, int64(999), last.LastAt)
	require.Equal(t, uint16(3), last.Rank, "first post's rank kept")
	require.Equal(t, before+2, len(allKeys(t, store)), "one label row + one header row, however many posts")

	h, err := store.GetRecallROHeader(ctx, ws, ev, RecallPurposeLabel)
	require.NoError(t, err)
	require.Equal(t, int64(99), h.At, "header written once, by the first post")

	// A second credential is a distinct label.
	other := [8]byte{9, 9, 9, 9, 9, 9, 9, 9}
	_, isFirst, err = store.UpsertRecallLabel(ctx, ws, ev, mem, other, RecallLabel{Polarity: 1, LastAt: 5}, nil)
	require.NoError(t, err)
	require.True(t, isFirst)
}

// TestLabels_ConcurrentUpserts (-race): simultaneous labels on the same
// (event, memory, credential) produce one row whose count equals the number
// of posts, and exactly one of them reports first.
func TestLabels_ConcurrentUpserts(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.ResolveVaultPrefix("label-concurrent")
	ev, mem := NewULID(), NewULID()
	writeTestEvent(t, store, ws, ev, mem)

	const workers, per = 16, 25
	var wg sync.WaitGroup
	var mu sync.Mutex
	firsts := 0
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < per; i++ {
				_, first, err := store.UpsertRecallLabel(ctx, ws, ev, mem, testLC,
					RecallLabel{Polarity: uint8(w % 2), LastAt: int64(w*per + i)}, &RecallROHeader{Unverified: 1})
				if err != nil {
					t.Error(err)
					return
				}
				if first {
					mu.Lock()
					firsts++
					mu.Unlock()
				}
			}
		}(w)
	}
	wg.Wait()
	require.Equal(t, 1, firsts)
	l, err := store.GetRecallLabel(ctx, ws, ev, mem, testLC, RecallPurposeLabel)
	require.NoError(t, err)
	require.Equal(t, uint32(workers*per), l.Count)
	require.Equal(t, 3, vaultKeysUnder(t, store, prefix.RecallEvent, ws), "parent + header + one label")
}

// TestRecallEventEntries_BatchAndGate: one call resolves several events; an
// absent id is absent; the read is purpose-gated.
func TestRecallEventEntries_BatchAndGate(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.ResolveVaultPrefix("label-entries")
	e1, e2, missing := NewULID(), NewULID(), NewULID()
	writeTestEvent(t, store, ws, e1, NewULID())
	writeTestEvent(t, store, ws, e2, NewULID(), NewULID())
	upsertTestLabel(t, store, ws, e1, NewULID(), testLC, nil)

	got, err := store.RecallEventEntries(ctx, ws, []ULID{e1, e2, missing, e1}, RecallPurposeLabel)
	require.NoError(t, err)
	require.Len(t, got, 2)
	require.Len(t, got[e2].Entries, 2)
	_, err = store.RecallEventEntries(ctx, ws, []ULID{e1}, "ranking")
	require.Error(t, err)
	// Cross-vault by construction: the same id under another vault is absent.
	got, err = store.RecallEventEntries(ctx, store.ResolveVaultPrefix("label-entries-other"), []ULID{e1}, RecallPurposeLabel)
	require.NoError(t, err)
	require.Empty(t, got)
}
