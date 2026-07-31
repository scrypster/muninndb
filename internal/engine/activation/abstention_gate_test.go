package activation_test

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/engine/activation"
	"github.com/scrypster/muninndb/internal/storage"
)

// ---------------------------------------------------------------------------
// THE ABSTENTION INVERSION — both directions, one fixture family.
//
// Two independent hands-on evaluations landed on the same complaint: recall
// refuses answerable questions and answers unanswerable ones confidently.
// These tests encode BOTH directions so a fix cannot pass by trading one for
// the other. A change that only tightens the gate breaks the ANSWERABLE
// tests; a change that only loosens it breaks the UNANSWERABLE ones.
//
// Everything here is driven by stubbed cosine / FTS-coverage scores rather
// than a real embedder, on purpose: the diagnosis is arithmetic about a gate,
// and every number below is hand-derived and asserted, so a drift in the
// scoring formula shows up as a specific numeric failure rather than a fuzzy
// "the model moved". Real-embedder end-to-end coverage of the semantic floor
// already exists in semantic_abstention_test.go (localassets-gated); this
// file deliberately costs no assets and no CI minutes.
//
// THE PIPELINE UNDER TEST (activation/engine.go, ACT-R default path):
//
//	semCal       = max(0, (cos-b)/(1-b))                          :2129
//	ContentMatch = 0.6*semCal + 0.4*ftsCoverage                    :2211
//	raw          = ContentMatch * softplus(B+4h+4t)/1.6931         :2273
//	scale        = 1/maxRaw when any candidate's raw exceeds 1.0   :1926
//	final        = min(raw*scale, 1.0) * Confidence                :1930
//	DROP if final < req.Threshold                                  :1934
//
// Threshold ownership is centralized in the ENGINE: the MCP surface forwards
// an omitted threshold as 0 (mcp/handlers.go, like every other transport) and
// the engine's fusion-aware coerce applies ACT-R 0.1 / weighted_sum 0.5 / rrf
// 0.001 (engine.go:2405); activation's own last-resort fallback is 0.05
// (activation/engine.go). All of these gate the SAME quantity, and that
// quantity is not a measure of "is this memory about the query".
//
// PRIVACY: every fixture string is synthetic and authored here.
// ---------------------------------------------------------------------------

const (
	// abBaseline is COG-26's registered anisotropy baseline b for
	// bge-small-en-v1.5 (plugin/embed/baseline.go). Mirrored, not imported,
	// so a registry drift surfaces as a failure here rather than silently
	// re-defining what these tests mean.
	abBaseline = 0.520
	// abRemovedSurfaceDefault is the OLD surface default (0.5), removed when the
	// absolute gate landed and mcp/handlers.go:392 moved to 0.1. Kept only so
	// the arithmetic pins below can state what that world required (a
	// semantic-only match needed raw cosine >= 0.92 to survive it). No caller
	// hits this value any more.
	abRemovedSurfaceDefault = 0.5
	// abEngineThreshold is engine.go:2406's non-RRF default — what a library
	// or direct caller gets, and the value COG-26's b was calibrated against.
	abEngineThreshold = 0.1
)

func abSemCal(cos float64) float64 {
	v := (cos - abBaseline) / (1 - abBaseline)
	if v < 0 {
		return 0
	}
	return v
}

// abFixture builds an activation engine over a synthetic corpus with fully
// controlled semantic and lexical scores.
type abFixture struct {
	eng   *activation.ActivationEngine
	store *stubStore
	ids   map[string]storage.ULID
}

type abDoc struct {
	name    string
	concept string
	content string
	cos     float64 // stubbed raw cosine against the query
	fts     float64 // stubbed calibrated [0,1] FTS coverage against the query
	// hebbian, when true, wires this doc an association to a just-activated
	// engram so phase4HebbianBoost gives it the maximum boost of 1.0.
	hebbian bool
	// age/accessCount drive ACT-R base-level. Zero age means "written just now".
	age         time.Duration
	accessCount uint32
}

