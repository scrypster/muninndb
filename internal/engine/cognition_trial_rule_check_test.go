//go:build localassets || cognitiontrial

package engine

import (
	"math"
	"reflect"
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
	return ctJudgeCalibration{
		Ran: true, Kappa: 0.72, FPR: 0.09, N: 180,
		// Both human marginals non-empty: 40 of the 180 adjudicated pairs are
		// human-irrelevant (§3c's planted negatives among them), so kappa's
		// expected agreement is defined and the FPR denominator is not empty.
		HumanRelevant: 140, HumanIrrelevant: 40,
		LabelHashMatches: true,
	}
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
		Label:    label,
		NQueries: 320,
		// 40 more queries were scored and every returned item was graded
		// irrelevant: excluded from the deltas (D1), counted here.
		ZeroRelevanceQueries: 40,
		DistinctEvents:       410,
		DeltaC:               ctDelta{Point: 0.055, CILower: 0.031, CIUpper: 0.079, N: 320},
		DeltaH:               ctDelta{Point: 0.028, CILower: 0.008, CIUpper: 0.048, N: 320},
		DeltaP:               ctDelta{Point: 0.006, CILower: -0.010, CIUpper: 0.022, N: 320},
		// This is the CLEAN-SHIP fixture: everything else about it says the
		// layer earns its complexity, so its S6 null is SURVIVED by
		// construction. Tests that want to probe S6 itself override this
		// field explicitly (TestCognitionTrialRule_ShuffledSeedNullGatesShip).
		ShuffledSeedNull: ctNullSurvived,
		// Inside the additivity bounds: max(Delta_H, Delta_P) <= Delta_HP <= Delta_C.
		DeltaHP:                 ctDelta{Point: 0.034, CILower: 0.012, CIUpper: 0.056, N: 320},
		DeltaCAllQueriesDiluted: ctDelta{Point: 0.049, CILower: 0.028, CIUpper: 0.070, N: 360},
		MRRDeltaH:               ctMeanDelta{Point: 0.031, N: 320},
		MRRDeltaP:               ctMeanDelta{Point: 0.002, N: 320},
		DeltaCByBucket:          ctBucketSeries(ctRisingBuckets(0.03)),
		BaselineEdges:           5000,
		ReplayedEdges:           2100,
		UnreplayableFrac:        0.30,
	}
}

func ctKillVault(label string) ctVaultResult {
	return ctVaultResult{
		Label:                   label,
		NQueries:                340,
		ZeroRelevanceQueries:    55,
		DistinctEvents:          460,
		DeltaC:                  ctDelta{Point: 0.002, CILower: -0.014, CIUpper: 0.018, N: 340},
		DeltaH:                  ctDelta{Point: 0.001, CILower: -0.011, CIUpper: 0.013, N: 340},
		DeltaP:                  ctDelta{Point: -0.003, CILower: -0.015, CIUpper: 0.009, N: 340},
		DeltaHP:                 ctDelta{Point: 0.0015, CILower: -0.012, CIUpper: 0.015, N: 340},
		DeltaCAllQueriesDiluted: ctDelta{Point: 0.0017, CILower: -0.012, CIUpper: 0.015, N: 395},
		MRRDeltaH:               ctMeanDelta{Point: 0.0, N: 340},
		MRRDeltaP:               ctMeanDelta{Point: -0.001, N: 340},
		DeltaCByBucket:          ctBucketSeries(ctFlatBuckets(0.001)),
		BaselineEdges:           5000,
		ReplayedEdges:           2400,
		UnreplayableFrac:        0.25,
	}
}

func ctThree(f func(string) ctVaultResult) []ctVaultResult {
	return []ctVaultResult{f("A"), f("B"), f("C")}
}

// ctRequireVerdict, ctExpectReason and ctExpect are the census's ASSERTION
// helpers, and they are the only ones a probe may use. Each bumps
// ctProbeAssertions, which is what lets TestCognitionTrialRule_EveryThresholdBinds
// reject a probe that never asserts anything — see the counters' comment in
// cognition_trial_rule_test.go for exactly how far that goes and where it stops.
func ctRequireVerdict(t *testing.T, got ctDecision, want ctVerdict, wantReason string) {
	t.Helper()
	ctProbeAssertions.Add(1)
	if got.Verdict != want {
		t.Fatalf("verdict = %s, want %s\n%s", got.Verdict, want, got)
	}
	if wantReason != "" {
		ctExpectReason(t, got, wantReason, "a gate that fires without saying why cannot be "+
			"reported honestly")
	}
}

// ctExpectReason requires the audit trail to carry a line containing sub.
func ctExpectReason(t *testing.T, got ctDecision, sub, why string) {
	t.Helper()
	ctProbeAssertions.Add(1)
	for _, r := range got.Reasons {
		if strings.Contains(r, sub) {
			return
		}
	}
	t.Errorf("no reason mentions %q — %s\n%s", sub, why, got)
}

// ctExpect is the arithmetic probes' assertion: they do not produce a decision,
// so they have no audit trail to inspect.
func ctExpect(t *testing.T, ok bool, format string, args ...any) {
	t.Helper()
	ctProbeAssertions.Add(1)
	if !ok {
		t.Errorf(format, args...)
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
		vaults[i].DeltaH = ctDelta{Point: 0.004, CILower: -0.006, CIUpper: 0.014, N: 320}
		vaults[i].DeltaP = ctDelta{Point: 0.003, CILower: -0.007, CIUpper: 0.013, N: 320}
		vaults[i].MRRDeltaH = ctMeanDelta{Point: 0.001, N: vaults[i].DeltaC.N}
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
		vaults[i].DeltaC = ctDelta{Point: 0.018, CILower: 0.006, CIUpper: 0.030, N: 320}
		vaults[i].DeltaH = ctDelta{Point: 0.010, CILower: 0.002, CIUpper: 0.018, N: 320}
		vaults[i].DeltaP = ctDelta{Point: 0.004, CILower: -0.004, CIUpper: 0.012, N: 320}
		vaults[i].DeltaHP = ctDelta{Point: 0.014, CILower: 0.004, CIUpper: 0.024, N: 320}
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
			v[1].DeltaC.N, v[1].DeltaHP.N = 299, 299
		}, "U2: vault B has 299 INFORMATIVE labeled held-out queries"},
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
			v[0].DeltaC = ctDelta{Point: 0.055, CILower: 0.001, CIUpper: 0.110, N: 320}
			v[1].DeltaC = ctDelta{Point: 0.055, CILower: 0.001, CIUpper: 0.110, N: 320}
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

// ---------------------------------------------------------------------------
// TestCognitionTrialRule_EveryThresholdBinds — the anti-inert-gate census.
//
// TestCognitionTrialRule_UnderpoweredGates above is a HAND-MAINTAINED list, and
// a hand-maintained list is exactly how ctJudgeCalibration.N came to be
// declared, documented and never read: it had no case, so nothing noticed. A
// sweep over the rule's inputs found it was the ONLY inert one, which is
// reassuring about the others and says nothing at all about the next field
// somebody adds.
//
// So this test REFLECTS over every field of ctPreregistered and every field of
// ctJudgeCalibration and requires a registered probe for each. A new threshold
// without a probe fails here; a probe for a field that no longer exists fails
// too. Each probe pushes exactly one input past that field's bound and requires
// the verdict to change — i.e. it proves the field is load-bearing, not merely
// present.
// ---------------------------------------------------------------------------

// ctProbe asserts that one mutation moves the verdict off SHIP (or off KILL,
// for the K-side thresholds) and says why in the audit trail.
type ctProbe func(t *testing.T)

// ctFromShip mutates an otherwise-SHIP input and requires the stated verdict.
func ctFromShip(want ctVerdict, wantSub string,
	mutate func(v []ctVaultResult, j *ctJudgeCalibration, fidelity *bool),
) ctProbe {
	return func(t *testing.T) {
		vaults := ctThree(ctShipVault)
		judge := ctGoodJudge()
		fidelity := true
		mutate(vaults, &judge, &fidelity)
		if base := ctDecide(ctThree(ctShipVault), ctGoodJudge(), true); base.Verdict != ctVerdictShip {
			t.Fatalf("baseline is not SHIP, so this probe proves nothing\n%s", base)
		}
		ctRequireVerdict(t, ctDecide(vaults, judge, fidelity), want, wantSub)
	}
}

// ctFromKill mutates an otherwise-KILL input and requires the stated verdict —
// the K-side thresholds can only be shown to bind from a killing baseline.
func ctFromKill(want ctVerdict, wantSub string, mutate func(v []ctVaultResult)) ctProbe {
	return func(t *testing.T) {
		vaults := ctThree(ctKillVault)
		mutate(vaults)
		if base := ctDecide(ctThree(ctKillVault), ctGoodJudge(), true); base.Verdict != ctVerdictKill {
			t.Fatalf("baseline is not KILL, so this probe proves nothing\n%s", base)
		}
		ctRequireVerdict(t, ctDecide(vaults, ctGoodJudge(), true), want, wantSub)
	}
}

// ctFromShipDescriptive is for an input that is READ AND REPORTED but no longer
// gates a verdict — S3's two thresholds since D3. It requires the audit trail to
// change and the verdict NOT to, which is the honest claim for a descriptive
// quantity: pretending it still gates would be worse than admitting it does not.
func ctFromShipDescriptive(t *testing.T, wantSub string, mutate func(v []ctVaultResult)) {
	t.Helper()
	base := ctDecide(ctThree(ctShipVault), ctGoodJudge(), true)
	if base.Verdict != ctVerdictShip {
		t.Fatalf("baseline is not SHIP, so this probe proves nothing\n%s", base)
	}
	vaults := ctThree(ctShipVault)
	mutate(vaults)
	got := ctDecide(vaults, ctGoodJudge(), true)
	if got.Verdict != ctVerdictShip {
		t.Fatalf("verdict moved to %s. This field is DESCRIPTIVE since D3 and must not gate a "+
			"verdict; if it now does, the probe is stale or the demotion was reverted\n%s",
			got.Verdict, got)
	}
	ctExpectReason(t, got, wantSub, "the field is neither gating NOR reported, i.e. inert")
}

