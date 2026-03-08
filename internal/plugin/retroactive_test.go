package plugin

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cockroachdb/pebble"
)

// ---------------------------------------------------------------------------
// Enrichment-capable mock for processEngram tests
// ---------------------------------------------------------------------------

type enrichMockForRetro struct {
	mockPlugin
	enrichResult *EnrichmentResult
	enrichErr    error
	callCount    int
}

func (m *enrichMockForRetro) Enrich(_ context.Context, _ *Engram) (*EnrichmentResult, error) {
	m.callCount++
	if m.enrichErr != nil {
		return nil, m.enrichErr
	}
	return m.enrichResult, nil
}

// ---------------------------------------------------------------------------
// Constructor & field tests
// ---------------------------------------------------------------------------

func TestNewRetroactiveProcessor(t *testing.T) {
	store := &mockPluginStore{}
	p := &mockEmbedPlugin{mockPlugin: mockPlugin{name: "rp-embed", tier: TierEmbed}}

	rp := NewRetroactiveProcessor(store, p, DigestEmbed)
	if rp == nil {
		t.Fatal("NewRetroactiveProcessor returned nil")
	}
	if rp.store != store {
		t.Error("store not set")
	}
	if rp.plugin != p {
		t.Error("plugin not set")
	}
	if rp.flagBit != DigestEmbed {
		t.Errorf("flagBit = %d, want %d", rp.flagBit, DigestEmbed)
	}
	if rp.notifyCh == nil {
		t.Error("notifyCh not initialized")
	}
}

func TestRetroactiveProcessor_Notify(t *testing.T) {
	rp := NewRetroactiveProcessor(&mockPluginStore{}, &mockEmbedPlugin{
		mockPlugin: mockPlugin{name: "n", tier: TierEmbed},
	}, DigestEmbed)

	// First notify should succeed (buffered channel)
	rp.Notify()
	select {
	case <-rp.notifyCh:
	default:
		t.Error("expected signal in notifyCh")
	}

	// Double-notify should not block (drops second signal)
	rp.Notify()
	rp.Notify()
	select {
	case <-rp.notifyCh:
	default:
		t.Error("expected at least one signal")
	}
}

func TestRetroactiveProcessor_Stats(t *testing.T) {
	rp := NewRetroactiveProcessor(&mockPluginStore{}, &mockEmbedPlugin{
		mockPlugin: mockPlugin{name: "s", tier: TierEmbed},
	}, DigestEmbed)

	stats := rp.Stats()
	if stats.PluginName != "" {
		t.Errorf("initial PluginName should be empty, got %q", stats.PluginName)
	}
	if stats.Status != "" {
		t.Errorf("initial Status should be empty, got %q", stats.Status)
	}
	if stats.Processed != 0 {
		t.Errorf("initial Processed should be 0, got %d", stats.Processed)
	}
}

// ---------------------------------------------------------------------------
// Start / Stop lifecycle
// ---------------------------------------------------------------------------

func TestRetroactiveProcessor_StartStop(t *testing.T) {
	store := &mockPluginStore{countResult: 0}
	p := &mockEmbedPlugin{mockPlugin: mockPlugin{name: "lifecycle", tier: TierEmbed}}
	rp := NewRetroactiveProcessor(store, p, DigestEmbed)

	ctx := context.Background()
	rp.Start(ctx)

	// Give the goroutine time to run the initial processBatch
	time.Sleep(50 * time.Millisecond)

	stats := rp.Stats()
	if stats.PluginName != "lifecycle" {
		t.Errorf("PluginName = %q, want %q", stats.PluginName, "lifecycle")
	}

	rp.Stop()

	stats = rp.Stats()
	if stats.Status != "stopped" {
		t.Errorf("Status = %q, want %q", stats.Status, "stopped")
	}
}

func TestRetroactiveProcessor_StopWithNilCancel(t *testing.T) {
	rp := NewRetroactiveProcessor(&mockPluginStore{}, &mockEmbedPlugin{
		mockPlugin: mockPlugin{name: "x", tier: TierEmbed},
	}, DigestEmbed)

	// Stop before Start should not panic
	rp.Stop()
}

