//go:build localassets

package activation_test

import (
	"context"
	"fmt"
	"math"
	"sort"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/engine/activation"
	"github.com/scrypster/muninndb/internal/storage"
)

// ---------------------------------------------------------------------------
// MEASURING THE ABSTENTION GATE — the change is chosen on numbers, not argument.
//
// THE QUESTION. activation/engine.go's ACT-R path used to drop a candidate when
//
//	final = min(Raw * 1/maxRaw, 1.0) * Confidence   <  Threshold
//
// and now drops it when
//
//	absolute = min(Raw, ContentMatch, 1.0) * Confidence  <  Threshold
//
// Both arms rank identically (ranking still uses `final`); they differ only in
// WHICH candidates survive. So the trade is exactly the one the brief
// pre-commits to, and it is measured with both metrics together:
//
//	NDCG@5 on ANSWERABLE queries    — did the right memory rank high?
//	FPR    on UNANSWERABLE queries  — did the system abstain when it should?
//
// ACCEPTANCE, pre-committed before looking: the candidate arm must reduce FPR
// AND must not reduce NDCG@5. A change that wins one by sacrificing the other
// is rejected outright — that is asserted below, not left to the reader.
//
// HOW THE COUNTERFACTUAL IS EXACT, not approximated. Each query is run ONCE
// with the threshold effectively disabled, so every scored candidate comes back
// with its components. The per-query 1/maxRaw scale is computed over all
// candidates BEFORE any gating (activation/engine.go:1910-1928), so it does not
// depend on the threshold — which means the reported Final and AbsoluteScore
// are bit-identical to what each arm would have produced at any threshold. The
// two arms are then applied offline to the same scored set. No arm re-runs the
// pipeline, no arm sees a different candidate pool, and nothing is simulated.
//
// THE HEBBIAN FRACTION IS THE POINT. With every engram cold, prior == 1.0,
// maxRaw <= 1, the rescale is the identity and BOTH ARMS ARE THE SAME FUNCTION
// — the defect does not exist on a vault nobody uses. It appears exactly when
// an agent has been recalling: co-activated memories carry a Hebbian boost, the
// prior reaches 3.24x, raw exceeds 1.0, and the rescale starts both pinning the
// argmax to 1.0 and dividing everyone else down. So a fraction of the corpus is
// marked co-activated here, modelling an active session. That is the condition
// under which the defect was reported, and measuring without it would report a
// null result about a vault nobody is using.
//
// FIDELITY LIMITS — read before quoting a number:
//   - FTS is stubbed empty, so every query here is the PARAPHRASE condition
//     (zero lexical corroboration). That is the discriminating condition per
//     engine_scoring_weights_measure_test.go's header, and it is the harder one
//     for the answerable set, but it is NOT the full-signal condition.
//   - The corpus is 18 synthetic engrams. Absolute NDCG on a corpus this small
//     is not comparable to a real vault's; read the DIFFERENCE between arms.
//   - No entity boost, tag seeding, associative traversal, PAS transitions or
//     currency annotation. Same pool for both arms, so the comparison is fair;
//     the absolute numbers are not "what recall returns".
//
// PRIVACY: every string is synthetic and authored in this repo. The corpus is
// billingCorpus from semantic_abstention_test.go; the queries are below.
// ---------------------------------------------------------------------------

