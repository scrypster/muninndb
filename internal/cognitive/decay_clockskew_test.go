package cognitive

import "testing"

// TestEbbinghausWithFloor_ClampsFutureAccess guards against clock skew: a
// LastAccess in the future yields a negative daysSinceAccess, which without a
// clamp makes exp(-neg/stability) > 1 — a retention above the [floor, 1] range
// the model promises. A future access must read as "just accessed" (1.0).
func TestEbbinghausWithFloor_ClampsFutureAccess(t *testing.T) {
	for _, days := range []float64{-0.001, -5, -3650} {
		got := EbbinghausWithFloor(days, 14, DefaultFloor)
		if got > 1.0 {
			t.Errorf("EbbinghausWithFloor(%v, 14, floor) = %v, want ≤ 1.0 (clock-skew clamp)", days, got)
		}
		if got != 1.0 {
			t.Errorf("EbbinghausWithFloor(%v, ...) = %v, want exactly 1.0 (future access = just accessed)", days, got)
		}
	}
}
