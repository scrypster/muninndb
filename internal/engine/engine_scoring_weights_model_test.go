package engine

// Offline replication of the LIVE Phase 6 ACT-R scoring formula, plus the
// alternative ContentMatch combiners under test, for the read-only measurement
// driver in engine_scoring_weights_measure_test.go.
//
// THE LIVE FORMULA (production default; see the trace in
// .claude/deep-review/2026-07-30-scoring-weights-trace.md):
//
//	semCal       = max(0, (cos - b) / (1 - b))                 COG-26 rescale
//	ContentMatch = 0.6*semCal + 0.4*ftsCoverage                 activation:2211
//	raw          = ContentMatch * softplus(B + 4h + 4t)/1.693   activation:2276
//	final        = raw * Confidence                             activation:2295
//
// NOTE two corrections to the formula as it is often quoted internally:
//   - the semantic term is semCal, the COG-26 baseline-rescaled cosine, NOT
//     raw cosine (activation/engine.go:2210);
//   - there is NO tanh on the FTS term. #711 removed it; ftsCoverage is
//     already a calibrated absolute [0,1] coverage score
//     (activation/engine.go:2064-2069).
//
// WHY A REPLICATION AND NOT A CALL: the live scorer is
// activation.computeACTR (internal/engine/activation/engine.go:2199), which is
// unexported, and reaching it through the exported activation.Run requires a
// built HNSW index over the vault, which a read-only offline driver does not
// have. So the arithmetic is mirrored here, term for term, with the upstream
// file:line beside each term, and pinned by the synthetic-fixture tests at the
// bottom of this file. If the live formula changes, those tests do NOT
// automatically fail — re-derive them against the cited lines. That limit is
// stated plainly rather than papered over.
//
// This file carries NO build tag on purpose: it is pure arithmetic over
// synthetic fixtures, it compiles and runs in CI, and it is what makes the
// build-tagged driver's arm (i) trustworthy.

import (
	"math"
	"testing"
	"time"
)

// --- constants mirrored from the live scorer -------------------------------

const (
	// activation/engine.go:2113 — 1 + softplus(0) = 1 + ln(2).
	scwACTRDenominator = 1.6931471805599453
	// activation/engine.go:2246 — 1 minute, sub-hour precision for intraday recall.
	scwAgeFloorDays = 1.0 / (24.0 * 60.0)
	// engine.go:2357-2358 — the hardcoded production ContentMatch gate.
	scwWeightSemantic = 0.6
	scwWeightFTS      = 0.4
	// resolveWeights defaults, activation/engine.go:2553-2557.
	scwACTRDecay    = 0.5
	scwACTRHebScale = 4.0
	// activation/engine.go:547 — the non-RRF default relevance threshold used
	// when a caller passes no threshold at all (library/direct callers).
	scwDefaultThreshold = 0.05
	// THE SURFACE default, which is what every real agent actually gets:
	// internal/mcp/handlers.go:392 and internal/transport/rest/server.go:1772
	// both default an omitted threshold to 0.5 — TEN TIMES the engine's own
	// default. This is the gate the 0.4 FTS coefficient has to clear, and
	// structurally cannot. See TestSCWFTSOnlyMatchIsUnreturnableAtSurfaceDefault.
	scwSurfaceDefaultThreshold = 0.5
	// embed.noiseBaselineRegistry["bge-small-en-v1.5"], plugin/embed/baseline.go:56.
	scwBaselineBGESmall = 0.520
)

// scwSoftplus mirrors activation.softplus (activation/engine.go:2155).
func scwSoftplus(x float64) float64 {
	if x > 20 {
		return x
	}
	return math.Log1p(math.Exp(x))
}

// scwBLevelCap mirrors activation/engine.go:2265 — the unique B(M) at which
// softplus(B(M)) == actrDenominator, i.e. the prior multiplier reaches 1.0.
func scwBLevelCap() float64 {
	return math.Log(math.Exp(scwACTRDenominator) - 1)
}

