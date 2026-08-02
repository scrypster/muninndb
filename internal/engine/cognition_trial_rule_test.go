//go:build localassets || cognitiontrial

package engine

import (
	"fmt"
	"math"
	"math/rand"
	"sort"
	"strings"
)

// ---------------------------------------------------------------------------
// THE PRE-REGISTERED ACCEPTANCE RULE FOR THE COGNITION TRIAL, AS CODE.
//
// Source of truth: .claude/deep-review/2026-08-02-cognition-trial-design.md §6,
// written before the first run. It is not moved after seeing numbers. Moving a
// threshold to rescue a result is tuning against the labeled set — principle #11
// — and the whole point of expressing the rule as code rather than prose is that
// a failing gate REPORTS ITSELF instead of being rounded up in a write-up.
//
// A KILL verdict is a legitimate, valuable outcome. So is UNDERPOWERED. The
// fourth outcome, INCONCLUSIVE-BUT-POWERED, exists precisely so that the
// temptation to round a real-but-small effect up to SHIP has nowhere to go.
//
// WHY THIS FILE IS TAGGED `localassets || cognitiontrial` AND ITS SIBLING
// HARNESS IS NOT. It is a _test.go file, so it is never in a shipped binary
// either way. Under `localassets` alone it compiles and its own table-driven
// tests run in the default CI job, at a cost of milliseconds — because the
// decision procedure and the metrics the verdict rests on must themselves be
// tested, and a rule that only compiles behind a tag CI never passes is a rule
// nobody has checked. Under `localassets cognitiontrial` the operator's harness
// (cognition_trial_measure_test.go) calls exactly these functions, so the thing
// CI tested is the thing that decides.
//
// PRIVACY: this file contains arithmetic and verdict strings. No query text, no
// memory content, no vault name, and no identifier of any kind ever enters it —
// the harness passes in per-query metric VALUES only.
// ---------------------------------------------------------------------------

// ctGains maps a judge grade (0..3) to its NDCG gain. Pre-registered.
var ctGains = [4]float64{0, 1, 3, 7}

// ctRelevantGrade is the binarization threshold: grade >= 2 is "relevant".
// Used by MRR and by the judge-calibration gate.
const ctRelevantGrade = 2

// ctPreregistered holds every pre-registered threshold in one place so a
// reviewer can diff them against §6 in a single glance, and so no threshold is
// spelled inline where it could drift.
var ctPreregistered = struct {
	MinDeltaC           float64 // S1: the whole prior must buy at least this
	MinDeltaMechanism   float64 // S2/K2: a NAMED mechanism must buy at least this
	KillDeltaCPoint     float64 // K1: point estimate below this is a kill
	KillDeltaCCIUpper   float64 // K1: ...and the CI upper bound must be below this
	TrendMinPositive    int     // S3: positive in >= this many of the last 10 buckets
	TrendWindow         int     // S3: ...out of this many trailing buckets
	TrendSlopeCILower   float64 // S3: OLS slope 95% CI lower bound floor
	VaultsRequired      int     // U2
	VaultsSupporting    int     // S1/S3: how many vaults must carry the claim
	MinQueriesPerVault  int     // U2
	MinEventsPerVault   int     // U2
	MinJudgeKappa       float64 // U1
	MaxJudgeFPR         float64 // U1
	MinReplayEdgeFrac   float64 // U4
	MaxUnreplayableFrac float64 // U4
	MaxCIHalfWidth      float64 // U5
	BootstrapResamples  int
}{
	MinDeltaC:           0.03,
	MinDeltaMechanism:   0.02,
	KillDeltaCPoint:     0.01,
	KillDeltaCCIUpper:   0.03,
	TrendMinPositive:    8,
	TrendWindow:         10,
	TrendSlopeCILower:   -0.005,
	VaultsRequired:      3,
	VaultsSupporting:    2,
	MinQueriesPerVault:  300,
	MinEventsPerVault:   200,
	MinJudgeKappa:       0.6,
	MaxJudgeFPR:         0.15,
	MinReplayEdgeFrac:   0.20,
	MaxUnreplayableFrac: 0.60,
	MaxCIHalfWidth:      0.03,
	BootstrapResamples:  10000,
}

