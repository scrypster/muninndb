# Self-Hosting MuninnDB

MuninnDB ships as a single binary. Pre-built release binaries from GitHub Releases include an embedded ONNX model for semantic search. When building from source, the model is optional and requires `make fetch-assets` at build time. No external dependencies are required for basic operation.

---

## Option 1: Binary (recommended for development)

### 1. Download

**macOS / Linux — latest release**
```sh
# macOS arm64 (Apple Silicon)
curl -sSL https://github.com/scrypster/muninndb/releases/latest/download/muninn_darwin_arm64.tar.gz | tar -xz
sudo mv muninn /usr/local/bin/

# macOS amd64 (Intel)
curl -sSL https://github.com/scrypster/muninndb/releases/latest/download/muninn_darwin_amd64.tar.gz | tar -xz
sudo mv muninn /usr/local/bin/

# Linux amd64
curl -sSL https://github.com/scrypster/muninndb/releases/latest/download/muninn_linux_amd64.tar.gz | tar -xz
sudo mv muninn /usr/local/bin/
```

### 2. Start

```sh
muninn init    # first-time: guided setup, generates auth token, configures AI tools
muninn start   # starts the server in the background
muninn status  # verify everything is running
```

### 3. Stop

```sh
muninn stop
```

---

## Option 2: Docker

### Quick start (bundled embedder, no API key required)

```sh
docker run -d \
  --name muninndb \
  -p 8474:8474 \
  -p 8475:8475 \
  -p 8476:8476 \
  -p 8477:8477 \
  -p 8750:8750 \
  -v muninndb-data:/data \
  ghcr.io/scrypster/muninndb:latest
```

Open the Web UI: http://localhost:8476

### Docker Compose

```sh
git clone https://github.com/scrypster/muninndb
cd muninndb
docker compose up -d
```

