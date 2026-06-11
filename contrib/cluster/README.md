# Local MuninnDB cluster (1 Cortex + 2 Lobes)

A runnable example that brings up a three-node MuninnDB cluster on your machine
so you can see Cortex → Lobe replication working end to end. This is the same
topology used to validate the replication fix in
[#448](https://github.com/scrypster/muninndb/issues/448).

For production operations (join tokens, TLS, failover, scaling, DR) see the full
[Cluster Operations Runbook](../../docs/cluster-operations.md). This directory is
the quickstart.

## Roles

| Node   | Role      | What it does                                                    |
|--------|-----------|-----------------------------------------------------------------|
| cortex | `primary` | Accepts writes, owns the WAL, streams replication to the Lobes. |
| lobe1  | `replica` | Read-only copy; receives the WAL stream and applies it.         |
| lobe2  | `replica` | Same as lobe1.                                                  |

## Run it

```bash
cd contrib/cluster
docker compose up -d --build      # first run builds the image

# Watch the Lobes join (no "broken pipe" — that was the #448 bug):
docker compose logs -f cortex lobe1 lobe2 | grep -Ei "joined|cluster"
```

Within a few seconds each Lobe logs `cluster: joined role=lobe cortex=cortex epoch=1`.

## Prove replication works

Write to the **Cortex**, then read the same id back from a **Lobe**:

```bash
# 1) Write to the cortex (REST on :8475). Default vault is public.
ID=$(curl -s -X POST http://localhost:8475/api/engrams \
  -H 'Content-Type: application/json' \
  -d '{"concept":"hello","content":"replicated from cortex"}' \
  | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')
echo "wrote $ID"

# 2) Read it from lobe1 (:18475) and lobe2 (:28475) — it replicated.
sleep 2
curl -s "http://localhost:18475/api/engrams/${ID}?vault=default"
curl -s "http://localhost:28475/api/engrams/${ID}?vault=default"
```

Both Lobes return the engram. Writes go only to the Cortex; a Lobe is read-only.

## Tear down

```bash
docker compose down -v
```

## Notes & gotchas

- **Cluster port ≠ MBP port (in containers).** Each node binds the SDK/MBP
  protocol on `0.0.0.0:8474`. Because `0.0.0.0` covers every interface, the
  cluster listener (`MUNINN_CLUSTER_BIND_ADDR`) must use a *different* port — this
  example uses `8479`. Reusing `8474` for the cluster bind address causes
  `address already in use`, and joins then fail with `unexpected frame type
  0xff` (they hit the MBP server instead of the cluster listener).
- **Seeds point at the cluster port.** Lobes seed `cortex:8479`; the Cortex
  seeds the Lobes for gossip.
- **Secrets.** `MUNINN_CLUSTER_SECRET` and `MUNINN_ADMIN_PASSWORD` here are
  placeholders — set strong values for anything real.
- **Cluster status reporting.** `GET /v1/cluster/info` currently under-reports
  topology (Cortex role and per-node lag); replication is unaffected. Tracked in
  [#516](https://github.com/scrypster/muninndb/issues/516).
