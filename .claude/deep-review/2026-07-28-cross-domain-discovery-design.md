# Cross-domain connection discovery — `muninn_discover` (DESIGN, not built)

The "memory that REALIZES" increment: surface non-obvious connections across domains that no
single document states ("every time the news reports weather above 70, flower stocks go up").
Grounded on develop @ 24f3b46. Design only.

Stance, non-negotiable: **Muninn surfaces co-occurrence CANDIDATES with their full evidence
(support, lift, permutation p, FDR q) — never causation.** A candidate without its denominator
is a plausible-wrong answer, the project's worst failure class (principles #1/#2). And
**discovery is a read-only layer** — it must never feed what it reads (no self-confirming loop).

---

## 1. Ground truth — what substrate exists (all verified in code)

| Piece | Where | What it gives discovery |
|---|---|---|
| Global entity registry, 0x1F | `internal/storage/entity.go` `EntityRecord{Name, Type, Confidence, FirstSeen, MentionCount, State}` | Domain partitioning by `Type` (14 valid types incl. `organization`, `event`, `concept`, `other` — `internal/mcp/handlers.go:1521`); popularity denominator via `MentionCount` |
| Engram→entity forward index, 0x20 | `WriteEntityEngramLink`; key `0x20|ws|engramID|nameHash` → raw name | **One vault-scoped Pebble range scan yields every (engram, entity) link.** This is the discovery scan's spine. |
| Entity→engram reverse index, 0x23 | `ScanEntityEngrams` | Per-entity event enumeration (what `entity_timeline` uses) |
| Same-engram co-occurrence, 0x24 | `IncrementEntityCoOccurrence` / `ScanEntityClusters` → `muninn_entity_clusters` | **Within-document co-mention counts only. No time axis, no denominator, no cross-document reach.** Honest finding: today's "clusters" cannot express "news on day T, stock move on day T+1" — the two facts are different engrams, so 0x24 never sees the pair. This is exactly the gap. |
| Timestamps | `Engram.CreatedAt` + valid-time `ValidFrom/ValidUntil` (`internal/storage/types.go:82-88`); `valid_from` settable on remember (`handlers.go:29-38`); `EffectiveValidFrom()` falls back to CreatedAt | Event time. **Use `EffectiveValidFrom`, not CreatedAt** — "soak up a year of news" means backdated ingest; CreatedAt would collapse a year of history onto ingest day and destroy every temporal signal. (Side note: `engine_entity_timeline.go:80` sorts by CreatedAt, not EffectiveValidFrom — pre-existing drift worth its own small fix.) |
| Cheap per-engram meta | `EngramMeta` (0x02, 100B) carries CreatedAt/ValidFrom/State — no content/embedding load needed | The time lookup per distinct engram costs a meta read, not a full engram read |
| Tag→engram indexes, 0x0C + 0x2C | keyspace-registry.md; 0x2C ordered raw-tag-range (S1) | Domain membership by tag (`domain:weather`) = a range scan returning engram IDs, no full-engram reads |
| `as_of` / COG-19 | `activation.PassesValidity`, `engine.go:2169` | Window bounding; the time-travel semantics discovery must respect |
| Hebbian co-activation | `internal/cognitive/hebbian.go` — recall-time association strengthening, log-space, capped (COG-17) | **The thing discovery must NOT write to.** Hebbian learns from co-*retrieval*; discovery reads stored valid-time structure. Different signals; if discovery fed Hebbian, every surfaced correlation would strengthen its own edge and be "rediscovered" more strongly — the self-confirming loop. |
| entityIDF | `engine_entity_boost.go:55-86` (df/n, ubiquitous → ~0) | Precedent for popularity normalization; discovery's lift denominator is the same idea in temporal form |
| Synthetic labeled-vault tests | `internal/consolidation/synthetic_vault_test.go` | Precedent for the planted-signal acceptance harness |

**What's missing (and what increment 1 does about it):**
1. *No temporal entity-event structure.* Nothing indexes (entity, day). Increment 1 derives it
   on the fly: one 0x20 range scan + one meta read per distinct engram → in-memory
   `map[entity]set[dayBucket]`. No new prefix. If vaults outgrow the on-demand scan (>~100k
   engrams), a persisted event index becomes a *future* increment with its own prefix — named,
   deferred.