func ctPreregisteredProbes() map[string]ctProbe {
	return map[string]ctProbe{
		// --- S-side ---------------------------------------------------------
		"MinDeltaC": ctFromShip(ctVerdictInconclusivePowered, "S1 FAIL",
			func(v []ctVaultResult, _ *ctJudgeCalibration, _ *bool) {
				for i := range v {
					// Just under the bar, CI still strictly positive and tight:
					// this is a REAL effect that is simply too small.
					v[i].DeltaC = ctDelta{Point: ctPreregistered.MinDeltaC - 0.001,
						CILower: 0.020, CIUpper: 0.038, N: 320}
					v[i].DeltaHP = ctDelta{Point: 0.028, CILower: 0.010, CIUpper: 0.046, N: 320}
				}
			}),
		"MinDeltaMechanism": ctFromShip(ctVerdictInconclusivePowered, "S2 FAIL",
			func(v []ctVaultResult, _ *ctJudgeCalibration, _ *bool) {
				for i := range v {
					v[i].DeltaH = ctDelta{Point: ctPreregistered.MinDeltaMechanism - 0.001,
						CILower: 0.008, CIUpper: 0.030, N: 320}
					v[i].DeltaP = ctDelta{Point: 0.003, CILower: -0.007, CIUpper: 0.013, N: 320}
				}
			}),
		// DESCRIPTIVE SINCE D3. S3 is no longer in the SHIP conjunction, because
		// the code does not compute the checkpoint trend S3 was pre-registered
		// as: ctReplay runs once, its checkpoint callback only logs, and every arm
		// scores against the FINAL graph. So this field can no longer be shown to
		// move a VERDICT — the honest probe is that it still moves the REPORTED
		// line, and that it still pins U6's derived floor (which does gate).
		// A probe asserting a verdict change here would be asserting a gate the
		// project deliberately removed.
		"TrendMinPositive": func(t *testing.T) {
			series := ctRisingBuckets(0.03)
			for i := 2; i < 2+(ctPreregistered.TrendWindow-ctPreregistered.TrendMinPositive+1); i++ {
				series[i] = -0.001
			}
			tr := ctComputeTrend(ctBucketSeries(series), ctPreregistered.TrendWindow)
			if tr.PositiveOfLastN != ctPreregistered.TrendMinPositive-1 {
				t.Fatalf("probe setup: %d positive in the window, want %d",
					tr.PositiveOfLastN, ctPreregistered.TrendMinPositive-1)
			}
			ctFromShipDescriptive(t, "S3 FAIL",
				func(v []ctVaultResult) {
					for i := range v {
						v[i].DeltaCByBucket = ctBucketSeries(series)
					}
				})
		},
		"TrendWindow": func(t *testing.T) {
			// A series whose positivity count DEPENDS on the window: positive in
			// the two oldest buckets, and only TrendMinPositive-1 positive
			// inside the trailing window. At the pre-registered window it fails
			// S3; at a wider window it would pass — which is what "the window is
			// load-bearing" means.
			series := ctRisingBuckets(0.03)
			for i := 2; i < 2+(ctPreregistered.TrendWindow-ctPreregistered.TrendMinPositive+1); i++ {
				series[i] = -0.001
			}
			narrow := ctComputeTrend(ctBucketSeries(series), ctPreregistered.TrendWindow)
			wide := ctComputeTrend(ctBucketSeries(series), ctPreregistered.TrendWindow+2)
			if narrow.PositiveOfLastN >= ctPreregistered.TrendMinPositive {
				t.Fatalf("probe setup: the pre-registered window already passes (%d positive)",
					narrow.PositiveOfLastN)
			}
			ctExpect(t, wide.PositiveOfLastN >= ctPreregistered.TrendMinPositive,
				"the trailing window is not load-bearing: widening it from %d to %d "+
					"changed nothing (%d vs %d positive)", ctPreregistered.TrendWindow,
				ctPreregistered.TrendWindow+2, narrow.PositiveOfLastN, wide.PositiveOfLastN)
		},
		// DESCRIPTIVE SINCE D3, same as TrendMinPositive.
		"TrendSlopeCILower": func(t *testing.T) {
			ctFromShipDescriptive(t, "S3 FAIL", func(v []ctVaultResult) {
				// Every bucket positive — so the positivity half of S3 passes —
				// but clearly DECREASING, which is the half this field reads.
				falling := make([]float64, 12)
				for i := range falling {
					falling[i] = 0.08 - 0.005*float64(i)
				}
				for i := range v {
					v[i].DeltaCByBucket = ctBucketSeries(falling)
				}
			})
		},
		"VaultsSupporting": ctFromShip(ctVerdictInconclusivePowered, "S1 FAIL",
			func(v []ctVaultResult, _ *ctJudgeCalibration, _ *bool) {
				// Only ONE vault carries the effect; the rule needs two.
				for i := 1; i < len(v); i++ {
					v[i].DeltaC = ctDelta{Point: 0.029, CILower: 0.020, CIUpper: 0.038, N: 320}
					v[i].DeltaHP = ctDelta{Point: 0.028, CILower: 0.010, CIUpper: 0.046, N: 320}
				}
			}),

		// --- K-side ---------------------------------------------------------
		"KillDeltaCPoint": ctFromKill(ctVerdictInconclusivePowered, "K1 FAIL",
			func(v []ctVaultResult) {
				for i := range v {
					v[i].DeltaC = ctDelta{Point: ctPreregistered.KillDeltaCPoint + 0.001,
						CILower: -0.005, CIUpper: 0.027, N: 340}
					v[i].DeltaHP = ctDelta{Point: 0.009, CILower: -0.006, CIUpper: 0.024, N: 340}
				}
			}),
		"KillDeltaCCIUpper": ctFromKill(ctVerdictInconclusivePowered, "K1 FAIL",
			func(v []ctVaultResult) {
				for i := range v {
					v[i].DeltaC = ctDelta{Point: 0.002, CILower: -0.025,
						CIUpper: ctPreregistered.KillDeltaCCIUpper + 0.001, N: 340}
					v[i].DeltaHP = ctDelta{Point: 0.0018, CILower: -0.025, CIUpper: 0.029, N: 340}
				}
			}),

		// --- U-side ---------------------------------------------------------
		"VaultsRequired": func(t *testing.T) {
			got := ctDecide(ctThree(ctShipVault)[:ctPreregistered.VaultsRequired-1], ctGoodJudge(), true)
			ctRequireVerdict(t, got, ctVerdictUnderpowered, "U2: 2 vaults, need 3")
		},
		"MinQueriesPerVault": ctFromShip(ctVerdictUnderpowered, "labeled held-out queries",
			func(v []ctVaultResult, _ *ctJudgeCalibration, _ *bool) {
				v[1].NQueries = ctPreregistered.MinQueriesPerVault - 1
			}),
		"MinEventsPerVault": ctFromShip(ctVerdictUnderpowered, "distinct recall events",
			func(v []ctVaultResult, _ *ctJudgeCalibration, _ *bool) {
				v[2].DistinctEvents = ctPreregistered.MinEventsPerVault - 1
			}),
		"MinJudgeKappa": ctFromShip(ctVerdictUnderpowered, "U1: judge kappa",
			func(_ []ctVaultResult, j *ctJudgeCalibration, _ *bool) {
				j.Kappa = ctPreregistered.MinJudgeKappa - 0.01
			}),
		"MaxJudgeFPR": ctFromShip(ctVerdictUnderpowered, "U1: judge false-positive rate",
			func(_ []ctVaultResult, j *ctJudgeCalibration, _ *bool) {
				j.FPR = ctPreregistered.MaxJudgeFPR + 0.01
			}),
		"MinAdjudicatedPairs": ctFromShip(ctVerdictUnderpowered, "adjudicated pairs, need",
			func(_ []ctVaultResult, j *ctJudgeCalibration, _ *bool) {
				j.N = ctPreregistered.MinAdjudicatedPairs - 1
			}),
		"MinReplayEdgeFrac": ctFromShip(ctVerdictUnderpowered, "below the 20% floor",
			func(v []ctVaultResult, _ *ctJudgeCalibration, _ *bool) {
				v[0].ReplayedEdges = int(ctPreregistered.MinReplayEdgeFrac*
					float64(v[0].BaselineEdges)) - 1
			}),
		"MaxUnreplayableFrac": ctFromShip(ctVerdictUnderpowered, "in RelTypes replay cannot produce",
			func(v []ctVaultResult, _ *ctJudgeCalibration, _ *bool) {
				v[0].UnreplayableFrac = ctPreregistered.MaxUnreplayableFrac + 0.01
			}),
		"MaxCIHalfWidth": ctFromShip(ctVerdictUnderpowered, "CI half-width",
			func(v []ctVaultResult, _ *ctJudgeCalibration, _ *bool) {
				w := ctPreregistered.MaxCIHalfWidth + 0.001
				for i := 0; i < ctPreregistered.VaultsSupporting; i++ {
					v[i].DeltaC = ctDelta{Point: 0.055, CILower: 0.055 - w, CIUpper: 0.055 + w, N: 320}
				}
			}),
		"MinPopulatedBuckets": func(t *testing.T) {
			// DERIVED, and pinned as derived: S3 asks for TrendMinPositive
			// positive buckets out of the trailing window, which is
			// unsatisfiable with fewer than TrendMinPositive populated ones. If
			// someone raises TrendMinPositive without raising this, the floor
			// silently stops being the floor.
			ctExpect(t, ctPreregistered.MinPopulatedBuckets == ctPreregistered.TrendMinPositive,
				"MinPopulatedBuckets = %d but TrendMinPositive = %d: the U6 floor is "+
					"DERIVED from S3's positivity requirement, not chosen",
				ctPreregistered.MinPopulatedBuckets, ctPreregistered.TrendMinPositive)
			ctFromShip(ctVerdictUnderpowered, "populated weekly buckets",
				func(v []ctVaultResult, _ *ctJudgeCalibration, _ *bool) {
					full := ctBucketSeries(ctRisingBuckets(0.03))
					for i := range v {
						v[i].DeltaCByBucket = full[:ctPreregistered.MinPopulatedBuckets-1]
					}
				})(t)
		},

		// --- not a gate: a parameter the harness consumes --------------------
		"BootstrapResamples": func(t *testing.T) {
			// This one is NOT a verdict gate — it is the resample count the
			// interval is computed from. Its probe therefore proves it is
			// CONSUMED: the pre-registered value must produce a different
			// interval from a coarser one on identical data and seed.
			a := make([]float64, 60)
			b := make([]float64, 60)
			for i := range a {
				// The paired difference must VARY, or every resample mean is
				// identical and the resample count could not be observed at all.
				b[i] = float64((i*13)%17) / 17.0
				a[i] = b[i] + 0.04 + 0.01*float64((i*7)%5) - 0.02
			}
			full := ctPairedBootstrap(a, b, ctPreregistered.BootstrapResamples, 7)
			coarse := ctPairedBootstrap(a, b, 20, 7)
			ctExpect(t, full != coarse,
				"the resample count is not consumed: %d resamples and 20 produced an "+
					"identical interval %+v", ctPreregistered.BootstrapResamples, full)
			ctExpect(t, ctPercentileIndex(ctPreregistered.BootstrapResamples, 0.025) ==
				ctPreregistered.BootstrapResamples/40,
				"the 2.5%% percentile index is not derived from the resample count")
		},

		// --- pins, not thresholds (Finding 8) --------------------------------
		// The arm set and the pooling depth are degrees of freedom the D1
		// exclusion inherits: the pool is the UNION of the arms' top-PoolDepth,
		// so both decide which queries are informative and therefore where every
		// absolute bar falls. Their probe is that a change to either is DETECTED
		// and named as a re-pre-registration.
		"Arms": func(t *testing.T) {
			ctExpect(t, ctInstrumentPinViolation(ctArmNames, ctPreregistered.PoolDepth) == "",
				"the arm set the rule reports over (%v) is not the pre-registered one (%v)",
				ctArmNames, ctPreregistered.Arms)
			added := append(append([]string(nil), ctArmNames...), "NO-BASE-LEVEL")
			ctExpect(t, strings.Contains(
				ctInstrumentPinViolation(added, ctPreregistered.PoolDepth), "ARM SET has changed"),
				"adding a sixth arm was not detected as a change to the instrument. D2 added a "+
					"FIFTH arm and that alone re-partitioned informative vs zero-relevance: the "+
					"same query is zero-relevance under a 4-arm pool and informative under a "+
					"5-arm pool where the extra arm surfaced one graded document.")
			ctExpect(t, strings.Contains(
				ctInstrumentPinViolation(ctArmNames[:len(ctArmNames)-1], ctPreregistered.PoolDepth),
				"ARM SET has changed"), "REMOVING an arm was not detected either")
			// Reordering is NOT a change: it moves no query between the two sets.
			shuffled := []string{ctArmNameContentOnly, ctArmNameFull, ctArmNameNoPAS,
				ctArmNameBaseLevelOnly, ctArmNameNoHebbian}
			ctExpect(t, ctInstrumentPinViolation(shuffled, ctPreregistered.PoolDepth) == "",
				"reordering the arms was reported as an instrument change; the pin is over the "+
					"SET, and reporting order moves no query between informative and zero-relevance")
		},
		"PoolDepth": func(t *testing.T) {
			ctExpect(t, strings.Contains(
				ctInstrumentPinViolation(ctArmNames, ctPreregistered.PoolDepth+5),
				"POOLING DEPTH is"),
				"a deeper pool was not detected as a change to the instrument. It is "+
					"env-configurable (MUNINN_COGTRIAL_POOL_DEPTH) and it decides how many "+
					"documents per arm enter the pooled label set, hence which queries have a "+
					"defined NDCG at all.")
			ctExpectReason(t,
				ctDecision{Reasons: []string{ctInstrumentPinViolation(ctArmNames, 1)}},
				"RE-PRE-REGISTRATION",
				"a change to a pinned instrument parameter must be named as what it is")
		},
	}
}

