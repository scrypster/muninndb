# Scoring-weight trace: what actually ranks a recall result

Date: 2026-07-30
Branch: `fix/agent-experience-hardening`
Scope: trace + measurement harness only. **No behaviour change shipped in this pass.**

## Framing: check the cheap causes before blaming the cognitive layer

Three separate defects have now been initially mis-attributed to ACT-R fusion or to "the
cognition layer being wrong":

1. **Confidence collapse** — the contradiction bug drives confidence to ~0.07, and confidence
   multiplies straight into the final score (§3.2). A 93% cut that has nothing to do with
   scoring weights.
2. **Missing-estimator handling** — the linear ContentMatch scores an absent signal as a zero
   rather than as absence of evidence, in both directions: missing FTS (§3.1–3.3) and missing
   cosine (§3.4).
3. **Indexing lag** — a memory is unfindable until the async embedder catches up, for up to
   ~3 minutes (§3.5).

None of the three is ACT-R. The ACT-R prior *does* have a large dynamic range (§2.3, >20×),
and that may yet turn out to be a real problem — but it has not been the cause of a single
reproduced miss so far. **Account for confidence, missing-signal handling, and indexing lag
before attributing a recall failure to the cognitive layer.** The harness in §4 exists so
that attribution is made by measurement rather than by argument.

---

## 1. The actual scoring path, end to end

### 1.1 Where the weights come from

`Engine.activateCore` builds `actReq.Weights` in one of three branches
(`internal/engine/engine.go:2306-2380`):

| branch | condition | what it sets |
|---|---|---|
| caller weights | `req.Weights != nil` | passthrough of the wire request |
| weight-carrying preset | `mode` is `semantic`/`recent`, no caller weights, vault not `rrf` | preset's full zero-base vector (`internal/auth/recall_modes.go:17`) |
| **production default** | everything else | **hardcoded `SemanticSimilarity: 0.6`, `FullTextRelevance: 0.4`** (`engine.go:2357-2358`) |

`resolveWeights` (`internal/engine/activation/engine.go:2509`) then fills the ACT-R
parameters: `ACTRDecay = 0.5`, `ACTRHebScale = 4.0`, `UseACTR = !DisableACTR`.

### 1.2 The formula that produces the number

Phase 6 dispatches on the scoring mode (`activation/engine.go:1488` `phase6Score`).
The production default is the **ACT-R** branch (`activation/engine.go:1895`), which calls
`computeACTR` (`activation/engine.go:2199`):

```
semCal       = max(0, (cos - b) / (1 - b))                      activation:2133, 2210
ContentMatch = 0.6*semCal + 0.4*ftsCoverage                     activation:2211
   (floored at 0.1 when the candidate matched an explicit tag filter, COG-5, activation:2227)

B(M)         = ln(n) - 0.5*ln(max(ageDays, 1min)/n),  n = AccessCount+1   activation:2251
               capped at bLevelCap = ln(e^1.6931 - 1) ≈ 1.4892            activation:2265
prior        = softplus(B(M) + 4*hebbian + 4*transition) / 1.6931         activation:2269-2276

raw          = ContentMatch * prior                             activation:2276
final        = raw * Confidence                                 activation:2295
```

Then, per query (`activation/engine.go:1922-1940`): if any candidate's `raw > 1`, all are
rescaled by `1/maxRaw` (a single positive scalar — ranking-neutral), and anything with
`final < Threshold` is dropped. Default threshold is **0.05** for ACT-R,
0.001 for RRF (`activation/engine.go:542-549`); the `semantic`/`recent`/`deep` presets
override to 0.3 / 0.2 / 0.1.

### 1.3 Where the constants came from

`git log -L` on `engine.go:2356-2358`: the 0.6/0.4 pair landed in **e51b985**
("feat(engine,storage): PAS transitions, checkpoints, migration, activation improvements",
2026-02-25), a large omnibus commit. It *replaced* plasticity-derived values
(`resolved.SemanticWeight` / `resolved.FTSWeight`) with literals, with the comment:

> ACT-R ContentMatch gate: 60% semantic, 40% FTS — **proven optimal**. … Use hardcoded
> **proven values** to guarantee deterministic results.

**No derivation is recorded anywhere.** No corpus is named, no measurement is cited, and
`docs/internals/` contains no entry for it. The commit body does not mention the change.
This is precisely the pattern principle #11 forbids: an unmeasured constant carrying the
word "proven".