2. *Events are binary, not numeric.* Engrams are atomic facts; "FLWR up 4%" has no structured
   value. Increment 1 discovers **presence/presence lift** between entities. The vision's
   example still works because atomic-fact ingest naturally makes direction an entity or a
   key:value tag ("flower stocks rose" → entity `flower-stock-rally` or tag `move:up`).
   Numeric co-movement (real-valued series) is increment 2+.

Conclusion: **the substrate CAN support a measurable cross-domain slice today.** The only hard
prerequisite already landed: valid-time (`valid_from` on remember). No blocker.

---

## 2. The mechanism — new read-only MCP tool `muninn_discover`

### Why an MCP tool (vs the alternatives)
- **Not a dream/consolidation pass:** dream writes (dedup, transitive edges). Housing discovery
  there puts a read-only analytic inside a write pass — structurally inviting the loop we
  forbid — and gives no on-demand UX.
- **Not pure LLM-over-`entity_timeline`:** timeline caps at 50 entries, has no denominators,
  and an LLM cannot honestly run a 500-shuffle permutation null or FDR over thousands of pairs.
  That path *is* the correlation-hallucinator.
- **Not an offline CLI:** the sentient feel lives where the agent lives — MCP. A CLI twin can
  wrap the same engine method later for free.
- Precedent: read-only analytic engine methods surfaced as MCP tools already exist
  (`GetEntityClusters`, `GetEntityTimeline`). `muninn_discover` extends that proven shape
  (principle #7), with real statistics behind it.

### Request
```
muninn_discover {
  vault,
  domain_a: { entity_type: "event" } | { tag: "domain:weather" },
  domain_b: { entity_type: "organization" } | { tag: "domain:markets" },
  bucket: "day" (only value in inc 1),
  max_lag: 0..7 (default 3),        // ℓ: A leads B by ℓ buckets
  window: { from?, to? }             // default: overlap of the two domains' active spans
  min_support: >=5 (floor, not lowerable below 3),
  top_k: default 10 (max 25)
}
```
Domain by entity type reads one 0x1F record per distinct entity; domain by tag resolves engram
membership via the 0x0C/0x2C tag index range scan. Both avoid full-engram loads.

### Computation (all in-memory, single pass over indexes)
1. Scan 0x20 for the vault → (engramID, entity) pairs. Meta-read each distinct engram once
   (0x02, metaCache): skip soft-deleted/archived (COG-9 pattern); event time =
   `EffectiveValidFrom` day-bucket. A closed ValidUntil does NOT remove the event — an
   invalidated fact was still true at its valid time; discovery is over event history.
2. Build per-entity day-sets `S_e` (presence, deduped per day — mention-count inflation can't
   game support). Assign entities to domain A/B; drop entities in both (self-pairs are not
   cross-domain). Cap each domain at the 200 entities with the most distinct-day events inside
   the window (distinct-day, not MentionCount, so bursty spam doesn't buy a seat).
3. For each pair (a,b) and lag ℓ∈[0..max_lag] over window of T buckets:
   - support `k = |{t : t ∈ S_a ∧ (t+ℓ) ∈ S_b}|`
   - lift `= k·T / (n_a · n_b)` where `n_a=|S_a∩window|`, `n_b=|S_b∩window|`
   - Skip if `k < min_support`.
4. **Null model — circular-shift permutation** (the non-gameable gate): N=500 draws; each draw
   circularly shifts `S_b` by a uniform random offset in [1, T-1] and recomputes lift.
   `p = (1 + #{null_lift ≥ observed_lift}) / (N + 1)`. Circular shift, not IID shuffle,
   deliberately: it preserves each series' burstiness/autocorrelation and destroys only the
   alignment — an IID shuffle of bursty series yields anti-conservative p-values (a classic
   way an implementation games its own gate).
5. **Multiple comparisons:** Benjamini–Hochberg FDR across ALL (pair, lag) tests that met the
   support floor (not just the survivors — that would be p-hacking). Default report gate
   q ≤ 0.05.
6. Rank survivors by `lift × log(1+k)`, return top_k.

### Response — the evidence contract (every field required, never omitted)
```
{ candidates: [ {
    entity_a, entity_b, lag_days,
    support, lift, p_value, q_value,
    n_a, n_b, window_days,
    example_engrams: { a: [≤3 ids], b: [≤3 ids] },   // auditability: the receipts
    statement: "X and Y co-occur at lag 1 (lift 3.2, support 61/90, p<0.002, q=0.01)"
  } ],
  tested_pairs, window: {from,to,days}, dropped: {below_support: n, fdr_rejected: n} }
```
Wording is always "co-occur at lag ℓ" — the word *cause* appears nowhere in tool output, tool
description, or guide text. When the window or support can't clear the floors, return an empty
candidate list **with the reason** ("window 12 days < 30 minimum for day buckets") — degrade
loudly, never lower the bar silently.

### Increment 1 computes vs defers
Computes: two domains, day buckets, presence lift, lags 0..7, circular-shift null, BH-FDR,
evidence contract, entity-type + tag domain selectors.
Defers (named): numeric co-movement; >2 domains; predicate events (entity ∧ tag, e.g.
`FLWR ∧ move:up`); hour/week buckets; persisted event index (new prefix) for very large vaults;
result caching; confound annotation; CLI twin; the Push bridge (§5).

---

## 3. The measurable proof — planted-signal vault + shuffle-RED (the flagship gate)

Synthetic-but-realistic two-domain vault, precedent `internal/consolidation/synthetic_vault_test.go`.
Seeded via the real write path (`Engine.Write` with `valid_from` backdating), not storage pokes.

**Construction (365 daily buckets, seeded RNG, labeled):**
- *Weather domain* (`domain:weather` tags; entity type `event`): one fact/day. ~90 days carry
  entity `hot-day` ("temperature above 70..."); plus a near-daily distractor entity
  `daily-weather-summary` on ~350 days.
- *Markets domain* (`domain:markets`; type `organization`): entity `FLWR-rally` ("flower
  stocks rose") on the day AFTER 80% of hot days (planted lag-1 signal) plus a 10% base rate
  on non-hot days ⇒ n_b ≈ 100. Plus ~30 null market entities on independent random days
  (20–120 days each). Plus a near-daily distractor `market-report` on ~340 days.
- The two distractors are the **popularity artifact**: their raw co-occurrence count (~330)
  is the largest in the vault — any frequency-ranked method surfaces them first.

**Expected numbers** (analytic, then pinned with tolerance bands in the test):
planted pair at lag 1: k ≈ 72+27 ≈ 80–105... conservatively `k ≥ 60`, lift ≈
`k·365/(90·100)` ≈ **3.0–3.6**; distractor pair: k ≈ 326, lift ≈ `326·365/(350·340)` ≈ **1.00**.

**The four assertions (all must hold; each is a hard failure):**
1. **Detection:** `hot-day → FLWR-rally` at lag 1 is in the top-3, with lift in [2.5, 4.0],
   support ≥ 60, p < 0.01, q < 0.05 — the *correct* evidence, not just presence.
2. **Popularity rejection:** the `daily-weather-summary × market-report` pair — despite the
   vault's highest raw co-occurrence — is NOT in the candidates: its lift ≈ 1 and its
   permutation p > 0.1 (its score is already indistinguishable from its own null — shuffling
   changes nothing for a frequency artifact, which is precisely why the null rejects it).
3. **Null-pair silence:** zero of the 30 independent null market entities appear at q ≤ 0.05
   (FDR is doing its job across ~30×lags×(weather entities) tests).
4. **RED / non-gameable half:** rebuild the identical vault but with the market facts'
   `valid_from` dates randomly permuted before ingest (same content, same counts, destroyed
   alignment). The planted pair must now report lift ≈ 1.0 ± 0.3 and p > 0.05 and be absent
   from candidates. A "detector" that pattern-matches the entities, hardcodes the answer, or
   ranks by frequency passes 1 and fails 4 (or passes 4's shuffled run and fails 2). The
   pairing of 1+4 with 2 is what fluff cannot pass: the signal must live in the *timestamps*,
   and only in the timestamps.

Placement/cost: statistics functions (lift, circular-shift null, BH) get table-driven unit
tests over in-memory series (free). The planted-vault test runs once against a real Pebble
store with N=200 null draws — one integration test, well inside the CI budget (§3 of
CLAUDE.md); no Playwright, no -race requirement (read-only, no shared mutable state).

---

## 4. Invariants + drift obligations

**Propose COG-21 (discovery is inert and evidenced):**
> Discovery (`muninn_discover` / `Engine.Discover`) performs **zero writes**: no association
> weights, no engram/entity state, no 0x24 counters, no TouchAccess, no activation/Hebbian/PAS
> events — enforced *structurally* by implementing it against a narrow read-only store
> interface (scan/get methods only; principle #3 — the bad state is unrepresentable), and
> pinned by a test running Discover against a store wrapper that fails on any mutating call.
> Every surfaced candidate carries support, lift, permutation p, FDR q, both marginals, and
> the window — a candidate without its denominator must be impossible to emit (the response
> type has no optional evidence fields). Output language asserts co-occurrence, never causation.

Relation to existing invariants: composes with COG-11 (reads don't mutate learning state —
discovery is the strictest instance: not even access metadata), COG-10 (never touches
confidence), COG-17/16 (Hebbian stays recall-fed only).

**Keyspace:** pure read — no new prefix; obligation #8 not triggered. (The persisted event
index, if ever needed, is a future increment that would trigger it.)

**drift-and-obligations impacts:** obligation #1 — new MCP tool ⇒ add to `allMCPTools` in
`cmd/muninn/smoke_exhaustive_test.go`, classify in `isReadOnlyTool` (SEC-6 census), tool schema
in `tools.go`, guide text in `guide.go` (which must itself state the candidates-not-causation
framing). No REST/proto/SDK surface in increment 1 (deferred ⇒ obligations #2/#3/#7 untouched).
Not privileged (SEC-2 n/a). No cluster write path (SEC-13 n/a).

**Tier: confirmed Tier-2.** Read-only analytic; no keyspace prefix; no write path; no
auth/transport surface beyond a standard read-only tool registration.

---

## 5. Composition with what's landed
- **Valid-time is the load-bearing dependency:** `EffectiveValidFrom` is the event clock, so a
  year of backdated news aligns correctly; `window.to` behaves like `as_of` (bound the history
  you analyze). Invalidated facts still count as events at their valid time — event history vs
  current truth, cleanly separated by COG-19's own semantics.
- **Entity graph:** domains partition over 0x1F `Type` or tag namespaces; `example_engrams`
  hand off to `muninn_read`/`muninn_entity_timeline` for the human audit trail;
  `muninn_entity_clusters` remains the within-document view, `muninn_discover` the
  across-document/temporal view — complementary, documented as such in the guide.
- **The Push (future bridge, explicitly NOT this increment):** a candidate that survives the
  null AND is user-confirmed could later arm an intention ("when a hot-day fact arrives,
  surface the FLWR connection") — discovery stays read-only; the *user's confirmation* is the
  write, via the Push's own tools. One sentence in the decision record, nothing more now.

## 6. Top risks / mistakes not to reproduce
1. **Multiple comparisons / p-hacking** (millions of pairs ⇒ garbage "hits"): support floor,
   200-entity domain caps, small default lag range, BH-FDR over *all* tests performed, q
   reported per candidate. Never FDR only the survivors.
2. **Anti-conservative null via IID shuffle** of autocorrelated series: circular-shift null is
   the design choice, with a unit test showing a bursty-but-independent pair gets p>0.05 under
   circular shift where IID shuffling would (wrongly) call it significant.
3. **Self-confirming loop:** COG-21's structural read-only interface. This is why "just log a
   discovered edge into 0x03/0x24" is banned even as an optimization.
4. **Popularity/survivorship bias:** lift's marginals + the null jointly reject
   globally-common pairs — proof assertion 2 pins it forever.
5. **Common cause / seasonality** (weekday drives both series): the signal is *real
   co-occurrence* and legitimately survives the shift null — which is exactly why output
   language never claims causation. Confound annotation (e.g. day-of-week partial) is a named
   deferral, not a silent gap.
6. **Silent degradation on thin data:** short windows/sparse domains return an empty list
   *with the stated reason*, never silently relaxed floors (principle #2).

## 7. Increment-1 checklist (for the BUILD step)
`Engine.Discover` over a read-only store view + stats pkg (lift/circular-null/BH) with unit
tests → planted-vault integration test with the 4 assertions (RED half = shuffled-vault run)
→ MCP `muninn_discover` (tools.go schema, isReadOnlyTool, allMCPTools smoke, guide text) →
COG-21 added to invariants.md + pinning test → decision-record entry naming the deferrals.
