package scoring

// CHARACTERIZATION TESTS for the per-vault learned scoring weights.
//
// These tests do not assert that the package is CORRECT. They assert what it
// currently DOES, because what it currently does is: nothing that can affect
// retrieval, and nothing that can move the weight distribution. Each test says
// in its failure message what a failure would mean — for most of them, a
// failure is GOOD NEWS and the test should be deleted along with the defect it
// documented.
//
// Three independent facts, each pinned below:
//
//	(1) Both production call sites pass a CONSTANT ScoreVector.
//	    internal/engine/engine.go:2101 and :4370 both set
//	    ScoreVector: scoring.DefaultWeights() — a uniform 1/6 vector — where
//	    FeedbackSignal's own doc comment says the field carries "the score
//	    components that produced this result" (weights.go:31). A constant
//	    gradient is not feedback.
//
//	(2) With a uniform ScoreVector the update is a mathematical no-op.
//	    Update adds lr*direction*ScoreVector[i] to every dimension — the SAME
//	    scalar to each — and Softmax is shift-invariant. The distribution is
//	    therefore provably unable to move, in either direction, for any number
//	    of updates. See TestUpdateWithUniformScoreVectorCannotMoveWeights.
//
//	(3) Update re-Softmaxes an already-normalised vector, which contracts it
//	    toward uniform. Even a genuine, informative gradient would be partly
//	    undone on every step. See TestRepeatedSoftmaxContractsTowardUniform.
//
// And the consequence: nothing reads the result. Store.Get has no caller
// outside this package's own tests, and no dimension index (DimFTS, DimHNSW,
// DimHebbian, DimDecay, DimRecency, DimAssociation) is referenced anywhere in
// the tree outside internal/scoring. The live recall path scores with a
// completely separate, hardcoded set of coefficients — see the trace in
// .claude/deep-review/2026-07-30-scoring-weights-trace.md.
//
// No real vault content, concept, tag, entity or vault name appears here.

import (
	"math"
	"testing"
	"time"
)

func maxAbsDiff(a, b [NumDims]float64) float64 {
	worst := 0.0
	for i := 0; i < NumDims; i++ {
		if d := math.Abs(a[i] - b[i]); d > worst {
			worst = d
		}
	}
	return worst
}

// TestDefaultWeightsIsUniform pins the value that both production call sites
// pass as their "score vector".
func TestDefaultWeightsIsUniform(t *testing.T) {
	w := DefaultWeights()
	want := 1.0 / float64(NumDims)
	for i, got := range w {
		if math.Abs(got-want) > 1e-12 {
			t.Fatalf("DefaultWeights()[%d] = %.17f, want uniform %.17f", i, got, want)
		}
	}
}

// TestUpdateWithUniformScoreVectorCannotMoveWeights is the core finding.
//
// It replays exactly what production does: FeedbackSignal.ScoreVector =
// DefaultWeights(), applied many times, positively and negatively. The weight
// distribution does not move — not slightly, not slowly, not at all.
//
// IF THIS TEST FAILS, someone wired a real per-result score vector into
// RecordFeedback's call sites or changed Update's normalization. That is the
// desired outcome; delete this test and pin the new behaviour instead.
func TestUpdateWithUniformScoreVectorCannotMoveWeights(t *testing.T) {
	for _, tc := range []struct {
		name     string
		accessed bool
	}{
		{"all positive feedback", true},
		{"all negative feedback", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			vw := &VaultWeights{Weights: DefaultWeights(), LearningRate: 0.1}
			before := vw.Weights

			for i := 0; i < 500; i++ {
				vw.Update(FeedbackSignal{
					Accessed:    tc.accessed,
					ScoreVector: DefaultWeights(), // <- what engine.go actually sends
					Timestamp:   time.Unix(int64(i), 0),
				})
			}

			if d := maxAbsDiff(before, vw.Weights); d > 1e-12 {
				t.Fatalf("weights MOVED by %.3e after 500 updates — the learning loop is no longer inert.\n"+
					"That is good news: delete this characterization test and pin the real behaviour.\n"+
					"before=%v after=%v", d, before, vw.Weights)
			}
			if vw.UpdateCount != 500 {
				t.Errorf("UpdateCount = %d, want 500 (the bookkeeping runs; only the learning does not)", vw.UpdateCount)
			}
		})
	}
}

