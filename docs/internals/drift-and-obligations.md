# Cross-surface obligations, drift risks, and the CI map

MuninnDB has many surfaces that must stay in sync but mostly *aren't* checked automatically.
This is where "you changed X but didn't update Y" bugs live. A reviewer must walk this list
for any PR that touches a synced surface.

Obligations marked 🪝 are additionally warned about by `.claude/hooks/drift-guard.mjs`, a
PostToolUse hook that fires when Claude Code edits the triggering path. It is a reminder,
not a gate: it warns once per session, stays quiet if you have already touched the
corresponding surface, and never blocks. The unmarked obligations need judgment or a build,
and remain the reviewer's job.

## "If a PR touches X, it must also do Y"

1. **An MCP tool handler** (`internal/mcp/*.go`) → update `allMCPTools` in
   `cmd/muninn/smoke_exhaustive_test.go` and run `go test -tags integration,localassets
   ./cmd/muninn/...` (the registry-parity smoke test). Also classify the new tool in
   `isMutatingTool`/`isReadOnlyTool` (`internal/mcp/context.go`) — the coverage test
   enforces this, but flag it early.

2. **A REST route or handler** (`internal/transport/rest/`) → update
   `internal/transport/rest/openapi.yaml` and pass `npx @redocly/cli lint`. The CI
   route-count parity check is **informational only** (±5 tolerance) and cannot see
   field-level drift — so this is a manual reviewer obligation, not a gate. 🪝

