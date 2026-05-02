package engine

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

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

func (m *mockEnrichPlugin) Name() string                                        { return "mock-enrich" }
func (m *mockEnrichPlugin) Tier() plugin.PluginTier                             { return plugin.TierEnrich }
func (m *mockEnrichPlugin) Init(_ context.Context, _ plugin.PluginConfig) error { return nil }
func (m *mockEnrichPlugin) Close() error                                        { return nil }

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

	// Write two engrams (no enrich plugin set; unique content to avoid content-hash dedup).
	for i := 0; i < 2; i++ {
		_, err := eng.Write(ctx, &mbp.WriteRequest{
			Vault:   "default",
			Content: fmt.Sprintf("content for engram %d", i),
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

func TestGetEnrichmentCandidates_ReturnsOnlyMissingStages(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()

	ctx := context.Background()
	const vault = "default"

	needsResp, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:   vault,
		Content: "needs summary",
		Concept: "candidate needs summary",
	})
	if err != nil {
		t.Fatalf("Write(needs): %v", err)
	}
	needsID, err := storage.ParseULID(needsResp.ID)
	if err != nil {
		t.Fatalf("ParseULID(needs): %v", err)
	}
	if err := eng.store.SetDigestFlag(ctx, needsID, plugin.DigestEntities); err != nil {
		t.Fatalf("SetDigestFlag(needs entities): %v", err)
	}

	doneResp, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:   vault,
		Content: "already done",
		Concept: "candidate already enriched",
	})
	if err != nil {
		t.Fatalf("Write(done): %v", err)
	}
	doneID, err := storage.ParseULID(doneResp.ID)
	if err != nil {
		t.Fatalf("ParseULID(done): %v", err)
	}
	for _, flag := range []uint8{
		plugin.DigestEntities,
		plugin.DigestRelationships,
		plugin.DigestClassified,
		plugin.DigestSummarized,
	} {
		if err := eng.store.SetDigestFlag(ctx, doneID, flag); err != nil {
			t.Fatalf("SetDigestFlag(done 0x%02x): %v", flag, err)
		}
	}

	candidates, stages, _, err := eng.GetEnrichmentCandidates(ctx, vault, nil, storage.ULID{}, 10)
	if err != nil {
		t.Fatalf("GetEnrichmentCandidates: %v", err)
	}
	if len(stages) != 4 {
		t.Fatalf("stage count: got %d, want 4", len(stages))
	}
	if len(candidates) != 1 {
		t.Fatalf("candidate count: got %d, want 1", len(candidates))
	}
	if candidates[0].ID != needsID {
		t.Fatalf("candidate id: got %s, want %s", candidates[0].ID, needsID)
	}
	wantMissing := []string{"relationships", "classification", "summary"}
	if fmt.Sprint(candidates[0].MissingStages) != fmt.Sprint(wantMissing) {
		t.Fatalf("missing stages: got %v, want %v", candidates[0].MissingStages, wantMissing)
	}
}

