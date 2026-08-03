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
	seedEndpoints(t, store, ws, idA, idB)

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
	seedEndpoints(t, store, ws, idA, idB)

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

// ---------------------------------------------------------------------------
// THE DECAY ANCHOR IS MONOTONIC: A LATE-ARRIVING STAMP MAY NOT MOVE IT BACK.
//
// Threading CoActivationEvent.At through to the on-disk lastActivated (the test
// above) made the field REMOTELY WRITABLE, and it is COG-27's decay input:
//
//	ceiling = max(peakWeight*0.05, peakWeight * 2^(-(now - lastActivated)/H))
//
// so an anchor moved BACKWARDS collapses the edge's ceiling on the next pass,
// and COG-27's never-raise guarantee means a corrected stamp cannot restore it.
// Only re-learning can. The write path read the stored lastActivated and threw
// it away (the `_` in getAssocValueFull's 7-tuple), so the last writer won
// regardless of order.
//
// It is reachable in cluster mode: the coordinator builds
// CoActivationEvent{At: time.Unix(0, effect.Timestamp)} from the REMOTE PEER'S
// CLOCK, verbatim. The Hebbian worker takes the per-pair max WITHIN a batch,
// which is order-independent and correct, but nothing compares against what is
// already on disk. A lagging or skewed peer, or a cog-forward backlog delivered
// after a partition heals, then rewrites another node's decay anchor.
//
// The guard is max(stored, incoming) — the same shape peakWeight already has
// three lines above it in the same function, and it restores COG-27's
// replication-convergence claim: max is commutative and associative, so nodes
// converge on the anchor regardless of delivery order.
//
// NOT decided here: clamping a FUTURE stamp DOWN to now. That is the opposite
// direction (an inflated anchor suppresses decay rather than over-applying it),
// it is not what this guard is for, and it would silently break the offline
// replay driver's virtual clock. Named so the omission is a choice.
//
// PRIVACY: synthetic IDs only.
// ---------------------------------------------------------------------------

func TestUpdateAssocWeightBatch_NeverMovesTheDecayAnchorBackwards(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("assoc-anchor-vault")
	idA, idB := NewULID(), NewULID()

	// Fixed instants: nothing here may depend on the wall clock.
	recent := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	stale := recent.Add(-400 * 24 * time.Hour)
	const halfLife = 30 * 24 * time.Hour

	seedEndpoints(t, store, ws, idA, idB)
	if err := store.WriteAssociation(ctx, ws, idA, idB, &Association{
		TargetID: idB, Weight: 0.9, Confidence: 1.0, CreatedAt: stale,
	}); err != nil {
		t.Fatalf("WriteAssociation: %v", err)
	}

	// The local co-activation: recent, and the anchor the vault's decay is owed.
	if err := store.UpdateAssocWeightBatch(ctx, []AssocWeightUpdate{{
		WS: ws, Src: idA, Dst: idB, Weight: 0.9, CountDelta: 1,
		LastActivatedAt: int32(recent.Unix()),
	}}); err != nil {
		t.Fatalf("UpdateAssocWeightBatch(recent): %v", err)
	}
	afterRecent := readAssocLastActivated(t, store, ws, idA, idB)
	if afterRecent != int32(recent.Unix()) {
		t.Fatalf("the RECENT co-activation did not land (lastActivated %d, want %d) — "+
			"the regression below would prove nothing", afterRecent, int32(recent.Unix()))
	}

	// The late arrival: a peer's clock, 400 days behind, for the same pair.
	if err := store.UpdateAssocWeightBatch(ctx, []AssocWeightUpdate{{
		WS: ws, Src: idA, Dst: idB, Weight: 0.9, CountDelta: 1,
		LastActivatedAt: int32(stale.Unix()),
	}}); err != nil {
		t.Fatalf("UpdateAssocWeightBatch(stale): %v", err)
	}
	afterStale := readAssocLastActivated(t, store, ws, idA, idB)
	if afterStale < afterRecent {
		t.Errorf("the decay anchor moved BACKWARDS by %d days: %d (%s) -> %d (%s). "+
			"COG-27 reads lastActivated as elapsed time, and its never-raise guard makes "+
			"the resulting collapse irreversible — a corrected stamp cannot restore the "+
			"weight, only re-learning can.",
			int(time.Duration(afterRecent-afterStale)*time.Second/(24*time.Hour)),
			afterRecent, time.Unix(int64(afterRecent), 0).UTC(),
			afterStale, time.Unix(int64(afterStale), 0).UTC())
	}

	// The consequence, measured rather than asserted in prose: one decay pass at
	// the instant of the recent co-activation. With the anchor intact the edge is
	// untouched; with it moved back 400 days it is clamped to the 5% floor.
	store.decayNow = func() time.Time { return recent }
	if _, err := store.DecayAssocWeights(ctx, ws, halfLife, 0.01, 0); err != nil {
		t.Fatalf("DecayAssocWeights: %v", err)
	}
	w, err := store.GetAssocWeight(ctx, ws, idA, idB)
	if err != nil {
		t.Fatalf("GetAssocWeight: %v", err)
	}
	if w < 0.9 {
		t.Errorf("one decay pass took the weight to %.6f (was 0.900000) because the "+
			"anchor had been moved back 400 days. The edge was co-activated moments "+
			"before the pass.", w)
	}
}

// TestUpdateAssocWeight_NeverMovesTheDecayAnchorBackwards is the same guard on
// the single-pair writer. It stamps time.Now() unconditionally, so the only way
// to drive it backwards is an anchor already AHEAD of the wall clock — which is
// exactly what a skewed peer's cog-forward writes through the batch path. The
// guard is one max() in each writer; both are pinned so neither can be reverted
// alone.
func TestUpdateAssocWeight_NeverMovesTheDecayAnchorBackwards(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("assoc-anchor-vault-single")
	idA, idB := NewULID(), NewULID()
	seedEndpoints(t, store, ws, idA, idB)

	if err := store.WriteAssociation(ctx, ws, idA, idB, &Association{
		TargetID: idB, Weight: 0.5, Confidence: 1.0,
	}); err != nil {
		t.Fatalf("WriteAssociation: %v", err)
	}

	ahead := time.Now().Add(72 * time.Hour).Truncate(time.Second)
	if err := store.UpdateAssocWeightBatch(ctx, []AssocWeightUpdate{{
		WS: ws, Src: idA, Dst: idB, Weight: 0.6, CountDelta: 1,
		LastActivatedAt: int32(ahead.Unix()),
	}}); err != nil {
		t.Fatalf("UpdateAssocWeightBatch: %v", err)
	}
	if got := readAssocLastActivated(t, store, ws, idA, idB); got != int32(ahead.Unix()) {
		t.Fatalf("the ahead-of-clock anchor did not land (%d, want %d)", got, int32(ahead.Unix()))
	}

	if err := store.UpdateAssocWeight(ctx, ws, idA, idB, 0.7, 1); err != nil {
		t.Fatalf("UpdateAssocWeight: %v", err)
	}
	got := readAssocLastActivated(t, store, ws, idA, idB)
	if got < int32(ahead.Unix()) {
		t.Errorf("UpdateAssocWeight moved the decay anchor backwards to %d (%s) from "+
			"%d (%s). The anchor is monotonic in both writers or in neither.",
			got, time.Unix(int64(got), 0).UTC(), int32(ahead.Unix()), ahead.UTC())
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