func ctJudgeProbes() map[string]ctProbe {
	return map[string]ctProbe{
		"Ran": ctFromShip(ctVerdictUnderpowered, "U1: the judge-calibration gate was not run",
			func(_ []ctVaultResult, j *ctJudgeCalibration, _ *bool) { j.Ran = false }),
		"Kappa": ctFromShip(ctVerdictUnderpowered, "U1: judge kappa",
			func(_ []ctVaultResult, j *ctJudgeCalibration, _ *bool) {
				j.Kappa = ctPreregistered.MinJudgeKappa - 0.01
			}),
		"FPR": ctFromShip(ctVerdictUnderpowered, "U1: judge false-positive rate",
			func(_ []ctVaultResult, j *ctJudgeCalibration, _ *bool) {
				j.FPR = ctPreregistered.MaxJudgeFPR + 0.01
			}),
		"N": ctFromShip(ctVerdictUnderpowered, "adjudicated pairs, need",
			func(_ []ctVaultResult, j *ctJudgeCalibration, _ *bool) {
				j.N = ctPreregistered.MinAdjudicatedPairs - 1
			}),
		"HumanRelevant": ctFromShip(ctVerdictUnderpowered, "no human-RELEVANT",
			func(_ []ctVaultResult, j *ctJudgeCalibration, _ *bool) { j.HumanRelevant = 0 }),
		"HumanIrrelevant": ctFromShip(ctVerdictUnderpowered, "no human-IRRELEVANT",
			func(_ []ctVaultResult, j *ctJudgeCalibration, _ *bool) { j.HumanIrrelevant = 0 }),
		"LabelHashMatches": ctFromShip(ctVerdictInconclusivePowered, "S5 FAIL",
			func(_ []ctVaultResult, j *ctJudgeCalibration, _ *bool) { j.LabelHashMatches = false }),
	}
}