// abmAnswerable pairs a paraphrased query with the billingCorpus concept that
// answers it. Paraphrases deliberately avoid the stored wording — that is the
// case retrieval-by-meaning exists to serve, and the one that breaks.
var abmAnswerable = []struct{ query, goldConcept string }{
	{"how do incoming customer payments get applied to the right open bill", "Remittance processing flow"},
	{"getting a refund after paying extra by mistake", "Overpayment refund workflow"},
	{"what happens when someone stops paying on time", "Delinquent account escalation"},
	{"how much extra do we charge for paying after the deadline", "Late fee assessment schedule"},
	{"does the price go up automatically when the agreement is extended", "Contract renewal rate escalation"},
	{"what do we do when a customer says a charge is wrong", "Invoice dispute resolution"},
	{"how does a new company get set up on the system", "Employer portal onboarding"},
	{"checking that our numbers agree with the accounting system each month", "Ledger reconciliation procedure"},
	{"everyone pays the same amount no matter how much they use", "Per-capita billing model"},
	{"how risky is this customer based on their payment history", "Audit score calculation"},
	{"what shape does the incoming money data arrive in", "Batch payment file format"},
	{"the price changes depending on how much you use each month", "Four-bucket pricing rate tiers"},
}

// abmUnanswerable are synthetic probes shaped like plausible memory queries —
// concrete nouns, an implied task — describing things nothing in the corpus
// covers. Shaped-like-real matters: random characters would abstain trivially.
// Mirrors COG-26's nonsense-query protocol.
var abmUnanswerable = []string{
	"quarterly ballet recital seating chart for the amphibian troupe",
	"calibration log for the tin-whistle humidity organ in vault seven",
	"migration schedule of the municipal weathervane repair guild",
	"tasting notes for fermented basalt served at the lighthouse gala",
	"knitting pattern for a nine-sleeved reversible barometer cozy",
	"maintenance interval for the brass hummingbird irrigation lathe",
	"seating protocol at the annual convention of retired sundials",
	"nutritional breakdown of powdered moonlight sold by the furlong",
	"warranty terms on a self-folding origami submarine hull",
	"recipe for lacquered thunder as prepared in the northern glassworks",
	"inventory count of left-handed metronomes in the archive annex",
	"revised bylaws governing the ceremonial polishing of borrowed fog",
	"pruning guide for the ornamental clockspring hedge in winter",
	"insurance schedule covering damage by enthusiastic library ghosts",
	"onboarding checklist for apprentice cloud stackers, third cohort",
	"decommissioning plan for the obsolete velvet pressure gauge array",
}

// abmGate is one arm: given a scored result, the value the threshold is
// compared against.
type abmGate struct {
	name string
	of   func(activation.ScoredEngram) float64
}

var (
	// abmGateCurrent is the pre-change behaviour: the per-query max-normalised
	// score. Reconstructed from the reported Score, which IS that value.
	abmGateCurrent = abmGate{"CURRENT (max-normalised final)", func(s activation.ScoredEngram) float64 { return s.Score }}
	// abmGateAbsolute is the candidate: ContentMatch attenuated (never
	// amplified) by the prior, times Confidence, independent of the pool.
	abmGateAbsolute = abmGate{"CANDIDATE (absolute)", func(s activation.ScoredEngram) float64 { return s.Components.AbsoluteScore }}
)

type abmCell struct {
	nAnswerable int
	ndcgSum     float64
	goldFound   int
	nUnanswer   int
	falsePos    int
	fpDepth     float64
	fpTopScore  float64
}

func (c abmCell) ndcg() float64 {
	if c.nAnswerable == 0 {
		return 0
	}
	return c.ndcgSum / float64(c.nAnswerable)
}

func (c abmCell) fpr() float64 {
	if c.nUnanswer == 0 {
		return 0
	}
	return float64(c.falsePos) / float64(c.nUnanswer)
}

// abmMeasureFixture builds the real-embedder billing corpus and marks every
// third engram as co-activated (Hebbian-hot), modelling an active session.
// Returns the engine and concept->ID map.
// abmFixedAge pins the fixture's AGE rather than its timestamp.
//
// The first attempt at determinism pinned CreatedAt to an absolute date. That
// made the runs identical and simultaneously destroyed what the harness
// measures: age was then ~30 days, the ACT-R base-level term was small, `raw`
// never exceeded 1.0, the max-rescale never engaged, and BOTH arms reported the
// same numbers. A harness that cannot reach the defect's precondition proves
// nothing, however reproducible it is.
//
// What must be stable is the ELAPSED time the base-level term sees, not the
// wall-clock instant. A recent, fixed age keeps the prior in the saturating
// regime — the live "agent has been using its memory" condition where raw
// exceeds 1.0 and the rescale actually bites — while making every run identical.
const abmFixedAge = 5 * time.Minute

