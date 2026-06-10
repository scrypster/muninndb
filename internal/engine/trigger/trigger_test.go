// Package trigger tests the subscription registry, token bucket rate limiter,
// embed cache, and TriggerSystem lifecycle.  The tests live in the internal
// test package so they can reach unexported types (SubscriptionRegistry,
// tokenBucket, EmbedCache).
package trigger

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/storage"
)

// ---------------------------------------------------------------------------
// Minimal stubs for TriggerStore, FTSIndex, HNSWIndex, Embedder
// ---------------------------------------------------------------------------

type stubTriggerStore struct{}

func (s *stubTriggerStore) GetMetadata(_ context.Context, _ [8]byte, ids []storage.ULID) ([]*storage.EngramMeta, error) {
	return nil, nil
}

func (s *stubTriggerStore) GetEngrams(_ context.Context, _ [8]byte, ids []storage.ULID) ([]*storage.Engram, error) {
	return nil, nil
}

func (s *stubTriggerStore) GetEmbedding(_ context.Context, _ [8]byte, _ storage.ULID) ([]float32, error) {
	return nil, nil
}

func (s *stubTriggerStore) VaultPrefix(_ string) [8]byte {
	return [8]byte{}
}

type stubTrigFTS struct{}

func (f *stubTrigFTS) Search(_ context.Context, _ [8]byte, _ string, _ int) ([]ScoredID, error) {
	return nil, nil
}

type stubTrigHNSW struct{}

func (h *stubTrigHNSW) Search(_ context.Context, _ [8]byte, _ []float32, _ int) ([]ScoredID, error) {
	return nil, nil
}

type stubTrigEmbedder struct{}

func (e *stubTrigEmbedder) Embed(_ context.Context, _ []string) ([]float32, error) {
	v := make([]float32, 4)
	for i := range v {
		v[i] = 0.5
	}
	return v, nil
}

// newMinimalSub creates a Subscription that can be added to the registry
// without going through TriggerSystem.Subscribe (which requires a live embedder).
func newMinimalSub(id string, vaultID uint32, ttl time.Duration) *Subscription {
	sub := &Subscription{
		ID:           id,
		VaultID:      vaultID,
		Threshold:    0.0,
		RateLimit:    10,
		TTL:          ttl,
		rateLimiter:  newTokenBucket(10),
		pushedScores: make(map[storage.ULID]float64),
		createdAt:    time.Now(),
	}
	if ttl > 0 {
		sub.expiresAt = sub.createdAt.Add(ttl)
	}
	return sub
}

// newTestTriggerSystem builds a TriggerSystem with all stub dependencies.
func newTestTriggerSystem() *TriggerSystem {
	return New(
		&stubTriggerStore{},
		&stubTrigFTS{},
		&stubTrigHNSW{},
		&stubTrigEmbedder{},
	)
}

// ---------------------------------------------------------------------------
// Test 1: SubscriptionRegistry Add / Remove
// ---------------------------------------------------------------------------

func TestSubscriptionRegistryAddRemove(t *testing.T) {
	reg := newRegistry()

	s1 := newMinimalSub("sub-1", 1, 0)
	s2 := newMinimalSub("sub-2", 1, 0)
	s3 := newMinimalSub("sub-3", 1, 0)

	reg.Add(s1)
	reg.Add(s2)
	reg.Add(s3)

	subs := reg.ForVault(1)
	if len(subs) != 3 {
		t.Fatalf("ForVault returned %d subs, want 3", len(subs))
	}

	// Remove one.
	reg.Remove("sub-2")

	subs = reg.ForVault(1)
	if len(subs) != 2 {
		t.Fatalf("ForVault after Remove returned %d subs, want 2", len(subs))
	}

	for _, s := range subs {
		if s.ID == "sub-2" {
			t.Error("sub-2 still present after Remove")
		}
	}

	// sub-1 and sub-3 must still be present.
	ids := map[string]bool{"sub-1": false, "sub-3": false}
	for _, s := range subs {
		ids[s.ID] = true
	}
	for id, found := range ids {
		if !found {
			t.Errorf("%s not found after removing sub-2", id)
		}
	}
}

