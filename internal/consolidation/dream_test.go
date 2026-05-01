package consolidation

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/storage"
)

func TestDreamOnce_DryRun_NoMutations(t *testing.T) {
	store, db, cleanup := testStoreWithDB(t)
	defer cleanup()

	ctx := context.Background()
	vault := "dream_dry"
	wsPrefix := store.ResolveVaultPrefix(vault)

	embed := []float32{1, 0, 0}
	id := writeEngramWithEmbedding(t, ctx, store, db, wsPrefix, &storage.Engram{
		Concept: "test", Content: "some content", Confidence: 0.8, Relevance: 0.6,
		Stability: 20, Embedding: embed,
	})

	mock := &mockEngineInterface{store: store}
	w := NewWorker(mock)

	report, err := w.DreamOnce(ctx, DreamOpts{DryRun: true, Force: true, Scope: vault})
	if err != nil {
		t.Fatal(err)
	}

	if len(report.Reports) != 1 {
		t.Fatalf("expected 1 vault report, got %d", len(report.Reports))
	}
	if !report.Reports[0].DryRun {
		t.Error("expected DryRun=true in report")
	}

	// Verify engram is untouched.
	eng, err := store.GetEngram(ctx, wsPrefix, id)
	if err != nil {
		t.Fatal(err)
	}
	if eng.State == storage.StateArchived {
		t.Error("engram should not be archived in dry-run mode")
	}
}

func TestDreamOnce_LegalVaultSkipped(t *testing.T) {
	store, db, cleanup := testStoreWithDB(t)
	defer cleanup()

	ctx := context.Background()
	vault := "legal/docs"
	wsPrefix := store.ResolveVaultPrefix(vault)

	embed := []float32{1, 0, 0}
	writeEngramWithEmbedding(t, ctx, store, db, wsPrefix, &storage.Engram{
		Concept: "contract", Content: "confidential agreement", Confidence: 0.9, Relevance: 0.9,
		Stability: 30, Embedding: embed,
	})

	mock := &mockEngineInterface{store: store}
	w := NewWorker(mock)

	report, err := w.DreamOnce(ctx, DreamOpts{Force: true, Scope: vault})
	if err != nil {
		t.Fatal(err)
	}

	if len(report.Skipped) != 1 || report.Skipped[0] != "legal/docs" {
		t.Errorf("expected legal/docs in Skipped, got %v", report.Skipped)
	}

	if len(report.Reports) != 1 {
		t.Fatalf("expected 1 report, got %d", len(report.Reports))
	}
	r := report.Reports[0]
	if r.LegalSkipped == 0 {
		t.Error("expected LegalSkipped > 0")
	}
	if r.MergedEngrams != 0 {
		t.Error("legal vault should have 0 merged engrams")
	}
}

func TestDreamOnce_ScopeFilter(t *testing.T) {
	store, db, cleanup := testStoreWithDB(t)
	defer cleanup()

	ctx := context.Background()

	for _, vault := range []string{"work", "personal"} {
		wsPrefix := store.ResolveVaultPrefix(vault)
		writeEngramWithEmbedding(t, ctx, store, db, wsPrefix, &storage.Engram{
			Concept: "test", Content: "content", Confidence: 0.5, Relevance: 0.5,
			Stability: 20, Embedding: []float32{1, 0, 0},
		})
	}

	mock := &mockEngineInterface{store: store}
	w := NewWorker(mock)

	report, err := w.DreamOnce(ctx, DreamOpts{Force: true, Scope: "work"})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Reports) != 1 {
		t.Fatalf("expected 1 vault report with scope, got %d", len(report.Reports))
	}
	if report.Reports[0].Vault != "work" {
		t.Errorf("expected vault 'work', got %q", report.Reports[0].Vault)
	}
}

func TestDreamOnce_EmptyVault(t *testing.T) {
	store, _, cleanup := testStoreWithDB(t)
	defer cleanup()

	ctx := context.Background()
	mock := &mockEngineInterface{store: store}
	w := NewWorker(mock)

	report, err := w.DreamOnce(ctx, DreamOpts{Force: true, Scope: "empty"})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Reports) != 1 {
		t.Fatalf("expected 1 report, got %d", len(report.Reports))
	}
	if report.Reports[0].Orient == nil {
		t.Error("expected orient summary even for empty vault")
	}
}

