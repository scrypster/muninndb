//go:build localassets || cognitiontrial

package engine

import (
	"math"
	"regexp"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// D1 / D2 / D3 — the acceptance rule's exclusion policy, its fifth arm, and the
// demotion of S3.
//
// PRIVACY: arithmetic and verdict strings only. No vault, no query text, no
// identifier.
// ---------------------------------------------------------------------------

// ctSynthSeries builds n informative queries across all five arms, ordered
// CONTENT-MATCH-ONLY <= BASE-LEVEL-ONLY <= {NO-HEBBIAN, NO-PAS} <= FULL so the
// ablation-additivity bounds hold by construction, with a mean Delta_C of about
// +0.066 and realistic per-query dispersion.
//
// It is deterministic: a property test that resamples differently per run is a
// property test that flakes.
func ctSynthSeries(n int) *ctQuerySeries {
	s := ctNewQuerySeries(ctArmNames)
	for i := 0; i < n; i++ {
		base := float64((i*37)%100) / 100.0 * 0.8
		jitter := func(k int) float64 { return float64((i*k)%21) / 21.0 }
		content := base
		baseOnly := content + 0.020 + 0.010*jitter(53)
		noHeb := baseOnly + 0.012 + 0.010*jitter(29)
		noPAS := baseOnly + 0.018 + 0.010*jitter(31)
		full := baseOnly + 0.040 + 0.020*jitter(41)
		for arm, v := range map[string]float64{
			ctArmNameContentOnly:   content,
			ctArmNameBaseLevelOnly: baseOnly,
			ctArmNameNoHebbian:     noHeb,
			ctArmNameNoPAS:         noPAS,
			ctArmNameFull:          full,
		} {
			s.NDCG[arm] = append(s.NDCG[arm], math.Min(v, 1))
			// MRR moves with NDCG so S4's sign agreement holds.
			s.MRR[arm] = append(s.MRR[arm], math.Min(v, 1))
		}
		s.Defined = append(s.Defined, true)
		s.Bucket = append(s.Bucket, i%12)
	}
	return s
}

// ctDilute appends m ZERO-RELEVANCE queries: NDCG undefined for every arm, so
// the paired difference is exactly 0 whatever the arms did.
func ctDilute(s *ctQuerySeries, m int) *ctQuerySeries {
	out := &ctQuerySeries{
		Defined: append([]bool(nil), s.Defined...),
		Bucket:  append([]int(nil), s.Bucket...),
		NDCG:    map[string][]float64{},
		MRR:     map[string][]float64{},
	}
	for arm := range s.NDCG {
		out.NDCG[arm] = append([]float64(nil), s.NDCG[arm]...)
		out.MRR[arm] = append([]float64(nil), s.MRR[arm]...)
	}
	for i := 0; i < m; i++ {
		for arm := range out.NDCG {
			out.NDCG[arm] = append(out.NDCG[arm], math.NaN())
			out.MRR[arm] = append(out.MRR[arm], 0)
		}
		out.Defined = append(out.Defined, false)
		out.Bucket = append(out.Bucket, i%12)
	}
	return out
}

// ctVaultFromSynth completes a vault result with the U4 reconstruction numbers
// the series does not carry.
func ctVaultFromSynth(label string, s *ctQuerySeries) ctVaultResult {
	v := ctVaultFromSeries(label, s, 410, 12, 7)
	v.BaselineEdges = 5000
	v.ReplayedEdges = 2100
	v.UnreplayableFrac = 0.30
	return v
}

// ctThreeFromSynth is A/B/C from ONE series at the PRE-REGISTERED resample
// count, built once and relabelled.
//
// The relabelling is what makes the pre-registered count affordable here. This
// test's second half runs the production builder over series up to 11n long,
// and every one of its intervals is drawn at ctPreregistered.BootstrapResamples
// — because U5 reads DeltaC's half-width and the verdict this test asserts
// invariance of depends on it. Cutting the resample count instead would have
// been cutting the very thing under test; cutting the redundant rebuild costs
// nothing. See ctThreeFromSeries for why relabelling is exact, and
// TestCognitionTrialRule_ThreeFromSeriesRelabelIsExact for the pin.
func ctThreeFromSynth(s *ctQuerySeries) []ctVaultResult {
	v := ctVaultFromSynth("A", s)
	out := make([]ctVaultResult, 0, 3)
	for _, label := range []string{"A", "B", "C"} {
		c := v
		c.Label = label
		out = append(out, c)
	}
	return out
}

// ---------------------------------------------------------------------------
// D1: DILUTION INVARIANCE.
//
// Appending zero-relevance queries must not move the verdict. Stated as a
// PROPERTY over m rather than as one example, because the general form catches
// the next diluting metric someone adds rather than only this one.
//
// Why the defect was invisible: a point mass at zero shrinks the mean by the
// informative fraction p AND the standard error by the same factor, so the
// t-statistic is INVARIANT while the point estimate slides across the absolute
// bars S1 (>= 0.03) and K1 (< 0.01) use. U5 gates the CI HALF-WIDTH, which only
// ever shrinks under dilution — so the one gate that could have caught it moves
// the wrong way by construction. There was never a guard.
// ---------------------------------------------------------------------------

func TestCognitionTrialRule_DilutionInvariance(t *testing.T) {
	const n = 320
	src := ctSynthSeries(n)

	// --- 1. what INCLUSION does, which is the defect -------------------------
	// Computed with the production bootstrap over the ZERO-FILLED series — the
	// policy that stood until D1 — so the demonstration cannot drift from what
	// the rule would have done.
	var includedVerdicts []ctVerdict
	for _, m := range []int{0, n, 3 * n, 10 * n} {
		s := ctDilute(src, m)
		d := ctPairedBootstrap(
			ctZeroFilled(s.Defined, s.NDCG[ctArmNameFull]),
			ctZeroFilled(s.Defined, s.NDCG[ctArmNameContentOnly]),
			ctPreregistered.BootstrapResamples, 7)
		vaults := ctThree(ctShipVault)
		for i := range vaults {
			vaults[i].NQueries = d.N
			vaults[i].DeltaC = d
			vaults[i].DeltaH = ctDelta{Point: 0.004, CILower: -0.006, CIUpper: 0.014, N: d.N}
			vaults[i].DeltaP = ctDelta{Point: 0.003, CILower: -0.007, CIUpper: 0.013, N: d.N}
			vaults[i].DeltaHP = ctDelta{Point: math.Min(0.006, d.Point),
				CILower: -0.006, CIUpper: 0.020, N: d.N}
		}
		got := ctDecide(vaults, ctGoodJudge(), true)
		t.Logf("INCLUDE m=%-5d n=%-5d Delta_C=%+.4f hw=%.5f -> %s",
			m, d.N, d.Point, d.halfWidth(), got.Verdict)
		includedVerdicts = append(includedVerdicts, got.Verdict)
		if d.halfWidth() > ctPreregistered.MaxCIHalfWidth {
			t.Errorf("m=%d: half-width %.5f exceeded U5's %.2f gate. The demonstration depends on "+
				"U5 being UNABLE to catch dilution; if it can, this test is no longer the case "+
				"it exists for", m, d.halfWidth(), ctPreregistered.MaxCIHalfWidth)
		}
	}
	same := true
	for _, v := range includedVerdicts {
		if v != includedVerdicts[0] {
			same = false
		}
	}
	if same {
		t.Fatalf("dilution under the INCLUDE policy did not move the verdict (%v). This test's "+
			"first half exists to pin the defect; if the synthetic series no longer exhibits it, "+
			"the second half proves nothing", includedVerdicts)
	}

	// --- 2. what EXCLUSION does, which is the fix ----------------------------
	baseVaults := ctThreeFromSynth(src)
	base := ctDecide(baseVaults, ctGoodJudge(), true)
	t.Logf("EXCLUDE m=0     Delta_C=%+.4f over %d informative -> %s",
		baseVaults[0].DeltaC.Point, baseVaults[0].NQueries, base.Verdict)
	for _, m := range []int{n, 3 * n, 10 * n} {
		s := ctDilute(src, m)
		vaults := ctThreeFromSynth(s)
		got := ctDecide(vaults, ctGoodJudge(), true)
		t.Logf("EXCLUDE m=%-5d Delta_C=%+.4f over %d informative (%d zero-relevance) -> %s",
			m, vaults[0].DeltaC.Point, vaults[0].NQueries, vaults[0].ZeroRelevanceQueries,
			got.Verdict)
		if got.Verdict != base.Verdict {
			t.Errorf("m=%d moved the verdict from %s to %s. Appending queries with a paired "+
				"difference of exactly zero must not change what the trial concludes\n%s",
				m, base.Verdict, got.Verdict, got)
		}
		if vaults[0].NQueries != n {
			t.Errorf("m=%d: %d informative queries, want %d — the exclusion is not exact",
				m, vaults[0].NQueries, n)
		}
		if vaults[0].ZeroRelevanceQueries != m {
			t.Errorf("m=%d: counted %d zero-relevance queries — the excluded ones must be "+
				"COUNTED, not merely dropped", m, vaults[0].ZeroRelevanceQueries)
		}
		// The ESTIMATE itself is invariant too, not just the verdict — a verdict
		// that happened to survive a moving point estimate would be luck.
		if math.Abs(vaults[0].DeltaC.Point-baseVaults[0].DeltaC.Point) > 1e-12 {
			t.Errorf("m=%d: Delta_C moved from %+.9f to %+.9f under dilution",
				m, baseVaults[0].DeltaC.Point, vaults[0].DeltaC.Point)
		}
	}
}

// ---------------------------------------------------------------------------
// D1: A VAULT THAT MEASURED NOTHING DOES NOT GET A VOTE.
//
// ctPairedBootstrap(nil, nil, ...) returns the zero value: Point 0, CI [0,0],
// N 0, half-width 0. That makes K1's clause TRUE (0 < +0.01 and 0 < +0.03) and
// U5's clause FALSE (half-width 0 is not > 0.03), so a vault where every query
// was zero-relevance votes KILL with perfect confidence. Two of them plus one
// strong vault produced a KILL verdict through the unmodified rule — "we did
// not look" rendered as "we looked and found nothing", which is #797's exact
// shape reappearing in the Delta_C series.
// ---------------------------------------------------------------------------

func TestCognitionTrialRule_AllZeroRelevanceVaultIsUnderpoweredNotKill(t *testing.T) {
	empty := ctPairedBootstrap(nil, nil, ctPreregistered.BootstrapResamples, 7)
	if empty.Point != 0 || empty.halfWidth() != 0 || empty.N != 0 {
		t.Fatalf("the empty-series bootstrap no longer returns the zero value (%+v) — this test's "+
			"premise has changed", empty)
	}
	if !(empty.Point < ctPreregistered.KillDeltaCPoint &&
		empty.CIUpper < ctPreregistered.KillDeltaCCIUpper) {
		t.Fatal("an empty Delta_C no longer satisfies K1's clause — the trap this test pins is gone")
	}
	if empty.halfWidth() > ctPreregistered.MaxCIHalfWidth {
		t.Fatal("an empty Delta_C is now caught by U5 — it was not, and that was the whole problem")
	}

	vaults := ctThree(ctShipVault)
	for i := 0; i < 2; i++ {
		vaults[i].NQueries = 0
		vaults[i].ZeroRelevanceQueries = 360
		vaults[i].DeltaC = empty
		vaults[i].DeltaH = empty
		vaults[i].DeltaP = empty
		vaults[i].DeltaHP = empty
	}
	vaults[2].DeltaC = ctDelta{Point: 0.100, CILower: 0.070, CIUpper: 0.130, N: 320}
	vaults[2].DeltaH = ctDelta{Point: 0.004, CILower: -0.006, CIUpper: 0.014, N: 320}
	vaults[2].DeltaP = ctDelta{Point: 0.003, CILower: -0.007, CIUpper: 0.013, N: 320}
	vaults[2].DeltaHP = ctDelta{Point: 0.006, CILower: -0.006, CIUpper: 0.020, N: 320}
	vaults[2].MRRDeltaH = ctMeanDelta{Point: 0.001, N: vaults[2].DeltaC.N}

	got := ctDecide(vaults, ctGoodJudge(), true)
	if got.Verdict == ctVerdictKill {
		t.Fatalf("two vaults that measured NOTHING out-voted one vault with Delta_C +0.100 and "+
			"produced a KILL\n%s", got)
	}
	ctRequireVerdict(t, got, ctVerdictUnderpowered, "NO INFORMATIVE QUERY")
}

// The load-bearing wire under an exclusion policy: the power gates and the
// estimate must bind to the same series. ctDecide reads NQueries for U2 and
// DeltaC.halfWidth() for U5, and until D1 it read DeltaC.N nowhere at all.
func TestCognitionTrialRule_NQueriesMustBindToTheDeltaSeries(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mutate  func(v *ctVaultResult)
		wantSub string
	}{
		{"Delta_C over a different sample", func(v *ctVaultResult) { v.DeltaC.N = 12 },
			"but Delta_C was computed over 12"},
		{"Delta_HP over a different sample", func(v *ctVaultResult) { v.DeltaHP.N = 12 },
			"computed Delta_HP over 12"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			vaults := ctThree(ctShipVault)
			tc.mutate(&vaults[1])
			ctRequireVerdict(t, ctDecide(vaults, ctGoodJudge(), true),
				ctVerdictUnderpowered, tc.wantSub)
		})
	}
}

