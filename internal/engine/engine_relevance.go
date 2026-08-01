package engine

import (
	"github.com/scrypster/muninndb/internal/engine/activation"
)

// #773 — score-presentation honesty.
//
// THE DEFECT. Recall's displayed `score` is per-query max-renormalized: when
// any candidate saturates (the ACT-R prior reaches 3.24x at full Hebbian
// boost, so this is routine) the argmax lands at raw exactly 1.0 and its final
// is exactly its Confidence — so the best row of ANY query, however weak, is
// pinned to ~1.0. `confidence` beside it is the engram's stored TRUTH-belief
// (COG-10), not a retrieval statistic. The measured live case in the issue:
// vector_score 0.09 -> score 1.0, confidence 1. For a query whose answer is
// not in the vault, the payload an agent consumes was
// {concept, content, score: 1.0, confidence: 1} with NO field anywhere on the
// row saying "weak".
//
// WHAT THIS PHASE IS NOT. It does not attempt answerability. #757 measured
// that ceiling against nine candidate gate arms under a pre-committed rule and
// shipped none: with the lexical channel silent the gate reduces to one cosine
// cutoff and topically-adjacent unanswerables pass at 87.5%. This is not a
// tenth arm. It changes NO row, NO score, NO order, NO threshold — see
// TestRelevanceBands_IsAPureAnnotation. We cannot compute answerability; we
// can stop overstating what we did compute.
//
// PLACEMENT. Runs LAST, after currency (COG-25), contradiction honesty
// (COG-29), the final truncation and the abstention recompute, so it sees
// exactly the row set the caller receives. Read-only, like COG-25/COG-29:
// no score change, no reorder, no truncation, no removal, no write
// (observe-safe by construction, COG-11).
//
// KNOWN CONSERVATIVE RESIDUAL (cold start): while a fresh write's embedding is
// still pending indexing, the semantic channel is silent and a row's absolute
// score is capped at w_fts (0.4 on default weights) — below the 0.5 strong
// floor — so `strong` is unreachable and a genuine match may under-band until
// the index catches up; the error direction is deliberate (understate, never
// overstate).
//
// The three measurement harnesses (abstention_gate_measure_test.go,
// recall_queryset_measure_test.go, shadow_measure_test.go) call
// activation.Run DIRECTLY. This phase lives in internal/engine and runs after
// Run returns, so those harnesses CANNOT SEE IT and their numbers cannot move.
// That is the load-bearing property of this increment and it is structural,
// not an assertion.

// THE RESPONSE-LEVEL WEAK-BAND HINT IS DEFERRED. Read this before adding it.
//
// The design proposed a `relevance_summary` block carrying "nothing matched
// strongly; these are neighbours" whenever EVERY banded row was weak, gated on
// a rule written BEFORE the first run:
//
//	1. U >= 70% — of the unanswerable queries in the 50-query labeled set that
//	   returned at least one row, the fraction where the hint would fire.
//	2. A(near-verbatim) = 0% AND A(moderate) = 0% — the hint must never fire on
//	   a query whose gold answer was found with real evidence. A SINGLE
//	   violation blocks it.
//	3. A(hard-paraphrase): report only, never a gate.
//
// MEASURED (relevance_band_measure_test.go, CURRENT arm, FTS live, real
// bge-small, 48-engram corpus):
//
//	U(combined)              16.7%  (2/12 returned)   <- RULE 1 FAILED
//	  out-of-domain         100.0%  (1/1)
//	  topically-adjacent     14.3%  (1/7)
//	  present-tense/stale     0.0%  (0/4)
//	A(near-verbatim)          0.0%  (0/10)
//	A(moderate)              10.0%  (1/10)            <- RULE 2 FAILED
//	A(hard-paraphrase)       25.0%  (2/8)             (report only)
//
// WHY, and why it is not fixable by moving a number. Topically-adjacent and
// present-tense unanswerables come back with absolute scores of 0.23-0.49 —
// solidly `moderate`, because they genuinely DO carry real semantic and
// IDF-weighted lexical evidence. "Has real evidence" is simply not the same
// predicate as "answers this question", which is exactly the answerability
// ceiling #757 measured against nine arms and shipped none of. Lowering the
// band edges until the hint fires would be tuning the two D4 ratios against
// the labeled set — the principle-#11 violation the design explicitly forbade
// ("§7.3 validates the resulting band ASSIGNMENT under a pre-committed rule;
// it does not tune the ratios against it").
//
// So the design's PRE-COMMITTED FALLBACK is what shipped: the per-row band —
// strictly more information than before, and the evaluators' #1 complaint was
// the ROW, not the response — and no hint. Rule 2's failure is independently
// disqualifying: a hint that fires on a moderate-difficulty query whose gold
// WAS found tells an agent to distrust a correct answer, which is a new
// dishonesty, not a cure for the old one.
//
// The measurement stays live and reports every run. If a future increment
// makes both rules pass, that test says so and the hint can ship then.

