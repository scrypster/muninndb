# Agent-experience findings — hands-on evaluation, 2026-07-31

Four AI models (Grok 4.5, GPT-5.6, Composer 2.5, Opus 5) each ran a realistic multi-session
workflow against a live sandbox over ~150 tool calls, as the product's actual user rather than as
code reviewers. A separate three-model design panel then answered the questions those runs raised.
This document records what they found, what was independently reproduced, and what it implies.

Every number here came from a live server. Claims that could not be reproduced are marked as
refuted, because a finding nobody can reproduce is worth less than no finding at all.

---

## 1. What they liked (unprompted, and consistent across models)

- **`muninn_evolve`.** All three external evaluators named it as the moment the product justifies
  itself: after evolving, default recall returns only the current fact and the predecessor is
  soft-deleted with a closed validity window.
- **Bitemporality** (`valid_from`/`valid_until`, `forget(not_true_since)`). "A vector store cannot
  fake those; you would rebuild them badly."
- **`muninn_entity_timeline`** — "the best tool in the product". *What changed about X, in order*
  is a question similarity search structurally cannot answer, and it answers it in one call.
- **Exact-duplicate detection**, `remember_tree`/`recall_tree`, and rejecting an impossible
  validity window instead of storing temporal nonsense.
- **The type-rejection hint shipped in #742** — called "textbook loud degradation", and explicitly
  named as the pattern the rest of the write path should copy. It landed hours before the
  evaluation and was noticed without prompting.

The praise is narrow and specific, and it clusters on the *declared* path: bitemporality, explicit
supersession, entity timelines. That is the product's real differentiator.

---

## 2. Confirmed defects

Reproduced independently; each needs its own fix.

| # | Defect | Evidence |
|---|---|---|
| 1 | **Declaring a contradiction destroys both memories** | confidence 1.0 → 0.797 → 0.313 → 0.0709 on BOTH sides; recall returns 0 |
| 2 | `as_of` cannot read an evolved predecessor | soft-delete gate runs before the validity filter; `restore` makes it work |
| 3 | `muninn_explain` returns all zeros unconditionally | including `threshold:0`, `confidence:0` for an engram whose confidence was 1.0 |
| 4 | Abstention is inverted | refused answerable questions; returned 5 confident results for an unanswerable one |
| 5 | `muninn_decide` produces a `fact`, not a `decision` | inherits fact-tier importance 0.4; invisible to type filters |
| 6 | `conflicts_with` annotates only one side | the stale memory ranked #1 carrying `"stale": false` |
| 7 | `restore` resurrects an expired fact into present-tense recall | returned at 0.532 with no annotation |
| 8 | Write responses report `"concept":""` | serialization only — the concept IS stored correctly |

### The headline: contradiction as data loss

Two opposing memories, linked `contradicts` in both directions — the natural thing for an agent to
do — both drop to confidence 0.0709 within ~80s and vanish from recall. The **correct** memory is
punished exactly as hard as the false one; the penalty is a flat blanket value, not
evidence-weighted, and confidence multiplies into the recall score.

Root cause: the same declared fact is treated as N independent Bayesian observations
(`1.0 → 0.975 → 0.797 → 0.313 → 0.0709`). Per-event, not time-based — a linked pair left alone
holds steady.

This is the project's worst failure class (silent, plausible-looking wrong answers) reached by
doing the *right* thing. Declarations are the scarcest resource in the system — 0.135% of
association edges are agent-authored — so a declaration that destroys data is worse than none.

**Two deeper problems sit underneath the immediate bug:**

1. **Penalising both sides is incoherent.** When A and B contradict, at most one is wrong. Halving
   confidence in both is not Bayesian evidence, it is a tax on honesty. A contradiction should
   mark a *conflict*, not degrade *truth*.
2. **Confidence should not silently gate visibility.** Multiplying a "there is a dispute here"
   signal into the recall score converts an annotation into a deletion.

### Refuted claims

Recorded because refutations are findings too:

- **"Version-clustering mislabels unrelated memories."** It does not — across five queries built to
  trigger it, the advisory currency signal produced *nothing whatsoever*. The feature never fired.
- **"`muninn_decide` loses the concept."** The concept is stored correctly; only the response
  serialization is empty. Two evaluators concluded data loss from this. The misdiagnosis is itself
  a cost of the bug.
