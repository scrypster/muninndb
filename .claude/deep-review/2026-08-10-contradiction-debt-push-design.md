# Unresolved-contradiction debt at session start

**Status:** increment 1 BUILT (branch `feat/contradiction-debt-push`, MCP only, no PR yet —
adversarial review runs first). Increment 2 (MBP/REST `ActivateResponse` field + the
count-only `StatResponse`/`muninn_status` readout) is committed per §11 Q2 and NOT built.
§7 Stage B is unstarted: whether this STAYS is undecided.
**Date:** 2026-08-10
**Touches:** COG-29 (amendment), COG-11 (compliance), obligations #1/#3.
**Tier:** 2 (MCP presentation + one read-only engine method). No Pebble prefix, no
on-disk format, no migration.

---

## 1. The premise, verified against the code

**Claim under test:** an unresolved declared contradiction is only ever spoken about when
one of its two memories happens to be retrieved. Nothing surfaces it as vault-level debt.

Verified. Every existing path is result-set-scoped, opt-in, or pull-only:

| Path | File | Why it did not fire in the motivating incident |
|---|---|---|
| Per-row `annotations.unresolved_contradiction` | `internal/mcp/convert.go:55-58` | Attached only to a returned row. Requires the conflicted memory to be *in the result set*. |
| Response-level `conflict` block | `internal/mcp/handlers.go:582-584`; built in `internal/engine/engine_contradiction.go:385`, pruned by `pruneConflictBlock` at `:472-476` | Explicitly pruned to pairs whose endpoints survived into the caller's results. Same scoping. |
| Prospective contradiction notice | `internal/engine/prospective.go:415-447` | Three independent reasons: (a) the whole delivery path is inert unless `MUNINN_PROSPECTIVE=1` (`internal/mcp/server.go:82`, `prospective.go:179`); (b) it intersects `resultIDs` with the 0x0A pairs — result-set-scoped again; (c) notices are attached only to `muninn_recall`/`muninn_remember`, and "notices on `where_left_off`" is a *named deferral* at `prospective.go:31`. |
| `muninn_contradictions` | `internal/mcp/handlers.go:743-780` | Pull-only. Correct, complete, and nothing ever prompts an agent to call it. |
| `muninn_guide` contradiction section | `internal/mcp/guide.go:182-193` | Explains what happens **when you declare one**. Says nothing about whether this vault currently has any. |
| `muninn_where_left_off` | `internal/mcp/handlers.go:1185-1232` | Returns `{memories, count, hint}`. No conflict awareness at all. |
| `muninn_status` | `internal/mcp/handlers.go:782-797` | Returns `{vault, total_memories, health:"good", enrichment_mode}`. `Health` is a hardcoded literal — see §9, finding 2. |
| MCP `initialize` instructions | `internal/mcp/server.go:715-725`, `internal/mcp/context.go:15` | A compile-time `const`, no vault context. Structurally cannot carry per-vault state. |

**The premise holds, and it is sharper than "nothing pushes."** The precise statement is:

> Recall's contradiction honesty is *conditional on retrieval*. A conflict the agent
> declared is only ever re-raised if a later query happens to retrieve one of its two
> members. Debt that stops being queried stops being visible — while the demote waits to be
> applied to both facts the moment either one IS retrieved.

**AMENDED per §11 (2026-08-10), before build.** The first draft of this paragraph said
"the penalty is unconditional and the notice is conditional." That is **false** and the
opt-in panelist refuted it: `applyContradictionHonesty` mutates `out[i].Score` over the
RETRIEVED result window only, so a declared pair on a never-queried topic — the motivating
incident — is charged nothing recurring. The recurring 10% demote is
**retrieval-conditional**. What survives is narrower and is what this design rests on:

1. The **verified asymmetry** is charged-when-retrieved vs told-when-returned. The demote is
   applied over the PRE-truncation examined window while `pruneConflictBlock` prunes the
   receipt to the POST-truncation set the caller actually received — so a row can be demoted
   and its explanation dropped in the same response.
2. The **one-time asynchronous confidence penalty IS unconditional** — it fires once per
   declared pair, from the batch worker, regardless of whether anything is ever queried.
3. COG-29's own accepted residual (invariants.md, COG-29 final paragraph) claims a
   declared-and-abandoned conflict leaves both facts "visible and recoverable via three named
   actions the warning spells out" — but the warning is only spelled out on a query that
   retrieves them. **This block is what actually delivers the visibility that residual
   already claims.** On a ~4,880-memory production vault, retrieval is not a guarantee, it is a
   coincidence.

### The presumed surface is wrong, and the code says so

The task presumed `muninn_where_left_off`. In the motivating deployment — a shared,
multi-user vault — the product **tells agents not to call it**:

- `internal/mcp/guide.go:49` — "Avoid `muninn_where_left_off` and `muninn_session` here —
  they return vault-global recency across all users (admin/audit use)."
- `internal/mcp/guide.go:82` — "…(admin/audit; **not for session start**)"
- `internal/mcp/guide.go:284`, `internal/mcp/handlers.go:606`, `:1225` — the same
  redirection repeated on three more surfaces.

A design that put the debt only on `where_left_off` would ship it into a surface the
motivating deployment is instructed never to call. **That is the load-bearing correction
to this proposal's stated mechanism.** The shared-vault session-start call the guide
actually prescribes is:

> `muninn_recall(context=["<your per-user tag>", "session start"], mode="recent")`
> — `internal/mcp/guide.go:47`

