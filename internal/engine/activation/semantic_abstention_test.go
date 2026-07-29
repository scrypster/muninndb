//go:build localassets

package activation_test

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/engine/activation"
	"github.com/scrypster/muninndb/internal/plugin"
	embedpkg "github.com/scrypster/muninndb/internal/plugin/embed"
	"github.com/scrypster/muninndb/internal/storage"
)

// ---------------------------------------------------------------------------
// Issue COG-26: semantic-abstention floor (the named residual of #711).
//
// Root cause (see
// .claude/deep-review/2026-07-29-semantic-abstention-floor-design.md):
// bge-small-en-v1.5 is strongly anisotropic on real text — an out-of-domain
// query/passage pair still lands at cosine ~=0.45-0.60, not ~=0. The ACT-R
// content gate contentMatch = 0.6*semantic + 0.4*fts clears its abstention
// threshold (0.1) on that noise alone (0.6*0.5 = 0.30 >> 0.1), so #711's
// lexical-nonsense fix left semantic-only nonsense still returning
// "confident" results.
//
// This test builds a real (non-mocked) bge-small-en-v1.5 embedder — the same
// bundled ONNX asset the server uses in production — and a small domain-
// coherent corpus, wires them into a real activation.ActivationEngine with a
// brute-force cosine "HNSW" (topK by real cosine, same shape as production's
// vector pool), and checks:
//
//   - RED-sanity: with the floor DISABLED (SemanticBaseline=0, i.e. today's
//     pre-COG-26 behavior), the nonsense query returns a confident match.
//     This is the bug #711 left unfixed — it must reproduce here or the test
//     proves nothing.
//   - GREEN: with the floor ENABLED (SemanticBaseline=0.520, the bge-small
//     registry value), the same nonsense query abstains (0 results).
//   - Positive control (a): a genuinely relevant query (high cosine) still
//     returns its match with the floor enabled — the floor must not be a
//     flat cutoff that kills real signal.
//   - Positive control (b): a paraphrase of the genuine query (moderate but
//     real cosine, the #711 "paraphrase paradox" case) still returns its
//     match with the floor enabled — the no-silent-substitution guard. This
//     is now a hard assertion (not a skip): an adversarial refute of the
//     original b=0.558 proved a genuine 0.59-cosine paraphrase silently
//     abstained, so a skip here would guard nothing.
//   - Small-vault zero-lexical-overlap probe: on a ~15-20 engram corpus (no
//     lexical overlap anywhere to mask the failure mode, unlike a large
//     battery), a genuine paraphrase at cosine ~0.61 must survive and a
//     nonsense query at cosine ~0.55 must still abstain.
// ---------------------------------------------------------------------------

// semanticBaselineBGE is COG-26's registered b for bge-small-en-v1.5
// (mu=0.450, sigma=0.054, b = mu+1.3*sigma = 0.520). Mirrors the constant in
// internal/plugin/embed/baseline.go — kept independent here (not imported)
// so this test still catches a registry value drifting away from what
// actually separates the two distributions.
//
// b was lowered from mu+2*sigma (0.558) after an adversarial refute found it
// false-abstained genuine moderate-cosine matches (~24% of the measured
// true-pair distribution, mu=0.693, sigma=0.088, fell below the 0.558
// survival bar of cos~=0.632). At b=0.520 the survival bar drops to
// cos~=0.600, rescuing that band while the measured noise top (~0.596)
// still abstains — with a thin margin (contentMatch ~0.095 vs the 0.1 gate).
const semanticBaselineBGE = 0.520