// ctVaultResultProbes is the same census applied to the VAULT RESULT, added
// with D1/D2 because that is where the new inputs landed. Every field a vault
// hands the rule must be shown to be read.
//
// The rule's inputs are what #797 got wrong: DeltaC.N was declared, wired
// correctly by the harness, and read by NOTHING — so an exclusion policy could
// have gated power on one sample while estimating on another with no test
// noticing. A field on this struct is a claim the rule uses it.
func ctVaultResultProbes() map[string]ctProbe {
	return map[string]ctProbe{
		"Label": ctFromShip(ctVerdictUnderpowered, "vault ZZ-9",
			func(v []ctVaultResult, _ *ctJudgeCalibration, _ *bool) {
				// The label is the ONLY identifier in the rule, and its job is to
				// name the offending vault in the audit trail. Prove it does.
				v[1].Label = "ZZ-9"
				v[1].NQueries = ctPreregistered.MinQueriesPerVault - 1
				v[1].DeltaC.N = v[1].NQueries
				v[1].DeltaHP.N = v[1].NQueries
			}),
		"NQueries": ctFromShip(ctVerdictUnderpowered, "INFORMATIVE labeled held-out queries",
			func(v []ctVaultResult, _ *ctJudgeCalibration, _ *bool) {
				v[1].NQueries = ctPreregistered.MinQueriesPerVault - 1
				v[1].DeltaC.N = v[1].NQueries
				v[1].DeltaHP.N = v[1].NQueries
			}),
		"ZeroRelevanceQueries": func(t *testing.T) {
			// D1 item 6: mandatory census output. It gates nothing on purpose —
			// a vault is not underpowered for having false-positive recalls — so
			// the probe is that the number REACHES THE REPORT, before any delta.
			vaults := ctThree(ctShipVault)
			vaults[0].ZeroRelevanceQueries = 137
			got := ctDecide(vaults, ctGoodJudge(), true)
			want := "CENSUS vault A: 457 scored queries = 320 informative + 137 zero-relevance (30.0% zero-relevance)"
			// THE ORDERING HALF, and it has to match a DELTA LINE rather than the
			// first line containing the string "Delta_C". It used to do the
			// latter, and the first such line is index 3 — the census block's own
			// trailing summary sentence, which mentions Delta_C in prose. So it
			// proved a one-line margin over a line of the census itself, not that
			// the census precedes the delta SECTION. The delta section begins at
			// the "REPORTED (gates nothing)" lines, which are the first place a
			// number a reader could form a conclusion from is printed.
			const firstDeltaLine = "REPORTED (gates nothing): vault"
			idxCensus, idxDelta := -1, -1
			for i, r := range got.Reasons {
				if strings.Contains(r, want) && idxCensus < 0 {
					idxCensus = i
				}
				if strings.Contains(r, firstDeltaLine) && idxDelta < 0 {
					idxDelta = i
				}
			}
			if idxCensus < 0 {
				t.Fatalf("the zero-relevance census line is missing. Expected %q\n%s", want, got)
			}
			if idxDelta < 0 {
				t.Fatalf("no delta line %q in the report, so the ordering claim is vacuous\n%s",
					firstDeltaLine, got)
			}
			// And the pin on WHY it had to change: the first reason containing
			// the string "Delta_C" is the census block's own trailing summary
			// sentence, which mentions Delta_C in prose. Matching on that proved
			// a one-line margin over a line of the census itself.
			for i, r := range got.Reasons {
				if strings.Contains(r, "Delta_C") {
					ctExpect(t, strings.HasPrefix(r, "CENSUS"),
						"reason %d is the first to mention Delta_C and it is %q. This probe used "+
							"to take that index as 'the first delta line'; the pin exists so that "+
							"if the census stops being the thing it matches, the reason is "+
							"re-examined rather than inherited", i, r)
					break
				}
			}
			ctExpect(t, idxCensus < idxDelta,
				"the census line appears at position %d and the first delta line at %d. D1 "+
					"requires the census BEFORE any delta is printed: it is the only visible "+
					"symptom of a too-conservative judge, and a reader who has already seen the "+
					"deltas has already formed the conclusion.", idxCensus, idxDelta)
		},
		"DistinctEvents": ctFromShip(ctVerdictUnderpowered, "distinct recall events",
			func(v []ctVaultResult, _ *ctJudgeCalibration, _ *bool) {
				v[2].DistinctEvents = ctPreregistered.MinEventsPerVault - 1
			}),
		"DeltaC": ctFromShip(ctVerdictInconclusivePowered, "S1 FAIL",
			func(v []ctVaultResult, _ *ctJudgeCalibration, _ *bool) {
				// Below S1's bar and above K1's: a real effect, too small to ship
				// and too large to kill.
				for i := range v {
					v[i].DeltaC = ctDelta{Point: 0.020, CILower: 0.005, CIUpper: 0.035, N: 320}
					v[i].DeltaH = ctDelta{Point: 0.010, CILower: -0.004, CIUpper: 0.024, N: 320}
					v[i].DeltaP = ctDelta{Point: 0.008, CILower: -0.006, CIUpper: 0.022, N: 320}
					v[i].DeltaHP = ctDelta{Point: 0.015, CILower: 0.001, CIUpper: 0.029, N: 320}
				}
			}),
		"DeltaH": ctFromShip(ctVerdictInconclusivePowered, "S2 FAIL",
			func(v []ctVaultResult, _ *ctJudgeCalibration, _ *bool) {
				for i := range v {
					v[i].DeltaH = ctDelta{Point: 0.004, CILower: -0.006, CIUpper: 0.014, N: 320}
					v[i].DeltaP = ctDelta{Point: 0.003, CILower: -0.007, CIUpper: 0.013, N: 320}
					v[i].DeltaHP = ctDelta{Point: 0.006, CILower: -0.006, CIUpper: 0.018, N: 320}
				}
			}),
		"DeltaP": func(t *testing.T) {
			// PAS alone carries S2, which is only observable through DeltaP.
			vaults := ctThree(ctShipVault)
			for i := range vaults {
				vaults[i].DeltaH = ctDelta{Point: 0.004, CILower: -0.006, CIUpper: 0.014, N: 320}
				vaults[i].DeltaP = ctDelta{Point: 0.026, CILower: 0.008, CIUpper: 0.044, N: 320}
				vaults[i].MRRDeltaP = ctMeanDelta{Point: 0.02, N: vaults[i].DeltaC.N}
			}
			got := ctDecide(vaults, ctGoodJudge(), true)
			ctRequireVerdict(t, got, ctVerdictShip, "(PAS)")
		},
		"DeltaHP": ctFromKill(ctVerdictInconclusivePowered, "K2 FAIL",
			func(v []ctVaultResult) {
				// D2: the arm that IS the post-KILL configuration. Delta_H and
				// Delta_P stay at their killing values; only the JOINT ablation
				// shows the cost, which is exactly the redundancy case the four
				// marginals provably cannot reconstruct.
				v[1].DeltaHP = ctDelta{Point: 0.045, CILower: 0.020, CIUpper: 0.070, N: 340}
				v[1].DeltaC = ctDelta{Point: 0.050, CILower: 0.025, CIUpper: 0.075, N: 340}
			}),
		"DeltaCAllQueriesDiluted": func(t *testing.T) {
			// Reported, gates nothing (D1 item 2). The probe is that it reaches
			// the report AND that changing it cannot change the verdict — the
			// second half is the point: this is the number that used to gate.
			vaults := ctThree(ctShipVault)
			for i := range vaults {
				vaults[i].DeltaCAllQueriesDiluted = ctDelta{
					Point: 0.0001, CILower: -0.0005, CIUpper: 0.0007, N: 9999}
			}
			got := ctDecide(vaults, ctGoodJudge(), true)
			if got.Verdict != ctVerdictShip {
				t.Fatalf("the DILUTED all-queries Delta_C changed the verdict to %s. It gates "+
					"NOTHING: including zero-relevance queries shrinks the mean and the SE by "+
					"the same factor, so it slides the point estimate across S1's and K1's "+
					"absolute bars while the t-statistic never moves\n%s", got.Verdict, got)
			}
			ctExpectReason(t, got, "over ALL 9999 scored queries = +0.0001",
				"the diluted number is not reported — it must be visible so the exclusion is "+
					"auditable rather than asserted")
		},
		"ShuffledSeedNull": func(t *testing.T) {
			// #798-A moved this field's VERDICT-BINDING reader from K4 (a KILL
			// veto, where it was provably inert — every vault clearing K4's bar
			// already clears K2's strictly weaker one, so ShuffledSeedNull never
			// changed a verdict there; see TestCognitionTrialRule_K4IsSubsumedByK2)
			// to S6 (a SHIP conjunct). The bindingness probe therefore has to be
			// on S6, and it is a genuine one: SHIP is reachable if and only if
			// the S2-crediting vault's null was SURVIVED.
			for _, tc := range []struct {
				null ctNullResult
				want ctVerdict
				sub  string
			}{
				{ctNullSurvived, ctVerdictShip, "S6 PASS"},
				{ctNullNotRun, ctVerdictInconclusivePowered, "S6 FAIL"},
				{ctNullReproducedByShuffle, ctVerdictInconclusivePowered, "S6 FAIL"},
			} {
				vaults := ctThree(ctShipVault)
				vaults[0].ShuffledSeedNull = tc.null // vault A is the one S2 credits
				got := ctDecide(vaults, ctGoodJudge(), true)
				ctRequireVerdict(t, got, tc.want, tc.sub)
			}

			// K4's REPORT line still reads the tri-state, for audit-trail
			// continuity with the pre-registration — scoped to the K4 line
			// specifically, because the census also prints the tri-state
			// elsewhere and an unscoped search would pass even if K4 stopped
			// reading it.
			for _, tc := range []struct {
				null ctNullResult
				want string
			}{
				{ctNullNotRun, "shuffled-seed null NOT RUN"},
				{ctNullSurvived, "would additionally have cleared K4's own bar"},
				{ctNullReproducedByShuffle, "REPRODUCED by a shuffled seed"},
			} {
				vaults := ctThree(ctKillVault)
				vaults[1].DeltaHP = ctDelta{Point: 0.045, CILower: 0.020, CIUpper: 0.070, N: 340}
				vaults[1].DeltaC = ctDelta{Point: 0.050, CILower: 0.025, CIUpper: 0.075, N: 340}
				vaults[1].ShuffledSeedNull = tc.null
				got := ctDecide(vaults, ctGoodJudge(), true)
				found := false
				for _, r := range got.Reasons {
					if strings.HasPrefix(r, "K4 ") && strings.Contains(r, tc.want) {
						found = true
					}
				}
				ctExpect(t, found,
					"K4's REPORT line does not distinguish %v (want %q on its own line). A null "+
						"that was never run is not a null that was survived, even in a line that "+
						"gates nothing\n%s", tc.null, tc.want, got)
				// And the verdict must be UNAFFECTED by K4's line — it is a
				// report, not a gate. This fixture's Delta_HP (+0.045, CI lower
				// +0.020) already clears K2's unconditioned bar, so K2 blocks the
				// kill regardless of what K4 says about the null.
				ctExpect(t, got.Verdict == ctVerdictInconclusivePowered,
					"K4's tri-state moved the verdict to %s (want INCONCLUSIVE-BUT-POWERED, decided "+
						"by K2). K4 is REPORT ONLY since #798-A; it must never change what "+
						"K1/K2/K3 already decided", got.Verdict)
			}
		},
		"MRRDeltaH": ctFromShip(ctVerdictInconclusivePowered, "S4 FAIL",
			func(v []ctVaultResult, _ *ctJudgeCalibration, _ *bool) {
				for i := range v {
					v[i].MRRDeltaH = ctMeanDelta{Point: -0.02, N: v[i].DeltaC.N} // NDCG up, MRR down
				}
			}),
		"MRRDeltaP": func(t *testing.T) {
			vaults := ctThree(ctShipVault)
			for i := range vaults {
				vaults[i].DeltaH = ctDelta{Point: 0.004, CILower: -0.006, CIUpper: 0.014, N: 320}
				vaults[i].DeltaP = ctDelta{Point: 0.026, CILower: 0.008, CIUpper: 0.044, N: 320}
				vaults[i].MRRDeltaP = ctMeanDelta{Point: -0.02, N: vaults[i].DeltaC.N}
			}
			ctRequireVerdict(t, ctDecide(vaults, ctGoodJudge(), true),
				ctVerdictInconclusivePowered, "S4 FAIL")
		},
		"DeltaCByBucket": ctFromShip(ctVerdictUnderpowered, "populated weekly buckets",
			func(v []ctVaultResult, _ *ctJudgeCalibration, _ *bool) {
				full := ctBucketSeries(ctRisingBuckets(0.03))
				for i := range v {
					v[i].DeltaCByBucket = full[:ctPreregistered.MinPopulatedBuckets-1]
				}
			}),
		"OmittedBuckets": func(t *testing.T) {
			// Reported, not gating: the count is what turns "the trend was fitted
			// on 12 buckets" into "on 7 of 12", which is a materially different
			// claim.
			vaults := ctThree(ctShipVault)
			vaults[0].OmittedBuckets = []int{3, 4, 5}
			got := ctDecide(vaults, ctGoodJudge(), true)
			ctExpectReason(t, got, "3 buckets omitted as empty",
				"the omitted-bucket count is not reported")
		},
		// BINDINGNESS IS NOT ABSENCE-TYPING, AND THIS CENSUS CANNOT DECIDE THE
		// LATTER.
		//
		// The probe here used to be the UP-probe alone: move BaselineEdges to
		// 50000 and the replay fraction falls under the floor, so the field is
		// read. That is true, and it is all a bindingness census can establish —
		// it proves the field REACHES a clause. It says nothing about what the
		// clause does at the field's ABSENT value, and U4's guard was
		// `if v.BaselineEdges > 0 {...}`, so at BaselineEdges == 0 the field
		// reached a clause that then declined to fire. A field can be perfectly
		// bound and still have an unmeasured value that reads as a pass.
		//
		// Absence-typing is a SEPARATE question and needs a SEPARATE probe per
		// field, asking "what does the rule do when this was never measured?".
		// Every probe on this struct that has a meaningful absent value should
		// carry one; the DOWN-probe below is BaselineEdges'.
		"BaselineEdges": func(t *testing.T) {
			// UP: the field is read — same replayed edges, 4.2% of a bigger baseline.
			ctFromShip(ctVerdictUnderpowered, "below the 20% floor",
				func(v []ctVaultResult, _ *ctJudgeCalibration, _ *bool) {
					v[0].BaselineEdges = 50000
				})(t)
			// DOWN, and this is the one the census could not have found: at 0 the
			// ratio cannot be formed at all, and "the floor did not fire" must not
			// be the same audit trail as "the floor passed".
			ctFromShip(ctVerdictUnderpowered, "reports 0 baseline edges",
				func(v []ctVaultResult, _ *ctJudgeCalibration, _ *bool) {
					v[0].BaselineEdges = 0
				})(t)
		},
		"ReplayedEdges": ctFromShip(ctVerdictUnderpowered, "below the 20% floor",
			func(v []ctVaultResult, _ *ctJudgeCalibration, _ *bool) {
				v[0].ReplayedEdges = 100
			}),
		"UnreplayableFrac": func(t *testing.T) {
			// UP: past the ceiling.
			ctFromShip(ctVerdictUnderpowered, "in RelTypes replay cannot produce",
				func(v []ctVaultResult, _ *ctJudgeCalibration, _ *bool) {
					v[0].UnreplayableFrac = ctPreregistered.MaxUnreplayableFrac + 0.01
				})(t)
			// ABSENT: the ratio's own unmeasurable value. NaN > 0.60 is false, and
			// false is "no objection" everywhere in this rule.
			ctFromShip(ctVerdictUnderpowered, "reports an unreplayable fraction of NaN",
				func(v []ctVaultResult, _ *ctJudgeCalibration, _ *bool) {
					v[0].UnreplayableFrac = math.NaN()
				})(t)
		},
	}
}

