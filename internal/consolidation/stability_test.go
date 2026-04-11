package consolidation

import (
	"context"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/storage"
)

func TestStability_HighSignalStrengthened(t *testing.T) {
	store, cleanup := testStore(t)
	defer cleanup()

	ctx := context.Background()
	vault := "stability_high"
	wsPrefix := store.ResolveVaultPrefix(vault)

	eng := &storage.Engram{
		Concept:     "high_signal",
		Content:     "frequently accessed content",
		Confidence:  0.9,
		Relevance:   0.8,
		Stability:   20.0,
		AccessCount: 6,
		CreatedAt:   time.Now().Add(-10 * 24 * time.Hour),
	}
	id, err := store.WriteEngram(ctx, wsPrefix, eng)
	if err != nil {
		t.Fatal(err)
	}

	w := &Worker{DryRun: false}
	report := &ConsolidationReport{}

	if err := w.runPhase4BidirectionalStability(ctx, store, wsPrefix, report, vault); err != nil {
		t.Fatal(err)
	}

	if report.StabilityStrengthened != 1 {
		t.Errorf("StabilityStrengthened = %d, want 1", report.StabilityStrengthened)
	}
	if report.StabilityWeakened != 0 {
		t.Errorf("StabilityWeakened = %d, want 0", report.StabilityWeakened)
	}

	updated, err := store.GetEngram(ctx, wsPrefix, id)
	if err != nil {
		t.Fatal(err)
	}

	// Stability 20 * 1.2 = 24
	want := float32(24.0)
	if updated.Stability != want {
		t.Errorf("Stability = %f, want %f", updated.Stability, want)
	}
}

func TestStability_LowSignalWeakened(t *testing.T) {
	store, cleanup := testStore(t)
	defer cleanup()

	ctx := context.Background()
	vault := "stability_low"
	wsPrefix := store.ResolveVaultPrefix(vault)

	oldTime := time.Now().Add(-45 * 24 * time.Hour)
	eng := &storage.Engram{
		Concept:     "low_signal",
		Content:     "rarely accessed old content",
		Confidence:  0.2,
		Relevance:   0.2,
		Stability:   20.0,
		AccessCount: 1,
		CreatedAt:   oldTime,
		UpdatedAt:   oldTime,
		LastAccess:  oldTime,
	}
	id, err := store.WriteEngram(ctx, wsPrefix, eng)
	if err != nil {
		t.Fatal(err)
	}

	w := &Worker{DryRun: false}
	report := &ConsolidationReport{}

	if err := w.runPhase4BidirectionalStability(ctx, store, wsPrefix, report, vault); err != nil {
		t.Fatal(err)
	}

	if report.StabilityWeakened != 1 {
		t.Errorf("StabilityWeakened = %d, want 1", report.StabilityWeakened)
	}
	if report.StabilityStrengthened != 0 {
		t.Errorf("StabilityStrengthened = %d, want 0", report.StabilityStrengthened)
	}

	updated, err := store.GetEngram(ctx, wsPrefix, id)
	if err != nil {
		t.Fatal(err)
	}

	// Stability 20 * 0.8 = 16
	want := float32(16.0)
	if updated.Stability != want {
		t.Errorf("Stability = %f, want %f", updated.Stability, want)
	}
}

func TestStability_FloorAtDefaultStability(t *testing.T) {
	store, cleanup := testStore(t)
	defer cleanup()

	ctx := context.Background()
	vault := "stability_floor"
	wsPrefix := store.ResolveVaultPrefix(vault)

	oldTime := time.Now().Add(-60 * 24 * time.Hour)
	eng := &storage.Engram{
		Concept:     "very_low_signal",
		Content:     "ancient barely-accessed content",
		Confidence:  0.1,
		Relevance:   0.1,
		Stability:   15.0, // 15 * 0.8 = 12, below floor of 14
		AccessCount: 0,
		CreatedAt:   oldTime,
		UpdatedAt:   oldTime,
		LastAccess:  oldTime,
	}
	id, err := store.WriteEngram(ctx, wsPrefix, eng)
	if err != nil {
		t.Fatal(err)
	}

	w := &Worker{DryRun: false}
	report := &ConsolidationReport{}

	if err := w.runPhase4BidirectionalStability(ctx, store, wsPrefix, report, vault); err != nil {
		t.Fatal(err)
	}

	if report.StabilityWeakened != 1 {
		t.Errorf("StabilityWeakened = %d, want 1", report.StabilityWeakened)
	}

	updated, err := store.GetEngram(ctx, wsPrefix, id)
	if err != nil {
		t.Fatal(err)
	}

	// max(15 * 0.8, 14.0) = max(12, 14) = 14
	want := float32(14.0)
	if updated.Stability != want {
		t.Errorf("Stability = %f, want %f (floor)", updated.Stability, want)
	}
}