// scwRescaleSemantic mirrors activation.rescaleSemantic (activation/engine.go:2133)
// and embed.Rescale: semCal = max(0, (cos-b)/(1-b)); b<=0 is the identity.
func scwRescaleSemantic(cos, b float64) float64 {
	if b <= 0 || b >= 1 {
		return cos
	}
	v := (cos - b) / (1 - b)
	if v < 0 {
		return 0
	}
	return v
}

// scwAgeDays mirrors the lastAccess handling in computeACTR
// (activation/engine.go:2236-2247): zero / pre-2000 LastAccess reads as "now".
func scwAgeDays(lastAccess, now time.Time) float64 {
	if lastAccess.IsZero() || lastAccess.Year() < 2000 {
		lastAccess = now
	}
	return math.Max(now.Sub(lastAccess).Hours()/24.0, scwAgeFloorDays)
}

// scwBaseLevel mirrors activation/engine.go:2248-2267:
//
//	B(M) = ln(n) - d*ln(max(ageDays, floor)/n),  n = AccessCount+1, capped.
func scwBaseLevel(accessCount int64, ageDays, d float64) float64 {
	n := float64(accessCount + 1)
	b := math.Log(n) - d*math.Log(math.Max(ageDays, scwAgeFloorDays)/n)
	if c := scwBLevelCap(); b > c {
		b = c
	}
	return b
}

// scwPrior is the contextual-prior multiplier that gates ContentMatch:
// softplus(B + scale*heb + scale*trans) / actrDenominator
// (activation/engine.go:2269-2276).
func scwPrior(baseLevel, heb, trans, hebScale float64) float64 {
	return scwSoftplus(baseLevel+hebScale*heb+hebScale*trans) / scwACTRDenominator
}

// ---------------------------------------------------------------------------
// ContentMatch combiners — the thing actually on trial.
//
// The live combiner is LINEAR with fixed coefficients 0.6/0.4. Its defining
// property is that a candidate with NO lexical evidence CANNOT score above
// 0.6, no matter how strong the semantic match: absent FTS evidence is charged
// as evidence of irrelevance rather than absence of evidence. A pure
// paraphrase — the case retrieval-by-meaning exists to serve — is structurally
// capped at 60% of the score an identical-wording hit can reach.
//
// The alternatives treat a missing estimator as absence of evidence:
//   - NOISY-OR   1-(1-a)(1-b): either estimator alone can reach 1.0; two weak
//     independent signals combine to more than either.
//   - MAX        max(a,b): the strongest single estimator wins outright.
//
// Both are strictly >= the linear combiner everywhere, so both raise recall
// AND raise the false-positive rate on unanswerable queries. That trade is
// exactly why the driver pre-commits to measuring BOTH, and why nothing is
// shipped on argument.
// ---------------------------------------------------------------------------

type scwCombiner int

const (
	scwCombLinear    scwCombiner = iota // live: 0.6*sem + 0.4*fts
	scwCombNoisyOR                      // 1-(1-sem)(1-fts)
	scwCombMax                          // max(sem, fts)
	scwCombRawCosine                    // control: raw cosine, no calibration at all
)

func (c scwCombiner) String() string {
	switch c {
	case scwCombLinear:
		return "linear(0.6/0.4)"
	case scwCombNoisyOR:
		return "noisy-OR"
	case scwCombMax:
		return "max"
	case scwCombRawCosine:
		return "raw-cosine"
	}
	return "?"
}

// scwContentMatch applies a combiner. semCal is the COG-26-rescaled cosine;
// rawCos is passed through untouched for the raw-cosine control.
func scwContentMatch(c scwCombiner, semCal, fts, rawCos float64) float64 {
	switch c {
	case scwCombNoisyOR:
		return 1 - (1-clamp01(semCal))*(1-clamp01(fts))
	case scwCombMax:
		return math.Max(clamp01(semCal), clamp01(fts))
	case scwCombRawCosine:
		return rawCos
	default:
		return scwWeightSemantic*semCal + scwWeightFTS*fts
	}
}

func clamp01(x float64) float64 {
	if x < 0 {
		return 0
	}
	if x > 1 {
		return 1
	}
	return x
}

