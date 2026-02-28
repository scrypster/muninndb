package storage

import (
	"context"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// associationsForOne tests
// ---------------------------------------------------------------------------

// TestAssociationsForOne_CacheMiss verifies that associationsForOne reads from
// Pebble (cache miss path) and returns the written edge.
func TestAssociationsForOne_CacheMiss(t *testing.T) {
	store := newTestStore(t)

	ctx := context.Background()
	ws := store.VaultPrefix("assoc-for-one-miss")

	idA := NewULID()
	idB := NewULID()

	// Write an engram and an association A → B.
	_, err := store.WriteEngram(ctx, ws, &Engram{Concept: "A", Content: "a"})
	if err != nil {
		t.Fatalf("WriteEngram A: %v", err)
	}
	if err := store.WriteAssociation(ctx, ws, idA, idB, &Association{
		TargetID: idB,
		Weight:   0.75,
		RelType:  RelSupports,
	}); err != nil {
		t.Fatalf("WriteAssociation: %v", err)
	}

	// Use a fresh store so the assoc cache is cold.
	fresh := NewPebbleStore(store.db, PebbleStoreConfig{CacheSize: 100})

	assocs, err := fresh.associationsForOne(ws, idA, 50)
	if err != nil {
		t.Fatalf("associationsForOne: %v", err)
	}
	if len(assocs) != 1 {
		t.Fatalf("expected 1 association, got %d", len(assocs))
	}
	got := assocs[0]
	if got.TargetID != idB {
		t.Errorf("TargetID: got %v, want %v", got.TargetID, idB)
	}
	if got.Weight < 0.74 || got.Weight > 0.76 {
		t.Errorf("Weight: got %v, want ~0.75", got.Weight)
	}
}

// TestAssociationsForOne_NoEdges verifies that associationsForOne returns an
// empty (non-nil) slice for an engram with no outbound edges.
func TestAssociationsForOne_NoEdges(t *testing.T) {
	store := newTestStore(t)

	ws := store.VaultPrefix("assoc-for-one-empty")
	idA := NewULID()

	assocs, err := store.associationsForOne(ws, idA, 50)
	if err != nil {
		t.Fatalf("associationsForOne: %v", err)
	}
	if len(assocs) != 0 {
		t.Errorf("expected 0 associations, got %d", len(assocs))
	}
}

// ---------------------------------------------------------------------------
// UpdateAssocWeightBatch tests
// ---------------------------------------------------------------------------

// TestUpdateAssocWeightBatch_SingleUpdate verifies that a batch update of a
// single edge is reflected via GetAssociations.
func TestUpdateAssocWeightBatch_SingleUpdate(t *testing.T) {
	store := newTestStore(t)

	ctx := context.Background()
	ws := store.VaultPrefix("batch-update-single")

	idA := NewULID()
	idB := NewULID()

	// Write initial edge with weight 0.5.
	if err := store.WriteAssociation(ctx, ws, idA, idB, &Association{
		TargetID: idB,
		Weight:   0.5,
	}); err != nil {
		t.Fatalf("WriteAssociation: %v", err)
	}

	// Batch-update the edge weight to 0.8.
	updates := []AssocWeightUpdate{
		{WS: ws, Src: idA, Dst: idB, Weight: 0.8},
	}
	if err := store.UpdateAssocWeightBatch(ctx, updates); err != nil {
		t.Fatalf("UpdateAssocWeightBatch: %v", err)
	}

	// Verify via GetAssocWeight (O(1) index path).
	w, err := store.GetAssocWeight(ctx, ws, idA, idB)
	if err != nil {
		t.Fatalf("GetAssocWeight: %v", err)
	}
	if w < 0.79 || w > 0.81 {
		t.Errorf("GetAssocWeight after batch update: got %v, want ~0.8", w)
	}

	// Verify via GetAssociations on a fresh (cold-cache) store.
	fresh := NewPebbleStore(store.db, PebbleStoreConfig{CacheSize: 100})
	results, err := fresh.GetAssociations(ctx, ws, []ULID{idA}, 10)
	if err != nil {
		t.Fatalf("GetAssociations: %v", err)
	}
	got := results[idA]
	if len(got) != 1 {
		t.Fatalf("expected 1 association after update, got %d", len(got))
	}
	if got[0].Weight < 0.79 || got[0].Weight > 0.81 {
		t.Errorf("GetAssociations weight after batch update: got %v, want ~0.8", got[0].Weight)
	}
}

// TestUpdateAssocWeightBatch_EmptyInput verifies that an empty batch is a no-op
// and returns no error.
func TestUpdateAssocWeightBatch_EmptyInput(t *testing.T) {
	store := newTestStore(t)

	ctx := context.Background()
	if err := store.UpdateAssocWeightBatch(ctx, []AssocWeightUpdate{}); err != nil {
		t.Fatalf("UpdateAssocWeightBatch with empty input: %v", err)
	}
}

// ---------------------------------------------------------------------------
// GetContradictions tests
// ---------------------------------------------------------------------------

// TestGetContradictions_Empty verifies that a fresh vault returns an empty slice.
func TestGetContradictions_Empty(t *testing.T) {
	store := newTestStore(t)

	ctx := context.Background()
	ws := store.VaultPrefix("contra-empty")

	pairs, err := store.GetContradictions(ctx, ws)
	if err != nil {
		t.Fatalf("GetContradictions: %v", err)
	}
	if len(pairs) != 0 {
		t.Errorf("expected 0 contradiction pairs, got %d", len(pairs))
	}
}

// TestGetContradictions_WithPairs verifies that after flagging contradictions via
// FlagContradiction, GetContradictions returns the deduplicated pairs.
func TestGetContradictions_WithPairs(t *testing.T) {
	store := newTestStore(t)

	ctx := context.Background()
	ws := store.VaultPrefix("contra-pairs")

	idA := NewULID()
	idB := NewULID()
	idC := NewULID()

	// Flag two distinct contradiction pairs.
	if err := store.FlagContradiction(ctx, ws, idA, idB); err != nil {
		t.Fatalf("FlagContradiction(A,B): %v", err)
	}
	if err := store.FlagContradiction(ctx, ws, idA, idC); err != nil {
		t.Fatalf("FlagContradiction(A,C): %v", err)
	}
	// Flag the same pair again in reverse order — should NOT produce a duplicate.
	if err := store.FlagContradiction(ctx, ws, idB, idA); err != nil {
		t.Fatalf("FlagContradiction(B,A): %v", err)
	}

	pairs, err := store.GetContradictions(ctx, ws)
	if err != nil {
		t.Fatalf("GetContradictions: %v", err)
	}
	// Expect exactly 2 unique pairs: (A,B) and (A,C).
	if len(pairs) != 2 {
		t.Fatalf("expected 2 contradiction pairs, got %d: %v", len(pairs), pairs)
	}

	type pairKey [32]byte
	seen := make(map[pairKey]bool)
	for _, p := range pairs {
		// Each pair must have canonical order (smaller first).
		if CompareULIDs(p[0], p[1]) > 0 {
			t.Errorf("pair not in canonical order: %v > %v", p[0], p[1])
		}
		var k pairKey
		copy(k[:16], p[0][:])
		copy(k[16:], p[1][:])
		if seen[k] {
			t.Errorf("duplicate pair returned: %v", p)
		}
		seen[k] = true
	}

	// Both idA–idB and idA–idC must be present.
	canonPair := func(a, b ULID) pairKey {
		if CompareULIDs(a, b) > 0 {
			a, b = b, a
		}
		var k pairKey
		copy(k[:16], a[:])
		copy(k[16:], b[:])
		return k
	}
	if !seen[canonPair(idA, idB)] {
		t.Error("pair (A,B) not found in GetContradictions result")
	}
	if !seen[canonPair(idA, idC)] {
		t.Error("pair (A,C) not found in GetContradictions result")
	}
}


// TestResolveContradiction verifies that ResolveContradiction removes both
// directions of the contradiction marker and GetContradictions no longer returns the pair.
func TestResolveContradiction(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("contra-resolve")

	idA := NewULID()
	idB := NewULID()
	idC := NewULID()

	// Flag two pairs.
	if err := store.FlagContradiction(ctx, ws, idA, idB); err != nil {
		t.Fatalf("FlagContradiction(A,B): %v", err)
	}
	if err := store.FlagContradiction(ctx, ws, idA, idC); err != nil {
		t.Fatalf("FlagContradiction(A,C): %v", err)
	}

	// Resolve the (A,B) pair.
	if err := store.ResolveContradiction(ctx, ws, idA, idB); err != nil {
		t.Fatalf("ResolveContradiction(A,B): %v", err)
	}

	pairs, err := store.GetContradictions(ctx, ws)
	if err != nil {
		t.Fatalf("GetContradictions: %v", err)
	}
	// Only (A,C) should remain.
	if len(pairs) != 1 {
		t.Fatalf("expected 1 pair after resolve, got %d: %v", len(pairs), pairs)
	}
}

// TestResolveContradiction_BothDirections verifies that ResolveContradiction works
// regardless of which direction (a,b) or (b,a) the caller passes.
func TestResolveContradiction_BothDirections(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("contra-resolve-dir")

	idA := NewULID()
	idB := NewULID()

	if err := store.FlagContradiction(ctx, ws, idA, idB); err != nil {
		t.Fatalf("FlagContradiction: %v", err)
	}

	// Resolve using (b,a) order — must still remove both directions.
	if err := store.ResolveContradiction(ctx, ws, idB, idA); err != nil {
		t.Fatalf("ResolveContradiction(B,A): %v", err)
	}

	pairs, err := store.GetContradictions(ctx, ws)
	if err != nil {
		t.Fatalf("GetContradictions: %v", err)
	}
	if len(pairs) != 0 {
		t.Fatalf("expected 0 pairs after resolve, got %d: %v", len(pairs), pairs)
	}
}

// newTestStore creates a PebbleStore backed by a temp dir.
// openTestPebble already registers Cleanup for the DB; we just wrap it in a store.
func newTestStore(t *testing.T) *PebbleStore {
	t.Helper()
	db := openTestPebble(t)
	return NewPebbleStore(db, PebbleStoreConfig{CacheSize: 100})
}

// TestWriteAssociationGetAssociationsRoundtrip verifies that WriteAssociation persists
// the edge and GetAssociations retrieves it with the correct fields.
func TestWriteAssociationGetAssociationsRoundtrip(t *testing.T) {
	store := newTestStore(t)

	ctx := context.Background()
	ws := store.VaultPrefix("assoc-roundtrip")

	src := NewULID()
	dst := NewULID()

	now := time.Now().Truncate(time.Millisecond)
	assoc := &Association{
		TargetID:      dst,
		RelType:       RelSupports,
		Weight:        0.65,
		Confidence:    0.9,
		CreatedAt:     now,
		LastActivated: int32(now.Unix()),
	}

	if err := store.WriteAssociation(ctx, ws, src, dst, assoc); err != nil {
		t.Fatalf("WriteAssociation: %v", err)
	}

	results, err := store.GetAssociations(ctx, ws, []ULID{src}, 10)
	if err != nil {
		t.Fatalf("GetAssociations: %v", err)
	}

	got, ok := results[src]
	if !ok {
		t.Fatal("no associations returned for src")
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 association, got %d", len(got))
	}

	a := got[0]
	if a.TargetID != dst {
		t.Errorf("TargetID: got %v, want %v", a.TargetID, dst)
	}
	if a.RelType != RelSupports {
		t.Errorf("RelType: got %v, want %v", a.RelType, RelSupports)
	}
	if a.Weight < 0.64 || a.Weight > 0.66 {
		t.Errorf("Weight: got %v, want ~0.65", a.Weight)
	}
	if a.Confidence < 0.89 || a.Confidence > 0.91 {
		t.Errorf("Confidence: got %v, want ~0.9", a.Confidence)
	}
}

// TestUpdateAssocWeightPersistsCorrectly verifies that after UpdateAssocWeight the new
// weight is reflected in GetAssociations (not just the index key).
func TestUpdateAssocWeightPersistsCorrectly(t *testing.T) {
	store := newTestStore(t)

	ctx := context.Background()
	ws := store.VaultPrefix("assoc-update")

	src := NewULID()
	dst := NewULID()

	// Write initial association.
	if err := store.WriteAssociation(ctx, ws, src, dst, &Association{
		TargetID: dst,
		Weight:   0.2,
	}); err != nil {
		t.Fatalf("WriteAssociation: %v", err)
	}

	// Verify initial weight via GetAssociations.
	results, err := store.GetAssociations(ctx, ws, []ULID{src}, 10)
	if err != nil {
		t.Fatalf("GetAssociations (initial): %v", err)
	}
	if got := results[src]; len(got) != 1 || got[0].Weight < 0.19 || got[0].Weight > 0.21 {
		t.Fatalf("initial weight unexpected: %+v", results[src])
	}

	// Update weight.
	if err := store.UpdateAssocWeight(ctx, ws, src, dst, 0.85); err != nil {
		t.Fatalf("UpdateAssocWeight: %v", err)
	}

	// Verify updated weight via GetAssocWeight (O(1) index path).
	w, err := store.GetAssocWeight(ctx, ws, src, dst)
	if err != nil {
		t.Fatalf("GetAssocWeight: %v", err)
	}
	if w < 0.84 || w > 0.86 {
		t.Errorf("GetAssocWeight after update: got %v, want ~0.85", w)
	}

	// Force a cache miss by creating a fresh store backed by the same DB.
	store2 := NewPebbleStore(store.db, PebbleStoreConfig{CacheSize: 100})
	results2, err := store2.GetAssociations(ctx, ws, []ULID{src}, 10)
	if err != nil {
		t.Fatalf("GetAssociations (fresh store): %v", err)
	}
	got2 := results2[src]
	if len(got2) != 1 {
		t.Fatalf("expected 1 assoc after update in fresh store, got %d", len(got2))
	}
	if got2[0].Weight < 0.84 || got2[0].Weight > 0.86 {
		t.Errorf("persisted weight wrong: got %v, want ~0.85", got2[0].Weight)
	}
}

// TestDecayAssocWeightsReducesBelowThreshold verifies that DecayAssocWeights
// removes associations whose weight falls below the minWeight threshold.
func TestDecayAssocWeightsReducesBelowThreshold(t *testing.T) {
	store := newTestStore(t)

	ctx := context.Background()
	ws := store.VaultPrefix("decay-roundtrip")

	// Write three associations with different weights.
	pairs := [][2]ULID{
		{NewULID(), NewULID()}, // weight 0.8 — stays after 50% decay (0.4 > 0.3)
		{NewULID(), NewULID()}, // weight 0.5 — stays after 50% decay (0.25 < 0.3, removed)
		{NewULID(), NewULID()}, // weight 0.1 — removed after 50% decay (0.05 < 0.3)
	}
	weights := []float32{0.8, 0.5, 0.1}

	for i, p := range pairs {
		if err := store.WriteAssociation(ctx, ws, p[0], p[1], &Association{
			TargetID: p[1],
			Weight:   weights[i],
		}); err != nil {
			t.Fatalf("WriteAssociation[%d]: %v", i, err)
		}
	}

	// Decay by 50% with minWeight=0.3 — should remove pairs[1] and pairs[2].
	removed, err := store.DecayAssocWeights(ctx, ws, 0.5, 0.3)
	if err != nil {
		t.Fatalf("DecayAssocWeights: %v", err)
	}
	if removed != 2 {
		t.Errorf("expected 2 removed, got %d", removed)
	}

	// pairs[0] should survive with weight ~0.4.
	w0, err := store.GetAssocWeight(ctx, ws, pairs[0][0], pairs[0][1])
	if err != nil {
		t.Fatalf("GetAssocWeight[0]: %v", err)
	}
	if w0 < 0.35 || w0 > 0.45 {
		t.Errorf("surviving weight: got %v, want ~0.4", w0)
	}

	// pairs[1] should be gone.
	w1, err := store.GetAssocWeight(ctx, ws, pairs[1][0], pairs[1][1])
	if err != nil {
		t.Fatalf("GetAssocWeight[1]: %v", err)
	}
	if w1 != 0.0 {
		t.Errorf("decayed-below-min weight should be 0, got %v", w1)
	}

	// pairs[2] should be gone.
	w2, err := store.GetAssocWeight(ctx, ws, pairs[2][0], pairs[2][1])
	if err != nil {
		t.Fatalf("GetAssocWeight[2]: %v", err)
	}
	if w2 != 0.0 {
		t.Errorf("decayed-below-min weight should be 0, got %v", w2)
	}
}

// TestGetAssociationsMultipleSourceIDs verifies batch retrieval works correctly
// for multiple source IDs in a single call.
func TestGetAssociationsMultipleSourceIDs(t *testing.T) {
	store := newTestStore(t)

	ctx := context.Background()
	ws := store.VaultPrefix("assoc-batch")

	srcA := NewULID()
	srcB := NewULID()
	dst1 := NewULID()
	dst2 := NewULID()
	dst3 := NewULID()

	_ = store.WriteAssociation(ctx, ws, srcA, dst1, &Association{TargetID: dst1, Weight: 0.7})
	_ = store.WriteAssociation(ctx, ws, srcA, dst2, &Association{TargetID: dst2, Weight: 0.5})
	_ = store.WriteAssociation(ctx, ws, srcB, dst3, &Association{TargetID: dst3, Weight: 0.9})

	results, err := store.GetAssociations(ctx, ws, []ULID{srcA, srcB}, 10)
	if err != nil {
		t.Fatalf("GetAssociations: %v", err)
	}

	if len(results[srcA]) != 2 {
		t.Errorf("srcA: expected 2 associations, got %d", len(results[srcA]))
	}
	if len(results[srcB]) != 1 {
		t.Errorf("srcB: expected 1 association, got %d", len(results[srcB]))
	}
	if results[srcB][0].TargetID != dst3 {
		t.Errorf("srcB target: got %v, want %v", results[srcB][0].TargetID, dst3)
	}
}
