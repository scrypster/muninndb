# Contradiction honoring — design increment for #764

**Date:** 2026-08-01
**Baseline:** `bb10f30` (`origin/develop` tip) — "feat(engine,activation,mbp): resolve declared version chains to their head before ranking (#763) (#767)"
**Issue:** #764 — joint #1 knockout for both round-7 evaluators (Grok and GPT, independently)
**Status:** design, ready to hand to a build agent. Every root cause below was reproduced empirically in a scratch copy of `bb10f30`, not inferred from reading.

---

## 0. TL;DR

Two defects, two different subsystems, one user-visible failure ("declaring a contradiction is theater").

| | Defect | Root cause (proven) | Fix shape |
|---|---|---|---|
| **D1** | `link(contradicts)` stays `pending_detection` forever | `internal/cognitive/worker.go` `Run()` resets the flush ticker on **every received item**, turning "flush at least every 30s" into "flush after 30s of **silence**". An active agent session starves `ContradictWorker` (and `ConfidenceWorker` — same generic) indefinitely. | 3-line change in `Worker.Run`: reset only on the transition *into* Active. Plus rename the status `pending_detection` → `declared`, because the declaration is already durable and (after D2) already honored. |
| **D2** | recall ignores an unresolved declared `contradicts` edge | Nothing in the recall pipeline ever reads contradiction state. Confirmed: both sides returned, tied, older first, zero annotation, zero response-level signal. | New post-scoring / pre-truncation phase `applyContradictionHonesty` beside `applyCurrencyAnnotation`: demote-only, score-capped, adjacent, first-class per-row + response-level `conflict` block. Option **(c)**, the hybrid — never abstain away real data. |
| **D3** *(discovered, in scope)* | **no operation in the product clears a declared contradiction.** evolve/forget(soft)/forget(not_true_since)/`muninn_decide` clear neither marker; hard-delete clears the edge and leaves a dangling `0x0A`; the REST resolve endpoint clears `0x0A` only and the worker re-flags it from the surviving edge. | Read surfaces union two durable markers and filter on neither liveness nor resolution. | One **liveness + resolution rule**, applied identically in recall and in `GetContradictionReport`. This is what makes "resolve via evolve → theater stops" actually true. |

---

## 1. D1 — the declared path: what actually happens

### 1.1 The path is wired correctly. #747 did not orphan it.

Traced end to end at `bb10f30`:

- `internal/mcp/handlers.go:676` `handleLink` → `s.engine.Link` (`internal/mcp/engine_adapter.go:61` → `internal/engine/engine.go:2970`).
- `Engine.Link` writes the forward `0x03` / reverse `0x04` / weight-index `0x14` association with `RelType = RelContradicts (0x0002)` (`engine.go:3012`).
- `engine.go:3031-3060`: on `RelContradicts` it submits a **synthetic matrix-eligible pseudo-pair** to `ContradictWorker` — assoc A = `{src → dst, RelContradicts(2)}`, assoc B = `{dst → src, RelSupports(1)}`. `ContradictionSeverity(2,1) = 1.0` (`internal/cognitive/contradict.go:29`, `contraMat` set by `setContra(1,2)`), so `processBatch` calls `FlagContradiction(dst, src)` (`contradict.go:105`).
- `#747` (`6f00ea1`) removed only the **second, unconditional `ConfidenceUpdate` submit**, not the worker submit. The comment block it left at `engine.go:3062-3080` is accurate. **The removal is not the bug.**

`FlagContradiction` (`internal/storage/association.go:961`) writes the canonical-ordered `0x0A` pair with the `#754` timestamped value, returning `newlyFlagged`. `GetContradictionReport` (`internal/engine/query.go:200`) unions `GetContradictionRecords` (`0x0A`) with `DeclaredContradictions` (a `0x03` scan) and labels the declared-but-unflagged half `pending_detection` (`query.go:151`).

Everything above is correct. Isolated, it works: **detection lands at exactly 30.006s.**

### 1.2 The real root cause: the generic worker's flush ticker is a debounce

`internal/cognitive/worker.go`, `Run()`, the item branch (currently ~`:211`):

```go
case item, ok := <-w.input:
        ...
        w.lastItem.Store(time.Now().UnixNano())
        w.state.Store(int32(WorkerStateActive))
        ticker.Reset(time.Duration(w.maxWait.Load()))   // <-- resets on EVERY item
        batch = append(batch, item)
        if len(batch) >= w.batchSize {                   // batchSize = 50
                flush(ctx)
        }
```

