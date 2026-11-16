package plugin

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/storage"
)

type blockingVaultClearEnricher struct {
	mockPlugin

	firstStarted chan struct{}
	releaseFirst chan struct{}

	mu      sync.Mutex
	callIDs []ULID
}

type blockingVaultClearEmbedder struct {
	mockPlugin

	firstStarted chan struct{}
	releaseFirst chan struct{}

	mu        sync.Mutex
	callCount int
}

type scanSignalingStore struct {
	PluginStore
	scanned chan struct{}
	once    sync.Once
}

func (s *scanSignalingStore) ScanWithoutFlag(ctx context.Context, flag, skipFlags uint16) EngramIterator {
	iter := s.PluginStore.ScanWithoutFlag(ctx, flag, skipFlags)
	s.once.Do(func() { close(s.scanned) })
	return iter
}

func (p *blockingVaultClearEmbedder) Embed(_ context.Context, texts []string) ([]float32, error) {
	p.mu.Lock()
	p.callCount++
	call := p.callCount
	p.mu.Unlock()

	if call == 1 {
		close(p.firstStarted)
		<-p.releaseFirst
	}
	return make([]float32, len(texts)*4), nil
}

func (p *blockingVaultClearEmbedder) Dimension() int    { return 4 }
func (p *blockingVaultClearEmbedder) MaxBatchSize() int { return 2 }

func (p *blockingVaultClearEmbedder) calls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.callCount
}

func (p *blockingVaultClearEnricher) Enrich(_ context.Context, eng *Engram) (*EnrichmentResult, error) {
	p.mu.Lock()
	p.callIDs = append(p.callIDs, eng.ID)
	call := len(p.callIDs)
	p.mu.Unlock()

	if call == 1 {
		close(p.firstStarted)
		<-p.releaseFirst
	}
	return &EnrichmentResult{Summary: "enriched"}, nil
}

func (p *blockingVaultClearEnricher) calls() []ULID {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]ULID(nil), p.callIDs...)
}

func TestRetroactiveProcessor_ConcurrentVaultClearInvalidatesActivePass(t *testing.T) {
	store, registry, _ := openTestStoreWithHNSW(t)
	ctx := context.Background()
	ws := store.VaultPrefix("retro-clear")

	for i := 0; i < 3; i++ {
		if _, err := store.WriteEngram(ctx, ws, &storage.Engram{
			Concept: "pre-clear",
			Content: "must not leave the server after clear",
		}); err != nil {
			t.Fatalf("WriteEngram(pre-clear %d): %v", i, err)
		}
	}

	provider := &blockingVaultClearEnricher{
		mockPlugin:   mockPlugin{name: "blocked-enricher", tier: TierEnrich},
		firstStarted: make(chan struct{}),
		releaseFirst: make(chan struct{}),
	}
	processor := NewRetroactiveProcessor(NewStoreAdapter(store, registry), provider, DigestEnrich)

	passDone := make(chan bool, 1)
	go func() { passDone <- processor.processBatch(ctx) }()
	<-provider.firstStarted

	clearDone := make(chan error, 1)
	go func() {
		finishClear, err := processor.BeginVaultClear(ctx, ws)
		if err != nil {
			clearDone <- err
			return
		}
		_, err = store.ClearVault(ctx, ws)
		finishClear()
		clearDone <- err
	}()

	// BeginVaultClear invalidates the pass before waiting for the one in-flight
	// call. Waiting on the generation makes this interleaving deterministic
	// without sleeps: the clear has established its boundary before release.
	for processor.vaultGeneration(ws) == 0 {
		runtime.Gosched()
	}
	close(provider.releaseFirst)

	if err := <-clearDone; err != nil {
		t.Fatalf("ClearVault: %v", err)
	}
	if ok := <-passDone; !ok {
		t.Fatal("pre-clear processor pass reported a systemic store failure")
	}

	preClearCalls := provider.calls()
	if len(preClearCalls) != 1 {
		t.Fatalf("provider calls after clear = %d, want only the already-started call: %v", len(preClearCalls), preClearCalls)
	}
	if stats := processor.Stats(); stats.Errors != 0 {
		t.Fatalf("stale records inflated processor errors: %+v", stats)
	}

	newID, err := store.WriteEngram(ctx, ws, &storage.Engram{
		Concept: "post-clear",
		Content: "must process without a server restart",
	})
	if err != nil {
		t.Fatalf("WriteEngram(post-clear): %v", err)
	}
	if ok := processor.processBatch(ctx); !ok {
		t.Fatal("post-clear processor pass reported a systemic store failure")
	}

	allCalls := provider.calls()
	if len(allCalls) != 2 || allCalls[1] != ULID(newID) {
		t.Fatalf("post-clear calls = %v, want exactly new engram %s", allCalls, newID)
	}
	if _, err := store.GetEngram(ctx, ws, newID); err != nil {
		t.Fatalf("post-clear engram was not preserved: %v", err)
	}
}

