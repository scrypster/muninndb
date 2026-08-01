# Version-head resolution: declared-chain substitution before thresholding

**Issue:** #763 — "Version-cluster queries must resolve to the current head"
**Baseline read:** `/Users/mjbonanno/github.com/scrypster/.worktrees/prefix-weight-repair` @ `33f1230` (= `origin/develop`)
**Status:** design, hand-off ready. No code written.
**Proposed invariant id:** COG-27 (plus an amendment to COG-22)

---

## 0. The one-line statement

> If a query's evidence reaches ANY member of a **declared** version chain with
> admission-worthy strength, recall resolves that evidence to the chain's
> authoritative current head **before** thresholding and ranking, and says so.

"Declared" means an explicit `storage.RelSupersedes` edge written by `Evolve`/`EvolveAt`
or by `Link(relation=supersedes)`. Nothing else. This design does **not** consult, extend,
or promote the heuristic currency layer.

---

## 1. Boundary statement (read this before anything else)

There are two version-awareness layers in this codebase and they must not merge:

| | **Declared** (this design) | **Heuristic currency** (#712/#758, COG-25) |
|---|---|---|
| Signal | `RelSupersedes` edge, author-asserted | pairwise similarity + anchors + version vocabulary |
| Authority | ground truth | **advisory only** |
| Effect on results | may inject a row, may set its rank | annotates only; never injects, never rescores (one tie-order permutation excepted) |
| Fields | `superseded_by`, `current_version`, **new:** `substituted_for` | `possibly_superseded_by`, `version_cluster`, `newest_of_cluster`, `cluster_size` |
| Code | `engine_supersession.go`, `visibility_gate.go` | `engine_currency.go` |

**COG-25 stays exactly as written.** The heuristic layer remains read-path advisory and is
already suppressed on both sides of any declared chain (`currencyInDeclaredChain`, R4). This
increment adds no similarity-inferred supersession of any kind. If a build agent finds
itself reaching for cosine to decide "is B the successor of A", the design has been
misread — that is the five-analysis impossibility result COG-25 records, and it is settled.

---

## 2. Verified mechanism of the reproduced failure

Confirmed by reading the pipeline at `33f1230`; each claim carries its file:line.

1. `EvolveAt` (`internal/engine/engine.go:3521`) commits one atomic batch:
   successor engram + `RelSupersedes` association (new → old) + `SupersedeEngram(old)` =
   soft-delete **and** a closed `ValidUntil` stamp (engine.go:3654–3663).
2. Nothing removes the predecessor from HNSW ("HNSW has no delete method",
   `activation/engine.go:1685`) or from the FTS index (only hard delete calls
   `fts.DeleteEngram`, engine.go:3179). **So the predecessor is still fully retrievable as a
   candidate** — its vector and its postings are exactly the ones the user's old wording
   matches.
3. Phase 6's lifecycle cut (`activation/engine.go:1696–1702`, `PassesLifecycle` in
   `activation/history_gate.go`) drops it: soft-deleted engrams are admitted only under
   `as_of != nil || include_invalid`. Under default recall it is **discarded before it is
   ever scored** — its evidence never reaches `actrCands`, never reaches `maxRaw`, never
   reaches the abstention gate, never reaches `ScoredEngram`.
4. `applySupersession` (`engine_supersession.go:80`) — which already does exactly the right
   thing for the *visible* stale case, including injecting an absent head — states the gap in
   its own doc comment: *"evolve() soft-deletes its predecessor, so those never reach here."*
   It operates on `result.Activations`. The predecessor is not in `result.Activations`. The
   phase is structurally unreachable for the evolve case.
5. The successor's only route into the result set is its **own** wording. If the rewrite
   changed the vocabulary ("last-writer-wins" → "field-level merging"), a query phrased
   against the old vocabulary reaches only the predecessor, and the predecessor's earned
   relevance is thrown away rather than redirected.

That is call 18. It is not a scoring bug, not a threshold bug, and not an embedding bug —
it is an **ordering** bug: the visibility cut runs before the substitution phase, and the
substitution phase can only see what survived the cut.

### 2b. The embed-lag half, verified

Two distinct findings, one of them a live defect:

