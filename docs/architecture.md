# Architecture

MuninnDB is a purpose-built cognitive database. It ships as a single Go binary. Raw key-value storage is Pebble. Everything else — the storage format, indexes, cognitive workers, wire protocols, activation engine, trigger system — is MuninnDB. There are no external dependencies at runtime.

---

## 1. Overview

```
Consumers (AI agents, applications, Claude, Cursor)
    ↓ MBP / REST / gRPC / MCP
┌─────────────────────────────────────────────────────┐
│  Interface Layer                                     │
│  8474: MBP (native binary protocol, TCP)            │
│  8475: REST API (JSON)                              │
│  8476: Web UI + health + metrics                    │
│  8477: gRPC (protocol buffers, TCP)                 │
│  8750: MCP (JSON-RPC)                               │
└────────────────────────┬────────────────────────────┘
                         ↓
┌─────────────────────────────────────────────────────┐
│  Plugin Layer (optional, zero required)             │
│  • Embed: HNSW vectors + retroactive indexing       │
│  • Enrich: LLM-powered semantic analysis            │
└────────────────────────┬────────────────────────────┘
                         ↓
┌─────────────────────────────────────────────────────┐
│  Core Engine                                        │
│  • Write path (<10ms ACK guarantee)                 │
│  • 6-phase ACTIVATE pipeline                        │
│  • Cognitive workers (async, never block)           │
│    temporal · hebbian · contradiction · confidence  │
│    · transition (PAS)                               │
│  • Semantic trigger system                          │
└────────────────────────┬────────────────────────────┘
                         ↓
┌─────────────────────────────────────────────────────┐
│  Index Layer                                        │
│  • Inverted (BM25 full-text search)                 │
│  • HNSW (vector similarity, plugin-activated)       │
│  • Adjacency (association graph, forward+reverse)   │
│  • Secondary (state, tag, creator)                  │
└────────────────────────┬────────────────────────────┘
                         ↓
┌─────────────────────────────────────────────────────┐
│  ERF Storage + Pebble KV                            │
│  • Hot: in-memory cache (sync.Map)                  │
│  • Warm: on-disk, high relevance                    │
│  • Cold: compressed, dormant                        │
└─────────────────────────────────────────────────────┘
```

The plugin layer is intentionally optional. MuninnDB without any plugins is a fully functional cognitive database: full-text search, ACT-R temporal scoring, Hebbian association, contradiction detection, Bayesian confidence updates, and graph traversal all work without an embedding model or LLM. The embed plugin is configured via env vars (MUNINN_OLLAMA_URL, MUNINN_OPENAI_KEY, MUNINN_OPENAI_URL, MUNINN_VOYAGE_KEY) and adds vector search. The enrich plugin is configured via MUNINN_ENRICH_URL and adds semantic contradiction detection and LLM-powered enrichment. Neither is required.

---

## 2. The Write Path

The write path has one contract with the client: your write is durable in under 10 milliseconds. Everything else — indexing, cognitive updates, trigger evaluation — happens after the ACK, asynchronously, without affecting write latency.

**Step 1: Validate and encode**
The incoming engram is validated (field lengths, state transitions, association limits) and encoded to ERF binary. ERF encoding is fast — fixed-size header and metadata sections, zstd compression only for content fields above 512 bytes.

**Step 2: Pebble batch commit (fsync)**
The ERF-encoded engram is committed to Pebble via a batch write with `pebble.Sync` (the default). This fsyncs Pebble's internal WAL to disk, providing the durability guarantee. If the process is killed after this point, the write is recoverable from Pebble's WAL on restart — Pebble replays its WAL automatically during `Open()`.

> **Note:** An optional `NoSyncEngrams` mode is available that defers fsync to a background `walSyncer` (every 10ms), trading per-write durability for throughput. In this mode, maximum data loss on `kill -9` is bounded to 10ms of writes — equivalent to PostgreSQL's `synchronous_commit=off`.

**Step 3: Client ACK**
The client receives its acknowledgment. At this point the write is on disk and survives `kill -9`.

**Step 4: MOL append (async)**
An entry is appended to the Muninn Operation Log asynchronously. The MOL serves as the replication log — it feeds WAL streaming to replicas and provides an audit trail. The MOL entry contains the operation type and engram ULID (not the full payload), so it is lightweight.

**Step 5: Async index updates**
The inverted index, adjacency graph, and secondary indexes are updated. These happen off the critical path.

