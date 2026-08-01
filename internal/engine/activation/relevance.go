package activation

// Relevance bands (#773). A WORD, not a float, deliberately.
//
// The displayed `Score` is per-query max-renormalized (engine.go, the 1/maxRaw
// rescale): the best row of ANY query — including one whose answer this vault
// does not contain — is pinned to ~1.0. `Confidence` is the engram's stored
// TRUTH-belief (COG-10), not a retrieval statistic. So the payload an agent
// consumes for an unanswerable query reads {score: 1.0, confidence: 1} and
// nothing on the row says "weak".
//
// Answering that with ANOTHER number (absolute_score: 0.09 next to score: 1.0)
// asks the agent to know which of two floats to believe. A categorical cannot
// be misread as 1.0.
const (
	// RelevanceStrong: absolute evidence at or above half this vault's own
	// resolved content-channel ceiling. On default weights that is 0.5, which
	// engine.go's measured note says is unreachable without near-verbatim
	// wording (cos >= 0.9200).
	RelevanceStrong = "strong"
	// RelevanceModerate: real absolute evidence, clear of the noise band.
	RelevanceModerate = "moderate"
	// RelevanceWeak: within one doubling of this vault's calibration gate —
	// which COG-26 deliberately placed just above the model's MEASURED
	// out-of-domain noise ceiling. Arithmetically hard to tell from noise.
	RelevanceWeak = "weak"
	// RelevanceFilterMatch: admitted by an explicit tag filter below the
	// relevance bar (COG-5 S1). The filter defined the set, not relevance.
	// Calling these "weak" would be technically true and practically a lie.
	RelevanceFilterMatch = "filter_match"
	// RelevanceUncalibrated: this row's absolute score is not on a scale this
	// vault can band. Loud, never invented (#582/#585/#589 doctrine applied to
	// a display field). RelevanceBasis names which reason.
	RelevanceUncalibrated = "uncalibrated"
)

// Band bases. Set only for filter_match and uncalibrated; they name WHY.
const (
	// BasisTagFilterBypass: COG-5 S1 threshold bypass.
	BasisTagFilterBypass = "tag_filter_bypass"
	// BasisRRFFusion: rank-based finals with a coerced ~0.001 default.
	// "Cleared the bar" carries almost no information there and neither the
	// noise-floor nor the ceiling reasoning holds. Same calibration reason
	// COG-18 R1 skips the entity boost under rrf.
	BasisRRFFusion = "rrf_fusion"
	// BasisWeightedSumFusion: the legacy DisableACTR path reports AbsoluteScore
	// "for parity" only and is NOT gated on it; ContentMatch is the ACT-R
	// aboutness term and this path computes no comparable quantity. Banding it
	// would be inventing a number.
	BasisWeightedSumFusion = "weighted_sum_fusion"
	// BasisNoModelBaseline: COG-26 gave this query's embed model the identity
	// transform (unregistered/unresolved model, SemanticBaseline == 0). Under
	// identity, semCal is RAW cosine, whose noise floor is ~0.45 rather than
	// ~0 — every band would be wrong. Cold start says so out loud.
	BasisNoModelBaseline = "no_model_baseline"
	// BasisSemanticFloorDisabled: the identity transform again, but because the
	// vault's OPERATOR explicitly set `semantic_floor: 0` — the documented way
	// to disable the COG-26 floor, honored per the #582/#585/#589 doctrine.
	// The arithmetic consequence is the same as BasisNoModelBaseline (semCal is
	// raw cosine; no band is honest), but the CAUSE is the opposite: a choice,
	// not a missing calibration. Reporting no_model_baseline here told the
	// operator their model was unregistered — a false cause (G6 refute of
	// #773, finding 3).
	BasisSemanticFloorDisabled = "semantic_floor_disabled"
	// BasisSemanticDegraded: the semantic weight was redistributed onto FTS for
	// this response and the ceiling shifted (COG-24's residual ~0.385-0.4).
	// Banding against an unshifted ceiling would be plausible-and-wrong.
	BasisSemanticDegraded = "semantic_degraded"
	// BasisNoContentChannel: the resolved content-channel ceiling is <= 0, so
	// there is no scale to band against.
	BasisNoContentChannel = "no_content_channel"
	// BasisNoCalibrationGate: the caller resolved no positive calibration gate.
	// Unreachable from the engine phase today (the COG-6 ACT-R default is 0.1,
	// pinned by TestRelevanceBand_EngineCalibrationGateIsPositive) — present so
	// a zero gate can never silently band everything "moderate".
	BasisNoCalibrationGate = "no_calibration_gate"
	// BasisNotScored: this row never went through phase-6 scoring — it was
	// appended by an engine-layer injector (entity-boost spread activation,
	// supersession head promotion) whose ScoreComponents are the zero value.
	// Its aboutness against this query was not MEASURED, which is a different
	// statement from "measured and low". See the Admission plumbing below.
	BasisNotScored = "not_scored"
)