Edit `docker-compose.yml` to configure your embedder (see [Embedder Configuration](#embedder-configuration) below).

### Build from source

```sh
git clone https://github.com/scrypster/muninndb
cd muninndb
docker build -t muninndb:local .
docker run -d --name muninndb -p 8474-8477:8474-8477 -p 8750:8750 \
  -v muninndb-data:/data muninndb:local
```

---

## Ports

| Port | Protocol | Purpose |
|------|----------|---------|
| 8474 | TCP | MBP binary protocol (Go SDK, Python SDK) |
| 8475 | HTTP | REST API |
| 8476 | HTTP | Web UI dashboard |
| 8477 | gRPC | gRPC API |
| 8750 | HTTP | MCP — AI tool integration (Claude, Cursor, VS Code) |

---

## Embedder Configuration

MuninnDB uses embeddings for semantic search and activation. Configure with environment variables:

### Bundled (no API key, no internet) — default

The bundled `all-MiniLM-L6-v2` INT8 model (384-dim, ~80MB) is active automatically when the binary was built with embedded assets. No configuration needed.

To disable it and fall back to noop (or use a cloud provider instead):

```sh
MUNINN_LOCAL_EMBED=0
```

### Ollama (local GPU/CPU, no API cost)

```sh
MUNINN_OLLAMA_URL=ollama://localhost:11434/nomic-embed-text
```

Install [Ollama](https://ollama.com), then:
```sh
ollama pull nomic-embed-text
```

### OpenAI

```sh
MUNINN_OPENAI_KEY=sk-...
# Optional: OpenAI-compatible base URL (e.g. LocalAI)
MUNINN_OPENAI_URL=http://localhost:8080/v1
```

Uses `text-embedding-3-small` (1536-dim). ~$0.02 per million tokens.

### Voyage AI

```sh
MUNINN_VOYAGE_KEY=pa-...
```

Uses `voyage-3` (1024-dim). High-quality retrieval, competitive pricing.

---

## Optional: LLM Enrichment

Enrichment adds summaries, keywords, and contradiction detection on top of what the cognitive engine does natively. It is not required for core functionality.

```sh
# Ollama (free, local)
MUNINN_ENRICH_URL=ollama://localhost:11434/llama3.2

# Anthropic Claude (best quality)
MUNINN_ENRICH_URL=anthropic://claude-haiku-4-5-20251001
MUNINN_ANTHROPIC_KEY=sk-ant-...

# OpenAI
MUNINN_ENRICH_URL=openai://gpt-4o-mini
MUNINN_ENRICH_API_KEY=sk-...
```

---

## Connecting AI Tools

### Automatic setup (binary install)

```sh
muninn init
```

`muninn init` detects Claude Desktop, Claude Code, Cursor, OpenClaw, Windsurf, and VS Code and writes the MCP config automatically.

### Manual setup

Add to your AI tool's MCP config:

**Claude Desktop** — `~/Library/Application Support/Claude/claude_desktop_config.json`
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

If you enabled MCP auth (token file at `~/.muninn/mcp.token`):
```json
{
  "mcpServers": {
    "muninn": {
      "type": "http",
      "url": "http://localhost:8750/mcp",
      "headers": {
        "Authorization": "Bearer <your-token>"
      }
    }
  }
}
```

**Claude Code / CLI** — `~/.claude.json`
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

**Cursor** — `~/.cursor/mcp.json`
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

**OpenClaw** — `~/.openclaw/openclaw.json`

OpenClaw only supports stdio-transport MCP servers. MuninnDB ships a built-in
proxy (`muninn mcp`) that bridges OpenClaw's subprocess model to the running
daemon. `muninn init` configures this automatically; for manual setup:

```json
{
  "mcpServers": {
    "muninn": {
      "command": "muninn",
      "args": ["mcp"],
      "transport": "stdio"
    }
  }
}
```

The `muninn mcp` proxy reads the bearer token from `~/.muninn/mcp.token` on
every request, so it works transparently even after a daemon restart — no
credential embedded in the config file.

To override the daemon endpoint (non-default port, TLS):
```sh
MUNINN_MCP_URL=https://my-server:8750/mcp muninn mcp
```

**Windsurf** — `~/.codeium/windsurf/mcp_config.json`
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

**VS Code** — `.vscode/mcp.json` (workspace)
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

Restart your AI tool after editing the config.

### Verify the connection

```sh
curl http://localhost:8750/mcp/health
# → {"status":"ok"}
```

---

## Environment Variables Reference

| Variable | Default | Description |
|----------|---------|-------------|
| `MUNINNDB_DATA` | `~/.muninn/data` | Data directory (binary) or `/data` (Docker) |
| `MUNINN_LOCAL_EMBED` | on | Set to `"0"` to disable the bundled ONNX embedder |
| `MUNINN_OPENAI_KEY` | `""` | OpenAI API key for embeddings |
| `MUNINN_OPENAI_URL` | `""` | Optional OpenAI base URL or provider URL override (invalid values skip OpenAI init) |
| `MUNINN_OLLAMA_URL` | `""` | Ollama URL for embeddings, e.g. `ollama://localhost:11434/nomic-embed-text` |
| `MUNINN_VOYAGE_KEY` | `""` | Voyage AI key for embeddings |
| `MUNINN_ENRICH_URL` | `""` | LLM enrichment URL (optional) |
| `MUNINN_ANTHROPIC_KEY` | `""` | Anthropic API key for enrichment |
| `MUNINN_ENRICH_API_KEY` | `""` | Generic enrichment API key |
| `MUNINN_MEM_LIMIT_GB` | `4` | GOMEMLIMIT in GB |
| `MUNINN_GC_PERCENT` | `200` | GOGC tuning |
| `MUNINN_CORS_ORIGINS` | `""` | Comma-separated allowed CORS origins |
| `MUNINN_MCP_URL` | `http://127.0.0.1:8750/mcp` | Override MCP endpoint used by `muninn mcp` proxy (OpenClaw) |

---

## Data Directory Layout

```
~/.muninn/data/         (or /data in Docker)
├── pebble/             Pebble key-value store (engrams, indices, weights)
├── wal/                Write-ahead log segments
├── auth_secret         Session signing key (auto-generated)
└── muninn.pid          Server PID (binary installs only)
~/.muninn/
└── mcp.token           MCP bearer token (auto-generated by muninn init)
```

---

## Upgrading

**Binary:**
```sh
muninn stop
# download new binary
muninn start
```

**Docker:**
```sh
docker pull ghcr.io/scrypster/muninndb:latest
docker compose up -d --pull always
```

Data in the volume is preserved across upgrades.

---

## Encryption at Rest

### Overview

MuninnDB does not encrypt data files itself. Encryption at rest is handled at the OS or volume level — no application changes, configuration flags, or environment variables are needed. You encrypt the disk or volume that holds MuninnDB's data directory, and the database is unaware it is running on encrypted storage.

### What's on disk

All persistent state lives under the data directory (default: `~/.muninn/data/` for binary installs, `/data` for Docker):

| Path | Contents |
|------|----------|
| `pebble/` | All engrams, Hebbian weights, and indices — the full cognitive state of every vault |
| `wal/` | Write-ahead log segments; contains recent writes before they are flushed to Pebble |
| `auth_secret` | Session signing key (HMAC secret); compromise allows forging admin session cookies |

The MCP bearer token lives outside the data directory at `~/.muninn/mcp.token`. If you are encrypting a home directory rather than a dedicated volume, this file is covered automatically.

Encrypt whichever path contains these files. The `auth_secret` file is especially sensitive — it is the only file in the data directory that is a credential rather than a memory record.

---

### Linux — LUKS / dm-crypt

LUKS is the standard Linux encrypted block device layer. The data directory sits on a LUKS volume; MuninnDB reads and writes normally through the transparent device mapper.

**Create and mount an encrypted data volume:**

```sh
# Create a 20 GB image file (or use a real block device like /dev/sdb)
dd if=/dev/zero of=/srv/muninn.img bs=1M count=20480

# Initialize the LUKS container (you will be prompted for a passphrase)
sudo cryptsetup luksFormat /srv/muninn.img

# Open the container — it appears as /dev/mapper/muninndata
sudo cryptsetup open /srv/muninn.img muninndata

# Create a filesystem on the decrypted device
sudo mkfs.ext4 /dev/mapper/muninndata

# Mount it at the data directory
sudo mkdir -p /data
sudo mount /dev/mapper/muninndata /data
sudo chown $(whoami):$(whoami) /data
```

**On subsequent boots, unlock and mount before starting MuninnDB:**

```sh
sudo cryptsetup open /srv/muninn.img muninndata
sudo mount /dev/mapper/muninndata /data
muninn start
```

**To lock (unmount and close) after stopping MuninnDB:**

```sh
muninn stop
sudo umount /data
sudo cryptsetup close muninndata
```

**Automate unlock at boot** by adding an entry to `/etc/crypttab` and `/etc/fstab`. Refer to your distribution's documentation for keyfile-based unlock if you need unattended boot.

**Check encryption status:**

```sh
sudo cryptsetup status muninndata
```

---

### macOS — FileVault

On macOS, FileVault encrypts the entire startup disk with AES-XTS 128-bit. If MuninnDB's data directory is on the boot volume (the default for binary installs at `~/.muninn/`), FileVault covers it automatically.

**Check FileVault status:**

```sh
fdesetup status
# FileVault is On.
```

**Enable FileVault if it is off:**

```sh
sudo fdesetup enable
```

You will be prompted to save a recovery key. After enabling, macOS encrypts the volume in the background — the system remains usable during encryption.

**If you store MuninnDB data on a separate external or secondary volume**, use Disk Utility or `diskutil` to format it as APFS with encryption:

```sh
# List available disks to identify the target
diskutil list

# Erase and format as encrypted APFS (replace disk2 with your actual disk identifier)
diskutil eraseDisk APFS "MuninnData" -withCrypto disk2
```

You will be prompted to set a volume password. The volume mounts automatically on unlock.

---

### Windows — BitLocker

BitLocker encrypts entire drives with AES 128-bit or 256-bit. If MuninnDB runs on the system drive, BitLocker on `C:` covers its data. For a dedicated data drive, enable BitLocker on that drive.

**Enable BitLocker on a drive via PowerShell (run as Administrator):**

```powershell
# Enable BitLocker on D: with TPM protection and save the recovery key to C:\
Enable-BitLocker -MountPoint "D:" -EncryptionMethod Aes256 `
  -TpmProtector -RecoveryKeyPath "C:\BitLockerRecovery" `
  -RecoveryKeyProtector
```

**Check encryption status:**

```powershell
Get-BitLockerVolume -MountPoint "D:" | Select-Object MountPoint, VolumeStatus, EncryptionPercentage
```

**Wait for full encryption before starting MuninnDB in production.** `EncryptionPercentage` must reach 100.

---

### Docker — Encrypted volumes

Docker named volumes (e.g., `muninndb-data`) are stored on the host filesystem under Docker's storage root. Host-level encryption covers them automatically — no Docker-specific configuration is needed.

**Linux (Docker on Linux):** Docker volumes live under `/var/lib/docker/volumes/`. Put `/var/lib/docker` on a LUKS-encrypted volume (see Linux section above), or encrypt the entire host disk. The container sees normal storage; encryption is transparent.

**macOS (Docker Desktop):** Docker Desktop runs containers inside a Linux VM. All volume data is stored inside the VM's disk image (`~/Library/Containers/com.docker.docker/Data/vms/0/data/Docker.raw` or similar). Enabling FileVault on the Mac encrypts this image. No additional steps are needed.

**Windows (Docker Desktop):** Docker Desktop stores volume data inside a WSL 2 virtual disk (typically `%LOCALAPPDATA%\Docker\wsl\`). Enable BitLocker on the drive that contains this path (usually `C:`). The WSL disk is encrypted along with everything else on that drive.

**Verify your Docker volume location (Linux):**

```sh
docker volume inspect muninndb-data
# "Mountpoint": "/var/lib/docker/volumes/muninndb-data/_data"
```

Ensure that path is on an encrypted filesystem before storing sensitive data.

---

### What encryption at rest does NOT cover

Encryption at rest protects data that is **stored on disk while the system is off or the volume is locked**. It does not protect:

- **The running process.** Once MuninnDB starts and the volume is unlocked, data is decrypted in memory. Any process with sufficient OS privileges can read process memory.
- **Network exposure.** Data in transit between clients and MuninnDB is not covered by disk encryption. Run MuninnDB behind a TLS-terminating reverse proxy in any networked deployment. See the [auth documentation](auth.md) for the transport security property.
- **API key compromise.** A stolen API key grants access to vault data over the network regardless of disk encryption state. Rotate keys immediately if compromise is suspected.
- **Backup files.** If you copy the `pebble/` directory or snapshot the volume without encryption, those copies are unprotected. Apply the same encryption controls to your backup destination.

---

## Health Check

All services expose the same health endpoint:
```sh
curl http://localhost:8750/mcp/health
```

Returns `{"status":"ok"}` when the server is ready to accept requests.

---

**See also:** [Auth](auth.md) · [Cluster Operations](cluster-operations.md) · [Quickstart](quickstart.md)
