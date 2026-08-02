package storage

import (
	"context"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// A co-activation write must be able to carry its OWN timestamp.
//
// `UpdateAssocWeightBatch` stamped `lastActivated = time.Now()` unconditionally,
// discarding the time the co-activation actually happened. In production that is
// a small lie (an event that waited in the Hebbian worker's channel is stamped
// late). For an OFFLINE REPLAY of historical co-activations it is fatal: every
// replayed edge would look freshly reinforced, association decay (a pure
// function of now - lastActivated, COG-27) would find ~0 elapsed for all of
// them, and the reconstruction would be a "no forgetting ever" graph that never
// existed.
//
// ZERO stays "stamp at write time", so production is byte-identical.
//
// PRIVACY: synthetic IDs only; nothing here comes from any vault.
// ---------------------------------------------------------------------------

func TestUpdateAssocWeightBatch_HonorsExplicitLastActivated(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("assoc-stamp-vault")
	idA, idB := NewULID(), NewULID()

	if err := store.WriteAssociation(ctx, ws, idA, idB, &Association{
		TargetID: idB,
		Weight:   0.5,
	}); err != nil {
		t.Fatalf("WriteAssociation: %v", err)
	}

	past := time.Now().Add(-45 * 24 * time.Hour).Truncate(time.Second)
	want := int32(past.Unix())

	if err := store.UpdateAssocWeightBatch(ctx, []AssocWeightUpdate{{
		WS: ws, Src: idA, Dst: idB, Weight: 0.8, CountDelta: 1,
		LastActivatedAt: want,
	}}); err != nil {
		t.Fatalf("UpdateAssocWeightBatch: %v", err)
	}

	got := readAssocLastActivated(t, store, ws, idA, idB)
	if got != want {
		t.Errorf("LastActivated = %d (%s), want %d (%s) — the batch must record the "+
			"caller's stamp, not the wall clock",
			got, time.Unix(int64(got), 0).UTC(), want, past.UTC())
	}
}

// TestUpdateAssocWeightBatch_ZeroStampMeansNow is the identity control: the new
// field must not change what production writes.
func TestUpdateAssocWeightBatch_ZeroStampMeansNow(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("assoc-stamp-vault")
	idA, idB := NewULID(), NewULID()

	if err := store.WriteAssociation(ctx, ws, idA, idB, &Association{
		TargetID: idB,
		Weight:   0.5,
	}); err != nil {
		t.Fatalf("WriteAssociation: %v", err)
	}

	before := time.Now().Add(-2 * time.Second).Unix()
	if err := store.UpdateAssocWeightBatch(ctx, []AssocWeightUpdate{{
		WS: ws, Src: idA, Dst: idB, Weight: 0.8, CountDelta: 1,
	}}); err != nil {
		t.Fatalf("UpdateAssocWeightBatch: %v", err)
	}
	after := time.Now().Add(2 * time.Second).Unix()

	got := int64(readAssocLastActivated(t, store, ws, idA, idB))
	if got < before || got > after {
		t.Errorf("LastActivated = %d, want within [%d, %d] — a zero stamp must "+
			"still mean 'now'", got, before, after)
	}
}

// readAssocLastActivated reads the edge back through a cold-cache store so the
// assertion is on what is on disk, not on a cached struct.
func readAssocLastActivated(t *testing.T, store *PebbleStore, ws [8]byte, src, dst ULID) int32 {
	t.Helper()
	fresh := newFreshStore(t, store.db)
	results, err := fresh.GetAssociations(context.Background(), ws, []ULID{src}, 10)
	if err != nil {
		t.Fatalf("GetAssociations: %v", err)
	}
	for _, a := range results[src] {
		if a.TargetID == dst {
			return a.LastActivated
		}
	}
	t.Fatalf("edge %s -> %s not found after batch update", src, dst)
	return 0
}
