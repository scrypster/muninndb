package storage

import (
	"context"
	"testing"
	"time"
)

// TestAssocMetadata_LastActivated_PreservedOnUpdate verifies that calling
// UpdateAssocWeight to change only the weight does NOT destroy the
// LastActivated field that was set on the original WriteAssociation.
//
// Currently FAILS because UpdateAssocWeight calls
// encodeAssocValue(0, 1.0, time.Time{}, 0), zeroing LastActivated.
func TestAssocMetadata_LastActivated_PreservedOnUpdate(t *testing.T) {
	store := newTestStore(t)

	ctx := context.Background()
	ws := store.VaultPrefix("assoc-meta-lastact")

	src := NewULID()
	dst := NewULID()

	lastAct := int32(time.Now().Add(-2 * time.Hour).Unix())

	// Write association with a specific LastActivated value.
	if err := store.WriteAssociation(ctx, ws, src, dst, &Association{
		TargetID:      dst,
		Weight:        0.5,
		RelType:       RelSupports,
		Confidence:    0.8,
		LastActivated: lastAct,
	}); err != nil {
		t.Fatalf("WriteAssociation: %v", err)
	}

	// Update only the weight — all other metadata should be preserved.
	if err := store.UpdateAssocWeight(ctx, ws, src, dst, 0.7, 0); err != nil {
		t.Fatalf("UpdateAssocWeight: %v", err)
	}

	// Read back on a fresh (cold-cache) store.
	fresh := newFreshStore(t, store.db)
	results, err := fresh.GetAssociations(ctx, ws, []ULID{src}, 10)
	if err != nil {
		t.Fatalf("GetAssociations: %v", err)
	}

	got, ok := results[src]
	if !ok || len(got) != 1 {
		t.Fatalf("expected 1 association for src, got %d", len(got))
	}

	a := got[0]

	// The weight should have been updated.
	if a.Weight < 0.69 || a.Weight > 0.71 {
		t.Errorf("Weight: got %v, want ~0.7", a.Weight)
	}

	// LastActivated must NOT have been zeroed by UpdateAssocWeight.
	// This is the core assertion that exposes the bug.
	if a.LastActivated == 0 {
		t.Errorf("LastActivated was zeroed by UpdateAssocWeight: got 0, want %d — metadata destruction bug", lastAct)
	}
}

// TestAssocMetadata_PreservedThroughDecay verifies that DecayAssocWeights
// preserves RelType, Confidence, and LastActivated on edges that survive decay.
//
// Currently FAILS because DecayAssocWeights calls
// encodeAssocValue(0, 1.0, time.Time{}, 0) when re-writing surviving edges,
// destroying all metadata fields.
func TestAssocMetadata_PreservedThroughDecay(t *testing.T) {
	store := newTestStore(t)

	ctx := context.Background()
	ws := store.VaultPrefix("assoc-meta-decay")

	src := NewULID()
	dst := NewULID()

	lastAct := int32(time.Now().Add(-30 * time.Minute).Unix())

	// Write association with rich metadata. Weight 0.8 * decay 0.9 = 0.72 > minWeight 0.05 → survives.
	if err := store.WriteAssociation(ctx, ws, src, dst, &Association{
		TargetID:      dst,
		Weight:        0.8,
		RelType:       RelType(5), // RelRelatesTo
		Confidence:    0.75,
		LastActivated: lastAct,
	}); err != nil {
		t.Fatalf("WriteAssociation: %v", err)
	}

	// Decay: factor 0.9, minWeight 0.05 — edge stays alive at 0.72.
	removed, err := store.DecayAssocWeights(ctx, ws, 0.9, 0.05, 0.0)
	if err != nil {
		t.Fatalf("DecayAssocWeights: %v", err)
	}
	if removed != 0 {
		t.Fatalf("expected 0 removed, got %d (edge should survive)", removed)
	}

	// Read back on a fresh (cold-cache) store.
	fresh := newFreshStore(t, store.db)
	results, err := fresh.GetAssociations(ctx, ws, []ULID{src}, 10)
	if err != nil {
		t.Fatalf("GetAssociations: %v", err)
	}

	got, ok := results[src]
	if !ok || len(got) != 1 {
		t.Fatalf("expected 1 surviving association, got %d", len(got))
	}

	a := got[0]

	// Weight should be decayed to ~0.72.
	if a.Weight < 0.70 || a.Weight > 0.74 {
		t.Errorf("Weight after decay: got %v, want ~0.72", a.Weight)
	}

	// RelType must be preserved through decay rewrite.
	if a.RelType != RelType(5) {
		t.Errorf("RelType destroyed by DecayAssocWeights: got %v, want 5 — metadata destruction bug", a.RelType)
	}

	// Confidence must be preserved through decay rewrite.
	if a.Confidence < 0.74 || a.Confidence > 0.76 {
		t.Errorf("Confidence destroyed by DecayAssocWeights: got %v, want ~0.75 — metadata destruction bug", a.Confidence)
	}

	// LastActivated must be preserved through decay rewrite.
	if a.LastActivated == 0 {
		t.Errorf("LastActivated zeroed by DecayAssocWeights: got 0, want %d — metadata destruction bug", lastAct)
	}
}