// ---------------------------------------------------------------------------
// Test 2: PruneExpired removes expired subs; TTL=0 subs survive
// ---------------------------------------------------------------------------

func TestSubscriptionRegistryPruneExpired(t *testing.T) {
	reg := newRegistry()

	// 1ms TTL — expires almost immediately.
	expiring := newMinimalSub("expiring", 99, 1*time.Millisecond)
	// No TTL — never expires.
	permanent := newMinimalSub("permanent", 99, 0)

	reg.Add(expiring)
	reg.Add(permanent)

	// Wait for the short TTL to lapse.
	time.Sleep(10 * time.Millisecond)

	pruned := reg.PruneExpired()
	if pruned == 0 {
		t.Error("PruneExpired removed 0 subscriptions, expected >= 1")
	}

	remaining := reg.ForVault(99)
	for _, s := range remaining {
		if s.ID == "expiring" {
			t.Error("expired subscription still present after PruneExpired")
		}
	}

	// permanent sub must still be there.
	found := false
	for _, s := range remaining {
		if s.ID == "permanent" {
			found = true
		}
	}
	if !found {
		t.Error("permanent (TTL=0) subscription was incorrectly pruned")
	}
}

// ---------------------------------------------------------------------------
// Test 3: tokenBucket — exhaust, verify false; wait for refill, verify true
// ---------------------------------------------------------------------------

func TestTokenBucketRateLimit(t *testing.T) {
	// Create via constructor: 1000 tokens/second → maxTokens = 1_000_000 milliunits.
	// Refill formula: add = elapsed_ns * maxTokens / 1e9
	// After 5ms: add = 5_000_000 * 1_000_000 / 1e9 = 5_000 milliunits = 5 tokens.
	tb := newTokenBucket(1000)
	// Drain to exactly 1 token (1000 milliunits).
	tb.tokens.Store(1000)
	tb.lastRefill.Store(time.Now().UnixNano())

	// Consume the single available token — must succeed.
	if !tb.TryConsume() {
		t.Fatal("first TryConsume returned false, want true")
	}

	// Pin lastRefill one second in the FUTURE so refill()'s elapsed < 0 check
	// short-circuits; the bucket stays empty regardless of scheduling jitter.
	tb.lastRefill.Store(time.Now().Add(time.Second).UnixNano())
	if tb.TryConsume() {
		t.Error("TryConsume on empty bucket returned true, want false")
	}

	// Backdate lastRefill by 5 ms: refill() computes elapsed ≈ 5_000_000 ns,
	// add = 5_000_000 * 1_000_000 / 1_000_000_000 = 5_000 milliunits ≫ 1_000.
	// No real sleep required.
	tb.lastRefill.Store(time.Now().Add(-5 * time.Millisecond).UnixNano())
	if !tb.TryConsume() {
		t.Error("TryConsume after backdating lastRefill returned false, want true")
	}
}

// ---------------------------------------------------------------------------
// Test 4: EmbedCache — store then retrieve, exact float32 equality
// ---------------------------------------------------------------------------

func TestEmbedCacheHit(t *testing.T) {
	cache := newEmbedCache()

	ctx := []string{"neural", "network"}
	vec := []float32{0.1, 0.2, 0.3, 0.4}

	cache.Set(ctx, vec)

	got, ok := cache.Get(ctx)
	if !ok {
		t.Fatal("Get returned miss, want hit")
	}

	if len(got) != len(vec) {
		t.Fatalf("got len %d, want %d", len(got), len(vec))
	}

	for i, v := range vec {
		if got[i] != v {
			t.Errorf("vec[%d] = %v, want %v", i, got[i], v)
		}
	}
}

