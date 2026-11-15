package engine

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/scrypster/muninndb/internal/auth"
	"github.com/scrypster/muninndb/internal/engine/activation"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// TestRecallModeCensus is the recall-mode preset census (recent-mode
// orientation design v2, §3.1 item 4; COG-6 amendment). Every preset × {wire
// req.Mode, vault-default RecallMode} × {actr, weighted_sum, rrf} cell is run
// through resolveScoringPlan — the exact function activateCore uses — with no
// caller weights and no caller threshold, and checked against five rules:
//
//	R1 fusion preserved: the formula that runs equals the vault's formula,
//	   unless the preset sets DisableACTR (→ weighted_sum); on rrf, rrf wins.
//	R2 read set: every nonzero scalar on the plan that differs from the
//	   vault-default plan for the same cell is READ by the formula that runs.
//	   ACT-R reads {Sem, FTS, ACTRDecay, ACTRHebScale}; weighted_sum reads
//	   {Sem, FTS, Decay, Hebbian, Access, Recency}; rrf reads none of them.
//	R3 bar: the effective threshold is not below cog6DefaultThreshold for the
//	   formula that runs.
//	R4 ACT-R gate weights: on an ACT-R plan no preset changes Sem/FTS from the
//	   vault default, raises the threshold, or alters ACTRDecay/ACTRHebScale —
//	   the values COG-26's b=0.520 was calibrated against.
//	R5 alias: `recent`'s plan is reflect.DeepEqual to `balanced`'s plan, for
//	   every fusion and both paths (principle #6, the #599 pinning shape).
//
// Known violations are allowlisted explicitly, each naming its follow-up, and
// the test FAILS if an allowlisted cell stops violating — so a fix for F1/F2
// cannot leave a stale entry behind to excuse a future regression.
//
// The presets are enumerated from auth.RecallModeNames(), not a hand list, so
// a new preset is censused the moment it is defined.

type scoringPlan struct {
	W         activation.Weights
	Threshold float64
	HopDepth  int
}

const (
	formulaACTR        = "actr"
	formulaWeightedSum = "weighted_sum"
	formulaRRF         = "rrf"
)

// planFormula mirrors what activation actually runs: phase 6 dispatches on
// UseRRFFusion first, and resolveWeights derives UseACTR as !DisableACTR
// (activation/engine.go resolveWeights), so DisableACTR is the switch.
func planFormula(w *activation.Weights) string {
	switch {
	case w.UseRRFFusion:
		return formulaRRF
	case !w.DisableACTR:
		return formulaACTR
	default:
		return formulaWeightedSum
	}
}

// censusBaseline is a representative registered-model b; the plan copies it
// through unchanged, so the census result does not depend on its value.
const censusBaseline = 0.52

// thresholdEpsilon absorbs float32→float64 widening: preset thresholds are
// float32 (deep's 0.1 arrives as 0.10000000149), the COG-6 defaults float64.
const thresholdEpsilon = 1e-6

func censusPlan(fusion, wireMode, vaultMode string) scoringPlan {
	cfg := &auth.PlasticityConfig{}
	if fusion != formulaACTR {
		f := fusion
		cfg.ScoringFusion = &f
	}
	if vaultMode != "" {
		m := vaultMode
		cfg.RecallMode = &m
	}
	resolved := auth.ResolvePlasticity(cfg)
	w, thr, hops := resolveScoringPlan(&mbp.ActivateRequest{Mode: wireMode}, &resolved, censusBaseline, false)
	return scoringPlan{W: *w, Threshold: thr, HopDepth: hops}
}

// scalarReads lists which formulas read each legacy/ACT-R scalar.
var scalarReads = map[string]map[string]bool{
	"SemanticSimilarity": {formulaACTR: true, formulaWeightedSum: true},
	"FullTextRelevance":  {formulaACTR: true, formulaWeightedSum: true},
	"DecayFactor":        {formulaWeightedSum: true},
	"HebbianBoost":       {formulaWeightedSum: true},
	"AccessFrequency":    {formulaWeightedSum: true},
	"Recency":            {formulaWeightedSum: true},
	"ACTRDecay":          {formulaACTR: true},
	"ACTRHebScale":       {formulaACTR: true},
}

func scalarValue(w *activation.Weights, name string) float32 {
	if name == "ACTRHebScale" {
		if w.ACTRHebScale == nil {
			return 0
		}
		return *w.ACTRHebScale
	}
	return float32(reflect.ValueOf(*w).FieldByName(name).Float())
}

