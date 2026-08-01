# THE PUSH — prospective memory over MCP (design, not built)

Roadmap #1 (Fable's ranking): the single biggest sentient-feel gap. Memory that speaks FIRST.
Grounded on develop @ 24f3b46.

## Core stance (neuroscience → architecture)
McDaniel & Einstein: intention retrieval is either costly MONITORING (polling) or cheap SPONTANEOUS
retrieval — and spontaneous works only when the cue is FOCAL to what's being processed. So:
**MuninnDB NEVER monitors. It arms intentions bound to entity cues, and checks them only at the
moment the agent is already processing those entities** (inside a recall/remember the agent itself
made). MCP has no honest interrupt channel, so delivery = piggyback a `notices` field on the tool
response ("push at the next natural exchange"). This IS the neuroscience, not a compromise — nobody's
hippocampus interrupts them at random. MCP push conditions on DURABLE STATE (armed intentions,
contradiction flags), never in-flight events (events fire when nobody's listening; state persists).

## Verified code truths
- trigger.TriggerSystem exists (TriggerNewWrite/ThresholdCrossed/Contradiction) — wired to
  gRPC/REST/MBP, NOT reachable from MCP. Its 30s handleSweep IS the polling #609 killed.
- MCP has a STILLBORN push vehicle (EventBus + notifications/muninn/* defined, nothing publishes).
- Contradiction detection is LIVE + durable (0x0A pair keys); muninn_contradictions is pull-only.
- Focal-cue substrate exists (entity index, entityIDF rarity, FindByEntity).
- TypeGoal exists w/ elevated importance + COG-20 protection, but NO "armed intention" state.
- #609: 523 ambient deliveries, ZERO uptake — but it killed TASK-BLIND push; says nothing about
  cue-driven surfacing inside an exchange the agent initiated.
- Delivery precedent: recall/remember already carry an additive `hint` field → notices rides it.

## The design
1. `muninn_intend` tool: {content, cues:[≥1 entity], valid_until? (a BOUND not a trigger),
   one_shot=true, importance?}. Stored as a normal TypeGoal engram + a new 0x2C armed-intention
   index (ws+entityHash(cue)+id). Ubiquitous cues REFUSED loudly (entityIDF floor — don't arm a nag).
   Time SILENCES, never fires (inverts the due:-tag failure).
2. Firing (`PendingNotices`, called ONLY inside tool handlers, never background): focal set =
   entities on the RETURNED top-K (recall) or inline entities (remember). Fires iff cue∈focal AND
   ValidAt AND not-self-echo AND one-shot-unfired AND session-dedup AND cap-2-per-response.
   read_only suppresses the fired-marker write (COG-11).
3. Delivery: additive `notices` field on recall/remember responses; OMITTED when empty (zero token
   cost on the 99% path). Each: {kind, note (verbatim), cue, why (states the focal cue — auditable),
   action_hint}.
4. Contradiction = flagship payload, ZERO new detection: intersect returned resultIDs against the
   durable 0x0A pairs → kind:"contradiction" notice. Focal by construction ("you just retrieved a
   memory flagged as contradicting another").
5. Composition: valid-time (intention windows), importance (TypeGoal COG-20 + notice ranking),
   entity-merge rewrites 0x2C, working-buffer cues deferred.

## Acceptance gate (non-gameable, 5 gates)
Seeded precision harness (~200 memories, 12 armed intentions, scripted ~60-call session: 15
should-fire, 10 cue-adjacent TRAPS, 35 unrelated):
- Gate1 Precision fired∧wanted/fired ≥ 0.90.
- Gate2 Recall ≥ 0.80 of should-fire (anti-gaming pair: silence fails G2, spam fails G1).
- Gate3 Negative control: 35 unrelated calls → EXACTLY ZERO notices (hard fail on 1).
- Gate4 RED: mechanism off → zero notices AND G1/G2 fail.
- Gate5 LIVE SHADOW (gates DEFAULT-ON, not merge): ship behind MUNINN_PROSPECTIVE=1; shadow-log
  would-have-fired for ≥1 week on a real vault; human labels each; default-on requires live
  precision ≥ 0.80.
- PRE-COMMITTED honest fallback: if Gate5 fails for intentions → #609-style negative result in the
  decision record; muninn_intend stays opt-in/pulled; CONTRADICTION-ONLY notices (no intention
  model, focal by construction) ship default-on instead.

## Increment 1 vs deferred
Inc1: 0x2C index + collision guard + registry; Intend/PendingNotices + contradiction intersection;
muninn_intend tool (SEC-6 census + registry smoke + guide) + notices on recall/remember +
MUNINN_PROSPECTIVE opt-in; the 5-gate harness + an invariant pin.
Deferred: SSE notifications/muninn/* wiring; recurring intentions + cooldowns; derived intentions
from muninn_decide; working-buffer cues; semantic (non-entity) cues; notices on
remember_batch/where_left_off/find_by_entity; contradiction auto-clear; gRPC/MBP intention parity.

## Mistakes NOT reproduced
#609 ambient/task-blind push (nothing fires outside an agent-initiated exchange; the 30s sweep is
deliberately NOT extended to MCP); due:-tag time-polling (time only silences; no scheduler).

## Top 2 risks
1. Real-vault focal sets dirtier than seeded → precision erosion. Mitigations load-bearing: IDF
   arming floor, 2-notice cap, session dedup, and Gate5 live-shadow labels (why default-on is gated
   there + the contradiction-only fallback is pre-committed).
2. Client attention economics: a field agents ignore = #609 with extra steps; one they overweight
   derails tasks. Shadow-mode UPTAKE (do agents act on notices?), not just precision, tells the truth.