// TestDreamOnce_GatesBlock_NoForce verifies that a vault is skipped when both
// the volume gate (< 3 new engrams) and the time gate (last dream was recent)
// block the run, with Force=false.
func TestDreamOnce_GatesBlock_NoForce(t *testing.T) {
	store, db, cleanup := testStoreWithDB(t)
	defer cleanup()

	ctx := context.Background()
	vault := "gates_block"
	wsPrefix := store.ResolveVaultPrefix(vault)

	// Write 1 engram (below volume gate of 3).
	writeEngramWithEmbedding(t, ctx, store, db, wsPrefix, &storage.Engram{
		Concept: "only", Content: "one engram", Confidence: 0.8, Relevance: 0.6,
		Stability: 20, Embedding: []float32{1, 0, 0},
	})

	// Write dream state as very recent (time gate blocks: last dream < 12h ago).
	if err := store.WriteDreamState(wsPrefix, time.Now(), 0); err != nil {
		t.Fatalf("WriteDreamState: %v", err)
	}

	mock := &mockEngineInterface{store: store}
	w := NewWorker(mock)

	report, err := w.DreamOnce(ctx, DreamOpts{Force: false, Scope: vault})
	if err != nil {
		t.Fatal(err)
	}

	if !slices.Contains(report.Skipped, vault) {
		t.Errorf("expected vault %q in Skipped=%v (gates should block)", vault, report.Skipped)
	}
	if len(report.Reports) != 1 {
		t.Errorf("expected 1 report (with orient) when gate blocks, got %d", len(report.Reports))
	}
}

// TestDreamOnce_GatesPass_EnoughTimeAndVolume verifies that a vault proceeds
// when both the time gate (last dream >= 12h ago) and volume gate (>= 3 new
// engrams) are satisfied, with Force=false.
func TestDreamOnce_GatesPass_EnoughTimeAndVolume(t *testing.T) {
	store, db, cleanup := testStoreWithDB(t)
	defer cleanup()

	ctx := context.Background()
	vault := "gates_pass"
	wsPrefix := store.ResolveVaultPrefix(vault)

	// Write 4 engrams (above volume gate of 3).
	for i := 0; i < 4; i++ {
		writeEngramWithEmbedding(t, ctx, store, db, wsPrefix, &storage.Engram{
			Concept: fmt.Sprintf("concept_%d", i), Content: "content", Confidence: 0.8,
			Relevance: 0.6, Stability: 20, Embedding: []float32{1, 0, 0},
		})
	}

	// Write dream state 13 hours ago (time gate passes: >= 12h) with 0 engrams at that time.
	if err := store.WriteDreamState(wsPrefix, time.Now().Add(-13*time.Hour), 0); err != nil {
		t.Fatalf("WriteDreamState: %v", err)
	}

	mock := &mockEngineInterface{store: store}
	w := NewWorker(mock)

	report, err := w.DreamOnce(ctx, DreamOpts{Force: false, Scope: vault})
	if err != nil {
		t.Fatal(err)
	}

	if slices.Contains(report.Skipped, vault) {
		t.Errorf("vault %q should NOT be in Skipped when gates pass, got Skipped=%v", vault, report.Skipped)
	}
	if len(report.Reports) != 1 {
		t.Errorf("expected 1 report when gates pass, got %d", len(report.Reports))
	}
}