- **"`muninn_contradictions` is permanently empty."** It is populated by an async worker on a 30s
  interval. It was empty for the entire window in which a working agent would look — which is why
  three independent evaluators called the feature dead. The honest answer is "not processed yet",
  not `[]`; an empty result standing in for an unknown one is the same silent-substitution class as
  #742/#743/#745.

---

## 3. The structural critiques

These are design questions, not bugs, and they matter more than the bug list.

### The cheap path silently succeeds and produces near-garbage

A bare `remember(content)` is ~20 tokens and yields an empty concept, no entities, and invisibility
to every entity-based tool. A well-formed write is roughly **8× the content in JSON**. There is no
middle gear, and **every evaluator said they would take the cheap path under time pressure with a
user waiting.** That is not an agent-discipline problem — it is what happens when the expensive
path is entirely voluntary and the cheap path silently succeeds. The measured 4.81% declaration
rate on a real corpus is the predictable result.

### The cognition layer is currently unproven, and in this run net-negative

ACT-R fusion turned a correct 0.758-raw-cosine answer into a below-threshold miss while returning
five confident irrelevancies to a question the vault could not answer. The Hebbian graph produced
~35 edges over 8 nodes — a near-complete graph that discriminates nothing. `score` is
max-normalised, so the top hit is always exactly 1 regardless of quality and cannot be used as a
confidence signal or a cutoff.

The honest reading is that the bitemporal + entity-timeline core is the product, and ACT-R fusion
and Hebbian association are hypotheses that have not yet been shown to beat plain cosine on a real
corpus. That is a measurement to run, not an assumption to keep.

### Newly-written memories are silently unfindable until the embedder catches up

Verified live at commit c3b5b60. Write a memory, query it seconds later with near-exact lexical
overlap, and recall returns ZERO results. Wait for the retroactive embed pass and the identical
query returns it at score 0.742 (raw cosine 0.881).

Timeline from the daemon log:

    22:05:31  evolve creates the successor
    22:05:34  activation complete ... results=0        <- the query
    22:05:36  retroactive processor: complete           <- embedding lands AFTER