type censusCell struct{ preset, path, fusion string }

func (c censusCell) String() string { return c.preset + "/" + c.path + "/" + c.fusion }

// censusViolations returns the rules (R1..R4) the cell's plan violates.
func censusViolations(preset auth.RecallModePreset, fusion string, plan, base scoringPlan) map[string]string {
	v := map[string]string{}
	got := planFormula(&plan.W)
	baseFormula := planFormula(&base.W)

	// R1
	want := baseFormula
	if fusion != formulaRRF && preset.DisableACTR {
		want = formulaWeightedSum
	}
	if got != want {
		v["R1"] = fmt.Sprintf("formula %s, vault formula %s", got, want)
	}

	// R2
	var unread []string
	names := make([]string, 0, len(scalarReads))
	for n := range scalarReads {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		pv, bv := scalarValue(&plan.W, n), scalarValue(&base.W, n)
		if pv != 0 && pv != bv && !scalarReads[n][got] {
			unread = append(unread, fmt.Sprintf("%s=%g (vault %g)", n, pv, bv))
		}
	}
	if len(unread) > 0 {
		v["R2"] = fmt.Sprintf("%s does not read %s", got, strings.Join(unread, ", "))
	}

	// R3
	if bar := cog6DefaultThreshold(got == formulaRRF, got == formulaACTR); plan.Threshold < bar-thresholdEpsilon {
		v["R3"] = fmt.Sprintf("threshold %g < COG-6 default %g for %s", plan.Threshold, bar, got)
	}

	// R4
	if got == formulaACTR {
		var diffs []string
		for _, n := range []string{"SemanticSimilarity", "FullTextRelevance", "ACTRDecay", "ACTRHebScale"} {
			if pv, bv := scalarValue(&plan.W, n), scalarValue(&base.W, n); pv != bv {
				diffs = append(diffs, fmt.Sprintf("%s %g (vault %g)", n, pv, bv))
			}
		}
		if plan.Threshold > base.Threshold+thresholdEpsilon {
			diffs = append(diffs, fmt.Sprintf("threshold raised %g > %g", plan.Threshold, base.Threshold))
		}
		if len(diffs) > 0 {
			v["R4"] = strings.Join(diffs, "; ")
		}
	}
	return v
}

// knownCensusViolations: cell → rule → follow-up. Each entry must still be
// violated (staleness guard below).
var knownCensusViolations = map[censusCell]map[string]string{
	// F1: `semantic` forces the legacy weighted_sum formula but carries a 0.3
	// bar, below the 0.5 validated for that formula.
	{"semantic", "wire", formulaACTR}:         {"R3": "F1"},
	{"semantic", "wire", formulaWeightedSum}:  {"R3": "F1"},
	{"semantic", "vault", formulaACTR}:        {"R3": "F1"},
	{"semantic", "vault", formulaWeightedSum}: {"R3": "F1"},
	// F2: `deep`'s 0.1 lowers weighted_sum's 0.5 bar (recency-spam risk).
	{"deep", "wire", formulaWeightedSum}:  {"R3": "F2"},
	{"deep", "vault", formulaWeightedSum}: {"R3": "F2"},
}

