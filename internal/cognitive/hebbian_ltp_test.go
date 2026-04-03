package cognitive

import (
	"context"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// ltpMockStore extends mockHebbianStore with co-activation count tracking.
// ---------------------------------------------------------------------------

type ltpMockStore struct {
	mu       sync.Mutex
	weights  map[[32]byte]float32
	coActCts map[[32]byte]uint32 // co-activation counts per pair
	decayed  int
}

func newLTPMockStore() *ltpMockStore {
	return &ltpMockStore{
		weights:  make(map[[32]byte]float32),
		coActCts: make(map[[32]byte]uint32),
	}
}

func (m *ltpMockStore) UpdateAssocWeight(_ context.Context, _ [8]byte, src, dst [16]byte, w float32) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.weights[pairKeyBytes(src, dst)] = w
	return nil
}

func (m *ltpMockStore) GetAssocWeight(_ context.Context, _ [8]byte, src, dst [16]byte) (float32, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.weights[pairKeyBytes(src, dst)], nil
}

func (m *ltpMockStore) DecayAssocWeights(_ context.Context, _ [8]byte, _ float64, _ float32, _ float64) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.decayed++
	return 0, nil
}

func (m *ltpMockStore) UpdateAssocWeightBatch(_ context.Context, updates []AssocWeightUpdate) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, u := range updates {
		key := pairKeyBytes(u.Src, u.Dst)
		m.weights[key] = u.Weight
		m.coActCts[key] += u.CountDelta
	}
	return nil
}

func (m *ltpMockStore) getCoActCount(src, dst [16]byte) uint32 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.coActCts[pairKeyBytes(src, dst)]
}

func (m *ltpMockStore) getWeight(src, dst [16]byte) float32 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.weights[pairKeyBytes(src, dst)]
}

// ---------------------------------------------------------------------------
// Test (a): Co-activation counter increments on each Hebbian pass
// ---------------------------------------------------------------------------

func TestLTP_CoActivationCounterIncrements(t *testing.T) {
	store := newLTPMockStore()

	ltpCfg := &LTPConfig{
		Threshold:   5,
		WeightFloor: 0.3,
	}
	hw := NewHebbianWorkerWithLTP(store, nil, nil, ltpCfg)

	idA := [16]byte{0xA0}
	idB := [16]byte{0xB0}
	ws := [8]byte{0, 0, 0, 1}

	// Submit 3 co-activation events
	for i := 0; i < 3; i++ {
		hw.Submit(CoActivationEvent{
			WS: ws,
			At: time.Now(),
			Engrams: []CoActivatedEngram{
				{ID: idA, Score: 0.9},
				{ID: idB, Score: 0.8},
			},
		})
	}

	hw.Stop()

	// The canonical pair should have accumulated 3 co-activation deltas
	// Check both orderings since canonicalPair may swap
	countAB := store.getCoActCount(idA, idB)
	countBA := store.getCoActCount(idB, idA)
	totalCount := countAB + countBA
	if totalCount < 3 {
		t.Errorf("co-activation count: got %d, want >= 3", totalCount)
	}
}

// ---------------------------------------------------------------------------
// Test (b): Association becomes potentiated after threshold co-activations
// ---------------------------------------------------------------------------

func TestLTP_PotentiationAfterThreshold(t *testing.T) {
	store := newLTPMockStore()

	ltpCfg := &LTPConfig{
		Threshold:   3,    // low threshold for testing
		WeightFloor: 0.3,  // should be enforced once potentiated
	}
	hw := NewHebbianWorkerWithLTP(store, nil, nil, ltpCfg)

	idA := [16]byte{0xA1}
	idB := [16]byte{0xB1}
	ws := [8]byte{0, 0, 0, 2}

	// Submit enough events to exceed threshold
	for i := 0; i < 5; i++ {
		hw.Submit(CoActivationEvent{
			WS: ws,
			At: time.Now(),
			Engrams: []CoActivatedEngram{
				{ID: idA, Score: 0.9},
				{ID: idB, Score: 0.8},
			},
		})
	}

	hw.Stop()

	// After 5 co-activations (threshold=3), the pair should be potentiated.
	// The LTP tracker in the worker should reflect this.
	pair := canonicalPair(idA, idB)
	if !hw.IsPotentiated(ws, pair) {
		t.Error("expected association to be potentiated after exceeding LTP threshold")
	}
}

// ---------------------------------------------------------------------------
// Test (c): Potentiated associations respect weight floor
// ---------------------------------------------------------------------------