So the orientation surface set is **three**, not one, and the shared-vault member of it is
`mode="recent"` recall.

---

## 2. What this is NOT — confronting the #609 kill

`docs/internals/decision-record.md:195` lists **"ambient push (negative result, #609)"**
under *"Explicitly deferred / rejected — do not reopen without new evidence."* The record
at `:160-166`: 523 ambient-push deliveries, zero uptake.

This design must clear that bar or die. Five structural differences, each checkable:

1. **It is not a salience ledger.** #609 pushed *what the system guessed you'd care about*.
   This pushes *an assertion the agent itself made and left open*. There is no inference and
   no ranking of the world — the vocabulary is `RelContradicts` edges the caller wrote.
2. **Silence is the overwhelming default.** A vault with no unresolved declared
   contradiction emits **zero bytes and does zero I/O** (§4, the gate). #609's delivery
   pipeline had something to say on every call; this one has something to say only when the
   agent left a declared conflict open.
3. **It is a receipt for a penalty already charged.** COG-29 is demoting both facts 10%
   right now, on every query that touches them, by default. Withholding the notice while
   applying the penalty is the asymmetry; this closes it. #609 pushed a *new* claim on the
   agent's attention. This pushes the *explanation of an existing cost*.
4. **No scheduler, no interrupt, no background delivery.** Same doctrine as COG-21: it is
   computed inside a tool call the agent itself made. There is no sweep and the EventBus
   stays dormant (`internal/mcp/eventbus.go` is untouched).
5. **New evidence exists.** A production vault, high write discipline, a correctly declared
   contradiction, all machinery working as specified, **~24h
   unresolved** because the only speaking path required re-retrieval. #609's evidence was
   "523 deliveries, zero uptake"; this design's acceptance rule (§7) is built to produce
   the same *kind* of number and to kill itself on the same result.

**If the acceptance rule in §7 returns #609's number, this ships to `docs/internals/` as an
honest negative and gets reverted.** That is written down before any measurement.

---

## 3. Mechanism

One read-only engine derivation, three MCP orientation surfaces, zero new tools.

### 3.1 `Engine.ContradictionDebt` — the derivation

New method in `internal/engine/engine_contradiction.go` (beside the COG-29 phase it
extends, not a new file, not a new subsystem):

```
func (e *Engine) ContradictionDebt(ctx, vault string) (*ContradictionDebt, error)

type ContradictionDebt struct {
    Count        int                  // TRUE total unresolved declared pairs, never capped
    Oldest       time.Time            // zero = unknown (legacy edge), never rendered as 1970
    Pairs        []ContradictionDebtPair // capped at debtPairsShown, oldest first
    Truncated    bool                 // Count > len(Pairs)
    ScanComplete bool                 // from ContradictionReport.ScanComplete
}

type ContradictionDebtPair struct {
    IDa, ConceptA, IDb, ConceptB string
    DeclaredAt time.Time  // zero = unknown
}
```

Implementation, in order:

1. **Gate first, for free.** `ws := e.store.ResolveVaultPrefix(vault)`; if
   `!e.vaultMayHaveContradictions(ctx, ws)` return `nil, nil`. This is COG-29's existing
   three-probe fast path (`engine_contradiction.go:142-183`): an in-process flag, then one
   bounded 0x0A `First()` seek, then a once-per-process memoised declared-edge scan. The
   clean-vault case is a `sync.Map` hit plus one iterator seek. **Reused verbatim — not
   re-derived.**
2. **Reuse `GetContradictionReport` verbatim** (`internal/engine/query.go:257-332`). It
   already does the 0x0A read, the declared-edge scan, the batched concept fill, and —
   critically — `markResolvedContradictions` (`query.go:344-371`), which is the *same*
   #764 D3 liveness-and-resolution rule recall applies. Calling anything else would create
   a **third** definition of "unresolved"; COG-29 already flags the existing two-site
   duplication as hygiene debt.
3. **Filter to `Status == ContradictionDeclared`.** Resolved pairs already carry
   `ContradictionResolved` and drop out. Detected-only pairs carry `ContradictionDetected`
   and are **deliberately excluded** — see §5, decision D2.