func TestRecallModeCensus(t *testing.T) {
	fusions := []string{formulaACTR, formulaWeightedSum, formulaRRF}
	paths := []string{"wire", "vault"}

	planFor := func(name, path, fusion string) scoringPlan {
		if path == "wire" {
			return censusPlan(fusion, name, "")
		}
		return censusPlan(fusion, "", name)
	}

	names := auth.RecallModeNames()
	for _, required := range []string{"semantic", "recent", "balanced", "deep"} {
		if _, err := auth.LookupRecallMode(required); err != nil {
			t.Fatalf("census expects preset %q: %v", required, err)
		}
	}

	seen := map[censusCell]bool{}
	for _, name := range names {
		preset, err := auth.LookupRecallMode(name)
		if err != nil {
			t.Fatalf("LookupRecallMode(%q): %v", name, err)
		}
		for _, path := range paths {
			for _, fusion := range fusions {
				cell := censusCell{name, path, fusion}
				seen[cell] = true
				plan := planFor(name, path, fusion)
				base := censusPlan(fusion, "", "") // the vault default: no wire mode, no vault RecallMode
				got := censusViolations(preset, fusion, plan, base)
				allowed := knownCensusViolations[cell]
				for rule, detail := range got {
					if _, ok := allowed[rule]; ok {
						continue
					}
					t.Errorf("%s violates %s: %s", cell, rule, detail)
				}
				for rule, followUp := range allowed {
					if _, still := got[rule]; !still {
						t.Errorf("%s no longer violates %s — remove its knownCensusViolations entry. This entry pins a NUMBER; "+
							"whether %s is actually fixed is decided by TestRecallModeCensus_ContentFreeAbstains", cell, rule, followUp)
					}
				}
			}
		}
	}
	for cell := range knownCensusViolations {
		if !seen[cell] {
			t.Errorf("knownCensusViolations entry %s names no censused cell", cell)
		}
	}

	// R5b: a WIRE mode replaces the vault's default recall mode, so wire
	// `recent` equals wire `balanced` (engine defaults) on EVERY vault —
	// including vaults whose default mode is semantic or deep, where neither
	// equals a no-mode recall. "recent equals the vault default" is false.
	for _, vaultMode := range append([]string{""}, names...) {
		for _, fusion := range fusions {
			rec, bal := censusPlan(fusion, "recent", vaultMode), censusPlan(fusion, "balanced", vaultMode)
			if !reflect.DeepEqual(rec, bal) {
				t.Errorf("wire recent on a %q-default %s vault violates R5b (must equal wire balanced):\nrecent   %+v\nbalanced %+v",
					vaultMode, fusion, rec, bal)
			}
		}
	}

	// R5: recent is a pinned alias of balanced on every fusion and path.
	for _, path := range paths {
		for _, fusion := range fusions {
			rec, bal := planFor("recent", path, fusion), planFor("balanced", path, fusion)
			if !reflect.DeepEqual(rec, bal) {
				t.Errorf("recent/%s/%s violates R5 (alias of balanced):\nrecent   %+v (actr heb %v)\nbalanced %+v (actr heb %v)",
					path, fusion, rec, scalarValue(&rec.W, "ACTRHebScale"), bal, scalarValue(&bal.W, "ACTRHebScale"))
			}
		}
	}
}

// TestRecallModeCensus_RejectsRecencyCarryingPreset proves the rules catch the
// shapes the design rejected, by feeding censusViolations hand-built plans
// (the preset map is not injectable, which is the point of it):
//   - develop's `recent` {Sem .3, Recency .7, MaxHops 1, Thr .2} on ACT-R —
//     a zero-base vector whose Recency ACT-R never reads (R2), with the gate
//     weights, decay and bar all moved (R4);
//   - v1's rejected {Recency .7, Sem .3, DisableACTR, Thr .2} — R3 on every
//     non-rrf vault (0.2 < weighted_sum's 0.5).
func TestRecallModeCensus_RejectsRecencyCarryingPreset(t *testing.T) {
	base := censusPlan(formulaACTR, "", "")

	oldRecent := auth.RecallModePreset{Recency: 0.7, SemanticSimilarity: 0.3, MaxHops: 1, Threshold: 0.2}
	oldPlan := scoringPlan{W: activation.Weights{SemanticSimilarity: 0.3, Recency: 0.7, UseACTR: true}, Threshold: 0.2, HopDepth: 1}
	v := censusViolations(oldRecent, formulaACTR, oldPlan, base)
	for _, rule := range []string{"R2", "R4"} {
		if _, ok := v[rule]; !ok {
			t.Errorf("develop's recent shape on ACT-R must violate %s; got %v", rule, v)
		}
	}

	v1 := auth.RecallModePreset{Recency: 0.7, SemanticSimilarity: 0.3, Threshold: 0.2, DisableACTR: true}
	for _, fusion := range []string{formulaACTR, formulaWeightedSum} {
		b := censusPlan(fusion, "", "")
		p := scoringPlan{W: activation.Weights{SemanticSimilarity: 0.3, Recency: 0.7, DisableACTR: true}, Threshold: 0.2}
		if _, ok := censusViolations(v1, fusion, p, b)["R3"]; !ok {
			t.Errorf("v1's rejected variant on %s must violate R3", fusion)
		}
	}
}

