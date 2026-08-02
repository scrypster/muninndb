//go:build localassets

package activation_test

import (
	"context"
	"math"
	"sort"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/engine/activation"
	"github.com/scrypster/muninndb/internal/storage"
)

// ---------------------------------------------------------------------------
// TestCognitionTrial_ArmReconstructionFidelity — the cognition trial's other
// pre-registered U3 stop condition. If this fails, every arm number the trial
// reports is fiction.
//
// WHAT THE TRIAL DOES, AND WHY IT NEEDS THIS. The trial compares four arms:
//
//	FULL                 everything on                       — live run
//	NO-PAS               no transition candidates injected   — live run
//	NO-HEBBIAN           hebbianBoost := 0                   — ARITHMETIC on FULL
//	CONTENT-MATCH-ONLY   contextualPrior := actrDenominator  — ARITHMETIC on NO-PAS
//
// PAS must be a live run because it INJECTS CANDIDATES into the pool, changing
// the candidate SET, which no offline arithmetic can reproduce. Hebbian and the
// base-level prior only RE-WEIGHT candidates already in the fused list, so
// zeroing their components offline is exact rather than simulated — the same
// method (and the same fidelity obligation) as
// TestRecallQuerySet_ReconstructionFidelity in this package.
//
// This file states the arm arithmetic once and proves it reproduces the live
// pipeline. The trial harness (`//go:build cognitiontrial`) re-derives the same
// formulas and ALSO re-checks them per candidate against the real corpus before
// computing any arm — a run-time check on the actual data, which is strictly
// stronger than a link to this synthetic one.
//
// THE ALGEBRA, and the reason a clean CONTENT-MATCH-ONLY arm exists at all:
//
//	B(M)      = min(ln n - d*ln(max(ageDays, ageFloor)/n), bLevelCap)
//	prior     = softplus(B(M) + scale*(hebbian + transition)) / D
//	rawUnscl  = contentMatch * prior
//
// with D = actrDenominator and bLevelCap = ln(e^D - 1) the unique B at which
// softplus(B) = D. So at the cap with no boosts, prior = 1 exactly and
// rawUnscl = contentMatch — CONTENT-MATCH-ONLY is an algebraic identity, not an
// approximation, and it touches NEITHER the content gate (COG-5) NOR the
// threshold (COG-6).
//
// PRIVACY: every engram, query and number below is synthetic and authored here.
// ---------------------------------------------------------------------------

// Mirrors of the ACT-R constants in activation/engine.go. Deliberately
// duplicated rather than exported (the same choice recall_queryset_measure_test
// makes): if engine.go's values drift, this test fails loudly instead of
// silently tracking them and blessing a formula nobody reviewed.
const (
	ctDenominator  = 1.6931471805599453 // 1 + softplus(0) = 1 + ln 2
	ctACTRDecay    = 0.5                // resolvedWeights default d
	ctAgeFloorDays = 1.0 / (24.0 * 60.0)
)

var ctBLevelCap = math.Log(math.Exp(ctDenominator) - 1)

func ctSoftplus(x float64) float64 { return math.Log(1 + math.Exp(x)) }

// ctBaseLevel recomputes B(M) from the engram fields the pipeline reads. Every
// input is on the returned engram, so no pipeline internal is assumed.
func ctBaseLevel(accessCount uint32, lastAccess, now time.Time) float64 {
	ageDays := math.Max(now.Sub(lastAccess).Hours()/24.0, ctAgeFloorDays)
	n := float64(accessCount + 1)
	b := math.Log(n) - ctACTRDecay*math.Log(math.Max(ageDays, ctAgeFloorDays)/n)
	return math.Min(b, ctBLevelCap)
}

// ctArm is one ablation. It receives the captured components and returns the
// UNSCALED raw score. Note what no arm may touch: contentMatch (COG-5) and the
// threshold (COG-6) are inputs, never outputs.
type ctArm struct {
	name string
	raw  func(contentMatch, baseLevel, hebbian, transition, hebScale float64) float64
}

