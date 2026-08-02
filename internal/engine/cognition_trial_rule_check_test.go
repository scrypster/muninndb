//go:build localassets || cognitiontrial

package engine

import (
	"math"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// The acceptance rule is the thing that decides whether a subsystem lives or
// dies, so it gets tested like production code. These run in the default CI
// job: pure arithmetic, no vault, no assets, milliseconds.
//
// What they are FOR: making sure a failing gate reports honestly instead of
// being rounded up. Every UNDERPOWERED condition is tested by pushing exactly
// one input past its threshold and requiring the verdict to change, so no gate
// can quietly become unreachable.
//
// PRIVACY: numbers only. Nothing here touches a vault.
// ---------------------------------------------------------------------------

// ctGoodJudge is a calibration that passes U1 and S5.
func ctGoodJudge() ctJudgeCalibration {
	return ctJudgeCalibration{Ran: true, Kappa: 0.72, FPR: 0.09, N: 180, LabelHashMatches: true}
}

// ctRisingBuckets is a 12-bucket Delta_C series that satisfies S3: positive in
// all of the last 10 and non-decreasing.
func ctRisingBuckets(base float64) []float64 {
	out := make([]float64, 12)
	for i := range out {
		out[i] = base + 0.002*float64(i)
	}
	return out
}

// ctFlatBuckets is a series that fails S3's positivity requirement.
func ctFlatBuckets(v float64) []float64 {
	out := make([]float64, 12)
	for i := range out {
		out[i] = v
	}
	return out
}

func ctShipVault(label string) ctVaultResult {
	return ctVaultResult{
		Label:            label,
		NQueries:         320,
		DistinctEvents:   410,
		DeltaC:           ctDelta{Point: 0.055, CILower: 0.031, CIUpper: 0.079, N: 320},
		DeltaH:           ctDelta{Point: 0.028, CILower: 0.008, CIUpper: 0.048, N: 320},
		DeltaP:           ctDelta{Point: 0.006, CILower: -0.010, CIUpper: 0.022, N: 320},
		MRRDeltaH:        0.031,
		MRRDeltaP:        0.002,
		DeltaCByBucket:   ctRisingBuckets(0.03),
		BaselineEdges:    5000,
		ReplayedEdges:    2100,
		UnreplayableFrac: 0.30,
	}
}

func ctKillVault(label string) ctVaultResult {
	return ctVaultResult{
		Label:            label,
		NQueries:         340,
		DistinctEvents:   460,
		DeltaC:           ctDelta{Point: 0.002, CILower: -0.014, CIUpper: 0.018, N: 340},
		DeltaH:           ctDelta{Point: 0.001, CILower: -0.011, CIUpper: 0.013, N: 340},
		DeltaP:           ctDelta{Point: -0.003, CILower: -0.015, CIUpper: 0.009, N: 340},
		MRRDeltaH:        0.0,
		MRRDeltaP:        -0.001,
		DeltaCByBucket:   ctFlatBuckets(0.001),
		BaselineEdges:    5000,
		ReplayedEdges:    2400,
		UnreplayableFrac: 0.25,
	}
}

func ctThree(f func(string) ctVaultResult) []ctVaultResult {
	return []ctVaultResult{f("A"), f("B"), f("C")}
}

func ctRequireVerdict(t *testing.T, got ctDecision, want ctVerdict, wantReason string) {
	t.Helper()
	if got.Verdict != want {
		t.Fatalf("verdict = %s, want %s\n%s", got.Verdict, want, got)
	}
	if wantReason != "" {
		found := false
		for _, r := range got.Reasons {
			if strings.Contains(r, wantReason) {
				found = true
			}
		}
		if !found {
			t.Errorf("no reason mentions %q — a gate that fires without saying why cannot be "+
				"reported honestly\n%s", wantReason, got)
		}
	}
}

func TestCognitionTrialRule_Ship(t *testing.T) {
	ctRequireVerdict(t, ctDecide(ctThree(ctShipVault), ctGoodJudge(), true), ctVerdictShip, "S2 PASS")
}

func TestCognitionTrialRule_Kill(t *testing.T) {
	ctRequireVerdict(t, ctDecide(ctThree(ctKillVault), ctGoodJudge(), true), ctVerdictKill, "K1 PASS")
}

// A Delta_C carried entirely by the base-level prior, with neither named
// mechanism reaching its bar, must NOT ship. This is S2's whole purpose and the
// single most likely way a SHIP verdict would be wrong.
func TestCognitionTrialRule_PriorWithoutAMechanismDoesNotShip(t *testing.T) {
	vaults := ctThree(ctShipVault)
	for i := range vaults {
		vaults[i].DeltaH = ctDelta{Point: 0.004, CILower: -0.006, CIUpper: 0.014}
		vaults[i].DeltaP = ctDelta{Point: 0.003, CILower: -0.007, CIUpper: 0.013}
		vaults[i].MRRDeltaH = 0.001
	}
	got := ctDecide(vaults, ctGoodJudge(), true)
	if got.Verdict == ctVerdictShip {
		t.Fatalf("a Delta_C with no attributable mechanism SHIPPED — S2 is not doing its job\n%s", got)
	}
	ctRequireVerdict(t, got, ctVerdictInconclusivePowered, "S2 FAIL")
}

// A real-but-small effect with tight intervals is neither SHIP nor KILL.
func TestCognitionTrialRule_InconclusiveButPowered(t *testing.T) {
	vaults := ctThree(ctShipVault)
	for i := range vaults {
		vaults[i].DeltaC = ctDelta{Point: 0.018, CILower: 0.006, CIUpper: 0.030}
		vaults[i].DeltaH = ctDelta{Point: 0.010, CILower: 0.002, CIUpper: 0.018}
		vaults[i].DeltaP = ctDelta{Point: 0.004, CILower: -0.004, CIUpper: 0.012}
	}
	ctRequireVerdict(t, ctDecide(vaults, ctGoodJudge(), true), ctVerdictInconclusivePowered,
		"INCONCLUSIVE-BUT-POWERED")
}

// Every UNDERPOWERED condition, one at a time, from an otherwise-SHIP input.
func TestCognitionTrialRule_UnderpoweredGates(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(v []ctVaultResult, j *ctJudgeCalibration, fidelity *bool)
		wantSub string
	}{
		{"U1 judge not run", func(_ []ctVaultResult, j *ctJudgeCalibration, _ *bool) {
			j.Ran = false
		}, "U1: the judge-calibration gate was not run"},
		{"U1 kappa", func(_ []ctVaultResult, j *ctJudgeCalibration, _ *bool) {
			j.Kappa = 0.59
		}, "U1: judge kappa"},
		{"U1 fpr", func(_ []ctVaultResult, j *ctJudgeCalibration, _ *bool) {
			j.FPR = 0.16
		}, "U1: judge false-positive rate"},
		{"U2 queries", func(v []ctVaultResult, _ *ctJudgeCalibration, _ *bool) {
			v[1].NQueries = 299
		}, "U2: vault B has 299 labeled held-out queries"},
		{"U2 events", func(v []ctVaultResult, _ *ctJudgeCalibration, _ *bool) {
			v[2].DistinctEvents = 199
		}, "U2: vault C has 199 distinct recall events"},
		{"U3 fidelity", func(_ []ctVaultResult, _ *ctJudgeCalibration, f *bool) {
			*f = false
		}, "U3: a reconstruction-fidelity test failed"},
		{"U4 edge floor", func(v []ctVaultResult, _ *ctJudgeCalibration, _ *bool) {
			v[0].ReplayedEdges = 999 // 19.98% of 5000
		}, "U4: vault A replayed 999 edges"},
		{"U4 unreplayable ceiling", func(v []ctVaultResult, _ *ctJudgeCalibration, _ *bool) {
			v[0].UnreplayableFrac = 0.61
		}, "in RelTypes replay cannot produce"},
		{"U5 wide intervals on two vaults", func(v []ctVaultResult, _ *ctJudgeCalibration, _ *bool) {
			v[0].DeltaC = ctDelta{Point: 0.055, CILower: 0.001, CIUpper: 0.110}
			v[1].DeltaC = ctDelta{Point: 0.055, CILower: 0.001, CIUpper: 0.110}
		}, "U5: the paired-bootstrap 95% CI half-width"},
		{"U2 only two vaults", func(_ []ctVaultResult, _ *ctJudgeCalibration, _ *bool) {}, "U2: 2 vaults, need 3"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			vaults := ctThree(ctShipVault)
			if tc.name == "U2 only two vaults" {
				vaults = vaults[:2]
			}
			judge := ctGoodJudge()
			fidelity := true
			tc.mutate(vaults, &judge, &fidelity)
			ctRequireVerdict(t, ctDecide(vaults, judge, fidelity), ctVerdictUnderpowered, tc.wantSub)
		})
	}
}