func newABFixture(t *testing.T, docs []abDoc) *abFixture {
	t.Helper()
	store := newStubStore()
	ids := make(map[string]storage.ULID, len(docs))
	now := time.Now()

	var hnswHits, ftsHits []activation.ScoredID
	// A "recently activated" engram that exists only to be the far end of a
	// Hebbian association — it is never a candidate itself.
	primer := &storage.Engram{
		Concept:    "prior turn of this session",
		Content:    "an unrelated memory the agent surfaced a moment ago",
		Confidence: 1.0, Stability: 30.0, CreatedAt: now,
	}
	store.writeEngram(primer)

	for _, d := range docs {
		created := now.Add(-d.age)
		e := &storage.Engram{
			Concept:     d.concept,
			Content:     d.content,
			Confidence:  1.0,
			Stability:   30.0,
			CreatedAt:   created,
			LastAccess:  created,
			AccessCount: d.accessCount,
		}
		store.writeEngram(e)
		ids[d.name] = e.ID
		if d.cos > 0 {
			hnswHits = append(hnswHits, activation.ScoredID{ID: e.ID, Score: d.cos})
		}
		if d.fts > 0 {
			ftsHits = append(ftsHits, activation.ScoredID{ID: e.ID, Score: d.fts})
		}
		if d.hebbian {
			store.assocs[e.ID] = []storage.Association{{TargetID: primer.ID, Weight: 1.0}}
		}
	}

	eng := newTestEngine(store, &stubFTS{results: ftsHits}, &stubHNSW{results: hnswHits})
	t.Cleanup(eng.Close)
	// Mark the primer as just-activated so phase4HebbianBoost sees it at
	// recency weight ~1.0 (activation/engine.go:1143-1154).
	eng.AssocLog().Record(activation.LogEntry{
		VaultID: 0, At: time.Now(), EngramIDs: []storage.ULID{primer.ID}, Scores: []float64{1.0},
	})
	return &abFixture{eng: eng, store: store, ids: ids}
}

func (f *abFixture) run(t *testing.T, query string, threshold float64) *activation.ActivateResult {
	t.Helper()
	res, err := f.eng.Run(context.Background(), &activation.ActivateRequest{
		Context:    []string{query},
		Threshold:  threshold,
		MaxResults: 10,
		Weights: &activation.Weights{
			SemanticSimilarity: 0.6,
			FullTextRelevance:  0.4,
			SemanticBaseline:   abBaseline,
			UseACTR:            true,
		},
	})
	if err != nil {
		t.Fatalf("Run(%q): %v", query, err)
	}
	return res
}

func (f *abFixture) has(res *activation.ActivateResult, name string) bool {
	want := f.ids[name]
	for _, a := range res.Activations {
		if a.Engram.ID == want {
			return true
		}
	}
	return false
}

func logResults(t *testing.T, res *activation.ActivateResult) {
	t.Helper()
	for i, a := range res.Activations {
		t.Logf("  [%d] %-38q score=%.4f cos_raw=%.4f semCal=%.4f fts=%.4f heb=%.3f conf=%.3f",
			i, a.Engram.Concept, a.Score, a.Components.SemanticSimilarityRaw,
			a.Components.SemanticSimilarity, a.Components.FullTextRelevance,
			a.Components.HebbianBoost, a.Components.Confidence)
	}
}