// ---------------------------------------------------------------------------
// METRICS
// ---------------------------------------------------------------------------

// ctNDCGAt10 computes graded NDCG@10 for one query.
//
// ranked is the arm's returned order (memory keys). grades is the POOLED label
// set for this query — the union of every arm's top-10, labeled once, which is
// what makes the labels arm-neutral. The ideal DCG is computed over the pooled
// set, so all four arms are normalized by the same denominator; normalizing
// each arm by its own returned set would let a short list win by returning less.
//
// An unlabeled ranked item contributes gain 0 rather than being skipped:
// skipping it would silently promote whatever came after it, which is the arm
// flattering itself.
func ctNDCGAt10(ranked []string, grades map[string]int) float64 {
	const k = 10
	dcg := 0.0
	for i, id := range ranked {
		if i >= k {
			break
		}
		dcg += ctGain(grades[id]) / math.Log2(float64(i)+2.0)
	}
	ideal := make([]int, 0, len(grades))
	for _, g := range grades {
		ideal = append(ideal, g)
	}
	sort.Sort(sort.Reverse(sort.IntSlice(ideal)))
	idcg := 0.0
	for i, g := range ideal {
		if i >= k {
			break
		}
		idcg += ctGain(g) / math.Log2(float64(i)+2.0)
	}
	if idcg == 0 {
		// No relevant document exists for this query in the pooled label set.
		// NDCG is undefined; 0 for every arm, which is paired and therefore
		// contributes exactly 0 to every delta. Reported as such by the caller.
		return 0
	}
	return dcg / idcg
}

func ctGain(grade int) float64 {
	if grade < 0 {
		return 0
	}
	if grade >= len(ctGains) {
		return ctGains[len(ctGains)-1]
	}
	return ctGains[grade]
}

// ctMRR is the reciprocal rank of the first returned item graded relevant.
func ctMRR(ranked []string, grades map[string]int) float64 {
	for i, id := range ranked {
		if grades[id] >= ctRelevantGrade {
			return 1.0 / float64(i+1)
		}
	}
	return 0
}

// ---------------------------------------------------------------------------
// PAIRED BOOTSTRAP
// ---------------------------------------------------------------------------

// ctDelta is a paired difference estimate with a bootstrap interval.
type ctDelta struct {
	Point    float64
	CILower  float64
	CIUpper  float64
	N        int
	SDOfDiff float64 // sigma_d, the quantity the sample-size calculation used
}

func (d ctDelta) halfWidth() float64 { return (d.CIUpper - d.CILower) / 2 }

// ctPairedBootstrap resamples QUERIES (not observations) with replacement, so
// the pairing between arms is preserved in every resample — that is the whole
// point of the paired design and the reason the same queries run through every
// arm.
//
// seed is explicit so a run is reproducible from its run log.
func ctPairedBootstrap(a, b []float64, resamples int, seed int64) ctDelta {
	n := len(a)
	if n == 0 || len(b) != n {
		return ctDelta{}
	}
	diff := make([]float64, n)
	sum := 0.0
	for i := range a {
		diff[i] = a[i] - b[i]
		sum += diff[i]
	}
	mean := sum / float64(n)
	varSum := 0.0
	for _, d := range diff {
		varSum += (d - mean) * (d - mean)
	}
	sd := 0.0
	if n > 1 {
		sd = math.Sqrt(varSum / float64(n-1))
	}

	rng := rand.New(rand.NewSource(seed))
	means := make([]float64, resamples)
	for r := 0; r < resamples; r++ {
		s := 0.0
		for i := 0; i < n; i++ {
			s += diff[rng.Intn(n)]
		}
		means[r] = s / float64(n)
	}
	sort.Float64s(means)
	lo := means[ctPercentileIndex(resamples, 0.025)]
	hi := means[ctPercentileIndex(resamples, 0.975)]
	return ctDelta{Point: mean, CILower: lo, CIUpper: hi, N: n, SDOfDiff: sd}
}

