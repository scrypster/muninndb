# Pebble keyspace registry

**The single most important artifact for preventing a whole class of bug.** MuninnDB
runs `internal/storage`, `internal/auth`, and `internal/replication` over **one shared
Pebble database**. Prefixes are single bytes. If two packages claim the same byte, they
silently corrupt each other's scans — this is exactly the live bug in #611.

**Rule for any PR that adds or changes a Pebble key:** the new prefix must be disjoint
from every row below and documented where the registry lives. Today the source of truth is
the `internal/storage/keys/keys.go` doc comments plus `internal/auth/keys.go`, and the
cross-check is `TestCapabilityPrefixesNoCollision` in `internal/auth/keys_test.go` (which
currently derives storage's upper bound from a `storageMaxPrefix` constant — bump it if you
extend storage past `0x2A`).

> **In flight — PR #618 (#611 fix):** this consolidates the whole registry into a new
> `internal/prefix/prefix.go` package with a stronger disjointness test
> (`TestAll_NoDuplicateBytes`, `TestAll_OwnerGroupsPairwiseDisjoint`) that derives its
> bounds from `prefix.All()`, and it **removes `storageMaxPrefix`** and relocates auth's
> 0x11–0x14 to 0x42–0x45. When #618 merges, update this doc and STO-1: the source of truth
> becomes `internal/prefix/`, and the "bump `storageMaxPrefix`" guidance is replaced by
> "add the prefix to `prefix.All()` — the disjointness test auto-tightens." Verify which
> world you're in with `ls internal/prefix/` before reviewing a keyspace change.

`ws` = 8-byte SipHash(vault name) prefix (deterministic; a name always maps to the same
prefix — see the vault-reuse note at the bottom).

## Storage prefixes (`internal/storage/keys/keys.go`)