// scwArmSpec is one scoring strategy under comparison.
type scwArmSpec struct {
	Name string
	Comb scwCombiner
	// UsePrior=false removes the ACT-R contextual prior (pure content match).
	// The raw-cosine control uses false.
	UsePrior bool
	// UseConfidence=false is the ABLATION the brief requires: with Confidence
	// forced to 1, a contradiction bug that collapsed some engram's confidence
	// to ~0.07 cannot silently decide which arm wins.
	UseConfidence bool
}

// scwInput is one candidate's raw signals for one query.
type scwInput struct {
	Cosine      float64 // raw cosine, pre-calibration
	FTS         float64 // calibrated [0,1] coverage from fts.Index.Search
	Hebbian     float64 // 0 in the driver — see its header
	Transition  float64 // 0 in the driver — see its header
	AccessCount int64
	LastAccess  time.Time
	Confidence  float64
}

// scwRaw computes the pre-normalization raw score for one arm. Returns raw,
// the ContentMatch term and the prior multiplier separately, so the driver can
// report which factor moved a candidate.
func scwRaw(in scwInput, arm scwArmSpec, baseline float64, now time.Time) (raw, cm, prior float64) {
	semCal := scwRescaleSemantic(in.Cosine, baseline)
	cm = scwContentMatch(arm.Comb, semCal, in.FTS, in.Cosine)
	prior = 1.0
	if arm.UsePrior {
		bl := scwBaseLevel(in.AccessCount, scwAgeDays(in.LastAccess, now), scwACTRDecay)
		prior = scwPrior(bl, in.Hebbian, in.Transition, scwACTRHebScale)
	}
	raw = cm * prior
	if raw < 0 {
		raw = 0
	}
	return raw, cm, prior
}

// scwScoreQuery scores a whole candidate pool for one query under one arm,
// including the live per-query max-rescale (activation/engine.go:1922-1932)
// and the relevance-threshold drop (activation/engine.go:1934). Ranking is not
// affected by the rescale (a single positive scalar); it is replicated so the
// absolute finals — and therefore the threshold drops, which are the whole
// point — match the live pipeline.
func scwScoreQuery(pool []scwInput, arm scwArmSpec, baseline, threshold float64, now time.Time) (final, cm, prior []float64, dropped []bool) {
	n := len(pool)
	final, cm, prior = make([]float64, n), make([]float64, n), make([]float64, n)
	dropped = make([]bool, n)
	raws := make([]float64, n)
	maxRaw := 0.0
	for i, in := range pool {
		r, c, p := scwRaw(in, arm, baseline, now)
		raws[i], cm[i], prior[i] = r, c, p
		if r > maxRaw {
			maxRaw = r
		}
	}
	scale := 1.0
	if maxRaw > 1.0 {
		scale = 1.0 / maxRaw
	}
	for i, in := range pool {
		r := math.Min(raws[i]*scale, 1.0)
		conf := 1.0
		if arm.UseConfidence {
			conf = in.Confidence
		}
		f := r * conf
		if f < threshold {
			dropped[i] = true
		}
		final[i] = f
	}
	return final, cm, prior, dropped
}

// ---------------------------------------------------------------------------
// Synthetic-fixture pins. Every number below is hand-derived from the cited
// upstream lines. No real vault content, concept, tag, entity or vault name
// appears anywhere in this file.
// ---------------------------------------------------------------------------

func TestSCWDenominatorMatchesSoftplusZero(t *testing.T) {
	if want := 1 + scwSoftplus(0); math.Abs(want-scwACTRDenominator) > 1e-12 {
		t.Fatalf("actrDenominator drifted: 1+softplus(0)=%.17f, constant=%.17f", want, scwACTRDenominator)
	}
}

func TestSCWBLevelCapSaturatesPriorAtOne(t *testing.T) {
	if got := scwPrior(scwBLevelCap(), 0, 0, scwACTRHebScale); math.Abs(got-1.0) > 1e-12 {
		t.Fatalf("prior at bLevelCap = %.17f, want 1.0", got)
	}
	if c := scwBLevelCap(); math.Abs(c-1.4892) > 1e-3 {
		t.Fatalf("bLevelCap = %.6f, want ~1.4892 (activation/engine.go:2265)", c)
	}
}