// billingCorpus is a small, domain-coherent corpus (SaaS billing/invoicing
// engineering notes) — coherent enough to be realistic, matching the design
// doc's observation that in-vault text is NOT random (its cosine floor is
// higher than pure random-pair noise), so a nonsense query against it
// reproduces the same "in-domain-adjacent noise" shape the design measured
// on the real vault, not an artificially easy zero-overlap case.
var billingCorpus = []struct {
	concept string
	content string
}{
	{"Four-bucket pricing rate tiers", "The billing platform defines four pricing tiers based on monthly usage volume, each with its own per-unit rate and overage charge."},
	{"Remittance processing flow", "Incoming remittances are matched against open invoices by reference number, then posted to the customer's account ledger."},
	{"Dues checkoff remittance handling", "Union dues checkoff remittances arrive as a batch file and are reconciled against the member roster before posting."},
	{"Market recovery fund calculation", "The market recovery fund surcharge is calculated as a percentage of gross billed revenue for the reporting period."},
	{"Member classification tiers", "Members are classified into tiers based on tenure and usage; classification determines the applicable discount schedule."},
	{"Audit score calculation", "The audit score aggregates late-payment frequency, dispute rate, and reconciliation variance into a single risk indicator."},
	{"Per-capita billing model", "Per-capita billing charges each account a flat rate regardless of usage, recalculated annually at contract renewal."},
	{"Ledger reconciliation procedure", "Monthly ledger reconciliation compares the billing system's posted transactions against the general ledger export."},
	{"Employer portal onboarding", "New employer accounts are onboarded through the portal, which walks them through rate selection and payment setup."},
	{"1919 validator quarter corrections", "Historical quarter corrections from 1919 forward were migrated into the validator's adjustment ledger during the archive import."},
	{"JATC fund rules", "The joint apprenticeship training fund assesses a fixed per-hour contribution rate set annually by the trustees."},
	{"Reciprocity agreement handling", "Reciprocity agreements route contributions to a member's home fund when work is performed in another jurisdiction."},
	{"Invoice dispute resolution", "Disputed line items are flagged, routed to a reviewer queue, and held out of the aging report until resolved."},
	{"Late fee assessment schedule", "Late fees accrue at a fixed percentage per 30-day period past the invoice due date, capped at four assessments."},
	{"Batch payment file format", "Incoming batch payment files use a fixed-width layout with a header record and one detail record per remittance line."},
	{"Contract renewal rate escalation", "Renewal contracts apply an automatic rate escalation tied to a published cost index unless manually overridden."},
	{"Overpayment refund workflow", "Overpayments detected during reconciliation are queued for refund approval before the disbursement batch runs."},
	{"Delinquent account escalation", "Accounts thirty days past due are flagged for a first notice; sixty days triggers escalation to the collections queue."},
}

// bruteForceCosineIndex implements activation.HNSWIndex by real cosine
// similarity over precomputed embeddings — the same top-K-by-cosine contract
// production HNSW provides, without pulling in the full hnsw package.
type bruteForceCosineIndex struct {
	vecs map[storage.ULID][]float32
}

func (b *bruteForceCosineIndex) Search(_ context.Context, _ [8]byte, query []float32, topK int) ([]activation.ScoredID, error) {
	type scored struct {
		id    storage.ULID
		score float64
	}
	all := make([]scored, 0, len(b.vecs))
	for id, v := range b.vecs {
		all = append(all, scored{id: id, score: cosine64(query, v)})
	}
	// simple insertion sort descending — corpus is tiny
	for i := 1; i < len(all); i++ {
		for j := i; j > 0 && all[j].score > all[j-1].score; j-- {
			all[j], all[j-1] = all[j-1], all[j]
		}
	}
	if topK > len(all) {
		topK = len(all)
	}
	out := make([]activation.ScoredID, topK)
	for i := 0; i < topK; i++ {
		out[i] = activation.ScoredID{ID: all[i].id, Score: all[i].score}
	}
	return out, nil
}

