package engine

import (
	"reflect"
	"testing"

	"github.com/scrypster/muninndb/internal/engine/activation"
	"github.com/scrypster/muninndb/internal/storage"
)

// The in-tree default vault: COG-6 ACT-R gate 0.1, w_sem 0.6 + w_fts 0.4.
// weakMax 0.2, strongMin 0.5.
var defaultCal = activation.RelevanceCalibration{
	ContentCeiling:   1.0,
	FusionMode:       activation.FusionACTR,
	SemanticBaseline: 0.520, // COG-26's b for bge-small-en-v1.5
}

const defaultGate = 0.1

func row(abs float64, adm activation.Admission) activation.ScoredEngram {
	return activation.ScoredEngram{
		Engram:     &storage.Engram{Concept: "c"},
		Score:      1.0, // the #773 defect: every top row looks like this
		Components: activation.ScoreComponents{AbsoluteScore: abs},
		Admission:  adm,
	}
}

func scored(abs float64) activation.ScoredEngram {
	return row(abs, activation.AdmissionScored)
}

// cog6DefaultThreshold is the calibration anchor. If it ever stops matching the
// engine's own default coerce, every band silently recalibrates.
func TestCog6DefaultThreshold(t *testing.T) {
	if got := cog6DefaultThreshold(false, true); got != 0.1 {
		t.Errorf("ACT-R default = %v, want 0.1 (the value COG-26's b=0.520 was calibrated against)", got)
	}
	if got := cog6DefaultThreshold(false, false); got != 0.5 {
		t.Errorf("weighted_sum default = %v, want 0.5", got)
	}
	if got := cog6DefaultThreshold(true, true); got != 0 {
		t.Errorf("rrf default = %v, want 0 (activation.Run applies its own 0.001, #590)", got)
	}
}

// The calibration gate the band phase is handed must be POSITIVE on the
// production ACT-R path, or RelevanceBand falls through to
// no_calibration_gate and nothing is ever banded.
func TestRelevanceBand_EngineCalibrationGateIsPositive(t *testing.T) {
	if cog6DefaultThreshold(false, true) <= 0 {
		t.Fatal("the ACT-R calibration gate must be > 0")
	}
	band, basis := activation.RelevanceBand(0.6, cog6DefaultThreshold(false, true), 1.0)
	if band != activation.RelevanceStrong || basis != "" {
		t.Fatalf("band = (%q, %q), want (strong, \"\")", band, basis)
	}
}

// ---------------------------------------------------------------------------
// D5 — where a band is emitted, and where it must REFUSE to be.
// Each exclusion is a decision, not an oversight; see relevance.go.
// ---------------------------------------------------------------------------

