package consolidation

import (
	"context"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/storage"
)

// TestTransitiveInference_PeakWeightFallback verifies Phase 5 creates A→C when
// A→B and B→C have PeakWeight >= 0.7 but current Weight < 0.7 due to decay.
// The inferred edge weight is based on current Weight (not Peak) to avoid
// inflating newly-created edges.
func TestTransitiveInference_PeakWeightFallback(t *testing.T) {
	store, cleanup := testStore(t)
	defer cleanup()

	ctx := context.Background()
	wsPrefix := store.ResolveVaultPrefix("transitive_peak")

	// Write 3 engrams — scanAllEngramIDs requires them to exist in storage.
	a := &storage.Engram{Concept: "a", Content: "content a", Confidence: 1.0, Relevance: 0.8, Stability: 30}
	b := &storage.Engram{Concept: "b", Content: "content b", Confidence: 1.0, Relevance: 0.8, Stability: 30}
	c := &storage.Engram{Concept: "c", Content: "content c", Confidence: 1.0, Relevance: 0.8, Stability: 30}

	idA, err := store.WriteEngram(ctx, wsPrefix, a)
	if err != nil {
		t.Fatalf("WriteEngram A: %v", err)
	}
	idB, err := store.WriteEngram(ctx, wsPrefix, b)
	if err != nil {
		t.Fatalf("WriteEngram B: %v", err)
	}
	idC, err := store.WriteEngram(ctx, wsPrefix, c)
	if err != nil {
		t.Fatalf("WriteEngram C: %v", err)
	}

	// Write A→B at weight 0.8 (above threshold); PeakWeight seeds to 0.8 via WriteAssociation.
	if err := store.WriteAssociation(ctx, wsPrefix, idA, idB, &storage.Association{
		TargetID: idB, Weight: 0.8, Confidence: 1.0, RelType: storage.RelSupports, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("WriteAssociation A→B: %v", err)
	}
	// Decay A→B to 0.5 — UpdateAssocWeight preserves PeakWeight monotonically (stays 0.8).
	if err := store.UpdateAssocWeight(ctx, wsPrefix, idA, idB, 0.5); err != nil {
		t.Fatalf("UpdateAssocWeight A→B to 0.5: %v", err)
	}

	// Write B→C at weight 0.8, then decay to 0.5 similarly.
	if err := store.WriteAssociation(ctx, wsPrefix, idB, idC, &storage.Association{
		TargetID: idC, Weight: 0.8, Confidence: 1.0, RelType: storage.RelSupports, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("WriteAssociation B→C: %v", err)
	}
	if err := store.UpdateAssocWeight(ctx, wsPrefix, idB, idC, 0.5); err != nil {
		t.Fatalf("UpdateAssocWeight B→C to 0.5: %v", err)
	}

	// Confirm A→C does not yet exist.
	priorWeight, err := store.GetAssocWeight(ctx, wsPrefix, idA, idC)
	if err != nil {
		t.Fatalf("GetAssocWeight A→C (pre-inference): %v", err)
	}
	if priorWeight > 0 {
		t.Fatalf("expected no A→C before inference, got weight %v", priorWeight)
	}

	// Run Phase 5.
	w := &Worker{MaxTransitive: 100}
	report := &ConsolidationReport{}
	if err := w.runPhase5TransitiveInference(ctx, store, wsPrefix, report); err != nil {
		t.Fatalf("runPhase5TransitiveInference: %v", err)
	}

	// Verify A→C was inferred.
	if report.InferredEdges != 1 {
		t.Errorf("expected 1 inferred edge (A→C via PeakWeight fallback), got %d", report.InferredEdges)
	}

	inferredWeight, err := store.GetAssocWeight(ctx, wsPrefix, idA, idC)
	if err != nil {
		t.Fatalf("GetAssocWeight A→C (post-inference): %v", err)
	}
	if inferredWeight <= 0 {
		t.Errorf("expected A→C to be created, got weight %v", inferredWeight)
	}

	// Inferred weight must use current Weight (0.5 * 0.5 * 0.8 = 0.2), not PeakWeight.
	expectedWeight := float32(0.5 * 0.5 * 0.8)
	if inferredWeight < expectedWeight-0.01 || inferredWeight > expectedWeight+0.01 {
		t.Errorf("inferred weight = %v, want ~%v (current weights, not peak)", inferredWeight, expectedWeight)
	}
}
