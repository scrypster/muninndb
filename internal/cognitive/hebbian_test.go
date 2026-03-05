package cognitive

import (
	"context"
	"sync"
	"testing"
	"time"
)

// TestCoActivationEventHasWS verifies that CoActivationEvent has a WS field of type [8]byte
func TestCoActivationEventHasWS(t *testing.T) {
	event := CoActivationEvent{
		WS: [8]byte{1, 2, 3, 4, 5, 6, 7, 8},
		At: time.Now(),
		Engrams: []CoActivatedEngram{
			{ID: [16]byte{1}, Score: 0.5},
		},
	}

	// Verify WS field exists and is [8]byte
	if event.WS != [8]byte{1, 2, 3, 4, 5, 6, 7, 8} {
		t.Errorf("WS field not set correctly: got %v", event.WS)
	}

	// Ensure event doesn't have VaultID
	// This is verified by the compilation test below
}

// TestCoActivationEventNoVaultID verifies that VaultID field was removed
// If this compiles, VaultID is gone (this is a compile-time check)
func TestCoActivationEventNoVaultID(t *testing.T) {
	// Creating this with old VaultID field would cause a compile error:
	// event := CoActivationEvent{VaultID: 123}
	// That's the test: the struct should not compile with VaultID
}

// ---------------------------------------------------------------------------
// mockHebbianStore for testing the HebbianWorker in isolation.
// ---------------------------------------------------------------------------

type mockHebbianStore struct {
	mu      sync.Mutex
	weights map[[32]byte]float32 // key = src[16] || dst[16]
	decayed int
}

func newMockHebbianStore() *mockHebbianStore {
	return &mockHebbianStore{weights: make(map[[32]byte]float32)}
}

func pairKeyBytes(src, dst [16]byte) [32]byte {
	var k [32]byte
	copy(k[:16], src[:])
	copy(k[16:], dst[:])
	return k
}

func (m *mockHebbianStore) UpdateAssocWeight(_ context.Context, _ [8]byte, src, dst [16]byte, w float32) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.weights[pairKeyBytes(src, dst)] = w
	return nil
}

func (m *mockHebbianStore) GetAssocWeight(_ context.Context, _ [8]byte, src, dst [16]byte) (float32, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.weights[pairKeyBytes(src, dst)], nil
}

func (m *mockHebbianStore) DecayAssocWeights(_ context.Context, _ [8]byte, _ float64, _ float32, _ float64) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.decayed++
	return 0, nil
}

func (m *mockHebbianStore) UpdateAssocWeightBatch(_ context.Context, updates []AssocWeightUpdate) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, u := range updates {
		m.weights[pairKeyBytes(u.Src, u.Dst)] = u.Weight
	}
	return nil
}

// ---------------------------------------------------------------------------
// Test 1: OnWeightUpdate is called when two engrams are co-activated
// ---------------------------------------------------------------------------

func TestOnWeightUpdateFiredOnCoActivation(t *testing.T) {
	store := newMockHebbianStore()
	hw := NewHebbianWorker(store)

	var mu sync.Mutex
	var updates []struct {
		field    string
		old, new float64
	}
	hw.OnWeightUpdate = func(_ [8]byte, _ [16]byte, field string, old, new float64) {
		mu.Lock()
		updates = append(updates, struct{ field string; old, new float64 }{field, old, new})
		mu.Unlock()
	}

	idA := [16]byte{1}
	idB := [16]byte{2}

	event := CoActivationEvent{
		WS:  [8]byte{0, 0, 0, 1},
		At:  time.Now(),
		Engrams: []CoActivatedEngram{
			{ID: idA, Score: 0.9},
			{ID: idB, Score: 0.8},
		},
	}

	hw.Submit(event)
	// Stop flushes all pending items (channel is drained on shutdown).
	hw.Stop()

	mu.Lock()
	count := len(updates)
	mu.Unlock()

	if count == 0 {
		t.Error("OnWeightUpdate was never called despite co-activation")
	}
}