func TestDreamOnce_FullPipeline_WithMockLLM(t *testing.T) {
	// Enable all phases — the safe defaults exclude 2b, 4, and 6.
	t.Setenv("MUNINN_DREAM_PHASES", "0,1,2,2b,3,4,5,6")

	store, db, cleanup := testStoreWithDB(t)
	defer cleanup()

	ctx := context.Background()
	vault := "full-pipeline"
	wsPrefix := store.ResolveVaultPrefix(vault)

	// Write near-duplicate engrams (cosine similarity ~0.87, between dream threshold
	// 0.85 and auto-merge threshold 0.95 — routes to LLM review, not auto-merge).
	writeEngramWithEmbedding(t, ctx, store, db, wsPrefix, &storage.Engram{
		Concept: "docker networking", Content: "Docker uses bridge networks by default",
		Confidence: 0.8, Relevance: 0.7, Stability: 20,
		AccessCount: 6, Embedding: []float32{1, 0, 0},
	})
	writeEngramWithEmbedding(t, ctx, store, db, wsPrefix, &storage.Engram{
		Concept: "docker network setup", Content: "Docker containers use bridge networking",
		Confidence: 0.7, Relevance: 0.5, Stability: 18,
		AccessCount: 1, Embedding: []float32{0.87, 0.5, 0},
	})
	// Write a high-signal engram (should be strengthened by Phase 4).
	writeEngramWithEmbedding(t, ctx, store, db, wsPrefix, &storage.Engram{
		Concept: "k8s auth", Content: "Kubernetes RBAC patterns",
		Confidence: 0.9, Relevance: 0.8, Stability: 25,
		AccessCount: 10, Embedding: []float32{0, 1, 0},
	})
	// Write a low-signal old engram (should be weakened by Phase 4).
	writeEngramWithEmbedding(t, ctx, store, db, wsPrefix, &storage.Engram{
		Concept: "old note", Content: "something forgotten",
		Confidence: 0.3, Relevance: 0.1, Stability: 16,
		AccessCount: 0, CreatedAt: time.Now().Add(-60 * 24 * time.Hour),
		Embedding: []float32{0, 0, 1},
	})

	mock := &mockEngineInterface{store: store}
	w := NewWorker(mock)
	// This all-phases smoke test uses a deliberately tiny vault to exercise the
	// LLM review path; lower the Phase 2 size guard for this test only.
	w.MinDedupVaultSize = 1
	w.Providers = []LLMProvider{&mockLLMProvider{
		name:     "mock",
		response: `{"merges":[],"contradictions":[],"cross_vault_suggestions":[],"stability_recommendations":[],"journal":"Reviewed 4 engrams. Docker networking notes are similar but not identical."}`,
	}}

	report, err := w.DreamOnce(ctx, DreamOpts{Force: true, Scope: vault})
	if err != nil {
		t.Fatal(err)
	}

	if len(report.Reports) != 1 {
		t.Fatalf("expected 1 report, got %d", len(report.Reports))
	}
	r := report.Reports[0]

	// Phase 0: orient should have scanned 4 engrams.
	if r.Orient == nil || r.Orient.EngramCount != 4 {
		t.Errorf("expected 4 engrams in orient, got %v", r.Orient)
	}

	// Phase 4: should have strengthened at least the k8s auth engram.
	if r.StabilityStrengthened < 1 {
		t.Error("expected at least 1 strengthened engram")
	}

	// Phase 4: should have weakened the old note.
	if r.StabilityWeakened < 1 {
		t.Error("expected at least 1 weakened engram")
	}

	// Journal should be populated from LLM response.
	if r.Journal == "" {
		t.Error("expected journal from LLM response")
	}

	// DreamReport journal entry should be formatted.
	if report.JournalEntry == "" {
		t.Error("expected JournalEntry to be generated")
	}

	// No fatal errors (non-fatal ones are OK).
	for _, e := range r.Errors {
		t.Logf("non-fatal error: %s", e)
	}
}

func TestParseDreamPhases_Empty(t *testing.T) {
	phases, isDefault := parseDreamPhases("")
	if !isDefault {
		t.Error("expected isDefault=true for empty input")
	}
	if len(phases) != 3 {
		t.Fatalf("expected 3 default phases, got %d", len(phases))
	}
	for _, p := range []string{"0", "2", "5"} {
		if !phases[p] {
			t.Errorf("expected default phase %q to be enabled", p)
		}
	}
}

func TestParseDreamPhases_Explicit(t *testing.T) {
	phases, isDefault := parseDreamPhases("0,2b,5")
	if isDefault {
		t.Error("expected isDefault=false for explicit input")
	}
	if len(phases) != 3 {
		t.Fatalf("expected 3 phases, got %d: %v", len(phases), phases)
	}
	for _, p := range []string{"0", "2b", "5"} {
		if !phases[p] {
			t.Errorf("expected phase %q to be enabled", p)
		}
	}
}