// ---------------------------------------------------------------------------
// processBatch — zero work
// ---------------------------------------------------------------------------

func TestRetroactiveProcessor_ProcessBatchNoWork(t *testing.T) {
	store := &mockPluginStore{countResult: 0}
	p := &mockEmbedPlugin{mockPlugin: mockPlugin{name: "noop", tier: TierEmbed}}
	rp := NewRetroactiveProcessor(store, p, DigestEmbed)

	ok := rp.processBatch(context.Background())
	if !ok {
		t.Error("processBatch should return true when there is no work")
	}
}

// ---------------------------------------------------------------------------
// processBatch — count error
// ---------------------------------------------------------------------------

func TestRetroactiveProcessor_ProcessBatchCountError(t *testing.T) {
	store := &mockPluginStore{countErr: errors.New("db down")}
	p := &mockEmbedPlugin{mockPlugin: mockPlugin{name: "fail", tier: TierEmbed}}
	rp := NewRetroactiveProcessor(store, p, DigestEmbed)

	ok := rp.processBatch(context.Background())
	if ok {
		t.Error("processBatch should return false on count error")
	}
}

// ---------------------------------------------------------------------------
// processBatch — nil iterator
// ---------------------------------------------------------------------------

func TestRetroactiveProcessor_ProcessBatchNilIterator(t *testing.T) {
	store := &mockPluginStore{countResult: 5, scanResult: nil}
	p := &mockEmbedPlugin{mockPlugin: mockPlugin{name: "niliter", tier: TierEmbed}}
	rp := NewRetroactiveProcessor(store, p, DigestEmbed)

	ok := rp.processBatch(context.Background())
	if ok {
		t.Error("processBatch should return false when iterator is nil")
	}
}

// ---------------------------------------------------------------------------
// processBatch — enrich path (processEngram)
// ---------------------------------------------------------------------------

func TestRetroactiveProcessor_ProcessBatchEnrich(t *testing.T) {
	eng := &Engram{Concept: "test", Content: "content"}
	iter := &mockIterator{engrams: []*Engram{eng}}

	store := &mockPluginStore{countResult: 1, scanResult: iter}
	enrichPlugin := &enrichMockForRetro{
		mockPlugin:   mockPlugin{name: "enrich-retro", tier: TierEnrich},
		enrichResult: &EnrichmentResult{Summary: "sum"},
	}
	rp := NewRetroactiveProcessor(store, enrichPlugin, DigestEnrich)

	ok := rp.processBatch(context.Background())
	if !ok {
		t.Error("processBatch should return true on success")
	}
	if enrichPlugin.callCount != 1 {
		t.Errorf("expected 1 enrich call, got %d", enrichPlugin.callCount)
	}

	stats := rp.Stats()
	if stats.Processed != 1 {
		t.Errorf("expected Processed=1, got %d", stats.Processed)
	}
}

// ---------------------------------------------------------------------------
// processBatch — enrich path with entities and relationships
// ---------------------------------------------------------------------------

func TestRetroactiveProcessor_ProcessBatchEnrichWithEntities(t *testing.T) {
	eng := &Engram{Concept: "project", Content: "uses postgres"}
	iter := &mockIterator{engrams: []*Engram{eng}}

	store := &mockPluginStore{countResult: 1, scanResult: iter}
	enrichPlugin := &enrichMockForRetro{
		mockPlugin: mockPlugin{name: "enrich-ent", tier: TierEnrich},
		enrichResult: &EnrichmentResult{
			Summary: "sum",
			Entities: []ExtractedEntity{
				{Name: "postgres", Type: "database", Confidence: 0.9},
			},
			Relationships: []ExtractedRelation{
				{FromEntity: "project", ToEntity: "postgres", RelType: "uses", Weight: 0.8},
			},
		},
	}
	rp := NewRetroactiveProcessor(store, enrichPlugin, DigestEnrich)

	ok := rp.processBatch(context.Background())
	if !ok {
		t.Error("processBatch should return true")
	}
}

