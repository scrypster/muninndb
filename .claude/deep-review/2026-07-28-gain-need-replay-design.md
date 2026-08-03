# Real replay ordered by Gain × Need — design (not built)

Roadmap #5: make dream consolidation BUILD structure, not just decay. Grounded at fb77813.

## Ground truth
- `runPhase1Replay` (consolidation/replay.go:12-33) is fully INERT: fetches 50 IDs by stale
  relevance bucket (RecentActive, query.go:24 — the doc lies, it's not recency/most-accessed),
  drops them, no consumer. DreamOnce runs orient→inert-replay→dedup(destroy); phases 2b/3/4/5 =
  "future PR". So dream only destroys.
- The BUILD machinery already exists: `HebbianWorker.Submit(CoActivationEvent)` (cognitive/
  hebbian.go:193-324) — batched, atomic, log-space multiplicative `w'=min(1,w·(1.01)^Σ(sᵢsⱼ))`,
  seed 0.01, NEVER touches confidence (COG-10 holds by construction). Replay should DRIVE it.
- Selection substrate (Yang et al. "reactivated at reward"): `Engine.Decide` (engine.go:3583)
  writes a "decision"-tagged engram + links decision--RelSupports-->evidence per cited ID — the
  reward tag, already on disk. Recall events (#573) exist but reads are purpose-gated to
  "calibration" only; inc-1 doesn't need them (leave the gate).
- Need already exists: `computeACTR` B(M)=ln(n+1)−d·ln(age/(n+1)) IS need-probability (Anderson &
  Schooler 1991), computable offline from AccessCount/LastAccess.
- Importance (types.go:88) has zero consumers — replay's Need is its first.

## What replay DOES (v1): build pairs online-Hebbian can't
Online Hebbian only strengthens CO-SURFACED pairs. Replay's job (Mattar & Daw): propagate
structure to pairs NEVER co-experienced. Two such classes exist and nothing builds them:
1. **Evidence siblings** — Decide links decision→A,→B,→C but never A↔B (evidence co-cited at one
   decision is related though never co-recalled). Yang: reward-reactivated memories replay together.
2. **Succession bridges** — when N supersedes/contradicts O, O's strong associates (w≥0.3) relate
   to N, but N's Hebbian neighborhood is cold (0.01 seed). Reverse replay wires the changed belief
   into its predecessor's earned structure.
v1 = for each top-K-by-EVB engram, synthesize ONE CoActivationEvent pairing it with its partners
(≤ReplayFanout=8) and Submit to the existing HebbianWorker. `replayScore=0.5` (per-pair signal
0.25 → +0.25%/run, weaker than a real recall so replay never outweighs use). LTP: nil ALWAYS
(replay never potentiates — permanence stays earned by genuine repeated use). Add
`SubmitCoActivation` to consolidation's EngineInterface (thin forward; WARN+degrade if worker nil).
Wire into DreamOnce AND RunOnce; report ReplayedEngrams/ReplayPairs/ReplaySkippedNoSignal; honor DryRun.

## Gain (window (lastDreamAt, now], graph+tag only, no new pipeline)
`Gain(m) = 1.00·citedAsEvidence(m) + 1.00·supersessionParty(m) + 0.75·contradictionParty(m)`, cap 2.0.
No signal in window → Gain=0 everywhere → INFO "no gain signals since last dream", zero report,
exit (honest no-op, never a fabricated queue — principles #1/#10). Deferred from Gain: feedback
(not separable post-#682), confidence deltas (no event log), embedding surprise (heavy).

## Need (reuse ACT-R, compose Importance)
`Need(m) = (softplus(B(m))/actrDenom) · (1 + 0.5·Importance(m))`, B(M) identical to computeACTR
(extract shared helper OR duplicate + equality-pin, principle #6). Importance is a prior on future
need → belongs in Need, not Gain (important-but-unsurprising shouldn't replay; important changed
belief replays sooner). Deferred: PAS-transition Need (the correct successor-representation term),
forward/planning replay modes.

## Selection + budget
EVB=Gain×Need; sort candidates with Gain>0 (dozens, not the vault); top MaxReplay=32; Fanout=8;
hard cap 256 pairs/vault/run. Partners: evidence siblings, then predecessor associates w≥0.3, then
nothing (no random padding). Skip partners with out-degree > FanCap=64 (fan-effect guard). Cost:
tag-scan + GetMetadataBatch + ≤32 GetAssociations — sub-second, in the 5-min RunOnce timeout, zero
new goroutines, unit-testable in-memory (CI-cheap).

## Invariants
- COG-10 holds structurally (only mutation channel is HebbianWorker → can't write confidence).
- COG-11 unaffected (background learner, no read-surface/activation-log writes).
- COG-12 SHARP: replay must NEVER call TouchAccess (bumping n would inflate its own Need next run →
  self-licking loop). PIN it.
- NEW invariant: "Dream replay mutates association weights ONLY, via the Hebbian path, with per-run
  caps (MaxReplay×Fanout pairs, event score ≤0.5, LTP never set). Never touches AccessCount,
  LastAccess, Relevance, Stability, Confidence, Importance." Pin with an instrumented-store test
  asserting the only writes are UpdateAssocWeightBatch.

## Measurable proof (before/after, RED-checked)
Primary: **evidence-sibling retrieval**. Synthetic vault: 30 decisions × 3 semantically-DISJOINT
evidence (no shared entity/content → neither embedding nor entity-boost can connect them). Probe
context matching evidence A; success = sibling B/C surfaces in top-k via the associative hop.
WITHOUT replay this MUST fail (no A↔B edge exists — RED sanity, working-guide rule #3). After one
DreamOnce with replay: report siblings-recall@10 lift (e.g. 3/60 → 41/60) + median rank shift.
Secondary: succession wiring (after O superseded-by N, probe O's old associate contexts; measure N
co-surfacing — the stale-recall mitigation). On a real production vault: DryRun "K
selected, P pairs, Q currently zero-edge" then live before/after multi-hop probes. If Q≈0 (nobody
uses muninn_decide) → published NEGATIVE result (principle #10); supersession term carries it (two
independent signal sources → design survives either outcome).

## Increment scope
Inc-1: rewrite runPhase1Replay; SubmitCoActivation on EngineInterface; Gain(3 terms)+Need(B×imp)+
EVB top-K; caps (32/8/256, score 0.5, FanCap 64, LTP nil); report fields; new invariant + pin;
sibling-retrieval benchmark w/ RED; dream-state window; fix the lying doc comment.
Deferred (named): outcome field on muninn_decide (v1 treats every decision as a reward event; inc-2
adds outcome:good|bad|unknown weighting Gain — the one real gap vs theory); separable feedback
counter; recall-event co-occurrence pairs (needs widening #573 allowlist); PAS-successor Need;
reverse/forward modes; moving schema/transitive phases into DreamOnce; global Hebbian
normalization/fan caps (roadmap #6 — replay ships only its local FanCap).

## Mistakes NOT reproduced
Recency-as-priority (the current stub); gain-only prioritized sweeping (Need not optional);
replay-inflates-itself (no-TouchAccess); LLM-in-consolidation (v1 pure graph arithmetic, zero
tokens); #609 ambient-push fate (zero user-behavior change, measured structurally not by uptake;
decide-usage dependency hedged by supersession).

## Top 2 risks
1. Rich-get-richer around decision hubs (hot evidence engram crowds recall). Mitigations in v1:
   FanCap 64, per-run cap, +0.25%/run ceiling, LTP never set, + a hub-degradation regression test.
2. Building edges from BAD decisions (v1 has no outcome signal → wrong-decision evidence gets
   wired). Bounded (weight = relatedness not truth; confidence/supersession govern truth) but it's
   the first thing inc-2's outcome field fixes; say so in the PR.

Files inc-1: consolidation/replay.go (rewrite), worker.go (interface+report), report.go (+3
fields), dream.go (window), engine/engine.go (SubmitCoActivation ~10 lines), invariants.md (+1),
new replay_test.go + benchmark. No keyspace/transport/migration changes.