// ---------------------------------------------------------------------------
// D2: THE ARM KILL SHIPS MUST BE AN ARM SOME K-CLAUSE READS.
//
// This is a CLASSIFICATION test, not a string check: it reads the config knobs
// KILL's action text flips, maps that set of disabled mechanisms to the arm that
// IS that configuration, then reads which deltas the K-clauses cite and maps
// those back to arms. If the two ever disconnect — because someone adds a knob
// to the action, or removes a clause — it fails naming both sides.
//
// It failed before D2: KILL shipped the BASE-LEVEL-ONLY configuration and the
// K-clauses read only Delta_C, Delta_H and Delta_P. The four marginals provably
// cannot reconstruct Delta_HP under redundancy, so the trial authorized an
// action it had measured nothing about.
// ---------------------------------------------------------------------------

// ctKillActionKnobs maps a plasticity knob KILL's action text can flip to the
// mechanism it turns off.
var ctKillActionKnobs = map[string]string{
	"hebbian_enabled:false":       "Hebbian",
	"predictive_activation:false": "PAS",
	"actr_base_level:false":       "base-level",
}

// ctDeltaToArm maps the delta a clause cites to the arm it is measured against.
// Longest name first: "Delta_HP" contains "Delta_H".
var ctDeltaToArm = []struct {
	re  *regexp.Regexp
	arm string
}{
	{regexp.MustCompile(`Delta_HP\b`), ctArmNameBaseLevelOnly},
	{regexp.MustCompile(`Delta_C\b`), ctArmNameContentOnly},
	{regexp.MustCompile(`Delta_H\b`), ctArmNameNoHebbian},
	{regexp.MustCompile(`Delta_P\b`), ctArmNameNoPAS},
}