func TestLTP_PotentiatedWeightFloor(t *testing.T) {
	store := newLTPMockStore()

	ltpCfg := &LTPConfig{
		Threshold:   2,    // low threshold
		WeightFloor: 0.3,  // weight floor for potentiated associations
	}
	hw := NewHebbianWorkerWithLTP(store, nil, nil, ltpCfg)

	idA := [16]byte{0xA2}
	idB := [16]byte{0xB2}
	ws := [8]byte{0, 0, 0, 3}

	// Seed an initial low weight for the pair
	store.mu.Lock()
	store.weights[pairKeyBytes(idA, idB)] = 0.05 // very low weight
	store.mu.Unlock()

	// Submit enough events to exceed threshold and trigger potentiation
	for i := 0; i < 4; i++ {
		hw.Submit(CoActivationEvent{
			WS: ws,
			At: time.Now(),
			Engrams: []CoActivatedEngram{
				{ID: idA, Score: 0.1}, // low scores to keep delta small
				{ID: idB, Score: 0.1},
			},
		})
	}

	hw.Stop()

	// The weight should be at least the LTP weight floor for potentiated pairs
	weightAB := store.getWeight(idA, idB)
	weightBA := store.getWeight(idB, idA)
	weight := weightAB
	if weightBA > weight {
		weight = weightBA
	}

	if weight < ltpCfg.WeightFloor {
		t.Errorf("potentiated association weight %v is below LTP floor %v",
			weight, ltpCfg.WeightFloor)
	}
}

// ---------------------------------------------------------------------------
// Test (d): Counter persists across Hebbian passes (accumulated in store)
// ---------------------------------------------------------------------------

func TestLTP_CounterPersistsAcrossPasses(t *testing.T) {
	store := newLTPMockStore()

	ltpCfg := &LTPConfig{
		Threshold:   10,   // high threshold so we can observe accumulation
		WeightFloor: 0.3,
	}
	hw := NewHebbianWorkerWithLTP(store, nil, nil, ltpCfg)

	idA := [16]byte{0xA3}
	idB := [16]byte{0xB3}
	ws := [8]byte{0, 0, 0, 4}

	// Submit 2 events, flush, then 2 more
	for i := 0; i < 2; i++ {
		hw.Submit(CoActivationEvent{
			WS: ws,
			At: time.Now(),
			Engrams: []CoActivatedEngram{
				{ID: idA, Score: 0.9},
				{ID: idB, Score: 0.9},
			},
		})
	}

	// Stop flushes the batch
	hw.Stop()

	countAfterFirst := store.getCoActCount(idA, idB) + store.getCoActCount(idB, idA)

	// Create new worker (simulating persistence across restarts)
	hw2 := NewHebbianWorkerWithLTP(store, nil, nil, ltpCfg)

	for i := 0; i < 2; i++ {
		hw2.Submit(CoActivationEvent{
			WS: ws,
			At: time.Now(),
			Engrams: []CoActivatedEngram{
				{ID: idA, Score: 0.9},
				{ID: idB, Score: 0.9},
			},
		})
	}

	hw2.Stop()

	countAfterSecond := store.getCoActCount(idA, idB) + store.getCoActCount(idB, idA)

	// Count should have accumulated across both passes
	if countAfterSecond <= countAfterFirst {
		t.Errorf("co-activation count did not accumulate: first=%d, second=%d",
			countAfterFirst, countAfterSecond)
	}
	if countAfterSecond < 4 {
		t.Errorf("expected total count >= 4, got %d", countAfterSecond)
	}
}

// ---------------------------------------------------------------------------
// Test (e): Default LTP config does not change existing behavior
// ---------------------------------------------------------------------------

func TestLTP_DefaultConfigPreservesExistingBehavior(t *testing.T) {
	store := newLTPMockStore()

	// nil LTP config = no LTP behavior
	hw := NewHebbianWorkerWithLTP(store, nil, nil, nil)

	idA := [16]byte{0xA4}
	idB := [16]byte{0xB4}
	ws := [8]byte{0, 0, 0, 5}

	hw.Submit(CoActivationEvent{
		WS: ws,
		At: time.Now(),
		Engrams: []CoActivatedEngram{
			{ID: idA, Score: 0.9},
			{ID: idB, Score: 0.8},
		},
	})

	hw.Stop()

	// Weight should be set (standard Hebbian behavior works)
	weightAB := store.getWeight(idA, idB)
	weightBA := store.getWeight(idB, idA)
	weight := weightAB
	if weightBA > weight {
		weight = weightBA
	}

	if weight <= 0 {
		t.Error("expected positive weight from standard Hebbian pass with nil LTP config")
	}

	// No pair should be potentiated with nil config
	pair := canonicalPair(idA, idB)
	if hw.IsPotentiated(ws, pair) {
		t.Error("no pair should be potentiated with nil LTP config")
	}
}

// ---------------------------------------------------------------------------
// Test (f): NewHebbianWorker (old constructor) still works unchanged
// ---------------------------------------------------------------------------

