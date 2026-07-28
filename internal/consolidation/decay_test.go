package consolidation

import (
	"context"
	"testing"
	"time"

	"github.com/cockroachdb/pebble"
	"github.com/scrypster/muninndb/internal/storage"
)

func testStore(t *testing.T) (*storage.PebbleStore, func()) {
	t.Helper()
	db, err := pebble.Open(t.TempDir(), &pebble.Options{})
	if err != nil {
		t.Fatal(err)
	}
	store := storage.NewPebbleStore(db, storage.PebbleStoreConfig{CacheSize: 100})
	return store, func() { store.Close() }
}

func TestDecayAcceleration_AllCriteriaMet(t *testing.T) {
	store, cleanup := testStore(t)
	defer cleanup()

	ctx := context.Background()
	wsPrefix := store.ResolveVaultPrefix("decay_test")

	oldTime := time.Now().Add(-40 * 24 * time.Hour)
	eng := &storage.Engram{
		Concept:     "old_low_access",
		Content:     "some content",
		Confidence:  0.5,
		Relevance:   0.2,
		Stability:   30.0,
		AccessCount: 1,
		CreatedAt:   oldTime,
		UpdatedAt:   oldTime,
		LastAccess:  oldTime,
	}
	id, err := store.WriteEngram(ctx, wsPrefix, eng)
	if err != nil {
		t.Fatal(err)
	}

	w := &Worker{MaxTransitive: 100}
	report := &ConsolidationReport{}
	if err := w.runPhase4DecayAcceleration(ctx, store, wsPrefix, report); err != nil {
		t.Fatal(err)
	}

	if report.DecayedEngrams != 1 {
		t.Errorf("expected 1 decayed engram, got %d", report.DecayedEngrams)
	}

	updated, err := store.GetEngram(ctx, wsPrefix, id)
	if err != nil {
		t.Fatal(err)
	}
	expectedRel := float32(0.2 * 0.5)
	if updated.Relevance != expectedRel {
		t.Errorf("relevance = %v, want %v", updated.Relevance, expectedRel)
	}
}

func TestDecayAcceleration_SkipsHighAccess(t *testing.T) {
	store, cleanup := testStore(t)
	defer cleanup()

	ctx := context.Background()
	wsPrefix := store.ResolveVaultPrefix("decay_test2")

	oldTime := time.Now().Add(-40 * 24 * time.Hour)
	eng := &storage.Engram{
		Concept:     "high_access",
		Content:     "content",
		Confidence:  0.5,
		Relevance:   0.2,
		Stability:   30.0,
		AccessCount: 5,
		CreatedAt:   oldTime,
		LastAccess:  oldTime,
	}
	if _, err := store.WriteEngram(ctx, wsPrefix, eng); err != nil {
		t.Fatal(err)
	}

	w := &Worker{MaxTransitive: 100}
	report := &ConsolidationReport{}
	if err := w.runPhase4DecayAcceleration(ctx, store, wsPrefix, report); err != nil {
		t.Fatal(err)
	}

	if report.DecayedEngrams != 0 {
		t.Errorf("expected 0 decayed (high access), got %d", report.DecayedEngrams)
	}
}

func TestDecayAcceleration_SkipsNewEngrams(t *testing.T) {
	store, cleanup := testStore(t)
	defer cleanup()

	ctx := context.Background()
	wsPrefix := store.ResolveVaultPrefix("decay_test3")

	eng := &storage.Engram{
		Concept:     "new_engram",
		Content:     "content",
		Confidence:  0.5,
		Relevance:   0.2,
		Stability:   30.0,
		AccessCount: 0,
		CreatedAt:   time.Now(),
		LastAccess:  time.Now(),
	}
	if _, err := store.WriteEngram(ctx, wsPrefix, eng); err != nil {
		t.Fatal(err)
	}

	w := &Worker{MaxTransitive: 100}
	report := &ConsolidationReport{}
	if err := w.runPhase4DecayAcceleration(ctx, store, wsPrefix, report); err != nil {
		t.Fatal(err)
	}

	if report.DecayedEngrams != 0 {
		t.Errorf("expected 0 decayed (too new), got %d", report.DecayedEngrams)
	}
}

func TestDecayAcceleration_SkipsHighRelevance(t *testing.T) {
	store, cleanup := testStore(t)
	defer cleanup()

	ctx := context.Background()
	wsPrefix := store.ResolveVaultPrefix("decay_test4")

	oldTime := time.Now().Add(-40 * 24 * time.Hour)
	eng := &storage.Engram{
		Concept:     "high_relevance",
		Content:     "content",
		Confidence:  0.5,
		Relevance:   0.8,
		Stability:   30.0,
		AccessCount: 0,
		CreatedAt:   oldTime,
		LastAccess:  oldTime,
	}
	if _, err := store.WriteEngram(ctx, wsPrefix, eng); err != nil {
		t.Fatal(err)
	}

	w := &Worker{MaxTransitive: 100}
	report := &ConsolidationReport{}
	if err := w.runPhase4DecayAcceleration(ctx, store, wsPrefix, report); err != nil {
		t.Fatal(err)
	}

	if report.DecayedEngrams != 0 {
		t.Errorf("expected 0 decayed (high relevance), got %d", report.DecayedEngrams)
	}
}

func TestDecayAcceleration_DryRunNoMutation(t *testing.T) {
	store, cleanup := testStore(t)
	defer cleanup()

	ctx := context.Background()
	wsPrefix := store.ResolveVaultPrefix("decay_dry")

	oldTime := time.Now().Add(-40 * 24 * time.Hour)
	eng := &storage.Engram{
		Concept:     "dry_run_target",
		Content:     "content",
		Confidence:  0.5,
		Relevance:   0.2,
		Stability:   30.0,
		AccessCount: 1,
		CreatedAt:   oldTime,
		LastAccess:  oldTime,
	}
	id, err := store.WriteEngram(ctx, wsPrefix, eng)
	if err != nil {
		t.Fatal(err)
	}

	w := &Worker{DryRun: true, MaxTransitive: 100}
	report := &ConsolidationReport{}
	if err := w.runPhase4DecayAcceleration(ctx, store, wsPrefix, report); err != nil {
		t.Fatal(err)
	}

	if report.DecayedEngrams != 1 {
		t.Errorf("expected 1 decay candidate reported, got %d", report.DecayedEngrams)
	}

	unchanged, err := store.GetEngram(ctx, wsPrefix, id)
	if err != nil {
		t.Fatal(err)
	}
	if unchanged.Relevance != 0.2 {
		t.Errorf("dry run mutated relevance to %v", unchanged.Relevance)
	}
}

func TestDecayAcceleration_EmptyVault(t *testing.T) {
	store, cleanup := testStore(t)
	defer cleanup()

	ctx := context.Background()
	wsPrefix := store.ResolveVaultPrefix("empty")

	w := &Worker{MaxTransitive: 100}
	report := &ConsolidationReport{}
	if err := w.runPhase4DecayAcceleration(ctx, store, wsPrefix, report); err != nil {
		t.Fatal(err)
	}

	if report.DecayedEngrams != 0 {
		t.Errorf("empty vault should have 0 decayed, got %d", report.DecayedEngrams)
	}
}