- **Live defect: `EvolveAt` never wakes the embed processor.** `Write` calls the
  `onWrite` hook after commit (`engine.go:1577–1580`), which is how the retroactive embed
  processor is notified (`plugin/retroactive.go` `Notify()`). `EvolveAt` does **not** —
  grep for `e.onWrite` returns only engine.go:1578 and engine.go:2101 (the batch-write
  path). So after an MCP `muninn_evolve` with no caller-supplied embedding, the successor
  waits for the processor's ticker, which backs off geometrically to `maxIdleInterval =
  3 * time.Minute` on an idle vault (`retroactive.go:36–40`, 149–197). **On a quiet vault
  the successor can be semantically unindexed for up to three minutes after evolve.**
  This is the single largest contributor to the "fresh-evolve retrieval window" carried
  since round 4, and it is a one-line fix.
- **The window itself.** Between the evolve commit and the successor's embedding landing:
  the successor is in FTS (submitted synchronously to `ftsWorker` at engine.go:3805) but
  **not** in HNSW; the predecessor is in both but excluded by the lifecycle cut. So for a
  semantically-phrased query there is a real interval in which *neither* version is
  retrievable. Substitution closes it for free (§5.6): the predecessor's vector is still in
  HNSW, so the evidence survives the window even though the successor's own vector does not
  exist yet.

---

## 3. Where the mechanism goes, and why

The issue's sketch says "substitute the head into the candidate pool". Taken literally
(phase 2, candidate assembly) that is wrong: at phase 2 there is no evidence yet, no score,
no visibility resolution, and no way to know the substitution is even warranted — we would
inject a head for every superseded engram any index happened to return.

Post-fusion-only is also wrong: by the time `applySupersession` runs, the predecessor's
components have been destroyed.

**The correct seam is a split, and it follows the existing division of labour exactly:**

- **Phase 6 (`internal/engine/activation/`) keeps the evidence.** It is the only place with
  the query embedding, the corpus IDF statistics, `computeACTR`/`computeComponents`, and the
  abstention formula. It scores the refused predecessor with the *same* functions and the
  *same* threshold, and — instead of appending it to `scored` — emits it on a new
  `ActivateResult.ShadowMatches` channel. **`Activations` is byte-identical to today.**
- **The engine layer (`internal/engine/`) resolves and injects.** It is the only place with
  reverse-association reads, the `visibilityGate`, and `resolveSupersessionHead` — which
  already implements the full chain-walk semantics (view-scoped head resolution, hidden-node
  traversal, fork refusal, cycle refusal, retracted-successor voiding, depth cap).

This is principle #7 (extend the proven in-tree mechanism) applied twice: the *evidence*
path reuses the abstention gate verbatim; the *resolution* path reuses the supersession
walker and the COG-22 gate verbatim. No new scoring math is invented anywhere in this
design.

---

## 4. Design — part A: shadow-match capture (phase 6)

### 4.1 What qualifies as a shadow candidate

A candidate is a **shadow** iff all of the following hold. This is deliberately narrow.

1. It reached phase 6 as a genuine retrieval candidate (vector, FTS, tag, or traversal
   pool) — we never fabricate candidates.
2. `req.AsOf == nil && !req.IncludeInvalid`. **Historical queries never produce shadows**
   (see §6.4 — under `as_of` the predecessor *is* the right answer and is already admitted
   normally; under `as_of` the successor is legitimately hidden and must stay hidden).
3. It was refused by exactly one of the two currency predicates, and it carries the
   **supersession signature**:
   - refused by `PassesLifecycle` with `State == StateSoftDeleted && !ValidUntil.IsZero()`
     (the evolve signature — soft-delete *plus* a closed stamp), **or**
   - refused by `PassesValidity` while `State == StateActive` with a closed `ValidUntil`
     (the `Link(supersedes)` / `forget(not_true_since)` signature).
   `StateArchived` is never a shadow (archival is a storage-tier fact, payload not
   guaranteed resident — same reasoning `PassesLifecycle` already gives). A plain
   soft-delete with an open `ValidUntil` is trash, not history, and is never a shadow.
4. It passes every **other** phase-6 admission predicate unchanged: `PassesMetaFilter`,
   `ExcludeUntrusted`, the standing exclude-tags set, and the lease check. A candidate the
   caller may not see does not get to speak through a proxy.

Rationale for (3): the signature is exactly the pair of states a declared supersession
produces. It is a cheap pre-filter that costs no store read; the *authoritative* test is
still the `RelSupersedes` edge walked in part B. A shadow that turns out to have no
declared successor simply produces nothing.

### 4.2 Scoring shadows honestly

Shadows are scored by the **same** functions as everything else (`computeACTR` /
`computeComponents` / the CGDN pass), with three hard rules:

- **Shadows must not influence the per-query normalization.** They are excluded from
  `maxRaw` (ACT-R path, `activation/engine.go:1966`) and from `sigma`/`denom` (CGDN path,
  ~1898–1920). A hot superseded predecessor must not rescale the live result set. Their
  own `final` is then computed *using* the live scale, so the injected head lands on the
  same scale as every other row.
- **Shadows are gated on exactly the quantity that fusion mode already gates on**, at
  exactly `req.Threshold`, with no relaxation:
  - ACT-R / CGDN → `AbsoluteScore = min(Raw, ContentMatch, 1) × Confidence`
  - weighted_sum → `Final`
  - RRF → **skipped entirely** (see §4.4)
  This mirrors `query.go:481–486`'s `WouldReturn` fusion switch — same quantity, same
  places, so explain and recall cannot disagree about what "would have cleared the bar"
  means.
- **The tag-pool bypass does not apply to shadows.** `inTagPool` lets a filter-defined
  candidate skip the relevance bar because *the filter defines the set*. A shadow is not in
  the returned set; it is evidence. Letting a tag hit manufacture a substitution would admit
  a head on zero aboutness. Explicit `if c.inTagPool { still require the threshold }`.

### 4.3 The new result channel

```go
// activation.ShadowMatch — a candidate that earned admission on relevance but was
// refused by the currency predicates while carrying the declared-supersession
// signature. NOT a result. The engine layer may resolve it to a declared chain head
// (COG-27); if it does not, nothing about the response changes.
type ShadowMatch struct {
    Engram     *storage.Engram  // loaded; the engine layer needs State/ValidUntil/ID
    Final      float64          // ranking score, on the live query's scale
    Gated      float64          // the quantity compared against req.Threshold
    Components ScoreComponents  // the predecessor's measured evidence, verbatim
}
// on ActivateResult:
ShadowMatches []ShadowMatch // score-desc, capped at shadowMatchCap
```

`shadowMatchCap = 16`. Each shadow costs one reverse-association iterator in part B; the
cap bounds hot-path I/O the way `supersessionMargin` already bounds the existing walk.
Sorted descending by `Final`, tie-broken by ULID, so the cap is deterministic.

**Zero-cost when empty:** the shadow map is `nil` unless a lifecycle/validity refusal with
the signature actually occurs, so the default path allocates nothing and the second scoring
pass never runs.

Implementation note: the post-load cosine backfill (`activation/engine.go:1727–1790`) keys
off `engramByID`, which contains only admitted engrams. Shadows must be reachable from that
lookup (add a small `lookupEngram(id)` closure consulting both maps) or an FTS-only shadow
keeps `vectorScore == 0` and under-scores. Vector-pool shadows already carry their HNSW
cosine and are unaffected.

### 4.4 RRF is skipped, deliberately

RRF finals are rank-based and its coerced default threshold is ~0.001 (COG-6), so
"cleared the bar" carries almost no information — nearly every candidate clears it.
Substitution under RRF would fire on essentially any superseded engram that any index
returned. That is precisely the promiscuity this design must not have. Skipped with a
one-line guard and a comment, mirroring COG-18's R1 amendment (entity boost is skipped
under RRF for a structurally identical calibration reason). Named as a deferral in §9.

RRF-mode abstention being structurally impossible is already a recorded COG-24/COG-26
deferral; this inherits it rather than papering over it.

---

## 5. Design — part B: head resolution and injection (engine layer)

New file `internal/engine/engine_version_head.go`, one exported-to-package entry point:

```go
func (e *Engine) applyVersionHeadSubstitution(
    ctx context.Context, ws [8]byte,
    results []activation.ScoredEngram,
    shadows []activation.ShadowMatch,
    req *activation.ActivateRequest, now time.Time,
) (out []activation.ScoredEngram, injected int, blocked []substitutionBlock)
```

Call site: `activateCore`, **immediately before** `applySupersession`
(`engine.go:~2527`), sharing the same `injectorNow` clock. Ordering matters: heads injected
here are ordinary result rows by the time `applySupersession` runs, so a head that is itself
superseded by a *further* declared successor already in the pool gets the existing
promote/demote treatment for free, with no interaction to reason about.

### 5.1 The walk

For each shadow, in score-descending order:

```
head, immediate, ok := e.resolveSupersessionHead(ctx, ws, shadow.Engram.ID, gate, nameable)
```

**Reused verbatim.** This function already delivers every chain-walk property the issue
asks for, and its semantics are already pinned by `engine_supersession_test.go`:

| Case | Existing behaviour | Correct for substitution? |
|---|---|---|
| A→B→C multi-hop | returns deepest view-valid node (C) | ✅ yes, that is the invariant |
| Fork (A→B, A→C) | WARN, returns `ok=false` | ✅ **preserve the refusal** — never guess |
| Cycle | WARN, returns `ok=false` | ✅ reject |
| Head soft-deleted/archived (successor retracted) | walk stops at that hop; head = deepest valid node *below* it, else `ok=false` | ✅ retraction is a lifecycle fact for every caller; do not resurrect the predecessor |
| Head hidden from this caller (lease/trust/filter) | traversable but unnameable; deepest *admitted* node wins; none → abstain whole | ✅ COG-22 unchanged |
| Head expired (chain head itself has closed ValidUntil, no successor) | no view-valid node → `ok=false` | ✅ see §5.5 |
| Depth > 8 | silently truncates; deepest admitted within cap | ⚠️ **must become loud** — see below |
| Dangling edge (target missing) | walk stops | ✅ |

**One behaviour change, in `resolveSupersessionHead`:** the depth-cap truncation is
currently silent, which was tolerable when the stale row was always visible and annotated
beside the head. On the substitution path the injected head is the *only* row the caller
sees, so presenting a non-terminal intermediate as "current" without saying so is
silently-wrong. Add a returned `truncated bool` (or an out-param on a small result struct);
the existing caller ignores it, the substitution caller propagates it as
`chain_truncated: true` on the annotation and logs one WARN. We still substitute — the
deepest node within the cap is strictly more current than the predecessor, and abstaining
would regress every long legacy chain, which is the reasoning already recorded in the
function's doc comment.

### 5.2 Admission of the head

The head must clear `gate.Admits` = `Nameable && ValidForView` (the full COG-22 contract:
lifecycle, meta filters, trust, structured/MQL filter, lease, valid-time). `Admits`, not
`Nameable` — a substituted head is presented as **current**, so validity is load-bearing
here in a way it is not for lineage annotations. `resolveSupersessionHead` already enforces
`ValidForView` on the head it returns; call `Admits` anyway as the explicit contract
statement at the injection site, matching how the entity-boost and supersession injectors
each state their own gate call.

**No relevance threshold is re-applied to the head.** This is not a new exemption: injected
supersession heads have never been re-thresholded (`engine_supersession.go` Phase 2a), for
exactly this reason — the score belongs to the topic, and the head is the topic's answer.
COG-18's "injections must clear the caller's threshold" is the *entity-boost* rule, where
the injection's evidence is the boost itself; here the evidence is the predecessor's
already-gated absolute score.

### 5.3 Naming the predecessor — a COG-22 amendment

The annotation `substituted_for: <predecessor id>` names a **soft-deleted** engram, which
today fails `gate.Nameable` (lifecycle is part of `Nameable`; validity deliberately is not).
Three facts make naming it correct, and they must be recorded rather than assumed:

1. The predecessor's ID is **already publicly derivable from the head**: `EvolveAt` writes
   provenance `Details.PredecessorID` on the successor (engine.go:3644), and `muninn_read`
   surfaces it. Naming it in the recall annotation leaks nothing that a follow-up `read` of
   the returned head does not already give.
2. Refusing to name it makes the row *silently* substituted — the exact failure class
   principle #2 forbids, and the whole point of the loud annotation.
3. The other five `Nameable` predicates are **not** relaxed. A predecessor under a live
   foreign lease, untrusted under `ExcludeUntrusted`, or excluded by a meta/structured
   filter is neither a shadow (§4.1 rule 4) nor nameable.

**Amendment:** add `visibilityGate.NameableAsLineage(ctx, store, ws, eng)` — identical to
`Nameable` except the lifecycle predicate is evaluated as
`PassesLifecycle(eng, nil, /*includeInvalid=*/true)`, i.e. it admits a soft-deleted record
**only when it carries a closed `ValidUntil`** (still refusing `StateArchived` and open-stamp
trash). Every annotation that names a predecessor goes through it. COG-22's text gains a
clause: *"a declared-supersession predecessor may be NAMED as substitution provenance under
`NameableAsLineage`, on the same reasoning that admits expired chain intermediates as
lineage; every other existence predicate is unchanged."*

Do **not** implement this by loosening `Nameable` itself. The bad state must stay
unrepresentable for every other caller (principle #3).

### 5.4 Scoring the substituted head — the crux, decided

The head did not match. What is honest?

**Decision:**

- **Ranking score** `ScoredEngram.Score = shadow.Final` (and, when the head is *also*
  already in the result set on its own merit, `max(own, shadow.Final)` — the identical MAX
  rule `applySupersession` already applies). Justification is the doctrine already written
  into `engine_supersession.go`: *"the score belongs to the TOPIC, and the head is the
  correct answer for it."* This design does not introduce that idea; it extends it to the
  case where the stale member happens to be invisible.
- **`ScoreComponents` = the predecessor's measured components, verbatim.** They are real
  measurements of a real engram against this query — nothing is fabricated, nothing is
  copied from a different query, and no number is invented. They are also *the* evidence
  that admitted this row, which is what a score card is for.
- **They are never presented as the head's own aboutness.** The annotation block carries
  the attribution, always, and it is not optional:
  ```
  substituted_for:      <predecessor ULID>
  substitution_basis:   { absolute_score: <gated>, content_match: <cm>, semantic_similarity: <sc>, full_text_relevance: <fts> }
  chain_truncated:      <bool, omitted when false>
  head_not_indexed_yet: <bool, omitted when false>   // §5.6
  ```
  and, when `include_why` is set, `Why` gains a leading clause in plain language:
  *"returned because your query matched the earlier version `<id>`, which this replaces —
  this memory's own wording did not match."*

**Two alternatives, both rejected, with reasons:**

- *Re-score the head against the query and gate it on its own absolute.* This is the
  "re-killed for wording it never had" failure. The successor's whole purpose is that its
  wording changed; gating it on its own absolute reproduces call 18 exactly, one layer
  deeper. Rejected.
- *Inject at `shadow.Final − ε`, or cap it below rank 1.* Superficially conservative,
  actually incoherent: there is no visible stale row for it to sit below (the ε in
  `supersessionEpsilon` exists to order a head *against its own stale twin*, which here is
  not in the set). A ranking penalty applied to a fact the author declared current is a
  silent statement that we trust the declaration less than we say we do. If the declaration
  is untrustworthy the substitution should not happen at all, not happen at a discount.
  Rejected.

**Why this cannot dishonestly bypass abstention:** the *only* admission path is a
predecessor that cleared `req.Threshold` on the unchanged formula against the unchanged
quantity, with the tag bypass explicitly disabled. There is no new way for a query with no
evidence to produce a row. The substitution is a **redirection** of admission-worthy
evidence, never a **creation** of it. This is the property the FPR corpora in §8 measure
rather than assert.

### 5.5 When the chain resolves to nothing

`ok == false` (fork, cycle, retracted successor, no admitted/valid node) → **no injection,
no annotation, no demotion.** The existing atomic-abstention rule, unchanged.

But silence is what produced this issue, so record the block:

```go
type substitutionBlock struct { PredecessorID storage.ULID; Reason string }
// reasons: "ambiguous" (fork) | "cycle" | "retracted" | "no_current_version" | "hidden"
```

If, after every phase, `Activations` is **empty** and at least one block exists, set a new
abstention reason instead of the generic one:

```go
AbstainSupersededOnly    = "superseded_only"      // matched only stale members; no current version is reachable
AbstainAmbiguousVersion  = "ambiguous_version"    // matched a stale member whose chain forks — refusing to choose
```

`"ambiguous_version"` wins if any block is a fork. This turns the honest-but-mute empty
response into a sentence an agent can act on ("there is a version chain here and I will not
guess which branch is current — read `<id>`"), which is the difference COG-24/the existing
abstention reasons were introduced to make. When `Activations` is non-empty, blocks are
DEBUG-logged only — do not manufacture a row and do not append a warning to an otherwise
good answer.

### 5.6 Embed lag

Three changes, in order of value:

1. **Wake the embed processor on evolve.** Add the `onWrite` callback invocation at the end
   of `EvolveAt`, guarded exactly as `Write` guards it, and only when the successor was not
   already inserted into HNSW inline (the `len(embedding) > 0` success branch,
   engine.go:3789–3801). This alone collapses a worst case of ~3 minutes to ~one poll.
   RED-checkable with a counting fake `onWrite`.
2. **Substitution *is* the embed-lag fix, and it needs no extra machinery.** During the
   window the successor has no vector, but the predecessor's vector is still in HNSW and
   still matches the old wording — which is the wording a user asks in the seconds after an
   evolve. The head is injected on the predecessor's evidence and returned, with no
   embedding of its own. Test 11 in §8 pins exactly this.
3. **Loud "not indexed yet".** When an injected head has no embedding (`DigestEmbed`
   unset / `GetEmbedding` empty — one point read, only for injected heads, only when a
   substitution actually happened), set `head_not_indexed_yet: true`. "Not indexed yet"
   and "not relevant" become distinguishable at the surface, which is the loud-degradation
   doctrine applied to the exact case #763 names.

**Predecessor-embedding inheritance is REFUTED, not deferred.** Copying the predecessor's
vector to the successor's ID would: (a) make the successor semantically indistinguishable
from the fact it replaces, so it matches the *old* wording forever and silently — the
inverse of the bug, and unannounceable because the vector carries no provenance;
(b) produce a vector swap mid-life when the real embedding lands, so identical queries
return different results before and after, with nothing in the response explaining it;
(c) poison every downstream consumer of the vector — dedup, consolidation similarity,
`similar_entities`, Hebbian neighbour selection — with a value that is not a measurement of
that engram's text. It buys a few seconds of coverage that substitution already provides
correctly. Do not build it.

---

## 6. Correctness cases, enumerated

**6.1 Multi-hop A→B→C.** Query matches A's wording. Shadow=A → walk → C (deepest view-valid).
Inject C at A's score, `substituted_for: A`. B is neither injected nor named (it is not
what replaced A from the caller's point of view — `immediate` is B, but the substitution
annotation names the *evidence source*, not the intermediate; `superseded_by` on a visible
stale row keeps its existing meaning). If both A and B are shadows (both matched), they
resolve to the same head C: dedupe by head ID and take `max(Final)` across contributing
shadows, annotating `substituted_for` with the **highest-scoring** contributor. One row, one
attribution, deterministic.

**6.2 Fork.** Preserved refusal, per §5.5. This is a deliberate, documented hole in the
"any member reaches the head" invariant, and the invariant text must say so: *"…except
where the declared chain forks or cycles, in which case recall refuses to choose and says
which query it refused."* An invariant that overstates itself is worse than one with a named
exception.

**6.3 Cycle.** Rejected, WARN (existing), block reason `"cycle"`.

**6.4 `as_of` / `include_invalid`.** **No shadows are collected at all** (§4.1 rule 2), so
no substitution can occur. Under `as_of=T` the predecessor is admitted normally by
`PassesLifecycle`'s historical branch and *is* the right answer; substituting the successor
would answer a different question than the one asked, and under `as_of` the successor may
not even exist yet in the view (`ViewFuture`). One branch, tested both directions (test 2).

**6.5 Head itself expired or forgotten.** `resolveSupersessionHead` already handles both:
a soft-deleted/archived successor voids the supersession at that hop for every caller; an
expired head with no valid successor yields `ok=false`. Result: no substitution, block
reason `"retracted"` / `"no_current_version"`, and — if that leaves an empty response —
`abstained_reason: "superseded_only"`. We never resurrect the predecessor. The chain having
been retracted is a real fact about the world and the honest answer is "there is no current
version of this".

**6.6 Head already in the results.** No injection. Raise its `Score` to
`max(own, shadow.Final)` (existing MAX doctrine), and do **not** set `substituted_for` —
nothing was substituted; the row earned its place. `injected` is not incremented, so
`TotalFound` stays honest.

**6.7 Observe / read_only.** The whole path is read-only: reverse-association reads, engram
reads, lease reads, one optional embedding read. No writes, no reinforcement, no
co-activation. Shadow predecessors never enter `scored`, so they never enter the activation
log, never contribute a Hebbian edge, and never have their access counts touched — a
superseded fact must not be *strengthened* by having been matched. Substituted heads are
reinforced exactly as supersession-injected heads are today; no new rule.

**6.8 `Threshold < 0` (Explain's diagnostic bypass, `query.go:451`).** Substitution is
**skipped**. At threshold −1 every superseded predecessor in the pool "clears the bar", so
substitution would fire for every chain and Explain would report a `would_return` that
recall would never produce. Explicit guard, documented at both sites.

---

## 7. Surfaces and drift obligations

Walked against `docs/internals/drift-and-obligations.md`:

- **Obligation 1 (MCP tool handler → registry smoke test):** not triggered. No new tool; a
  field is added to an existing response.
- **Obligation 2 (REST route/handler → `openapi.yaml`):** no new route, but the
  `ActivationItem` schema gains fields. Update `openapi.yaml` and run
  `npx @redocly/cli lint`.
- **Obligation 3 (REST types → SDKs):** follow the recorded supersession/currency
  precedent **exactly**: new fields go on `mbp.ActivationItem` and `mcp.MemoryAnnotations`
  only; REST inherits via the `rest.ActivateResponse = mbp.ActivateResponse` alias;
  **proto/gRPC and the non-Go SDKs get nothing**, because their `ActivationItem` carries no
  supersession annotations at all and adding a partial block is the silently-wrong class.
  Update the obligation-3 paragraph in `drift-and-obligations.md` to list
  `substituted_for` / `substitution_basis` alongside the existing fields.
- **Obligation 4 (presets):** not triggered — no plasticity change. (See §9 for why no
  kill-switch preset knob is proposed.)
- **Obligation 7 (proto):** not triggered, per obligation 3's scope decision.
- **Obligation 8 (Pebble prefix):** not triggered — no new prefix, no new key. Purely
  read-path.
- **Obligation 10 (doc promises):** `muninn_guide` must state the behaviour ("a query
  phrased against an older version returns the current version, annotated
  `substituted_for`"), because agents read the guide and this changes what recall promises.
- **Obligation 11 (replication):** not triggered — no write path.
- **`docs/internals/invariants.md`:** add **COG-27** (the substitution invariant, its
  boundary against COG-25, the fork/cycle exception, the `as_of` non-substitution rule, the
  RRF skip, the honesty rules, and the pinning tests) and amend **COG-22** with the
  `NameableAsLineage` clause.
- **`docs/internals/decision-record.md`:** entry for #763 — the ordering diagnosis, the
  rejected alternatives (embedding inheritance, re-scoring the head, ε-discount), and the
  named exception.

### Field additions, concretely

```go
// activation.ScoredEngram
SubstitutedFor    storage.ULID     // zero unless this row was injected by COG-27
SubstitutionBasis *ScoreComponents // the predecessor's measured evidence; nil otherwise
ChainTruncated    bool
HeadNotIndexedYet bool