func TestCognitionTrialRule_KillActionArmIsMeasured(t *testing.T) {
	got := ctDecide(ctThree(ctKillVault), ctGoodJudge(), true)
	if got.Verdict != ctVerdictKill {
		t.Fatalf("the KILL fixture no longer kills, so the action text under test is not "+
			"produced\n%s", got)
	}

	var action string
	var kLines []string
	for _, r := range got.Reasons {
		if strings.HasPrefix(r, "KILL:") {
			action = r
		}
		if len(r) > 1 && r[0] == 'K' && r[1] >= '1' && r[1] <= '9' {
			kLines = append(kLines, r)
		}
	}
	if action == "" {
		t.Fatal("no KILL action line in the decision")
	}

	// 1. What configuration does the action ship?
	off := map[string]bool{}
	for knob, mech := range ctKillActionKnobs {
		if strings.Contains(action, knob) {
			off[mech] = true
		}
	}
	shipped := ""
	for arm, mechs := range ctMechanismsOffInArm {
		if len(mechs) != len(off) {
			continue
		}
		all := true
		for _, m := range mechs {
			if !off[m] {
				all = false
			}
		}
		if all {
			shipped = arm
		}
	}
	if shipped == "" {
		t.Fatalf("KILL's action turns off %v, which corresponds to NO ARM of the trial. Either "+
			"the action changed or an arm is missing; both mean the verdict authorizes a "+
			"configuration nothing measured.\n%s", off, action)
	}

	// 2. Which arms do the K-clauses actually read?
	read := map[string]bool{}
	for _, l := range kLines {
		for _, m := range ctDeltaToArm {
			if m.re.MatchString(l) {
				read[m.arm] = true
			}
		}
	}

	t.Logf("KILL ships the %s configuration; K-clauses read %v", shipped, ctSortedKeys(read))
	if !read[shipped] {
		t.Errorf("KILL's action ships the %s configuration and NO K-clause reads that arm "+
			"(clauses read %v). The trial yields no information about the cost of the action it "+
			"authorizes: Delta_HP is bounded only by [max(Delta_H, Delta_P), Delta_C], which on a "+
			"typical vault is the entire range.", shipped, ctSortedKeys(read))
	}
}

