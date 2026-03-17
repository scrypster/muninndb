# MuninnDB Feature Reference

Personal reference doc — everything MuninnDB does in one place.

---

## What Is It

A cognitive memory database for AI agents. Stores memories ("engrams"), retrieves them using a neuroscience-inspired pipeline, and learns relationships between them automatically. Multi-tenant, multi-protocol, clusterable.

---

## Transport Protocols (4 ways to talk to it)

| Protocol | Port | Use Case |
|----------|------|----------|
| **REST/HTTP** | `:8740` (default) | Standard JSON API, SSE subscriptions, admin UI |
| **gRPC** | `:8745` (default) | High-performance binary protocol, server-streaming Activate, bidirectional Subscribe |
| **MBP** (MuninnDB Binary Protocol) | `:8748` (default) | Custom MessagePack-over-TCP framing with zstd compression, designed for persistent agent connections |
| **MCP** (Model Context Protocol) | `127.0.0.1:8750` (default, localhost only) | JSON-RPC 2.0 over HTTP — designed for LLM tool use (Claude, GPT, etc.) |

### SSE Push (Server-Sent Events)
- `GET /api/subscribe` opens a long-lived SSE connection over REST
- gRPC has bidirectional streaming `Subscribe` RPC
- MBP has native push frames
- All three protocols deliver real-time `ActivationPush` events

### Push Trigger Types
- `new_write` — a new engram was written that matches your subscription context
- `threshold_crossed` — a cognitive value (Hebbian weight, confidence) crossed your delta threshold
- `contradiction_detected` — two memories were found to contradict each other

---

## Core Operations

### 1. Writing Memories (Engrams)

**REST:** `POST /api/engrams` (single) · `POST /api/engrams/batch` (bulk)  
**gRPC:** `Write(WriteRequest)` · `BatchWrite(BatchWriteRequest)`  
**MCP:** `muninn_remember` (single) · `muninn_remember_batch` (bulk, max 50)

A memory consists of:
- **concept** — short label (max 512 bytes), required
- **content** — the actual information (max 16KB), required
- **tags** — topic tags for auto-association
- **type** — memory type (built-in enum name or free-form label)
- **type_label** — free-form label (e.g. "architectural_decision", "coding_pattern")
- **confidence** — 0.0–1.0 (default 1.0)
- **stability** — temporal scoring resistance in days (used in score reporting)
- **created_at** — custom timestamp (see below)
- **embedding** — pre-computed vector (optional, system can compute it)
- **associations** — initial links to other engrams
- **idempotent_id** — dedup key for safe retries
- **summary** — caller-provided one-line summary (skips background summarization)
- **entities** — caller-provided entity list (skips background entity extraction)
- **relationships** — caller-provided links to existing memories

#### Can you write historical memories?
**Yes.** Pass `created_at` with a past ISO 8601 timestamp. The ULID is still generated with current time (for ordering), but the engram's `CreatedAt` field stores the historical date. This affects temporal scoring — a memory from 2 years ago will score differently than one from today. Use case: seeding an agent's memory with historical knowledge.

#### Can you write future memories?
**Yes.** Pass `created_at` with a future timestamp. Use case: recording planned events, scheduled tasks, or anticipated deadlines. ACT-R temporal scoring will give them high activation until that time passes and access frequency determines their ongoing weight.

### What happens on write (automatically):
1. Persisted to Pebble (durable immediately)
2. Indexed in BM25 FTS (async, ~100ms lag)
3. Indexed in HNSW vector index (if embedding exists)
4. Contradiction detection (async) — checks for conflicting memories
5. Bayesian confidence update (if contradiction found)
6. Novelty detection (async) — finds near-duplicate memories, creates `refines` links
7. Auto-association — finds memories with overlapping tags, creates `relates_to` links
8. Semantic neighbor linking — finds vector-similar memories via HNSW, creates links
9. Coherence counters updated (orphan ratio, etc.)
10. Trigger system notified (pushes to subscribers)
11. PAS transition recorded (if enabled, records sequential patterns)
12. Memory type auto-classified (12 types — see Memory Types below)
13. Inline enrichment stored (if caller provided summary, entities, or relationships)

