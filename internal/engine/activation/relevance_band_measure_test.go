//go:build localassets

package activation_test

import (
	"fmt"
	"math"
	"testing"

	"github.com/scrypster/muninndb/internal/engine/activation"
)

// ---------------------------------------------------------------------------
// #773 §6.3 — AGGREGATE MEASUREMENT OF THE WEAK-BAND HINT, against the EXISTING
// 50-query labeled set (48-engram synthetic corpus, real bge-small).
//
// THE RULE WAS WRITTEN BEFORE THE FIRST RUN and is reproduced verbatim from the
// design. Do not move these after seeing the numbers.
//
//  1. U >= 70% across rqAdjacent + rqStale + rqOOD combined, of the
//     unanswerable queries that RETURNED at least one row. These are the
//     queries #757 measured we cannot stop returning (87.5% / 100% FPR). We are
//     not fixing that. We are claiming that when we return them, we now say so.
//  2. A(nearVerbatim) = 0% and A(moderate) = 0%. The hint must NEVER fire on a
//     query whose gold answer was found with real evidence. A single violation
//     blocks the hint.
//  3. A(hard): REPORT ONLY, not a gate. A hard paraphrase genuinely IS weak
//     absolute evidence, and D3's wording ("nothing matched strongly", never
//     "no answer here") stays true there. Quantifying it is the point; failing
//     on it would be punishing honesty.
//
// PRE-COMMITTED FALLBACK, also written before the first run: if rule 1 fails,
// ship the PER-ROW BAND anyway — strictly more information than today, and the
// evaluators' #1 complaint is the row, not the response — and DEFER the hint,
// recording the measured number. That is the honest negative-result path.
//
// OUTCOME OF THE FIRST RUN: BOTH RULES FAILED. U(combined) = 16.7% (2/12), far
// under 70%; and A(moderate) = 10.0%, which rule 2 declares independently
// disqualifying. THE FALLBACK WAS TAKEN: the per-row band shipped, the
// response-level hint did not. Nothing was retuned to rescue it — moving the
// two D4 ratios until this set fired would be tuning them AGAINST the labeled
// set, which is the principle-#11 violation the design forbade in advance.
//
// This test therefore REPORTS the hint rules rather than gating on them — the
// hint is not shipped, so gating CI on a rule about it would be gating on
// nothing. It prints the verdict every run, and if a future increment makes
// both rules pass the log says so and the hint can ship then, with this file
// as the evidence.
//
// It DOES still gate one thing, because that one thing IS shipped: the band
// must never call a near-verbatim gold match weak. See the bottom of the test.
//
// This runs against the CURRENT arm only. It proposes no new arm, moves no
// threshold, and changes no corpus: it reads the same kept-row set rqEvaluate
// already computes and asks one extra question of it.
// ---------------------------------------------------------------------------

// The vault this set is measured on: the COG-6 ACT-R default gate, and the
// default 0.6 + 0.4 content-channel ceiling. Exactly what the engine phase
// would be handed for this configuration.
const (
	rqbGate    = 0.10
	rqbCeiling = 1.0
)

type rqbCell struct {
	returned int // queries that returned >= 1 row
	fired    int // ... of which the hint would fire
}

func (c rqbCell) pct() float64 {
	if c.returned == 0 {
		return 0
	}
	return 100 * float64(c.fired) / float64(c.returned)
}

// rqbHintFires applies D3 to a kept row set: every BANDED row weak, at least
// one banded row. Every row in this harness is scored evidence — there are no
// tag filters and no post-pipeline injectors here — so no row is excluded.
func rqbHintFires(abs []float64) (fires bool, banded int, best float64) {
	for _, a := range abs {
		band, basis := activation.RelevanceBand(a, rqbGate, rqbCeiling)
		if basis != "" {
			continue // excluded in both directions
		}
		banded++
		if band != activation.RelevanceWeak {
			return false, banded, 0
		}
		if a > best {
			best = a
		}
	}
	return banded > 0, banded, best
}

