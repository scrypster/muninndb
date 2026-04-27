package plugin

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cockroachdb/pebble"
	"github.com/scrypster/muninndb/internal/engine/circuit"
)

// ---------------------------------------------------------------------------
// Enrichment-capable mock for processEnrichEngram tests
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
// processBatch — enrich path (processEnrichEngram)
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
// processBatch — enrich processEnrichEngram error
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
	// LLM errors are permanent: DigestEnrichFailed must be set to prevent
	// infinite retry loops that trip the circuit breaker (regression test for #390).
	if store.setFlagCalls != 1 {
		t.Errorf("expected DigestEnrichFailed flag to be set after LLM error, got %d calls", store.setFlagCalls)
	}
	if len(store.setFlags) == 0 || store.setFlags[0] != DigestEnrichFailed {
		t.Errorf("expected flag %d (DigestEnrichFailed), got %v", DigestEnrichFailed, store.setFlags)
	}
}

func TestRetroactiveProcessor_ProcessBatchEnrichNilResult(t *testing.T) {
	eng := &Engram{Concept: "nil", Content: "content"}
	iter := &mockIterator{engrams: []*Engram{eng}}

	store := &mockPluginStore{countResult: 1, scanResult: iter}
	enrichPlugin := &enrichMockForRetro{
		mockPlugin: mockPlugin{name: "enrich-nil", tier: TierEnrich},
		// enrichResult and enrichErr both zero: Enrich returns (nil, nil).
		// processEnrichEngram treats nil result as a permanent LLM failure.
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
	// Nil result is a permanent LLM failure: DigestEnrichFailed must be set.
	if store.setFlagCalls != 1 {
		t.Errorf("expected DigestEnrichFailed flag to be set on nil result, got %d calls", store.setFlagCalls)
	}
	if len(store.setFlags) == 0 || store.setFlags[0] != DigestEnrichFailed {
		t.Errorf("expected flag %d, got %v", DigestEnrichFailed, store.setFlags)
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
// processEnrichEngram — plain plugin (not embed, not enrich) → no-op
// ---------------------------------------------------------------------------

func TestRetroactiveProcessor_ProcessEnrichEngramPlainPlugin(t *testing.T) {
	store := &mockPluginStore{}
	p := &mockPlugin{name: "plain", tier: TierEmbed}
	rp := NewRetroactiveProcessor(store, p, DigestEmbed)

	eng := &Engram{Concept: "x", Content: "y"}
	err := rp.processEnrichEngram(context.Background(), eng)
	if err != nil {
		t.Errorf("processEnrichEngram plain should return nil, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// processEnrichEngram — enrich path
// ---------------------------------------------------------------------------

func TestRetroactiveProcessor_ProcessEnrichEngramEnrich(t *testing.T) {
	store := &mockPluginStore{}
	p := &enrichMockForRetro{
		mockPlugin:   mockPlugin{name: "pe-enrich", tier: TierEnrich},
		enrichResult: &EnrichmentResult{Summary: "s"},
	}
	rp := NewRetroactiveProcessor(store, p, DigestEnrich)

	eng := &Engram{Concept: "hello", Content: "world"}
	err := rp.processEnrichEngram(context.Background(), eng)
	if err != nil {
		t.Errorf("processEnrichEngram enrich should succeed, got %v", err)
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
// processEnrichEngram — DigestEntities flag suppresses UpsertEntity calls
// ---------------------------------------------------------------------------

// TestRetroactiveProcessor_DigestEntitiesFlagSkipsUpsert verifies the core bug fix:
// when the DigestEntities flag is already set on an engram, processEnrichEngram must NOT
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

	err := rp.processEnrichEngram(context.Background(), eng)
	if err != nil {
		t.Fatalf("processEnrichEngram should succeed, got %v", err)
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

	err := rp.processEnrichEngram(context.Background(), eng)
	if err != nil {
		t.Fatalf("processEnrichEngram should succeed, got %v", err)
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

	err := rp.processEnrichEngram(context.Background(), eng)
	if err != nil {
		t.Fatalf("processEnrichEngram should succeed, got %v", err)
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

	if err := rp.processEnrichEngram(context.Background(), eng); err != nil {
		t.Fatalf("processEnrichEngram should succeed when digest flags are missing: %v", err)
	}
	if enrichPlugin.callCount != 1 {
		t.Fatalf("expected enrich to run when digest flags are missing, got %d calls", enrichPlugin.callCount)
	}
}

// ---------------------------------------------------------------------------
// Progress rate/ETA tests
// ---------------------------------------------------------------------------

// TestRetroactiveProcessor_ProgressEvery100 verifies that RatePerSec is populated
// after fewer than 1000 engrams are processed (i.e., after every micro-batch flush,
// not only at the 1000-engram boundary).
func TestRetroactiveProcessor_ProgressEvery100(t *testing.T) {
	const n = 50 // fewer than 1000, so old code would never update rate

	// Build n engrams for the mock iterator
	engrams := make([]*Engram, n)
	for i := range engrams {
		engrams[i] = &Engram{Content: "text"}
	}

	store := &mockPluginStore{
		countResult: int64(n),
		scanResult:  &mockIterator{engrams: engrams},
	}

	// maxBatchSize=10 so flushMicroBatch fires after every 10 engrams
	plugin := &mockEmbedPlugin{
		mockPlugin: mockPlugin{name: "embed-test", tier: TierEmbed},
	}

	rp := NewRetroactiveProcessor(store, plugin, DigestEmbed)

	// Sleep a tiny bit so elapsed > 0 during the rate calculation
	time.Sleep(10 * time.Millisecond)

	rp.processBatch(context.Background())

	stats := rp.Stats()
	if stats.RatePerSec <= 0 {
		t.Errorf("expected RatePerSec > 0 after %d engrams, got %v (old code only updated at 1000)", n, stats.RatePerSec)
	}
}

// TestRetroactiveProcessor_StaleRateReset verifies that RatePerSec and ETASeconds
// are zeroed at the start of each processBatch pass so stale values from a prior
// pass don't leak into the embed-status API response while the processor is idle.
func TestRetroactiveProcessor_StaleRateReset(t *testing.T) {
	store := &mockPluginStore{countResult: 0} // zero-work pass to trigger early-return path
	plugin := &mockEmbedPlugin{
		mockPlugin: mockPlugin{name: "embed-reset", tier: TierEmbed},
	}

	rp := NewRetroactiveProcessor(store, plugin, DigestEmbed)

	// Seed non-zero rate/ETA to simulate stale values left over from a prior pass.
	rp.statsMu.Lock()
	rp.stats.RatePerSec = 99.9
	rp.stats.ETASeconds = 9999
	rp.statsMu.Unlock()

	// processBatch must zero rate/ETA at the very start, before the count check.
	rp.processBatch(context.Background())

	rp.statsMu.RLock()
	rate := rp.stats.RatePerSec
	eta := rp.stats.ETASeconds
	rp.statsMu.RUnlock()

	if rate != 0 {
		t.Errorf("expected RatePerSec=0 at new pass start, got %v", rate)
	}
	if eta != 0 {
		t.Errorf("expected ETASeconds=0 at new pass start, got %v", eta)
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

// ---------------------------------------------------------------------------
// Regression tests for issue #390: ghost queue + circuit breaker deadlock
// ---------------------------------------------------------------------------

// TestRetroactiveProcessor_CircuitOpen_NoEnrichFailedFlag verifies that when
// the circuit breaker is open (transient systemic failure), the engram is NOT
// marked with DigestEnrichFailed. The circuit will reset on its own; the engram
// should be retried once the provider recovers.
func TestRetroactiveProcessor_CircuitOpen_NoEnrichFailedFlag(t *testing.T) {
	eng := &Engram{Concept: "valid", Content: "content"}
	iter := &mockIterator{engrams: []*Engram{eng}}

	store := &mockPluginStore{countResult: 1, scanResult: iter}
	enrichPlugin := &enrichMockForRetro{
		mockPlugin: mockPlugin{name: "enrich-circuit", tier: TierEnrich},
		enrichErr:  circuit.ErrOpen, // circuit breaker is open — transient
	}
	rp := NewRetroactiveProcessor(store, enrichPlugin, DigestEnrich)

	ok := rp.processBatch(context.Background())
	if !ok {
		t.Error("processBatch should return true even when circuit is open")
	}

	stats := rp.Stats()
	if stats.Errors != 1 {
		t.Errorf("expected Errors=1, got %d", stats.Errors)
	}
	// Circuit-open is transient: DigestEnrichFailed must NOT be set.
	if store.setFlagCalls != 0 {
		t.Errorf("DigestEnrichFailed must not be set for circuit-open errors, got %d flag calls", store.setFlagCalls)
	}
}

// TestRetroactiveProcessor_PersistenceError_NoEnrichFailedFlag verifies that
// when enrichment succeeds (LLM returns valid output) but storage persistence
// fails, the engram is NOT marked with DigestEnrichFailed. The storage error is
// transient; the engram should be retried when storage recovers.
func TestRetroactiveProcessor_PersistenceError_NoEnrichFailedFlag(t *testing.T) {
	eng := &Engram{Concept: "valid", Content: "content"}
	iter := &mockIterator{engrams: []*Engram{eng}}

	store := &mockPluginStore{
		countResult: 1,
		scanResult:  iter,
		linkErr:     errors.New("storage unavailable"), // persistence fails, not LLM
	}
	enrichPlugin := &enrichMockForRetro{
		mockPlugin: mockPlugin{name: "enrich-persist-fail", tier: TierEnrich},
		enrichResult: &EnrichmentResult{
			Summary: "good summary",
			Entities: []ExtractedEntity{
				{Name: "postgres", Type: "database", Confidence: 0.9},
			},
		},
	}
	rp := NewRetroactiveProcessor(store, enrichPlugin, DigestEnrich)

	ok := rp.processBatch(context.Background())
	if !ok {
		t.Error("processBatch should return true even with persistence errors")
	}

	// Storage error: DigestEnrichFailed must NOT be set — the engram should be
	// retried when storage recovers.
	if store.setFlagCalls != 0 {
		t.Errorf("DigestEnrichFailed must not be set for storage errors, got %d flag calls (flags: %v)", store.setFlagCalls, store.setFlags)
	}
}

// TestRetroactiveProcessor_SkipFlags_Enrich verifies that the enrich processor
// passes DigestEnrichFailed as skipFlags so engrams that failed LLM enrichment
// are excluded from future scans. This is the core fix for the infinite-retry
// loop that tripped the circuit breaker in issue #390.
func TestRetroactiveProcessor_SkipFlags_Enrich(t *testing.T) {
	rp := NewRetroactiveProcessor(&mockPluginStore{}, &enrichMockForRetro{
		mockPlugin: mockPlugin{name: "enrich-skipflags", tier: TierEnrich},
	}, DigestEnrich)

	if got := rp.skipFlags(); got != DigestEnrichFailed {
		t.Errorf("enrich processor skipFlags() = %d, want DigestEnrichFailed (%d)", got, DigestEnrichFailed)
	}
}
