//go:build localassets || cognitiontrial

package engine

import (
	"fmt"
	"math"
	"math/rand"
	"sort"
	"strings"
	"sync/atomic"
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
// D5, and it is a CORRECTION of D2 rather than a new amendment. D2 wired the
// NDCG-level ablation-additivity bounds in as a U3-class GATE. They cannot be
// one: they compare four POINT estimates with no interval, so under a null where
// the arms merely reorder results, Delta_H / Delta_P / Delta_HP are exchangeable,
// P(Delta_HP is the largest) = 1/3, and the lower bound is violated ~2/3 of the
// time BY CONSTRUCTION — measured at 92.5% / 90.7% / 91.8% / 92.7% for
// n = 320 / 1000 / 5000 / 20000. FLAT IN n: more data cannot fix it, which is the
// definition of a gate that is not an instrument check. End to end it produced
// UNDERPOWERED on 199 of 200 trials.
//
// And it could not simply be widened, because every violation DECODES TO A REAL
// WORLD: Delta_HP - Delta_C == NDCG(CONTENT-MATCH-ONLY) - NDCG(BASE-LEVEL-ONLY),
// so the upper bound breaks exactly when the ACT-R base-level prior is
// net-harmful, and the two lower-bound breaks decode the same way for each boost
// on top of it. The old comment here claimed the gate "suppresses a
// kill-supporting finding rather than a ship-supporting one — the direction the
// design says to err in". THAT WAS FALSE: it suppressed both, and it was worst
// precisely where a mechanism is HARMFUL, which is the finding a delete-or-keep
// trial most needs. So the NDCG-level bounds are DEMOTED to a reported
// diagnostic that names the harmful mechanism (ctAdditivityDiagnostic), and the
// genuinely arithmetic form of the same relation stays a hard invariant where it
// belongs: on RAW SCORES, in TestCognitionTrial_ArmReconstructionFidelity §6.
//
//	raw-score bounds  = an INVARIANT (non-negative boosts, increasing softplus,
//	                    softplus(bLevelCap)/D == 1) -> a test failure
//	NDCG-level bounds = an OBSERVATION about the mechanisms -> a reported finding
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
	// Arms and PoolDepth are PINS, not thresholds, and they are pre-registered
	// for a reason that is not obvious.
	//
	// Arms DUPLICATES ctArmNames AS A LITERAL ON PURPOSE — do not "DRY" it into
	// `Arms: ctArmNames`. A pin that is defined as the thing it pins cannot
	// detect a change to that thing: ctInstrumentPinViolation would compare
	// ctArmNames against itself and report agreement forever. The duplication IS
	// the mechanism.
	//
	// D1's exclusion is arm-NEUTRAL in the strong form — the pooled label set is
	// shared, so ctNDCGAt10's `ok` is the same for every arm (verified over
	// 50 000 random pools x 6 arms, zero divergence). But the POOL is the union
	// of the arms' top-PoolDepth, so WHICH queries are informative is a function
	// of the arm SET and the pooling DEPTH. D2 added a fifth arm: the same query
	// with the same ranking is zero-relevance under a 4-arm pool and informative
	// under a 5-arm pool where the extra arm surfaced one graded document. Any
	// later change to either re-partitions informative vs zero-relevance and
	// moves every ABSOLUTE bar S1 and K1 read.
	//
	// So a change to either is a RE-PRE-REGISTRATION and must fail loudly rather
	// than silently shift the bars. ctInstrumentPinViolation is what says so, and
	// the harness calls it with its own (env-configurable) pool depth before it
	// scores anything.
	Arms      []string
	PoolDepth int
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
	Arms: []string{
		ctArmNameFull, ctArmNameNoPAS, ctArmNameNoHebbian,
		ctArmNameBaseLevelOnly, ctArmNameContentOnly,
	},
	PoolDepth: 10,
}

// ctInstrumentPinViolation reports why the running instrument is not the
// pre-registered one, or "" if it is.
//
// It is deliberately a comparison of SETS plus the depth, not of the reporting
// order: reordering ctArmNames changes nothing about which queries are
// informative, while adding, removing or renaming an arm changes it for every
// vault, and so does moving the pooling depth.
func ctInstrumentPinViolation(arms []string, poolDepth int) string {
	ctRuleEntered()
	want := map[string]bool{}
	for _, a := range ctPreregistered.Arms {
		want[a] = true
	}
	got := map[string]bool{}
	for _, a := range arms {
		got[a] = true
	}
	var added, missing []string
	for a := range got {
		if !want[a] {
			added = append(added, a)
		}
	}
	for a := range want {
		if !got[a] {
			missing = append(missing, a)
		}
	}
	sort.Strings(added)
	sort.Strings(missing)
	var msgs []string
	if len(added) > 0 || len(missing) > 0 {
		msgs = append(msgs, fmt.Sprintf("the ARM SET has changed (added %v, missing %v; "+
			"pre-registered %v)", added, missing, ctPreregistered.Arms))
	}
	if poolDepth != ctPreregistered.PoolDepth {
		msgs = append(msgs, fmt.Sprintf("the POOLING DEPTH is %d, pre-registered %d",
			poolDepth, ctPreregistered.PoolDepth))
	}
	if len(msgs) == 0 {
		return ""
	}
	return strings.Join(msgs, "; ") + ". The pooled label set is the UNION of the arms' " +
		"top-PoolDepth results, so both of these decide WHICH queries have a defined NDCG at " +
		"all (D1). Changing either re-partitions informative vs zero-relevance and moves every " +
		"absolute bar S1 and K1 read. That is a RE-PRE-REGISTRATION, made explicitly and " +
		"BEFORE any number is looked at — not a config tweak."
}