---

### 2. Retrieving Memories (ACTIVATE Pipeline)

**REST:** `POST /api/activate`  
**gRPC:** `Activate(ActivateRequest)` (server-streaming)  
**MCP:** `muninn_recall`

The ACTIVATE pipeline has **6 phases**:

| Phase | Name | What It Does |
|-------|------|-------------|
| **Phase 1** | Embed + Tokenize | Embeds the query context into a vector, tokenizes for FTS |
| **Phase 2** | Parallel Candidate Retrieval | Runs FTS (BM25), HNSW (vector), and decay pool in parallel. Also fetches PAS transition candidates and time-bounded candidates |
| **Phase 3** | RRF Fusion | Reciprocal Rank Fusion merges all candidate lists into a unified scored set |
| **Phase 4** | Hebbian Boost | Boosts candidates that were co-activated with previous results (learned associations) |
| **Phase 4.5** | PAS Transition Boost | Boosts candidates that sequentially followed previous activations (predictive) |
| **Phase 5** | BFS Traversal | Walks the association graph from top candidates, discovering related memories up to N hops deep |
| **Phase 6** | Final Scoring + Filter | Computes ACT-R score, applies filters, ranks, truncates, and builds response |

#### Scoring Models
- **ACT-R** (default, production) — `ContentMatch × softplus(BaseLevel + HebbianScale × HebbianBoost + TransitionBoost)`. Power-law temporal decay based on access count and recency. This is the only production scorer
- **CGDN** (Cognitive-Gated Divisive Normalization) — experimental. Multiplicative cognitive gating with divisive normalization across candidates. Requires `experimental_cgdn: true` in vault plasticity config

#### Score Components
- Semantic Similarity (vector cosine)
- Full-Text Relevance (BM25)
- Decay Factor (temporal, ACT-R power-law)
- Hebbian Boost (co-activation strength)
- Transition Boost (PAS sequential prediction)
- Access Frequency
- Recency

#### Traversal Profiles (for Phase 5 BFS)
- **default** — balanced, contradiction edges dampened
- **causal** — follow cause/effect/dependency chains
- **confirmatory** — find supporting evidence, contradiction edges excluded
- **adversarial** — surface conflicts and contradictions
- **structural** — follow project/person/hierarchy edges
- Auto-inferred from query context if not specified

#### Recall Modes (MCP shortcuts)
- `semantic` — high-precision vector search (threshold=0.3)
- `recent` — recency-biased, 1 hop (threshold=0.2)
- `balanced` — engine defaults
- `deep` — exhaustive graph traversal, 4 hops (threshold=0.1)

#### Filtering
- `created_after` / `created_before` — time bounds
- `tag` — filter by tag
- `state` — filter by lifecycle state
- `memory_type` — filter by type (fact, decision, etc.)

---

### 3. Associating Memories (Links)

**REST:** `POST /api/link`  
**gRPC:** `Link(LinkRequest)`  
**MCP:** `muninn_link`

#### Relationship Types (16 built-in)
| Type | Description |
|------|-------------|
| `supports` | Evidence or backing |
| `contradicts` | Conflicts with or refutes |
| `depends_on` | Requires the other to be understood |
| `supersedes` | Replaces or updates (other is outdated) |
| `relates_to` | General association (safe default) |
| `is_part_of` | Component or section of the other |
| `causes` | Cause or contributing factor |
| `preceded_by` | Chronologically follows the other |
| `followed_by` | Chronologically precedes the other |
| `created_by_person` | Authored by the person |
| `belongs_to_project` | Belongs to a project/context |
| `references` | Cites or links to |
| `implements` | Concrete realization (e.g. code for a spec) |
| `blocks` | Obstacle preventing progress |
| `resolves` | Solution or fix |
| `refines` | Near-duplicate refinement |
| `user_defined` | Custom (0x8000+) |

#### How associations are created
1. **Manually** — via `Link` / `muninn_link` with explicit source, target, relation, weight
2. **On write** — auto-association finds memories with overlapping tags
3. **On write** — semantic neighbor worker finds vector-similar memories via HNSW
4. **On write** — novelty detector finds near-duplicates, creates `refines` links
5. **On contradiction** — contradiction detector creates `contradicts` links
6. **During consolidation** — transitive inference (A→B, B→C ⇒ A→C)