`ticker.Reset` on every arrival means the 30s timer never elapses while items keep arriving. The batch flushes only when the worker sees **30 consecutive seconds with no submissions at all**, or when 50 items accumulate. `Engine.Write` submits a `ContradictItem` on every remember (`engine.go:1476-1500`), so an agent doing ordinary session work every few seconds holds the flush off indefinitely — which is exactly the evaluators' shape (Grok: three polls past 35s while continuing to work; round 6: 60+ seconds across calls 28→62→77).

**Proven, RED/GREEN, in a scratch copy of `bb10f30`:**

- Idle control: link → poll. Detected at **30.0s**. Passes.
- Active session: link, then one ordinary `Write` + one poll every 5s for 60s.
  → `detected=0 pending=1` at every one of 12 polls. **RED.**
- With the candidate fix below: same test **passes at 31s**. `go test ./internal/cognitive/...` stays green.

Blast radius is wider than contradictions: `ConfidenceWorker` is the same `Worker[T]`, so **confidence updates are starved by the same mechanism**. That is why `#747`'s "the penalty now arrives up to one worker interval later — that is the correct trade" did not hold in practice: under load the penalty arrived *never*.

### 1.3 D1 fix

**F1.1 — `internal/cognitive/worker.go` `Run()`, item branch.** Reset the ticker only when actually leaving idle/dormant:

```go
w.lastItem.Store(time.Now().UnixNano())
// Reset the flush ticker ONLY when leaving idle/dormant. Resetting on every
// item turned "flush at least every maxWait" into "flush after maxWait of
// SILENCE" — a busy session starved the flush forever (#764 D1).
if WorkerState(w.state.Swap(int32(WorkerStateActive))) != WorkerStateActive {
        ticker.Reset(time.Duration(w.maxWait.Load()))
}
batch = append(batch, item)
```

`Swap` keeps the dormant→active interval change (5min poll → 30s) that the old unconditional reset provided, without the debounce. Do **not** shorten `maxWait`; the interval is not the problem.

**F1.2 — do NOT write the `0x0A` marker synchronously in `Link`.** This is the obvious-looking "make it instant" fix and it is a trap: `FlagContradiction`'s `newlyFlagged` return **is** the idempotency token for the confidence penalty (`contradict.go:110-128`, `association.go:986-998`). If `Link` writes the marker first, the worker's later `FlagContradiction` returns `newlyFlagged=false` and the penalty **never fires** — silently reverting `#746`'s idempotency into a silent no-op. Record this in the PR body so a reviewer does not ask for it.

**F1.3 — rename the status: `pending_detection` → `declared`.** `pending_detection` is a mislabel that reads as "your declaration has not taken effect yet". After F1.1 + D2, a declared contradiction is durable at `Link` return *and honored by recall on the very next query* — the `0x0A` marker's only remaining job is making the confidence penalty fire exactly once. Change `engine.ContradictionPending` (`query.go:151`) to the wire value `"declared"`, and add a sibling field rather than overloading status:

```go
Status               string // "declared" | "detected"
ConfidencePenalty    string // "pending" | "applied"   (derived: 0x0A present ⇒ applied)
```

Update the MCP note at `handlers.go:763` to say what is actually true:

> N contradiction(s) are declared by an explicit link and are already honored by recall; the asynchronous confidence penalty for them has not been applied yet (it runs on a ~30s batch interval).

This is a wire-visible string change on `muninn_contradictions`; it is additive-plus-rename on a read-only surface, and `internal/mcp/handlers_contradictions_test.go:32,56` pins the old value and must be updated in the same commit.

---

## 2. D2 — recall must honor unresolved declared contradictions

### 2.1 RED, reproduced at `bb10f30`

Two engrams ("the request timeout limit is 180ms" / "…is 320ms"), `link(b contradicts a)`, then recall "what is the request timeout limit":

```
rank 0: concept="request timeout limit"          score=0.0935 conf=1.000  superseded_by="" current_version="" substituted_for=""
rank 1: concept="request timeout limit revised"  score=0.0935 conf=1.000  superseded_by="" current_version="" substituted_for=""
```

Both sides returned, tied, **older first**, **zero** conflict signal on either row and none on the response. With a real embedder and a real vault this is the evaluators' "one side at score 1.0 with only a side-channel annotation". The `conflicts_with` annotation exists (`internal/engine/annotation.go:46`) but only on the opt-in `annotate=true` path (`internal/mcp/handlers.go:548-560`) — a side channel, exactly as reported.