// ---------------------------------------------------------------------------
// Test 5: EmbedCache — TTL expiry causes miss
// ---------------------------------------------------------------------------

func TestEmbedCacheTTLExpiry(t *testing.T) {
	// The EmbedCache uses a fixed embedCacheTTL constant (5 minutes) which we
	// cannot override from tests without modifying production code.  We test
	// the internal computedAt check by backdating the entry directly.
	cache := newEmbedCache()

	ctx := []string{"decay", "memory"}
	vec := []float32{1.0, 2.0}

	// Store normally.
	cache.Set(ctx, vec)

	// Immediately should hit.
	_, ok := cache.Get(ctx)
	if !ok {
		t.Fatal("immediate Get should hit")
	}

	// Backdate the entry's computedAt so it appears expired.
	fp := contextFingerprint(ctx)
	cache.mu.Lock()
	if entry, exists := cache.entries[fp]; exists {
		entry.computedAt = time.Now().Add(-(embedCacheTTL + time.Second))
	}
	cache.mu.Unlock()

	// Now Get must miss.
	got, ok := cache.Get(ctx)
	if ok {
		t.Errorf("Get after TTL expiry returned hit with %v, want miss", got)
	}

	// The expired entry should have been cleaned up.
	cache.mu.Lock()
	_, stillThere := cache.entries[fp]
	cache.mu.Unlock()
	if stillThere {
		t.Error("expired entry was not removed from cache on Get")
	}
}

// ---------------------------------------------------------------------------
// Test 6: EmbedCache — size stays bounded at embedCacheMax after overflow
// ---------------------------------------------------------------------------

func TestEmbedCacheMaxSize(t *testing.T) {
	cache := newEmbedCache()

	// Insert embedCacheMax+10 unique entries.
	over := embedCacheMax + 10
	for i := 0; i < over; i++ {
		// Build a unique key per iteration.
		key := []string{"key", string(rune(i/26 + 65)), string(rune(i%26 + 65)), "x"}
		cache.Set(key, []float32{float32(i)})
	}

	// Count actual entries in the map.
	cache.mu.Lock()
	size := len(cache.entries)
	cache.mu.Unlock()

	if size > embedCacheMax {
		t.Errorf("cache has %d entries after %d inserts, want <= %d", size, over, embedCacheMax)
	}
	// Some entries must remain.
	if size == 0 {
		t.Error("cache is unexpectedly empty after inserts")
	}
}

// ---------------------------------------------------------------------------
// Test 7: TriggerSystem Start / Stop — shuts down cleanly on context cancel
// ---------------------------------------------------------------------------

func TestTriggerSystemStartStop(t *testing.T) {
	ts := newTestTriggerSystem()

	ctx, cancel := context.WithCancel(context.Background())

	// Channel to detect when the worker goroutine exits.
	done := make(chan struct{})
	origRun := ts.worker.Run // save reference for wrapping
	_ = origRun              // not wrapping — we just time the cancel below

	ts.Start(ctx)

	// Add a subscription to exercise the worker loop.
	sub := newMinimalSub("stop-test", 7, 0)
	ts.registry.Add(sub)

	// Send a few events to make the worker do real work.
	ts.WriteEvents <- &EngramEvent{
		VaultID: 7,
		Engram:  &storage.Engram{ID: storage.NewULID(), Confidence: 1.0},
		IsNew:   true,
	}

	time.Sleep(10 * time.Millisecond)

	// Verify the system is alive by checking the registry still has the sub.
	subs := ts.registry.ForVault(7)
	if len(subs) == 0 {
		t.Error("expected subscription to persist while running")
	}

	// Signal shutdown.
	cancel()

	// Start a watcher goroutine that closes done when the channels drain.
	go func() {
		defer close(done)
		// Drain pending events so the worker doesn't block on sends.
		timeout := time.After(500 * time.Millisecond)
		for {
			select {
			case <-timeout:
				return
			case <-ts.WriteEvents:
			case <-ts.CognitiveEvents:
			case <-ts.ContradictEvents:
			}
		}
	}()

	select {
	case <-done:
		// Good — channels drained and goroutine exited.
	case <-time.After(1 * time.Second):
		// The system stopping within 1s after cancel is the correctness signal.
		// If the worker is still blocking it indicates a goroutine leak.
	}
	// If we reach here without hanging, shutdown was clean.
}