// TestUpdateIsShiftInvariantForAnyConstantScoreVector generalises (2): the
// no-op is not an artifact of the value 1/6. ANY constant vector — every
// dimension equal — produces the same shift on every weight, and Softmax
// cancels it. The defect is the constancy, not the magnitude.
func TestUpdateIsShiftInvariantForAnyConstantScoreVector(t *testing.T) {
	for _, c := range []float64{0.01, 1.0 / NumDims, 0.5, 5.0} {
		var sv [NumDims]float64
		for i := range sv {
			sv[i] = c
		}
		vw := &VaultWeights{Weights: DefaultWeights(), LearningRate: 0.1}
		before := vw.Weights
		for i := 0; i < 50; i++ {
			vw.Update(FeedbackSignal{Accessed: true, ScoreVector: sv, Timestamp: time.Unix(int64(i), 0)})
		}
		if d := maxAbsDiff(before, vw.Weights); d > 1e-12 {
			t.Errorf("constant ScoreVector %.3f moved the weights by %.3e; expected exact invariance", c, d)
		}
	}
}

// TestUpdateWithAnInformativeScoreVectorDoesMove is the contrast case, and the
// proof that Update itself is not broken — only its inputs are. Give it a
// genuinely non-uniform score vector (what the field's doc comment asks for)
// and the distribution moves in the expected direction.
func TestUpdateWithAnInformativeScoreVectorDoesMove(t *testing.T) {
	var sv [NumDims]float64
	sv[DimHNSW] = 0.9 // this result was carried by semantic similarity
	sv[DimFTS] = 0.1

	vw := &VaultWeights{Weights: DefaultWeights(), LearningRate: 0.1}
	before := vw.Weights
	for i := 0; i < 50; i++ {
		vw.Update(FeedbackSignal{Accessed: true, ScoreVector: sv, Timestamp: time.Unix(int64(i), 0)})
	}
	if vw.Weights[DimHNSW] <= before[DimHNSW] {
		t.Fatalf("informative positive feedback on DimHNSW did not raise it: %.6f -> %.6f",
			before[DimHNSW], vw.Weights[DimHNSW])
	}
	if vw.Weights[DimHNSW] <= vw.Weights[DimDecay] {
		t.Errorf("DimHNSW (%.6f) should outrank the untouched DimDecay (%.6f) after 50 positive signals",
			vw.Weights[DimHNSW], vw.Weights[DimDecay])
	}
	t.Logf("with a real score vector the loop DOES learn: DimHNSW %.4f -> %.4f (DimDecay %.4f)",
		before[DimHNSW], vw.Weights[DimHNSW], vw.Weights[DimDecay])
}

// TestRepeatedSoftmaxContractsTowardUniform pins fact (3). Update applies
// Softmax to a vector that is already a probability distribution. Softmax of a
// distribution is much flatter than the distribution, so every step erodes
// whatever was learned on previous steps — an informative gradient is fighting
// its own normalization.
func TestRepeatedSoftmaxContractsTowardUniform(t *testing.T) {
	spread := func(w [NumDims]float64) float64 {
		lo, hi := w[0], w[0]
		for _, v := range w {
			if v < lo {
				lo = v
			}
			if v > hi {
				hi = v
			}
		}
		return hi - lo
	}

	w := Softmax([NumDims]float64{3, 0, 0, 0, 0, 0}) // a strongly peaked distribution
	first := spread(w)
	prev := first
	for i := 0; i < 10; i++ {
		w = Softmax(w)
		s := spread(w)
		if s >= prev {
			t.Fatalf("iteration %d: spread did not shrink (%.6f -> %.6f) — Softmax is no longer contracting", i, prev, s)
		}
		prev = s
	}
	if prev > first/10 {
		t.Errorf("after 10 re-normalizations the spread is %.6f, only %.1fx smaller than the initial %.6f; "+
			"expected strong contraction toward uniform", prev, first/prev, first)
	}
	t.Logf("re-Softmax contraction: initial spread %.6f -> %.6f after 10 applications", first, prev)
}

// TestUpdateWithZeroSignalStillContracts is the same defect observed through
// the public API rather than through Softmax directly: a feedback signal that
// carries NO information (an all-zero score vector — the gradient is exactly
// zero) still flattens the learned weights, purely from the re-normalization.
// Learning decays even when nothing happens.
func TestUpdateWithZeroSignalStillContracts(t *testing.T) {
	vw := &VaultWeights{Weights: Softmax([NumDims]float64{3, 0, 0, 0, 0, 0}), LearningRate: 0.1}
	peakBefore := vw.Weights[0]

	for i := 0; i < 20; i++ {
		vw.Update(FeedbackSignal{Accessed: true, Timestamp: time.Unix(int64(i), 0)}) // ScoreVector is the zero value
	}

	if vw.Weights[0] >= peakBefore {
		t.Fatalf("a zero-information signal did not erode the learned peak (%.6f -> %.6f) — "+
			"if this now holds, Update stopped re-normalizing an already-normalized vector, which is the fix",
			peakBefore, vw.Weights[0])
	}
	t.Logf("20 zero-information updates eroded the learned peak from %.4f to %.4f", peakBefore, vw.Weights[0])
}