Mechanism: `ContentMatch = 0.6*semCal + 0.4*ftsCoverage`, where `semCal` is the COG-26-calibrated
semantic score (there is no `tanh` on FTS — #711 removed it). Before the embedding exists, cosine is 0 —
scored as *zero semantic relevance* rather than *unknown* — so ContentMatch cannot exceed 0.4
however perfect the lexical match, which falls below the default threshold. Recall returns an empty
set with a hint suggesting `mode=recent`, indistinguishable from "no such memory".

This is the same defect as the paraphrase cap, from the other direction: a MISSING estimator is
treated as a zero rather than as absence of evidence. It matters more than the paraphrase case
because agents write-then-recall constantly within a session, and it plausibly explains part of the
"abstains on answerable questions" behaviour observed during the hands-on evaluation — those vaults
were being written and queried in the same minutes.

It is also the third cause initially mis-attributed to ACT-R fusion, after the confidence collapse
and the 0.6 content-match cap. The lesson recorded here: before blaming the cognitive layer,
account for confidence, for missing-signal handling, and for indexing lag.

Candidate fixes to EVALUATE (not to ship on argument): noisy-OR or max composition so a missing
estimator does not cap the other; and/or an explicit "not yet indexed" signal on the response so an
empty result is never silently confused with an unknown one.

### The combiner and the abstention floor are coupled — the obvious fix has a cost

A traced measurement of the live scoring path (commit `ada475e`) corrected two things believed
earlier in this investigation, and the correction matters more than the original claim.

**The production formula**, verified against source:

    semCal       = max(0, (cos - b) / (1 - b))
    ContentMatch = 0.6*semCal + 0.4*ftsCoverage
    B(M)         = ln(n) - 0.5*ln(max(ageDays, 1min)/n),  n = AccessCount+1, capped ~1.4892
    prior        = softplus(B(M) + 4*hebbian + 4*transition) / 1.6931
    final        = ContentMatch * prior * Confidence

Relevance multiplicatively gates salience; salience cannot rescue a zero-relevance memory (the one
exception is COG-5's tag floor, which sets ContentMatch=0.1 for an explicit tag match).

**The earlier worked example was optimistic.** Omitting the COG-26 rescale gave 0.455 for a
cosine-0.758 paraphrase; the real value with `b=0.520` is `0.6 * 0.4958 = 0.2975`. At confidence
1.0 that survives comfortably. Under noisy-OR at the collapsed confidence 0.07 it scores 0.0347 and
is **still dropped** — so the content-match cap is *not* the cause of the observed miss. It is a
standing ~1.67x handicap on every semantic-only match, turning near-misses into misses.

**And the naive fix is not free.** COG-26's noise floor `b=0.520` was derived *against* the 0.6
coefficient — the survival bar is `0.520 + (0.1/0.6)*0.480 ~= 0.600`. At the measured noise ceiling
(cosine 0.596), the current linear form scores 0.0950 and correctly abstains, while both noisy-OR
and max score 0.1583 and clear the 0.1 gate. **Switching the combiner buys recall and pays for it
in abstention**, which is precisely the trade the evaluation says must not be made blind: a system
must not win NDCG on answerable queries by returning confident nonsense on unanswerable ones.

The combiner and the abstention floor are one calibration, not two knobs. Changing either requires
re-deriving the other and measuring BOTH metrics together — which is what the harness at
`internal/engine/engine_scoring_weights_measure_test.go` exists to do (4 combiners x confidence
live/ablated x full-signal/paraphrase, with NDCG@5 on answerable queries and false-positive rate on
committed nonsense probes).

### 44 tools

Unanimously "too many" / "hostile". Six different tools can change a memory (`evolve`, `forget`,
`forget+not_true_since`, `state`, `consolidate`, `trust`) with no map of which to use. Evaluators
guessed wrong about `link` directionality, about `decide` producing a decision, and about
`contradictions` being synchronous.

Panel consensus: **8–12 always-visible tools**, the rest behind progressive disclosure or toolset
flags — and *small named tools* rather than one mega-tool with a verb enum, because models handle a
30-way enum no better than 30 tools.

---

## 4. The write-time reconcile question

All four evaluators independently proposed the same fix: **on write, when a new memory conflicts
with an active peer, force a choice rather than silently accepting a second live truth.** A
separate scoping analysis reached the same conclusion from pure code reading. Four methods, one
answer.

The design panel split on the mechanism, and the dissent is the valuable part:

- **Refuse-pending-acknowledgement** (Grok, GPT) buys an *invariant* a notice cannot: default recall
  never contains two live truths for one claim. It is also auditable — you can count
  `conflict_pending → resolved`; you cannot count "the agent read the notice and thought about it".
- **Accept-and-quarantine** (Fable) gets the same invariant without the failure mode that should
  worry us most: *under refusal, the information dropped is usually the newer, correct fact.* The
  stale memory stays live and comfortable while the correction stands outside asking permission. If
  the agent abandons the round trip — under exactly the time pressure the evaluation documented —
  refusal silently preserves the wrong answer. That is the contradiction data-loss bug wearing a
  different hat.
- **Paraphrase evasion** (Fable): a refused agent mid-task rewords and resubmits, and rewording
  specifically defeats both the semantic detector and exact-dedup. "Cannot be ignored" only wins
  when the required action is cheaper than the workaround.

Unanimous on one point: **withhold the system's own guess.** Never ask "you may be superseding X —
confirm?"; every agent says yes and a heuristic has been laundered into a declaration. Fable's
mechanism: make each verdict require a *different artifact* (`supersede` needs `what_changed`;
`coexist` needs a `scope` distinguishing the two), so even a rubber-stamp forces the agent to
generate something auditable.

**Ordering warning:** reconcile *depends on* substrate. At a 4.81% declaration rate the detector has
almost nothing to key on — it will underfire where it matters and misfire on semantic coincidence,
and a good idea will die on bad data.

---

## 5. The middle gear

Independent convergence, and the most implementable finding in this document.

**Accept entities as bare strings** — `entities: ["PostgreSQL", "Alice"]` — and let the server
resolve types from its existing entity table (known name → known type; unknown → `other`). ~15
extra tokens instead of typed JSON objects. That single change buys the biggest thing the cheap
path loses: visibility to every entity tool.

Further, all non-LLM:

- **Inline markup**: `content: "Migrated [[Auth Service]] to [[PostgreSQL]] 16"`, parsed
  server-side. Four brackets per entity, ~2 tokens, no JSON structure at all — a rich write at
  ~1.1× content cost instead of 8×. Needs a tokenizer, not a model.
- **Known-entity matching**: scan incoming content against the vault's existing entity names, so
  every bare write mentioning a known entity gets linked for free with provenance `matched`.
- **Cut redundant fields**: `type` vs `type_label`, `concept` vs `summary`, per-entity confidence,
  per-relationship weight — default them all.
- Audit the *response* side too: echoing the full engram back is part of the 8×.

**The honest counter-argument**, which must be measured rather than assumed: the moment agents
learn the server extracts entities for them, the declaration rate may fall from 4.81% toward zero,
and the graph's ceiling becomes whatever shallow string-matching can see — silent systematic
mediocrity, which is harder to detect than the loud absence we have today.

---

## 5b. The counterfactual ceiling — substrate was NOT the flagship's blocker

The "fix capture and the graph features will fire" hypothesis was tested directly and **refuted**.

Two identical clones of a real 4,216-engram corpus. One left alone (CONTROL); one had types and
entities re-derived from content by a LOCAL model (TREATMENT) — offline analysis on a throwaway
clone, no LLM in the recall or write path. Co-activation edges were byte-identical across arms, so
substrate was the only variable.

**Substrate moved a lot:**

| | control | treatment | lift |
|---|---|---|---|
| JOINT (graph-typed AND >=1 entity) | 2.49% | **12.29%** | 4.93x |
| entity coverage | 29.9% | **98.6%** | 3.30x |
| graph-typed | 4.2% | 12.5% | 2.95x |
| co-activation edges | 135,254 | 135,254 | 1.00x |

**And associative surprise fired exactly zero times in BOTH arms**, on the same 27,255 real
candidate edges.

The gate-rejection histogram explains why: rejections *redistributed* rather than cleared.
`type` went 59.0% -> **72.4%**; `not-focal` fell 38.6% -> 12.2%. Entity enrichment pushed many more
candidates past focality and straight into the type gate, which absorbed all of them.

**The binding constraint was never sparsity. It is that five independent gates in series
(non-obvious AND valid AND focal AND type AND idf-armed AND null) leave no survivors on real data.**
Associative surprise is over-gated, not substrate-starved. The next increment for that feature is
re-deriving or removing gates — not feeding it more substrate.

Currency (2.73x) and the Push (1.56x) moved only on *preconditions* no user ever sees; the Push
still fires 0 in both arms because nothing is armed. Contradiction was structurally untouched (it
reads association RelTypes, which this intervention does not change).

**Undercut the lifts, not the zero.** Hand-adjudication of 80 distinct inferred entity names gave
~0.76 genuine-entity precision and ~0.66 genuine-and-correctly-typed — below the 0.8 bar set before
the run. The type half is no cleaner: the model labelled **56.9% of the corpus `task`**. The
control's 82.8% `fact` was a bug sink; the treatment's 56.9% `task` is a model-bias sink. Neither
is a credible distribution of declared intent, and a synthetic-fixture accuracy of 0.88 did not
transfer to the real corpus. So the currency and push lifts rest on a ~24%-junk substrate and are
soft; adding that substrate to a live vault would be the #713 pollution trade, not a fix.

**The zero is the robust result** — it is a negative, and noise could only have helped it fire.

Consequence for the roadmap: fixing capture is **necessary but nowhere near sufficient**. The type
and entity capture bugs were real and are fixed on their own merits, but they do not unlock the
graph features, and any plan that assumed they would needs rewriting. This also means the earlier
conclusion "the moat is getting memories typed and entity-tagged" was too strong: it is a
precondition, not the lever.

## 6. What this implies

Ranked by leverage, with the honest caveat on each:

1. **Fix the confirmed defect list.** All eight are defects with known causes, not architecture
   problems. The product feels bad because it is broken, not because it is wrong.
2. **Ship the middle gear.** Highest measured leverage, non-LLM, and a prerequisite for reconcile.
   Measure the declaration rate against the 4.81% baseline; if it *falls*, the Goodhart critique
   was right.
3. **Cut the tool surface to a core.** Cheap, and it removes the single most consistent complaint.
4. **Measure the cognition layer against plain cosine** before defending it. If ACT-R fusion cannot
   beat raw similarity on a real corpus, put it behind a default-off flag and say so.
5. **Then** decide reconcile vs quarantine, with a decoy arm to detect rubber-stamping.

The verdict the evaluators converged on: *the `evolve` path works, the default path does not, and
agents live on the default path.* Everything above is in service of making the default path honest.