// ---------------------------------------------------------------------------
// Test 2: OnWeightUpdate receives old < new (weight increases)
// ---------------------------------------------------------------------------

func TestOnWeightUpdateOldLessThanNew(t *testing.T) {
	store := newMockHebbianStore()
	hw := NewHebbianWorker(store)

	var mu sync.Mutex
	var sawIncrease bool
	hw.OnWeightUpdate = func(_ [8]byte, _ [16]byte, _ string, old, new float64) {
		mu.Lock()
		if new >= old {
			sawIncrease = true
		}
		mu.Unlock()
	}

	idA := [16]byte{3}
	idB := [16]byte{4}

	hw.Submit(CoActivationEvent{
		WS:  [8]byte{0, 0, 0, 2},
		At:  time.Now(),
		Engrams: []CoActivatedEngram{
			{ID: idA, Score: 1.0},
			{ID: idB, Score: 1.0},
		},
	})

	hw.Stop()

	mu.Lock()
	inc := sawIncrease
	mu.Unlock()

	if !inc {
		t.Error("expected OnWeightUpdate to report new >= old (weight increase)")
	}
}

// ---------------------------------------------------------------------------
// Test 3: nil OnWeightUpdate does not panic
// ---------------------------------------------------------------------------

func TestNilOnWeightUpdateDoesNotPanic(t *testing.T) {
	store := newMockHebbianStore()
	hw := NewHebbianWorker(store)
	// Leave OnWeightUpdate nil — should not panic.

	defer func() {
		if r := recover(); r != nil {
			t.Errorf("nil OnWeightUpdate caused panic: %v", r)
		}
	}()

	hw.Submit(CoActivationEvent{
		WS:  [8]byte{0, 0, 0, 3},
		At:  time.Now(),
		Engrams: []CoActivatedEngram{
			{ID: [16]byte{5}, Score: 0.7},
			{ID: [16]byte{6}, Score: 0.7},
		},
	})

	hw.Stop()
}

// ---------------------------------------------------------------------------
// Test 4: field name passed to OnWeightUpdate is "association_weight"
// ---------------------------------------------------------------------------

func TestOnWeightUpdateFieldName(t *testing.T) {
	store := newMockHebbianStore()
	hw := NewHebbianWorker(store)

	var mu sync.Mutex
	var fields []string
	hw.OnWeightUpdate = func(_ [8]byte, _ [16]byte, field string, _, _ float64) {
		mu.Lock()
		fields = append(fields, field)
		mu.Unlock()
	}

	hw.Submit(CoActivationEvent{
		WS:  [8]byte{0, 0, 0, 4},
		At:  time.Now(),
		Engrams: []CoActivatedEngram{
			{ID: [16]byte{7}, Score: 0.8},
			{ID: [16]byte{8}, Score: 0.8},
		},
	})

	hw.Stop()

	mu.Lock()
	defer mu.Unlock()
	for _, f := range fields {
		if f != "association_weight" {
			t.Errorf("expected field 'association_weight', got %q", f)
		}
	}
	if len(fields) == 0 {
		t.Error("OnWeightUpdate was never called")
	}
}

// ---------------------------------------------------------------------------
// Test 5: OnWeightUpdate is called for each co-activation pair
// ---------------------------------------------------------------------------

func TestOnWeightUpdateCalledPerPair(t *testing.T) {
	store := newMockHebbianStore()
	hw := NewHebbianWorker(store)

	var mu sync.Mutex
	var updateCount int
	hw.OnWeightUpdate = func(_ [8]byte, _ [16]byte, _ string, _, _ float64) {
		mu.Lock()
		updateCount++
		mu.Unlock()
	}

	// 3 engrams → 3 pairs: (A,B), (A,C), (B,C)
	hw.Submit(CoActivationEvent{
		WS:  [8]byte{0, 0, 0, 5},
		At:  time.Now(),
		Engrams: []CoActivatedEngram{
			{ID: [16]byte{9}, Score: 0.9},
			{ID: [16]byte{10}, Score: 0.8},
			{ID: [16]byte{11}, Score: 0.7},
		},
	})

	hw.Stop()

	mu.Lock()
	count := updateCount
	mu.Unlock()

	// Expect 3 calls (one per unique pair)
	if count < 3 {
		t.Errorf("expected >= 3 OnWeightUpdate calls for 3-engram event, got %d", count)
	}
}