// ---------------------------------------------------------------------------
// PROBE-BINDINGNESS INSTRUMENTATION
//
// The anti-inert-gate census (TestCognitionTrialRule_EveryThresholdBinds)
// requires a registered probe for every field of every input struct. It caught
// that a probe can be MISSING. It did not catch that a probe can be EMPTY: an
// `InertNewField` plus a `func(t *testing.T) {}` passed the whole census. So
// presence was enforced and BINDINGNESS was not, which is the same shape as the
// declared-and-never-read field the census exists to find.
//
// ctRuleCalls is incremented by every entry point of the rule. A probe that
// never enters the rule cannot be showing anything about it, and the census now
// fails such a probe by arithmetic rather than by review.
//
// THE LIMIT, stated precisely rather than implied: this makes an EMPTY probe
// decidably inert, and (with ctProbeAssertions) a probe that calls the rule and
// then discards the answer. It does NOT make bindingness decidable in general —
// `ctExpect(t, true, "")` after a ctDecide call still passes, and no in-process
// check can distinguish a vacuous assertion from a real one. The decidable form
// of that is mutation testing (perturb the rule, require the probe to fail),
// which is a separate harness and is NOT built here.
//
// Both counters are atomic because the harness (`cognitiontrial`) also calls
// these functions and this file is compiled into it.
// ---------------------------------------------------------------------------

var (
	ctRuleCalls       atomic.Int64
	ctProbeAssertions atomic.Int64
)