#### How associations are strengthened
- **Hebbian learning** (automatic) — when two memories are co-activated in the same query, their association weight increases. "Neurons that fire together wire together." Managed by the HebbianWorker background goroutine
- **Manual weight update** — set weight 0.0–1.0 when creating a link
- **Consolidation** — transitive inference can create and strengthen edges

---

### 4. Bulk Insert

**REST:** `POST /api/engrams/batch`  
**gRPC:** `BatchWrite(BatchWriteRequest)`  
**MCP:** `muninn_remember_batch`

- Maximum 50 memories per batch
- Per-item error reporting (partial failure: successful items are kept)
- Counts as 1 rate-limit event, not N
- All async workers (FTS, novelty, auto-assoc, PAS) process all items
- More efficient than looping — fewer round-trips, better throughput

---

### 5. Other Core Operations

| Operation | REST | gRPC | MCP |
|-----------|------|------|-----|
| Read a single memory by ID | `GET /api/engrams/{id}` | `Read()` | `muninn_read` |
| Soft-delete (recoverable) | `DELETE /api/engrams/{id}` | `Forget()` | `muninn_forget` |
| Hard-delete (permanent) | `DELETE /api/engrams/{id}?hard=true` | `Forget(hard=true)` | — |
| Restore soft-deleted | — | — | `muninn_restore` |
| List deleted (recovery window) | — | — | `muninn_list_deleted` |
| Evolve (update + archive old) | — | — | `muninn_evolve` |
| Consolidate (merge N into 1) | — | — | `muninn_consolidate` |
| Record a decision | — | — | `muninn_decide` |
| Change lifecycle state | — | — | `muninn_state` |
| Traverse graph (BFS) | — | — | `muninn_traverse` |
| Explain score breakdown | — | — | `muninn_explain` |
| Retry enrichment | — | — | `muninn_retry_enrich` |
| Get usage guide | — | — | `muninn_guide` |
| Get session activity | `GET /api/session` | — | `muninn_session` |
| Get activity counts | `GET /api/activity-counts?days=N&until=YYYY-MM-DD` | — | — |
| Get contradictions | — | — | `muninn_contradictions` |
| Stats + coherence | `GET /api/stats` | `Stat()` | `muninn_status` |
| List engrams | `GET /api/engrams` | — | — |
| Get engram links | `GET /api/engrams/{id}/links` | — | — |
| List vaults | `GET /api/vaults` | — | — |
| Subscribe (SSE) | `GET /api/subscribe` | `Subscribe()` (bidi stream) | — |
| Health check | `GET /api/health` | — | — |
| Readiness probe | `GET /api/ready` | — | — |
| Worker stats | `GET /api/workers` | — | — |

---

## Cognitive Features (what makes it "think")

### Hebbian Learning
Co-activated memories have their association weights strengthened automatically. Background `HebbianWorker` processes co-activation events from an in-memory ring buffer (`ActivationLog`).

### ACT-R Temporal Scoring
Power-law decay based on access count and time since last access. Frequently accessed memories stay strong. Configurable per vault via `actr_decay` and `actr_heb_scale`.

### Bayesian Confidence
Memories have a confidence score (0.0–1.0) that's adjusted based on evidence:
- Corroboration increases confidence
- Contradiction decreases confidence
- Background `ConfidenceWorker` processes updates

### Contradiction Detection
Background `ContradictWorker` analyzes new writes for semantic conflicts with existing memories. When detected:
- Creates `contradicts` association
- Reduces confidence of both memories
- Fires `contradiction_detected` trigger event

### Predictive Activation Signal (PAS)
Sequential activation tracking: records which memories appeared in activation N and then activation N+1 for the same vault. Builds a transition probability table.
- **TransitionWorker** — background goroutine processes transition events
- **Tiered storage** — hot tier (in-memory `sync.Map`) with periodic flush to Pebble (warm tier)
- **Retrieval integration** — Phase 4.5 boosts candidates predicted by transition patterns; Phase 2 can inject transition-predicted candidates
- **Configurable per vault** — `predictive_activation` (bool), `pas_max_injections` (0–10)