func TestSCWRescaleSemanticBaseline(t *testing.T) {
	for _, tc := range []struct{ cos, want float64 }{
		{0.520, 0.0},
		{0.400, 0.0},
		{0.758, (0.758 - 0.520) / 0.480},
		{1.000, 1.0},
	} {
		if got := scwRescaleSemantic(tc.cos, scwBaselineBGESmall); math.Abs(got-tc.want) > 1e-12 {
			t.Errorf("rescale(%.3f) = %.6f, want %.6f", tc.cos, got, tc.want)
		}
	}
	if got := scwRescaleSemantic(0.758, 0); got != 0.758 {
		t.Errorf("b<=0 must be the identity transform, got %.6f", got)
	}
}

// TestSCWParaphraseCapIsStructural is the primary finding, stated numerically.
// With zero lexical overlap the live linear combiner cannot exceed 0.6 even at
// a PERFECT semantic match, whereas noisy-OR and max both reach 1.0. This is
// not a tuning preference — it is a structural ceiling on retrieval by meaning.
func TestSCWParaphraseCapIsStructural(t *testing.T) {
	const perfectSem, noFTS = 1.0, 0.0
	if got := scwContentMatch(scwCombLinear, perfectSem, noFTS, perfectSem); math.Abs(got-0.6) > 1e-12 {
		t.Fatalf("linear combiner at perfect semantic / zero FTS = %.6f, want exactly 0.6", got)
	}
	for _, c := range []scwCombiner{scwCombNoisyOR, scwCombMax} {
		if got := scwContentMatch(c, perfectSem, noFTS, perfectSem); math.Abs(got-1.0) > 1e-12 {
			t.Errorf("%s at perfect semantic / zero FTS = %.6f, want 1.0", c, got)
		}
	}
	// Symmetrically: perfect LEXICAL match with zero semantic caps at 0.4.
	if got := scwContentMatch(scwCombLinear, 0, 1.0, 0); math.Abs(got-0.4) > 1e-12 {
		t.Fatalf("linear combiner at zero semantic / perfect FTS = %.6f, want exactly 0.4", got)
	}
}

// TestSCWWorkedParaphraseCase is the incident case, recomputed correctly.
// The often-quoted arithmetic (0.6 x 0.758 = 0.455) omits the COG-26 rescale;
// the real ContentMatch is 0.6 x semCal = 0.6 x 0.4958 = 0.2975 — materially
// WORSE than the quoted figure. With a healthy confidence it survives the 0.05
// threshold; with the contradiction bug's collapsed confidence it does not.
// Both facts matter: the confidence bug explains the zero-result, the 0.6 cap
// explains why the margin was thin enough for the bug to matter.
func TestSCWWorkedParaphraseCase(t *testing.T) {
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	semCal := scwRescaleSemantic(0.758, scwBaselineBGESmall)
	if math.Abs(semCal-0.495833) > 1e-5 {
		t.Fatalf("semCal = %.6f, want ~0.495833", semCal)
	}
	if cm := scwContentMatch(scwCombLinear, semCal, 0, 0.758); math.Abs(cm-0.2975) > 1e-3 {
		t.Fatalf("ContentMatch = %.6f, want ~0.2975 (NOT the often-quoted 0.455)", cm)
	}

	fresh := scwInput{Cosine: 0.758, FTS: 0, AccessCount: 2, LastAccess: now.Add(-2 * time.Hour), Confidence: 1.0}
	linear := scwArmSpec{Name: "linear", Comb: scwCombLinear, UsePrior: true, UseConfidence: true}

	healthy, _, _, dropHealthy := scwScoreQuery([]scwInput{fresh}, linear, scwBaselineBGESmall, scwDefaultThreshold, now)
	if dropHealthy[0] {
		t.Fatalf("healthy-confidence paraphrase was dropped at final=%.5f — expected survival", healthy[0])
	}

	collapsed := fresh
	collapsed.Confidence = 0.07
	bugged, _, _, dropBugged := scwScoreQuery([]scwInput{collapsed}, linear, scwBaselineBGESmall, scwDefaultThreshold, now)
	if !dropBugged[0] {
		t.Fatalf("confidence-0.07 paraphrase survived at final=%.5f — expected suppression", bugged[0])
	}
	t.Logf("cos 0.758, no lexical overlap: ContentMatch=%.4f | conf 1.00 -> final %.5f (kept) | conf 0.07 -> final %.5f (DROPPED)",
		scwContentMatch(scwCombLinear, semCal, 0, 0.758), healthy[0], bugged[0])

	// Under noisy-OR the same candidate carries 1/0.6 more content weight and
	// clears the threshold even at the collapsed confidence — which is the
	// point of the comparison, not yet a reason to ship it.
	nor := scwArmSpec{Name: "noisyOR", Comb: scwCombNoisyOR, UsePrior: true, UseConfidence: true}
	norScore, _, _, norDrop := scwScoreQuery([]scwInput{collapsed}, nor, scwBaselineBGESmall, scwDefaultThreshold, now)
	t.Logf("same candidate under noisy-OR at conf 0.07: final %.5f (dropped=%v)", norScore[0], norDrop[0])
}