// TestRecallModeCensus_ContentFreeAbstains is the BEHAVIOURAL census: the
// structural rules above pin numbers, and a preset can satisfy every one of
// them while still returning rows for a query that matches nothing — e.g.
// {DisableACTR: true, Threshold: 0.5} as an ACT-R vault default scores fresh
// engrams ~0.7 on recency/decay alone. So for every preset (enumerated from
// auth.RecallModeNames), on both paths (wire req.Mode; vault-default
// RecallMode) and on actr and weighted_sum vaults, a nonsense query and a
// content-free orientation query must ABSTAIN. rrf is excluded: its default
// bar is rank-based (~0.001) and admits rows for any query by design.
//
// Known violations are allowlisted BY BEHAVIOUR: an entry goes stale — and
// fails this test — only when that cell actually abstains on both queries.
func TestRecallModeCensus_ContentFreeAbstains(t *testing.T) {
	eng, as, _, cleanup := testEnvWithAuth(t)
	defer cleanup()

	// cell → follow-up. Measured on this fixture (fresh engrams, no embedder).
	known := map[censusCell]string{
		// F1, REPRODUCED (adversarial review, on develop and on this branch):
		// an ACT-R vault whose DEFAULT mode is semantic returns 10 nonsense
		// rows at 0.7000 — semantic applies DisableACTR over the vault's
		// resolved decay/recency weights, against its 0.3 bar. As a WIRE mode
		// semantic uses the zero base and abstains. Out of scope here.
		{"semantic", "vault", formulaACTR}: "F1",
		// F4: the weighted_sum vault DEFAULT itself admits content-free
		// queries (~0.7 vs its 0.5 bar; legacy blended formula). Every preset
		// that keeps the vault's resolved weights on weighted_sum inherits it;
		// deep additionally lowers the bar to 0.1 (F2).
		{"balanced", "wire", formulaWeightedSum}:  "F4",
		{"balanced", "vault", formulaWeightedSum}: "F4",
		{"recent", "wire", formulaWeightedSum}:    "F4",
		{"recent", "vault", formulaWeightedSum}:   "F4",
		{"deep", "wire", formulaWeightedSum}:      "F2+F4",
		{"deep", "vault", formulaWeightedSum}:     "F2+F4",
		{"semantic", "vault", formulaWeightedSum}: "F1+F4",
	}

	queries := []string{"zxqv blorptangle quuxified", "session start"}
	fusionCfg := map[string]*string{formulaACTR: nil, formulaWeightedSum: ptr("weighted_sum")}
	names := auth.RecallModeNames()

	// One wire-path vault per fusion (no default mode), one vault per
	// (fusion, preset) for the vault-default path.
	vaultFor := map[censusCell]string{}
	for _, fusion := range []string{formulaACTR, formulaWeightedSum} {
		wireVault := "beh-wire-" + fusion
		setupAliasVault(t, eng, as, wireVault, &auth.PlasticityConfig{ScoringFusion: fusionCfg[fusion]})
		for _, name := range names {
			vaultFor[censusCell{name, "wire", fusion}] = wireVault
			v := "beh-vault-" + fusion + "-" + name
			setupAliasVault(t, eng, as, v, &auth.PlasticityConfig{ScoringFusion: fusionCfg[fusion], RecallMode: ptr(name)})
			vaultFor[censusCell{name, "vault", fusion}] = v
		}
	}

	seen := map[censusCell]bool{}
	for cell, vault := range vaultFor {
		seen[cell] = true
		wireMode := ""
		if cell.path == "wire" {
			wireMode = cell.preset
		}
		admitted := []string{}
		for _, q := range queries {
			o := recallOutcomeOf(t, eng, vault, q, wireMode, true)
			if !o.Abstained || len(o.Rows) > 0 {
				top := float32(0)
				if len(o.Rows) > 0 {
					top = o.Rows[0].Floats[0]
				}
				admitted = append(admitted, fmt.Sprintf("%q→%d rows (top %.4f)", q, len(o.Rows), top))
			}
		}
		followUp, allowed := known[cell]
		switch {
		case len(admitted) > 0 && !allowed:
			t.Errorf("%s returns rows for content-free queries: %s", cell, strings.Join(admitted, ", "))
		case len(admitted) == 0 && allowed:
			t.Errorf("%s now abstains on every content-free query — %s appears FIXED; remove its allowlist entry", cell, followUp)
		}
	}
	for cell := range known {
		if !seen[cell] {
			t.Errorf("behavioural allowlist entry %s names no censused cell", cell)
		}
	}
}
