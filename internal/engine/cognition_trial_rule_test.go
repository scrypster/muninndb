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
// AMENDMENTS, all pre-registered in #798 BEFORE any number was produced, and
// each traceable to the clause that implements it:
//
//	D1  zero-relevance queries are EXCLUDED, COUNTED, and the power gates
//	    re-bind to the retained series (ctNDCGAt10's second return value,
//	    ctVaultFromSeries, U2's three new clauses, the census block)
//	D2  BASE-LEVEL-ONLY is added as a fifth arm; K2 gains Delta_HP and K4 is
//	    the veto on it (ctArmNames, K2, K4)
//	D3  S3 is DEMOTED to descriptive and leaves the SHIP conjunction
//	D4  KILL's actions are split by what evidence licenses them
//
// These are amendments to the RULE, not to a result: none was made after seeing
// an arm number, and each one only makes a conclusion harder to reach.
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
	MinPopulatedBuckets int     // U6: populated buckets required inside the window
	VaultsRequired      int     // U2
	VaultsSupporting    int     // S1/S3: how many vaults must carry the claim
	MinQueriesPerVault  int     // U2
	MinEventsPerVault   int     // U2
	MinJudgeKappa       float64 // U1
	MaxJudgeFPR         float64 // U1
	MinAdjudicatedPairs int     // U1: §3c's minimum human-adjudicated subsample
	MinReplayEdgeFrac   float64 // U4
	MaxUnreplayableFrac float64 // U4
	MaxCIHalfWidth      float64 // U5
	BootstrapResamples  int
}{
	MinDeltaC:         0.03,
	MinDeltaMechanism: 0.02,
	KillDeltaCPoint:   0.01,
	KillDeltaCCIUpper: 0.03,
	TrendMinPositive:  8,
	TrendWindow:       10,
	TrendSlopeCILower: -0.005,
	// DERIVED, not a new number: you cannot have TrendMinPositive positive
	// buckets out of the last TrendWindow if fewer than TrendMinPositive of
	// them contain any data. Pinned equal to TrendMinPositive by
	// TestCognitionTrialRule_EveryThresholdBinds.
	MinPopulatedBuckets: 8,
	VaultsRequired:      3,
	VaultsSupporting:    2,
	MinQueriesPerVault:  300,
	MinEventsPerVault:   200,
	MinJudgeKappa:       0.6,
	MaxJudgeFPR:         0.15,
	MinAdjudicatedPairs: 150,
	MinReplayEdgeFrac:   0.20,
	MaxUnreplayableFrac: 0.60,
	MaxCIHalfWidth:      0.03,
	BootstrapResamples:  10000,
}

// ---------------------------------------------------------------------------
// METRICS
// ---------------------------------------------------------------------------

// ctNDCGAt10 computes graded NDCG@10 for one query, and reports whether it is
// DEFINED AT ALL.
//
// ranked is the arm's returned order (memory keys). grades is the POOLED label
// set for this query — the union of every arm's top-10, labeled once, which is
// what makes the labels arm-neutral. The ideal DCG is computed over the pooled
// set, so all five arms are normalized by the same denominator; normalizing
// each arm by its own returned set would let a short list win by returning less.
//
// An unlabeled ranked item contributes gain 0 rather than being skipped:
// skipping it would silently promote whatever came after it, which is the arm
// flattering itself.
//
// # THE SECOND RETURN VALUE, AND WHY IT IS NOT A CONVENIENCE
//
// When the pooled label set contains no relevant document, IDCG is 0 and NDCG
// is 0/0 — undefined. This function used to say exactly that in a comment and
// then `return 0`, which is the SAME DEFECT SHAPE this file family has now hit
// three times: ctMean(nil)==0 padding absent trend buckets as measured zeros
// (fixed by U6), the FTS `idf<=0 -> continue` path (fixed by COG-24's idfMax),
// and this one. A policy comment does not stop the fourth. A TYPE does.
//
// So the undefined case is unrepresentable as a number: ok is false and the
// value is NaN. `ndcg := ctNDCGAt10(...)` no longer compiles, and a caller that
// discards ok with `_` gets a NaN that poisons any mean it enters rather than a
// plausible zero that silently dilutes it. See ctInformative/ctVaultFromSeries
// for what the caller must do with it (D1: EXCLUDE, count, and re-bind the
// power gates to the retained series).
func ctNDCGAt10(ranked []string, grades map[string]int) (float64, bool) {
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
		return math.NaN(), false
	}
	return dcg / idcg, true
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
// THE ARMS
//
// FULL                 everything on                       LIVE run
// NO-PAS               no transition candidates injected   LIVE run
// NO-HEBBIAN           hebbianBoost := 0                   ARITHMETIC on FULL
// BASE-LEVEL-ONLY      hebbianBoost := 0; transition := 0  ARITHMETIC on NO-PAS
// CONTENT-MATCH-ONLY   contextualPrior := actrDenominator  ARITHMETIC on NO-PAS
//
// BASE-LEVEL-ONLY (D2) is the fifth arm and it exists because NOTHING ELSE
// CORRESPONDS TO THE CONFIGURATION A KILL VERDICT SHIPS. KILL sets
// hebbian_enabled:false + predictive_activation:false and KEEPS the ACT-R
// base-level prior — that is base-level ON with both boosts OFF, which was not
// an arm. The four marginals cannot reconstruct it under redundancy: two worlds
// exist that are bit-identical on all four arms while Delta_HP differs by the
// entire effect (one where Hebbian and PAS are fully redundant with each other,
// one where the whole benefit is base-level), and the general bound
// Delta_HP in [max(Delta_H, Delta_P), Delta_C] spans the whole range. Without
// it the trial yields NO INFORMATION ABOUT THE COST OF THE ACTION IT AUTHORIZES.
//
// It costs ZERO additional live runs: it is an arithmetic ablation of the
// existing NO-PAS run, exactly the relationship CONTENT-MATCH-ONLY already has
// to NO-PAS.
// ---------------------------------------------------------------------------

