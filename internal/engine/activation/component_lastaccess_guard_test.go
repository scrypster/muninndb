package activation

import (
	"math"
	"testing"
	"time"
)

// TestComputeComponents_ZeroOrPreY2KLastAccess pins the weighted_sum-path
// counterpart of computeACTR's long-standing "never accessed = just now" guard.
//
// computeComponents had NO year check at all: a zero (or 1754 sentinel)
// LastAccess produced daysSince ~= 740,000, which drives recency to 0 and pins
// decayFactor at its 0.05 floor. On a weighted_sum vault that scored an
// otherwise-perfect row at 0.42 against COG-6's 0.5 default threshold — a
// silently-EMPTY recall, the worst failure class in the project (#810).
//
// This is asserted by a DIRECT call, not end-to-end, on purpose: stopping clone
// from writing the sentinel (#810 part 1) and mapping it back to zero at the
// ERF boundary (part 2) both mask this guard's absence through the recall path.
// The guard defends data already at rest and any future writer.
func TestComputeComponents_ZeroOrPreY2KLastAccess(t *testing.T) {
	now := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
	// The value uint64(time.Time{}.UnixNano()) decodes back to, pre-fix.
	sentinel := time.Unix(0, time.Time{}.UnixNano())

	cases := []struct {
		name       string
		lastAccess time.Time
	}{
		{"zero time", time.Time{}},
		{"1754 sentinel (legacy cloned record)", sentinel},
		{"pre-Y2K", time.Date(1999, 12, 31, 23, 59, 59, 0, time.UTC)},
	}

	w := resolvedWeights{FullTextRelevance: 0.4, SemanticSimilarity: 0.6}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			eng := makeEngram(0, 30, 1, tc.lastAccess)
			sc := computeComponents(0, 1.0, 0, eng, 0, now, w)

			if math.Abs(sc.Recency-1.0) > 1e-9 {
				t.Errorf("Recency = %v, want 1.0 (a never-accessed engram is treated as just-accessed, like computeACTR does)", sc.Recency)
			}
			if math.Abs(sc.DecayFactor-1.0) > 1e-9 {
				t.Errorf("DecayFactor = %v, want 1.0 (0.05 is the clamp floor — the #810 signature)", sc.DecayFactor)
			}
		})
	}

	// Control: a genuinely old LastAccess must still decay. The guard must not
	// swallow real age.
	old := now.Add(-90 * 24 * time.Hour)
	sc := computeComponents(0, 1.0, 0, makeEngram(0, 30, 1, old), 0, now, w)
	if sc.Recency > 0.01 {
		t.Errorf("control: Recency = %v for a 90-day-old access, want it decayed", sc.Recency)
	}
	if sc.DecayFactor > 0.1 {
		t.Errorf("control: DecayFactor = %v for a 90-day-old access, want it decayed", sc.DecayFactor)
	}
}
