# MuninnDB — Agent Instructions

## Build Tags

This repo uses **three build tags** that gate entire subsystems. Using the wrong tags is the #1 cause of silent failures.

| Tag | Effect | Required for |
|-----|--------|-------------|
| `localassets` | Enables bundled ONNX embedder via `go:embed` | **ALL production binary builds** (Dockerfile, Makefile, CI, goreleaser). Without it, `LocalAvailable()` always returns false and semantic search silently no-ops. |
| `vectors` | Enables bleve+FAISS vector search | Vector indexing, KNN search, vector tests. Without it, stub methods return `ErrVectorSearchUnavailable`. |
| `integration` | Gates integration tests | CLI lifecycle tests, replication tests, plugin integration tests that need a live server. |

**Rule:** `go build ./cmd/muninn/...` → must have `-tags localassets`.  
**Rule:** `go test` for vector code → must have `-tags vectors`.  
**Rule:** `go test ./...` without tags → runs non-vector, non-integration tests. Fine for quick iteration.

Validation: `scripts/check-build-tags.sh` fails CI if any muninn binary build command misses `-tags localassets`.

### Build-tag-gated files

```
//go:build vectors          → internal/search/bleve/{vector_vectors,mapping_vectors}.go
//go:build vectors          → internal/search/bleve/*_vectors_test.go
//go:build vectors          → internal/search/bleve/integration_test.go
//go:build vectors          → internal/transport/rest/search_backend_vectors_test.go
//go:build !vectors         → internal/search/bleve/{vector_noop,mapping_noop,backend_noop_test}.go

//go:build localassets      → internal/plugin/embed/local_assets_*.go  (per-platform)
//go:build !localassets     → internal/plugin/embed/local_assets_noembed.go
//go:build integration      → cmd/muninn/*_test.go (many)
//go:build integration      → internal/replication/*_test.go
//go:build integration      → internal/plugin/embed/local_integration_test.go
```

## Go & Docker Versions

- **Go:** 1.25.0 (`go.mod` → `go 1.25.0`)
- **Production Docker:** `golang:1.25-bookworm` → build → `debian:bookworm-slim` runtime
- **Vector test Docker:** `golang:1.25-bookworm` builder + `debian:bookworm-slim` FAISS stage

Never downgrade Docker base images to 1.24 without updating go.mod first.

## Pre-Build Dependencies

Before `go build` with `-tags localassets`:

1. **Fetch embed assets** (one-shot, cached in CI):
   ```bash
   make fetch-model _ort-linux-${TARGETARCH}   # ONNX model + tokenizer + ORT .so
   ```
   Assets go to `internal/plugin/embed/assets/`. Without them, `go:embed` directives fail at compile time.

2. **Build web assets** (Node.js required):
   ```bash
   cd web && npm ci --ignore-scripts && npm run build
   ```
   Produces `web/dist/` used by the embedded web UI handler.

CI caches these: cache key `embed-assets-ort-1.24.2-minilm-v2-linux` in `.github/workflows/ci.yml`.

## Search Backend Architecture

Two backends implementing `search.Backend` (TextIndexer + TextSearcher + VectorIndexer + VectorSearcher):

| Backend | Package | Selection |
|---------|---------|-----------|
| Bleve (+ optional FAISS) | `internal/search/bleve` | `MUNINN_SEARCH_BACKEND=bleve` |
| Native (fallback/default) | `internal/search/native` | Default when env unset or `=native` |

**Vector filtering design (bleve only):**
- Uses `AddKNNWithFilter()` for pre-filtering during KNN search.
- Supported filters: `created_by`, `tags`, `created_after/before`.
- `state` filter is **deliberately excluded** from pre-filtering — avoids index synchronization issues from frequent state transitions. State filtering happens post-retrieval.
- Native backend ignores vector filters (falls through to post-filtering).

The `search/adapters/` package bridges search backends to `internal/index/fts` worker.

## Docker Build Quirks

### Vector test image (`Dockerfile.faiss`)

Multi-stage: FAISS C library built from source → Go test runner.

```bash
# Build requires --network host (GitHub clone fails on Docker bridge in constrained networks)
docker build --network host -f Dockerfile.faiss -t muninndb-vector-test .
docker run --rm --network host muninndb-vector-test
```

