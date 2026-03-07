# Quickstart

Everything you need to go from zero to a running MuninnDB with memories stored, retrieved, and connected to your AI tools.

---

## 1. Install

**macOS / Linux (recommended):**

```bash
curl -sSL https://muninndb.com/install.sh | sh
```

This downloads the right binary for your platform, moves it to `/usr/local/bin/muninn`, and prints the version.

**Homebrew:**

```bash
brew tap scrypster/tap
brew install muninndb
```

**Docker:**

```bash
docker run -d \
  --name muninndb \
  -p 8474:8474 -p 8475:8475 -p 8476:8476 -p 8477:8477 -p 8750:8750 \
  -v muninndb-data:/data \
  ghcr.io/scrypster/muninndb:latest
```

**Windows (PowerShell):**

```powershell
irm https://muninndb.com/install.ps1 | iex
```

This downloads the latest release, extracts to `%LOCALAPPDATA%\muninn`, and adds it to your PATH automatically.

> **Note (Windows):** The bundled local embedder requires the [Visual C++ Redistributable](https://aka.ms/vs/17/release/vc_redist.x64.exe). Most Windows machines already have it. If `muninn start` shows an ORT/DLL error, install the redistributable and restart.

**Build from source:**

```bash
git clone https://github.com/scrypster/muninndb.git
cd muninndb
go build -o muninn ./cmd/muninn/...
```

---

## 2. Start

```bash
muninn start
```

First run automatically:
- Creates the data directory at `~/.muninn/data`
- Creates the `root` admin account (password: `password` — change this)
- Sets the default vault to public (no API key required)
- Prints the first-run banner

**Verify it's running:**

```bash
muninn status
# or
curl http://127.0.0.1:8750/mcp/health
```

**Stop it:**

```bash
muninn stop
```

**Ports:**

| Port | Protocol | Use |
|------|----------|-----|
| 8474 | MBP | Native binary protocol, lowest latency |
| 8475 | REST | JSON API, curl-friendly |
| 8476 | Web UI | Admin dashboard |
| 8477 | gRPC | Protobuf over HTTP/2 |
| 8750 | MCP | AI agent integration |

---

## 3. Store Your First Memory

```bash
curl -sX POST http://127.0.0.1:8475/api/engrams \
  -H 'Content-Type: application/json' \
  -d '{
    "concept": "payment incident",
    "content": "We switched to idempotency keys after the double-charge incident in Q3. Root cause: retry logic was not respecting 202 Accepted responses.",
    "tags": ["payments", "incident", "reliability"],
    "confidence": 0.95
  }' | jq .
```

You'll get back an engram ID. That memory now exists in MuninnDB with a relevance score, a Hebbian weight table, and a Bayesian confidence value — all initialised automatically.

---

## 4. Activate — Retrieve What's Relevant Now

```bash
curl -sX POST http://127.0.0.1:8475/api/activate \
  -H 'Content-Type: application/json' \
  -d '{
    "context": ["debugging the payment retry logic"],
    "max_results": 5
  }' | jq .
```

The Q3 incident surfaces — even though you queried about "retry logic," not "double-charge." MuninnDB ran a 6-phase pipeline (full-text + vector search, result fusion, Hebbian boost, association traversal, confidence scoring) and understood these are the same conversation.

---

## 5. Connect Your AI Tools

The interactive wizard detects Claude Desktop, Claude Code, Cursor, Codex, OpenClaw, Windsurf, VS Code, and others:

```bash
muninn init
```

It will ask which tools to configure, which embedder to use (local is recommended and works offline), a behavior mode (how proactively the AI should use memory), and whether to use a token for MCP access (default vault is open — no token needed).

Once connected, the AI can call `muninn_guide` to get vault-aware instructions on how and when to use memory — no manual prompt engineering required.

**Non-interactive (CI / scripts):**

```bash
muninn init --tool claude --yes
muninn init --tool cursor,claude --yes
```

**Manual MCP config:** Point any MCP client to `http://127.0.0.1:8750/mcp`.

---

## 6. Choose an Embedder

The bundled local embedder (all-MiniLM-L6-v2, 384-dim) is included and works offline with no API key. It's the default. For higher quality or different dimensions:

| Embedder | Config | Notes |
|----------|--------|-------|
| Local (bundled) | On by default — no config needed | Offline. ~80MB. Opt out with `MUNINN_LOCAL_EMBED=0`. |
| Ollama | `MUNINN_OLLAMA_URL=ollama://localhost:11434/nomic-embed-text` | Self-hosted. |
| OpenAI | `MUNINN_OPENAI_KEY=sk-...` | `text-embedding-3-small`, 1536d. Optional base URL override: `MUNINN_OPENAI_URL=http://localhost:8080/v1` (invalid override disables OpenAI init). |
| Voyage | `MUNINN_VOYAGE_KEY=pa-...` | voyage-3, 1024d. |
| Cohere | `MUNINN_COHERE_KEY=...` | embed-v4, 1024d. |
| Google | `MUNINN_GOOGLE_KEY=...` | text-embedding-004, 768d. |
| Jina | `MUNINN_JINA_KEY=...` | jina-embeddings-v3, 1024d. |
| Mistral | `MUNINN_MISTRAL_KEY=...` | mistral-embed, 1024d. |

Set these as environment variables before `muninn start`, or configure them with `muninn init`.

---

## 7. Optional: LLM Enrichment

Add an LLM to automatically extract entities, generate summaries, and detect typed relationships in every memory — applied retroactively to everything already stored:

```bash
export MUNINN_ENRICH_URL=anthropic://claude-haiku-4-5-20251001
export MUNINN_ANTHROPIC_KEY=sk-ant-...
muninn start
```

The enrich worker runs in the background. You don't restart or migrate anything.

---

## 8. Python SDK

```bash
pip install muninn-python
```

```python
import asyncio
from muninn import MuninnClient

async def main():
    async with MuninnClient("http://127.0.0.1:8475") as m:
        # Write
        eid = await m.write(
            vault="default",
            concept="auth architecture",
            content="Short-lived JWTs (15min), refresh tokens in HttpOnly cookies, sessions server-side in Redis",
            tags=["auth", "security"],
            confidence=0.9,
        )
        print(f"Stored: {eid}")

        # Activate
        result = await m.activate(
            vault="default",
            context=["reviewing the login flow for mobile"],
            max_results=5,
        )
        for item in result.activations:
            print(f"  {item.concept}  score={item.score:.3f}  confidence={item.confidence:.2f}")

asyncio.run(main())
```

[More examples →](../sdk/python/examples/)

---

## 9. Change the Admin Password

Log into the Web UI at `http://127.0.0.1:8476` with `root` / `password` and change the password from the settings page. Or via the shell:

```bash
muninn shell
```

---

## 10. What's Next

| | |
|---|---|
| [How Memory Works](how-memory-works.md) | Why temporal priority + Hebbian + confidence + PAS produces genuine memory |
| [Architecture](architecture.md) | The ERF format, 6-phase engine, four wire protocols |
| [Auth & Vaults](auth.md) | Multiple vaults, API keys, full vs. observe mode |
| [Semantic Triggers](semantic-triggers.md) | Subscribe to contexts; DB pushes when relevance changes |
| [Plugins](plugins.md) | Embed and enrich — upgrade the database without touching your code |
| [Cognitive Primitives](cognitive-primitives.md) | The math: ACT-R temporal scoring, Hebbian weights, Bayesian confidence, PAS |
| [Feature Reference](feature-reference.md) | Complete list of every feature, operation, and config option |
