# MuninnDB Cluster Operations Runbook

Operational guide for running and maintaining MuninnDB clusters. For architecture details, see [architecture.md](architecture.md) and [internal/replication/INTEGRATION.md](../internal/replication/INTEGRATION.md).

---

## 1. Cluster Architecture Overview

### Cortex/Lobe Model

MuninnDB uses a **Cortex/Lobe** model (internally: leader/replica terminology):

- **Cortex (Primary)**: Single writer. Accepts writes, runs cognitive workers (temporal, Hebbian, contradiction, confidence), streams WAL to Lobes, handles join requests.
- **Lobe (Replica)**: Read-only copy. Receives WAL stream from Cortex, applies entries to local Pebble, forwards cognitive side effects to Cortex. Can be promoted to Cortex during failover. **A write sent to a Lobe is refused, not forwarded** — see [Write routing](#write-routing-cortex-only) below.

### Node Roles

| Role | Data | Voting | Purpose |
|------|------|--------|---------|
| **Primary** | Yes | Yes | Leader (Cortex). Single writer. |
| **Replica** | Yes | Yes | Follower (Lobe). Receives replication. |
| **Sentinel** | No | Yes | Quorum voter only. Improves fault detection. No data storage. |
| **Observer** | Yes | No | Receives replication but does not vote. Read scaling without affecting quorum. |

### WAL Streaming Replication

- The Cortex writes to Pebble and appends to the Muninn Operation Log (MOL).
- Each Lobe connects over MBP and receives a **NetworkStreamer** pushing replication entries.
- Lobes apply entries idempotently and send `ReplAck` with last applied seq.
- The Cortex runs **SafePrune** every 60s to garbage-collect WAL segments once all Lobes have confirmed receipt.

### Write routing (Cortex only)

In cluster mode, **client writes are accepted only on the Cortex.** A write that
reaches a Lobe, Sentinel or Observer — via a load balancer without leader
affinity, a stale DNS record, or a VIP that failed over — is **refused with the
Cortex named**, not silently committed locally and not transparently forwarded.

| Surface | Refusal |
|---|---|
| REST | `421 Misdirected Request`, error code `4015`, headers `X-Muninn-Cortex-Id` / `X-Muninn-Cortex-Addr` |
| MCP | JSON-RPC error `-32002`; the message names the Cortex |
| gRPC | `FAILED_PRECONDITION` |
| MBP | error frame, code `4015` |

Clients should treat this as "retry against the named node", not as a transient
failure — retrying the same node will keep failing until it is promoted.

**Reads are unaffected** on every surface: serving reads is what a Lobe is for.
So is the cluster-administration API (`/api/admin/cluster/*`,
`/v1/replication/promote`) — those must work *on* a Lobe so an operator can
recover from a lost Cortex.

This covers configuration too: vault config and per-vault plasticity, `mk_` API
keys, `cap_` capability tokens and the admin password are refused on a non-Cortex
node and replicated from the Cortex like any engram write.

A **standalone (non-cluster) server** installs no such gate and is unaffected.

*Why refuse rather than forward:* there is no node-to-node write RPC to forward
over (the cluster wire is one-directional log shipping plus join/heartbeat/vote),
forwarding would have correctly-following nodes pump traffic into the wrong side
of a split brain, and refusing adds no timeout. See invariant SEC-13.

### Epoch-Based Fencing

- Every leadership change increments the **epoch** (stored in EpochStore).
- The epoch is **monotonic**: `EpochStore.Advance` refuses any value at or below
  the current one and *reports* that it refused. The one path that may lower it
  is `AdoptForSnapshot`, reachable only after a full snapshot has replaced the
  node's entire local state — i.e. after the Cortex was rebuilt or restored.
- A node that joins a Cortex whose epoch is **behind** its own and that is offered
  **no** resnapshot **refuses the join** and logs the remedy: stop the node, delete
  its data directory, and restart it so it takes a fresh snapshot. Accepting would
  leave it reporting `lag: 0` while applying nothing.
- **Known gap:** the epoch is *published* as a fencing token
  (`GET /v1/cluster/status` → `fencing_token`) and `ValidateFencingToken` exists,
  but **nothing calls it on the write path today**. Split-brain writes are bounded
  by the pre-emptive quorum-loss demotion below and by the Cortex-only write gate,
  not by token validation. Do not rely on fencing to reject a demoted Cortex's
  in-flight write.

---

## 2. Initial Cluster Setup

> **Just want to try it locally?** The [`contrib/cluster/`](../contrib/cluster/)
> directory has a runnable `docker compose` example (1 Cortex + 2 Lobes) with a
> step-by-step replication check. Start there, then come back here for production
> operations.

### Prerequisites

- **Network**: All nodes must reach each other on the cluster bind port (typically 8474, MBP protocol).
- **Ports**:
  - Cluster inter-node: `bind_addr` (default `:8474`, same as MBP). **In containers**, where MBP binds `0.0.0.0:8474`, give the cluster its own port (e.g. `:8479`) — a `0.0.0.0` MBP bind already owns 8474 on every interface, so a same-port cluster listener fails with `address already in use` and joins then fail with `unexpected frame type 0xff`.
  - REST API: 8475
  - gRPC: 8477
  - MCP: 8750
  - Web UI: 8476

### Starting the Primary Node

1. Create `{dataDir}/cluster.yaml` on the primary:

```yaml
enabled: true
node_id: "primary-1"
role: primary
bind_addr: "0.0.0.0:8474"
cluster_secret: "your-secure-secret"
```

2. Start MuninnDB:

```sh
muninn start
```

3. The primary bootstraps at epoch 0, starts an election, and becomes Cortex.

**Or** write the cluster configuration through REST, then restart the node:

```sh
curl -X POST http://127.0.0.1:8475/api/admin/cluster/enable \
  -H "Content-Type: application/json" \
  -H "Cookie: <admin-session-cookie>" \
  -d '{
    "role": "primary",
    "bind_addr": "0.0.0.0:8474",
    "cluster_secret": "your-secure-secret"
  }'
# 202 Accepted
# {"enabled":false,"configured":true,"restart_required":true,"role":"primary",
#  "message":"cluster configuration saved. Restart muninn on this node ..."}
```

> **Enabling clustering requires a restart.** The endpoint persists
> `cluster.yaml` and answers `202 Accepted` with `restart_required: true`; it
> does not start a coordinator. The storage layer's replication hook is wired
> when the store is built at boot and only when clustering was already enabled
> at that moment, so a coordinator started mid-process would report itself
> clustered, accept replicas, hand them a snapshot — and replicate none of the
> node's subsequent writes. Restart the node to activate clustering.

### Adding Replica Nodes

1. On the Cortex, obtain a join token (required when `cluster_secret` is set):

```sh
curl http://127.0.0.1:8475/api/admin/cluster/token \
  -H "Cookie: <admin-session-cookie>"
# {"token":"...", "expires_at":"...", "ttl_seconds":900}
```

2. Register the pending peer on the Cortex:

```sh
curl -X POST http://127.0.0.1:8475/api/admin/cluster/nodes \
  -H "Content-Type: application/json" \
  -H "Cookie: <admin-session-cookie>" \
  -d '{"addr": "10.0.1.6:8474", "token": "<token-from-step-1>"}'
```

3. On the new node, create `{dataDir}/cluster.yaml`:

```yaml
enabled: true
node_id: "replica-1"
role: replica
bind_addr: "0.0.0.0:8474"
seeds:
  - "10.0.1.5:8474"
cluster_secret: "your-secure-secret"
```

4. Start MuninnDB on the new node:

```sh
muninn start
```

5. The node connects to the seed (Cortex), performs the join handshake, receives a snapshot if needed, and then streams WAL.

### Adding Sentinel Nodes

Sentinels participate in quorum voting but do not store data. Add them for improved failure detection (ODOWN) without increasing storage.

1. Create `{dataDir}/cluster.yaml` on the sentinel:

```yaml
enabled: true
node_id: "sentinel-1"
role: sentinel
bind_addr: "0.0.0.0:8474"
seeds:
  - "10.0.1.5:8474"
cluster_secret: "your-secure-secret"
```

2. Register the peer on the Cortex (same as adding a replica) and start the node.

### Verifying Cluster Health

```sh
# CLI
muninn cluster info
muninn cluster status

# REST (cluster auth: Bearer token = cluster_secret)
curl -H "Authorization: Bearer <cluster_secret>" http://127.0.0.1:8475/v1/cluster/health
curl -H "Authorization: Bearer <cluster_secret>" http://127.0.0.1:8475/v1/cluster/info
```

Expected: `status: "ok"`, `is_leader: true` on Cortex, `replication_lag: 0` on Cortex, non-zero lag on Lobes until caught up.

---

## 3. Day-to-Day Operations

### Monitoring Cluster Health

**GET /v1/cluster/health** — Health check suitable for load balancers:

```sh
curl -H "Authorization: Bearer $MUNINN_CLUSTER_SECRET" \
  http://127.0.0.1:8475/v1/cluster/health
```

Response: `status` (`ok` | `degraded` | `down`), `role`, `is_leader`, `epoch`, `replication_lag`.

**GET /v1/cluster/info** — Full topology:

```sh
curl -H "Authorization: Bearer $MUNINN_CLUSTER_SECRET" \
  http://127.0.0.1:8475/v1/cluster/info
```

### Checking Replica Lag

```sh
# On a Lobe — returns this node's lag
curl -H "Authorization: Bearer $MUNINN_CLUSTER_SECRET" \
  http://127.0.0.1:8475/v1/replication/lag
# {"lag": 42, "role": "replica"}

# Full status
muninn cluster status
```

### Viewing Cluster Events

**GET /api/admin/cluster/events** — Server-Sent Events stream of replication log entries:

```sh
curl -N -H "Cookie: <admin-session>" \
  http://127.0.0.1:8475/api/admin/cluster/events
```

Use for debugging replication flow. Requires admin session authentication.

---

## 4. Scaling

### Adding a New Replica

1. Obtain join token: `GET /api/admin/cluster/token`
2. Register peer: `POST /api/admin/cluster/nodes` with `{"addr": "<host:8474>", "token": "..."}`
3. Create `cluster.yaml` on the new node with `role: replica`, `seeds: [<cortex_addr>]`, same `cluster_secret`
4. Start MuninnDB. The node joins and streams from seq 0 (or receives a snapshot if far behind).

### Removing a Node

**DELETE /api/admin/cluster/nodes/{id}** — Removes the node from the cluster. Stops the streamer, removes from MSP, closes connection.

```sh
# Optional: drain=true waits up to 30s for replica to catch up before removal
curl -X DELETE "http://127.0.0.1:8475/api/admin/cluster/nodes/replica-1?drain=true" \
  -H "Cookie: <admin-session>"
```

**CLI instructions** (when REST remove is not available): Stop the node (`muninn stop`), then after MSP timeout (~30s) the Cortex marks it down. Use DELETE API when implemented.

### Adding a Sentinel for Improved Fault Detection

Add sentinel nodes as in §2. They increase quorum size and improve ODOWN detection when the Cortex fails.

---

## 5. Failover Operations

### Automatic Failover (MSP + Election)

1. **MSP (Muninn Sentinel Protocol)** heartbeats detect peers. After `missedThreshold` (default 3) missed heartbeats, a peer is **SDOWN** (subjectively down).
2. When **quorum** of non-observer nodes agree a peer is SDOWN, that peer is **ODOWN** (objectively down).
3. **OnODown** for the Cortex → triggers `StartElection`.
4. Lobes and Sentinels vote. A Lobe with the highest epoch and caught-up WAL can win.
5. New leader broadcasts `CortexClaim`, others demote to Lobe.

**Quorum loss**: If the Cortex cannot reach quorum for 5 seconds, it **pre-emptively demotes** itself to avoid split-brain writes.

### Manual / Graceful Failover

**POST /api/admin/cluster/failover** — Planned handoff to a specific Lobe:

```sh
curl -X POST http://127.0.0.1:8475/api/admin/cluster/failover \
  -H "Content-Type: application/json" \
  -H "Cookie: <admin-session>" \
  -d '{"target_node_id": "replica-1"}'
```

**Process**:
1. Cortex enters **DRAINING** — rejects new writes
2. Flush cognitive workers (Hebbian, etc.)
3. Wait for replication convergence (all Lobes acked current seq)
4. Send **HANDOFF** to target with epoch and cortex_seq
5. Target: bump epoch, persist role, broadcast CortexClaim, start cognitive workers, send **HANDOFF_ACK**
6. Old Cortex demotes, becomes Lobe

Timeout: 5s for HANDOFF_ACK, 30s for convergence.

### Emergency Promotion (Election Trigger)

**POST /v1/replication/promote** — Triggers a new election without a specific target (e.g., Cortex is unresponsive):

```sh
curl -X POST -H "Authorization: Bearer $MUNINN_CLUSTER_SECRET" \
  http://127.0.0.1:8475/v1/replication/promote
```

Use when the Cortex has failed and you need a new leader elected. The Lobe with quorum votes will become Cortex.

---

## 6. Rolling Upgrades

### Upgrade Order

1. **Sentinels** → 2. **Replicas** → 3. **Failover** → 4. **Old Primary (now Replica)**

### Step-by-Step Procedure

1. **Upgrade Sentinels**
   - Stop sentinel: `muninn stop`
   - Replace binary, start: `muninn start`
   - Verify: `muninn cluster info` shows sentinel as member

2. **Upgrade Replicas (one at a time)**
   - Stop replica: `muninn stop`
   - Replace binary
   - Start: `muninn start`
   - Wait for WAL catch-up: `muninn cluster status` until `replication_lag` is 0 or low

3. **Trigger Graceful Failover**
   - Target the upgraded replica with least lag
   - `POST /api/admin/cluster/failover` with `target_node_id`
   - Old primary becomes replica

4. **Upgrade Old Primary**
   - Stop the demoted primary (now replica)
   - Replace binary, start
   - Wait for catch-up

### One-time migration on first start of a post-#726 binary

The replication keyspace moved off Pebble prefix `0x19` (which it shared with idempotency
receipts) onto `0x2F`. Storage migration **v5** runs automatically on first start and:

- relocates the replication metadata (sequence counter, applied watermark, schema version,
  cluster epoch, node role, snapshot sentinel) — values preserved, so sequence numbering
  and fencing state carry over unchanged;
- **drops the old replication log entries** and compacts the range. On the deployments that
  motivated this, that is where the tens of gigabytes come back.

Operational consequence: a Lobe that was behind the Cortex's retained log when the Cortex
restarts will **rejoin by snapshot** rather than by incremental catch-up. That is the same
consequence a prune has, it is automatic, and it is bounded by snapshot transfer time.
Follow the normal upgrade order below so at most one node is resnapshotting at a time.

Migration v5 cannot be undone: **a pre-v5 binary refuses to start against a migrated data
directory** (the refuse-newer guard). That refusal is deliberate — an older binary would
find no sequence counter, restart the replication log at 1, and re-issue sequence numbers
its followers have already applied. To roll back, restore the data directory from the
backup taken before the upgrade.

### Schema Version Compatibility

- Schema version is stored in Pebble. **Downgrades are blocked** if the stored version > binary version.
- Upgrades (stored < current) auto-update the stored version.
- Ensure all nodes run the same or compatible binary version before starting the rolling upgrade.

### Rollback Procedure

- If a node fails to start after upgrade: restore the previous binary and data directory from backup.
- If failover fails: the old Cortex remains in DRAINING until it receives HANDOFF_ACK or times out. Restart the old Cortex to clear draining; it will rejoin as Lobe.
- If schema incompatibility: do not downgrade the binary; restore from backup taken before the upgrade.

---

## 7. Disaster Recovery

### Creating Backups

**Option A: `muninn backup` (recommended)**

```sh
# Offline backup (server stopped):
muninn backup --data-dir ~/.muninn/data --output /backups/muninn-$(date +%Y%m%d)

# Online backup (server running) — via REST API:
curl -X POST http://127.0.0.1:8475/api/admin/backup \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"output_dir": "/backups/muninn-online"}'
```

The backup creates a Pebble checkpoint (hardlinked, space-efficient) plus copies of the `wal/` directory and `auth_secret` file.

**Option B: Vault export (per-vault)**

```sh
curl -H "Cookie: <admin-session>" \
  "http://127.0.0.1:8475/api/admin/vaults/default/export" \
  -o default.muninn
```

**Option B: Data directory copy**

Stop the node, then:

```sh
tar -czvf muninn-backup-$(date +%Y%m%d).tar.gz ~/.muninn/data/
# or /data in Docker
```

Includes: `pebble/`, `wal/`, `auth_secret`, `cluster.yaml`, etc.

### Restoring from Backup

**Vault import** (for vault export):

```sh
curl -X POST "http://127.0.0.1:8475/api/admin/vaults/import?vault=restored" \
  -H "Content-Type: application/gzip" \
  -H "Cookie: <admin-session>" \
  --data-binary @default.muninn
```

**Full restore**: Replace `{dataDir}` with the backup contents, then start. For a cluster, restore to a standalone node first, verify, then reconfigure as primary and re-add replicas if needed.

### Recovering from Split-Brain

Epoch-based fencing prevents data corruption during partitions. If a partition heals:

- The node with the **higher epoch** is authoritative.
- Reconciliation runs automatically when a Lobe reconnects (after ~2s delay for initial WAL catch-up).
- The Cortex's Reconciler compares Hebbian weights across Lobes and syncs divergent state.

Manual reconciliation (if needed) is available via internal APIs; the coordinator triggers it on `OnRecover`.

### Rebuilding a Replica from Scratch

1. Stop the replica.
2. Delete its data directory (or just `pebble/` and `wal/`).
3. Restart. The JoinHandler will send a **snapshot** if the Lobe's `LastApplied` is far behind or missing.
4. After snapshot, the NetworkStreamer streams from seq 0.

---

## 8. Troubleshooting

### Node Won't Join

| Symptom | Check |
|---------|-------|
| "invalid cluster secret" | Ensure `cluster_secret` matches on all nodes. Join token must be valid when token auth is enabled. |
| "protocol version not supported" | Upgrade the Cortex first if Lobe binary is newer. Upgrade the Lobe if it's older than `MinSupportedProtocolVersion`. |
| Connection refused | Ensure `bind_addr` is reachable (firewall, security groups). Seeds must use the same port as cluster traffic (typically 8474). |
| "epoch 0" rejections | Cortex has not completed bootstrap. Wait for Cortex to elect itself, then retry join. |

### Replica Falling Behind

- **Network**: Check bandwidth and latency between Cortex and Lobe. Replication is WAL streaming over MBP.
- **SafePrune**: Cortex prunes WAL every 60s once all Lobes have acked. If a Lobe is very far behind, it may need a snapshot (restart with empty/fresh data dir to trigger).
- **Apply throughput**: Check Lobe CPU and disk; applying entries must keep up with stream rate.

### Failover Won't Complete

- **Graceful failover timeout**: Ensure target is a connected Lobe. Check epoch store is writable. Verify HANDOFF reaches the target (network).
- **Election fails**: Quorum must be available. Add Sentinels if you have few voters. Ensure `cluster_secret` and network allow all voters to communicate.
- **ODOWN not firing**: Increase heartbeat frequency or add more Sentinels so quorum can agree on SDOWN.

### Partition Recovery

When connectivity is restored, reconciliation triggers automatically after ~2s (to allow initial WAL catch-up). Monitor logs for `post-reconnect reconciliation complete`. If cognitive consistency (CCS) drops, check `GET /v1/cluster/cognitive/consistency`.

---

## 9. Configuration Reference

### ClusterConfig Fields

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `enabled` | bool | false | Enable cluster mode |
| `node_id` | string | auto | Unique node ID. Auto: `{hostname}-{hash}` |
| `bind_addr` | string | — | Address for cluster traffic (e.g. `0.0.0.0:8474`) |
| `seeds` | []string | [] | Seed addresses for replicas/sentinels/observers |
| `cluster_secret` | string | "" | Shared secret; enables join tokens. Empty = insecure dev |
| `role` | string | "auto" | `primary` \| `replica` \| `sentinel` \| `observer` \| `auto` |
| `lease_ttl` | int | 10 | Lease TTL in seconds |
| `heartbeat_ms` | int | 1000 | MSP heartbeat interval |
| `quorum_loss_timeout_sec` | int | 5 | How long Cortex tolerates lost quorum before self-demoting |
| `join_token_ttl_min` | int | 15 | Lifetime of join tokens in minutes |
| `failover_convergence_timeout_sec` | int | 30 | How long graceful failover waits for Lobes to catch up |
| `handoff_ack_timeout_sec` | int | 5 | Timeout for HANDOFF_ACK during graceful failover |
| `prune_interval_sec` | int | 60 | How often Cortex prunes fully-replicated WAL segments and replication-log entries |
| `max_log_backlog` | int | 5000 | Hard ceiling on replication-log entries retained behind the head, regardless of replica acks. A Lobe left behind the prune point is dropped and rejoins via snapshot. `0` disables the ceiling (unbounded retention) |
| `recon_delay_ms` | int | 2000 | Delay before reconciliation after Lobe reconnects |
| `tls` | TLSConfig | — | Mutual TLS for inter-node traffic |

Config files: `{dataDir}/muninn.yaml` (cluster: section) or `{dataDir}/cluster.yaml`.

### Environment Variables

| Variable | Overrides |
|----------|-----------|
| `MUNINN_CLUSTER_ENABLED` | `enabled` |
| `MUNINN_CLUSTER_NODE_ID` | `node_id` |
| `MUNINN_CLUSTER_BIND_ADDR` | `bind_addr` |
| `MUNINN_CLUSTER_SEEDS` | `seeds` (comma-separated) |
| `MUNINN_CLUSTER_SECRET` | `cluster_secret` |
| `MUNINN_CLUSTER_ROLE` | `role` |
| `MUNINN_CLUSTER_LEASE_TTL` | `lease_ttl` |
| `MUNINN_CLUSTER_HEARTBEAT_MS` | `heartbeat_ms` |

### Port Requirements

| Port | Protocol | Purpose |
|------|----------|---------|
| 8474 | TCP | MBP + cluster inter-node (typical `bind_addr`) |
| 8475 | HTTP | REST API |
| 8476 | HTTP | Web UI |
| 8477 | HTTP/2 | gRPC |
| 8750 | HTTP | MCP |

Ensure `bind_addr` (cluster traffic) is open between all nodes; other ports are for client access.
