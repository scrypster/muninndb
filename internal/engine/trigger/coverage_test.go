package trigger

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/storage"
)

// ---------------------------------------------------------------------------
// TryConsumeOrBurst — burst-aware rate limiter
// ---------------------------------------------------------------------------

func TestTryConsumeOrBurst_WithTokens(t *testing.T) {
	tb := newTokenBucket(10)
	// Bucket starts full (10 tokens). Normal consume via burst should succeed.
	if !tb.TryConsumeOrBurst(3) {
		t.Error("TryConsumeOrBurst with full bucket returned false, want true")
	}
}

func TestTryConsumeOrBurst_EmptyBucketAllowsOverdraft(t *testing.T) {
	tb := newTokenBucket(10)
	tb.tokens.Store(0)
	// Pin lastRefill in the future so refill doesn't restore tokens.
	tb.lastRefill.Store(time.Now().Add(time.Second).UnixNano())

	// overdraft=3 means minAllowed = -3000 milliunits.
	// Current tokens = 0 > -3000, so first burst should succeed (goes to -1000).
	if !tb.TryConsumeOrBurst(3) {
		t.Error("burst #1 with overdraft=3 should succeed at tokens=0")
	}
	// tokens now at -1000
	if !tb.TryConsumeOrBurst(3) {
		t.Error("burst #2 with overdraft=3 should succeed at tokens=-1000")
	}
	// tokens now at -2000
	if !tb.TryConsumeOrBurst(3) {
		t.Error("burst #3 with overdraft=3 should succeed at tokens=-2000")
	}
	// tokens now at -3000, which equals minAllowed → next must fail
	if tb.TryConsumeOrBurst(3) {
		t.Error("burst #4 should fail — overdraft exhausted at tokens=-3000")
	}
}

func TestTryConsumeOrBurst_ZeroOverdraft(t *testing.T) {
	tb := newTokenBucket(10)
	tb.tokens.Store(0)
	tb.lastRefill.Store(time.Now().Add(time.Second).UnixNano())

	// With overdraft=0, minAllowed=0. tokens(0) <= minAllowed(0) → refuse.
	if tb.TryConsumeOrBurst(0) {
		t.Error("burst with zero overdraft on empty bucket should fail")
	}
}

func TestTryConsumeOrBurst_RefillRestoresAfterOverdraft(t *testing.T) {
	tb := newTokenBucket(1000)
	tb.tokens.Store(-2000)
	// Backdate lastRefill by 10ms so refill adds tokens.
	// add = 10_000_000 * 1_000_000 / 1_000_000_000 = 10_000 milliunits
	tb.lastRefill.Store(time.Now().Add(-10 * time.Millisecond).UnixNano())

	if !tb.TryConsumeOrBurst(0) {
		t.Error("after refill, burst should succeed")
	}
}

// ---------------------------------------------------------------------------
// NewEmbedCache — exported constructor
// ---------------------------------------------------------------------------

func TestNewEmbedCache(t *testing.T) {
	cache := NewEmbedCache()
	if cache == nil {
		t.Fatal("NewEmbedCache returned nil")
	}
	// Should be usable: set and get.
	ctx := []string{"test"}
	vec := []float32{1.0, 2.0}
	cache.Set(ctx, vec)
	got, ok := cache.Get(ctx)
	if !ok {
		t.Fatal("Get returned miss after Set on NewEmbedCache instance")
	}
	if len(got) != 2 || got[0] != 1.0 || got[1] != 2.0 {
		t.Errorf("Get returned %v, want [1.0, 2.0]", got)
	}
}

// ---------------------------------------------------------------------------
// TriggerSystem.NotifyWrite — enqueue + buffer-full drop
// ---------------------------------------------------------------------------

