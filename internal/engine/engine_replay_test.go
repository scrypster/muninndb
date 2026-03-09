package engine

import (
	"context"
	"fmt"
	"testing"

	"github.com/scrypster/muninndb/internal/plugin"
	"github.com/scrypster/muninndb/internal/plugin/enrich"
	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// mockEnrichPlugin implements plugin.EnrichPlugin for testing.
type mockEnrichPlugin struct {
	enrichFn func(ctx context.Context, eng *storage.Engram) (*plugin.EnrichmentResult, error)
	calls    int
}

func (m *mockEnrichPlugin) Name() string  { return "mock-enrich" }
func (m *mockEnrichPlugin) Tier() plugin.PluginTier { return plugin.TierEnrich }
func (m *mockEnrichPlugin) Init(_ context.Context, _ plugin.PluginConfig) error { return nil }
func (m *mockEnrichPlugin) Close() error  { return nil }

func (m *mockEnrichPlugin) Enrich(ctx context.Context, eng *storage.Engram) (*plugin.EnrichmentResult, error) {
	m.calls++
	if m.enrichFn != nil {
		return m.enrichFn(ctx, eng)
	}
	return &plugin.EnrichmentResult{
		Summary:   "mock summary",
		KeyPoints: []string{"point1"},
	}, nil
}

// writeTestEngrams creates n engrams in the given vault.
func writeTestEngrams(t *testing.T, ctx context.Context, eng *Engine, vault string, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		_, err := eng.Write(ctx, &mbp.WriteRequest{
			Vault:   vault,
			Content: fmt.Sprintf("content %d", i),
			Concept: fmt.Sprintf("concept %d", i),
		})
		if err != nil {
			t.Fatalf("Write[%d]: %v", i, err)
		}
	}
}

// assertResultInvariant checks that Processed + Skipped + Failed + Remaining == wantTotal.
func assertResultInvariant(t *testing.T, result *ReplayEnrichmentResult, wantTotal int) {
	t.Helper()
	total := result.Processed + result.Skipped + result.Failed + result.Remaining
	if total != wantTotal {
		t.Errorf("invariant violated: Processed(%d) + Skipped(%d) + Failed(%d) + Remaining(%d) = %d, want %d",
			result.Processed, result.Skipped, result.Failed, result.Remaining, total, wantTotal)
	}
}

// TestReplayEnrichment_DryRunNoModification verifies that dry_run=true returns
// a count of what would be processed without actually writing enrichment data.
func TestReplayEnrichment_DryRunNoModification(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()

	ctx := context.Background()

	// Write two engrams (no enrich plugin set).
	for i := 0; i < 2; i++ {
		_, err := eng.Write(ctx, &mbp.WriteRequest{
			Vault:   "default",
			Content: "content for engram",
			Concept: "test concept",
		})
		if err != nil {
			t.Fatalf("Write: %v", err)
		}
	}

	// Dry run — no enrichPlugin set, should still succeed because we only scan.
	result, err := eng.ReplayEnrichment(ctx, "default", nil, 50, true)
	if err != nil {
		t.Fatalf("ReplayEnrichment(dry_run=true): %v", err)
	}
	if !result.DryRun {
		t.Error("expected DryRun=true in result")
	}
	// Both engrams have no digest flags set, so all should be counted as needing enrichment.
	if result.Processed < 2 {
		t.Errorf("expected at least 2 engrams to need enrichment, got %d", result.Processed)
	}
	// Fresh vault with unenriched engrams: none should be skipped.
	if result.Skipped != 0 {
		t.Errorf("expected Skipped=0 for fresh vault (no prior enrichment), got %d", result.Skipped)
	}
	assertResultInvariant(t, result, 2)
	// Verify no enrichment actually ran (no enrichPlugin was set).
	// Checking the dry_run field is sufficient: engine would error on real run without plugin.
}