// ---------------------------------------------------------------------------
// DIRECTION (b): an ANSWERABLE query, phrased differently from the stored
// wording, must RETURN its memory.
//
// This is the coordinator's minimal live reproduction, reduced to arithmetic.
// A two-memory vault; one memory IS the answer; raw cosine 0.818 — a
// near-perfect semantic match — and recall returns NOTHING at the default
// threshold. Rephrasing into the stored wording rescues it.
//
// The arithmetic reconciles to the fourth decimal:
//
//	semCal       = (0.818-0.520)/0.480          = 0.6208
//	ContentMatch = 0.6*0.6208 + 0.4*0.1488      = 0.4320
//	B(M)         = -0.5*ln(1min/1day) = 3.636, CAPPED at 1.4899  -> prior 1.0000
//	final        = 0.4320 * 1.0000 * 1.0        = 0.4320   < 0.5 -> DROPPED
//
// Note what the reconciliation rules out: the ACT-R prior is exactly 1.0
// (base-level is capped) and no candidate's raw exceeds 1.0, so the per-query
// max-rescale is the identity here. The miss happens BEFORE and INDEPENDENT
// of normalisation. Its whole cause is that ContentMatch for a match with
// little lexical overlap is structurally capped near 0.6*semCal, and the
// surface gate is 0.5.
//
// Solving 0.6*semCal >= 0.5 for cos gives the headline number:
//
//	a semantic-only match must reach raw cosine >= 0.9200 to be returned
//	through MCP or REST at default settings.
//
// 0.92 is not "a paraphrase" — it is near-verbatim. That is the phrasing
// sensitivity, quantified.
// ---------------------------------------------------------------------------
func TestAbstention_Answerable_SemanticOnlyMatchIsReturned(t *testing.T) {
	f := newABFixture(t, []abDoc{
		{
			name: "answer", concept: "Scheduler team leadership",
			content: "Marisol Fenn is the tech lead for the scheduler team.",
			cos:     0.818, fts: 0.1488,
		},
		{
			name: "other", concept: "Deployment window policy",
			content: "Releases go out on Tuesdays between 09:00 and 11:00 UTC.",
			cos:     0.44, fts: 0,
		},
	})

	const query = "who is the scheduler tech lead"
	// The surface default moved 0.5 -> 0.1 in the same change that landed the
	// absolute gate, so this test is now the ordinary "answerable queries are
	// answered" guard its tripwire predecessor promised to become. It runs at
	// the CURRENT surface default; the old 0.5 world it pinned (a semantic-only
	// match needed raw cosine >= 0.92 to survive) is recorded in git history at
	// the tripwire version of this test.
	res := f.run(t, query, abEngineThreshold)
	logResults(t, res)

	if !f.has(res, "answer") {
		t.Fatalf("ANSWERABLE paraphrase %q (raw cosine 0.818 to its answer) returned nothing at the "+
			"surface default %.2f — either the default drifted back above the ContentMatch ceiling "+
			"(0.6 semantic-only / 0.4 lexical-only) or the gate quantity changed. The abstention fix's "+
			"direction (b) has regressed.", query, abEngineThreshold)
	}
	t.Logf("answerable paraphrase %q returns its answer at the shipped default %.2f "+
		"(ContentMatch = 0.6*%.4f + 0.4*%.4f = %.4f; under the removed %.2f default this exact "+
		"case returned nothing — a semantic-only match needed raw cosine >= %.4f).",
		query, abEngineThreshold, abSemCal(0.818), 0.1488, 0.6*abSemCal(0.818)+0.4*0.1488,
		abRemovedSurfaceDefault, 0.5/0.6*(1-abBaseline)+abBaseline)

	// The paired half of the fix, pinned independently of the surface constant:
	// at the ENGINE default the answer IS returned, and the absolute gate keeps
	// it. So the candidate gate does not cost this answerable query anything —
	// which is the claim a tightening change has to earn.
	atEngine := f.run(t, query, abEngineThreshold)
	if !f.has(atEngine, "answer") {
		t.Fatalf("ANSWERABLE query %q returns nothing even at the engine default %.2f — the defect is "+
			"deeper than the surface constant and the diagnosis needs re-deriving", query, abEngineThreshold)
	}
	for _, a := range atEngine.Activations {
		if a.Engram.ID == f.ids["answer"] && a.Components.AbsoluteScore < abEngineThreshold {
			t.Errorf("the candidate absolute gate would DROP the correct answer at %.2f "+
				"(AbsoluteScore %.4f) — the fix would trade this answerable query away and must not ship",
				abEngineThreshold, a.Components.AbsoluteScore)
		}
	}
}