func TestRetroactiveProcessor_ConcurrentVaultClearInvalidatesEmbeddingMicroBatch(t *testing.T) {
	store, registry, _ := openTestStoreWithHNSW(t)
	ctx := context.Background()
	ws := store.VaultPrefix("retro-clear-embed")

	for i := 0; i < 4; i++ {
		if _, err := store.WriteEngram(ctx, ws, &storage.Engram{
			Concept: "pre-clear embed",
			Content: "must not enter a later embedding micro-batch",
		}); err != nil {
			t.Fatalf("WriteEngram(pre-clear %d): %v", i, err)
		}
	}

	provider := &blockingVaultClearEmbedder{
		mockPlugin:   mockPlugin{name: "blocked-embedder", tier: TierEmbed},
		firstStarted: make(chan struct{}),
		releaseFirst: make(chan struct{}),
	}
	processor := NewRetroactiveProcessor(NewStoreAdapter(store, registry), provider, DigestEmbed)

	passDone := make(chan bool, 1)
	go func() { passDone <- processor.processBatch(ctx) }()
	<-provider.firstStarted

	clearDone := make(chan error, 1)
	go func() {
		finishClear, err := processor.BeginVaultClear(ctx, ws)
		if err != nil {
			clearDone <- err
			return
		}
		_, err = store.ClearVault(ctx, ws)
		finishClear()
		clearDone <- err
	}()
	for processor.vaultGeneration(ws) == 0 {
		runtime.Gosched()
	}
	close(provider.releaseFirst)

	if err := <-clearDone; err != nil {
		t.Fatalf("ClearVault: %v", err)
	}
	if ok := <-passDone; !ok {
		t.Fatal("pre-clear embedding pass reported a systemic store failure")
	}
	if calls := provider.calls(); calls != 1 {
		t.Fatalf("embedding calls after clear = %d, want only the already-started micro-batch", calls)
	}
	if stats := processor.Stats(); stats.Errors != 0 {
		t.Fatalf("stale embedding records inflated processor errors: %+v", stats)
	}

	newID, err := store.WriteEngram(ctx, ws, &storage.Engram{
		Concept: "post-clear embed",
		Content: "must embed without a server restart",
	})
	if err != nil {
		t.Fatalf("WriteEngram(post-clear): %v", err)
	}
	if ok := processor.processBatch(ctx); !ok {
		t.Fatal("post-clear embedding pass reported a systemic store failure")
	}
	if calls := provider.calls(); calls != 2 {
		t.Fatalf("post-clear embedding calls = %d, want one new micro-batch", calls)
	}
	if vec, err := store.GetEmbedding(ctx, ws, newID); err != nil || len(vec) != provider.Dimension() {
		t.Fatalf("post-clear embedding = (len %d, %v), want dimension %d", len(vec), err, provider.Dimension())
	}
}

func TestRetroactiveProcessor_PassOpenedDuringVaultClearIsInvalidated(t *testing.T) {
	store, registry, _ := openTestStoreWithHNSW(t)
	ctx := context.Background()
	ws := store.VaultPrefix("retro-clear-open-pass")
	if _, err := store.WriteEngram(ctx, ws, &storage.Engram{
		Concept: "pre-clear",
		Content: "snapshot opened during clear",
	}); err != nil {
		t.Fatalf("WriteEngram(pre-clear): %v", err)
	}

	provider := &blockingVaultClearEnricher{
		mockPlugin:   mockPlugin{name: "clear-window-enricher", tier: TierEnrich},
		firstStarted: make(chan struct{}),
		releaseFirst: make(chan struct{}),
	}
	close(provider.releaseFirst)
	adapter := &scanSignalingStore{
		PluginStore: NewStoreAdapter(store, registry),
		scanned:     make(chan struct{}),
	}
	processor := NewRetroactiveProcessor(adapter, provider, DigestEnrich)

	finishClear, err := processor.BeginVaultClear(ctx, ws)
	if err != nil {
		t.Fatalf("BeginVaultClear: %v", err)
	}
	passDone := make(chan bool, 1)
	go func() { passDone <- processor.processBatch(ctx) }()
	<-adapter.scanned

	if _, err := store.ClearVault(ctx, ws); err != nil {
		finishClear()
		t.Fatalf("ClearVault: %v", err)
	}
	finishClear()
	if ok := <-passDone; !ok {
		t.Fatal("pass opened during clear reported a systemic store failure")
	}
	if calls := provider.calls(); len(calls) != 0 {
		t.Fatalf("pass opened during clear called provider with stale records: %v", calls)
	}
}