The `DefaultWeights` struct in `activation.New` (`activation/engine.go:401-407`:
0.35/0.25/0.20/0.10/0.05/0.05) has the same problem but is **not reachable in production** —
see §2.1.

---

## 2. Findings

### 2.1 CONFIRMED — the 6-dimension additive path is not reachable in production

`resolveWeights` sets `UseACTR = !req.DisableACTR` (`activation/engine.go:2534`), and
the production-default branch sets `UseACTR: true` (`engine.go:2365`). The additive
weighted sum in `computeComponents` (`activation/engine.go:2035-2110`) runs **only** if:

- a caller sets `DisableACTR`, or
- the vault sets `ScoringFusion: "weighted_sum"` (`engine.go:2376-2379`), or
- the `semantic` recall-mode preset is used, which carries `DisableACTR: true`
  (`internal/auth/recall_modes.go:23`).

`engine.go:2204` says it outright: *"All scoring goes through ACT-R; legacy temporal path
is kept in code but not reachable for now."*

**Therefore the original framing — "semantic similarity carries 1/6 of the score" — is
REFUTED for production.** Semantic carries 0.6 of ContentMatch, and ContentMatch gates
everything else multiplicatively.

### 2.2 PARTIALLY REFUTED — the formula as commonly quoted

Two corrections to the quoted production formula:

- **There is no `tanh` on the FTS term.** #711 removed it; `ftsCoverage` is already a
  calibrated absolute [0,1] coverage score (`activation/engine.go:2064-2069`). The comment
  there explains why the tanh was actively harmful: it saturated by x≈3 while real BM25
  magnitudes ran 2–40, making one common-word match indistinguishable from a genuine
  multi-term match.
- **The semantic term is `semCal`, not raw cosine** — COG-26's baseline-rescaled value
  (`activation/engine.go:2210`, `b = 0.520` for bge-small-en-v1.5,
  `internal/plugin/embed/baseline.go:56`). This matters a great deal for the worked example
  in §3.

The structural claim — *relevance gates salience multiplicatively; salience cannot rescue a
zero-relevance memory* — is **CONFIRMED** (`activation/engine.go:2276`), with one exception
worth noting: the COG-5 tag floor sets `ContentMatch = 0.1` for an explicit tag-filter match
with zero content overlap (`activation/engine.go:2227`), so an explicit filter can surface a
content-unrelated memory by design.

### 2.3 CONFIRMED (all three sub-claims) — the learned-weight loop cannot learn

Pinned by characterization tests in `internal/scoring/deadloop_characterization_test.go`.

**(a) Both call sites pass a constant.** `internal/engine/engine.go:2101` (implicit
read-is-access feedback) and `internal/engine/engine.go:4370` (explicit
`Engine.RecordFeedback`) both set `ScoreVector: scoring.DefaultWeights()` — the uniform
1/6 vector — where the field's own doc comment (`internal/scoring/weights.go:31`) says it
carries *"the score components that produced this result"*. A constant is not feedback.

**(b) With a constant vector the update is provably a no-op.** `Update`
(`internal/scoring/weights.go:94`) adds `lr * direction * ScoreVector[i]` to each
dimension — the *same scalar* to every dimension — and `Softmax` is shift-invariant. The
0.05 floor does not rescue it: from a uniform start all six dimensions hit the floor
simultaneously and clamp equally. Verified over 500 updates in both feedback directions:
max weight movement `< 1e-12`. `UpdateCount` increments correctly — the bookkeeping runs,
only the learning does not.

**(c) `Update` re-Softmaxes an already-normalised vector.** Softmax of a probability
distribution is far flatter than the distribution. Measured: a peaked distribution
(spread 0.7608) collapses to spread **0.000000** — exactly uniform — after 10
re-normalizations. Twenty *zero-information* updates erode a learned peak from 0.8007 to
0.1667. Even a genuinely informative gradient would be fighting its own normalization.
The contrast test confirms `Update` is not broken in itself: given a real non-uniform
score vector it does move the weights in the right direction.

**(d) Nothing reads the result.** `scoring.Store.Get` has no caller outside
`internal/scoring`'s own tests. `PebbleStore.ScoringStore()` (`internal/storage/impl.go:669`)
is called exactly once, at `internal/engine/engine.go:446`, to populate `e.scoring` — which
is then used only for `RecordFeedback`. No `Weights[Dim…]` indexing appears anywhere in the
tree outside `internal/scoring`. (The brief's mention of `internal/replication` is
incorrect: replication ships the 0x13 keyspace prefix like any other, but never reads the
struct.)