func ctPercentileIndex(n int, p float64) int {
	i := int(math.Floor(p * float64(n)))
	if i < 0 {
		i = 0
	}
	if i >= n {
		i = n - 1
	}
	return i
}

// ---------------------------------------------------------------------------
// TREND (S3)
// ---------------------------------------------------------------------------

// ctTrend is the OLS regression of Delta_C on bucket index, with a t-based 95%
// interval on the slope.
type ctTrend struct {
	Slope           float64
	SlopeCILower    float64
	PositiveOfLastN int
	WindowN         int
}

// ctTrendT975 is the two-sided 97.5th percentile of Student's t by degrees of
// freedom. Tabulated for the small df this design actually produces (12 weekly
// buckets => df 10) rather than approximated by 1.96, which would be ~12% too
// narrow at df=10 and would make S3 easier to pass than pre-registered.
var ctTrendT975 = map[int]float64{
	1: 12.706, 2: 4.303, 3: 3.182, 4: 2.776, 5: 2.571, 6: 2.447, 7: 2.365,
	8: 2.306, 9: 2.262, 10: 2.228, 11: 2.201, 12: 2.179, 13: 2.160, 14: 2.145,
	15: 2.131, 16: 2.120, 17: 2.110, 18: 2.101, 19: 2.093, 20: 2.086,
	21: 2.080, 22: 2.074, 23: 2.069, 24: 2.064, 25: 2.060, 26: 2.056,
	27: 2.052, 28: 2.048, 29: 2.045, 30: 2.042,
}

func ctT975(df int) float64 {
	if df <= 0 {
		return math.Inf(1)
	}
	if v, ok := ctTrendT975[df]; ok {
		return v
	}
	return 1.96
}

// ctComputeTrend takes Delta_C per weekly bucket, in bucket order.
func ctComputeTrend(deltaByBucket []float64, window int) ctTrend {
	n := len(deltaByBucket)
	tr := ctTrend{WindowN: window}
	start := n - window
	if start < 0 {
		start = 0
	}
	for _, d := range deltaByBucket[start:] {
		if d > 0 {
			tr.PositiveOfLastN++
		}
	}
	if n < 3 {
		tr.SlopeCILower = math.Inf(-1)
		return tr
	}
	var sx, sy float64
	for i, y := range deltaByBucket {
		sx += float64(i)
		sy += y
	}
	mx, my := sx/float64(n), sy/float64(n)
	var sxy, sxx float64
	for i, y := range deltaByBucket {
		dx := float64(i) - mx
		sxy += dx * (y - my)
		sxx += dx * dx
	}
	if sxx == 0 {
		tr.SlopeCILower = math.Inf(-1)
		return tr
	}
	slope := sxy / sxx
	intercept := my - slope*mx
	var sse float64
	for i, y := range deltaByBucket {
		resid := y - (intercept + slope*float64(i))
		sse += resid * resid
	}
	df := n - 2
	se := math.Sqrt(sse / float64(df) / sxx)
	tr.Slope = slope
	tr.SlopeCILower = slope - ctT975(df)*se
	return tr
}

// ---------------------------------------------------------------------------
// INPUTS TO THE RULE
// ---------------------------------------------------------------------------

// ctJudgeCalibration is the §3c human-adjudication gate, which is run and
// recorded BEFORE any arm is scored. Ran=false is not "no result" — it is U1.
type ctJudgeCalibration struct {
	Ran   bool
	Kappa float64 // Cohen's kappa on the binarized (grade >= 2) label
	FPR   float64 // judge >= 2 where human < 2
	N     int     // adjudicated pairs
	// LabelHashMatches records that the SHA-256 of the sorted
	// (queryHash, memoryID, grade) triples used for scoring equals the hash
	// frozen in the run log before scoring. S5.
	LabelHashMatches bool
}

