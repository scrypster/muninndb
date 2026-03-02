# MuninnDB — Technical Capability Statement

This document describes what MuninnDB does, how it is implemented, and what it is not.
Every claim cites a specific source file in the repository.

---

## 1. What It Does Differently

| Capability | Code Reference |
|---|---|
| Hebbian association strengthening on every read | `internal/cognitive/hebbian.go` |
| Temporal decay scoring (ACT-R model) | `internal/cognitive/decay.go`, `internal/engine/activation/engine.go` |
| 6-phase predictive spread activation (`Run` in `ActivationEngine`) | `internal/engine/activation/engine.go` (phases 1–6, line 340+) |
| Semantic push triggers (subscription registry + worker) | `internal/engine/trigger/registry.go`, `internal/engine/trigger/worker.go` |
| SGD feedback loop — adaptive per-vault scoring weights | `internal/scoring/weights.go` (`VaultWeights.Update()`), `internal/scoring/store.go` (`RecordFeedback`) |
| Hierarchical memory with ordinal ordering | `internal/engine/tree.go`, `internal/storage/keys/keys.go` (prefix `0x1E`) |
| Entity graph with cross-vault reverse index | `internal/storage/entity.go` (`UpsertEntityRecord`), key-space prefix `0x23` in `internal/storage/keys/keys.go` |
| Full audit trail with provenance tracking | `internal/provenance/store.go`, `internal/provenance/types.go` |

---

## 2. Implementation Signals

Concrete signals that speak to production readiness:

- **WAL group-commit durability** — writes are coalesced and synced via `internal/storage/wal_syncer.go`, preventing partial-write corruption on crash.
- **Atomic Pebble batches across key-space prefixes** — multi-key operations (ordinal index, engram record, association edges) are committed in a single atomic Pebble batch; see `internal/engine/tree.go` for the tree write path.
- **Race-tested** — 44 packages contain test files, all verified clean under the Go `-race` detector.
- **Per-entity mutex** — `UpsertEntityRecord` in `internal/storage/entity.go` acquires a per-entity lock (see file header comment: "Per-entity lock via `getEntityLock(nameHash)`") to prevent TOCTOU races on concurrent entity upserts.
- **Per-parent mutex** — `getChildMutex` defined in `internal/engine/engine.go` (called from `internal/engine/tree.go`) serializes writes under each parent node, preventing sibling-ordering races without a global tree lock.
- **Provisional patent** — U.S. Provisional Patent Application No. 63/991,402, filed February 26, 2026.

---

## 3. Feature Surface

### Cognitive

- [Hebbian association strengthening](cognitive-primitives.md) — co-activated engram pairs have their association weights incremented on every successful recall.
- [ACT-R temporal decay](cognitive-primitives.md) — base-level activation scores follow the ACT-R decay equation, parameterized per vault via Plasticity presets.
- [6-phase spread activation](retrieval-design.md) — the ACTIVATE pipeline (embed → candidate retrieval → RRF fusion → Hebbian boost → BFS traversal → final scoring) runs on every recall request.
- [Semantic push triggers](semantic-triggers.md) — subscriptions fire server-side push events on new writes, threshold crossings, and contradiction detection.
- [SGD feedback loop](feature-reference.md) — per-vault scoring weight vectors are updated from user feedback signals via stochastic gradient descent.

### Storage

- [Custom Pebble KV engine](architecture.md) — all data is stored in an embedded Pebble (LevelDB-family) instance with a structured key-space schema.
- [WAL group-commit durability](durability-guarantees.md) — write-ahead log is synced in coalesced batches for crash safety.
- [Two-tier Sync/NoSync write path](durability-guarantees.md) — callers choose between fully durable (fsync) and low-latency (async) write modes per operation.
- [Atomic cross-prefix batches](architecture.md) — writes that touch multiple key-space prefixes are committed atomically via Pebble batch.

### Retrieval

- [Multi-strategy recall](retrieval-design.md) — candidates are gathered in parallel from full-text search, HNSW vector index, temporal decay pool, and tag index before fusion.
- [Relevance scoring](retrieval-design.md) — final scores are composites of semantic similarity, FTS relevance, ACT-R decay, Hebbian boost, recency, and access frequency.
- [Activation spread via BFS](retrieval-design.md) — the BFS traversal phase (Phase 5) walks the association graph up to a configurable hop depth to surface indirectly related memories.

### Organization

- [Hierarchical memory (tree)](hierarchical-memory.md) — engrams can be nested under parent nodes with ordinal ordering, forming a persistent memory tree per vault.
- [Entity graph](entity-graph.md) — named entities are extracted from engram content and linked to the engrams that mention them, forming a cross-engram relationship graph.
- [Vaults](feature-reference.md) — data is namespaced into isolated vaults with per-vault Plasticity configuration, scoring weights, and access control.

### Transport

- [MCP — 35 tools](feature-reference.md) — Model Context Protocol server (JSON-RPC 2.0 over HTTP) exposes 35 tools for LLM agent use.
- [REST API — 70+ endpoints](feature-reference.md) — HTTP/JSON API with Server-Sent Events for real-time push; primary interface for human-facing integrations.
- [OpenAPI 3.0 spec](quickstart.md) — machine-readable API description ships with the binary.

### Observability

- [Full provenance trail](feature-reference.md) — every write records its origin (tool, caller, session) in `internal/provenance/`; queryable via `muninn_provenance`.
- [Episodic sessions](feature-reference.md) — `muninn_session` groups recall and write events into named episodes for replay and audit.
- [Feedback loop](feature-reference.md) — explicit user feedback (confirmed / rejected) drives Bayesian confidence updates and SGD weight adjustment.

### Deployment

- [Single binary](self-hosting.md) — the entire server compiles to one statically linked binary with no external runtime dependencies.
- [Self-hosted](self-hosting.md) — designed for on-premises and private-cloud deployment; no telemetry or external calls are required.
- [Cluster mode](cluster-operations.md) — Cortex/Lobe topology distributes cognitive side-effects (Hebbian updates, transition tracking) across nodes.
- [Multi-language SDKs](quickstart.md) — client libraries are available alongside the MCP, REST, gRPC, and MBP transport layers.

---

## 4. What This Is Not

MuninnDB is purpose-built as a cognitive memory layer. Understanding its boundaries is as important as understanding its capabilities.

- **Not a general-purpose database.** MuninnDB does not implement SQL, relational joins, or arbitrary query languages. It is optimized for memory-style workloads: write once, recall by context.
- **Not a replacement for PostgreSQL or other relational databases.** Structured, tabular, transactional workloads belong in a relational system. MuninnDB is a complement to those systems, not a substitute.
- **Not horizontally sharded.** The storage engine is single-node. Cluster mode distributes cognitive side-effects but does not shard the Pebble key-space across nodes.
- **Not a RAG pipeline replacement.** Retrieval-augmented generation pipelines own chunking, document ingestion, and LLM prompt assembly. MuninnDB is a memory layer that sits alongside or beneath a RAG system — it is not a drop-in replacement for one.

---

**See also:**
- [Feature Reference](feature-reference.md)
- [How Memory Works](how-memory-works.md)
- [Architecture](architecture.md)
- [Key-Space Schema](key-space-schema.md)
