# Valid-time (bitemporality) — design for MuninnDB (not built)

The #1 architectural gap from the strategic review: the engine has only transaction time
(CreatedAt/UpdatedAt/LastAccess), no valid-time — the ROOT of "current runway led with the May
figure" (supersedes-aware recall patches the symptom). Valid-time is the moat: "what is true
now" + "what was true at T". Learn from Zep/Graphiti's model + TSQL2's ergonomic death; don't
reproduce their mistakes.

## The structural gift (shapes everything)
The ERF metadata block is a FIXED 100 bytes with bytes 72–99 RESERVED and zero-filled
(erf/format.go:33; encoder zero-inits at encode.go:37/191). Trust/MemoryType/Classification
were all added into reserved bytes with NO version bump and NO rewrite migration — zero decodes
as the legacy default (the PeakWeight "0 = untracked (legacy)" idiom). So valid-time costs
**0 bytes/record, 0 ERF version bump, 0 rewrite migration**. Timestamps live in the 664-byte
0x02 metadata value the decay worker + phase-6 filter already read.

## Fields (Engram + EngramMeta, types.go:58/86)
- `ValidFrom time.Time` — app time the fact became true. ERF offset 72–79 (uint64 UnixNano BE).
  Zero decodes → CreatedAt.
- `ValidUntil time.Time` — app time it stopped; ZERO = open/"current". Offset 80–87. Zero → open.
- (bundled) `Importance float32` — offset 88–91, zero = unset. Same reserved area, one PR.
- Semantics: **half-open `[ValidFrom, ValidUntil)`** (SQL:2011 convention; adjacent windows
  never overlap). `ValidAt(t) := ValidFrom ≤ t && (ValidUntil.IsZero() || t < ValidUntil)`.
- "Current" = `ValidUntil.IsZero()`. No magic max, no nullable pointer.
- Every existing on-disk record decodes as "valid from creation, still true" — silently correct.

## How a fact becomes invalid (never deleted — stamped)
1. **Evolve** (engine.go:2965): existing atomic batch also sets `old.ValidUntil = new.ValidFrom`
   (default now; optional `effective_at`). **Evolve KEEPS soft-deleting the predecessor** (see
   Composition). 
2. **Explicit `RelSupersedes` link**: stamps target's `ValidUntil = now` (skip if already closed).
   Write-time closure makes the shipped read-time chain-walk the LEGACY-DATA fallback —
   supersession becomes literally a special case of valid-time.
3. **`muninn_forget.not_true_since` (RFC3339)**: sets ValidUntil instead of soft-deleting. No new
   tool → no SEC-6 churn.
- **remember**: optional `valid_from`/`valid_until` for historical facts. Default
  `ValidFrom = CreatedAt` — retroactively legitimizes the `created_at` backdating habit.
- **Edge case (must handle):** content-hash dedup (0x28, COG-13). Re-remembering content identical
  to an EXPIRED engram must NOT reinforce the expired record — write a new engram/window. Two
  same-content facts with disjoint windows are two facts.

## Recall semantics + ergonomic surface (the TSQL2 lesson: optional decorations, no query language)
Exactly TWO optional params on muninn_recall (next to existing since/before, which stay = the
TRANSACTION axis):
| Call | Question | Gate |
|---|---|---|
| default (zero ceremony) | "what is true now" | drop facts with closed `ValidUntil ≤ now` |
| `as_of: T` | "what was true at T" | full `[from,until)` at T; supersession demotion skipped |
| `include_invalid: true` | "show history" | no drop; expired annotated + shipped superseded_by |
| `belief_at` | "what did we KNOW at T" | **DEFERRED** (transaction replay over provenance 0x16) |
Two deliberate asymmetries:
- Default gate excludes only EXPIRED facts, not future-`ValidFrom` ones (hiding a just-stored
  future fact until a clock ticks kills trust). Full interval check only under `as_of`.
- **HARD filter, SOFT ranker**: validity is binary, applied in `passesMetaFilter`
  (activation/engine.go:1915) where `created_after` lives, + a final gate after applySupersession,
  before truncation. Activation ranks only survivors. Decay stops being a recency-as-truth proxy.