// ctVaultResult is one vault's contribution. Vaults are A/B/C; the label is the
// only identifier that ever exists here, by construction.
type ctVaultResult struct {
	Label    string
	NQueries int // labeled held-out queries
	// DistinctEvents is the vault's distinct recall events after dedup (U2).
	DistinctEvents int

	DeltaC ctDelta // FULL - CONTENT-MATCH-ONLY
	DeltaH ctDelta // FULL - NO-HEBBIAN
	DeltaP ctDelta // FULL - NO-PAS

	MRRDeltaH float64 // sign-agreement input for S4
	MRRDeltaP float64

	DeltaCByBucket []float64 // S3, in weekly-bucket order

	// Reconstruction composition (U4). BaselineEdges is G0's total edge count,
	// ReplayedEdges is G1's, UnreplayableFrac is the share of G0's edges whose
	// RelType replay cannot produce (declared / autoassoc / consolidation).
	BaselineEdges    int
	ReplayedEdges    int
	UnreplayableFrac float64
}

// ctVerdict is the outcome. The four values are exhaustive by construction:
// ctDecide always returns one of them.
type ctVerdict string

const (
	ctVerdictShip                ctVerdict = "SHIP"
	ctVerdictKill                ctVerdict = "KILL"
	ctVerdictUnderpowered        ctVerdict = "UNDERPOWERED"
	ctVerdictInconclusivePowered ctVerdict = "INCONCLUSIVE-BUT-POWERED"
)

// ctDecision is the verdict plus the audit trail: which numbered criteria
// passed, which failed, and why. The reasons are what goes into the write-up,
// so a failing gate cannot be quietly dropped.
type ctDecision struct {
	Verdict ctVerdict
	Reasons []string
}

func (d ctDecision) String() string {
	return fmt.Sprintf("%s\n  %s", d.Verdict, strings.Join(d.Reasons, "\n  "))
}

// ---------------------------------------------------------------------------
// THE RULE
// ---------------------------------------------------------------------------