### Novelty Detection
Write-time near-duplicate detection using Jaccard similarity. When a near-duplicate is found:
- Creates a `refines` relationship
- Does NOT block the write (async, off hot path)

### Coherence Tracking
Per-vault metrics tracking memory graph health:
- Orphan ratio (memories with no associations)
- Contradiction density
- Duplication pressure
- Temporal variance
- Overall coherence score

---

## Consolidation (Background Maintenance)

Runs every 6 hours (configurable). 5-phase pipeline per vault:

| Phase | Name | What It Does |
|-------|------|-------------|
| 1 | Activation Replay | Replays recent activation patterns to reinforce Hebbian weights |
| 2 | Semantic Dedup | Finds near-duplicate memories, merges them (max 100 per run) |
| 3 | Schema Promotion | Promotes frequently-referenced concepts to schema nodes |
| 4 | Decay Acceleration | *(Disabled)* — ACT-R handles temporal scoring at query time |
| 5 | Transitive Inference | Infers new edges: if A→B and B→C, create A→C (max 1000 per run) |

---

## Lifecycle States

Memories have a state machine:

`planning` → `active` → `paused` / `blocked` → `completed` / `cancelled` → `archived`

Also: `soft_deleted` (recoverable within 7 days)

Default on write: `active`

---

## Memory Types (12 built-in + free-form labels)

Auto-classified by the enrichment pipeline, or set explicitly by the caller.

| Type | Enum | Description |
|------|------|-------------|
| `fact` | 0 | Factual information, data points |
| `decision` | 1 | Choices made with rationale |
| `observation` | 2 | Something noticed, insights |
| `preference` | 3 | Opinions, personal choices |
| `issue` | 4 | Bugs, problems, defects |
| `task` | 5 | Action items, to-dos |
| `procedure` | 6 | How-to, workflows, processes |
| `event` | 7 | Something that happened, temporal |
| `goal` | 8 | Objectives, targets, intentions |
| `constraint` | 9 | Rules, limitations, requirements |
| `identity` | 10 | About a person, role, entity |
| `reference` | 11 | Documentation, specifications |

In addition to the enum, each memory can have a free-form **TypeLabel** (e.g. `architectural_decision`, `coding_pattern`, `meeting_note`). The enum provides structured filtering; the label provides specificity. Both are set automatically by the enrichment pipeline or provided by the caller at write time.

Backward compatible: `bugfix` is accepted as an alias for `issue`.

---

## Multi-Tenancy (Vaults)

- Every operation is scoped to a **vault** (namespace)
- Default vault: `"default"`
- Each vault has its own workspace prefix (8-byte key prefix in Pebble)
- Vaults are isolated: memories, associations, indexes, coherence, PAS transitions

### Per-Vault Plasticity Config
Every cognitive behavior is tunable per vault:

| Setting | Default | Range | Description |
|---------|---------|-------|-------------|
| `preset` | `"default"` | default/reference/scratchpad/knowledge-graph | Base behavior template |
| `hebbian_enabled` | true | bool | Hebbian co-activation learning |
| `temporal_enabled` | true | bool | Temporal decay scoring |
| `auto_link_neighbors` | true | bool | Semantic neighbor auto-linking on write |
| `hop_depth` | 2 | 0–8 | BFS traversal depth |
| `semantic_weight` | 0.6 | 0–1 | Vector similarity weight |
| `fts_weight` | 0.3 | 0–1 | BM25 full-text weight |
| `relevance_floor` | 0.05 | 0–1 | Minimum activation threshold |
| `temporal_halflife` | 30 | days | Stability / decay resistance |
| `traversal_profile` | auto | string | Default traversal profile |
| `actr_decay` | 0.5 | 0.01–2.0 | ACT-R power-law exponent |
| `actr_heb_scale` | 4.0 | 0–50 | Hebbian amplifier in ACT-R |
| `experimental_cgdn` | false | bool | Enable experimental CGDN scorer |
| `predictive_activation` | true | bool | Enable PAS |
| `pas_max_injections` | 5 | 0–10 | Max PAS candidates to inject |
| `behavior_mode` | `"autonomous"` | autonomous/prompted/selective/custom | How the AI should use memory (see below) |
| `behavior_instructions` | `""` | string | Custom instructions for "custom" mode |
| `inline_enrichment` | `"caller_preferred"` | caller_only/caller_preferred/background_only/disabled | How inline vs background enrichment interact (see below) |
| `enrichment_enabled` | true | bool | Kill switch for all enrichment on this vault |
| `max_engrams` | 0 (unlimited) | int | Auto-prune when exceeded |
| `retention_days` | 0 (unlimited) | float | Auto-prune older than N days |