// TestReplayEnrichment_SkipsAlreadyEnriched verifies that engrams with all
// requested digest flags already set are skipped (counted in Skipped, not Processed).
func TestReplayEnrichment_SkipsAlreadyEnriched(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()

	ctx := context.Background()

	// Write one engram.
	resp, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:   "default",
		Content: "already enriched content",
		Concept: "fully enriched",
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	// Manually set all per-stage digest flags on the engram.
	id, err := storage.ParseULID(resp.ID)
	if err != nil {
		t.Fatalf("ParseULID: %v", err)
	}
	for _, flag := range []uint8{
		plugin.DigestEntities,
		plugin.DigestRelationships,
		plugin.DigestClassified,
		plugin.DigestSummarized,
	} {
		if err := eng.store.SetDigestFlag(ctx, id, flag); err != nil {
			t.Fatalf("SetDigestFlag(0x%02x): %v", flag, err)
		}
	}

	mock := &mockEnrichPlugin{}
	eng.SetEnrichPlugin(mock)

	result, err := eng.ReplayEnrichment(ctx, "default", nil, 50, false)
	if err != nil {
		t.Fatalf("ReplayEnrichment: %v", err)
	}

	if result.Skipped < 1 {
		t.Errorf("expected at least 1 skipped (fully enriched), got %d", result.Skipped)
	}
	if mock.calls > 0 {
		t.Errorf("expected 0 enrich calls for fully-enriched engram, got %d", mock.calls)
	}
}

// TestReplayEnrichment_NoPipelineReturnsError verifies that if no enrich plugin
// is configured and dry_run=false, an appropriate error is returned.
func TestReplayEnrichment_NoPipelineReturnsError(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()

	ctx := context.Background()

	// Write one engram.
	_, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:   "default",
		Content: "needs enrichment",
		Concept: "no plugin",
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	// No enrichPlugin is set on the engine.
	_, err = eng.ReplayEnrichment(ctx, "default", nil, 50, false)
	if err == nil {
		t.Fatal("expected error when no enrich plugin is configured, got nil")
	}
	if err.Error() != "enrichment pipeline not configured: no enrich plugin available" {
		t.Errorf("unexpected error message: %v", err)
	}
}

// TestReplayEnrichment_DryRunEmptyVault verifies that a vault with no engrams
// returns zero counts without error.
func TestReplayEnrichment_DryRunEmptyVault(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()

	result, err := eng.ReplayEnrichment(context.Background(), "empty-vault", nil, 50, true)
	if err != nil {
		t.Fatalf("ReplayEnrichment on empty vault: %v", err)
	}
	if result.Processed != 0 || result.Skipped != 0 {
		t.Errorf("expected 0/0 for empty vault, got processed=%d skipped=%d",
			result.Processed, result.Skipped)
	}
	assertResultInvariant(t, result, 0)
}

// TestReplayEnrichment_InvalidStageName verifies that unknown stage names
// return an error immediately.
func TestReplayEnrichment_InvalidStageName(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()

	_, err := eng.ReplayEnrichment(context.Background(), "default", []string{"bogus_stage"}, 50, true)
	if err == nil {
		t.Fatal("expected error for unknown stage name, got nil")
	}
}

// TestReplayEnrichment_StagesRunReflectsRequest verifies that StagesRun in the
// result matches the requested stages.
func TestReplayEnrichment_StagesRunReflectsRequest(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()

	requested := []string{"summary", "classification"}
	result, err := eng.ReplayEnrichment(context.Background(), "default", requested, 50, true)
	if err != nil {
		t.Fatalf("ReplayEnrichment: %v", err)
	}

	if len(result.StagesRun) != len(requested) {
		t.Fatalf("StagesRun length: got %d, want %d", len(result.StagesRun), len(requested))
	}
	for i, stage := range requested {
		if result.StagesRun[i] != stage {
			t.Errorf("StagesRun[%d] = %q, want %q", i, result.StagesRun[i], stage)
		}
	}
}