// ctDecide applies §6 verbatim.
//
// ORDER IS LOAD-BEARING. UNDERPOWERED is evaluated FIRST and short-circuits,
// because every U condition is a statement that the data cannot support ANY
// conclusion — including a kill. K3 ("power was adequate") is therefore
// automatically satisfied for anything that reaches the KILL test, and is
// re-asserted there anyway so the criterion appears in the audit trail rather
// than being implied by control flow.
//
// fidelityOK is U3: the two reconstruction-fidelity tests
// (TestCognitionTrial_ReplayFidelity, TestCognitionTrial_ArmReconstructionFidelity).
// The harness passes their result in rather than assuming it.
func ctDecide(vaults []ctVaultResult, judge ctJudgeCalibration, fidelityOK bool) ctDecision {
	var u []string

	// --- U1: the judge-calibration gate --------------------------------------
	if !judge.Ran {
		u = append(u, "U1: the judge-calibration gate was not run. It is required BEFORE any "+
			"arm is scored; an unrun gate is not a passed gate.")
	} else {
		if judge.Kappa < ctPreregistered.MinJudgeKappa {
			u = append(u, fmt.Sprintf("U1: judge kappa %.3f < %.2f on the human-adjudicated subsample",
				judge.Kappa, ctPreregistered.MinJudgeKappa))
		}
		if judge.FPR > ctPreregistered.MaxJudgeFPR {
			u = append(u, fmt.Sprintf("U1: judge false-positive rate %.1f%% > %.0f%%",
				100*judge.FPR, 100*ctPreregistered.MaxJudgeFPR))
		}
	}

	// --- U2: sample size -----------------------------------------------------
	if len(vaults) < ctPreregistered.VaultsRequired {
		u = append(u, fmt.Sprintf("U2: %d vaults, need %d", len(vaults), ctPreregistered.VaultsRequired))
	}
	for _, v := range vaults {
		if v.NQueries < ctPreregistered.MinQueriesPerVault {
			u = append(u, fmt.Sprintf("U2: vault %s has %d labeled held-out queries, need %d",
				v.Label, v.NQueries, ctPreregistered.MinQueriesPerVault))
		}
		if v.DistinctEvents < ctPreregistered.MinEventsPerVault {
			u = append(u, fmt.Sprintf("U2: vault %s has %d distinct recall events after dedup, need %d",
				v.Label, v.DistinctEvents, ctPreregistered.MinEventsPerVault))
		}
	}

	// --- U3: reconstruction fidelity ----------------------------------------
	if !fidelityOK {
		u = append(u, "U3: a reconstruction-fidelity test failed — the reconstruction is not "+
			"the thing it claims to be, and no arm number may be quoted")
	}

	// --- U4: the reconstruction is too partial -------------------------------
	for _, v := range vaults {
		if v.BaselineEdges > 0 {
			frac := float64(v.ReplayedEdges) / float64(v.BaselineEdges)
			if frac < ctPreregistered.MinReplayEdgeFrac {
				u = append(u, fmt.Sprintf("U4: vault %s replayed %d edges vs %d baseline (%.1f%%), "+
					"below the %.0f%% floor", v.Label, v.ReplayedEdges, v.BaselineEdges,
					100*frac, 100*ctPreregistered.MinReplayEdgeFrac))
			}
		}
		if v.UnreplayableFrac > ctPreregistered.MaxUnreplayableFrac {
			u = append(u, fmt.Sprintf("U4: vault %s has %.1f%% of baseline edges in RelTypes replay "+
				"cannot produce (declared / autoassoc / consolidation), above the %.0f%% ceiling",
				v.Label, 100*v.UnreplayableFrac, 100*ctPreregistered.MaxUnreplayableFrac))
		}
	}

	// --- U5: the interval cannot distinguish SHIP from KILL -------------------
	wide := 0
	for _, v := range vaults {
		if v.DeltaC.halfWidth() > ctPreregistered.MaxCIHalfWidth {
			wide++
		}
	}
	if wide >= ctPreregistered.VaultsSupporting {
		u = append(u, fmt.Sprintf("U5: the paired-bootstrap 95%% CI half-width for Delta_C exceeds "+
			"%.2f on %d vaults — the data cannot distinguish SHIP from KILL",
			ctPreregistered.MaxCIHalfWidth, wide))
	}

	if len(u) > 0 {
		u = append(u, "UNDERPOWERED is a real outcome, not a failure of the instrument: report it, "+
			"change no defaults, publish the composition and power numbers, and schedule the "+
			"confirmatory run after natural re-learning.")
		return ctDecision{Verdict: ctVerdictUnderpowered, Reasons: u}
	}

	// --- SHIP ---------------------------------------------------------------
	var ship, kill []string

	// S1
	s1Vaults, s1NegativeVault := 0, ""
	for _, v := range vaults {
		if v.DeltaC.Point >= ctPreregistered.MinDeltaC && v.DeltaC.CILower > 0 {
			s1Vaults++
		}
		if v.DeltaC.CIUpper < 0 {
			s1NegativeVault = v.Label
		}
	}
	s1 := s1Vaults >= ctPreregistered.VaultsSupporting && s1NegativeVault == ""
	ship = append(ship, fmt.Sprintf("S1 %s: Delta_C >= %.2f with CI lower > 0 on %d/%d vaults (need %d)%s",
		ctPassFail(s1), ctPreregistered.MinDeltaC, s1Vaults, len(vaults), ctPreregistered.VaultsSupporting,
		ctIf(s1NegativeVault != "", fmt.Sprintf("; vault %s is SIGNIFICANTLY NEGATIVE", s1NegativeVault))))

	// S2 — the win must be attributable to a NAMED mechanism. A Delta_C carried
	// entirely by the ACT-R base-level prior is not a win for Hebbian/PAS and
	// does not license keeping them.
	s2 := false
	s2Which := ""
	for _, v := range vaults {
		if v.DeltaH.Point >= ctPreregistered.MinDeltaMechanism && v.DeltaH.CILower > 0 {
			s2, s2Which = true, "Hebbian"
			break
		}
		if v.DeltaP.Point >= ctPreregistered.MinDeltaMechanism && v.DeltaP.CILower > 0 {
			s2, s2Which = true, "PAS"
			break
		}
	}
	ship = append(ship, fmt.Sprintf("S2 %s: a named mechanism reaches +%.2f with CI lower > 0 on some vault%s",
		ctPassFail(s2), ctPreregistered.MinDeltaMechanism, ctIf(s2, " ("+s2Which+")")))

	// S3 — the trend. Applied per vault and required on the same number of
	// vaults as S1: the design states the count for S1 and not for S3, and
	// requiring the trend on FEWER vaults than the effect would let a single
	// vault's trend carry the reproducibility claim. Symmetry is the
	// conservative reading, and it is recorded here rather than chosen silently.
	s3Vaults := 0
	var trendDetail []string
	for _, v := range vaults {
		tr := ctComputeTrend(v.DeltaCByBucket, ctPreregistered.TrendWindow)
		ok := tr.PositiveOfLastN >= ctPreregistered.TrendMinPositive &&
			tr.SlopeCILower >= ctPreregistered.TrendSlopeCILower
		if ok {
			s3Vaults++
		}
		trendDetail = append(trendDetail, fmt.Sprintf("%s:%d/%d pos, slope %.5f (CI lo %.5f)",
			v.Label, tr.PositiveOfLastN, tr.WindowN, tr.Slope, tr.SlopeCILower))
	}
	s3 := s3Vaults >= ctPreregistered.VaultsSupporting
	ship = append(ship, fmt.Sprintf("S3 %s: trend holds on %d/%d vaults (need %d) [%s]",
		ctPassFail(s3), s3Vaults, len(vaults), ctPreregistered.VaultsSupporting,
		strings.Join(trendDetail, "; ")))

	// S4 — MRR agrees in sign with NDCG@10 on whichever mechanism satisfied S2.
	s4 := false
	if s2 {
		for _, v := range vaults {
			if s2Which == "Hebbian" && v.DeltaH.Point >= ctPreregistered.MinDeltaMechanism && v.DeltaH.CILower > 0 {
				s4 = ctSameSign(v.DeltaH.Point, v.MRRDeltaH)
				break
			}
			if s2Which == "PAS" && v.DeltaP.Point >= ctPreregistered.MinDeltaMechanism && v.DeltaP.CILower > 0 {
				s4 = ctSameSign(v.DeltaP.Point, v.MRRDeltaP)
				break
			}
		}
	}
	ship = append(ship, fmt.Sprintf("S4 %s: MRR agrees in sign with NDCG@10 on the S2 mechanism", ctPassFail(s4)))

	// S5
	s5 := judge.Ran && judge.LabelHashMatches
	ship = append(ship, fmt.Sprintf("S5 %s: judge gate passed before scoring and the label hash matches",
		ctPassFail(s5)))

	if s1 && s2 && s3 && s4 && s5 {
		ship = append(ship, "SHIP: the cognitive layer earns its complexity.")
		return ctDecision{Verdict: ctVerdictShip, Reasons: ship}
	}

	// --- KILL ---------------------------------------------------------------
	// K1: Delta_C < +0.01 point estimate with CI upper < +0.03 on >= 2 of 3.
	// A wide interval is not a kill; it is U5, already excluded above.
	k1Vaults := 0
	for _, v := range vaults {
		if v.DeltaC.Point < ctPreregistered.KillDeltaCPoint && v.DeltaC.CIUpper < ctPreregistered.KillDeltaCCIUpper {
			k1Vaults++
		}
	}
	k1 := k1Vaults >= ctPreregistered.VaultsSupporting
	kill = append(kill, fmt.Sprintf("K1 %s: Delta_C < +%.2f with CI upper < +%.2f on %d/%d vaults (need %d)",
		ctPassFail(k1), ctPreregistered.KillDeltaCPoint, ctPreregistered.KillDeltaCCIUpper,
		k1Vaults, len(vaults), ctPreregistered.VaultsSupporting))

	// K2 is the exact negation of S2.
	k2 := !s2
	kill = append(kill, fmt.Sprintf("K2 %s: neither Delta_H nor Delta_P reaches +%.2f with CI lower > 0 on any vault",
		ctPassFail(k2), ctPreregistered.MinDeltaMechanism))

	// K3 — re-asserted rather than implied by control flow, so it appears in
	// the audit trail.
	k3 := judge.Ran && judge.Kappa >= ctPreregistered.MinJudgeKappa && judge.FPR <= ctPreregistered.MaxJudgeFPR
	for _, v := range vaults {
		if v.NQueries < ctPreregistered.MinQueriesPerVault {
			k3 = false
		}
	}
	kill = append(kill, fmt.Sprintf("K3 %s: power was adequate (judge gate passed, n >= %d per vault, U2/U4/U5 clear)",
		ctPassFail(k3), ctPreregistered.MinQueriesPerVault))

	if k1 && k2 && k3 {
		kill = append(kill,
			"KILL: retire the cognitive layer. Executes in one PR referencing the design: "+
				"default preset hebbian_enabled:false + predictive_activation:false (now actually "+
				"effective on the read side, COG-31); record the negative in the decision record; "+
				"delete the dead SGD feedback loop and correct muninn_feedback's description; decide "+
				"the fate of internal/working and internal/episodic; rewrite the README's first "+
				"clause; KEEP the ACT-R base-level prior unless increment 2 measures it out.")
		return ctDecision{Verdict: ctVerdictKill, Reasons: append(ship, kill...)}
	}

	// --- INCONCLUSIVE-BUT-POWERED -------------------------------------------
	out := append(ship, kill...)
	out = append(out,
		"INCONCLUSIVE-BUT-POWERED: the layer is real but below the pre-committed bar for its "+
			"complexity. Default it OFF for NEW vaults, leave existing vaults untouched, defer "+
			"the deletions. Written down as its own outcome so that the temptation to round it "+
			"up to SHIP has nowhere to go.")
	return ctDecision{Verdict: ctVerdictInconclusivePowered, Reasons: out}
}

