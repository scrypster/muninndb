package embed

// NoiseBaseline is the per-embedder registry backing the semantic-abstention
// floor (COG-26, issue tracked in docs/internals/invariants.md).
//
// bge-small-en-v1.5 (the bundled local model) is strongly anisotropic on real
// text: an out-of-domain query/passage pair still lands at cosine ≈0.45, not
// ≈0. Left uncorrected, that noise floor clears recall's contentMatch bar
// (COG-6, threshold 0.1) and semantic nonsense returns "confident" results
// (the #711 residual measured in
// .claude/deep-review/2026-07-29-semantic-abstention-floor-design.md).
//
// The registry is keyed by the exact embed model identifier string recorded
// for a vault/engine (see engine.resolveSemanticBaseline). A model with no
// entry here — including the empty string, meaning "we don't know what
// embedded this vault's vectors" — MUST resolve to the identity transform
// (b=0) plus a one-time WARN, never a guessed floor: an unrecognized model's
// cosine distribution is unmeasured, and a wrong floor would silently hide
// genuine matches or (less badly) silently fail to abstain. This mirrors the
// #582/#585/#589 rule that explicit config is honored or fails loudly, never
// silently substituted.
//
// Values are μ+1.3σ of the measured out-of-domain query→passage cosine
// distribution for that model (432 nonsense-query × real-passage pairs for
// bge-small-en-v1.5-int8: μ=0.450, σ=0.054 → b=0.520). See the design doc
// §2b for the measurement protocol; re-derive, don't hand-edit, if the
// bundled model ever changes.
//
// b was originally set at μ+2σ (0.558, PR landing COG-26). An adversarial
// refute (2026-07-29) proved that value too aggressive: at b=0.558 the
// sem-only survival bar (the raw cosine below which contentMatch can never
// clear the 0.1 abstention threshold with zero FTS/tag corroboration) sits
// at cos ≈ 0.632 — high enough to false-abstain genuine moderate-cosine
// matches. A real paraphrase measured at cosine 0.59 scored contentMatch
// 0.043 < 0.1 and recall returned EMPTY where it should have ranked #2.
// Modeling the true-pair cosine distribution as μ=0.693, σ=0.088, roughly
// 24% of genuine matches fell below that bar — masked on large vaults (the
// match is often outside the √N HNSW candidate pool anyway) but a real
// recall regression on small/fresh vaults.
//
// μ+1.3σ (0.520) moves the sem-only survival bar to cos ≈ 0.600 (see
// docs/internals/invariants.md COG-26 for the full derivation), which:
//   - rescues genuine matches at cosine >= 0.60 (the refute's 0.59 case and
//     the real-vault "annuity plan design" case at cosine 0.63);
//   - still abstains every measured noise top observed so far (max measured
//     432-pair noise cosine ≈ 0.596, "quarterly ballet recital" against an
//     unrelated corpus) — but with a THIN margin (contentMatch ≈ 0.095 vs
//     the 0.1 gate, not a comfortable buffer);
//   - leaves an IRREDUCIBLE sub-band — genuine matches at cosine <= the
//     model's own noise top (≈0.596) — that no flat floor can separate from
//     noise; that residual is an accepted precision/recall trade, not a bug
//     this value can fix. The per-vault plasticity SemanticFloor override is
//     the recourse for a vault that needs a different point on that curve.
var noiseBaselineRegistry = map[string]float64{
	"bge-small-en-v1.5": 0.520,
}

// NoiseBaseline looks up the measured anisotropy noise baseline b for the
// given embed model identifier. ok=false means "no calibration on record" —
// callers must treat that as the identity transform (b=0), not as b=0 with
// ok=true; the two are different claims ("measured to be zero" vs "unknown").
func NoiseBaseline(model string) (b float64, ok bool) {
	b, ok = noiseBaselineRegistry[model]
	return b, ok
}

// Rescale applies the baseline-calibrated relevance transform:
//
//	semCal = max(0, (cos - b) / (1 - b))
//
// b=0 is the identity transform (semCal == cos, clamped to [0,1] as before).
// A cos at or below the baseline maps to exactly 0 — it contributes nothing
// to contentMatch, regardless of how it would otherwise have scored.
func Rescale(cos, b float64) float64 {
	if b <= 0 {
		return cos
	}
	if b >= 1 {
		// Degenerate configuration (shouldn't happen via the registry or a
		// sane plasticity override) — never divide by <=0; fail to identity
		// rather than NaN/Inf.
		return cos
	}
	v := (cos - b) / (1 - b)
	if v < 0 {
		return 0
	}
	return v
}
