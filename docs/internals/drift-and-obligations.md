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

7. **`proto/muninn/v1/service.proto`** → regenerate `proto/gen/go/...`. No CI step verifies
   the generated code is current — the reviewer must confirm regen ran. 🪝

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
    at 1.

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
    `internal/cognitive/replay_cognitiontrial.go`) → CI never passes this tag, so nothing
    checks these files compile. Build them explicitly when you touch the packages they
    reach into: `go build -tags 'localassets cognitiontrial' ./... && go vet -tags
    'localassets cognitiontrial' ./...`. The cognition-trial acceptance rule
    (`internal/engine/cognition_trial_rule_test.go`) is deliberately tagged
    `localassets || cognitiontrial` so its own unit tests DO run in CI — a rule that
    decides whether a subsystem lives must itself be tested.

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
| `go` (build & test) | 3.5–4 min | gofmt gate, embed-asset cache, web build, `go build -tags localassets`, `go vet`, **`go test -race -tags localassets ./...`**, coverage |
| `windows` (build & smoke) | 2–3 min | parallel to `go`; now fetches the same bge-small-en-v1.5 the release ships |
| `shellcheck` | 10–45s | lints scripts + runs `check-build-tags.sh` |
| `api-spec-validation` | 15–20s | Redocly lint + informational route-count diff |
| `vuln-check` | 15–60s | `govulncheck`, pinned to v1.6.0 |
| `cli-integration` | 1.5–2 min | **runs after `go`** (sequential); this is where the MCP registry-parity smoke test runs |
| `playwright-e2e` | 1.5–2 min | management-console E2E (thin: ~5 specs) |
| `python-sdk` | 1–2 min | **serialized after `cli-integration`** "so integration jobs are sequential" — this coupling is not a real data dependency and costs ~1.5 min of avoidable critical path |
| `node-sdk` | 15–20s | `npm ci` + `tsc` + vitest for `sdk/node`. Before it existed the SDK was first compiled by `publish-sdk.yml` at tag time |
| `web-unit` | 15–25s | `npm test` for `web/`. Before it existed the vitest suites there ran nowhere |

Critical path: `go` → (`cli-integration` ∥ `playwright-e2e`) → `python-sdk`. What's cached:
embed assets (~130MB, keyed by ORT+model+platform), Go module cache, and npm for the two
lockfile-scoped jobs (`node-sdk`, `web-unit`). npm is **not** cached in the jobs that build
the web bundle as a side effect. Race detector runs only in the `go` job.

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