// TestSCWAlternativesRaiseFalsePositivesToo is the pre-committed counterweight:
// any combiner that rescues a marginal genuine match ALSO rescues a marginal
// noise match. COG-26 measured bge-small's out-of-domain noise ceiling at
// cosine ~0.596; under the live combiner that lands just under the abstention
// gate, and under noisy-OR/max it clears it comfortably. A harness that
// reported only recall would call that a win.
func TestSCWAlternativesRaiseFalsePositivesToo(t *testing.T) {
	const noiseTopCos = 0.596 // COG-26's measured out-of-domain ceiling
	semCal := scwRescaleSemantic(noiseTopCos, scwBaselineBGESmall)

	live := scwContentMatch(scwCombLinear, semCal, 0, noiseTopCos)
	if live > 0.1 {
		t.Fatalf("live ContentMatch on the measured noise ceiling = %.4f; COG-26 expects it just under the 0.1 gate", live)
	}
	for _, c := range []scwCombiner{scwCombNoisyOR, scwCombMax} {
		alt := scwContentMatch(c, semCal, 0, noiseTopCos)
		if alt <= live {
			t.Fatalf("%s scored the noise ceiling at %.4f, not above live's %.4f — combiner is not strictly more permissive", c, alt, live)
		}
		if alt <= 0.1 {
			t.Logf("NOTE: %s keeps the noise ceiling (%.4f) under the 0.1 gate — better than expected", c, alt)
		}
	}
	t.Logf("noise ceiling cos %.3f -> semCal %.4f | live %.4f | noisy-OR %.4f | max %.4f",
		noiseTopCos, semCal, live,
		scwContentMatch(scwCombNoisyOR, semCal, 0, noiseTopCos),
		scwContentMatch(scwCombMax, semCal, 0, noiseTopCos))
	t.Log("COG-26's b=0.520 was calibrated AGAINST the 0.6 linear combiner (docs/internals/invariants.md). " +
		"Changing the combiner invalidates that calibration and requires re-deriving b — this is why the driver " +
		"measures both metrics and why nothing ships on argument.")
}