**(e) The dimensions don't map onto the production knobs anyway.** The six dims are
FTS/HNSW/Hebbian/Decay/Recency/Association. Production has: a two-term ContentMatch
(0.6/0.4), `ACTRDecay`, and `ACTRHebScale`. `DimDecay`, `DimRecency` and `DimAssociation`
have no additive counterpart in the ACT-R formula at all. Wiring the vector in would require
first deciding what it *means*.

**Net: this is not merely a write-only loop. It is a write-only loop whose writes are
provably constant, whose normalization erases learning even when it happens, whose output
nothing reads, and whose coordinate system does not match the live scorer.**

---

## 3. The primary defect: the paraphrase cap

### 3.1 The arithmetic

```
ContentMatch = 0.6*semCal + 0.4*ftsCoverage
```

A candidate with **no lexical overlap** has `ftsCoverage = 0` and therefore
`ContentMatch ≤ 0.6`, *at a perfect semantic match*. A missing estimator is charged as
evidence of irrelevance rather than absence of evidence. Symmetrically, a perfect lexical
match with zero semantic similarity caps at 0.4.

This is not a tuning preference. It is a hard ceiling on retrieval-by-meaning — the exact
capability the vector index exists to provide.

### 3.2 The worked case, corrected

The incident case is usually quoted as `0.6 × 0.758 = 0.455`. **That omits the COG-26
rescale and is wrong in the optimistic direction.** With `b = 0.520`:

```
semCal       = (0.758 - 0.520) / 0.480 = 0.4958
ContentMatch = 0.6 × 0.4958          = 0.2975     (not 0.455)
prior        ≈ 1.0 (fresh, recently accessed)
final (conf 1.00) = 0.2975           → survives the 0.05 threshold, comfortably
final (conf 0.07) = 0.0208           → DROPPED, zero results
```

Both facts are load-bearing, and they are not in competition:

- **the confidence bug explains the zero result** — 0.07 is a 93% cut, and Confidence
  multiplies straight into the final score (`activation/engine.go:2295`);
- **the 0.6 cap explains why the margin was thin enough for a confidence bug to matter at
  all.** Under noisy-OR the same candidate at the same collapsed confidence scores 0.0347 —
  still dropped. So the cap is not *the* cause of this particular miss. It is a standing
  1.67× handicap applied to every semantic-only match, which turns near-misses into misses.

### 3.3 How much of a realistic pool is capped

Structurally: **every candidate that is not in the FTS result set has `ftsScore = 0`.**
`phase3RRF` (`activation/engine.go:1088-1092`) assigns `c.ftsScore` only while walking
`sets.fts`; candidates arriving from the vector, decay, time or tag sets keep the zero
value. So the cap applies to:

- the entire vector-only portion of the pool (the ANN neighborhood minus the FTS∩HNSW
  intersection),
- every decay-, time- and tag-seeded candidate,
- and, for a query with no meaningful lexical overlap with anything in the vault, the
  **whole pool**.

The exact fraction is vault- and query-dependent and is precisely what the harness measures
(`gold-found` under the PARAPHRASE condition). It is not estimated here, because estimating
it is the thing the harness exists to avoid doing by argument.

### 3.4 The same defect from the opposite direction: missing cosine

A live reproduction was reported at commit `c3b5b60`: a newly-written memory returns **zero
results** to a near-exact lexical query ~3s after the write, then returns at **0.742** once
the async embedder catches up. Independently verified below — and the mechanism is **sharper
than the one reported**.

**The reported mechanism is incomplete.** "ContentMatch ≤ 0.4, which falls below the default
recall threshold" understates it, because 0.4 is comfortably above the *engine's* default of
0.05. The real gate is the **surface** default:

| surface | default when `threshold` is omitted | source |
|---|---|---|
| engine / library | 0.05 | `activation/engine.go:547` |
| **MCP** | **0.5** | `internal/mcp/handlers.go:392` |
| **REST** | **0.5** | `internal/transport/rest/server.go:1772` |

With no embedding, `cosine = 0` → `semCal = 0` → `ContentMatch = 0.4 × fts ≤ **0.4**`, and
the prior can only multiply *down* (it saturates at 1.0, and the per-query rescale never
scales up). So:

> **An FTS-only match is structurally unreturnable through MCP or REST at default settings —
> for any query, at any lexical quality, permanently.** The 0.4 coefficient sits below the
> 0.5 gate outright.