// A wide interval is NOT a kill — it is U5. Pinned because rounding a wide
// interval down to "no effect" is the most inviting mistake in the whole rule.
func TestCognitionTrialRule_WideIntervalIsNotAKill(t *testing.T) {
	vaults := ctThree(ctKillVault)
	for i := range vaults {
		vaults[i].DeltaC = ctDelta{Point: 0.005, CILower: -0.060, CIUpper: 0.070}
	}
	ctRequireVerdict(t, ctDecide(vaults, ctGoodJudge(), true), ctVerdictUnderpowered, "U5")
}

// A vault where the prior is significantly NEGATIVE blocks S1 even if two other
// vaults clear the bar.
func TestCognitionTrialRule_SignificantlyNegativeVaultBlocksShip(t *testing.T) {
	vaults := ctThree(ctShipVault)
	vaults[2].DeltaC = ctDelta{Point: -0.04, CILower: -0.06, CIUpper: -0.02}
	got := ctDecide(vaults, ctGoodJudge(), true)
	if got.Verdict == ctVerdictShip {
		t.Fatalf("shipped despite a significantly negative vault\n%s", got)
	}
	ctRequireVerdict(t, got, ctVerdictInconclusivePowered, "SIGNIFICANTLY NEGATIVE")
}

// S5: a label hash that does not match what was frozen before scoring blocks
// SHIP outright, however good the numbers look.
func TestCognitionTrialRule_LabelHashMismatchBlocksShip(t *testing.T) {
	judge := ctGoodJudge()
	judge.LabelHashMatches = false
	got := ctDecide(ctThree(ctShipVault), judge, true)
	if got.Verdict == ctVerdictShip {
		t.Fatalf("shipped with a label-hash mismatch — S5 is not doing its job\n%s", got)
	}
	ctRequireVerdict(t, got, ctVerdictInconclusivePowered, "S5 FAIL")
}