// Band edge ratios (#773 D4). JUDGMENT, NOT MEASUREMENT — no corpus was
// consulted to pick 2.0 and 0.5. Each is a ratio whose MEANING is sourced to a
// landed invariant, and each is applied against a quantity THIS VAULT resolves
// for itself, so a vault that reweights its channels or moves its gate gets
// band edges that move with it (principle #11: self-derivation, never a
// constant tuned on one corpus).
//
// Both live here, in one place, with their derivation (principle #6).
const (
	// relevanceWeakGateMultiple — why 2x the calibration gate is the weak
	// ceiling: COG-26 deliberately placed the MEASURED out-of-domain noise
	// ceiling immediately UNDER the gate. b = 0.520 was chosen because the top
	// observed noise cosine (~0.596) rescales to semCal ~0.158, i.e.
	// ContentMatch ~0.095 — clearing a 0.1 gate "by only ~0.005" (COG-26,
	// verbatim). So on this codebase "just above the gate" already means
	// "arithmetically indistinguishable from this model's noise". A row within
	// ONE DOUBLING of that floor is weak. That is a restatement of a landed
	// invariant, not a new number.
	relevanceWeakGateMultiple = 2.0
	// relevanceStrongCeilingFraction — why half the content ceiling is the
	// strong floor: engine.go records the measured structural fact that
	// ContentMatch is capped at w_sem for a semantic-only match and w_fts for a
	// lexical-only one, so "NO honest absolute score reaches 0.5 without
	// near-verbatim wording (cos >= 0.9200)". Expressed as a FRACTION of the
	// vault's own resolved ceiling; on default weights (0.6 + 0.4 = 1.0) it
	// reproduces the in-tree 0.5 exactly.
	relevanceStrongCeilingFraction = 0.5
)

// Fusion modes reported on ActivateResult.Calibration.
const (
	FusionACTR        = "actr"
	FusionCGDN        = "cgdn"
	FusionRRF         = "rrf"
	FusionWeightedSum = "weighted_sum"
)

// RelevanceCalibration is what Run reports about the scale its AbsoluteScores
// live on, resolved at the ONE site that resolves weights. The engine-layer
// band phase reads these rather than re-resolving weights and hoping the two
// agree (principle #6).
type RelevanceCalibration struct {
	// ContentCeiling is w_sem + w_fts from the RESOLVED weights Run actually
	// used — the structural maximum an honest ContentMatch can reach.
	ContentCeiling float64
	// FusionMode is the EFFECTIVE mode this run scored under, decided after
	// the rrf/cgdn conflict resolution, not read off the request.
	FusionMode string
	// SemanticBaseline is COG-26's resolved b. 0 means the identity transform
	// (unregistered/unresolved embed model) — see BasisNoModelBaseline.
	SemanticBaseline float64
	// BaselineExplicitlyDisabled distinguishes the two causes of
	// SemanticBaseline == 0: true means the vault's operator explicitly set
	// `semantic_floor: 0` (a documented, legal choice — see
	// BasisSemanticFloorDisabled); false means no calibrated baseline exists
	// for this model (BasisNoModelBaseline). Meaningless when
	// SemanticBaseline > 0.
	BaselineExplicitlyDisabled bool
}

// Admission records HOW a row entered the response, so the relevance band
// phase never has to DERIVE it. The zero value is AdmissionInjected on purpose
// (principle #3, make the bad state unrepresentable): any engine-layer phase
// that appends a fresh ScoredEngram literal without phase-6 measurements is
// automatically reported as unmeasured rather than being silently banded from
// a zero-value ScoreComponents.
//
// #773's design derived filter_match as "AbsoluteScore < req.Threshold" and
// named plumbing as the required alternative IF a second sub-gate admission
// route existed. Two do — engine_entity_boost.go's pass-2b spread-activation
// injection and engine_supersession.go's phase-2a head promotion both append
// rows with a zero-value ScoreComponents — so this is plumbed, not derived.
type Admission uint8