func TestRetroactiveProcessor_VaultClearDoesNotWaitForAnotherVaultCall(t *testing.T) {
	store, registry, _ := openTestStoreWithHNSW(t)
	ctx := context.Background()
	activeWS := store.VaultPrefix("retro-clear-active-vault")
	clearWS := store.VaultPrefix("retro-clear-unrelated-vault")
	if _, err := store.WriteEngram(ctx, activeWS, &storage.Engram{
		Concept: "active vault",
		Content: "provider call remains in flight",
	}); err != nil {
		t.Fatalf("WriteEngram(active): %v", err)
	}
	clearID, err := store.WriteEngram(ctx, clearWS, &storage.Engram{
		Concept: "clear vault",
		Content: "unrelated record",
	})
	if err != nil {
		t.Fatalf("WriteEngram(clear): %v", err)
	}
	if err := store.SetDigestFlag(ctx, clearID, DigestEnrich); err != nil {
		t.Fatalf("SetDigestFlag(clear): %v", err)
	}

	provider := &blockingVaultClearEnricher{
		mockPlugin:   mockPlugin{name: "per-vault-enricher", tier: TierEnrich},
		firstStarted: make(chan struct{}),
		releaseFirst: make(chan struct{}),
	}
	processor := NewRetroactiveProcessor(NewStoreAdapter(store, registry), provider, DigestEnrich)
	passDone := make(chan bool, 1)
	go func() { passDone <- processor.processBatch(ctx) }()
	<-provider.firstStarted

	// The active call holds activeWS's read lock. Acquiring and completing the
	// clear boundary for clearWS must not wait for that unrelated provider call.
	finishClear, err := processor.BeginVaultClear(ctx, clearWS)
	if err != nil {
		t.Fatalf("BeginVaultClear(unrelated): %v", err)
	}
	if _, err := store.ClearVault(ctx, clearWS); err != nil {
		finishClear()
		t.Fatalf("ClearVault(unrelated): %v", err)
	}
	finishClear()

	close(provider.releaseFirst)
	if ok := <-passDone; !ok {
		t.Fatal("active vault pass reported a systemic store failure")
	}
	if calls := provider.calls(); len(calls) != 1 {
		t.Fatalf("active vault provider calls = %v, want exactly the in-flight record", calls)
	}
}

func TestRetroactiveProcessor_ProviderFailureReleasesVaultClearAdmission(t *testing.T) {
	store, registry, _ := openTestStoreWithHNSW(t)
	ctx := context.Background()
	ws := store.VaultPrefix("retro-clear-provider-failure")
	if _, err := store.WriteEngram(ctx, ws, &storage.Engram{
		Concept: "provider failure",
		Content: "the pass must release its vault admission on every exit",
	}); err != nil {
		t.Fatalf("WriteEngram: %v", err)
	}

	provider := &enrichMockForRetro{
		mockPlugin: mockPlugin{name: "failed-enricher", tier: TierEnrich},
		enrichErr: &ProviderError{
			Provider:   "fake",
			StatusCode: 503,
			Retryable:  true,
		},
	}
	processor := NewRetroactiveProcessor(NewStoreAdapter(store, registry), provider, DigestEnrich)

	if ok := processor.processBatch(ctx); !ok {
		t.Fatal("provider failure should stop only the current pass")
	}

	clearAcquired := make(chan struct{})
	go func() {
		finishClear, err := processor.BeginVaultClear(ctx, ws)
		if err != nil {
			return
		}
		close(clearAcquired)
		finishClear()
	}()

	select {
	case <-clearAcquired:
	case <-time.After(time.Second):
		t.Fatal("BeginVaultClear remained blocked after the failed enrichment pass returned")
	}
}