func TestStability_LegalVaultExempt(t *testing.T) {
	store, cleanup := testStore(t)
	defer cleanup()

	ctx := context.Background()
	vault := "legal:contracts"
	wsPrefix := store.ResolveVaultPrefix(vault)

	oldTime := time.Now().Add(-45 * 24 * time.Hour)
	eng := &storage.Engram{
		Concept:     "contract",
		Content:     "legal contract content",
		Confidence:  0.9,
		Relevance:   0.2,
		Stability:   20.0,
		AccessCount: 1,
		CreatedAt:   oldTime,
	}
	id, err := store.WriteEngram(ctx, wsPrefix, eng)
	if err != nil {
		t.Fatal(err)
	}

	w := &Worker{DryRun: false}
	report := &ConsolidationReport{}

	if err := w.runPhase4BidirectionalStability(ctx, store, wsPrefix, report, vault); err != nil {
		t.Fatal(err)
	}

	// Legal vault — no changes
	if report.StabilityStrengthened != 0 || report.StabilityWeakened != 0 {
		t.Errorf("legal vault should be exempt: strengthened=%d weakened=%d",
			report.StabilityStrengthened, report.StabilityWeakened)
	}

	// Stability must be unchanged
	unchanged, err := store.GetEngram(ctx, wsPrefix, id)
	if err != nil {
		t.Fatal(err)
	}
	if unchanged.Stability != 20.0 {
		t.Errorf("Stability = %f, want 20.0 (legal vault exempt)", unchanged.Stability)
	}
}

func TestStability_DryRunNoMutation(t *testing.T) {
	store, cleanup := testStore(t)
	defer cleanup()

	ctx := context.Background()
	vault := "stability_dryrun"
	wsPrefix := store.ResolveVaultPrefix(vault)

	// High-signal engram — would normally be strengthened
	engHigh := &storage.Engram{
		Concept:     "high_signal",
		Content:     "frequently accessed",
		Confidence:  0.9,
		Relevance:   0.8,
		Stability:   20.0,
		AccessCount: 7,
		CreatedAt:   time.Now().Add(-5 * 24 * time.Hour),
	}
	idHigh, err := store.WriteEngram(ctx, wsPrefix, engHigh)
	if err != nil {
		t.Fatal(err)
	}

	// Low-signal engram — would normally be weakened
	oldTime := time.Now().Add(-40 * 24 * time.Hour)
	engLow := &storage.Engram{
		Concept:     "low_signal",
		Content:     "rarely accessed old content",
		Confidence:  0.1,
		Relevance:   0.2,
		Stability:   20.0,
		AccessCount: 1,
		CreatedAt:   oldTime,
		UpdatedAt:   oldTime,
		LastAccess:  oldTime,
	}
	idLow, err := store.WriteEngram(ctx, wsPrefix, engLow)
	if err != nil {
		t.Fatal(err)
	}

	w := &Worker{DryRun: true}
	report := &ConsolidationReport{}

	if err := w.runPhase4BidirectionalStability(ctx, store, wsPrefix, report, vault); err != nil {
		t.Fatal(err)
	}

	// Counts must be recorded even in dry-run
	if report.StabilityStrengthened != 1 {
		t.Errorf("DryRun StabilityStrengthened = %d, want 1 (counted)", report.StabilityStrengthened)
	}
	if report.StabilityWeakened != 1 {
		t.Errorf("DryRun StabilityWeakened = %d, want 1 (counted)", report.StabilityWeakened)
	}

	// But stability values must remain unchanged
	high, err := store.GetEngram(ctx, wsPrefix, idHigh)
	if err != nil {
		t.Fatal(err)
	}
	if high.Stability != 20.0 {
		t.Errorf("DryRun: high stability mutated to %f, want 20.0", high.Stability)
	}

	low, err := store.GetEngram(ctx, wsPrefix, idLow)
	if err != nil {
		t.Fatal(err)
	}
	if low.Stability != 20.0 {
		t.Errorf("DryRun: low stability mutated to %f, want 20.0", low.Stability)
	}
}

func TestStability_ArchivedEngramSkipped(t *testing.T) {
	store, cleanup := testStore(t)
	defer cleanup()

	ctx := context.Background()
	vault := "stability_archived"
	wsPrefix := store.ResolveVaultPrefix(vault)

	// Write a high-signal engram and archive it
	eng := &storage.Engram{
		Concept:     "archived_high",
		Content:     "content",
		Confidence:  0.9,
		Relevance:   0.8,
		Stability:   20.0,
		AccessCount: 10,
		CreatedAt:   time.Now().Add(-5 * 24 * time.Hour),
	}
	id, err := store.WriteEngram(ctx, wsPrefix, eng)
	if err != nil {
		t.Fatal(err)
	}

	// Archive the engram
	meta := &storage.EngramMeta{
		State:       storage.StateArchived,
		Confidence:  eng.Confidence,
		Relevance:   eng.Relevance,
		Stability:   eng.Stability,
		AccessCount: eng.AccessCount,
		UpdatedAt:   time.Now(),
		LastAccess:  eng.LastAccess,
	}
	if err := store.UpdateMetadata(ctx, wsPrefix, id, meta); err != nil {
		t.Fatal(err)
	}

	w := &Worker{DryRun: false}
	report := &ConsolidationReport{}

	if err := w.runPhase4BidirectionalStability(ctx, store, wsPrefix, report, vault); err != nil {
		t.Fatal(err)
	}

	// Archived engram must be skipped
	if report.StabilityStrengthened != 0 {
		t.Errorf("StabilityStrengthened = %d, want 0 (archived skipped)", report.StabilityStrengthened)
	}

	// Stability must be unchanged
	unchanged, err := store.GetEngram(ctx, wsPrefix, id)
	if err != nil {
		t.Fatal(err)
	}
	if unchanged.Stability != 20.0 {
		t.Errorf("archived engram stability = %f, want 20.0", unchanged.Stability)
	}
}
