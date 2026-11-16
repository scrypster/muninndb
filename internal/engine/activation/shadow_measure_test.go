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
// synthetic booking corpus, the same 12 answerable paraphrases and 16 nonsense
// probes, the same real bge-small embedder. The live-metric check compares the
// grafted run against a graft-free run measured IN THE SAME TEST, from the same
// embeddings, rather than against a number recorded on one machine: the
// embedder's arithmetic is platform-dependent, so a pinned NDCG is a statement
// about the machine it was recorded on, not about the graft.
// ---------------------------------------------------------------------------

// shadowRecordedBaseline* are the graft-free numbers recorded on one platform
// (darwin/arm64, real bge-small) when this corpus was introduced. They are
// LOGGED as context only and never asserted — the assertion uses the same-run
// baseline.
const (
	shadowRecordedBaselineNDCG = 0.8026
	shadowRecordedBaselineFPR  = 2.0 / 16.0
)

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

// shadowArm is one engine over the corpus: either the plain corpus (the
// graft-free baseline) or the corpus with a declared chain grafted in.
type shadowArm struct {
	run       func(query string) *activation.ActivateResult
	byConcept map[string]storage.ULID
	pred      storage.ULID // zero on the graft-free arm
}

// shadowBuildArm writes the corpus into a fresh store, reusing the vectors in
// corpusVecs (keyed by concept) so both arms see byte-identical embeddings, and
// grafts the declared chain when graft is true.
func shadowBuildArm(t *testing.T, embedder activation.Embedder, embedOne func(string) []float32,
	corpusVecs map[string][]float32, graft bool) shadowArm {
	t.Helper()
	store := newStubStore()
	vecs := make(map[storage.ULID][]float32, len(bookingCorpus)+1)
	byConcept := make(map[string]storage.ULID, len(bookingCorpus))
	for _, n := range bookingCorpus {
		eng := &storage.Engram{
			Concept: n.concept, Content: n.content,
			Confidence: 1.0, Stability: 30.0, CreatedAt: abmFixtureNow(),
		}
		store.writeEngram(eng)
		byConcept[n.concept] = eng.ID
		vecs[eng.ID] = corpusVecs[n.concept]
	}
	var pred storage.ULID
	if graft {
		pred, _ = shadowGraftChain(t, store, byConcept, embedOne, vecs)
	}

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
	return shadowArm{run: run, byConcept: byConcept, pred: pred}
}

// shadowLiveMetrics measures the live gate on one arm: NDCG@5 over the
// answerable set (gold by concept, resolved in that arm's store) and FPR over
// the unanswerable set. perQuery holds each answerable query's NDCG@5.
func shadowLiveMetrics(t *testing.T, a shadowArm) (ndcg, fpr float64, perQuery []float64) {
	t.Helper()
	var ndcgSum float64
	for _, q := range abmAnswerable {
		gold, ok := a.byConcept[q.goldConcept]
		if !ok {
			t.Fatalf("gold concept %q is not in the corpus — fixture drift", q.goldConcept)
		}
		v := abmNDCG5(a.run(q.query).Activations, gold)
		perQuery = append(perQuery, v)
		ndcgSum += v
	}
	falsePos := 0
	for _, q := range abmUnanswerable {
		if len(a.run(q).Activations) > 0 {
			falsePos++
		}
	}
	return ndcgSum / float64(len(abmAnswerable)), float64(falsePos) / float64(len(abmUnanswerable)), perQuery
}

// TestMeasureShadowPrecision_GraftedChain is the gate: with a declared chain in
// the corpus, the 16 nonsense probes must produce ZERO shadows, and the
// answerable set's NDCG@5 / the unanswerable set's FPR must not move from the
// graft-free baseline measured in the same run.
func TestMeasureShadowPrecision_GraftedChain(t *testing.T) {
	embedder, svc := realBGEEmbedder(t)
	embedOne := func(text string) []float32 {
		vec, err := svc.Embed(context.Background(), []string{text})
		if err != nil {
			t.Fatalf("embed %q: %v", text, err)
		}
		return vec
	}

	// Embed the corpus ONCE; both arms reuse these vectors.
	corpusVecs := make(map[string][]float32, len(bookingCorpus))
	for _, n := range bookingCorpus {
		corpusVecs[n.concept] = embedOne(n.concept + " " + n.content)
	}
	baseline := shadowBuildArm(t, embedder, embedOne, corpusVecs, false)
	grafted := shadowBuildArm(t, embedder, embedOne, corpusVecs, true)
	run, pred := grafted.run, grafted.pred

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
	// leave the live gate's numbers where the same run WITHOUT the graft puts
	// them. (The absolute gate does not depend on the pool, so the baseline FPR
	// is also TestMeasureAbstentionGate's absolute-arm FPR.)
	baseNDCG, baseFPR, basePerQuery := shadowLiveMetrics(t, baseline)
	ndcg, fpr, perQuery := shadowLiveMetrics(t, grafted)

	t.Logf("graft-free baseline (this run): NDCG@5=%.4f FPR=%.1f%% | recorded on darwin/arm64, context only: NDCG@5=%.4f FPR=%.1f%%",
		baseNDCG, 100*baseFPR, shadowRecordedBaselineNDCG, 100*shadowRecordedBaselineFPR)
	t.Logf("with a declared chain grafted in: NDCG@5=%.4f FPR=%.1f%% (delta NDCG %+.4f)",
		ndcg, 100*fpr, ndcg-baseNDCG)
	for i, q := range abmAnswerable {
		if perQuery[i] != basePerQuery[i] {
			t.Logf("  per-query NDCG@5 moved by the graft: %.4f -> %.4f  %q", basePerQuery[i], perQuery[i], q.query)
		}
	}
	// NDCG is compared with tolerance rather than for equality: the graft
	// REPLACES one gold document with a rewritten successor, so the query that
	// targeted it legitimately scores differently. FPR has no such excuse —
	// nothing about a nonsense query changed.
	if ndcg < baseNDCG-0.10 {
		t.Errorf("NDCG@5 %.4f fell more than 0.10 below the same-run graft-free baseline %.4f with a declared chain in the corpus",
			ndcg, baseNDCG)
	}
	if fpr > baseFPR+1e-9 {
		t.Errorf("FPR rose to %.1f%% from the same-run graft-free %.1f%% merely by putting a declared version chain in the corpus — "+
			"shadows must not reach the live gate at all", 100*fpr, 100*baseFPR)
	}
}