// ctDeltaProbes is the census applied to ctDelta itself — the struct EVERY
// number the verdict rests on is carried in, and the one the walk did not reach.
// Its fields are read through four different vault fields, so "is this field
// read" is a question about the rule, not about any one delta.
func ctDeltaProbes() map[string]ctProbe {
	return map[string]ctProbe{
		// EXACTLY ONE FLOAT MOVES IN EACH OF THESE, and the interval it moves
		// inside stays coherent (the point estimate never leaves its own CI), so
		// the probe cannot be passing on an impossible fixture.
		"Point": ctFromKill(ctVerdictInconclusivePowered, "K1 FAIL",
			func(v []ctVaultResult) {
				for i := range v {
					// Above K1's point bar, still inside the killing interval
					// [-0.014, +0.018] and still nowhere near S1's.
					v[i].DeltaC.Point = ctPreregistered.KillDeltaCPoint + 0.001
				}
			}),
		"CILower": func(t *testing.T) {
			// CILower is read as "does the interval exclude zero" by S1, S2, K2
			// and K4. Probed on K2 because that is where a ONE-FIELD change is
			// coherent: the same Delta_HP point estimate, the same width, the
			// interval merely lifted off zero.
			mk := func(ciLower float64) []ctVaultResult {
				v := ctThree(ctKillVault)
				v[1].DeltaHP = ctDelta{Point: 0.045, CILower: ciLower, CIUpper: 0.091, N: 340}
				return v
			}
			if base := ctDecide(mk(-0.001), ctGoodJudge(), true); base.Verdict != ctVerdictKill {
				t.Fatalf("baseline is not KILL, so this probe proves nothing\n%s", base)
			}
			ctRequireVerdict(t, ctDecide(mk(+0.001), ctGoodJudge(), true),
				ctVerdictInconclusivePowered, "K2 FAIL")
		},
		"CIUpper": ctFromKill(ctVerdictInconclusivePowered, "K1 FAIL",
			func(v []ctVaultResult) {
				for i := range v {
					// K1 is the only clause that reads an upper bound.
					v[i].DeltaC.CIUpper = ctPreregistered.KillDeltaCCIUpper + 0.001
				}
			}),
		"N": ctFromShip(ctVerdictUnderpowered, "it must be the SAME paired series",
			func(v []ctVaultResult, _ *ctJudgeCalibration, _ *bool) {
				v[1].DeltaH.N = 0
			}),
		"SDOfDiff": func(t *testing.T) {
			// sigma_d GATES NOTHING and that is deliberate: the pre-registered n
			// was computed at sigma_d ~= 0.15, and turning an observed dispersion
			// into a gate mid-trial would be a threshold derived after seeing the
			// data. It is the input every power calculation needs, so the honest
			// probe is that it REACHES THE REPORT — the shape ctJudgeCalibration.N
			// failed, one struct over.
			vaults := ctThree(ctShipVault)
			vaults[0].DeltaC.SDOfDiff = 0.1234
			got := ctDecide(vaults, ctGoodJudge(), true)
			ctExpectReason(t, got, "sigma_d(Delta_C)=0.1234",
				"the observed paired SD is not reported anywhere. It is the sigma_d every "+
					"sample-size calculation this design makes depends on, and a number that "+
					"reaches no reader is inert whether or not it gates")
			ctExpect(t, got.Verdict == ctVerdictShip,
				"sigma_d moved the verdict to %s. It is REPORTED, not gated: deriving a "+
					"threshold from the run's own observed dispersion is exactly the "+
					"after-the-fact tuning this rule exists to prevent", got.Verdict)
		},
	}
}

// ctQuerySeriesProbes and ctBucketDeltaProbes extend the census to the two
// structs the walk did not reach. Both feed the rule through ctVaultFromSeries
// — they are UPSTREAM of ctVaultResult, so a field that dies there is invisible
// to every probe on the four structs already walked, which is the same
// blind-spot argument that put ctDelta on the list.
//
// It found one: ctBucketDelta.NQueries was declared by ctBucketDeltas,
// guaranteed non-zero by it, and read by NOTHING. It is now in S3's reported
// line, which is where it belonged — a populated bucket holding one query and
// one holding forty are not the same evidence.
func ctQuerySeriesProbes() map[string]ctProbe {
	// Every probe here drives ctVaultFromSeries, the seam the harness itself
	// calls, so what is proven is proven about the code that runs.
	build := func(s *ctQuerySeries) ctVaultResult {
		return ctVaultFromSeriesResamples("A", s, 410, 12, 7, 50)
	}
	return map[string]ctProbe{
		"Defined": func(t *testing.T) {
			s := ctSynthSeries(60)
			full := build(s)
			s.Defined[7], s.Defined[11] = false, false
			excluded := build(s)
			ctExpect(t, excluded.NQueries == full.NQueries-2,
				"flipping two Defined flags left NQueries at %d (was %d). Defined is the ONLY "+
					"thing that decides which queries enter a delta (D1)",
				excluded.NQueries, full.NQueries)
			ctExpect(t, excluded.ZeroRelevanceQueries == 2,
				"the two excluded queries were not COUNTED (%d) — D1 excludes and counts",
				excluded.ZeroRelevanceQueries)
			ctExpect(t, excluded.DeltaC.Point != full.DeltaC.Point,
				"excluding two queries did not move Delta_C at all (%v)", full.DeltaC.Point)
		},
		"NDCG": func(t *testing.T) {
			s := ctSynthSeries(60)
			full := build(s)
			for i := range s.NDCG[ctArmNameFull] {
				s.NDCG[ctArmNameFull][i] = math.Min(s.NDCG[ctArmNameFull][i]+0.10, 1)
			}
			lifted := build(s)
			ctExpect(t, lifted.DeltaC.Point > full.DeltaC.Point,
				"lifting the FULL arm's NDCG by 0.10 did not raise Delta_C (%v -> %v)",
				full.DeltaC.Point, lifted.DeltaC.Point)
		},
		"MRR": func(t *testing.T) {
			// UP: the field is read.
			s := ctSynthSeries(60)
			full := build(s)
			for i := range s.MRR[ctArmNameFull] {
				s.MRR[ctArmNameFull][i] = 0
			}
			zeroed := build(s)
			ctExpect(t, zeroed.MRRDeltaH != full.MRRDeltaH && zeroed.MRRDeltaP != full.MRRDeltaP,
				"zeroing the FULL arm's MRR moved neither MRR delta (H %v -> %v, P %v -> %v). "+
					"S4's sign agreement reads exactly these",
				full.MRRDeltaH, zeroed.MRRDeltaH, full.MRRDeltaP, zeroed.MRRDeltaP)

			// DOWN, and this is the one the UP-probe alone could not have found —
			// the same lesson BaselineEdges taught one struct over, and the reason
			// the note above says every field with a meaningful ABSENT value needs
			// its own probe. This probe WAS UP-only, and the field it guards is a
			// MEAN: ctMean(nil) == 0, so an MRR arm that was never collected
			// produced `mean(FULL) - 0`, a plausible measured number that
			// manufactured a SHIP. See ctMeanDelta and
			// TestCognitionTrialRule_UnmeasuredMRRIsNotAMeasuredZero.
			absent := ctSynthSeries(60)
			absent.MRR[ctArmNameNoHebbian] = nil
			gone := build(absent)
			ctExpect(t, gone.MRRDeltaH.N == 0,
				"an ABSENT NO-HEBBIAN MRR arm produced MRRDeltaH %+.4f over N=%d. An arm that "+
					"was never collected must not carry a count", gone.MRRDeltaH.Point, gone.MRRDeltaH.N)
			vaults := ctThree(ctShipVault)
			vaults[0].MRRDeltaH = gone.MRRDeltaH
			ctRequireVerdict(t, ctDecide(vaults, ctGoodJudge(), true),
				ctVerdictUnderpowered, "ABSENT OR LENGTH-MISMATCHED MRR series")
		},
		"Bucket": func(t *testing.T) {
			s := ctSynthSeries(60)
			full := build(s)
			for i := range s.Bucket {
				s.Bucket[i] = 0 // every query into one bucket
			}
			collapsed := build(s)
			ctExpect(t, len(collapsed.DeltaCByBucket) == 1 && len(full.DeltaCByBucket) > 1,
				"collapsing every query into bucket 0 produced %d populated buckets (was %d) — "+
					"the bucket index is not deciding the trend series",
				len(collapsed.DeltaCByBucket), len(full.DeltaCByBucket))
			ctExpect(t, len(collapsed.OmittedBuckets) == 11,
				"%d buckets reported as omitted, want 11 — an empty bucket must be OMITTED and "+
					"COUNTED, never regressed as a zero", len(collapsed.OmittedBuckets))
		},
	}
}

// ctMeanDeltaProbes is the census applied to the MRR aggregate's own struct.
// It is on the walk for the same reason ctDelta is: the fields the verdict
// rests on live inside it, and a field that dies there is invisible to every
// probe on ctVaultResult.
func ctMeanDeltaProbes() map[string]ctProbe {
	return map[string]ctProbe{
		"Point": ctFromShip(ctVerdictInconclusivePowered, "S4 FAIL",
			func(v []ctVaultResult, _ *ctJudgeCalibration, _ *bool) {
				for i := range v {
					// The N stays intact: this is a MEASURED disagreement, which
					// must remain distinguishable from an absent series below.
					v[i].MRRDeltaH.Point = -0.02
				}
			}),
		"N": ctFromShip(ctVerdictUnderpowered, "ABSENT OR LENGTH-MISMATCHED MRR series",
			func(v []ctVaultResult, _ *ctJudgeCalibration, _ *bool) {
				// The point estimate is left exactly where the SHIP fixture had
				// it — agreeing in sign — so the ONLY thing that can move the
				// verdict is the count. That is the whole claim: a number
				// without its N is not a measurement.
				v[1].MRRDeltaH.N = 0
			}),
	}
}