func TestMeasureRelevanceBandHint(t *testing.T) {
	embedder, svc := realBGEEmbedder(t)
	f := rqBuildFixture(t, embedder, svc, true, rqFixtureNow()) // FTS live = production
	arm := rqArms[0]                                            // CURRENT

	// absolutes returns the kept rows' AbsoluteScores under the CURRENT arm,
	// using the arm's own combiner so this can never drift from rqApply.
	absolutes := func(query string) ([]float64, []string) {
		kept := rqApply(rqProbeAt(t, f, query, 0), arm)
		abs := make([]float64, 0, len(kept))
		concepts := make([]string, 0, len(kept))
		for _, c := range kept {
			abs = append(abs, rqGateValue(c, arm))
			concepts = append(concepts, c.concept)
		}
		return abs, concepts
	}

	// ---- U: the unanswerable side --------------------------------------
	byKind := map[rqKind]*rqbCell{}
	for _, k := range rqKinds {
		byKind[k] = &rqbCell{}
	}
	var uAll rqbCell
	var silentLeaks []string

	t.Log("")
	t.Log("=========== U — UNANSWERABLE QUERIES THAT RETURNED SOMETHING ===========")
	for _, q := range rqUnanswerable {
		abs, concepts := absolutes(q.query)
		if len(abs) == 0 {
			continue // abstained: nothing to say, and nothing to say it about
		}
		fires, banded, best := rqbHintFires(abs)
		byKind[q.kind].returned++
		uAll.returned++
		if fires {
			byKind[q.kind].fired++
			uAll.fired++
		} else {
			silentLeaks = append(silentLeaks, fmt.Sprintf("[%-19s] %-58s top=%s abs=%.4f (%s)",
				q.kind, q.query, bandOf(maxOf(abs)), maxOf(abs), concepts[0]))
		}
		t.Logf("  [%-19s] hint=%-5v banded=%d best=%.4f  %s", q.kind, fires, banded, best, q.query)
	}

	t.Log("")
	for _, k := range rqKinds {
		c := byKind[k]
		t.Logf("  U(%-19s) = %5.1f%%  (%d/%d returned)", k, c.pct(), c.fired, c.returned)
	}
	t.Logf("  U(COMBINED)              = %5.1f%%  (%d/%d returned)", uAll.pct(), uAll.fired, uAll.returned)
	if len(silentLeaks) > 0 {
		t.Log("")
		t.Log("  Unanswerable queries returned SILENTLY (no hint) — the residual:")
		for _, s := range silentLeaks {
			t.Logf("    %s", s)
		}
	}

	// ---- A: the answerable side ----------------------------------------
	byBand := map[rqBand]*rqbCell{}
	for _, b := range rqBands {
		byBand[b] = &rqbCell{}
	}
	var overfires []string

	t.Log("")
	t.Log("=========== A — ANSWERABLE QUERIES WHOSE GOLD WAS FOUND ===========")
	for _, q := range rqAnswerable {
		gold, ok := f.byConcept[q.gold]
		if !ok {
			t.Fatalf("label %q does not resolve to a corpus engram", q.gold)
		}
		kept := rqApply(rqProbeAt(t, f, q.query, 0), arm)
		found := false
		abs := make([]float64, 0, len(kept))
		for _, c := range kept {
			abs = append(abs, rqGateValue(c, arm))
			if c.id == gold {
				found = true
			}
		}
		if !found {
			continue // gold missed: the hint's behaviour there is #757's problem
		}
		fires, _, best := rqbHintFires(abs)
		byBand[q.band].returned++
		if fires {
			byBand[q.band].fired++
			overfires = append(overfires, fmt.Sprintf("[%-15s] %-58s best abs=%.4f", q.band, q.query, best))
		}
		t.Logf("  [%-15s] hint=%-5v best=%.4f  %s", q.band, fires, best, q.query)
	}

	t.Log("")
	for _, b := range rqBands {
		c := byBand[b]
		t.Logf("  A(%-15s) = %5.1f%%  (%d/%d gold-found)", b, c.pct(), c.fired, c.returned)
	}
	if len(overfires) > 0 {
		t.Log("")
		t.Log("  Queries whose gold was FOUND and which still fired the hint:")
		for _, s := range overfires {
			t.Logf("    %s", s)
		}
	}

	// ---- THE PRE-COMMITTED RULE ----------------------------------------
	t.Log("")
	t.Log("=========== PRE-COMMITTED ACCEPTANCE RULE ===========")
	rule1 := uAll.pct() >= 70.0
	rule2 := byBand[rqNearVerbatim].fired == 0 && byBand[rqModerate].fired == 0
	t.Logf("  RULE 1  U(combined) = %.1f%% >= 70%%                 : %v", uAll.pct(), rule1)
	t.Logf("  RULE 2  A(near-verbatim) = %.1f%%, A(moderate) = %.1f%%, both must be 0%% : %v",
		byBand[rqNearVerbatim].pct(), byBand[rqModerate].pct(), rule2)
	t.Logf("  RULE 3  A(hard-paraphrase) = %.1f%%  — REPORT ONLY, deliberately not a gate",
		byBand[rqHard].pct())

	t.Log("")
	switch {
	case rule1 && rule2:
		t.Log("  VERDICT: BOTH RULES PASS. The response-level weak-band hint is currently")
		t.Log("  DEFERRED (see internal/engine/engine_relevance.go). On these numbers it")
		t.Log("  could now ship — reopen #773's D3 with this run as the evidence.")
	case !rule2:
		t.Log("  VERDICT: RULE 2 VIOLATED — the hint would fire on a query whose gold answer")
		t.Log("  WAS found with real evidence, telling an agent to distrust a correct")
		t.Log("  answer. Independently disqualifying. The hint stays deferred.")
		fallthrough
	default:
		if !rule1 {
			t.Log("  VERDICT: RULE 1 FAILED — most unanswerable queries come back with")
			t.Log("  MODERATE absolute evidence, because they genuinely carry real semantic")
			t.Log("  and lexical signal. \"Has evidence\" is not \"answers the question\";")
			t.Log("  that gap is #757's measured ceiling, not something a band edge fixes.")
		}
		t.Log("  The PRE-COMMITTED FALLBACK is in effect: the per-row band ships, the")
		t.Log("  response-level hint does not. Do NOT retune the D4 ratios against this")
		t.Log("  set to rescue it — that is the principle-#11 violation the design")
		t.Log("  forbade in advance.")
	}

	// The band's own quality bar, which DID hold and is worth pinning: a
	// near-verbatim query whose gold was found never looks weak.
	if byBand[rqNearVerbatim].fired != 0 {
		t.Errorf("A(near-verbatim) = %.1f%%: the band called a near-verbatim gold match "+
			"weak. Whatever happens to the hint, the BAND must never do that.",
			byBand[rqNearVerbatim].pct())
	}

	// ---- SELF-DEFENSE OF THE HONEST-NEGATIVE RECORD ---------------------
	// engine_relevance.go's deferral quotes U(combined) = 16.7% and
	// A(moderate) = 10.0% as the measured reasons the hint did not ship. A
	// record that only ever LOGS can rot invisibly: an embedder, corpus or
	// band-edge change could move these numbers in EITHER direction — toward
	// "both rules now pass, the hint could ship" or toward "the quoted numbers
	// are fiction" — with CI silently green throughout. Bounded assertions,
	// deliberately loose enough (± one query on these denominators) to absorb
	// float noise, deliberately two-sided so improvement is surfaced too. On
	// drift: re-run, update the recorded numbers HERE and in
	// engine_relevance.go's deferral note together, and re-evaluate the
	// pre-committed rule — never delete the assertion.
	const tolerancePts = 5.0
	if math.Abs(uAll.pct()-16.7) > tolerancePts {
		t.Errorf("U(combined) = %.1f%% (%d/%d) drifted from the recorded 16.7%% (2/12) by more "+
			"than %.0f points — the honest-negative record in engine_relevance.go is stale; "+
			"update BOTH together and re-evaluate the pre-committed rule",
			uAll.pct(), uAll.fired, uAll.returned, tolerancePts)
	}
	if math.Abs(byBand[rqModerate].pct()-10.0) > tolerancePts {
		t.Errorf("A(moderate) = %.1f%% (%d/%d) drifted from the recorded 10.0%% (1/10) by more "+
			"than %.0f points — the honest-negative record in engine_relevance.go is stale; "+
			"update BOTH together and re-evaluate the pre-committed rule",
			byBand[rqModerate].pct(), byBand[rqModerate].fired, byBand[rqModerate].returned, tolerancePts)
	}
}

func maxOf(xs []float64) float64 {
	m := 0.0
	for _, x := range xs {
		m = math.Max(m, x)
	}
	return m
}

func bandOf(abs float64) string {
	b, _ := activation.RelevanceBand(abs, rqbGate, rqbCeiling)
	return b
}
