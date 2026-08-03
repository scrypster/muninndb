# Time-normalized association decay — design for #762

**Date:** 2026-08-01
**Issue:** #762 (assoc decay grinds every edge to the floor within ~a day)
**Related:** #756 / #759 (full-weight repair), #757 (encoder fix), #760 (role-blind decay in clusters), dd640dc (importance)
**Code read at:** `/Users/mjbonanno/github.com/scrypster/.worktrees/prefix-weight-repair` @ `33f1230` (= `origin/develop`)
**Status:** design only. No code changed.

---

## 1. Diagnosis — verified, with two corrections to the brief

### 1.1 What the code actually does

- `internal/auth/plasticity.go:141` — `AssocDecayFactor float32 // multiplier per prune pass; 0 = disabled`.
  Preset values: `default` 0.95 (`:230`), `reference` 0.95 (`:256`), `working` 0.95 (`:337`),
  `knowledge-graph` 0.98 (`:308`), `scratchpad` 0 (`:282`, disabled — Hebbian is off there).
- `internal/engine/engine.go:4272` `runPruneWorker` — `timer := time.NewTimer(60*time.Second + jitter)`,
  jitter `rand.Intn(10)` seconds. Every tick calls `e.decayAllVaults` (`:4296`).
- `internal/engine/engine.go:4320` `decayAllVaults` — for each vault with
  `HebbianEnabled && AssocDecayFactor > 0` (COG-16), calls
  `store.DecayAssocWeights(ctx, ws, factor, minWeight, archiveThreshold)`. Gated behind
  `assocWeightRepairComplete()` (#759).
- `internal/storage/association.go:542` `DecayAssocWeights` — for every 0x03 edge:
  skip if `now − lastActivated < assocDecayGraceWindow` (5 min, `:529`);
  else `newW = oldW × decayFactor`; if `newW < minWeight` either archive (consolidation
  score > `archiveThreshold`) or clamp to `dynamicFloor = peakWeight × 0.05`.

**The unit is the pass, and the pass rate is an implementation detail of a worker that
predates decay.** Confirmed by history: `runPruneWorker` and its 60s cadence were
introduced by `e51b985` (2026-02-25) as an *engram* prune sweep (MaxEngrams/RetentionDays),
where 60s is a responsiveness choice. `79605b2` (2026-02-26, "Hebbian edge pruning and
activation snapshot isolation") bolted association decay into that same loop the next day
and only edited the doc comment:

```
- // MaxEngrams and RetentionDays policies. Runs every 60s with ±5s jitter to spread load.
+ // MaxEngrams, RetentionDays, and AssocDecayFactor policies. Runs every 60s with ±5s jitter.
```

So this is a units mismatch introduced by evolution, exactly as hypothesized: 0.95 was
chosen for a "pass" that inherited its period from an unrelated worker. Nobody ever wrote
down what a pass was supposed to mean.

### 1.2 The arithmetic

Mean pass interval 65 s (60 + U(0,10)).

| quantity | value |
|---|---|
| half-life in passes, f=0.95 | `ln0.5/ln0.95` = **13.51 passes** |
| half-life wall-clock | **≈ 14.6 minutes** |
| passes to 1.0 → 0.05 (floor for peak=1) | 58.4 passes ≈ **63 minutes** |
| passes/day | 1329 |
| one day of decay | `0.95^1329 ≈ 2.4e-30` |
| knowledge-graph (f=0.98) half-life | 34.3 passes ≈ **37 minutes** |

This matches the empirical observation in #762 (store-wide max weight ≈ 0.05 = the floor).

### 1.3 Can reinforcement outrun it? (#762 step 2, answered analytically)

Hebbian growth is `w ← min(1, w·(1+0.01)^S)` where `S = Σ(score_a·score_b)` over the batch
(`internal/cognitive/hebbian.go:260`, `HebbianLearningRate = 0.01`).

To exactly offset one 0.95 pass: `S = ln(1/0.95)/ln(1.01) = 5.16 signal units per 65 s`
= **6,850 signal units/day**. A realistic co-activation contributes `score_a·score_b ≈
0.1–0.3`. So holding one edge steady requires ~20–70 co-retrievals *per minute, forever*.
Reinforcement loses by roughly three orders of magnitude.

In practice the behaviour is not a curve at all but a **cliff**, because of the 5-minute
grace window: an edge co-activated at least once every 5 minutes is skipped entirely and
never decays; an edge that misses that window is at the floor within the hour. "Fades when
unused" is implemented as "fades unless used every five minutes."

### 1.4 Correction 1 — restarts are not a material contributor today

The brief hypothesized that frequent deploys cause *extra* compounding. They do not. The
worker's first tick is 60 s **after** start (no immediate pass), and `decayAllVaults` is
additionally parked until the #759 repair sweep completes cleanly. So each restart *forfeits*
at least one pass. At 1329 passes/day, ten restarts/day removes 0.75 % of the day's decay —
noise. The cadence itself is the entire bug. (This matters for the design: it removes the
main argument for a persisted per-vault watermark, see §3.2.)

### 1.5 Correction 2 — the archival branch is also mis-firing, in the opposite direction

`consolidationScore = peakWeight × coActivationCount / daysSinceLastActivated`, with
`daysSince` clamped to a minimum of 1.0 (`association.go:667`). Under the current regime an
edge reaches the floor ~1 hour after its last use, so `daysSince` is *always* clamped to 1.0
at the moment of the floor test, and the archive test degenerates to
`peakWeight × coActivationCount > 0.05`. Any edge that ever reached weight 0.05 with one
co-activation qualifies. **Today's regime promotes edges into the 0x25 archive namespace
(removing them from the live 0x14 weight index) roughly an hour after their last use.** The
measurement plan (§7) must count this; changing the decay rate changes archive volume by
orders of magnitude in the *other* direction, and that interaction has to be measured, not
assumed.

Also worth recording: pre-#757, a weight-1.0 edge read back as weight 0, so `peakWeight`
bootstrapped to 0, `dynamicFloor` was 0, and the `else` branch set `e.remove = true` —
the *strongest* edges were hard-deleted rather than floored. The encoder bug and the decay
rate compounded. #759 repairs the key layout; this design repairs the rate.

---

## 2. Mechanism — peak-anchored, elapsed-time ceiling

Replace *compounding per pass* with a **decay ceiling computed from state the edge already
carries**:

```
Δt      = max(0, now − lastActivated)                  // seconds
ceiling = max(dynamicFloor, peakWeight × 2^(−Δt / H))  // H = half-life, seconds
w_new   = min(w_old, ceiling)                          // NEVER raises a weight
```

`dynamicFloor = peakWeight × 0.05` — unchanged from today.

Both inputs already exist in the 26-byte association value
(`association.go:19`: `relType(2) | confidence(4) | createdAt(8) | lastActivated(4) |
peakWeight(4) | coActivationCount(4)`). **No value-format change, no new Pebble prefix, no
migration.** That is the single strongest argument for this shape over every alternative.

### 2.1 Properties this buys

1. **Cadence-independence (the actual fix).** With no activation between two evaluations,
   `ceiling` is monotone decreasing in `t`, so the running minimum over *any* grid of
   evaluation times equals `ceiling(t_final)`. Sixty one-minute passes and one sixty-minute
   pass produce a **bit-identical** result. The pass rate stops being a hidden multiplier on
   the configured rate. That is what makes the config field have a unit at all.
   (With activations interleaved, denser evaluation gives a marginally lower running min;
   the gap is bounded by one interval of decay — at H=30 d, 0.0016 % for a 60 s grid vs
   0.096 % for a 60 min grid. Bounded and negligible, versus today's *proportional-to-pass-count*
   divergence.)

2. **Restart- and downtime-immune.** No accumulated "decay debt" exists to dump after an
   outage; the weight is a function of wall-clock elapsed since last use, so a process down
   for a month resumes at exactly the right place with no catch-up loop and no cap needed.
   Also makes #759's decay parking free: parking accrues nothing.

3. **Convergent under replication (#760).** The ceiling is a pure function of
   `(peakWeight, lastActivated, now)`; the first two replicate. `min` is commutative and
   associative, and decay writes replicate (`ps.replicateBatch`), so independently-scheduled
   nodes converge to `min` over the union of their evaluation times rather than diverging by
   pass count. Clock skew of 5 minutes against a 30-day half-life is a 0.008 % weight
   difference. This does **not** close #760 (writes still race, the worker is still
   role-blind), but it removes the mechanism by which drift was unbounded, and it preserves
   the local-repair precedent (#681, #759) that #760 explicitly asks for.

4. **Write-amplification collapse.** Today every edge is delete+re-Set on 5 keys (fwd, rev,
   weight index, ±legacy) **every 65 s**. Under the new curve at H=30 d, per-pass drop is
   1.7e-5 relative. With a `writeEpsilon` skip (§2.3) each edge is rewritten roughly every
   2 hours instead of every 65 s — a ~99 % reduction in decay write volume. The skip is
   **lossless** precisely because the ceiling is absolute: skipping a write does not lose
   decay, the next pass recomputes from `lastActivated` regardless.

### 2.2 What it gives up, stated honestly

`w_old` stops being an independent state variable that decay evolves; it becomes a value
clamped under a peak-anchored curve. The only writer that can put a weight *below* that
curve and expect it to stay is the dream engine
(`internal/consolidation/transitive.go:106` writes `inferredWeight` directly). Under the new
mechanism a dream-lowered weight is never *raised* (the `min`), so nothing is silently
undone — but a future mechanism that wants "decay this edge faster than its peak curve"
has no lever. That lever does not exist today either. Named as a residual, not a regression.

Second: the model asserts "an edge's current strength is its peak strength faded by time
since last use." That is the ACT-R/Ebbinghaus shape (retention decays from encoding
strength as a function of elapsed time, reset by retrieval), and it is a *stronger* claim
than the current code makes. It is the right claim for this system, but it is a claim, and
the measurement in §7 is what tests it.

### 2.3 Concrete replacement for `DecayAssocWeights`'s inner loop

```go
// association.go — inside the iterator loop, replacing the grace-window skip
// and `newW = oldW * decayFactor`.

const assocDecayWriteEpsilon = 0.001 // absolute, on the [0,1] weight scale:
                                     // a write-avoidance quantum, not a behavioural threshold

// Legacy/never-activated guard. lastActivated == 0 means "unknown", NOT "epoch".
// Fall back to createdAt; if that is zero too, adopt the edge by stamping
// lastActivated = now and leaving the weight untouched this pass.
anchorTime := lastActivated
if anchorTime == 0 {
    if !createdAt.IsZero() {
        anchorTime = int32(createdAt.Unix())
    } else {
        adopt = true // rewrite value with lastActivated = now, weight unchanged
    }
}

elapsed := now.Sub(time.Unix(int64(anchorTime), 0))
if elapsed < 0 {
    elapsed = 0 // backwards clock skew / future stamp: never decay, never boost
}

if peakWeight == 0 {
    peakWeight = oldW // existing bootstrap, unchanged
}

ceiling := float64(peakWeight) * math.Exp2(-elapsed.Seconds()/halfLife.Seconds())
newW := oldW
if float32(ceiling) < oldW {
    newW = float32(ceiling)
}
if oldW-newW < assocDecayWriteEpsilon && !adopt {
    continue // lossless skip — the ceiling is absolute, not compounded
}
// …from here the existing floor / archive / clamp branch is unchanged, with
// `newW < minWeight` still selecting archive-vs-clamp against dynamicFloor.
```

Signature change: `DecayAssocWeights(ctx, wsPrefix, halfLife time.Duration, minWeight float32,
archiveThreshold float64)`. All six implementors/mocks are in-tree
(`internal/storage/store.go:146`, `internal/cognitive/hebbian.go:29` + `store_adapters.go:30`,
`cmd/bench/adapters.go:22`, three test mocks). Keeping the name and changing the parameter's
type makes every call site a compile error — no silent mis-wiring. Do that rather than
adding a parallel method.

### 2.4 The 5-minute grace window

Delete it. It becomes redundant: an edge activated 30 s ago has `Δt ≈ 0`, so
`ceiling ≈ peakWeight ≥ w_old` and the `min` is a no-op. Keeping it would be a second,
undocumented cliff in a mechanism whose whole point is that there is no cliff. The `TODO:
make this configurable per vault` at `association.go:528` dies with it.

---

## 3. Config semantics — the honest migration story

### 3.1 The problem (principle #1)

`assoc_decay_factor` is documented as "multiplier per pass". Reinterpreting the same field
as "multiplier per day" is a silent semantics change to a value operators may have set
explicitly — precisely the failure class principle #1 exists to prevent.

The counter-argument, which is the one that decides it: **"per pass" was never a meaning.**
A pass is an unspecified interval owned by an unrelated worker; `0.95` denominated in passes
does not describe any rate a user could reason about. There is no semantics to preserve —
only a number to translate, loudly.

### 3.2 Chosen path: new field carries the rate, old field keeps the switch

- **New:** `assoc_half_life_days` (`AssocHalfLifeDays *float32` on `PlasticityConfig`,
  `float32` on `plasticityPreset` and `ResolvedPlasticity`). This is the rate. Clamped, never
  rejected (COG-2): `[0.5, 3650]` days; `0` means "unset → use preset".
- **Kept:** `assoc_decay_factor` remains the **enable/disable switch**, so COG-16
  (`HebbianEnabled && AssocDecayFactor > 0`) is untouched and `scratchpad`'s `0` still
  disables decay with no special case. Its doc comment changes to say it no longer sets a
  rate.
- **Legacy conversion, loud:** if a vault has an *explicit* `assoc_decay_factor` override
  (`cfg.AssocDecayFactor != nil`) and **no** `assoc_half_life_days` override, resolve
  `halfLifeDays = ln(0.5) / ln(f) × (reference interval)` where the reference interval is
  declared to be **1 day** — i.e. the old number is reinterpreted as per-day — and emit a
  **one-time WARN per vault** naming the vault, the old factor, the derived half-life, and
  the field to set to make it explicit. Rationale for per-day rather than per-pass: per-pass
  is unrepresentable in a cadence-independent mechanism, and preserving the observed
  behaviour would mean preserving the bug. Degrade loudly (principle #2).
  Worked example: `0.95 → 13.5 day half-life`; `0.98 → 34.3 days`.
- **Not chosen:** bumping `PlasticityConfig.version`. It exists but nothing dispatches on
  it; introducing versioned resolution for one field is more machinery than the problem
  needs, and the WARN carries the same information to the operator who can act on it.

### 3.3 Preset values and the target half-life regime

| preset | `assoc_decay_factor` (switch) | **`assoc_half_life_days`** | rationale |
|---|---|---|---|
| default | 0.95 (unchanged) | **30** | matches the preset's own `TemporalHalflife: 30` — engram-level and edge-level forgetting on the same clock |
| reference | 0.95 (unchanged) | **30** | same as default today; unchanged relationship |
| working | 0.95 (unchanged) | **30** | must stay `default + {RetentionDays:7, BehaviorMode:selective}` (COG-3) |
| knowledge-graph | 0.98 (unchanged) | **90** | was already ~2.5× slower than default in pass terms (34.3 vs 13.5 passes); structure durability is the preset's purpose |
| scratchpad | 0 (unchanged) | 0 | decay disabled; Hebbian off |

**Justification for 30 days** — and note it is anchored to an existing in-tree constant, not
tuned against the origin deployment's numbers (#762 explicitly warns against calibrating on a
population corrupted by the #757 encoder bug; principle #11):

| elapsed since last use | weight / peak |
|---|---|
| 1 day | 0.977 |
| 7 days | 0.851 |
| 30 days | 0.500 |
| 90 days | 0.125 |
| **129.7 days** | **0.050 → hits the dynamic floor** |

The regime: *noticeable within days, halved in a month, floored within a quarter.* That is
the hours-to-days-to-months band the Ebbinghaus/ACT-R literature puts consolidated
associative memory in, and it is a regime in which weight-ordered 0x03 traversal carries real
information for the entire life of a working vault.

**The other constraint this exposes, stated up front:** the decay/reinforcement balance point
at H=30 d is `ln2/(30·ln1.01) = 2.32` signal units/day — roughly 5–12 co-retrievals/day to
hold an edge's weight flat. Today's equivalent is 6,850 units/day. So this moves
"strengthens with use" from *physically impossible* to *achievable by a genuinely hot edge*,
and every other edge fades — which is the intended behaviour. Whether 2.32/day is the right
balance is a `HebbianLearningRate` question, deliberately **out of scope** here (§5).

---

## 4. Invariant, drift, and keyspace impact

| surface | impact |
|---|---|
| **Keyspace registry** | **None.** No new prefix, no value-format change. `internal/prefix/prefix.All()` untouched. Say so explicitly in the PR body so the reviewer doesn't hunt for it. |
| **COG-16** | Holds verbatim (`AssocDecayFactor > 0` remains the gate). Add an amendment recording that the factor is now a *switch* and `AssocHalfLifeDays` is the rate. |
| **COG-2** | New field must be clamped, never rejected. Add to the clamp list in the invariant text. |
| **COG-3** | `working == default + exactly {RetentionDays:7, BehaviorMode:selective}` — adding the field to both preserves it. `TestResolvePlasticity_WorkingEqualsDefaultPlusTwoDeltas` must stay green **without editing it**; if it needs editing, the change is wrong. |
| **COG-4** | Cross-preset guarantees list does not include assoc decay; unchanged. Consider adding "all presets share `ArchiveThreshold: 0.05`" is already there — verify still true (it is). |
| **New invariant (propose COG-27)** | *Association decay is cadence-independent and never raises a weight: `w_new = min(w_old, max(peak·0.05, peak·2^(−Δt/H)))` with `Δt = max(0, now − lastActivated)`. Two decay evaluations at times t₁ < t₂ with no intervening activation produce exactly the weight of a single evaluation at t₂.* Pinned by the tests in §6. |
| **Obligation #2 (openapi)** | `internal/transport/rest/openapi.yaml` `PlasticityConfig` enumerates fields explicitly (`:1007`). Add `assoc_half_life_days`. (Note: that enum is *already drifted* — it lists `[default, reference, scratchpad, knowledge-graph]` and is missing `working`. Fix in the same PR; it is one line and it is a live drift.) Run `npx @redocly/cli lint`. |
| **Obligation #3 (SDKs)** | `PlasticityConfig` is admin-surface config, not a recall type. Check `sdk/python|node|php` for a mirrored plasticity struct; if none carries `assoc_decay_factor` today (it does not appear in any non-Go surface — grep confirmed), nothing to do. Record the grep in the PR. |
| **Obligation #4 (web console)** | Grep confirms `assoc_decay` appears in **no** `web/` template or JS: the preset cards do not display it and the plasticity form does not expose it. **No web change required** — but say so explicitly rather than silently, and re-run `TestPlasticityPresets_WebConsoleParity` (names only; unaffected). |
| **Obligation #11 (replication)** | Decay writes already go through `replicateBatch`. The worker stays role-blind (#760 remains open); note in the PR that this change makes independent execution convergent rather than divergent, and does not close #760. |
| **Obligation #12 (async seam)** | No new worker. |
| **#759 repair gate** | `decayAllVaults`'s `assocWeightRepairComplete()` check stays first and unchanged. Its cost drops to zero: a parked period accrues no debt. |
| **COG-20 / importance (dd640dc)** | Engram-level; associations have no importance analogue. Untouched. Named as a deferral (§5). |

---

## 5. Minimal increment — scope and deferrals

### In scope (one PR)

1. `internal/storage/association.go` — ceiling mechanism, epsilon skip, legacy
   `lastActivated == 0` adoption guard, negative-elapsed guard, delete
   `assocDecayGraceWindow`. Signature → `halfLife time.Duration`.
2. `internal/storage/store.go`, `internal/cognitive/hebbian.go` +
   `store_adapters.go`, `cmd/bench/adapters.go`, three test mocks — signature propagation.
3. `internal/auth/plasticity.go` — `AssocHalfLifeDays` on config/preset/resolved, five preset
   values, clamp `[0.5, 3650]`, legacy-factor conversion + one-time WARN.
4. `internal/engine/engine.go` `decayAllVaults` — pass the resolved half-life. Gate order
   unchanged.
5. `internal/transport/rest/openapi.yaml` — new field + the missing `working` enum value.
6. Docs — `invariants.md` (COG-16 amendment + COG-27), `decision-record.md` entry citing
   #762 and naming the 79605b2 units-mismatch history.
7. Tests — §6.

### Explicitly deferred, named

- **Declared-edge protection.** `RelContradicts` / `RelSupersedes` / explicit `muninn_link`
  edges decay identically to learned ones today, so a declared contradiction ends up at 0.05,
  below noise (#762 step 3). The fix is a `RelType`-aware half-life (or no decay for declared
  edges) and needs its own design — it changes what an edge *means*, not just how fast it
  fades. **Increment 2.**
- **`ArchiveThreshold` recalibration.** §1.5 shows today's archive test degenerates. Under
  H=30 d, floor-hit happens at `daysSince ≈ 130`, so the test becomes
  `peak × coAct > 6.5` — far stricter. The measurement in §7 gate 5 decides whether this
  needs retuning in the same PR or a follow-up; do not pre-tune it.
- **Importance-modulated assoc decay.** Blocked on the same three-scoring-mode analysis
  COG-20 defers.
- **`HebbianLearningRate` / the 2.32-signal-units-per-day balance point** (§3.3).
- **Decoupling the decay cadence from the prune cadence.** Now cost-free to do (the mechanism
  is cadence-independent) and therefore not urgent. A separate, slower decay ticker would cut
  the per-vault 0x03 full scan; the epsilon skip already removes most of the write cost.
- **#760 proper** (leader-gated decay). Improved, not closed.
- **`lastActivated` is `int32`** — 2038 overflow. Post-2038 `Δt` goes negative, the guard
  clamps it to 0, and decay silently *stops*. Fail-safe direction, but file a follow-up issue;
  the field is in the on-disk value so widening it is a format change.
- **Option B (pass-elapsed normalization with a per-vault watermark)** — rejected, see §8.

---

## 6. Tests (RED-first)

All are unit tests in `internal/storage` / `internal/auth` — near-zero CI cost
(drift-and-obligations CI budget). `-race` on the storage tests per the standing rule.

1. **`TestDecayAssoc_CadenceIndependent`** — *the RED pin for the whole change.* Seed an edge
   at weight 0.8, peak 0.8, `lastActivated = now−0`. Drive decay with an injectable clock:
   arm A runs 60 evaluations at 1-minute simulated steps; arm B runs 1 evaluation at
   +60 minutes. Assert `|w_A − w_B| < 1e-6`. **RED on `develop`:** arm A is
   `0.8·0.95^60 ≈ 0.037`, arm B is `0.8·0.95 = 0.76` — a 20× divergence. GREEN after.
2. **`TestDecayAssoc_NeverRaisesWeight`** — edge with `w_old = 0.2`, `peak = 0.9`,
   `lastActivated = now`. Ceiling ≈ 0.9. Assert post-decay weight is still 0.2.
   RED if the `min` is dropped.
3. **`TestDecayAssoc_LegacyZeroLastActivated_Adopted`** — edge with `lastActivated = 0` and a
   zero `createdAt`. Assert weight unchanged and `lastActivated` stamped to now. **RED
   without the guard the naive formula floors it instantly** (Δt = 56 years).
4. **`TestDecayAssoc_HalfLifeMatchesTable`** — table-driven over the §3.3 rows
   (1/7/30/90/129.7 days → 0.977/0.851/0.5/0.125/floor), tolerance 1e-3.
5. **`TestDecayAssoc_FloorAndArchiveBranchUnchanged`** — the existing
   `TestDecayAssocWeights_ArchivesStrongEdge` /
   `TestAssocWeightIndex_DecayFloorsClampsEntry` /
   `TestAssocMetadata_PreservedThroughDecay` must pass with the parameter translated, proving
   the floor/archive/metadata behaviour is untouched. If any needs *behavioural* editing,
   stop — the change has leaked past its scope.
6. **`TestDecayAssoc_EpsilonSkipIsLossless`** — run with epsilon 0.001 for 24 simulated hours
   at 65 s steps, versus one evaluation at +24 h. Assert equal within 1e-6.
7. **`TestResolvePlasticity_LegacyFactorConvertsWithWarn`** — explicit
   `assoc_decay_factor: 0.95`, no half-life override → resolved 13.51 days; assert the WARN
   fires exactly once per vault (capture via `slog` handler).
8. **`TestResolvePlasticity_HalfLifeClamped`** — `-5 → 0.5`, `99999 → 3650` (COG-2: clamp,
   never reject).
9. **Unchanged, must stay green untouched:**
   `TestResolvePlasticity_WorkingEqualsDefaultPlusTwoDeltas` (COG-3),
   `TestPlasticityPresets_WebConsoleParity`,
   `TestAll_NoDuplicateBytes` / `TestAll_OwnerGroupsPairwiseDisjoint`.

---

## 7. Measurement — the acceptance gate

Two harnesses. **Aggregates only: quantiles, counts, and ratios. No engram IDs, no content,
no tags, no vault names in any output.** The offline harness opens a *copy* of a real backup
read-only and never writes to it.

### 7.1 Offline projection on a real vault copy (`cmd/bench`, or a `//go:build measure` tool)

Extract per-edge tuples `(weight, peakWeight, lastActivated, coActivationCount)` by scanning
0x03, then simulate both regimes forward from `t0 = backup timestamp`.

| # | metric | old (expected) | new (gate) |
|---|---|---|---|
| M1 | fraction of live edges at the dynamic floor, at t0+7 d | ≈ 1.0 (measured: store-wide max = 0.05) | **< 0.10** |
| M2 | p50 / p90 of `weight/peak`, at t0+1 d / +7 d / +30 d | ~0.05 / 0.05 at every horizon | +1 d p50 ≥ 0.95; **+7 d p90 ≥ 0.80**; +30 d p50 ≥ 0.40 |
| M3 | Spearman ρ between weight order and `coActivationCount` order over live edges at +7 d | degenerate (ties at floor) | **ρ ≥ 0.6** — the "learned structure survives" number |
| M4 | count of distinct weight values among live edges at +7 d (resolution of weight-ordered 0x03 traversal) | ~1 per peak bucket | report; must be ≥ 100× the old count on a vault with ≥ 10 k edges |
| M5 | edges promoted to 0x25 archive per simulated 180 d | report (§1.5 predicts near-total) | report + **kill check**: if new-regime promotions are 0 on a vault where old-regime promotions were > 0, archival is dead → retune `ArchiveThreshold` in this PR or defer it with an explicit WARN, do not ship silently |
| M6 | Pebble key-writes per decay pass | 5 per edge per 65 s | report; expect ≥ 95 % reduction |

### 7.2 Live two-arm "strengthens with use" observable

On a scratch vault (`default` preset, real engine, injectable clock or accelerated half-life
scaled so the ratios are identical):

- Arm A: an edge co-activated once per simulated day for 7 days.
- Arm B: an identical edge, co-activated at t0 only.
- Gate: `w_A / w_B ≥ 1.20` at 24 h and **`≥ 2.0` at 7 days**.
- **RED arm (mandatory):** re-run with the mechanism reverted (legacy per-pass factor path,
  or `H` set to the current effective 14.6 minutes). Assert the ratio collapses to
  `1.0 ± 0.05` — both arms floored. A harness that cannot show the *old* regime failing is
  not measuring the mechanism.

### 7.3 Recall no-regression check (report, mostly not a gate)

Fixed query set against a copied vault, before/after. Results *should* change — that is the
point — so top-k overlap is **reported, not gated**. The one hard gate:
**no query that returned results before may return empty after.** (Weights only rise
relative to today, and the entity/Hebbian boost paths read them, so an empty-after is
evidence of a sign or clamp error, not of the intended behaviour change.)

### 7.4 Kill thresholds — abandon or redesign, do not tune-and-ship

- M1 (test 1) cannot be made to hold exactly → the mechanism is not what this doc claims;
  stop and redesign.
- Any edge observed with `w_new > w_old` → the `min` is broken; stop.
- After 30 simulated days on a real vault copy, **> 50 %** of edges are floored → H=30 d is
  too hot for real access patterns; re-derive H from the measured inter-activation-interval
  distribution *before* landing, and record the derivation.
- Conversely, if after 180 simulated days **< 5 %** of edges are floored, decay is too cold to
  deliver "fades when unused"; same treatment.
- M5 shows archival went to zero → §5 deferral becomes in-scope or an explicit WARN.

---

## 8. Alternative considered and rejected: pass-elapsed normalization

`w ← w · f^(elapsed / referenceInterval)`, with a persisted per-vault last-decay watermark
so restarts don't lose elapsed time.

- **Cost:** a new keyspace entry → Tier-3 review, `prefix.All()` + disjointness obligations,
  `ClearVault`'s `dataPrefixes` list (STO-6), replication of the watermark. All of which the
  peak-anchored form avoids entirely.
- **Still compounding, therefore still path-dependent:** the result depends on *how the
  elapsed time was partitioned into passes* only to floating-point error — but more
  importantly, two cluster nodes with independent watermarks produce genuinely different
  weights, so #760 gets no better and arguably worse (a per-node watermark is per-node state
  that never converges).
- **Downtime debt:** a node down for 30 days applies 30 days of decay in one pass. Correct in
  principle, but it needs a cap, and a cap is a second magic constant.
- **Its one real advantage:** it preserves `w_old` as an independent state variable, so a
  future mechanism that deliberately writes a weight below the peak curve keeps it. That
  advantage is worth exactly the residual named in §2.2 — and it is not worth a new keyspace
  entry today.

Verdict: rejected for increment 1. If the dream engine (or anything else) later needs
sub-curve weights to persist against decay, the fix is a per-edge anchor field in the
association value (26 → 30 bytes, a format the decoder already handles), not a per-vault
watermark.

---

## 9. Top risks

1. **The half-life is a judgment call.** 30 days is anchored to `TemporalHalflife: 30`, not to
   data — deliberately, because the only data available comes from a population corrupted by
   #757 and ground flat by this very bug (#762's own warning). §7.4's kill thresholds are the
   mitigation: if the real inter-activation distribution says 30 d is wrong, the measurement
   catches it before landing.
2. **Weights will jump by ~20× on the next pass after deploy.** Every edge currently sitting
   at `peak × 0.05` has `w_old = 0.05·peak` and a ceiling of `peak · 2^(−Δt/30d)`. The `min`
   means **weights do not jump up** — the floor survivors stay at the floor and only recover
   through Hebbian growth. This is the *correct* conservative behaviour (no free
   resurrection), but it also means **#762's damage is not undone by this fix**: existing
   vaults keep their flattened structure and re-learn from the floor. Say this in the release
   note. A one-shot re-anchoring migration (`w ← ceiling` for edges at the floor with
   `coActivationCount > 0`) is a *separate, opt-in* decision — it would fabricate weights that
   were never earned, and I would not ship it by default.
3. **`lastActivated` fidelity.** The mechanism now depends on that field being right
   everywhere. `UpdateAssocWeight` / `UpdateAssocWeightBatch` set it (`association.go:383`,
   `:444`); `DecayAssocWeights` correctly does *not* (`:598`). Any path that writes an
   association without stamping it becomes a silent "this edge never fades" bug. The census
   is small and in-tree — enumerate it in the PR body.
4. **Archive-volume swing** (§1.5, M5) in either direction.
5. **Signature change ripples to `cmd/bench`** — build with `-tags localassets` across
   `./...` including `cmd/bench`, per obligation #9.

---

## 10. Handoff checklist for the build agent

- Work in a fresh worktree off `origin/develop`; confirm with `git log -1` before starting.
- Write test 1 (`TestDecayAssoc_CadenceIndependent`) **first** and confirm it is RED on
  unmodified `develop` (arm A ≈ 0.037, arm B ≈ 0.76). Record the RED output in the PR body.
- Then §5 items 1–4, then tests 2–8, then §5 items 5–6.
- `go build -tags localassets ./... && go vet -tags localassets ./... && gofmt -l .`
- `go test -race -tags localassets ./internal/storage/... ./internal/auth/... ./internal/engine/... ./internal/cognitive/...`
- `npx @redocly/cli lint internal/transport/rest/openapi.yaml`
- Run §7.1 offline against a real backup copy; paste M1–M6 (aggregates only) into the PR.
- PR body must state: no new Pebble prefix; no association value-format change; no web-console
  change (with the grep that proves it); #760 improved but not closed; #762's existing damage
  not retroactively repaired (risk 2).
