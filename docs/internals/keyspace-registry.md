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
| 0x0A | ws+conceptHash(4)+relType(2)+id | partner id(16) + detectedAt unixnano(8) | contradiction marker; conceptHash/relType are written as 0. 16-byte values are legacy (pre-timestamp) and decode to an UNKNOWN detection time, never a zero instant |
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
| 0x16 | ws+id(16)+ts(8)+seq(4) | provenance entry (**JSON**, additively extensible) | async worker; the value is `provenance.ProvenanceEntry` marshalled with encoding/json, so a new *optional* field needs no version byte: old entries decode with the field ABSENT (never a zero value posing as data), and new entries decode on an old binary, which ignores the unknown key. `Details` (evolve's predecessor_id / reason / effective_at; extensible per-operation) was added this way. Renaming or retyping an existing field, or making a new field load-bearing for correctness, WOULD require a real format version |
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
| 0x2B | ws | 1B repair version | evolve entity-link repair watermark (#622); presence at current version skips the startup scan |
| **0x2C** | ws+Hash(tagKey)(4)+value+0x00+id(16) | — | **ordered raw-tag-range index (S1).** Distinct from 0x0C: keys on `Hash(tagKey)` (the part before the first `:`) with the raw VALUE bytes sorted after it, so a bounded range scan (e.g. `due:<=2026-07-27`) is a real Pebble range scan, not a post-hoc filter. Only tags containing `:` get an entry (gates write-amp to key:value tag conventions). The `0x00` separator after value resolves prefix-of-each-other values ("2026" < "2026-07" because `0x00 < '-'`). A tag value containing a `0x00` byte is rejected at write time (`storage.WriteRawTagIndexEntry`). Hash collisions between two distinct tag keys make their ranges interleave — phase-6 `passesMetaFilter` re-checks the real tag, so correctness holds and only perf degrades, mirroring 0x0C's own collision tolerance. Maintained at every 0x0C write/delete site (`internal/storage/batch.go`, `impl.go`, `engram.go`); backfilled for pre-existing data by migration v4 (`internal/storage/migrate/v4_raw_tag_range.go`). Seeds activation candidates via `ActivationEngine.seedTagCandidates`/`ScanRawTagRange` for `tag_prefix` filters with `lte`/`gte`/`lt`/`gt`/`eq`, instead of only being checked in phase 6. |
| **0x2D** | ws+EntityNameHash(cue)(8)+intentionID(16) | msgpack {one_shot, created_at, fired_count, last_fired_at, cues[]} | **armed-intention index (THE PUSH, prospective memory).** One key per (intention, cue entity); `ScanArmedForEntity` is a 17-byte-prefix scan consulted ONLY inside recall/remember tool handlers when the cue entity is focal — nothing polls it. The value duplicates the full cue list across an intention's keys so a one-shot fire deletes every sibling key atomically (`MarkIntentionFired`) and entity-merge can rewrite stale cue names (`RelinkProspectiveIntent`, mirroring the 0x26 relink; called from `MergeEntity`). Cleared by `ClearVault`. NOT exported/cloned (intentions are session-arming state; documented residual). Stale keys for a deleted intention engram are inert — the firing rule re-verifies the engram (exists, active, ValidAt) before delivery. The design doc allocated 0x2C for this index, but 0x2C was taken by RawTagRange (S1) first; 0x2D is the real allocation. |
| **0x2E** | ws | 1B repair version | **pre-fix full-weight association-key repair watermark (#756).** The original `WeightComplement` overflowed at weight exactly 1.0 and wrote those 0x03/0x04 keys at the weight-0.0 complement (`0xFFFFFFFF`), where they read back as weight 0; the encoder was fixed byte-compatibly in #757 but the misplaced keys remain. `Engine.runLegacyFullWeightAssocRepair` scans 0x03 for that complement and, when the pair's 0x14 index reads **exactly 1.0**, relocates fwd/rev to the true 1.0 position (complement `0x00000000`) carrying the value bytes verbatim, deleting the legacy position; any other index value is left alone. Presence at the current version skips the scan. A one-shot watermark is sound because the fixed encoder cannot create new damage of this kind. Cleared by `ClearVault`. Edges a decay pass already clamped or deleted are unrepairable **and unidentifiable** — no count is claimed for them, and neither are pre-0x14-era pairs that carry no weight index at all. `runPruneWorker` gates assoc decay (`Engine.decayAllVaults`) on the pass completing **cleanly**: a pass that errored leaves the gate shut for the process lifetime and logs at ERROR, because decay over a still-damaged vault destroys that evidence permanently and unidentifiably. The pass is **local to each node** — nothing is replicated (the #681 precedent); it is deterministic and idempotent, so nodes converge. Upgrade the **leader first**, and upgrade followers promptly: an ungated old-binary node can destroy its own evidence before it is upgraded. |

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

`0x2F`–`0x3F` and `0x46`+ are free for new storage/auth keys (0x2B–0x2E are now
allocated: 0x2B evolve-repair watermark (#681), 0x2C raw-tag-range index (S1), 0x2D
armed-intention index (THE PUSH), 0x2E full-weight assoc-key repair watermark (#756);
0x40–0x45 are allocated: 0x40/0x41 capability, 0x42–0x45 auth). (`0x29`/`0x40`/`0x41`
also appear in `internal/transport/mbp/frame.go` as **wire opcodes**, a different
keyspace; coincidental, safe, but confusing. Prefer `0x2F+` for new storage prefixes.)

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

5. **0x2B and 0x2E are repair watermarks and share one set of semantics.** Both hold a
   single version byte per vault (0x2B: evolve entity-link repair, #681; 0x2E: pre-fix
   full-weight association-key repair, #756), and both mean the same thing: "a pass at
   this version completed cleanly over this vault, skip the scan." Consequences that
   apply to both, and to any watermark added later — put it in this family and follow
   them: presence at or above the current version short-circuits the whole scan, so
   forcing a re-run means bumping the pass's version constant (or deleting the marks);
   both are listed in `clearVaultDataPrefixes`, so `ClearVault` drops them and a reused
   vault name cannot inherit a stranger's "already repaired" claim at the cost of one
   no-op scan; and a repair whose watermark is present is never revisited, so the
   watermark must only be written on a genuinely clean pass.
