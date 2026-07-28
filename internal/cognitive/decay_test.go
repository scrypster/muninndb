package cognitive

import (
	"context"
	"math"
	"testing"
	"time"
)

// TestDecayCandidateHasWS verifies that DecayCandidate has a WS field of type [8]byte.
func TestDecayCandidateHasWS(t *testing.T) {
	candidate := DecayCandidate{
		WS:          [8]byte{1, 2, 3, 4, 5, 6, 7, 8},
		ID:          [16]byte{1},
		LastAccess:  time.Now(),
		AccessCount: 5,
		Stability:   14.0,
	}
	if candidate.WS != [8]byte{1, 2, 3, 4, 5, 6, 7, 8} {
		t.Errorf("WS field not set correctly: got %v", candidate.WS)
	}
}

// TestEbbinghausWithFloor verifies the Ebbinghaus forgetting curve with a floor.
func TestEbbinghausWithFloor(t *testing.T) {
	cases := []struct {
		days      float64
		stability float64
		floor     float64
		wantMin   float64
		wantMax   float64
	}{
		// At t=0 retention must be 1.0
		{0, 14.0, 0.05, 0.999, 1.001},
		// At t=stability, retention ≈ 1/e ≈ 0.368
		{14.0, 14.0, 0.05, 0.36, 0.38},
		// Floor must kick in when retention is very low
		{1000, 14.0, 0.05, 0.05, 0.05 + 1e-9},
		// Zero stability falls back to DefaultStability
		{DefaultStability, 0, 0.05, 0.36, 0.38},
	}
	for _, tt := range cases {
		got := EbbinghausWithFloor(tt.days, tt.stability, tt.floor)
		if got < tt.wantMin || got > tt.wantMax {
			t.Errorf("EbbinghausWithFloor(days=%v, stability=%v, floor=%v) = %v, want [%v, %v]",
				tt.days, tt.stability, tt.floor, got, tt.wantMin, tt.wantMax)
		}
	}
}

// TestComputeStabilityMonotonicallyGrows verifies that stability grows with access count.
func TestComputeStabilityMonotonicallyGrows(t *testing.T) {
	prev := 0.0
	for _, n := range []uint32{1, 2, 5, 10, 20, 50, 100} {
		s := ComputeStability(n, 7.0)
		if s <= prev && n > 1 {
			t.Errorf("stability did not grow: count=%d s=%v prev=%v", n, s, prev)
		}
		if s > MaxStability {
			t.Errorf("stability exceeded MaxStability at count=%d: %v > %v", n, s, MaxStability)
		}
		prev = s
	}
}

// TestComputeStabilityCapsAtMax verifies that stability never exceeds MaxStability.
func TestComputeStabilityCapsAtMax(t *testing.T) {
	s := ComputeStability(100000, 30.0)
	if s > MaxStability {
		t.Errorf("stability %v exceeds MaxStability %v", s, MaxStability)
	}
	if s < DefaultStability {
		t.Errorf("stability %v below DefaultStability %v", s, DefaultStability)
	}
}

// TestDecayWorkerProcessBatch verifies that processBatch passes the vault prefix (ws)
// to the store and computes Ebbinghaus decay correctly.
func TestDecayWorkerProcessBatch(t *testing.T) {
	capturedWS := [8]byte{}
	capturedID := [16]byte{}
	capturedRelevance := float32(0)

	ws := [8]byte{0xAA, 0xBB, 0xCC, 0xDD, 0, 0, 0, 0}
	id := [16]byte{1, 2, 3, 4}

	store := &stubDecayStore{
		onUpdateRelevance: func(ctx context.Context, gotWS [8]byte, gotID [16]byte, rel, stab float32) error {
			capturedWS = gotWS
			capturedID = gotID
			capturedRelevance = rel
			return nil
		},
	}

	dw := NewDecayWorker(store)
	ctx := context.Background()

	batch := []DecayCandidate{{
		WS:          ws,
		ID:          id,
		LastAccess:  time.Now().Add(-24 * time.Hour),
		AccessCount: 5,
		Stability:   DefaultStability,
	}}
	if err := dw.processBatch(ctx, batch); err != nil {
		t.Fatalf("processBatch: %v", err)
	}

	if capturedWS != ws {
		t.Errorf("ws not passed to store: got %v, want %v", capturedWS, ws)
	}
	if capturedID != id {
		t.Errorf("id not passed to store: got %v, want %v", capturedID, id)
	}

	// After 1 day with 14-day stability, Ebbinghaus gives e^(-1/14) ≈ 0.931
	expected := EbbinghausWithFloor(1.0, DefaultStability, DefaultFloor)
	if math.Abs(float64(capturedRelevance)-expected) > 0.01 {
		t.Errorf("relevance: got %v, want ≈ %v", capturedRelevance, expected)
	}
}

// stubDecayStore is a test double for DecayStore.
type stubDecayStore struct {
	onUpdateRelevance func(ctx context.Context, ws [8]byte, id [16]byte, rel, stab float32) error
}

func (s *stubDecayStore) GetMetadataBatch(_ context.Context, _ [8]byte, ids [][16]byte) ([]DecayMeta, error) {
	result := make([]DecayMeta, len(ids))
	for i, id := range ids {
		result[i] = DecayMeta{ID: id, LastAccess: time.Now().Add(-24 * time.Hour), Stability: DefaultStability}
	}
	return result, nil
}

func (s *stubDecayStore) UpdateRelevance(ctx context.Context, ws [8]byte, id [16]byte, rel, stab float32) error {
	if s.onUpdateRelevance != nil {
		return s.onUpdateRelevance(ctx, ws, id, rel, stab)
	}
	return nil
}