func ctRuleEntered() { ctRuleCalls.Add(1) }

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
// When EVERY POOLED GRADE IS 0, IDCG is 0 and NDCG is 0/0 — undefined.
//
// Say that precisely, because the obvious paraphrase is wrong and was in this
// comment: the trigger is NOT "no RELEVANT document". Relevance is the
// binarization grade >= ctRelevantGrade (2), but the NDCG gain vector gives
// grade 1 a gain of 1, so a pool whose every grade is 1 contains no relevant
// document and still has a perfectly well-defined IDCG. Executed: a four-item
// all-grade-1 pool returns (0.7654, true) and is RETAINED and SCORED. The
// undefined set is strictly smaller than the not-relevant set, and D1's
// "zero-relevance" exclusion means ALL GRADES ZERO — not "nothing relevant".
//
// This function used to say "no relevant document" in a comment and
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
	ctRuleEntered()
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
	ctRuleEntered()
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
// A query whose pooled label set is ALL GRADE 0 has an UNDEFINED
// NDCG for every arm. (All grade 0 — not merely 'nothing relevant': grade 1 is
// below the relevance binarization but carries NDCG gain, so an all-grade-1 pool
// is defined, retained and scored. See ctNDCGAt10.) It is NOT a correct abstention: internal/engine/engine.go
// writes the 0x29 recall event ONLY when len(items) > 0, so an abstention
// cannot appear in this population at all. What it is, is a recall that RETURNED
// items every one of which the judge graded 0 — a false-positive recall, and a
// real retrieval-quality number in its own right.
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
//
// AN ENTRY WITH NO DEFINEDNESS FLAG IS UNDEFINED, NOT A NUMBER TO BE COPIED.
// The old `i < len(defined) && !defined[i]` copied xs[i] verbatim for any index
// past the end of Defined, and ctNDCGAt10 writes NaN there — so a series whose
// Defined slice was one short leaked a NaN straight into
// DeltaCAllQueriesDiluted and printed it in the report. Same for a NaN sitting
// under a true flag, which is a contradiction the caller should never produce
// and which must not be laundered into a plausible number here. The length
// mismatch that causes it is itself an UNDERPOWERED condition (U2's N-consistency
// clauses); this guard is what stops the printed line being nonsense in the
// meantime.
func ctZeroFilled(defined []bool, xs []float64) []float64 {
	out := make([]float64, 0, len(xs))
	for i := range xs {
		if i >= len(defined) || !defined[i] || math.IsNaN(xs[i]) {
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
	return ctVaultFromSeriesResamples(label, s, distinctEvents, nBuckets, seed,
		ctPreregistered.BootstrapResamples)
}

// ctVaultFromSeriesResamples is ctVaultFromSeries with the resample count as a
// parameter. EVERY caller that produces a number the trial quotes goes through
// ctVaultFromSeries and gets the pre-registered 10 000; the parameter exists so
// a property test can sweep n up to 20 000 queries without spending
// 20 000 x 10 000 x 5 resample draws per trial.
//
// WHAT IS AND IS NOT SOUND ABOUT A REDUCED COUNT, corrected. The comment here
// used to end "The interval is not, and no test asserts on an interval from a
// reduced count." THE SECOND HALF WAS FALSE. Both reduced-count callers reach
// ctDecide, and S1, S2, K1, K2, K4 and U5 all read CILower/CIUpper — the
// additivity-convergence test asserts Verdict != UNDERPOWERED on vaults whose
// DeltaC interval came from 100 resamples, and that assertion depends on U5's
// half-width check against a 100-draw interval. It is the third claim of this
// kind on this branch to have been asserted rather than checked.
//
// The correct statement, and it is narrower:
//
//	ctDelta.Point is the EXACT arithmetic mean of the paired differences and is
//	never resampled, so it is bit-identical at any resample count. The INTERVAL
//	is not. A reduced count is therefore sound ONLY at a call site that reads no
//	interval, or one that reads an interval whose insensitivity to the count has
//	been CHECKED at that site — never by blanket assertion here.
//
// Both reduced-count call sites now carry that check AT THE SITE:
//
//   - TestCognitionTrialRule_AdditivityDoesNotConvergeAndIsNotAGate reaches
//     ctDecide and U5 reads its interval, so it re-decides its WIDEST-interval
//     trial at the pre-registered count on every run and requires the same
//     verdict. (Its intervals sit 1.46x inside U5's gate, not the order of
//     magnitude a comment here once guessed — which is why the check is a
//     rebuild and not a margin constant.)
//   - TestCognitionTrialRule_EveryDeltaNMustBindToTheSameSeries' `before` vault
//     reads .Point and .N and never reaches ctDecide at all.
func ctVaultFromSeriesResamples(label string, s *ctQuerySeries, distinctEvents, nBuckets int,
	seed int64, resamples int,
) ctVaultResult {
	inf := func(arm string) []float64 { return ctInformative(s.Defined, s.NDCG[arm]) }
	boot := func(a, b string) ctDelta {
		return ctPairedBootstrap(inf(a), inf(b), resamples, seed)
	}
	// The MRR arms carry their OWN N, taken from the same informative subset the
	// NDCG deltas use, and an absent or length-mismatched arm yields the zero
	// value — N of 0 — exactly as ctPairedBootstrap does. See ctMeanDelta.
	mrrDelta := func(a, b string) ctMeanDelta {
		ia := ctInformative(s.Defined, s.MRR[a])
		ib := ctInformative(s.Defined, s.MRR[b])
		if len(ia) == 0 || len(ia) != len(ib) {
			return ctMeanDelta{}
		}
		return ctMeanDelta{Point: ctMean(ia) - ctMean(ib), N: len(ia)}
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
			resamples, seed),
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

// ctMeanDelta is a difference of two arm MEANS, inseparable from the count of
// paired observations it was computed over. It exists for the MRR aggregates,
// which had neither.
//
// THE SIXTH INSTANCE OF THIS FILE FAMILY'S SIGNATURE DEFECT, and the first that
// is a MEAN rather than a RATIO — every prior sweep went looking for zero
// denominators. ctMean returns 0 for an empty slice, and the aggregate was
// `ctMean(inf(A)) - ctMean(inf(B))`, so an MRR arm that was never collected
// produced +0.0000: a plausible measured number. S4's entire job is to
// corroborate the S2 mechanism ON A SECOND METRIC, and it was deciding on a
// metric that may never have existed. Reproduced through the production builder,
// sole mutation `s.MRR[NO-HEBBIAN] = nil`:
//
//	HONEST                    : Delta_H=+0.0478  MRRDeltaH=-0.0798  -> INCONCLUSIVE-BUT-POWERED
//	UNMEASURED NO-HEBBIAN MRR : Delta_H=+0.0478  MRRDeltaH=+0.4992  -> SHIP
//
// and the mirror flips too: both MRR deltas at the Go zero turn a SHIP fixture
// into INCONCLUSIVE with an audit trail reading "S4 FAIL: MRR agrees in sign
// with NDCG@10", indistinguishable from genuine measured disagreement.
//
// Unlike the four NDCG deltas, MRR had no N and no U2 binding clause, so the
// truncation case slipped through as well: ctInformative breaks at
// `i >= len(xs)`, so an MRR arm half as long as Defined is SILENTLY TRUNCATED
// where the NDCG control is caught by U2's N-mismatch clause. Carrying N — and
// refusing to form the difference at all when the two arms disagree in length —
// types both absences at the place they are produced.
type ctMeanDelta struct {
	Point float64
	// N is the count of paired observations behind Point. Zero means the series
	// was ABSENT or length-mismatched, never "the two means were equal".
	N int
}

// ctNonFinite names the first non-finite component of a delta, or "" if all
// three are numbers. Used by U3's delta-arithmetic clause.
//
// NaN is reported before infinity across ALL THREE components rather than
// component-by-component, because the two mean different things — a NaN is 0/0,
// an infinity is x/0 — and a delta carrying both should be named by the worse
// one.
func ctNonFinite(d ctDelta) string {
	for _, f := range []struct {
		name string
		v    float64
	}{{"NaN point estimate", d.Point}, {"NaN CI lower bound", d.CILower},
		{"NaN CI upper bound", d.CIUpper}} {
		if math.IsNaN(f.v) {
			return f.name
		}
	}
	for _, f := range []struct {
		name string
		v    float64
	}{{"infinite point estimate", d.Point}, {"infinite CI lower bound", d.CILower},
		{"infinite CI upper bound", d.CIUpper}} {
		if math.IsInf(f.v, 0) {
			return f.name
		}
	}
	return ""
}

// ctNonFiniteScalar is ctNonFinite for a single number: it names WHY the value
// is not one, or "" if it is.
//
// It exists because ctNonFinite covered the four deltas and nothing else, while
// U4 compares a fifth float — UnreplayableFrac — against a threshold. `NaN >
// 0.60` is false, and FALSE MEANS "NO OBJECTION" everywhere in this rule, so an
// unmeasurable ratio read as a passed ceiling. That is the U3 delta-arithmetic
// clause's exact class, one field over, and "not reachable through today's
// harness" is the reasoning that clause was written to reject.
func ctNonFiniteScalar(v float64) string {
	switch {
	case math.IsNaN(v):
		return "NaN"
	case math.IsInf(v, 1):
		return "+Inf"
	case math.IsInf(v, -1):
		return "-Inf"
	}
	return ""
}

// ctPairedBootstrap resamples QUERIES (not observations) with replacement, so
// the pairing between arms is preserved in every resample — that is the whole
// point of the paired design and the reason the same queries run through every
// arm.
//
// seed is explicit so a run is reproducible from its run log.
func ctPairedBootstrap(a, b []float64, resamples int, seed int64) ctDelta {
	ctRuleEntered()
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
	ctRuleEntered()
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
	ctRuleEntered()
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
	// every one of which the judge graded 0. Mandatory census output,
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

	// The sign-agreement inputs for S4, each carrying the N it was computed
	// over. See ctMeanDelta: an unmeasured MRR arm used to render as a
	// measured +0.0000 and manufacture a SHIP.
	MRRDeltaH ctMeanDelta
	MRRDeltaP ctMeanDelta

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
// THE NDCG-LEVEL ABLATION-ADDITIVITY RELATIONS — A DIAGNOSTIC, NOT A GATE (D5)
//
// The three relations are
//
//	Delta_HP >= Delta_H     Delta_HP >= Delta_P     Delta_HP <= Delta_C
//
// and on RAW SCORES, per candidate, they are arithmetic invariants: the boosts
// are non-negative, softplus is strictly increasing, and softplus(bLevelCap)/D
// is exactly 1. That is where they are checked as a hard failure —
// TestCognitionTrial_ArmReconstructionFidelity §6, in the activation package.
//
// AT THE NDCG LEVEL THEY ARE NEITHER, AND THEY WERE WIRED IN AS A U3 GATE.
// Two independent reasons that had to be taken together:
//
//  1. NDCG IS NOT MONOTONE IN SCORES, so a mean-of-NDCG delta need not inherit
//     the per-candidate raw ordering at all. Comparing four POINT estimates with
//     a 1e-9 tolerance and no interval is not a test of anything: under a null
//     where the arms reorder results but none is systematically better, Delta_H,
//     Delta_P and Delta_HP are exchangeable, P(Delta_HP largest) = 1/3, and the
//     lower relation is broken ~2/3 of the time BY CONSTRUCTION. Measured
//     through this builder at sigma_d = 0.15: 92.5% / 90.7% / 91.8% / 92.7% of
//     600 trials at n = 320 / 1000 / 5000 / 20000. FLAT IN n — the signature of
//     a quantity no amount of data resolves. As a gate it produced UNDERPOWERED
//     on 199 of 200 end-to-end trials while a true strong effect shipped 199/200
//     and an exact null killed 50/50, so the harness was fine and the gate was
//     not.
//
//  2. IT COULD NOT BE WIDENED, BECAUSE EVERY BREAK DECODES TO A REAL WORLD:
//
//     Delta_HP - Delta_P  == NDCG(NO-PAS)             - NDCG(BASE-LEVEL-ONLY)
//     Delta_HP - Delta_H  == NDCG(NO-HEBBIAN)         - NDCG(BASE-LEVEL-ONLY)
//     Delta_HP - Delta_C  == NDCG(CONTENT-MATCH-ONLY) - NDCG(BASE-LEVEL-ONLY)
//
//     each of which is exactly one mechanism's own contribution measured on top
//     of the base-level prior. A break says that mechanism is NET-HARMFUL on
//     this vault. That is a finding, and it is the single most decision-relevant
//     finding a delete-or-keep trial can produce.
//
// The comment that stood here claimed routing both to UNDERPOWERED "suppresses a
// kill-supporting finding rather than a ship-supporting one — the direction the
// design says to err in". That was FALSE and is removed rather than softened: it
// suppressed BOTH, and it was worst precisely where a mechanism is harmful.
// Executed, one vault set per line:
//
//	base-level HARMFUL, boosts carry a real +0.065 (SHIP was right) -> UNDERPOWERED
//	EVERYTHING harmful — the layer actively hurts (KILL was right)  -> UNDERPOWERED
//	PAS-on-top-of-base HARMFUL, Hebbian carries the win             -> UNDERPOWERED
//
// So: the relations are REPORTED, DECODED and NAMED, and they gate nothing.
//
// NOT DONE, deliberately: an interval-based version (break the relation only
// when the paired CI on the DIFFERENCE of deltas clears zero) would be
// statistically honest and would converge in n. It is not added speculatively —
// it needs its own convergence evidence before it may gate anything, and a
// diagnostic that names the harmful mechanism already carries the information a
// reader needs.
// ---------------------------------------------------------------------------

// ctAdditivityTolerance guards FLOAT ROUNDING and nothing else — the residue is
// ~1e-16 in magnitude.
//
// The comment here used to call it "the bootstrap's own resolution ... four
// separate resamples of the same paired series". That was false twice over:
// ctDelta.Point is the exact arithmetic mean of the paired differences and is
// never resampled, and ctVaultFromSeries passes the SAME seed to all four
// bootstraps, so even the intervals are drawn from identical index sequences.
const ctAdditivityTolerance = 1e-9

// ctAdditivityDiagnostic decodes each broken relation into the finding it IS.
// It returns one sentence per net-harmful mechanism, and nothing at all when the
// relations hold. It is called for its report; no verdict reads it.
func ctAdditivityDiagnostic(v ctVaultResult) []string {
	ctRuleEntered()
	var out []string
	if v.DeltaHP.Point < v.DeltaP.Point-ctAdditivityTolerance {
		out = append(out, fmt.Sprintf("vault %s: THE HEBBIAN BOOST IS NET-HARMFUL on top of the "+
			"base-level prior. Delta_HP %+.4f < Delta_P %+.4f, and Delta_HP - Delta_P (%+.4f) is "+
			"exactly NDCG(NO-PAS) - NDCG(BASE-LEVEL-ONLY) — the Hebbian term's own contribution "+
			"with PAS already off. Negative means adding it LOWERED NDCG@10.",
			v.Label, v.DeltaHP.Point, v.DeltaP.Point, v.DeltaHP.Point-v.DeltaP.Point))
	}
	if v.DeltaHP.Point < v.DeltaH.Point-ctAdditivityTolerance {
		out = append(out, fmt.Sprintf("vault %s: PAS IS NET-HARMFUL on top of the base-level "+
			"prior. Delta_HP %+.4f < Delta_H %+.4f, and Delta_HP - Delta_H (%+.4f) is exactly "+
			"NDCG(NO-HEBBIAN) - NDCG(BASE-LEVEL-ONLY) — the transition term's own contribution "+
			"with Hebbian already off.",
			v.Label, v.DeltaHP.Point, v.DeltaH.Point, v.DeltaHP.Point-v.DeltaH.Point))
	}
	if v.DeltaHP.Point > v.DeltaC.Point+ctAdditivityTolerance {
		out = append(out, fmt.Sprintf("vault %s: THE ACT-R BASE-LEVEL PRIOR IS NET-HARMFUL. "+
			"Delta_HP %+.4f > Delta_C %+.4f, and Delta_HP - Delta_C (%+.4f) is exactly "+
			"NDCG(CONTENT-MATCH-ONLY) - NDCG(BASE-LEVEL-ONLY): the prior scored BELOW the content "+
			"gate alone. Note what this does to a KILL — KILL keeps the base-level prior, so on "+
			"this vault the configuration KILL ships is worse than content matching by itself.",
			v.Label, v.DeltaHP.Point, v.DeltaC.Point, v.DeltaHP.Point-v.DeltaC.Point))
	}
	return out
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
	ctRuleEntered()
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
				"zero-relevance); %d distinct recall events; sigma_d(Delta_C)=%.4f; %s",
			v.Label, scored, v.NQueries, v.ZeroRelevanceQueries, 100*frac,
			v.DistinctEvents, v.DeltaC.SDOfDiff, v.ShuffledSeedNull))
	}
	census = append(census, "CENSUS: zero-relevance queries — those whose every POOLED GRADE IS "+
		"ZERO, which is a strictly smaller set than 'nothing relevant' since grade 1 is not "+
		"relevant but does carry NDCG gain — are EXCLUDED from every delta (D1). They are "+
		"false-positive recalls, not abstentions: an abstention cannot appear in this population. "+
		"Delta_C over ALL queries is reported below as DILUTED and gates nothing. sigma_d is the "+
		"observed paired SD of the Delta_C differences, the input any power calculation needs; it "+
		"is reported, not gated.")

	// --- THE ABLATION-ADDITIVITY DIAGNOSTIC (D5) -----------------------------
	// A finding ABOUT THE MECHANISMS, decoded and named, printed with the census
	// and read by no verdict. See ctAdditivityDiagnostic for why this is not a
	// gate: the point-estimate comparison breaks ~2/3 of the time under an
	// exchangeable null and is FLAT IN n, and every break it does report decodes
	// to "this mechanism is net-harmful on this vault" — which must be surfaced,
	// never converted into "we could not measure".
	for _, v := range vaults {
		for _, m := range ctAdditivityDiagnostic(v) {
			census = append(census, "DIAGNOSTIC (a finding about the mechanisms; gates nothing, "+
				"by D5): "+m)
		}
	}

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
		// ALL FOUR DELTAS, not two. The clauses above typed absence for Delta_C
		// and Delta_HP and left Delta_H and Delta_P untyped — which is D1's exact
		// defect relocated one field over, because ctPairedBootstrap returns
		// {Point:0, CI:[0,0], N:0} on an empty OR LENGTH-MISMATCHED series. One
		// arm's series one element short is all it takes: reached through
		// ctVaultFromSeries, an intact run reporting Delta_H = +0.1000 over N=320
		// becomes Delta_H = +0.0000 CI [+0.0000,+0.0000] N=0 while Delta_C still
		// reads N=320 and NQueries still reads 320. S2 and K2 read Delta_H and
		// Delta_P; a KILL built on that has an audit trail BYTE-IDENTICAL to one
		// where both were genuinely measured at zero.
		for _, d := range []struct {
			name    string
			val     ctDelta
			readBy  string
			purpose string
		}{
			{"Delta_HP", v.DeltaHP, "K2 and K4", "what a KILL actually costs"},
			{"Delta_H", v.DeltaH, "S2 and K2", "the Hebbian mechanism's own contribution"},
			{"Delta_P", v.DeltaP, "S2 and K2", "PAS's own contribution"},
		} {
			if d.val.N != v.DeltaC.N {
				u = append(u, fmt.Sprintf("U2: vault %s computed %s over %d queries and Delta_C "+
					"over %d. %s read %s (%s); it must be the SAME paired series, and an N of 0 "+
					"is an empty or length-mismatched arm series rendering as a confident zero.",
					v.Label, d.name, d.val.N, v.DeltaC.N, d.readBy, d.name, d.purpose))
			}
		}
		// THE SECOND METRIC, bound to the same series as the first. S4 exists to
		// corroborate the S2 mechanism on MRR; a corroboration computed over a
		// different sample — or over no sample — is not one. ctMean(nil) == 0 and
		// 0 is a plausible delta, so an absent arm reads as a measured agreement
		// or a measured disagreement depending only on which way the NDCG delta
		// points. Both directions were demonstrated; see ctMeanDelta.
		for _, m := range []struct {
			name string
			val  ctMeanDelta
			arm  string
		}{
			{"MRRDelta_H", v.MRRDeltaH, "FULL vs NO-HEBBIAN"},
			{"MRRDelta_P", v.MRRDeltaP, "FULL vs NO-PAS"},
		} {
			if m.val.N != v.DeltaC.N {
				u = append(u, fmt.Sprintf("U2: vault %s computed %s over %d queries and Delta_C "+
					"over %d. MRR IS THE SECOND METRIC S4 corroborates the S2 mechanism with (%s), "+
					"and it must be the SAME paired series. An N of 0 is an ABSENT OR "+
					"LENGTH-MISMATCHED MRR series: the aggregate is a mean, ctMean of an empty "+
					"slice is 0, and a 0 delta reads as measured agreement or measured "+
					"disagreement depending only on which way NDCG happens to point.",
					v.Label, m.name, m.val.N, v.DeltaC.N, m.arm))
			}
			// A mean that is not a number is not "no objection" — the U3 delta
			// clause's reasoning, on the one aggregate U3's loop does not cover.
			// Unreachable through today's harness (ctMRR returns a finite
			// reciprocal rank and ctInformative drops the undefined queries), and
			// here for the same reason U3's is: "unreachable today" is a property
			// that has to be re-derived after every edit, and ctSameSign(x, NaN)
			// is false — which S4 reads as honest disagreement.
			if bad := ctNonFiniteScalar(m.val.Point); bad != "" {
				u = append(u, fmt.Sprintf("U2: vault %s reports %s as %s. ctSameSign against a "+
					"non-number is false and S4 reads false as MEASURED DISAGREEMENT, so an "+
					"ABSENCE OF ARITHMETIC would block SHIP under an audit trail identical to a "+
					"metric that genuinely disagreed.", v.Label, m.name, bad))
			}
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
	// U3 (delta arithmetic): A DELTA THAT IS NOT A NUMBER IS NOT "NO OBJECTION".
	//
	// Every clause in this rule is written so that FALSE means "no objection",
	// and every comparison against NaN is false. So a NaN delta sails through
	// K1, K2, K4 and U5 alike, each reading it as nothing to say, and the verdict
	// swings on it: the same vault set reporting Delta_HP +0.045 CI [0.020,0.070]
	// returns INCONCLUSIVE-BUT-POWERED, and with Delta_HP := NaN and nothing else
	// changed returns KILL.
	//
	// It is NOT reachable today — ctNDCGAt10's `ok` is a pure function of the
	// pooled grades, so NaN and !Defined coincide exactly (verified over 50 000
	// random pools x 6 arms) and ctInformative drops the NaNs before the
	// bootstrap ever sees them. This clause is here because nothing DOWNSTREAM
	// rejected one: any future edit that decouples definedness from NaN-ness
	// re-opens a silent verdict flip, and "unreachable today" is not a property
	// the rule should have to keep being re-derived.
	for _, v := range vaults {
		for _, d := range []struct {
			name string
			val  ctDelta
		}{
			{"Delta_C", v.DeltaC}, {"Delta_H", v.DeltaH},
			{"Delta_P", v.DeltaP}, {"Delta_HP", v.DeltaHP},
		} {
			if bad := ctNonFinite(d.val); bad != "" {
				u = append(u, fmt.Sprintf("U3 (delta arithmetic): vault %s reports %s with a %s. "+
					"Every clause here is written so FALSE means 'no objection' and every "+
					"comparison against NaN is false, so a non-finite delta reads as no "+
					"objection at every kill gate. It is an ABSENCE OF ARITHMETIC.",
					v.Label, d.name, bad))
			}
		}
	}

	// --- U4: the reconstruction is too partial -------------------------------
	//
	// AN UNMEASURED BASELINE IS NOT A PASSED FLOOR — the FIFTH instance of this
	// file family's signature defect, in the one U gate nobody had audited for
	// it. The clause was `if v.BaselineEdges > 0 { ...the floor... }`, so a vault
	// with an EMPTY baseline association graph, or one whose edge count simply
	// failed to be collected, skipped the replay-fraction floor entirely. And
	// ctUnreplayableFrac returned a plausible 0 for the 0/0 ratio, so BOTH halves
	// of U4 fell silent together at exactly the same moment, leaving an audit
	// trail byte-identical to a vault that genuinely replayed everything.
	//
	// Nothing upstream catches it. U2 gates DistinctEvents >= 200, which counts
	// RECALL EVENTS, not edges; a vault can plausibly have 200+ recall events and
	// a near-empty association graph, and that is precisely the vault whose
	// reconstruction deserves the least trust.
	//
	// The two absences travel together because they have ONE CAUSE (no baseline
	// edges were counted), and they are typed separately anyway: the ratio is a
	// float the rule compares against a threshold, and a float that is not a
	// number must be rejected where it is READ, not where it happened to be
	// produced. Go cannot make an unset float64 field distinguishable from a
	// measured 0.0, so the integer count is the field that carries the absence
	// and the clause below is what reads it.
	for _, v := range vaults {
		if v.BaselineEdges <= 0 {
			u = append(u, fmt.Sprintf("U4: vault %s reports %d baseline edges, so the replay "+
				"fraction ReplayedEdges/BaselineEdges (%d/%d) cannot be formed and the %.0f%% "+
				"floor was NEVER APPLIED. This is not a reconstruction that replayed everything; "+
				"it is an ABSENCE OF MEASUREMENT, and U2's event floor cannot stand in for it "+
				"because it counts recall events, not edges.",
				v.Label, v.BaselineEdges, v.ReplayedEdges, v.BaselineEdges,
				100*ctPreregistered.MinReplayEdgeFrac))
		} else {
			frac := float64(v.ReplayedEdges) / float64(v.BaselineEdges)
			if frac < ctPreregistered.MinReplayEdgeFrac {
				u = append(u, fmt.Sprintf("U4: vault %s replayed %d edges vs %d baseline (%.1f%%), "+
					"below the %.0f%% floor", v.Label, v.ReplayedEdges, v.BaselineEdges,
					100*frac, 100*ctPreregistered.MinReplayEdgeFrac))
			}
		}
		if bad := ctNonFiniteScalar(v.UnreplayableFrac); bad != "" {
			u = append(u, fmt.Sprintf("U4: vault %s reports an unreplayable fraction of %s. "+
				"Every clause here is written so FALSE means 'no objection' and every comparison "+
				"against a non-number is false, so the %.0f%% ceiling silently stops applying — "+
				"and printing it as a percentage would render an ABSENCE OF MEASUREMENT as a "+
				"measured composition.", v.Label, bad, 100*ctPreregistered.MaxUnreplayableFrac))
		} else if v.UnreplayableFrac > ctPreregistered.MaxUnreplayableFrac {
			u = append(u, fmt.Sprintf("U4: vault %s has %.1f%% of baseline edges in RelTypes replay "+
				"cannot produce (declared / autoassoc / consolidation), above the %.0f%% ceiling",
				v.Label, 100*v.UnreplayableFrac, 100*ctPreregistered.MaxUnreplayableFrac))
		}
	}

	// --- U5: the interval cannot distinguish SHIP from KILL -------------------
	//
	// The reason line is appended HERE, before U6's, because the audit trail is
	// read in order and a rule whose numbered criteria print out of numbered
	// order invites the reader to assume one of them is missing. It used to be
	// appended after the U6 loop for no reason other than where the counting
	// loop happened to sit.
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
	//
	// S2 PICKS THE MECHANISM S4 MUST THEN VINDICATE, and that coupling is
	// deliberate but was undocumented. The loop below stops at the FIRST vault
	// and the FIRST mechanism that clears the bar, checking Hebbian before PAS,
	// and S4 goes on to require MRR sign-agreement on THAT mechanism and no
	// other. So a vault whose Hebbian clears S2 while its MRR disagrees blocks
	// SHIP even if PAS also clears S2 and its MRR does agree.
	//
	// That is the CONSERVATIVE direction and it is kept: S4 exists to catch a
	// metric-specific artefact, and "the first mechanism we credited the win to
	// is not corroborated by a second metric" is exactly the artefact it is
	// looking for. Searching for ANY mechanism that clears both bars would let
	// the rule shop for the metric pair that agrees, which is the same freedom
	// the pre-registration exists to remove. Named here so a future reader meets
	// the coupling instead of discovering it.
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
		// ctBucketDelta.NQueries was CARRIED AND NEVER READ — declared by the
		// producer, guaranteed non-zero by ctBucketDeltas, and reaching no
		// reader, which is the shape ctJudgeCalibration.N shipped in. It belongs
		// in the report rather than in a gate: "the trend was fitted on 7 of 12
		// buckets" and "on 7 of 12 buckets holding 9 queries between them" are
		// materially different claims, and the second is the one that says
		// whether a populated bucket means anything. The thinnest bucket is
		// named because an average hides a bucket of one.
		//
		// `thinnest == 0 || ...` treated a bucket holding ZERO queries as an
		// uninitialised accumulator and overwrote it, so [0, 40, 40, ...] printed
		// "thinnest bucket 40" — the report claiming the healthiest possible
		// bucket exactly where the thinnest possible one sat. Ironic in the one
		// field that exists because "an average hides a bucket of one". Not
		// reachable through ctBucketDeltas, which omits empty buckets, and
		// reachable through any hand-built ctBucketDelta; the sentinel is the
		// INDEX, which cannot collide with a value.
		fitted, thinnest := 0, 0
		for i, bd := range v.DeltaCByBucket {
			fitted += bd.NQueries
			if i == 0 || bd.NQueries < thinnest {
				thinnest = bd.NQueries
			}
		}
		trendDetail = append(trendDetail, fmt.Sprintf(
			"%s:%d/%d pos over %d populated of the last %d (%d buckets omitted as empty), "+
				"slope %.5f (CI lo %.5f), fitted on %d queries across %d buckets, thinnest "+
				"bucket %d",
			v.Label, tr.PositiveOfLastN, ctPreregistered.TrendMinPositive, tr.PopulatedInWindow,
			tr.WindowN, len(v.OmittedBuckets), tr.Slope, tr.SlopeCILower, fitted,
			len(v.DeltaCByBucket), thinnest))
	}
	s3 := s3Vaults >= ctPreregistered.VaultsSupporting
	ship = append(ship, fmt.Sprintf("S3 %s (DESCRIPTIVE ONLY — gates nothing since D3; this is a "+
		"per-bucket breakdown of ONE measurement against the FINAL graph, not a checkpoint "+
		"trend): would-hold on %d/%d vaults [%s]",
		ctPassFail(s3), s3Vaults, len(vaults), strings.Join(trendDetail, "; ")))

	// S4 — MRR agrees in sign with NDCG@10 on whichever mechanism satisfied S2.
	//
	// THE TWO NUMBERS BEING COMPARED, AND THE N BEHIND THE SECOND, ARE PRINTED.
	// A sign agreement is a comparison of two quantities and the report used to
	// carry neither, so a reader could not tell a +0.03-vs-+0.03 corroboration
	// from a +0.03-vs-+0.4999 one — and +0.4999 was the value an UNMEASURED MRR
	// arm produced (ctMeanDelta). U2 now refuses that input outright; printing it
	// is the backstop, because a gate whose inputs never reach the write-up is
	// re-derivable only by rerunning the trial.
	s4 := false
	s4Detail := "no mechanism cleared S2, so there is nothing to corroborate"
	if s2 {
		for _, v := range vaults {
			if s2Which == "Hebbian" && v.DeltaH.Point >= ctPreregistered.MinDeltaMechanism && v.DeltaH.CILower > 0 {
				s4 = ctSameSign(v.DeltaH.Point, v.MRRDeltaH.Point)
				s4Detail = fmt.Sprintf("vault %s Hebbian: NDCG@10 %+.4f vs MRR %+.4f over N=%d",
					v.Label, v.DeltaH.Point, v.MRRDeltaH.Point, v.MRRDeltaH.N)
				break
			}
			if s2Which == "PAS" && v.DeltaP.Point >= ctPreregistered.MinDeltaMechanism && v.DeltaP.CILower > 0 {
				s4 = ctSameSign(v.DeltaP.Point, v.MRRDeltaP.Point)
				s4Detail = fmt.Sprintf("vault %s PAS: NDCG@10 %+.4f vs MRR %+.4f over N=%d",
					v.Label, v.DeltaP.Point, v.MRRDeltaP.Point, v.MRRDeltaP.N)
				break
			}
		}
	}
	ship = append(ship, fmt.Sprintf("S4 %s: MRR agrees in sign with NDCG@10 on the S2 mechanism (%s)",
		ctPassFail(s4), s4Detail))

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
	// arm, whose delta is Delta_HP.
	//
	// D2's GATING justification is NARROWER THAN IT WAS WRITTEN, and the honest
	// version is this: on a CLEAN-KILL vault — every marginal small and every
	// interval straddling zero — the Delta_HP clause is effectively unreachable,
	// 0 hits in 2 000 000 sampled points. Flipping K2 through Delta_HP alone
	// needs redundancy strong enough that neither boost clears +0.02 on its own
	// while removing BOTH clears it with the interval off zero, which a
	// clean-kill vault by definition does not have.
	//
	// The clause STAYS, and the REPORTING justification is untouched and is the
	// real one: Delta_HP is what a KILL costs, the other four arms provably
	// cannot reconstruct it under redundancy, and it must appear in the audit
	// trail of any verdict that spends it. Removing a clause because it has not
	// yet fired is how a gate becomes unreachable on the one vault where it
	// mattered.
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
				"effective on the read side, COG-32), with the per-vault override that already "+
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
	ctRuleEntered()
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