func TestGetEnrichmentCandidates_CursorPaginates(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()

	ctx := context.Background()
	const vault = "default"

	// Write 6 engrams; mark the first 4 fully enriched, leave last 2 unenriched.
	ids := make([]storage.ULID, 6)
	for i := 0; i < 6; i++ {
		resp, err := eng.Write(ctx, &mbp.WriteRequest{
			Vault:   vault,
			Content: fmt.Sprintf("engram %d", i),
			Concept: fmt.Sprintf("concept %d", i),
		})
		if err != nil {
			t.Fatalf("Write %d: %v", i, err)
		}
		id, err := storage.ParseULID(resp.ID)
		if err != nil {
			t.Fatalf("ParseULID %d: %v", i, err)
		}
		ids[i] = id
	}
	allFlags := []uint8{plugin.DigestEntities, plugin.DigestRelationships, plugin.DigestClassified, plugin.DigestSummarized}
	for i := 0; i < 4; i++ {
		for _, flag := range allFlags {
			if err := eng.store.SetDigestFlag(ctx, ids[i], flag); err != nil {
				t.Fatalf("SetDigestFlag(%d, 0x%02x): %v", i, flag, err)
			}
		}
	}

	// Page 1: limit=1 should return the first unenriched engram in state-index order.
	expected := []storage.ULID{ids[4], ids[5]}
	if expected[1].String() < expected[0].String() {
		expected[0], expected[1] = expected[1], expected[0]
	}
	candidates1, _, cursor1, err := eng.GetEnrichmentCandidates(ctx, vault, nil, storage.ULID{}, 1)
	if err != nil {
		t.Fatalf("page1: %v", err)
	}
	if len(candidates1) != 1 {
		t.Fatalf("page1 count: got %d, want 1", len(candidates1))
	}
	if candidates1[0].ID != expected[0] {
		t.Errorf("page1 candidate: got %s, want %s", candidates1[0].ID, expected[0])
	}
	if cursor1 == (storage.ULID{}) {
		t.Error("page1: expected non-zero cursor")
	}

	// Page 2: continue from cursor1, should return the second unenriched engram.
	candidates2, _, cursor2, err := eng.GetEnrichmentCandidates(ctx, vault, nil, cursor1, 1)
	if err != nil {
		t.Fatalf("page2: %v", err)
	}
	if len(candidates2) != 1 {
		t.Fatalf("page2 count: got %d, want 1", len(candidates2))
	}
	if candidates2[0].ID != expected[1] {
		t.Errorf("page2 candidate: got %s, want %s", candidates2[0].ID, expected[1])
	}

	// Page 3: exhausted — cursor2 should be zero.
	candidates3, _, cursor3, err := eng.GetEnrichmentCandidates(ctx, vault, nil, cursor2, 1)
	if err != nil {
		t.Fatalf("page3: %v", err)
	}
	if len(candidates3) != 0 {
		t.Fatalf("page3 count: got %d, want 0", len(candidates3))
	}
	if cursor3 != (storage.ULID{}) {
		t.Errorf("page3: expected zero cursor (exhausted), got %s", cursor3)
	}
}