const (
	ctArmNameFull          = "FULL"
	ctArmNameNoPAS         = "NO-PAS"
	ctArmNameNoHebbian     = "NO-HEBBIAN"
	ctArmNameBaseLevelOnly = "BASE-LEVEL-ONLY"
	ctArmNameContentOnly   = "CONTENT-MATCH-ONLY"
)

// ctArmNames is the reporting order.
var ctArmNames = []string{
	ctArmNameFull, ctArmNameNoPAS, ctArmNameNoHebbian,
	ctArmNameBaseLevelOnly, ctArmNameContentOnly,
}

// ctMechanismsOffInArm names, for each arm, the mechanisms it removes relative
// to FULL. It is the bridge between an ARM (a measurement) and a CONFIGURATION
// (an action), and TestCognitionTrialRule_KillActionArmIsMeasured walks it in
// reverse: it reads the config knobs KILL's action text flips, maps them to the
// arm that IS that configuration, and requires some K-clause to read that arm.
var ctMechanismsOffInArm = map[string][]string{
	ctArmNameFull:          {},
	ctArmNameNoHebbian:     {"Hebbian"},
	ctArmNameNoPAS:         {"PAS"},
	ctArmNameBaseLevelOnly: {"Hebbian", "PAS"},
	ctArmNameContentOnly:   {"Hebbian", "PAS", "base-level"},
}

// ---------------------------------------------------------------------------
// D1: ZERO-RELEVANCE QUERIES ARE EXCLUDED, COUNTED, AND THE POWER GATES RE-BIND
// TO THE RETAINED SERIES.
//
// A query whose pooled label set contains nothing relevant has an UNDEFINED
// NDCG for every arm. It is NOT a correct abstention: internal/engine/engine.go
// writes the 0x29 recall event ONLY when len(items) > 0, so an abstention
// cannot appear in this population at all. What it is, is a recall that RETURNED
// items every one of which the judge graded irrelevant — a false-positive
// recall, and a real retrieval-quality number in its own right.
//
// Including it as a paired 0-vs-0 difference shrinks the mean by the informative
// fraction p AND the standard error by the same factor, so the t-statistic is
// invariant while the point estimate slides across the ABSOLUTE bars S1 and K1
// use. Measured on a synthetic 320-query series: the verdict walks
// INCONCLUSIVE-BUT-POWERED -> KILL as zero-relevance queries are appended, while
// the CI half-width only ever SHRINKS — so U5, which gates half-width, can never
// catch it. There was never a guard.
//
// Exclusion cannot manufacture a positive: Delta_inf = Delta_all / p is
// sign-preserving, applied identically to every arm, and the excluded pairs have
// difference exactly 0 independent of arm. If the informative effect is 0, the
// excluded series is 0. It is also what a Wilcoxon/sign test does by
// construction, which is why those are dilution-invariant.
// ---------------------------------------------------------------------------

// ctQuerySeries is one vault's per-query scores in evaluation order. Every
// slice is aligned index-for-index with Defined, which records whether that
// query's NDCG existed at all.
type ctQuerySeries struct {
	// Defined[i] is ctNDCGAt10's ok for query i. False = zero-relevance.
	Defined []bool
	// NDCG and MRR are keyed by arm name; every arm carries one entry per
	// SCORED query, including the undefined ones (NaN), so the slices stay
	// aligned and the exclusion happens in exactly one place.
	NDCG map[string][]float64
	MRR  map[string][]float64
	// Bucket[i] is query i's weekly bucket index, for S3's descriptive series.
	Bucket []int
}

func ctNewQuerySeries(arms []string) *ctQuerySeries {
	s := &ctQuerySeries{NDCG: map[string][]float64{}, MRR: map[string][]float64{}}
	for _, a := range arms {
		s.NDCG[a] = nil
		s.MRR[a] = nil
	}
	return s
}

// ctInformativeCount is how many scored queries had a defined NDCG.
func (s *ctQuerySeries) ctInformativeCount() int {
	n := 0
	for _, d := range s.Defined {
		if d {
			n++
		}
	}
	return n
}

// ctZeroRelevanceCount is the excluded remainder — mandatory census output.
func (s *ctQuerySeries) ctZeroRelevanceCount() int {
	return len(s.Defined) - s.ctInformativeCount()
}

// ctInformative returns the sub-series at the indexes where the query's NDCG
// was defined. This is the ONLY place the exclusion happens.
func ctInformative(defined []bool, xs []float64) []float64 {
	out := make([]float64, 0, len(xs))
	for i, d := range defined {
		if i >= len(xs) {
			break
		}
		if d {
			out = append(out, xs[i])
		}
	}
	return out
}