func TestLTP_OldConstructorUnchanged(t *testing.T) {
	store := newLTPMockStore()
	hw := NewHebbianWorker(store)

	idA := [16]byte{0xA5}
	idB := [16]byte{0xB5}

	hw.Submit(CoActivationEvent{
		WS: [8]byte{0, 0, 0, 6},
		At: time.Now(),
		Engrams: []CoActivatedEngram{
			{ID: idA, Score: 0.9},
			{ID: idB, Score: 0.8},
		},
	})

	hw.Stop()

	// Weight should be set (old constructor still works)
	weightAB := store.getWeight(idA, idB)
	weightBA := store.getWeight(idB, idA)
	weight := weightAB
	if weightBA > weight {
		weight = weightBA
	}

	if weight <= 0 {
		t.Error("expected positive weight from old NewHebbianWorker constructor")
	}
}

// ---------------------------------------------------------------------------
// Test (g): Event-level LTP config wires potentiation through a plain worker
// ---------------------------------------------------------------------------
// This integration test verifies the full wiring path: a HebbianWorker created
// WITHOUT worker-level LTP config receives events WITH per-vault LTP config
// (as would happen when the engine builds LTP config from ResolvedPlasticity).

func TestLTP_EventLevelConfig_Integration(t *testing.T) {
	store := newLTPMockStore()

	// Create worker with NO worker-level LTP config (like server.go does).
	hw := NewHebbianWorker(store)

	idA := [16]byte{0xA6}
	idB := [16]byte{0xB6}
	ws := [8]byte{0, 0, 0, 7}

	// Seed a low weight so we can verify the floor is enforced.
	store.mu.Lock()
	store.weights[pairKeyBytes(idA, idB)] = 0.02
	store.mu.Unlock()

	// Event-level LTP config (as the engine would build from resolved plasticity).
	eventLTP := &LTPConfig{
		Threshold:   3,
		WeightFloor: 0.25,
	}

	// Submit enough events (with event-level LTP) to exceed threshold.
	for i := 0; i < 5; i++ {
		hw.Submit(CoActivationEvent{
			WS: ws,
			At: time.Now(),
			Engrams: []CoActivatedEngram{
				{ID: idA, Score: 0.1}, // low scores to keep Hebbian delta small
				{ID: idB, Score: 0.1},
			},
			LTP: eventLTP,
		})
	}

	hw.Stop()

	// Verify potentiation occurred via event-level config.
	pair := canonicalPair(idA, idB)
	if !hw.IsPotentiated(ws, pair) {
		t.Fatal("expected association to be potentiated via event-level LTP config")
	}

	// Verify weight floor is enforced.
	weightAB := store.getWeight(idA, idB)
	weightBA := store.getWeight(idB, idA)
	weight := weightAB
	if weightBA > weight {
		weight = weightBA
	}

	if weight < eventLTP.WeightFloor {
		t.Errorf("potentiated weight %v is below event-level LTP floor %v",
			weight, eventLTP.WeightFloor)
	}
}

// ---------------------------------------------------------------------------
// Test (h): Event-level LTP config is per-vault (different vaults, different configs)
// ---------------------------------------------------------------------------

func TestLTP_EventLevelConfig_PerVault(t *testing.T) {
	store := newLTPMockStore()
	hw := NewHebbianWorker(store)

	idA := [16]byte{0xA7}
	idB := [16]byte{0xB7}
	wsVault1 := [8]byte{0, 0, 0, 8}
	wsVault2 := [8]byte{0, 0, 0, 9}

	// Vault 1: LTP enabled with threshold 2
	ltp1 := &LTPConfig{Threshold: 2, WeightFloor: 0.3}
	// Vault 2: LTP disabled (nil)

	// Submit 3 events for vault 1 (exceeds threshold of 2)
	for i := 0; i < 3; i++ {
		hw.Submit(CoActivationEvent{
			WS:      wsVault1,
			At:      time.Now(),
			Engrams: []CoActivatedEngram{{ID: idA, Score: 0.9}, {ID: idB, Score: 0.9}},
			LTP:     ltp1,
		})
	}

	// Submit 3 events for vault 2 (no LTP)
	for i := 0; i < 3; i++ {
		hw.Submit(CoActivationEvent{
			WS:      wsVault2,
			At:      time.Now(),
			Engrams: []CoActivatedEngram{{ID: idA, Score: 0.9}, {ID: idB, Score: 0.9}},
			LTP:     nil,
		})
	}

	hw.Stop()

	pair := canonicalPair(idA, idB)

	// Vault 1 should be potentiated
	if !hw.IsPotentiated(wsVault1, pair) {
		t.Error("vault 1: expected association to be potentiated")
	}

	// Vault 2 should NOT be potentiated (no LTP config)
	if hw.IsPotentiated(wsVault2, pair) {
		t.Error("vault 2: expected association NOT to be potentiated (LTP disabled)")
	}
}
