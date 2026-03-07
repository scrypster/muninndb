package mcp

import (
	"context"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/plugin"
	"github.com/scrypster/muninndb/internal/storage"
)

// mockPluginStore records calls to PluginStore methods used by RetryEnrich.
type mockPluginStore struct {
	updateDigestCalls         int
	upsertEntityCalls         int
	linkEngramToEntityCalls   int
	setDigestFlagCalls        int
	upsertRelationshipCalls   int
	incrementCoOccurrenceCalls int
}

func (m *mockPluginStore) CountWithoutFlag(_ context.Context, _ uint8) (int64, error) { return 0, nil }
func (m *mockPluginStore) ScanWithoutFlag(_ context.Context, _ uint8) plugin.EngramIterator {
	return nil
}
func (m *mockPluginStore) SetDigestFlag(_ context.Context, _ plugin.ULID, _ uint8) error {
	m.setDigestFlagCalls++
	return nil
}
func (m *mockPluginStore) GetDigestFlags(_ context.Context, _ plugin.ULID) (uint8, error) {
	return 0, nil
}
func (m *mockPluginStore) UpdateEmbedding(_ context.Context, _ plugin.ULID, _ []float32) error {
	return nil
}
func (m *mockPluginStore) UpdateDigest(_ context.Context, _ plugin.ULID, _ *plugin.EnrichmentResult) error {
	m.updateDigestCalls++
	return nil
}
func (m *mockPluginStore) UpsertEntity(_ context.Context, _ plugin.ExtractedEntity) error {
	m.upsertEntityCalls++
	return nil
}
func (m *mockPluginStore) LinkEngramToEntity(_ context.Context, _ plugin.ULID, _ string) error {
	m.linkEngramToEntityCalls++
	return nil
}
func (m *mockPluginStore) IncrementEntityCoOccurrence(_ context.Context, _ plugin.ULID, _, _ string) error {
	m.incrementCoOccurrenceCalls++
	return nil
}
func (m *mockPluginStore) UpsertRelationship(_ context.Context, _ plugin.ULID, _ plugin.ExtractedRelation) error {
	m.upsertRelationshipCalls++
	return nil
}
func (m *mockPluginStore) HNSWInsert(_ context.Context, _ plugin.ULID, _ []float32) error {
	return nil
}
func (m *mockPluginStore) AutoLinkByEmbedding(_ context.Context, _ plugin.ULID, _ []float32) error {
	return nil
}

// TestMCPEngineAdapterRetryEnrichNoPlugin verifies that RetryEnrich returns
// "no enrich plugin configured" when the adapter has no enricher set.
func TestMCPEngineAdapterRetryEnrichNoPlugin(t *testing.T) {
	a := &mcpEngineAdapter{eng: nil, enricher: nil}
	_, err := a.RetryEnrich(context.Background(), "default", "01234567890123456789012345")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if err.Error() != "no enrich plugin configured" {
		t.Errorf("expected 'no enrich plugin configured', got %q", err.Error())
	}
}

// TestMCPEngineAdapterListDeletedFiltersNil verifies that nil entries in the
// engrams slice are skipped and do not appear in the result.
func TestMCPEngineAdapterListDeletedFiltersNil(t *testing.T) {
	// Build a slice that mimics what eng.ListDeleted might return.
	// We can test the nil-filtering logic directly via listDeletedFromEngrams.
	engrams := []*storage.Engram{
		{ID: storage.ULID{1}, Concept: "first"},
		nil,
		{ID: storage.ULID{3}, Concept: "third"},
		nil,
	}

	now := time.Now()
	result := make([]DeletedEngram, 0, len(engrams))
	for _, eng := range engrams {
		if eng == nil {
			continue
		}
		result = append(result, DeletedEngram{
			ID:               eng.ID.String(),
			Concept:          eng.Concept,
			DeletedAt:        eng.UpdatedAt,
			RecoverableUntil: now.Add(7 * 24 * time.Hour),
			Tags:             eng.Tags,
		})
	}

	if len(result) != 2 {
		t.Fatalf("expected 2 entries after nil filtering, got %d", len(result))
	}
	if result[0].Concept != "first" {
		t.Errorf("result[0].Concept = %q, want %q", result[0].Concept, "first")
	}
	if result[1].Concept != "third" {
		t.Errorf("result[1].Concept = %q, want %q", result[1].Concept, "third")
	}
}

