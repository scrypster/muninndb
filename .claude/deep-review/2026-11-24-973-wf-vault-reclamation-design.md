# Design: reclaim expired workflow vaults (`wf-*` only) — issue #973

Status: design (no production code written). Branch:
`madeinoz67/investigate-wf-vaults-not-removing-after-time` off develop `e7b3f2ce`.
Incorporates the maintainer rulings of 2026-11-24: expiry = the cap TTL instant;
this increment ships reclamation + read-time expiry visibility on `muninn_status`;
the extend tool is a named deferral whose supported alternative is a fresh vault.

## Root cause (verified against the code, not the report)

Every claim below was read in this checkout:

- `internal/mcp/handlers.go:3433` `handleCreateWorkflowVault` registers the name
  (`RegisterVaultName`, permanent 0x0E/0x0F keys — `internal/storage/vault.go:17`),
  writes a `VaultConfig` with the `working` preset (`internal/auth/plasticity.go:393`,
  `RetentionDays: 7`), and mints a `cap_` with `expires_at` (default 168 h, floored 1 h,
  ceiling `MUNINN_WORKFLOW_CAP_TTL_HOURS`, `handlers.go:3479-3494`). **No expiry is
  recorded on the vault itself.**
- `internal/engine/engine.go:5959` `runPruneWorker` ticks every 60 s + jitter, resolves
  plasticity per vault, and calls `PruneVault` only when `MaxEngrams > 0 ||
  RetentionDays > 0`. `PruneVault` (`engine.go:5777`) hard-deletes old **engrams**.
  Nothing in the worker (or anywhere periodic) deletes the **vault**.
- The only non-test `DeleteVault` caller is the admin REST route
  `internal/transport/rest/admin_handlers.go:859` (manual, human-initiated).
- `internal/auth/capability_store.go:102` rejects an expired cap at validate time, but
  the record is only removed by manual `RevokeCapability` (`capability_store.go:139`,
  REST-only). The admin vault-delete route does **not** revoke the vault's caps either —
  even a manually deleted wf vault leaks its cap rows.

So each workflow run permanently adds: one 0x0E + one 0x0F name key, one 0x45 config
row, and ≥1 capability rows (0x40 + 0x41). This is the RFC #597 leftover the decision
record lists as "TTL-driven cap reclamation" (green-lit, unshipped). Issue #973 is that
increment plus the vault itself.

**Corrections to the briefing's claims, both verified in code:**

- Vault configs **are** replicated: `internal/auth/cluster.go:49` `SetReplicator`
  routes every auth write (`set`/`del`/`commit`) into the replication log (#596/#631).
  What is **not** replicated: the 0x0E/0x0F name keys (`WriteVaultName` and
  `DeleteVaultNameOnly` commit without `replicateBatch` — `storage/vault.go:37`,
  `storage/vault_lifecycle.go:192-197`) and `ClearVault`'s range tombstones
  (`vault_lifecycle.go:119`). That is a pre-existing cluster residual the manual admin
  REST delete shares today; this design inherits it, documents it, and defers fixing it.
- The engine's `Write` path does **not** take the per-vault mutex (verified:
  `getVaultMutex` callers are prune/clear/reindex/reembed/repair only) — see Risks.

## Mechanism

