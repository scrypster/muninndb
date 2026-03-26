# Claude.com / ChatGPT + MuninnDB via Traefik

This guide is for users running MuninnDB on a **publicly accessible cloud server** who want to connect it to Claude.com Connectors or ChatGPT's custom MCP support.

> **This is not a local setup guide.** Claude.com and ChatGPT make MCP calls server-to-server — their backend infrastructure calls your endpoint directly. MuninnDB must be reachable from the internet for this to work. If you're running MuninnDB locally, use [Claude Code](../../quickstart.md) or another local MCP client instead.

---

## How it works

Claude.com and ChatGPT only allow you to configure an MCP URL — they cannot send custom headers like `Authorization: Bearer`. MuninnDB's token auth requires that header. The workaround is to use **Traefik v3 as a reverse proxy** to:

1. Accept requests at a secret URL (`https://muninn.example.com/mcp?vault_key=<secret>`)
2. Only route requests that include the correct `vault_key` query parameter
3. Forward matched requests to MuninnDB

The secret lives in Traefik's routing rule, not in MuninnDB itself. MuninnDB runs with no token configured (open access on the internal Docker network only — not exposed directly to the internet).

---

## Security considerations

> **Read this before proceeding.**

**URL query parameters are a security anti-pattern for secrets.** The `vault_key` value will appear in:
- Traefik access logs (disable per-router — see below)
- Browser history if you ever open the URL directly
- Any HTTP proxy or CDN logs between the client and your server
- Error messages and monitoring tools that log full request URLs

**Mitigations:**
- Use a long random secret (`openssl rand -hex 24` generates 48 characters)
- Disable Traefik access logs for the MuninnDB router (see below)
- Use HTTPS — the query param is encrypted in transit over TLS
- Rotate the key if you suspect it has been exposed
- Do not share the full URL in chat, email, or any logged channel

This is a practical workaround, not a hardened auth solution. It is appropriate for personal use on a self-hosted server where you control the infrastructure.

---

## Prerequisites

- A cloud VM with a publicly accessible domain name (e.g. `muninn.example.com`)
- Docker and Docker Compose installed
- **Traefik v3** running as your reverse proxy — the `Query()` matcher syntax used here requires v3; see below for the v2 equivalent
- A Docker network shared between Traefik and MuninnDB (commonly named `traefik` or `proxy`)
- A DNS A record pointing your domain to the VM's IP
- Let's Encrypt or another TLS certificate provider configured in Traefik

---

## Setup

### 1. Generate a secret key

```bash
openssl rand -hex 24
```

Copy the output — this is your `VAULT_KEY`.

### 2. Create `.env`

```bash
# .env
# Keep this file out of version control
VAULT_KEY=your-generated-secret-here
```

> **Note:** `${VAULT_KEY}` is resolved by Docker Compose from `.env` at `docker compose up` time. If you change `.env`, recreate the container (`docker compose up -d --force-recreate`) for the new value to take effect.

### 3. Create `docker-compose.yaml`

```yaml
name: muninn-example-com

networks:
  traefik:
    external: true  # must match the network your Traefik instance is on

services:
  muninn:
    image: ghcr.io/scrypster/muninndb:latest
    container_name: muninn.example.com
    hostname: muninn
    restart: always
    networks:
      - traefik

    volumes:
      - ./data:/data
      - ./backup:/backup

    environment:
      MUNINN_LISTEN_HOST: "0.0.0.0"
      MUNINN_MEM_LIMIT_GB: "4"
      MUNINN_GC_PERCENT: "200"
      MUNINN_CORS_ORIGINS: "https://muninn.example.com"
      # No MUNINN_MCP_TOKEN set — auth is handled by Traefik routing rule

    healthcheck:
      test: ["CMD", "curl", "-sf", "http://localhost:8750/mcp/health"]
      interval: 15s
      timeout: 5s
      retries: 5
      start_period: 10s

    labels:
      - traefik.enable=true
      # Only route requests that include the correct vault_key query parameter (Traefik v3 syntax)
      - traefik.http.routers.muninn.rule=Host(`muninn.example.com`) && PathPrefix(`/mcp`) && Query(`vault_key`,`${VAULT_KEY}`)
      - traefik.http.routers.muninn.tls=true
      - traefik.http.routers.muninn.tls.certresolver=lets-encrypt
      - traefik.http.routers.muninn.service=muninn
      - traefik.http.services.muninn.loadbalancer.server.port=8750
      # Disable access logs for this router to keep vault_key out of log files
      - traefik.http.routers.muninn.observability.accesslogs=false
```