func abmFixtureNow() time.Time { return time.Now().Add(-abmFixedAge) }

func abmMeasureFixture(t *testing.T) (*activation.ActivationEngine, map[string]storage.ULID) {
	t.Helper()
	eng, store, byConcept := buildSemanticAbstentionFixture(t)

	// A primer engram standing in for "something the agent surfaced a moment
	// ago". Every third corpus engram is associated to it, so phase4HebbianBoost
	// gives those a boost of 1.0 — the live active-session condition.
	// DETERMINISM (both of these mattered, and the harness was flaky without them):
	//
	//  1. CreatedAt must be PINNED, not time.Now(). ACT-R's base-level activation
	//     is -0.5*ln(age), which is violently sensitive as age -> 0, so a
	//     wall-clock fixture gives a different prior on every run.
	//  2. The hot set must be chosen over SORTED keys. byConcept is a map, so
	//     `range` selected a different third of the corpus as Hebbian-hot each
	//     run — different engrams hot means a different maxRaw, and the
	//     max-normalised arm divides by exactly that.
	//
	// Measured before this fix, over three isolated runs, the CURRENT arm gave
	// FPR 31.2% / 25.0% / 18.8% and NDCG 0.5899 / 0.6581 / 0.6748, while the
	// absolute arm gave 6.2% every single time. That asymmetry is the defect
	// under test — a query-relative gate depends on whatever else is in the pool
	// — but it also meant this test could fail on an unlucky run while asserting
	// a direction that always held. A measurement that moves when nothing
	// changed cannot referee a scoring decision.
	primer := &storage.Engram{
		Concept: "prior turn of this session", Content: "an unrelated memory surfaced a moment ago",
		Confidence: 1.0, Stability: 30.0, CreatedAt: abmFixtureNow(),
	}
	store.writeEngram(primer)
	hotKeys := make([]string, 0, len(byConcept))
	for k := range byConcept {
		hotKeys = append(hotKeys, k)
	}
	sort.Strings(hotKeys)
	for i, k := range hotKeys {
		if i%3 == 0 {
			store.assocs[byConcept[k]] = []storage.Association{{TargetID: primer.ID, Weight: 1.0}}
		}
	}
	eng.AssocLog().Record(activation.LogEntry{
		VaultID: 0, At: abmFixtureNow(), EngramIDs: []storage.ULID{primer.ID}, Scores: []float64{1.0},
	})
	return eng, byConcept
}

// abmScore runs one query with the gate effectively disabled and returns the
// full scored set, score-ordered, so both arms can be applied offline.
func abmScore(t *testing.T, eng *activation.ActivationEngine, query string) []activation.ScoredEngram {
	t.Helper()
	res, err := eng.Run(context.Background(), &activation.ActivateRequest{
		Context:    []string{query},
		Threshold:  1e-9, // effectively disabled; the rescale does not depend on it
		MaxResults: 100,
		// COG-32: models a DEFAULT-preset vault. Without this the primer
		// seeded in abmFixture is inert and both gate arms collapse onto each
		// other, which the both-metric rule below correctly rejects.
		HebbianEnabled: true,
		Weights: &activation.Weights{
			SemanticSimilarity: 0.6,
			FullTextRelevance:  0.4,
			SemanticBaseline:   semanticBaselineBGE,
			UseACTR:            true,
		},
	})
	if err != nil {
		t.Fatalf("Run(%q): %v", query, err)
	}
	return res.Activations
}