func ctPassFail(ok bool) string {
	if ok {
		return "PASS"
	}
	return "FAIL"
}

func ctIf(cond bool, s string) string {
	if cond {
		return s
	}
	return ""
}

func ctSameSign(a, b float64) bool {
	return (a > 0 && b > 0) || (a < 0 && b < 0) || (a == 0 && b == 0)
}

// ---------------------------------------------------------------------------
// JUDGE CALIBRATION ARITHMETIC (§3c)
// ---------------------------------------------------------------------------

// ctCohensKappa computes Cohen's kappa on the BINARIZED label (grade >= 2),
// which is what §3c pre-registers. judge and human must be paired and equal
// length.
//
// Returns kappa and the judge false-positive rate (judge relevant where the
// human said not relevant) in one pass, because the gate needs both and
// computing them apart invites them being computed over different subsets.
func ctCohensKappa(judge, human []int) (kappa, fpr float64, n int) {
	n = len(judge)
	if n == 0 || len(human) != n {
		return 0, 0, 0
	}
	var bothRel, bothIrrel, judgeOnly, humanOnly float64
	for i := range judge {
		jr := judge[i] >= ctRelevantGrade
		hr := human[i] >= ctRelevantGrade
		switch {
		case jr && hr:
			bothRel++
		case !jr && !hr:
			bothIrrel++
		case jr && !hr:
			judgeOnly++
		default:
			humanOnly++
		}
	}
	total := float64(n)
	po := (bothRel + bothIrrel) / total
	pJudgeRel := (bothRel + judgeOnly) / total
	pHumanRel := (bothRel + humanOnly) / total
	pe := pJudgeRel*pHumanRel + (1-pJudgeRel)*(1-pHumanRel)
	if 1-pe == 0 {
		// Perfect agreement with zero expected disagreement: kappa is undefined
		// (0/0). Report 1 when they agreed everywhere, 0 otherwise — never NaN,
		// which would silently compare false against the gate and read as a
		// PASS-by-accident in a naive comparison.
		if po == 1 {
			kappa = 1
		}
	} else {
		kappa = (po - pe) / (1 - pe)
	}
	humanIrrel := bothIrrel + judgeOnly
	if humanIrrel > 0 {
		fpr = judgeOnly / humanIrrel
	}
	return kappa, fpr, n
}