### 2.2 The decision: option (c), the hybrid. Never abstain away real data.

**Against (a) top-level abstention** (GPT's literal ask):

1. `#747`/`#754` is the hard-learned lesson in this exact area: suppressing *both* sides of a declared contradiction is "doing the right thing and getting punished" — the true memory is destroyed exactly as hard as the false one. Total abstention is the same failure with a nicer name: the agent that declared the conflict loses access to both facts *and* cannot see what the conflict was.
2. It contradicts the abstention doctrine already in the codebase. `Abstained` means "the pipeline ran and every candidate fell below the bar or was filtered" and the wire contract is **"Empty iff Abstained is false"** (`mbp/types.go:286-302`, recomputed at `engine.go:2600-2629`). Here two candidates *are* admission-worthy. Setting `Abstained` on a non-empty set would break the contract; emptying the set to set it would destroy data.
3. GPT's actual complaint — "an agent commonly consumes only the first result… confidently misleading" — is about **presenting one side as the answer**, not about returning the data. The score cap plus the response-level `conflict` block plus forced adjacency removes "the answer" while keeping both facts.

**Against (b) demote-and-annotate alone:** correct in spirit but under-specified on the case that matters — when only one side is in the result set, and on what "the winner" means when neither side has been declared to supersede the other. (c) is (b) plus those rules.

**Chosen behavior — COG-29:**

> When two results in a recall response are joined by an **unresolved declared** `contradicts` edge, recall must not present either as the answer. Both are returned, adjacent, in a defensible order, with a hard score ceiling and a first-class per-row annotation, and the response carries a `conflict` block naming the pair and the resolution actions. The phase is **demote-only** (it can never lift a row above a genuinely better unrelated match), **never removes a row**, **never abstains**, and **writes nothing**.

### 2.3 Where it runs

`internal/engine/engine.go` `activateCore`, immediately **after** `applyCurrencyAnnotation` (`engine.go:~2588`) and **before** the `MaxResults` re-truncation (`engine.go:~2592`):

```
  entity boost
  applyVersionHeadSubstitution   (COG-28, #763)
  applySupersessionWithGate      (COG-22)
  COG-19 final validity gate
  applyCurrencyAnnotation        (COG-25)
→ applyContradictionHonesty      (COG-29, NEW)
  MaxResults truncation
  Abstained recompute
```

Rationale for that slot, each point matching an existing precedent:

- **After the COG-19 gate**: a side already removed for being invalid/soft-deleted must not be resurrected or referenced. This is what makes evolve/forget stop the theater (§4).
- **After supersession/substitution**: a declared `supersedes` between the pair *is* the resolution; by this point the loser has been demoted or removed and the phase can see the final shape.
- **After currency**: COG-25 may reorder exact ties. COG-29's ordering guarantee must be the last word, so it runs last.
- **Before truncation**: so the adjacency guarantee holds and a demoted partner is not silently cut.

### 2.4 Algorithm

```
func (e *Engine) applyContradictionHonesty(
        ctx, ws, results []activation.ScoredEngram,
        req *activation.ActivateRequest, gate *visibilityGate, now time.Time,
) ([]activation.ScoredEngram, []ConflictPair)
```

**Step 0 — fast-path gate (optional for correctness, required for the hot path).**
Skip the whole phase when the vault provably has no contradiction edges. Cheap test: `0x0A` prefix scan is empty **and** no `RelContradicts` edge has been written by this process for this vault (a per-vault in-process atomic set by `Engine.Link`). Documented residual: a `contradicts` edge declared by *another* node/process is honored only once its `0x0A` marker lands (≤30s after F1.1). The single-binary default — the evaluators' case — is instant.

**Step 1 — examine window.** Top `min(len, MaxResults + contradictionMargin)` rows, `contradictionMargin = supersessionMargin = 16`. Results are already score-descending. Same rationale as `engine_supersession.go:37-41`.

**Step 2 — collect declared edges.** For each examined row: one batched `GetAssociations(ctx, ws, ids, contradictionAssocScan)` for forward edges plus a per-row `GetReverseAssociations(ctx, ws, id, contradictionReverseScan=256)`. Keep only `RelType == storage.RelContradicts`. Both directions carry `CreatedAt` (`decodeAssocValue`, verified at `association.go:899`). This is the same order of I/O `applySupersession` already pays per row.

**Step 3 — the unresolved test.** An edge `(a,b)` is *unresolved* iff **all** hold:

1. **Declared.** `RelContradicts` on a `0x03`/`0x04` key in either direction. **Declared edges only** — no similarity-inferred conflicts, ever (the proven `#758`/`#763` boundary; COG-25's "an inferred signal never gets authority").
2. **Both endpoints live under the caller's view.** Both must pass `gate.Nameable` and `activation.PassesValidity` at the shared `now`. A partner that is soft-deleted, expired, or invisible is **not** a live conflict. This single clause is what makes evolve, `forget` (soft), `forget(not_true_since)`, and hard-delete all stop the theater — see §4. For a partner *not in the result set*, this costs one batched `GetMetadata` + one `GetEngram` over the distinct partner IDs (small: contradiction sets are tiny).
3. **Not resolved by a declared supersession.** No `RelSupersedes` between `a` and `b` in either direction. Reuse `e.currencyHasExplicitSupersedesEdge` (`engine_currency.go:864`) verbatim — asserted beats asserted, newest declaration wins, same doctrine as COG-25. *"I declared which one wins" is a resolution.*
4. **Existed at the caller's instant.** When `req.AsOf` is set, require `edge.CreatedAt <= req.AsOf`. A conflict declared *after* the as-of instant is not part of the truth of that time — no conflict theater on historical queries. A zero `CreatedAt` (legacy edge) is treated as "always existed" and the conflict is shown: never invent a time (CLAUDE.md §2.1), and over-warn beats under-warn on a correctness signal.

**Step 4 — cluster.** Union-find over the examined window on unresolved edges (same structure as `engine_currency.go:204-222`), so `A⊥B, B⊥C` is one conflict cluster. Cap cluster size at `contradictionMaxCluster = 8`; beyond that annotate and set `cluster_truncated`.

**Step 5 — order within the cluster.** Deterministic ladder, first rule that discriminates:

1. Newer `EffectiveValidFrom` first (the same currency signal COG-25 uses for its tie reorder).
2. Newer declaration: the **source** of the `contradicts` edge is the asserting side ("this new thing contradicts that old one") — source first.
3. ULID descending (monotonic ⇒ newer first), so the order is total and reproducible.

Then place cluster members **adjacent**, winner first, at the position of the cluster's highest post-demote score. Never above the row that previously occupied that rank if that row is unrelated — see Step 6's demote-only guarantee.

**Step 6 — demote and cap. Demote-only, by construction.**

```go
const (
    contradictionDemote       = 0.10 // relative demote applied to every cluster member
    contradictionScoreCeiling = 0.95 // no member may be presented at ~1.0
)
newScore := min(ownScore, ownScore*(1-contradictionDemote))
newScore  = min(newScore, contradictionScoreCeiling)
```

Both operations are `min()` against the row's own earned score, so a conflict can only ever push a row **down** — never above a genuinely better unrelated match. This is the `applySupersession` demote-only precedent (`engine_supersession.go:76-100`) restated. Re-sort by score after the pass; the sort is stable and demote-only, so unrelated rows keep their relative order and can only gain rank.

Three guards:

- **Never remove.** The phase has no delete path. `len(out) == len(in)`.
- **Never re-threshold.** No score filter runs after this phase in `activateCore` (verified: only the `MaxResults` slice), so a capped row cannot be silently dropped below the bar. Assert this with a test rather than a comment.
- **Adjacency survives truncation.** If any cluster member survives the `MaxResults` cut, all of them do. Implement by treating the cluster as an atomic unit at truncation time (allow the response to exceed `MaxResults` by at most `contradictionMaxCluster - 1`, and say so in the `conflict` block). Returning one side of a conflict alone is the failure this whole increment exists to remove.

**Step 7 — one-sided conflicts (partner not in the result set).**
**First increment: annotate-by-reference, no injection.** The row is still capped and annotated with `partner_in_results: false` and the partner's ID + concept.

Weighed against `#763`'s injection precedent: COG-28 injects because the chain *head* is the answer and the predecessor demonstrably is not — a declared ordering exists. Here **neither side is known to be right**; injecting a memory the query did not match would let a conflict *lift* content into a result set it did not earn, which is precisely what the demote-only constraint forbids, and it doubles the noise on every ambient query touching a disputed topic. Injection is a deferral (§6), not a rejection.

**Step 8 — zero writes.** Association reads, engram/metadata reads, lease reads via the shared gate. Nothing else. Observe-safe (COG-11) by construction; add the assertion to the observe-mode test.

### 2.5 Payload

**Per-row, always-on** (one new field, a struct, to keep the drift surface minimal):

```go
// mbp.ActivationItem
UnresolvedContradiction *ContradictionConflict `msgpack:"unresolved_contradiction,omitempty" json:"unresolved_contradiction,omitempty"`

type ContradictionConflict struct {
    With            string `json:"with"`                        // partner ULID
    WithConcept     string `json:"with_concept,omitempty"`
    Side            string `json:"side"`                        // "asserted" | "challenged"
    DeclaredAt      string `json:"declared_at,omitempty"`       // RFC3339; OMITTED when unknown, never zero-time
    PartnerInResults bool  `json:"partner_in_results"`
    ScoreCapped     bool   `json:"score_capped"`
    ClusterSize     int    `json:"cluster_size,omitempty"`
    ClusterTruncated bool  `json:"cluster_truncated,omitempty"`
}
```

Always-on, for the same reason `superseded_by` is (`internal/mcp/convert.go:31-34`): an agent must never be handed a disputed fact without being told.

**Response-level** — new field on `mbp.ActivateResponse`, mirrored by hand into the MCP result map:

```json
"conflict": {
  "unresolved": true,
  "pairs": [{
    "a": "01K…", "b": "01K…",
    "a_concept": "request timeout limit", "b_concept": "request timeout limit revised",
    "declared_at": "2026-08-01T18:52:23Z",
    "preferred": "b", "basis": "newer_valid_from"
  }],
  "warning": "Two returned memories are declared to contradict each other and the conflict is unresolved. Neither is presented as the answer: both are returned adjacent and score-capped. Resolve it — muninn_evolve the memory that should survive, muninn_forget(not_true_since=…) the side that stopped being true, or muninn_link(relation=\"supersedes\") to declare which one wins."
}
```

`Abstained` is **not** set (the response is non-empty); the "Empty iff Abstained is false" contract is untouched.

---

## 3. Cross-surface plumbing checklist (exact, for the build agent)

**Per-row field** (`UnresolvedContradiction`):

1. `internal/engine/activation/engine.go` ~`:218` — add to `ScoredEngram` (beside the COG-25 block at `:212-215`).
2. `internal/engine/engine_contradiction.go` (**new file**) — `applyContradictionHonesty` sets it.
3. `internal/engine/engine.go` ~`:2688` — copy `ScoredEngram` → `mbp.ActivationItem` (the block that already copies `PossiblySupersededBy`/`VersionCluster`).
4. `internal/transport/mbp/types.go` ~`:353` — field on `ActivationItem` with **both** `msgpack:` and `json:` tags (the json tag is load-bearing: REST aliases this type). Encoding is reflection-based msgpack (`mbp/codec.go:11`) — no hand-written encoder to touch. Add the `ContradictionConflict` struct beside `SubstitutionBasis` (`:389`).
5. `internal/mcp/types.go` ~`:153` — field on `MemoryAnnotations`.
6. `internal/mcp/convert.go` **`:39-41`** — **add to the "should I allocate annotations at all" predicate.** Miss this and the field is silently dropped on rows carrying no other annotation. Then copy at `:48`.
7. `internal/transport/rest/openapi.yaml` ~`:437` — schema entry (hand-maintained; `npx @redocly/cli lint` must pass). REST needs **no Go change** — `rest.ActivateResponse = mbp.ActivateResponse` alias, `rest/types.go:25`.
8. **proto/gRPC and the non-Go SDKs: add nothing.** Obligation #3 (`drift-and-obligations.md:31-42`) is explicit — `pb.ActivationItem` carries no annotation fields at all, and adding only the new one is the silently-wrong class. Note the deliberate omission in the PR body.

**Response-level field** (`Conflict`):

9. `internal/transport/mbp/types.go` ~`:302` — on `ActivateResponse`, beside `AbstainedReason`.
10. `internal/engine/engine.go` ~`:2617-2628` — set it in the same block that recomputes `Abstained`.
11. `internal/mcp/handlers.go` ~`:588` — **the MCP response is a hand-built `map[string]any`, not a mirror of the mbp struct.** A field added to mbp alone reaches REST and *silently vanishes on MCP*. Add `result["conflict"] = …`.
12. `internal/transport/rest/openapi.yaml` ~`:511` — response-level schema. (`abstained`/`abstained_reason` are **already missing** here — fixing that drift in the same PR is cheap and welcome.)

**Docs / invariants:**

13. `docs/internals/invariants.md` — add **COG-29** (the §2.2 statement, with the pinning test names). Amend **COG-23**: its Link→`ContradictWorker` claim is still true; add that the worker's flush was a debounce until #764 and that `FlagContradiction`'s `newlyFlagged` is the penalty's idempotency token (so a synchronous marker write in `Link` would kill the penalty).
14. `docs/internals/drift-and-obligations.md:34-36` — add `unresolved_contradiction` and the response-level `conflict` to the MCP+MBP-only annotation list.
15. `docs/internals/keyspace-registry.md:35` — no new prefix, but amend the `0x0A` row: the key is `…|id(16)` with a single-partner value, so **one engram can record exactly one `0x0A` partner** — a second contradiction on the same engram overwrites the first. This is why COG-29 keys on declared `0x03`/`0x04` edges, not on `0x0A`.
16. `internal/mcp/guide.go` (~`:165-181`) and `internal/mcp/tools.go:293` — describe the new annotation and the resolution actions.

**No MCP tool is added or removed in this increment** → `allMCPTools` in `cmd/muninn/smoke_exhaustive_test.go` and `isMutatingTool`/`isReadOnlyTool` are unchanged (obligation #1). **No new worker** → obligation #12 is unchanged; F1.1 *improves* the existing drain behavior.

---

## 4. The resolution story — verified path by path

Every row below was verified against the code at `bb10f30`.

| Operation | declared `0x03` edge | `0x0A` marker | today: still in `muninn_contradictions`? | today: recall theater? | **after this increment** |
|---|---|---|---|---|---|
| `Evolve`/`EvolveAt` (`engine.go:3586`/`:3601`) | survives on the predecessor; **successor does not inherit it** | survives | yes (`detected`, pointing at a soft-deleted engram) | n/a (no recall behavior exists yet) | **cleared on both surfaces** — predecessor fails the liveness clause |
| `Forget` `not_true_since` (`engine.go:3187`) | survives | survives | yes | — | **cleared** — `ValidUntil` elapsed ⇒ not live |
| `Forget` soft (`engine.go:3247`) | survives | survives | yes | — | **cleared** — `StateSoftDeleted` ⇒ not live |
| `Forget` hard (`engine.go:3209` → `storage/engram.go:491`) | **deleted** (both directions) | **survives, dangling** | yes — `detected` with a permanently blank concept | — | **cleared** — no edge, and the dangling `0x0A` is filtered by the endpoint-resolution filter |
| `Link(relation="supersedes")` between the pair | survives | survives | yes | — | **cleared** — resolution clause 3 (`currencyHasExplicitSupersedesEdge`) |
| REST `POST /api/admin/contradictions/resolve` (`rest/server.go:1730` → `query.go:306`) | **survives** | deleted | yes — flips back to pending and the worker re-flags it | — | unchanged; still not a durable resolution (see deferrals) |
| `muninn_decide` (`engine.go:4058`) | survives | survives | yes | — | unchanged — `Decide` writes a `TypeDecision` engram + `RelSupports` links and touches contradictions **not at all** |

There is **no unlink anywhere in the product**: `grep DeleteAssociation\|RemoveAssociation` across `internal/` and `cmd/` returns zero production definitions. The only `0x03` deletes are `DeleteEngram` (`storage/engram.go:577`, `:606`) and the whole-vault wipes (`storage/vault_lifecycle.go:26`). The `OpAssocUnlink = 0x0006` WAL opcode (`internal/wal/mol.go:33`) is unused.

**So the evaluators were right twice over:** they resolved via evolve+forget, expected the theater to stop, and nothing in the product could have stopped it.

### 4.1 The change that makes resolution work — one rule, two surfaces

Apply the **same liveness + resolution test** in both places:

- **Recall** — §2.4 Step 3, clauses 2 and 3.
- **`Engine.GetContradictionReport`** (`internal/engine/query.go:200`):
  - `fillContradictionConcepts` (`query.go:267`) already does a batched `GetEngrams` over the distinct pair IDs. **Reuse that read** to drop pairs where either endpoint is missing, soft-deleted, archived, or has an elapsed `ValidUntil` — zero extra I/O.
  - Add the `RelSupersedes`-between-the-pair check to mark such pairs `resolved` (or omit them; prefer `Status: "resolved"` with `resolved_by: "supersedes"` — an omission is another unknown-reported-as-known).
  - Keep `ScanComplete`/`Scanned` semantics exactly as-is.

Empirically verified at `bb10f30`: after `evolve(a)`, recall no longer returns `a` (only `b` and the successor `a'`), while `GetContradictionReport` **still lists the `(a,b)` pair as pending — with `a` soft-deleted.** That is the exact defect this rule closes, and it is why the recall-side design keys on the *live result set* rather than a durable flag: **the theater stops for free on the recall surface** the moment either side stops being live.

---

## 5. Measurable acceptance

### RED repros (must FAIL on `bb10f30`)

- **R1 — D1, engine level, RED-checked.** `internal/engine/engine_contradiction_detect_test.go`: engine wired with a live `ContradictWorker`; `Link(contradicts)`; then one ordinary `Write` + one report poll every 5s for 60s; assert `DetectedCount == 1`. Fails 12/12 polls at `bb10f30`; passes at ~31s with F1.1. *(Already written and run in a scratch copy — hand it to the build agent as-is.)* Per obligation #12, prefer a deterministic drain seam over `time.Sleep` if one can be added to `Worker[T]` (a `Flush()`/`WaitIdle()` test hook); otherwise mark the test as timing-based and keep it out of `-race` CI.
- **R2 — D1, MCP surface.** `internal/mcp/` handler test over `muninn_link(relation="contradicts")` → `muninn_contradictions`: assert `status == "declared"` immediately and `confidence_penalty` flips to `"applied"` after the worker flushes.
- **R3 — D2, engine level.** The §2.1 fixture: assert (i) both rows carry `UnresolvedContradiction`, (ii) neither score exceeds `contradictionScoreCeiling`, (iii) the newer side is rank 0, (iv) the two rows are adjacent, (v) the response carries `Conflict`. All five fail at `bb10f30`.
- **R4 — D2, MCP surface.** Same through `muninn_recall`: `memories[*].annotations.unresolved_contradiction` present on both, and a top-level `conflict` key. Guards the `convert.go:39-41` predicate and the hand-built MCP map — the two places a field silently disappears.
- **R5 — resolution.** `link(contradicts)` → assert theater; `evolve` the losing side → assert **no** `Conflict`, **no** per-row annotation, and `GetContradictionReport` no longer lists the pair. Repeat for `forget` (soft), `forget(not_true_since)`, `forget(hard)`, and `link(supersedes)`. All five fail at `bb10f30` on the report half.
- **R6 — demote-only.** A conflicting pair plus a genuinely better unrelated match: assert the unrelated row's rank **never worsens** and the pair never rises above it. Assert `len(out) == len(in)`.
- **R7 — as-of.** Declare the conflict at T1; query with `as_of = T0 < T1`: assert **no** conflict block and **no** score cap. Query with `as_of > T1`: assert the block is present.
- **R8 — observe-safe.** Run the phase under `ReadOnly`/observe and assert zero writes (existing observe-mode harness).

### Corpora that must NOT move

- `TestMeasureRecallQuerySet` (`internal/engine/activation/recall_queryset_measure_test.go:915`) — labeled set NDCG@5 **0.8587**, unchanged.
- `TestMeasureAbstentionGate` (`internal/engine/activation/abstention_gate_measure_test.go`) — **0.6410 / 6.2%**, unchanged.
- `TestMeasureShadowPrecision_GraftedChain` (`shadow_measure_test.go:66`) — unchanged.

None of these corpora contain a `contradicts` edge, so with the Step-0 fast-path gate the new phase must be a **provable no-op** on all three. Record before/after numbers in the PR body; a delta of any size is a bug, not a tradeoff.

**Watch item:** F1.1 changes flush timing for *every* `Worker[T]`, including `ConfidenceWorker`. Confidence multiplies into the recall score, so run the corpora **with F1.1 applied** as well as with the new phase, and run `go test -race -tags localassets ./...` (storage + worker paths).

### New conflict corpus (small, unit-cost)

`internal/engine/engine_contradiction_measure_test.go` — ~8 engrams, 4 queries, no embedder required:

1. declared conflict, both sides retrieved → both surfaced, conflict block present, neither above the ceiling, newer first, adjacent;
2. same fixture after `evolve` → clean response, no block, no cap;
3. conflict pair plus a stronger unrelated match → unrelated row's rank unchanged;
4. one-sided (partner below the retrieval cut) → row capped and annotated with `partner_in_results: false`, **no injection**.

---

## 6. Minimal first increment, and what it explicitly defers

**In (one PR, or two if the reviewer prefers D1 landing first):**

- F1.1 worker-ticker fix + F1.3 status rename, with R1/R2.
- `applyContradictionHonesty` (COG-29) with the payload of §2.5 and R3/R4/R6/R7/R8.
- The liveness + resolution filter on `GetContradictionReport`, with R5.
- Invariants, drift, keyspace-registry note, guide/tools text, openapi.

**Deferred, named:**

1. **Injecting the absent partner** on a one-sided conflict (§2.4 Step 7). Revisit only with measured evidence that annotate-by-reference is insufficient.
2. **A durable resolution surface** — an MCP `muninn_resolve_contradiction` (or an unlink primitive) that removes the declared `0x03` edge *and* the `0x0A` marker atomically. Today the REST resolve endpoint removes only the marker and the worker re-flags it; the web "dismiss/keep_a/keep_b" flows (`web/static/js/app.js:1073-1109`) add a supersedes link and archive one side but never remove the contradicts edge. After this increment the archive/supersede paths *do* clear the recall theater via the liveness/resolution rule, so the missing primitive is no longer user-blocking — but it should still be built.
3. **The `0x0A` single-partner limitation** (keyspace-registry §15). Recall keys on declared edges and is unaffected; the `detected` half of the report can still lose a partner when one engram contradicts two others. A migration to `0x0A|ws|…|id|partner` is out of scope.
4. **The incoherent both-sides confidence penalty** — `#747`'s named residual ("at most one of a contradicting pair is wrong"). COG-29 makes the *visibility* story honest without touching confidence; whether the penalty should exist at all is a separate decision. Note that F1.1 makes that penalty actually fire under load for the first time — watch for it in the corpora run.
5. **gRPC / non-Go SDK annotation parity** — the whole annotation block, or nothing (obligation #3).
6. **Cross-process freshness of the Step-0 fast path** (≤30s for an edge declared by another node).

---

## 7. Top risks

1. **F1.1's blast radius.** It changes flush cadence for every `Worker[T]`. `ConfidenceWorker` will now fire under load where it previously never did — which is correct, and also a behavior change that can move recall scores on a vault with contradictions or feedback. *Mitigation:* run all three measure corpora with F1.1 alone before adding the new phase, so the two effects are separable. `go test -race` on storage/cognitive.
2. **A stale declaration nobody resolves permanently caps a pair.** *Mitigation:* the liveness clause (any lifecycle action clears it), the supersedes-resolves clause, and a warning that names all three resolution actions in words. Accepted residual: an agent that declares a conflict and walks away leaves both facts capped at 0.95 — visible, recoverable, and strictly better than one of them being presented as truth.
3. **Recall-path I/O.** Two association reads per examined row (≤ K+16 rows). Same order as `applySupersession`, and the Step-0 gate makes it free on the overwhelming majority of vaults. *Mitigation:* record p50/p99 recall latency on a real vault before/after; if the gate is skipped, budget ~26 bounded prefix iterators.
4. **Adjacency vs `MaxResults`.** Letting a cluster exceed `MaxResults` by up to `contradictionMaxCluster-1` breaks the strict "at most K results" expectation. *Mitigation:* bound it, state it in the `conflict` block, and test it. Returning one side of a conflict alone would be the worse bug.
5. **The MCP hand-built response map.** `internal/mcp/handlers.go:563-607` is not a mirror of `mbp.ActivateResponse`. R4 exists specifically to catch the field vanishing there, and `convert.go:39-41` is the second such trap.
6. **The tempting wrong fix for D1** — writing `0x0A` synchronously in `Link`. It silently disables the confidence penalty forever (F1.2). Put this in the PR body; a reviewer *will* ask for it.

---

## 8. Evidence log

All runs against a byte-copy of the worktree at `bb10f30` (target repo unmodified).

| # | What | Result |
|---|---|---|
| E1 | Link→poll, idle engine, live `ContradictWorker` | detected at **30.006s** — the path is wired correctly |
| E2 | Link→poll with one `Write` + one poll every 5s for 60s | `detected=0 pending=1` at **all 12 polls** — RED, D1 confirmed |
| E3 | E2 with the F1.1 ticker fix | **passes at 31s**; `go test -tags localassets ./internal/cognitive/...` green |
| E4 | recall over a declared conflict | both rows, tied `0.0935`, **older first**, no annotation, no response signal — RED, D2 confirmed |
| E5 | `evolve` the losing side, then recall + report | predecessor **gone from recall**; report **still lists the pair as pending** with a soft-deleted endpoint — D3 confirmed, and the reason recall keys on the live set |
| E6 | code trace of `Evolve`/`Forget`/`DeleteEngram`/`ResolveContradiction`/`Decide` vs the `0x03` edge and the `0x0A` marker | §4 table — **no operation clears both** |