// mbp.ActivationItem
SubstitutedFor    string            `json:"substituted_for,omitempty"`
SubstitutionBasis *ScoreComponents  `json:"substitution_basis,omitempty"`
ChainTruncated    bool              `json:"chain_truncated,omitempty"`
HeadNotIndexedYet bool              `json:"head_not_indexed_yet,omitempty"`

// mcp.MemoryAnnotations — same four, same json tags, doc comment stating that these are
// ASSERTED (declared chain), sibling to superseded_by/current_version and explicitly NOT
// part of the advisory possibly_superseded_by block.
```

`TotalFound += injected`, same accounting and same known double-count deviation as the two
existing injectors (record it in COG-27 rather than pretending it is new).

---

## 8. Acceptance gate — measurable, RED-first

Everything below is `-tags localassets` with the real bundled `bge-small-en-v1.5`, because
the failure is a wording/embedding failure and a fake embedder cannot reproduce it.

**Gate 1 — the eval-shaped RED test (the issue's own reproduction).**
`internal/engine/engine_version_head_test.go`, `TestVersionHead_PredecessorWordingReturnsHead`:
1. `remember` A: *"Simultaneous edits are resolved last-writer-wins."*
2. `evolve` A→B: *"Simultaneous edits are resolved by field-level merging."*
3. `recall("What is the current rule for merging simultaneous edits?")` at the vault's real
   default threshold, default mode, no rephrase, no deep mode.
4. **Assert:** B is present; A is absent; B's `substituted_for == A`; B's
   `substitution_basis.absolute_score >= threshold`.
**RED requirement:** must fail at `33f1230` and must fail with the mechanism disabled
(assert it fails with the shadow-collection guard forced off). A test that passes both ways
proves nothing (CLAUDE.md §3.3).

**Gate 2 — historical queries do not substitute.** Same corpus:
`recall(..., as_of=<before evolve>)` returns A and **no** B, `substituted_for` empty;
`recall(..., include_invalid=true)` returns A annotated expired and B, with **no**
`substituted_for` on B. RED-checkable by removing the `AsOf` guard.

**Gate 3 — chain shapes.** Table-driven, unit-cheap:
A→B→C returns C (`substituted_for: A`); fork A→{B,C} returns nothing substituted and
`abstained_reason == "ambiguous_version"` when the set is otherwise empty; cycle rejected;
`evolve` then `forget(B)` returns neither A nor B and abstains `"superseded_only"`;
9-hop chain substitutes with `chain_truncated: true`.

**Gate 4 — visibility (COG-22 parity).** Head under a live foreign lease → no injection and
no annotation. Head failing a structured (MQL) filter → no injection. Predecessor under a
foreign lease → not even a shadow. Each guard RED-checked against its bypassed check, the
way `engine_supersession_test.go` already does it.

**Gate 5 — no ranking regression on the labeled query set.**
`TestMeasureRecallQuerySet` / `TestRecallQuerySet_AcceptanceRule` /
`TestRecallQuerySet_ReconstructionFidelity`
(`internal/engine/activation/recall_queryset_measure_test.go`): NDCG@5 and MRR must be
**≥ the recorded pre-change baseline**, with both numbers quoted in the PR body. The corpus
contains no declared chains, so the expected delta is exactly zero — any movement means
shadows leaked into normalization (§4.2) and is a bug, not a tuning result.

**Gate 6 — abstention FPR does not move.** `TestMeasureAbstentionGate`
(`internal/engine/activation/abstention_gate_measure_test.go`): at threshold 0.10, FPR must
stay **≤ 6.2%** (the recorded post-fix value) and NDCG@5 ≥ 0.6410, run on the existing
18-engram corpus **plus a variant with a two-member declared chain grafted in**. The 16
nonsense probes must produce **zero** substitutions — that is the direct measurement of "the
substitution only fires on genuine predecessor matches".

**Gate 7 — topically-adjacent set produces no substitutions.** Reuse the corpus behind
`TestCurrencyPrecision_AdjacentTopics_NoBroadHints` (`engine_currency_precision_test.go`):
evolve one member, then query with wording adjacent-but-not-matching. Assert zero
substitutions. This is the false-positive case the design is most exposed to (§10 risk 1)
and it must be measured, not argued.

**Gate 8 — embed lag.** `TestEvolve_NotifiesEmbedProcessor`: counting fake `onWrite`,
RED without the added call. Plus `TestVersionHead_SubstitutesWithUnindexedHead`: evolve
with no client embedding, HNSW deliberately not fed for the successor, predecessor-wording
query still returns the head with `head_not_indexed_yet: true`.

**Gate 9 — race.** `go test -race` over `internal/engine/...` for the new phase: it mutates
the results slice, and `applySupersession` documents a live hazard there (the async
log-drain goroutine reads the backing array Run() returned). **Copy before mutating**, same
as `engine_supersession.go:187`. This is a known-live race, not a hypothetical.

**Gate 10 — CI budget.** All of the above are unit/measure tests in existing packages; the
only asset-gated additions are the two `localassets` measure runs, which already exist and
gain corpus rows rather than new jobs. No new integration or Playwright job. Target: no
measurable movement in the ~6–7 minute baseline.

---

## 9. Minimal first increment, and explicit deferrals

**In scope (increment 1):**
1. `activation.ShadowMatch` + shadow capture in phase 6 (ACT-R, CGDN, weighted_sum), with
   normalization exclusion and the tag-bypass exclusion.
2. `engine_version_head.go` — `applyVersionHeadSubstitution`, wired before
   `applySupersession` on the shared clock.
3. `resolveSupersessionHead` returns `truncated`.
4. `visibilityGate.NameableAsLineage`.
5. Annotation fields on `ScoredEngram` / `mbp.ActivationItem` / `mcp.MemoryAnnotations`,
   plus the `Why` clause under `include_why`.
6. Two new abstention reasons.
7. `EvolveAt` → `onWrite` notification.
8. `head_not_indexed_yet`.
9. Docs: COG-27, COG-22 amendment, decision record, obligation-3 paragraph, `openapi.yaml`,
   `muninn_guide`.
10. Gates 1–10.

**Deferred, named:**
- **RRF-mode substitution** (§4.4) — needs a calibrated notion of "admission-worthy" that
  rank-based fusion does not currently have. Inherits COG-24 deferral (4).
- **Explain integration.** Increment 1 only *disables* substitution under the `-1` bypass.
  A follow-up should give `muninn_explain` a `would_return_via_substitution` +
  `substituted_for` on the head, and, when asked about a superseded predecessor, the
  sentence "recall does not return this; it substitutes `<head>`". Explain exists for
  exactly the "why didn't my memory come back" question this issue is about, so this is the
  first follow-up, not the last.
- **`annotate:true` raw-reverse-edge fallback (#700)** still bypasses the gate. Unchanged
  here; the new annotation must **not** be added to that path until it routes through the
  gate, or it re-leaks IDs the ranking phase refused to name.
- **Substituting into non-recall surfaces** (`entity_state`, `where_left_off`, `brief`,
  `find_by_entity`) — COG-19 already records per-surface gate adoption as an obligation;
  substitution should follow the same per-surface path, not be retrofitted blind.
- **A plasticity kill-switch.** Deliberately not proposed: a per-vault toggle for a
  correctness invariant invites "turn it off when it misfires" instead of fixing the
  misfire, and presets are a hand-duplicated drift surface (obligation 4). If gates 6–7
  cannot be met, the design is wrong and should not ship behind a flag.

---

## 10. Top risks

1. **Topic drift through loose evolve usage** (highest). `muninn_evolve` is sometimes used
   to *replace* a memory with something substantially different, not to revise it. The head
   is then substituted for a query about the old subject and returned at the old subject's
   score. Declared-only confines the blast radius to author assertions, and the annotation
   makes it inspectable, but it is real. Measured by gates 6–7; if adjacent-topic
   substitutions appear, the lever is raising the shadow's admission bar (e.g. requiring
   `absolute >= threshold` *and* a margin), **not** adding a similarity check between
   predecessor and successor — that would smuggle inference into the declared channel.
2. **Normalization leakage.** If shadows reach `maxRaw` / `sigma` / `denom`, every score in
   every query with a superseded candidate in the pool shifts. Gate 5's expected-exactly-zero
   delta is the detector; make it an assertion, not an eyeball.
3. **Hot-path I/O.** One reverse-assoc iterator per shadow (`shadowMatchCap = 16`) on top of
   the existing supersession scan. Bounded, but on a vault with many evolve chains a broad
   query pays both. Mitigation: the cap, plus reuse of the existing `nameableCache` across
   both phases (share one gate + cache instance between `applyVersionHeadSubstitution` and
   `applySupersession` rather than building two).
4. **The `NameableAsLineage` relaxation.** Getting it wrong turns a visibility invariant
   into a leak. Mitigation: separate method, five predicates untouched, per-predicate RED
   guards (gate 4) in the style already established.
5. **Slice-mutation race** (§8 gate 9). Known-live in this exact code region; copy-before-
   mutate, `-race` mandatory.
6. **Invariant overstatement.** "Any member resolves to the head" is false for forks,
   cycles, and retracted chains. Writing COG-27 without those exceptions would make the
   documentation itself silently wrong — the failure class this project treats as worst.

**No LLM anywhere in the runtime path.** Every decision above is a stored edge, a stored
state flag, a stored timestamp, or an arithmetic comparison against an existing threshold.
