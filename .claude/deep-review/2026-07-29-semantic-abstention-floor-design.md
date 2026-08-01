# DESIGN — Semantic-abstention floor (the named residual of #711)

**Date:** 2026-07-29 · **Step:** DESIGN (increment loop) · **Code base:** `origin/develop` @ `fd9dddb`
(fix(recall): calibrated FTS relevance → recall can abstain, #711/#715). Local checkout was 9 commits
behind; all code reads and the pair-cosine probe were done against a worktree of `origin/develop`.
**Measured on:** the live labs daemon (fixed binary) over a real 3,296-memory production vault clone,
REST `127.0.0.1:9475`, plus a direct-embedder probe (`cmd/cosprobe`, built `-tags localassets` on the
worktree, bundled bge-small-en-v1.5 int8 ONNX — verified to agree with the daemon within quantization
error: probe 0.8327 vs daemon-reported 0.8309 on a control pair).

---

## 1. The residual, reproduced and mechanically explained

Post-#711, lexical nonsense abstains (Patagonia / chocolate cake / snorkeling / lunar eclipse / "violin"
single-word all return **0 results** on the clone). But semantic-cosine leakage remains: 13 of 18
out-of-domain nonsense queries still return results, driven purely by `semantic_similarity` at 0.45–0.60
with `full_text_relevance = 0`.

The exact mechanism, from `internal/engine/activation/engine.go` (origin/develop):

