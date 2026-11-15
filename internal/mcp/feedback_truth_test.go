package mcp

import (
	"strings"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/auth"
	"github.com/scrypster/muninndb/internal/scoring"
)

// feedbackWeightUpdateIsInert replays exactly what Engine.RecordFeedback sends
// to the scoring store (ScoreVector = scoring.DefaultWeights()) and reports
// whether the learned weights stay put. It is the same fact
// internal/scoring/deadloop_characterization_test.go pins, recomputed HERE so
// this test is coupled to the mechanism rather than to a comment.
func feedbackWeightUpdateIsInert() bool {
	for _, accessed := range []bool{true, false} {
		vw := &scoring.VaultWeights{Weights: scoring.DefaultWeights(), LearningRate: 0.1}
		before := vw.Weights
		for i := 0; i < 200; i++ {
			vw.Update(scoring.FeedbackSignal{Accessed: accessed, ScoreVector: scoring.DefaultWeights(), Timestamp: time.Unix(int64(i), 0)})
		}
		for d := range before {
			diff := before[d] - vw.Weights[d]
			if diff > 1e-12 || diff < -1e-12 {
				return false
			}
		}
	}
	return true
}

// weightLearningClaims are phrasings that tell an agent its feedback trains
// ranking. While the update is inert, any of them is a false statement.
var weightLearningClaims = []string{"sgd", "scoring weight", "learned weight", "learns", "adjusts ranking", "improves ranking", "trains"}

// TestFeedback_DescriptionMakesNoWeightLearningClaimWhileTheUpdateIsInert:
// muninn_feedback's description said it "updates the vault's learned scoring
// weights via SGD". The update is a mathematical no-op and nothing outside
// internal/scoring reads the weights. If someone makes the update real, the
// precondition below flips and this test tells them to re-pin it.
func TestFeedback_DescriptionMakesNoWeightLearningClaimWhileTheUpdateIsInert(t *testing.T) {
	if !feedbackWeightUpdateIsInert() {
		t.Fatal("the feedback weight update now MOVES the weights — the no-op this test was written against is gone. " +
			"Re-pin muninn_feedback's description (and the guide) to what feedback now does, then update this test.")
	}
	var desc string
	for _, td := range allToolDefinitions() {
		if td.Name == "muninn_feedback" {
			desc = td.Description
		}
	}
	if desc == "" {
		t.Fatal("muninn_feedback not found")
	}
	surfaces := map[string]string{
		"tool description": desc,
		"guide":            generateGuide("railway", auth.ResolvePlasticity(nil), engineStats{}, ""),
	}
	for name, text := range surfaces {
		lower := strings.ToLower(text)
		if name == "guide" {
			// The guide is long; only its feedback statements are in scope.
			lower = feedbackSentences(lower)
		}
		for _, claim := range weightLearningClaims {
			if strings.Contains(lower, claim) {
				t.Errorf("%s claims weight learning (%q) while the update is inert:\n%s", name, claim, lower)
			}
		}
	}
	// What feedback DOES do must be stated: useful=true counts as an access,
	// useful=false changes nothing.
	lower := strings.ToLower(desc)
	if !strings.Contains(lower, "access") || !strings.Contains(lower, "useful=false") {
		t.Errorf("description must say useful=true counts as an access and what useful=false does: %q", desc)
	}
}

// feedbackSentences returns the guide's lines that mention feedback.
func feedbackSentences(guide string) string {
	var out []string
	for _, line := range strings.Split(guide, "\n") {
		if strings.Contains(line, "feedback") {
			out = append(out, line)
		}
	}
	return strings.Join(out, "\n")
}
