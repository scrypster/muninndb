# FTS relevance calibration + honest abstention — design (issue #711)

**Date:** 2026-07-29 · **Step:** DESIGN (no production code) · **Base:** origin/develop @ `cdbe355`
**Problem:** Recall never abstains. Off-topic/nonsense queries return confident irrelevant
results because `full_text_relevance` is ~1.0 for *any* single-token match, so no threshold
can separate nonsense from genuine matches. Measured on the real 3,296-memory simplorium
vault: `"how to bake sourdough bread"` → "Platform validation methodology", score 1.0,
full_text_relevance 0.9999, semantic_similarity 0.

---

## 1. Root cause — CONFIRMED mechanism, hypothesis REFUTED (mostly)

### The hypothesis ("per-query-max normalization") is REFUTED for full_text_relevance

There is **no per-query-max normalization on the FTS score**. The saturation is
**`tanh` applied to raw, field-weighted, unbounded BM25**:

- `internal/engine/activation/engine.go:1810` (weighted-sum/CGDN path, in `computeComponents`):
  ```go
  normalizedFTS := math.Tanh(ftsScore)
  ```
  with the comment claiming "tanh(0)=0, tanh(1)≈0.76, tanh(3)≈0.995 — preserves relative
  ordering" (engine.go:1806–1809). That comment is the bug in prose: tanh is in *deep
  saturation* by x≈3, and real BM25 magnitudes here are 2–40 (see below), so virtually
  every match lands on the flat top of the curve.
- `internal/engine/activation/engine.go:1909` (ACT-R path, `computeACTR`) — same `math.Tanh(ftsScore)`.
- `internal/engine/activation/engine.go:1549` (RRF path, reporting only) — same.

The raw score entering tanh is the **sum over query terms of field-weighted BM25**
(`internal/index/fts/fts.go:459–467`):

```go
tfNorm := tf * (k1 + 1) / (tf + k1*(1-b+b*dl/avgdl))   // fts.go:459, k1=1.2, b=0.75
bm25   := idf * tfNorm * fieldWeight(pv.Field)          // fts.go:460
scores[engramID] += bm25                                 // fts.go:467 (summed across terms & fields)
```

with `idf = ln((N-df+0.5)/(df+0.5) + 1)` (fts.go:536) and field weights **3.0 concept /
2.0 tags / 1.0 content / 0.5 created_by** (fts.go:27–30). This raw sum is carried into the
activation engine unmodified: `c.ftsScore = s.Score` at
`internal/engine/activation/engine.go:948` (phase3RRF), then tanh'd at the three sites above.

**Real magnitudes for N=3,296 (simplorium):**

| match | idf | bm25 (≈) | tanh |
|---|---|---|---|
| 1 common token (df≈800, e.g. "how"), tf=1 in content | 1.42 | 0.65 | 0.57 |
| 1 moderately common token (df≈300), tf≥2 in content | 2.40 | 3–5 | 0.995–0.9999 |
| 1 token in **concept** (fw=3.0), df≈300 | 2.40 | 5–16 | 0.9999–1.0 |
| genuine 3-term match, concept+content | — | 15–40 | 1.0000 |

tanh(2.65)=0.99, tanh(5)=0.9999, tanh(10)=1.0 to float precision. So a nonsense query
matching **one** moderately common word in a concept field is *indistinguishable* from a
genuine multi-term match: both report full_text_relevance ≈ 0.9999–1.0. The score carries
zero information above bm25≈3, and with a concept field weight of 3.0, bm25≈3 is *one
occurrence of one unremarkable word*.

### The sourdough case, explained exactly

Query tokenization (`fts.go:97–138`): stopwords removed ("to" is a stopword, fts.go:39),
tokens < 2 chars dropped, Porter2 stemmed. `"how to bake sourdough bread"` →
`[how, bake, sourdough, bread]`. **"how" is not in the stopword list** (fts.go:36–45).
"sourdough"/"bread"/"bake" almost certainly have no term-stats entry in a software vault →
`getIDF` returns 0 (fts.go:530–532: missing TermStatsKey → `return 0`) → those terms are
**skipped entirely** at fts.go:391–392 (`if idf <= 0 { continue }`). The only term that
searches is "how". Any memory whose concept/content contains "how" prominently (e.g. a
methodology memory: "how we validate…") accumulates bm25 ≈ 3–6 → tanh → **0.9999**. The
match is real at the token level ("how" ∈ both) but carries no information — and the
mapping to `full_text_relevance` erases that distinction. So the fix is **not** primarily
tokenization (adding "how" to stopwords is whack-a-mole; "color"/"sky" caused the same bug
in the Vuetify case) — it is the **activation mapping**: absent query terms and low-IDF
matches must drag relevance *down*, which requires query-level calibration, not a
per-hit squash.