**Step 6: Async cognitive worker notifications**
The Hebbian worker is queued. The transition worker records PAS events (if PAS is enabled for the vault). The trigger system evaluates subscriptions. All notifications use non-blocking channel sends — if a worker's input channel is full because it is processing a large batch, the notification is dropped. The worker will process the engram on its next sweep.

This is a deliberate design choice. The write path must not be held hostage to background processing speed. The cognitive workers are eventually consistent. A write that takes 50ms because the Hebbian worker was busy is not acceptable. Temporal scoring (ACT-R) is computed at query time from stored access counts and timestamps, so it never needs a background worker and is always up to date.

---

## 3. The 6-Phase Activation Engine

ACTIVATE is the primary read operation in MuninnDB. It is not a query in the SQL sense — it is activation, the same word used in cognitive science for how context spreads through a memory network and surfaces related content.

The question ACTIVATE answers is: *given this context, what should I be thinking about right now?*

### Phase 1: Embed and Tokenize

The context string is processed in parallel:
- If the embed plugin is active, the context is embedded to a `float32` vector for semantic similarity search
- The context is tokenized for FTS: case-folded, stop words removed, trigram fallback applied for short tokens, minimum 2 characters
- If the client includes a precomputed embedding in the request, it is used directly — the embed plugin call is skipped

### Phase 2: Parallel Retrieval

Three goroutines run concurrently via `errgroup`, each querying a different index:

**FTS goroutine:** BM25 query against the inverted index. Uses field-weighted scoring (concept=3.0, tags=2.0, content=1.0, creator=0.5). Returns a ranked list of engrams matching the tokenized query terms.

**HNSW goroutine:** Cosine similarity search in the in-memory vector index. Returns approximate nearest neighbors by embedding. No-op if the embed plugin is not active.

**Temporal pool goroutine:** Returns recently-accessed engrams above the relevance floor. This is the temporal signal — it captures context that is cognitively active right now, even if it does not textually or semantically match the current query. An engram you accessed five minutes ago is probably relevant even if the words do not overlap.

**PAS transition candidates:** If Predictive Activation Signal is enabled for the vault, transition candidates are fetched from the transition cache based on the previous activation's result set. These candidates are injected into the RRF fusion alongside the three retrieval streams, with their own rank constant (K=50).

All results are collected when all goroutines complete.

### Phase 3: Reciprocal Rank Fusion

RRF merges three ranked lists into one unified ranking without requiring score normalization across different scoring systems.

The formula is:

```
score(d) = Σ 1 / (k + rank(d, list_i))
```

An engram that ranks highly in multiple lists scores higher than one that dominates a single list. The `k` constant controls how much top-rank positions are rewarded over lower positions.

MuninnDB uses custom k values calibrated to each signal's precision:
- FTS: k=60 (tightly ranked signal, precision matters)
- HNSW: k=40 (even tighter — cosine similarity is a strong signal when the embed plugin is active)
- Temporal pool: k=120 (looser signal — temporal relevance is a weaker predictor of query relevance, so top positions are less heavily rewarded)

The result is a single ranked list built from three orthogonal signals: textual, semantic, and temporal.

### Phase 4: Hebbian Boost

The activation log — a ring buffer of recent ACTIVATE calls — is consulted. Engrams that co-appeared in recent activations receive a score boost proportional to:
- How recently they co-appeared
- How frequently they co-appeared
- The scores at which they co-appeared (high-confidence co-activations produce stronger Hebbian signal)

This implements Hebb's Rule at query time: ideas that have fired together recently are more likely to fire together again.

### Phase 4.5: PAS Transition Boost

If Predictive Activation Signal is enabled for the vault, the engine consults the transition cache for candidates predicted by sequential patterns. If activation N returned engram A, and PAS has learned that engram B frequently follows A, engram B is injected as a candidate and receives a transition boost proportional to its transition count relative to the maximum across all targets.

PAS candidates enter the scoring pipeline alongside standard retrieval results. The transition boost is additive in the ACT-R formula: `totalActivation = baseLevel + scale × hebbianBoost + scale × transitionBoost`. This surfaces memories that would not otherwise appear in the result set — candidate expansion, not just re-ranking.

### Phase 5: BFS Association Traversal

Starting from the top-K candidates from Phase 4, the association graph is traversed breadth-first.

**Hop penalty:** Each graph hop multiplies the score by 0.7. An engram directly in the retrieval set scores at full weight. An engram one hop away scores at 70%. Two hops away scores at 49%. This ensures that direct candidates always rank above discovered associations, and discovered associations rank in proportion to their graph distance.

