package cognitive

import (
	"context"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// CoActivationEvent.At must reach the association write.
//
// The field has existed on the struct since the worker was written and is set
// by the engine on every submission — and `processBatch` never referenced it.
// The stamp was therefore always the storage layer's `time.Now()`.
//
// Two properties are asserted, not one:
//   - the event's time is carried through, and
//   - when a batch aggregates several events for the same pair, the LATEST time
//     wins REGARDLESS OF ORDER. Order-independence matters because the whole
//     replay argument rests on `processBatch` being batch- and order-invariant
//     (the weight update is log-space multiplicative, so exponents add); a
//     last-write-wins stamp would have quietly broken that property in the one
//     field that decides how much forgetting gets replayed.
//
// PRIVACY: synthetic IDs only.
// ---------------------------------------------------------------------------

// recordingHebbianStore captures the batch `processBatch` submits.
type recordingHebbianStore struct {
	mu      sync.Mutex
	weights map[[32]byte]float32
	batches [][]AssocWeightUpdate
}

func newRecordingHebbianStore() *recordingHebbianStore {
	return &recordingHebbianStore{weights: make(map[[32]byte]float32)}
}

func (m *recordingHebbianStore) UpdateAssocWeight(_ context.Context, _ [8]byte, src, dst [16]byte, w float32) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.weights[pairKeyBytes(src, dst)] = w
	return nil
}

func (m *recordingHebbianStore) GetAssocWeight(_ context.Context, _ [8]byte, src, dst [16]byte) (float32, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.weights[pairKeyBytes(src, dst)], nil
}

func (m *recordingHebbianStore) DecayAssocWeights(_ context.Context, _ [8]byte, _ time.Duration, _ float32, _ float64) (int, error) {
	return 0, nil
}

func (m *recordingHebbianStore) UpdateAssocWeightBatch(_ context.Context, updates []AssocWeightUpdate) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := make([]AssocWeightUpdate, len(updates))
	copy(cp, updates)
	m.batches = append(m.batches, cp)
	for _, u := range updates {
		m.weights[pairKeyBytes(u.Src, u.Dst)] = u.Weight
	}
	return nil
}

func (m *recordingHebbianStore) onlyUpdate(t *testing.T) AssocWeightUpdate {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.batches) != 1 || len(m.batches[0]) != 1 {
		t.Fatalf("want exactly one update in one batch, got %v", m.batches)
	}
	return m.batches[0][0]
}

func coActEvent(ws [8]byte, at time.Time) CoActivationEvent {
	return CoActivationEvent{
		WS: ws,
		At: at,
		Engrams: []CoActivatedEngram{
			{ID: [16]byte{0xA1}, Score: 0.9},
			{ID: [16]byte{0xB2}, Score: 0.8},
		},
	}
}

func TestProcessBatch_CarriesCoActivationEventTime(t *testing.T) {
	store := newRecordingHebbianStore()
	hw := NewHebbianWorker(store)
	t.Cleanup(hw.Stop)

	ws := [8]byte{9, 9, 9, 9, 9, 9, 9, 9}
	at := time.Now().Add(-30 * 24 * time.Hour).Truncate(time.Second)

	if err := hw.processBatch(context.Background(), []CoActivationEvent{coActEvent(ws, at)}); err != nil {
		t.Fatalf("processBatch: %v", err)
	}

	got := store.onlyUpdate(t).LastActivatedAt
	if want := int32(at.Unix()); got != want {
		t.Errorf("LastActivatedAt = %d, want %d — CoActivationEvent.At must reach "+
			"the association write, or a replay cannot replay the forgetting", got, want)
	}
}

func TestProcessBatch_LatestEventTimeWinsRegardlessOfOrder(t *testing.T) {
	ws := [8]byte{9, 9, 9, 9, 9, 9, 9, 9}
	older := time.Now().Add(-60 * 24 * time.Hour).Truncate(time.Second)
	newer := time.Now().Add(-10 * 24 * time.Hour).Truncate(time.Second)

	for _, tc := range []struct {
		name  string
		batch []CoActivationEvent
	}{
		{"ascending", []CoActivationEvent{coActEvent(ws, older), coActEvent(ws, newer)}},
		{"descending", []CoActivationEvent{coActEvent(ws, newer), coActEvent(ws, older)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newRecordingHebbianStore()
			hw := NewHebbianWorker(store)
			t.Cleanup(hw.Stop)

			if err := hw.processBatch(context.Background(), tc.batch); err != nil {
				t.Fatalf("processBatch: %v", err)
			}
			got := store.onlyUpdate(t).LastActivatedAt
			if want := int32(newer.Unix()); got != want {
				t.Errorf("LastActivatedAt = %d, want %d — the aggregation must be "+
					"order-independent, like the weight update it accompanies", got, want)
			}
		})
	}
}

// TestProcessBatch_ZeroEventTimeLeavesStampToStorage is the identity control:
// an event with no time (a direct construction, not the engine's) must leave
// the stamp at 0 so storage still writes time.Now().
func TestProcessBatch_ZeroEventTimeLeavesStampToStorage(t *testing.T) {
	store := newRecordingHebbianStore()
	hw := NewHebbianWorker(store)
	t.Cleanup(hw.Stop)

	ws := [8]byte{9, 9, 9, 9, 9, 9, 9, 9}
	if err := hw.processBatch(context.Background(), []CoActivationEvent{coActEvent(ws, time.Time{})}); err != nil {
		t.Fatalf("processBatch: %v", err)
	}
	if got := store.onlyUpdate(t).LastActivatedAt; got != 0 {
		t.Errorf("LastActivatedAt = %d, want 0 for a zero-time event", got)
	}
}
