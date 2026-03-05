package storage

import (
	"context"
	"encoding/binary"
	"math"
	"testing"
)

// TestCoActivationCount_NewAssocStartsAtOne verifies that WriteAssociation seeds
// CoActivationCount=1 (creation is itself a co-activation event).
func TestCoActivationCount_NewAssocStartsAtOne(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("coact-new-starts-at-one")

	src := NewULID()
	dst := NewULID()

	if err := store.WriteAssociation(ctx, ws, src, dst, &Association{
		TargetID: dst,
		Weight:   0.5,
	}); err != nil {
		t.Fatalf("WriteAssociation: %v", err)
	}

	fresh := newFreshStore(t, store.db)
	results, err := fresh.GetAssociations(ctx, ws, []ULID{src}, 10)
	if err != nil {
		t.Fatalf("GetAssociations: %v", err)
	}

	got, ok := results[src]
	if !ok || len(got) != 1 {
		t.Fatalf("expected 1 association for src, got %d", len(got))
	}

	if got[0].CoActivationCount != 1 {
		t.Errorf("CoActivationCount on new association: got %d, want 1", got[0].CoActivationCount)
	}
}

// TestCoActivationCount_IncrementOnUpdate verifies that UpdateAssocWeight accumulates
// the countDelta into CoActivationCount.
func TestCoActivationCount_IncrementOnUpdate(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("coact-increment-on-update")

	src := NewULID()
	dst := NewULID()

	if err := store.WriteAssociation(ctx, ws, src, dst, &Association{
		TargetID: dst,
		Weight:   0.5,
	}); err != nil {
		t.Fatalf("WriteAssociation: %v", err)
	}

	// After write: count=1. Add delta=3 → expect count=4.
	if err := store.UpdateAssocWeight(ctx, ws, src, dst, 0.6, 3); err != nil {
		t.Fatalf("UpdateAssocWeight (delta=3): %v", err)
	}

	fresh := newFreshStore(t, store.db)
	results, err := fresh.GetAssociations(ctx, ws, []ULID{src}, 10)
	if err != nil {
		t.Fatalf("GetAssociations after first update: %v", err)
	}
	got := results[src]
	if len(got) != 1 {
		t.Fatalf("expected 1 association, got %d", len(got))
	}
	if got[0].CoActivationCount != 4 {
		t.Errorf("CoActivationCount after delta=3: got %d, want 4", got[0].CoActivationCount)
	}

	// Add delta=10 → expect count=14.
	if err := store.UpdateAssocWeight(ctx, ws, src, dst, 0.7, 10); err != nil {
		t.Fatalf("UpdateAssocWeight (delta=10): %v", err)
	}

	fresh2 := newFreshStore(t, store.db)
	results2, err := fresh2.GetAssociations(ctx, ws, []ULID{src}, 10)
	if err != nil {
		t.Fatalf("GetAssociations after second update: %v", err)
	}
	got2 := results2[src]
	if len(got2) != 1 {
		t.Fatalf("expected 1 association, got %d", len(got2))
	}
	if got2[0].CoActivationCount != 14 {
		t.Errorf("CoActivationCount after delta=10: got %d, want 14", got2[0].CoActivationCount)
	}
}

// TestCoActivationCount_BatchUpdate verifies that UpdateAssocWeightBatch accumulates
// CountDelta per pair.
func TestCoActivationCount_BatchUpdate(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("coact-batch-update")

	src1, dst1 := NewULID(), NewULID()
	src2, dst2 := NewULID(), NewULID()

	// Write both associations — each starts at count=1.
	if err := store.WriteAssociation(ctx, ws, src1, dst1, &Association{
		TargetID: dst1, Weight: 0.4,
	}); err != nil {
		t.Fatalf("WriteAssociation pair1: %v", err)
	}
	if err := store.WriteAssociation(ctx, ws, src2, dst2, &Association{
		TargetID: dst2, Weight: 0.4,
	}); err != nil {
		t.Fatalf("WriteAssociation pair2: %v", err)
	}

	// Batch update: pair1 delta=5 (expect 1+5=6), pair2 delta=2 (expect 1+2=3).
	updates := []AssocWeightUpdate{
		{WS: ws, Src: src1, Dst: dst1, Weight: 0.6, CountDelta: 5},
		{WS: ws, Src: src2, Dst: dst2, Weight: 0.5, CountDelta: 2},
	}
	if err := store.UpdateAssocWeightBatch(ctx, updates); err != nil {
		t.Fatalf("UpdateAssocWeightBatch: %v", err)
	}

	fresh := newFreshStore(t, store.db)
	results, err := fresh.GetAssociations(ctx, ws, []ULID{src1, src2}, 10)
	if err != nil {
		t.Fatalf("GetAssociations: %v", err)
	}

	pair1 := results[src1]
	if len(pair1) != 1 {
		t.Fatalf("expected 1 association for src1, got %d", len(pair1))
	}
	if pair1[0].CoActivationCount != 6 {
		t.Errorf("pair1 CoActivationCount: got %d, want 6", pair1[0].CoActivationCount)
	}

	pair2 := results[src2]
	if len(pair2) != 1 {
		t.Fatalf("expected 1 association for src2, got %d", len(pair2))
	}
	if pair2[0].CoActivationCount != 3 {
		t.Errorf("pair2 CoActivationCount: got %d, want 3", pair2[0].CoActivationCount)
	}
}

