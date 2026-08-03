# Score-presentation honesty — design for #773

**Status:** design, ready to build.
**Base commit:** `e5ecb96` (= `origin/develop` tip), read in
`/Users/mjbonanno/github.com/scrypster/muninndb/.worktrees/contradiction-honesty`.
**Issue:** #773 — "Score presentation dresses weak matches as certainties".
**Scope:** presentation only. Ranking, the abstention gate, thresholds and the measured
corpora are unchanged **by construction** (§7.1 proves it structurally, not by assertion).

---

## 0. What this increment refuses to do, stated first

#773 has two halves. **Only the second one is buildable, and this design builds only that one.**

The **answerability half** — "distinguish a related memory from a memory that answers this
query" — is at a **measured ceiling**. `internal/engine/activation/recall_queryset_measure_test.go`
is the instrument and `TestRecallQuerySet_AcceptanceRule` is the verdict: a 48-engram,
50-query labeled set (30 answerable graded near-verbatim/moderate/hard, 20 unanswerable
split out-of-domain / topically-adjacent / present-tense) run against **nine candidate gate
arms** under a **pre-committed** rule. Its recorded finding:

> with the lexical channel silent the gate reduces to a single effective **cosine cutoff** —
> combiner shape is irrelevant, every arm slides along one ROC — and the real defect is on
> the other side: FPR on **topically-adjacent** queries is **87.5%** and on **present-tense**
> queries **100%**. No arm fixes that. Nothing ships behind an argument.

**This design does not propose a tenth arm.** It does not touch `computeACTR`, the
`absolute < req.Threshold` gate, COG-24's coverage formula, COG-26's `b`, or any threshold
default. Any reviewer who finds this design changing which rows come back should reject it:
that would be re-litigating a closed measurement.

What is left is a **pure honesty defect**, and it is entirely ours:

> We cannot compute answerability. We can stop overstating what we *did* compute.

---

## 1. The defect, traced at `e5ecb96`

Three facts, each with a file:line.

**(1) The displayed `score` is per-query max-renormalized, and pins the best row to 1.0.**
`internal/engine/activation/engine.go:2147-2168` (ACT-R path):

```go
maxRaw := 0.0
… if components.Raw > maxRaw { maxRaw = components.Raw }
scale := 1.0
if maxRaw > 1.0 { scale = 1.0 / maxRaw }
…
raw   := math.Min(cc.components.Raw*scale, 1.0)
final := raw * cc.components.Confidence     // ← this is the wire `score`
```

When any candidate saturates (`Raw > 1.0`, which the code's own comment at 2175-2182 notes
happens routinely — the ACT-R prior reaches 3.24× at full Hebbian boost), the argmax lands
at `raw == 1.0` exactly, so `final == Confidence`, and for the overwhelmingly common
`Confidence == 1` that is **`score: 1.0`, for the best row of any query, however weak**.
The measured live case in #773: `vector_score 0.09 → score 1.0`.

The comment at engine.go:2169-2237 already names this ("THE ARGMAX EXEMPTION"). #757's gate
fix removed it from the **gate** (the gate now compares `AbsoluteScore`, line 2247). It was
never removed from the **display**. `score` is still `Final`.