// ctZeroFilled returns the series with undefined entries written as 0 — the
// DILUTED form. It exists so the all-queries number can still be REPORTED under
// a name that says what it is. Nothing gates on it.
func ctZeroFilled(defined []bool, xs []float64) []float64 {
	out := make([]float64, 0, len(xs))
	for i := range xs {
		if i < len(defined) && !defined[i] {
			out = append(out, 0)
			continue
		}
		out = append(out, xs[i])
	}
	return out
}

// ctVaultFromSeries builds a vault result from the raw per-query series. It is
// the seam D1's dilution-invariance property test drives, and it is what the
// harness calls, so the property is proved about the code that runs.
func ctVaultFromSeries(label string, s *ctQuerySeries, distinctEvents, nBuckets int, seed int64) ctVaultResult {
	inf := func(arm string) []float64 { return ctInformative(s.Defined, s.NDCG[arm]) }
	boot := func(a, b string) ctDelta {
		return ctPairedBootstrap(inf(a), inf(b), ctPreregistered.BootstrapResamples, seed)
	}
	mrrDelta := func(a, b string) float64 {
		return ctMean(ctInformative(s.Defined, s.MRR[a])) - ctMean(ctInformative(s.Defined, s.MRR[b]))
	}

	perBucket := make([][]float64, nBuckets)
	full, content := s.NDCG[ctArmNameFull], s.NDCG[ctArmNameContentOnly]
	for i, d := range s.Defined {
		if !d || i >= len(s.Bucket) || i >= len(full) || i >= len(content) {
			continue
		}
		b := s.Bucket[i]
		if b < 0 || b >= nBuckets {
			continue
		}
		perBucket[b] = append(perBucket[b], full[i]-content[i])
	}
	bucketDeltas, omitted := ctBucketDeltas(perBucket)

	return ctVaultResult{
		Label:                label,
		NQueries:             s.ctInformativeCount(),
		ZeroRelevanceQueries: s.ctZeroRelevanceCount(),
		DistinctEvents:       distinctEvents,
		DeltaC:               boot(ctArmNameFull, ctArmNameContentOnly),
		DeltaH:               boot(ctArmNameFull, ctArmNameNoHebbian),
		DeltaP:               boot(ctArmNameFull, ctArmNameNoPAS),
		DeltaHP:              boot(ctArmNameFull, ctArmNameBaseLevelOnly),
		DeltaCAllQueriesDiluted: ctPairedBootstrap(
			ctZeroFilled(s.Defined, s.NDCG[ctArmNameFull]),
			ctZeroFilled(s.Defined, s.NDCG[ctArmNameContentOnly]),
			ctPreregistered.BootstrapResamples, seed),
		MRRDeltaH:      mrrDelta(ctArmNameFull, ctArmNameNoHebbian),
		MRRDeltaP:      mrrDelta(ctArmNameFull, ctArmNameNoPAS),
		DeltaCByBucket: bucketDeltas,
		OmittedBuckets: omitted,
	}
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

// ctBucketDelta is ONE WEEKLY BUCKET's mean Delta_C, carrying its bucket index
// and the number of queries it was computed from.
//
// WHY THE INDEX TRAVELS WITH THE VALUE. The trend series used to be a dense
// []float64 with one entry per bucket, built as ctMean(perBucket[b]) — and
// ctMean(nil) is 0, so a bucket with NO EVALUATED QUERIES entered the
// regression as a measured Delta_C of exactly zero. That is absent data scored
// as a result. Demonstrated: six real +0.04 buckets followed by six empty ones
// manufacture an OLS slope of +0.005 with a CI lower bound of +0.003 — S3's
// slope gate PASSING on six buckets of data that do not exist — and reversing
// the padding fails the same gate on the same non-data. Sparse buckets are
// anticipated by the design's risk 8 (0x29 retention is 90 days but pruning is
// amortized every 256th write PROCESS-WIDE, so a vault's usable window is
// uneven), so this is honoring the design rather than departing from it.
//
// Empty buckets are now OMITTED, and the regression uses the real bucket index
// as its x — omitting a bucket must not silently re-space the ones that remain.
type ctBucketDelta struct {
	Bucket   int     // weekly bucket index, 0-based
	Mean     float64 // mean Delta_C over that bucket's evaluated queries
	NQueries int     // how many queries it was computed from; never 0
}

// ctBucketDeltas builds the trend series from per-bucket per-query deltas,
// dropping empty buckets and reporting how many were dropped. The count is
// mandatory output: "the trend was fitted on 7 of 12 buckets" is a materially
// different claim from "the trend was fitted on 12 buckets".
func ctBucketDeltas(perBucket [][]float64) (series []ctBucketDelta, omitted []int) {
	for b, vals := range perBucket {
		if len(vals) == 0 {
			omitted = append(omitted, b)
			continue
		}
		series = append(series, ctBucketDelta{Bucket: b, Mean: ctMean(vals), NQueries: len(vals)})
	}
	return series, omitted
}

// ctBucketSeries lifts a dense per-bucket slice into the indexed form. Used by
// the rule's own tests, where every bucket is populated by construction.
func ctBucketSeries(vals []float64) []ctBucketDelta {
	out := make([]ctBucketDelta, len(vals))
	for i, v := range vals {
		out[i] = ctBucketDelta{Bucket: i, Mean: v, NQueries: 1}
	}
	return out
}

// ctTrend is the OLS regression of Delta_C on bucket index, with a t-based 95%
// interval on the slope.
type ctTrend struct {
	Slope        float64
	SlopeCILower float64
	// PositiveOfLastN counts POPULATED buckets with a positive mean whose index
	// falls in the trailing window. PopulatedInWindow is how many buckets in
	// that window had any data at all — the denominator the count is against,
	// and the quantity U6 gates.
	PositiveOfLastN   int
	PopulatedInWindow int
	WindowN           int
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

// ctComputeTrend takes the POPULATED weekly buckets, in bucket order. Absent
// buckets are absent — they are neither regressed as zeros nor counted as
// non-positive, because "no queries fell in week 9" is not an observation about
// week 9's Delta_C.
//
// The trailing window is measured in BUCKET INDEX, not in list position: with
// omissions those differ, and "the last 10 weeks" means weeks, not entries.
func ctComputeTrend(series []ctBucketDelta, window int) ctTrend {
	n := len(series)
	tr := ctTrend{WindowN: window}
	if n == 0 {
		tr.SlopeCILower = math.Inf(-1)
		return tr
	}
	lastBucket := series[n-1].Bucket
	for _, d := range series {
		if d.Bucket <= lastBucket-window {
			continue
		}
		tr.PopulatedInWindow++
		if d.Mean > 0 {
			tr.PositiveOfLastN++
		}
	}
	if n < 3 {
		tr.SlopeCILower = math.Inf(-1)
		return tr
	}
	var sx, sy float64
	for _, d := range series {
		sx += float64(d.Bucket)
		sy += d.Mean
	}
	mx, my := sx/float64(n), sy/float64(n)
	var sxy, sxx float64
	for _, d := range series {
		dx := float64(d.Bucket) - mx
		sxy += dx * (d.Mean - my)
		sxx += dx * dx
	}
	if sxx == 0 {
		tr.SlopeCILower = math.Inf(-1)
		return tr
	}
	slope := sxy / sxx
	intercept := my - slope*mx
	var sse float64
	for _, d := range series {
		resid := d.Mean - (intercept + slope*float64(d.Bucket))
		sse += resid * resid
	}
	df := n - 2
	se := math.Sqrt(sse / float64(df) / sxx)
	tr.Slope = slope
	tr.SlopeCILower = slope - ctT975(df)*se
	return tr
}

// ctMean is the arithmetic mean, 0 for an empty slice. It lives HERE, beside
// ctBucketDeltas, because ctMean(nil) == 0 is exactly the identity that made
// absent buckets look like measured zeros: the two belong in one place where a
// reader meets the trap and its handling together.
func ctMean(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := 0.0
	for _, x := range xs {
		s += x
	}
	return s / float64(len(xs))
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
	// HumanRelevant / HumanIrrelevant are the human marginal counts. Both must
	// be non-zero or the sample cannot measure agreement: with every pair on one
	// side of the binarization, kappa is 1 by the degenerate 1-pe==0 branch and
	// the FPR denominator is empty, so a sample carrying zero discriminative
	// information reports a perfect calibration. §3c's planted negatives exist
	// to prevent exactly that; these two counts are what ASSERTS they were there.
	HumanRelevant   int
	HumanIrrelevant int
	// LabelHashMatches records that the SHA-256 of the sorted
	// (queryHash, memoryID, grade) triples used for scoring equals the hash
	// frozen in the run log before scoring. S5.
	LabelHashMatches bool
}

// ctNullResult is S6's shuffled-seed null on ONE vault, as a TRI-STATE. "Not
// run" is not "survived": K4's veto requires that the vetoing vault's effect
// survives a shuffled seed, and a null that was never run cannot say that.
//
// The SHUFFLED-SEED arm itself is the instrument-repair increment's item
// (#797/F1, design v2 §2.4), so ctNullNotRun is the honest state of every vault
// today. Wiring the FIELD now is what stops the arm landing later against a rule
// that has nowhere to put its answer.
type ctNullResult int

const (
	ctNullNotRun ctNullResult = iota
	ctNullSurvived
	ctNullReproducedByShuffle
)

func (r ctNullResult) String() string {
	switch r {
	case ctNullSurvived:
		return "survived the shuffled-seed null"
	case ctNullReproducedByShuffle:
		return "REPRODUCED by a shuffled seed"
	default:
		return "shuffled-seed null NOT RUN"
	}
}

// ctVaultResult is one vault's contribution. Vaults are A/B/C; the label is the
// only identifier that ever exists here, by construction.
type ctVaultResult struct {
	Label string
	// NQueries is the count of INFORMATIVE labeled held-out queries — those with
	// a defined NDCG. D1: zero-relevance queries are excluded from every delta,
	// so the power gates U2 and U5 must bind to the retained series or they are
	// gating a sample size the estimate was not computed from. This makes the
	// eligibility arithmetic worse (300 INFORMATIVE queries per vault) and that
	// honesty is the point.
	NQueries int
	// ZeroRelevanceQueries is the excluded remainder: recalls that returned items
	// every one of which the judge graded irrelevant. Mandatory census output,
	// emitted BEFORE any delta. It is a real retrieval-quality number and the
	// only visible symptom of a too-conservative judge — U1 gates the judge's
	// false-positive rate but nothing gates its false-NEGATIVE rate.
	ZeroRelevanceQueries int
	// DistinctEvents is the vault's distinct recall events after dedup (U2).
	DistinctEvents int

	DeltaC  ctDelta // FULL - CONTENT-MATCH-ONLY
	DeltaH  ctDelta // FULL - NO-HEBBIAN
	DeltaP  ctDelta // FULL - NO-PAS
	DeltaHP ctDelta // FULL - BASE-LEVEL-ONLY: what a KILL verdict actually costs

	// DeltaCAllQueriesDiluted is Delta_C over ALL scored queries, zero-relevance
	// ones included as paired 0-vs-0. It is REPORTED so the dilution is visible
	// and GATES NOTHING. The name says what it is on purpose.
	DeltaCAllQueriesDiluted ctDelta

	// ShuffledSeedNull is S6's result ON THIS VAULT. K4's veto requires it.
	ShuffledSeedNull ctNullResult

	MRRDeltaH float64 // sign-agreement input for S4
	MRRDeltaP float64

	// DeltaCByBucket is S3's series: the POPULATED weekly buckets only, in
	// bucket order. OmittedBuckets are the bucket indexes that had no evaluated
	// query — reported, never regressed as zeros. DESCRIPTIVE since D3: see
	// ctDecide's S3 block for why it is not in the SHIP conjunction.
	DeltaCByBucket []ctBucketDelta
	OmittedBuckets []int

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
// THE ABLATION-ADDITIVITY BOUNDS (U3-class)
// ---------------------------------------------------------------------------

// ctAdditivityViolation checks the two bounds that make the redundancy gap
// VISIBLE rather than assumed:
//
//	Delta_HP >= max(Delta_H, Delta_P)   removing BOTH boosts cannot hurt less
//	                                    than removing either one alone
//	Delta_HP <= Delta_C                 removing both boosts cannot hurt more
//	                                    than also removing the base-level prior
//
// A violation is not a small result — it means the ARITHMETIC ABLATION IS NOT
// WHAT IT CLAIMS TO BE, which is a U3-class failure (the reconstruction is not
// the thing it says it is), so ctDecide routes it to UNDERPOWERED rather than
// letting a number be quoted from it.
//
// The tolerance is the bootstrap's own resolution, not a fudge: the four deltas
// are separate resamples of the same paired series, so a boundary case can
// disagree in the last few digits without anything being wrong.
const ctAdditivityTolerance = 1e-9

func ctAdditivityViolation(v ctVaultResult) string {
	lower := math.Max(v.DeltaH.Point, v.DeltaP.Point)
	if v.DeltaHP.Point < lower-ctAdditivityTolerance {
		return fmt.Sprintf("vault %s: Delta_HP %+.4f < max(Delta_H %+.4f, Delta_P %+.4f) — "+
			"removing BOTH boosts hurt LESS than removing one, so the arithmetic ablation is "+
			"not the configuration it claims to be", v.Label, v.DeltaHP.Point,
			v.DeltaH.Point, v.DeltaP.Point)
	}
	if v.DeltaHP.Point > v.DeltaC.Point+ctAdditivityTolerance {
		return fmt.Sprintf("vault %s: Delta_HP %+.4f > Delta_C %+.4f — removing both boosts hurt "+
			"MORE than also removing the base-level prior, which the ablation cannot produce",
			v.Label, v.DeltaHP.Point, v.DeltaC.Point)
	}
	return ""
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

	// --- THE CENSUS, BEFORE ANY DELTA ----------------------------------------
	// The zero-relevance fraction is mandatory output, not a footnote. It is a
	// retrieval-quality number in its own right (these are recalls that RETURNED
	// items the judge graded entirely irrelevant — the 0x29 event is only written
	// when len(items) > 0, so no abstention can be hiding in here), and it is the
	// only visible symptom of a too-conservative judge: U1 gates the judge's
	// false-POSITIVE rate and nothing gates its false-negative rate, so a judge
	// that grades everything 0 shows up HERE or nowhere.
	census := make([]string, 0, len(vaults)+1)
	for _, v := range vaults {
		scored := v.NQueries + v.ZeroRelevanceQueries
		frac := 0.0
		if scored > 0 {
			frac = float64(v.ZeroRelevanceQueries) / float64(scored)
		}
		census = append(census, fmt.Sprintf(
			"CENSUS vault %s: %d scored queries = %d informative + %d zero-relevance (%.1f%% "+
				"zero-relevance); %d distinct recall events; %s",
			v.Label, scored, v.NQueries, v.ZeroRelevanceQueries, 100*frac,
			v.DistinctEvents, v.ShuffledSeedNull))
	}
	census = append(census, "CENSUS: zero-relevance queries are EXCLUDED from every delta (D1). "+
		"They are false-positive recalls, not abstentions — an abstention cannot appear in this "+
		"population. Delta_C over ALL queries is reported below as DILUTED and gates nothing.")

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
		// The SIZE of the adjudicated subsample. §3c pre-registers "a uniformly
		// random 15% subsample (minimum 150 pairs per vault)". Without this,
		// ONE agreeing pair yields kappa=1.000, fpr=0.000 and a clean SHIP —
		// demonstrated, which is why the field is now read rather than merely
		// declared. (The rule takes one calibration for the run; the minimum is
		// pre-registered PER VAULT, so this is the weakest form of the gate and
		// the operator must still confirm each vault's own subsample.)
		if judge.N < ctPreregistered.MinAdjudicatedPairs {
			u = append(u, fmt.Sprintf("U1: the judge-calibration subsample has %d adjudicated "+
				"pairs, need %d (§3c, per vault). A kappa computed on a handful of pairs is "+
				"not a calibration.", judge.N, ctPreregistered.MinAdjudicatedPairs))
		}
		// The SHAPE of it. If every adjudicated pair falls on one side of the
		// grade>=2 binarization, kappa is 1 by the degenerate 1-pe==0 branch and
		// the FPR denominator is empty: perfect agreement carrying ZERO
		// discriminative information. Demonstrated both ways — 200 all-relevant
		// pairs and 200 all-irrelevant pairs each produced kappa=1.000 fpr=0.000
		// and a SHIP. §3c's ~8% planted negatives exist to make the irrelevant
		// side non-empty; these two counts are what ASSERTS they were there
		// instead of trusting that they were.
		if judge.HumanIrrelevant < 1 {
			u = append(u, "U1: the judge-calibration subsample contains no human-IRRELEVANT "+
				"pair, so the judge false-positive rate has an EMPTY DENOMINATOR and kappa's "+
				"expected agreement is degenerate. §3c's planted negatives are what prevent "+
				"this; a 0.0% FPR measured over nothing is not a 0.0% FPR.")
		}
		if judge.HumanRelevant < 1 {
			u = append(u, "U1: the judge-calibration subsample contains no human-RELEVANT "+
				"pair — the mirror-image degeneracy, kappa=1 by the same 1-pe==0 branch over "+
				"a sample that cannot show the judge agreeing about anything relevant.")
		}
	}

	// --- U2: sample size -----------------------------------------------------
	if len(vaults) < ctPreregistered.VaultsRequired {
		u = append(u, fmt.Sprintf("U2: %d vaults, need %d", len(vaults), ctPreregistered.VaultsRequired))
	}
	for _, v := range vaults {
		// D1: a vault where EVERY query was zero-relevance has an EMPTY Delta_C
		// series, and ctPairedBootstrap(nil, nil, ...) returns the zero value —
		// Point 0, CI [0,0], N 0. That makes K1's clause TRUE and U5's clause
		// FALSE, so such a vault votes KILL WITH PERFECT CONFIDENCE. Two of them
		// plus one strong vault produced a KILL verdict through the unmodified
		// rule: "we did not look" rendered as "we looked and found nothing",
		// which is #797's exact shape reappearing in the Delta_C series.
		//
		// Informative-N must travel with the number, and informative-N of 0 must
		// route HERE, never to a K1 vote.
		if v.DeltaC.N == 0 {
			u = append(u, fmt.Sprintf("U2: vault %s has NO INFORMATIVE QUERY — every scored query "+
				"was zero-relevance (%d of them), so Delta_C was computed from an empty series. "+
				"An empty paired bootstrap returns Point 0 with a zero-width CI, which reads as "+
				"a confident null. It is an ABSENCE OF MEASUREMENT.",
				v.Label, v.ZeroRelevanceQueries))
		}
		// D1: the load-bearing wire. U2 reads NQueries and U5 reads
		// DeltaC.halfWidth(), and until now NOTHING read DeltaC.N — so a harness
		// that reported a pre-exclusion NQueries beside a post-exclusion interval
		// would gate power on one sample and estimate on another, silently. Under
		// an exclusion policy that is the whole ballgame, so it is asserted rather
		// than trusted.
		if v.NQueries != v.DeltaC.N {
			u = append(u, fmt.Sprintf("U2: vault %s reports %d informative queries but Delta_C was "+
				"computed over %d. The power gates and the estimate must bind to the SAME series.",
				v.Label, v.NQueries, v.DeltaC.N))
		}
		if v.DeltaHP.N != v.DeltaC.N {
			u = append(u, fmt.Sprintf("U2: vault %s computed Delta_HP over %d queries and Delta_C "+
				"over %d. K2 and K4 read Delta_HP; it must be the same paired series.",
				v.Label, v.DeltaHP.N, v.DeltaC.N))
		}
		if v.NQueries < ctPreregistered.MinQueriesPerVault {
			u = append(u, fmt.Sprintf("U2: vault %s has %d INFORMATIVE labeled held-out queries "+
				"(%d more were zero-relevance and excluded), need %d",
				v.Label, v.NQueries, v.ZeroRelevanceQueries, ctPreregistered.MinQueriesPerVault))
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
	for _, v := range vaults {
		if msg := ctAdditivityViolation(v); msg != "" {
			u = append(u, "U3 (ablation additivity): "+msg)
		}
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
	// --- U6: the weekly trend cannot be evaluated -----------------------------
	// Not in §6's numbered list, and deliberately added rather than assumed: §6
	// pre-registers a TREND over 12 weekly checkpoints, and design risk 8 warns
	// the buckets will be uneven. When too few buckets in the trailing window
	// contain any evaluated query, the honest answer is that S3 has no data —
	// NOT a slope of zero, which is what padding the gaps with ctMean(nil)==0
	// produced (six real +0.04 buckets plus six empty ones manufactured a
	// +0.005 slope with a CI lower bound of +0.003, i.e. S3 PASSING on six
	// buckets that do not exist).
	//
	// This gate can only ever turn a fabricated conclusion into UNDERPOWERED —
	// it cannot rescue a result, so it is not a threshold moved to fit numbers.
	for _, v := range vaults {
		tr := ctComputeTrend(v.DeltaCByBucket, ctPreregistered.TrendWindow)
		if tr.PopulatedInWindow < ctPreregistered.MinPopulatedBuckets {
			u = append(u, fmt.Sprintf("U6: vault %s has %d populated weekly buckets in the "+
				"trailing window of %d (%d omitted as empty overall), below the %d needed to "+
				"evaluate S3's trend. An empty bucket is ABSENT DATA, not a Delta_C of zero.",
				v.Label, tr.PopulatedInWindow, ctPreregistered.TrendWindow,
				len(v.OmittedBuckets), ctPreregistered.MinPopulatedBuckets))
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
		return ctDecision{Verdict: ctVerdictUnderpowered, Reasons: append(census, u...)}
	}

	// --- SHIP ---------------------------------------------------------------
	ship, kill := append([]string(nil), census...), []string(nil)

	// The diluted number, reported and gating nothing, so the exclusion is
	// auditable rather than asserted.
	for _, v := range vaults {
		ship = append(ship, fmt.Sprintf(
			"REPORTED (gates nothing): vault %s Delta_C over ALL %d scored queries = %+.4f "+
				"CI [%+.4f, %+.4f]; over the %d INFORMATIVE queries = %+.4f CI [%+.4f, %+.4f]",
			v.Label, v.DeltaCAllQueriesDiluted.N, v.DeltaCAllQueriesDiluted.Point,
			v.DeltaCAllQueriesDiluted.CILower, v.DeltaCAllQueriesDiluted.CIUpper,
			v.DeltaC.N, v.DeltaC.Point, v.DeltaC.CILower, v.DeltaC.CIUpper))
	}

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

	// S3 — DESCRIPTIVE SINCE D3, NOT A SHIP GATE.
	//
	// S3 was pre-registered as a TREND OVER 12 WEEKLY CHECKPOINTS, and the code
	// does not compute that: ctReplay runs ONCE, its checkpoint callback only
	// logs, and every arm scores against the FINAL graph. What is actually
	// computed is a per-bucket breakdown of a single measurement — the same
	// evidence S1 already rests on, re-read through a different lens.
	//
	// Leaving it in the SHIP conjunction gates the verdict on a quantity the code
	// does not produce, which is #797's shape (a gate that cannot bind on
	// evidence it never receives). The alternative — score each arm against the
	// graph AS OF each checkpoint — is correct and is deferred, because equal
	// -width weekly buckets need ~84 days of recall corpus and redefining
	// "weekly" to mean ~26 hours to make the gate satisfiable today would be
	// exactly the tuning this rule exists to prevent.
	//
	// It is still COMPUTED AND REPORTED. U6's empty-bucket guard stays regardless:
	// an absent bucket is absent data whether or not a verdict depends on it.
	s3Vaults := 0
	var trendDetail []string
	for _, v := range vaults {
		tr := ctComputeTrend(v.DeltaCByBucket, ctPreregistered.TrendWindow)
		ok := tr.PositiveOfLastN >= ctPreregistered.TrendMinPositive &&
			tr.SlopeCILower >= ctPreregistered.TrendSlopeCILower
		if ok {
			s3Vaults++
		}
		trendDetail = append(trendDetail, fmt.Sprintf(
			"%s:%d/%d pos over %d populated of the last %d (%d buckets omitted as empty), "+
				"slope %.5f (CI lo %.5f)",
			v.Label, tr.PositiveOfLastN, ctPreregistered.TrendMinPositive, tr.PopulatedInWindow,
			tr.WindowN, len(v.OmittedBuckets), tr.Slope, tr.SlopeCILower))
	}
	s3 := s3Vaults >= ctPreregistered.VaultsSupporting
	ship = append(ship, fmt.Sprintf("S3 %s (DESCRIPTIVE ONLY — gates nothing since D3; this is a "+
		"per-bucket breakdown of ONE measurement against the FINAL graph, not a checkpoint "+
		"trend): would-hold on %d/%d vaults [%s]",
		ctPassFail(s3), s3Vaults, len(vaults), strings.Join(trendDetail, "; ")))

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

	if s1 && s2 && s4 && s5 {
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

	// K2 — restated by D2 to include Delta_HP. S2 asks whether a NAMED mechanism
	// earns its keep; K2 asks whether ANY of them does, and the configuration
	// KILL actually ships (both boosts off, base-level kept) is the BASE-LEVEL-ONLY
	// arm, whose delta is Delta_HP. Omitting it let KILL be authorized by three
	// marginals that provably cannot reconstruct it.
	hpVault := ""
	for _, v := range vaults {
		if v.DeltaHP.Point >= ctPreregistered.MinDeltaMechanism && v.DeltaHP.CILower > 0 {
			hpVault = v.Label
			break
		}
	}
	k2 := !s2 && hpVault == ""
	kill = append(kill, fmt.Sprintf("K2 %s: neither Delta_H nor Delta_P nor Delta_HP reaches +%.2f "+
		"with CI lower > 0 on any vault%s", ctPassFail(k2), ctPreregistered.MinDeltaMechanism,
		ctIf(hpVault != "", fmt.Sprintf("; vault %s's Delta_HP does", hpVault))))

	// K3 — re-asserted rather than implied by control flow, so it appears in
	// the audit trail.
	k3 := judge.Ran && judge.Kappa >= ctPreregistered.MinJudgeKappa &&
		judge.FPR <= ctPreregistered.MaxJudgeFPR &&
		judge.N >= ctPreregistered.MinAdjudicatedPairs &&
		judge.HumanIrrelevant >= 1 && judge.HumanRelevant >= 1
	for _, v := range vaults {
		if v.NQueries < ctPreregistered.MinQueriesPerVault {
			k3 = false
		}
	}
	kill = append(kill, fmt.Sprintf("K3 %s: power was adequate (judge gate passed, n >= %d per vault, U2/U4/U5 clear)",
		ctPassFail(k3), ctPreregistered.MinQueriesPerVault))

	// K4 — THE VETO, ON Delta_HP (D2).
	//
	// KILL is blocked if the configuration KILL SHIPS costs a vault at least
	// MinDeltaC with CI lower > 0 — the S1 per-vault bar, applied to the quantity
	// the action actually spends. The integrity condition matters as much as the
	// bar: THE VETOING VAULT MUST ITSELF HAVE SURVIVED THE S6 SHUFFLED-SEED NULL.
	// A veto that a shuffled seed reproduces is ambient lift, not memory, and is
	// not a veto. A veto whose null was never run is not a veto either — an unrun
	// gate is not a passed gate, the same reading U1 takes.
	//
	// SUBSUMPTION, RECORDED SO IT IS NOT MISTAKEN FOR INDEPENDENT EVIDENCE:
	// K4's bar (MinDeltaC = 0.03) is ABOVE K2's (MinDeltaMechanism = 0.02) and
	// both must hold for KILL, so as long as MinDeltaC >= MinDeltaMechanism every
	// K4 veto is already blocked by K2's Delta_HP clause and K4 cannot change a
	// verdict on its own. It is retained because it is what was pre-registered,
	// because it names the veto and its integrity condition in the audit trail,
	// and because it becomes load-bearing the moment the two bars are re-ordered.
	// TestCognitionTrialRule_K4IsSubsumedByK2 pins the relationship so a future
	// edit to either bar surfaces it instead of silently changing what KILL means.
	k4Vetoing, k4Weak := "", ""
	for _, v := range vaults {
		if v.DeltaHP.Point >= ctPreregistered.MinDeltaC && v.DeltaHP.CILower > 0 {
			if v.ShuffledSeedNull == ctNullSurvived {
				k4Vetoing = v.Label
				break
			}
			k4Weak = fmt.Sprintf("%s (%s)", v.Label, v.ShuffledSeedNull)
		}
	}
	k4 := k4Vetoing == ""
	kill = append(kill, fmt.Sprintf("K4 %s: no vault's Delta_HP — what KILL actually costs, the "+
		"FULL minus %s arm — reaches +%.2f with CI lower > 0 AND survives the S6 shuffled-seed "+
		"null%s%s", ctPassFail(k4), ctArmNameBaseLevelOnly, ctPreregistered.MinDeltaC,
		ctIf(k4Vetoing != "", fmt.Sprintf("; vault %s VETOES the kill", k4Vetoing)),
		ctIf(k4Vetoing == "" && k4Weak != "",
			fmt.Sprintf("; vault %s clears the bar but its null did not qualify it to veto", k4Weak))))

	if k1 && k2 && k3 && k4 {
		kill = append(kill,
			"KILL: retire the cognitive layer. Executes in one PR referencing the design: "+
				"default preset hebbian_enabled:false + predictive_activation:false (now actually "+
				"effective on the read side, COG-31), with the per-vault override that already "+
				"ships left in place; record the negative in the decision record; rewrite the "+
				"README's first clause; KEEP the ACT-R base-level prior unless increment 2 "+
				"measures it out.")
		// D4: split by what evidence licenses what. A result that is REFUTED AT
		// THE PREMISE — a census, no labels needed — licenses DELETION; that is
		// the #609 precedent. A mechanism that FIRES BUT BELOW THE BAR licenses a
		// DEFAULT CHANGE with the per-vault override, not a deletion.
		kill = append(kill,
			"NOT LICENSED BY THIS VERDICT, and deliberately not bundled into it: "+
				"internal/working and internal/episodic are measured by NO ARM of this trial. "+
				"Deciding their fate on this measurement is precisely the failure the design "+
				"exists to prevent. They need their own census, and it can run today.")
		kill = append(kill,
			"NOT VERDICT-DEPENDENT, and deliberately not bundled into it: deleting the dead SGD "+
				"feedback loop and correcting muninn_feedback's description are wrong under SHIP "+
				"too. They are their own change and should not wait on this trial or borrow its "+
				"authority.")
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
// It also returns the two MARGINAL COUNTS the gate needs to know whether the
// sample can measure anything at all. Perfect agreement on a sample that is
// entirely on one side of the binarization gives kappa=1 (via the 1-pe==0
// branch) and fpr=0 (via an empty denominator) while carrying zero
// discriminative information — see ctDecide's U1.
func ctCohensKappa(judge, human []int) (kappa, fpr float64, n, humanRelevant, humanIrrelevant int) {
	n = len(judge)
	if n == 0 || len(human) != n {
		return 0, 0, 0, 0, 0
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
	return kappa, fpr, n, int(bothRel + humanOnly), int(humanIrrel)
}