### Presets
- **default** — balanced, all features on, autonomous behavior
- **reference** — long-term knowledge, minimal decay, strong Hebbian, autonomous behavior
- **scratchpad** — short-lived, high recency bias, no Hebbian, PAS off, selective behavior
- **knowledge-graph** — deep traversal (4 hops), strong Hebbian (8.0 scale), autonomous behavior

### Behavior Modes (How the AI Uses Memory)

Configurable per vault. Controls what the `muninn_guide` tool tells AI agents about when and how to use memory.

| Mode | Description |
|------|-------------|
| **autonomous** (recommended) | AI remembers proactively — decisions, preferences, errors, context. Recalls before every task. |
| **prompted** | AI only stores memories when the user explicitly asks. Recalls only when asked. |
| **selective** | Auto-remembers decisions and errors. Other memories only when asked. |
| **custom** | Free-form `behavior_instructions` text injected into the guide verbatim. |

Set during `muninn init` (wizard step), via web UI, or via REST admin API (`PUT /api/admin/vault/{name}/plasticity`).

---

## MCP Guide (AI Onboarding)

When an AI agent first connects via MCP, it can call `muninn_guide` to receive vault-aware usage instructions. The guide includes:

- **Memory strategy** based on the vault's behavior mode
- **Available tools** with "when to use" guidance
- **Vault configuration summary** (which features are enabled)
- **Quick-start tips** for effective memory use

This is what makes MuninnDB plug-and-play — the AI doesn't need pre-configured instructions about how to use memory. It asks the database, and the database tells it.

The guide also teaches **atomic memory writing** — each memory should capture one concept, one decision, or one fact. This produces sharper embeddings, better associations, and more accurate contradiction detection.

---

## Inline Enrichment (Caller-Side)

The calling LLM already has full conversation context when it decides to remember something. Rather than running a separate background LLM to guess at entities, summaries, and types, MuninnDB lets the caller provide this data directly at write time.

### Optional fields on `muninn_remember` / `muninn_remember_batch`:
- **summary** — one-line summary (skips background summarization)
- **entities** — `[{"name": "PostgreSQL", "type": "database"}]` (skips entity extraction)
- **relationships** — `[{"target_id": "01ABC...", "relation": "depends_on", "weight": 0.9}]`
- **type** / **type_label** — memory classification

### Inline Enrichment Modes (per vault)

| Mode | Behavior |
|------|----------|
| **caller_preferred** (default) | Use caller-provided fields; run background enrichment only for missing fields |
| **caller_only** | Trust the caller entirely; skip background enrichment when caller provides data |
| **background_only** | Ignore caller enrichment fields; always run background pipeline (legacy) |
| **disabled** | No enrichment at all |

### Why this matters
- **Better quality** — the calling LLM has the conversation context the background LLM doesn't
- **Zero extra cost** — no additional LLM API calls
- **Zero latency** — enrichment happens inline with the write
- **Graceful degradation** — if the AI provides nothing, background enrichment still works

---

## Background Enrichment Pipeline

When the enrich plugin is active, a modular 4-stage pipeline runs asynchronously after writes:

| Stage | What It Does | Config Flag |
|-------|-------------|-------------|
| **Summarization** | Generates a concise summary | `enrich_summary` |
| **Entity Extraction** | Extracts people, tools, projects, etc. | `enrich_entities` |
| **Relationship Detection** | Detects typed relationships between entities | `enrich_relationships` |
| **Classification** | Maps content to one of 12 memory types + free-form label | `enrich_classification` |