const (
	// AdmissionInjected: appended by an engine-layer phase without phase-6
	// measurements. The zero value.
	AdmissionInjected Admission = iota
	// AdmissionScored: cleared the relevance gate on its own measured evidence.
	AdmissionScored
	// AdmissionTagFilter: below the relevance bar, admitted ONLY because an
	// explicit tag filter named it (COG-5 S1).
	AdmissionTagFilter
)

// admissionOf classifies a row that has just passed a scoring path's gate, on
// the REASON it passed — never on arithmetic alone (G6 refute of #773,
// finding 1).
//
// `floored` is ScoreComponents.ContentMatchFloored: the COG-5 S1 tag-match
// floor replaced this row's measured (lower) aboutness because an explicit tag
// filter named it. A floored row is a tag-filter admission BY CONSTRUCTION —
// its ContentMatch is the floor, not a measurement — and that must not depend
// on where the floored arithmetic lands relative to the threshold:
// tagMatchFloor (0.1) numerically equals the COG-6 ACT-R default gate, so a
// fresh confidence-1.0 floored row lands EXACTLY ON the boundary and a
// `gated < threshold` test would band it `weak` while its 0.9-confidence or
// stale twin banded filter_match. Confidence and recency must not decide what
// the filter did. Only computeACTR ever sets the flag (computeComponents
// applies no floor; the RRF path builds its literal without one), so on the
// rrf/weighted_sum call sites — which pass Final, not AbsoluteScore, and whose
// rows are response-wide uncalibrated anyway — it is structurally false.
//
// `gated` is whatever quantity that path compared against req.Threshold
// (AbsoluteScore on ACT-R/CGDN, Final on rrf/weighted_sum) — the SAME
// expression as the gate two lines above each call site. It still matters for
// the UNFLOORED tag-pool row whose own measured evidence fell below the bar:
// that row too was admitted only because the filter named it (the S1 threshold
// bypass), and it stays AdmissionTagFilter.
func admissionOf(gated, threshold float64, inTagPool, floored bool) Admission {
	if inTagPool && (floored || gated < threshold) {
		return AdmissionTagFilter
	}
	return AdmissionScored
}

// RelevanceBandEdges returns (weakMax, strongMin) for a vault, given its
// resolved calibration gate and content-channel ceiling. Both are inclusive
// lower bounds of the band above them.
//
// weakMax is clamped to strongMin so a vault with an unusually high gate or an
// unusually low ceiling degenerates to two bands (weak/strong) rather than to
// an empty or inverted moderate band.
func RelevanceBandEdges(calibrationGate, contentCeiling float64) (weakMax, strongMin float64) {
	strongMin = relevanceStrongCeilingFraction * contentCeiling
	weakMax = relevanceWeakGateMultiple * calibrationGate
	if weakMax > strongMin {
		weakMax = strongMin
	}
	return weakMax, strongMin
}

// RelevanceBand maps an ABSOLUTE score onto a band. Pure: no I/O, no engine
// state.
//
// `abs` MUST be ScoreComponents.AbsoluteScore — never Score/Final, which is
// divided by this query's own maximum and therefore says nothing comparable
// across queries. `calibrationGate` MUST be the vault's resolved DEFAULT gate,
// never the caller's req.Threshold: the noise floor is a property of the model
// and the vault, not of what the caller asked for. A caller passing
// threshold:0.01 must not thereby see a 0.09 row — BELOW the model's own
// measured noise ceiling — banded "moderate". That is the exact
// silently-plausible failure class this exists to remove.
func RelevanceBand(abs, calibrationGate, contentCeiling float64) (band, basis string) {
	if contentCeiling <= 0 {
		return RelevanceUncalibrated, BasisNoContentChannel
	}
	if calibrationGate <= 0 {
		return RelevanceUncalibrated, BasisNoCalibrationGate
	}
	weakMax, strongMin := RelevanceBandEdges(calibrationGate, contentCeiling)
	switch {
	case abs >= strongMin:
		return RelevanceStrong, ""
	case abs >= weakMax:
		return RelevanceModerate, ""
	default:
		return RelevanceWeak, ""
	}
}

// FusionBandBasis returns the uncalibrated basis for a fusion mode that does
// NOT gate on AbsoluteScore, or "" when the mode does gate on it and its rows
// may be banded. One predicate, reused rather than reinvented: a band is
// emitted exactly where the fusion mode gates on AbsoluteScore (ACT-R at
// engine.go's `absolute < req.Threshold`, CGDN at the same comparison).
func FusionBandBasis(mode string) string {
	switch mode {
	case FusionRRF:
		return BasisRRFFusion
	case FusionWeightedSum:
		return BasisWeightedSumFusion
	default:
		return ""
	}
}
