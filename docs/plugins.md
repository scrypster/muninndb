# Plugin Architecture

> Three tiers. One guarantee: no plugin ever blocks the read/write path.

---

## 1. Three Tiers, One Guarantee

MuninnDB's plugin system has a single non-negotiable constraint: **no plugin ever touches the synchronous read/write path.** Enable the Embed plugin and your write latency doesn't change. Enable the Enrich plugin and your ACTIVATE queries don't wait for LLM calls. All plugin work is async. All of it is retroactive. None of it is in your critical path.

The three tiers build on each other:

| Tier | Plugin | Requires | What it adds |
|------|--------|----------|--------------|
| 1 | Core | nothing | Everything. Ships in the binary. |
| 2 | Embed | Tier 1 | Vector index, semantic recall, embedding providers |
| 3 | Enrich | Tier 2 | LLM summaries, entity extraction, typed relationships, semantic contradiction detection |

Tier 1 is not a stripped-down version of the database. It is the complete cognitive database. Tiers 2 and 3 make it smarter.

**Important:** Embed and Enrich are in-process plugins compiled into the MuninnDB binary. They are not separate executables or binaries. They are activated at startup via environment variables, not loaded dynamically.

---

## 2. Tier 1: Core

No configuration required. Ships as a single binary (~15MB on Alpine). Zero external dependencies.

**What you get:**

- Full-text search via BM25 inverted index
- Association graph with Hebbian learning (co-activation strengthens edges)
- Temporal priority — memories you use stay sharp, memories you ignore fade naturally
- Bayesian confidence scoring
- Structural contradiction detection
- Semantic triggers — `new_write`, `threshold_crossed`, `contradiction_detected`
- All four wire protocols: MBP (binary), gRPC, REST, MCP
- Web UI with graph visualization

**ACTIVATE in Tier 1** runs two parallel retrieval streams: FTS recall and temporal pool retrieval. Results are fused using RRF (Reciprocal Rank Fusion). Cognitively, this means the system finds engrams that match by text and engrams that are currently active in the temporal model — then fuses the two ranked lists into a single result set.

This is not a degraded mode. For structured knowledge bases, technical documentation, explicit concept recall, and anything where terminology is consistent, FTS + temporal scoring is excellent. Recall is roughly 60% on paraphrases — you'll miss "preventing duplicate charges" when the memory says "idempotency keys for payments." But within consistent vocabulary, Tier 1 is fully capable.

The choice to add Tier 2 is a recall calibration decision, not a requirement.

---

## 3. Tier 2: Embed Plugin

### What It Adds

The Embed plugin adds a third retrieval stream to ACTIVATE: vector similarity via an HNSW index. The three streams — FTS, HNSW, and temporal pool — are fused with weighted RRF (FTS weight 60, HNSW weight 40, Temporal weight 120).

With all three streams active, ACTIVATE finds engrams that match by text, engrams that match by semantic meaning, and engrams that are currently cognitively active — then produces a single ranked result set from the fusion.

**The recall jump:** FTS + temporal scoring achieves roughly 60% recall on paraphrases and semantic drift. FTS + HNSW + temporal scoring achieves 95%+. The gap is "idempotency keys for payments" vs. "preventing duplicate charges in the payment service." Same concept, different words. FTS misses it. Vectors catch it.

**Trigger system upgrade:** Subscription scoring gains vector similarity. Semantic triggers become significantly more capable — a subscription context of "payment reliability" will now surface memories about retry budgets, circuit breakers, and idempotency even when those exact words aren't in the context string.

### Installation and Configuration

Embed is activated by setting an embedding provider environment variable when starting the MuninnDB server:

```bash
# Bundled local model — on by default, no config needed
# Disable with MUNINN_LOCAL_EMBED=0
muninn server

# Ollama — local, zero API cost, works offline
export MUNINN_OLLAMA_URL="http://localhost:11434/llama2"
muninn server

# OpenAI
export MUNINN_OPENAI_KEY="sk-..."
# Optional: override OpenAI base URL (for LocalAI/OpenAI-compatible gateways)
export MUNINN_OPENAI_URL="http://localhost:8080/v1"
muninn server

# Voyage AI — optimized for retrieval tasks
export MUNINN_VOYAGE_KEY="pa-..."
muninn server

# Cohere
export MUNINN_COHERE_KEY="..."
muninn server

# Google (Gemini)
export MUNINN_GOOGLE_KEY="..."
muninn server

# Jina
export MUNINN_JINA_KEY="..."
muninn server

# Mistral
export MUNINN_MISTRAL_KEY="..."
muninn server
```

Provider comparison:

| Provider | Env Var | Model (default) | Dimensions | Cost | Privacy |
|----------|---------|-----------------|------------|------|---------|
| Bundled local | On by default | all-MiniLM-L6-v2 | 384 | Zero | Full (local) |
| Ollama | `MUNINN_OLLAMA_URL` | configurable | varies | Zero (local) | Full (local) |
| OpenAI | `MUNINN_OPENAI_KEY` | text-embedding-3-small | 1536 | Per token | API |
| Voyage AI | `MUNINN_VOYAGE_KEY` | voyage-3 | 1024 | Per token | API |
| Cohere | `MUNINN_COHERE_KEY` | embed-v4 | 1024 | Per token | API |
| Google | `MUNINN_GOOGLE_KEY` | text-embedding-004 | 768 | Per token | API |
| Jina | `MUNINN_JINA_KEY` | jina-embeddings-v3 | 1024 | Per token | API |
| Mistral | `MUNINN_MISTRAL_KEY` | mistral-embed | 1024 | Per token | API |

`MUNINN_OPENAI_URL` can optionally override the OpenAI base URL for compatible endpoints (for example LocalAI or an internal gateway). If set to an invalid value, MuninnDB skips OpenAI initialization instead of falling back to `api.openai.com`. This override also applies to the Enrich plugin when `MUNINN_ENRICH_URL` is set to an `openai://` provider — see [Tier 3](#4-tier-3-enrich-plugin) below.

### Retroactive Enrichment

Install the Embed plugin against a vault with existing data. You don't re-index manually. You don't write a migration script. You start the plugin and walk away.

The retroactive processor works in the background:

1. **New writes first** — embeddings are generated immediately after write ACK. New engrams get vectors before any existing engrams. This ensures the live system benefits immediately.

2. **Existing engrams by relevance** — the processor walks the existing vault in descending relevance order. The most important memories get vectors fastest.

3. **Non-blocking** — the processor runs on a separate goroutine pool with configurable concurrency. It yields to write and read operations. It cannot starve the main path.

**Zero-blocking guarantee on write:** Embedding generation happens after the ACK is sent to the client. If the embedding model is slow, unavailable, or rate-limited, the write succeeds without a vector. The engram is marked for retry. The embed worker picks it up on a backoff schedule.

This means your write path is never gated on an external model. Ever.

---

## 4. Tier 3: Enrich Plugin

### What It Adds

Tier 3 requires Tier 2. It adds LLM-powered intelligence across five dimensions:

**Abstractive summaries** — Not extractive. Not copy-paste of the first sentence. The LLM reads the engram content and generates a concise summary that captures the core meaning in normalized language. This summary is stored alongside the original content and used in FTS and semantic trigger scoring. Over time, summaries make the FTS layer significantly more capable — because the indexed text reflects meaning, not just the exact words used when the engram was written.

**Entity extraction** — The LLM extracts structured entities from engram content: people, organizations, tools, projects, dates, version numbers. Extracted entities are stored as structured metadata on the engram and become first-class query targets. "Who is mentioned in memories about the payment service?" becomes a real query.

**Typed relationship detection** — This is where Enrich starts to do things that feel qualitatively different. The LLM reads pairs of engrams and detects typed relationships between entities across them. "Steve manages the payment team" in one engram, "payment team owns the retry service" in another — Enrich detects and creates: `manages` (Steve → payment_team), `owns` (payment_team → retry_service). These become native typed associations in the adjacency graph, which strengthens the relevant Hebbian connections automatically. The graph gets denser without any manual curation.

Relationship types include: `manages`, `depends_on`, `implements`, `contradicts`, `replaces`, `owns`, `uses`, `authored_by`, and extensible custom types.

**Semantic contradiction detection** — The structural contradiction detector in Tier 1 catches obvious cases: same concept, opposite confidence signals, direct negation. It cannot catch this: "We deploy all services to AWS" and "We completed the migration to GCP last quarter." There is no structural signal. The contradiction lives in the meaning.

Enrich catches these. The LLM reads both engrams together, reasons about their claims, and fires a `contradiction_detected` event when it finds a logical conflict. For agents managing knowledge bases that evolve over time, this is the feature that prevents confident mistakes.

**LLM-assisted consolidation** — Vaults accumulate near-duplicate engrams. Same concept, written slightly differently at different times, from different sources. Consolidation identifies these clusters, presents them as candidates, and (with explicit confirmation) merges them. The merged engram inherits the union of both records' associations, the maximum stability value, and the averaged confidence. Associations from both source engrams are preserved in the merged result.

### Installation and Configuration

Enrich is activated by setting the `MUNINN_ENRICH_URL` environment variable (and optionally `MUNINN_ENRICH_API_KEY` or provider-specific keys) when starting the MuninnDB server:

```bash
# Ollama — local, zero cost
export MUNINN_ENRICH_URL="ollama://localhost:11434/llama3.2"
muninn server

# OpenAI — gpt-4o-mini for cost, gpt-4o for quality
export MUNINN_ENRICH_URL="openai://gpt-4o-mini"
export MUNINN_ENRICH_API_KEY="sk-..."
muninn server

# OpenAI-compatible gateway (LocalAI, Together AI, etc.)
# MUNINN_OPENAI_URL applies to both the Embed and Enrich plugins when using openai:// URLs.
# Note: use MUNINN_ENRICH_API_KEY for the enrich provider's API key — MUNINN_OPENAI_KEY
# is used by the Embed plugin only and is not shared with the Enrich plugin.
export MUNINN_ENRICH_URL="openai://your-model"
export MUNINN_ENRICH_API_KEY="your-api-key"
export MUNINN_OPENAI_URL="https://your-gateway.example.com/v1"
muninn server

# Anthropic
export MUNINN_ENRICH_URL="anthropic://claude-haiku-4-5-20251001"
export MUNINN_ANTHROPIC_KEY="sk-ant-..."
muninn server
```

Provider comparison:

| Provider | Model | Cost | Quality | Best for |
|----------|-------|------|---------|---------|
| Ollama | llama3.2, phi3 | Zero (local) | Good | Development, sensitive data |
| OpenAI | gpt-4o-mini | Low | Very good | Cost-sensitive production |
| OpenAI | gpt-4o | Medium | Excellent | Quality-critical production |
| Anthropic | claude-haiku-4-5 | Low | Very good | Cost-sensitive production |
| Anthropic | claude-sonnet-4-6 | Medium | Excellent | Quality-critical production |

For OpenAI-compatible gateways, MuninnDB accepts enrich JSON returned in either
`message.content` or `message.reasoning`. Some gateways emit structured JSON in
`reasoning` while leaving `content` empty.

### Retroactive Enrichment

Same guarantee as Tier 2: enable the plugin, walk away. The retroactive processor enriches existing engrams in the background — highest relevance first, zero impact on the read/write path.

Enrichment jobs are tracked. If an LLM call fails — rate limit, timeout, provider error — the engram is queued for retry with exponential backoff. Failed jobs can be manually triggered via the MCP tool `muninn_retry_enrich`:

```bash
# Retry enrichment for a specific engram (via MCP)
# Tool name: muninn_retry_enrich
# Arguments: vault="default", id="<engram-id>"

# Example (using MCP client):
# Call muninn_retry_enrich with vault="default" and id="e1"
```

You can also check enrichment status via the REST admin endpoint `/api/admin/embed/status`, which includes tier information and plugin registration status.

The retry mechanism means a temporary provider outage does not result in permanently un-enriched engrams. When the provider recovers, the queue processes automatically.

Retry and retroactive enrichment only mark entity and relationship stages complete
after the corresponding graph writes succeed. If persistence fails partway
through, MuninnDB leaves the stage incomplete so the work can be retried instead
of silently treating a partial write as finished.

---

## 5. The Retroactive Guarantee

Both plugins guarantee retroactive enrichment. This is worth stating plainly, because the alternative is the norm everywhere else.

Most vector database integrations require you to re-index when you switch embedding models. Most enrichment pipelines require you to write a migration that runs against existing data, wait for it to complete, verify it, and then cut over. If the migration fails halfway through, you figure out where it stopped and re-run from there.

MuninnDB plugins do not work this way. Install a plugin against a vault with ten thousand engrams. The plugin handles the backfill. You handle your application.

The retroactive processor has four properties:

**Non-blocking** — Runs on a separate goroutine pool. Never shares resources with the read/write path. You can watch your write latency while the retroactive processor is running. It doesn't move.

**Prioritized** — Processes engrams in descending relevance order. The engrams your ACTIVATE queries return most often get enriched first. The long tail gets enriched eventually. This means the system improves where it matters fastest.

**Idempotent** — Safe to restart, re-run, and resume. If the plugin crashes and restarts, it picks up from the remaining queue. No double-processing, no partial states, no manual cleanup.

**Observable** — Four metrics track retroactive processing progress:
- `enrich_enriched_total` — cumulative enriched count
- `enrich_failed_total` — cumulative failures
- `enrich_queue_depth` — remaining work
- `enrich_processing_duration_seconds` — per-engram timing

---

## 6. What You Never Have to Do

When you enable Tier 2 or Tier 3 on a running MuninnDB deployment:

- You do not change your write code. `Remember()` is unchanged.
- You do not change your activation queries. `ACTIVATE` automatically uses whichever retrieval streams are available.
- You do not re-index existing data manually.
- You do not run a migration.
- You do not update client configuration.

Plugins are checked at startup by reading environment variables. If a plugin is configured (e.g., `MUNINN_ENRICH_URL` is set), the MuninnDB server initializes it and registers it for use. If the environment variable is not set, the system operates without that plugin's functionality. To enable or change a plugin, update the environment variables and restart the MuninnDB server. The client API is identical across all three tiers.

This is the architectural consequence of the zero-blocking guarantee. Because all plugin work is async and all retrieval streams are optional fusions, the core system is never dependent on plugin state. Plugins enhance. They do not change the contract.

---

**See also:** [Semantic Triggers](semantic-triggers.md) · [Feature Reference](feature-reference.md)
