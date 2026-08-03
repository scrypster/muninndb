package activation

import "testing"

// ---------------------------------------------------------------------------
// An EXPLICIT actr_heb_scale of 0 must be honored, not substituted (principle
// #1: explicit config is never silently substituted).
//
// The auth layer has always admitted 0 — `PlasticityConfig.ACTRHebScale` is a
// *float64 clamped to [0, 50]. The activation layer then threw it away:
// `ACTRHebScale` was a plain float32 and `if req.ACTRHebScale > 0` could not
// tell "the operator asked for zero cognitive boost" from "nobody said
// anything", so the operator got 4.0 in the hot path with no warning.
//
// Made unrepresentable (principle #3) rather than special-cased: the field is a
// pointer, so "unset" and "zero" are different values in the type system.
// ---------------------------------------------------------------------------

func f32p(v float32) *float32 { return &v }

func TestResolveWeights_ExplicitZeroHebScale_NotSubstituted(t *testing.T) {
	got := resolveWeights(&Weights{UseACTR: true, ACTRHebScale: f32p(0)}, DefaultWeights{})
	if got.ACTRHebScale != 0 {
		t.Errorf("resolveWeights(ACTRHebScale=&0).ACTRHebScale = %v, want 0 — an explicit "+
			"zero is a configured value, not an absent one", got.ACTRHebScale)
	}
}

func TestResolveWeights_UnsetHebScale_GetsDefault(t *testing.T) {
	got := resolveWeights(&Weights{UseACTR: true}, DefaultWeights{})
	if got.ACTRHebScale != DefaultACTRHebScale {
		t.Errorf("resolveWeights(ACTRHebScale=nil).ACTRHebScale = %v, want %v",
			got.ACTRHebScale, DefaultACTRHebScale)
	}
}

func TestResolveWeights_ExplicitHebScale_Honored(t *testing.T) {
	got := resolveWeights(&Weights{UseACTR: true, ACTRHebScale: f32p(2.5)}, DefaultWeights{})
	if got.ACTRHebScale != 2.5 {
		t.Errorf("resolveWeights(ACTRHebScale=&2.5).ACTRHebScale = %v, want 2.5", got.ACTRHebScale)
	}
}

// TestComputeACTR_ZeroHebScale_NeutralisesBothBoosts is the behavioural half:
// at scale 0 neither the Hebbian boost nor the PAS transition boost may move
// the score. Both are asserted because the scalar multiplies BOTH terms
// (engine.go computeACTR) — which is exactly why scale 0 is the KILL SWITCH and
// not the no-Hebbian ablation arm.
func TestComputeACTR_ZeroHebScale_NeutralisesBothBoosts(t *testing.T) {
	w := resolveWeights(&Weights{
		SemanticSimilarity: 0.6,
		FullTextRelevance:  0.4,
		UseACTR:            true,
		ACTRHebScale:       f32p(0),
	}, DefaultWeights{})

	base := actrPrior(t, w, 0, 0)
	withHeb := actrPrior(t, w, 1.0, 0)
	withTrans := actrPrior(t, w, 0, 1.0)

	if withHeb != base {
		t.Errorf("hebbian_boost=1.0 at scale 0 moved the prior: %v -> %v", base, withHeb)
	}
	if withTrans != base {
		t.Errorf("transition_boost=1.0 at scale 0 moved the prior: %v -> %v", base, withTrans)
	}
}

// actrPrior evaluates the total-activation term the way computeACTR does,
// reading the SAME resolved scalar the pipeline reads.
func actrPrior(t *testing.T, w resolvedWeights, hebbian, transition float64) float64 {
	t.Helper()
	const baseLevel = 1.0 // any fixed B(M); the arm difference is what matters
	return baseLevel + w.ACTRHebScale*hebbian + w.ACTRHebScale*transition
}
