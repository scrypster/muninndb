package engine

import "testing"

func TestLiftScore_NoSignalIsOne(t *testing.T) {
	// A frequency artifact: two entities both present on ~90% of days,
	// independently. Raw co-occurrence count is huge, but lift should sit at
	// ~1 (no more co-occurrence than base rates alone predict).
	T := 100
	nA, nB := 90, 90
	k := 81 // exactly what independence at 0.9*0.9 predicts
	lift := liftScore(k, T, nA, nB)
	if lift < 0.95 || lift > 1.05 {
		t.Fatalf("expected lift ~= 1.0 for independent-frequency pair, got %.4f", lift)
	}
}

func TestLiftScore_RealSignalExceedsOne(t *testing.T) {
	T := 365
	nA, nB := 90, 100
	k := 80 // planted-style: much higher than chance (chance ~= 90*100/365 ~= 24.7)
	lift := liftScore(k, T, nA, nB)
	if lift < 3.0 {
		t.Fatalf("expected lift > 3 for planted-style signal, got %.4f", lift)
	}
}

func TestLiftScore_ZeroMarginalIsZero(t *testing.T) {
	if got := liftScore(5, 100, 0, 10); got != 0 {
		t.Fatalf("expected 0 for zero marginal, got %v", got)
	}
}

func TestCircularShift_PreservesCount(t *testing.T) {
	presence := make([]bool, 30)
	for i := 0; i < 30; i += 3 {
		presence[i] = true
	}
	wantCount := 0
	for _, v := range presence {
		if v {
			wantCount++
		}
	}
	shifted := circularShift(presence, 7)
	gotCount := 0
	for _, v := range shifted {
		if v {
			gotCount++
		}
	}
	if gotCount != wantCount {
		t.Fatalf("circular shift changed presence count: got %d want %d", gotCount, wantCount)
	}
}

func TestCircularShift_RotatesCorrectly(t *testing.T) {
	presence := []bool{true, false, false, false, false}
	shifted := circularShift(presence, 1)
	want := []bool{false, true, false, false, false}
	for i := range want {
		if shifted[i] != want[i] {
			t.Fatalf("circularShift(offset=1) = %v, want %v", shifted, want)
		}
	}
	shifted0 := circularShift(presence, 0)
	for i := range presence {
		if shifted0[i] != presence[i] {
			t.Fatalf("circularShift(offset=0) must be identity, got %v", shifted0)
		}
	}
}

func TestDeterministicShiftOffsets_Reproducible(t *testing.T) {
	a := deterministicShiftOffsets(365, 500)
	b := deterministicShiftOffsets(365, 500)
	if len(a) != len(b) {
		t.Fatalf("length mismatch")
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("offsets not reproducible at index %d: %d != %d", i, a[i], b[i])
		}
	}
	// Every offset must be in [1, T-1] — offset 0 is the identity (not a
	// permutation) and T would also be identity under modular arithmetic.
	for i, off := range a {
		if off < 1 || off >= 365 {
			t.Fatalf("offset[%d] = %d out of range [1, 364]", i, off)
		}
	}
}

func TestDeterministicShiftOffsets_GoodCoverage(t *testing.T) {
	// With N=500 draws over T-1=364 possible offsets and a coprime step, the
	// walk should cover a large fraction of distinct offsets before it wraps
	// — a degenerate implementation that always returns the same few offsets
	// would under-sample the null distribution.
	offsets := deterministicShiftOffsets(365, 364)
	seen := map[int]bool{}
	for _, o := range offsets {
		seen[o] = true
	}
	if len(seen) < 300 {
		t.Fatalf("expected broad offset coverage, got only %d distinct offsets out of 364 draws", len(seen))
	}
}

func TestBenjaminiHochberg_MonotoneAndOverAllTests(t *testing.T) {
	// 10 tests: 2 genuinely small p-values, 8 large (noise). BH must not
	// promote the noise, and q must be computed over all 10, not just the 2
	// that look interesting (that would be p-hacking).
	pvals := []float64{0.001, 0.002, 0.5, 0.6, 0.7, 0.8, 0.4, 0.55, 0.65, 0.9}
	q := benjaminiHochberg(pvals)
	if len(q) != len(pvals) {
		t.Fatalf("expected q for every input p-value, got %d for %d", len(q), len(pvals))
	}
	if q[0] > 0.05 || q[1] > 0.05 {
		t.Fatalf("expected the two small p-values to survive at q<=0.05, got q0=%.4f q1=%.4f", q[0], q[1])
	}
	for i := 2; i < len(pvals); i++ {
		if q[i] <= 0.05 {
			t.Fatalf("noise p-value at index %d unexpectedly survived FDR: p=%.4f q=%.4f", i, pvals[i], q[i])
		}
	}
	// Monotonicity: sorted by p ascending, q must be non-decreasing.
	type pr struct{ p, q float64 }
	prs := make([]pr, len(pvals))
	for i := range pvals {
		prs[i] = pr{pvals[i], q[i]}
	}
	for i := 0; i < len(prs); i++ {
		for j := i + 1; j < len(prs); j++ {
			if prs[i].p < prs[j].p && prs[i].q > prs[j].q {
				t.Fatalf("BH q-values not monotone with p: p=%.4f q=%.4f vs p=%.4f q=%.4f",
					prs[i].p, prs[i].q, prs[j].p, prs[j].q)
			}
		}
	}
}