// ---------------------------------------------------------------------------
// processBatch — embed path
// ---------------------------------------------------------------------------

func TestRetroactiveProcessor_ProcessBatchEmbed(t *testing.T) {
	eng := &Engram{Concept: "vec", Content: "data"}
	iter := &mockIterator{engrams: []*Engram{eng}}

	store := &mockPluginStore{countResult: 1, scanResult: iter}
	embedPlugin := &mockEmbedPlugin{
		mockPlugin: mockPlugin{name: "embed-retro", tier: TierEmbed},
	}
	rp := NewRetroactiveProcessor(store, embedPlugin, DigestEmbed)

	ok := rp.processBatch(context.Background())
	if !ok {
		t.Error("processBatch should return true")
	}
	if store.updateEmbedCalls != 1 {
		t.Errorf("expected 1 UpdateEmbedding call, got %d", store.updateEmbedCalls)
	}
	if store.hnswInsertCalls != 1 {
		t.Errorf("expected 1 HNSWInsert call, got %d", store.hnswInsertCalls)
	}
	if store.setFlagCalls != 1 {
		t.Errorf("expected 1 SetDigestFlag call, got %d", store.setFlagCalls)
	}
}

// ---------------------------------------------------------------------------
// processBatch — embed path with UpdateEmbedding error
// ---------------------------------------------------------------------------

func TestRetroactiveProcessor_ProcessBatchEmbedUpdateError(t *testing.T) {
	eng := &Engram{Concept: "vec", Content: "data"}
	iter := &mockIterator{engrams: []*Engram{eng}}

	store := &mockPluginStore{countResult: 1, scanResult: iter, updateEmbedErr: errors.New("write fail")}
	embedPlugin := &mockEmbedPlugin{
		mockPlugin: mockPlugin{name: "embed-err", tier: TierEmbed},
	}
	rp := NewRetroactiveProcessor(store, embedPlugin, DigestEmbed)

	ok := rp.processBatch(context.Background())
	if !ok {
		t.Error("processBatch should return true even with embed errors")
	}

	stats := rp.Stats()
	if stats.Errors != 1 {
		t.Errorf("expected Errors=1, got %d", stats.Errors)
	}
}

// ---------------------------------------------------------------------------
// processBatch — enrich processEngram error
// ---------------------------------------------------------------------------

func TestRetroactiveProcessor_ProcessBatchEnrichError(t *testing.T) {
	eng := &Engram{Concept: "fail", Content: "content"}
	iter := &mockIterator{engrams: []*Engram{eng}}

	store := &mockPluginStore{countResult: 1, scanResult: iter}
	enrichPlugin := &enrichMockForRetro{
		mockPlugin: mockPlugin{name: "enrich-fail", tier: TierEnrich},
		enrichErr:  errors.New("llm timeout"),
	}
	rp := NewRetroactiveProcessor(store, enrichPlugin, DigestEnrich)

	ok := rp.processBatch(context.Background())
	if !ok {
		t.Error("processBatch should return true even with enrich errors")
	}

	stats := rp.Stats()
	if stats.Errors != 1 {
		t.Errorf("expected Errors=1, got %d", stats.Errors)
	}
}

func TestRetroactiveProcessor_ProcessBatchEnrichNilResult(t *testing.T) {
	eng := &Engram{Concept: "nil", Content: "content"}
	iter := &mockIterator{engrams: []*Engram{eng}}

	store := &mockPluginStore{countResult: 1, scanResult: iter}
	enrichPlugin := &enrichMockForRetro{
		mockPlugin: mockPlugin{name: "enrich-nil", tier: TierEnrich},
	}
	rp := NewRetroactiveProcessor(store, enrichPlugin, DigestEnrich)

	ok := rp.processBatch(context.Background())
	if !ok {
		t.Error("processBatch should return true even with enrich errors")
	}

	stats := rp.Stats()
	if stats.Errors != 1 {
		t.Errorf("expected Errors=1, got %d", stats.Errors)
	}
	if store.setFlagCalls != 0 {
		t.Errorf("expected no digest flags to be set on nil result, got %d calls", store.setFlagCalls)
	}
}