**Max nodes:** 500 nodes explored per activation. On a dense graph, BFS without a cap will spend unbounded time discovering tenuous connections. 500 nodes is empirically sufficient to surface meaningful associations without the traversal becoming the bottleneck.

The BFS step discovers engrams that were not in the original retrieval set but are strongly connected to what was found. This is how MuninnDB surfaces context you did not know to ask for.

### Phase 6: Final Scoring, Filter, and Response

**Composite score:**

```
score = (semantic × 0.35)
      + (FTS      × 0.25)
      + (temporal × 0.20)
      + (Hebbian  × 0.10)
      + (access   × 0.05)
      + (recency  × 0.05)
```

Then multiplied by confidence. A 0.3-confidence engram scores 30% of what it would at full confidence — it ranks lower automatically, without being excluded from results.

**Filters** are applied: lifecycle state (ARCHIVED engrams excluded by default), tag filters, date ranges, creator filters.

**"Why" explanations** are built per result: which retrieval signals matched, what associations were traversed, how the score was composed. These are included in the response so consumers can understand why each engram surfaced.

**Streaming:** Results are streamed to the client as they are scored. The client starts receiving results before the full result set is finalized.

---

## 4. Cognitive Workers

Four async goroutines run continuously in the background, evolving the database's cognitive state. None of them block reads or writes. All of them use non-blocking Submit — if a worker's input channel is full, the item is dropped, not queued indefinitely.

The infrastructure is a generic `Worker[T]` type with configurable batch size and max-wait timeout. Each worker drains its input channel in batches, processes the batch, and sleeps until the next input arrives. Telemetry tracks processed count, batch count, error count, and drop count per worker.

Temporal scoring (ACT-R) is not a background worker — it is computed at query time from stored access counts and timestamps using the base-level activation equation `B(M) = ln(n+1) - d × ln(ageDays / (n+1))`. This is a total-recall design: no background process mutates stored scores, so activation values are always correct and deterministic.

### Hebbian Worker

The Hebbian worker processes co-activation events produced by ACTIVATE calls. When an ACTIVATE call returns a result set, each pair of engrams in the result set generates a co-activation event.

The canonical pair key is `(min(idA, idB), max(idA, idB))` — since ULIDs are lexicographically comparable, smaller first produces a unique, consistent key for any ordered pair. This deduplicates bidirectional association updates.

Weight updates use a multiplicative formula (detailed in `cognitive-primitives.md`). The key properties: each co-activation strengthens the association, dormancy weakens it, and the magnitude of the update is proportional to the confidence of both engrams at activation time.

### Contradiction Worker

Two detection modes run in the contradiction worker:

**Structural:** Uses a 64×64 boolean matrix for O(1) contradiction lookup. The matrix is a static, init-time table encoding which relationship types are logically incompatible (e.g., `supports` and `contradicts` for the same pair). When the contradiction worker finds two engrams holding incompatible relationship types with a common third engram, it fires a contradiction event.

**Semantic:** When the enrich plugin is active, the plugin fires contradiction events when LLM analysis detects logical contradiction. These events arrive via an event channel and are processed by the contradiction worker.

On contradiction detection, the contradiction worker:
1. Fires immediately to the trigger system at highest priority (not rate-limited)
2. Queues both engrams for Bayesian confidence updates
3. Creates a typed `contradicts` association between the two engrams
4. Pushes a notification to any active subscriptions that include these engrams

### Confidence Worker

Processes confidence update events from contradiction detection and reinforcement signals. Applies the Bayesian posterior formula with Laplace smoothing (detailed in `cognitive-primitives.md`). Updates the stored confidence in the engram's ERF metadata block — touching only the fixed-offset metadata section, not the full record.

Confidence updates immediately affect activation scoring because confidence is a multiplier applied in Phase 6. There is no lag between a confidence update and its effect on results.

### Transition Worker (PAS)

Processes sequential activation events for the Predictive Activation Signal. After each ACTIVATE call, the engine records which engrams appeared in the current result set alongside the previous result set. The transition worker aggregates these (source → destination) pairs and writes them to the tiered transition cache.

The transition cache uses an in-memory hot tier (`sync.Map`) for O(1) increments, with periodic flush to Pebble for durability. Cold sources are loaded from Pebble on first access via read-through caching. Eviction uses a combined heat score (count × recency) to keep the hot tier bounded.

PAS is configurable per vault via `predictive_activation` (bool) and `pas_max_injections` (0–20). When disabled, no transitions are recorded and no candidates are injected.

---

## 5. Index Layer

### Inverted Index (Full-Text Search)