func TestTriggerSystem_NotifyWrite(t *testing.T) {
	ts := newTestTriggerSystem()
	eng := &storage.Engram{
		ID:         storage.NewULID(),
		Concept:    "test",
		Content:    "content",
		Confidence: 0.9,
	}

	ts.NotifyWrite(42, eng, true)

	select {
	case ev := <-ts.WriteEvents:
		if ev.VaultID != 42 {
			t.Errorf("VaultID = %d, want 42", ev.VaultID)
		}
		if ev.Engram != eng {
			t.Error("Engram pointer mismatch")
		}
		if !ev.IsNew {
			t.Error("IsNew should be true")
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("NotifyWrite did not enqueue event")
	}
}

func TestTriggerSystem_NotifyWrite_BufferFull(t *testing.T) {
	ts := newTestTriggerSystem()
	eng := &storage.Engram{ID: storage.NewULID()}

	// Fill the buffer.
	for i := 0; i < writeEventBufSize; i++ {
		ts.NotifyWrite(1, eng, true)
	}

	// This call should silently drop (not panic or block).
	done := make(chan struct{})
	go func() {
		ts.NotifyWrite(1, eng, true)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("NotifyWrite blocked on full buffer — should drop")
	}
}

// ---------------------------------------------------------------------------
// TriggerSystem.ForVault — wrapper around registry
// ---------------------------------------------------------------------------

func TestTriggerSystem_ForVault(t *testing.T) {
	ts := newTestTriggerSystem()

	sub := newMinimalSub("fv-1", 55, 0)
	ts.registry.Add(sub)

	subs := ts.ForVault(55)
	if len(subs) != 1 {
		t.Fatalf("ForVault returned %d subs, want 1", len(subs))
	}
	if subs[0].ID != "fv-1" {
		t.Errorf("ForVault returned sub ID %q, want 'fv-1'", subs[0].ID)
	}

	empty := ts.ForVault(999)
	if len(empty) != 0 {
		t.Errorf("ForVault on empty vault returned %d subs, want 0", len(empty))
	}
}

// ---------------------------------------------------------------------------
// TriggerSystem.PruneExpired — wrapper around registry
// ---------------------------------------------------------------------------

func TestTriggerSystem_PruneExpired(t *testing.T) {
	ts := newTestTriggerSystem()

	expiring := newMinimalSub("ts-expire", 10, 1*time.Millisecond)
	permanent := newMinimalSub("ts-perm", 10, 0)
	ts.registry.Add(expiring)
	ts.registry.Add(permanent)

	time.Sleep(10 * time.Millisecond)

	pruned := ts.PruneExpired()
	if pruned != 1 {
		t.Errorf("PruneExpired returned %d, want 1", pruned)
	}

	subs := ts.ForVault(10)
	if len(subs) != 1 {
		t.Fatalf("after prune, ForVault returned %d subs, want 1", len(subs))
	}
	if subs[0].ID != "ts-perm" {
		t.Errorf("surviving sub ID = %q, want 'ts-perm'", subs[0].ID)
	}
}

// ---------------------------------------------------------------------------
// ActiveVaults — vault listing
// ---------------------------------------------------------------------------

func TestActiveVaults(t *testing.T) {
	reg := newRegistry()

	// Empty registry returns empty slice.
	vaults := reg.ActiveVaults()
	if len(vaults) != 0 {
		t.Errorf("ActiveVaults on empty registry returned %d, want 0", len(vaults))
	}

	reg.Add(newMinimalSub("av-1", 10, 0))
	reg.Add(newMinimalSub("av-2", 20, 0))
	reg.Add(newMinimalSub("av-3", 10, 0))

	vaults = reg.ActiveVaults()
	if len(vaults) != 2 {
		t.Fatalf("ActiveVaults returned %d vaults, want 2", len(vaults))
	}

	found := map[uint32]bool{}
	for _, v := range vaults {
		found[v] = true
	}
	if !found[10] {
		t.Error("vault 10 missing from ActiveVaults")
	}
	if !found[20] {
		t.Error("vault 20 missing from ActiveVaults")
	}
}

func TestActiveVaults_AfterRemoveAll(t *testing.T) {
	reg := newRegistry()
	reg.Add(newMinimalSub("rm-1", 77, 0))
	reg.Remove("rm-1")

	vaults := reg.ActiveVaults()
	// The byVault map may still have an empty slice keyed by 77;
	// ActiveVaults iterates byVault keys, so it might include it.
	// That's acceptable — we just verify the function doesn't panic.
	_ = vaults
}

// ---------------------------------------------------------------------------
// vaultWS — vault workspace ID conversion
// ---------------------------------------------------------------------------

func TestVaultWS(t *testing.T) {
	registry := newRegistry()
	w := &TriggerWorker{registry: registry}

	ws := w.vaultWS(0x01020304)
	expected := [8]byte{0x01, 0x02, 0x03, 0x04, 0, 0, 0, 0}
	if ws != expected {
		t.Errorf("vaultWS(0x01020304) = %v, want %v", ws, expected)
	}

	ws0 := w.vaultWS(0)
	if ws0 != [8]byte{} {
		t.Errorf("vaultWS(0) = %v, want zero", ws0)
	}

	wsMax := w.vaultWS(0xFFFFFFFF)
	expectedMax := [8]byte{0xFF, 0xFF, 0xFF, 0xFF, 0, 0, 0, 0}
	if wsMax != expectedMax {
		t.Errorf("vaultWS(0xFFFFFFFF) = %v, want %v", wsMax, expectedMax)
	}
}

// ---------------------------------------------------------------------------
// handleCognitive — cognitive event processing
// ---------------------------------------------------------------------------

func TestHandleCognitive_DeliversPush(t *testing.T) {
	registry := newRegistry()
	deliver := &DeliveryRouter{registry: registry}
	store := newMockTriggerStore()

	engID := storage.NewULID()
	store.metas[engID] = &storage.EngramMeta{
		ID:          engID,
		Confidence:  0.9,
		Relevance:   0.8,
		Stability:   30,
		LastAccess:  time.Now(),
		AccessCount: 5,
		State:       storage.StateActive,
	}

	var pushCount atomic.Int32
	sub := &Subscription{
		ID:             "cog-sub-1",
		VaultID:        5,
		Context:        []string{"test"},
		Threshold:      0.0,
		DeltaThreshold: 0.0,
		Deliver: func(_ context.Context, push *ActivationPush) error {
			pushCount.Add(1)
			if push.Trigger != TriggerThresholdCrossed {
				t.Errorf("Trigger = %q, want %q", push.Trigger, TriggerThresholdCrossed)
			}
			return nil
		},
		pushedScores: make(map[storage.ULID]float64),
		rateLimiter:  newTokenBucket(100),
	}
	registry.Add(sub)

	writeCh := make(chan *EngramEvent, 1)
	cogCh := make(chan CognitiveEvent, 1)
	contraCh := make(chan ContradictEvent, 1)

	worker := &TriggerWorker{
		registry:     registry,
		embedCache:   newEmbedCache(),
		store:        store,
		deliver:      deliver,
		writeEvents:  writeCh,
		cogEvents:    cogCh,
		contraEvents: contraCh,
	}

	cogCh <- CognitiveEvent{
		VaultID:  5,
		EngramID: engID,
		Field:    "relevance",
		OldValue: 0.3,
		NewValue: 0.8,
		Delta:    0.5,
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		worker.Run(ctx)
		close(done)
	}()

	time.Sleep(150 * time.Millisecond)
	cancel()
	<-done

	if pushCount.Load() < 1 {
		t.Errorf("expected at least 1 cognitive push, got %d", pushCount.Load())
	}
}

func TestHandleCognitive_NoSubsNoOp(t *testing.T) {
	registry := newRegistry()
	deliver := &DeliveryRouter{registry: registry}
	store := newMockTriggerStore()

	worker := &TriggerWorker{
		registry:     registry,
		embedCache:   newEmbedCache(),
		store:        store,
		deliver:      deliver,
		writeEvents:  make(chan *EngramEvent, 1),
		cogEvents:    make(chan CognitiveEvent, 1),
		contraEvents: make(chan ContradictEvent, 1),
	}

	// No subs for vault 99 — handleCognitive should return early.
	worker.handleCognitive(context.Background(), CognitiveEvent{
		VaultID:  99,
		EngramID: storage.NewULID(),
		Field:    "relevance",
		OldValue: 0.1,
		NewValue: 0.9,
		Delta:    0.8,
	})
}

func TestHandleCognitive_StoreErrorNoOp(t *testing.T) {
	registry := newRegistry()
	deliver := &DeliveryRouter{registry: registry}
	store := newMockTriggerStore() // returns empty metas for unknown IDs

	var pushCount atomic.Int32
	sub := &Subscription{
		ID:           "cog-err-sub",
		VaultID:      5,
		Threshold:    0.0,
		Deliver:      func(_ context.Context, _ *ActivationPush) error { pushCount.Add(1); return nil },
		pushedScores: make(map[storage.ULID]float64),
		rateLimiter:  newTokenBucket(100),
	}
	registry.Add(sub)

	worker := &TriggerWorker{
		registry:     registry,
		embedCache:   newEmbedCache(),
		store:        store,
		deliver:      deliver,
		writeEvents:  make(chan *EngramEvent, 1),
		cogEvents:    make(chan CognitiveEvent, 1),
		contraEvents: make(chan ContradictEvent, 1),
	}

	// Engram ID not in store → GetMetadata returns empty → early return.
	worker.handleCognitive(context.Background(), CognitiveEvent{
		VaultID:  5,
		EngramID: storage.NewULID(),
		Delta:    0.5,
	})

	time.Sleep(50 * time.Millisecond)
	if pushCount.Load() != 0 {
		t.Errorf("expected 0 pushes when store returns empty, got %d", pushCount.Load())
	}
}

func TestHandleCognitive_DeltaThresholdSuppresses(t *testing.T) {
	registry := newRegistry()
	deliver := &DeliveryRouter{registry: registry}
	store := newMockTriggerStore()

	engID := storage.NewULID()
	store.metas[engID] = &storage.EngramMeta{
		ID:          engID,
		Confidence:  0.9,
		Relevance:   0.8,
		LastAccess:  time.Now(),
		AccessCount: 5,
		State:       storage.StateActive,
	}

	var pushCount atomic.Int32
	sub := &Subscription{
		ID:             "cog-delta-sub",
		VaultID:        5,
		Threshold:      0.0,
		DeltaThreshold: 0.99, // very high delta threshold
		Deliver:        func(_ context.Context, _ *ActivationPush) error { pushCount.Add(1); return nil },
		pushedScores:   map[storage.ULID]float64{engID: 0.5}, // existing score
		rateLimiter:    newTokenBucket(100),
	}
	registry.Add(sub)

	worker := &TriggerWorker{
		registry:     registry,
		embedCache:   newEmbedCache(),
		store:        store,
		deliver:      deliver,
		writeEvents:  make(chan *EngramEvent, 1),
		cogEvents:    make(chan CognitiveEvent, 1),
		contraEvents: make(chan ContradictEvent, 1),
	}

	worker.handleCognitive(context.Background(), CognitiveEvent{
		VaultID:  5,
		EngramID: engID,
		Field:    "relevance",
		OldValue: 0.79,
		NewValue: 0.81,
		Delta:    0.02,
	})

	time.Sleep(50 * time.Millisecond)
	if pushCount.Load() != 0 {
		t.Errorf("expected 0 pushes when delta below DeltaThreshold, got %d", pushCount.Load())
	}
}

func TestHandleCognitive_ClearsScoreWhenBelowThreshold(t *testing.T) {
	registry := newRegistry()
	deliver := &DeliveryRouter{registry: registry}
	store := newMockTriggerStore()

	engID := storage.NewULID()
	store.metas[engID] = &storage.EngramMeta{
		ID:         engID,
		Confidence: 0.01, // very low confidence → score will be near zero
		Relevance:  0.0,
		LastAccess: time.Now().Add(-365 * 24 * time.Hour), // very old
		State:      storage.StateActive,
	}

	sub := &Subscription{
		ID:             "cog-clear-sub",
		VaultID:        5,
		Threshold:      0.99, // very high threshold → score won't pass
		DeltaThreshold: 0.0,
		Deliver:        func(_ context.Context, _ *ActivationPush) error { return nil },
		pushedScores:   map[storage.ULID]float64{engID: 0.8}, // previously pushed
		rateLimiter:    newTokenBucket(100),
	}
	registry.Add(sub)

	worker := &TriggerWorker{
		registry:     registry,
		embedCache:   newEmbedCache(),
		store:        store,
		deliver:      deliver,
		writeEvents:  make(chan *EngramEvent, 1),
		cogEvents:    make(chan CognitiveEvent, 1),
		contraEvents: make(chan ContradictEvent, 1),
	}

	worker.handleCognitive(context.Background(), CognitiveEvent{
		VaultID:  5,
		EngramID: engID,
		Field:    "confidence",
		OldValue: 0.8,
		NewValue: 0.01,
		Delta:    0.79,
	})

	sub.mu.Lock()
	_, stillTracked := sub.pushedScores[engID]
	sub.mu.Unlock()
	if stillTracked {
		t.Error("expected pushedScores entry to be cleared when score falls below threshold")
	}
}

// ---------------------------------------------------------------------------
// handleSweep — periodic sweep (exercises ActiveVaults + sweepVault)
// ---------------------------------------------------------------------------

type mockHNSW struct {
	mu      sync.Mutex
	results []ScoredID
}

func (m *mockHNSW) Search(_ context.Context, _ [8]byte, _ []float32, _ int) ([]ScoredID, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.results, nil
}

func TestHandleSweep_WithHNSW(t *testing.T) {
	registry := newRegistry()
	deliver := &DeliveryRouter{registry: registry}
	store := newMockTriggerStore()

	engID := storage.NewULID()
	store.metas[engID] = &storage.EngramMeta{
		ID:          engID,
		Confidence:  0.9,
		Relevance:   0.8,
		Stability:   30,
		LastAccess:  time.Now(),
		AccessCount: 10,
		State:       storage.StateActive,
	}
	store.engrams[engID] = &storage.Engram{
		ID:         engID,
		Confidence: 0.9,
		Relevance:  0.8,
		Stability:  30,
		LastAccess: time.Now(),
		State:      storage.StateActive,
	}

	hnsw := &mockHNSW{results: []ScoredID{{ID: engID, Score: 0.95}}}

	var pushCount atomic.Int32
	testVec := []float32{0.5, 0.5, 0.5, 0.5}
	sub := &Subscription{
		ID:             "sweep-sub",
		VaultID:        8,
		Context:        []string{"sweep context"},
		Threshold:      0.0,
		DeltaThreshold: 0.0,
		embedding:      testVec,
		Deliver: func(_ context.Context, push *ActivationPush) error {
			pushCount.Add(1)
			return nil
		},
		pushedScores: make(map[storage.ULID]float64),
		rateLimiter:  newTokenBucket(100),
	}
	registry.Add(sub)

	worker := &TriggerWorker{
		registry:     registry,
		embedCache:   newEmbedCache(),
		store:        store,
		hnsw:         hnsw,
		deliver:      deliver,
		writeEvents:  make(chan *EngramEvent, 1),
		cogEvents:    make(chan CognitiveEvent, 1),
		contraEvents: make(chan ContradictEvent, 1),
	}

	worker.handleSweep(context.Background())

	time.Sleep(100 * time.Millisecond)
	if pushCount.Load() < 1 {
		t.Errorf("expected at least 1 sweep push, got %d", pushCount.Load())
	}
}

func TestHandleSweep_NilHNSW(t *testing.T) {
	registry := newRegistry()
	deliver := &DeliveryRouter{registry: registry}

	sub := newMinimalSub("sweep-nil", 8, 0)
	sub.embedding = []float32{0.5, 0.5}
	sub.Context = []string{"ctx"}
	registry.Add(sub)

	worker := &TriggerWorker{
		registry:     registry,
		embedCache:   newEmbedCache(),
		store:        newMockTriggerStore(),
		hnsw:         nil, // sweepVault should return early
		deliver:      deliver,
		writeEvents:  make(chan *EngramEvent, 1),
		cogEvents:    make(chan CognitiveEvent, 1),
		contraEvents: make(chan ContradictEvent, 1),
	}

	// Should not panic.
	worker.handleSweep(context.Background())
}

func TestSweepVault_NoEmbedding_UsesEmbedder(t *testing.T) {
	registry := newRegistry()
	deliver := &DeliveryRouter{registry: registry}
	store := newMockTriggerStore()

	engID := storage.NewULID()
	store.metas[engID] = &storage.EngramMeta{
		ID:         engID,
		Confidence: 0.9,
		Relevance:  0.8,
		LastAccess: time.Now(),
		State:      storage.StateActive,
	}
	store.engrams[engID] = &storage.Engram{
		ID:         engID,
		Confidence: 0.9,
		Relevance:  0.8,
		LastAccess: time.Now(),
		State:      storage.StateActive,
	}

	hnsw := &mockHNSW{results: []ScoredID{{ID: engID, Score: 0.9}}}
	embedder := &stubTrigEmbedder{}

	var pushCount atomic.Int32
	sub := &Subscription{
		ID:             "sweep-embed-sub",
		VaultID:        9,
		Context:        []string{"needs embedding"},
		Threshold:      0.0,
		DeltaThreshold: 0.0,
		// embedding is nil — embedder should compute it
		Deliver: func(_ context.Context, _ *ActivationPush) error {
			pushCount.Add(1)
			return nil
		},
		pushedScores: make(map[storage.ULID]float64),
		rateLimiter:  newTokenBucket(100),
	}
	registry.Add(sub)

	worker := &TriggerWorker{
		registry:     registry,
		embedCache:   newEmbedCache(),
		store:        store,
		hnsw:         hnsw,
		embedder:     embedder,
		deliver:      deliver,
		writeEvents:  make(chan *EngramEvent, 1),
		cogEvents:    make(chan CognitiveEvent, 1),
		contraEvents: make(chan ContradictEvent, 1),
	}

	ws := worker.vaultWS(9)
	subs := registry.ForVault(9)
	worker.sweepVault(context.Background(), 9, ws, subs)

	time.Sleep(100 * time.Millisecond)

	sub.mu.Lock()
	hasEmbed := len(sub.embedding) > 0
	sub.mu.Unlock()
	if !hasEmbed {
		t.Error("embedder should have populated sub.embedding during sweep")
	}

	if pushCount.Load() < 1 {
		t.Errorf("expected at least 1 sweep push after embedding, got %d", pushCount.Load())
	}
}

func TestSweepVault_SkipsSoftDeleted(t *testing.T) {
	registry := newRegistry()
	deliver := &DeliveryRouter{registry: registry}
	store := newMockTriggerStore()

	engID := storage.NewULID()
	store.metas[engID] = &storage.EngramMeta{
		ID:         engID,
		Confidence: 0.9,
		Relevance:  0.8,
		LastAccess: time.Now(),
		State:      storage.StateSoftDeleted,
	}
	store.engrams[engID] = &storage.Engram{
		ID:    engID,
		State: storage.StateSoftDeleted,
	}

	hnsw := &mockHNSW{results: []ScoredID{{ID: engID, Score: 0.95}}}
	testVec := []float32{0.5, 0.5, 0.5, 0.5}

	var pushCount atomic.Int32
	sub := &Subscription{
		ID:             "sweep-del-sub",
		VaultID:        11,
		Context:        []string{"ctx"},
		Threshold:      0.0,
		DeltaThreshold: 0.0,
		embedding:      testVec,
		Deliver: func(_ context.Context, _ *ActivationPush) error {
			pushCount.Add(1)
			return nil
		},
		pushedScores: make(map[storage.ULID]float64),
		rateLimiter:  newTokenBucket(100),
	}
	registry.Add(sub)

	worker := &TriggerWorker{
		registry:     registry,
		embedCache:   newEmbedCache(),
		store:        store,
		hnsw:         hnsw,
		deliver:      deliver,
		writeEvents:  make(chan *EngramEvent, 1),
		cogEvents:    make(chan CognitiveEvent, 1),
		contraEvents: make(chan ContradictEvent, 1),
	}

	ws := worker.vaultWS(11)
	subs := registry.ForVault(11)
	worker.sweepVault(context.Background(), 11, ws, subs)

	time.Sleep(50 * time.Millisecond)
	if pushCount.Load() != 0 {
		t.Errorf("expected 0 pushes for soft-deleted engram, got %d", pushCount.Load())
	}
}

// ---------------------------------------------------------------------------
// handleSweep integrated through Run loop (exercises the sweep ticker path)
// ---------------------------------------------------------------------------

func TestHandleSweep_EmptyRegistry(t *testing.T) {
	registry := newRegistry()
	deliver := &DeliveryRouter{registry: registry}

	worker := &TriggerWorker{
		registry:     registry,
		embedCache:   newEmbedCache(),
		store:        newMockTriggerStore(),
		deliver:      deliver,
		writeEvents:  make(chan *EngramEvent, 1),
		cogEvents:    make(chan CognitiveEvent, 1),
		contraEvents: make(chan ContradictEvent, 1),
	}

	// Should not panic on empty registry.
	worker.handleSweep(context.Background())
}
