# Cross-surface obligations, drift risks, and the CI map

MuninnDB has many surfaces that must stay in sync but mostly *aren't* checked automatically.
This is where "you changed X but didn't update Y" bugs live. A reviewer must walk this list
for any PR that touches a synced surface.

## "If a PR touches X, it must also do Y"

1. **An MCP tool handler** (`internal/mcp/*.go`) → update `allMCPTools` in
   `cmd/muninn/smoke_exhaustive_test.go` and run `go test -tags integration,localassets
   ./cmd/muninn/...` (the registry-parity smoke test). Also classify the new tool in
   `isMutatingTool`/`isReadOnlyTool` (`internal/mcp/context.go`) — the coverage test
   enforces this, but flag it early.

2. **A REST route or handler** (`internal/transport/rest/`) → update
   `internal/transport/rest/openapi.yaml` and pass `npx @redocly/cli lint`. The CI
   route-count parity check is **informational only** (±5 tolerance) and cannot see
   field-level drift — so this is a manual reviewer obligation, not a gate.

3. **REST request/response types** (`internal/transport/rest/types.go`) → check
   `sdk/python`, `sdk/node`, `sdk/php` (and Kotlin/Swift/Go) for the corresponding change.
   **There is no automated check** — only the `.claude/hookify.sdk-types-drift.local.md`
   warning. Manual.

4. **A plasticity preset value** (`internal/auth/plasticity.go`) → update the web-UI preset
   cards (`web/templates/index.html`) and the JS descriptions/radar data
   (`web/static/js/app.js`), and any docs table. Presets are **hand-duplicated** across Go
   and the web UI with no parity test — a known drift surface. If you add a preset, also
   add a `reflect.DeepEqual`-style pinning test if it's derived from another (see #599).

5. **The embed model or ORT version** (`Makefile`) → update **all** of: `ci.yml` cache keys
   (Linux *and* Windows jobs), `release.yml` matrix cache keys (5 platforms), and the
   `docker-compose.yml` comment. **These are already drifted today** — see the live-drift
   list below.

6. **`cmd/muninn/upgrade.go`** → if a PR claims to fix upgrade integrity (#600), verify it
   actually verifies the downloaded binary against `checksums.txt` (which `release.yml`
   already publishes). Don't accept an executable-bit + version-string check as "integrity."

7. **`proto/muninn/v1/service.proto`** → regenerate `proto/gen/go/...`. No CI step verifies
   the generated code is current — the reviewer must confirm regen ran.

8. **A new Pebble prefix** → see `keyspace-registry.md`: disjoint, documented in `keys.go`,
   and bump `storageMaxPrefix` in `keys_test.go` if ≥ 0x2B.

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

## Live drift found during the guardian audit (worth fixing)

- **Windows CI tests the wrong embedding model.** `ci.yml`'s Windows job hardcodes URLs for
  `all-MiniLM-L6-v2`, while `release.yml` ships `bge-small-en-v1.5` for Windows — CI
  validates a different model than users get. Also the Linux cache key literally says
  `minilm-v2` though the Makefile fetches bge-small.
- **`muninn upgrade` has no checksum verification** (#600) despite `release.yml` generating
  `checksums.txt` — a real supply-chain gap.
- **Presets are hand-duplicated** Go ↔ web UI with no parity test.
- **SDK type drift is warning-only** — no CI gate keeps Python/Node/PHP in sync with
  `types.go`.
- **`govulncheck@latest` is unpinned** in CI — non-reproducible vuln-check runs.
- **Doc drift**: `middleware.go` claims write-mode keys can't authenticate to MCP (false);
  a `toolset.go` comment says "39 tools" (now 42).

## CI map and budget

The full gate must stay **under ~10 minutes** and cheap. Current baseline ~6–7 min
wall-clock. Jobs (from `.github/workflows/ci.yml`, real recent timings):

| Job | ~duration | Notes |
|---|---|---|
| `go` (build & test) | 3.5–4 min | gofmt gate, embed-asset cache, web build, `go build -tags localassets`, `go vet`, **`go test -race -tags localassets ./...`**, coverage |
| `windows` (build & smoke) | 2.5–3 min | parallel to `go`; **wrong-model drift, see above** |
| `shellcheck` | 10–45s | lints scripts + runs `check-build-tags.sh` |
| `api-spec-validation` | 15–20s | Redocly lint + informational route-count diff |
| `vuln-check` | 30–90s | `govulncheck` (unpinned) |
| `cli-integration` | 1.5–2 min | **runs after `go`** (sequential); this is where the MCP registry-parity smoke test runs |
| `playwright-e2e` | 1.5–2 min | management-console E2E (thin: ~5 specs) |
| `python-sdk` | 1.5–2 min | **serialized after `cli-integration`** "so integration jobs are sequential" — this coupling is not a real data dependency and costs ~1.5 min of avoidable critical path |

Critical path: `go` → (`cli-integration` ∥ `playwright-e2e`) → `python-sdk`. What's cached:
embed assets (~130MB, keyed by ORT+model+platform), Go module cache. npm is **not** cached
(re-`npm ci` in four jobs). Race detector runs only in the `go` job.

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
- **Cross-package disjointness test** for Pebble prefixes (`TestCapabilityPrefixesNoCollision`).
- **Coverage test that enumerates all handlers** so a new tool can't escape mode
  classification (`TestToolClassification_CoversAllRegisteredHandlers`).
- **`-race` on anything touching** storage, the Hebbian/PAS workers, the pruner,
  replication, or MCP session state.