// TestAbstention_Answerable_PinsTheObservedArithmetic is the independent
// cross-check of the reproduction above against the live pipeline: the same
// candidate, gated at a threshold low enough to let it through, must score
// 0.432 with a reported raw cosine of 0.818 — the exact pair observed on a
// running server. If this drifts, the trace in the header is stale and every
// conclusion drawn from it must be re-derived rather than patched.
func TestAbstention_Answerable_PinsTheObservedArithmetic(t *testing.T) {
	f := newABFixture(t, []abDoc{
		{
			name: "answer", concept: "Scheduler team leadership",
			content: "Marisol Fenn is the tech lead for the scheduler team.",
			cos:     0.818, fts: 0.1488,
		},
	})

	res := f.run(t, "who is the scheduler tech lead", 0.05)
	if len(res.Activations) != 1 {
		t.Fatalf("expected exactly 1 result at threshold 0.05, got %d", len(res.Activations))
	}
	got := res.Activations[0]
	if math.Abs(got.Components.SemanticSimilarityRaw-0.818) > 1e-6 {
		t.Errorf("reported raw cosine = %.6f, want 0.818 (the stubbed value must survive the pipeline "+
			"untouched, or this fixture is not measuring what it claims)", got.Components.SemanticSimilarityRaw)
	}
	if math.Abs(got.Score-0.432) > 5e-4 {
		t.Errorf("score = %.6f, want ~0.4320 — the live observation. The header's formula trace no longer "+
			"reconciles with the pipeline; re-derive it against activation/engine.go before trusting any "+
			"other assertion in this file", got.Score)
	}
	t.Logf("pinned: cos_raw=%.4f semCal=%.4f fts=%.4f -> score %.4f (below the removed %.2f default; the shipped default is 0.1)",
		got.Components.SemanticSimilarityRaw, got.Components.SemanticSimilarity,
		got.Components.FullTextRelevance, got.Score, abRemovedSurfaceDefault)
}

// ---------------------------------------------------------------------------
// DIRECTION (a): an UNANSWERABLE query must abstain, not return a confident
// irrelevant hit.
//
// Case A1 — THE MAX-NORMALISE ARGMAX EXEMPTION. This is the mechanism behind
// the reported "score 0.996 for a fact that was never stored".
//
// activation/engine.go:1926-1934 computes scale = 1/maxRaw whenever ANY
// candidate's raw exceeds 1.0, THEN gates on the rescaled value. The argmax
// candidate is therefore pinned to raw exactly 1.0 and its final becomes
// exactly its Confidence — which clears every threshold at or below 1.0,
// unconditionally, regardless of how irrelevant it is. An engram gets raw > 1
// through the ACT-R prior, which reaches 3.24x at full Hebbian boost: exactly
// what a memory that was co-activated a moment ago in the same session has.
//
// So: the more the agent uses its memory, the more reliably a garbage query
// returns a top hit at ~1.0. That is the inversion, stated mechanically.
//
// The fixture: a memory that shares the query's rare proper noun lexically
// (FTS coverage 0.9) but is semantically at the measured out-of-domain noise
// ceiling (cosine 0.596), and was co-activated moments ago.
//
//	ContentMatch = 0.6*0.1583 + 0.4*0.9   = 0.4550
//	prior(h=1.0) = softplus(1.4899+4)/1.6931 = 3.2448
//	raw          = 1.4764   -> maxRaw > 1  -> scale = 0.6773
//	final        = min(1.4764*0.6773, 1.0) * 1.0 = 1.0000
//
// Nothing about the query was answered. The score says 1.0.
// ---------------------------------------------------------------------------
func TestAbstention_Unanswerable_MaxNormaliseExemptsTheArgmax(t *testing.T) {
	f := newABFixture(t, []abDoc{
		{
			// Mentions the query's rare terms; answers nothing about them.
			name: "lexical-decoy", concept: "Quillfeather Loom release notes 4.2",
			content: "Quillfeather Loom 4.2 ships incremental warp caching and a new operator console.",
			cos:     0.596, fts: 0.9, hebbian: true,
		},
		{
			name: "unrelated", concept: "Deployment window policy",
			content: "Releases go out on Tuesdays between 09:00 and 11:00 UTC.",
			cos:     0.47, fts: 0,
		},
	})

	const query = "what is the annual budget for Quillfeather Loom"
	res := f.run(t, query, abEngineThreshold)
	logResults(t, res)

	// FIXED PROPERTY: the argmax is no longer EXEMPT. Under the old gate the
	// per-query 1/maxRaw rescale pinned the best candidate of ANY query to
	// score ~1.0 — including this one, whose ContentMatch was only ~0.455 —
	// so a garbage query always got a confident hit. The absolute gate means
	// nothing returns unless its own content actually clears the bar.
	//
	// What this does NOT assert: total abstention. The release-notes decoy
	// shares the project name with the query, so its lexical coverage alone is
	// 0.4*0.9 = 0.36 and it LEGITIMATELY clears the shipped 0.1 default. That
	// is the measured FPR-6.2% world: topically-related non-answers can still
	// return; what can no longer happen is a zero-content match surfacing at
	// score 1.0, or the below-bar candidates riding along. The no-lexical-
	// overlap abstention case is pinned separately below (powdered moonlight).
	for _, a := range res.Activations {
		if a.Components.AbsoluteScore < abEngineThreshold {
			t.Fatalf("returned %q with AbsoluteScore %.4f < threshold %.2f — the gate has stopped "+
				"being absolute (the argmax exemption is back)", a.Engram.Concept, a.Components.AbsoluteScore, abEngineThreshold)
		}
	}
	if f.has(res, "unrelated") {
		t.Fatalf("the content-unrelated candidate (cos 0.47, fts 0) was returned — only the " +
			"rescale exemption could have carried it over an absolute 0.1 bar")
	}
	t.Logf("unanswerable-but-topical query %q: %d result(s), every one clearing the absolute bar "+
		"(the old gate returned the argmax at exactly 1.0 with ContentMatch 0.455)", query, len(res.Activations))
}