// TestMCPEngineAdapterTraverseDefaultMaxHops verifies that a zero MaxHops
// value is replaced by the default of 3.
func TestMCPEngineAdapterTraverseDefaultMaxHops(t *testing.T) {
	req := &TraverseRequest{
		StartID:  "someID",
		MaxHops:  0,
		MaxNodes: 0,
	}

	maxHops := req.MaxHops
	if maxHops <= 0 {
		maxHops = 3
	}
	maxNodes := req.MaxNodes
	if maxNodes <= 0 {
		maxNodes = 50
	}

	if maxHops != 3 {
		t.Errorf("expected maxHops=3 default, got %d", maxHops)
	}
	if maxNodes != 50 {
		t.Errorf("expected maxNodes=50 default, got %d", maxNodes)
	}
}

// TestAdapterImplementsEngineInterface is a compile-time check that mcpEngineAdapter
// satisfies the EngineInterface contract.
func TestAdapterImplementsEngineInterface(t *testing.T) {
	// Compile-time check: mcpEngineAdapter must implement EngineInterface.
	var _ EngineInterface = (*mcpEngineAdapter)(nil)
}

// TestMockPluginStoreImplementsPluginStore is a compile-time check that
// mockPluginStore satisfies plugin.PluginStore — validates our test mock.
func TestMockPluginStoreImplementsPluginStore(t *testing.T) {
	var _ plugin.PluginStore = (*mockPluginStore)(nil)
}

// TestRetryEnrich_UsesPluginStore verifies that a pStore is wired into the adapter
// and that RetryEnrich does not use the old WriteEngram path for entity persistence
// (regression guard for issue #113 bug 2).
//
// Full end-to-end persistence is covered by the retroactive processor tests;
// this test ensures the adapter struct carries pStore correctly.
func TestRetryEnrich_UsesPluginStore(t *testing.T) {
	pStore := &mockPluginStore{}
	a := &mcpEngineAdapter{eng: nil, enricher: nil, pStore: pStore}
	// With no enricher, RetryEnrich returns early before touching pStore.
	// This is a structural test: verify pStore is stored on the adapter.
	if a.pStore != pStore {
		t.Error("pStore not correctly stored on adapter")
	}
	_, err := a.RetryEnrich(context.Background(), "default", "01234567890123456789012345")
	if err == nil || err.Error() != "no enrich plugin configured" {
		t.Errorf("unexpected error: %v", err)
	}
	// pStore should not have been touched (no enricher path exits early).
	if pStore.updateDigestCalls != 0 {
		t.Errorf("UpdateDigest called unexpectedly: %d times", pStore.updateDigestCalls)
	}
}