func TestGetEnrichmentCandidates_SkipsEnrichedAtStart(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()

	ctx := context.Background()
	const vault = "default"

	// Write 3 engrams. First 2 fully enriched, last one not.
	ids := make([]storage.ULID, 3)
	for i := 0; i < 3; i++ {
		resp, err := eng.Write(ctx, &mbp.WriteRequest{
			Vault:   vault,
			Content: fmt.Sprintf("content %d", i),
			Concept: fmt.Sprintf("concept %d", i),
		})
		if err != nil {
			t.Fatalf("Write %d: %v", i, err)
		}
		id, err := storage.ParseULID(resp.ID)
		if err != nil {
			t.Fatalf("ParseULID %d: %v", i, err)
		}
		ids[i] = id
	}
	allFlags := []uint8{plugin.DigestEntities, plugin.DigestRelationships, plugin.DigestClassified, plugin.DigestSummarized}
	for _, flag := range allFlags {
		if err := eng.store.SetDigestFlag(ctx, ids[0], flag); err != nil {
			t.Fatalf("SetDigestFlag(0): %v", err)
		}
		if err := eng.store.SetDigestFlag(ctx, ids[1], flag); err != nil {
			t.Fatalf("SetDigestFlag(1): %v", err)
		}
	}

	// Without cursor fix, a limit=2 call would scan only ids[0] and ids[1],
	// both enriched, and return 0 candidates. With the fix it continues.
	candidates, _, cursor, err := eng.GetEnrichmentCandidates(ctx, vault, nil, storage.ULID{}, 2)
	if err != nil {
		t.Fatalf("GetEnrichmentCandidates: %v", err)
	}
	if len(candidates) != 1 {
		t.Fatalf("candidates: got %d, want 1", len(candidates))
	}
	if candidates[0].ID != ids[2] {
		t.Errorf("candidate id: got %s, want %s", candidates[0].ID, ids[2])
	}
	// ids[2] was the last engram — cursor should be zero (exhausted).
	if cursor != (storage.ULID{}) {
		t.Errorf("cursor should be zero (exhausted), got %s", cursor)
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

func TestApplyEnrichment_PersistsExplicitOutput(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()

	ctx := context.Background()
	const vault = "default"
	ws := eng.store.ResolveVaultPrefix(vault)

	resp, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:   vault,
		Content: "postgres is the primary database",
		Concept: "database note",
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	id, err := storage.ParseULID(resp.ID)
	if err != nil {
		t.Fatalf("ParseULID: %v", err)
	}
	original, err := eng.GetEngram(ctx, vault, id)
	if err != nil {
		t.Fatalf("GetEngram(before): %v", err)
	}

	result, err := eng.ApplyEnrichment(ctx, vault, &EnrichmentApplyRequest{
		ID:                resp.ID,
		ExpectedUpdatedAt: original.UpdatedAt,
		Summary:           "PostgreSQL is the primary system of record.",
		MemoryType:        "reference",
		TypeLabel:         "database_note",
		Entities: []EnrichmentApplyEntity{
			{Name: "PostgreSQL", Type: "database", Confidence: 0.95},
			{Name: "System of Record", Type: "concept", Confidence: 0.7},
		},
		Relationships: []EnrichmentApplyRelationship{
			{FromEntity: "PostgreSQL", ToEntity: "System of Record", RelType: "is_a", Weight: 0.8},
		},
		StagesCompleted: []string{"summary", "classification", "entities", "relationships"},
		Source:          "test-agent",
	})
	if err != nil {
		t.Fatalf("ApplyEnrichment: %v", err)
	}
	if len(result.AppliedStages) != 4 {
		t.Fatalf("AppliedStages len: got %d, want 4", len(result.AppliedStages))
	}

	updated, err := eng.store.GetEngram(ctx, ws, id)
	if err != nil {
		t.Fatalf("GetEngram(after): %v", err)
	}
	if updated.Summary != "PostgreSQL is the primary system of record." {
		t.Fatalf("Summary: got %q", updated.Summary)
	}
	if updated.TypeLabel != "database_note" {
		t.Fatalf("TypeLabel: got %q, want %q", updated.TypeLabel, "database_note")
	}
	if updated.MemoryType.String() != "reference" {
		t.Fatalf("MemoryType: got %q, want %q", updated.MemoryType.String(), "reference")
	}

	flags, err := eng.store.GetDigestFlags(ctx, id)
	if err != nil {
		t.Fatalf("GetDigestFlags: %v", err)
	}
	wantMask := plugin.DigestEntities | plugin.DigestRelationships | plugin.DigestClassified | plugin.DigestSummarized
	if flags&wantMask != wantMask {
		t.Fatalf("digest flags: got 0x%02x, want mask 0x%02x", flags, wantMask)
	}

	entityRecord, err := eng.store.GetEntityRecord(ctx, "PostgreSQL")
	if err != nil {
		t.Fatalf("GetEntityRecord: %v", err)
	}
	if entityRecord == nil || entityRecord.Source != "test-agent" {
		t.Fatalf("entity record: got %+v", entityRecord)
	}

	var relationships []storage.RelationshipRecord
	if err := eng.store.ScanEngramRelationships(ctx, ws, id, func(record storage.RelationshipRecord) error {
		relationships = append(relationships, record)
		return nil
	}); err != nil {
		t.Fatalf("ScanEngramRelationships: %v", err)
	}
	if len(relationships) != 1 {
		t.Fatalf("relationship count: got %d, want 1", len(relationships))
	}
	if relationships[0].Source != "test-agent" {
		t.Fatalf("relationship source: got %q, want %q", relationships[0].Source, "test-agent")
	}
}

func TestApplyEnrichment_ConflictOnUpdatedAtMismatch(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()

	ctx := context.Background()
	const vault = "default"

	resp, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:   vault,
		Content: "conflict target",
		Concept: "conflict target",
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	id, err := storage.ParseULID(resp.ID)
	if err != nil {
		t.Fatalf("ParseULID: %v", err)
	}
	current, err := eng.GetEngram(ctx, vault, id)
	if err != nil {
		t.Fatalf("GetEngram: %v", err)
	}
	_, err = eng.ApplyEnrichment(ctx, vault, &EnrichmentApplyRequest{
		ID:                resp.ID,
		ExpectedUpdatedAt: current.UpdatedAt.Add(-time.Second),
		Summary:           "stale summary",
	})
	if !errors.Is(err, ErrEnrichmentConflict) {
		t.Fatalf("ApplyEnrichment error: got %v, want ErrEnrichmentConflict (current=%s)", err, current.UpdatedAt.UTC().Format(time.RFC3339Nano))
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

// TestGetEnrichmentCandidates_AdaptiveBatch verifies that GetEnrichmentCandidates
// finds unenriched engrams that appear after a run of fully-enriched ones,
// exercising the "skip enriched, find unenriched at end" behavior of the
// adaptive batch loop.
func TestGetEnrichmentCandidates_AdaptiveBatch(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()

	ctx := context.Background()
	const vault = "default"

	// Write 25 fully enriched + 1 unenriched at the end.
	enrichedCount := 25
	ids := make([]storage.ULID, enrichedCount+1)
	for i := 0; i <= enrichedCount; i++ {
		resp, err := eng.Write(ctx, &mbp.WriteRequest{
			Vault:   vault,
			Content: fmt.Sprintf("content %d", i),
			Concept: fmt.Sprintf("concept %d", i),
		})
		if err != nil {
			t.Fatalf("Write %d: %v", i, err)
		}
		id, err := storage.ParseULID(resp.ID)
		if err != nil {
			t.Fatalf("ParseULID %d: %v", i, err)
		}
		ids[i] = id
	}
	allFlags := []uint8{plugin.DigestEntities, plugin.DigestRelationships, plugin.DigestClassified, plugin.DigestSummarized}
	for i := 0; i < enrichedCount; i++ {
		for _, flag := range allFlags {
			if err := eng.store.SetDigestFlag(ctx, ids[i], flag); err != nil {
				t.Fatalf("SetDigestFlag(%d): %v", i, err)
			}
		}
	}

	// The unenriched engram is ids[enrichedCount].
	// With limit=5, batchSize starts at max(50, 20)=50, which covers all 26 engrams.
	// All 25 enriched ones are skipped, and the 1 unenriched is found.
	candidates, _, _, err := eng.GetEnrichmentCandidates(ctx, vault, nil, storage.ULID{}, 5)
	if err != nil {
		t.Fatalf("GetEnrichmentCandidates: %v", err)
	}
	if len(candidates) != 1 {
		t.Fatalf("candidates count: got %d, want 1", len(candidates))
	}
	if candidates[0].ID != ids[enrichedCount] {
		t.Errorf("candidate: got %s, want %s", candidates[0].ID, ids[enrichedCount])
	}
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

// TestReplayEnrichment_SkipsAfterMaxFailures verifies that an engram which has
// failed maxReplayFails consecutive times is silently skipped on the next call.
func TestReplayEnrichment_SkipsAfterMaxFailures(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	writeTestEngrams(t, ctx, eng, "default", 1)

	errBoom := errors.New("LLM boom")
	calls := 0
	mock := &mockEnrichPlugin{
		enrichFn: func(_ context.Context, _ *storage.Engram) (*plugin.EnrichmentResult, error) {
			calls++
			return nil, errBoom
		},
	}
	eng.SetEnrichPlugin(mock)

	// First maxReplayFails calls should each attempt enrichment and record a failure.
	for i := 0; i < maxReplayFails; i++ {
		result, err := eng.ReplayEnrichment(ctx, "default", nil, 50, false)
		if err != nil {
			t.Fatalf("call %d: unexpected error: %v", i+1, err)
		}
		if result.Failed != 1 {
			t.Errorf("call %d: want Failed=1, got %d", i+1, result.Failed)
		}
	}
	if calls != maxReplayFails {
		t.Errorf("want %d Enrich calls, got %d", maxReplayFails, calls)
	}

	// Next call: engram has hit the threshold — must be skipped, not attempted again.
	callsBefore := calls
	result, err := eng.ReplayEnrichment(ctx, "default", nil, 50, false)
	if err != nil {
		t.Fatalf("post-threshold call: unexpected error: %v", err)
	}
	if calls != callsBefore {
		t.Errorf("Enrich should not be called after threshold; got %d extra calls", calls-callsBefore)
	}
	if result.Skipped != 1 {
		t.Errorf("post-threshold: want Skipped=1, got %d", result.Skipped)
	}
	if result.Failed != 0 {
		t.Errorf("post-threshold: want Failed=0, got %d", result.Failed)
	}
}

// TestReplayEnrichment_FailCountResetOnSuccess verifies that a successful enrichment
// run clears the failure counter so the engram is attempted again next time.
func TestReplayEnrichment_FailCountResetOnSuccess(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	writeTestEngrams(t, ctx, eng, "default", 1)

	shouldFail := true
	mock := &mockEnrichPlugin{
		enrichFn: func(_ context.Context, _ *storage.Engram) (*plugin.EnrichmentResult, error) {
			if shouldFail {
				return nil, errors.New("temporary failure")
			}
			return &plugin.EnrichmentResult{Summary: "ok", KeyPoints: []string{"k"}}, nil
		},
	}
	eng.SetEnrichPlugin(mock)

	// Fail once to set fail count to 1.
	result, err := eng.ReplayEnrichment(ctx, "default", nil, 50, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Failed != 1 {
		t.Errorf("want Failed=1, got %d", result.Failed)
	}

	// Now succeed — should clear the failure counter.
	shouldFail = false
	result, err = eng.ReplayEnrichment(ctx, "default", nil, 50, false)
	if err != nil {
		t.Fatalf("unexpected error after success: %v", err)
	}
	if result.Processed != 1 {
		t.Errorf("want Processed=1, got %d", result.Processed)
	}

	// The engram now has all digest flags set, so further calls skip it as done.
	_ = result
}

// TestReplayEnrichment_PerEngramTimeout verifies that SetReplayEnrichTimeout
// causes Enrich to receive a context that has a deadline.
func TestReplayEnrichment_PerEngramTimeout(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	writeTestEngrams(t, ctx, eng, "default", 1)

	var deadlineSet bool
	mock := &mockEnrichPlugin{
		enrichFn: func(enrichCtx context.Context, _ *storage.Engram) (*plugin.EnrichmentResult, error) {
			dl, ok := enrichCtx.Deadline()
			deadlineSet = ok && !dl.IsZero()
			return &plugin.EnrichmentResult{Summary: "ok", KeyPoints: []string{"k"}}, nil
		},
	}
	eng.SetEnrichPlugin(mock)
	eng.SetReplayEnrichTimeout(30 * time.Second)

	result, err := eng.ReplayEnrichment(ctx, "default", nil, 50, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Processed != 1 {
		t.Errorf("want Processed=1, got %d", result.Processed)
	}
	if !deadlineSet {
		t.Error("expected per-engram context to have a deadline when SetReplayEnrichTimeout > 0")
	}
}

// TestReplayEnrichment_TimeoutFires verifies that when the per-engram timeout
// expires before Enrich returns, the error is counted as a failure (not silently
// swallowed) and the context deadline exceeded error propagates correctly.
func TestReplayEnrichment_TimeoutFires(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	writeTestEngrams(t, ctx, eng, "default", 1)

	mock := &mockEnrichPlugin{
		enrichFn: func(enrichCtx context.Context, _ *storage.Engram) (*plugin.EnrichmentResult, error) {
			// Block until the per-engram context is cancelled.
			<-enrichCtx.Done()
			return nil, enrichCtx.Err()
		},
	}
	eng.SetEnrichPlugin(mock)
	eng.SetReplayEnrichTimeout(10 * time.Millisecond) // fire immediately

	result, err := eng.ReplayEnrichment(ctx, "default", nil, 50, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Failed != 1 {
		t.Errorf("want Failed=1 after timeout, got %d", result.Failed)
	}
	if result.Processed != 0 {
		t.Errorf("want Processed=0 after timeout, got %d", result.Processed)
	}
}

// TestReplayEnrichment_ResetFailCount verifies that ResetReplayFailCount clears
// the failure counter, allowing a previously-skipped engram to be retried.
func TestReplayEnrichment_ResetFailCount(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	writeTestEngrams(t, ctx, eng, "default", 1)

	// Fail enough times to trip the circuit breaker.
	calls := 0
	mock := &mockEnrichPlugin{
		enrichFn: func(_ context.Context, _ *storage.Engram) (*plugin.EnrichmentResult, error) {
			calls++
			return nil, errors.New("permanent failure")
		},
	}
	eng.SetEnrichPlugin(mock)

	for i := 0; i < maxReplayFails; i++ {
		eng.ReplayEnrichment(ctx, "default", nil, 50, false) //nolint:errcheck
	}

	// Confirm the engram is now skipped.
	callsBefore := calls
	result, _ := eng.ReplayEnrichment(ctx, "default", nil, 50, false)
	if calls != callsBefore {
		t.Fatalf("engram should be skipped after threshold, got extra call")
	}
	if result.Skipped != 1 {
		t.Errorf("want Skipped=1 before reset, got %d", result.Skipped)
	}

	// Reset the counter — the engram must be attempted again.
	ids, err := eng.store.ListByState(ctx, eng.store.ResolveVaultPrefix("default"), storage.StateActive, 10)
	if err != nil || len(ids) == 0 {
		t.Fatalf("could not list engrams: %v", err)
	}
	eng.ResetReplayFailCount(ids[0])

	callsBefore = calls
	result, _ = eng.ReplayEnrichment(ctx, "default", nil, 50, false)
	if calls == callsBefore {
		t.Error("expected Enrich to be called again after ResetReplayFailCount")
	}
	if result.Failed != 1 {
		t.Errorf("want Failed=1 after reset (still failing), got %d", result.Failed)
	}
}