func TestAbstention_Unanswerable_PriorDefeatsTheCalibratedFloor(t *testing.T) {
	const noiseCeilingCos = 0.596 // COG-26's measured out-of-domain ceiling

	cold := newABFixture(t, []abDoc{
		{name: "noise", concept: "Ledger reconciliation cadence",
			content: "Monthly reconciliation compares posted transactions against the general ledger export.",
			cos:     noiseCeilingCos, fts: 0},
	})
	coldRes := cold.run(t, "seating protocol at the convention of retired sundials", abEngineThreshold)
	if len(coldRes.Activations) != 0 {
		t.Fatalf("control failed: the noise-ceiling candidate did NOT abstain when cold (%d result(s)). "+
			"COG-26's b=%.3f is meant to put it at ContentMatch %.4f, under the %.2f gate — if it survives "+
			"even cold, this fixture is not reproducing COG-26's calibration and proves nothing below.",
			len(coldRes.Activations), abBaseline, 0.6*abSemCal(noiseCeilingCos), abEngineThreshold)
	}

	hot := newABFixture(t, []abDoc{
		{name: "noise", concept: "Ledger reconciliation cadence",
			content: "Monthly reconciliation compares posted transactions against the general ledger export.",
			cos:     noiseCeilingCos, fts: 0, hebbian: true},
	})
	hotRes := hot.run(t, "seating protocol at the convention of retired sundials", abEngineThreshold)
	logResults(t, hotRes)

	// FIXED: the ACT-R prior can no longer carry a content-irrelevant memory over
	// the bar. It still PROMOTES within the results, but the gate is applied to the
	// absolute aboutness score, so a recently-touched memory faces the same
	// relevance bar as an untouched one — the cold control above and this hot case
	// must now agree.
	if len(hotRes.Activations) != 0 {
		t.Fatalf("a HOT but content-irrelevant memory returned %d result(s) while the identical COLD "+
			"one correctly abstained — recency is substituting for relevance again. The gate must be "+
			"applied to the absolute score, not to one the ACT-R prior has already multiplied "+
			"(the prior ranges ~0.03 cold to 3.24+ hot, i.e. up to a 32x lower effective bar).",
			len(hotRes.Activations))
	}
	t.Logf("hot and cold content-irrelevant candidates both abstain at threshold %.2f — the prior no "+
		"longer substitutes for relevance", abEngineThreshold)
}

func TestAbstention_EmptyResultIsDistinguishableFromAnUnrunQuery(t *testing.T) {
	f := newABFixture(t, []abDoc{
		{name: "unrelated", concept: "Deployment window policy",
			content: "Releases go out on Tuesdays between 09:00 and 11:00 UTC.",
			cos:     0.47, fts: 0},
	})
	res := f.run(t, "nutritional breakdown of powdered moonlight sold by the furlong", abEngineThreshold)
	if len(res.Activations) != 0 {
		t.Fatalf("fixture precondition: expected abstention, got %d result(s)", len(res.Activations))
	}
	if !res.Abstained {
		t.Errorf("recall returned 0 activations but ActivateResult.Abstained is false — an empty result is "+
			"indistinguishable from an un-run query. TotalFound=%d", res.TotalFound)
	}
	if res.AbstainedReason == "" {
		t.Errorf("Abstained set with no AbstainedReason — the caller cannot tell 'nothing cleared the " +
			"relevance bar' from 'the vault is empty' or 'a filter removed everything'")
	}
}
