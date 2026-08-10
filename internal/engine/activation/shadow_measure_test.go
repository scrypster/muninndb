//go:build localassets

package activation_test

import (
	"context"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/engine/activation"
	"github.com/scrypster/muninndb/internal/storage"
)

// ---------------------------------------------------------------------------
// GATE 6 (COG-28 / #763) — the abstention corpus with a DECLARED VERSION CHAIN
// grafted in, measured rather than argued.
//
// The claim COG-28 has to earn is that substitution REDIRECTS admission-worthy
// evidence and never CREATES it. The direct measurement of that claim is: on
// queries nothing in the vault answers, a superseded predecessor sitting in the
// pool must produce ZERO shadows — because a shadow is the one and only
// admission path a substitution has. No shadows, no substitutions, no matter
// what the chain looks like.
//
// This is measured HERE, at the shadow layer, rather than at the engine layer,
// because it is the tighter statement of the two: the engine layer can only
// refuse what phase 6 offers it, so "zero shadows on the nonsense probes" is
// strictly stronger than "zero substitutions on the nonsense probes".
//
// It reuses TestMeasureAbstentionGate's fixture verbatim — the same 18-engram
// synthetic billing corpus, the same 12 answerable paraphrases and 16 nonsense
// probes, the same real bge-small embedder — so the numbers are directly
// comparable to the recorded 0.6410 / 6.2% baseline.
// ---------------------------------------------------------------------------

// shadowGraftChain marks one corpus member as a superseded predecessor,
// exactly as EvolveAt leaves it: soft-deleted AND stamped with a closed
// ValidUntil, in favour of a newly written successor whose wording is a genuine
// revision of the same subject. Returns (predecessorID, successorID).
//
// It supersedes "Late fee assessment schedule" because one of the 12 answerable
// paraphrases targets it, so the grafted chain is exercised by a query that
// SHOULD produce a shadow as well as by the 16 that must not.
func shadowGraftChain(t *testing.T, store *stubStore, byConcept map[string]storage.ULID, svcEmbed func(string) []float32, vecs map[storage.ULID][]float32) (storage.ULID, storage.ULID) {
	t.Helper()
	predID, ok := byConcept["Late fee assessment schedule"]
	if !ok {
		t.Fatalf("fixture drift: the corpus no longer contains the concept this graft supersedes")
	}
	pred := store.engrams[predID]
	pred.State = storage.StateSoftDeleted
	pred.ValidUntil = time.Now().Add(-time.Minute)

	succ := &storage.Engram{
		Concept:    "Late fee assessment schedule",
		Content:    "Overdue balances now accrue a graduated surcharge each billing cycle, ceilinged after the fourth cycle.",
		Confidence: 1.0, Stability: 30.0, CreatedAt: abmFixtureNow(),
	}
	store.writeEngram(succ)
	vecs[succ.ID] = svcEmbed(succ.Concept + " " + succ.Content)
	return predID, succ.ID
}