// TestReplayEnrichment_WritesBackToEngram verifies that ReplayEnrichment actually
// persists the Summary and KeyPoints returned by the enrich plugin into each engram.
func TestReplayEnrichment_WritesBackToEngram(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()

	ctx := context.Background()
	const vault = "default"
	ws := eng.store.ResolveVaultPrefix(vault)

	// Write 3 engrams without enrichment.
	concepts := []string{"alpha concept", "beta concept", "gamma concept"}
	ids := make([]storage.ULID, 0, len(concepts))
	for _, concept := range concepts {
		resp, err := eng.Write(ctx, &mbp.WriteRequest{
			Vault:   vault,
			Content: "content for " + concept,
			Concept: concept,
		})
		if err != nil {
			t.Fatalf("Write(%q): %v", concept, err)
		}
		id, err := storage.ParseULID(resp.ID)
		if err != nil {
			t.Fatalf("ParseULID(%q): %v", resp.ID, err)
		}
		ids = append(ids, id)
	}

	// Wire a mock that returns a concept-specific summary.
	mock := &mockEnrichPlugin{
		enrichFn: func(_ context.Context, e *storage.Engram) (*plugin.EnrichmentResult, error) {
			return &plugin.EnrichmentResult{
				Summary:   "mock summary for " + e.Concept,
				KeyPoints: []string{"kp1", "kp2"},
			}, nil
		},
	}
	eng.SetEnrichPlugin(mock)

	// Run replay enrichment (nil stages = all stages, dryRun=false).
	result, err := eng.ReplayEnrichment(ctx, vault, nil, 10, false)
	if err != nil {
		t.Fatalf("ReplayEnrichment: %v", err)
	}

	if result.Processed != 3 {
		t.Errorf("Processed: got %d, want 3", result.Processed)
	}
	if result.Skipped != 0 {
		t.Errorf("Skipped: got %d, want 0", result.Skipped)
	}
	if mock.calls != 3 {
		t.Errorf("mock.calls: got %d, want 3", mock.calls)
	}

	// Read each engram back and verify enrichment was persisted.
	for i, id := range ids {
		got, err := eng.store.GetEngram(ctx, ws, id)
		if err != nil {
			t.Fatalf("GetEngram[%d]: %v", i, err)
		}
		want := "mock summary for " + concepts[i]
		if got.Summary != want {
			t.Errorf("engram[%d] Summary: got %q, want %q", i, got.Summary, want)
		}
		if len(got.KeyPoints) != 2 {
			t.Errorf("engram[%d] KeyPoints length: got %d, want 2", i, len(got.KeyPoints))
		}
	}
}

// TestRetryEnrich_WritesBackToEngram verifies that a single-engram enrichment
// via ReplayEnrichment (limit=1) persists the Summary field into the engram.
func TestRetryEnrich_WritesBackToEngram(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()

	ctx := context.Background()
	const vault = "default"
	ws := eng.store.ResolveVaultPrefix(vault)

	// Write one engram.
	resp, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:   vault,
		Content: "single engram content",
		Concept: "single concept",
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	id, err := storage.ParseULID(resp.ID)
	if err != nil {
		t.Fatalf("ParseULID: %v", err)
	}

	// Wire the mock.
	mock := &mockEnrichPlugin{}
	eng.SetEnrichPlugin(mock)

	// ReplayEnrichment with limit=1 acts as a single-engram retry-enrich.
	_, err = eng.ReplayEnrichment(ctx, vault, nil, 1, false)
	if err != nil {
		t.Fatalf("ReplayEnrichment: %v", err)
	}

	// Read the engram back and verify Summary is populated.
	got, err := eng.store.GetEngram(ctx, ws, id)
	if err != nil {
		t.Fatalf("GetEngram: %v", err)
	}
	if got.Summary == "" {
		t.Error("expected Summary to be populated after enrichment, got empty string")
	}
	if mock.calls == 0 {
		t.Error("expected enrich plugin to be called at least once")
	}
}

// TestReplayEnrichment_FailedCount verifies that when the enrich plugin returns
// an error for some engrams, they are counted as Failed (not silently skipped).
func TestReplayEnrichment_FailedCount(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()

	ctx := context.Background()
	writeTestEngrams(t, ctx, eng, "default", 3)

	// Mock that fails on the 2nd call.
	callCount := 0
	mock := &mockEnrichPlugin{
		enrichFn: func(_ context.Context, _ *storage.Engram) (*plugin.EnrichmentResult, error) {
			callCount++
			if callCount == 2 {
				return nil, fmt.Errorf("simulated enrichment failure")
			}
			return &plugin.EnrichmentResult{
				Summary:   "mock summary",
				KeyPoints: []string{"kp1"},
			}, nil
		},
	}
	eng.SetEnrichPlugin(mock)

	result, err := eng.ReplayEnrichment(ctx, "default", nil, 50, false)
	if err != nil {
		t.Fatalf("ReplayEnrichment: %v", err)
	}

	if result.Processed != 2 {
		t.Errorf("Processed: got %d, want 2", result.Processed)
	}
	if result.Skipped != 0 {
		t.Errorf("Skipped: got %d, want 0", result.Skipped)
	}
	if result.Failed != 1 {
		t.Errorf("Failed: got %d, want 1", result.Failed)
	}
	if result.Remaining != 0 {
		t.Errorf("Remaining: got %d, want 0", result.Remaining)
	}
	assertResultInvariant(t, result, 3)
}