var (
	ctArmFull = ctArm{"FULL", func(cm, b, h, tr, s float64) float64 {
		return cm * ctSoftplus(b+s*h+s*tr) / ctDenominator
	}}
	ctArmNoHebbian = ctArm{"NO-HEBBIAN", func(cm, b, _, tr, s float64) float64 {
		return cm * ctSoftplus(b+s*tr) / ctDenominator
	}}
	ctArmContentOnly = ctArm{"CONTENT-MATCH-ONLY", func(cm, _, _, _, _ float64) float64 {
		return cm // prior := D, i.e. contextualPrior/D = 1
	}}
)

// ctCorpus is a small synthetic corpus with DELIBERATELY VARIED access counts
// and ages, so the base-level reconstruction is exercised both at the cap and
// well below it. A corpus of uniformly-fresh engrams would sit at bLevelCap
// everywhere and would not test B(M) at all.
var ctCorpus = []struct {
	concept     string
	content     string
	accessCount uint32
	ageDays     float64
}{
	{"kiln firing schedule", "the bisque kiln ramps at eighty degrees an hour to cone zero four", 0, 0},
	{"glaze mixing ratio", "the celadon glaze is mixed at one part ash to two parts feldspar", 3, 40},
	{"wedging table height", "the wedging table sits at hip height so the potter can use body weight", 0, 200},
	{"clay body shrinkage", "the stoneware body shrinks about twelve percent from wet to fired", 11, 5},
	{"kiln shelf coating", "kiln shelves are coated with alumina hydrate before every glaze firing", 1, 90},
	{"slip trailing nozzle", "the slip trailer uses a one millimetre nozzle for fine line work", 0, 12},
	{"bisque cooling window", "the bisque load is not opened until the pyrometer reads below two hundred", 7, 1},
	{"studio humidity target", "the damp room is held near ninety percent so trimming can wait a day", 2, 365},
}

type ctCandidate struct {
	id           storage.ULID
	contentMatch float64
	baseLevel    float64
	hebbian      float64
	transition   float64
	// as reported by the pipeline
	gotRaw, gotAbsolute, gotConfidence float64
	rank                               int
}

