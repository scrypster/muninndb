package activation

import (
	"testing"
	"time"
)

// TestComputeComponents_ClampsClockSkew guards the scoring recency and decay
// factors against a future LastAccess (NTP step / clock skew). A negative
// "days since" must not push Recency or DecayFactor above 1.0.
func TestComputeComponents_ClampsClockSkew(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	future := now.Add(48 * time.Hour) // LastAccess two days in the future

	eng := makeEngram(3, 30, 1.0, future)
	comp := computeComponents(0.5, 0.5, 0, eng, 0, now, defaultTestWeights())

	if comp.Recency > 1.0 {
		t.Errorf("Recency = %v, want ≤ 1.0 under future LastAccess (clock-skew clamp)", comp.Recency)
	}
	if comp.DecayFactor > 1.0 {
		t.Errorf("DecayFactor = %v, want ≤ 1.0 under future LastAccess (clock-skew clamp)", comp.DecayFactor)
	}
}