func TestParseDreamPhases_Invalid(t *testing.T) {
	phases, isDefault := parseDreamPhases("0,bogus,5")
	if !isDefault {
		t.Error("expected isDefault=true for invalid input")
	}
	if len(phases) != 3 {
		t.Fatalf("expected 3 default phases (fallback), got %d", len(phases))
	}
}

func TestParseDreamPhases_AllBlanks(t *testing.T) {
	phases, isDefault := parseDreamPhases(", , ,")
	if !isDefault {
		t.Error("expected isDefault=true for all-blanks input")
	}
	if len(phases) != 3 {
		t.Fatalf("expected 3 default phases (fallback), got %d", len(phases))
	}
}

// TestDreamOnce_Phase0Disabled_VolumeGateStillWorks verifies that when phase 0
// is disabled via MUNINN_DREAM_PHASES, the volume gate still correctly counts
// engrams (via the always-run Orient) and does not skip the vault.
func TestDreamOnce_Phase0Disabled_VolumeGateStillWorks(t *testing.T) {
	// Disable phase 0 — only enable phases 2 and 5.
	t.Setenv("MUNINN_DREAM_PHASES", "2,5")

	store, db, cleanup := testStoreWithDB(t)
	defer cleanup()

	ctx := context.Background()
	vault := "no_phase0"
	wsPrefix := store.ResolveVaultPrefix(vault)

	// Write 4 engrams (above volume gate of 3).
	for i := 0; i < 4; i++ {
		writeEngramWithEmbedding(t, ctx, store, db, wsPrefix, &storage.Engram{
			Concept: fmt.Sprintf("concept_%d", i), Content: "content", Confidence: 0.8,
			Relevance: 0.6, Stability: 20, Embedding: []float32{1, 0, 0},
		})
	}

	// Write dream state 13 hours ago with 0 engrams at that time.
	if err := store.WriteDreamState(wsPrefix, time.Now().Add(-13*time.Hour), 0); err != nil {
		t.Fatalf("WriteDreamState: %v", err)
	}

	mock := &mockEngineInterface{store: store}
	w := NewWorker(mock)

	report, err := w.DreamOnce(ctx, DreamOpts{Force: false, Scope: vault})
	if err != nil {
		t.Fatal(err)
	}

	// Vault must NOT be skipped — Orient always runs for gate logic even
	// when phase 0 is disabled.
	if slices.Contains(report.Skipped, vault) {
		t.Errorf("vault %q should NOT be in Skipped when volume gate passes, got Skipped=%v", vault, report.Skipped)
	}
	if len(report.Reports) != 1 {
		t.Fatalf("expected 1 report, got %d", len(report.Reports))
	}

	// Phase 0 is disabled, so report.Orient should be nil (not exposed).
	if report.Reports[0].Orient != nil {
		t.Error("expected Orient=nil when phase 0 is disabled")
	}
}

func TestDreamOnce_PhasesSchemaAndTransitiveRun(t *testing.T) {
	// Enable phases 3 and 5 explicitly — safe defaults exclude phase 3.
	t.Setenv("MUNINN_DREAM_PHASES", "0,2,3,5")

	store, db, cleanup := testStoreWithDB(t)
	defer cleanup()

	ctx := context.Background()
	vault := "phases-all"
	wsPrefix := store.ResolveVaultPrefix(vault)

	for i := 0; i < 3; i++ {
		writeEngramWithEmbedding(t, ctx, store, db, wsPrefix, &storage.Engram{
			Concept: fmt.Sprintf("concept-%d", i), Content: fmt.Sprintf("content-%d", i),
			Confidence: 0.8, Relevance: 0.6, Stability: 20,
			Embedding: []float32{float32(i + 1), 0, 0},
		})
	}

	mock := &mockEngineInterface{store: store}
	w := NewWorker(mock)

	report, err := w.DreamOnce(ctx, DreamOpts{Force: true, Scope: vault})
	if err != nil {
		t.Fatal(err)
	}

	if len(report.Reports) != 1 {
		t.Fatalf("expected 1 report, got %d", len(report.Reports))
	}

	for _, e := range report.Reports[0].Errors {
		if strings.Contains(e, "phase3") || strings.Contains(e, "phase5") {
			t.Errorf("unexpected error from phase 3/5: %s", e)
		}
	}
}