func TestRelevanceBands_D5ExclusionTable(t *testing.T) {
	tests := []struct {
		name      string
		cal       activation.RelevanceCalibration
		degraded  bool
		rows      []activation.ScoredEngram
		wantBands []string
		wantBases []string
	}{
		{
			name:      "rrf fusion is uncalibrated, never banded",
			cal:       activation.RelevanceCalibration{ContentCeiling: 1.0, FusionMode: activation.FusionRRF, SemanticBaseline: 0.52},
			rows:      []activation.ScoredEngram{scored(0.9), scored(0.01)},
			wantBands: []string{activation.RelevanceUncalibrated, activation.RelevanceUncalibrated},
			wantBases: []string{activation.BasisRRFFusion, activation.BasisRRFFusion},
		},
		{
			name:      "legacy weighted_sum is uncalibrated",
			cal:       activation.RelevanceCalibration{ContentCeiling: 1.0, FusionMode: activation.FusionWeightedSum, SemanticBaseline: 0.52},
			rows:      []activation.ScoredEngram{scored(0.05)},
			wantBands: []string{activation.RelevanceUncalibrated},
			wantBases: []string{activation.BasisWeightedSumFusion},
		},
		{
			// COG-26 identity fallback: semCal is RAW cosine, noise floor ~0.45
			// not ~0. Cold start says so out loud rather than inventing a band.
			name:      "unregistered embed model is uncalibrated, loudly",
			cal:       activation.RelevanceCalibration{ContentCeiling: 1.0, FusionMode: activation.FusionACTR, SemanticBaseline: 0},
			rows:      []activation.ScoredEngram{scored(0.05)},
			wantBands: []string{activation.RelevanceUncalibrated},
			wantBases: []string{activation.BasisNoModelBaseline},
		},
		{
			// Same arithmetic (identity transform), OPPOSITE cause: the
			// operator explicitly set semantic_floor: 0, a documented, legal
			// choice. Reporting no_model_baseline here is a false cause — it
			// tells the operator their model is unregistered (G6 refute of
			// #773, finding 3). RED at dda8a3e: emitted no_model_baseline.
			name:      "explicitly disabled semantic floor names the choice, not a missing model",
			cal:       activation.RelevanceCalibration{ContentCeiling: 1.0, FusionMode: activation.FusionACTR, SemanticBaseline: 0, BaselineExplicitlyDisabled: true},
			rows:      []activation.ScoredEngram{scored(0.05)},
			wantBands: []string{activation.RelevanceUncalibrated},
			wantBases: []string{activation.BasisSemanticFloorDisabled},
		},
		{
			name:      "semantic degraded is uncalibrated",
			cal:       defaultCal,
			degraded:  true,
			rows:      []activation.ScoredEngram{scored(0.05)},
			wantBands: []string{activation.RelevanceUncalibrated},
			wantBases: []string{activation.BasisSemanticDegraded},
		},
		{
			name:      "no content channel is uncalibrated",
			cal:       activation.RelevanceCalibration{ContentCeiling: 0, FusionMode: activation.FusionACTR, SemanticBaseline: 0.52},
			rows:      []activation.ScoredEngram{scored(0.05)},
			wantBands: []string{activation.RelevanceUncalibrated},
			wantBases: []string{activation.BasisNoContentChannel},
		},
		{
			// CGDN gates on AbsoluteScore exactly as ACT-R does, so it bands.
			name:      "cgdn bands like act-r",
			cal:       activation.RelevanceCalibration{ContentCeiling: 1.0, FusionMode: activation.FusionCGDN, SemanticBaseline: 0.52},
			rows:      []activation.ScoredEngram{scored(0.6)},
			wantBands: []string{activation.RelevanceStrong},
			wantBases: []string{""},
		},
		{
			// COG-5 S1: the filter defined the set, not relevance. Calling
			// these weak would be technically true and practically a lie.
			name:      "tag-filter bypass is filter_match",
			cal:       defaultCal,
			rows:      []activation.ScoredEngram{row(0.02, activation.AdmissionTagFilter)},
			wantBands: []string{activation.RelevanceFilterMatch},
			wantBases: []string{activation.BasisTagFilterBypass},
		},
		{
			// Entity-boost pass 2b / supersession phase 2a append rows with a
			// ZERO-VALUE ScoreComponents. AbsoluteScore 0 there means "not
			// measured", not "measured and low".
			name:      "an unmeasured injected row is uncalibrated, not weak",
			cal:       defaultCal,
			rows:      []activation.ScoredEngram{row(0, activation.AdmissionInjected)},
			wantBands: []string{activation.RelevanceUncalibrated},
			wantBases: []string{activation.BasisNotScored},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bands, bases := applyRelevanceBands(tt.rows, tt.cal, defaultGate, tt.degraded)
			if !reflect.DeepEqual(bands, tt.wantBands) {
				t.Errorf("bands = %v, want %v", bands, tt.wantBands)
			}
			if !reflect.DeepEqual(bases, tt.wantBases) {
				t.Errorf("bases = %v, want %v", bases, tt.wantBases)
			}
		})
	}
}

