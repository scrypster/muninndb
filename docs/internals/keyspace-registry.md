# Pebble keyspace registry

**The single most important artifact for preventing a whole class of bug.** MuninnDB
runs `internal/storage`, `internal/auth`, and `internal/replication` over **one shared
Pebble database**. Prefixes are single bytes. If two packages claim the same byte, they
silently corrupt each other's scans — this *was* the live bug in #611 (auth's 0x11–0x14
overlapping storage), **resolved by #618**, which relocated auth to 0x42–0x45 with a
one-time migration and consolidated the whole registry behind a single source of truth.

**Rule for any PR that adds or changes a Pebble key:** the new prefix must be disjoint
from every row below and added to the registry. The source of truth is
`internal/prefix/prefix.go` (`prefix.All()` — the one authoritative allocation table);
`internal/storage/keys/keys.go` and `internal/auth/keys.go` reference those constants and
never inline a raw byte. The disjointness cross-check is `TestAll_NoDuplicateBytes` and
`TestAll_OwnerGroupsPairwiseDisjoint` in `internal/prefix/prefix_test.go`, both deriving
their bounds from `prefix.All()`. There is **no `storageMaxPrefix` constant** anymore:
adding a prefix means adding an entry to `prefix.All()`, and the disjointness test
auto-tightens to cover it (no bound to bump).

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
| **0x0C** | ws+tagHash(4)+id | — | **tag index — now READ by tag-scoped recall (`ListByTagInRange`/`ListByTagsAllInRange`, wired #619); no longer dead weight** |
| 0x0D | ws+creatorHash(4)+id | — | creator index |
| 0x0E | ws | vault name string | vault meta |
| 0x0F | siphash(name)(8) | ws(8) | name→prefix index |
| 0x10 | ws+bucket(1)+id | — | relevance bucket |
| 0x11 | ulid(16) — **GLOBAL, not vault-scoped** | DigestFlags | storage-only since #618 (auth moved off 0x11) |
| 0x12 | ws | CoherenceKey | storage-only since #618 |
| 0x13 | ws | VaultWeights | storage-only since #618 |
| 0x14 | ws+src(16)+dst(16) | AssocWeightIndex float32 | storage-only since #618 |
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
| 0x29 | ws+eventULID(16) | msgpack RecallEvent | recall-event calibration record (#573); event-time key order; reads purpose-gated |
| **0x2A** | ws+ulid | JSON Lease{owner,heartbeat,ttl} | ownership-lease sidecar (advisory) |
| **0x2C** | ws+Hash(tagKey)(4)+value+0x00+id(16) | — | **ordered raw-tag-range index (S1).** Distinct from 0x0C: keys on `Hash(tagKey)` (the part before the first `:`) with the raw VALUE bytes sorted after it, so a bounded range scan (e.g. `due:<=2026-07-27`) is a real Pebble range scan, not a post-hoc filter. Only tags containing `:` get an entry (gates write-amp to key:value tag conventions). The `0x00` separator after value resolves prefix-of-each-other values ("2026" < "2026-07" because `0x00 < '-'`). A tag value containing a `0x00` byte is rejected at write time (`storage.WriteRawTagIndexEntry`). Hash collisions between two distinct tag keys make their ranges interleave — phase-6 `passesMetaFilter` re-checks the real tag, so correctness holds and only perf degrades, mirroring 0x0C's own collision tolerance. Maintained at every 0x0C write/delete site (`internal/storage/batch.go`, `impl.go`, `engram.go`); backfilled for pre-existing data by migration v4 (`internal/storage/migrate/v4_raw_tag_range.go`). Seeds activation candidates via `ActivationEngine.seedTagCandidates`/`ScanRawTagRange` for `tag_prefix` filters with `lte`/`gte`/`lt`/`gt`/`eq`, instead of only being checked in phase 6. |

## Auth prefixes (`internal/auth/keys.go`)

| Prefix | Key shape | Value | Notes |
|---|---|---|---|
| 0x40 | hash16 | Capability (JSON) | cap_ tokens (#612), relocated off 0x15/0x16 |
| 0x41 | vault+0x00+capID(8) | storageHash16 | cap_ vault index |
| 0x42 | username-bytes | AdminUser (JSON) | relocated from 0x11 by #618 |
| 0x43 | hash16 | APIKey (JSON) | relocated from 0x12 by #618 |
| 0x44 | vault+0x00+keyID(8) | — | APIKey vault index; relocated from 0x13 by #618 |
| 0x45 | vault-name | VaultCfg (JSON) | relocated from 0x14 by #618 |

## Replication prefixes (`internal/replication/`) — all under 0x19

| Key | Meaning |
|---|---|
| 0x19 + seq_be64(8) | replication log entry (msgpack) |
| 0x19 0x02 \| "last_app" | last applied |
| 0x19 0x03 \| "cluster_epoch" / "node_role" / "schema_v" | epoch/role/schema |
| 0x19 0x10 \| "snap_complete" | snapshot marker |

## Free bytes

`0x2B` and `0x2D`–`0x3F` and `0x46`+ are free for new storage/auth keys (0x2C is now
allocated: raw-tag-range index (S1); 0x40–0x45 are allocated: 0x40/0x41
capability, 0x42–0x45 auth). (`0x2B` is deliberately held for the evolve-repair
watermark in PR #681, which allocated 0x2B independently; S1 took 0x2C to avoid
that collision, so a reviewer should treat 0x2B as spoken-for until #681 lands or
is abandoned.) (`0x29`/`0x40`/`0x41` also appear in
`internal/transport/mbp/frame.go` as **wire opcodes** — a different keyspace;
coincidental, safe, but confusing. Prefer `0x2D+` for new storage prefixes.)

## Live hazards a reviewer must know

1. **#611 — auth 0x11–0x14 collided with storage — RESOLVED by #618.** Before #618,
   `AdminExists` scanned `[0x11,0x12)` and saw storage's global DigestFlags →
   false-positive admin existence / miscount, and `ListVaultConfigs` scanned `[0x14,0x15)`
   = O(all association weights in the DB), not O(vault configs). #618 relocated auth to
   0x42–0x45 with a one-time migration and added the cross-package disjointness test
   (`prefix_test.go`), so 0x11–0x14 are now storage-only. No action needed; do not
   reintroduce an auth key in 0x11–0x14.

2. **0x19 is shared idempotency+replication territory.** `PurgeExpiredIdempotency` scans
   the *entire* 0x19 range including replication log/epoch entries and only survives
   because msgpack payloads fail JSON unmarshal (silently skipped). Any special-cased key
   there must be **exact-match, never prefix-skip** (as `snapshot.go` already does for
   `cluster_epoch`). Never switch replication values to JSON or receipts to msgpack
   without revisiting this.

3. **0x0C tag index is now read by tag-scoped recall (#619).** `ListByTagInRange` /
   `ListByTagsAllInRange` (`internal/storage/query.go`) seed the candidate pool for
   `tags_all`/`tags_any`/`tag_prefix` recalls (`internal/engine/activation/engine.go`), so
   the index is a live read path — no longer write-amplifying dead weight. Keep it in sync
   on the write path; a PR that drops maintenance now breaks tag-scoped recall.

4. **Vault prefixes are name-deterministic and therefore REUSED on name reuse.** Deleting
   a vault (`ClearVault` + `DeleteVaultNameOnly`) does not clean 0x11 DigestFlags
   ("orphans acceptable") or 0x1F global entity records. Re-creating a vault of the same
   name recomputes the identical SipHash prefix and resurfaces those orphans. The correct
   invariant is "prefixes are name-deterministic," **not** "never reused." A PR asserting
   the latter is wrong about this codebase.