BM25 scoring with field-weighted term frequencies. Field weights:
- `concept`: 3.0 — what the engram is about; highest signal
- `tags`: 2.0 — curated labels; stronger than content
- `content`: 1.0 — detailed but potentially noisy
- `creator`: 0.5 — attribution; weakest signal

Trigram fallback: for query terms shorter than 3 characters, or when exact term matching yields no results, the index falls back to trigram overlap scoring. This handles substrings and approximate matches without requiring a separate fuzzy search path.

Pebble key structure for the inverted index: `0x05 | term | 0x00 | ulid → posting list`. The posting list carries term frequency per field and field presence flags, allowing BM25 computation without reading the full engram record.

### HNSW Vector Index

Hierarchical Navigable Small World graph. In-memory per vault. O(log n) approximate nearest neighbor search with cosine similarity.

The HNSW graph is loaded from Pebble on startup and snapshotted back on graceful shutdown. Between snapshots, all mutations are in memory. This keeps vector search at memory speed — no disk I/O on the hot path.

Only activated when the embed plugin is present. Without the embed plugin, the HNSW goroutine in Phase 2 is a no-op and contributes no results to RRF.

### Adjacency Graph

Forward index (`engram → targets`) and reverse index (`engram ← sources`) stored in separate Pebble namespaces (0x03 forward, 0x04 reverse). The reverse index makes "what points to this engram?" efficient — critical for impact analysis when an engram's confidence or state changes.

Within each adjacency list, edges are stored weight-sorted. BFS in Phase 5 explores the heaviest edges first, which means the most significant associations are discovered before the traversal cap (500 nodes) is reached.

### Secondary Indexes

Thin indexes for lifecycle state, tag membership, and creator. Used for filter operations in Phase 6 and for administrative queries. Namespace 0x0B for the state index. These are small and fast — they store only the engram ULID, not the full record.

---

## 6. Wire Protocols

Four protocols are currently active. All share the same underlying engine — they are interface adapters, not separate implementations.

### MBP — Port 8474 (Muninn Binary Protocol)

Native binary protocol over TCP. 16-byte fixed header:

```
version(1) | type(1) | flags(2) | length(4) | correlation_id(8)
```

MessagePack payload. The correlation ID enables pipelining — a client can send multiple requests without waiting for responses, and route incoming responses by correlation ID. Responses can arrive out of order. This eliminates round-trip latency for bulk operations.

22 message types covering all CRUD operations, ACTIVATE, subscription management, and admin commands. This is the lowest-latency protocol. Use it for any client where you control the implementation.

### gRPC — Port 8477

Protocol buffers over HTTP/2. Streaming and unary RPCs for all core operations: Write, BatchWrite, Read, Activate, Link, Forget, Stat, Subscribe. API key authentication via "authorization" Bearer token or "x-api-key" metadata header. Supports keepalive, automatic reconnection, and multiplexing over a single HTTP/2 connection. Medium latency; excellent for polyglot systems with gRPC tooling available.

### REST — Port 8475

JSON over HTTP. Standard resource-oriented API:
- `POST /api/engrams` — write
- `POST /api/engrams/batch` — bulk write (up to 50)
- `GET /api/engrams/:id` — point read
- `DELETE /api/engrams/:id` — state transition (not hard delete by default)
- `POST /api/activate` — activation query
- `GET /api/admin/*` — administrative operations

Slowest of the three — JSON serialization overhead and no pipelining. Most compatible. Use it when you need to integrate from an environment that cannot use binary protocols.

### MCP — Port 8750

JSON-RPC over HTTP. Implements the Model Context Protocol for direct integration with AI agents.

19 tools exposed:

| Tool | Operation |
|---|---|
| `remember` | Write an engram |
| `remember_batch` | Write up to 50 engrams in a single call |
| `recall` | Activation query |
| `read` | Point read by ID |
| `forget` | Archive or delete |
| `link` | Create association |
| `contradictions` | List contradiction pairs |
| `status` | Vault health and statistics |
| `guide` | Vault-aware usage instructions for AI onboarding |
| `evolve` | Update engram content or confidence |
| `consolidate` | Merge similar engrams |
| `session` | Session context management |
| `decide` | Record a decision with confidence |
| `restore` | Restore an archived engram |
| `traverse` | Walk the association graph |
| `explain` | Explain why an engram scored as it did |
| `state` | Transition engram lifecycle state |
| `list_deleted` | List soft-deleted engrams |
| `retry_enrich` | Re-run enrichment on failed engrams |

