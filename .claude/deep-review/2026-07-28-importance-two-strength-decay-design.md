# Importance dimension + two-strength decay — design (not built)

Roadmap item #4. Builds directly on valid-time (which already added the `Importance float32` ERF
field, offset 88–91, zero=unset — but NOTHING reads it yet, so storage/migration cost is paid).
References verified against integration/land-stack (the field is not on develop until #688 lands).

## The surprise: the DecayWorker is UNWIRED
`cognitive.NewDecayWorker` / `ComputeStability` (decay.go:66) have ZERO non-test callers.
Production "decay" today is three things: (1) read-time ACT-R base-level B(M)=ln(n+1)−d·ln(age/(n+1))
in computeACTR (activation/engine.go:1942, cap ≈1.489 per COG-7); (2) dream phase-4
`relevance *= 0.5` for old/low-access/low-relevance (consolidation/decay.go:59); (3) the pruner.
`Stability` is caller-set at write, passed through TouchAccess unchanged, used only for the
*reported* DecayFactor — nearly vestigial. That's the opening: Stability becomes Bjork's storage
strength without fighting any live consumer. (Do NOT wire the DecayWorker in this increment.)

## Bjork mapping
- Storage strength (durable, grows, ~never shrinks) → `Stability` (importance-boosted, gain inverse
  to current retrieval strength; slows dream-decay; raises the prune bar).
- Retrieval strength (accessible now, decays) → ACT-R base-level B(M) + `Relevance` (same formula;
  decay RATE importance-modulated).
- `Importance` = caller-asserted priority — orthogonal to Confidence (truth) and AccessCount (use).

## 1. Where Importance comes from — minimal honest v1
Explicit optional `Importance *float32` on WriteRequest (pointer: explicit 0 ≠ unset) → Write →
MCP remember/remember_batch/evolve + REST/gRPC. Clamped [0,1]. PLUS a USE-TIME (never stored)
default: `EffectiveImportance(meta)` returns the explicit value else a documented table on
MemoryType (Decision/Goal/Constraint/Identity 0.6; Preference/Procedure 0.5; Fact/Reference/Issue
0.4; Observation/Event/Task 0.3; +0.1 if Trust==verified, cap 1.0). Use-time derivation is the
honest shape (principle #1): storing a derived value = a silent write the caller never made +
freezes the heuristic into every record. Storage stays 0=unset; heuristic retroactively improvable
with zero migration; "asserted" vs "assumed" structurally distinct. Expose `importance` (+ explicit
vs derived) on read/recall surfaces (the df90e96 memory-type pattern). Evolve inherits explicit
importance unless overridden.
- Wrinkle: explicit 0.0 vs unset collide in the stored float → quantize explicit importance to
  [0.01,1.0] on write (explicit 0 stores 0.01; behaviorally identical everywhere). Don't spend a flag byte.

## 2. Two-strength decay — exact changes
(a) ComputeStability(accessCount, avgDaysBetween, importance): `* (1 + 1.0*importance)`; clamps
    [14,365] unchanged. Canonical even though worker unwired.
(b) Dream phase-4 (consolidation/decay.go:59): `newRelevance = Relevance * (0.5 + 0.5*imp)` —
    imp=0 halves (today's behavior), imp=1 no decay. Continuous, no cliff.
(c) computeACTR (activation/engine.go:1941): `d_eff = ACTRDecay * (1 - 0.5*imp)`, bounded [0.5d,d];
    COG-7 cap applied after, unchanged. Importance NEVER multiplies contentMatch or final score.
(d) Pruner MaxEngrams path (engine.go:3683): skip candidates with EffectiveImportance >= 0.7
    (HighImportanceFloor); if exemptions leave pruned<excess, WARN (degrade loudly, no loop).
    RetentionDays stays authoritative (explicit age policy, COG-3 working-preset contract) — NOT
    importance-exempt (else working vaults grow unbounded). Documented residual.

## 3. Inverted reinforcement (desirable difficulty — fixes finding #7 rich-get-richer)
In TouchAccess (storage/engram.go:970 — holds stripe lock, all four COG-12 channels funnel here):
AccessCount+1 stays (feeds B(M)); the DURABILITY payoff is inverse in current retrieval strength:
`B = ln(n+1) − 0.5·ln(max(ageDays,floor)/(n+1))`;
`gain = 7days / (1 + exp(2.0*(B − 0.75)))`; `newStability = min(Stability + gain*(1+0.5*imp), 365)`.
Hard retrieval (cold, B≈−1) earns ~+6.9 days; hot-set re-touch (B at cap) ~+1.2 days. Monotone,
bounded, capped — no oscillation. NO feedback loop: Stability is NOT in B(M), so growing storage
strength can't inflate the signal that suppresses further gain. Rate-limit 1/day-per-engram (the
#682 dedup shape) so ReinforceOnRead-default-true agents can't saturate (skip the gain, not the
count, when LastAccess <24h). Amend the TouchAccess doc contract; Confidence stays untouched.

## 4. Composition with valid-time
- Expired facts STILL decay (history fades via inaccessibility; invalidation is a stamp not a
  delete — COG-19 — and decay never becomes a delete). No special case.
- Importance PROTECTS expired-but-important facts from pruning (pruner exemption reads
  EffectiveImportance with no validity check) → an expired Decision marked 0.9 stays findable by
  as_of forever (modulo RetentionDays). Load-bearing: valid-time gives time travel, importance
  guarantees the destination still exists.
- Evolve chain: predecessor keeps importance when SupersedeEngram stamps it; successor inherits.

## 5. Invariants
- COG-10: amend — "Importance is likewise never moved by access or co-activation — only the caller
  (write/evolve) sets it." (priority axis vs truth axis vs use axis, kept structurally distinct.)
- COG-12: unchanged (no new bump site). COG-7: untouched (d_eff before the cap).
- COG-15: core holds; amend with exemption + WARN.
- NEW COG-20: "An engram with EffectiveImportance >= 0.7 is never hard-deleted by the MaxEngrams
  (retrieval-strength) prune path; RetentionDays remains an authoritative age policy, exempt from
  this protection." Pinned by a RED-checked test (vault over MaxEngrams, lowest-B(M) engram is
  high-importance → survives while next-worst is deleted).
- Drift: MCP schema + registry smoke; REST/gRPC/MBP request types; SDK; web console; docs. ERF: 0.

## 6. First increment scope
IN (one PR): Importance param across transports + MCP tools (clamped, quantized-explicit-0);
EffectiveImportance table + doc; exposure on read/recall; dream phase-4 modulation; pruner
exemption + COG-20 pin + WARN; inverted stability gain in TouchAccess + 1/day cap + RED test;
ComputeStability importance arg; evolve inheritance; invariant docs.
DEFERRED (inc 2+): schema-fit consolidation latency (Tse 2007); retroactive window-scoped salience
/ synaptic tagging (Frey & Morris — needs session-window bookkeeping); wiring the DecayWorker; any
importance term in the recall SCORE; a retroactive bump-importance API; RetentionDays exemption.

## Mistakes NOT reproduced
1. Naive Ebbinghaus that drops accuracy — importance never enters contentMatch/score; modulates
   only decay rate, prune protection, stability gain. Ranking for equal-recency memories unchanged.
2. Silent inference — no derived value ever written; explicit wins; one documented table function;
   effective value + provenance returned on read.
3. Rich-get-richer — gain inverse in B(M), bounded, capped, no Stability→B(M) feedback path.

## Top 2 risks
1. Touching TouchAccess semantics (audited locked primitive, STO-2/3, #594 resurrection class; doc
   promises "only AccessCount/LastAccess move"; ReinforceOnRead default-on → every non-read-only
   read becomes a stability write). Mitigate: 1/day cap, bounded gain, -race tests over concurrent
   TouchAccess/CAS/Delete, doc update in-PR. Residual: SEC-13 replication of the new write content.
2. Pruner exemption vs MaxEngrams contract: a vault marking everything ≥0.7 can't shrink; the
   LowestRelevanceIDs prefilter is importance-blind. Mitigate: WARN must fire + be tested;
   protected-fraction cap is the inc-2 lever; v1 documents the residual (principle #8).
