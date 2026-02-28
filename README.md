# MuninnDB

**Memory that strengthens with use, fades when unused, and pushes to you when it matters** — accessible over MCP, REST, gRPC, or SDK.

*Provisional patent filed Feb 26, 2026 on the core cognitive primitives (engine-native Ebbinghaus decay, Hebbian learning, Bayesian confidence, semantic triggers). This helps protect the project so we can keep it open and innovative for everyone.*

[![CI](https://github.com/scrypster/muninndb/actions/workflows/ci.yml/badge.svg)](https://github.com/scrypster/muninndb/actions/workflows/ci.yml)
[![License](https://img.shields.io/badge/license-BSL%201.1-blue)](LICENSE)
[![Go](https://img.shields.io/badge/go-1.22%2B-00ADD8)](https://go.dev)
[![Status](https://img.shields.io/badge/status-alpha-orange)](https://github.com/scrypster/muninndb/releases)

> **Prerequisites:** None. Single binary, zero dependencies, zero configuration required.
> To uninstall: `rm $(which muninn)` and delete `~/.muninn`.

---

## Try It — 30 Seconds

**macOS / Linux:**

```bash
# 1. Install
curl -sSL https://muninndb.com/install.sh | sh

# 2. Start (first-run setup is automatic)
muninn start
```

**Windows (PowerShell):**

```powershell
# 1. Install
irm https://muninndb.com/install.ps1 | iex

# 2. Start (first-run setup is automatic)
muninn start
```

```bash
# 3. Store a memory
curl -sX POST http://localhost:8475/api/engrams \
  -H 'Content-Type: application/json' \
  -d '{"concept":"payment incident","content":"We switched to idempotency keys after the double-charge incident in Q3"}'

# 4. Ask what is relevant RIGHT NOW
curl -sX POST http://localhost:8475/api/activate \
  -H 'Content-Type: application/json' \
  -d '{"context":["debugging the payment retry logic"]}'
```

That Q3 incident surfaces. You never mentioned it. MuninnDB connected the concepts.

**Web UI:** `http://localhost:8476` · **Admin:** `root` / `password` (change after first login)

---

## Connect Your AI Tools

MuninnDB auto-detects and configures Claude Desktop, Cursor, OpenClaw, Windsurf, VS Code, and others:

```bash
muninn init
```

Follow the prompts. Done. Your AI tools now have persistent, cognitive memory.

**Manual MCP configuration** — if you prefer to configure by hand:

<details>
<summary>Claude Desktop</summary>

Add to `~/Library/Application Support/Claude/claude_desktop_config.json` (macOS) or `%APPDATA%\Claude\claude_desktop_config.json` (Windows):

```json
{
  "mcpServers": {
    "muninn": {
      "type": "http",
      "url": "http://localhost:8750/mcp"
    }
  }
}
```
</details>

<details>
<summary>Claude Code / CLI</summary>

Add to `~/.claude.json`:

```json
{
  "mcpServers": {
    "muninn": {
      "type": "http",
      "url": "http://localhost:8750/mcp"
    }
  }
}
```
</details>

<details>
<summary>Cursor</summary>

Add to `~/.cursor/mcp.json`:

```json
{
  "mcpServers": {
    "muninn": {
      "type": "http",
      "url": "http://localhost:8750/mcp"
    }
  }
}
```
</details>

<details>
<summary>OpenClaw</summary>

Add to `~/.openclaw/mcp.json`:

```json
{
  "mcpServers": {
    "muninn": {
      "type": "http",
      "url": "http://localhost:8750/mcp"
    }
  }
}
```
</details>

<details>
<summary>Windsurf</summary>

Add to `~/.codeium/windsurf/mcp_config.json`:

```json
{
  "mcpServers": {
    "muninn": {
      "type": "http",
      "url": "http://localhost:8750/mcp"
    }
  }
}
```
</details>

<details>
<summary>VS Code</summary>

Add to `.vscode/mcp.json` in your workspace:

```json
{
  "servers": {
    "muninn": {
      "type": "http",
      "url": "http://localhost:8750/mcp"
    }
  }
}
```
</details>

MuninnDB exposes **19 MCP tools** — store, activate, search, batch insert, get usage guidance, manage vaults, and more. On first connect, call `muninn_guide` for vault-aware instructions. No token required against the default vault. [Full MCP reference →](https://muninndb.com/docs)

---

## What Just Happened

Most databases store data and wait. MuninnDB stores *memory traces* — called **engrams** — and continuously works on them in the background. When you called `activate`, it ran a 6-phase pipeline: parallel full-text + vector search, fused the results, applied Hebbian co-activation boosts from past queries, injected predictive candidates from sequential patterns, traversed the association graph, and scored everything with ACT-R temporal weighting — in under 20ms.

The Q3 incident surfaced because MuninnDB understood that *"payment retry logic"* and *"idempotency keys after a double-charge"* are part of the same conversation. You never wrote that relationship. It emerged from semantic proximity and how these concepts travel together. That is the difference between a database and memory.

[Deep dive: How Memory Works →](docs/how-memory-works.md)

---

## Why MuninnDB

- **Temporal priority** — the database continuously recalculates what matters based on how recently and how often you've accessed each memory. Memories you use stay sharp. Memories you ignore fade naturally. The database moves while you sleep.
- **Hebbian learning** — memories activated together automatically form associations. Edges strengthen with co-activation, fade when the pattern stops. You never define a schema of relationships.
- **Predictive activation** — the database tracks sequential patterns across activations and learns to surface the *next* memory before you ask for it. Recall@10 improves 21% in workflow-oriented use cases.
- **Semantic triggers** — subscribe to a context. The database pushes when something becomes relevant — not because you queried, but because *relevance changed*. No polling. No cron. The DB initiates.
- **Bayesian confidence** — every engram tracks how sure MuninnDB is. Reinforcing memories raise confidence; contradictions lower it. Grounded in evidence, not a label you assign.
- **Plug-and-play AI onboarding** — call `muninn_guide` and the database tells your AI exactly how to use memory, customized to the vault's configuration. No manual prompt engineering.
- **Retroactive enrichment** — add the embed or enrich plugin and every existing memory upgrades automatically in the background. No migration. No code change. The database improves what it already holds.
- **Bulk insert** — batch up to 50 memories in a single call across all protocols (REST, gRPC, MCP). Efficient for data seeding, migration, and high-throughput agents.
- **Four protocols** — MBP (binary, <10ms ACK), REST (JSON), gRPC (protobuf), MCP (AI agents). Pick your stack; they all hit the same cognitive engine.
- **Single binary** — no Redis, no Kafka, no Postgres dependency. One process. One install command. Runs on a MacBook or a 3-node cluster.

---

## Examples

**REST — the full cycle:**

```bash
# Write
curl -sX POST http://localhost:8475/api/engrams \
  -H 'Content-Type: application/json' \
  -d '{
    "concept": "auth architecture",
    "content": "Short-lived JWTs (15min), refresh tokens in HttpOnly cookies, sessions server-side in Redis",
    "tags": ["auth", "security"]
  }'

# Activate by context (returns ranked, time-weighted, associated memories)
curl -sX POST http://localhost:8475/api/activate \
  -H 'Content-Type: application/json' \
  -d '{"context": ["reviewing the login flow for the mobile app"], "max_results": 5}'

# Search by text
curl 'http://localhost:8475/api/engrams?q=JWT&vault=default'
```

**Python SDK:**

```python
from muninn import MuninnClient

async with MuninnClient("http://localhost:8475") as m:
    # Store
    await m.write(vault="default", concept="auth architecture",
                  content="Short-lived JWTs, refresh in HttpOnly cookies")

    # Activate — context-aware, ranked, cognitively weighted
    result = await m.activate(vault="default",
                              context=["reviewing the login flow"],
                              max_results=5)
    for item in result.activations:
        print(f"{item.concept}  score={item.score:.3f}")
```

```bash
pip install muninn-python
```

**LangChain integration:**

```python
from muninn.langchain import MuninnDBMemory
from langchain.chains import ConversationChain

memory = MuninnDBMemory(vault="my-agent")
chain = ConversationChain(llm=your_llm, memory=memory)
# Every turn is stored. Every response draws on relevant past context.
```

**Go SDK:**

```go
import "github.com/scrypster/muninndb/sdk/go/muninn"

client := muninn.NewClient("http://localhost:8475", "your-api-key")
id, _ := client.Write(ctx, "default", "auth architecture",
    "Short-lived JWTs, refresh in HttpOnly cookies", []string{"auth"})
resp, _ := client.Activate(ctx, "default", []string{"login flow"}, 5)
```

```bash
go get github.com/scrypster/muninndb/sdk/go/muninn
```

**Node.js / TypeScript SDK:**

```typescript
import { MuninnClient } from '@muninndb/client';

const client = new MuninnClient({ token: 'your-api-key' });
const { id } = await client.write({ concept: 'auth architecture',
    content: 'Short-lived JWTs, refresh in HttpOnly cookies' });
const result = await client.activate({ context: ['login flow'] });
```

```bash
npm install @muninndb/client
```

**PHP SDK:**

```php
use MuninnDB\MuninnClient;

$client = new MuninnClient(token: 'your-api-key');
$result = $client->write(concept: 'auth architecture',
    content: 'Short-lived JWTs, refresh in HttpOnly cookies');
$memories = $client->activate(context: ['login flow']);
```

```bash
composer require muninndb/client
```

[More examples →](sdk/python/examples/) · [Full API reference →](https://muninndb.com/docs)

---

## License

MuninnDB uses the **Business Source License 1.1** (BSL 1.1).

- Free for individuals, hobbyists, researchers, and open-source projects.
- Free for small organizations (<50 employees **and** <$5M revenue).
- Free for all internal use.
- **Commercial hosted/SaaS/DBaaS/managed services require a license from the author.**
- Automatically becomes Apache 2.0 on February 26, 2030.
- Provisional patent filed Feb 26, 2026 on the core cognitive engine.

Full terms: [LICENSE](LICENSE). See also [CLA](CLA.md) for contributors.

---

## Configuration

MuninnDB works out of the box with no configuration. The bundled local embedder is included — offline, no API key, no setup.

When you're ready to customize:

| What | How |
|------|-----|
| Embedder: bundled (default) | On automatically — set `MUNINN_LOCAL_EMBED=0` to disable |
| Embedder: Ollama | `MUNINN_OLLAMA_URL=ollama://localhost:11434/nomic-embed-text` |
| Embedder: OpenAI | `MUNINN_OPENAI_KEY=sk-...` |
| Embedder: Voyage | `MUNINN_VOYAGE_KEY=pa-...` |
| Embedder: Cohere | `MUNINN_COHERE_KEY=...` |
| Embedder: Google (Gemini) | `MUNINN_GOOGLE_KEY=...` |
| Embedder: Jina | `MUNINN_JINA_KEY=...` |
| Embedder: Mistral | `MUNINN_MISTRAL_KEY=...` |
| LLM enrichment | `MUNINN_ENRICH_URL=anthropic://claude-haiku-4-5-20251001` + `MUNINN_ANTHROPIC_KEY=sk-ant-...` |
| Data directory | `MUNINNDB_DATA=/path/to/data` (default: `~/.muninn/data`) |
| Memory limit | `MUNINN_MEM_LIMIT_GB=4` |

**Docker:**

```bash
docker run -d \
  --name muninndb \
  -p 8474:8474 -p 8475:8475 -p 8476:8476 -p 8750:8750 \
  -v muninndb-data:/data \
  ghcr.io/scrypster/muninndb:latest
```

[Full self-hosting guide →](docs/self-hosting.md)

---

## Documentation

| | |
|---|---|
| [Quickstart](docs/quickstart.md) | Detailed install, Docker, embedder setup, first vault |
| [How Memory Works](docs/how-memory-works.md) | The neuroscience behind why this works |
| [Architecture](docs/architecture.md) | ERF format, 6-phase engine, wire protocols, cognitive workers |
| [Cognitive Primitives](docs/cognitive-primitives.md) | Temporal scoring, Hebbian learning, Bayesian confidence, PAS |
| [Semantic Triggers](docs/semantic-triggers.md) | Push-based memory — how and why |
| [Auth & Vaults](docs/auth.md) | Two-layer model, API keys, full vs. observe mode |
| [Plugins](docs/plugins.md) | Embed + enrich — retroactive enrichment without code changes |
| [Feature Reference](docs/feature-reference.md) | Complete list of every feature, operation, and config option |
| [vs. Other Databases](docs/vs-other-databases.md) | Full comparison with vector, graph, relational, document |

---

## Contributing

PRs welcome. For large changes, open an issue first.

By submitting a pull request, you agree to the [Contributor License Agreement](CLA.md).

Ports at a glance: `8474` MBP · `8475` REST · `8476` Web UI · `8477` gRPC · `8750` MCP

---

## Troubleshooting

<details>
<summary>MCP connection fails with schema validation error</summary>

MuninnDB uses `"type": "http"` for MCP transport. Older versions of some AI tools (notably Claude Code before v2.1.53) only supported `"type": "sse"`. If your tool reports a schema validation or config parsing error, change `"type": "http"` to `"type": "sse"` in your config file. MuninnDB's server handles both transports — only the client-side config needs to match what your tool supports.
</details>

<details>
<summary>muninn_remember or muninn_recall hangs</summary>

Check that the server is running: `muninn status`. If the server is running but tools hang, check `muninn logs` for errors. The most common cause is a stale MCP config pointing to the wrong port — verify the URL in your config matches `http://localhost:8750/mcp`.
</details>

<details>
<summary>muninn: command not found</summary>

The binary must be in your `PATH`. On macOS/Linux, the default install location is `~/.local/bin/muninn` — run `echo $PATH` to verify it includes `~/.local/bin`. If not, add `export PATH="$HOME/.local/bin:$PATH"` to your `~/.zshrc` or `~/.bashrc` and restart your shell. On Windows, the default is `%LOCALAPPDATA%\muninn` — run `$env:PATH` in PowerShell to verify.
</details>

<details>
<summary>Windows: DLL or ORT initialization error on first start</summary>

The bundled local embedder uses ONNX Runtime, which requires the Visual C++ Redistributable on Windows. Most machines already have it. If you see an error about `onnxruntime.dll` or "ORT environment init", install the [Visual C++ 2019+ Redistributable (x64)](https://aka.ms/vs/17/release/vc_redist.x64.exe) and restart muninn.
</details>

---

*Named after Muninn — one of Odin's two ravens, whose name means "memory" in Old Norse. Muninn flies across the nine worlds and returns what has been forgotten.*

Built by [MJ Bonanno](https://scrypster.com) · [muninndb.com](https://muninndb.com) · BSL 1.1

---

*MuninnDB is patent pending (U.S. Provisional Patent Application No. 63/991,402) and licensed under BSL 1.1.*