**FAISS build stage gotchas (hard-earned):**
- `WORKDIR /faiss` is mandatory — cmake `#include <faiss/...>` resolution depends on working directory context.
- CMake must use: `-DFAISS_ENABLE_C_API=ON -DBUILD_SHARED_LIBS=ON -DFAISS_OPT_LEVEL=generic -DBUILD_TESTING=OFF -DFAISS_ENABLE_GPU=OFF -DFAISS_ENABLE_PYTHON=OFF`
- `libfaiss.so` links against LAPACK (via OpenBLAS). Builder stage needs `libopenblas-dev` installed **and** `CGO_LDFLAGS="-lfaiss_c -lfaiss -lopenblas"`. Missing `-lopenblas` → `undefined reference to dsyev_/sgeqrf_/...`.
- Source: `blevesearch/faiss`, `bleve` branch (i.e. the branch named `bleve`, not a specific commit). Matches `go-faiss v1.0.34`.
- Do **not** copy FAISS `.so` files from the host. Build inside the container.

### Production image (`Dockerfile`)

- Stage 1 (builder): `golang:1.25-bookworm`, builds with `-tags localassets`, requires Node.js+`npm ci` for web assets.
- Stage 2 (runtime): `debian:bookworm-slim`, no Go or Node. Only `ca-certificates curl`.
- `CGO_ENABLED=1` is mandatory — the ONNX embedder uses `dlopen` for the ORT native library at runtime.
- Binary must be `muninndb-server`, not `muninn`.
- **`ARG SKIP_WEB_BUILD=1`** — skips Node.js install and web asset build for faster CI/test-only builds. Default is `0` (full build).

## Test Commands

```bash
# All non-vector, non-integration tests (fast, no prerequisites)
go test ./...

# Vector tests (requires FAISS C libraries installed on host OR Docker):
CGO_LDFLAGS="-lfaiss_c -lfaiss -lopenblas" go test -tags vectors -count=1 -v ./internal/search/bleve/...
CGO_LDFLAGS="-lfaiss_c -lfaiss -lopenblas" go test -tags vectors -count=1 -v ./internal/transport/rest/...

# Run vector tests in Docker (fully self-contained):
docker build --network host -f Dockerfile.faiss -t muninndb-vector-test . && docker run --rm muninndb-vector-test

# Integration tests (needs no muninn already running on :8750):
go test -tags localassets,integration -v -timeout 120s ./cmd/muninn/...

# CI test (what GitHub Actions runs):
go test -tags localassets ./... -timeout 300s -race -coverprofile=coverage.out -covermode=atomic
```

## Commit & CI Conventions

- **Conventional commits:** `feat:`, `fix:`, `test:`, `refactor:`, `docs:`, `bench:`, `chore:`
- **CI:** `.github/workflows/ci.yml` runs on push/PR to `main` and `develop`.
- **Skipped in release changelog:** commits prefixed `docs:`, `test:`, `bench:`, plus merge commits.
- **Node.js:** CI enforces `FORCE_JAVASCRIPT_ACTIONS_TO_NODE24: true` (silences deprecation warnings ahead of June 2026 forced migration).

## Cross-Compilation (goreleaser)

`.goreleaser.yml` defines per-platform CGO compilers:
- **linux/amd64:** `CC=x86_64-linux-musl-gcc`
- **windows/amd64:** `CC=x86_64-w64-mingw32-gcc`
- **darwin/amd64:** via `SergioBenitez/osxct/x86_64-apple-darwin14`
- `linux/arm64` and `windows/arm64` are **skipped** in releases.

Pre-build hook: `make fetch-assets` (fetches ORT libs for all platforms before cross-compilation).

## API Spec Drift Guards

`.claude/` contains Claude Code hooks that warn when editing files that require cascading updates:

- **api-spec-drift:** Editing `internal/transport/rest/server.go` or related handlers → check `openapi.yaml`. Validate with `npx @redocly/cli lint internal/transport/rest/openapi.yaml`.
- **sdk-types-drift:** Editing `internal/transport/rest/types.go` → update Python/Node/PHP SDKs in separate repos.

## Key Directories

| Path | Purpose |
|------|---------|
| `cmd/muninn/` | CLI binary (server, init, start, status, etc.) |
| `internal/search/` | Search backend interfaces + bleve/native impls |
| `internal/engine/` | Core cognitive engine (6-phase activation pipeline) |
| `internal/plugin/embed/` | Bundled ONNX embedder (go:embed assets per platform) |
| `internal/transport/rest/` | REST API + OpenAPI spec |
| `internal/mcp/` | MCP protocol handler |
| `internal/index/fts/` | FTS index worker (delegates to search backend) |
| `internal/storage/` | PebbleDB-backed engram store |
| `sdk/` | Python, Node, Go, PHP client SDKs |
| `web/` | Vite+Tailwind CSS web UI |
| `proto/` | Protobuf definitions for gRPC |

## Repository URL & Module Path

- **Module:** `github.com/scrypster/muninndb`
- **Repo:** `https://github.com/scrypster/muninndb`
- **License:** BSL 1.1 (auto-converts to Apache 2.0 on Feb 26, 2030)
