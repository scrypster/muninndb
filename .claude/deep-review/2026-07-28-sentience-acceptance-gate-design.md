# DESIGN — The Sentience Acceptance Gate (capstone)

Status: DESIGN ONLY (increment-loop DESIGN step). No production code.
Grounded in HEAD of origin/develop = `10ec929` (verified in a detached worktree; every
capability cited below was read in code at that commit, not assumed).

Owner's bar, verbatim:

> "AI should FEEL sentient. It should just KNOW. If I have to say 'remember when Dana
> said X,' it has failed. It should feel like a colleague who's been in every meeting."

This gate turns that sentence into one honest, reproducible, non-gameable measurement —
with a control that smoke-and-mirrors cannot pass, and an explicit statement of what the
number does NOT prove.

---

## 0. Verified capability inventory (what the gate is allowed to measure)

| Capability | Where (verified at 10ec929) | Gate axis |
|---|---|---|
| THE PUSH: `muninn_intend` arms intentions on entity cues; `notices` piggyback on recall/remember when a cue is FOCAL (top result or ≥2 corroborating results); cap 2/response; session dedup; one-shot markers; IDF ubiquity floor refuses hub-entity cues; #693 self-focality guard | `internal/engine/prospective.go`, `internal/mcp/prospective.go`, `internal/mcp/handlers.go:199,575` | A1, A3 |
| Push acceptance harness already in-tree: Gates 1–4 (precision ≥0.90, recall ≥0.80, 35 unrelated → zero notices, RED arm), 60-call scripted session, ~200-memory seeded vault | `internal/engine/prospective_harness_test.go` + `testdata/prospective_session.json` | pattern to extend |
| Supersedes-aware recall: demote-only ranking (stale ≤ head−ε), head injection into the candidate pool (#607 precedent), always-on `superseded_by`/`current_version` annotation | `internal/engine/engine_supersession.go`, `internal/mcp/handlers.go:565,2099` | A2 |
| Valid-time: `as_of`, `include_invalid`, `not_true_since` invalidation (COG-19 — default recall stops returning invalidated facts, history stays reachable) | `internal/mcp/handlers.go:467–486,629–650`, `guide.go:188–189` | A2 |
| Honest, deterministic recall: ULID tie-break at equal scores (#698/#699), RRF R1 threshold honesty (#705), no fabricated contradictions (contradiction notices intersect only durable 0x0A pairs — zero new detection) | `e9cd4e1`, `10ec929`, `prospective.go:412–456` | preconditions for reproducibility |
| Continuity surfaces: `WhereLeftOff` (no reinforcement on read), recall `mode=recent` | `internal/engine/engine_where_left_off.go:18` | A4 |
| Entity graph + associative traversal, entity boost (rarity-weighted, #570) | `engine_entity_boost.go` | substrate for A1 focality |

**Explicitly NOT in scope** (and the gate must not pretend otherwise):
- Cross-domain insight/discovery — HELD, honest-negative #706. No axis measures it.
- Ambient/background push — rejected by design (#609: task-blind push got zero uptake;
  there is no scheduler; time SILENCES intentions, never fires them). "Unprompted" in
  this gate means *within a tool call the agent made*, on a cue it did not name.
- Decay/Ebbinghaus over real weeks — the scenario compresses "six weeks" into one
  process; no clock injection exists in the harness path. Deferred (§5).

---

## 1. Operational definition — the Sentient-Feel Score (SFS)

"Feels like a colleague who's been in every meeting" decomposes into four measurable
behaviors, each mapped to a landed mechanism and a number:

- **A1 — UNPROMPTED SURFACING** ("it just knows"). When the conversation *becomes about*
  a cue entity, the right memory appears in `notices` even though no query named it.
  Mechanism: the Push. Metric: **colleague-moment hit rate** = planted moments that
  surface unprompted / planted moments, and **notice precision** = fired-and-wanted /
  fired.
- **A2 — CURRENCY** ("it knows what's true NOW"). When a fact has evolved, been
  superseded, or been invalidated, the *current* version leads recall and the stale
  version is annotated — even when the query's phrasing lexically matches the stale
  text. Mechanism: supersession ranking + evolve soft-delete + valid-time. Metric:
  **currency win rate** = topics where the top-ranked answer is the current fact, and
  **annotation completeness** = stale results carrying `superseded_by`.
- **A3 — NON-INTRUSION** ("a colleague, not a nag"). When nothing relevant applies, it
  is silent. Mechanism: the focality rule, the 2-notice cap, the IDF ubiquity floor,
  session dedup. Metric: **false-surfacing count** on unrelated and cue-adjacent trap
  calls (budget: zero — same bar as the existing Gate 3).
- **A4 — CONTINUITY** ("been in every meeting"). After a session break, it picks up the
  open thread without being reminded which thread it was. Mechanism: `WhereLeftOff` /
  `mode=recent`. Metric: **thread pickup rate** = probes where the genuinely-open item
  ranks in the top-3 of the orientation call.

**Headline number:** SFS is reported as the 4-tuple, plus a composite defined as the
**minimum** of the four normalized axis scores. Minimum, not average, deliberately: a
system that dumps everything maxes A1 and zeros A3 → composite 0. Averaging is exactly
the gameable aggregation this gate exists to refuse.

**Honest ceiling statement (say it up front):** with cross-domain insight held (#706),
the measurable sentient-feel at HEAD is *the Push's unprompted surfacing + currency
awareness + disciplined silence + thread continuity*. That is what a passing SFS claims
— no more. It is a large fraction of "colleague who was in every meeting"; it is not
"connects things you never connected," and the gate must never be advertised as
measuring that.

---

## 2. Scenario spec — "Six weeks on the Nordlys project"

One scripted, deterministic, seeded-vault scenario replayed by a Go harness (extends the
proven `prospective_harness_test.go` pattern: seed → arm → scripted calls, each call a
fresh session-dedup map = a separate agent session; durable one-shot markers persist
across calls, exactly the production semantics).

**Fixture:** `testdata/sentience_scenario.json`. ~250 memories over 12 simulated
sessions of a realistic software project: teammates (Dana, Priya, Marcus), components
(billing-service, auth-gateway, the Kafka migration), decisions, an investor/runway
thread, a deploy freeze, plus ~140 generated filler memories so the vault is big enough
for the ubiquity floor and real ranking negatives (same technique as the existing
harness). Not toy — every planted moment is written as something that actually happens
on a project.

**Plants (the measured events):**

1. **12 colleague moments (A1).** Each = one armed intention (via the real `Intend`
   path, IDF floor live) + one eliciting call later in the script. Example: session 2
   arms "when the billing-service migration comes up, flag that a teammate found the Stripe
   idempotency-key bug in the old retry path" (cue: `billing-service migration`).
   Session 9's eliciting call is *about* the migration — e.g. recall context "planning
   the cutover order for the billing move" — and **never contains the intention's
   content words** ("Dana", "idempotency", "retry"). If the note surfaces, the system
   "just knew"; the user never said "remember when so-and-so said X." Mix: 8 one-shot,
   4 recurring; importance spread so the 2-notice cap is exercised at least twice
   (two moments deliberately share one eliciting call).
2. **8 stale/current fact pairs (A2)**, split across all three currency mechanisms:
   3 supersedes-link pairs (new fact + `RelSupersedes` link, predecessor stays active —
   the exact gap `engine_supersession.go` closes), 3 evolve chains (predecessor
   soft-deleted), 2 valid-time invalidations (`not_true_since`, e.g. the deploy freeze
   lifted). The canonical one is the runway figure: "runway 8 months" (week 1) →
   "bridge extended runway to 11 months" (week 4). Each pair gets TWO probes: one
   neutral ("what's our current runway") and one **stale-phrased** ("runway 8 months
   figure" — lexically closest to the stale text). Plus one `as_of` probe per
   mechanism verifying "what was true then" still answers with the old fact.
3. **30 silence calls (A3).** 20 fully unrelated (other project areas, filler topics),
   10 cue-adjacent traps: stem collisions ("billing address typo in the invoice
   template" vs the `billing-service` cue), hub-entity mentions, and one call whose
   results include an intention's own engram (the #693 regression shape). Expected
   notices across all 30: **exactly zero**.
4. **6 continuity probes (A4).** At 6 of the 12 session boundaries, the script leaves a
   thread demonstrably open (a decision recorded as pending, a task mid-flight), then
   the next session opens with a `WhereLeftOff` / `mode=recent` orientation call. Hit =
   the open thread is in the top-3. Two probes are made harder: an *older, closed*
   thread on the same entity exists, and picking it instead is a miss.

**Held-out phrasing sets (anti-memorization):** every eliciting call and currency probe
exists in two disjoint phrasings, set A and set B, written at design time. The harness
runs one set per invocation (selectable); the standing rule (enforced in the /increment
refute step, documented in the test header) is that any PR touching focality, scoring,
supersession, or the notices path must pass **both** sets. A change tuned to the fixture
wording fails the other set.

**Runtime:** v1 runs in-process against `testEnv` (engine-level, like the existing
harness) — no daemon. The same JSON fixture is designed to be replayable over MCP
against a labs daemon (own data dir + ports, `-tags localassets`,
`MUNINN_LOCAL_EMBED=1`, `MUNINN_PROSPECTIVE=1`) in v1.1 (§5). Never the live daemon.

---

## 3. Metric, pass bars, and the non-gameable controls

### Pass bars (system under test, Push ON)

| Axis | Metric | Bar | Rationale |
|---|---|---|---|
| A1 | colleague-moment hit rate | ≥ 10/12 (0.83) | Gate-2 lineage (0.80), rounded to the fixture size |
| A1 | notice precision | ≥ 0.90 | Gate-1 lineage; measured 1.0 on the existing harness |
| A2 | currency win rate (top result = current) | ≥ 15/16 probes (incl. all 8 stale-phrased) | demote-only + head injection makes this the *designed* behavior; one miss tolerated for ranking noise, zero tolerated on supersedes-link pairs |
| A2 | annotation completeness (`superseded_by` on stale results) | 8/8 | always-on annotation is landed; anything less is a regression |
| A2 | `as_of` correctness | 3/3 | bitemporality either works or it doesn't |
| A3 | notices on 30 silence calls | exactly 0 | Gate-3 lineage; the axis a dumper cannot fake |
| A4 | thread pickup | ≥ 5/6 in top-3 | weakest axis (see honesty note §4.5) |
| Composite | min(normalized axes) | ≥ 0.83 | min-aggregation; one failed axis fails the gate |

### The controls (the crux — what a non-sentient system scores)

- **C1 — Push-OFF baseline (the sentient increment).** Same engine, same vault, same
  script, prospective delivery disabled (the existing harness's `enabled=false` arm —
  Gate-4 generalized). Expected: A1 unprompted = **0/12** (structurally: notices are the
  only unprompted channel and it is off). The **sentient increment** Δ_push =
  A1_on − A1_off must be ≥ 0.83. This is the number that answers the owner directly:
  *how much "just knows" did the Push add over plain memory?* Arithmetic of a passing
  run: 10–12/12 vs 0/12 → Δ ≈ 0.83–1.0.
- **C2 — Explicit-query baseline (what plain search gets when ASKED).** For each of the
  12 moments, run plain recall with the eliciting call's text (Push OFF) and check
  whether the intention's content lands in the top-3. Because eliciting phrasings are
  held out from the content words, pure semantic/entity retrieval should find only a
  fraction — *predicted* 3–5/12 via entity overlap, but the gate **reports the measured
  number rather than asserting the prediction** (principle 9: verify, don't assume; if
  recall finds 9/12, that is honest good news that shrinks the Push's unique claim, and
  the report must say so). Required margin: A1_on − C2 ≥ 0.40. This control distinguishes
  "surfaces it unprompted" from "would have found it if you'd asked" — the owner's bar
  is precisely the difference.
- **C3 — Dump-everything strawman.** A degenerate policy scripted in the harness (no
  engine changes): attach every armed, valid intention to every response / recall with
  threshold 0. Expected: A1 = 12/12, A3 = 30/30 intrusions → composite **0**. Reported
  in the gate output as one line, permanently, so nobody ever "improves" A1 by loosening
  the focality rule without seeing what that curve ends at. The A1×A3 pair is the
  non-gameability core: precision-free surfacing maxes one and zeros the other, and
  min-aggregation refuses the trade.
- **C4 — Stale-phrased currency control.** Already built into the A2 probes: the query
  is lexically closest to the STALE text, so a system doing pure similarity ranks stale
  first. Only genuine supersession awareness (demote-only + head injection) puts current
  on top. A BM25/embedding-only baseline (supersession phase disabled is not togglable
  at HEAD — so v1 approximates this control analytically via the pre-supersession
  scores the RFC documented: stale 1.15 vs current 0.92 on the labs seed) — the fixture
  reproduces that shape and the gate asserts the *post*-supersession inversion.
- **C5 — Memorization guard.** Phrasing set B (§2) + the refute-step rule. A system (or
  a tuning PR) that memorizes set A fails set B.

### Why each smoke-and-mirrors strategy fails

| Strategy | Killed by |
|---|---|
| Dump everything / lower thresholds | A3 zero-budget + min composite (C3 shows the endpoint) |
| Arm intentions on everything | IDF ubiquity floor refuses hub cues at arming (verified: `prospective.go:112–128`); traps in A3; precision bar |
| Annotate but keep ranking stale first | A2 measures RANK, not annotation |
| Hard-code / memorize the fixture | held-out set B + both-sets rule |
| Fire on any entity mention (drop focality) | the 10 cue-adjacent traps + #693-shape call in A3 |
| Cherry-pick a lucky run | determinism preconditions (#698 tie-break, #705 R1) + fixed fixture → the run is reproducible; flakiness is itself a failure |

---

## 4. Honesty guards — how this gate could lie, and what prevents it

1. **The subjectivity gap (the big one).** A passing SFS means exactly: *"across K
   scripted project moments, the engine surfaced the right memory unprompted with
   precision ≥0.9, kept current facts ranked above superseded ones even under
   stale-phrased queries, stayed completely silent on 30 irrelevant exchanges, and
   picked up 5/6 open threads after session breaks."* It is a concrete, falsifiable
   proxy for the owner's sentence. It is **not** "the system is sentient," not "it will
   feel this way on your real vault," and not evidence of insight (#706 held). The
   gate's report template prints this sentence with the numbers filled in — the claim
   ships with its own boundary.
2. **Harness ≠ production drift.** v1 calls `eng.Activate`/`Intend` directly (the
   existing harness's honest shortcut). MCP-layer semantics (session dedup wiring,
   readOnly, the `MUNINN_PROSPECTIVE` gate itself, cap enforcement at the transport)
   are covered by `internal/mcp/prospective_test.go` — the gate's report must cite that
   split, and v1.1's MCP-replay closes it. Until Gate-5 (live shadow) runs, no claim
   about "feel" on the owner's real vault is permitted — only "feel" on the scenario.
3. **Author bias.** The same designer writes plants and probes, so probes can be
   unconsciously easy. Mitigation: the /increment REFUTE step must add ≥5 adversarial
   probes (new traps, new stale-phrasings) *after* the implementation freeze, and the
   gate must pass with them included. This is a process rule recorded in the test
   header, not a code mechanism — say so.
4. **Cold start.** A fresh vault scores SFS = n/a by construction: all four axes are
   properties of accumulated history, and the scenario spends its first sessions
   building ~250 memories before any probe runs. The gate refuses to produce a number
   on a vault below the seed size (mirrors `cueUbiquityMinVault` reasoning). A new
   user's first day *will not* feel like this gate's number, and the report says so.
5. **A4 is the weakest axis — admit it.** `WhereLeftOff` is recency-based retrieval the
   agent must still *call*; MCP server instructions make session-start orientation a
   convention, not a reflex. A4 measures "the thread is there when the standard
   orientation call is made," not spontaneous continuity. The report labels it
   accordingly rather than inflating it into "remembers on its own."
6. **Compressed time.** Weeks are simulated by write order, not clock time; decay,
   pruning, and base-level activation over real intervals are untested here. A system
   whose decay silently kills week-1 intentions by week 6 would PASS this gate and
   fail reality. Deferred explicitly (§5); Gate-5's two-week live window is the real
   test of that.
7. **Metric can go up for bad reasons.** Any future change that raises A1 by >0.05 must
   show A3 unchanged at zero and precision non-decreasing in the same run — the gate
   prints all axes on every run precisely so single-axis "improvements" are visible as
   the trades they are (severity can go up, not just down — principle 9).

---

## 5. Scope — minimal buildable v1, deferrals, and the road to Gate-5

**v1 (one increment, small PR):**
- `internal/engine/sentience_gate_test.go` + `testdata/sentience_scenario.json` —
  direct extension of the existing `prospective_harness_test.go` runner (reuse
  `testEnv`, the scripted-call loop, the fresh-dedup-per-call session model). One new
  helper for the A2/A4 probe types. No production code changes; the gate measures what
  shipped.
- Emits the SFS 4-tuple + composite + all control lines via `t.Logf`; fails on the §3
  bars. RED-sanity per repo rule: the C1 arm doubles as the RED proof (Push OFF must
  fail A1), and the fixture must include one probe demonstrated to fail if the
  supersession phase were absent (the RFC's documented 1.15-vs-0.92 shape).
- CI cost: unit-level, in-process, ~200–300 writes with local embeddings — the existing
  harness pattern is near-free; target <60s, well inside the ~10-minute budget. Runs
  with `-tags localassets` (obligation #9).
- Deliberately defers: MCP-replay mode, decay-over-time axis (needs injectable clock),
  multi-agent/shared-vault scenarios, any REST/gRPC parity probes, and any UI.

**v1.1 (labs replay):** a small runner under `cmd/` or a labs script that replays the
same JSON over MCP against a scratch daemon (own data dir + ports, `-tags localassets`,
`MUNINN_LOCAL_EMBED=1`, `MUNINN_PROSPECTIVE=1`). Closes the harness-vs-transport gap in
§4.2. Manual, not CI.

**Gate-5 (live shadow — the only claim about real "feel"):** once the owner enables
`MUNINN_PROSPECTIVE=1` on the real daemon, a two-week observation window: log every
notice delivered in real sessions (`memory_id`, cue, `why` — the Notice struct already
carries the audit trail), owner tags each as *useful / neutral / noise*, plus a weekly
"did it miss a moment a colleague would have caught?" note. Live precision ≥0.8 with
zero "nag" complaints is the acceptance; the SFS scenario number is the *predictor*,
the shadow is the *verdict*. Per the existing harness header, Gate-5 is a post-merge
activity, deliberately not a test.

**Honest bottom line:** with #706 held, what we can measure — and all we should claim —
is that MuninnDB at HEAD delivers the *push half* of sentient feel: it speaks up
unprompted at the right moment (Δ_push expected ≈ 0.83–1.0 over the same engine without
the Push), knows what is currently true even when asked in yesterday's words, and shuts
up otherwise. That is a real, controlled, falsifiable increment toward "a colleague in
every meeting" — and this gate is built so that nothing less than that behavior can
produce the number.