func TestRetroactiveProcessor_ProcessBatchEntityPersistenceError(t *testing.T) {
	eng := &Engram{Concept: "retry", Content: "uses postgres"}
	iter := &mockIterator{engrams: []*Engram{eng}}

	store := &mockPluginStore{
		countResult: 1,
		scanResult:  iter,
		linkErr:     errors.New("link fail"),
	}
	enrichPlugin := &enrichMockForRetro{
		mockPlugin: mockPlugin{name: "enrich-entity-fail", tier: TierEnrich},
		enrichResult: &EnrichmentResult{
			Entities: []ExtractedEntity{
				{Name: "postgres", Type: "database", Confidence: 0.9},
			},
		},
	}
	rp := NewRetroactiveProcessor(store, enrichPlugin, DigestEnrich)

	ok := rp.processBatch(context.Background())
	if !ok {
		t.Error("processBatch should return true even with enrich errors")
	}

	stats := rp.Stats()
	if stats.Errors != 1 {
		t.Errorf("expected Errors=1, got %d", stats.Errors)
	}
	if store.setFlagCalls != 0 {
		t.Errorf("expected no digest flags to be set on entity persistence failure, got %v", store.setFlags)
	}
}

// ---------------------------------------------------------------------------
// processBatch — SetDigestFlag error on enrich path
// ---------------------------------------------------------------------------

func TestRetroactiveProcessor_ProcessBatchSetFlagError(t *testing.T) {
	eng := &Engram{Concept: "ok", Content: "content"}
	iter := &mockIterator{engrams: []*Engram{eng}}

	store := &mockPluginStore{countResult: 1, scanResult: iter, setFlagErr: errors.New("flag fail")}
	enrichPlugin := &enrichMockForRetro{
		mockPlugin:   mockPlugin{name: "enrich-flagfail", tier: TierEnrich},
		enrichResult: &EnrichmentResult{},
	}
	rp := NewRetroactiveProcessor(store, enrichPlugin, DigestEnrich)

	ok := rp.processBatch(context.Background())
	if !ok {
		t.Error("processBatch should return true even with flag errors")
	}

	stats := rp.Stats()
	if stats.Errors != 1 {
		t.Errorf("expected Errors=1, got %d", stats.Errors)
	}
}

// ---------------------------------------------------------------------------
// processBatch — context cancellation mid-batch
// ---------------------------------------------------------------------------

func TestRetroactiveProcessor_ProcessBatchCancelled(t *testing.T) {
	engs := make([]*Engram, 5)
	for i := range engs {
		engs[i] = &Engram{Concept: "c", Content: "x"}
	}
	iter := &mockIterator{engrams: engs}

	store := &mockPluginStore{countResult: 5, scanResult: iter}
	enrichPlugin := &enrichMockForRetro{
		mockPlugin:   mockPlugin{name: "cancel", tier: TierEnrich},
		enrichResult: &EnrichmentResult{},
	}
	rp := NewRetroactiveProcessor(store, enrichPlugin, DigestEnrich)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	ok := rp.processBatch(ctx)
	if !ok {
		t.Error("processBatch should return true on cancellation")
	}
}

// ---------------------------------------------------------------------------
// processEngram — plain plugin (not embed, not enrich) → no-op
// ---------------------------------------------------------------------------