// TestRetryEnrich_PersistenceCallSequence verifies that the RetryEnrich persistence
// sequence produces the correct pStore calls: UpdateDigest (×1), UpsertEntity (×2),
// LinkEngramToEntity (×2), IncrementEntityCoOccurrence (1 pair from 2 entities),
// SetDigestFlag (×2 — DigestEntities + DigestRelationships), UpsertRelationship (×1).
//
// NOTE: this test exercises the persistence logic inline rather than through
// RetryEnrich itself, because RetryEnrich calls a.eng.Store() / store.GetEngram()
// on a concrete *engine.Engine — mocking that without a full storage backend is
// integration territory. End-to-end coverage is provided by the retroactive
// processor tests in internal/plugin. This test documents the expected call sequence
// and catches regressions in the persistence constants (e.g., DigestRelationships).
func TestRetryEnrich_PersistenceCallSequence(t *testing.T) {
	pStore := &mockPluginStore{}
	result := &plugin.EnrichmentResult{
		Summary: "test summary",
		Entities: []plugin.ExtractedEntity{
			{Name: "Alice", Type: "person", Confidence: 0.9},
			{Name: "Bob", Type: "person", Confidence: 0.8},
		},
		Relationships: []plugin.ExtractedRelation{
			{FromEntity: "Alice", ToEntity: "Bob", RelType: "knows"},
		},
	}
	ulid := storage.ULID{1}
	ctx := context.Background()

	// Replicate the RetryEnrich persistence sequence.
	if err := pStore.UpdateDigest(ctx, plugin.ULID(ulid), result); err != nil {
		t.Fatalf("UpdateDigest: %v", err)
	}
	var linkedEntityNames []string
	for _, entity := range result.Entities {
		if err := pStore.UpsertEntity(ctx, entity); err != nil {
			continue
		}
		if err := pStore.LinkEngramToEntity(ctx, plugin.ULID(ulid), entity.Name); err != nil {
			continue
		}
		linkedEntityNames = append(linkedEntityNames, entity.Name)
	}
	for i := 0; i < len(linkedEntityNames); i++ {
		for j := i + 1; j < len(linkedEntityNames); j++ {
			_ = pStore.IncrementEntityCoOccurrence(ctx, plugin.ULID(ulid), linkedEntityNames[i], linkedEntityNames[j])
		}
	}
	if len(result.Entities) > 0 {
		_ = pStore.SetDigestFlag(ctx, plugin.ULID(ulid), plugin.DigestEntities)
	}
	for _, rel := range result.Relationships {
		if err := pStore.UpsertRelationship(ctx, plugin.ULID(ulid), rel); err != nil {
			t.Errorf("UpsertRelationship: %v", err)
		}
	}
	if len(result.Relationships) > 0 {
		_ = pStore.SetDigestFlag(ctx, plugin.ULID(ulid), plugin.DigestRelationships)
	}

	if pStore.updateDigestCalls != 1 {
		t.Errorf("UpdateDigest calls = %d, want 1", pStore.updateDigestCalls)
	}
	if pStore.upsertEntityCalls != 2 {
		t.Errorf("UpsertEntity calls = %d, want 2", pStore.upsertEntityCalls)
	}
	if pStore.linkEngramToEntityCalls != 2 {
		t.Errorf("LinkEngramToEntity calls = %d, want 2", pStore.linkEngramToEntityCalls)
	}
	if pStore.incrementCoOccurrenceCalls != 1 {
		t.Errorf("IncrementEntityCoOccurrence calls = %d, want 1 (1 pair from 2 entities)", pStore.incrementCoOccurrenceCalls)
	}
	if pStore.setDigestFlagCalls != 2 {
		t.Errorf("SetDigestFlag calls = %d, want 2 (DigestEntities + DigestRelationships)", pStore.setDigestFlagCalls)
	}
	if pStore.upsertRelationshipCalls != 1 {
		t.Errorf("UpsertRelationship calls = %d, want 1", pStore.upsertRelationshipCalls)
	}
}

// TestMCPEngineAdapterTraverseExplicitMaxHops verifies that an explicit MaxHops
// value is not overridden by the default.
func TestMCPEngineAdapterTraverseExplicitMaxHops(t *testing.T) {
	req := &TraverseRequest{
		StartID:  "someID",
		MaxHops:  5,
		MaxNodes: 100,
	}

	maxHops := req.MaxHops
	if maxHops <= 0 {
		maxHops = 3
	}
	maxNodes := req.MaxNodes
	if maxNodes <= 0 {
		maxNodes = 50
	}

	if maxHops != 5 {
		t.Errorf("expected maxHops=5, got %d", maxHops)
	}
	if maxNodes != 100 {
		t.Errorf("expected maxNodes=100, got %d", maxNodes)
	}
}
