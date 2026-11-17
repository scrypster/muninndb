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

Response: `status` (`ok` | `degraded` | `down`), `role`, `is_leader`, `epoch`, `replication_lag`,
and `reason` on a Lobe that has stopped replicating because its Cortex refused it (see
[A replica the leader refuses](#a-replica-the-leader-refuses)). `down` is answered with HTTP 503.

`replication_lag` on a Lobe is measured against the Lobe's own log, not the Cortex's, so it reads
0 on a Lobe that is receiving nothing. Do not read `ok` with lag 0 as proof that a Lobe is
complete. See [What a failover does to replication](#what-a-failover-does-to-replication).

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
4. Lobes and Sentinels vote. The first candidate to collect a majority for the new epoch wins. **A vote does not compare replication positions**: a Lobe that was behind another Lobe when the Cortex died can be elected. See [What a failover does to replication](#what-a-failover-does-to-replication) for what follows from that.
5. New leader broadcasts `CortexClaim`, others demote to Lobe.

**Quorum loss**: If the Cortex cannot reach quorum for 5 seconds, it **pre-emptively demotes** itself to avoid split-brain writes.

### What a failover does to replication

Every replicated write has a sequence number, and a Lobe remembers the highest one it has
applied. It ignores anything numbered at or below that, which is what makes a replayed stream
harmless.

**The new Cortex continues the numbering.** When a Lobe is promoted (by election, by a graceful
handoff, or by `POST /v1/replication/promote`) it numbers its first write one past the last
entry it applied. Lobes that were level with it carry straight on. The promotion logs it:

```
cluster: replication sequence continues from this node's applied position node=<id> epoch=<n> log_head_was=<old> log_head=<new>
```

What that does for a Lobe depends on where it stood when the old Cortex went away. Only the
first of these three cases is repaired by the numbering:

| Where the Lobe stood | What happens | Visible? |
|---|---|---|
| **Level** with the node that won | It receives every write the new Cortex makes. | Nothing to see. |
| **Ahead** of the node that won | It holds entries the new Cortex never received, and the new Cortex's first writes reuse those sequence numbers. The new Cortex refuses it, and the refusal stays until an operator acts. See [A replica the leader refuses](#a-replica-the-leader-refuses), including what is still admitted. | Yes, when refused: `503` from its `/v1/cluster/health`, `ERROR` lines on both nodes. |
| **Behind** the node that won | It receives every write the new Cortex makes from then on, but not the entries between its own position and the new Cortex's position at promotion. The new Cortex applied those as a Lobe and has no log of them to send. | **No.** Its health reads `ok`, lag 0. This is an open defect. |

The followers of a healthy, idle cluster are level. They stop being level when one of them is
slow, disconnected or restarting at the moment the Cortex goes away, or when the old Cortex is
still taking writes while the role moves.

If a Lobe is refused, or you have reason to think it was behind at a failover, re-seed it from
the Cortex:

1. Stop the Lobe you are re-seeding. Do this first. A refused Lobe still takes part in
   elections, and stopping the Cortex starts one.
2. Stop the Cortex. This is a write outage for the length of the copy.
3. Replace the Lobe's `pebble/` and `wal/` directories with copies of the Cortex's. Leave the
   Lobe's own `cluster.yaml` and `auth_secret` in place.
4. Start the Lobe, then the Cortex.
5. Check that the cluster has a leader: `muninn cluster status`, or `is_leader` in
   `GET /v1/cluster/health` on each node.

Step 5 matters because the node you stopped may not take the lead back by itself. A Cortex
configured `role: primary` does, as soon as it starts and finds no other leader. A Cortex that
got the role through a failover is configured `role: replica`, and no node is then configured to
lead. The nodes elect one by themselves when failure detection reports a configured peer down,
which took about three seconds in a three-node cluster whose original Cortex was gone. If no
node reports `is_leader` after ten heartbeats, start an election on the node that was the
Cortex with `POST /v1/replication/promote`. Either of the two nodes may win. After the copy
they hold the same data.

Anything the Lobe held that the Cortex did not is discarded by the copy. So is the Lobe's
refusal record, which lives in `pebble/`. Deleting the Lobe's data directory and restarting it
is not a substitute: no snapshot is sent to a joining Lobe (see the list below), so it would
come back holding only what the Cortex's log still has.

### A replica the leader refuses

A Cortex refuses a Lobe that holds an entry the Cortex does not. There are two ways it can
tell. The Lobe's applied position is past the end of the Cortex's log. Or the Lobe's last
applied entry is not the entry the Cortex holds at that sequence number: every Lobe keeps a
checksum of the last entry it applied and sends it when it joins. The second is the one that
matters after a failover, because a new Cortex that is taking writes passes the Lobe's position
within seconds and the two positions then look fine.

On the Cortex:

```
join: REFUSED: the joining replica holds an entry this Cortex does not lobe=<id> lobe_last_applied=<a> cortex_head=<h> why="..."
```

On the Lobe:

```
join: REFUSED: this node holds an entry the Cortex does not node=<lobe> cortex=<id> cortex_epoch=<n> last_applied=<a> cortex_head=<h> why="..."
```

and `GET /v1/cluster/health` on the Lobe answers **503** with `"status":"down"` and a `reason`
that says the same and names both things an operator can do. `muninn cluster status` prints
that reason. The Lobe's `replication_lag` still reads 0.

The Lobe stays a member. It heartbeats and votes, so quorum does not depend on it replicating.
It keeps answering reads from the data it has. It applies nothing further.

**The refusal is kept on the Lobe's disk.** The Cortex proved the difference by reading its
own log, and a Cortex prunes its log. Asked again later, the same Cortex can often only say
that it cannot compare. So the Lobe writes the refusal down, and from then on it follows a
Cortex only when that Cortex confirms that the Lobe's last entry is its own. Being unable to
compare is not that. This means:

- Restarting the Lobe does not clear it. The Lobe answers 503 with the recorded reason from the
  moment it starts, before it has reached any Cortex.
- Restarting the Cortex, or electing a different one, does not clear it. The Lobe asks the new
  term once. The answer is logged on both nodes:
  `join: REFUSED: the joining replica was refused by a Cortex before, and this Cortex cannot confirm that its last entry is its own`.
- A Cortex running an older version does not clear it. Such a Cortex accepts every join. The
  Lobe declines by itself and stays at 503.

Four things remove it:

- A Cortex that does hold the Lobe's last entry confirms it. The Lobe logs
  `join: the refusal this node held is lifted` and follows. This is what happens when the
  cluster ends up led by a node from the Lobe's own side of the split.
- The Lobe becomes the Cortex. See "A refused Lobe can be elected" below. The record is
  removed before the node takes the role, in the same step, and none is recorded while it
  leads, even from a refusal that was on its way when it was elected.
- The re-seed above, which replaces the Lobe's `pebble/` directory.
- A snapshot from the Cortex replaces the Lobe's state. The receiving node wipes its store
  before the snapshot is written, and that wipe removes the record. No Lobe is sent a snapshot
  today.

There is no command that clears it. A refused Lobe that followed anyway would skip every write
the Cortex numbered at or below the Lobe's position, which is the failure the refusal exists to
stop.

A backup or a copy of a refused Lobe's data directory contains the record. A node started
from that copy is refused too, which is correct: it holds the same history. If you rebuild the
whole cluster from such a backup, every node starts refused. Whichever is elected removes its
own record and logs `cluster: THE CLUSTER'S HISTORY HAS CHANGED LINE`. In that case the line is
not true: every node holds the same history. The others are then confirmed by the new Cortex
and their records are lifted.

**What it means.** Over some range of sequence numbers the Lobe and the Cortex followed
different leaders. There are three ways to get here:

- a failover elected a Lobe that was behind this one
- the Cortex was rebuilt, or restored from a backup older than what its Lobes had applied
- the old Cortex was still writing while the role moved (a graceful failover or a manual
  promotion under write load), and this Lobe received entries the new Cortex did not

**What to do.** Decide which history is the cluster's. There are two choices.

- *Keep the Cortex's.* This is the usual choice, because the Cortex is the one taking writes.
  The Lobe's extra entries are discarded. If any of them matter, read them off the Lobe first,
  while it still serves reads. Then re-seed the Lobe from the Cortex with the copy procedure in
  [What a failover does to replication](#what-a-failover-does-to-replication).
- *Keep the Lobe's.* Only sensible when the Cortex is the node that is wrong, for example
  rebuilt or restored by mistake. Everything the Cortex accepted that the Lobe does not hold
  is given up. Stop writes to the cluster. Make the Lobe the Cortex with
  `POST /v1/replication/promote` on the Lobe, and confirm it with `is_leader` in its health.
  Then re-seed **every** other node from it with the same copy procedure, the previous Cortex
  first. Do not leave the previous Cortex running: nothing refuses it (see the next heading).

Do not delete the Lobe's data directory and restart it.

**A refused Lobe can be elected.** A vote looks at the epoch and nothing else, so a refused
Lobe can stand in an election and win it. It stands by itself when failure detection reports
the Cortex down, and it stands when you call promote on it. If it wins:

- It leads from its own position. Its history is the cluster's from then on, and its refusal
  record is removed.
- It logs one `ERROR` line that begins `cluster: THE CLUSTER'S HISTORY HAS CHANGED LINE` and
  names the Cortex that had refused it and the position.
- Every write the previous Cortex acknowledged that this node never applied is on the previous
  Cortex only.
- **The previous Cortex is not refused when it comes back.** It rejoins as a Lobe, keeps those
  writes on its own disk, serves them to anyone who reads from it, and reports health `ok` with
  lag 0. It is also in the "Behind" row of the table above: the new Cortex has no log of the
  entries it had applied past the previous Cortex's position. Compare the two nodes and re-seed
  one of them.

This is why the re-seed procedure stops the refused Lobe before it stops the Cortex.

**What is still admitted although the histories differ.** The Cortex refuses only when it can
prove the difference, because a wrong refusal takes a healthy replica out of service until it
is re-seeded. Each of these is silent: health `ok`, lag 0.

- *The entry was pruned before the Lobe first asked.* The Cortex has already deleted the entry
  at the Lobe's position, so there is nothing to compare, and this Lobe has no refusal on
  record. Pruning takes another replica having acknowledged past that position, or the Cortex
  being more than `max_log_backlog` entries (default 5000) past it, and a prune interval
  passing. Once a Lobe has been refused, pruning no longer matters.
- *The Lobe has no checksum yet.* It runs a version from before this check, or it has applied
  nothing since it was upgraded. Only the position is compared then, and that comparison stops
  failing once the Cortex has written past the Lobe.
- *The Cortex runs a version from before this check.* It compares nothing. It does not release
  a Lobe that already holds a refusal.
- *The difference is below the Cortex's own applied position.* The Lobe's position lies in the
  range the Cortex applied while it was itself a Lobe. The Cortex kept no log of that range.
- *After one wrong admission.* A Lobe admitted in one of the cases above applies the Cortex's
  later entries. Its last entry is then the Cortex's own, every later comparison matches, and
  the entries it skipped are never looked at again.
- *An entry the deposed Cortex sent after the new one took the role.* The new Cortex applies
  it, and its own first write then gets the same number. A Lobe that holds the deposed
  Cortex's entry at that number is compared with the entry the new Cortex applied there, which
  is the same entry, and is admitted. It never receives the new Cortex's own entry at that
  number.
- *A Cortex that was deposed.* When it rejoins it asks from the position it had applied up to
  when it was promoted, not from the end of the log it wrote as Cortex. Nothing it wrote in its
  term is compared with anything. This is how the previous Cortex gets back in after a refused
  Lobe is elected, and it is equally true of any Cortex that returns after an ordinary
  failover.
- *A checksum collision.* One in four billion.

**Upgrading and downgrading with a refused Lobe in the cluster.** Re-seed or stop a refused
Lobe before you restart the Cortex. Restarting the Cortex starts an election, and a refused
Lobe can win it (see "A refused Lobe can be elected" above). During a rolling upgrade a
refused Lobe stays at 503 whichever version the Cortex runs, until it is re-seeded or a Cortex
confirms its last entry. If the refused Lobe itself is put back on an older version, that
version does not read the record. It joins, skips as described above and reports `ok`. When it
is upgraded again the record is still there and the node is refused from the moment it starts.
Re-seed it.

**The web console does not show a refusal.** Its cluster page asks for the same health
endpoint, treats the 503 as a failed request and keeps whatever it showed last. Use
`muninn cluster status` or the endpoint itself.

**Other gaps on the failover path.** None of these is changed by the numbering fix or by the
refusal:

- **After a graceful failover, the other Lobes do not switch by themselves.** The handoff target
  becomes the Cortex and the old Cortex rejoins it as a Lobe, but any other Lobe keeps reading
  from its connection to the old Cortex, which no longer sends anything. It receives none of
  the new Cortex's writes until that connection closes, in practice until the old Cortex is
  stopped or restarted. Its health reads `ok` with lag 0 in the meantime. When the connection
  does close, the Lobe joins the new Cortex and is sent what the new Cortex's log still holds.
  After a graceful failover, restart the demoted node promptly.
- **`POST /api/admin/cluster/failover` can answer 500 although the handoff took effect.** The
  response says only that an internal error occurred. The old Cortex's log says
  `graceful failover: HANDOFF_ACK timeout (5s)`. Check `GET /v1/cluster/health` on the target
  before retrying.
- **Writes are not stopped during a graceful failover.** Step 1 of the process below says the
  Cortex enters DRAINING and rejects new writes. Nothing enforces that today. A write the old
  Cortex accepts while the role is moving can reach some Lobes and not others.
- **The old Cortex's unshipped writes.** Anything the old Cortex had accepted but not yet
  shipped when it went away stays on its disk and is in nobody else's history. Nothing detects
  that when it rejoins.
- **Keyword index entries are not replicated.** The full-text index writes its postings
  straight to the local store, outside the replication log. A Lobe holds the memories and not
  their postings. After a failover the new Cortex has postings only for what it has written
  since it took the role, so a keyword search does not find older memories there.
  `POST /api/admin/vaults/{name}/reindex-fts` on the Cortex rebuilds a vault's keyword index
  from the stored memories.
- **No Lobe is sent a snapshot.** Other sections of this guide say a joining Lobe receives one.
  In a running server it does not. A Lobe that joins, or reattaches, after the Cortex has
  pruned its log is sent only what the log still holds.
- **A node can count one voter too many.** If a peer's handshake is served before startup has
  finished registering this node's seeds, a `seed-<addr>` placeholder stays in its voter list
  alongside that peer's real id. The quorum on that node is then one higher than it should
  be, and a three-node cluster that loses a node may fail to elect. The list is rebuilt when
  that node restarts. How often this happens is not measured.

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
   - Promote an **upgraded** node. Whether the sequence numbering continues across a failover
     is decided by the binary of the node that is promoted. A node running a build from before
     that fix restarts the numbering, and every Lobe, whatever it runs, then discards its
     writes until the counter passes the old Cortex's head.

4. **Upgrade Old Primary**
   - Stop the demoted primary (now replica). Do this promptly: the other replicas start
     following the new primary only when their connection to the old one closes (see
     [What a failover does to replication](#what-a-failover-does-to-replication)).
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
| "lobe cannot follow this cortex", or `join: REFUSED: this node holds an entry the Cortex does not` | The Lobe holds entries the Cortex never received. It will not join until an operator chooses a history. See [A replica the leader refuses](#a-replica-the-leader-refuses). |

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
| `tls` | TLSConfig | — | Read but **not applied** in this release. Inter-node traffic is not encrypted. See [Network security](#network-security) |

Config files: `{dataDir}/muninn.yaml` (cluster: section) or `{dataDir}/cluster.yaml`.

### Network security

Traffic between cluster nodes is **not encrypted** in this release. The `tls` setting above is read but not applied, so memory contents, replication entries and elections travel between nodes as plain TCP.

What the cluster secret does and does not cover:

- A node asking to join sends a value derived from the secret, and the primary refuses a node that cannot produce it.
- Peer hellos are signed with the secret. The signature does not change between connections, so it can be replayed, and the frames after it are not signed.
- The answer to a join, and the replication stream that follows, carry nothing the joining node can check. Whatever answers a node's join dial is trusted by that node.

So treat the cluster `bind_addr` like a database port: run the nodes on a private network you control, or link them over an encrypted tunnel such as WireGuard or a VPN, and firewall the cluster port so that only the other cluster members can reach it. With an empty `cluster_secret` any host that can reach the port can join.

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