### Enrichment Modes

| Mode | LLM Calls | What It Produces |
|------|-----------|-----------------|
| **full** (default) | All 4 stages | Summary, entities, relationships, classification |
| **light** | 1 stage only | Summary + key points (minimal cost) |

Each stage can be individually enabled/disabled via server config. Stages are also skipped when the caller already provided that data (inline enrichment integration).

---

## Authentication

- **API Keys** — Bearer token per vault, created via admin API
- **Admin Sessions** — session cookie for admin endpoints
- **Vault visibility** — fail-closed by default (require key unless explicitly public)
- **Cluster auth** — separate auth for inter-node communication

---

## Admin Operations

All via `POST/GET/PUT/DELETE /api/admin/*` (require admin session):

- API key management (create, list, revoke)
- Vault config (set visibility, plasticity)
- Change admin password
- Vault lifecycle: delete, clear, clone, merge, export, import, reindex FTS
- Plugin management (list, configure, embed status)
- Online backup (Pebble checkpoint)
- Consolidation trigger (`POST /api/consolidation/run`)

---

## Clustering

### Architecture: Cortex/Lobe Model
- **Cortex** — leader node, handles writes, runs cognitive workers
- **Lobe** — follower node, handles reads, streams WAL from Cortex
- **Sentinel** — monitors cluster health
- **Observer** — read-only node

### Capabilities
- Leader election with quorum
- WAL streaming replication (MOL — MuninnDB Operation Log)
- Automatic failover (`POST /api/admin/cluster/failover`)
- Node add/remove
- TLS certificate rotation
- Cognitive consistency checking across nodes
- Cognitive effect forwarding (Lobe → Cortex)

### Cluster Admin Endpoints
- Enable/disable cluster mode
- Add/remove nodes
- Test node connectivity
- View cluster events (SSE)
- Regenerate cluster token
- Update cluster settings

---

## Storage Engine

### Pebble (LSM key-value store)
- RocksDB-compatible, written in Go (by CockroachDB)
- L1 in-memory cache for hot engrams (LRU, vault-scoped)
- Batch iterator optimization for multi-get (sorted forward seeks)

### ERF v2 (Engram Record Format)
- Binary format with separate key prefixes:
  - `0x01` — content key
  - `0x02` — metadata key (100-byte fixed, for fast reads without content)
  - `0x18` — embedding key (separated to avoid loading large vectors unnecessarily)
  - `0x19` — TypeLabel tagged extension field (free-form memory type label)

### WAL/MOL (Write-Ahead Log)
- MuninnDB Operation Log for crash recovery
- Numerical sequence sorting for correct replay order
- Used for replication streaming in cluster mode

### Indexes
- **HNSW** — per-vault vector similarity index (Hierarchical Navigable Small World graph)
- **BM25 FTS** — full-text search with TF-IDF scoring
- **Adjacency graph** — in-memory association graph for Hebbian lookups

---

## Plugin System

### Embedding Plugins (8 providers)
- **Local** (bundled, default) — offline, no API key, no setup required
- **Ollama** — self-hosted LLM embedding
- **OpenAI** — OpenAI embedding API
- **Voyage** — Voyage AI embedding
- **Cohere** — Cohere embed-v3/v4, multilingual
- **Google** — Gemini text-embedding-004
- **Jina** — Jina embeddings-v3, code + multilingual
- **Mistral** — Mistral embed
- Supports 384, 768, 1536 dimensions

### Enrichment Plugins
- **Ollama** — self-hosted LLM enrichment
- **OpenAI** — OpenAI enrichment
- **Anthropic** — Claude-based enrichment
- 4-stage pipeline per engram: entity extraction, relationship extraction, classification, summarization
- Retroactive processor for backfilling existing memories
- Health checking with automatic unhealthy detection
- Modular: each stage can be individually enabled/disabled

---

## Observability