// COG-28 x COG-30: how substituted rows band. TWO shapes, deliberately
// different, both documented at the substitution site (engine_version_head.go):
//
//   - An INJECTED head (the head was not in the pool; the query's evidence
//     reached its predecessor): Run never scored it, but the substitution
//     copies the PREDECESSOR's measured Components onto the row and marks it
//     AdmissionScored — so it bands on that real predecessor evidence, which
//     substitution_basis already attributes. It must NOT fall into the
//     not_scored bucket reserved for zero-value injections.
//   - An IN-POOL RAISE (the head matched on its own but the predecessor
//     matched harder): only Score is raised to the predecessor's Final;
//     Components remain the row's OWN measurements, so it bands on its own
//     evidence — the raise moves the DISPLAYED score, never the band, exactly
//     as the COG-29 demote doesn't (bands read AbsoluteScore, not Score).
func TestRelevanceBand_SubstitutedRowBandsOnPredecessorEvidence(t *testing.T) {
	pred := storage.ULID{0x01}
	predComponents := activation.ScoreComponents{AbsoluteScore: 0.6} // strong predecessor evidence

	injected := activation.ScoredEngram{
		Engram:            &storage.Engram{Concept: "injected-head"},
		Score:             0.9,
		Components:        predComponents, // the predecessor's measurements, copied at injection
		SubstitutedFor:    pred,
		SubstitutionBasis: &predComponents,
		Admission:         activation.AdmissionScored,
	}
	raised := activation.ScoredEngram{
		Engram:            &storage.Engram{Concept: "raised-head"},
		Score:             0.9,                                             // raised to the predecessor's Final
		Components:        activation.ScoreComponents{AbsoluteScore: 0.14}, // its OWN weak evidence
		SubstitutedFor:    pred,
		SubstitutionBasis: &predComponents,
		Admission:         activation.AdmissionScored,
	}

	bands, bases := applyRelevanceBands(
		[]activation.ScoredEngram{injected, raised}, defaultCal, defaultGate, false)

	if bands[0] != activation.RelevanceStrong || bases[0] != "" {
		t.Errorf("injected head = (%q, %q), want (strong, \"\") — it carries the predecessor's "+
			"REAL measurements and must band on them, not fall into not_scored", bands[0], bases[0])
	}
	if bands[1] != activation.RelevanceWeak || bases[1] != "" {
		t.Errorf("raised head = (%q, %q), want (weak, \"\") — the raise moves Score only; the band "+
			"reads the row's OWN AbsoluteScore", bands[1], bases[1])
	}
}

// The response-level weak-band hint is DEFERRED (see engine_relevance.go for
// the pre-committed rule, the measured numbers and why). These pin the
// interactions the band itself still has to get right.

// COG-29's demote multiplies SCORE; the band reads
// ABSOLUTE SCORE. So a demoted row never changes band. That is correct and
// deliberate — "is this disputed" and "did this match" are different questions.
func TestRelevanceBands_ContradictionDemoteDoesNotChangeBand(t *testing.T) {
	r := scored(0.6)
	before, _ := applyRelevanceBands([]activation.ScoredEngram{r}, defaultCal, defaultGate, false)
	// COG-29 demote: min() against Score, AbsoluteScore untouched.
	r.Score = 0.05
	after, _ := applyRelevanceBands([]activation.ScoredEngram{r}, defaultCal, defaultGate, false)
	if before[0] != activation.RelevanceStrong || after[0] != before[0] {
		t.Errorf("band moved with a Score demote: %q -> %q (want strong -> strong)", before[0], after[0])
	}
}

// D4's anchor, at the phase level: the caller's threshold is NOT an input to
// this function at all, so no request parameter can promote a below-noise row.
func TestRelevanceBands_CallerThresholdIsNotAnInput(t *testing.T) {
	fn := reflect.TypeOf(applyRelevanceBands)
	for i := 0; i < fn.NumIn(); i++ {
		if fn.In(i) == reflect.TypeOf((*activation.ActivateRequest)(nil)) {
			t.Fatal("applyRelevanceBands must not take the request: req.Threshold must not " +
				"be reachable as a band anchor (D4 — threshold:0.01 must not band a 0.09 row moderate)")
		}
	}
}

// §6.1 Pin 1 — the phase is a PURE ANNOTATION. Structurally: it is a free
// function that returns strings and a summary, so the only way it could change
// a response is by mutating the rows it was handed. It does not.
func TestRelevanceBands_IsAPureAnnotation(t *testing.T) {
	rows := []activation.ScoredEngram{
		scored(0.9), scored(0.14), row(0.02, activation.AdmissionTagFilter), row(0, activation.AdmissionInjected),
	}
	snapshot := make([]activation.ScoredEngram, len(rows))
	copy(snapshot, rows)

	applyRelevanceBands(rows, defaultCal, defaultGate, false)

	if !reflect.DeepEqual(rows, snapshot) {
		t.Fatal("applyRelevanceBands mutated its input: it must not change any score, " +
			"order, component or annotation — ranking, the abstention gate and every " +
			"threshold are untouched by this increment BY CONSTRUCTION")
	}
	if len(rows) != len(snapshot) {
		t.Fatal("the phase removed or added rows")
	}
}
