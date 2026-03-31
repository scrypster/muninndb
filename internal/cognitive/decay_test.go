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

// --- Hybrid decay model tests ---

// TestHybridRetention_ExponentialModelMatchesCurrent verifies that "exponential"
// model produces identical results to the original EbbinghausWithFloor.
func TestHybridRetention_ExponentialModelMatchesCurrent(t *testing.T) {
	stability := 14.0
	floor := DefaultFloor
	for _, days := range []float64{0, 0.5, 1, 3, 7, 14, 30, 100, 1000} {
		want := EbbinghausWithFloor(days, stability, floor)
		got := HybridRetention(days, stability, floor, "exponential", 0.5, 3.0)
		if math.Abs(got-want) > 1e-12 {
			t.Errorf("exponential model at days=%v: got %v, want %v", days, got, want)
		}
	}
}

// TestHybridRetention_HybridUsesExponentialBeforeTransition verifies that
// the hybrid model matches pure exponential for t < TransitionDays.
func TestHybridRetention_HybridUsesExponentialBeforeTransition(t *testing.T) {
	stability := 14.0
	floor := DefaultFloor
	transitionDays := 3.0
	for _, days := range []float64{0, 0.1, 0.5, 1.0, 2.0, 2.99} {
		want := EbbinghausWithFloor(days, stability, floor)
		got := HybridRetention(days, stability, floor, "hybrid", 0.5, transitionDays)
		if math.Abs(got-want) > 1e-12 {
			t.Errorf("hybrid model before transition at days=%v: got %v, want %v", days, got, want)
		}
	}
}

// TestHybridRetention_HybridUsesPowerLawAfterTransition verifies that
// the hybrid model uses power-law decay for t >= TransitionDays.
func TestHybridRetention_HybridUsesPowerLawAfterTransition(t *testing.T) {
	stability := 14.0
	floor := DefaultFloor
	transitionDays := 3.0
	powerLawExponent := 0.5

	rTransition := math.Exp(-transitionDays / stability)

	for _, days := range []float64{3.0, 5.0, 10.0, 30.0, 100.0} {
		// Expected: R_transition * (t / transitionDays)^(-powerLawExponent)
		expectedPowerLaw := rTransition * math.Pow(days/transitionDays, -powerLawExponent)
		if expectedPowerLaw < floor {
			expectedPowerLaw = floor
		}
		got := HybridRetention(days, stability, floor, "hybrid", powerLawExponent, transitionDays)
		if math.Abs(got-expectedPowerLaw) > 1e-12 {
			t.Errorf("hybrid model power-law at days=%v: got %v, want %v", days, got, expectedPowerLaw)
		}
	}
}

// TestHybridRetention_ContinuityAtTransition verifies there is no discontinuous
// jump at the transition point between exponential and power-law phases.
func TestHybridRetention_ContinuityAtTransition(t *testing.T) {
	stability := 14.0
	floor := DefaultFloor
	transitionDays := 3.0

	// Value just before transition
	justBefore := HybridRetention(transitionDays-1e-9, stability, floor, "hybrid", 0.5, transitionDays)
	// Value exactly at transition
	atTransition := HybridRetention(transitionDays, stability, floor, "hybrid", 0.5, transitionDays)
	// Value just after transition
	justAfter := HybridRetention(transitionDays+1e-9, stability, floor, "hybrid", 0.5, transitionDays)

	// The jump from just-before to at-transition should be negligible
	if math.Abs(justBefore-atTransition) > 1e-6 {
		t.Errorf("discontinuity at transition: justBefore=%v, atTransition=%v, delta=%v",
			justBefore, atTransition, math.Abs(justBefore-atTransition))
	}
	// The jump from at-transition to just-after should also be negligible
	if math.Abs(atTransition-justAfter) > 1e-6 {
		t.Errorf("discontinuity after transition: atTransition=%v, justAfter=%v, delta=%v",
			atTransition, justAfter, math.Abs(atTransition-justAfter))
	}
}

// TestHybridRetention_FloorAppliesInPowerLaw verifies that the floor constraint
// is enforced even in the power-law phase for very large t values.
func TestHybridRetention_FloorAppliesInPowerLaw(t *testing.T) {
	stability := 14.0
	floor := DefaultFloor
	// Very large time where power-law would go below floor
	days := 1e6
	got := HybridRetention(days, stability, floor, "hybrid", 0.5, 3.0)
	if got < floor {
		t.Errorf("hybrid retention below floor: got %v, want >= %v", got, floor)
	}
	if math.Abs(got-floor) > 1e-9 {
		t.Errorf("hybrid retention at extreme t should equal floor: got %v, want %v", got, floor)
	}
}

// TestHybridRetention_PowerLawRetainsMoreThanExponential verifies that the
// power-law tail retains more than pure exponential for large t values.
// This is the core motivation for the hybrid model.
//
// Note: near the transition point, the power-law may still be below exponential
// (the crossover depends on stability and exponent). The advantage of the heavy
// tail emerges for sufficiently large t. With stability=14 and transition=3,
// the crossover is around t~23 days; beyond that, power-law retains more.
func TestHybridRetention_PowerLawRetainsMoreThanExponential(t *testing.T) {
	stability := 14.0
	floor := DefaultFloor

	// At large t values, the power-law heavy tail preserves more than exponential.
	for _, days := range []float64{30.0, 60.0, 100.0, 365.0} {
		expo := HybridRetention(days, stability, floor, "exponential", 0.5, 3.0)
		hybrid := HybridRetention(days, stability, floor, "hybrid", 0.5, 3.0)
		if hybrid < expo {
			t.Errorf("at days=%v: hybrid (%v) should be >= exponential (%v)", days, hybrid, expo)
		}
	}
}

// TestHybridRetention_UnknownModelFallsToExponential verifies that an unknown
// decay model name defaults to exponential behavior.
func TestHybridRetention_UnknownModelFallsToExponential(t *testing.T) {
	stability := 14.0
	floor := DefaultFloor
	days := 10.0
	want := EbbinghausWithFloor(days, stability, floor)
	got := HybridRetention(days, stability, floor, "unknown", 0.5, 3.0)
	if math.Abs(got-want) > 1e-12 {
		t.Errorf("unknown model should fall back to exponential: got %v, want %v", got, want)
	}
}

// TestHybridRetention_ZeroStabilityUsesDefault verifies that zero stability
// falls back to DefaultStability in the hybrid model.
func TestHybridRetention_ZeroStabilityUsesDefault(t *testing.T) {
	floor := DefaultFloor
	days := 5.0
	got := HybridRetention(days, 0, floor, "hybrid", 0.5, 3.0)
	want := HybridRetention(days, DefaultStability, floor, "hybrid", 0.5, 3.0)
	if math.Abs(got-want) > 1e-12 {
		t.Errorf("zero stability should use default: got %v, want %v", got, want)
	}
}
