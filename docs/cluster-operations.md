# MuninnDB Cluster Operations Runbook

Operational guide for running and maintaining MuninnDB clusters. For architecture details, see [architecture.md](architecture.md) and [internal/replication/INTEGRATION.md](../internal/replication/INTEGRATION.md).

---

## 1. Cluster Architecture Overview

### Cortex/Lobe Model

MuninnDB uses a **Cortex/Lobe** model (internally: leader/replica terminology):

- **Cortex (Primary)**: Single writer. Accepts writes, runs cognitive workers (temporal, Hebbian, contradiction, confidence), streams WAL to Lobes, handles join requests.
- **Lobe (Replica)**: Read-only copy. Receives WAL stream from Cortex, applies entries to local Pebble, forwards cognitive side effects to Cortex. Can be promoted to Cortex during failover.

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

### Epoch-Based Fencing

- Every leadership change increments the **epoch** (stored in EpochStore).
- The epoch serves as a **fencing token**: writes from a demoted Cortex with a stale token are rejected.
- Prevents split-brain writes during failover or partition recovery.

---

## 2. Initial Cluster Setup

### Prerequisites

- **Network**: All nodes must reach each other on the cluster bind port (typically 8474, MBP protocol).
- **Ports**:
  - Cluster inter-node: `bind_addr` (default `:8474`, same as MBP)
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

**Or** enable cluster mode on a running node via REST:

```sh
curl -X POST http://127.0.0.1:8475/api/admin/cluster/enable \
  -H "Content-Type: application/json" \
  -H "Cookie: <admin-session-cookie>" \
  -d '{
    "role": "primary",
    "bind_addr": "0.0.0.0:8474",
    "cluster_secret": "your-secure-secret"
  }'
```

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
| `prune_interval_sec` | int | 60 | How often Cortex prunes fully-replicated WAL segments |
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