func TestRetroactiveProcessor_ProcessEngramPlainPlugin(t *testing.T) {
	store := &mockPluginStore{}
	p := &mockPlugin{name: "plain", tier: TierEmbed}
	rp := NewRetroactiveProcessor(store, p, DigestEmbed)

	eng := &Engram{Concept: "x", Content: "y"}
	err := rp.processEngram(context.Background(), eng)
	if err != nil {
		t.Errorf("processEngram plain should return nil, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// processEngram — embed path
// ---------------------------------------------------------------------------

func TestRetroactiveProcessor_ProcessEngramEmbed(t *testing.T) {
	store := &mockPluginStore{}
	p := &mockEmbedPlugin{mockPlugin: mockPlugin{name: "pe", tier: TierEmbed}}
	rp := NewRetroactiveProcessor(store, p, DigestEmbed)

	eng := &Engram{Concept: "hello", Content: "world"}
	err := rp.processEngram(context.Background(), eng)
	if err != nil {
		t.Errorf("processEngram embed should succeed, got %v", err)
	}
	if store.updateEmbedCalls != 1 {
		t.Errorf("expected 1 UpdateEmbedding call, got %d", store.updateEmbedCalls)
	}
	if store.hnswInsertCalls != 1 {
		t.Errorf("expected 1 HNSWInsert call, got %d", store.hnswInsertCalls)
	}
	if store.autoLinkCalls != 1 {
		t.Errorf("expected 1 AutoLinkByEmbedding call, got %d", store.autoLinkCalls)
	}
}

// ---------------------------------------------------------------------------
// processEngram — enrich path
// ---------------------------------------------------------------------------

func TestRetroactiveProcessor_ProcessEngramEnrich(t *testing.T) {
	store := &mockPluginStore{}
	p := &enrichMockForRetro{
		mockPlugin:   mockPlugin{name: "pe-enrich", tier: TierEnrich},
		enrichResult: &EnrichmentResult{Summary: "s"},
	}
	rp := NewRetroactiveProcessor(store, p, DigestEnrich)

	eng := &Engram{Concept: "hello", Content: "world"}
	err := rp.processEngram(context.Background(), eng)
	if err != nil {
		t.Errorf("processEngram enrich should succeed, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// processEngram — embed errors propagate
// ---------------------------------------------------------------------------

func TestRetroactiveProcessor_ProcessEngramEmbedError(t *testing.T) {
	store := &mockPluginStore{updateEmbedErr: errors.New("store fail")}
	p := &mockEmbedPlugin{mockPlugin: mockPlugin{name: "ee", tier: TierEmbed}}
	rp := NewRetroactiveProcessor(store, p, DigestEmbed)

	eng := &Engram{Concept: "a", Content: "b"}
	err := rp.processEngram(context.Background(), eng)
	if err == nil {
		t.Error("expected error from UpdateEmbedding failure")
	}
}

// ---------------------------------------------------------------------------
// backoff
// ---------------------------------------------------------------------------

func TestRetroactiveProcessor_BackoffFirstError(t *testing.T) {
	rp := NewRetroactiveProcessor(&mockPluginStore{}, &mockEmbedPlugin{
		mockPlugin: mockPlugin{name: "b", tier: TierEmbed},
	}, DigestEmbed)

	start := time.Now()
	rp.backoff(context.Background(), 1)
	elapsed := time.Since(start)

	// First error: no extra wait
	if elapsed > 100*time.Millisecond {
		t.Errorf("backoff(1) should not wait, took %v", elapsed)
	}
}

func TestRetroactiveProcessor_BackoffContextCancel(t *testing.T) {
	rp := NewRetroactiveProcessor(&mockPluginStore{}, &mockEmbedPlugin{
		mockPlugin: mockPlugin{name: "bc", tier: TierEmbed},
	}, DigestEmbed)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	rp.backoff(ctx, 3)
	elapsed := time.Since(start)

	if elapsed > 100*time.Millisecond {
		t.Errorf("backoff with cancelled context should return immediately, took %v", elapsed)
	}
}

// ---------------------------------------------------------------------------
// Start with Notify wakeup
// ---------------------------------------------------------------------------

func TestRetroactiveProcessor_NotifyWakeup(t *testing.T) {
	store := &mockPluginStore{countResult: 0}
	p := &mockEmbedPlugin{mockPlugin: mockPlugin{name: "wake", tier: TierEmbed}}
	rp := NewRetroactiveProcessor(store, p, DigestEmbed)

	ctx := context.Background()
	rp.Start(ctx)
	time.Sleep(20 * time.Millisecond)

	// Notify should wake the loop and trigger a processBatch
	rp.Notify()
	time.Sleep(20 * time.Millisecond)

	rp.Stop()

	stats := rp.Stats()
	if stats.Status != "stopped" {
		t.Errorf("Status = %q, want %q", stats.Status, "stopped")
	}
}

// ---------------------------------------------------------------------------
// processEngram — DigestEntities flag suppresses UpsertEntity calls
// ---------------------------------------------------------------------------

// TestRetroactiveProcessor_DigestEntitiesFlagSkipsUpsert verifies the core bug fix:
// when the DigestEntities flag is already set on an engram, processEngram must NOT
// call UpsertEntity. Previously, hasEntities was derived from len(KeyPoints) > 0,
// which incorrectly conflated summarization key points with entity extraction.
// After the fix, GetDigestFlags is the authoritative source.
func TestRetroactiveProcessor_DigestEntitiesFlagSkipsUpsert(t *testing.T) {
	// The engram has KeyPoints set (simulating summarization having run),
	// but NO Summary — the old buggy code used len(KeyPoints)>0 as the entity proxy,
	// which would have skipped UpsertEntity even when entities hadn't been extracted.
	// The fixed code reads GetDigestFlags; with DigestEntities set, it must skip.
	eng := &Engram{
		Concept:   "service",
		Content:   "uses postgres",
		KeyPoints: []string{"uses postgres"}, // set by summarization — NOT entity extraction
	}
	iter := &mockIterator{engrams: []*Engram{eng}}

	store := &mockPluginStore{
		countResult:    1,
		scanResult:     iter,
		getFlagsResult: DigestEntities, // authoritative: entities already extracted
	}
	enrichPlugin := &enrichMockForRetro{
		mockPlugin: mockPlugin{name: "enrich-flag-test", tier: TierEnrich},
		enrichResult: &EnrichmentResult{
			Summary: "summary",
			Entities: []ExtractedEntity{
				{Name: "postgres", Type: "database", Confidence: 0.9},
			},
		},
	}
	rp := NewRetroactiveProcessor(store, enrichPlugin, DigestEnrich)

	err := rp.processEngram(context.Background(), eng)
	if err != nil {
		t.Fatalf("processEngram should succeed, got %v", err)
	}

	// UpsertEntity must NOT have been called because DigestEntities flag was set.
	if store.upsertEntityCalls != 0 {
		t.Errorf("UpsertEntity should not be called when DigestEntities flag is set, got %d calls",
			store.upsertEntityCalls)
	}
}

// TestRetroactiveProcessor_NoDigestEntitiesFlagCallsUpsert verifies the positive case:
// when DigestEntities flag is NOT set and the enrich result has entities, UpsertEntity
// must be called for each entity.
func TestRetroactiveProcessor_NoDigestEntitiesFlagCallsUpsert(t *testing.T) {
	eng := &Engram{
		Concept: "service",
		Content: "uses postgres",
	}
	iter := &mockIterator{engrams: []*Engram{eng}}

	store := &mockPluginStore{
		countResult:    1,
		scanResult:     iter,
		getFlagsResult: 0, // no flags set — entities not yet extracted
	}
	enrichPlugin := &enrichMockForRetro{
		mockPlugin: mockPlugin{name: "enrich-noupsert-test", tier: TierEnrich},
		enrichResult: &EnrichmentResult{
			Summary: "summary",
			Entities: []ExtractedEntity{
				{Name: "postgres", Type: "database", Confidence: 0.9},
				{Name: "service", Type: "service", Confidence: 0.8},
			},
		},
	}
	rp := NewRetroactiveProcessor(store, enrichPlugin, DigestEnrich)

	err := rp.processEngram(context.Background(), eng)
	if err != nil {
		t.Fatalf("processEngram should succeed, got %v", err)
	}

	// UpsertEntity MUST be called once per entity when flag is not set.
	if store.upsertEntityCalls != 2 {
		t.Errorf("expected 2 UpsertEntity calls, got %d", store.upsertEntityCalls)
	}
}

// TestRetroactiveProcessor_KeyPointsAloneDoNotSkipEntities verifies the bug fix directly:
// KeyPoints being non-empty (set by summarization) must NOT prevent entity extraction.
// The old code used len(eng.KeyPoints) > 0 as the hasEntities proxy — this test would
// have FAILED before the fix (UpsertEntity would have been skipped).
func TestRetroactiveProcessor_KeyPointsAloneDoNotSkipEntities(t *testing.T) {
	eng := &Engram{
		Concept:   "service",
		Content:   "uses postgres",
		Summary:   "",                      // no summary yet
		KeyPoints: []string{"key point 1"}, // set by a prior summarization run
	}
	iter := &mockIterator{engrams: []*Engram{eng}}

	store := &mockPluginStore{
		countResult:    1,
		scanResult:     iter,
		getFlagsResult: 0, // DigestEntities NOT set — entities must be extracted
	}
	enrichPlugin := &enrichMockForRetro{
		mockPlugin: mockPlugin{name: "enrich-kp-test", tier: TierEnrich},
		enrichResult: &EnrichmentResult{
			Summary: "summary",
			Entities: []ExtractedEntity{
				{Name: "postgres", Type: "database", Confidence: 0.9},
			},
		},
	}
	rp := NewRetroactiveProcessor(store, enrichPlugin, DigestEnrich)

	err := rp.processEngram(context.Background(), eng)
	if err != nil {
		t.Fatalf("processEngram should succeed, got %v", err)
	}

	// Even though KeyPoints is non-empty, entities must be upserted because
	// the DigestEntities flag was not set. The bug fix ensures flags are authoritative.
	if store.upsertEntityCalls != 1 {
		t.Errorf("expected 1 UpsertEntity call (KeyPoints alone must not skip entities), got %d",
			store.upsertEntityCalls)
	}
}

func TestRetroactiveProcessor_MissingDigestFlagsDoNotSkipEngram(t *testing.T) {
	eng := &Engram{Concept: "service", Content: "uses postgres"}
	iter := &mockIterator{engrams: []*Engram{eng}}

	store := &mockPluginStore{
		countResult:    1,
		scanResult:     iter,
		getFlagsErr:    pebble.ErrNotFound,
		getFlagsResult: 0,
	}
	enrichPlugin := &enrichMockForRetro{
		mockPlugin: mockPlugin{name: "enrich-missing-flags", tier: TierEnrich},
		enrichResult: &EnrichmentResult{
			Summary: "summary",
		},
	}
	rp := NewRetroactiveProcessor(store, enrichPlugin, DigestEnrich)

	if err := rp.processEngram(context.Background(), eng); err != nil {
		t.Fatalf("processEngram should succeed when digest flags are missing: %v", err)
	}
	if enrichPlugin.callCount != 1 {
		t.Fatalf("expected enrich to run when digest flags are missing, got %d calls", enrichPlugin.callCount)
	}
}

// ---------------------------------------------------------------------------
// processBatch — iterator with nil engram
// ---------------------------------------------------------------------------

func TestRetroactiveProcessor_ProcessBatchNilEngram(t *testing.T) {
	iter := &mockIterator{engrams: []*Engram{nil, {Concept: "ok", Content: "c"}}}

	store := &mockPluginStore{countResult: 2, scanResult: iter}
	enrichPlugin := &enrichMockForRetro{
		mockPlugin:   mockPlugin{name: "nileng", tier: TierEnrich},
		enrichResult: &EnrichmentResult{},
	}
	rp := NewRetroactiveProcessor(store, enrichPlugin, DigestEnrich)

	ok := rp.processBatch(context.Background())
	if !ok {
		t.Error("processBatch should return true")
	}
	// Only the non-nil engram should be processed
	if enrichPlugin.callCount != 1 {
		t.Errorf("expected 1 enrich call (skipping nil), got %d", enrichPlugin.callCount)
	}
}
