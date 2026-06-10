# TLS

MuninnDB can serve its client-facing endpoints over HTTPS in two ways:

1. **Native TLS** — the daemon terminates TLS itself from a certificate and key you supply. This guide covers it.
2. **Reverse-proxy TLS** — a TLS-terminating proxy (nginx, Traefik, Caddy) sits in front and MuninnDB stays on plain HTTP behind it. See [Reverse-proxy TLS](#reverse-proxy-tls-alternative) below and the [Traefik integration](integrations/traefik-claude-chatgpt.md).

MuninnDB does **not** issue certificates — you bring your own (self-signed for dev/internal, Let's Encrypt or a corporate CA for production).

---

## What TLS protects

When native TLS is enabled, the **same** certificate is applied to every client-facing listener:

| Listener | Default port | Encrypted |
|----------|--------------|-----------|
| REST / admin API | `:8475` | ✅ |
| MCP (AI tools) | `:8750` | ✅ |
| Web UI | `:8476` | ✅ |
| MBP (binary protocol) | `:8474` | ✅ |
| gRPC | `:8477` | ✅ |
| Metrics | `--metrics-addr` (off by default) | ❌ — plain HTTP, no auth; bind it to loopback only |

The minimum protocol version is **TLS 1.2**.

> **Note:** The metrics endpoint (`--metrics-addr`, off by default) is the one exception — it is plain HTTP with no auth and is **not** covered by `--tls-cert`. If you enable it, bind it to loopback (e.g. `127.0.0.1:9090`); never expose it on a public interface.

> **Note:** `--tls-cert` configures **client-facing** TLS only. It does **not** configure inter-node **cluster** TLS — clustering uses its own auto-generated mutual-TLS certificates on the cluster `bind_addr` (which also defaults to `:8474`). See [Cluster Operations](cluster-operations.md) for inter-node transport security.

---

## Enabling native TLS

Configure a certificate and private key (PEM) through the `MUNINN_TLS_CERT` and `MUNINN_TLS_KEY` environment variables. They must be set **together** — providing only one aborts startup with:

```
tls: --tls-cert and --tls-key must both be set (or neither)
```

(The message names the `--tls-cert`/`--tls-key` flags even when you set the values through the environment variables below.)

| Variable | Default | Description |
|----------|---------|-------------|
| `MUNINN_TLS_CERT` | _(none)_ | Path to the PEM certificate (full chain, leaf first). |
| `MUNINN_TLS_KEY` | _(none)_ | Path to the PEM private key. |

```bash
export MUNINN_TLS_CERT=/etc/muninndb/cert.pem
export MUNINN_TLS_KEY=/etc/muninndb/key.pem
muninn start
```

For an interactive / CLI deployment you can put them in `~/.muninn/muninn.env` instead of exporting them (uncomment the TLS section):

```ini
MUNINN_TLS_CERT=/etc/muninndb/cert.pem
MUNINN_TLS_KEY=/etc/muninndb/key.pem
```

> **Note:** The server also accepts `--tls-cert`/`--tls-key` flags directly, but `muninn start` launches the server as a background daemon and does **not** forward them — configure TLS via the environment variables above (or, under systemd, `Environment=`; see [systemd](#systemd)). `muninn.env` is loaded from `~/.muninn/`, so it does **not** apply under systemd (which runs with no `$HOME`).

---

## Obtaining a certificate

MuninnDB loads a certificate; it does not create one.

**Dev / internal — self-signed.** Modern clients (curl, Go's TLS stack, the MCP clients) reject a certificate that has no Subject Alternative Name, so you **must** include `-addext "subjectAltName=…"`:

```bash
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes \
  -keyout key.pem -out cert.pem -days 365 \
  -subj "/CN=muninn.example" \
  -addext "subjectAltName=DNS:muninn.example,DNS:localhost,IP:127.0.0.1"
```

Replace `muninn.example` / the IP with the hostname(s) and address(es) clients will use to reach the server.

**Production.** Use a certificate from Let's Encrypt or your corporate CA. The certificate file must be PEM and, if there are intermediates, contain the **full chain with the leaf certificate first**.

---

## Verifying

`muninn status` and `muninn start` auto-detect TLS — an HTTP health probe that fails is retried over HTTPS — so they print `https://` URLs with no extra configuration once a TLS daemon is up:

```bash
muninn status
```

Inspect the certificate the server actually presents:

```bash
openssl s_client -connect 127.0.0.1:8475 </dev/null 2>/dev/null \
  | openssl x509 -noout -subject -issuer -dates -ext subjectAltName
```

A self-signed dev certificate is not in any trust store, so a quick liveness check skips verification with `-k`:

```bash
curl -k https://127.0.0.1:8475/api/health
```

A certificate issued by a CA your client already trusts needs no flags; to trust an **internal CA**, pass that **CA** certificate via `curl --cacert ca.pem …`.

---

## Connecting clients over TLS

AI-tool MCP configurations must point at the HTTPS endpoint and include the bearer token:

```
https://<host>:8750/mcp
```

For the `muninn mcp` stdio proxy, set the endpoint explicitly (see [self-hosting](self-hosting.md)):

```bash
MUNINN_MCP_URL=https://<host>:8750/mcp muninn mcp
```

To trust an **internal CA**, point clients at the **CA** certificate (`curl --cacert ca.pem`, or your SDK's CA-bundle option). A self-signed certificate is awkward to verify: OpenSSL-based clients such as `curl` reject it as a depth-zero self-signed certificate even when it is passed via `--cacert`. Use a CA-issued certificate for verified client connections, and reserve `-k`/skip-verify for local testing only. (Go-based clients, including the MCP clients, *can* trust a self-signed certificate by adding it to their root pool.)

When the server is bound to a non-loopback address, override the URLs that `muninn status` probes with `MUNINNDB_ADMIN_URL` / `MUNINNDB_UI_URL` / `MUNINNDB_MCP_URL` (documented in the [self-hosting environment variables](self-hosting.md#environment-variables-reference) table).

---

## systemd

The shipped [`contrib/muninndb.service`](../contrib/muninndb.service) runs MuninnDB without a `User=`, so `$HOME` is unset and `~/.muninn/muninn.env` is **not** loaded. Set TLS through the unit instead:

```ini
[Service]
Environment=MUNINN_TLS_CERT=/etc/muninndb/cert.pem
Environment=MUNINN_TLS_KEY=/etc/muninndb/key.pem
# or, to keep secrets out of the unit:
# EnvironmentFile=-/etc/muninndb/muninndb.env
```

> **Note:** `muninn.env` and a systemd `EnvironmentFile=` are **not** interchangeable formats — MuninnDB's loader strips an `export ` prefix and surrounding quotes and ignores non-`MUNINN*` keys, while systemd's `EnvironmentFile=` does not. Don't point both at the same file.

---

## Reverse-proxy TLS (alternative)

If you prefer to terminate TLS at a proxy, leave MuninnDB on plain HTTP (bound to loopback) and front it with nginx, Traefik, or Caddy:

```nginx
server {
    listen 443 ssl;
    server_name muninn.example;
    ssl_certificate     /etc/ssl/muninn/cert.pem;
    ssl_certificate_key /etc/ssl/muninn/key.pem;

    location / {
        proxy_pass http://127.0.0.1:8750;   # MCP only
        proxy_set_header Host $host;
    }
}
```

Each client-facing port you want to expose needs its own proxy route — the snippet above fronts MCP (`:8750`) only; add equivalent routes for the REST/admin API (`:8475`) and Web UI (`:8476`) as needed.

For a full cloud walkthrough (Claude.com / ChatGPT connectors via Traefik), see the [Traefik integration guide](integrations/traefik-claude-chatgpt.md).

---

## Troubleshooting

- **`tls: --tls-cert and --tls-key must both be set (or neither)`** — you provided one of the pair. Provide both, or neither.
- **`http: TLS handshake error … first record does not look like a TLS handshake`** in the server log — a plaintext client hit an HTTPS port. Switch the client to `https://`.
- **`certificate signed by unknown authority` / `x509: … relies on legacy Common Name`** — the client doesn't trust the cert, or the cert has no SAN. Trust the issuing **CA** (`curl --cacert ca.pem`), and regenerate with a `subjectAltName` if it's missing. Note OpenSSL-based clients reject a *self-signed* certificate (`curl` reports `self-signed certificate`) even via `--cacert` — use `-k` for local testing, or a CA-issued cert for real verification.

---

**See also:** [Self-Hosting](self-hosting.md) · [Auth](auth.md) · [Cluster Operations](cluster-operations.md) · [Traefik integration](integrations/traefik-claude-chatgpt.md)
