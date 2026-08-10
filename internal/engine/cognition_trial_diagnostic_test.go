//go:build localassets || cognitiontrial

package engine

import (
	"math"
	"math/rand"
	"reflect"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// D5 — THE NDCG-LEVEL ADDITIVITY RELATIONS ARE A DIAGNOSTIC, NOT A GATE.
//
// D2 wired them in as a U3-class gate. This file is the evidence that they
// cannot be one, and that gating on them suppressed findings in BOTH
// directions — worst of all where a mechanism is HARMFUL, which is the finding a
// delete-or-keep trial most needs.
//
// PRIVACY: arithmetic and verdict strings only. Every series here is generated
// in this file. No vault, no query text, no identifier.
// ---------------------------------------------------------------------------

// ctArmMeans is a synthetic world: one mean NDCG@10 per arm. Every scenario
// below is stated as five numbers, so what each one claims about the mechanisms
// is readable without running it.
type ctArmMeans struct {
	content  float64 // CONTENT-MATCH-ONLY
	baseOnly float64 // BASE-LEVEL-ONLY   — the configuration a KILL ships
	noHeb    float64 // NO-HEBBIAN
	noPAS    float64 // NO-PAS
	full     float64 // FULL
}

// ctSeriesFromMeans builds n informative queries realizing those arm means.
//
// The per-query common component is large (queries differ in difficulty) and the
// per-arm perturbation is small, which is what a PAIRED design is for: the
// difference series is tight even though the levels are not. Deterministic — a
// property test that resamples differently per run is a property test that
// flakes.
func ctSeriesFromMeans(n int, m ctArmMeans) *ctQuerySeries {
	s := ctNewQuerySeries(ctArmNames)
	byArm := map[string]float64{
		ctArmNameContentOnly:   m.content,
		ctArmNameBaseLevelOnly: m.baseOnly,
		ctArmNameNoHebbian:     m.noHeb,
		ctArmNameNoPAS:         m.noPAS,
		ctArmNameFull:          m.full,
	}
	for i := 0; i < n; i++ {
		common := float64((i*37)%100)/100.0*0.30 - 0.15
		k := 0
		for _, arm := range ctArmNames {
			k++
			// A small, mean-zero, arm-specific wobble so no delta is a constant
			// (a constant paired difference makes every bootstrap resample
			// identical and would hide a seed that was never wired in).
			jitter := (float64((i*(7+3*k))%11)/11.0 - 0.5) * 0.02
			v := math.Min(math.Max(byArm[arm]+common+jitter, 0), 1)
			s.NDCG[arm] = append(s.NDCG[arm], v)
			s.MRR[arm] = append(s.MRR[arm], v)
		}
		s.Defined = append(s.Defined, true)
		s.Bucket = append(s.Bucket, i%12)
	}
	return s
}

// ctThreeFromSeries builds a three-vault result set from ONE series, through the
// production builder, with the U4 reconstruction numbers the series does not
// carry. Three identical vaults is the cleanest way to hold everything except
// the quantity under test fixed.
//
// THE BUILDER RUNS ONCE AND THE RESULT IS RELABELLED, which is exact rather than
// approximate: ctVaultFromSeriesResamples is a pure function of (series,
// distinctEvents, nBuckets, seed, resamples) plus the label, which it only
// copies into ctVaultResult.Label. Same inputs, same seed, same output — running
// it three times produced three bit-identical structs at three times the cost,
// and the cost is the whole reason this test family was 36x more expensive under
// -race than its non-race timing suggested.
//
// TestCognitionTrialRule_ThreeFromSeriesRelabelIsExact pins the equivalence
// against a per-label rebuild, so if the builder ever acquires a label-dependent
// term this stops being sound LOUDLY.
//
// The slices ctVaultResult carries (DeltaCByBucket, OmittedBuckets) are shared
// between the three copies. That is safe here and is asserted rather than
// assumed: no caller of this helper mutates them, and the pinning test compares
// contents, not identity.
func ctThreeFromSeries(s *ctQuerySeries, resamples int) []ctVaultResult {
	v := ctVaultFromSeriesResamples("A", s, 410, 12, 7, resamples)
	v.BaselineEdges = 5000
	v.ReplayedEdges = 2100
	v.UnreplayableFrac = 0.30
	// Same reasoning as ctShipVault/ctVaultFromSynth: this builder's callers
	// use it for SHIP-shaped fixtures, so its S6 null is survived unless a
	// test overrides it.
	v.ShuffledSeedNull = ctNullSurvived
	out := make([]ctVaultResult, 0, 3)
	for _, label := range []string{"A", "B", "C"} {
		c := v
		c.Label = label
		out = append(out, c)
	}
	return out
}

// ctNullSeries builds n queries under an EXCHANGEABLE NULL: every arm's score is
// the query's own difficulty plus independent noise from the same distribution,
// so every pairwise delta has expectation exactly 0 and NO arm is systematically
// better than another. This is the world in which an instrument check must stay
// quiet, and in which a POINT comparison between four exchangeable estimates
// cannot.
//
// The per-arm noise SD is chosen so the PAIRED difference has sigma_d ~= 0.15,
// the dispersion the design pre-registers its sample size at.
func ctNullSeries(n int, rng *rand.Rand) *ctQuerySeries {
	const pairedSD = 0.15
	perArmSD := pairedSD / math.Sqrt2
	s := ctNewQuerySeries(ctArmNames)
	for i := 0; i < n; i++ {
		difficulty := 0.2 + 0.6*rng.Float64()
		for _, arm := range ctArmNames {
			v := math.Min(math.Max(difficulty+rng.NormFloat64()*perArmSD, 0), 1)
			s.NDCG[arm] = append(s.NDCG[arm], v)
			s.MRR[arm] = append(s.MRR[arm], v)
		}
		s.Defined = append(s.Defined, true)
		s.Bucket = append(s.Bucket, i%12)
	}
	return s
}

// ctThreeFromSeries and ctVaultFromSynth build one vault and relabel it twice
// instead of running the builder three times. That is only sound if the builder
// is a pure function of its inputs whose ONLY use of the label is to copy it,
// so the property is pinned here rather than argued in a comment — the same
// standard the rest of this branch is held to.
//
// Deliberately at a small n and a small resample count: this asserts an
// EQUIVALENCE between two call patterns, which is scale-free, and paying the
// pre-registered 10 000 to assert it would reintroduce the cost the relabelling
// exists to remove.
func TestCognitionTrialRule_ThreeFromSeriesRelabelIsExact(t *testing.T) {
	const (
		n         = 40
		resamples = 50
	)
	s := ctSynthSeries(n)
	got := ctThreeFromSeries(s, resamples)
	if len(got) != 3 {
		t.Fatalf("%d vaults, want 3", len(got))
	}
	for i, label := range []string{"A", "B", "C"} {
		want := ctVaultFromSeriesResamples(label, s, 410, 12, 7, resamples)
		want.BaselineEdges = 5000
		want.ReplayedEdges = 2100
		want.UnreplayableFrac = 0.30
		want.ShuffledSeedNull = ctNullSurvived
		if !reflect.DeepEqual(got[i], want) {
			t.Errorf("vault %s from the relabelled build differs from a per-label rebuild.\n"+
				" got: %+v\nwant: %+v\nThe builder has acquired a label-dependent term, so "+
				"building once and relabelling is no longer the same measurement.",
				label, got[i], want)
		}
	}
}

// ---------------------------------------------------------------------------
// FINDING 1: THE POINT COMPARISON DOES NOT CONVERGE IN n, SO IT CANNOT GATE.
//
// Under the null, Delta_H, Delta_P and Delta_HP are exchangeable, so
// P(Delta_HP is the largest of the three) = 1/3 and the lower relation is broken
// 2/3 of the time BY CONSTRUCTION. Adding data does not help: the comparison is
// between three estimates that all converge to the SAME value, so their ORDER
// stays a coin toss however tight each one gets. That is the difference between
// a statistic and an instrument check, and it is why widening the tolerance
// cannot rescue it either — there is no tolerance at which a coin toss becomes
// evidence.
//
// The test asserts the two things that matter:
//
//  1. the break rate stays high AT EVERY n, i.e. it is flat rather than
//     converging — measured here, not assumed; and
//  2. NOT ONE of those breaks routes a verdict to UNDERPOWERED.
//
// (2) is the pre-D5 behaviour, and it was catastrophic: on an otherwise clean
// vault set — judge calibrated, n over the bar, intervals tight, buckets
// populated — the additivity clause was the ONLY reachable U condition, so the
// UNDERPOWERED rate below IS the break rate.
// ---------------------------------------------------------------------------

func TestCognitionTrialRule_AdditivityDoesNotConvergeAndIsNotAGate(t *testing.T) {
	// A reduced resample count, and the justification is NARROWER than it was
	// written. The additivity relations read ctDelta.Point, which is the exact
	// arithmetic mean and is bit-identical at any resample count — that part
	// holds. But this test ALSO asserts Verdict != UNDERPOWERED, and U5 reads
	// DeltaC's half-width, so an interval drawn from 100 resamples IS load-bearing
	// here. The old blanket claim ("nothing here asserts on an interval") was
	// false.
	//
	// So it is checked instead of argued, twice over:
	//
	//  1. EXECUTED, once, by hand. Re-running this whole sweep at
	//     ctPreregistered.BootstrapResamples gives identical break counts and
	//     identical UNDERPOWERED counts — 93/100, 46/50, 20/20, zero UNDERPOWERED
	//     at every n — at 48.07s instead of 10.80s.
	//  2. RE-CHECKED ON EVERY RUN, cheaply, on the trial where it is TIGHTEST:
	//     the loop records the seed of the WIDEST Delta_C interval it produced,
	//     and afterwards rebuilds exactly that vault set at the pre-registered
	//     count and requires the same verdict. One extra build, at the one seed
	//     where a 100-draw interval has the least room.
	//
	// A MARGIN HEURISTIC WAS TRIED FIRST AND REJECTED BY MEASUREMENT. The obvious
	// guard is "require the widest half-width to stay well inside U5's gate", and
	// the guess written here was that these intervals sit an order of magnitude
	// inside it. They do not: the widest is 0.0206 against a 0.03 gate, a margin
	// of 1.46x. Under this null sigma_d is ~0.15 by construction and n is as low
	// as 320, so 1.96*0.15/sqrt(320) ~= 0.0164 is simply where the interval lands.
	// Any "comfortable margin" constant would have had to be picked AFTER seeing
	// that number, which is the tuning this file exists to refuse. Checking the
	// verdict directly needs no constant at all.
	const resamples = 100
	widest, widestSeed, widestN := 0.0, int64(0), 0

	type row struct {
		n, trials, broke, underpowered int
	}
	var rows []row
	// THE TRIAL COUNTS ARE HALVED FROM {200, 100, 40}, AND HERE IS WHY THAT DOES
	// NOT WEAKEN WHAT THIS TEST ASSERTS.
	//
	// CI runs the default job with -race, where this test cost 58.36s against a
	// 300s per-package timeout — and it is deterministic single-goroutine
	// arithmetic, so the race detector is paying ~10x for a schedule it cannot
	// find anything in. The whole package sat at 256s of its 300s budget.
	//
	// The assertion below is that the break rate at the largest n stays ABOVE
	// 0.50, against a measured rate of ~0.91. At 20 trials a binomial 95%
	// interval around p = 0.91 is roughly [0.70, 0.99] — nowhere near the bar.
	// And the seeds are a pure function of (n, trial), so the counts here are
	// FIXED, not sampled: this test cannot flake in either direction. Halving the
	// trials halves the resolution of a number that has ~40 points of margin.
	//
	// ctThreeFromSeries additionally stopped rebuilding the identical vault three
	// times, which is where the rest of the reduction comes from and costs no
	// resolution at all.
	for _, tc := range []struct{ n, trials int }{
		{320, 100}, {1000, 50}, {4000, 20},
	} {
		r := row{n: tc.n, trials: tc.trials}
		for trial := 0; trial < tc.trials; trial++ {
			seed := int64(1000*tc.n + trial)
			rng := rand.New(rand.NewSource(seed))
			vaults := ctThreeFromSeries(ctNullSeries(tc.n, rng), resamples)
			if hw := vaults[0].DeltaC.halfWidth(); hw > widest {
				widest, widestSeed, widestN = hw, seed, tc.n
			}
			if len(ctAdditivityDiagnostic(vaults[0])) > 0 {
				r.broke++
			}
			got := ctDecide(vaults, ctGoodJudge(), true)
			if got.Verdict == ctVerdictUnderpowered {
				r.underpowered++
				if trial == 0 {
					t.Logf("first UNDERPOWERED at n=%d:\n%s", tc.n, got)
				}
			}
		}
		rows = append(rows, r)
		t.Logf("n=%-5d trials=%-4d additivity relations broken %3d (%.1f%%)  ->  UNDERPOWERED %d",
			r.n, r.trials, r.broke, 100*float64(r.broke)/float64(r.trials), r.underpowered)
	}

	// 0. THE REDUCED RESAMPLE COUNT IS STILL SOUND, checked where it is tightest.
	//    This test asserts a verdict, U5 reads Delta_C's half-width, and a
	//    100-draw interval is therefore load-bearing. The trial with the WIDEST
	//    interval is the one with the least room before U5 fires, so that is the
	//    one rebuilt at the pre-registered count and re-decided.
	t.Logf("widest Delta_C 95%% CI half-width over every trial above: %.5f at n=%d (U5 gates at "+
		"%.2f — a margin of %.2fx, which is why this is checked by rebuilding rather than by a "+
		"margin constant)", widest, widestN, ctPreregistered.MaxCIHalfWidth,
		ctPreregistered.MaxCIHalfWidth/widest)
	{
		s := ctNullSeries(widestN, rand.New(rand.NewSource(widestSeed)))
		reduced := ctDecide(ctThreeFromSeries(s, resamples), ctGoodJudge(), true)
		full := ctDecide(ctThreeFromSeries(s, ctPreregistered.BootstrapResamples),
			ctGoodJudge(), true)
		t.Logf("tightest-margin trial (n=%d, seed %d): %d resamples -> %s; %d resamples -> %s",
			widestN, widestSeed, resamples, reduced.Verdict,
			ctPreregistered.BootstrapResamples, full.Verdict)
		if reduced.Verdict != full.Verdict {
			t.Errorf("on the trial with the widest interval, %d resamples gave %s and the "+
				"pre-registered %d gave %s. The reduced count is deciding this sweep's verdicts, "+
				"so it can no longer stand in for the pre-registered one here.\nreduced:\n%s\n"+
				"full:\n%s", resamples, reduced.Verdict, ctPreregistered.BootstrapResamples,
				full.Verdict, reduced, full)
		}
	}

	// 1. FLAT IN n. A converging instrument check would break less often as the
	//    estimates tighten; this one does not, because what it compares is an
	//    ORDER between three quantities with the same limit.
	first, last := rows[0], rows[len(rows)-1]
	rate := func(r row) float64 { return float64(r.broke) / float64(r.trials) }
	if rate(last) < 0.50 {
		t.Errorf("the break rate at n=%d is %.1f%%. This test's premise is that the point "+
			"comparison does NOT converge — if it now does, the demotion in D5 should be "+
			"re-argued from the new evidence rather than inherited", last.n, 100*rate(last))
	}
	if rate(last) < rate(first)-0.20 {
		t.Errorf("the break rate fell from %.1f%% at n=%d to %.1f%% at n=%d — that is "+
			"convergence, and this test is no longer measuring the thing it exists for",
			100*rate(first), first.n, 100*rate(last), last.n)
	}

	// 2. AND IT GATES NOTHING. Every vault set above is clean on U1/U2/U4/U5/U6
	//    by construction, so before D5 the UNDERPOWERED count WAS the break count.
	for _, r := range rows {
		if r.underpowered != 0 {
			t.Errorf("n=%d: %d of %d trials returned UNDERPOWERED on an otherwise-clean vault "+
				"set whose ONLY objection can be the additivity relation. Broken relations were "+
				"%d. A gate that fires ~%.0f%% of the time under a null where nothing is wrong "+
				"is not measuring the instrument — and it cannot be fixed with more data.",
				r.n, r.underpowered, r.trials, r.broke, 100*rate(r))
		}
	}
}

// ---------------------------------------------------------------------------
// FINDING 2: EVERY BREAK DECODES TO A REAL WORLD, AND THE GATE SUPPRESSED BOTH
// DIRECTIONS.
//
// The three differences are identities, not approximations:
//
//	Delta_HP - Delta_P == NDCG(NO-PAS)             - NDCG(BASE-LEVEL-ONLY)
//	Delta_HP - Delta_H == NDCG(NO-HEBBIAN)         - NDCG(BASE-LEVEL-ONLY)
//	Delta_HP - Delta_C == NDCG(CONTENT-MATCH-ONLY) - NDCG(BASE-LEVEL-ONLY)
//
// so a break is exactly "this mechanism, measured on top of the base-level
// prior, is net-harmful on this vault". The four worlds below are the ones the
// gate silently converted into "we could not measure", and the claim it carried
// — that it errs toward suppressing KILLs — is refuted by the second row.
// ---------------------------------------------------------------------------

func TestCognitionTrialRule_AdditivityBreaksDecodeToAHarmfulMechanism(t *testing.T) {
	for _, tc := range []struct {
		name     string
		means    ctArmMeans
		want     ctVerdict
		wantDiag []string
	}{
		{
			// Delta_C +0.04 with the boosts carrying +0.065 of it: the prior is
			// LOSING 0.025 and the layer is still worth keeping. SHIP is right,
			// and it is the row that refutes "it only suppresses kills".
			name:  "the base-level prior is harmful and the boosts still earn their keep",
			means: ctArmMeans{content: 0.560, baseOnly: 0.535, noHeb: 0.575, noPAS: 0.580, full: 0.600},
			want:  ctVerdictShip,
			wantDiag: []string{
				"THE ACT-R BASE-LEVEL PRIOR IS NET-HARMFUL",
			},
		},
		{
			// Every arm beats FULL. The layer actively hurts; KILL is right, and
			// this is the direction the old comment claimed could not be
			// suppressed.
			name:  "EVERYTHING is harmful — the layer actively hurts",
			means: ctArmMeans{content: 0.600, baseOnly: 0.580, noHeb: 0.550, noPAS: 0.560, full: 0.500},
			want:  ctVerdictKill,
			wantDiag: []string{
				"THE HEBBIAN BOOST IS NET-HARMFUL on top of the base-level prior",
				"PAS IS NET-HARMFUL on top of the base-level prior",
				"THE ACT-R BASE-LEVEL PRIOR IS NET-HARMFUL",
			},
		},
		{
			// Hebbian carries a real +0.03; PAS on top of the base-level prior
			// costs 0.01. A mixed result, and the most useful kind.
			name:  "PAS on top of base-level is harmful and Hebbian carries the win",
			means: ctArmMeans{content: 0.550, baseOnly: 0.580, noHeb: 0.570, noPAS: 0.610, full: 0.600},
			want:  ctVerdictShip,
			wantDiag: []string{
				"PAS IS NET-HARMFUL on top of the base-level prior",
			},
		},
		{
			// The control: every mechanism helps, no relation is broken, no
			// diagnostic is emitted. Without this row the test would pass on a
			// diagnostic that fires unconditionally.
			name:     "CONTROL: everything helps and the relations hold",
			means:    ctArmMeans{content: 0.550, baseOnly: 0.570, noHeb: 0.580, noPAS: 0.585, full: 0.600},
			want:     ctVerdictShip,
			wantDiag: nil,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			vaults := ctThreeFromSeries(ctSeriesFromMeans(320, tc.means), 2000)
			v := vaults[0]
			t.Logf("Delta_C %+.4f  Delta_H %+.4f  Delta_P %+.4f  Delta_HP %+.4f  (n=%d)",
				v.DeltaC.Point, v.DeltaH.Point, v.DeltaP.Point, v.DeltaHP.Point, v.DeltaC.N)

			diag := ctAdditivityDiagnostic(v)
			if len(diag) != len(tc.wantDiag) {
				t.Fatalf("%d diagnostics, want %d:\n%s", len(diag), len(tc.wantDiag),
					strings.Join(diag, "\n"))
			}
			for _, want := range tc.wantDiag {
				found := false
				for _, d := range diag {
					if strings.Contains(d, want) {
						found = true
					}
				}
				if !found {
					t.Errorf("no diagnostic names %q. A broken relation that is not DECODED is "+
						"just a number nobody can act on:\n%s", want, strings.Join(diag, "\n"))
				}
			}

			got := ctDecide(vaults, ctGoodJudge(), true)
			if got.Verdict == ctVerdictUnderpowered {
				t.Fatalf("this world returned UNDERPOWERED. It is a MEASURED world with a named "+
					"harmful mechanism, and the correct verdict is %s. Converting it into 'we "+
					"could not measure' destroys the most decision-relevant finding a "+
					"delete-or-keep trial can produce\n%s", tc.want, got)
			}
			ctRequireVerdict(t, got, tc.want, "")
			// The finding must reach the reader, and it must be marked as gating
			// nothing so it is not mistaken for an instrument failure.
			for _, want := range tc.wantDiag {
				ctExpectReason(t, got, want, "the decoded finding is not in the audit trail")
				ctExpectReason(t, got, "DIAGNOSTIC (a finding about the mechanisms; gates nothing",
					"the finding is not marked as a diagnostic, so a reader cannot tell it from a "+
						"U-gate")
			}
			if strings.Contains(strings.Join(got.Reasons, "\n"), "U3 (ablation additivity)") {
				t.Errorf("the additivity relation is raising a U3 objection again\n%s", got)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// FINDING 4: ABSENCE-TYPING MUST COVER ALL FOUR DELTAS, NOT TWO.
//
// ctPairedBootstrap returns {Point:0, CI:[0,0], N:0} on an empty OR
// length-mismatched series — a CONFIDENT ZERO. D1 typed that for Delta_C and
// Delta_HP; Delta_H and Delta_P were left untyped, which is D1's exact defect
// relocated one field over. One arm's series one element short is all it takes,
// and it is reachable through the production builder rather than only through a
// hand-built struct.
// ---------------------------------------------------------------------------

func TestCognitionTrialRule_EveryDeltaNMustBindToTheSameSeries(t *testing.T) {
	const n = 320
	for _, tc := range []struct {
		arm     string
		delta   string
		wantSub string
	}{
		{ctArmNameNoHebbian, "Delta_H", "computed Delta_H over 0 queries"},
		{ctArmNameNoPAS, "Delta_P", "computed Delta_P over 0 queries"},
		{ctArmNameBaseLevelOnly, "Delta_HP", "computed Delta_HP over 0 queries"},
		{ctArmNameContentOnly, "Delta_C", "NO INFORMATIVE QUERY"},
	} {
		t.Run(tc.arm+" truncated by one", func(t *testing.T) {
			intact := ctSynthSeries(n)
			short := ctSynthSeries(n)
			short.NDCG[tc.arm] = short.NDCG[tc.arm][:n-1]

			// `before` exists ONLY for the log line below, which prints .Point and
			// .N. ctDelta.Point is the exact arithmetic mean of the paired
			// differences and is never resampled, so it is bit-identical at any
			// resample count, and .N is the series length. NOTHING reads an
			// interval off `before` — it never reaches ctDecide. So it is built at
			// a reduced count and `after`, which DOES reach ctDecide and whose
			// interval U5 reads, is built at the pre-registered one.
			//
			// That asymmetry is the whole rule for reduced counts, applied per
			// CALL SITE rather than asserted once about the helper. See
			// ctVaultFromSeriesResamples' comment for why the blanket form of this
			// claim was wrong.
			before := ctVaultFromSeriesResamples("A", intact, 410, 12, 7, 100)
			after := ctVaultFromSeries("A", short, 410, 12, 7)
			t.Logf("intact:         %s = %+.4f (N=%d)", tc.delta,
				ctDeltaByName(before, tc.delta).Point, ctDeltaByName(before, tc.delta).N)
			t.Logf("truncated-by-1: %s = %+.4f CI[%+.4f,%+.4f] N=%d   Delta_C N=%d  NQueries=%d",
				tc.delta, ctDeltaByName(after, tc.delta).Point,
				ctDeltaByName(after, tc.delta).CILower, ctDeltaByName(after, tc.delta).CIUpper,
				ctDeltaByName(after, tc.delta).N, after.DeltaC.N, after.NQueries)

			if ctDeltaByName(after, tc.delta).N != 0 {
				t.Fatalf("truncating one arm did not empty %s's paired series — this test's "+
					"premise (that a length mismatch renders as a confident zero) has changed",
					tc.delta)
			}

			vaults := ctThree(ctShipVault)
			vaults[1] = after
			vaults[1].Label = "B"
			vaults[1].BaselineEdges, vaults[1].ReplayedEdges = 5000, 2100
			vaults[1].UnreplayableFrac = 0.30
			ctRequireVerdict(t, ctDecide(vaults, ctGoodJudge(), true),
				ctVerdictUnderpowered, tc.wantSub)
		})
	}
}

// ---------------------------------------------------------------------------
// FINDING 5: AN UNMEASURED MRR ARM MANUFACTURES A SHIP.
//
// The SIXTH instance of the signature defect and the FIRST that is a MEAN
// rather than a ratio — every prior sweep went looking for zero denominators.
// ctMean returns 0 for an empty slice and the aggregate was
// `ctMean(inf(A)) - ctMean(inf(B))`, so an MRR arm that was never collected
// produced a plausible measured number, and S4 — whose entire job is
// corroborating the S2 mechanism ON A SECOND METRIC — decided against a metric
// that may never have existed.
//
// Both directions flip, which is why neither a "conservative direction"
// argument nor a bindingness probe could have found it:
//
//   - ABSENT NO-HEBBIAN arm: mean(FULL) - mean(nil) = mean(FULL), a large
//     POSITIVE number that agrees in sign with any positive NDCG delta.
//     INCONCLUSIVE-BUT-POWERED -> SHIP.
//   - ABSENT BOTH arms, the Go zero: S4 FAIL, i.e. an audit trail
//     indistinguishable from genuine measured disagreement. SHIP ->
//     INCONCLUSIVE.
//
// And the truncation case slipped through where the NDCG control is caught:
// ctInformative breaks at `i >= len(xs)`, so a half-length MRR arm is silently
// truncated while a half-length NDCG arm trips U2's N-mismatch clause.
//
// The fixture inverts MRR against NDCG (MRR := 1 - NDCG) so the HONEST verdict
// is a genuine S4 failure. Every mutation below therefore starts from a world
// where the second metric really does disagree, and any movement toward SHIP is
// the artifact and nothing else.
// ---------------------------------------------------------------------------

func ctMRRDisagreeingSeries(n int) *ctQuerySeries {
	s := ctSynthSeries(n)
	for arm := range s.NDCG {
		for i := range s.NDCG[arm] {
			s.MRR[arm][i] = 1 - s.NDCG[arm][i]
		}
	}
	return s
}

func TestCognitionTrialRule_UnmeasuredMRRIsNotAMeasuredZero(t *testing.T) {
	const n = 320
	decide := func(mutate func(s *ctQuerySeries)) (ctDecision, ctVaultResult) {
		s := ctMRRDisagreeingSeries(n)
		if mutate != nil {
			mutate(s)
		}
		vaults := ctThreeFromSeries(s, ctPreregistered.BootstrapResamples)
		return ctDecide(vaults, ctGoodJudge(), true), vaults[0]
	}

	// The honest world: NDCG says the Hebbian arm helps, MRR says it hurts, and
	// the pre-registered answer is that SHIP is blocked.
	honest, hv := decide(nil)
	t.Logf("HONEST                    : Delta_H=%+.4f  MRRDeltaH=%+.4f (N=%d)  -> %s",
		hv.DeltaH.Point, hv.MRRDeltaH.Point, hv.MRRDeltaH.N, honest.Verdict)
	if honest.Verdict != ctVerdictInconclusivePowered {
		t.Fatalf("the honest fixture is %s, not INCONCLUSIVE-BUT-POWERED — every mutation "+
			"below is measured against it, so this test would prove nothing\n%s",
			honest.Verdict, honest)
	}

	for _, tc := range []struct {
		name    string
		wantSub string
		mutate  func(s *ctQuerySeries)
	}{
		{
			name:    "NO-HEBBIAN MRR arm never collected",
			wantSub: "computed MRRDelta_H over 0 queries",
			mutate:  func(s *ctQuerySeries) { s.MRR[ctArmNameNoHebbian] = nil },
		},
		{
			name:    "NO-PAS MRR arm never collected",
			wantSub: "computed MRRDelta_P over 0 queries",
			mutate:  func(s *ctQuerySeries) { s.MRR[ctArmNameNoPAS] = nil },
		},
		{
			name:    "FULL MRR arm never collected",
			wantSub: "computed MRRDelta_H over 0 queries",
			mutate:  func(s *ctQuerySeries) { s.MRR[ctArmNameFull] = nil },
		},
		{
			// The one the NDCG control catches and MRR did not: half the series.
			name:    "NO-HEBBIAN MRR arm truncated to half",
			wantSub: "computed MRRDelta_H over 0 queries",
			mutate: func(s *ctQuerySeries) {
				s.MRR[ctArmNameNoHebbian] = s.MRR[ctArmNameNoHebbian][:n/2]
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, v := decide(tc.mutate)
			t.Logf("%-40s: Delta_H=%+.4f  MRRDeltaH=%+.4f (N=%d)  MRRDeltaP=%+.4f (N=%d)  -> %s",
				tc.name, v.DeltaH.Point, v.MRRDeltaH.Point, v.MRRDeltaH.N,
				v.MRRDeltaP.Point, v.MRRDeltaP.N, got.Verdict)
			if got.Verdict == ctVerdictShip {
				t.Fatalf("SHIPPED on an MRR arm that was never collected. S4's whole job is to "+
					"corroborate the S2 mechanism on a SECOND metric, and the second metric does "+
					"not exist here\n%s", got)
			}
			ctRequireVerdict(t, got, ctVerdictUnderpowered, tc.wantSub)
		})
	}

	// THE MIRROR, on the hand-built fixture: both MRR aggregates at their ZERO
	// VALUE — "never collected" — must not read as "measured, and it disagrees".
	// Pre-fix this was a plain float64 0.0 and produced `S4 FAIL: MRR agrees in
	// sign with NDCG@10`, an audit trail byte-identical to a real disagreement.
	t.Run("both MRR aggregates at the Go zero", func(t *testing.T) {
		vaults := ctThree(ctShipVault)
		if base := ctDecide(vaults, ctGoodJudge(), true); base.Verdict != ctVerdictShip {
			t.Fatalf("the SHIP fixture is %s, so the mirror proves nothing\n%s", base.Verdict, base)
		}
		for i := range vaults {
			vaults[i].MRRDeltaH = ctMeanDelta{}
			vaults[i].MRRDeltaP = ctMeanDelta{}
		}
		got := ctDecide(vaults, ctGoodJudge(), true)
		if got.Verdict == ctVerdictInconclusivePowered {
			t.Fatalf("an MRR series that was NEVER COLLECTED rendered as a measured "+
				"disagreement and blocked SHIP through S4. 'We did not look' and 'we looked "+
				"and the metrics disagree' are different findings and this audit trail cannot "+
				"tell them apart\n%s", got)
		}
		ctRequireVerdict(t, got, ctVerdictUnderpowered, "ABSENT OR LENGTH-MISMATCHED MRR series")
	})
}

func ctDeltaByName(v ctVaultResult, name string) ctDelta {
	switch name {
	case "Delta_H":
		return v.DeltaH
	case "Delta_P":
		return v.DeltaP
	case "Delta_HP":
		return v.DeltaHP
	default:
		return v.DeltaC
	}
}

// ---------------------------------------------------------------------------
// FINDING 3: A NaN READS AS "NO OBJECTION" AT EVERY KILL GATE.
//
// Every clause is written so FALSE means "no objection", and every comparison
// against NaN is false. So a NaN sails through K1, K2, K4 and U5 alike and the
// verdict swings on it. NOT reachable today — ctNDCGAt10's ok is a pure function
// of the pooled grades, so NaN and !Defined coincide exactly and ctInformative
// drops the NaNs before the bootstrap sees them — but nothing DOWNSTREAM
// rejected one, so any future edit that decouples them re-opens it silently.
// ---------------------------------------------------------------------------

// ctNaNDemoVaults is a vault set that would KILL but for ONE vault whose
// Delta_HP — what a KILL costs — is +0.045 with the interval clear of zero. That
// single number is the whole reason the verdict is INCONCLUSIVE-BUT-POWERED, so
// it is exactly the place to show what a NaN does.
func ctNaNDemoVaults() []ctVaultResult {
	v := ctThree(ctKillVault)
	v[1].DeltaC = ctDelta{Point: 0.050, CILower: 0.025, CIUpper: 0.075, N: 340}
	v[1].DeltaHP = ctDelta{Point: 0.045, CILower: 0.020, CIUpper: 0.070, N: 340}
	return v
}

func TestCognitionTrialRule_NaNDeltaIsNotNoObjection(t *testing.T) {
	// The demonstration first, and it is a VERDICT FLIP, not a cosmetic one.
	ctRequireVerdict(t, ctDecide(ctNaNDemoVaults(), ctGoodJudge(), true),
		ctVerdictInconclusivePowered, "K2 FAIL")

	for _, tc := range []struct {
		name  string
		field func(*ctVaultResult, float64)
		// flipsToKill records the cases where the NaN does not merely go
		// unnoticed but actively hands the verdict to KILL, because the clause
		// that was blocking it reads FALSE against a NaN.
		flipsToKill bool
	}{
		{"Delta_HP point", func(v *ctVaultResult, x float64) { v.DeltaHP.Point = x }, true},
		{"Delta_HP CI lower", func(v *ctVaultResult, x float64) { v.DeltaHP.CILower = x }, true},
		{"Delta_C point", func(v *ctVaultResult, x float64) { v.DeltaC.Point = x }, false},
		{"Delta_C CI upper", func(v *ctVaultResult, x float64) { v.DeltaC.CIUpper = x }, false},
		{"Delta_H point", func(v *ctVaultResult, x float64) { v.DeltaH.Point = x }, false},
		{"Delta_P point", func(v *ctVaultResult, x float64) { v.DeltaP.Point = x }, false},
	} {
		for _, poison := range []struct {
			what string
			val  float64
		}{{"NaN", math.NaN()}, {"+Inf", math.Inf(1)}} {
			t.Run(tc.name+" = "+poison.what, func(t *testing.T) {
				vaults := ctNaNDemoVaults()
				tc.field(&vaults[1], poison.val)
				got := ctDecide(vaults, ctGoodJudge(), true)
				if got.Verdict == ctVerdictKill {
					t.Fatalf("a %s in %s produced a KILL on the vault set that returns "+
						"INCONCLUSIVE-BUT-POWERED with a number there. The clause that was "+
						"blocking the kill reads FALSE against %s, and FALSE means 'no "+
						"objection' everywhere in this rule\n%s",
						poison.what, tc.name, poison.what, got)
				}
				ctRequireVerdict(t, got, ctVerdictUnderpowered, "U3 (delta arithmetic)")
				if tc.flipsToKill && poison.what == "NaN" {
					t.Logf("without the U3 delta-arithmetic clause this case is a KILL: the only "+
						"thing standing between this vault set and retiring the cognitive layer "+
						"is %s, and a NaN there is silently no objection", tc.name)
				}
			})
		}
	}
}

// The one data-reachable leak: ctZeroFilled copied xs[i] verbatim for any index
// past the end of Defined, and ctNDCGAt10 writes NaN there. It gates nothing —
// it PRINTS, which is its own kind of wrong.
func TestCognitionTrialRule_ZeroFilledNeverEmitsANaN(t *testing.T) {
	xs := []float64{0.4, math.NaN(), 0.6, math.NaN()}
	for _, defined := range [][]bool{
		{true, false, true, false}, // the intended shape
		{true, false, true},        // one short: index 3 has no flag at all
		nil,                        // no flags at all
		{true, true, true, true},   // a NaN under a true flag: a contradiction
	} {
		got := ctZeroFilled(defined, xs)
		if len(got) != len(xs) {
			t.Fatalf("defined=%v: length %d, want %d", defined, len(got), len(xs))
		}
		for i, v := range got {
			if math.IsNaN(v) {
				t.Errorf("defined=%v: a NaN survived at index %d and would be bootstrapped into "+
					"DeltaCAllQueriesDiluted and printed in the report", defined, i)
			}
		}
	}
	// And the intended case is untouched: a defined entry keeps its value.
	got := ctZeroFilled([]bool{true, false, true, false}, xs)
	if got[0] != 0.4 || got[2] != 0.6 || got[1] != 0 || got[3] != 0 {
		t.Errorf("ctZeroFilled = %v, want [0.4 0 0.6 0] — the diluted series must still write "+
			"undefined entries as the zeros it is named for", got)
	}
}

// ---------------------------------------------------------------------------
// FINDING 8: THE EXCLUSION INHERITS A DEGREE OF FREEDOM FROM THE ARM SET.
//
// The exclusion is arm-NEUTRAL — ctNDCGAt10's ok is a pure function of the
// POOLED grades, so it is identical across arms for a given query. But the POOL
// is the UNION of the arms' top-PoolDepth, so WHICH queries are informative
// depends on the arm SET and the DEPTH. This is that dependence, executed: same
// query, same ranking, one extra arm in the pool.
// ---------------------------------------------------------------------------

func TestCognitionTrialMetrics_ThePoolDependsOnTheArmSet(t *testing.T) {
	ranked := []string{"m1", "m2"}

	// Four arms between them surfaced only ungraded-or-zero documents.
	fourArmPool := map[string]int{"m1": 0, "m2": 0, "m3": 0}
	if v, ok := ctNDCGAt10(ranked, fourArmPool); ok {
		t.Fatalf("a pool whose every grade is 0 reported a defined NDCG of %v", v)
	}

	// A fifth arm surfaces ONE graded document. The query under test returned
	// exactly the same two items in exactly the same order — and it has just
	// moved from zero-relevance (excluded, counted, invisible to S1 and K1) to
	// informative (included, and contributing a 0.0 to the mean of every arm).
	fiveArmPool := map[string]int{"m1": 0, "m2": 0, "m3": 0, "m4": 2}
	v, ok := ctNDCGAt10(ranked, fiveArmPool)
	if !ok {
		t.Fatalf("the five-arm pool is still undefined — this demonstration has changed shape")
	}
	t.Logf("same query, same ranking: 4-arm pool -> defined=false (EXCLUDED); "+
		"5-arm pool -> defined=true, NDCG=%.4f (INCLUDED, as a zero)", v)
	if v != 0 {
		t.Errorf("NDCG %.4f, want 0 — the retained query contributes a zero to every arm, which "+
			"is what moves the ABSOLUTE bars S1 and K1 read", v)
	}

	// Which is why both the arm set and the pooling depth are pinned, and why a
	// change to either is a re-pre-registration rather than a config tweak. D2
	// added the fifth arm; any later change re-partitions the population again.
	if msg := ctInstrumentPinViolation(ctArmNames, ctPreregistered.PoolDepth); msg != "" {
		t.Fatalf("the running instrument is not the pre-registered one: %s", msg)
	}
}