// ---------------------------------------------------------------------------
// Metrics
// ---------------------------------------------------------------------------

func TestCognitionTrialMetrics_NDCGGradedAndPooled(t *testing.T) {
	grades := map[string]int{"m1": 3, "m2": 2, "m3": 1, "m4": 0}

	perfect := ctNDCGAt10([]string{"m1", "m2", "m3", "m4"}, grades)
	if math.Abs(perfect-1.0) > 1e-12 {
		t.Errorf("ideal order NDCG@10 = %.12f, want 1", perfect)
	}
	reversed := ctNDCGAt10([]string{"m4", "m3", "m2", "m1"}, grades)
	if reversed >= perfect {
		t.Errorf("reversed order scored %.6f >= ideal %.6f", reversed, perfect)
	}

	// An arm that returns FEWER items must not win by shortening its list: the
	// IDCG denominator comes from the pooled label set, not from the arm.
	truncated := ctNDCGAt10([]string{"m1"}, grades)
	if truncated >= perfect {
		t.Errorf("a one-item list scored %.6f >= the full ideal %.6f — the denominator is "+
			"being taken from the arm instead of the pool", truncated, perfect)
	}

	// An unlabeled item occupies its rank at gain 0 rather than being skipped.
	withNoise := ctNDCGAt10([]string{"unlabeled-x", "m1", "m2", "m3"}, grades)
	if withNoise >= perfect {
		t.Errorf("an unlabeled item at rank 1 did not cost anything (%.6f vs %.6f) — it is "+
			"being skipped, which silently promotes everything after it", withNoise, perfect)
	}

	// A query whose pooled labels are all 0 has no defined NDCG; 0 for every
	// arm, hence exactly 0 in every paired delta.
	if got := ctNDCGAt10([]string{"m4"}, map[string]int{"m4": 0}); got != 0 {
		t.Errorf("all-irrelevant pool NDCG = %v, want 0", got)
	}
}

func TestCognitionTrialMetrics_MRRUsesTheBinarizedGrade(t *testing.T) {
	grades := map[string]int{"m1": 1, "m2": 2, "m3": 3}
	if got := ctMRR([]string{"m1", "m2", "m3"}, grades); math.Abs(got-0.5) > 1e-12 {
		t.Errorf("MRR = %v, want 0.5 — grade 1 is NOT relevant, so the first hit is at rank 2", got)
	}
	if got := ctMRR([]string{"m1"}, grades); got != 0 {
		t.Errorf("MRR = %v, want 0 when nothing relevant was returned", got)
	}
}