func ctBucketDeltaProbes() map[string]ctProbe {
	return map[string]ctProbe{
		"Bucket": func(t *testing.T) {
			// The index travels with the value so an omission cannot re-space the
			// buckets that remain. Six identical buckets at their REAL indexes
			// have slope exactly 0; renumbered densely they would too, so the
			// discriminating case is the trailing WINDOW, which is measured in
			// bucket index rather than list position.
			series, _ := ctBucketDeltas(func() [][]float64 {
				pb := make([][]float64, 12)
				for b := 0; b < 6; b++ {
					pb[b] = []float64{0.04}
				}
				return pb
			}())
			tr := ctComputeTrend(series, ctPreregistered.TrendWindow)
			ctExpect(t, tr.PopulatedInWindow == 6 && len(series) == 6,
				"%d populated in the window over %d entries — with real indexes 0..5 and a "+
					"window of %d ending at bucket 5, all six fall inside",
				tr.PopulatedInWindow, len(series), ctPreregistered.TrendWindow)
			// Now the same six values at indexes 0..5 of a series whose LAST
			// bucket is 11: the window slides and the early ones fall out.
			shifted := append(append([]ctBucketDelta(nil), series...),
				ctBucketDelta{Bucket: 11, Mean: 0.04, NQueries: 1})
			trShifted := ctComputeTrend(shifted, ctPreregistered.TrendWindow)
			ctExpect(t, trShifted.PopulatedInWindow < 7,
				"adding a bucket at index 11 left all %d entries inside a %d-wide trailing "+
					"window — the window is being measured in LIST POSITION, not bucket index, "+
					"so an omission silently re-spaces the series",
				trShifted.PopulatedInWindow, ctPreregistered.TrendWindow)
		},
		"Mean": ctFromShipDescriptiveProbe("S3 FAIL", func(v []ctVaultResult) {
			falling := make([]float64, 12)
			for i := range falling {
				falling[i] = 0.08 - 0.005*float64(i)
			}
			for i := range v {
				v[i].DeltaCByBucket = ctBucketSeries(falling)
			}
		}),
		"NQueries": func(t *testing.T) {
			// FOUND BY THIS CENSUS: carried, guaranteed non-zero, and read by
			// nothing. It gates nothing — a thin bucket is not an UNDERPOWERED
			// condition, U6 already gates how MANY buckets there are — so the
			// honest probe is that it REACHES THE REPORT, the same shape
			// ctDelta.SDOfDiff takes one struct over.
			vaults := ctThree(ctShipVault)
			series := ctBucketSeries(ctRisingBuckets(0.03))
			for i := range series {
				series[i].NQueries = 40
			}
			series[3].NQueries = 1 // one bucket resting on a single query
			for i := range vaults {
				vaults[i].DeltaCByBucket = series
			}
			got := ctDecide(vaults, ctGoodJudge(), true)
			ctExpectReason(t, got, "fitted on 441 queries across 12 buckets, thinnest bucket 1",
				"the per-bucket query count reaches no reader. A trend fitted on 12 buckets one "+
					"of which rests on a SINGLE query is a different claim from one fitted on 12 "+
					"buckets of 40, and nothing else in the report distinguishes them")
			ctExpect(t, got.Verdict == ctVerdictShip,
				"a thin bucket moved the verdict to %s. It is REPORTED, not gated: U6 already "+
					"gates how many buckets exist, and inventing a per-bucket minimum here would "+
					"be a threshold chosen after seeing the data", got.Verdict)

			// THE THINNEST BUCKET OF ALL. `thinnest == 0 ||` used the value as its
			// own uninitialised sentinel, so a bucket holding ZERO queries was
			// overwritten by the next one and [0, 40, 40, ...] reported "thinnest
			// bucket 40" — the healthiest possible claim printed over the thinnest
			// possible bucket, in the field that exists because an average hides a
			// bucket of one. ctBucketDeltas omits empty buckets so this arrives
			// only through a hand-built ctBucketDelta, which is exactly what a
			// future producer is.
			empty := ctBucketSeries(ctRisingBuckets(0.03))
			for i := range empty {
				empty[i].NQueries = 40
			}
			empty[0].NQueries = 0
			for i := range vaults {
				vaults[i].DeltaCByBucket = empty
			}
			gotEmpty := ctDecide(vaults, ctGoodJudge(), true)
			ctExpectReason(t, gotEmpty, "fitted on 440 queries across 12 buckets, thinnest bucket 0",
				"a bucket holding ZERO queries was reported as the thinnest bucket being some "+
					"other bucket's count — the value cannot be its own sentinel")
		},
	}
}

// ctFromShipDescriptiveProbe adapts ctFromShipDescriptive to the ctProbe shape.
func ctFromShipDescriptiveProbe(wantSub string, mutate func(v []ctVaultResult)) ctProbe {
	return func(t *testing.T) { ctFromShipDescriptive(t, wantSub, mutate) }
}