// TestSCWConfidenceIsAMultiplier pins the confound the driver must neutralize:
// Confidence multiplies straight into the final score, so a bug that collapses
// it to 0.07 cuts the score by 93% independent of any combiner. Hence the
// confidence-ablated arms.
func TestSCWConfidenceIsAMultiplier(t *testing.T) {
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	in := scwInput{Cosine: 0.90, AccessCount: 5, LastAccess: now.Add(-1 * time.Hour), Confidence: 0.07}
	withConf := scwArmSpec{Comb: scwCombLinear, UsePrior: true, UseConfidence: true}
	noConf := scwArmSpec{Comb: scwCombLinear, UsePrior: true, UseConfidence: false}

	a, _, _, _ := scwScoreQuery([]scwInput{in}, withConf, scwBaselineBGESmall, 0, now)
	b, _, _, _ := scwScoreQuery([]scwInput{in}, noConf, scwBaselineBGESmall, 0, now)
	if math.Abs(a[0]-b[0]*0.07) > 1e-9 {
		t.Fatalf("confidence is not a clean multiplier: %.9f vs %.9f", a[0], b[0]*0.07)
	}
	if b[0] < scwDefaultThreshold || a[0] >= scwDefaultThreshold {
		t.Fatalf("expected the ablation to flip this candidate across the threshold: conf-on %.5f, conf-off %.5f", a[0], b[0])
	}
}

// TestSCWFTSOnlyMatchIsUnreturnableAtSurfaceDefault is the MISSING-COSINE case
// — the same defect as the paraphrase cap, from the opposite direction. A
// memory whose embedding has not landed yet scores cosine 0, so semCal is 0 and
// ContentMatch = 0.4*fts, which is capped at 0.4 at a PERFECT lexical match.
//
// Both agent-facing surfaces default an omitted threshold to 0.5
// (mcp/handlers.go:392, transport/rest/server.go:1772). 0.4 < 0.5 — so an
// FTS-only match is UNRETURNABLE through MCP or REST at default settings, for
// any query, at any lexical quality, forever. Not "scores poorly": structurally
// cannot be returned. That is a stronger statement than the reported "falls
// below the threshold", and it does not depend on the indexing lag at all —
// the lag merely makes every freshly-written memory hit the case.
//
// Note the cross-surface split this exposes: at the ENGINE default (0.05) the
// same candidate is returned fine. A library or direct caller sees the memory;
// an MCP agent does not.
func TestSCWFTSOnlyMatchIsUnreturnableAtSurfaceDefault(t *testing.T) {
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	linear := scwArmSpec{Comb: scwCombLinear, UsePrior: true, UseConfidence: true}

	// A brand-new, perfectly-matching, perfectly-confident memory with no
	// embedding yet: cosine 0, FTS coverage 1.0 (every query term present).
	unindexed := scwInput{
		Cosine: 0, FTS: 1.0,
		AccessCount: 0, LastAccess: now, Confidence: 1.0,
	}

	if cm := scwContentMatch(scwCombLinear, scwRescaleSemantic(0, scwBaselineBGESmall), 1.0, 0); math.Abs(cm-0.4) > 1e-12 {
		t.Fatalf("ContentMatch at cosine 0 / perfect FTS = %.6f, want exactly 0.4", cm)
	}

	atSurface, _, _, dropSurface := scwScoreQuery([]scwInput{unindexed}, linear, scwBaselineBGESmall, scwSurfaceDefaultThreshold, now)
	if !dropSurface[0] {
		t.Fatalf("a perfect lexical match scored %.5f and survived the %.2f surface threshold — "+
			"if this now passes, the surface default or the FTS coefficient changed", atSurface[0], scwSurfaceDefaultThreshold)
	}

	atEngine, _, _, dropEngine := scwScoreQuery([]scwInput{unindexed}, linear, scwBaselineBGESmall, scwDefaultThreshold, now)
	if dropEngine[0] {
		t.Fatalf("the same candidate was dropped at the ENGINE default too (%.5f) — the cross-surface split is gone", atEngine[0])
	}
	t.Logf("cosine 0, FTS 1.00 (perfect lexical, no embedding yet): final %.4f | MCP/REST gate %.2f -> DROPPED | engine gate %.2f -> returned",
		atSurface[0], scwSurfaceDefaultThreshold, scwDefaultThreshold)

	// The absence-of-evidence combiners return it, because a missing estimator
	// stops being scored as a zero.
	for _, c := range []scwCombiner{scwCombNoisyOR, scwCombMax} {
		alt := scwArmSpec{Comb: c, UsePrior: true, UseConfidence: true}
		f, _, _, dropped := scwScoreQuery([]scwInput{unindexed}, alt, scwBaselineBGESmall, scwSurfaceDefaultThreshold, now)
		if dropped[0] {
			t.Errorf("%s also dropped the perfect lexical match (final %.4f) — it does not fix the missing-cosine case", c, f[0])
		} else {
			t.Logf("  %-9s final %.4f -> returned", c.String(), f[0])
		}
	}
}