// TestAssocPeakWeight_TrackedAcrossUpdates verifies PeakWeight records
// the historical maximum and is never reduced by subsequent lower updates.
func TestAssocPeakWeight_TrackedAcrossUpdates(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("peak-tracking")
	src, dst := NewULID(), NewULID()

	// Write at 0.4
	if err := store.WriteAssociation(ctx, ws, src, dst, &Association{
		TargetID: dst, Weight: 0.4,
	}); err != nil {
		t.Fatalf("WriteAssociation: %v", err)
	}

	// Boost to 0.8 — peak becomes 0.8
	if err := store.UpdateAssocWeight(ctx, ws, src, dst, 0.8, 0); err != nil {
		t.Fatalf("UpdateAssocWeight to 0.8: %v", err)
	}

	// Drop to 0.3 — peak should remain 0.8
	if err := store.UpdateAssocWeight(ctx, ws, src, dst, 0.3, 0); err != nil {
		t.Fatalf("UpdateAssocWeight to 0.3: %v", err)
	}

	// Open fresh store (bypass cache) to read from Pebble directly
	fresh := newFreshStore(t, store.db)
	assocs, err := fresh.GetAssociations(ctx, ws, []ULID{src}, 10)
	if err != nil {
		t.Fatalf("GetAssociations: %v", err)
	}
	got := assocs[src]
	if len(got) != 1 {
		t.Fatalf("expected 1 association, got %d", len(got))
	}
	if got[0].PeakWeight != 0.8 {
		t.Errorf("PeakWeight: want 0.8, got %.4f (must not decrease)", got[0].PeakWeight)
	}
	if got[0].Weight != 0.3 {
		t.Errorf("Weight: want 0.3, got %.4f", got[0].Weight)
	}
}

// TestAssocPeakWeight_InitialWriteSetsPeak verifies WriteAssociation seeds PeakWeight = Weight.
func TestAssocPeakWeight_InitialWriteSetsPeak(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("peak-initial")
	src, dst := NewULID(), NewULID()

	if err := store.WriteAssociation(ctx, ws, src, dst, &Association{
		TargetID: dst, Weight: 0.7,
	}); err != nil {
		t.Fatalf("WriteAssociation: %v", err)
	}

	fresh := newFreshStore(t, store.db)
	assocs, err := fresh.GetAssociations(ctx, ws, []ULID{src}, 10)
	if err != nil {
		t.Fatalf("GetAssociations: %v", err)
	}
	if len(assocs[src]) != 1 {
		t.Fatalf("expected 1 association, got %d", len(assocs[src]))
	}
	if assocs[src][0].PeakWeight != 0.7 {
		t.Errorf("PeakWeight on initial write: want 0.7, got %.4f", assocs[src][0].PeakWeight)
	}
}