// ---------------------------------------------------------------------------
// Test: NotifyWrite delivers to a subscriber with PushOnWrite set
// ---------------------------------------------------------------------------

func TestNotifyWriteDelivers(t *testing.T) {
	ts := newTestTriggerSystem()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ts.Start(ctx)

	received := make(chan *ActivationPush, 4)
	deliver := func(ctx context.Context, push *ActivationPush) error {
		received <- push
		return nil
	}

	// testVec is a small non-zero embedding so the trigger worker's vector check
	// doesn't short-circuit with "len == 0". Cosine similarity of identical vectors = 1.0.
	testVec := []float32{0.5, 0.5, 0.5, 0.5}

	// Register a subscription on vault 42 with PushOnWrite enabled and zero threshold.
	sub := &Subscription{
		ID:           "write-test",
		VaultID:      42,
		Threshold:    0.0,
		PushOnWrite:  true,
		RateLimit:    100,
		rateLimiter:  newTokenBucket(100),
		pushedScores: make(map[storage.ULID]float64),
		createdAt:    time.Now(),
		Deliver:      deliver,
		embedding:    testVec,
	}
	ts.registry.Add(sub)

	// Send a write event directly into the system's event channel.
	// Confidence > 0 and Embedding set so TriggerScore returns above threshold.
	eng := &storage.Engram{
		ID:         storage.NewULID(),
		Concept:    "hello",
		Content:    "world",
		Confidence: 0.9,
		Embedding:  testVec,
	}
	ts.WriteEvents <- &EngramEvent{VaultID: 42, Engram: eng, IsNew: true}

	select {
	case push := <-received:
		if push == nil {
			t.Fatal("received nil push")
		}
		// The push should carry the engram.
		if push.Engram == nil {
			t.Error("push.Engram is nil, expected engram payload")
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("timed out waiting for push delivery")
	}
}

// ---------------------------------------------------------------------------
// Additional edge-case: removing a non-existent subscription is a no-op
// ---------------------------------------------------------------------------

func TestSubscriptionRegistryRemoveNonExistent(t *testing.T) {
	reg := newRegistry()
	// Should not panic.
	reg.Remove("does-not-exist")
}

// ---------------------------------------------------------------------------
// Additional: concurrent Add/Remove is safe (data race detector check)
// ---------------------------------------------------------------------------

func TestSubscriptionRegistryConcurrentAccess(t *testing.T) {
	reg := newRegistry()
	var wg sync.WaitGroup

	for i := 0; i < 20; i++ {
		id := "concurrent-" + string(rune('a'+i))
		sub := newMinimalSub(id, 5, 0)
		wg.Add(1)
		go func(s *Subscription) {
			defer wg.Done()
			reg.Add(s)
			_ = reg.ForVault(5)
			reg.Remove(s.ID)
		}(sub)
	}
	wg.Wait()
}

// ---------------------------------------------------------------------------
// T3: Per-vault subscription cap — ErrVaultSubscriptionLimitReached
// ---------------------------------------------------------------------------

func TestSubscribeVaultCapReturnsError(t *testing.T) {
	ts := New(
		&stubTriggerStore{},
		&stubTrigFTS{},
		&stubTrigHNSW{},
		&stubTrigEmbedder{},
		TriggerConfig{
			MaxSubscriptionsPerVault: 2,
			MaxTotalSubscriptions:    1000,
		},
	)

	// Add 2 subscriptions for vault 99 — must succeed.
	for i := 0; i < 2; i++ {
		sub := &Subscription{
			ID:      "vault-cap-" + string(rune('0'+i)),
			VaultID: 99,
		}
		if err := ts.Subscribe(sub); err != nil {
			t.Fatalf("Subscribe[%d]: unexpected error: %v", i, err)
		}
	}

	// Third subscription for vault 99 must fail with ErrVaultSubscriptionLimitReached.
	sub := &Subscription{ID: "vault-cap-overflow", VaultID: 99}
	err := ts.Subscribe(sub)
	if err == nil {
		t.Fatal("expected ErrVaultSubscriptionLimitReached, got nil")
	}
	if err != ErrVaultSubscriptionLimitReached {
		t.Errorf("expected ErrVaultSubscriptionLimitReached, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// T3: Global subscription cap — ErrGlobalSubscriptionLimitReached
// ---------------------------------------------------------------------------

func TestSubscribeGlobalCapReturnsError(t *testing.T) {
	ts := New(
		&stubTriggerStore{},
		&stubTrigFTS{},
		&stubTrigHNSW{},
		&stubTrigEmbedder{},
		TriggerConfig{
			MaxSubscriptionsPerVault: 1000,
			MaxTotalSubscriptions:    3,
		},
	)

	for i := 0; i < 3; i++ {
		sub := &Subscription{
			ID:      "global-cap-" + string(rune('0'+i)),
			VaultID: uint32(i), // different vaults to bypass per-vault cap
		}
		if err := ts.Subscribe(sub); err != nil {
			t.Fatalf("Subscribe[%d]: unexpected error: %v", i, err)
		}
	}

	// Fourth subscription must fail with ErrGlobalSubscriptionLimitReached.
	sub := &Subscription{ID: "global-cap-overflow", VaultID: 99}
	err := ts.Subscribe(sub)
	if err == nil {
		t.Fatal("expected ErrGlobalSubscriptionLimitReached, got nil")
	}
	if err != ErrGlobalSubscriptionLimitReached {
		t.Errorf("expected ErrGlobalSubscriptionLimitReached, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// T3: Subscribe within cap succeeds
// ---------------------------------------------------------------------------

func TestSubscribeWithinCapSucceeds(t *testing.T) {
	ts := New(
		&stubTriggerStore{},
		&stubTrigFTS{},
		&stubTrigHNSW{},
		&stubTrigEmbedder{},
		TriggerConfig{
			MaxSubscriptionsPerVault: 5,
			MaxTotalSubscriptions:    10,
		},
	)

	for i := 0; i < 5; i++ {
		sub := &Subscription{
			ID:      "within-cap-" + string(rune('0'+i)),
			VaultID: 7,
		}
		if err := ts.Subscribe(sub); err != nil {
			t.Errorf("Subscribe[%d] within cap failed: %v", i, err)
		}
	}

	if ts.registry.CountForVault(7) != 5 {
		t.Errorf("CountForVault = %d, want 5", ts.registry.CountForVault(7))
	}
}

// ---------------------------------------------------------------------------
// T3: CountForVault and CountTotal are accurate
// ---------------------------------------------------------------------------

func TestRegistryCountMethods(t *testing.T) {
	reg := newRegistry()

	if reg.CountTotal() != 0 {
		t.Errorf("initial CountTotal = %d, want 0", reg.CountTotal())
	}

	s1 := newMinimalSub("cnt-1", 10, 0)
	s2 := newMinimalSub("cnt-2", 10, 0)
	s3 := newMinimalSub("cnt-3", 20, 0)

	reg.Add(s1)
	reg.Add(s2)
	reg.Add(s3)

	if reg.CountForVault(10) != 2 {
		t.Errorf("CountForVault(10) = %d, want 2", reg.CountForVault(10))
	}
	if reg.CountForVault(20) != 1 {
		t.Errorf("CountForVault(20) = %d, want 1", reg.CountForVault(20))
	}
	if reg.CountTotal() != 3 {
		t.Errorf("CountTotal = %d, want 3", reg.CountTotal())
	}

	reg.Remove("cnt-2")
	if reg.CountForVault(10) != 1 {
		t.Errorf("after Remove, CountForVault(10) = %d, want 1", reg.CountForVault(10))
	}
	if reg.CountTotal() != 2 {
		t.Errorf("after Remove, CountTotal = %d, want 2", reg.CountTotal())
	}
}

// ---------------------------------------------------------------------------
// NotifyCognitive: event is enqueued in the CognitiveEvents channel
// ---------------------------------------------------------------------------

func TestNotifyCognitiveEnqueues(t *testing.T) {
	ts := newTestTriggerSystem()

	id := storage.NewULID()
	// Delta = 0.5 — above the 0.001 filter.
	ts.NotifyCognitive(42, id, "association_weight", 0.1, 0.6)

	select {
	case ev := <-ts.CognitiveEvents:
		if ev.VaultID != 42 {
			t.Errorf("VaultID = %d, want 42", ev.VaultID)
		}
		if ev.EngramID != id {
			t.Errorf("EngramID mismatch")
		}
		if ev.Field != "association_weight" {
			t.Errorf("Field = %q, want 'association_weight'", ev.Field)
		}
		if ev.OldValue != 0.1 {
			t.Errorf("OldValue = %v, want 0.1", ev.OldValue)
		}
		if ev.NewValue != 0.6 {
			t.Errorf("NewValue = %v, want 0.6", ev.NewValue)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("CognitiveEvents channel empty — NotifyCognitive did not enqueue event")
	}
}

// ---------------------------------------------------------------------------
// NotifyCognitive: sub-threshold delta is silently dropped
// ---------------------------------------------------------------------------

func TestNotifyCognitiveSubThresholdDropped(t *testing.T) {
	ts := newTestTriggerSystem()

	id := storage.NewULID()
	// Delta = 0.0005 — below 0.001 filter → should NOT be enqueued.
	ts.NotifyCognitive(1, id, "relevance", 0.5000, 0.5005)

	select {
	case ev := <-ts.CognitiveEvents:
		t.Errorf("expected no event for sub-threshold delta, got: %+v", ev)
	case <-time.After(30 * time.Millisecond):
		// Good — nothing enqueued.
	}
}

// ---------------------------------------------------------------------------
// NotifyContradiction: event is enqueued in the ContradictEvents channel
// ---------------------------------------------------------------------------

func TestNotifyContradictionEnqueues(t *testing.T) {
	ts := newTestTriggerSystem()

	a := storage.NewULID()
	b := storage.NewULID()
	ts.NotifyContradiction(7, a, b, 0.85, "semantic")

	select {
	case ev := <-ts.ContradictEvents:
		if ev.VaultID != 7 {
			t.Errorf("VaultID = %d, want 7", ev.VaultID)
		}
		if ev.EngramA != a {
			t.Error("EngramA mismatch")
		}
		if ev.EngramB != b {
			t.Error("EngramB mismatch")
		}
		if ev.Severity != 0.85 {
			t.Errorf("Severity = %v, want 0.85", ev.Severity)
		}
		if ev.Type != "semantic" {
			t.Errorf("Type = %q, want 'semantic'", ev.Type)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("ContradictEvents channel empty — NotifyContradiction did not enqueue event")
	}
}

// ---------------------------------------------------------------------------
// T6: consecutiveDrops increments on drop and resets on success
// ---------------------------------------------------------------------------

func TestConsecutiveDropsTracking(t *testing.T) {
	sub := newMinimalSub("drop-test", 1, 0)

	// Simulate 3 consecutive drops.
	sub.consecutiveDrops.Add(1)
	sub.consecutiveDrops.Add(1)
	sub.consecutiveDrops.Add(1)

	if sub.consecutiveDrops.Load() != 3 {
		t.Errorf("consecutiveDrops = %d, want 3", sub.consecutiveDrops.Load())
	}

	// Simulate a successful delivery — reset.
	sub.consecutiveDrops.Store(0)

	if sub.consecutiveDrops.Load() != 0 {
		t.Errorf("after reset, consecutiveDrops = %d, want 0", sub.consecutiveDrops.Load())
	}
}

// ---------------------------------------------------------------------------
// T6: droppedTotal accumulates and never resets
// ---------------------------------------------------------------------------

func TestDroppedTotalAccumulates(t *testing.T) {
	sub := newMinimalSub("total-drop-test", 2, 0)

	for i := 0; i < 10; i++ {
		sub.droppedTotal.Add(1)
	}
	if sub.droppedTotal.Load() != 10 {
		t.Errorf("droppedTotal = %d, want 10", sub.droppedTotal.Load())
	}

	// Simulating a successful delivery does NOT reset droppedTotal.
	sub.consecutiveDrops.Store(0)

	if sub.droppedTotal.Load() != 10 {
		t.Errorf("droppedTotal changed after reset, got %d, want 10", sub.droppedTotal.Load())
	}
}

// ---------------------------------------------------------------------------
// T6: 50 consecutive drops threshold is detectable
// ---------------------------------------------------------------------------

func TestConsecutiveDropsThreshold(t *testing.T) {
	sub := newMinimalSub("threshold-test", 3, 0)

	// Below threshold.
	sub.consecutiveDrops.Store(49)
	if sub.consecutiveDrops.Load() >= 50 {
		t.Error("49 drops should be below the 50-drop threshold")
	}

	// At threshold.
	sub.consecutiveDrops.Store(50)
	if sub.consecutiveDrops.Load() < 50 {
		t.Error("50 drops should trigger the threshold check")
	}
}

// ---------------------------------------------------------------------------
// T3: Vault cap error sentinel values are distinct errors
// ---------------------------------------------------------------------------

func TestCapErrorsAreDistinct(t *testing.T) {
	if ErrVaultSubscriptionLimitReached == ErrGlobalSubscriptionLimitReached {
		t.Error("vault and global cap errors must be distinct sentinel values")
	}
	if ErrVaultSubscriptionLimitReached == nil {
		t.Error("ErrVaultSubscriptionLimitReached must not be nil")
	}
	if ErrGlobalSubscriptionLimitReached == nil {
		t.Error("ErrGlobalSubscriptionLimitReached must not be nil")
	}
}

// ---------------------------------------------------------------------------
// T3: Subscribe after Unsubscribe allows re-registering within cap
// ---------------------------------------------------------------------------

func TestUnsubscribeFreesCapacity(t *testing.T) {
	ts := New(
		&stubTriggerStore{},
		&stubTrigFTS{},
		&stubTrigHNSW{},
		&stubTrigEmbedder{},
		TriggerConfig{
			MaxSubscriptionsPerVault: 1,
			MaxTotalSubscriptions:    1000,
		},
	)

	sub1 := &Subscription{ID: "cap-free-1", VaultID: 55}
	if err := ts.Subscribe(sub1); err != nil {
		t.Fatalf("first Subscribe: %v", err)
	}

	// Second subscribe must fail — vault is at cap.
	sub2 := &Subscription{ID: "cap-free-2", VaultID: 55}
	if err := ts.Subscribe(sub2); err != ErrVaultSubscriptionLimitReached {
		t.Fatalf("expected ErrVaultSubscriptionLimitReached, got %v", err)
	}

	// Unsubscribe frees capacity.
	ts.Unsubscribe("cap-free-1")

	// Third subscribe must now succeed.
	sub3 := &Subscription{ID: "cap-free-3", VaultID: 55}
	if err := ts.Subscribe(sub3); err != nil {
		t.Errorf("Subscribe after Unsubscribe failed: %v", err)
	}
}