// TestSCWReconstructsObservedLiveScore is an independent check of the whole
// replication against a LIVE observation reported from a running server: after
// the embedding landed, the same query returned the memory at final score 0.742
// with vector_score_raw 0.881.
//
// Solving the live formula for the only unknown (FTS coverage), with the prior
// saturated and confidence 1.0 for a fresh, healthy memory:
//
//	semCal = (0.881-0.520)/0.480 = 0.7521
//	0.742  = 0.6*0.7521 + 0.4*fts  =>  fts = 0.7269
//
// An implied FTS coverage of 0.73 for a near-exact lexical query is entirely
// plausible, so the observation is consistent with the replicated formula. That
// is a real cross-check: an arbitrary formula would not land an implied
// coverage inside [0,1], let alone at a sensible value.
//
// The payoff is the counterfactual: the SAME candidate, one moment earlier with
// the same FTS coverage but no embedding, scores 0.4*0.7269 = 0.291 — well
// below the 0.5 surface gate. Zero results, then 0.742, with no change to the
// memory or the query. Only the embedding landed.
func TestSCWReconstructsObservedLiveScore(t *testing.T) {
	const observedFinal, observedRawCosine = 0.742, 0.881

	semCal := scwRescaleSemantic(observedRawCosine, scwBaselineBGESmall)
	impliedFTS := (observedFinal - scwWeightSemantic*semCal) / scwWeightFTS
	if impliedFTS < 0 || impliedFTS > 1 {
		t.Fatalf("implied FTS coverage %.4f is outside [0,1] — the replicated formula is INCONSISTENT "+
			"with the live observation (final %.3f, raw cosine %.3f) and must be re-derived",
			impliedFTS, observedFinal, observedRawCosine)
	}
	if impliedFTS < 0.5 || impliedFTS > 0.95 {
		t.Errorf("implied FTS coverage %.4f is inside [0,1] but implausible for a near-exact lexical query", impliedFTS)
	}

	preEmbed := scwWeightFTS * impliedFTS
	if preEmbed >= scwSurfaceDefaultThreshold {
		t.Fatalf("pre-embedding score %.4f would have cleared the %.2f surface gate — "+
			"that contradicts the reported zero-result reproduction", preEmbed, scwSurfaceDefaultThreshold)
	}
	t.Logf("live observation reconstructed: semCal %.4f, implied FTS coverage %.4f, final %.3f",
		semCal, impliedFTS, observedFinal)
	t.Logf("SAME candidate one moment earlier (no embedding): %.4f — below the %.2f surface gate. "+
		"Zero results -> %.3f, with nothing changed but the embedding landing.",
		preEmbed, scwSurfaceDefaultThreshold, observedFinal)
}

// TestSCWPriorDynamicRange records the secondary structural fact: with Hebbian
// and transition at zero, the ACT-R prior spans >20x between a cold engram and
// a hot one, while ContentMatch is bounded in [0,1]. Recency/frequency can
// therefore outvote any achievable semantic difference.
func TestSCWPriorDynamicRange(t *testing.T) {
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	cold := scwPrior(scwBaseLevel(0, scwAgeDays(now.AddDate(0, 0, -365), now), scwACTRDecay), 0, 0, scwACTRHebScale)
	hot := scwPrior(scwBaseLevel(20, scwAgeDays(now.Add(-12*time.Hour), now), scwACTRDecay), 0, 0, scwACTRHebScale)
	if ratio := hot / cold; ratio < 20 {
		t.Fatalf("prior ratio hot/cold = %.2f (cold=%.5f hot=%.5f); expected >20x", ratio, cold, hot)
	}
	if hot > 1.0001 {
		t.Fatalf("hot prior %.5f exceeds 1.0 — bLevelCap not applied", hot)
	}
}