**(2) The honest quantities exist and never reach the MCP caller.**
`activation.ScoreComponents` carries `AbsoluteScore` (engine.go:163-168 — "Raw before the
per-query 1/maxRaw rescale … 0.9 means the same thing on a good query and a garbage one,
which Final does not") and `ContentMatch` (:151-162 — "the only reported number that answers
'is this memory about the query' on an absolute scale comparable across queries"). Both are
mapped onto the wire at `internal/engine/engine.go:2788-2789` into `mbp.ScoreComponents`.

But `internal/mcp/convert.go:69-94` — `activationToMemory` — maps **only**
`SemanticSimilarity`, `SemanticSimilarityRaw` and `EntityBoost` onto `mcp.Memory`.
**`absolute_score` and `content_match` are structurally invisible to every MCP agent.**
They are visible only inside `annotations.substitution_basis`, i.e. only on a COG-28
substituted row. `internal/mcp/types.go:71-77` confirms it: no `absolute_score` field on
`Memory`.

**(3) `confidence` is the only certainty-shaped number on the row, and it means something
else.** `mcp.Memory.Confidence` (types.go:85) is the engram's stored **truth-belief** — the
quantity COG-10 protects ("evidence of relevance, not evidence of truth"; co-activation
never moves it) and COG-23/#747 penalize on contradiction. It is not a retrieval statistic.
An agent reading `memories[0]` sees `score: 1.0` and `confidence: 1` side by side and reads
*"certain match, certain fact."* Both evaluators arrived at that misread independently.

**The composite:** for a query whose answer is not in the vault, the payload an agent
actually consumes is `{concept, content, score: 1.0, confidence: 1}` and there is no field
anywhere on the row that says *weak*.

---

## 2. Decisions

### D1 — Do NOT change `score`. Add a **band** (a word, not a float). Option (b), phased toward (c).

The three options in the issue:

| | verdict |
|---|---|
| (a) stop renormalizing displayed `score` | **rejected for increment 1** |
| (b) keep `score`, add a first-class `relevance` band from the ABSOLUTE score | **ship** |
| (c) both, phased | (a) is the deferred second phase; see §9 |

Why (a) is rejected *now*, not forever:

- **It is not presentation-only.** `Score` is load-bearing downstream: COG-29's demote is a
  `min()` against `Score` and the whole response is re-sorted by it
  (`applyContradictionHonesty`); COG-25's currency reorder permutes **exact score ties**;
  COG-28's doctrine is "the head ranks at the predecessor's `Final`". Changing `Score`
  changes numbers three landed invariants are stated on, and forfeits this increment's one
  clean property (§7.1).
- **It is a breaking wire change.** `mbp.ActivationItem.Score` is non-`omitempty` on MBP,
  REST (alias), gRPC and every SDK; agents and evaluator harnesses filter on
  `score >= 0.8`. `[0,1]-per-query` is the de-facto contract.
- **The #331 rescale exists for a reason** — a hard clamp at 1.0 collapsed all saturated
  scores and destroyed ranking in new vaults. Removing the rescale is a ranking change.

Why (b) is the right shape, and specifically why a **word**:

- The misread is *"a number that looks like certainty."* Answering it with **another
  number** (`absolute_score: 0.09` next to `score: 1.0`) asks the agent to know which of two
  floats to believe. A **categorical** cannot be misread as 1.0. `relevance: "weak"` is the
  field the agent was reaching for when it grabbed `confidence`.
- It reuses a proven in-tree mechanism (principle #7): a read-only post-pipeline annotation
  phase, exactly like `applyCurrencyAnnotation` (COG-25) and `applyContradictionHonesty`
  (COG-29), both of which are already `min()`-only / annotate-only and both of which are
  provable no-ops on the corpora.

**Also ship, alongside the band:** surface `absolute_score` and `content_match` on the MCP
recall row. They already exist on the wire everywhere except MCP; the operator/debug path
(and the band's own auditability) needs them, and it is a two-line map in `convert.go`.
This is COG-26's `semantic_similarity_raw` precedent: the honesty backstop must be readable
without a second tool call.

### D2 — Do **not** rename `confidence`. Supply the field it was being asked to be.

Rename to `fact_confidence` at the MCP layer: **rejected.**

- Drift cost is out of proportion: `Memory.Confidence` is shared by `muninn_recall` **and**
  `muninn_read` (`readResponseToMemory`), plus `muninn_state`, `where_left_off`, `brief`,
  the web console, and Python/Node/PHP SDKs — none of which have a parity gate (obligation
  #3 is warning-only). A rename either breaks all of them or ships a duplicate field
  forever.
- The diagnosis is sharper than "bad name". `confidence` is misread **because it is the
  only certainty-shaped number on the row**. Once `relevance` exists, `confidence` stops
  having to carry a meaning it never had. Fixing the vacancy is the fix; the rename is a
  second, weaker attempt at the same thing.

But a guide/tooltip fix **alone** is not honest enough either — the misread happens while
reading a JSON payload, not while reading docs. So D2 is three things, all cheap:

1. `relevance` on the row (D1) — the structural fix.
2. **The weak-band hint text explicitly separates the two** (D3): the sentence is delivered
   in the payload, at the moment of the misread, not in a document.
3. Guide (`internal/mcp/guide.go`) + `muninn_recall` tool description each get one
   paragraph: *score is query-relative, relevance is absolute, confidence is truth-belief.*

`fact_confidence` as an additive alias stays available as a later lever if a future round
still measures the misread. Named, not built.

### D3 — Weak-band-only hint: response-level, evidence-phrased, never fires on strong results.

When **every banded row** in a non-empty response is `weak`, the response carries a
`relevance_summary` block with a hint. Wording (final):

> `Nothing matched strongly. The best absolute relevance here is 0.14 on a 0–1 scale where
> this vault's noise floor is ~0.10 — these memories are RELATED to your query, not
> necessarily ANSWERS to it. Note: 'score' is relative to this query's own best candidate,
> so the top row is near 1.0 on every query including this one; 'confidence' is belief that
> the stored fact is TRUE, not a measure of how well it matches. Verify before relying on
> these.`

Three properties of that wording, each deliberate:

- **Evidence-phrased, not conclusion-phrased.** It says *nothing matched strongly*, never
  *there is no answer here*. That matters because a genuine hard paraphrase (the `rqHard`
  band) legitimately produces weak absolute evidence — the hint must still be **true** when
  the weak row happens to be right. This is the line that keeps us out of re-litigating §0.
- It carries D2's distinction at the point of misread.
- It quotes the vault's own numbers, so it is checkable.

**Interaction rules:**

- **Abstention block** (`abstained`/`abstained_reason`): mutually exclusive **by
  construction** — `handlers.go:595` only emits it when `len(memories) == 0`, and
  `relevance_summary` requires ≥1 banded row. Pinned by assertion, not left to luck.
- **Conflict block** (COG-29): independent; both may appear. The hint text makes no claim
  about disputedness and the conflict warning makes no claim about relevance. Note the
  clean separation: COG-29's demote multiplies **`Score`**, and the band reads
  **`AbsoluteScore`** — so **a demoted row never changes band**. That is correct and
  deliberate ("is this disputed" and "did this match" are different questions) and it gets
  its own pin.
- **Never on strong/moderate.** One `moderate` row suppresses the hint entirely. Rows
  banded `filter_match` or `uncalibrated` (§3) are excluded from the all-weak test in both
  directions — they neither trigger nor suppress it.

### D4 — Band edges: derived per-vault from quantities the vault already self-calibrates.

Principle #11 forbids a constant tuned on one corpus. It permits **self-derivation** (like
#711's per-corpus IDF, COG-25's self-derived ubiquity ratio) or a per-vault override.

Both anchors here are **ratios against quantities the vault itself resolves**, not values
lifted from a measurement:

```
calibrationGate = the vault's COG-6 fusion-aware DEFAULT recall threshold
                  (ACT-R: 0.1) — deliberately NOT req.Threshold; see below
contentCeiling  = w.SemanticSimilarity + w.FullTextRelevance
                  from the RESOLVED weights activation.Run actually used

weakMax   = min(relevanceWeakGateMultiple   * calibrationGate,  strongMin)   // default 2.0
strongMin = relevanceStrongCeilingFraction  * contentCeiling                 // default 0.5
```

**Why `2 × calibrationGate` is the weak ceiling.** COG-26 deliberately placed the *measured
out-of-domain noise ceiling* immediately under the gate: `b = 0.520` was chosen because the
top observed noise cosine (≈0.596) rescales to `semCal ≈ 0.158`, i.e. `ContentMatch ≈ 0.095`
— clearing a 0.1 gate "by only ≈0.005" (COG-26, verbatim). So **on this codebase, "just
above the gate" already means "arithmetically indistinguishable from this model's noise."**
A row within one doubling of that floor is weak, and that statement is a restatement of a
landed invariant rather than a new number.

**Why `0.5 × contentCeiling` is the strong floor.** engine.go:2218-2227 records the measured
structural fact: `ContentMatch` is capped at `w_sem` (0.6) for a semantic-only match and
`w_fts` (0.4) for a lexical-only one, so **"NO honest absolute score reaches 0.5 without
near-verbatim wording (cos ≥ 0.9200)."** Expressing it as a *fraction of the vault's own
resolved ceiling* is what makes it per-vault: a vault that reweights its channels gets band
edges that move with its weights, and on default weights (0.6 + 0.4 = 1.0) it reproduces the
in-tree 0.5 exactly.

**Why the calibration gate, not the caller's `threshold`.** The noise floor is a property of
the model and the vault, not of what the caller asked for. If the anchor were
`req.Threshold`, a caller passing `threshold: 0.01` would see a 0.09 row banded *moderate* —
a row **below the model's own noise ceiling**. That is the exact silently-plausible failure
class this increment exists to remove. The band therefore anchors on the vault's resolved
default; `req.Threshold` is used only for the `filter_match` test below.

**Honest statement about the two ratios.** They are **judgment, not measurement.** No corpus
was consulted to pick 2.0 and 0.5; each is a ratio whose *meaning* is sourced to a landed
invariant. §7.3 validates the resulting band **assignment** against the labeled 50-query set
under a **pre-committed** rule — it does not tune the ratios against it, which would be the
principle-#11 violation the query-set file itself warns about. Both live as named constants
in one place with that derivation in the comment (principle #6).

**Per-vault override is deferred, and named:** a `relevance_bands` plasticity block
(`weak_gate_multiple`, `strong_ceiling_fraction`, clamped COG-2 style, `0` = unset). Adding
plasticity fields fires obligation #4 (web preset cards + JS + a pinning test) for zero
increment-1 value, and the edges are already per-vault-derived without it. This mirrors
COG-26 shipping `SemanticFloorOverride` as its recourse lever rather than its mechanism.

### D5 — Where a band is emitted, and where it must refuse to be.

The rule is one predicate, reused rather than reinvented: **a band is emitted exactly where
the fusion mode gates on `AbsoluteScore`.**

| condition | `relevance` | `relevance_basis` |
|---|---|---|
| RRF fusion | `uncalibrated` | `rrf_fusion` |
| legacy weighted-sum (`DisableACTR`) | `uncalibrated` | `weighted_sum_fusion` |
| COG-26 baseline came from the identity fallback (unregistered/empty embed model) | `uncalibrated` | `no_model_baseline` |
| `SemanticDegraded` on this response | `uncalibrated` | `semantic_degraded` |
| `contentCeiling <= 0` | `uncalibrated` | `no_content_channel` |
| `AbsoluteScore < req.Threshold` (admitted by an explicit tag filter, COG-5 S1) | `filter_match` | `tag_filter_bypass` |
| `AbsoluteScore >= strongMin` | `strong` | — |
| `AbsoluteScore >= weakMax` | `moderate` | — |
| otherwise | `weak` | — |

Each exclusion is a decision, in the style COG-18 R1 / COG-28 already use:

- **RRF** — rank-based finals with a coerced ~0.001 default; "cleared the bar" carries
  almost no information and the noise/ceiling reasoning does not hold. Same calibration
  reason COG-18 R1 skips the entity boost and COG-28 collects no shadows.
- **weighted_sum** — its `AbsoluteScore` is reported "for parity" only and the path is *not*
  gated on it (engine.go:2289-2294); `ContentMatch` is the ACT-R aboutness term and this
  path computes no comparable quantity. Banding it would be inventing a number.
- **No model baseline** — COG-26 gives an unregistered model the identity transform plus a
  WARN. Under identity, `semCal` is raw cosine, whose noise floor is ~0.45, not ~0. Every
  band would be wrong. **Cold start displays `uncalibrated` with the reason. Loud, never
  invented** — this is #582/#585/#589 doctrine applied to a display field.
- **Degraded** — the semantic weight is redistributed onto FTS and the ceiling shifts
  (COG-24's residual: ≈0.385–0.4). The response already says `semantic_degraded: true`;
  banding against a shifted ceiling would be plausible-and-wrong. A lexical-only band is a
  named deferral (§9).
- **`filter_match`** — COG-5 S1's threshold bypass admits explicit tag-filter matches
  *below* the bar (a `due:<=today` reminder whose content is unrelated). Calling those
  `weak` would be technically true and practically a lie: the filter defined the set, not
  relevance. They get their own value and are excluded from the D3 all-weak test, so a
  reminder query never triggers the "these are just neighbors" hint.

  *Implementation note.* `inTagPool` is not currently carried past
  `activation.scoredItem` (engine.go:1932-1937), so the phase derives it: on the ACT-R and
  CGDN paths the **only** admission routes are `absolute >= req.Threshold` or `inTagPool`
  (engine.go:2090, 2247), so `absolute < req.Threshold` **⟺** filter bypass. That equivalence
  must be **pinned by a test**, and it is a named residual: if a future admission path lands
  below the bar it would be mislabeled `filter_match`. If the build agent finds a second
  sub-gate admission route, plumb an explicit `AdmittedByFilter bool`
  `scoredItem → ScoredEngram → ActivationItem` instead of deriving it.

- **CGDN caution.** At engine.go:2085-2093 `absolute` is computed *before* `cc.components.Raw`
  is overwritten with the CGDN ratio `r`. The band depends on that ordering. Add a comment
  at both CGDN sites; a refactor that reorders those two lines silently corrupts every band
  on that path.

### D6 — Placement: a post-pipeline phase in `internal/engine`, plus one pure function.

- **Pure function** `activation.RelevanceBand(abs, calibrationGate, contentCeiling) (band, basis string)` —
  no I/O, no engine state, table-testable for free, and callable from the activation-package
  measurement harness (§7.3) without the engine phase existing there.
- **Phase** `internal/engine/engine_relevance.go` → `applyRelevanceBands(resp, …)`, run in
  `activateCore` **after** currency (COG-25) and contradiction honesty (COG-29) and before
  response assembly, so it sees the final row set. It **reads only**: no score change, no
  reorder, no truncation, no removal, no write. Same contract as COG-29's phase.

---

## 3. Wire surface (all additive)

**`internal/transport/mbp/types.go`**

```go
// ActivationItem
// Relevance is the ABSOLUTE relevance band for this row: strong | moderate |
// weak | filter_match | uncalibrated. Derived from ScoreComponents.AbsoluteScore
// against this vault's own calibration (its resolved default gate and its resolved
// content-channel ceiling) — NOT from Score, which is renormalized per query and
// pins the best row of ANY query, however weak, to ~1.0. RelevanceBasis is set
// only for filter_match and uncalibrated and names WHY.
Relevance      string `msgpack:"relevance"                 json:"relevance"`
RelevanceBasis string `msgpack:"relevance_basis,omitempty" json:"relevance_basis,omitempty"`
```

```go
// ActivateResponse
// RelevanceSummary is present only when EVERY banded row in a non-empty response
// is weak: nothing in this response matched strongly. nil otherwise — an
// annotation on every response would stop meaning anything.
RelevanceSummary *RelevanceSummary `msgpack:"relevance_summary,omitempty" json:"relevance_summary,omitempty"`

type RelevanceSummary struct {
    BestBand     string  `msgpack:"best_band"                json:"best_band"`      // always "weak" today
    BestAbsolute float32 `msgpack:"best_absolute"            json:"best_absolute"`
    NoiseFloor   float32 `msgpack:"noise_floor"              json:"noise_floor"`    // the vault's calibrationGate
    Banded       int     `msgpack:"banded"                   json:"banded"`         // rows that got a real band
    Hint         string  `msgpack:"hint,omitempty"           json:"hint,omitempty"`
}
```

**REST** — inherits both via the `rest.ActivateResponse = mbp.ActivateResponse` alias. Zero
code, but `openapi.yaml` is a **manual** obligation (#2).

**MCP** — `internal/mcp/types.go`:

```go
// Memory
Relevance      string  `json:"relevance,omitempty"`       // recall only; read omits it
RelevanceBasis string  `json:"relevance_basis,omitempty"`
AbsoluteScore  float64 `json:"absolute_score,omitempty"`  // COG-26 backstop parity
ContentMatch   float64 `json:"content_match,omitempty"`
```

`Relevance` is **top-level, deliberately not inside `MemoryAnnotations`** — `convert.go:39-41`
has an "allocate an annotations object at all" predicate that has already silently dropped a
field once (#764, drift-and-obligations note 3). A top-level field cannot be dropped by it.
It is `omitempty` because `Memory` is shared with `muninn_read`, which must not emit an empty
band; the recall path always sets a non-empty value, pinned by a test.

`handlers.go` recall — the hand-built `map[string]any` (the **other** #764 trap):

```go
if resp.RelevanceSummary != nil {
    result["relevance_summary"] = resp.RelevanceSummary
}
```

**gRPC / proto / non-Go SDKs** — **nothing added, deliberately.** Their `ActivationItem` is a
minimal subset carrying no annotation fields at all; obligation #3 is explicit that adding
part of the block is the silently-wrong class. Named omission.

**Docs** — `internal/mcp/guide.go` gets one "reading a recall result" paragraph; the
`muninn_recall` tool description gets one sentence. Both state the three-way distinction.

---

## 4. #769 — verdict: **NOT a rider. It needs its own loop.**

#769: after `muninn_evolve` changes a fact's substance, the successor's `content` is current
but its `concept` still carries the predecessor's wording; `where_left_off` and every
concept-surfacing view then present the stale claim as current. Round-7 **and** round-8
confirmed.

It looks like a sibling ("both are display honesty"). It is not, for three reasons:

1. **It is not presentation — it is stored data on a scored field.** `concept` is an FTS
   field with `fieldWeightConcept = 3.0` (COG-24). Changing which concept a successor carries
   changes postings → coverage → `ContentMatch` → `AbsoluteScore` → **which rows come back
   and in what order**. This increment's single clean property is "the corpora are unchanged
   by construction" (§7.1). Riding #769 forfeits it, and forfeits it on the exact quantity
   the bands are computed from.
2. **Different surface, different invariants.** #773 is a read-only post-pipeline
   annotation. #769 is the write path: `Evolve`/`EvolveAt`, MCP/MBP/REST argument surfaces,
   COG-20 importance inheritance, COG-28 chain resolution, plus a `warnings[]` contract.
   Bundling them makes one PR that no single reviewer routing (CLAUDE.md §4) covers.
3. **Its real content is an unmade product decision.** Optional `concept` param vs.
   carried-concept warning vs. both is a design choice deserving its own RED and its own
   refute — not a paragraph appended to someone else's design (principle #5).

**So the loop isn't lost, here is #769's increment in four lines:**

- (a) Accept an optional `concept` on `muninn_evolve`, plumbed `MCP → mbp.EvolveRequest →
  Engine.EvolveAt`. Verify first whether the MCP tool schema exposes one at all — #769 says
  "verify", and it is the cheapest thing to check.
- (b) When the caller supplies new content and **no** concept, keep carrying the
  predecessor's (a reasonable default) **and return a `warnings[]` entry naming the carried
  concept verbatim and how to set one**. Loud, never silent — the `WriteResult.Warnings`
  channel already exists.
- (c) **Never derive a concept.** No LLM, no heuristic, no truncation of new content into a
  title. Principle #1: an invented-but-plausible concept is strictly worse than a stale one
  you were warned about.
- (d) RED: evolve a fact's substance without a concept; assert the warning fires and names
  the carried concept; assert a supplied concept lands on the successor and reaches recall.
  Both rounds already reproduce it, so the RED is nearly free.

#769's two folded-in siblings (`muninn_decide(evidence_ids=[old_id])` warning "soft-deleted"
instead of hinting the successor; the guide's session-start recipe that abstains) are cheap
UX fixes and belong with (a)-(d) or in a docs/UX increment — not here.

---

## 5. Minimal first increment (hand this to a build agent in this order)

1. **`activation.RelevanceBand`** — pure function + the two named constants with the D4
   derivation comment. Table test over the edge cases (below gate, at gate, at `weakMax`, at
   `strongMin`, degenerate `weakMax >= strongMin`, `contentCeiling <= 0`).
2. **Calibration inputs, computed once.** `activation.Run` reports the resolved
   `contentCeiling` and effective fusion mode on its result (one computation site — principle
   #6; do **not** re-resolve weights in the engine and hope they match).
3. **`internal/engine/engine_relevance.go`** — `applyRelevanceBands`: per-row band + basis,
   the D5 exclusion table, and the D3 response summary/hint. Read-only.
4. **Wire** — mbp fields; MCP `types.go`/`convert.go` (incl. `absolute_score`,
   `content_match`); `handlers.go` map key; `openapi.yaml` + `npx @redocly/cli lint`;
   `guide.go` + tool description.
5. **Tests** — §7.
6. **Measurement** — §7.3, with the rule written into this file **before** the first run.

Build/verify: `go build -tags localassets ./... && go vet -tags localassets ./... && gofmt -l .`,
plus `-race` on the engine tests (post-pipeline phase on the recall path).

---

## 6. Acceptance — measurable

### 6.1 Corpora unchanged **by construction** (the load-bearing property)

Not an assertion — a placement argument, then two pins:

- The three measurement harnesses (`abstention_gate_measure_test.go`,
  `recall_queryset_measure_test.go`, `shadow_measure_test.go`) call
  **`activation.Run` directly**. `applyRelevanceBands` lives in `internal/engine` and runs
  *after* `Run` returns. **The harnesses cannot see it.** Their numbers cannot move.
- **Pin 1** `TestRelevanceBands_IsAPureAnnotation` — run `Activate` over a fixture with the
  phase applied and not applied; `reflect.DeepEqual` the two responses after zeroing
  `Relevance`/`RelevanceBasis`/`RelevanceSummary`. Same rows, same order, same scores, same
  `TotalFound`, same abstention. (COG-29's `TestContradictionHonesty_NoContradictionsIsANoOp`
  is the template.)
- **Pin 2** The phase writes nothing — observe-safe by construction (COG-11), same as COG-29.

### 6.2 RED repros on `e5ecb96` (`internal/engine/engine_relevance_test.go`, real bge-small, localassets)

One fixture, the round-8 shapes. Each test names what it asserts, not merely that a new
field exists — a new-field test is trivially red and proves nothing.

| test | assertion | RED at `e5ecb96` |
|---|---|---|
| `TestRelevanceBand_WeakNeighborIsNotDressedAsCertain` | the round-8 **"light"** shape (ambiguous single word over a weakly-related memory): premise `Score >= 0.9 && AbsoluteScore < 0.2`; assert `memories[0].relevance == "weak"` and `relevance_summary.hint != ""` | today: `score 1.0`, `confidence 1`, **no field on the row says weak** |
| `TestRelevanceBand_AbsentFactQuery_AllWeakHint` | the round-8 **absent-composer / deleted-door-code** shape: every returned row `weak`, hint fires, hint text names both `score` and `confidence` | today: neighbors returned at `score 1.0` with no qualifier |
| `TestRelevanceBand_StrongMatch_NoHint` | near-verbatim query → `strong`, `relevance_summary == nil` | anti-overfire control |
| `TestRelevanceBand_ModerateMatch_NoHint` | reworded query → `moderate`, no hint | anti-overfire control |
| `TestRelevanceBand_TagBypassIsFilterMatch` | `due:<=today` filter, content unrelated → `filter_match`, **hint does not fire** | COG-5 S1 interaction |
| `TestRelevanceBand_UncalibratedModel_NoBandNoHint` | vault with no registry baseline → `uncalibrated` + `no_model_baseline`, no hint | cold start, loud |
| `TestRelevanceBand_RRFVault_Uncalibrated` | rrf vault → `uncalibrated` + `rrf_fusion` | named exclusion |
| `TestRelevanceBand_ContradictionDemoteDoesNotChangeBand` | a COG-29-demoted row keeps the band it had pre-demote | `Score` moved, `AbsoluteScore` did not |
| `TestRelevanceBand_SubstitutedRowBandsOnPredecessorEvidence` | a COG-28 substituted row bands on the predecessor's `AbsoluteScore`, consistent with `substitution_basis` | the components on that row *are* the predecessor's |
| `TestRelevanceBand_AbstentionAndSummaryAreExclusive` | never both in one response | D3 contract |
| `TestRecallOverMCP_RelevanceBandAndSummary` (`internal/mcp/`) | `relevance` survives on a row carrying **no other annotation**; `relevance_summary` survives the hand-built map | the two #764 traps, verbatim |
| `TestRelevanceBand_EveryRecallRowIsBanded` | every recall row has non-empty `relevance`; `muninn_read` emits none | the `omitempty`-sharing risk |

### 6.3 Aggregate measurement — pre-committed rule, written **before** the first run

Add band columns to the existing `TestMeasureRecallQuerySet` (it already runs the 50-query
labeled set with a real embedder, so the marginal cost is a `RelevanceBand` call per row).
Report, per label kind:

- **U** = of the 20 **unanswerable** queries that returned ≥1 row, the fraction where the
  D3 hint would fire (every banded row weak).
- **A** = of the **answerable** queries where the gold was found, the fraction where the hint
  would fire, split by difficulty (`rqNearVerbatim` / `rqModerate` / `rqHard`).

**Pre-committed rule (do not move these after seeing the numbers):**

1. **U ≥ 70%** across `rqAdjacent` + `rqStale` + `rqOOD` combined. These are the queries the
   #757 measurement showed we *cannot stop returning* (87.5% / 100% FPR). We are not fixing
   that. We are claiming that when we return them, we now say so.
2. **A(nearVerbatim) = 0% and A(moderate) = 0%.** The hint must never fire on a query whose
   gold answer was found with real evidence. A single violation blocks the hint.
3. **A(hard): report only, not a gate.** A hard paraphrase genuinely *is* weak absolute
   evidence; the D3 wording ("nothing matched strongly", not "no answer here") stays true
   there. Quantifying it is the point; failing on it would be punishing honesty.

**If rule 1 fails:** ship the **per-row band anyway** (strictly more information than today,
and the #1 evaluator complaint is the row, not the response) and **defer the hint** with the
measured number recorded here. That is the honest negative-result path (principle #10), and
it is pre-committed too.

### 6.4 Drift walk

| obligation | applies | action |
|---|---|---|
| #1 MCP tool handler | no | no new tool; registry smoke untouched |
| #2 REST route/handler | **yes (fields)** | `openapi.yaml`: `relevance`, `relevance_basis`, `absolute_score`, `content_match` on the item schema; `relevance_summary` on the response schema; Redocly lint 🪝 |
| #3 REST types → SDKs | **yes** | additive only. gRPC/proto/non-Go SDKs: **deliberate omission** (they carry no annotation block at all). Both #764 traps re-checked: `convert.go` allocation predicate (avoided — field is top-level) and `handlers.go` hand-built map (explicitly handled) 🪝 |
| #4 plasticity preset | no | override deferred (D4) — this is *why* it is deferred |
| #7 proto regen | no | proto untouched |
| #8 new Pebble prefix | no | none |
| #9 `-tags localassets` | **yes** | the band tests are asset-gated; keep the tag |
| #12 async worker | no | phase is synchronous and read-only |
| CI budget | ~seconds | one asset-gated engine test file + pure-function unit tests + columns on an existing measure test. No new job, no Playwright, no integration tag. |

Also touched, non-obligation: `internal/mcp/guide.go` (+ `guide_test.go` if it asserts
content), the `muninn_recall` tool description. **Web console: deferred** — it renders
`score`; a band chip is a follow-up, named in §9.

---

## 7. Risks

- **R1 — the two ratios are judgment.** Mitigated three ways: each is sourced to a landed
  invariant rather than a corpus (D4); §6.3 validates the *assignment* under a pre-committed
  rule rather than tuning the ratios to the set; the hint is evidence-phrased so it survives
  a mis-set edge. Residual: a vault whose bands read wrong has no override until increment 2.
- **R2 — hint over-fires on hard paraphrases.** Real and expected (§6.3 rule 3). The wording
  is the mitigation. Do **not** "fix" it by raising the gate — that is §0.
- **R3 — `filter_match` is derived, not plumbed.** The `absolute < req.Threshold ⟺ tag
  bypass` equivalence is true on the ACT-R/CGDN paths today and must be pinned. If a second
  sub-gate admission path exists or appears, plumb an explicit flag instead (D5).
- **R4 — CGDN ordering dependency.** `absolute` is computed before `Raw` is overwritten with
  the CGDN ratio; a refactor reordering those two lines corrupts every band on that path.
  Comment both sites.
- **R5 — two relevance-ish quantities on one row.** Mitigated by the band being a **word**
  and by the guide paragraph. Agents filtering on `score >= 0.8` keep working unchanged —
  and keep the misread. That is the accepted cost of *not* shipping option (a) yet.
- **R6 — the band is only as calibrated as COG-26's `b`.** Its own irreducible sub-band
  (cosine ≤ ~0.596) is inherited: near the floor, weak-vs-noise is genuinely undecidable.
  The band says `weak` there, which is the honest answer, and the hint says why.

---

## 8. Deferrals, each named

1. **Option (a): stop renormalizing the displayed `score`** — the second phase of D1. Needs
   its own increment because it touches COG-25/COG-28/COG-29 numbers and the wire contract.
   Worth doing *after* bands have been in the field: with `relevance` present, `score` can be
   redefined as an explicit rank strength (or joined by `absolute_score` as the primary) with
   consumers already reading the honest field.
2. **`relevance_bands` per-vault plasticity override** (D4) — obligation #4's cost for zero
   increment-1 value.
3. **`fact_confidence` additive alias** (D2) — only if a later round still measures the
   misread with `relevance` shipped.
4. **Lexical-only bands under `SemanticDegraded`** (D5) — needs the redistributed ceiling
   measured, not assumed.
5. **RRF and weighted-sum bands** (D5) — rank-based finals cannot abstain by construction
   (COG-24 deferral 4); a band there would need its own calibration story.
6. **gRPC/proto + non-Go SDK annotation block** — obligation #3: whole block wired
   end-to-end, or nothing.
7. **Web console band chip.**
8. **#769** — its own increment, sketched in §4.

---

## 9. One-paragraph summary for the PR body

Recall's displayed `score` is renormalized per query, so the best row of *any* query —
including one whose answer the vault does not contain — is pinned to ≈1.0, and `confidence`
(a stored truth-belief, COG-10) is the only certainty-shaped number on the row, so agents
read the pair as "certain match, certain fact". The engine already computes the honest
quantities (`AbsoluteScore`, `ContentMatch`) and never shows them to an MCP caller. This
increment adds a first-class **`relevance` band** (strong / moderate / weak / filter_match /
uncalibrated) derived from `AbsoluteScore` against the vault's **own** calibration — its
resolved default gate, which COG-26 deliberately placed at the model's measured noise
ceiling, and its resolved content-channel ceiling — plus a response-level hint when *every*
banded row is weak, and surfaces `absolute_score`/`content_match` on the MCP row. Ranking,
the abstention gate and every threshold are untouched: the phase is read-only, runs after
`activation.Run` returns, and the measurement harnesses cannot see it. It does **not**
attempt answerability — #757 measured that ceiling against nine arms under a pre-committed
rule and this is not a tenth.
