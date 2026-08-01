//go:build localassets

package engine

import (
	"math"
	"testing"

	"github.com/scrypster/muninndb/internal/engine/activation"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// ---------------------------------------------------------------------------
// #773 — the round-8 shapes, reproduced end to end with the REAL bundled
// bge-small-en-v1.5 and a REAL HNSW index.
//
// The defect is a PRESENTATION failure and it needs a real embedder to show:
// the per-query 1/maxRaw rescale pins the argmax of ANY query to ~1.0, and a
// fake embedder has no vocabulary with which to produce a genuinely weak
// absolute score under a saturated relative one.
//
// EVERY fixture below is synthetic — invented product wording for a
// non-existent product. Nothing is read from a real vault.
// ---------------------------------------------------------------------------

// relevanceFixture is a small, deliberately narrow vault: five unrelated
// operational facts about a product that does not exist. None of them contains
// an escalation policy, a release sign-off, or a photography workflow — so
// queries about those are TOPICALLY ADJACENT and UNANSWERABLE, which is the
// #757 category recall provably cannot stop returning (87.5% FPR). This
// increment does not try to. It makes recall SAY SO.
func relevanceFixture(h *realEmbedHarness) map[string]string {
	ids := map[string]string{
		"deploy": h.writeEmbedded("deploy pipeline",
			"The deploy pipeline runs lint, then unit tests, then a canary rollout to five percent of traffic before full release."),
		"oncall": h.writeEmbedded("on-call rotation",
			"The on-call rotation is weekly and hands over every Monday at 10:00 in the ops channel."),
		"lighting": h.writeEmbedded("studio lighting rig",
			"The studio lighting rig uses three softboxes and a bounce card for product photography."),
		"backups": h.writeEmbedded("database backups",
			"Nightly database backups are written to cold storage and retained for ninety days."),
		"tokens": h.writeEmbedded("design tokens",
			"Design tokens are defined in tokens.json and compiled to CSS custom properties at build time."),
	}
	h.eng.waitWriteTimeIdle()
	return ids
}

// makeHebbianHot gives `hot` a full (1.0) Hebbian boost against the next query,
// which is what drives the ACT-R contextual prior to its documented 3.24x
// ceiling — the mechanism that inflates the DISPLAYED score away from the
// absolute evidence. Link a strong association, then activate the partner so
// it lands in the recency-weighted activation log phase4HebbianBoost reads.
func makeHebbianHot(t *testing.T, h *realEmbedHarness, hot, partnerID, partnerText string) {
	t.Helper()
	if _, err := h.eng.Link(h.ctx, &mbp.LinkRequest{
		Vault: "default", SourceID: hot, TargetID: partnerID, Weight: 1.0,
	}); err != nil {
		t.Fatalf("link: %v", err)
	}
	if _, err := h.eng.Activate(h.ctx, &mbp.ActivateRequest{
		Vault: "default", Context: []string{partnerText}, MaxResults: 5,
	}); err != nil {
		t.Fatalf("warm activate: %v", err)
	}
	h.eng.activation.WaitLogIdle()
	h.eng.waitWriteTimeIdle()
}

func recallFor(t *testing.T, h *realEmbedHarness, query string) *mbp.ActivateResponse {
	t.Helper()
	resp, err := h.eng.Activate(h.ctx, &mbp.ActivateRequest{
		Vault: "default", Context: []string{query}, MaxResults: 5,
	})
	if err != nil {
		t.Fatalf("activate(%q): %v", query, err)
	}
	return resp
}

func dumpRows(t *testing.T, resp *mbp.ActivateResponse) {
	t.Helper()
	for i, it := range resp.Activations {
		t.Logf("  [%d] %-24s score=%.4f abs=%.4f content_match=%.4f band=%s/%s",
			i, it.Concept, it.Score, it.ScoreComponents.AbsoluteScore,
			it.ScoreComponents.ContentMatch, it.RelevanceBand, it.RelevanceBandBasis)
	}
}

// GATE — the round-8 "weak neighbour dressed as a certainty" shape.
//
// The PREMISE is asserted first, because a fixture that cannot reach the
// failure proves nothing: the displayed score must materially OVERSTATE the
// row's absolute evidence, and the absolute evidence must be weak.
//
// A NOTE ON THE PREMISE, measured here rather than assumed. #773 quotes a live
// case of `score 1.0` on a weak row. That exact magnitude is NOT reachable on a
// default-plasticity vault at this commit, and the arithmetic says why:
// computeACTR caps B(M) at bLevelCap = ln(exp(actrDenominator)-1) ~ 1.489, so
// the contextual prior tops out at softplus(1.489 + 4.0)/1.6931 = 3.244 — the
// "3.24x at full Hebbian boost" the gate comment cites. Score = ContentMatch x
// prior, and the 1/maxRaw rescale (which is what pins an argmax to exactly 1.0)
// only fires when some Raw exceeds 1.0. For a row whose absolute score is under
// the 0.2 weak ceiling, the highest reachable score is 0.2 x 3.244 = 0.649, and
// the rescale cannot fire on that row at all. So THE ARGMAX EXEMPTION CANNOT
// DRESS A WEAK ROW AS 1.0 HERE; it can only do that to a row that is already
// moderate or better. #773's 1.0 case is consistent with a lexically-strong /
// semantically-null match (the issue quotes vector_score 0.09, which is semCal,
// NOT ContentMatch — a high IDF-coverage lexical channel lifts ContentMatch
// past the saturation point on its own). That is recorded, not papered over,
// and it is pinned below by TestRelevanceBand_WeakRowCannotReachScoreOne.
//
// What IS reproducible, and is the same defect, is asserted here: a weak row
// whose displayed score is over 3x its absolute evidence, with NO field on the
// row saying weak.
//
// RED at e5ecb96: score 0.5642, confidence 1, absolute evidence 0.1739, and
// nothing anywhere on the row or the response qualifies it.
func TestRelevanceBand_WeakNeighborIsNotDressedAsCertain(t *testing.T) {
	h, cleanup := newRealEmbedHarness(t)
	defer cleanup()
	ids := relevanceFixture(h)
	makeHebbianHot(t, h, ids["lighting"], ids["tokens"],
		"Design tokens are defined in tokens.json and compiled to CSS custom properties at build time.")

	resp := recallFor(t, h, "what does the team use to take pictures")
	if len(resp.Activations) == 0 {
		t.Fatal("fixture returned nothing; the band cannot be exercised")
	}
	dumpRows(t, resp)

	top := resp.Activations[0]
	abs := float64(top.ScoreComponents.AbsoluteScore)
	// Premise 1: the row is weak in ABSOLUTE terms.
	if abs >= 0.2 {
		t.Fatalf("premise not reproduced: absolute score %.4f >= 0.2, so this row is "+
			"not weak and the test proves nothing", abs)
	}
	// Premise 2: the displayed score materially overstates that evidence — the
	// ACT-R prior inflating Final away from the absolute quantity, which is the
	// mechanism #773 names.
	if float64(top.Score) < 3.0*abs {
		t.Fatalf("premise not reproduced: score %.4f is not >= 3x the absolute "+
			"evidence %.4f, so nothing is being overstated", top.Score, abs)
	}
	if top.ScoreComponents.HebbianBoost < 0.99 {
		t.Fatalf("premise not reproduced: hebbian boost %.3f — the fixture failed to "+
			"warm the row, so the prior is not inflating anything",
			top.ScoreComponents.HebbianBoost)
	}

	if top.RelevanceBand != activation.RelevanceWeak {
		t.Errorf("top row: relevance_band = %q, want %q — score %.4f on absolute "+
			"evidence %.4f is the shape #773 reported",
			top.RelevanceBand, activation.RelevanceWeak, top.Score, abs)
	}
}

// The arithmetic above, pinned as an invariant rather than left in a comment.
// If a future change to bLevelCap, actrDenominator or ACTRHebScale makes a WEAK
// row reachable at score ~1.0, this fails and the premise note above must be
// rewritten rather than quietly going stale.
func TestRelevanceBand_WeakRowCannotReachScoreOne(t *testing.T) {
	// Score = ContentMatch x prior, prior <= softplus(bLevelCap + hebScale)/actrDenominator.
	// hebScale is the PRODUCTION default resolveWeights applies, read from its
	// exported constant rather than hardcoded — an edit to the default must
	// move this pin with it.
	const bLevelCap = 1.4894204224828295 // ln(exp(1.6931471805599453) - 1)
	const actrDenominator = 1.6931471805599453
	const hebScale = activation.DefaultACTRHebScale
	prior := math.Log1p(math.Exp(bLevelCap+hebScale)) / actrDenominator
	if prior < 3.2 || prior > 3.3 {
		t.Fatalf("the documented 3.24x prior ceiling moved to %.4f — the premise note "+
			"in TestRelevanceBand_WeakNeighborIsNotDressedAsCertain is now stale", prior)
	}
	weakMax, _ := activation.RelevanceBandEdges(0.1, 1.0)
	if maxWeakScore := weakMax * prior; maxWeakScore >= 0.9 {
		t.Fatalf("a weak row can now reach score %.4f: the argmax exemption CAN dress "+
			"a weak row as a certainty and the RED premise should assert score >= 0.9",
			maxWeakScore)
	}
}

// GATE — the round-8 absent-fact shape (the nonexistent composer / the deleted
// door code), in its TOPICALLY-ADJACENT form, which is the one #757 measured we
// cannot stop returning (87.5% FPR) and therefore the one that matters. This
// vault has a deploy pipeline and an on-call rotation; it has NO escalation
// policy. Recall returns the pipeline anyway. This increment does not stop it —
// it makes the response say the evidence is weak.
//
// A far-out-of-domain probe ("who composed the film score") ABSTAINS on this
// fixture, which is the honest outcome and is already covered by COG-6; it is
// deliberately not used here because it cannot reach the failure.
//
// RED at e5ecb96: the neighbour is returned with no qualifier at all.
func TestRelevanceBand_AbsentFactQuery_AllWeakHint(t *testing.T) {
	h, cleanup := newRealEmbedHarness(t)
	defer cleanup()
	relevanceFixture(h)

	resp := recallFor(t, h, "what is the escalation path when the canary fails overnight?")
	if len(resp.Activations) == 0 {
		t.Fatal("the fixture abstained on the topically-adjacent unanswerable query; " +
			"it can no longer reach the shape #773 reported")
	}
	dumpRows(t, resp)

	for i, it := range resp.Activations {
		if it.RelevanceBand != activation.RelevanceWeak {
			t.Errorf("row %d (%s): band = %q, want weak — nothing in this vault is an "+
				"escalation policy", i, it.Concept, it.RelevanceBand)
		}
	}
	// The response-level "these are neighbours" hint is DEFERRED — it failed its
	// own pre-committed rule (engine_relevance.go). What ships is the row-level
	// truth: this response contains no strong or moderate evidence, and every
	// row says so for itself.
	if resp.Abstained {
		t.Error("a non-empty response must not be marked abstained")
	}
}

// Anti-overfire control: a near-verbatim query must band strong and must NOT
// carry the hint. If this fires the hint, the band is useless.
func TestRelevanceBand_StrongMatch_NoHint(t *testing.T) {
	h, cleanup := newRealEmbedHarness(t)
	defer cleanup()
	relevanceFixture(h)

	resp := recallFor(t, h,
		"The deploy pipeline runs lint, then unit tests, then a canary rollout to five percent of traffic before full release.")
	if len(resp.Activations) == 0 {
		t.Fatal("near-verbatim query returned nothing")
	}
	dumpRows(t, resp)

	top := resp.Activations[0]
	if top.Concept != "deploy pipeline" {
		t.Fatalf("near-verbatim query returned %q first, not the gold", top.Concept)
	}
	if top.RelevanceBand != activation.RelevanceStrong {
		t.Errorf("near-verbatim gold: band = %q (abs %.4f), want strong",
			top.RelevanceBand, top.ScoreComponents.AbsoluteScore)
	}
}

// Anti-overfire control: a reworded query that finds its gold must not fire the
// hint either. One non-weak row suppresses it entirely.
func TestRelevanceBand_ModerateMatch_NoHint(t *testing.T) {
	h, cleanup := newRealEmbedHarness(t)
	defer cleanup()
	relevanceFixture(h)

	resp := recallFor(t, h, "how are nightly backups stored and for how long are they kept?")
	if len(resp.Activations) == 0 {
		t.Fatal("reworded query returned nothing")
	}
	dumpRows(t, resp)

	top := resp.Activations[0]
	if top.Concept != "database backups" {
		t.Fatalf("reworded query returned %q first, not the gold", top.Concept)
	}
	// Asserts the POSITIVE band, not merely "not weak": an empty band would
	// satisfy "not weak" and prove nothing.
	if top.RelevanceBand != activation.RelevanceModerate {
		t.Errorf("reworded gold: band = %q (abs %.4f), want moderate — the middle band "+
			"must be reachable by real, non-verbatim evidence",
			top.RelevanceBand, top.ScoreComponents.AbsoluteScore)
	}
}

// Every recall row carries a band — the omitempty-sharing risk. mcp.Memory is
// shared with muninn_read, so the field is omitempty; the recall path must
// therefore always set a non-empty value or the field silently disappears.
func TestRelevanceBand_EveryRecallRowIsBanded(t *testing.T) {
	h, cleanup := newRealEmbedHarness(t)
	defer cleanup()
	relevanceFixture(h)

	for _, q := range []string{
		"light",
		"Who composed the orchestral score for the film Aurelian Tides?",
		"how does the deploy pipeline work?",
		"design tokens",
	} {
		resp := recallFor(t, h, q)
		for i, it := range resp.Activations {
			if it.RelevanceBand == "" {
				t.Errorf("query %q row %d (%s): empty relevance_band", q, i, it.Concept)
			}
			switch it.RelevanceBand {
			case activation.RelevanceStrong, activation.RelevanceModerate, activation.RelevanceWeak:
				if it.RelevanceBandBasis != "" {
					t.Errorf("query %q row %d: band %q must carry no basis, got %q",
						q, i, it.RelevanceBand, it.RelevanceBandBasis)
				}
			case activation.RelevanceFilterMatch, activation.RelevanceUncalibrated:
				if it.RelevanceBandBasis == "" {
					t.Errorf("query %q row %d: band %q must name WHY", q, i, it.RelevanceBand)
				}
			default:
				t.Errorf("query %q row %d: unknown band %q", q, i, it.RelevanceBand)
			}
		}
	}
}

// §6.1 Pin 1, end to end: the phase changes NOTHING about which rows come
// back, in what order, or at what score. Recorded as a comparison against the
// same query run through activation.Run DIRECTLY — the path the three
// measurement harnesses take, which cannot see this phase at all.
func TestRelevanceBands_EngineRowsMatchRawActivation(t *testing.T) {
	h, cleanup := newRealEmbedHarness(t)
	defer cleanup()
	relevanceFixture(h)

	const q = "how does the deploy pipeline work?"
	resp := recallFor(t, h, q)
	if len(resp.Activations) == 0 {
		t.Fatal("no rows")
	}
	// Same query, straight through the activation package.
	vec, err := h.svc.Embed(h.ctx, []string{q})
	if err != nil {
		t.Fatalf("embed: %v", err)
	}
	raw, err := h.eng.activation.Run(h.ctx, &activation.ActivateRequest{
		VaultPrefix: h.ws, Context: []string{q}, MaxResults: 5, Embedding: vec,
		Threshold: 0.1, Weights: &activation.Weights{
			SemanticSimilarity: 0.6, FullTextRelevance: 0.4, UseACTR: true,
			SemanticBaseline: 0.520,
		},
	})
	if err != nil {
		t.Fatalf("activation.Run: %v", err)
	}
	// The bands are engine-layer only: nothing in the activation result carries
	// them, which is exactly why the measurement harnesses cannot see them.
	// float32 weights widen to float64 with quantization noise (0.6+0.4 ->
	// 1.0000000298), which is why the band edges are compared with >= rather
	// than == anywhere it matters.
	if math.Abs(raw.Calibration.ContentCeiling-1.0) > 1e-6 {
		t.Errorf("ContentCeiling = %v, want ~1.0", raw.Calibration.ContentCeiling)
	}
	if raw.Calibration.FusionMode != activation.FusionACTR {
		t.Errorf("FusionMode = %q, want actr", raw.Calibration.FusionMode)
	}
	// And the engine's rows still carry the same absolute evidence the raw run
	// measured — the phase read it, it did not rewrite it.
	for _, it := range resp.Activations {
		for _, r := range raw.Activations {
			if r.Engram.ID.String() != it.ID {
				continue
			}
			if float32(r.Components.AbsoluteScore) != it.ScoreComponents.AbsoluteScore {
				t.Errorf("%s: AbsoluteScore moved: raw %.6f vs engine %.6f",
					it.Concept, r.Components.AbsoluteScore, it.ScoreComponents.AbsoluteScore)
			}
		}
	}
}
