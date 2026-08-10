# Enterprise Design — MuninnDB Reflex + Product Fixes (server-first, no bandaids)

**Bar:** world-class, enterprise-grade, no hacks. Fix the product, not the client. Local proof on a labs
instance; nothing committed/pushed/PR'd. Opus + Fable must converge on this design before implementation.

**GENERIC-ONLY MANDATE:** every change must benefit ALL MuninnDB users; nothing may be scrypster-specific.
- Server changes (S1-S6) are inherently generic product improvements.
- The reflex hooks are **generic, binding-driven reference implementations** — zero hardcoded vault names,
  hosts, or paths (they read `.muninn/vaults.json`, `~/.muninn/mcp.token`, `127.0.0.1:8750`, all standard).
- The ONLY local artifact is our own `.muninn/vaults.json` binding content — which is *config every user
  writes for themselves*, never shipped.
- Therefore the reflex must reach users through the product (S7), not by hand-wiring one machine. If a
  behavior only works because of something in *our* setup, it is a bug against this mandate.

**Verified facts (this session, in code):**
- A1: `extractTagFilters` (activation/engine.go:603) handles only `tags_all`/`tags_any`; `tag_prefix` reaches
  only phase-6 `passesMetaFilter` (:1943), never candidate seeding. Tag index stores `Hash(tag)` (4-byte,
  seedTagCandidates:625) — cannot prefix/range scan.
- C: `RecordAccess` (engine.go:3244) does GetEngram→UpdateMetadata with **no casLocks** (STO-2 violation
  waiting for callers); `UpdateConfidence` (engram.go:800) is the locked pattern to copy.
- Observe: `ReadOnly` on the activation req (activation/engine.go:163,469) gates all write side-effects;
  set from `auth.ObserveFromContext(ctx)` (engine.go:2005) — credential-bound, no per-request flag.
- Export: `vaultScopedExportPrefixes` (export.go:26) omits Entity(0x1F) and all entity-graph prefixes →
  entity graph does not round-trip; root cause is #683's global keying. Backups (reset_metadata=false) verified.

---

## Server changes (the real work; all RED-first, `-race`, invariants honored)

### S1 — Reliable tag-range recall (the reminder fix, at the source)
**Problem:** due-date reminders are best-effort semantic recall because `tag_prefix`/range never seeds candidates.
**Design:** add an ordered raw-tag index — key `RawTagIndex(ws, tagKeyPrefix, tagValue, engramID)` where the value
is stored **unhashed and lexically ordered** so `due:` + `lte:2026-07-27` is a bounded range scan. Wire it into
`seedTagCandidates` alongside the existing hashed `tags_all/tags_any` paths, activated when a filter carries
`tag_prefix`/`lte`/`gte`. Keep the hashed index for exact `tags_all/tags_any` (fast, small); the raw index is
additive, populated on write for tags containing a `:`.
**Obligations:** new Pebble prefix → STO-1 (add to `prefix.All()` + disjointness tests + keyspace-registry.md);
add to `vaultScopedExportPrefixes`; drift-guard. **Open question for panel:** raw-tag index size/write cost vs.
gating to only `key:value`-shaped tags; backfill for existing tagged engrams (migration or lazy).
**RED:** seed N due-tagged memories that are semantically unrelated to any phrase, backdated 30d, never
accessed; `recall(tag_filter{prefix:"due:",lte:today})` returns all N. Fails today.

### S2 — #682 reinforcement, STO-2-safe
**Design:** `storage.TouchAccess(ctx,ws,id)` — acquire `casLocks.For(id[:])`, GetEngram, `UpdateMetadata`
bumping only AccessCount+1/LastAccess=now with all other fields read **under the lock** (keeps 0x0B index
consistent, STO-3). `RecordAccess` becomes a thin wrapper (fixes the latent race before it has callers).
Wire fire-and-forget from: `engine.Read` (by-id) when not observe + plasticity allows; `RecordFeedback`
when `useful==true` (COG-10 untouched — confidence still never moves). **Recall never reinforces (COG-12
stands).** Plasticity gate `ReinforceOnRead bool` (default true; clamped per COG-1/2). Observe-mode never touches.
**RED:** `TestRead_ReinforcesAccess` (AccessCount 0→1, where_left_off order moves); `TestFeedbackUseful_Bumps`;
`TestRecall_DoesNotReinforce` (20 recalls → 0, pins COG-12); `TestTouchAccess_ConcurrentWithCAS` under `-race`
(final state `completed`, count preserved). All fail today.
**COG-12 amendment** documented in invariants.md.