Five small pieces, all extending proven in-tree mechanisms (principle #7).

### 1. Vault expiry lives on `auth.VaultConfig` — one new optional field

`internal/auth/types.go:21`:

```go
type VaultConfig struct {
    Name       string            `json:"name"`
    Public     bool              `json:"public"`
    Plasticity *PlasticityConfig `json:"plasticity,omitempty"`
    ExpiresAt  *time.Time        `json:"expires_at,omitempty"` // wf-* lifecycle; nil = no auto-reclamation
}
```

- **Not derived from the cap at sweep time.** A cap can be revoked (record gone → no
  expiry → vault never reclaimed) and multiple caps give no single answer. Per the
  maintainer ruling, expiry is **one stored instant, set at create time from the same
  clamped `ttl_hours` the cap gets** (`handlers.go:3479-3494` computes it once;
  `expiresAt` at `:3514` goes to both the cap and the config). Vault expiry == cap
  expiry by construction.
- **Divergence policy.** The only post-create source of divergence is an operator
  manually minting a later-expiring cap through the admin mint
  (`GenerateCapability` is generic REST). That grants **access**, not **lifetime**: the
  vault expiry does not move (moving it would be an implicit extension, and extension
  is ruled a separate increment), and the sweep resolves the divergence in favor of the
  recorded vault expiry by revoking the vault's whole cap set at reclamation. One
  number, one owner: the create-time instant.
- **Not a new lifecycle record.** That needs a new Pebble prefix (Tier 3: keyspace
  registry, collision guard, migration) for zero added safety over an optional field on
  the record that already exists per vault, is already written by the create handler,
  is already cleaned up by `deleteVault` (`engine_vault.go:297` `DeleteVaultConfig`),
  and already replicates on a cluster (`cluster.go:77-106`).
- **Fail-safe by shape.** `omitempty` + pointer: every pre-existing config record (and
  every `GetVaultConfig` miss, which returns a zero value with nil error —
  `vault_config.go:131`) unmarshals to `ExpiresAt == nil` → **never a reclamation
  candidate**. A vault whose expiry cannot be read is not deleted.

No migration: JSON is additive. The plasticity REST PUT survives —
`admin_handlers.go:480-487` does read-modify-write on the whole `VaultConfig`
(`vc.Plasticity = &cfg`), so an operator tuning a wf vault's cognition cannot strip its
expiry.

### 2. The `wf-` prefix guard moves to `auth`

`auth.IsWorkflowVaultName(name)` beside `IsValidVaultName`
(`internal/auth/token.go:36`) — prefix + non-empty body + `IsValidVaultName`. The
engine cannot import `internal/mcp` (mcp imports engine); the sweep needs the same
predicate, and the engine already imports auth (`engine.go:21`) — no cycle.
`mcp.isWorkflowVaultName` (`handlers.go:3416`) becomes a one-line delegate so there is
exactly one definition (SEC-11 precedent: name rules live in `auth`).

### 3. The sweep: one engine method, called from the existing prune tick

New method `Engine.reclaimExpiredWorkflowVaults(ctx, vaults []string) int` (in
`engine_vault.go`, the vault-lifecycle file), called from `runPruneWorker`'s tick
**after** the per-vault prune loop and before `decayAllVaults` (`engine.go:5975-5983`)
— so a vault the retention pass emptied this tick can be reclaimed in the same tick:

```
reclaimExpiredWorkflowVaults(ctx, vaults []string) int
  1. refuseNonLeaderWrite() != nil → Debug log, return 0. (Cluster single-writer,
     same gate as decayAllVaults — engine.go:6026. Nil gate (standalone) proceeds:
     topology check, not auth.) No new goroutine, no new Done channel, no new drain
     seam (drift obligation 12 dodged — this is the existing worker.)
  2. e.authStore == nil → return 0 (nothing can carry an expiry).
  3. for each name in vaults:
     a. !auth.IsWorkflowVaultName(name) → continue.     // operator vaults can never match
     b. cfg, err := e.authStore.GetVaultConfig(name); err != nil || cfg.ExpiresAt == nil
        → continue.                                     // fail-safe; covers missing config
     c. !time.Now().After(*cfg.ExpiresAt) → continue.
     d. e.store.VaultHasEngrams(ws) → continue.         // grace rule (see §4)
     e. e.jobManager != nil && e.jobManager.HasActiveJobTargeting(name) → continue.
        (Pre-check per the settled shape; DeleteVault's own check at
        engine_vault.go:266 remains the enforcement floor — checking first avoids
        revoking caps for a vault whose delete would then be refused.)
     f. for each cap in e.authStore.ListCapabilities(name):
          e.authStore.RevokeCapability(name, cap.ID)    // idempotent; NotFound is fine
     g. err := e.DeleteVault(ctx, name)
        ErrVaultJobActive → Debug, continue (retry next tick)
        ErrVaultNotFound → Debug, continue (someone else finished the job)
        other err → Warn, continue (retry next tick; every step is idempotent)
        nil → Info log "reclaimed expired workflow vault", count++
```

Everything under (g) is the **existing, proven `DeleteVault`** (`engine_vault.go:246`):
`refuseWrite` (leader + append gates), `beginVaultOp`, `HasActiveJobTargeting`,
`clearVault` (per-vault mutex, processor clear boundaries, FTS/HNSW quiesce, cache
eviction, engramCount fixup), `DeleteVaultNameOnly`, `vaultMu.Delete`,
`DeleteVaultConfig`. `RevokeCapability` itself gates on `refuseNonLeaderWrite`
(`capability_store.go:140`) and `DeleteVault` on `refuseWrite`, so a follower refuses
at the first mutating step and the next tick on the leader retries cleanly. The order
is caps-then-vault so a partial failure never strands orphan cap rows behind a deleted
vault.

Two deliberate deviations from the settled shape's letter, both with in-tree evidence:

- **A method, not an inline block in the worker loop.** The project's own B2 lesson
  (`engine.go:5995-5998`, the `decayAllVaults` comment): wiring inlined in the worker
  was deleted while the whole engine suite stayed green. The method is directly
  callable by tests; calling it from the tick satisfies "in the prune worker".
- **Emptiness is a direct 0x01 range probe, not `GetVaultCount`.** New ~15-line storage
  helper `VaultHasEngrams(ws)`: iterator `First()` over `[0x01|ws, 0x01|ws+1)`. The
  count cache is what `PruneVault` trusts for MaxEngrams, but STO-30 marks counts
  uncertifiable after foreign commits, and this is a deletion decision — read the
  truth, not a maintained counter. One seek on an empty range; same predicate, stronger
  implementation.

Cost note: the sweep adds one config read and one prefix probe per **wf-\*** vault per
tick (the worker already resolves plasticity per vault per tick; operator vaults pay
only the prefix test, which is a string compare).

### 4. Grace rule and what a writer sees (issue open question 2, as ruled)

Reclaim requires **past expiry AND empty**. Concretely, a workflow agent's experience:

- **Before cap expiry:** everything works. The notice in §5 is the only new signal.
- **After cap expiry, before reclamation:** every request fails at auth with
  `capability has expired` (`capability_store.go:103`) — unchanged behavior.
- **After reclamation:** the cap record is revoked, so the same token now fails at auth
  with `invalid capability` (`capability_store.go:94`). The two states are already
  distinguishable in the error text, and **a vault-level error is unreachable for a cap
  holder**: authentication precedes vault resolution on every transport, so the
  vault-gone state can never be reported to a stale worker through the credential it
  holds. No new error plumbing is needed or added. (For an `mk_` key holder whose vault
  was reclaimed, reads return empty results and a write re-registers the name — the
  documented fail-safe resurrection in Risks.)
- A vault that still has engrams is never touched, no matter how far past expiry — the
  retention pass empties it on the working preset's own 7-day schedule, and only then
  does reclamation land. **Reclamation never destroys data a day earlier than the
  vault's own configured retention would.**
- **A workflow that needs to outlive its TTL creates a fresh vault**
  (`muninn_create_workflow_vault`, `ttl_hours` as needed, name omitted so the server
  generates one — avoiding the "vault already exists" collision against the stale
  un-reclaimed vault) and moves the work across. There is exactly one documented route
  past expiry, and it is not extension.

Residual (documented, not fixed): an operator who pins a longer-lived credential on a
wf vault (an `mk_` key, or a later admin-minted cap) and writes at the moment of
reclamation races the sweep. Writes do not take the per-vault mutex, so no lock closes
this; the window is the milliseconds between the emptiness probe and the range
tombstone — the same race class the manual admin DELETE route has today. A write that
lands post-deletion re-registers the name (`WriteVaultName` on the write hot path,
`engine.go:1979`) and resurrects a locked, default-configured vault with no expiry —
never reclaimed again, data preserved, fail-safe. An operator who pins a permanent
credential to an auto-reclaimed scratch vault has already opted out of its lifecycle;
the docs say so.

### 5. Read-time expiry visibility: `muninn_status` only (the ruled notice route)

`VaultStatus` (`internal/mcp/types.go:361`) gains two optional fields:

```go
// Workflow-vault lifecycle (issue #973). Populated only for a wf-* vault whose
// stored config carries an expiry; omitted otherwise, so every other vault's
// status response stays byte-identical.
ExpiresAt    *time.Time `json:"expires_at,omitempty"`
ExpiryNotice string     `json:"expiry_notice,omitempty"`
```

`handleStatus` (`internal/mcp/handlers.go:967`) already receives `vault` and holds
`s.authStore`; after building the status it adds, when
`auth.IsWorkflowVaultName(vault)` **and** the stored config has non-nil `ExpiresAt`
(the two guards the sweep itself uses — the notice must never promise a reclamation
the sweep will not perform, and an operator vault can never see it):

- `expires_at`: the instant, always (not only in the window) — visibility is free once
  the guard passes.
- `expiry_notice`: only within the last 24 h before expiry **and onward while the vault
  still exists** (an expired-but-not-yet-drained vault should keep saying why writes
  will fail), reading in substance:

  > this workflow vault expires at `<instant>` and is then reclaimed once empty;
  > create a fresh vault with muninn_create_workflow_vault before then (omit `name` to
  > get a generated one, or set `ttl_hours` for a longer-lived workflow) and move the
  > work across

  The wording never implies an extension exists — per the maintainer ruling, the
  supported route past expiry is a fresh vault, and the notice names that concrete
  next action.

Chosen over the alternatives the lead surveyed (verified: no extend/renew tool exists;
`muninn_intend`/THE PUSH is cue-triggered and cannot carry a time-based warning;
server-push exists at `internal/mcp/eventbus.go` but only reaches subscribed clients):
this is a few lines, no new tool, no registry/classification churn, no timer, and it
reaches any client that asks. Its cost is stated plainly: **a workflow that never
calls `muninn_status` never sees the notice** — the create response remains the only
other place expiry is reported. `muninn_status`'s tool description
(`internal/mcp/tools.go:412`) gains one clause. REST/openapi/SDK/console: no change
(`muninn_status` is MCP-only; the fields are `omitempty`).

## Minimal first slice

1. `auth.VaultConfig.ExpiresAt` field (+ round-trip/fail-safe tests).
2. `auth.IsWorkflowVaultName` + mcp delegate.
3. `handleCreateWorkflowVault` writes `ExpiresAt` (same instant as the cap); response
   `warning` string updated to say the vault is reclaimed once the capability expires
   and the engrams have evaporated.
4. `storage.PebbleStore.VaultHasEngrams`.
5. `Engine.reclaimExpiredWorkflowVaults` + one call site in `runPruneWorker`.
6. `VaultStatus.ExpiresAt`/`ExpiryNotice` + `handleStatus` wiring (§5).
7. Tests (below): engine sweep matrix, leader gate, job-race skip, idempotent re-run;
   mcp create persists expiry; status notice matrix; auth round-trip.
8. Docs: `docs/auth.md` workflow-vault section (expiry, reclamation, fresh-vault
   route, the two error texts); `internal/mcp/tools.go` descriptions (create + status);
   `internal/mcp/guide.go:112` line; new invariant in `docs/internals/invariants.md`.

### Explicitly DEFERRED

- **The extend/re-new tool.** Cost as ruled: a workflow that must outlive its TTL
  cannot be kept alive in place — the supported route is a fresh vault plus moving the
  work (recall from the old vault still works until reclamation drains it). Shipping it
  means a new mutating MCP tool: registry-parity smoke (`allMCPTools`),
  `isMutatingTool` classification, tools_test, and a new write surface — its own
  increment against RFC #597.
- **Global expired-cap GC** across all vaults (the remainder of the #597 "TTL-driven
  cap reclamation" increment) — its own sweep over 0x40/0x41, its own tests. This
  slice purges caps only of the vault being reclaimed.
- **Cap-cleanup parity for the manual admin DELETE route** (it also leaks caps today).
- **Cluster-complete reclamation**: replicating the Pebble-side deletions — the
  0x0E/0x0F name keys and `ClearVault` range tombstones (the config and cap deletes DO
  replicate). Pre-existing gap shared with admin deletes; followers keep two tiny name
  keys per reclaimed vault until a re-snapshot. Cost: follower-side `ListVaults` shows
  reclaimed names until re-snapshot; no data divergence (the vault was empty).
- **Console card / REST visibility of expiry.** Cost: expiry is visible only via
  `muninn_status` and the create response; the web console shows nothing.
- **Server-push expiry notification** (`notifications/muninn/vault_expiring` via
  `eventbus.go`). Cost: only subscribed clients would hear it anyway; deferred with the
  console work.
- **Reclaiming pre-existing stale wf vaults** (created before this lands): they carry
  no expiry → fail-safe never reclaimed. Operators clean them with the existing admin
  DELETE route. A backfill deriving expiry from old caps was considered and rejected —
  it is the fail-open direction.
- **Name-reuse tombstones**: the name is simply free again (fixes issue effect #2).
- **A prune-interval test seam**: the sweep is a directly-callable method (the
  `decayAllVaults` B2 lesson); the worker-loop wiring is unpinned by test, exactly as
  the decay wiring is.

## Invariant and cross-surface impacts

- **New invariant SEC-32** (invariants.md, Security & transport): *Automated vault
  reclamation is structurally scoped: it can only match a `wf-*`-prefixed name whose
  stored `VaultConfig` carries a non-nil `expires_at` in the past and whose 0x01 engram
  range is empty; a vault with no readable expiry (nil, missing config, corrupt) is
  never reclaimed. Reclamation is leader-gated, never runs against a vault targeted by
  an active clone/merge job, and revokes every capability of the vault before deleting
  it. The expiry notice on `muninn_status` is emitted only under the same two guards
  and names the fresh-vault route, never an extension.* COG/STO invariants untouched —
  the prune paths this rides are unchanged.
- **No new Pebble prefix** — expiry rides 0x45 (VaultCfg), caps stay 0x40/0x41. Not a
  Tier 3 change. `keyspace-registry.md` needs no new row (optionally one sentence on
  the 0x45 row noting the optional field).
- **Surfaces that move**: MCP tool descriptions (create + status) + create-handler
  warning string + guide line (text only — no new tool, so no registry smoke-test
  change, no classification change, no openapi/SDK change); `VaultStatus` gains two
  `omitempty` fields (MCP-only type); `docs/auth.md`; invariants.md.
  **Surfaces that do NOT move**: REST/openapi, SDKs, CLI, web console, gRPC, MBP.
- **Replication** (drift obligation 11): sweep is leader-gated via the existing
  `refuseNonLeaderWrite`; its cap/config mutations replicate through the auth store's
  existing hooks; its Pebble-only mutations inherit the documented pre-existing
  name-key/tombstone gap. Fail-direction check: `refuseNonLeaderWrite` returns nil when
  no gate is installed (standalone) — the correct direction for a topology check
  (`RoleUnknown` must not suppress leader-side work).
- **CI cost**: ~7 unit tests across `internal/engine`, `internal/mcp`, `internal/auth`,
  all direct-call (no sleeps, no new goroutines, no integration tag). Seconds; no new
  job; the ~10-minute budget is untouched.

## The measurable proof — pre-committed acceptance rule

Written before any implementation, per house rules. This is a deterministic mechanism
test, not a statistic: there is no tuning space, so the pre-commit is the fixture
matrix plus both kill directions.

**Fixture (one `testEnvWithAuth` engine, six vaults):**

| # | vault | expiry | engrams | expected |
|---|-------|--------|---------|----------|
| 1 | `wf-target` | past | 0 | RECLAIMED (name, config, caps all gone) |
| 2 | `wf-live` | future | 1 | untouched |
| 3 | `wf-full` | past | 1 | untouched (grace rule) |
| 4 | `wf-norecord` | none (config written without ExpiresAt) | 0 | untouched (fail-safe) |
| 5 | `operator-past` (no prefix) | past (set directly on config) | 1 | untouched — proves the **prefix** is the guard, not the expiry field |
| 6 | `operator-plain` | none | 1 | untouched |

Vault 1 additionally has two caps minted (one expired, one with a future expiry to
prove reclamation revokes the vault's whole cap set). Call
`e.reclaimExpiredWorkflowVaults(ctx, vaultNames)` **directly** — no 60 s wait, no
`time.Sleep` (testing-hermeticity, #722).

**SHIP iff**: reclaimed count == 1; vault 1's `VaultNameExists` == false, absent from
`ListVaultConfigs`, `ListCapabilities` empty; vaults 2-6 byte-identical on name
registration, engram count, and cap count; and a second sweep invocation returns 0
with no errors (idempotency).

**KILL (the mechanism is wrong) if**: any of vaults 2-6 loses data, registration, or
caps — the guard is unsound; or vault 1 survives with the mechanism enabled — the
eligibility computation is wrong; or the sweep deletes anything on a follower-gated
engine (separate test, mirror of `TestDecayAllVaults_FollowerSkipsDecay`).

**UNDERPOWERED (the test is wrong, rewrite it) if**: the result depends on timing,
sleeps, or worker scheduling; or the RED control below cannot distinguish on/off — a
test that cannot go red is a pin, not a proof.

**RED control (run before merging, record the output)**: comment out the `DeleteVault`
call (step 3g) — the matrix test must fail with vault 1 still present and reclaimed
count 0. Additionally run the matrix against a tree with only pieces 1-2 (field +
guard, no sweep): nothing is reclaimed anywhere (proves no accidental reclamation path
exists outside the sweep).

**Notice matrix (own RED)**: three `handleStatus` calls against `testEnvWithAuth` +
auth store — (a) wf vault expiring in 1 h → response carries `expires_at` and a notice
containing "muninn_create_workflow_vault"; (b) wf vault expiring in 72 h → `expires_at`
present, `expiry_notice` ABSENT (outside the 24 h window); (c) operator vault and a
no-expiry wf vault → neither field, and the JSON is byte-identical to the pre-change
response (the omitempty pin). RED: with the notice block removed from `handleStatus`,
(a) fails. A notice that mentions "extend" or "renew" fails review on sight.

**Job-race test**: register an active clone job targeting `wf-target`
(`jobManager`), invoke the sweep → vault survives AND its caps are untouched (the
pre-check at 3e fires before revocation); release the job, invoke again → reclaimed.
Pins the skip-and-retry ordering.

**Post-land operator validation (not CI)**: on a production server with stale wf-*
vaults, new wf vaults disappear within ~one retention window of their cap expiry;
pre-existing ones (no expiry record) remain until manually deleted — the documented
fail-safe.

## Top risks and how the build kills each

1. **Deleting a non-empty vault (data loss).** Emptiness is a direct 0x01 range probe,
   not the maintained counter; three independent guards must all be wrong at once
   (prefix + recorded past expiry + empty). Killed by vaults 3 and 5 in the matrix.
2. **A late write racing the sweep destroys fresh data.** Standard credentials are
   auth-dead at eligibility; the residual needs an operator-pinned long-lived
   credential on a wf vault (documented opt-out). The failure mode is fail-safe
   (resurrected locked vault / placeholder-name data, never silent data loss beyond
   the race window — same class as admin DELETE today). Documented as residual.
3. **Cluster divergence.** Leader gate (test), caps/config replicate via existing
   hooks; name keys don't — pre-existing, bounded (two tiny keys), deferred with a
   name.
4. **Clone/merge racing reclamation.** Sweep pre-check plus `DeleteVault`'s own
   `HasActiveJobTargeting` → skip, retry next tick. Killed by the job-race test.
5. **Config-edit drift stripping expiry.** Plasticity PUT is a whole-VaultConfig
   read-modify-write (`admin_handlers.go:480-487`) — verified it preserves unknown
   fields; pinned by an auth round-trip test (old JSON without the field → nil → never
   reclaimed).
6. **The notice promising the wrong thing.** Wording is pinned by the notice matrix
   (must name `muninn_create_workflow_vault`, must not mention extend/renew); guards
   mirror the sweep's own two guards so the notice can never describe a reclamation
   that will not happen.
7. **Prune-worker cost growing with stale vaults** (issue effect #4): solved as a side
   effect — stale vaults now leave the list. The sweep adds one config read and one
   prefix probe per wf-* vault per tick.

## Addendum 2026-11-24 (post-refute)

Three corrections after the adversarial pass. The design's mechanism is unchanged; two
claims in it were wrong and are corrected here rather than silently edited above.

1. **`expires_at` had a second writer, and that falsified the design's own guard claim.**
   `PUT /api/admin/vaults/config` (`handleSetVaultConfig`) decoded the full `VaultConfig`
   from the body and stored it verbatim, and `handleRenameVault` accepts any charset-valid
   name, so a body could stamp an expiry on a vault renamed into `wf-` and the sweep then
   destroyed its registration, its config and its live capabilities. The handler now carries
   the stored `ExpiresAt` forward and discards the body's, making the create path the only
   writer; pinned by `internal/transport/rest/vault_config_expiry_test.go`.
2. **The cluster deferral understated its cost.** It is not "follower-side visibility": after
   failover the promoted node still reports `VaultNameExists`, so the reclaimed name cannot be
   reused until a re-snapshot, and that node's sweep skips the vault because the replicated
   config has no expiry. Recorded in SEC-32's residual and in the PR body.
3. **The two pre-committed tests that had not shipped are now shipped**: the job-race test
   (the job guard precedes any capability revocation) and the follower-gate test. Both were
   RED-proven by removing their guard.

Also corrected: the probe's docstring claimed a prefix re-check the code does not perform
(the iterator bounds make it redundant). The claim is gone; the bounds are the argument.