func TestRetroactiveProcessor_CancelledVaultClearAbortsBoundary(t *testing.T) {
	store, registry, _ := openTestStoreWithHNSW(t)
	ctx := context.Background()
	ws := store.VaultPrefix("retro-clear-cancelled")
	id, err := store.WriteEngram(ctx, ws, &storage.Engram{
		Concept: "blocked call",
		Content: "cancelling the clear must leave this record intact",
	})
	if err != nil {
		t.Fatalf("WriteEngram: %v", err)
	}

	provider := &blockingVaultClearEnricher{
		mockPlugin:   mockPlugin{name: "cancelled-clear-enricher", tier: TierEnrich},
		firstStarted: make(chan struct{}),
		releaseFirst: make(chan struct{}),
	}
	processor := NewRetroactiveProcessor(NewStoreAdapter(store, registry), provider, DigestEnrich)
	passDone := make(chan bool, 1)
	go func() { passDone <- processor.processBatch(ctx) }()
	<-provider.firstStarted

	clearCtx, cancelClear := context.WithCancel(ctx)
	clearDone := make(chan error, 1)
	go func() {
		_, err := processor.BeginVaultClear(clearCtx, ws)
		clearDone <- err
	}()
	for processor.vaultGeneration(ws) == 0 {
		runtime.Gosched()
	}
	cancelClear()

	if err := <-clearDone; !errors.Is(err, context.Canceled) {
		close(provider.releaseFirst)
		t.Fatalf("BeginVaultClear error = %v, want context.Canceled", err)
	}
	if _, err := store.GetEngram(ctx, ws, id); err != nil {
		close(provider.releaseFirst)
		t.Fatalf("cancelled boundary allowed storage clear: %v", err)
	}

	close(provider.releaseFirst)
	if ok := <-passDone; !ok {
		t.Fatal("processor pass reported a systemic store failure")
	}

	finishClear, err := processor.BeginVaultClear(ctx, ws)
	if err != nil {
		t.Fatalf("BeginVaultClear after cancellation: %v", err)
	}
	finishClear()
}

// scanCountingStore counts ScanWithoutFlag invocations. It exists for the
// self-notify pin below: pass COUNT is the observable, so the wrapper counts
// rather than signals (its sibling scanSignalingStore signals first-scan).
type scanCountingStore struct {
	PluginStore
	scans atomic.Int64
}

func (s *scanCountingStore) ScanWithoutFlag(ctx context.Context, flag, skipFlags uint16) EngramIterator {
	s.scans.Add(1)
	return s.PluginStore.ScanWithoutFlag(ctx, flag, skipFlags)
}

// TestRetroactiveProcessor_InvalidatedPassDoesNotSelfNotify pins the feedback
// loop found in review: an invalidated pass used to call rp.Notify on itself,
// so a held clear boundary turned the processor into a hot loop (~175k
// passes/sec measured, per processor, starving unrelated vaults' scans since
// ScanWithoutFlag is global). The clear's finish closure and cancellation arm
// are the ONLY wake points an invalidation may rely on.
//
// The observable is pass count over a real-time window while the boundary is
// held: with the self-notify deleted the window sees the initial pass plus the
// single explicit notify; with it restored the count is in the tens of
// thousands. The generous bound is deliberate — this pins the absence of a
// runaway, not a precise schedule (pollInterval is 3s, far past the window).
func TestRetroactiveProcessor_InvalidatedPassDoesNotSelfNotify(t *testing.T) {
	store, registry, _ := openTestStoreWithHNSW(t)
	ctx := context.Background()
	ws := store.VaultPrefix("retro-clear-no-self-notify")
	if _, err := store.WriteEngram(ctx, ws, &storage.Engram{
		Concept: "pending work",
		Content: "keeps the scan non-empty while the boundary is held",
	}); err != nil {
		t.Fatalf("WriteEngram: %v", err)
	}

	provider := &blockingVaultClearEnricher{
		mockPlugin:   mockPlugin{name: "no-self-notify-enricher", tier: TierEnrich},
		firstStarted: make(chan struct{}),
		releaseFirst: make(chan struct{}),
	}
	close(provider.releaseFirst)
	adapter := &scanCountingStore{PluginStore: NewStoreAdapter(store, registry)}
	processor := NewRetroactiveProcessor(adapter, provider, DigestEnrich)

	finishClear, err := processor.BeginVaultClear(ctx, ws)
	if err != nil {
		t.Fatalf("BeginVaultClear: %v", err)
	}
	defer finishClear()

	processor.Start(ctx)
	defer processor.Stop()
	processor.Notify()

	time.Sleep(300 * time.Millisecond)
	if scans := adapter.scans.Load(); scans == 0 {
		// Lower bound, so the pin cannot be disarmed silently: a fixture change
		// that leaves no pending work (e.g. the digest flag already set) makes
		// the processor return before ever scanning, and an upper-bound-only
		// assertion would stay green while guarding nothing.
		t.Fatal("processor never scanned — the fixture has no pending work and this test is vacuous")
	}
	if scans := adapter.scans.Load(); scans > 5 {
		t.Fatalf("processor ran %d scan passes in 300ms with a clear boundary held — an invalidated pass is waking itself (want <= 5: the initial pass, the explicit notify, and margin)", scans)
	}
}