func TestCognitionTrialRule_EveryThresholdBinds(t *testing.T) {
	for _, tc := range []struct {
		what   string
		typ    reflect.Type
		probes map[string]ctProbe
	}{
		{"ctPreregistered", reflect.TypeOf(ctPreregistered), ctPreregisteredProbes()},
		{"ctJudgeCalibration", reflect.TypeOf(ctJudgeCalibration{}), ctJudgeProbes()},
		{"ctVaultResult", reflect.TypeOf(ctVaultResult{}), ctVaultResultProbes()},
		{"ctDelta", reflect.TypeOf(ctDelta{}), ctDeltaProbes()},
		{"ctMeanDelta", reflect.TypeOf(ctMeanDelta{}), ctMeanDeltaProbes()},
		// The two UPSTREAM structs. They reach the rule through
		// ctVaultFromSeries, so a field that dies between the harness and
		// ctVaultResult is invisible to every probe above.
		{"ctQuerySeries", reflect.TypeOf(ctQuerySeries{}), ctQuerySeriesProbes()},
		{"ctBucketDelta", reflect.TypeOf(ctBucketDelta{}), ctBucketDeltaProbes()},
	} {
		seen := map[string]bool{}
		for i := 0; i < tc.typ.NumField(); i++ {
			name := tc.typ.Field(i).Name
			seen[name] = true
			probe, ok := tc.probes[name]
			if !ok {
				t.Errorf("%s.%s has NO PROBE. Every field of this struct must be shown to bind: "+
					"add a case that pushes exactly this input past its bound and asserts the "+
					"verdict changes. A declared-and-never-read gate (which is how "+
					"ctJudgeCalibration.N shipped) is worse than no gate, because it reads as "+
					"protection.", tc.what, name)
				continue
			}
			// PRESENCE IS NOT BINDINGNESS. The census caught a MISSING probe and
			// happily accepted an EMPTY one: an `InertNewField` plus a
			// `func(t *testing.T) {}` passed the whole walk, which is the
			// declared-and-never-read shape this test exists to find, one level up.
			//
			// So a probe must, at minimum, ENTER THE RULE and ASSERT SOMETHING
			// ABOUT WHAT CAME BACK. Both are counted; neither is inferrable from
			// the probe's source. What this still cannot decide is whether the
			// assertion is vacuous — `ctExpect(t, true, "")` after a ctDecide call
			// passes — and no in-process check can. The decidable form is mutation
			// testing (perturb the rule, require the probe to fail), which is a
			// separate harness and is deliberately NOT built here.
			//
			// The counters are read INSIDE the subtest, not around t.Run: a
			// subtest excluded by -run never executes its body, and a check
			// outside would then report every filtered-out probe as inert.
			// (Found by running exactly that command.)
			what, field := tc.what, name
			t.Run(what+"."+field, func(t *testing.T) {
				ruleBefore := ctRuleCalls.Load()
				assertBefore := ctProbeAssertions.Load()
				probe(t)
				if ctRuleCalls.Load() == ruleBefore {
					t.Errorf("%s.%s's probe NEVER ENTERED THE RULE. A probe that does not call "+
						"ctDecide or any rule function cannot be showing that this field binds — "+
						"an empty probe body passes every other check in this census.", what, field)
				}
				if ctProbeAssertions.Load() == assertBefore {
					t.Errorf("%s.%s's probe made NO ASSERTION (via ctRequireVerdict / "+
						"ctExpectReason / ctExpect). It called the rule and discarded the answer, "+
						"which proves the rule does not panic and nothing else.", what, field)
				}
			})
		}
		for name := range tc.probes {
			if !seen[name] {
				t.Errorf("%s has a probe for %q, which is not a field of the struct — the probe "+
					"list has drifted", tc.what, name)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// ABSENT BUCKETS ARE NOT MEASURED ZEROS.
//
// The trend series was a dense []float64 built as ctMean(perBucket[b]), and
// ctMean(nil) is 0 — so a week with no evaluated query entered the OLS
// regression as a Delta_C of exactly zero. This test pins the demonstration
// that made the defect undeniable: the SAME six real buckets yield a slope of
// +0.0046 (CI lower > 0, i.e. S3's non-decreasing gate PASSING) or -0.0046
// (failing it) depending only on WHERE the six absent buckets are padded in.
// The conclusion is being decided by data that does not exist.
// ---------------------------------------------------------------------------

func TestCognitionTrialRule_EmptyBucketsAreOmittedNotScoredAsZero(t *testing.T) {
	const real = 0.04
	// Six buckets with one query each at +0.04; six with no queries at all.
	sparseLate := make([][]float64, 12)  // populated 0..5, absent 6..11
	sparseEarly := make([][]float64, 12) // absent 0..5, populated 6..11
	for b := 0; b < 6; b++ {
		sparseLate[b] = []float64{real}
		sparseEarly[b+6] = []float64{real}
	}

	// --- 1. what PADDING does, which is the defect ---------------------------
	pad := func(perBucket [][]float64) []ctBucketDelta {
		out := make([]ctBucketDelta, len(perBucket))
		for b, vals := range perBucket {
			out[b] = ctBucketDelta{Bucket: b, Mean: ctMean(vals), NQueries: len(vals)}
		}
		return out
	}
	up := ctComputeTrend(pad(sparseEarly), ctPreregistered.TrendWindow)
	down := ctComputeTrend(pad(sparseLate), ctPreregistered.TrendWindow)
	// 0.72/143 exactly: sum((x-5.5)*(y-0.02)) / sum((x-5.5)^2) for
	// y = six 0s then six 0.04s. Every term of the numerator is contributed by
	// the SIX ZEROS, which are not measurements.
	if math.Abs(up.Slope-0.0050350) > 1e-6 || math.Abs(down.Slope+0.0050350) > 1e-6 {
		t.Fatalf("the padding demonstration has drifted: slopes %+.7f / %+.7f, expected "+
			"+0.0050350 / -0.0050350", up.Slope, down.Slope)
	}
	if up.SlopeCILower <= 0 {
		t.Fatalf("padded-early slope CI lower %+.6f — the demonstration is meant to show a "+
			"CI that EXCLUDES zero on fabricated data", up.SlopeCILower)
	}
	if !(up.SlopeCILower >= ctPreregistered.TrendSlopeCILower &&
		down.SlopeCILower < ctPreregistered.TrendSlopeCILower) {
		t.Fatalf("padding did not flip S3's slope gate (%+.6f vs %+.6f against floor %+.6f) — "+
			"the case this test exists for is no longer the case",
			up.SlopeCILower, down.SlopeCILower, ctPreregistered.TrendSlopeCILower)
	}

	// --- 2. what OMISSION does, which is the fix -----------------------------
	for _, tc := range []struct {
		name      string
		perBucket [][]float64
		wantFirst int
	}{
		{"absent buckets early", sparseEarly, 6},
		{"absent buckets late", sparseLate, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			series, omitted := ctBucketDeltas(tc.perBucket)
			if len(series) != 6 || len(omitted) != 6 {
				t.Fatalf("%d populated / %d omitted, want 6/6 — an empty bucket is being "+
					"carried into the series", len(series), len(omitted))
			}
			if series[0].Bucket != tc.wantFirst {
				t.Errorf("first populated bucket index %d, want %d — omitting a bucket must "+
					"not re-space the ones that remain", series[0].Bucket, tc.wantFirst)
			}
			for _, bd := range series {
				if bd.NQueries == 0 {
					t.Errorf("bucket %d survived with 0 queries", bd.Bucket)
				}
			}
			tr := ctComputeTrend(series, ctPreregistered.TrendWindow)
			if math.Abs(tr.Slope) > 1e-12 {
				t.Errorf("slope %+.9f over six identical +%.2f buckets, want exactly 0 — the "+
					"fabricated slope is coming from somewhere", tr.Slope, real)
			}
			if tr.PopulatedInWindow != 6 {
				t.Errorf("PopulatedInWindow = %d, want 6", tr.PopulatedInWindow)
			}

			// --- 3. and the verdict is UNDERPOWERED, not a trend -------------
			vaults := ctThree(ctShipVault)
			for i := range vaults {
				vaults[i].DeltaCByBucket = series
				vaults[i].OmittedBuckets = omitted
			}
			got := ctDecide(vaults, ctGoodJudge(), true)
			if got.Verdict == ctVerdictShip {
				t.Fatalf("SHIPPED on a trend fitted over %d of 12 weekly buckets\n%s",
					len(series), got)
			}
			ctRequireVerdict(t, got, ctVerdictUnderpowered, "populated weekly buckets")
		})
	}
}

// ---------------------------------------------------------------------------
// U4: A BASELINE OF ZERO EDGES IS AN ABSENCE OF MEASUREMENT, NOT A PASSED FLOOR.
//
// This is the FIFTH instance of this branch's signature defect — "we did not
// look" rendering as "we looked and found nothing" — and it was in the one U
// gate nobody audited for it. The clause read
//
//	if v.BaselineEdges > 0 { ...the replay-fraction floor... }
//
// so a vault whose baseline association graph is empty, or whose edge count
// simply failed to be collected, SKIPPED the floor entirely. And the producer
// (ctUnreplayableFrac) returned a plausible 0 for a 0/0 ratio, so the second
// U4 clause went quiet at exactly the same moment. Both halves of U4 fall
// silent together, and the audit trail is byte-identical to a vault that
// genuinely replayed everything.
//
// Nothing upstream catches it: U2 gates DistinctEvents >= 200, which counts
// RECALL EVENTS, not edges — a vault can plausibly have 200+ recall events and
// a near-empty association graph, which is precisely the vault whose
// reconstruction is least trustworthy.
//
// The two vault sets below are identical in every field U4 reads EXCEPT that
// one measured its baseline and the other did not, and that difference alone
// must be the difference between a verdict and no verdict.
// ---------------------------------------------------------------------------

func TestCognitionTrialRule_ZeroBaselineEdgesIsAnAbsenceOfMeasurement(t *testing.T) {
	// The control: a vault set that genuinely replayed EVERY baseline edge. U4
	// has nothing to say, and the verdict is SHIP.
	measured := ctThree(ctShipVault)
	for i := range measured {
		measured[i].BaselineEdges = 5000
		measured[i].ReplayedEdges = 5000
		measured[i].UnreplayableFrac = 0
	}
	control := ctDecide(measured, ctGoodJudge(), true)
	if control.Verdict != ctVerdictShip {
		t.Fatalf("the control (a full replay) is not SHIP, so the comparison below proves "+
			"nothing\n%s", control)
	}
	for _, r := range control.Reasons {
		if strings.Contains(r, "U4:") {
			t.Fatalf("the control emitted a U4 objection %q — it replayed every edge", r)
		}
	}

	// THE ZERO-BASELINE CLAUSE, ISOLATED, and it has to be isolated to prove
	// anything. This case used to set UnreplayableFrac = NaN as well — faithful
	// to what the harness produces, since ctUnreplayableFrac forms 0/0 there and
	// the two absences have one cause — but that made the SIBLING clause
	// (ctNonFiniteScalar) sufficient to reach UNDERPOWERED on its own. With the
	// zero-baseline clause reverted to `if v.BaselineEdges > 0 {`, the test still
	// failed, but only on two ctExpectReason SUBSTRINGS: a reword of either
	// message would have turned it green with the clause deleted. A pin that REDs
	// on its strings rather than on its verdict is not a pin.
	//
	// So the fraction here is FINITE and comfortably inside the ceiling. The only
	// objection U4 can raise is the one under test.
	unmeasured := ctThree(ctShipVault)
	for i := range unmeasured {
		unmeasured[i].BaselineEdges = 0
		unmeasured[i].UnreplayableFrac = 0.30
	}
	got := ctDecide(unmeasured, ctGoodJudge(), true)
	if got.Verdict == ctVerdictShip {
		t.Fatalf("SHIPPED with ZERO baseline edges on every vault. The replay-fraction floor "+
			"cannot be formed at all, so it was never applied, and the audit trail is "+
			"byte-identical to the control that replayed all 5000\n%s", got)
	}
	ctRequireVerdict(t, got, ctVerdictUnderpowered, "0 baseline edges")
	ctExpectReason(t, got, "ABSENCE OF MEASUREMENT",
		"U4's zero-baseline clause must say what it is in the same language D1's clauses use, "+
			"or a reader cannot tell an unmeasured reconstruction from a complete one")

	// ONE vault is enough: U4 is per-vault, and a reconstruction nobody measured
	// cannot be averaged away by two that were.
	one := ctThree(ctShipVault)
	one[2].BaselineEdges = 0
	one[2].UnreplayableFrac = 0.30
	gotOne := ctDecide(one, ctGoodJudge(), true)
	if gotOne.Verdict == ctVerdictShip {
		t.Fatalf("SHIPPED with one vault's baseline unmeasured\n%s", gotOne)
	}
	ctRequireVerdict(t, gotOne, ctVerdictUnderpowered, "vault C reports 0 baseline edges")

	// The harness's OWN shape, kept rather than dropped: both absences together,
	// because that is what a real unmeasured reconstruction looks like coming out
	// of ctUnreplayableFrac. It proves the two clauses coexist without one
	// swallowing the other's message; it is NOT what proves the zero-baseline
	// clause exists.
	together := ctThree(ctShipVault)
	for i := range together {
		together[i].BaselineEdges = 0
		together[i].UnreplayableFrac = math.NaN()
	}
	gotBoth := ctDecide(together, ctGoodJudge(), true)
	ctRequireVerdict(t, gotBoth, ctVerdictUnderpowered, "0 baseline edges")
	ctExpectReason(t, gotBoth, "unreplayable fraction of NaN",
		"the sibling clause's message disappeared when both absences arrived together — a "+
			"reader must see BOTH causes, not whichever one happened to be appended first")
}

// U4, the same shape one field over: NaN > 0.60 is FALSE, and FALSE means "no
// objection" everywhere in this rule. This is the exact class the U3
// delta-arithmetic clause exists for, applied to the one non-delta float the
// rule compares against a threshold.
//
// It is not reachable through today's harness with a non-zero baseline —
// ctUnreplayableFrac's only non-finite output is the 0/0 case, which the clause
// above already catches. It is here for the same reason the U3 delta clause is:
// "unreachable today" is a property that has to be re-derived after every edit,
// and nothing DOWNSTREAM rejected one.
func TestCognitionTrialRule_UnmeasurableUnreplayableFracIsNotNoObjection(t *testing.T) {
	for _, poison := range []struct {
		what string
		val  float64
	}{{"NaN", math.NaN()}, {"+Inf", math.Inf(1)}, {"-Inf", math.Inf(-1)}} {
		t.Run(poison.what, func(t *testing.T) {
			vaults := ctThree(ctShipVault)
			// The baseline IS measured, so the zero-baseline clause cannot be the
			// thing that fires: the only objection available is the ratio itself.
			vaults[0].UnreplayableFrac = poison.val
			got := ctDecide(vaults, ctGoodJudge(), true)
			if got.Verdict == ctVerdictShip {
				t.Fatalf("a %s unreplayable fraction SHIPPED. Every comparison against %s is "+
					"false and false means 'no objection', so the ceiling silently stopped "+
					"applying\n%s", poison.what, poison.what, got)
			}
			ctRequireVerdict(t, got, ctVerdictUnderpowered, "unreplayable fraction")
		})
	}
	// The control: a REAL 0.0 is a measurement (every baseline edge is one replay
	// can produce) and must pass, or the fix would have replaced a silent
	// no-objection with a false objection.
	vaults := ctThree(ctShipVault)
	for i := range vaults {
		vaults[i].UnreplayableFrac = 0
	}
	got := ctDecide(vaults, ctGoodJudge(), true)
	ctRequireVerdict(t, got, ctVerdictShip, "")
}

// ---------------------------------------------------------------------------
// The two ways the judge gate could be passed by a sample that measures nothing.
// Both were DEMONSTRATED against the first version of this rule, which declared
// and documented ctJudgeCalibration.N and then never read it.
// ---------------------------------------------------------------------------

// ONE agreeing pair yields kappa=1.000, fpr=0.000 and — before N was bound —
// a clean SHIP. §3c pre-registers a minimum of 150 adjudicated pairs; a gate
// that is declared and never read is not a gate.
func TestCognitionTrialRule_OneAdjudicatedPairIsNotACalibration(t *testing.T) {
	judge := ctJudgeCalibration{
		Ran: true, Kappa: 1.0, FPR: 0.0, N: 1,
		HumanRelevant: 1, HumanIrrelevant: 0, LabelHashMatches: true,
	}
	got := ctDecide(ctThree(ctShipVault), judge, true)
	if got.Verdict == ctVerdictShip {
		t.Fatalf("SHIPPED on a judge calibration of ONE pair — the pre-registered minimum of "+
			"%d adjudicated pairs is not binding\n%s", ctPreregistered.MinAdjudicatedPairs, got)
	}
	ctRequireVerdict(t, got, ctVerdictUnderpowered, "U1: the judge-calibration subsample has 1")
}

// 200 pairs where EVERY pair is relevant to both judge and human also yields
// kappa=1.000 fpr=0.000 — kappa via the 1-pe==0 branch, FPR via an empty
// denominator. Perfect agreement carrying zero discriminative information.
// §3c's planted negatives exist to prevent exactly this; nothing asserted one
// was present. The arithmetic is computed here rather than asserted by hand so
// the demonstration cannot drift from the implementation.
func TestCognitionTrialRule_DegenerateAdjudicationSampleIsNotACalibration(t *testing.T) {
	all := func(g int, n int) []int {
		out := make([]int, n)
		for i := range out {
			out[i] = g
		}
		return out
	}
	for _, tc := range []struct {
		name          string
		judge, human  []int
		wantSubstring string
	}{
		{"every pair relevant", all(3, 200), all(3, 200),
			"U1: the judge-calibration subsample contains no human-IRRELEVANT pair"},
		{"every pair irrelevant", all(0, 200), all(0, 200),
			"U1: the judge-calibration subsample contains no human-RELEVANT pair"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			k, fpr, n, hRel, hIrrel := ctCohensKappa(tc.judge, tc.human)
			if k != 1 || fpr != 0 || n != 200 {
				t.Fatalf("setup: kappa=%v fpr=%v n=%v — this case is meant to be the "+
					"degenerate kappa=1 fpr=0 one", k, fpr, n)
			}
			judge := ctJudgeCalibration{
				Ran: true, Kappa: k, FPR: fpr, N: n,
				HumanRelevant: hRel, HumanIrrelevant: hIrrel, LabelHashMatches: true,
			}
			got := ctDecide(ctThree(ctShipVault), judge, true)
			if got.Verdict == ctVerdictShip {
				t.Fatalf("SHIPPED on a degenerate adjudication sample (kappa=%.3f fpr=%.3f, "+
					"every pair on one side of the binarization) — perfect agreement that "+
					"measures nothing\n%s", k, fpr, got)
			}
			ctRequireVerdict(t, got, ctVerdictUnderpowered, tc.wantSubstring)
		})
	}
}

// A wide interval is NOT a kill — it is U5. Pinned because rounding a wide
// interval down to "no effect" is the most inviting mistake in the whole rule.
func TestCognitionTrialRule_WideIntervalIsNotAKill(t *testing.T) {
	vaults := ctThree(ctKillVault)
	for i := range vaults {
		vaults[i].DeltaC = ctDelta{Point: 0.005, CILower: -0.060, CIUpper: 0.070, N: 340}
		vaults[i].DeltaHP = ctDelta{Point: 0.004, CILower: -0.060, CIUpper: 0.068, N: 340}
	}
	ctRequireVerdict(t, ctDecide(vaults, ctGoodJudge(), true), ctVerdictUnderpowered, "U5")
}

// A vault where the prior is significantly NEGATIVE blocks S1 even if two other
// vaults clear the bar.
func TestCognitionTrialRule_SignificantlyNegativeVaultBlocksShip(t *testing.T) {
	vaults := ctThree(ctShipVault)
	// A vault where the whole prior HURTS. Its mechanism deltas move with it —
	// a vault cannot simultaneously report that the prior costs it 0.04 and that
	// each individual mechanism buys it 0.03, and the U3 additivity bounds now
	// say so, which is why this fixture had to become internally coherent.
	vaults[2].DeltaC = ctDelta{Point: -0.04, CILower: -0.06, CIUpper: -0.02, N: 320}
	vaults[2].DeltaH = ctDelta{Point: -0.05, CILower: -0.07, CIUpper: -0.03, N: 320}
	vaults[2].DeltaP = ctDelta{Point: -0.06, CILower: -0.08, CIUpper: -0.04, N: 320}
	vaults[2].DeltaHP = ctDelta{Point: -0.045, CILower: -0.065, CIUpper: -0.025, N: 320}
	vaults[2].MRRDeltaH = ctMeanDelta{Point: -0.05, N: vaults[2].DeltaC.N}
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
	must := func(v float64, ok bool) float64 {
		t.Helper()
		if !ok {
			t.Fatalf("NDCG unexpectedly undefined on a pool containing relevant documents")
		}
		return v
	}

	perfect := must(ctNDCGAt10([]string{"m1", "m2", "m3", "m4"}, grades))
	if math.Abs(perfect-1.0) > 1e-12 {
		t.Errorf("ideal order NDCG@10 = %.12f, want 1", perfect)
	}
	reversed := must(ctNDCGAt10([]string{"m4", "m3", "m2", "m1"}, grades))
	if reversed >= perfect {
		t.Errorf("reversed order scored %.6f >= ideal %.6f", reversed, perfect)
	}

	// An arm that returns FEWER items must not win by shortening its list: the
	// IDCG denominator comes from the pooled label set, not from the arm.
	truncated := must(ctNDCGAt10([]string{"m1"}, grades))
	if truncated >= perfect {
		t.Errorf("a one-item list scored %.6f >= the full ideal %.6f — the denominator is "+
			"being taken from the arm instead of the pool", truncated, perfect)
	}

	// An unlabeled item occupies its rank at gain 0 rather than being skipped.
	withNoise := must(ctNDCGAt10([]string{"unlabeled-x", "m1", "m2", "m3"}, grades))
	if withNoise >= perfect {
		t.Errorf("an unlabeled item at rank 1 did not cost anything (%.6f vs %.6f) — it is "+
			"being skipped, which silently promotes everything after it", withNoise, perfect)
	}
}

// ---------------------------------------------------------------------------
// D1, part 1: AN UNDEFINED NDCG IS NOT A ZERO, AND IS NOT REPRESENTABLE AS ONE.
//
// A query whose pooled label set is ALL GRADE 0 has IDCG 0, so NDCG
// is 0/0. The function used to say "NDCG is undefined" in a comment and then
// return 0 — the third instance in this file family of undefined written as a
// measured zero (ctMean(nil) padding trend buckets, fixed by U6; FTS
// `idf<=0 -> continue`, fixed by COG-24's idfMax; this one).
// ---------------------------------------------------------------------------

func TestCognitionTrialMetrics_UndefinedNDCGIsNotAZero(t *testing.T) {
	v, ok := ctNDCGAt10([]string{"m4"}, map[string]int{"m4": 0})
	if ok {
		t.Fatalf("an all-irrelevant pool reported a DEFINED NDCG of %v", v)
	}
	if !math.IsNaN(v) {
		t.Errorf("undefined NDCG returned %v, want NaN. A caller that discards ok with `_` must "+
			"get a value that POISONS any mean it enters, not a plausible zero that silently "+
			"dilutes it toward the KILL side of an absolute bar.", v)
	}
	// The empty pool is the same case and must answer the same way.
	if v, ok := ctNDCGAt10(nil, nil); ok || !math.IsNaN(v) {
		t.Errorf("empty pool returned (%v, %v), want (NaN, false)", v, ok)
	}
	// And a pool with ANY positive grade is defined, including grade 1, which is
	// below the MRR binarization but still carries NDCG gain.
	if v, ok := ctNDCGAt10([]string{"m3"}, map[string]int{"m3": 1}); !ok || v != 1 {
		t.Errorf("a grade-1-only pool returned (%v, %v), want (1, true) — grade 1 is not "+
			"relevant for MRR but it IS a defined NDCG", v, ok)
	}
	// THE CASE THAT SEPARATES THE TRIGGER FROM ITS TEMPTING PARAPHRASE, executed
	// rather than argued. ctNDCGAt10's comment said the undefined case was "the
	// pooled label set contains no relevant document"; it is not. Relevance is
	// grade >= 2, but ctGains gives grade 1 a gain of 1, so a pool of four
	// grade-1 documents contains NO RELEVANT DOCUMENT AT ALL and still has a
	// perfectly well-defined IDCG — it is retained, scored, and it discriminates
	// between arms. The undefined set is strictly smaller than the not-relevant
	// set, and D1 excludes only the former.
	allOnes := map[string]int{"m1": 1, "m2": 1, "m3": 1, "m4": 1}
	if got := ctMRR([]string{"m1", "m2", "m3", "m4"}, allOnes); got != 0 {
		t.Fatalf("MRR = %v on an all-grade-1 pool, want 0 — the premise is that this pool "+
			"contains no RELEVANT document", got)
	}
	partial, ok := ctNDCGAt10([]string{"m1", "m2"}, allOnes)
	if !ok {
		t.Fatalf("an all-grade-1 pool reported an UNDEFINED NDCG. It contains no relevant " +
			"document but every grade is positive, so IDCG > 0 and the query must be RETAINED")
	}
	if !(partial > 0 && partial < 1) {
		t.Errorf("all-grade-1 pool, 2 of 4 returned: NDCG %v, want a value strictly inside "+
			"(0,1) — the arm is genuinely being discriminated, not degenerately scored", partial)
	}
	t.Logf("all-grade-1 pool (NO relevant document), 2 of 4 returned: NDCG@10 = %.4f, "+
		"defined = %v — retained and scored", partial, ok)
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
	rising := ctComputeTrend(ctBucketSeries(ctRisingBuckets(0.03)), 10)
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
	fall := ctComputeTrend(ctBucketSeries(falling), 10)
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
	k, fpr, n, _, _ := ctCohensKappa(judge, human)
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
	if k, _, _, _, _ := ctCohensKappa([]int{3, 3, 0, 0}, []int{3, 3, 0, 0}); math.Abs(k-1) > 1e-12 {
		t.Errorf("kappa on perfect agreement = %v, want 1", k)
	}
	if k, _, _, _, _ := ctCohensKappa([]int{3, 3}, []int{3, 3}); math.IsNaN(k) || k != 1 {
		t.Errorf("kappa on a degenerate all-relevant sample = %v, want 1 and never NaN", k)
	}
	if _, _, n, _, _ := ctCohensKappa(nil, nil); n != 0 {
		t.Errorf("empty input n = %d, want 0", n)
	}
}