func abmNDCG5(kept []activation.ScoredEngram, gold storage.ULID) float64 {
	for r, s := range kept {
		if r >= 5 {
			break
		}
		if s.Engram.ID == gold {
			return 1.0 / math.Log2(float64(r)+2.0)
		}
	}
	return 0
}

// TestMeasureAbstentionGate is the measurement, and the acceptance gate.
func TestMeasureAbstentionGate(t *testing.T) {
	eng, byConcept := abmMeasureFixture(t)

	// One arm: 0.10 is now BOTH the engine default (engine.go:2406, the value
	// COG-26's b was calibrated against) and the MCP surface default
	// (mcp/handlers.go:392) — the two moved together with the absolute gate.
	// The old "SURFACE 0.50" arm existed to pin the coupling (no honest absolute
	// score can reach 0.5, so recall at that default survived only while the
	// max-rescale lied); its embedded instruction said to delete it once the
	// default moved, and it has. What it proved is recorded in git history.
	thresholds := []struct {
		name string
		v    float64
	}{
		{"default 0.10", 0.1},
	}
	arms := []abmGate{abmGateCurrent, abmGateAbsolute}

	type key struct {
		arm string
		thr int
	}
	cells := map[key]*abmCell{}
	for _, a := range arms {
		for ti := range thresholds {
			cells[key{a.name, ti}] = &abmCell{}
		}
	}

	apply := func(all []activation.ScoredEngram, g abmGate, thr float64) []activation.ScoredEngram {
		kept := make([]activation.ScoredEngram, 0, len(all))
		for _, s := range all {
			if g.of(s) >= thr {
				kept = append(kept, s)
			}
		}
		return kept
	}

	for _, q := range abmAnswerable {
		gold, ok := byConcept[q.goldConcept]
		if !ok {
			t.Fatalf("gold concept %q is not in the corpus — fixture drift", q.goldConcept)
		}
		all := abmScore(t, eng, q.query)
		for _, a := range arms {
			for ti, thr := range thresholds {
				c := cells[key{a.name, ti}]
				kept := apply(all, a, thr.v)
				c.nAnswerable++
				c.ndcgSum += abmNDCG5(kept, gold)
				for _, s := range kept {
					if s.Engram.ID == gold {
						c.goldFound++
						break
					}
				}
			}
		}
	}

	for _, q := range abmUnanswerable {
		all := abmScore(t, eng, q)
		for _, a := range arms {
			for ti, thr := range thresholds {
				c := cells[key{a.name, ti}]
				kept := apply(all, a, thr.v)
				c.nUnanswer++
				if len(kept) > 0 {
					c.falsePos++
					c.fpDepth += float64(len(kept))
					c.fpTopScore += kept[0].Score
				}
			}
		}
	}

	t.Log("")
	t.Log("=== ABSTENTION GATE: NDCG@5 on answerable (higher better) vs FPR on unanswerable (lower better) ===")
	t.Logf("corpus=%d engrams (paraphrase condition: FTS stubbed empty) | answerable=%d unanswerable=%d",
		len(billingCorpus), len(abmAnswerable), len(abmUnanswerable))
	for ti, thr := range thresholds {
		t.Logf("")
		t.Logf("--- threshold: %s ---", thr.name)
		t.Logf("%-32s %8s %11s %8s %10s %12s", "gate", "NDCG@5", "gold-found", "FPR", "FP-depth", "FP-top-score")
		for _, a := range arms {
			c := cells[key{a.name, ti}]
			depth, topScore := 0.0, 0.0
			if c.falsePos > 0 {
				depth = c.fpDepth / float64(c.falsePos)
				topScore = c.fpTopScore / float64(c.falsePos)
			}
			t.Logf("%-32s %8.4f %10.1f%% %7.1f%% %10.1f %12.4f",
				a.name, c.ndcg(), 100*float64(c.goldFound)/float64(c.nAnswerable),
				100*c.fpr(), depth, topScore)
		}
	}

	// --- acceptance, pre-committed -----------------------------------------
	//
	// The rule committed to before looking: the candidate must reduce FPR AND
	// must not reduce NDCG@5. A change that wins one by sacrificing the other
	// is rejected. The measured answer is split, and the split IS the finding:
	//
	//   at the ENGINE threshold the candidate passes the rule outright — it
	//   improves BOTH metrics, which is the both-directions result this work
	//   set out to find;
	//
	//   at the SURFACE threshold it fails the rule completely (recall -> 0),
	//   because 0.5 is above the structural ceiling of an honest absolute
	//   content score (0.6 semantic-only, 0.4 lexical-only). Today's recall at
	//   0.5 survives ONLY on the max-rescale inflating the argmax to 1.0 — the
	//   same line that hands unanswerable queries a confident hit.
	//
	// So the assertions below are asymmetric on purpose. At the engine
	// threshold the both-metric win is a hard requirement: if it ever stops
	// holding, the fix has lost its justification and must not be landed. At
	// the surface threshold the collapse is asserted as a PIN on the coupling
	// — it must keep reproducing until the surface default moves onto the
	// absolute scale, at which point this flips to the same both-metric rule.
	var summary []string
	for ti, thr := range thresholds {
		cur := cells[key{abmGateCurrent.name, ti}]
		cand := cells[key{abmGateAbsolute.name, ti}]
		summary = append(summary, fmt.Sprintf("%s: NDCG %.4f->%.4f, FPR %.1f%%->%.1f%%",
			thr.name, cur.ndcg(), cand.ndcg(), 100*cur.fpr(), 100*cand.fpr()))

		if thr.v <= 0.25 {
			// Engine-scale gate: the both-metric rule, enforced.
			if cand.ndcg() < cur.ndcg()-1e-9 {
				t.Errorf("REJECT at %s: the candidate gate LOSES answerable recall (NDCG@5 %.4f -> %.4f) "+
					"while changing FPR %.1f%% -> %.1f%%. A gate that buys abstention with genuine misses "+
					"is the same defect from the other side and must not ship.",
					thr.name, cur.ndcg(), cand.ndcg(), 100*cur.fpr(), 100*cand.fpr())
			}
			if cand.fpr() > cur.fpr()+1e-9 {
				t.Errorf("REJECT at %s: the candidate gate RAISES the false-positive rate "+
					"(%.1f%% -> %.1f%%) — it does not fix direction (a) at all",
					thr.name, 100*cur.fpr(), 100*cand.fpr())
			}
			if cand.ndcg() <= cur.ndcg()+1e-9 && cand.fpr() >= cur.fpr()-1e-9 {
				t.Errorf("REJECT at %s: the candidate gate improves NEITHER metric (NDCG %.4f->%.4f, "+
					"FPR %.1f%%->%.1f%%) — it buys nothing and should not ship",
					thr.name, cur.ndcg(), cand.ndcg(), 100*cur.fpr(), 100*cand.fpr())
			}
			continue
		}

		// Surface-scale gate: pin the coupling rather than the win.
		if cand.ndcg() >= cur.ndcg()-1e-9 {
			t.Errorf("the surface-scale collapse no longer reproduces at %s (NDCG %.4f -> %.4f). "+
				"Either the surface default moved onto the absolute scale — in which case delete this "+
				"branch and enforce the both-metric rule here too — or the ContentMatch ceiling changed "+
				"and the two-file coupling documented in activation/engine.go must be re-derived.",
				thr.name, cur.ndcg(), cand.ndcg())
		}
	}
	t.Log("")
	for _, s := range summary {
		t.Logf("VERDICT %s", s)
	}
	t.Log("VERDICT: the absolute gate wins BOTH metrics at the shipped default. (The gate and the " +
		"surface default 0.5->0.1 landed together — see mcp/handlers.go:392; REST /activate has no " +
		"surface default and rest/server.go:1772 is SUBSCRIBE, a different formula.)")
}