func ctSortedKeys(m map[string]bool) []string {
	var out []string
	for _, a := range ctArmNames {
		if m[a] {
			out = append(out, a)
		}
	}
	return out
}

// K4 is retained as pre-registered, and it is SUBSUMED by K2 for as long as
// MinDeltaC >= MinDeltaMechanism: both must hold for KILL, and K4's bar is the
// higher one, so every K4 veto is already blocked by K2's Delta_HP clause.
//
// This is pinned rather than left to be rediscovered, because the consequence is
// non-obvious and matters: K4's integrity condition — that the vetoing vault
// survived the S6 shuffled-seed null — CANNOT currently change a verdict, since
// K2 has no such condition and blocks first. A veto that a shuffled seed
// reproduces still blocks KILL, via K2. If either bar is ever re-ordered, this
// test fails and the relationship must be re-decided rather than inherited.
func TestCognitionTrialRule_K4IsSubsumedByK2(t *testing.T) {
	if ctPreregistered.MinDeltaC < ctPreregistered.MinDeltaMechanism {
		t.Fatalf("MinDeltaC (%.3f) is now BELOW MinDeltaMechanism (%.3f). K4 is no longer "+
			"subsumed by K2 and becomes independently load-bearing — including its S6 integrity "+
			"condition, which until now could not change a verdict. Re-decide, do not re-pin.",
			ctPreregistered.MinDeltaC, ctPreregistered.MinDeltaMechanism)
	}
	// A vault whose Delta_HP clears K4's bar but whose null was REPRODUCED BY A
	// SHUFFLE: K4 grants no veto, and KILL is still blocked — by K2.
	vaults := ctThree(ctKillVault)
	vaults[1].DeltaC = ctDelta{Point: 0.050, CILower: 0.025, CIUpper: 0.075, N: 340}
	vaults[1].DeltaHP = ctDelta{Point: 0.045, CILower: 0.020, CIUpper: 0.070, N: 340}
	vaults[1].ShuffledSeedNull = ctNullReproducedByShuffle
	got := ctDecide(vaults, ctGoodJudge(), true)
	if got.Verdict == ctVerdictKill {
		t.Fatalf("a vault whose Delta_HP is +0.045 did not block the kill\n%s", got)
	}
	ctRequireVerdict(t, got, ctVerdictInconclusivePowered, "K2 FAIL")
	ctRequireVerdict(t, got, ctVerdictInconclusivePowered, "K4 PASS")
}