// TestCoActivationCount_ZeroDeltaDoesNotChange verifies that UpdateAssocWeight
// with countDelta=0 does not modify CoActivationCount (weight-only update).
func TestCoActivationCount_ZeroDeltaDoesNotChange(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("coact-zero-delta")

	src := NewULID()
	dst := NewULID()

	// Write: count starts at 1.
	if err := store.WriteAssociation(ctx, ws, src, dst, &Association{
		TargetID: dst,
		Weight:   0.5,
	}); err != nil {
		t.Fatalf("WriteAssociation: %v", err)
	}

	// Verify initial count is 1.
	fresh := newFreshStore(t, store.db)
	results, err := fresh.GetAssociations(ctx, ws, []ULID{src}, 10)
	if err != nil {
		t.Fatalf("GetAssociations after write: %v", err)
	}
	got := results[src]
	if len(got) != 1 {
		t.Fatalf("expected 1 association, got %d", len(got))
	}
	if got[0].CoActivationCount != 1 {
		t.Errorf("CoActivationCount after write: got %d, want 1", got[0].CoActivationCount)
	}

	// Update weight only (countDelta=0) — count must remain at 1.
	if err := store.UpdateAssocWeight(ctx, ws, src, dst, 0.7, 0); err != nil {
		t.Fatalf("UpdateAssocWeight (delta=0): %v", err)
	}

	fresh2 := newFreshStore(t, store.db)
	results2, err := fresh2.GetAssociations(ctx, ws, []ULID{src}, 10)
	if err != nil {
		t.Fatalf("GetAssociations after zero-delta update: %v", err)
	}
	got2 := results2[src]
	if len(got2) != 1 {
		t.Fatalf("expected 1 association, got %d", len(got2))
	}
	if got2[0].CoActivationCount != 1 {
		t.Errorf("CoActivationCount after delta=0: got %d, want 1 (must not change)", got2[0].CoActivationCount)
	}
}

// TestCoActivationCount_LegacyValueDecodesAsZero verifies that a 22-byte legacy
// association value (without the CoActivationCount field) decodes with count=0.
func TestCoActivationCount_LegacyValueDecodesAsZero(t *testing.T) {
	// Construct a 22-byte legacy value by hand (the old format).
	// relType=1, confidence=0.9, createdAt nanos=0, lastActivated=0, peakWeight=0.5
	var legacy [22]byte
	// relType (bytes 0-1): RelSupports = 1
	binary.BigEndian.PutUint16(legacy[0:2], uint16(RelSupports))
	// confidence (bytes 2-5): 0.9 as float32 bits big-endian
	binary.BigEndian.PutUint32(legacy[2:6], math.Float32bits(0.9))
	// createdAt nanos (bytes 6-13): 0 (zero time)
	binary.BigEndian.PutUint64(legacy[6:14], 0)
	// lastActivated (bytes 14-17): 0
	binary.BigEndian.PutUint32(legacy[14:18], 0)
	// peakWeight (bytes 18-21): 0.5 as float32 bits big-endian
	binary.BigEndian.PutUint32(legacy[18:22], math.Float32bits(0.5))

	_, _, _, _, _, coActivationCount, _ := decodeAssocValue(legacy[:])
	if coActivationCount != 0 {
		t.Errorf("legacy 22-byte value: CoActivationCount got %d, want 0", coActivationCount)
	}
}

// TestCoActivationCount_SaturatesAtMaxUint32 verifies that CoActivationCount saturates
// at math.MaxUint32 rather than wrapping around on overflow.
func TestCoActivationCount_SaturatesAtMaxUint32(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("coact-saturate")

	src := NewULID()
	dst := NewULID()

	// Write: count starts at 1.
	if err := store.WriteAssociation(ctx, ws, src, dst, &Association{
		TargetID: dst, Weight: 0.5,
	}); err != nil {
		t.Fatalf("WriteAssociation: %v", err)
	}

	// Add delta = MaxUint32 - 1 → count should be 1 + (MaxUint32-1) = MaxUint32.
	if err := store.UpdateAssocWeight(ctx, ws, src, dst, 0.6, math.MaxUint32-1); err != nil {
		t.Fatalf("UpdateAssocWeight (delta=MaxUint32-1): %v", err)
	}

	fresh := newFreshStore(t, store.db)
	results, err := fresh.GetAssociations(ctx, ws, []ULID{src}, 10)
	if err != nil {
		t.Fatalf("GetAssociations after saturation update: %v", err)
	}
	got := results[src]
	if len(got) != 1 {
		t.Fatalf("expected 1 association, got %d", len(got))
	}
	if got[0].CoActivationCount != math.MaxUint32 {
		t.Errorf("CoActivationCount after saturation: got %d, want %d", got[0].CoActivationCount, uint32(math.MaxUint32))
	}

	// Another update — count must remain at MaxUint32 (no overflow).
	if err := store.UpdateAssocWeight(ctx, ws, src, dst, 0.7, 1); err != nil {
		t.Fatalf("UpdateAssocWeight (delta=1, post-saturation): %v", err)
	}

	fresh2 := newFreshStore(t, store.db)
	results2, err := fresh2.GetAssociations(ctx, ws, []ULID{src}, 10)
	if err != nil {
		t.Fatalf("GetAssociations after post-saturation update: %v", err)
	}
	got2 := results2[src]
	if len(got2) != 1 {
		t.Fatalf("expected 1 association, got %d", len(got2))
	}
	if got2[0].CoActivationCount != math.MaxUint32 {
		t.Errorf("CoActivationCount post-saturation: got %d, want %d (must not overflow)", got2[0].CoActivationCount, uint32(math.MaxUint32))
	}
}
