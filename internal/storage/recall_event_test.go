package storage

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestRecallEvent_WriteGetRoundtrip verifies the msgpack roundtrip and the
// nil,nil contract for absent events.
func TestRecallEvent_WriteGetRoundtrip(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.ResolveVaultPrefix("recall-event-roundtrip")

	id := NewULID()
	ev := &RecallEvent{
		Context:    []string{"primary relational database"},
		Threshold:  0.1,
		SurfacedAt: time.Now().UnixNano(),
		Entries: []RecallSurfacedEntry{
			{ID: NewULID().String(), Score: 0.81},
			{ID: NewULID().String(), Score: 0.44},
		},
	}
	require.NoError(t, store.WriteRecallEvent(ctx, ws, id, ev))

	got, err := store.GetRecallEvent(ctx, ws, id, RecallPurposeCalibration)
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, ev.Context, got.Context)
	require.Equal(t, ev.Threshold, got.Threshold)
	require.Equal(t, ev.SurfacedAt, got.SurfacedAt)
	require.Equal(t, ev.Entries, got.Entries)

	// Absent event: nil, nil.
	absent, err := store.GetRecallEvent(ctx, ws, NewULID(), RecallPurposeCalibration)
	require.NoError(t, err)
	require.Nil(t, absent)
}

// TestRecallEvent_ScanOrder verifies events iterate in event-time (ULID key)
// order and stay vault-scoped.
func TestRecallEvent_ScanOrder(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.ResolveVaultPrefix("recall-event-order")
	otherWS := store.ResolveVaultPrefix("recall-event-other")

	base := time.Now().Add(-time.Hour)
	var ids []ULID
	for i := 0; i < 3; i++ {
		id := NewULIDWithTime(base.Add(time.Duration(i) * time.Minute))
		ids = append(ids, id)
		require.NoError(t, store.WriteRecallEvent(ctx, ws, id, &RecallEvent{
			SurfacedAt: base.Add(time.Duration(i) * time.Minute).UnixNano(),
			Entries:    []RecallSurfacedEntry{{ID: NewULID().String(), Score: 0.5}},
		}))
	}
	// An event in a different vault must not appear in the scan.
	require.NoError(t, store.WriteRecallEvent(ctx, otherWS, NewULID(), &RecallEvent{
		SurfacedAt: time.Now().UnixNano(),
		Entries:    []RecallSurfacedEntry{{ID: NewULID().String(), Score: 0.5}},
	}))

	var seen []ULID
	require.NoError(t, store.ScanRecallEvents(ctx, ws, RecallPurposeCalibration, func(id ULID, ev *RecallEvent) error {
		seen = append(seen, id)
		return nil
	}))
	require.Equal(t, ids, seen, "scan must return this vault's events in event-time order")
}

// TestRecallEvent_Prune verifies time-based pruning via the ULID timestamp
// embedded in the key: events older than the cutoff are deleted, newer ones
// survive.
func TestRecallEvent_Prune(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.ResolveVaultPrefix("recall-event-prune")

	now := time.Now()
	oldA := NewULIDWithTime(now.AddDate(0, 0, -10))
	oldB := NewULIDWithTime(now.AddDate(0, 0, -5))
	fresh := NewULIDWithTime(now.Add(-time.Hour))
	for _, id := range []ULID{oldA, oldB, fresh} {
		require.NoError(t, store.WriteRecallEvent(ctx, ws, id, &RecallEvent{
			SurfacedAt: now.UnixNano(),
			Entries:    []RecallSurfacedEntry{{ID: NewULID().String(), Score: 0.5}},
		}))
	}

	deleted, err := store.PruneRecallEvents(ctx, ws, now.AddDate(0, 0, -2))
	require.NoError(t, err)
	require.Equal(t, 2, deleted, "both events older than the cutoff must be pruned")

	gone, err := store.GetRecallEvent(ctx, ws, oldA, RecallPurposeCalibration)
	require.NoError(t, err)
	require.Nil(t, gone)
	kept, err := store.GetRecallEvent(ctx, ws, fresh, RecallPurposeCalibration)
	require.NoError(t, err)
	require.NotNil(t, kept, "event newer than the cutoff must survive")
}

// TestRecallEvent_PurposeGate verifies the read fence: recall events are
// purpose-gated instrument data, and reads without an allowlisted purpose
// fail loudly by construction — no watcher required.
func TestRecallEvent_PurposeGate(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.ResolveVaultPrefix("recall-event-fence")

	id := NewULID()
	require.NoError(t, store.WriteRecallEvent(ctx, ws, id, &RecallEvent{
		SurfacedAt: time.Now().UnixNano(),
		Entries:    []RecallSurfacedEntry{{ID: NewULID().String(), Score: 0.5}},
	}))

	// Empty purpose: refused.
	_, err := store.GetRecallEvent(ctx, ws, id, "")
	require.Error(t, err, "read with empty purpose must be refused")
	require.Contains(t, err.Error(), "purpose-gated")

	// Unknown purpose: refused — narrative reading is not a purpose.
	_, err = store.GetRecallEvent(ctx, ws, id, "narrative")
	require.Error(t, err, "read with non-allowlisted purpose must be refused")

	err = store.ScanRecallEvents(ctx, ws, "recall", func(ULID, *RecallEvent) error { return nil })
	require.Error(t, err, "scan with non-allowlisted purpose must be refused")

	// Allowlisted purpose: permitted.
	got, err := store.GetRecallEvent(ctx, ws, id, RecallPurposeCalibration)
	require.NoError(t, err)
	require.NotNil(t, got)
}

// TestRecallEvent_ClearVaultRemovesEvents is the privacy regression from the
// #574 review: RecallEvent.Context stores raw, unredacted query text, so a
// cleared vault must not leave its recall events behind for the remainder of
// the retention window. A sibling vault's events must survive untouched.
func TestRecallEvent_ClearVaultRemovesEvents(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.ResolveVaultPrefix("recall-event-cleared")
	otherWS := store.ResolveVaultPrefix("recall-event-survivor")

	for i := 0; i < 3; i++ {
		require.NoError(t, store.WriteRecallEvent(ctx, ws, NewULID(), &RecallEvent{
			Context:    []string{"private query text"},
			SurfacedAt: time.Now().UnixNano(),
			Entries:    []RecallSurfacedEntry{{ID: NewULID().String(), Score: 0.5}},
		}))
	}
	require.NoError(t, store.WriteRecallEvent(ctx, otherWS, NewULID(), &RecallEvent{
		Context:    []string{"unrelated vault query"},
		SurfacedAt: time.Now().UnixNano(),
		Entries:    []RecallSurfacedEntry{{ID: NewULID().String(), Score: 0.5}},
	}))

	_, err := store.ClearVault(ctx, ws)
	require.NoError(t, err)

	cleared := 0
	require.NoError(t, store.ScanRecallEvents(ctx, ws, RecallPurposeCalibration,
		func(ULID, *RecallEvent) error { cleared++; return nil }))
	require.Zero(t, cleared, "cleared vault must retain no recall events (raw query text)")

	survived := 0
	require.NoError(t, store.ScanRecallEvents(ctx, otherWS, RecallPurposeCalibration,
		func(ULID, *RecallEvent) error { survived++; return nil }))
	require.Equal(t, 1, survived, "sibling vault's events must survive the clear")
}