func TestCognitionTrialMetrics_PairedBootstrapIsPairedAndReproducible(t *testing.T) {
	a := make([]float64, 200)
	b := make([]float64, 200)
	for i := range a {
		// A large, noisy common component with a small CONSTANT paired
		// difference: an unpaired bootstrap would drown the +0.04 in the noise;
		// a paired one recovers it with a tight interval.
		common := float64((i*37)%100) / 100.0
		// The paired difference VARIES (+-0.02 around 0.04, alternating, so its
		// mean is exactly 0.04 over an even n) rather than being constant: a
		// constant difference makes every resample identical and would hide a
		// seed that was never wired in.
		pert := 0.02
		if i%2 == 0 {
			pert = -0.02
		}
		a[i] = common + 0.04 + pert
		b[i] = common
	}
	d1 := ctPairedBootstrap(a, b, 2000, 42)
	if math.Abs(d1.Point-0.04) > 1e-9 {
		t.Errorf("point estimate %.9f, want 0.04", d1.Point)
	}
	if d1.CILower <= 0 {
		t.Errorf("CI lower %.9f <= 0 on a constant positive paired difference — the resample "+
			"is not preserving the pairing", d1.CILower)
	}
	d2 := ctPairedBootstrap(a, b, 2000, 42)
	if d1 != d2 {
		t.Errorf("same seed produced different intervals: %+v vs %+v — a run log cannot "+
			"reproduce its own numbers", d1, d2)
	}
	if d3 := ctPairedBootstrap(a, b, 2000, 43); d3 == d1 {
		t.Error("a different seed produced an identical interval — the seed is not wired in")
	}
	if d1.N != 200 {
		t.Errorf("N = %d, want 200", d1.N)
	}
}

func TestCognitionTrialMetrics_TrendSlopeInterval(t *testing.T) {
	rising := ctComputeTrend(ctRisingBuckets(0.03), 10)
	if rising.PositiveOfLastN != 10 {
		t.Errorf("positive buckets = %d, want 10", rising.PositiveOfLastN)
	}
	if rising.SlopeCILower < ctPreregistered.TrendSlopeCILower {
		t.Errorf("rising series slope CI lower %.6f is below the non-decreasing floor %.6f",
			rising.SlopeCILower, ctPreregistered.TrendSlopeCILower)
	}

	falling := make([]float64, 12)
	for i := range falling {
		falling[i] = 0.08 - 0.01*float64(i)
	}
	fall := ctComputeTrend(falling, 10)
	if fall.SlopeCILower >= ctPreregistered.TrendSlopeCILower {
		t.Errorf("a clearly decreasing series passed the non-decreasing floor (CI lower %.6f)",
			fall.SlopeCILower)
	}

	// The t-quantile must be the small-sample one. At df=10 using 1.96 instead
	// of 2.228 narrows the interval by ~12% and makes S3 easier than
	// pre-registered.
	if got := ctT975(10); math.Abs(got-2.228) > 1e-9 {
		t.Errorf("ctT975(10) = %v, want 2.228", got)
	}
}

func TestCognitionTrialJudge_KappaAndFPR(t *testing.T) {
	// 10 pairs: 4 both-relevant, 3 both-irrelevant, 2 judge-only (false
	// positives), 1 human-only.
	judge := []int{3, 2, 2, 3, 0, 1, 0, 3, 2, 0}
	human := []int{3, 3, 2, 2, 0, 0, 1, 0, 0, 3}
	k, fpr, n := ctCohensKappa(judge, human)
	if n != 10 {
		t.Fatalf("n = %d, want 10", n)
	}
	// human-irrelevant = 3 both-irrelevant + 2 judge-only = 5; FPR = 2/5.
	if math.Abs(fpr-0.4) > 1e-12 {
		t.Errorf("FPR = %v, want 0.4", fpr)
	}
	if k <= 0 || k >= 1 {
		t.Errorf("kappa = %v, want a value in (0,1) for partial agreement", k)
	}

	// Perfect agreement must be exactly 1, never NaN — a NaN compares false
	// against the gate and would read as a failure for the wrong reason (or,
	// with the comparison flipped, as a pass).
	if k, _, _ := ctCohensKappa([]int{3, 3, 0, 0}, []int{3, 3, 0, 0}); math.Abs(k-1) > 1e-12 {
		t.Errorf("kappa on perfect agreement = %v, want 1", k)
	}
	if k, _, _ := ctCohensKappa([]int{3, 3}, []int{3, 3}); math.IsNaN(k) || k != 1 {
		t.Errorf("kappa on a degenerate all-relevant sample = %v, want 1 and never NaN", k)
	}
	if _, _, n := ctCohensKappa(nil, nil); n != 0 {
		t.Errorf("empty input n = %d, want 0", n)
	}
}