This is not "scores poorly". It is unreachable. And it is not limited to the indexing-lag
case: *any* candidate with zero semantic score — an unembedded engram, a dimension mismatch,
a degraded embed backend — is invisible to both agent-facing surfaces.

It also exposes a **cross-surface split**: at the engine default the same candidate is
returned at 0.291. A library or direct caller sees the memory; an MCP agent does not.

**Independent verification of the formula against the live observation.** The reported
post-embed numbers were `final = 0.742`, `vector_score_raw = 0.881`. Solving the replicated
formula for its only unknown:

```
semCal = (0.881 - 0.520) / 0.480 = 0.7521
0.742  = 0.6×0.7521 + 0.4×fts    =>  fts = 0.7269
```

An implied FTS coverage of 0.73 for a near-exact lexical query is entirely plausible, and an
arbitrary formula would not land the implied value inside [0,1] at a sensible point. This is
a genuine cross-check of the replication against a live server. Pinned in
`TestSCWReconstructsObservedLiveScore`.

The counterfactual is the finding: the **same candidate**, one moment earlier, same FTS
coverage, no embedding → `0.4 × 0.7269 = 0.291` → below the 0.5 surface gate → zero results.
Nothing about the memory or the query changed. Only the embedding landed.

**Noisy-OR and max both fix this case** (they return the perfect lexical match at 1.0),
which is the first evidence that one composition change addresses both directions of the
defect. That strengthens the case for running the harness, not for shipping the change.

### 3.5 Exposure of the indexing lag

Quantified from `internal/plugin/retroactive.go`:

- poll interval **3s** (`:25`); idle back-off `3s × 2^min(idle,6)` capped at **3 minutes**
  (`:36`, `:188-192`); reset to fast polling on real work (`:195-197`).
- A `notifyCh` fast-wake path exists (`:169`) — **but no write path calls `Notify()`**. The
  only caller in the tree is the processor itself, when it hits `maxBatchSize` and knows more
  work remains (`:444`).

So the exposure window is **~3s on a busy vault** (what the live reproduction saw) and **up
to ~3 minutes on a vault quiet for ~3 minutes** — reaching `consecutiveIdle=6` takes
3+6+12+24+48+96 ≈ 189s of quiet. The worst case is therefore the *common agent pattern*: a
session begins after a pause, writes a memory, and immediately recalls it.

FTS indexing is also async (`internal/index/fts/worker.go`, 100ms tick) and **drops jobs when
its queue is full** (`:37`, `:143`) — a second, rarer path into the same missing-estimator
state.

Wiring `Notify()` into the write path is an obvious, cheap mitigation. It is **not** a fix:
it shrinks the window, it does not close the structural 0.4 < 0.5 gap. Out of scope for this
pass, and not my file.

### 3.6 The counterweight nobody should skip

COG-26's `b = 0.520` was derived **against the 0.6 linear combiner**
(`docs/internals/invariants.md`, COG-26 entry): the "sem-only survival bar" of cosine ≈ 0.600
is computed as `0.520 + 0.1667×(1−0.520)`, where `0.1667 = 0.1/0.6` — the abstention
threshold divided by the semantic coefficient. **Changing the combiner changes that bar and
invalidates the calibration.**

Measured on the model's own noise ceiling (cosine 0.596, the highest observed out-of-domain
pair in COG-26's 432-pair measurement):

| combiner | ContentMatch at the noise ceiling | vs the 0.1 abstention gate |
|---|---|---|
| linear 0.6/0.4 | **0.0950** | abstains, by 0.005 |
| noisy-OR | **0.1583** | **clears it — noise returns as a result** |
| max | **0.1583** | **clears it — noise returns as a result** |

So the naive fix trades a recall gain for a measurable abstention regression. That is why
this pass ships no behaviour change.

---

## 4. What was built

### `internal/engine/engine_scoring_weights_model_test.go` — no build tag, runs in CI

Offline replication of the live ACT-R formula, term for term with upstream `file:line`
citations, plus the three alternative ContentMatch combiners. Synthetic fixtures only.
Pins: the denominator identity, the `bLevelCap` saturation point, the COG-26 rescale, the
0.6/0.4 structural caps, the corrected worked case, the noise-ceiling counterweight, the
missing-cosine case against the 0.5 surface gate, the reconstruction of the live 0.742
observation, that Confidence is a clean multiplier, and the prior's >20× dynamic range.