**Traefik v2 users:** Replace the `Query()` matcher with the v2 single-argument syntax:
```
traefik.http.routers.muninn.rule=Host(`muninn.example.com`) && PathPrefix(`/mcp`) && Query(`vault_key=${VAULT_KEY}`)
```

> **Watchtower:** The original community setup includes `com.centurylinklabs.watchtower.enable=true` for automatic image updates. Add it if you run Watchtower.

### 4. Start

```bash
docker compose up -d
```

Verify it's running:

```bash
curl -sf "https://muninn.example.com/mcp/health?vault_key=your-secret" | jq .
```

A request without the key will return a **404** — Traefik has no matching route for it.

---

## Connect Claude.com

1. Go to **Claude.com → Settings → Connectors → Add custom connector**
2. Set the MCP URL to:
   ```
   https://muninn.example.com/mcp?vault_key=your-secret
   ```
3. Save and start a new conversation

Verify by asking Claude to call `muninn_guide`.

---

## Connect ChatGPT

ChatGPT's MCP connector support requires **Developer Mode**, available on Pro, Plus, Business, Enterprise, and Education plans.

1. Go to **ChatGPT → Settings → Apps → Advanced → Enable Developer Mode**
2. Create a new connector and set the MCP URL to:
   ```
   https://muninn.example.com/mcp?vault_key=your-secret
   ```
3. Save and start a new conversation

---

## System prompt

For Claude.com or ChatGPT to use memory proactively, add this to your Project instructions:

```markdown
# Memory: MuninnDB (Canonical)

MuninnDB (muninn MCP) is the canonical memory system. Never use local auto memory.

## Session Start — Always
Call muninn_recall with relevant context before beginning any work.
This loads prior context. Vault: default.

## During Every Session
- Save to Muninn continuously — this is a mindset, not a checklist.
- Anything the user shares or that emerges from the work should be saved immediately.
- Do not evaluate whether it is "important enough".
- Do not wait to be asked. When in doubt, save it.

## Tools
- Recall: muninn_recall (vault, context)
- Store: muninn_remember (vault, concept, content)
- Batch: muninn_remember_batch (vault, memories[])
- Guide: muninn_guide — call on first connect to learn best practices
```

See [Agent Prompting](../agent-prompting.md) for more detail on this pattern.

---

## Troubleshooting

| Symptom | Fix |
|---------|-----|
| Request returns 404 | Traefik route not matched — verify `vault_key` param matches `.env` exactly, and that the container was recreated after any `.env` change |
| `connection refused` | MuninnDB container not healthy — check `docker compose logs muninn` |
| Traefik can't reach MuninnDB | Ensure both services are on the same Docker network (`traefik.enable=true` is not enough) |
| TLS cert error | Let's Encrypt cert not yet issued — wait a minute and retry |
| Tools not appearing in Claude/ChatGPT | Restart/reconnect the integration after saving the URL |
| Agent not storing proactively | Add the system prompt above to your Project instructions |
| ChatGPT connector option missing | Developer Mode must be enabled first (Settings → Apps → Advanced) |

---

## Credit

This setup was contributed by [@rsubr](https://github.com/rsubr) who solved the Claude.com/ChatGPT connector problem and shared the full working configuration. Thanks for publishing it.
