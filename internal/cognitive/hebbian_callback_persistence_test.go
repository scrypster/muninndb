package cognitive

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// errPersistFailed is the synthetic persistence failure these tests inject.
var errPersistFailed = errors.New("injected persistence failure")

// partialBatchError is the shape storage.AssocBatchSkipError presents to this
// package: an error that also reports which updates did not land. Declared
// locally and structurally so cognitive keeps no dependency on the concrete
// storage type — the production code matches on the SkippedUpdates() method,
// not on the type.
type partialBatchError struct {
	skipped []int
}

func (e *partialBatchError) SkippedUpdates() []int { return e.skipped }
func (e *partialBatchError) Error() string {
	return fmt.Sprintf("skipped updates %v: %v", e.skipped, errPersistFailed)
}
func (e *partialBatchError) Unwrap() error { return errPersistFailed }

// selectiveHebbianStore applies only the updates whose Src is not in skipSrc,
// and reports the rest as skipped — the storage behaviour under a permanently
// unreadable pair, without needing a Pebble instance.
type selectiveHebbianStore struct {
	mu      sync.Mutex
	skipSrc map[[16]byte]bool
	failAll bool

	applied map[[16]byte]float32
}

func newSelectiveHebbianStore() *selectiveHebbianStore {
	return &selectiveHebbianStore{
		skipSrc: map[[16]byte]bool{},
		applied: map[[16]byte]float32{},
	}
}

func (s *selectiveHebbianStore) GetAssocWeight(context.Context, [8]byte, [16]byte, [16]byte) (float32, error) {
	return 0, nil
}

func (s *selectiveHebbianStore) UpdateAssocWeight(context.Context, [8]byte, [16]byte, [16]byte, float32) error {
	return nil
}

func (s *selectiveHebbianStore) DecayAssocWeights(context.Context, [8]byte, time.Duration, float32, float64) (int, error) {
	return 0, nil
}

func (s *selectiveHebbianStore) UpdateAssocWeightBatch(_ context.Context, updates []AssocWeightUpdate) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failAll {
		return errPersistFailed
	}
	var skipped []int
	for i, u := range updates {
		if s.skipSrc[u.Src] {
			skipped = append(skipped, i)
			continue
		}
		s.applied[u.Src] = u.Weight
	}
	if len(skipped) > 0 {
		return &partialBatchError{skipped: skipped}
	}
	return nil
}

// twoPairBatch builds a batch producing exactly two aggregated pairs with
// distinct, known sources: (idA,idB) and (idC,idD).
func twoPairBatch(ws [8]byte, idA, idB, idC, idD [16]byte) []CoActivationEvent {
	now := time.Now()
	return []CoActivationEvent{
		{WS: ws, At: now, Engrams: []CoActivatedEngram{{ID: idA, Score: 0.8}, {ID: idB, Score: 0.8}}},
		{WS: ws, At: now, Engrams: []CoActivatedEngram{{ID: idC, Score: 0.8}, {ID: idD, Score: 0.8}}},
	}
}

// TestProcessBatch_CallbacksOnlyForPersistedUpdates pins that OnWeightUpdate
// describes what is ON DISK.
//
// In production OnWeightUpdate is NotifyCognitive (engine.go), which feeds the
// trigger system. Firing it for a weight transition that was never persisted
// tells the trigger system about a change to memory that memory does not have —
// the same silently-wrong class the read fix exists to remove, one layer up.
// Before this fix the callback loop ran unconditionally after the persist error
// was merely logged.
func TestProcessBatch_CallbacksOnlyForPersistedUpdates(t *testing.T) {
	store := newSelectiveHebbianStore()

	ws := [8]byte{9, 9, 9, 9, 9, 9, 9, 9}
	idA := [16]byte{1}
	idB := [16]byte{2}
	idC := [16]byte{3}
	idD := [16]byte{4}

	// canonicalPair orders the pair, so the callback's id is the lower of the
	// two engram IDs: idA for the first pair, idC for the second.
	store.skipSrc[idC] = true

	var mu sync.Mutex
	fired := map[[16]byte]int{}
	// The callback is passed to the constructor, not assigned afterwards: the
	// worker starts its goroutine in the constructor, so a later assignment
	// would be a data race (see NewHebbianWorkerWithDB).
	hw := NewHebbianWorkerWithDB(store, nil, func(_ [8]byte, id [16]byte, _ string, _, _ float64) {
		mu.Lock()
		defer mu.Unlock()
		fired[id]++
	})
	defer hw.Stop()

	err := hw.processBatch(context.Background(), twoPairBatch(ws, idA, idB, idC, idD))

	mu.Lock()
	defer mu.Unlock()

	if fired[idC] != 0 {
		t.Errorf("OnWeightUpdate fired %d time(s) for the pair whose update was SKIPPED — "+
			"the trigger system was told about a weight transition that was never persisted", fired[idC])
	}
	if fired[idA] != 1 {
		t.Errorf("OnWeightUpdate fired %d time(s) for the pair that WAS persisted, want 1 — "+
			"suppressing the skipped callback must not suppress the applied ones", fired[idA])
	}
	if store.applied[idA] == 0 {
		t.Error("the readable pair's update was not applied by the store double — test setup is wrong")
	}
	if err == nil {
		t.Error("processBatch: want the partial-persistence failure returned so Worker.Stats().Errors " +
			"counts it, got nil")
	}
}

// TestProcessBatch_NoCallbacksWhenTheWholeBatchIsDiscarded is the other end of
// the same property: when nothing persisted, nothing is announced.
func TestProcessBatch_NoCallbacksWhenTheWholeBatchIsDiscarded(t *testing.T) {
	store := newSelectiveHebbianStore()
	store.failAll = true

	ws := [8]byte{7, 7, 7, 7, 7, 7, 7, 7}
	idA := [16]byte{1}
	idB := [16]byte{2}
	idC := [16]byte{3}
	idD := [16]byte{4}

	var mu sync.Mutex
	var fields []string
	hw := NewHebbianWorkerWithDB(store, nil, func(_ [8]byte, _ [16]byte, field string, _, _ float64) {
		mu.Lock()
		defer mu.Unlock()
		fields = append(fields, field)
	})
	defer hw.Stop()

	err := hw.processBatch(context.Background(), twoPairBatch(ws, idA, idB, idC, idD))

	mu.Lock()
	defer mu.Unlock()
	if len(fields) != 0 {
		t.Errorf("OnWeightUpdate fired %d time(s) despite the batch being discarded: %v", len(fields), fields)
	}
	if err == nil {
		t.Error("processBatch: want the persistence failure returned, got nil — a batch that lost every " +
			"update is not a clean batch, and Worker.Stats().Errors is the only place that shows it")
	}
	if !errors.Is(err, errPersistFailed) {
		t.Errorf("processBatch: want the injected failure wrapped, got %v", err)
	}
}