// TestReplayEnrichment_ContextCancellation verifies that when the context is
// cancelled mid-loop, the remaining engrams are counted as Remaining (not processed or failed).
func TestReplayEnrichment_ContextCancellation(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()

	ctx := context.Background()
	writeTestEngrams(t, ctx, eng, "default", 5)

	replayCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Cancel the context after 2 successful enrichments.
	callCount := 0
	mock := &mockEnrichPlugin{
		enrichFn: func(_ context.Context, _ *storage.Engram) (*plugin.EnrichmentResult, error) {
			callCount++
			if callCount == 2 {
				cancel()
			}
			return &plugin.EnrichmentResult{
				Summary:   "mock summary",
				KeyPoints: []string{"kp1"},
			}, nil
		},
	}
	eng.SetEnrichPlugin(mock)

	result, err := eng.ReplayEnrichment(replayCtx, "default", nil, 50, false)
	if err != nil {
		t.Fatalf("ReplayEnrichment should not return an error on context cancellation, got: %v", err)
	}

	if result.Processed != 2 {
		t.Errorf("Processed: got %d, want 2", result.Processed)
	}
	if result.Skipped != 0 {
		t.Errorf("Skipped: got %d, want 0", result.Skipped)
	}
	if result.Failed != 0 {
		t.Errorf("Failed: got %d, want 0", result.Failed)
	}
	if result.Remaining != 3 {
		t.Errorf("Remaining: got %d, want 3", result.Remaining)
	}
	assertResultInvariant(t, result, 5)
}

// TestReplayEnrichment_NothingToEnrich_CountedAsSkipped verifies that when the
// enrich plugin returns ErrNothingToEnrich (wrapped), the engram is counted as
// Skipped (not Failed).
func TestReplayEnrichment_NothingToEnrich_CountedAsSkipped(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()

	ctx := context.Background()
	writeTestEngrams(t, ctx, eng, "default", 3)

	// Mock: engrams 1 and 3 succeed, engram 2 returns a wrapped ErrNothingToEnrich.
	callCount := 0
	mock := &mockEnrichPlugin{
		enrichFn: func(_ context.Context, _ *storage.Engram) (*plugin.EnrichmentResult, error) {
			callCount++
			if callCount == 2 {
				return nil, fmt.Errorf("inline data covers all stages: %w", enrich.ErrNothingToEnrich)
			}
			return &plugin.EnrichmentResult{
				Summary:   "mock summary",
				KeyPoints: []string{"kp1"},
			}, nil
		},
	}
	eng.SetEnrichPlugin(mock)

	result, err := eng.ReplayEnrichment(ctx, "default", nil, 50, false)
	if err != nil {
		t.Fatalf("ReplayEnrichment: %v", err)
	}

	if result.Processed != 2 {
		t.Errorf("Processed: got %d, want 2", result.Processed)
	}
	if result.Skipped != 1 {
		t.Errorf("Skipped: got %d, want 1", result.Skipped)
	}
	if result.Failed != 0 {
		t.Errorf("Failed: got %d, want 0", result.Failed)
	}
	if result.Remaining != 0 {
		t.Errorf("Remaining: got %d, want 0", result.Remaining)
	}
	assertResultInvariant(t, result, 3)
}

// TestReplayEnrichment_ContextAlreadyExpired verifies that when the context is
// already cancelled before the loop starts, all engrams are counted as Remaining.
func TestReplayEnrichment_ContextAlreadyExpired(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()

	ctx := context.Background()
	writeTestEngrams(t, ctx, eng, "default", 3)

	expiredCtx, cancel := context.WithCancel(ctx)
	cancel() // Cancel immediately so the context is already expired.

	mock := &mockEnrichPlugin{}
	eng.SetEnrichPlugin(mock)

	result, err := eng.ReplayEnrichment(expiredCtx, "default", nil, 50, false)
	if err != nil {
		t.Fatalf("ReplayEnrichment should not return an error on expired context, got: %v", err)
	}

	if result.Processed != 0 {
		t.Errorf("Processed: got %d, want 0", result.Processed)
	}
	if result.Skipped != 0 {
		t.Errorf("Skipped: got %d, want 0", result.Skipped)
	}
	if result.Failed != 0 {
		t.Errorf("Failed: got %d, want 0", result.Failed)
	}
	if result.Remaining != 3 {
		t.Errorf("Remaining: got %d, want 3", result.Remaining)
	}
	assertResultInvariant(t, result, 3)
}