| Prefix | Key shape after prefix | Value | Notes |
|---|---|---|---|
| 0x01 | ws+ulid(16) | Engram (ERF) | primary record |
| 0x02 | ws+ulid(16) | EngramMeta | |
| 0x03 | ws+src(16)+weightComplement(4)+dst(16) | — | forward assoc, sorted desc by weight |
| 0x04 | ws+dst(16)+weightComplement(4)+src(16) | — | reverse assoc |
| 0x05 | ws+term+0x00+field(1)+id | posting | FTS |
| 0x06 | ws+trigram(3)+id | — | FTS trigram |
| 0x07 | ws+id+layer(1) | neighbor list | HNSW graph |
| 0x08 / 0x09 | ws+"stats" / ws+term | FTS stats | |
| 0x0A | ws+conceptHash(4)+relType(2)+id | contradiction | |
| 0x0B | ws+state(1)+id | — | state index |
| **0x0C** | ws+tagHash(4)+id | — | **tag index — maintained on every write, ZERO non-test readers (#607)** |
| 0x0D | ws+creatorHash(4)+id | — | creator index |
| 0x0E | ws | vault name string | vault meta |
| 0x0F | siphash(name)(8) | ws(8) | name→prefix index |
| 0x10 | ws+bucket(1)+id | — | relevance bucket |
| **0x11** | ulid(16) — **GLOBAL, not vault-scoped** | DigestFlags | **collides with auth AdminUser (#611)** |
| **0x12** | ws | CoherenceKey | **collides with auth APIKey (#611)** |
| **0x13** | ws | VaultWeights | **collides with auth APIKey vault idx (#611)** |
| **0x14** | ws+src(16)+dst(16) | AssocWeightIndex float32 | **collides with auth VaultCfg (#611)** |
| 0x15 | ws | BE int64 count | vault engram count ("sole user" per comment) |
| 0x16 | ws+id(16)+ts(8)+seq(4) | provenance | async worker |
| 0x17 | ws | migration version+cursor | |
| 0x18 | ws+ulid | quantize params + int8 embedding | ERF v2 vector |
| **0x19** | siphash(opID)(8) → JSON receipt | idempotency | **shared with replication (see below) — safe only by JSON-vs-msgpack decode accident** |
| 0x1A | ws+episodeID(16)[+0xFF+pos(4)] | episode/frame | |
| 0x1B | ws | uint8 FTS schema version | |
| 0x1C | ws+src+dst | PAS transition | |
| 0x1D | ws | embed model name string | |
| 0x1E | ws+parentID+childID | ordinal | |
| 0x1F | entityNameHash(8) — **GLOBAL** | entity record | identity = NFKC+lower+trim |
| 0x20 | ws+engramID+nameHash(8) | — | engram→entity link |
| 0x21 | ws+engramID+fromHash(8)+relType(1)+toHash(8) | — | relationship record |
| 0x22 | ws+^millis(8)+id | — | last-access (inverted for MRU-first) |
| 0x23 | nameHash(8)+ws(8)+id | — | entity reverse index (global prefix, vault in suffix) |
| 0x24 | ws+hashA(8)+hashB(8) | msgpack count | co-occurrence |
| 0x25 | ws+src+dst | archived assoc (no weight sort, no reverse) | |
| 0x26 | ws+entityHash(8)+engramID | — | rel-entity index |
| 0x27 | ws | 16B dream state | |
| 0x28 | ws+sha256(32) | engramID(16) | content-hash dedup |
| **0x2A** | ws+ulid | JSON Lease{owner,heartbeat,ttl} | ownership-lease sidecar (advisory) |

## Auth prefixes (`internal/auth/keys.go`)

| Prefix | Key shape | Value | Notes |
|---|---|---|---|
| **0x11** | username-bytes | AdminUser (JSON) | **collides with storage DigestFlags** |
| **0x12** | hash16 | APIKey (JSON) | **collides with storage CoherenceKey** |
| **0x13** | vault+0x00+keyID(8) | — | APIKey vault index; **collides with storage VaultWeights** |
| **0x14** | vault-name | VaultCfg (JSON) | **collides with storage AssocWeightIndex** |
| 0x40 | hash16 | Capability (JSON) | cap_ tokens (#612), relocated off 0x15/0x16 |
| 0x41 | vault+0x00+capID(8) | storageHash16 | cap_ vault index |

## Replication prefixes (`internal/replication/`) — all under 0x19

| Key | Meaning |
|---|---|
| 0x19 + seq_be64(8) | replication log entry (msgpack) |
| 0x19 0x02 \| "last_app" | last applied |
| 0x19 0x03 \| "cluster_epoch" / "node_role" / "schema_v" | epoch/role/schema |
| 0x19 0x10 \| "snap_complete" | snapshot marker |

## Free bytes

`0x29`, `0x2B`–`0x3F`, `0x42`+ are free for new storage/auth keys. (`0x29`/`0x40`/`0x41`
also appear in `internal/transport/mbp/frame.go` as **wire opcodes** — a different
keyspace; coincidental, safe, but confusing. Prefer `0x2B+` for new storage prefixes.)

## Live hazards a reviewer must know

1. **#611 — auth 0x11–0x14 collide with storage.** `AdminExists` scans `[0x11,0x12)` and
   sees storage's global DigestFlags → false-positive admin existence / miscount.
   `ListVaultConfigs` scans `[0x14,0x15)` = O(all association weights in the DB), not
   O(vault configs). The fix (green-lit) is relocation + one-time migration + a
   cross-package disjointness test — **not** an assertion-only patch. Until then, any PR
   widening a scan over 0x11–0x14 makes it worse.

2. **0x19 is shared idempotency+replication territory.** `PurgeExpiredIdempotency` scans
   the *entire* 0x19 range including replication log/epoch entries and only survives
   because msgpack payloads fail JSON unmarshal (silently skipped). Any special-cased key
   there must be **exact-match, never prefix-skip** (as `snapshot.go` already does for
   `cluster_epoch`). Never switch replication values to JSON or receipts to msgpack
   without revisiting this.

3. **`storageMaxPrefix` is hardcoded 0x2A** in the collision test. The day storage adds
   0x2B, the guard silently weakens unless the constant is bumped. Flag any new storage
   prefix ≥ 0x2B for this.

4. **0x0C tag index is write-amplifying dead weight** — maintained on every tag mutation,
   zero production readers (#607). Don't add write-path cost assuming it's consumed;
   either wire the reader (the green-lit #607 fix) or plan removal.

5. **Vault prefixes are name-deterministic and therefore REUSED on name reuse.** Deleting
   a vault (`ClearVault` + `DeleteVaultNameOnly`) does not clean 0x11 DigestFlags
   ("orphans acceptable") or 0x1F global entity records. Re-creating a vault of the same
   name recomputes the identical SipHash prefix and resurfaces those orphans. The correct
   invariant is "prefixes are name-deterministic," **not** "never reused." A PR asserting
   the latter is wrong about this codebase.