// TestAssocDecay_DynamicFloor verifies associations with earned PeakWeight are
// clamped to PeakWeight*0.05 floor rather than deleted when below minWeight.
func TestAssocDecay_DynamicFloor(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("dynamic-floor")
	src, dst := NewULID(), NewULID()

	// Write at 0.8 — PeakWeight is seeded to 0.8 by WriteAssociation.
	if err := store.WriteAssociation(ctx, ws, src, dst, &Association{
		TargetID: dst, Weight: 0.8,
	}); err != nil {
		t.Fatalf("WriteAssociation: %v", err)
	}

	// Decay aggressively — 5 passes of 0.3 factor.
	// 0.8 * 0.3^5 ≈ 0.002, well below minWeight=0.05.
	for i := 0; i < 5; i++ {
		if _, err := store.DecayAssocWeights(ctx, ws, 0.3, 0.05, 0.0); err != nil {
			t.Fatalf("DecayAssocWeights pass %d: %v", i, err)
		}
	}

	fresh := newFreshStore(t, store.db)
	assocs, err := fresh.GetAssociations(ctx, ws, []ULID{src}, 10)
	if err != nil {
		t.Fatalf("GetAssociations: %v", err)
	}
	got := assocs[src]
	if len(got) != 1 {
		t.Fatalf("edge should survive via dynamic floor (PeakWeight=0.8 → floor=0.04), got %d edges", len(got))
	}
	// Floor = 0.8 * 0.05 = 0.04
	wantFloor := float32(0.8 * 0.05)
	tolerance := float32(0.001)
	if got[0].Weight < wantFloor-tolerance || got[0].Weight > wantFloor+tolerance {
		t.Errorf("weight should be clamped to floor ~%.4f, got %.4f", wantFloor, got[0].Weight)
	}
	if got[0].PeakWeight < 0.8-tolerance {
		t.Errorf("PeakWeight should remain ~0.8, got %.4f", got[0].PeakWeight)
	}
}

// TestAssocDecay_LowPeakEdgeClampsToVeryLowFloor verifies that an edge that never earned
// a meaningful peak is clamped to its (very low) floor and
// is NOT returned by GetAssociations (below practical threshold).
// Note: After Task 5, all associations bootstrap peakWeight from oldW, so
// the "no peak" scenario is only possible for legacy zero-weight entries.
func TestAssocDecay_LowPeakEdgeClampsToVeryLowFloor(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("low-floor")
	src, dst := NewULID(), NewULID()

	// Write at just above minWeight — peak bootstraps to this value.
	if err := store.WriteAssociation(ctx, ws, src, dst, &Association{
		TargetID: dst, Weight: 0.06, // PeakWeight seeds to 0.06
	}); err != nil {
		t.Fatalf("WriteAssociation: %v", err)
	}

	// Decay below minWeight — floor = 0.06 * 0.05 = 0.003
	if _, err := store.DecayAssocWeights(ctx, ws, 0.5, 0.05, 0.0); err != nil {
		t.Fatalf("DecayAssocWeights: %v", err)
	}

	fresh := newFreshStore(t, store.db)
	assocs, err := fresh.GetAssociations(ctx, ws, []ULID{src}, 10)
	if err != nil {
		t.Fatalf("GetAssociations: %v", err)
	}
	if len(assocs[src]) != 1 {
		t.Fatalf("expected edge to survive at floor (PeakWeight=0.06 → floor=0.003), got %d edges", len(assocs[src]))
	}
	got := assocs[src][0]
	if got.PeakWeight < 0.06-0.001 {
		t.Errorf("PeakWeight should be ~0.06, got %.4f", got.PeakWeight)
	}
	if got.Weight > 0.01 {
		t.Errorf("clamped weight should be very low (<0.01), got %.4f", got.Weight)
	}
}