On first connect, AI agents should call `muninn_guide` to receive vault-aware instructions on how and when to use memory, customized to the vault's behavior mode. MCP integration allows Claude, Cursor, and any other MCP-compatible client to use MuninnDB as their persistent memory without custom integration work.

### Web UI — Port 8476

Browser-based UI for browsing engrams, visualizing the association graph, and monitoring vault health. Also serves health and metrics endpoints.

---

## 7. Major Features and Subsystems

**Cluster/HA** — Cortex/Lobe replication and consensus coordination via internal/replication/, enabling horizontal scaling and fault tolerance.

**MQL Query Language** — Multi-vault query language in internal/query/mql/ for composing complex activation and link traversal queries.

**Vault Plasticity** — Dynamic vault sizing and schema adaptability in internal/auth/plasticity.go, allowing vaults to grow and reshape without full rebuilds.

**Novelty Detection** — Semantic change detection in internal/engine/novelty/, identifying engrams that represent genuinely new concepts.

**Vault Coherence Score** — Consistency metric in internal/engine/coherence/, measuring the quality of association weights and contradiction resolution.

**Episodic Store** — Temporal memory for session context and activation history in internal/episodic/, enabling time-aware retrieval.

**Provenance Tracking** — Complete audit trail in internal/provenance/, recording the origin and transformation history of every engram.

**Schema Versioning** — Multi-version schema support in internal/replication/schema_version.go, enabling rolling upgrades and backward compatibility.

---

## 8. The Muninn Operation Log (MOL)

The MOL is MuninnDB's replication and audit log. Every write operation is appended asynchronously after the Pebble commit. The MOL serves two purposes:

1. **Replication** — WAL streaming to replicas reads from the MOL. Each entry carries the operation type, vault ID, and engram ULID.
2. **Audit trail** — The MOL provides a sequential record of all mutations for debugging and compliance.

**Crash recovery** is handled by Pebble's internal WAL, not the MOL. Pebble uses an LSM-tree architecture with its own write-ahead log that is replayed automatically on startup. The `walSyncer` (see `internal/storage/wal_syncer.go`) fsyncs Pebble's WAL every 10ms, providing group-commit durability semantics. This is the same trade-off as MySQL's `innodb_flush_log_at_trx_commit=2` or PostgreSQL's `synchronous_commit=off` — maximum data loss on crash is bounded to the sync interval (10ms).

On startup, MuninnDB scans the MOL to recover the sequence counter for replication continuity. Sealed segments are retained for replica catch-up. The `SafePrune` method garbage-collects sealed segments once all replicas have confirmed receiving them, preventing unbounded log growth.

---

## 9. Performance Targets

| Operation | Target | Notes |
|---|---|---|
| Write (ACK) | <10ms | ERF encode + Pebble batch commit (fsync) + async MOL append |
| Point read | <2ms | L1 cache hit typical; sync.Map before Pebble |
| Activation query | <20ms | Full 6-phase pipeline, parallel Phase 2 |
| FTS only | <5ms | Inverted index + BM25, no graph traversal |
| Vector search only | <10ms | HNSW approximate NN in memory |
| BFS depth-2 | <5ms | Weight-sorted adjacency graph traversal |

The write ACK target is a hard guarantee. The query targets are design targets — actual performance depends on vault size, result set size, and hardware, but these are the numbers the system is designed around.

---

## 10. Scale Characteristics

| Tier | Engrams | Disk | Deployment |
|---|---|---|---|
| Personal | 10K | 17–40MB | Single binary |
| Power user | 100K | 170–400MB | Docker |
| Team | 1M | 1.7–4GB | Single node |
| Enterprise | 100M+ | 170–400GB | Sharded cluster |

Disk estimates assume ~1.7KB average with embedding, ~400 bytes without. Actual usage depends heavily on content length.

**Vault sharding:** Memory is personal. Each vault is an isolated namespace with its own indexes, its own HNSW graph, and its own cognitive worker state. Horizontal scaling is natural: shard by vault. A sharded cluster routes ACTIVATE requests to the correct vault shard and federates results when multi-vault activation is requested.

The single-binary deployment tier is intentional. MuninnDB should be trivially deployable for personal or small team use without infrastructure overhead. The same binary that runs as a personal memory store for one user can be the node in a sharded cluster serving 100 million engrams across an enterprise.

---

**See also:** [Retrieval Design](retrieval-design.md) · [Key-Space Schema](key-space-schema.md) · [Durability Guarantees](durability-guarantees.md) · [Entity Graph](entity-graph.md)