func TestCircularShiftPValue_IndependentBurstyPairIsNotSignificant(t *testing.T) {
	// Two bursty-but-genuinely-independent series (each present in
	// contiguous blocks, common in real data — e.g. a multi-day event).
	// A circular-shift null should NOT call this significant, because the
	// alignment (not the burstiness) is what's destroyed by the shift.
	T := 60
	a := make([]bool, T)
	b := make([]bool, T)
	for i := 0; i < 10; i++ { // block at start
		a[i] = true
	}
	for i := 30; i < 40; i++ { // unrelated block later
		b[i] = true
	}
	lag := 0
	k := dayLagCoOccurrence(a, b, lag)
	lift := liftScore(k, T, 10, 10)
	offsets := deterministicShiftOffsets(T, 200)
	p := circularShiftPValue(a, b, lag, T, 10, offsets, lift)
	if p <= 0.1 {
		t.Fatalf("expected a non-overlapping independent pair to be non-significant, got lift=%.2f p=%.4f", lift, p)
	}
}

func TestCircularShiftPValue_VsIIDShuffle_CircularIsMoreConservative(t *testing.T) {
	// This test documents WHY circular shift (not IID shuffle) is required:
	// build two series that are each strongly autocorrelated (long
	// contiguous runs) but have no real cross-series alignment. An IID
	// shuffle destroys the runs, making almost every permutation look
	// "less bursty" than the real (non-shuffled) alignment by chance,
	// which drives p spuriously low (anti-conservative). A circular shift
	// preserves each series' run-structure, so it does not suffer this
	// failure mode — verified here structurally: circular-shift null lifts
	// should span a wide range including values >= the observed lift often
	// enough that p stays large for an unaligned bursty pair.
	T := 90
	a := make([]bool, T)
	b := make([]bool, T)
	for i := 0; i < 30; i++ {
		a[i] = true // one long run
	}
	for i := 45; i < 75; i++ {
		b[i] = true // another long run, no overlap with a
	}
	lag := 0
	k := dayLagCoOccurrence(a, b, lag)
	lift := liftScore(k, T, 30, 30)
	offsets := deterministicShiftOffsets(T, 500)
	p := circularShiftPValue(a, b, lag, T, 30, offsets, lift)
	if p <= 0.05 {
		t.Fatalf("circular-shift null wrongly flagged a burst-only artifact as significant: lift=%.2f p=%.4f", lift, p)
	}
}

// TestCircularShiftPValue_CannotResolveBelowThePermutationFloor pins the
// correctness property that #706's adversarial refute found violated: a
// permutation p-value can never be finer than the permutation SPACE, no
// matter how many draws are requested.
//
// deterministicShiftOffsets draws from [1, T-1], so the space has exactly
// T-1 distinct rotations. Asking for N >> T-1 draws re-evaluates rotations
// that were already evaluated; each repeat is the SAME statistic, not an
// additional independent draw. Dividing by N+1 anyway reports evidence the
// data cannot contain.
//
// RED before the fix: with T=365 and N=4000 the uncapped implementation
// returns 1/4001 = 0.00025 for a perfectly-aligned pair whose exact p is
// 1/365 = 0.00274 — an ~11x overstatement, and precisely the inflation that
// let the proof's planted signal clear BH-FDR.
func TestCircularShiftPValue_CannotResolveBelowThePermutationFloor(t *testing.T) {
	const T = 365
	// A pair that no rotation can match: a and b are identical, so the
	// unshifted lift is maximal and every non-zero rotation scores lower.
	// exceed == 0, therefore p is exactly the floor 1/(space+1).
	a := make([]bool, T)
	b := make([]bool, T)
	for i := 0; i < T; i += 4 {
		a[i] = true
		b[i] = true
	}
	nA := 0
	for _, v := range a {
		if v {
			nA++
		}
	}
	lift := liftScore(dayLagCoOccurrence(a, b, 0), T, nA, nA)

	distinct := map[int]struct{}{}
	for _, o := range deterministicShiftOffsets(T, 4000) {
		distinct[o] = struct{}{}
	}
	if len(distinct) != T-1 {
		t.Fatalf("precondition: expected the shift space to hold %d distinct rotations, got %d", T-1, len(distinct))
	}
	exactFloor := 1.0 / float64(len(distinct)+1)

	for _, n := range []int{500, 4000, 40000} {
		offsets := deterministicShiftOffsets(T, n)
		p := circularShiftPValue(a, b, 0, T, nA, offsets, lift)
		if p < exactFloor {
			t.Errorf("N=%d: p=%.8f is finer than the exact permutation floor %.8f (%.1fx overstated) — "+
				"the shift space holds only %d distinct rotations",
				n, p, exactFloor, exactFloor/p, len(distinct))
		}
	}

	// And the floor must be REACHED, not merely respected: a maximally
	// aligned pair should report exactly 1/(T-1+1), never something coarser.
	p := circularShiftPValue(a, b, 0, T, nA, deterministicShiftOffsets(T, 4000), lift)
	if diff := p - exactFloor; diff > 1e-12 || diff < -1e-12 {
		t.Errorf("expected the exact floor %.8f for an unmatchable pair, got %.8f", exactFloor, p)
	}
}