func TestCognitionTrial_ArmReconstructionFidelity(t *testing.T) {
	store := newStubStore()
	now := time.Now()

	ids := make([]storage.ULID, 0, len(ctCorpus))
	ftsHits := make([]activation.ScoredID, 0, len(ctCorpus))
	for i, c := range ctCorpus {
		eng := &storage.Engram{
			Concept:     c.concept,
			Content:     c.content,
			Embedding:   []float32{0.1, 0.1, 0.1, 0.1, 0.1, 0.1, 0.1, 0.1},
			Confidence:  1.0,
			Stability:   30.0,
			AccessCount: c.accessCount,
			CreatedAt:   now.Add(-time.Duration(c.ageDays*24) * time.Hour),
			LastAccess:  now.Add(-time.Duration(c.ageDays*24) * time.Hour),
		}
		store.writeEngram(eng)
		ids = append(ids, eng.ID)
		// Descending FTS scores so contentMatch varies across candidates.
		ftsHits = append(ftsHits, activation.ScoredID{ID: eng.ID, Score: 1.0 - 0.07*float64(i)})
	}

	// A primer that is "recently co-activated", and edges into it from every
	// third engram — so some candidates carry a Hebbian boost and some do not.
	// The NO-HEBBIAN arm is only meaningful if both kinds are present.
	primer := &storage.Engram{
		Concept:    "studio opening checklist",
		Content:    "the studio opening checklist covers kiln vents, damp room and wheel bearings",
		Embedding:  []float32{0.1, 0.1, 0.1, 0.1, 0.1, 0.1, 0.1, 0.1},
		Confidence: 1.0,
		Stability:  30.0,
		CreatedAt:  now,
		LastAccess: now,
	}
	store.writeEngram(primer)
	for i, id := range ids {
		if i%3 == 0 {
			store.assocs[id] = []storage.Association{
				{TargetID: primer.ID, Weight: float32(0.4 + 0.15*float64(i%4)), RelType: storage.RelRelatesTo},
			}
		}
	}

	eng := newTestEngine(store, &stubFTS{results: ftsHits}, &emptyHNSW{})
	t.Cleanup(eng.Close)
	eng.AssocLog().Record(activation.LogEntry{
		VaultID: 7, At: now, EngramIDs: []storage.ULID{primer.ID}, Scores: []float64{1.0},
	})

	res, err := eng.Run(context.Background(), &activation.ActivateRequest{
		VaultID:            7,
		Context:            []string{"kiln firing schedule"},
		Threshold:          -1, // diagnostic bypass: score every candidate
		MaxResults:         100,
		CandidatesPerIndex: 60,
		HebbianEnabled:     true,
		// PASEnabled deliberately false: PAS is a LIVE arm, never reconstructed
		// (it changes the candidate set). See the header.
		Weights: &activation.Weights{
			SemanticSimilarity: 0.6,
			FullTextRelevance:  0.4,
			UseACTR:            true,
		},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(res.Activations) < len(ctCorpus) {
		t.Fatalf("pipeline returned %d rows for a %d-engram corpus — the pool truncated, "+
			"and the fidelity check would be comparing different candidate sets",
			len(res.Activations), len(ctCorpus))
	}

	hebScale := float64(activation.DefaultACTRHebScale)
	cands := make([]ctCandidate, 0, len(res.Activations))
	boosted := 0
	for rank, a := range res.Activations {
		c := a.Components
		if c.HebbianBoost > 0 {
			boosted++
		}
		cands = append(cands, ctCandidate{
			id:            a.Engram.ID,
			contentMatch:  c.ContentMatch,
			baseLevel:     ctBaseLevel(a.Engram.AccessCount, a.Engram.LastAccess, now),
			hebbian:       c.HebbianBoost,
			transition:    c.TransitionBoost,
			gotRaw:        c.Raw,
			gotAbsolute:   c.AbsoluteScore,
			gotConfidence: c.Confidence,
			rank:          rank,
		})
	}
	if boosted == 0 {
		t.Fatal("no candidate carried a Hebbian boost — the NO-HEBBIAN arm would be a " +
			"no-op and this test would prove nothing")
	}
	if boosted == len(cands) {
		t.Fatal("EVERY candidate carried a Hebbian boost — the fixture cannot show that " +
			"the ablation is selective")
	}

	// --- 1. THE ALGEBRAIC IDENTITY the CONTENT-MATCH-ONLY arm rests on -------
	if got := ctSoftplus(ctBLevelCap) / ctDenominator; math.Abs(got-1.0) > 1e-12 {
		t.Fatalf("softplus(bLevelCap)/D = %.15f, want 1 — CONTENT-MATCH-ONLY is not the "+
			"identity it claims to be, and its numbers mean nothing", got)
	}

	// --- 2. THE PRIOR ITSELF: reconstructed FULL reproduces reported Raw ----
	// This is the assertion that actually tests the reconstructed prior, and it
	// has to be made on Raw rather than on AbsoluteScore. AbsoluteScore is
	// min(rawUnscaled, contentMatch, 1) x Confidence: whenever the prior is >= 1
	// — which it is for every engram at bLevelCap, i.e. every fresh or
	// frequently-accessed one — the min() clamps the prior straight back out of
	// view, and an assertion on AbsoluteScore alone is BLIND to a wrong
	// hebScale, a wrong cap and a wrong softplus alike. (Verified: two
	// deliberate perturbations of computeACTR both passed an AbsoluteScore-only
	// version of this test.)
	//
	// Raw as reported is min(rawUnscaled * scale, 1.0) where scale = 1/maxRaw if
	// any candidate saturated above 1.0, else 1 — all of which the
	// reconstruction can predict, because maxRaw is just the max reconstructed
	// rawUnscaled over the same pool.
	const tol = 1e-6
	reconRaw := make([]float64, len(cands))
	reconMax := 0.0
	for i, c := range cands {
		reconRaw[i] = ctArmFull.raw(c.contentMatch, c.baseLevel, c.hebbian, c.transition, hebScale)
		if reconRaw[i] > reconMax {
			reconMax = reconRaw[i]
		}
	}
	scalePred := 1.0
	if reconMax > 1.0 {
		scalePred = 1.0 / reconMax
	}
	for i, c := range cands {
		wantRaw := math.Min(reconRaw[i]*scalePred, 1.0)
		if math.Abs(wantRaw-c.gotRaw) > tol {
			t.Errorf("%s: reconstructed Raw %.12f vs pipeline %.12f (cm=%.6f B=%.6f "+
				"heb=%.6f trans=%.6f scale=%.9f) — the FULL arm does not reproduce the "+
				"live pipeline, so no ablation of it is trustworthy",
				c.id, wantRaw, c.gotRaw, c.contentMatch, c.baseLevel, c.hebbian,
				c.transition, scalePred)
		}
		// AbsoluteScore too — weaker (see above) but it is what the trial's
		// abstention-adjacent columns read, so a divergence must not pass.
		wantAbs := math.Min(math.Min(reconRaw[i], c.contentMatch), 1.0) * c.gotConfidence
		if math.Abs(wantAbs-c.gotAbsolute) > tol {
			t.Errorf("%s: reconstructed absolute %.12f vs pipeline %.12f",
				c.id, wantAbs, c.gotAbsolute)
		}
	}

	// The fixture must actually exercise the saturating branch; otherwise the
	// scale prediction above is trivially 1 and half the assertion is untested.
	if reconMax <= 1.0 {
		t.Errorf("no candidate saturated (max reconstructed raw %.6f <= 1) — the "+
			"per-query rescale branch is not exercised", reconMax)
	}

	// --- 3. RANKING: the reconstruction orders the pool as the pipeline did --
	// The pipeline ranks by Final = rawUnscaled x scale x Confidence, and a
	// positive per-query constant cannot change an order — so agreeing on the
	// ORDER is exactly the claim an NDCG column needs, and a wrong hebScale, a
	// wrong cap or a wrong softplus would each break it.
	recon := append([]ctCandidate(nil), cands...)
	sort.SliceStable(recon, func(i, j int) bool {
		ri := ctArmFull.raw(recon[i].contentMatch, recon[i].baseLevel, recon[i].hebbian, recon[i].transition, hebScale) * recon[i].gotConfidence
		rj := ctArmFull.raw(recon[j].contentMatch, recon[j].baseLevel, recon[j].hebbian, recon[j].transition, hebScale) * recon[j].gotConfidence
		return ri > rj
	})
	for i, c := range recon {
		if c.rank != i {
			t.Errorf("ranking disagreement at position %d: reconstruction puts %s "+
				"(pipeline rank %d) here", i, c.id, c.rank)
		}
	}

	// --- 4. COG-5 / COG-6: the arms move ONLY the cognitive prior -----------
	// Every arm consumes the identical captured contentMatch, and none of them
	// takes a threshold at all. Asserted rather than asserted-in-prose: run
	// each arm at a deliberately absurd prior and require the output to be
	// unchanged in contentMatch terms.
	for _, c := range cands {
		if got := ctArmContentOnly.raw(c.contentMatch, 99, 99, 99, 99); got != c.contentMatch {
			t.Errorf("%s: CONTENT-MATCH-ONLY returned %.12f for contentMatch %.12f — the "+
				"arm must be the content gate itself, untouched", c.id, got, c.contentMatch)
		}
	}

	// --- 5. THE ABLATION ACTUALLY BITES ------------------------------------
	strictlyLower := 0
	for _, c := range cands {
		full := ctArmFull.raw(c.contentMatch, c.baseLevel, c.hebbian, c.transition, hebScale)
		noHeb := ctArmNoHebbian.raw(c.contentMatch, c.baseLevel, c.hebbian, c.transition, hebScale)
		if noHeb > full+1e-12 {
			t.Errorf("%s: NO-HEBBIAN (%.12f) scored ABOVE FULL (%.12f) — removing a "+
				"non-negative boost cannot raise a score", c.id, noHeb, full)
		}
		if c.hebbian > 0 && noHeb < full-1e-12 {
			strictlyLower++
		}
		if c.hebbian == 0 && math.Abs(noHeb-full) > 1e-12 {
			t.Errorf("%s: NO-HEBBIAN changed a candidate with no Hebbian boost "+
				"(%.12f -> %.12f) — the ablation is touching something else",
				c.id, full, noHeb)
		}
	}
	if strictlyLower == 0 {
		t.Error("NO-HEBBIAN changed nothing anywhere — the arm is inert and cannot " +
			"measure Hebbian's contribution")
	}
}