// ---------------------------------------------------------------------------
// D2/D5: THE ABLATION-ADDITIVITY RELATIONS, AS A DIAGNOSTIC.
//
//	Delta_HP >= max(Delta_H, Delta_P)
//	Delta_HP <= Delta_C
//
// These make the REDUNDANCY GAP visible rather than assumed — the whole reason
// the fifth arm exists is that Delta_HP can sit anywhere in that interval, and
// on a typical vault the interval is the entire range.
//
// D2 also made them a U3 GATE, and D5 removed that. The reasons are in
// ctAdditivityDiagnostic's comment and are pinned by
// TestCognitionTrialRule_AdditivityDoesNotConvergeAndIsNotAGate: the comparison
// is between four POINT estimates, it breaks ~2/3 of the time under an
// exchangeable null, it is FLAT IN n, and every break it does report decodes to
// a named net-harmful mechanism — a finding, not an instrument failure. The
// genuinely invariant form of the relation is on RAW SCORES and stays a hard
// failure in the activation package's reconstruction-fidelity test.
// ---------------------------------------------------------------------------

func TestCtArms_AblationAdditivity(t *testing.T) {
	// The relations hold on a series built from the real arm ordering.
	v := ctVaultFromSynth("A", ctSynthSeries(320))
	if msgs := ctAdditivityDiagnostic(v); len(msgs) != 0 {
		t.Fatalf("a well-formed arm series broke the relations: %v", msgs)
	}
	if !(v.DeltaHP.Point >= math.Max(v.DeltaH.Point, v.DeltaP.Point) &&
		v.DeltaHP.Point <= v.DeltaC.Point) {
		t.Fatalf("Delta_HP %+.4f outside [max(Delta_H %+.4f, Delta_P %+.4f), Delta_C %+.4f]",
			v.DeltaHP.Point, v.DeltaH.Point, v.DeltaP.Point, v.DeltaC.Point)
	}
	// The gap is REAL on this series, i.e. the bounds are not trivially tight —
	// which is the fact that makes the fifth arm necessary rather than derivable.
	lo, hi := math.Max(v.DeltaH.Point, v.DeltaP.Point), v.DeltaC.Point
	if hi-lo < 0.005 {
		t.Errorf("the bound interval [%+.4f, %+.4f] is degenerate on this fixture, so it cannot "+
			"demonstrate that the marginals fail to determine Delta_HP", lo, hi)
	}
	t.Logf("Delta_H %+.4f  Delta_P %+.4f  Delta_HP %+.4f  Delta_C %+.4f  (bound width %.4f)",
		v.DeltaH.Point, v.DeltaP.Point, v.DeltaHP.Point, v.DeltaC.Point, hi-lo)

	// And each break is detected, DECODED to the mechanism it names, reported in
	// the audit trail — and changes NO verdict.
	for _, tc := range []struct {
		name    string
		mutate  func(v *ctVaultResult)
		wantSub string
	}{
		{"below Delta_H: PAS harmful on top of base-level", func(v *ctVaultResult) {
			v.DeltaHP = ctDelta{Point: v.DeltaH.Point - 0.01, CILower: -0.01,
				CIUpper: 0.03, N: v.DeltaC.N}
		}, "PAS IS NET-HARMFUL on top of the base-level prior"},
		{"below Delta_P: Hebbian harmful on top of base-level", func(v *ctVaultResult) {
			v.DeltaP = ctDelta{Point: v.DeltaHP.Point + 0.01, CILower: 0.001,
				CIUpper: 0.05, N: v.DeltaC.N}
			v.MRRDeltaP = ctMeanDelta{Point: 0.01, N: v.DeltaC.N}
		}, "THE HEBBIAN BOOST IS NET-HARMFUL on top of the base-level prior"},
		{"above Delta_C: the base-level prior is harmful", func(v *ctVaultResult) {
			v.DeltaHP = ctDelta{Point: v.DeltaC.Point + 0.01, CILower: -0.01,
				CIUpper: 0.09, N: v.DeltaC.N}
		}, "THE ACT-R BASE-LEVEL PRIOR IS NET-HARMFUL"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := ctDecide(ctThree(ctShipVault), ctGoodJudge(), true)
			vaults := ctThree(ctShipVault)
			tc.mutate(&vaults[1])
			if msgs := ctAdditivityDiagnostic(vaults[1]); len(msgs) == 0 {
				t.Fatal("the broken relation was not detected at all")
			}
			got := ctDecide(vaults, ctGoodJudge(), true)
			// It is REPORTED...
			ctRequireVerdict(t, got, base.Verdict, tc.wantSub)
			// ...and it gated nothing: the verdict is the untouched baseline's.
			if got.Verdict != base.Verdict {
				t.Fatalf("a broken NDCG-level additivity relation moved the verdict %s -> %s. "+
					"Since D5 it is a DIAGNOSTIC: the comparison breaks ~2/3 of the time under an "+
					"exchangeable null and is flat in n, so gating on it converts a finding about "+
					"a harmful mechanism into 'we could not measure'\n%s",
					base.Verdict, got.Verdict, got)
			}
			if strings.Contains(strings.Join(got.Reasons, "\n"), "U3 (ablation additivity)") {
				t.Errorf("the additivity relation is still raising a U3 objection\n%s", got)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// D3: S3 IS DESCRIPTIVE.
//
// ctReplay runs ONCE, its checkpoint callback only logs, and every arm scores
// against the FINAL graph — so what the code computes is a per-bucket breakdown
// of a single measurement, not a trend over checkpoints. Gating the verdict on
// it gates on a quantity the code does not produce, which is the same failure
// shape as #797.
//
// U6's empty-bucket guard STAYS regardless: an absent bucket is absent data
// whether or not a verdict depends on it.
// ---------------------------------------------------------------------------

func TestCognitionTrialRule_S3IsDescriptiveNotAShipGate(t *testing.T) {
	vaults := ctThree(ctShipVault)
	for i := range vaults {
		falling := make([]float64, 12)
		for j := range falling {
			falling[j] = 0.08 - 0.005*float64(j)
		}
		vaults[i].DeltaCByBucket = ctBucketSeries(falling)
	}
	got := ctDecide(vaults, ctGoodJudge(), true)
	if got.Verdict != ctVerdictShip {
		t.Fatalf("a clearly falling per-bucket breakdown blocked SHIP. S3 gates nothing since "+
			"D3 — the quantity it was pre-registered over (a trend across checkpoints) is not "+
			"what the harness computes\n%s", got)
	}
	ctRequireVerdict(t, got, ctVerdictShip, "S3 FAIL (DESCRIPTIVE ONLY")

	// U6 is unaffected: too few populated buckets is still UNDERPOWERED.
	sparse := ctThree(ctShipVault)
	full := ctBucketSeries(ctRisingBuckets(0.03))
	for i := range sparse {
		sparse[i].DeltaCByBucket = full[:ctPreregistered.MinPopulatedBuckets-1]
	}
	ctRequireVerdict(t, ctDecide(sparse, ctGoodJudge(), true), ctVerdictUnderpowered,
		"populated weekly buckets")
}

// ---------------------------------------------------------------------------
// D4: KILL's action text carries only what this measurement licenses.
// ---------------------------------------------------------------------------

func TestCognitionTrialRule_KillActionSplitsByEvidence(t *testing.T) {
	got := ctDecide(ctThree(ctKillVault), ctGoodJudge(), true)
	if got.Verdict != ctVerdictKill {
		t.Fatalf("fixture no longer kills\n%s", got)
	}
	joined := strings.Join(got.Reasons, "\n")

	// internal/working and internal/episodic are measured by NO arm. A verdict
	// that deletes them decides their fate on a measurement that never looked.
	for _, pkg := range []string{"internal/working", "internal/episodic"} {
		idx := strings.Index(joined, pkg)
		if idx < 0 {
			t.Errorf("%s is not mentioned at all — it must be explicitly SPLIT OUT, not silently "+
				"dropped, or the next reader re-bundles it", pkg)
			continue
		}
		if !strings.Contains(joined[:idx+len(pkg)+200], "NOT LICENSED BY THIS VERDICT") &&
			!strings.Contains(joined[max0(idx-300):idx], "NOT LICENSED BY THIS VERDICT") {
			t.Errorf("%s appears in the KILL action without being marked as not licensed by it", pkg)
		}
	}

	// The dead SGD loop and muninn_feedback's description are wrong under SHIP
	// too, so they are not this verdict's to authorize.
	if !strings.Contains(joined, "NOT VERDICT-DEPENDENT") ||
		!strings.Contains(joined, "SGD") || !strings.Contains(joined, "muninn_feedback") {
		t.Errorf("the non-verdict-dependent cleanups are not split out of the KILL action\n%s", got)
	}

	// A SHIP verdict must not carry a KILL action at all.
	ship := ctDecide(ctThree(ctShipVault), ctGoodJudge(), true)
	if strings.Contains(strings.Join(ship.Reasons, "\n"), "retire the cognitive layer") {
		t.Error("a SHIP verdict carried the KILL action text")
	}
}

func max0(i int) int {
	if i < 0 {
		return 0
	}
	return i
}