// TestMeasureShadowPrecision_GraftedChain is the gate: with a declared chain in
// the corpus, the 16 nonsense probes must produce ZERO shadows, and the
// answerable set's NDCG@5 / the unanswerable set's FPR must not move from the
// recorded baseline.
func TestMeasureShadowPrecision_GraftedChain(t *testing.T) {
	embedder, svc := realBGEEmbedder(t)
	_ = embedder
	embedOne := func(text string) []float32 {
		vec, err := svc.Embed(context.Background(), []string{text})
		if err != nil {
			t.Fatalf("embed %q: %v", text, err)
		}
		return vec
	}

	store := newStubStore()
	vecs := make(map[storage.ULID][]float32, len(billingCorpus)+1)
	byConcept := make(map[string]storage.ULID, len(billingCorpus))
	for _, n := range billingCorpus {
		eng := &storage.Engram{
			Concept: n.concept, Content: n.content,
			Confidence: 1.0, Stability: 30.0, CreatedAt: abmFixtureNow(),
		}
		store.writeEngram(eng)
		byConcept[n.concept] = eng.ID
		vecs[eng.ID] = embedOne(n.concept + " " + n.content)
	}
	pred, succ := shadowGraftChain(t, store, byConcept, embedOne, vecs)
	_ = succ

	eng := activation.New(store, &stubFTS{}, &bruteForceCosineIndex{vecs: vecs}, embedder)
	t.Cleanup(eng.Close)

	run := func(query string) *activation.ActivateResult {
		t.Helper()
		res, err := eng.Run(context.Background(), &activation.ActivateRequest{
			Context:    []string{query},
			Threshold:  0.1, // the engine default, the value COG-26's b was calibrated against
			MaxResults: 100,
			Weights: &activation.Weights{
				SemanticSimilarity: 0.6, FullTextRelevance: 0.4,
				SemanticBaseline: semanticBaselineBGE, UseACTR: true,
			},
		})
		if err != nil {
			t.Fatalf("Run(%q): %v", query, err)
		}
		return res
	}

	// --- the gate: zero shadows on everything the vault cannot answer -------
	nonsenseShadows := 0
	nonsenseWithShadows := 0
	for _, q := range abmUnanswerable {
		res := run(q)
		if n := len(res.ShadowMatches); n > 0 {
			nonsenseShadows += n
			nonsenseWithShadows++
			t.Logf("FALSE SHADOW on %q: %d", q, n)
		}
	}
	if nonsenseShadows != 0 {
		t.Errorf("COG-28 PRECISION FAILURE: %d shadow(s) across %d of %d nonsense probes. A shadow is the ONLY "+
			"admission path a substitution has, so a shadow here is a substitution that would fire on a query "+
			"the vault cannot answer — substitution must REDIRECT admission-worthy evidence, never CREATE it.",
			nonsenseShadows, nonsenseWithShadows, len(abmUnanswerable))
	}
	t.Logf("nonsense probes: %d/%d produced a shadow (want 0/%d)", nonsenseWithShadows, len(abmUnanswerable), len(abmUnanswerable))

	// --- the answerable side: the chain is REACHED, so the gate is not
	// passing by being inert ------------------------------------------------
	answerableShadows := 0
	for _, q := range abmAnswerable {
		for _, sh := range run(q.query).ShadowMatches {
			if sh.Engram.ID == pred {
				answerableShadows++
			}
		}
	}
	if answerableShadows == 0 {
		t.Errorf("no answerable query in the set reached the grafted predecessor at all — the zero-shadow result "+
			"above then proves nothing about precision, only that the mechanism is unreachable in this fixture "+
			"(%d answerable queries)", len(abmAnswerable))
	}
	t.Logf("answerable probes reaching the grafted predecessor as a shadow: %d/%d", answerableShadows, len(abmAnswerable))

	// --- the live metrics must not move -------------------------------------
	//
	// Shadows are excluded from maxRaw by construction (they are scored in a
	// separate pass over a disjoint set), so a declared chain in the pool must
	// leave the live gate's numbers exactly where TestMeasureAbstentionGate
	// recorded them: NDCG@5 0.6410, FPR 6.2%.
	const (
		wantNDCG = 0.6410
		// The recorded 6.2%% is 1 false positive in 16 probes; compare the exact
		// fraction so the assertion is not a rounding artefact.
		wantFPR = 1.0 / 16.0
	)
	var ndcgSum float64
	for _, q := range abmAnswerable {
		gold, ok := byConcept[q.goldConcept]
		if !ok {
			t.Fatalf("gold concept %q is not in the corpus — fixture drift", q.goldConcept)
		}
		kept := run(q.query).Activations
		ndcgSum += abmNDCG5(kept, gold)
	}
	ndcg := ndcgSum / float64(len(abmAnswerable))

	falsePos := 0
	for _, q := range abmUnanswerable {
		if len(run(q).Activations) > 0 {
			falsePos++
		}
	}
	fpr := float64(falsePos) / float64(len(abmUnanswerable))

	t.Logf("with a declared chain grafted in: NDCG@5=%.4f (baseline %.4f) FPR=%.1f%% (baseline %.1f%%)",
		ndcg, wantNDCG, 100*fpr, 100*wantFPR)
	// NDCG is compared with tolerance rather than for equality: the graft
	// REPLACES one gold document with a rewritten successor, so the query that
	// targeted it legitimately scores differently. FPR has no such excuse —
	// nothing about a nonsense query changed.
	if ndcg < wantNDCG-0.10 {
		t.Errorf("NDCG@5 %.4f fell more than 0.10 below the recorded baseline %.4f with a declared chain in the corpus",
			ndcg, wantNDCG)
	}
	if fpr > wantFPR+1e-9 {
		t.Errorf("FPR rose to %.1f%% from the recorded %.1f%% merely by putting a declared version chain in the corpus — "+
			"shadows must not reach the live gate at all", 100*fpr, 100*wantFPR)
	}
}