4. **Sort oldest-first** by `DeclaredAt`, with a **zero `DeclaredAt` sorting first**
   (unknown-age legacy edges are the oldest thing in the vault by construction, and
   COG-29's clause-4 doctrine is over-warn beats under-warn), tiebroken by `IDa` ULID for
   determinism (the COG-29 `TestContradictionHonesty_PartnerChoiceIsDeterministic` lesson:
   map-range order made an identical query flip 33/7 over 40 calls).
5. **Truncate to `debtPairsShown = 3`**, set `Truncated`, and **never truncate `Count`**.

### 3.2 The wire block

One additive top-level JSON object, identical on all three surfaces:

```json
"unresolved_contradictions": {
  "count": 3,
  "oldest_declared_at": "2026-08-09T04:11:02Z",
  "oldest_age_hours": 26.4,
  "showing": 3,
  "truncated": false,
  "scan_complete": true,
  "pairs": [ { "id_a": "...", "concept_a": "...", "id_b": "...", "concept_b": "...",
               "declared_at": "...", "age_hours": 26.4 } ],
  "action": "<resolution sentence, shared const>",
  "scope_note": "<multi-user vaults only>"
}
```

- **Omitted entirely when `Count == 0`.** Absent, not `{"count":0}`.
- `age_hours` and `declared_at` are **omitted** (not zero, not 1970) when `DeclaredAt` is
  zero, and the pair carries `"declared_at_unknown": true`. Mirrors the existing
  "zero means unknown; leave the field absent" rule at
  `internal/mcp/engine_adapter.go:124-129`.
- `action` is a shared `const` in `engine_contradiction.go` next to `contradictionWarning`
  (`:92`), naming the same three verbs — `muninn_evolve`,
  `muninn_forget(not_true_since=…)`, `muninn_link(relation="supersedes")` — so the two
  strings cannot drift.
- `scope_note` fires only when `GetVaultPlasticity(...).MultiUser`, and mirrors the
  existing shared-vault language at `handlers.go:1225`: these are conflicts across all
  users of this shared vault, not necessarily yours.
- `scan_complete: false` propagates the existing "pending_count is a lower bound" honesty
  (`handlers.go:770-771`) — a capped scan must not report a count as exhaustive.

### 3.3 Attachment points (increment 1)

| Surface | Condition | Where |
|---|---|---|
| `muninn_guide` | always, when `Count > 0` | one paragraph appended to the existing contradiction section, `internal/mcp/guide.go:182-193` |
| `muninn_where_left_off` | always, when `Count > 0` | the `map[string]any` at `internal/mcp/handlers.go:1227-1231` |
| `muninn_recall` | **only when `args["mode"] == "recent"`** and `Count > 0` | the hand-built `result` map at `internal/mcp/handlers.go:564-617`, beside the existing `conflict` key at `:582` |

The `mode == "recent"` condition is deliberate and self-identifying: it is the *exact*
call `guide.go:47` and `guide.go:287` instruct agents to make for session continuity, in
both the shared and single-user branches. It is an **agent-declared orientation intent**,
not a heuristic guess at "is this a session start". Default-mode recall — the hot path,
p50 ~350µs — is untouched.

**The trap the obligations doc names explicitly** (`drift-and-obligations.md:50-53`):
`handleRecall`'s response is a hand-built `map[string]any` that is NOT a mirror of
`mbp.ActivateResponse`. This block is added to that map directly, and the test in §7 asserts
it survives the MCP serializer, not just the engine struct.

**The second trap** (`drift-and-obligations.md:46-49`): `convert.go`'s allocate-annotations
predicate. Not applicable — this is a **response-level** block, never a per-row field, and
never inside `MemoryAnnotations`. It is deliberately not fused with the COG-29 `conflict`
block: `conflict` describes *what you received*, `unresolved_contradictions` describes
*what the vault owes*. Merging them would make a vault-scoped fact look result-scoped and
would break `pruneConflictBlock`'s contract at `engine_contradiction.go:472-476`.

---

## 4. Cost, and the zero-cost-when-clean property

| Vault state | Work per attachment call |
|---|---|
| No contradiction ever declared, probe memoised clean | one `sync.Map` load + one 0x0A iterator `First()` — the COG-29 steady state, already measured *inside the noise* of a normal recall (COG-29: gate closed p50 348-383µs, gate open p50 360-372µs) |
| First call in a process on a clean vault | + one bounded declared-edge scan, memoised forever after: **8.5ms over 100k associations** (`BenchmarkDeclaredContradictions`, cited in COG-29) |
| Vault with declared contradictions | full `GetContradictionReport`: 0x0A scan + declared-edge scan + one batched `GetEngrams` over the (small, deduplicated) endpoint set |

**AMENDED after the adversary pass (2026-08-10): the table above was incomplete and its
conclusion was wrong.** It had no row for the state the motivating vault ends up in, and
the "no cache" decision did not survive measurement.

| Vault state | Work per attachment call (as BUILT) |
|---|---|
| No contradiction ever declared | one `sync.Map` load + one 0x0A `First()` — **~1.2µs/op, 3 allocs** measured |
| **Debt declared and RESOLVED** (the missing row) | the fast-path flag is STICKY and resolution never deletes the declaring edge, so the gate stays open forever. Uncached this paid the FULL scan on every orientation call to emit nothing: **~206-253µs** on a 2,000-association vault, **~8ms** at 100k. Now **~37µs** |
| Vault with live declared debt, steady state | scan cached, everything else re-derived: **~41µs/op** (was ~206-244µs) |
| Vault with live declared debt, first call after a declaration | full scan: **~206µs/op** at 2,000 associations, ~8.3ms/100k, bounded by the 500k scan cap at roughly **~55ms** |

The §11 gate TRIPPED on the capped-scan case (~55ms > the ~50ms R5 line), so per §11 the
cache was promoted from deferral to blocker and BUILT. What is cached is **only the
declared-edge scan**, invalidated by a per-vault `RelContradicts` write counter in the
store — no TTL, and no caching of the derived answer, because resolution depends on engram
state and on the CLOCK and a stale resolution is the bug #764 closed. See §8/R7.

---

## 5. Decisions taken, and why

**D1 — Three surfaces, not one, because no single surface is universal.**
`where_left_off` is contraindicated in shared vaults (`guide.go:49`); `mode="recent"`
recall is prescribed there but is not guaranteed in a single-user vault; `muninn_guide` is
"call on first connect" per `mcpInstructions` (`context.go:15`) and `tools.go:667` but is
also not guaranteed. **There is no MCP surface a client is obliged to call at session
start** — `initialize` is the only guaranteed one and it is a vault-agnostic `const`
(`server.go:724`). Honest statement of the guarantee: this makes the debt visible on every
orientation call the product itself recommends, in both vault shapes. It does not make it
unmissable, and this design does not claim it does. The residual is named in §8, R1.

**D2 — Declared only. Detected-but-not-declared pairs are excluded.** Two independent
reasons, either sufficient:
1. The asserted-vs-inferred boundary that COG-25, COG-28 and COG-29 all hold. COG-29:
   *"'Declared' means an explicit `storage.RelContradicts` edge … and nothing else."*
   A session-start push is a stronger claim on attention than a per-row annotation and must
   sit on the stronger evidence.
2. **The COG-23 R2 residual makes the alternative actively harmful.** Before R2,
   `contradict.go` fabricated a 0.8-severity contradiction for *any* two same-`RelType`
   associations to different targets. Those 0x0A markers **are not migrated and are
   mechanically indistinguishable from genuine ones** (COG-23, "Residual"). A debt block
   counting 0x0A markers would greet agents on long-lived upgraded vaults with a session-
   start nag about contradictions that were never real. That is #609's failure mode
   reproduced with worse data.

**D3 — No age threshold anywhere. Principle #11 is satisfied by construction, not by
calibration.** There is no "older than N hours ⇒ push" rule to tune. Behaviour: *always
list unresolved declared contradictions, oldest first, capped, with age reported as a
number the agent can judge.* The only constant is `debtPairsShown = 3`, which is an
**output-size budget, not a property of vault data** — the same class of constant as
`noticeCapPerResponse = 2` (`prospective.go:37`, "the attention budget") and
`shadowMatchCap = 16` (COG-28). It does not encode one vault's shape, and `Count` is always
the true total so no vault's debt is ever under-reported. If a maintainer disagrees, it
belongs in plasticity as a per-vault override — but shipping it as an override in
increment 1 would be a knob with no evidence that anyone needs a different value.

**D4 — RESOLVED by §11: a per-vault plasticity bool, resolved default TRUE.** Not an env
flag, and not pure default-on. The original argument ran: (a) the COG-29 **demote is already
on by default** — ~~both facts are being charged 10% right now~~ **AMENDED: both facts are
charged the one-time confidence penalty unconditionally, and the 10% demote the moment
either is retrieved; the notice is the receipt, and a flagged receipt for an unflagged
penalty is the asymmetry this design exists to remove**; (b) the zero-contradiction case
emits zero bytes and does zero extra I/O, so the default-on blast radius on a clean vault is
exactly nothing; (c) the motivating deployment's failure is *partly* that the one existing
push path (prospective) is off by default and therefore invisible; shipping the fix behind
another default-off flag reproduces it. §11 accepted (b) and (c), narrowed (a) as above, and
landed the control at vault grain — see §11 Q1 for why the third shape won.

**D5 — Exposure is unchanged, and here is the evidence.** `GetContradictionReport` resolves
concepts via `fillContradictionConcepts` (`query.go:373+`), which does **not** apply the
COG-22 visibility gate that recall's `resolveContradictionPartners`
(`engine_contradiction.go:608`) does. So this block inherits `muninn_contradictions`'
exposure semantics, not recall's. That is **not an escalation**: `muninn_contradictions`
is classified read-only (`internal/mcp/context.go:219`) and is reachable by every
credential that can reach `muninn_recall`/`where_left_off`/`guide`. No caller learns
anything they could not already have learned in one extra tool call. It is a genuine
*difference* though, it is on a presentation surface, and §8/R3 flags it for the adversary
pass.

**D6 — No new tool.** SEC-6 classification and the `allMCPTools` registry-parity smoke
test (obligation #1) do not need a new entry. Three existing handlers change; the smoke
test is re-run but not amended.

---

## 6. Minimal first increment, and what it DEFERS

**In scope (increment 1):**
1. `Engine.ContradictionDebt` + its two types (`internal/engine/engine_contradiction.go`).
2. The `unresolved_contradictions` block on `muninn_guide`, `muninn_where_left_off`, and
   `mode="recent"` `muninn_recall` (MCP only).
3. One shared `action` const, reusing the COG-29 warning's three verbs.
4. A COG-29 amendment in `docs/internals/invariants.md`.
5. The tests in §7.

**DEFERRED, explicitly:**
1. **Default-mode recall, `muninn_remember`, `muninn_read`, `muninn_session`.** The hot
   path stays untouched until §7 Stage B returns a number.
2. **REST / gRPC / MBP / SDKs / web console / CLI.** MCP-only. Rationale: REST/gRPC/MBP have
   no session-start concept, and obligation #3's "silently-wrong class" concerns *per-row
   annotation fields on a shared struct* — this is a response-level block on three
   MCP-specific handlers, not a field added to `mbp.ActivationItem`. **Flagged for the
   maintainer:** if MBP is considered an agent-facing session surface, `where_left_off`
   parity there is the natural increment 2.
3. ~~**Any caching/memoisation of the debt snapshot.**~~ **NO LONGER DEFERRED — the §11
   gate tripped and this shipped in increment 1.** The measurement that was missing now
   exists (§4): the capped scan is ~55ms, above the R5 line, and a RESOLVED vault paid the
   full scan forever to emit nothing. Built as an event-invalidated cache of the SCAN ONLY,
   with no TTL. The derived answer is deliberately NOT cached.
4. **Detected-but-not-declared pairs** (D2) and any migration/cleanup of COG-23's legacy
   fabricated 0x0A markers.
5. **Per-user scoping of debt in shared vaults.** Increment 1 says so in `scope_note`
   rather than filtering, matching how `where_left_off` already handles vault-global scope
   (`handlers.go:1224-1226`).
6. **`muninn_status.Health` honesty.** `handlers.go:791` hardcodes `Health: "good"`. Making
   vault health reflect contradiction debt (and anything else) is a separate, larger
   honesty fix — see §9, finding 2. Not this increment.
7. **SSE / `notifications/muninn/contradiction`.** The EventBus stays dormant, per THE
   PUSH increment 1's own doctrine (`prospective.go:15-23`).
8. **Escalation / nagging over time.** No behaviour changes with age. Age is *reported*;
   nothing is *triggered* by it (D3).

---

## 7. The measurable proof, and the PRE-COMMITTED acceptance rule

Written before any number is looked at.

### Stage A — structural, in CI, decides whether it MERGES

**A1. The motivating scenario becomes structurally impossible (the RED check).**
`TestContradictionDebt_SurfacedAtSessionStartWhenNeitherSideIsRetrieved`:
declare a `contradicts` link between two memories on topic X; backdate the edge 24h; then
issue the three orientation calls with a context/topic Y that retrieves **neither** member
(assert that first — zero conflicted IDs in `resp.Activations`, and `resp.Conflict == nil`,
so the pre-existing paths are proven silent). Assert `unresolved_contradictions.count == 1`,
the pair named, `age_hours >= 24`, on **all three** surfaces.
*RED arm:* with `ContradictionDebt` stubbed to return `nil`, the same test must fail on all
three surfaces, and `resp.Conflict` must still be `nil` — i.e. the RED arm reproduces the
production incident exactly. A test that passes both ways proves nothing (CLAUDE.md §3.3).

**A2. Zero-debt is byte-identical to today.** `TestContradictionDebt_CleanVaultIsAByteForByteNoOp`:
on a vault with no `contradicts` edge, the JSON of all three responses is identical to the
pre-change golden. No `"unresolved_contradictions"` key, not even empty. Mirrors COG-29's
own `TestContradictionHonesty_NoContradictionsIsANoOp`.

**A3. Bounded under load.** `TestContradictionDebt_FiftyPairsStaysBounded`: 50 unresolved
declared pairs ⇒ `count == 50`, `len(pairs) == 3`, `truncated == true`, and the serialized
block is `< 1.5 KB`. (Adversary pre-empt: what displaces what at 50 conflicts.)

**A4. Resolution turns it off, on the same rule recall uses.**
`TestContradictionDebt_ResolvedPairsDisappear`, parameterised over all three resolution
verbs (`evolve`, `forget(not_true_since)`, `link(supersedes)`) — each must drop `count` to
0. This is the pin that keeps the debt readout and COG-29 on **one** definition of
"unresolved". RED arm: bypass `markResolvedContradictions` and it must fail.

**A5. Determinism and unknown-age honesty.**
`TestContradictionDebt_OrderingIsDeterministic` (40 identical calls, byte-identical block —
the COG-29 33/7 map-range lesson) and `TestContradictionDebt_ZeroDeclaredAtIsUnknownNot1970`
(zero `DeclaredAt` ⇒ sorts first, `declared_at` and `age_hours` absent,
`declared_at_unknown: true`).

**A6. Cost.** `BenchmarkContradictionDebt_CleanVault` must sit inside the noise of the
existing closed COG-29 gate. `BenchmarkContradictionDebt_WithDebt` is **recorded, not
gated** — it is the input to the deferred-cache decision (deferral 3).

**Stage A merge rule:** all six green, with A1 and A4 RED-checked at a named commit, or it
does not merge. CI cost: unit-only, no `-race` requirement (nothing is written), no new
integration job. Estimated **< 15s** added to the ~6-7 min gate.

### Stage B — field, decides whether it STAYS

Stage A proves the debt is *visible*. #609 proves visible ≠ used. Stage B is the control
that fluff cannot pass.

- **Instrumentation (no LLM, a counter):** for each declared contradiction pair, record
  (i) `declared_at`; (ii) `first_debt_delivery_at` — the first time the block named it;
  (iii) `resolved_at` — when `markResolvedContradictions` first reports it resolved; (iv)
  whether the resolving call arrived in the **same MCP session** (`Mcp-Session-Id`, the
  identity already used by `noticeSessionFromContext`, `prospective.go:59-64`) as a
  delivery.
- **Primary metric:** `P_2 =` fraction of debt-delivered pairs resolved within **2 sessions**
  of first delivery.
- **Control (baseline):** the matched pre-change quantity — fraction of pairs resolved
  within 2 sessions of the first COG-29 `conflict`-block delivery that named them. Same
  vaults, the observation window immediately preceding the change, same extraction code.
  This is a real control, not a strawman: it asks whether *pushing at orientation* beats
  *annotating on retrieval*, which is the actual claim.
- **Window:** 30 days, ≥ 2 production vaults, ≥ 1 of them multi-user.
- **Sample:** `n` = distinct declared pairs that were unresolved at some point in the window.

**Pre-committed decision rule (all three branches written now):**

| Result | Verdict |
|---|---|
| `n ≥ 12` **and** `P_2 ≥ baseline + 0.25` **and** median age-at-resolution reduced by ≥ 30% | **SHIPS.** Consider increment 2 (MBP parity, cache, per-user scoping). |
| `n ≥ 12` **and** `P_2 ≤ baseline + 0.05` | **KILLED.** Revert the three attachments, keep `ContradictionDebt` only if `muninn_status` (§9, finding 2) adopts it. Record as an honest negative in `decision-record.md` alongside #609, with the numbers. |
| `n ≥ 12` and `P_2` lands strictly between | **INCONCLUSIVE — does not ship as a success.** Keep behind an opt-in flag, do not widen surfaces, re-measure at 90 days. |
| `n < 12` | **UNDERPOWERED, not a pass.** The measurement, not the mechanism, failed. Do not report a percentage. Extend the window or add vaults; if `n < 12` again at 90 days, the effect is too rare to justify carrying the surface area — revert. |

**Explicitly forbidden after seeing the data:** changing the window, the vault set, the
2-session horizon, the 0.25 effect size, or the `n ≥ 12` floor. Any of those changes voids
the result and it reverts to UNDERPOWERED.

*Honest note on power:* `n ≥ 12` is not a lot, and this rule cannot detect a small true
effect. That is stated up front rather than discovered afterwards — it is why the third and
fourth branches exist and why neither of them is spelled "success".

---

## 8. Top risks, each with what falsifies the design

**R1 — No surface is guaranteed at session start.** If the real-world agent calls neither
`guide` nor `where_left_off` nor `mode="recent"` recall, the debt stays invisible and Stage
B returns `n` deliveries ≈ 0. *Falsifier:* instrument delivery count in Stage B **before**
resolution rate. If `first_debt_delivery_at` is null for most pairs, the mechanism never
fired and the surface choice — not the idea — is wrong. That outcome argues for a different
increment entirely (a `session_start: true` recall argument, or debt on default recall),
not for tuning this one.

**R2 — #609 repeats: delivered and ignored.** *Falsifier:* Stage B branch 2. This is the
single most likely kill and the rule is written to reach it cleanly.

**R3 — The block leaks vault-global content into a deliberately user-scoped response.** In
a shared vault, an agent that carefully scoped its recall to its own per-user tag now
receives concepts from other users' conflicts. Mitigated by `scope_note` and by D5's
argument that `muninn_contradictions` already exposes exactly this to the same credentials.
*Falsifier:* a credential class that can call `muninn_recall` but **not**
`muninn_contradictions` — a per-key toolset restriction. Note SEC-9: toolset filtering is
advertisement-only and dispatch never consults it, so today no such class exists. If a
future increment makes toolsets a dispatch boundary, this block must route through the
COG-22 gate. **This is the item for the adversary pass.**

**R4 — Debt becomes wallpaper on a vault that legitimately carries many open conflicts.** A
research vault might hold 40 unresolved contradictions as a normal working state. The block
then appears on every orientation call forever. Bounded (A3) but not silent. *Falsifier:*
if Stage B shows `P_2` **below** baseline on high-debt vaults, the push is actively
crowding out the per-row annotation and should be suppressed above a count the vault
derives itself — not a constant.

**R5 — Cost on a large-association vault.** `GetContradictionReport`'s declared-edge scan
is 8.5ms/100k associations; a 10M-association vault would be ~850ms on an orientation call.
*Falsifier:* A6's `BenchmarkContradictionDebt_WithDebt` above ~50ms makes the deferred cache
(deferral 3) a blocker rather than a deferral.

**R7 — Named residuals accepted in increment 1, each with the reason and the test that
would flag a change of mind.**
- **#713 `ExcludeTags` does not filter this readout.** The exclusion is applied inside the
  activation pipeline; this derivation never builds an `ActivateRequest`, so a memory the
  operator excluded from recall RANKING can still have its concept named here. **Reproduced
  behaviourally**, with a control proving recall does drop it
  (`TestContradictionDebt_ExcludeTagsDoesNotFilterTheReadout`). Accepted: `ExcludeTags` is
  documented as ranking-only and explicitly not a hiding mechanism — the engram is not
  deleted and stays visible to direct-id reads. That test fails if it is ever re-scoped into
  a visibility control, which is the flag to filter here too.
- **The block attaches to an ABSTAINED response.** Deliberate. Abstention describes the
  ANSWER to this query ("the vault has nothing for you"); the block describes what the VAULT
  OWES. Suppressing it there would make a debt least visible exactly when the agent got no
  results to read.
- **Association DELETION does not invalidate the scan cache.** A contradicts edge pruned by
  weight decay can leave a cached scan listing a pair whose edge is gone. That is an
  OVER-warn, and a pair whose endpoints are also gone is dropped downstream as dangling.
  Under-warning is what the counter structurally prevents; over-warning is what it accepts.
- **Lease-hidden concepts.** D5 holds: exposure matches `muninn_contradictions`, reachable by
  every credential that reaches these three tools, and SEC-9 makes toolset filtering
  advertisement-only so no splitting credential class exists. Revisit if toolsets become a
  dispatch boundary.

**R6 — Two definitions of "unresolved" drift apart.** If a later change to
`markResolvedContradictions` or `contradictionEndpointLive` touches only one site (COG-29
already flags this duplication), recall and the debt block could disagree — the exact
"resolved it and the theater continued" bug #764 closed. *Falsifier:* A4 fails. Mitigation:
`ContradictionDebt` calls `GetContradictionReport` and nothing else.

---

## 9. Findings surfaced while reading, not part of this increment

1. **The presumed surface was wrong.** `where_left_off` is the documented *wrong* session-
   start tool for exactly the deployment class that motivated this work (`guide.go:49, :82,
   :284`; `handlers.go:606, :1225`). Recorded because the same presumption will recur.
2. **`muninn_status` reports `Health: "good"` as a hardcoded literal** —
   `internal/mcp/handlers.go:791`. It is not derived from anything. An agent asking "is this
   vault healthy" gets a constant. That is the principle-#2 silently-wrong class on a
   surface whose entire purpose is to report state. Not fixed here (scope), flagged as its
   own increment.
3. **Every contradiction-awareness path in the product is result-set-scoped or opt-in.**
   The table in §1 is the complete census. Worth keeping: the *pattern* is that COG-29's
   penalty is unconditional while all of its notices are conditional.

---

## 10. Invariant and obligation impacts

- **COG-29 — amendment** (not a new invariant; this adds no new rule about what a
  contradiction *is* or *does to scoring*). Proposed text:

  > *Amendment (debt readout).* The same unresolved-declared set is also reported
  > **vault-wide** on the MCP orientation surfaces (`muninn_guide`,
  > `muninn_where_left_off`, and `muninn_recall` with `mode="recent"`) as an additive
  > top-level `unresolved_contradictions` block, because the 10% demote is unconditional
  > while every pre-existing notice was conditional on one of the two members being
  > retrieved — a declared conflict on an un-queried topic was penalised silently. The
  > readout derives from `GetContradictionReport` and therefore from
  > `markResolvedContradictions` — the SAME liveness-and-resolution rule recall applies, so
  > a third definition of "unresolved" cannot appear. **DECLARED pairs only**: a detected-
  > only pair is excluded, both by the asserted/inferred boundary and because COG-23's
  > un-migrated fabricated 0x0A markers are mechanically indistinguishable from genuine
  > ones, so counting them would nag upgraded vaults about conflicts that never existed.
  > The block is gated by `vaultMayHaveContradictions`, writes nothing (COG-11), changes no
  > score, order or row membership, is absent (not empty) at zero debt, reports the TRUE
  > count while showing at most `debtPairsShown` pairs oldest-first, and uses **no age
  > threshold** — age is reported, never a trigger (principle #11).

- **COG-11** — compliance only. Read-only path; no marker write, no `TouchAccess`.
- **COG-22 / visibility** — deviation named in D5/R3; matches `muninn_contradictions`, not
  recall.
- **Obligation #1** (MCP handler) — three handlers change, no new tool. Re-run the
  `allMCPTools` registry-parity smoke test; no amendment to it, no SEC-6 classification
  change.
- **Obligation #3** (REST/SDK parity) — deliberately not taken (deferral 2). This is a
  response-level block on three MCP handlers, not a field on `mbp.ActivationItem`, so the
  "add the whole annotation block or nothing" rule does not bind. Named for the reviewer.
- **Storage** — none. No Pebble prefix, no value-format change, no migration. **Not Tier 3.**
- **CI** — unit tests only, estimated `< 15s` on a ~6-7 min baseline. Two benchmarks, one
  gated, one recorded.

---

## 11. Panel outcome (2026-08-10) — D4 and deferral 2 RESOLVED

Three blind panelists (default-on advocate on Opus; opt-in advocate and third-shape on
Fable), judged against the pre-registered rule in
`2026-08-10-contradiction-debt-panel-prereg.md`. All citations below re-verified by the
judge against origin/develop @ fc16e78.

### Q1 decision: per-vault plasticity bool, default TRUE. Env flag rejected. Pure default-on rejected in its designed form.

Ship `ContradictionDebt *bool` on `PlasticityConfig` — non-preset-varying, resolved
default-true, per-vault overridable: the exact `ReinforceOnRead` idiom
(`internal/auth/plasticity.go:98-103`, resolution comment "not preset-varying: default
true across every preset", override at `:620-622`). The three attachment sites already
fetch plasticity for `scope_note`/MultiUser, so the bool rides a fetch the block performs
anyway. Add a #713-style pin test (nil config resolves byte-identical to today). Obligation
#4 is not triggered (field is non-preset-varying; the parity test pins preset names only).

**Conditions folded in from the default-on advocate's own concession:** Stage-A benchmark
A6 (`BenchmarkContradictionDebt_WithDebt`) is promoted from test to MERGE GATE; if the
with-debt derivation exceeds the R5 line, the cache (§6 deferral 3) is promoted from
deferral to blocker. The storage layer's own comment (`association.go:1821-1827`: the 0x0A
scan is "a deliberate, low-frequency call, not part of recall") is the reason this gate is
not optional — the design attaches that scan to recall.

**Why not the env flag (0/3 pre-registered criteria):** no strict-parsing consumer exists
anywhere in-tree (zero `DisallowUnknownFields`, no `outputSchema`, permissive SDK decoders
— verified independently by BOTH advocates); the COG-23 0x0A-leakage attack is refuted in
code (declared-only filter reads the 0x03 keyspace via `DeclaredContradictions`, never 0x0A;
`query.go:288/300/307`, `association.go:1875` — the opt-in advocate conceded this against
its own side); and no documented rationale for prospective's flag exists that transfers
(prospective writes fired-markers and infers focality; this block writes nothing and
infers nothing — its vocabulary is edges the caller declared).

**Why not D4's own default-on (correction to §1 and D4(a)):** the opt-in panelist REFUTED
the strong form of the receipt argument, and the judge confirms it. The recurring 10%
demote is NOT unconditional: `applyContradictionHonesty` mutates `out[i].Score` over the
retrieved result window only (`engine_contradiction.go` Step 6) — a declared pair on a
never-queried topic (the motivating incident) is charged nothing recurring. The only
unconditional charge is the ONE-TIME async confidence penalty. What survives is narrower:
the demote applies to the pre-truncation window while `pruneConflictBlock`
(`engine.go:3204` region) prunes the receipt to the post-truncation received set, and
COG-29's accepted residual ("visible, recoverable") claims a visibility this block is what
actually delivers. §1's sentence "the penalty is unconditional and the notice is
conditional" must be amended to this verified form before build.

**Why the third shape wins:** three independent runs converged on the discriminating fact —
Stage B branch 3 (INCONCLUSIVE) already pre-commits "keep behind an opt-in flag", so the
off-state machinery is scheduled to exist in one branch regardless. Building it as the
per-vault bool now makes every Stage B branch a config-default flip with zero wire churn
(SHIPS → stays true; INCONCLUSIVE → default flips false; KILLED → attachments removed),
gives Stage B a live control arm, and puts the control at the grain principle #11 sanctions
(R4's two vault shapes — 40-open-conflicts-normal vs 1-conflict-is-the-incident — cannot
share one process-global answer). Judge's honesty note: the prereg's "strictly less
mechanism than an env flag" clause required judgment — more lines of code, strictly fewer
new concepts (third instance of an existing idiom vs a new process-global control point).
Residual accepted: an operator can now deliberately run demote-on/receipt-off per vault;
a deliberate, explicit config choice honored is principle #1, not silent suppression.

### Q2 decision: MBP parity COMMITTED as increment 2, scoped to the Activate surface, plus the Stat count field. "Out of scope" rejected on refuted premises.

Deferral 2's rationale was factually wrong on both premises: obligation #3's parity list
explicitly carries "the RESPONSE-level `conflict` block" since #764
(`drift-and-obligations.md`, obligation #3 body), and COG-29's invariant fixes surface
scope as "MCP + MBP + REST (alias)" with reasoned exclusions naming only gRPC and non-Go
SDKs (`invariants.md`, COG-29). An MCP-only COG-29 amendment silently narrows the invariant
it amends. The wire already carries the trigger: `mbp.ActivateRequest.Mode` exists
(`mbp/types.go:205`) and MCP forwards `mode` verbatim, so an MBP/REST caller issuing the
identical orientation call would get the demote-window without the receipt — the exact
divergence this increment closes, relocated.

**Increment 2 scope (committed):** the debt block as an additive omitempty field on
`mbp.ActivateResponse`, set inside `activateCore` when `Mode == "recent"` — reaches MBP and
REST-by-alias in one seam; #764's `Conflict *ConflictBlock` is the precedent including
no-version-bump additive practice. PLUS the third-shape panelist's companion: count-only
`unresolved_contradictions` {count, oldest_age_hours, scan_complete} on `mbp.StatResponse`
/ `muninn_status` — pull-only (structurally #609-immune), no concepts exposed, precedented
by `CoherenceScores` already carrying derived cognitive state on Stat
(`mbp/types.go:625`), and it is the exact component §7's KILL branch pre-declares as the
survivor ("keep `ContradictionDebt` only if `muninn_status` adopts it"). Obligation #3's
manual SDK sweep is acknowledged as increment 2's real cost. NO new MBP ops:
`where_left_off`/`guide` do not exist as MBP frames and REST's guide is a separately
drifted stub — guide-surface parity is out of scope on its own merits.

**Builder note (from the default-on advocate's C11):** increment 1 hand-builds the block
into MCP's `map[string]any`; when increment 2 lands the shared-struct field, the MCP
handler must read FROM that field rather than keeping a second construction site — the
drift doc names the dual-source trap explicitly.

**Honest concession preserved:** no in-tree SDK or MBP client performs an orientation-
shaped call today (python `activate()` exposes no `mode`; the only in-tree MBP client is
cluster wiring). The commitment rests on the verified obligation/invariant scope, not on a
live consumer. If Stage B kills the push, the Activate-surface field reverts with the MCP
attachments; the Stat field survives.

### Losing arguments preserved (strongest points intact)

- **Env-flag opt-in (Fable):** the debt block is the product's first task-blind push —
  its own RED test A1 makes delivery-on-an-unrelated-query the success condition — while
  COG-21's invariant sentence reads "no task-blind push exists to misfire", and
  prospective shipped opt-in with strictly STRONGER delivery gating (focality, IDF floor,
  caps, dedup). #609 is do-not-reopen, and the design's new evidence is n=1 against its
  own n≥12 floor. If the field measurement ever shows orientation-time debt gets ignored
  like #609's pushes were, this argument was right and the default flips to false — the
  plasticity bool makes that a one-line change.
- **Pure default-on (Opus):** default-on is the project's norm for presentation honesty —
  every COG-25/28/29/30 signal ships un-flagged — and the only two behavioural env opt-ins
  in the MCP server gate authority-minting and state-writing, neither of which this block
  does. The per-vault bool concedes a suppression channel that pure default-on would not
  have had.
- **Out-of-scope MBP (Fable):** committing wire schema ahead of Stage B commits the most
  expensive-to-revert artifact to the least-validated idea, and "MBP parity" via the alias
  is really REST-plus-six-SDK parity with no automated check. This cost argument was
  weighed and accepted as increment 2's price; it lost only because the deferral's stated
  premises were refuted in the project's own doctrine documents.