### S3 — `read_only` request flag (observe for any caller)
**Design:** optional `read_only bool` on `muninn_recall`/`muninn_where_left_off` (and REST activate). When true,
set `actReq.ReadOnly=true` regardless of credential mode → skips Hebbian/PAS/activation-log (COG-11). Lets the
brief peek without teaching the brain. **Panel question:** should a write/full credential be *allowed* to request
read_only (yes — it's a strict de-escalation, always safe), and should observe-credentials be forbidden from
requesting read_only=false (yes — cannot escalate). Enforce: effective ReadOnly = credentialObserve || reqReadOnly.
**RED:** `TestRecall_ReadOnly_NoHebbian` (co-activation weights unchanged); `TestReadOnly_CannotEscalate`.

### S4 — #684 L1: tags in read responses
**Design:** populate `Memory.Tags` in `activationToMemory` (convert.go:24) and add `Tags` to `WhereLeftOffEntry`
(engine_adapter.go:392). Additive. **Obligation:** types.go → SDK drift (#3): expose in Python/Node/PHP response
types. **RED:** `TestRecall_ReturnsTags`, `TestWhereLeftOff_ReturnsTags`.

### S5 — #684 L2: salience-aware where_left_off
**Design:** `exclude_type_labels []string` param on `muninn_where_left_off`, default `["session","session-log"]`,
applied in the scan so low-salience orientation noise never reaches clients. **Panel question:** default-exclude
vs. opt-in — default-exclude is the correct product behavior (orientation ≠ activity log) but is a behavior change
for existing callers; gate on an explicit param with a sensible default and document. **RED:**
`TestWhereLeftOff_ExcludesSessionLogs`.

### S6 — #683 entity keying + export (SCOPING DECISION)
Vault-scope the 0x1F entity keys and add entity-graph prefixes to export. This is a **Tier-3 on-disk migration**
(re-key existing global 0x1F per vault by replaying the reverse index, behind a version bump). **Recommendation:
NOT in this sprint** — a keyspace migration crammed alongside the reflex work is the opposite of enterprise-grade;
it deserves its own focused sprint with its own migration test matrix. Track as the next sprint. **Panel: confirm
or overrule.** (If overruled-in, it gates everything and the timeline triples.)

### S6b — Cross-cutting identity/preferences tier (surfaced in EVERY brief)
**Problem (found live 2026-07-27):** durable cross-cutting facts — the user's identity and standing working
preferences (e.g. "the bar is world-class, no hacks") — get written to whatever project vault is active, so
they surface only there. A universal preference must orient every session regardless of project. Today the
binding read-list is project-scoped only; there is no shared identity tier.
**Design (generic):** the reflex resolves an implicit shared vault (default `personal`, configurable) and
always includes it in the brief's read set, tagged/typed as `identity`/`preference`, capped tightly (1-2 lines)
so it orients without dominating. `muninn init` seeds it and every binding inherits it unless opted out.
Every MuninnDB user has cross-cutting identity/preferences; this is not scrypster-specific.
**Panel question:** implicit-always-include vs. an explicit `include_shared` in the binding; how the flush
decides project-vault vs. identity-tier routing for a preference (a preference about *how to work* is
identity-tier; a preference about *this project's stack* is project-tier).

### S7 — Delivery to all users (the reflex must ship, not be hand-wired)
Per the generic-only mandate, the reflex reaches users through the product installer, not manual setup:
- **`muninn init --here`** (repo-aware): detect the git repo, create/bind a vault, write `.muninn/vaults.json`
  (read-list + write_default + optional route), prompt for a 2nd vault (multi-vault), ensure the reflex hooks
  are installed for the detected host. Extends the existing `cmd/muninn/setup_ai.go` host-detection.
- **Host-aware reflex install:** for Claude Code, write the SessionStart brief + Stop flush hooks (generic
  reference implementations, parameterized only by the binding). For hosts without hooks, degrade to the
  existing `muninn_guide` teaching. Reuses setup_ai's per-host file-writing (it already writes CLAUDE.md +
  OpenClaw skill + MCP config).
- The hook scripts ship **inside the muninndb repo** (e.g. `contrib/reflex/` or embedded), copied out by the
  installer — so there is one source of truth and updates ship to everyone.
**Panel question:** this-sprint vs. next-sprint. Recommendation: the hooks + `muninn init --here` are in scope
(that is *how* the work benefits all users); the broader setup_ai host-matrix polish can be incremental.

---

## Client changes (thin + correct once the server is right)

### Brief hook (`~/.claude/hooks/muninn-brief.mjs`)
- Uses S3 `read_only:true` (observe peek — no Hebbian pollution from the reflex layer).
- Uses S1 `tag_filter{prefix:"due:",lte:today}` — now reliable server-side; **no client date enumeration.**
- Uses S5 `exclude_type_labels` — server-side salience; the client `isLowSalience` filter is deleted, not trusted.
- Uses S4 tags in responses where useful.
- **Legitimate client robustness (not bandaids):** A3 SSE parse scans all `data:` frames for `id:1`+`result`;
  A4 per-call `-m` = remaining-deadline so 2.5s is a hard wall; A5 git-worktree resolution (read `.git` file →
  commondir → main worktree root; stop walk at `$HOME`); A6 inject full brief on `startup`/`clear`, DUE-only on
  `resume`/`compact`; A7 cap lines ~180ch / brief ~1500ch; guard: no-op when `MUNINN_FLUSH_CHILD` set.

### Flush hook (`~/.claude/hooks/muninn-flush.mjs`) — Stop hook, guards in order
0 `stop_hook_active` → exit. 1 `MUNINN_FLUSH_CHILD` → exit. 2 `CLAUDE_CODE_REMOTE` → exit. 3 no binding → exit.
4 build digest (user/assistant text only, no tool bodies, ≤40KB tail-weighted); <8 turns or <4KB → exit `skip:small`.
5 atomic `mkdir` lock on sha256(repo+date+digest-head); EEXIST&<1h → `skip:dup`; >6/repo/day → `skip:cap`.
6 `spawn('claude',['-p',prompt,'--model','sonnet','--allowedTools','mcp__muninn__muninn_remember_batch,
muninn_recall,muninn_evolve,muninn_feedback,Read'],{detached,stdio:'ignore',cwd,env:{...,MUNINN_FLUSH_CHILD:1}}).unref()`.
Child: reads digest only; recall-before-write; **dedup-hit → `feedback(useful=true)`, write nothing** (the honest
reinforcement channel for S2); evolve for updates; 3-10 typed memories + one `type_label=session-log`; forbidden
content list; success line to `~/.muninn/flush.log` from the **child** (parent is detached). Model sonnet v1;
haiku only after a week of audited flush.log. Daemon down → non-zero, no fingerprint, lock TTL allows retry.

---

## Labs runbook (binding — live daemon never touched)
1. Verify all backups `reset_metadata=false` (done). 2. Foreground labs: distinct `MUNINN_MCP_TOKEN`, `--data
~/muninn-labs/data`, alt ports 9474-9750. 3. All CLI via `muninn-labs` wrapper (MUNINNDB_ADMIN_URL/UI_URL set);
never bare `muninn` in labs terminal; labs never added to any MCP client. 4. Import → poll job → **restart labs**
→ proofs. 5. Record entity-loss on import as the #683 backup-integrity finding.

## Acceptance criteria (measurable, local, honest)
1. Reminders: 10 adversarial due items → 10/10 in next brief (S1). 2. Brief p95 ≤2.5s hard across 20 cold starts,
0 malformed, 20/20 no-op with daemon down. 3. Reinforcement: 4 RED tests green; labs 5 reads move a memory to
where_left_off top-3 (count==5); 20 recalls leave control at 0; `-race` clean. 4. Observe: brief peeks change no
Hebbian weight (S3 test + labs check). 5. Flush: ≥8/10 sessions produce 3-10 mems; 0 secrets/code/quotes; re-run
writes 0; child session 0 flushes w/ skip line; every trigger logged. 6. Honesty: brief always "verify before
acting"; restore drill documents entity loss. 7. All invariants pass: STO-1/2/3, COG-1/2/10/11/12 tests green.

---

# RECONCILED DESIGN (Opus + Fable, stricter position — AUTHORITATIVE, supersedes drafts above)

All four load-bearing claims verified in code 2026-07-27. Both panels converged on the biggest risks;
reconciliation takes the stricter verdict on every split. Nothing is implemented until this section is the plan.

## S0 — NEW PREREQUISITE (both reviewers, verified): thread credential mode into MCP ctx
Pre-existing bug: `internal/mcp/` never injects `auth.ContextMode` (gRPC server.go:172 / REST middleware.go:49
do). So `ObserveFromContext` is always false on MCP → observe creds fire Hebbian/PAS today (COG-11 violation),
and S2/S3 observe guarantees are unenforceable on the reflex's transport. **Fix first**: inject
`ctx = context.WithValue(ctx, auth.ContextMode, a.Mode)` in MCP dispatch (server.go ~:335) before handler.
RED: `TestMCP_ObserveCredential_NoHebbian` (fails today). This is also a standalone security fix.

## S1 — tag range index — APPROVE W/ CHANGES (all required)
- **New prefix 0x2B+** (a range index cannot live under fixed-width 0x0C — Opus verified query.go:366 parsing).
- **Key encoding, exact:** `newPrefix|ws|Hash(tagKey)4B|value|0x00|id` (fixed-width key segment like 0x0C/0x0D,
  no framing ambiguity, no raw-key-bytes leak) — resolves the prefix-of-each-other + delimiter traps (both).
- **Split tag on first `:`** into (key,value); range applies within one tag-key.
- **Lexical range only, documented + RED-tested** with prefix-of-each-other values AND non-ISO dates
  (`due:2026-7-4` lexical≠chronological). Gate range ops to lexically-sortable shapes.
- **Full obligation set (STO-1/STO-6):** `prefix.All()` + disjointness; ALL four pinned lists —
  `clearVaultDataPrefixes` (MANDATORY, else resurrection bug), `vaultScopedSwapPrefixes` (clone),
  `vaultScopedExportPrefixes` (export), `clearFTSKeysPrefixes` (N/A) — update `prefix_lists_test.go`; ALL
  write/delete/evolve sites (batch.go:128, impl.go:320/485, engram.go:475/732); registry.md.
- **Eager backfill** (BucketMigration-style cursor, version-gated) — REQUIRED; lazy fails the pre-upgrade
  acceptance case. Gate index writes to `key:value`-shaped tags to bound write-amp (2 entries/tag).

## S2 — reinforcement — RECONCILED (stricter = Fable's channel deletion)
- **`TouchAccess(ctx,ws,id)`**: acquire `casLocks.For(id[:])`, GetEngram, `UpdateMetadata` under lock
  (AccessCount+1/LastAccess=now, all else read under lock). `RecordAccess` → thin wrapper.
- **Replace the UNLOCKED dedup call site (engine.go:922) with TouchAccess** — else the STO-2 fix is cosmetic
  (Fable A-3, live race today).
- **DELETE the recall-similarity dedup→feedback channel** (Fable A-2, over Opus's keep). Reinforcement sources:
  (a) explicit read-by-id (`engine.Read`), (b) exact content-hash reinforcement on write (existing, now locked).
  A flush that re-encounters a topic ATTEMPTS the write: identical content reinforces via content-hash; changed
  content routes to `evolve`/new. No similarity-without-identity feedback ever.
- **Kills the self-confirmation loop (Fable A-1):** with the fuzzy channel gone the flush no longer re-feeds
  brief content. Additional guards: flush digest strips brief-injected lines; per-memory reinforce cap 1/day.
- **Observe gate that actually works** (depends on S0): gate BOTH the new TouchAccess AND the existing
  unconditional Read feedback signal (engine.go:1851, Fable A-4) on `!observe`.
- **Fire on `e.stopCtx` via `spawnFireAndForget`** (engine.go:639 pattern), not request ctx.
- **Plasticity gate** `ReinforceOnRead` (default true, clamped COG-1/2). **Recall never reinforces (COG-12 stands).**
- Do NOT over-claim STO-2: `restore` (engine.go:2830) and `consolidation/dedup.go:186` are pre-existing unlocked
  state mutators — carve out explicitly or bring under lock; don't assert blanket compliance.
- RED: Read-reinforces; Feedback(useful)-bumps; Recall-does-not (pins COG-12); TouchAccess||CAS under -race;
  MCP-observe-no-reinforce (needs S0).

## S3 — read_only flag — APPROVE W/ CHANGES
`effective ReadOnly = credentialObserve(a.Mode) || reqReadOnly`. Enforce at the MCP handler using `a.Mode`
(needs S0), reject observe-cred + `read_only=false` (cannot escalate), OR the flag into `actReq.ReadOnly`.
MCP + REST uniformly. Scope acceptance to Hebbian/PAS/activation-log (COG-11's phase-4.75 wound is pre-existing).

## S4 — tags in responses — APPROVE W/ CHANGES
`Read` already returns Tags (engine.go:1904). Scope = recall + where_left_off. Requires **`Tags` on
`mbp.ActivationItem`** + populate at engine.go:2146, THEN convert.go:24 + engine_adapter.go:392 (Opus B4 — two
wire types, not one). SDK drift across ALL six SDKs (go/kotlin/node/php/python/swift) or documented exclusion.

## S5 — salience-aware where_left_off — APPROVE W/ CHANGES
Ship `exclude_type_labels` mechanism; **default `[]` (no server-side exclusion)** — a non-empty default is a
breaking contract change and hardcodes reflex policy in the server (both). Policy lives in the client.

## S6 / #683 — DEFER re-key (both concur), but MANDATORY this sprint:
(a) public #683 update + backup-docs warning that `vault export` does NOT round-trip the entity graph (done —
issue comment posted); (b) evaluate **S6a**: export entity records reachable via vault-scoped 0x20/0x23 links
WITHOUT re-keying (feasible pre-migration). Silent lossy backup + shipped installer violates the bar; loud
disclosure is the floor. Reflex must not depend on entity round-trip.

## S6b — cross-cutting identity/preferences tier — APPROVE (design item)
Every brief read-set implicitly includes a shared identity/preferences vault (default `personal`), tightly
capped, so standing preferences orient every session. Flush routes "how to work" prefs → identity tier,
"this project's stack" → project tier.

## S7 — delivery — APPROVE W/ CHANGES (honest reframe)
The **server work (S0–S5) is the generic product benefit.** The hooks are **opt-in Claude-Code reference
implementations** with `muninn_guide` fallback for non-hook hosts — the flush `spawn('claude','-p',...)` is
Claude-Code-CLI-specific by construction and spends the user's tokens, so: **explicit opt-in at install with
cost + data-flow disclosure, default OFF; model/binary configurable (not hardcoded sonnet); drop `Read` from
child --allowedTools; mechanical secret-redaction pass over the digest before spawn (not a prompt promise);
each `--allowedTools` entry fully `mcp__muninn__`-qualified.** Defer `muninn init --here` multi-vault UX.

## Flush guards (final): parent logs spawn attempt (observability); child removes lock on failure (else
1h unretryable, Opus N5); digest = user/assistant text only + redaction; sonnet v1.

## Acceptance criteria (replacing the gameable set)
1. Reminders on PRE-UPGRADE data: seed due-tags BEFORE the migration, upgrade, then 10/10 surface (not fresh).
2. Observe non-interference over **MCP** (not REST): brief peek changes 0 Hebbian weights; RED at the MCP layer.
3. Reinforcement: 4 RED green; labs 5 read-by-id → count==5 + where_left_off top-3; 20 recalls → 0; -race clean.
4. Reinforcement-distribution guard: after N simulated sessions, flush-fed memories do NOT monopolize
   where_left_off top-K; AccessCount share bounded (the anti-self-confirmation measure).
5. Brief RELEVANCE (the real "sentient vs technically-works"): across 20 sessions, ≤1 brief item the user
   would correct; every item traces to a real stored memory.
6. Flush: mechanical redaction proven (seeded fake secret never reaches a memory); re-run writes 0; child
   session 0 flushes w/ skip line; every trigger (incl. parent spawn) logged.
7. Latency p95 ≤2.5s hard; daemon-down → 20/20 clean no-op. Honesty banner always present.
8. Backups: restore drill documents entity-graph loss; S6a evaluated.

## Build order (dependencies): S0 → S2/S3 (need S0) ; S1 (independent, largest) ; S4/S5 (independent) ;
then client hooks (need S1/S3/S4/S5) ; then flush (opt-in) ; labs proof each server change ; dual impl review.

---

# CAPTURE MODEL (expanded per MJ 2026-07-27 — panel must confirm/complete)

Not a fixed "2 tiers." A **salience-driven** model with two mechanisms, tiered by source-confidence.

**Mechanism A — event-driven immediate capture.** Fires when something DURABLE crystallizes, regardless of
source. Trigger = salience (durable + non-obvious), not who originated it:
- **User standing directives/preferences** — `source_type=human`, `trust=stated`, confidence 1.0.
- **Decisions** (choice + rationale) made in-session.
- **Agent-originated learnings** — research results, reasoning conclusions, discoveries that shape future work
  and were never explicitly stated by the user — `source_type=agent`, `trust=inferred`, confidence <1.0.
Captured immediately so it survives compaction/crash and informs the rest of the session.

**Mechanism B — rollup backstop (flush, session end + optional periodic).** Catches durable items A missed +
extracts implicit session patterns. Also source-tiered.

**Anti-pollution (uses EXISTING MuninnDB primitives — not new machinery):** `source_type` human|agent,
`trust` stated|inferred|untrusted, `confidence`. Recall/brief weight by trust so agent-inferred learnings
accumulate but never outrank human-stated facts; same decay applies. This is how the brain "learns" without
its own guesses dominating.

**Works for all users:** every user wants a brain that remembers what they said AND what the agent figured out;
the agent-learning tier is what makes it feel like accumulated expertise vs. a dictation log. Generic.

**OPEN FORKS for the panel (both must agree):**
1. Do agent-originated findings capture IMMEDIATELY (event-driven, MJ's instinct — risks volume/low-confidence
   pollution) or ALWAYS via the rollup (batched, judged, volume-controlled)? Or a threshold: findings that
   change a decision/approach → immediate; incremental reasoning → rollup. Settle it.
2. Is the taxonomy COMPLETE (user-directive / decision / agent-learning + rollup backstop), or is a class missing
   (e.g. commitments/promises made TO the user; corrections/retractions; cross-session pattern-learning that only
   emerges over many sessions and no single session's flush can see)?
3. Does event-driven agent-capture reopen the self-confirmation risk (agent persists a finding → brief surfaces
   it → agent "confirms" it next session)? How does trust-weighting + the S2 reinforcement rules interact?
4. Generic-mandate check: does agent-learning capture behave sanely for a user whose agent is a different host
   (Cursor/OpenClaw) with no flush hook — does Mechanism A alone degrade gracefully?

---

# FINAL — LOCKED (Opus + Fable both CONFIRM implement-ready; deltas below are binding)

Both confirmation panels verified every load-bearing claim in code and returned CONFIRM on S0–S7. Build order
locked: **S0 → S2/S3 → S1 ∥ S4/S5 → client hooks → flush → labs proof → dual impl review → acceptance.**
Apply these reconciled deltas (stricter union) before/within implementation:

D1. **S2↔flush contradiction (BOTH flagged, blocking).** Delete "dedup-hit → feedback(useful=true), write
    nothing" from the flush child spec. Replace: child ALWAYS attempts the write — identical content reinforces
    via content-hash (locked TouchAccess), materially different routes to `evolve`. Drop `muninn_feedback` AND
    `Read` from child `--allowedTools`; each remaining tool fully `mcp__muninn__`-qualified. The fuzzy feedback
    channel must exist in neither server nor client.

D2. **`read_only` extends to `muninn_read` (Fable — closes the #1-most-likely-to-bite loop).** The
    brief→"verify before acting"→`muninn_read`→TouchAccess→LastAccess→where_left_off loop is not closed by the
    cap. `muninn_read` gets `read_only bool`; when true it skips TouchAccess AND the engine.go:1851 implicit
    feedback. The brief instructs: verification reads of brief-surfaced items use `read_only:true`.

D3. **Reinforce-cap scope (resolves cap vs acceptance #3).** The 1/day per-memory reinforce cap applies to the
    content-hash/flush write channel ONLY. Explicit read-by-id is uncapped → acceptance "5 reads → count==5"
    stands. State this in spec text so the implementer can't pick at random.

D4. **S8 (NEW, both): `trust` write-param on `remember`/`remember_batch`.** Validated against the real enum
    `verified|inferred|external|untrusted` (storage/types.go:293 — NOT "stated"); setting `verified` requires a
    write/full credential. Without it every capture lands `inferred` and the anti-pollution tiering is theater.
    `source_type` is provenance-derived (not a write arg), so **trust is the discriminator.**

D5. **Capture taxonomy — add `commitment` class (both).** Commitments/promises made TO the user are a distinct
    immediate class (`type=commitment`), surfaced in the brief as an open loop. Corrections are NOT a class →
    always `evolve`/supersede, never a sibling. Cross-session patterns → consolidation tier only.

D6. **Consolidation leg (both): the model is immediate + rollup + CONSOLIDATION (episodic→semantic).**
    `type_label=session-log` = episodic; typed memories = semantic; `muninn_consolidate`/dream promotes recurring
    episodic → semantic and is the ONLY place cross-session patterns surface. Route there; finishing dream gates
    (dream.go:32 TODO) is the enabler, tracked separately — not built this sprint.

D7. **Gold-plating cuts (Fable).** (a) NO server-side trust-weighted scoring this sprint — the brief tier-sorts
    client-side using `Trust`/`SourceType` already on `ActivationItem`. (b) NO periodic flush v1 — session-end
    only. (c) S6b routing = one line (explicit "how to work" → identity tier; else project tier), not a classifier.

D8. **Fork-1 rule (both converged): THRESHOLD.** Agent finding captured immediately iff (a) decision/approach/
    constraint/commitment-altering for a FUTURE session AND (b) not reconstructable from an already-stored memory
    (and not re-derivable in ~1 tool call). Else → rollup. Hard budget ≤5 immediate agent-captures/session.
    Mechanism A also does recall-before-write to avoid re-deriving the same finding as a new sibling.

D9. **S1 encoding hardening (both).** Reject/escape 0x00 in tag values at write (RED-tested); `value|0x00|id`
    with upper bound `prefix|X|0x01`. Document lexical-only range in `muninn_guide` (can't enforce server-side).
    Eager backfill + all four pinned lists + pre-upgrade acceptance case land together.

D10. **Acceptance #5 operationalized (Fable):** "brief item the user would correct" = fails (a) traceability
    (maps to a memory ID whose content entails the claim) OR (b) staleness (a newer superseding/contradicting
    memory exists). Both mechanical/auditable → the 20-session run is a count, not a vibe. Acceptance #4 computed
    per source_type/trust tier (bound agent-inferred share of where_left_off top-K).

STATUS: design locked. No further design review needed. Proceed to S0.