- **No validity index** — post-filter like created_after; reuse the time-bounded candidate
  injection pool (engine.go:864, #607) if as_of under-retrieves. No new Pebble prefix.
- muninn_read always echoes valid_from/valid_until/is_current (teaches the two axes).

## Composition (the load-bearing decision)
Trap: if evolve STOPS soft-deleting so as_of can see predecessors, invalid facts leak into
list/find_by_entity/entity_state/where_left_off/brief — recreating the "May figure" bug per
surface. **Chosen: evolve KEEPS soft-deleting AND stamps ValidUntil.** Soft-delete = "hidden from
the present"; the validity stamp re-opens a record for time-travel ONLY. Under `as_of=T`, the
phase-6 state filter (COG-9) is relaxed to admit soft-deleted engrams carrying an explicit
validity window covering T. Records soft-deleted by `muninn_forget` (no stamp) stay hidden even
from as_of — deletion still means deletion. Confines increment-1's behavior change to recall.
- Evolve-superseded: soft-deleted (never reach applySupersession) + now stamped → as_of resurrects.
- Manual RelSupersedes on two active engrams: link stamps ValidUntil → default recall drops the
  stale fact AFTER applySupersession does head-promotion/injection (so a query matching only the
  stale phrasing still returns the current head). Legacy unstamped links keep demote+annotate.
- `muninn_restore` must clear ValidUntil (else restore is a recall no-op) — one line, in scope.
- **Dedup separation guard** (just built): add a validity clause — differing validity windows
  block a merge (they ARE a differing load-bearing token: the "when"), and the open-ValidUntil
  member wins survivor selection (the currency rule, earned for free).
- **Decay/pruning**: decay still applies to expired facts (history may FADE, never ERASED by
  invalidation). COG-15 pruner unchanged; RetentionDays visibly bounds the as_of horizon (doc it).

## Migration
ERF: NO version bump, NO rewrite (zero-filled reserved bytes = correct legacy semantics).
Tests: encode/decode/DecodeMeta round-trip, fuzz corpus, corruption, and a PIN asserting old
bytes decode to `ValidFrom==CreatedAt, ValidUntil.IsZero()`. No new keyspace prefix (no STO-1).
Invariants: amend COG-9 (as_of exception); add **COG-19**: "Invalidation is always a ValidUntil
stamp, never a delete; default recall never returns an engram whose ValidUntil ≤ now" + pin test.
Backfill of old evolve chains (walk RelSupersedes, stamp predecessor via 0x17 MigrateBuckets
framework, query.go:645): idempotent, DEFERRED to increment 2.

## First increment (one PR) vs deferred
Inc 1 — "valid-time core": (1) two fields + Importance + ERF offsets 72–91 + decode defaults +
tests; (2) evolve stamps ValidUntil (+effective_at); RelSupersedes link stamps;
forget.not_true_since; remember.valid_from/until; restore clears; (3) recall default expired-gate
+ as_of + include_invalid in phase-6/passesMetaFilter + post-supersession gate; MCP schema +
registry smoke; read echoes validity; (4) content-hash expired-hit fix; dedup guard validity
clause (coordinate with the dedup branch); (5) COG-19 + COG-9 amend + muninn_guide docs.
Deferred (named): belief_at; backfill migration; REST/gRPC/MBP/SDK param mirrors (drift
obligation); entity_timeline/entity_state validity; web UI; archive-aware as_of; validity-seeded
injection; Allen-interval/overlap queries (probably never).

## Mistakes deliberately NOT reproduced
1. TSQL2 ceremony → one optional param per question, default is the common case, no query lang.
2. Zep/Graphiti LLM-extracted validity at ingest → validity only from EXPLICIT signals (evolve,
   link, caller params). Inferred temporal metadata = silent plausible wrongness (principle #1).
3. Their four-timestamps-per-fact → don't duplicate a transaction axis we already own
   (CreatedAt + provenance), don't fake a belief_at we can't keep honest under in-place mutation.
4. Temporal-DB purism → validity is a binary gate; cognition stays the ranker; not every query
   is bitemporal.
5. Invalidation-as-deletion → stamp, never delete; hard delete stays a distinct explicit op.

## Top 2 risks
1. **Gate-coverage drift** — recall gets the gate; entity_state/where_left_off/brief/find_by_entity
   don't, and one leads with the May figure again. Soft-delete-preserving design confines inc-1 to
   recall, but ACTIVE facts invalidated via not_true_since/manual-link CAN leak. Mitigation: one
   shared `Engram.ValidAt(t)`/`IsExpired()` predicate, COG-19, a drift-guard hook entry, a PR audit
   checklist, inc-2 per-surface adoption.
2. **Axis conflation by clients** — agents read as_of as transaction time or keep backdating
   created_at. Mitigation: valid_from defaults to created_at, read/recall echo both axes, guide
   teaches both with one example each. Ship the vocabulary with the feature.

Cost: 0 bytes/record, 0 new prefixes, 0 ERF bump, no index. Runtime = two int64 compares in an
existing filter loop. Feature = metadata semantics + one stamp on three write paths + one gate on
one read path.