- Prometheus metrics endpoint
- Structured logging (slog)
- Per-request tracing with request IDs
- Worker stats endpoint (`GET /api/workers`)
- Coherence scores in stats response
- Rate limiting (global + per-IP, configurable via env vars)

---

## Web UI

Built-in admin web UI served at the REST port root. Provides visual management of vaults, memories, and configuration.

---

## MCP Tool Summary (35 tools)

For quick reference, these are the MCP tools available to AI agents:

1. `muninn_remember` — store a memory
2. `muninn_remember_batch` — store multiple memories at once (max 50)
3. `muninn_recall` — semantic search with modes and profiles
4. `muninn_read` — fetch by ID
5. `muninn_forget` — soft-delete
6. `muninn_link` — create/strengthen association
7. `muninn_contradictions` — check for contradictions
8. `muninn_status` — health and stats
9. `muninn_guide` — get vault-aware usage instructions (call on first connect)
10. `muninn_evolve` — update a memory (archive old version)
11. `muninn_consolidate` — merge N memories into 1
12. `muninn_session` — recent activity summary
13. `muninn_decide` — record a decision with rationale
14. `muninn_restore` — recover a soft-deleted memory
15. `muninn_traverse` — BFS graph exploration
16. `muninn_explain` — score breakdown for debugging
17. `muninn_state` — change lifecycle state
18. `muninn_list_deleted` — list recoverable deletions
19. `muninn_retry_enrich` — re-queue for plugin enrichment
20. `muninn_remember_tree` — write an entire nested hierarchy as linked engrams; returns root_id and node_map. See [docs/hierarchical-memory.md](hierarchical-memory.md)
21. `muninn_recall_tree` — retrieve the complete, ordered hierarchy rooted at a given engram ID. See [docs/hierarchical-memory.md](hierarchical-memory.md)
22. `muninn_add_child` — append or insert a single child node into an existing tree without resending the whole structure. See [docs/hierarchical-memory.md](hierarchical-memory.md)
23. `muninn_entities` — list all entities in a vault sorted by mention count; optional state filter (active, deprecated, merged, resolved). See [docs/entity-graph.md](entity-graph.md)
24. `muninn_entity` — full aggregate view of one named entity: metadata, engrams mentioning it, relationships, and co-occurring entities. See [docs/entity-graph.md](entity-graph.md)
25. `muninn_entity_clusters` — co-occurrence cluster discovery; returns entity pairs that frequently appear together, sorted by count. See [docs/entity-graph.md](entity-graph.md)
26. `muninn_entity_state` — set the lifecycle state of a named entity (active, deprecated, merged, resolved); use merged_into when state=merged. See [docs/entity-graph.md](entity-graph.md)
27. `muninn_entity_timeline` — chronological view of an entity's evolution: all engrams mentioning it sorted oldest-first. See [docs/entity-graph.md](entity-graph.md)
28. `muninn_find_by_entity` — fast reverse-index lookup returning all memories that mention a given entity; O(matches). See [docs/entity-graph.md](entity-graph.md)
29. `muninn_similar_entities` — trigram duplicate detection; returns pairs of entity names above a similarity threshold (default 0.85). See [docs/entity-graph.md](entity-graph.md)
30. `muninn_merge_entity` — merge entity_a into canonical entity_b: relinks all engrams, marks entity_a as merged; supports dry_run. See [docs/entity-graph.md](entity-graph.md)
31. `muninn_export_graph` — export the entity relationship graph as JSON-LD (default) or GraphML for visualisation or external analysis. See [docs/entity-graph.md](entity-graph.md)
32. `muninn_feedback` — SGD scoring weight update; pass `useful` (bool) to signal whether a retrieved engram was helpful. Note: `useful` is the external parameter — `direction` is an internal variable in scoring/weights.go and is never exposed.
33. `muninn_provenance` — full audit trail for an engram: who wrote it, what changed, and why. Implementation: internal/provenance/
34. `muninn_replay_enrichment` — re-run the enrichment pipeline delta on one or more engrams that are missing specific stages (entities, relationships, classification, summary); supports dry_run
35. `muninn_where_left_off` — most recently accessed memories for session context; uses the 0x22 LastAccess index for O(limit) performance regardless of vault size