// TestAssocPeakWeight_BatchUpdatePreservesPeak verifies UpdateAssocWeightBatch
// correctly tracks PeakWeight across batch weight updates.
func TestAssocPeakWeight_BatchUpdatePreservesPeak(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("peak-batch")
	src, dst := NewULID(), NewULID()

	// Write initial association at 0.4
	if err := store.WriteAssociation(ctx, ws, src, dst, &Association{
		TargetID: dst, Weight: 0.4,
	}); err != nil {
		t.Fatalf("WriteAssociation: %v", err)
	}

	// Batch-update to 0.9 — peak becomes 0.9
	updates := []AssocWeightUpdate{{WS: ws, Src: src, Dst: dst, Weight: 0.9}}
	if err := store.UpdateAssocWeightBatch(ctx, updates); err != nil {
		t.Fatalf("UpdateAssocWeightBatch to 0.9: %v", err)
	}

	// Batch-update back to 0.2 — peak should remain 0.9
	updates[0].Weight = 0.2
	if err := store.UpdateAssocWeightBatch(ctx, updates); err != nil {
		t.Fatalf("UpdateAssocWeightBatch to 0.2: %v", err)
	}

	// Read via fresh store to bypass cache
	fresh := newFreshStore(t, store.db)
	assocs, err := fresh.GetAssociations(ctx, ws, []ULID{src}, 10)
	if err != nil {
		t.Fatalf("GetAssociations: %v", err)
	}
	got := assocs[src]
	if len(got) != 1 {
		t.Fatalf("expected 1 association, got %d", len(got))
	}
	if got[0].PeakWeight != 0.9 {
		t.Errorf("PeakWeight after batch update: want 0.9, got %.4f", got[0].PeakWeight)
	}
	if got[0].Weight != 0.2 {
		t.Errorf("Weight after batch update: want 0.2, got %.4f", got[0].Weight)
	}
}

// TestAssocDecay_RecencySkip verifies that an edge activated very recently
// (within the last few minutes) is NOT decayed, even with an aggressive factor.
//
// This is a recency-skip feature that does not yet exist. Currently FAILS
// because DecayAssocWeights decays all edges unconditionally, including those
// that were just activated.
func TestAssocDecay_RecencySkip(t *testing.T) {
	store := newTestStore(t)

	ctx := context.Background()
	ws := store.VaultPrefix("assoc-recency-skip")

	src := NewULID()
	dst := NewULID()

	// LastActivated = right now — should be skipped by recency-aware decay.
	recentlyActivated := int32(time.Now().Unix())

	const initialWeight = float32(0.8)

	if err := store.WriteAssociation(ctx, ws, src, dst, &Association{
		TargetID:      dst,
		Weight:        initialWeight,
		RelType:       RelSupports,
		Confidence:    1.0,
		LastActivated: recentlyActivated,
	}); err != nil {
		t.Fatalf("WriteAssociation: %v", err)
	}

	// Aggressive decay factor 0.1 with minWeight 0.05.
	// Without recency skip: 0.8 * 0.1 = 0.08 (survives but weight is massacred).
	// With recency skip: weight must remain at 0.8 unchanged.
	removed, err := store.DecayAssocWeights(ctx, ws, 0.1, 0.05, 0.0)
	if err != nil {
		t.Fatalf("DecayAssocWeights: %v", err)
	}

	// Edge must not have been removed.
	if removed != 0 {
		t.Fatalf("recently-activated edge was removed by decay: removed=%d, want 0", removed)
	}

	// Read back the weight — it must be unchanged at ~0.8.
	w, err := store.GetAssocWeight(ctx, ws, src, dst)
	if err != nil {
		t.Fatalf("GetAssocWeight: %v", err)
	}

	if w < 0.75 || w > 0.85 {
		t.Errorf("recently-activated edge weight was decayed: got %v, want ~%.1f — recency skip not implemented", w, initialWeight)
	}
}