It is a replication and not a call because `activation.computeACTR` is unexported and
`activation.Run` needs a built HNSW index. **If the live formula changes, these tests will
not automatically fail** — that limit is stated in the file header rather than hidden.

### `internal/engine/engine_scoring_weights_measure_test.go` — build tag `scoringmeasure`

Read-only real-vault driver. CI builds with `-tags localassets` and never with this tag, so
it does not compile in any CI job. Guards: refuses a data dir containing `muninn.pid`,
requires an explicit `MUNINN_SCOREW_DATA_DIR`, writes no files, prints aggregate statistics
only (never content, concept, summary, tags, entities, IDs, or the vault name).

Design: 4 combiners × {Confidence live, **Confidence ablated to 1.0**} × {FULL-SIGNAL,
PARAPHRASE (FTS forced to 0)} × {engine threshold 0.05, **surface threshold 0.5**}.

The threshold dimension was added after §3.4: measuring only at 0.05 reports numbers no MCP
or REST agent ever sees, and hides that the 0.4 FTS coefficient sits below the surface gate
outright.

Two pre-committed metrics that pull against each other:

- **NDCG@5 on answerable queries** — query text is a sampled engram's own summary; gold is
  that engram by identity.
- **FPR on unanswerable queries** — 20 committed synthetic nonsense probes, authored in the
  file, shaped like plausible memory queries so abstention is non-trivial.

Labels are mechanical, assigned before scoring, identical across arms, and invisible to the
scorer. This is not human relevance judgment and the file says so.

**Confidence confound — both mitigations, as required.** The driver *always reports*
confidence (pool-wide histogram, count below a suspect threshold, mean confidence of each
arm's top-1) **and** runs every arm a second time with Confidence ablated to 1.0. The
ablated rows are the primary read. If ablated and live rank the combiners differently, the
measurement is contaminated and the conf-on numbers must not be quoted. An optional
`MUNINN_SCOREW_MIN_CONFIDENCE` additionally excludes depressed engrams from the pool.

**Indexing-lag confound — settlement is PROVEN, not assumed.** The driver counts active
engrams with no stored embedding, always reports the fraction, and **refuses to run above
1%** (`MUNINN_SCOREW_MAX_UNEMBEDDED_FRAC`). Unembedded engrams are excluded from the pool —
they cannot be scored by the raw-cosine control at all, and silently keeping them would
flatter that arm. So the driver measures the combiners on a settled corpus and does *not*
measure the missing-cosine case itself; that one is pinned analytically in the model file,
where it is exact rather than sampled.

**Stated limits:** Hebbian and PAS-transition boosts are 0 in every arm — the cognition
layer's strongest arguments for itself are absent, so the harness is biased *against* the
parts of the layer it cannot see. The pool is not `engine.Recall`'s output (no entity boost,
tag seeding, traversal, currency, or time filters). Queries are engram summaries, whose
cosine distribution runs higher than real user queries.

---

## 5. Options for the learned-weight loop (proposed, not shipped)

**A. Delete it.** ~300 lines plus a Pebble prefix, a keyspace entry, a clone exclusion, a
migration case, and an LRU throttle — all serving a loop that cannot learn and that nothing
reads. Honest negative results are first-class here (principle #10). Cost: gives up the
idea, and the 0x13 prefix needs a tombstone entry in the keyspace registry.

**B. Wire it in as-is.** Rejected. The dimensions do not map onto the ACT-R knobs (§2.3e),
and the defaults are uniform, so wiring them in would *replace* a measured-ish 0.6/0.4 with
an unmeasured 1/6 — strictly worse and a direct principle #1 violation (explicit config
silently substituted).

**C. Fix it properly, in increments.** Requires, in order: (i) pass the *real* per-result
score components at both call sites; (ii) drop the redundant re-Softmax and normalize by sum
instead; (iii) re-coordinate the six dims onto the knobs the ACT-R formula actually has;
(iv) *then* wire it into scoring behind a per-vault plasticity flag, defaulted off, with a
measurement gate. That is four increments, and step (iv) needs exactly the harness this pass
built.

**Recommendation: A or C, decided on appetite — but not before the harness runs.** In the
meantime the loop should at minimum stop claiming to be feedback: the two call sites pass a
constant, and the field they pass it to is documented as carrying real score components.

The 0.6/0.4 constants need a separate decision. They are not obviously *wrong* — but
"proven optimal" with no recorded derivation cannot stand, and COG-26's calibration is now
downstream of them. Either the derivation gets reconstructed and written down, or the words
come out of the comment.