// cog6DefaultThreshold is the COG-6 fusion-aware DEFAULT recall threshold,
// keyed on the EFFECTIVE scoring mode. ONE computation site (principle #6):
// the threshold coerce in activateCore and the relevance-band calibration
// anchor both read it — but note they do NOT read it at the same instant.
// The coerce runs BEFORE applyRecallModePreset and the band anchor after, so
// a recall-mode preset that flips UseACTR post-coerce would hand the two
// calls different arguments. Today that divergence is unobservable only by
// COINCIDENCE, not structure: !UseACTR forces FusionWeightedSum, whose rows
// are response-wide uncalibrated, so the mismatched anchor is never consulted
// — two independent predicates that happen to line up. If a preset ever
// changes fusion mode in a way that stays bandable, re-derive the anchor from
// the weights the run actually used.
//
// rrf returns 0 = "unset", leaving activation.Run to apply its own rrf default
// (0.001, #590's mechanism); coercing an ACT-R-calibrated 0.1 here made #590's
// fix unreachable on every production transport.
func cog6DefaultThreshold(useRRF, useACTR bool) float64 {
	if useRRF {
		return 0
	}
	if useACTR {
		// The value COG-26's b=0.520 was calibrated against, on the
		// absolute-score scale the ACT-R gate now compares.
		return 0.1
	}
	// Legacy weighted_sum: its blended Final gives content-irrelevant fresh
	// memories ~0.3 from decay/recency/access alone, and the only bar ever
	// validated against that formula is the old 0.5 surface default. 0.1 was
	// measured on the ACT-R absolute scale ONLY — applying it here would let
	// recency spam through (#754 review, finding 5).
	return 0.5
}

// applyRelevanceBands assigns each row its absolute relevance band. It returns
// bands and bases parallel to rows; it emits no response-level block (see the
// deferral above).
//
// calibrationGate is the vault's resolved COG-6 DEFAULT gate — deliberately
// NOT actReq.Threshold. The noise floor is a property of the model and the
// vault, not of what the caller asked for: a caller passing threshold:0.01
// must not thereby see a 0.09 row (below the model's own measured noise
// ceiling) banded "moderate". actReq.Threshold is not consulted at all here;
// the filter-match case is carried on Admission instead of derived from it.
func applyRelevanceBands(
	rows []activation.ScoredEngram,
	cal activation.RelevanceCalibration,
	calibrationGate float64,
	semanticDegraded bool,
) (bands []string, bases []string) {
	bands = make([]string, len(rows))
	bases = make([]string, len(rows))
	if len(rows) == 0 {
		return bands, bases
	}

	// Response-wide exclusions, evaluated once. Each is a DECISION, in the
	// style COG-18 R1 / COG-28 already use — see the basis constants in
	// activation/relevance.go for why each mode cannot be banded.
	// PRECEDENCE (deliberate, per the design's D5 table order): a
	// response-wide uncalibrated basis (fusion mode, semantic_degraded,
	// disabled/absent baseline) overrides per-row AdmissionTagFilter, so a
	// due:<=today reminder in a degraded response reads
	// uncalibrated/<cause>, not filter_match — even though the
	// filter-admission fact is calibration-independent. The trade: one
	// consistent "this response's numbers are uncalibrated" signal beats a
	// mixed response where some rows look confidently classified. A
	// follow-up may flip tag rows specifically; do it deliberately, not as
	// a drive-by.
	responseBasis := activation.FusionBandBasis(cal.FusionMode)
	if responseBasis == "" && semanticDegraded {
		responseBasis = activation.BasisSemanticDegraded
	}
	if responseBasis == "" && cal.SemanticBaseline <= 0 {
		// Same arithmetic (identity transform, raw-cosine noise floor ~0.45,
		// no honest band), two OPPOSITE causes — name the right one. An
		// operator who explicitly set `semantic_floor: 0` made a documented
		// choice and must not be told their model is unregistered (G6 refute
		// of #773, finding 3).
		if cal.BaselineExplicitlyDisabled {
			responseBasis = activation.BasisSemanticFloorDisabled
		} else {
			responseBasis = activation.BasisNoModelBaseline
		}
	}

	for i, row := range rows {
		switch {
		case responseBasis != "":
			bands[i], bases[i] = activation.RelevanceUncalibrated, responseBasis
		case row.Admission == activation.AdmissionTagFilter:
			// COG-5 S1: an explicit tag filter admitted this row BELOW the
			// relevance bar (a due:<=today reminder whose content is unrelated
			// to the query text). Calling it "weak" would be technically true
			// and practically a lie: the filter defined the set, not
			// relevance. A reminder surfaced by due:<=today makes no claim
			// about how well its text matched the query.
			bands[i], bases[i] = activation.RelevanceFilterMatch, activation.BasisTagFilterBypass
		case row.Admission == activation.AdmissionInjected:
			// Appended by an engine-layer injector with no phase-6
			// measurement: entity-boost spread activation
			// (engine_entity_boost.go pass 2b) or supersession head promotion
			// (engine_supersession.go phase 2a). Its ScoreComponents are the
			// ZERO VALUE — AbsoluteScore 0 means "not measured", which is a
			// different statement from "measured and low". Banding it "weak"
			// off a zero it never earned would be inventing a number
			// (#582/#585/#589 doctrine). COG-28 substituted heads are NOT in
			// this case: they carry the predecessor's real measurements and
			// are marked AdmissionScored at the substitution site, consistent
			// with the substitution_basis that attributes them.
			bands[i], bases[i] = activation.RelevanceUncalibrated, activation.BasisNotScored
		default:
			bands[i], bases[i] = activation.RelevanceBand(
				row.Components.AbsoluteScore, calibrationGate, cal.ContentCeiling)
		}
	}
	return bands, bases
}