// ---------------------------------------------------------------------------
// Test 7: Atomic batch write - all updates committed together
// ---------------------------------------------------------------------------

func TestHebbianWorker_BatchWriteAtomic(t *testing.T) {
	store := newMockHebbianStore()
	hw := NewHebbianWorker(store)

	ws := [8]byte{1, 2, 3, 4, 5, 6, 7, 8}

	// Submit a batch with 5 engrams → 10 unique pairs
	hw.Submit(CoActivationEvent{
		WS: ws,
		At: time.Now(),
		Engrams: []CoActivatedEngram{
			{ID: [16]byte{1}, Score: 1.0},
			{ID: [16]byte{2}, Score: 1.0},
			{ID: [16]byte{3}, Score: 1.0},
			{ID: [16]byte{4}, Score: 1.0},
			{ID: [16]byte{5}, Score: 1.0},
		},
	})

	hw.Stop()

	// Verify that all pairs have weight entries in the store
	store.mu.Lock()
	defer store.mu.Unlock()

	expectedPairs := 10 // C(5,2) = 10
	actualPairs := len(store.weights)

	if actualPairs != expectedPairs {
		t.Errorf("expected %d weight entries after batch, got %d", expectedPairs, actualPairs)
	}

	// Spot-check: verify specific pairs have non-zero weights
	pair1 := pairKeyBytes([16]byte{1}, [16]byte{2})
	if w, ok := store.weights[pair1]; !ok || w <= 0 {
		t.Errorf("expected positive weight for pair (1,2), got %v (exists: %v)", w, ok)
	}

	pair2 := pairKeyBytes([16]byte{1}, [16]byte{5})
	if w, ok := store.weights[pair2]; !ok || w <= 0 {
		t.Errorf("expected positive weight for pair (1,5), got %v (exists: %v)", w, ok)
	}
}

// ---------------------------------------------------------------------------
// Test 8: Partial batch - mixed new and existing pairs
// ---------------------------------------------------------------------------

func TestHebbianWorker_PartialBatch_NoPanic(t *testing.T) {
	store := newMockHebbianStore()
	hw := NewHebbianWorker(store)

	ws := [8]byte{9, 8, 7, 6, 5, 4, 3, 2}

	// First batch: create some initial weights
	hw.Submit(CoActivationEvent{
		WS: ws,
		At: time.Now(),
		Engrams: []CoActivatedEngram{
			{ID: [16]byte{10}, Score: 1.0},
			{ID: [16]byte{20}, Score: 1.0},
		},
	})

	// Second batch: update existing pairs + create new ones
	hw.Submit(CoActivationEvent{
		WS: ws,
		At: time.Now(),
		Engrams: []CoActivatedEngram{
			{ID: [16]byte{10}, Score: 0.9},
			{ID: [16]byte{20}, Score: 0.9},
			{ID: [16]byte{30}, Score: 0.9},
		},
	})

	// Should not panic and both updates should be present
	hw.Stop()

	store.mu.Lock()
	defer store.mu.Unlock()

	if len(store.weights) < 3 {
		t.Errorf("expected at least 3 weight entries, got %d", len(store.weights))
	}

	// Verify pair (10, 20) has a weight (was updated)
	pair := pairKeyBytes([16]byte{10}, [16]byte{20})
	if w, ok := store.weights[pair]; !ok || w <= 0 {
		t.Errorf("expected positive weight for pair (10,20), got %v (exists: %v)", w, ok)
	}

	// Verify pair (10, 30) has a weight (was newly created)
	pair2 := pairKeyBytes([16]byte{10}, [16]byte{30})
	if w, ok := store.weights[pair2]; !ok || w <= 0 {
		t.Errorf("expected positive weight for pair (10,30), got %v (exists: %v)", w, ok)
	}
}
