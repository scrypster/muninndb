package plugin

import (
	"context"
	"runtime"
	"sync"
	"testing"

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

func (s *scanSignalingStore) ScanWithoutFlag(ctx context.Context, flag, skipFlags uint8) EngramIterator {
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
		finishClear := processor.BeginVaultClear(ws)
		_, err := store.ClearVault(ctx, ws)
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
		finishClear := processor.BeginVaultClear(ws)
		_, err := store.ClearVault(ctx, ws)
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

	finishClear := processor.BeginVaultClear(ws)
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
	finishClear := processor.BeginVaultClear(clearWS)
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