### Where "score 1.0" comes from (secondary mechanism — the hypothesis is HALF right)

The ACT-R path *does* contain a per-query-max rescale — `internal/engine/activation/engine.go:1663–1683`
(`maxRaw` scan + `scale = 1.0/maxRaw` when `maxRaw > 1.0`, added for #331). When any
candidate's `raw = contentMatch × softplus(B+heb)/1.693` exceeds 1.0 (possible via Hebbian/
transition boosts pushing past the bLevelCap at engine.go:1953), the whole set rescales so
the **top hit lands at exactly 1.0 regardless of absolute quality** — the exact anti-pattern
the ranking-honesty design (`.claude/deep-review/2026-07-29-ranking-honesty-design.md`,
"considered and rejected: dividing by the per-query max makes every query's top hit score
1.0 regardless of quality") rejected for RRF. That explains the observed `score: 1.0`.
It only fires when saturation occurs; with calibrated FTS (below), nonsense queries no
longer reach raw > 1.0, so the artifact disappears for this bug class. Full removal of the
#331 rescale is a separate increment (it exists to fix fresh-vault rank collapse and is
pinned by tests) — **named deferral, not silently absorbed**.

### Chain of dishonesty, end to end (ACT-R default path)

1. fts.Search returns raw summed BM25 (unbounded) — fts.go:322–408.
2. `c.ftsScore = s.Score` — activation/engine.go:948.
3. `normalizedFTS = tanh(ftsScore)` ≈ 1.0 for any match — engine.go:1909. **← the bug**
4. `contentMatch = 0.6·semantic + 0.4·normalizedFTS` (hardcoded blend, `internal/engine/engine.go:2236–2237`)
   → any FTS-matched candidate starts at contentMatch ≥ 0.4 even with semantic 0.
5. `raw = contentMatch × softplus(B)/1.693` — fresh memory (B capped at 1.489,
   engine.go:1953) → raw ≈ contentMatch ≈ 0.4.
6. Default threshold 0.1 (`internal/engine/engine.go:2183–2185`; activation-layer default
   0.05 at activation/engine.go:446). 0.4 ≫ 0.1 → nonsense **always** clears the gate.
   Even the user's explicit `threshold: 0.5` loses when Hebbian/confidence nudge 0.4 up.

---

## 2. The fix — calibrated absolute FTS relevance (IDF-weighted query coverage)

### Design

Replace tanh-of-raw-BM25 with an **absolute, per-query-calibrated coverage score computed
inside `fts.Search`**, returned as the `ScoredID.Score` in [0,1]:

For each query term group *t* (grouped by stem, so the legacy dual-path raw/stemmed union
at fts.go:351–369 can't double-count):

```
idf_t     = getIDF(t)                    // existing fts.go:519
idf_t'    = idf_t if idf_t > 0
            else idfMax = ln((N+0.5)/0.5 + 1) ≈ ln(2N+1)   // absent term = maximally informative
cov_t(d)  = min(1, Σ_fields tfNorm·fieldWeight / (k1+1))    // per-doc term coverage, capped at 1

Score(d)  = Σ_t idf_t'·cov_t(d) / Σ_t idf_t'                // ∈ [0,1]
```

- **Absolute**: the denominator depends only on the query and corpus statistics, never on
  the result set. No per-query-max. Two queries' scores are comparable; a score of 0.9
  *means* "this engram covers ~90% of the query's IDF mass".
- **IDF-weighted**: matching only a common token ("how", "color") contributes its small
  idf against the full query mass — sourdough's r ≈ 1.42·0.46 / (1.42 + 3×8.8) ≈ **0.02**,
  down from 0.9999. Terms **absent from the corpus** (idf lookup = 0 today, silently
  skipped) enter the denominator at idfMax ≈ ln(2N+1) ≈ 8.8 for N=3296 — an unseen term is
  the strongest possible evidence the vault knows nothing about this query.
- **Saturating per term**: `cov_t` caps at 1, so term-stuffing one word can't substitute
  for covering the query. tf=1 in content gives cov ≈ 0.46 (tfNorm/(k1+1) at avgdl);
  a concept/tags placement (fw 3.0/2.0) hits the cap — prominence still counts, honestly.
- **Ordering**: single-term queries keep exactly today's order (monotone transform).
  Multi-term queries now rank breadth-of-coverage above single-term-stuffing — a strictly
  more honest order, aligned with what BM25 users expect.

Then in the activation engine, **delete the three tanh sites** (engine.go:1549, 1810, 1909)
and use the calibrated score directly as `FullTextRelevance`. The `// Normalize BM25…tanh`
comment block (1806–1809) goes with it.

Worked before/after (N=3,296, default ACT-R path, threshold 0.1, fresh engrams, conf 1.0):

| query | match | full_text before | full_text after | final before | final after | result |
|---|---|---|---|---|---|---|
| "how to bake sourdough bread" | "Platform validation methodology" (matched: "how") | 0.9999 | ≈0.02 | ≈0.40–1.0 | ≈0.01 | **abstains** (< 0.1) |
| "the color of the sky at dawn" | "Vuetify theme" (matched: "color") | 0.9999 | ≈0.12 | ≈0.40+ | ≈0.05 | **abstains** |
| "database performance tuning" | genuine db-perf memory (3/3 stems, sem≈0.6) | 1.0 | ≈0.85–0.96 | ≈0.7–1.0 | ≈0.70 | **returned** |
| "pricing and revenue" | genuine pricing memory (sem≈0.55, 2/2 terms) | ≈1.0 | ≈0.9 | high | ≈0.66 | **returned** |
| natural-language query, 3 of 6 content words covered, sem≈0.6 | genuine | 1.0 | ≈0.5 | high | ≈0.55 | **returned** |

Genuine matches drop from "pegged at 1.0" to 0.5–0.96 — but their *final* scores stay far
above the 0.1 default and the 0.3 semantic-mode threshold because semantic similarity
(0.6 weight) carries them. The failures now concentrate exactly where they should: sem≈0
AND low IDF coverage.

### Abstention mechanism: the existing threshold, no new machinery

**Decision: abstention = the honest score falls below the existing threshold.** No new
response field, no "abstain" flag. Rationale:
- The threshold gate already exists on every scoring path (engine.go:1543/1633/1687/1708)
  and empty results are already a first-class response on every surface.
- Principle 7 (extend proven mechanisms): the only reason the threshold "didn't work" was
  that the score was dishonest. Fix the score; the mechanism was never broken.
- A separate abstain signal would be a second source of truth to drift (and the
  tag-filter bypass `!c.inTagPool` semantics — a filter defines the set — must keep working
  unchanged; explicit tag matches still bypass, correctly).

### No-silent-substitution analysis (the R1 lesson)

The recalibration **lowers full_text_relevance for everything**, so the question is whether
true positives survive:

- **ACT-R default (threshold 0.1, blend 0.6/0.4 at engine.go:2236–2237):** a genuine match
  needs `0.6·sem + 0.4·r ≳ 0.1` (× prior/conf ≈ 1 for fresh). That's sem ≥ 0.17 alone, or
  r ≥ 0.25 alone. Real matches have sem 0.5+ and/or r 0.5+ — comfortable margin. **No
  threshold retune needed for the default path**, but this is asserted-then-measured, not
  assumed: the real-vault probe (§3) is the gate.
- **Mode presets (#590, `internal/auth/recall_modes.go`):** "semantic" mode
  (threshold 0.3, weights 0.8/0.2, DisableACTR) — nonsense previously got
  0.2·1.0 = 0.2 FTS contribution + decay/recency components in the weighted-sum formula;
  after, 0.2·0.02 ≈ 0.004. Genuine: 0.8·0.6 = 0.48 ≥ 0.3. Survives. Named shift: in
  weighted-sum mode the FTS component's typical magnitude drops from "0.25·1.0" to
  "0.25·r"; the decay/recency components (0.20/0.05 weights) now dominate weak-FTS
  candidates — this is the semantic/recency hole (§ deferrals), not a regression introduced
  here, but the recalibration makes it *relatively* more visible.
- **RRF vaults (`ScoringFusion == "rrf"`, engine.go:2250):** final score is rank-based
  (rrfScore, threshold default 0.001); the calibration changes only the *reported*
  full_text_relevance component (engine.go:1549) and FTS-internal ordering. **RRF mode
  still cannot abstain by construction** (every candidate has a nonzero rank score).
  Named residual — consistent with the ranking-honesty design's refusal to fake absolute
  scores out of ranks. If RRF-mode abstention is wanted, it needs its own design (e.g.
  gate RRF entry on calibrated r or semantic floor), deferred.
- **Degraded BM25-only mode (#578, embedder down → sem=0):** effective bar becomes
  r ≥ 0.25 at threshold 0.1. Genuine keyword matches (r 0.5–1.0) pass; *marginal*
  single-common-term matches abstain — which is the correct honest behavior, but it is a
  behavior change in degraded mode and must be in the probe set. If the probe shows real
  degraded-mode recall loss, the remedy is redistributing the dead 0.6 semantic weight
  (as `activation.New` already does at activation/engine.go:340–348 for nil-HNSW), never
  re-inflating r.
- **Explicit user thresholds:** anyone who tuned `threshold:` against the old inflated
  scores sees fewer results. That is the point of #711, but it goes in the changelog as a
  behavior change, loudly (principle 2).

### Consumers of `fts.Search` Score audited

- `internal/engine/activation/engine.go:792,830` — the recall path; intended change.
- `internal/engine/autoassoc/autoassoc.go:158–174` — uses IDs/rank only, never `.Score`. Safe.
- `internal/engine/trigger/worker.go:150,213,258,486` — every `TriggerScore` call passes
  `ftsScore=0` today (FTS leg unused). Safe; when the trigger system grows an FTS leg it
  inherits the calibrated scale for free.

### Minimal increment vs named deferrals

**Increment 1 (this design):** calibrated coverage score in `fts.Search`; remove the three
tanh sites; invariant + pinning tests; docs. No storage change, no API shape change.

**Deferred, named honestly — the fix above does NOT close these:**
1. **Semantic floor / cosine recalibration.** `"photosynthesis in ferns"` scored 0.65 via
   semantic_similarity 0.54 with full_text 0 — embedding cosine between *arbitrary* texts
   sits ~0.35–0.55 (anisotropy), so raw cosine ≥ threshold-relevant values for pure
   nonsense. After the FTS fix this class **still returns results**
   (0.6·0.54 = 0.32 ≥ 0.1). Fixing it needs an absolute cosine baseline that is
   embed-model-dependent (measure the vault's background cosine distribution; affine-rescale
   or floor). That's a measurement-first increment of its own; doing it blind risks exactly
   the silent-substitution failure this increment forbids.
2. **Recency/decay floating in weighted-sum/CGDN modes.** ACT-R's contentMatch gate already
   prevents recency from floating a zero-relevance engram (engine.go:1902 "zero semantic
   relevance = zero score"); the legacy weighted-sum path does not (0.20 decay + 0.05
   recency + 0.05 access can clear low thresholds alone). Deferred with #1.
3. **The #331 per-query maxRaw rescale in ACT-R** (engine.go:1663–1683) — a genuine
   per-query-max normalization that stamps 1.0 on the top hit when saturation occurs.
   Rarely triggered after this fix; removal must be designed against #331's fresh-vault
   rank-collapse tests.
4. **RRF-mode abstention** (rank scores can't abstain), see above.
5. **Stopword list gaps** ("how", "what", "color"-class words) — irrelevant once IDF
   weighting is in; not worth churning the on-disk token set for.

---

## 3. Measurable proof (non-gameable)

### In-process RED (CI-cheap, unit-tier)

New test in `internal/engine/activation/` (or fts) building a small honest corpus —
~40 engrams of realistic dev-note text (multi-sentence, shared vocabulary, no synthetic
score plants):
1. **RED half:** nonsense query ("how to bake sourdough bread") through the full ACT-R
   scoring path at default threshold 0.1 → **0 results**. Must be shown to FAIL with the
   tanh mapping restored (revert = the confident wrong match comes back). Not a test that
   passes both ways.
2. **Positive control:** a topical query returns its genuine match with final ≥ 0.3 —
   pins that calibration didn't gut recall.
3. **Calibration pins (table-driven, pure functions):** single common-token match → r < 0.15;
   full coverage of a 3-term query → r > 0.7; absent-term denominator uses idfMax;
   score ∈ [0,1]; denominator independent of result set (two corpora, same query+doc →
   same score — the anti-per-query-max pin).

### Real-vault validation (the honest gate)

Against the labs clone (isolated daemon, REST :9475, vault=simplorium, 3,296 real
memories), run the ~12-query probe before and after, default threshold, and report a
before/after table of (top concept, score, full_text_relevance, semantic_similarity, n):

- **Must abstain (0 results, or every score < 0.1):** "how to bake sourdough bread",
  "the color of the sky at dawn", "my grandmother's garden", "photosynthesis in ferns"†,
  plus 2 more nonsense probes not used during development (guard against tuning to the
  probe set).
- **Must still return their known-good matches:** "database performance tuning",
  "pricing and revenue", "platform validation", "Vuetify theme setup", and 2 real
  workflow queries pulled from actual session history.

† expected to **still fail** (semantic 0.54 hole, deferral #1) — report it as a known
residual in the table, not hidden. If it passes, good; if the table shows any *topical*
query losing its match, the increment does not ship until the blend/threshold is retuned
and re-measured. Success criterion: all FTS-driven nonsense rows abstain, all topical rows
keep their match, and the residual semantic-hole rows are explicitly listed.

---

## 4. Invariant impact

Propose **COG-24** (next free; COG-22 discovery-parked, COG-23 = ranking-honesty R2):

> **[COG-24]** `full_text_relevance` is an **absolute, query-calibrated** signal in [0,1]:
> the IDF-weighted fraction of the query's term mass covered by the engram, with
> corpus-absent query terms counted at maximum IDF. It is never normalized against the
> result set (no per-query-max) and never saturated by an unbounded raw score (no tanh of
> raw BM25). Consequently the recall threshold is a real abstention gate: a query whose
> candidates all score below threshold returns empty, and matching only common tokens
> cannot clear a default threshold. — `internal/index/fts/fts.go` Search,
> `internal/engine/activation/engine.go` (computeACTR/computeComponents/RRF reporting),
> pinned by the calibration + RED tests above.

Interaction notes: COG-5/tagMatchFloor (0.1 floor for explicit tag matches) is untouched —
its rationale comment cites "genuine content matches typically 0.5–0.9", which remains true
post-calibration. The bLevelCap "score absoluteness" rationale (engine.go:1949–1951) is
*strengthened* by this change.

## 5. Cross-surface obligations

- `full_text_relevance` in `score_components` (MCP recall/explain, REST, gRPC, SDK
  passthrough): **shape unchanged, meaning changes** from "≈1.0 for any match" to
  calibrated coverage. Update the score-components documentation and `muninn_explain`
  interpretation guidance; `buildWhy`'s "strong full-text match (NN%)" string
  (engine.go:2275) becomes honest for free.
- Web console Search Scoring panel (#590): any copy describing full-text relevance needs
  the new meaning; no schema change.
- MCP tool registry: no tool/schema change → no registry smoke-test churn.
- Changelog: behavior change for explicit-threshold users and degraded BM25-only mode,
  stated loudly.
- No new Pebble prefix, no FTS on-disk format change (postings/DF/stats/FTSVersion all
  untouched — this is search-time math only). Keyspace registry unaffected.

## 6. Tier & risks

**Tier: recall-scoring, core-precision-critical.** Not Tier-3: no on-disk format, no auth,
no replication. Requires `-race` on activation tests (touches the recall hot path) and the
real-vault probe as the merge gate.

Top risks:
1. **Silently dropping genuine FTS-only matches** (sem weak, e.g. exact-keyword lookups,
   degraded mode). Mitigation: positive controls in both the unit RED and the real-vault
   probe; degraded-mode probes included; blend redistribution (not score re-inflation) is
   the sanctioned lever if needed.
2. **Ordering shift inside FTS results** (coverage vs raw-sum) changes RRF ranks slightly
   for multi-term queries. Mitigation: probe table includes RRF-mode spot checks; the shift
   direction (breadth over stuffing) is defensible and documented.
3. **idfMax denominator overweights absent terms on tiny vaults** (N small → idfMax small,
   but relative dominance still holds; N=1 startup default at fts.go:513 gives idfMax≈1.1).
   Mitigation: calibration pins include a tiny-corpus case; behavior degrades toward
   "plain coverage fraction", which is still honest.
4. **Users interpret lower full_text_relevance as regression.** Mitigation: changelog +
   score-components doc rewrite in the same PR (obligation list above).
