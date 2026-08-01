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
    (the pruner) inherit the obligation.

12. **A new async worker or fire-and-forget path on the write or scoring path**
    (`internal/engine`, `internal/engine/activation`, `internal/storage`) → give it a
    `WaitIdle`/`Flush` seam, fold it into `Engine.waitWriteTimeIdle()` (or document it as
    its own drain if it doesn't fit that call), and add it to the async-source table in
    `testing-hermeticity.md`. Any test asserting on that worker's output must drain it
    deterministically — `time.Sleep` is not synchronization and flakes under `-race` on
    constrained CI cores (#722).

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
- **Preset-pinning `reflect.DeepEqual` tests** for derived config (#599).
- **Registry-parity smoke** for the MCP tool surface (`smoke_exhaustive_test.go`).
- **Cross-package disjointness test** for Pebble prefixes, derived from `prefix.All()`
  (`TestAll_NoDuplicateBytes`/`TestAll_OwnerGroupsPairwiseDisjoint` in `internal/prefix/prefix_test.go`).
- **Coverage test that enumerates all handlers** so a new tool can't escape mode
  classification (`TestToolClassification_CoversAllRegisteredHandlers`).
- **`-race` on anything touching** storage, the Hebbian/PAS workers, the pruner,
  replication, or MCP session state.