- Query context is embedded raw, no instruction prefix by default (`e.embedder.Embed(ctx, req.Context)`,
  **line 567**). Stored memories are embedded as `Concept + " " + Content`
  (`internal/plugin/retroactive.go:469`); passage prefixing exists only for e5-family opt-in
  (`internal/plugin/embed/adapters.go:32`, #583).
- HNSW pool: `sets.vector` gets the top-K cosine hits with **no similarity cutoff anywhere**
  (`internal/index/hnsw/hnsw.go:255ff` — pure beam search; `phase2` at engine.go:848 passes only `k`).
  K = `CalcCandidatesPerIndex(3296)` = ⌊√3296⌋ = **57** (engine.go:419–434; wired at
  `internal/engine/engine.go:2161`).
- Candidate cosine: only vector-pool members carry a cosine (`c.vectorScore = s.Score`, engine.go:961).
  **FTS-only candidates keep `vectorScore = 0`** — cosine is backfilled *only* for BFS-traversed and
  tag-seeded candidates (engine.go:1514–1521). Confirmed empirically: FTS-matched results report
  `semantic_similarity: 0`.
- ACT-R content gate: `contentMatch := w.SemanticSimilarity*vectorScore + w.FullTextRelevance*normalizedFTS`
  (engine.go:1941), with production weights **0.6·sem + 0.4·fts** hardcoded at
  `internal/engine/engine.go:2232–2233`. Score: `raw = contentMatch × softplus(B(M) + 4·hebbian + 4·transition) / 1.693`
  (engine.go:1993–2003), `B(M)` capped at ≈1.489 (engine.go:1984) so a fresh, accessed memory has prior ≈ 1.0.
- Abstention bar: production recall coerces `Threshold = 0.1` for ACT-R mode
  (`internal/engine/engine.go:2308`, COG-6 block). Observed on the clone: no returned result below final ≈ 0.107.

**So the leak is:** noise cosine 0.50–0.60 → contentMatch 0.30–0.36 → × fresh prior ≈ 1.0 (and sometimes
× transition/hebbian boosts, e.g. the jet-engines query scored a flaky-factory memory at **final = 1.0**
via `transition_boost = 1`) → sails over the 0.1 bar.

**Recency floating:** ACT-R has no additive recency term — recency in `score_components` is
reporting-only (engine.go:2016). Recency enters through `B(M)`'s `ageDays`, i.e. *multiplicatively via
the prior*. The measured float is precisely "fresh noise beats stale signal": for the paraphrase query
below, ~55 stale vector candidates were threshold-dropped (prior ≈ 0.1 → raw ≈ 0.03 < 0.1) while 2
recently-accessed ones survived. Once the semantic floor zeroes noise `sem`, the existing contentMatch
gate ("zero semantic relevance = zero score regardless of recency", engine.go:1931) already blocks
recency floating — **no separate recency gating is needed in ACT-R mode** (principle 7: reuse the proven
gate). The legacy weighted-sum path (`computeComponents`, engine.go:1841–1846) *does* have additive
recency/decay reachable without content — that is #711's named deferral (2) and stays deferred here
(path only reachable via `scoring_fusion: weighted_sum`).

---

## 2. Measurement: do the distributions separate?

### 2a. Corpus-top cosine (what recall actually sees), via REST activate

Top `semantic_similarity` over the whole vault per query:

| Out-of-domain nonsense (18) | top sem | Relevant (18) | top sem |
|---|---|---|---|
| photosynthesis in ferns | 0.559 | quarterly rate table by tier | 0.620 |
| color of the sky at dawn | 0.487 | service-api validation layer boundaries | 0.688 |
| how to tune a violin | 0.487 | reciprocity agreement handling | 0.679 |
| quarterly ballet recital | **0.596** | remittance processing flow | 0.642 |
| jet engines produce thrust | 0.560 | JATC fund rules | **0.586** |
| sourdough starter | 0 (FTS-only) | market recovery fund calculation | 0.674 |
| hiking in Patagonia | abstained | dues checkoff remittance | 0 (FTS-only) |
| chocolate cake frosting | abstained | PHPStan worktree test failures | 0.788 |
| arctic terns migration | 0.564 | Vuetify theme configuration | 0.790 |
| knit a wool sweater | 0.497 | ConfirmDialog singleton pattern | 0.779 |
| medieval castle sieges | 0.574 | 1919 validator quarter corrections | 0.639 |
| vitamin D deficiency | 0.533 | per-capita billing | 0 (FTS-only) |
| snorkeling Maldives | abstained | employer portal onboarding | 0.682 |
| change a car tire | 0 (FTS-only) | ledger reconciliation | 0 (FTS-only) |
| Ottoman Empire history | 0.500 | API versioning for service-api | 0 (FTS-only) |
| watercolor techniques | 0.508 | factory hardcoded attr flakiness | 0.602 |
| training a puppy | 0 (FTS-only) | member classification tiers | 0.664 |
| lunar eclipse viewing | abstained | audit score calculation | 0.739 |

- Nonsense corpus-top band: **0.487–0.596** (n=11 with sem>0).
- Relevant corpus-top band: **0.586–0.790** (n=13 with sem>0; the sem=0 rows matched via FTS and need no
  semantic rescue — that is the #711 channel working).
- **They separate, but the tails nearly touch:** worst noise 0.596 (ballet) vs weakest sem-carrying
  genuine 0.586 (JATC — which also has strong FTS corroboration) / 0.602 (factory flakiness).

**Query length is not the discriminator:** a 15-word nonsense sentence tops at 0.581 vs. one-word
"photosynthesis" 0.565, "ballet" 0.451 — the noise band is length-stable.

### 2b. Single-pair cosine distributions, via the repo's own embedder (cosprobe)

- **Anisotropy noise baseline** — 18 nonsense queries × 24 real memory texts = 432 pairs:
  **μ = 0.450, σ = 0.054**, p50 0.448, p95 0.542, max 0.638.
  bge-small-en-v1.5 (int8) is strongly anisotropic on real text: "random pair" ≈ 0.45, not 0.
- **True pairs** — 17 relevant queries vs their actual top memory: **μ = 0.693, σ = 0.088**, min 0.560,
  max 0.814.
- **In-vault memory↔memory** — 276 pairs: **μ = 0.665, σ = 0.077**. ⚠️ This is ~4σ above the
  query-noise baseline because the vault is domain-coherent. **A per-vault baseline naively computed
  from stored vectors would overestimate the noise floor by ~0.2 and kill genuine matches** — ruled out
  as the calibration source.

Single-pair means separate by ≈ 4.5 noise-σ. What overlaps is the *extreme-value* tail: recall competes
against the max over 3,296 memories, which pushes per-query top-noise to 0.49–0.60.

### 2c. The 0.37-paraphrase paradox — resolved: it was a mismeasurement

The Nordlys memory does not exist in this clone (verified: "Nordlys", "Nordlys telemetry pipeline moving
to Kafka", "telemetry to Kafka migration" all return zero Nordlys/Kafka/telemetry hits). Re-created it
verbatim ("Nordlys telemetry to Kafka" / migration content), measured, then deleted it.

- True pair cosine, repo embedder: **"observability stack change for that Scandinavian client" ↔ Nordlys
  memory = 0.6241** (0.6798 vs concept-only). **Not 0.37.** The probe agrees with the daemon within
  ±0.002 on a control pair, so this is the number the engine sees. The 0.37 was measured with a
  different model, text composition, or normalization — there is no genuine-signal-below-noise-floor
  paradox. **A calibrated floor is not fatal to real paraphrase signal.**
- Why the paraphrase *still* fails to retrieve Nordlys (measured with the memory present): it never
  entered the K=57 HNSW pool — this tech-flavored query has ≥57 in-domain memories above cosine 0.624
  (returned vector candidates ran 0.6697/0.6799; `total_found = 6` because the other ~55 pool members
  were stale and threshold-dropped by the ACT-R prior). It DID return via FTS ("Scandinavian" token) and
  hebbian, at rank 3.
- **Consequence:** for in-domain-phrased queries the anisotropy band shifts up to ~0.62–0.68 — genuine
  weak paraphrase (0.624) sits *inside* the in-domain noise band. No flat cosine floor can separate
  "in-domain noise" from "weak in-domain paraphrase"; that is a *ranking/pool* problem (anisotropy
  compression + K=57), not an abstention problem, and this increment neither fixes nor worsens it
  (a monotone per-candidate rescale cannot change cosine order or pool membership). Named residual, §7.

---

## 3. Design: baseline-rescaled semantic relevance (COG-26)

**Verdict from the data:** the distributions separate enough for a *calibrated, model-dependent* floor
against the out-of-domain residual #711 named (photosynthesis / violin / ballet class — all ≤ 0.596),
provided (a) the baseline is measured, never guessed, and (b) weak-but-genuine matches keep their two
existing corroboration channels (FTS in contentMatch, Hebbian/transition in the prior). A *flat hard
cutoff* is wrong (kills 0.586–0.62 genuine tails); a *rescale* composes.

### 3a. The transform

Mirror #711 exactly: `full_text_relevance` became a calibrated absolute score; `semantic_similarity`
now does the same. At the three sites where `vectorScore` feeds scoring (`computeACTR` engine.go:1941,
`computeComponents` engine.go:1841, and the RRF reporting site — the same trio #711 touched for tanh):

```
semCal = max(0, (cos − b) / (1 − b))      // b = calibrated noise baseline for this embed model
contentMatch = 0.6·semCal + 0.4·ftsCal
```

with **b = μ_noise + 2σ_noise** for the embedding model in use. For bundled
`bge-small-en-v1.5-int8`: **b = 0.450 + 2×0.054 = 0.558** (this document is the measurement of record;
the build step re-derives it with the calibration harness, §5).

No magic constant: `b` comes from a **per-embedder registry** keyed by the vault's recorded embed model
(`store.GetEmbedModel` / `SetEmbedModel`, see `internal/engine/engine_reembed.go:113`) — honoring
#582/#585/#589: a different model has a different floor, and is never silently given this one.
**Unknown/custom model or no registry entry → floor disabled (`b = 0`, identity transform) with a
one-time WARN** — fail open on presentation (principle 4): a missing calibration must never silently
hide someone's memories. Per-vault plasticity override `semantic_floor` (explicit config wins; `0`
disables) for operators who calibrate their own.

Deferred to a follow-up increment, named now: `muninn vault calibrate` — per-vault empirical `b` from a
shipped multi-domain probe set (~64 probes × ~128 sampled stored vectors, **robust median of per-probe
means** so probes that happen to be in-domain for that vault can't inflate the baseline; recomputed on
reembed). §2b shows why stored-vector pairwise stats alone cannot be the source.

### 3b. Verified targets at b = 0.558 (production bar: final < 0.1 abstains, engine.go:2308)

| Query | cos → semCal → contentMatch | Outcome |
|---|---|---|
| photosynthesis in ferns | 0.559 → 0.002 → 0.001 | **abstains** ✓ |
| how to tune a violin | 0.487 → 0 → 0 | **abstains** ✓ |
| quarterly ballet recital | 0.596 → 0.086 → 0.052 | **abstains** (< 0.1) ✓ |
| jet engines (worst leak, was final=1.0) | 0.560 → 0.005 → 0.003; transition boost now multiplies ≈0 | **abstains** ✓ |
| medieval sieges / arctic terns / vitamin D / Ottoman / watercolor / sweater / sky | 0.50–0.574 → ≤0.036 → ≤0.022 | **abstain** ✓ |
| audit score calculation | 0.739 → 0.410 → 0.246 (+fts) | returns ✓ |
| ConfirmDialog singleton | 0.779 → 0.500 → 0.300 (+fts) | returns ✓ |
| service-api validation boundaries | 0.688 → 0.294 → 0.176 | returns ✓ |
| remittance processing flow (weakest sem-only) | 0.642 → 0.190 → 0.114 | returns ✓ (thin margin — see risks) |
| JATC fund rules | 0.586 → 0.063 → 0.038 **+ 0.4·fts** | returns via FTS corroboration ✓ |
| Nordlys direct ("Nordlys telemetry Kafka") | 0.831 → 0.617 → 0.370 + 0.4·1.0 | returns ✓ |
| Nordlys paraphrase (0.624, sem-only, stale) | → 0.149 → 0.089 alone; ×3.17 prior when hebbian-linked (measured heb 0.97) → 0.28 | **unchanged**: today it never enters the K=57 pool; with corroboration it survives the floor; without, it dies at 0.089<0.1 — zero net regression, named residual |

**Composition with #711:** both signals in `contentMatch` are now absolute calibrated relevances in
[0,1], so the existing threshold is a genuine abstention gate on BOTH axes — lexical nonsense dies via
COG-24's calibrated FTS, semantic nonsense dies via COG-26's rescaled cosine, and the ACT-R gate
(engine.go:1931) makes recency/decay/access unable to resurrect either. No new mechanism, no new
threshold — the fix is injected into the same candidate-scoring path the FTS calibration used
(principle 7).

**COG-5 / tag pool:** unaffected — tagMatchFloor (engine.go:1955) applies after contentMatch, so
explicit tag hits still surface. **RRF mode:** rescale is monotone → RRF ranks unchanged; RRF
abstention remains structurally impossible (COG-24 deferral (4), unchanged). **Traversed/tag cosine
backfill** (engine.go:1514–1521) flows through the same scoring functions → floored automatically.

**Reporting:** follow #711's precedent — `score_components.semantic_similarity` reports the calibrated
`semCal` (absolute relevance, consistent with COG-24's `full_text_relevance` semantics); the raw cosine
stays visible through `muninn_explain`. Cross-surface obligations: web console "Search Scoring" panel
(#590), MCP tool descriptions, openapi.yaml component docs, invariants.md.

### 3c. Invariant — COG-26 (draft)

> **[COG-26]** `semantic_similarity` is an **absolute, model-calibrated relevance in [0,1]**:
> `semCal = max(0, (cos − b)/(1 − b))` where `b` is the embedding model's measured anisotropy noise
> baseline (μ+2σ of out-of-domain query↔passage cosines), resolved from a per-embedder registry keyed by
> the vault's recorded embed model. A model with no registered/calibrated baseline gets the identity
> transform plus a WARN — the floor is never silently substituted across models (#582/#585/#589).
> Consequently the recall threshold gates semantic nonsense the same way COG-24 gates lexical nonsense:
> near-baseline cosine (anisotropy noise, ≈0.45–0.60 for bundled bge-small) contributes ≈0 contentMatch,
> and the ACT-R contentMatch gate (COG-…, engine.go:1931) prevents recency/prior from resurrecting it.
> Corroborated weak matches survive by construction: FTS coverage adds to contentMatch; Hebbian/
> transition boosts multiply the prior. Pinned by: registry-constant pin for the bundled model, RED/GREEN
> abstention pair, monotonicity pin (rescale never reorders), unknown-model-identity pin.
> (COG-24 = #711 FTS calibration; COG-25 = #712 currency.)

---

## 4. Implementation sketch (build step, small)

1. `internal/plugin/embed/baseline.go` — `NoiseBaseline(model string) (b float64, ok bool)`; registry
   with the bundled model's measured `{μ: 0.450, σ: 0.054}`; helper `Rescale(cos, b)`.
2. `internal/engine/engine.go` weight-resolution block (~2229): resolve `b` from the vault's embed model,
   plumb as `Weights.SemanticBaseline`; plasticity override; WARN-once on unknown model.
3. `internal/engine/activation/engine.go`: apply `Rescale` at the three #711 sites; adjust the two
   comment blocks that describe contentMatch.
4. Docs: invariants.md COG-26, decision-record entry, drift-and-obligations check for the web scoring
   panel; openapi + MCP descriptions.

~150–250 LOC + tests. No new Pebble prefix (Tier A uses a code registry; the deferred per-vault
calibration stores under existing vault config). Tier: standard small increment, single PR referencing
this design; `muninn vault calibrate` is the named increment 2.

## 5. Proof plan on the clone (MEASURE step)

- RED first: `internal/engine/activation/semantic_abstention_test.go` — synthetic vault, query with
  cosine ≈ b (noise) must abstain, cosine ≥ b+0.1 must return; **shown failing without the rescale**.
- Calibration pin (asset-gated, `-tags localassets`): re-measure μ against the bundled model on a small
  fixed text set; fails if the shipped model drifts from the registered constant (seconds, within CI budget).
- Monotonicity + unknown-model-identity + COG-5-unaffected + RRF-ranks-unchanged unit pins.
- Clone battery (this document is the baseline): rerun the 18/18 set via REST. Targets: **18/18 nonsense
  abstain** (today 5/18); **18/18 relevant keep their current top result** (today's tops recorded in §2a);
  Nordlys direct-query unchanged; paraphrase unchanged (documented zero-regression). Also re-check
  jet-engines: transition boost must no longer produce final=1.0 on noise.

## 6. Risks & residuals

- **Thin margin:** top-noise 0.596 vs weakest sem-only genuine 0.642 leaves ~0.05 of cosine headroom on
  this vault; k=2 is calibrated to these measurements. Mitigations: per-vault `semantic_floor` override
  now, `vault calibrate` next, and an abstention-rate line in `muninn_status` worth considering.
- **Other-vault noise tails:** a vault whose queries are habitually in-domain-phrased has a higher
  effective noise band (§2c measured 0.62–0.68 for one such query); the floor helps less there but never
  makes ranking worse (monotone). Honest scope: this increment kills the *out-of-domain* residual #711
  named, not in-domain near-noise.
- **Hebbian rescue of noise:** repeated probing builds co-activation; a heavily-probed memory can ride
  `4·hebbian` over the gate even with semCal≈0.05 (observed heb up to 0.97 in this lab session). By
  design (corroboration channel), but a long agent session could resurrect a noise hit it accidentally
  co-activated. Watch in MEASURE.
- **Weighted-sum path** still has additive recency without a content gate (#711 deferral (2)) — floor
  reduces but does not eliminate floating there; unchanged scope.
- **Named residual (ranking, not abstention):** genuine weak paraphrases (~0.62 cosine) to stale
  memories lose the K=√N HNSW pool to in-domain noise and are prior-suppressed even when pooled. Needs
  corroboration-aware retrieval (entity/association expansion or larger pool + rerank), not a floor.
  This is the successor residual to carry into the next design, with the Nordlys protocol in §2c as its
  reproduction recipe.
- Quantization delta between stored (int8-dequantized) and fresh embeddings measured ≈0.002 — negligible
  vs the 0.108 rescale span.