func cosine64(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

// realBGEEmbedder builds the real bundled bge-small-en-v1.5 embed service
// (the same code path production uses) for use as an activation.Embedder in
// this test, and precomputes the corpus embeddings.
func realBGEEmbedder(t *testing.T) (activation.Embedder, *embedpkg.EmbedService) {
	t.Helper()
	dir := t.TempDir()
	svc, err := embedpkg.NewEmbedService("local://")
	if err != nil {
		t.Fatalf("NewEmbedService: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := svc.Init(ctx, plugin.PluginConfig{DataDir: dir}); err != nil {
		t.Skipf("local embed provider init failed (assets missing? run make fetch-assets): %v", err)
	}
	t.Cleanup(func() { _ = svc.Close() })
	return embedpkg.NewEmbedServiceAdapter(svc), svc
}

// buildSemanticAbstentionFixture embeds the billing corpus and wires a real
// ActivationEngine over it. Returns the engine and a lookup from concept to
// engram ID (for identifying the correct top match).
func buildSemanticAbstentionFixture(t *testing.T) (*activation.ActivationEngine, *stubStore, map[string]storage.ULID) {
	t.Helper()
	embedder, svc := realBGEEmbedder(t)
	store := newStubStore()
	vecs := make(map[storage.ULID][]float32, len(billingCorpus))
	byConcept := make(map[string]storage.ULID, len(billingCorpus))

	for _, n := range billingCorpus {
		eng := &storage.Engram{
			Concept:    n.concept,
			Content:    n.content,
			Confidence: 1.0,
			Stability:  30.0,
			CreatedAt:  time.Now(),
		}
		store.writeEngram(eng)
		byConcept[n.concept] = eng.ID

		// Production embeds "Concept + ' ' + Content" (retroactive.go:469).
		text := n.concept + " " + n.content
		vec, err := svc.Embed(context.Background(), []string{text})
		if err != nil {
			t.Fatalf("embed corpus item %q: %v", n.concept, err)
		}
		vecs[eng.ID] = vec
	}

	idx := &bruteForceCosineIndex{vecs: vecs}
	eng := activation.New(store, &stubFTS{}, idx, embedder)
	t.Cleanup(eng.Close)
	return eng, store, byConcept
}

func runSemanticQuery(t *testing.T, eng *activation.ActivationEngine, query string, baseline float64) *activation.ActivateResult {
	t.Helper()
	result, err := eng.Run(context.Background(), &activation.ActivateRequest{
		Context:    []string{query},
		Threshold:  0.1,
		MaxResults: 10,
		IncludeWhy: true,
		Weights: &activation.Weights{
			SemanticSimilarity: 0.6,
			FullTextRelevance:  0.4,
			SemanticBaseline:   float32(baseline),
			UseACTR:            true,
		},
	})
	if err != nil {
		t.Fatalf("Run(%q): %v", query, err)
	}
	return result
}

// TestSemanticAbstention_REDSanity_NonsenseClearsGateWithoutFloor is the
// RED-sanity check: it MUST FAIL (i.e. the nonsense query must return a
// confident result) with the floor disabled (baseline=0), reproducing
// today's pre-COG-26 bug. If this test does not fail without the fix, it
// proves nothing about the fix.
func TestSemanticAbstention_REDSanity_NonsenseClearsGateWithoutFloor(t *testing.T) {
	eng, _, _ := buildSemanticAbstentionFixture(t)

	const nonsenseQuery = "how to tune a violin"
	result := runSemanticQuery(t, eng, nonsenseQuery, 0 /* floor disabled: identity transform */)

	if len(result.Activations) == 0 {
		t.Fatalf("RED-sanity failed: nonsense query %q abstained even with the floor DISABLED (baseline=0). "+
			"Either the bug this increment fixes doesn't reproduce with this corpus, or something else already "+
			"gates it — this test proves nothing until it fails here.", nonsenseQuery)
	}
	top := result.Activations[0]
	t.Logf("RED (expected to leak): nonsense query %q returned %d result(s); top=%q score=%.4f semantic_similarity=%.4f",
		nonsenseQuery, len(result.Activations), top.Engram.Concept, top.Score, top.Components.SemanticSimilarity)
}

// TestSemanticAbstention_NonsenseQueryAbstainsWithFloor is the GREEN case:
// the same nonsense query, with the COG-26 floor enabled, must abstain.
func TestSemanticAbstention_NonsenseQueryAbstainsWithFloor(t *testing.T) {
	eng, _, _ := buildSemanticAbstentionFixture(t)

	const nonsenseQuery = "how to tune a violin"
	result := runSemanticQuery(t, eng, nonsenseQuery, semanticBaselineBGE)

	if len(result.Activations) > 0 {
		top := result.Activations[0]
		t.Errorf("nonsense query %q returned %d result(s) with the floor enabled (b=%.3f); want 0 (abstention). "+
			"Top match: concept=%q score=%.4f semantic_similarity=%.4f why=%q",
			nonsenseQuery, len(result.Activations), semanticBaselineBGE,
			top.Engram.Concept, top.Score, top.Components.SemanticSimilarity, top.Why)
	} else {
		t.Logf("nonsense query correctly abstained with the floor enabled (b=%.3f)", semanticBaselineBGE)
	}
}

// TestSemanticAbstention_PositiveControl_GenuineQueryStillMatches: a
// genuinely on-topic query (high cosine) must still return its match with
// the floor enabled — the floor rescales magnitude, it must not be a cutoff
// that also kills real signal.
func TestSemanticAbstention_PositiveControl_GenuineQueryStillMatches(t *testing.T) {
	eng, _, byConcept := buildSemanticAbstentionFixture(t)
	wantID := byConcept["Four-bucket pricing rate tiers"]

	const genuineQuery = "four-bucket pricing rate tiers"
	result := runSemanticQuery(t, eng, genuineQuery, semanticBaselineBGE)

	if len(result.Activations) == 0 {
		t.Fatalf("genuine query %q returned 0 results with the floor enabled (b=%.3f); want >= 1 (positive control)",
			genuineQuery, semanticBaselineBGE)
	}
	top := result.Activations[0]
	if top.Engram.ID != wantID {
		t.Errorf("top result = %q, want the pricing-tiers engram", top.Engram.Concept)
	}
	t.Logf("genuine query matched: concept=%q score=%.4f semantic_similarity=%.4f",
		top.Engram.Concept, top.Score, top.Components.SemanticSimilarity)
}

// TestSemanticAbstention_PositiveControl_ParaphraseStillMatches is the
// no-silent-substitution guard: a paraphrase using different vocabulary than
// the stored text (moderate cosine, the #711 "paraphrase paradox" shape)
// must still clear the floor and return its match.
//
// This is a hard assertion, not a skip: an earlier revision of this test
// skipped (rather than failed) if the paraphrase abstained, which meant the
// test suite never actually caught the regression an adversarial refute
// later found — b=0.558 silently abstained a genuine 0.59-cosine paraphrase
// in a real vault, and this test's Skipf let that pass CI clean. A skip
// guards nothing; if the paraphrase doesn't survive, that IS the failure
// this test exists to catch.
func TestSemanticAbstention_PositiveControl_ParaphraseStillMatches(t *testing.T) {
	eng, _, byConcept := buildSemanticAbstentionFixture(t)
	wantID := byConcept["Remittance processing flow"]

	// Paraphrase: no shared content words with the stored concept/content
	// ("remittance", "matched", "invoices", "posted", "ledger") beyond the
	// generic domain vocabulary — a genuine reformulation, not a lexical echo.
	const paraphraseQuery = "how do incoming customer payments get applied to the right open bill"
	result := runSemanticQuery(t, eng, paraphraseQuery, semanticBaselineBGE)

	if len(result.Activations) == 0 {
		t.Fatalf("paraphrase query %q abstained with the floor enabled (b=%.3f); want >= 1 result. "+
			"A skip here would prove nothing — the whole point of this test is that a genuine "+
			"reformulation must survive the floor.", paraphraseQuery, semanticBaselineBGE)
	}
	top := result.Activations[0]
	if top.Engram.ID != wantID {
		t.Errorf("top result = %q (id=%v), want the remittance-processing engram (id=%v)",
			top.Engram.Concept, top.Engram.ID, wantID)
	}
	t.Logf("paraphrase query matched: concept=%q score=%.4f semantic_similarity=%.4f semantic_similarity_raw=%.4f",
		top.Engram.Concept, top.Score, top.Components.SemanticSimilarity, top.Components.SemanticSimilarityRaw)
}

// TestSemanticAbstention_ModerateCosineBand_GenuineMatchSurvives is finding
// #1 of the 2026-07-29 adversarial refute made test-shaped: a genuine match
// in the 0.60-0.65 raw-cosine band. At the old b=0.558 (mu+2sigma), the
// sem-only survival bar sat at cos~=0.632, so a genuine match at 0.60-0.65
// would fall below it and silently abstain — exactly the bug the refute
// found (a real paraphrase at cosine 0.59 scored contentMatch=0.043 < 0.1
// and recall returned EMPTY where it should have ranked #2). At the lowered
// b=0.520, the survival bar drops to cos~=0.600, so this same band survives.
//
// This test MUST FAIL if b is raised back to 0.558 and MUST PASS at 0.520 —
// that's the whole point: it pins the fix, not just a general "matches
// still work" smoke check.
func TestSemanticAbstention_ModerateCosineBand_GenuineMatchSurvives(t *testing.T) {
	eng, _, byConcept := buildSemanticAbstentionFixture(t)
	wantID := byConcept["Overpayment refund workflow"]

	// Measured raw cosine against the corpus (see the exploration notes in
	// the COG-26 refute follow-up): ~0.6265 against "Overpayment refund
	// workflow", ~0.041 clear of the next-closest candidate — squarely in
	// the 0.60-0.65 band this increment's b=0.520 was chosen to rescue.
	const moderateBandQuery = "getting a refund after paying extra by mistake"
	result := runSemanticQuery(t, eng, moderateBandQuery, semanticBaselineBGE)

	if len(result.Activations) == 0 {
		t.Fatalf("0.60-0.65-band genuine match %q abstained with the floor enabled (b=%.3f); "+
			"want >= 1 result — this is exactly the false-abstention the refute found at b=0.558.",
			moderateBandQuery, semanticBaselineBGE)
	}
	top := result.Activations[0]
	if top.Engram.ID != wantID {
		t.Errorf("top result = %q (id=%v), want the overpayment-refund engram (id=%v)",
			top.Engram.Concept, top.Engram.ID, wantID)
	}
	raw := top.Components.SemanticSimilarityRaw
	if raw < 0.60 || raw > 0.65 {
		t.Errorf("measured raw cosine = %.4f, want in [0.60, 0.65] — this test is meant to pin the "+
			"specific band the refute found unrescued at the old b=0.558; if the corpus/model "+
			"realization drifted out of band, pick a new query rather than widening this check", raw)
	}
	t.Logf("moderate-band query matched: concept=%q score=%.4f semantic_similarity=%.4f semantic_similarity_raw=%.4f",
		top.Engram.Concept, top.Score, top.Components.SemanticSimilarity, raw)
}

// TestSemanticAbstention_SmallVault_ZeroLexicalOverlapProbe is the small-
// vault probe the refute called for: an 18-engram vault (the billing
// corpus above — no lexical overlap decoy engrams padding out a large
// candidate pool) where a genuine, zero-lexical-overlap paraphrase must
// survive the floor and a nonsense query must still abstain. A large-vault
// battery (dozens to hundreds of engrams with lexical overlap scattered
// throughout) can mask this failure mode — the genuine match doesn't need
// the floor to be perfectly tuned because FTS or a lexical near-match
// usually corroborates it. On a small, fresh vault (a brand-new user's
// first session, or a narrow single-topic vault) there is no such
// corroboration: the semantic floor is the ONLY gate, and this is where a
// too-aggressive b silently drops a genuine first match.
func TestSemanticAbstention_SmallVault_ZeroLexicalOverlapProbe(t *testing.T) {
	eng, _, byConcept := buildSemanticAbstentionFixture(t)

	t.Run("genuine_paraphrase_survives", func(t *testing.T) {
		wantID := byConcept["Overpayment refund workflow"]
		// No shared words with the stored concept/content ("overpayment",
		// "refund", "reconciliation", "disbursement", "batch", "queued",
		// "approval") beyond incidental filler words — a genuine
		// reformulation. Measured raw cosine ~0.604, ~0.026 clear of the
		// next-closest candidate.
		const query = "before extra money goes back out, a manager has to say yes first"
		result := runSemanticQuery(t, eng, query, semanticBaselineBGE)
		if len(result.Activations) == 0 {
			t.Fatalf("zero-lexical-overlap paraphrase %q abstained on the small vault (b=%.3f); want >= 1 result",
				query, semanticBaselineBGE)
		}
		top := result.Activations[0]
		if top.Engram.ID != wantID {
			t.Errorf("top result = %q, want the overpayment-refund engram", top.Engram.Concept)
		}
		t.Logf("small-vault paraphrase matched: concept=%q semantic_similarity_raw=%.4f",
			top.Engram.Concept, top.Components.SemanticSimilarityRaw)
	})

	t.Run("nonsense_still_abstains", func(t *testing.T) {
		// Clearly out-of-domain (community arts scheduling vs. billing/
		// invoicing) yet lands at the top of this corpus's in-domain-
		// adjacent noise (measured raw cosine ~0.545 against "Late fee
		// assessment schedule" — the shared word "schedule" is exactly the
		// kind of incidental lexical echo that inflates bge-small's
		// out-of-domain cosine without any real topical relevance).
		const query = "quarterly schedule of community recital events downtown"
		result := runSemanticQuery(t, eng, query, semanticBaselineBGE)
		if len(result.Activations) > 0 {
			top := result.Activations[0]
			t.Errorf("nonsense query %q returned %d result(s) on the small vault (b=%.3f); want 0. "+
				"Top: concept=%q semantic_similarity=%.4f semantic_similarity_raw=%.4f",
				query, len(result.Activations), semanticBaselineBGE,
				top.Engram.Concept, top.Components.SemanticSimilarity, top.Components.SemanticSimilarityRaw)
		} else {
			t.Logf("small-vault nonsense query correctly abstained (b=%.3f)", semanticBaselineBGE)
		}
	})
}

// TestSemanticAbstention_UnknownModel_IdentityPlusWarn documents the
// unknown-model contract at the transform level: rescaleSemantic (exercised
// indirectly here via SemanticBaseline=0, the value resolveSemanticBaseline
// produces for an unregistered model) leaves cosine unchanged — the registry
// WARN itself is logged by internal/engine.resolveSemanticBaseline, which is
// covered separately in internal/engine (root package) tests since the
// registry lookup lives there, not in this package.
func TestSemanticAbstention_UnknownModel_IdentityPlusWarn(t *testing.T) {
	eng, _, byConcept := buildSemanticAbstentionFixture(t)
	wantID := byConcept["Four-bucket pricing rate tiers"]

	// baseline=0 is exactly what an unresolved/unregistered model falls back
	// to (see internal/engine.resolveSemanticBaseline) — identity transform,
	// pre-COG-26 behavior preserved for models with no calibration on record.
	result := runSemanticQuery(t, eng, "four-bucket pricing rate tiers", 0)
	if len(result.Activations) == 0 {
		t.Fatal("identity transform (unknown-model fallback) unexpectedly abstained on a genuine query")
	}
	if result.Activations[0].Engram.ID != wantID {
		t.Errorf("top result = %q, want the pricing-tiers engram", result.Activations[0].Engram.Concept)
	}
}