3. **REST request/response types** (`internal/transport/rest/types.go`) → check
   `sdk/python`, `sdk/node`, `sdk/php` (and Kotlin/Swift/Go) for the corresponding change.
   **There is no automated check** — nothing verifies SDK parity in CI. Claude Code users
   get a warning from `.claude/hooks/drift-guard.mjs`; otherwise manual. 🪝

   **Recall supersession/currency annotations are MCP + MBP only, deliberately** (COG-22,
   COG-25, COG-28). `SupersededBy`/`CurrentVersion` and `substituted_for`/
   `substitution_basis`/`chain_truncated`/`head_not_indexed_yet` (all ASSERTED — the last
   four are COG-28 version-head substitution, #763) and `possibly_superseded_by`/
   `version_cluster`/`newest_of_cluster`/`cluster_size` (advisory, COG-25) live on
   `mbp.ActivationItem` + `mcp.MemoryAnnotations`; REST inherits them via the
   `rest.ActivateResponse = mbp.ActivateResponse` type alias, so it is automatically in sync.
   Since #764 the same list carries `unresolved_contradiction` (per-row, ASSERTED, COG-29)
   and the RESPONSE-level `conflict` block. Since #773 it also carries `relevance_band` /
   `relevance_band_basis` (per-row, absolute relevance) plus `absolute_score` and
   `content_match` on `mcp.Memory` — the last two had existed on MBP/REST since COG-26 and
   were invisible to MCP entirely. **`relevance_band` is deliberately TOP-LEVEL on
   `mcp.Memory`, not inside `MemoryAnnotations`**, precisely so the allocation predicate
   below cannot drop it; note it could not be called `relevance`, which is already taken on
   both `mbp.ActivationItem` and `mcp.Memory` by the engram's stored decay strength.
   Two traps that list does not protect you from,
   both hit in #764 and both now pinned by tests: `internal/mcp/convert.go`'s
   "should I allocate an annotations object at all" predicate must name the new field or a
   row carrying no OTHER annotation drops it silently
   (`TestRecallOverMCP_ConflictBlockAndAnnotations`), and `internal/mcp/handlers.go`'s
   recall response is a hand-built `map[string]any` that is NOT a mirror of
   `mbp.ActivateResponse` — a response-level field added to the struct alone reaches REST
   and vanishes on MCP.
   **proto/gRPC and the non-Go SDKs carry NO supersession/currency annotation fields at all**
   (their `ActivationItem` is a minimal subset that never had even `superseded_by`). Do NOT
   "complete" the schema by adding only the currency fields there — an unpopulated field the
   adapter never fills is the silently-wrong class (principle #2). Add the whole annotation
   block, wired end-to-end, or nothing.

4. **A plasticity preset value** (`internal/auth/plasticity.go`) → update the web-UI preset
   cards (`web/templates/index.html`) and the JS descriptions/radar data
   (`web/static/js/app.js`), and any docs table. Presets are **hand-duplicated** across Go
   and the web UI with no parity test — a known drift surface. If you add a preset, also
   add a `reflect.DeepEqual`-style pinning test if it's derived from another (see #599). 🪝
   Preset *names* are pinned across Go and all four web sites by
   `TestPlasticityPresets_WebConsoleParity`; values are still on you.

5. **The embed model or ORT version** (`Makefile`) → update **all** of: `ci.yml` cache keys
   (Linux *and* Windows jobs), `release.yml` matrix cache keys (5 platforms), and the
   `docker-compose.yml` comment. **These are already drifted today** — see the live-drift
   list below.

6. **`cmd/muninn/upgrade.go`** → upgrade integrity is checksum-verified as of #600. The
   ordering inside `selfUpdate` is the security property, not the presence of a hash:
   `verifyChecksum` must stay **before** `verifyBinary`, because `verifyBinary` executes
   the downloaded file (`<path> version`). A PR that reorders those, or that makes a
   missing/unmatched checksum non-fatal, reopens the hole. Don't accept an executable-bit
   plus version-string check as "integrity" — that is what the gap was.

7. **`proto/muninn/v1/service.proto`** → regenerate `proto/gen/go/...` via `make proto`.
   CI gates this as of #873: the `proto-check` job runs `make proto-check` (back up
   `proto/gen`, regenerate, diff) and fails on drift. The reviewer no longer has to
   eyeball it. 🪝

8. **A new Pebble prefix** → see `keyspace-registry.md`: disjoint, and added to
   `internal/prefix/prefix.All()` (the single source of truth). The disjointness tests in
   `internal/prefix/prefix_test.go` auto-tighten — there is no `storageMaxPrefix` to bump.
   Two things that do NOT auto-tighten: the hard-coded `owners` slice in
   `TestAll_OwnerGroupsPairwiseDisjoint` (a new owner string silently opts out of the
   pairwise check until it is listed) and the `named` slice in
   `TestAll_ConstSliceComplete`. Also check whether the prefix is vault-scoped: if it is,
   it belongs in one or more of the four lists pinned by
   `internal/storage/prefix_lists_test.go`; if it is global (as `prefix.Replication` is),
   say so in the registry row so the next reader does not have to re-derive it. And obey
   STO-14's general rule: **never mix fixed-width hash-keyed with fixed-width
   sequence-keyed records under one prefix** — that was #726.

   **Changing the SCOPE of an existing prefix is the same obligation plus a migration.**
   #683 moved 0x1F from global to vault-scoped: that meant a new key length, the
   `prefix.All()` category, membership in `clearVaultDataPrefixes` (and its scope test),
   the registry row, an invariant (STO-17), and migration v6 — plus a `MaxRegisteredVersion`
   bump that a previous migration's registration test pinned to an exact value and therefore
   broke. Check for that pin: `TestRegisterMigrations_IncludesV5` asserted `== 5`. Pin the
   exact value in the NEWEST migration's test and assert only a floor in the older ones.

9. **Any `go build` of the muninn binary** → must keep `-tags localassets`. Enforced by
   `scripts/check-build-tags.sh` (CI `shellcheck` job), but that script only scans the
   Dockerfile, Makefile, workflows, and one test file — a build command added elsewhere
   slips through. Flag manually.

10. **A user-facing doc promise** (`docs/quickstart.md`, `self-hosting.md`, `auth.md`,
    `tls.md`, `capabilities.md`) → verify example commands still match current CLI flags in
    `cmd/muninn/`.

11. **Anything under `internal/replication/` or a new write path in a cluster deployment** →
    confirm it goes through `RepLogAppend` and is leader-gated or Cortex-originated (bug
    #596 is exactly the absence of this). Background workers that mutate replicated state
    (the pruner) inherit the obligation. Since #826 the callback is built by
    `replication.LocalAppendFunc`, which suppresses the append on a node whose role is
    *definitively* follower — **know which way a role gate fails.** `IsFollower()` is
    false for `RoleUnknown` on purpose: suppressing leader work on a node that turns out
    to be the leader loses data, while doing it on a follower only wastes resources. A gate
    written the other way (`if !IsLeader() { skip }`) would drop every write accepted during
    the startup window out of the stream its followers read, silently and permanently.
    Equally: anything that filters keys out of a snapshot (`skipFromSnapshot`) must keep
    replication METADATA flowing — dropping the seq counter restarts a promoted node's log
    at 1. And **anything that makes a node leader goes through `handlePromotion`**: its
    `takeLeaderRole` raises the sequence counter to the node's applied position before the
    role flips (STO-26). A second place that gave a node the leader role would promote with
    a stale counter, and every follower would discard that leader's writes as already
    applied while reporting no lag. `TestOnlyTakeLeaderRoleAssignsThePrimaryRole` fails on it.

12. **A new async worker or fire-and-forget path on the write or scoring path**
    (`internal/engine`, `internal/engine/activation`, `internal/storage`) → give it a
    `WaitIdle`/`Flush` seam, fold it into `Engine.waitWriteTimeIdle()` (or document it as
    its own drain if it doesn't fit that call), and add it to the async-source table in
    `testing-hermeticity.md`. Any test asserting on that worker's output must drain it
    deterministically — `time.Sleep` is not synchronization and flakes under `-race` on
    constrained CI cores (#722).

13. **A new field on `activation.ActivateRequest` that gates a scoring phase**
    → audit EVERY constructor (`rg 'activation\.ActivateRequest\{'`) and add a pinning
    test that the production path (`internal/engine.Activate`) sets it. There is exactly
    ONE production constructor and dozens of test ones, so a bool defaulting to `false` is
    a silent behaviour change for every in-tree fixture that relied on the phase. COG-32's
    `HebbianEnabled` did exactly this: four fixtures seed `AssocLog().Record(...)` plus
    `store.assocs` and their priming went inert until they were updated with the gate, and
    one of them (`abstention_gate_measure_test.go`) reported a collapsed both-arms-equal
    result that its own both-metric rule correctly rejected. That failure is the desirable
    shape — but only because a standing corpus happened to cover it.

14. **A trial/measurement seam behind `//go:build cognitiontrial`**
    (`internal/storage/trial_clock_cognitiontrial.go`,
    `internal/cognitive/replay_cognitiontrial.go`,
    `internal/engine/activation/legacy_clock_cognitiontrial.go`,
    `internal/rankorder/legacy_cognitiontrial.go` — the honest-clock A/B control,
    `MUNINN_TRIAL_LEGACY_ACTR_CLOCK=1`;
    `internal/index/hnsw/legacy_select_cognitiontrial.go` — the HNSW neighbour-selection
    A/B control, `MUNINN_TRIAL_LEGACY_HNSW_SELECT=1`) → CI never passes this tag, so nothing
    checks these files compile. Build them explicitly when you touch the packages they
    reach into: `go build -tags 'localassets cognitiontrial' ./... && go vet -tags
    'localassets cognitiontrial' ./...`. The cognition-trial acceptance rule
    (`internal/engine/cognition_trial_rule_test.go`) is deliberately tagged
    `localassets || cognitiontrial` so its own unit tests DO run in CI — a rule that
    decides whether a subsystem lives must itself be tested.

15. **A path that rewrites an ALREADY-indexed engram's FTS entries** (`internal/index/fts/`) → call `fts.ReindexEngram`, never `DeleteEngram` followed by `IndexEngram`. The pair is not statistics-neutral — `IndexEngram` bumps `TotalEngrams`/`AvgDocLen` and increments `df_t` for every term of the document, `DeleteEngram` decrements neither — so it silently decays the engram's own BM25 score on every call (COG-24; measured −82.6% over 100 retags, #720). **One carve-out, named by mechanism:** a FULL-VAULT rebuild that first range-tombstones the 0x08 global-stats and 0x09 per-term-stats prefixes via `store.ClearFTSKeys` may call `IndexEngram` on already-indexed engrams — `Engine.ReindexFTSVault` does (`internal/engine/engine_reindex_fts.go:67` clears, `:80` re-indexes), and it is correct precisely because the stats it would otherwise inflate no longer exist. Do **not** read this as "unless the stats are handled": if a path has not range-tombstoned 0x08/0x09, it is in violation. There is no automated check: nothing stops a new call site from pairing them, and nothing distinguishes a legitimate post-`ClearFTSKeys` rebuild from an illegitimate in-place one. 🪝

16. **A write path that accepts inline edges to existing memories** (`associations` or `relationships` on `mbp.WriteRequest`, or any new field or writer that stores an edge at write time — a tree node that gains relationships, a proto field that carries them) → a `supersedes` among them is a declaration, not an edge (COG-38). A writer that creates a memory routes it through `commitNewEngramSuperseding` (or through `Engine.Write`, as `WriteBatch` does per item); never `Set` it with the engram. Gate it with `refuseInlineSupersedesForAppend` at the writer's own entry point if the writer can be reached without passing through `Engine.Write` (the upsert path is, from `WriteBatch`). Then add the writer to the per-writer tests in `internal/engine/engine_inline_supersedes_test.go` and its credential rows to the mode table, and say so in the surface's own description of the field (`inlineRelationshipsDescription` in `internal/mcp/tools.go`, `WriteRequest.relationships`/`associations` in `openapi.yaml`). Today's surfaces: MCP sends `relationships`; REST accepts both fields (`rest.WriteRequest` is `mbp.WriteRequest`), and the REST SDKs send `relationships`; gRPC carries `associations` only (the proto has no `relationships`); MBP carries both; `RememberTree`/`AddChild` carry neither. Nothing mechanical detects a new writer that skips this — a storage-level inline `Set` of a supersedes edge still works and is exactly the pre-COG-38 defect.

## Live drift found during the guardian audit (worth fixing)

- ~~**Windows CI tests the wrong embedding model.**~~ — fixed. `ci.yml`'s Windows job now
  fetches `bge-small-en-v1.5`, matching `release.yml`, and its cache key was corrected from
  `minilm-v2`. Note this was a *recurrence*: #455 already fixed the same divergence in
  `release.yml` and left `ci.yml` behind. Both models are 384-dim, so nothing ever failed —
  which is exactly why it survived. When changing the model, grep for the old name across
  `ci.yml`, `release.yml`, `Makefile`, `Dockerfile`, and `docker-compose.yml`; the label
  appears in prose comments that no test covers.
- ~~**`muninn upgrade` has no checksum verification** (#600)~~ — fixed. `selfUpdate` now
  fetches `checksums.txt`, hashes the whole downloaded archive, and verifies before
  anything executes it. Fails closed if the file is unreachable or the asset isn't listed.
- ~~**Presets are hand-duplicated** Go ↔ web UI with no parity test.~~ — partially closed.
  `TestPlasticityPresets_WebConsoleParity` (`internal/auth/`) now pins preset *names* across
  the Go table and all four web sites. The preset *values* are still hand-duplicated: the
  radar-chart numbers in `_plasticityData` are hand-tuned for visual separation and are
  deliberately not asserted (see the test's comment), and the prose in
  `plasticityPresetDescription` cites values that nothing checks.
- **SDK type drift is warning-only** — no CI gate keeps Python/Node/PHP in sync with
  `types.go`.
- ~~**`govulncheck@latest` is unpinned** in CI~~ — fixed, pinned to v1.6.0.
- ~~**Doc drift**: `middleware.go` claims write-mode keys can't authenticate to MCP (false);
  a `toolset.go` comment says "39 tools"~~ — both corrected. The real count is **43**
  (context.go's classification table and `allMCPTools` agree), and MCP does accept
  write-mode keys, restricting them by tool rather than rejecting them.

## CI map and budget

The full gate must stay **under ~10 minutes** and cheap. Current baseline ~6–7 min
wall-clock. Jobs (from `.github/workflows/ci.yml`, real recent timings):

| Job | ~duration | Notes |
|---|---|---|
| `go` (build & test) | ~5 min on develop before the split (expect less after) | gofmt gate, embed-asset cache, web build, `go build -tags localassets`, `go vet`, **`go test -race -tags localassets` on every package except `internal/engine` and `internal/index/hnsw`**, coverage (`coverage` artifact, which no longer includes those two) |
| `go-engine` (shards 0, 1) | ~2 and ~3 min (first CI run) | parallel to `go`. `internal/engine` under `-race -covermode=atomic -timeout 300s`, split in two by `scripts/go-test-shard.sh` (a test's shard is a hash of its own name, so adding a test never moves another). A `--check` step fails the job unless the shards partition the package's test list exactly. Coverage per shard as `coverage-engine-0`/`-1` |
| `go-hnsw` | not yet measured in CI (~2.5 min of tests in a 4-CPU container) | parallel to `go`. `internal/index/hnsw` under `-race -timeout 300s` **without coverage**, then a separate uninstrumented `-covermode=set` run for the `coverage-hnsw` artifact. `-race` forces atomic coverage, and here the atomic counters sit inside the vector loops: one test took 99s with it and 30s without |
| `windows` (build & smoke) | 2–3 min | parallel to `go`; now fetches the same bge-small-en-v1.5 the release ships |
| `shellcheck` | 10–45s | lints scripts + runs `check-build-tags.sh` |
| `api-spec-validation` | 15–20s | Redocly lint + informational route-count diff |
| `vuln-check` | 15–60s | `govulncheck`, pinned to v1.6.0 |
| `cli-integration` | 1.5–2 min | **runs after `go`, `go-engine` and `go-hnsw`** (sequential); this is where the MCP registry-parity smoke test runs |
| `playwright-e2e` | 1.5–2 min | management-console E2E (thin: ~5 specs) |
| `python-sdk` | 1–2 min | **serialized after `cli-integration`** "so integration jobs are sequential" — this coupling is not a real data dependency and costs ~1.5 min of avoidable critical path |
| `node-sdk` | 15–20s | `npm ci` + `tsc` + vitest for `sdk/node`. Before it existed the SDK was first compiled by `publish-sdk.yml` at tag time |
| `web-unit` | 15–25s | `npm test` for `web/`. Before it existed the vitest suites there ran nowhere |

**Per-package `-race` headroom, measured (#815).** The `go` job runs `go test -race
./... -timeout 300s`, and the per-package timeout is the real cliff: crossing it produces a
panic dump of every goroutine, not a failed assertion. `internal/engine` is the largest
package and the one nearest that cliff. #815 reported it at **293.3s under the full
parallel invocation — 7 seconds of headroom** and flagged that nobody had measured a
parallel baseline. That baseline now exists: **six runs of the exact CI invocation on an
18-core Apple Silicon machine, across two tree states, put `internal/engine` at 187.4 /
188.6 / 190.7 / 195.3 / 200.9 / 215.9s** — 84-113s of headroom, 28-38%, not 7s. Isolated
(not parallel) it is 172.0s, so contention costs ~15-45s here rather than the ~99s the
issue extrapolated; the 293.3s single sample did not reproduce. The 215.9s outlier came
from a run with other work on the machine, which is worth stating rather than dropping:
the spread is dominated by ambient load, and that is the most likely explanation of the
293.3s original too. Two independent cross-checks
agree: the `go` job's own 3.5-4 min wall time in the table above cannot contain a 293s
package, and the in-tree reduction documented at
`cognition_trial_diagnostic_test.go` ("the whole package sat at 256s of its 300s budget")
landed after that sample was taken.

**The distribution, which is not what #815 assumed.** It described "a long tail under 3s —
accumulation, not one offender", led by `TestConcurrentWriteActivate_StressSmall` (5.5s)
and `TestCrossVault_FacetDFBoundary` (3.9s). That is no longer true. Of a 214.7s CPU-sum
over 698 top-level tests, **five `TestCognitionTrial*` tests account for 47.4s (22%)**:
`DilutionInvariance` 20.6s, `AdditivityDoesNotConvergeAndIsNotAGate` 10.6s,
`UnmeasuredMRRIsNotAMeasuredZero` 9.1s, `EveryDeltaNMustBindToTheSameSeries` 5.8s,
`AdditivityBreaksDecodeToAHarmfulMechanism` 1.5s. They are deterministic single-goroutine
bootstrap arithmetic, so `-race` pays ~10x for a schedule it cannot find anything in. They
are in CI deliberately (obligation 14: a rule that decides whether a subsystem lives must
itself be tested), and they have **already** been through a measured reduction round whose
levers and rejected alternatives are documented in place — resample counts are load-bearing
for the U5 half-width gate, and cutting them further is statistics work, not budget
trimming. Left alone on that basis, with the numbers recorded so the next person starts
from a measurement.

**Still unmeasured, and only CI can close it:** every figure above is one workstation.
`ubuntu-latest` has far fewer cores, so ~40 concurrent package binaries contend harder
there. The `go` job's reported duration is the cheapest proxy; watch it rather than
inferring from a laptop.

**The engine split (2026-11).** The trunk that followed develop `34b505f` grew
`internal/engine` from 720 to 876 top-level tests, and the new ones were heavy. The one
real CI figure: develop's last run put `internal/engine` at **169.1s** (and `hnsw` at
42.4s) under the `go` job's `-race -cover`; that job took 5m03s. On an 18-core machine
under load 18–36 (one run each, so load accounts for much of the spread): develop's engine
package took 192s under `-race`; the trunk's took **412s** under `-race` and 176s plain. Its
`-race` CPU time was 3x develop's. A `GOMAXPROCS=4 -p 4` copy of the CI command put it at
542s, and **`internal/index/hnsw` at 702s**: the trunk's SearchLive, migration and
identical-vector suites took that package to 295s even on its own. Scaling develop's 169s
CI figure says ~360s, so the 300s claim held, for two packages. Three changes, cheapest
first:

1. `testEnv`, `testEnvWithStore` and `testEnvWithAuth` (behind ~530 call sites) open Pebble
   with `storage.DefaultOptions()` on `vfs.NewMem()`, through the new optional
   `storage.Options.FS`. On disk each environment cost ~130ms under `-race` (the open alone
   was 50–70ms of fsyncs on macOS, where Go's fsync is `F_FULLFSYNC`); in memory, ~3.5ms.
   The code paths are unchanged; only the fsyncs are free. Linux fsync is cheaper, so expect
   CI to gain less than the workstation did. One latent dependency on write latency
   surfaced and was fixed (see `writeRRFThresholdFixture`). Engine 412s → 215s.
2. `t.Parallel()` on the heaviest tests that are verifiably hermetic: six in
   `internal/engine` (the two retired-set fixtures, the four `TestCognitionTrialRule_*`
   bootstraps above) and twelve in `hnsw`. Engine 215s → **144s**; hnsw 295s → 125s.
   Assertions, fixture sizes and resample counts are unchanged.
3. The split: `internal/engine` moved out of the `go` job into `go-engine`, two shards.
   At `GOMAXPROCS=4` with `-race -covermode=atomic` the shards took 69s and 79s. The
   `go test -list` names of the two shards, combined, are identical to the unsharded
   package's 876. No timeout was raised.

**The first CI run (integration branch) and what it changed.** The engine shards passed
in ~2 and ~3 minutes. `internal/index/hnsw` timed out at 300s, and two guard tests failed
first with "repair never reached stage precommit". In a linux container pinned to 4 CPUs,
running the CI command for that package: the package timed out both with and without
the commit that added `t.Parallel()`; without it, a third guard test failed the same way.
The guard tests run in the sequential phase, before any parallel test resumes, so that
commit was not starving them. Two causes:

- **The guard watchdog was 20s and a full repair took 15–27s** there under `-race -cover`.
  The waits were already event-driven (a stage hook closes a channel). The watchdog is now
  a 2-minute hang detector. The guard tests repair a 250-vector old-rule fixture instead of
  400. They test the commit protocol, not repair quality, and they now assert the vault
  still needs a *full* repair. Two bulk-insert tests closed their registries too late, so a
  background repair kept running into later tests; they now close them before the
  database. Each guard test: 15–27s → ~4s.
- **Atomic coverage under `-race` tripled the package's cost.** Container figures for
  `hnsw`, CI command: 300s+ timeout (two shards of it each timed out too). With `-race`
  alone: 116s. Uninstrumented `-covermode=set`: 20s, 87.8%. So `go-hnsw` runs those two
  steps, and every test still runs under `-race`.

Still to confirm in CI: `go-hnsw`'s real duration, and `storage` (184s on a workstation, led
by `TestRetiredSet_DigestRewriteRacingForget` at 46s), which stays in `go`. If a package
nears the limit, split it the same way. Do not raise the timeout.

Critical path: (`go` ∥ `go-engine` ∥ `go-hnsw`) → (`cli-integration` ∥ `playwright-e2e`) →
`python-sdk`. What's cached:
embed assets (~130MB, keyed by ORT+model+platform), Go module cache, and npm for the two
lockfile-scoped jobs (`node-sdk`, `web-unit`). npm is **not** cached in the jobs that build
the web bundle as a side effect. The race detector runs in `go`, `go-engine` and `go-hnsw`.

**When adding tests:** unit + invariant tests are nearly free — prefer them. Integration,
Playwright, `-race`, and asset-gated tests cost real minutes. Reach for end-to-end
(Playwright, live-server) only when a change genuinely needs behavioral proof a unit test
can't give (protocol conformance, UI flows, the money-path class of "the button is actually
wired" bugs). Never balloon the pipeline to prove a point a table-driven unit test could
make. If you must add a slow test, consider whether it belongs behind an integration tag
rather than in the default `go test ./...` path.

## Testing conventions worth upholding

- **RED-sanity verification**: a bug-fix/race-fix test must be shown to fail without the fix.
  `no tests to run` is a FAILED RED check — `go test -run` exits 0 saying `ok` when nothing
  matched, and so does a file the toolchain excluded by filename (#814, guarded by
  `scripts/check-filename-build-constraints.sh` in the `shellcheck` job).
- **Claim discipline** (`claim-discipline.md`): a set named in prose is regenerated by a
  test from a mechanism; a guard states what it does not catch; *cannot/never* means
  structurally unrepresentable and says why inline, otherwise *is refused unless* plus the
  residual; every number carries its denominator, N and machine.
- **The standing corpora are `make corpora`** — the four `TestMeasure*` harnesses in
  `internal/engine/activation`, normalised into a diffable `.artifacts/corpora.txt`. They
  also run inside `go test ./...`; the target exists to produce the diff.
- **Preset-pinning `reflect.DeepEqual` tests** for derived config (#599).
- **Registry-parity smoke** for the MCP tool surface (`smoke_exhaustive_test.go`).
- **Cross-package disjointness test** for Pebble prefixes, derived from `prefix.All()`
  (`TestAll_NoDuplicateBytes`/`TestAll_OwnerGroupsPairwiseDisjoint` in `internal/prefix/prefix_test.go`).
- **Coverage test that enumerates all handlers** so a new tool can't escape mode
  classification (`TestToolClassification_CoversAllRegisteredHandlers`).
- **`-race` on anything touching** storage, the Hebbian/PAS workers, the pruner,
  replication, or MCP session state.
