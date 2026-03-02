# Entity Graph

MuninnDB maintains a two-layer entity knowledge graph. The first layer is a
global entity registry keyed only by entity name (vault-agnostic). The second
layer is a vault-scoped relationship and link index that connects entities to
the engrams that mention them.

The graph is populated in two ways and queried through nine dedicated MCP tools.

---

## What Entities Are

An entity is a named thing extracted from memory content. Supported categories
include people, organizations, technologies, concepts, and events — but the
type field is a free-form string, so any category may be used. The entity name
is normalized to NFKC Unicode form and lower-cased before hashing so that
`PostgreSQL`, `postgresql`, and `PostgreSQL` all resolve to the same record.

Each entity is represented by an `EntityRecord` struct stored at the `0x1F`
key prefix:

```go
type EntityRecord struct {
    Name         string  `msgpack:"name"`
    Type         string  `msgpack:"type"`
    Confidence   float32 `msgpack:"confidence"`
    Source       string  `msgpack:"source"`        // "inline", "plugin:enrich", etc.
    UpdatedAt    int64   `msgpack:"updated_at"`    // Unix nanos
    FirstSeen    int64   `msgpack:"first_seen"`    // Unix nanos, set once on first upsert
    MentionCount int32   `msgpack:"mention_count"` // incremented on every upsert
    State        string  `msgpack:"state"`         // "active", "deprecated", "merged", "resolved"
    MergedInto   string  `msgpack:"merged_into"`   // set when State == "merged"
}
```

---

## Populating the Graph

### Path 1: Inline at write time

When calling `muninn_remember` or `muninn_remember_batch`, pass the `entities`
and `entity_relationships` parameters. This bypasses the background enrichment
pipeline and writes the graph synchronously with the engram.

```json
{
  "vault": "default",
  "concept": "service architecture",
  "content": "Auth Service delegates session caching to Redis.",
  "entities": [
    { "name": "Auth Service", "type": "service" },
    { "name": "Redis", "type": "database" }
  ],
  "entity_relationships": [
    {
      "from_entity": "Auth Service",
      "to_entity": "Redis",
      "rel_type": "caches_with",
      "weight": 0.9
    }
  ]
}
```

`entities` is an array of `{ name, type }` objects. `entity_relationships` is
an array of `{ from_entity, to_entity, rel_type, weight }` objects. All four
fields are required on each relationship entry. The `rel_type` must be one of
the named types in the `relTypeBytes` map (see Relationship Types below) or it
will be stored with byte `0xFF` (unknown).

### Path 2: Automatic via enrichment plugin

When no `entities` or `entity_relationships` are provided, the enrichment
pipeline runs in the background after the engram is written. The pipeline
extracts entities and relationships using an LLM provider configured at server
startup. Use `muninn_replay_enrichment` to retroactively run this pipeline for
engrams stored before an LLM provider was configured.

---

## Storage Model

The entity graph occupies five key-space prefixes in the Pebble store. Note
that prefix `0x22` is used by the LastAccess index and is not part of the
entity graph.

| Prefix | Scope  | Key layout                                                    | Value                        | Purpose |
|--------|--------|---------------------------------------------------------------|------------------------------|---------|
| `0x1F` | Global | `0x1F \| nameHash(8)`                                         | `msgpack(EntityRecord)`      | Global entity registry. One record per unique normalized name. Cross-vault. |
| `0x20` | Vault  | `0x20 \| ws(8) \| engramID(16) \| entityNameHash(8)` (33 B)  | Raw entity name string       | Forward link: "which entities does engram X mention?" Scanned by prefix `0x20\|ws\|engramID`. |
| `0x21` | Vault  | `0x21 \| ws(8) \| engramID(16) \| fromHash(8) \| relTypeByte(1) \| toHash(8)` (42 B) | `msgpack(RelationshipRecord)` | Per-engram relationship assertion. `ExportGraph` deduplicates by max-weight per triple. |
| `0x23` | Cross  | `0x23 \| nameHash(8) \| ws(8) \| engramID(16)` (33 B)        | Empty (key encodes all data) | Reverse index: "which engrams mention entity Y?" Written atomically with `0x20` in `WriteEntityEngramLink`. |
| `0x24` | Vault  | `0x24 \| ws(8) \| hashA(8) \| hashB(8)` (25 B)               | `msgpack(coOccurrenceRecord)`| Co-occurrence counter. Canonical order: `hashA <= hashB` byte-by-byte so `(A,B) == (B,A)`. |

The `0x20` and `0x23` writes are committed atomically in a single Pebble batch
inside `WriteEntityEngramLink`. All writes use `pebble.NoSync` with the
`walSyncer` group-commit (durability window ≤10 ms; see
`storage/wal_syncer.go`).

---

## Relationship Types

The following types are defined in the `relTypeBytes` map in
`internal/storage/entity.go`. These are the canonical values. Any string not
in this map is stored with byte `0xFF`.

| Byte   | String           |
|--------|------------------|
| `0x01` | `manages`        |
| `0x02` | `uses`           |
| `0x03` | `depends_on`     |
| `0x04` | `implements`     |
| `0x05` | `created_by`     |
| `0x06` | `part_of`        |
| `0x07` | `causes`         |
| `0x08` | `contradicts`    |
| `0x09` | `supports`       |
| `0x0A` | `co_occurs_with` |
| `0x0B` | `caches_with`    |
| `0xFF` | _(unknown/unmapped fallback)_ |

The `relTypeByte` is embedded directly in the `0x21` key, so relationship type
is part of the key layout rather than only the value. Scanning a prefix that
includes the byte therefore filters by relationship type without decoding the
value.

---

## Entity Lifecycle

Each `EntityRecord` carries a `State` field. The valid states are enforced by
`UpsertEntityRecord` and cannot be set to an arbitrary string.

| State        | Meaning |
|--------------|---------|
| `active`     | Default state. Entity is current and in use. |
| `deprecated` | Entity is outdated but kept for historical reference. |
| `merged`     | Entity was merged into another. `MergedInto` field names the canonical entity. |
| `resolved`   | Entity conflict or ambiguity has been resolved (no replacement implied). |

`MergedInto` is only valid when `State == "merged"`. Setting `MergedInto` with
any other state causes `UpsertEntityRecord` to return an error.

---

## Confidence-Preserving Merge

When two calls to `UpsertEntityRecord` describe the same entity (same
normalized name), the upsert applies the following merge logic:

- **Confidence**: `max(existing.Confidence, new.Confidence)` — the higher
  confidence score is always kept.
- **State** and **MergedInto**: preserved from the existing record if the
  caller does not explicitly set them.
- **MentionCount**: incremented on every upsert.
- **FirstSeen**: set only on the first write; never overwritten.
- **UpdatedAt**: always set to `time.Now()`.
- All other fields (`Name`, `Type`, `Source`) are last-writer-wins.

This means repeated extraction of the same entity from different engrams or
vaults will accumulate mention counts and always converge to the highest
observed confidence, without clobbering lifecycle state set by a previous
caller.

---

## Concurrency Safety

Two independent mutex pools prevent TOCTOU (time-of-check/time-of-use) races:

- **`getEntityLock(name string) *sync.Mutex`** — returns a per-entity lock
  keyed by the normalized entity name. `UpsertEntityRecord` acquires this lock
  around the read-modify-write cycle for the `0x1F` record.
- **`getCoOccurrenceLock(hashA, hashB [8]byte) *sync.Mutex`** — returns a
  per-pair lock keyed by the canonical `(hashA, hashB)` pair.
  `IncrementEntityCoOccurrence` acquires this lock around the increment of the
  `0x24` counter.

Both pools use `sync.Map` with `LoadOrStore` for lock-free initialization.

---

## MCP Tools

### `muninn_find_by_entity`

Return all engrams in a vault that mention a named entity. Uses the `0x23`
reverse index for O(matches) lookup — it does not scan all engrams.

**Parameters:** `entity_name` (required), `vault`, `limit` (1–50, default 20).

```json
{
  "entity_name": "Redis",
  "vault": "default",
  "limit": 10
}
```

---

### `muninn_entity`

Returns the full aggregate view for a named entity: the `EntityRecord`
metadata, all engrams in the vault that mention it, its relationships, and its
co-occurring entities.

**Parameters:** `name` (required), `vault`, `limit` (max engrams to include,
default 20).

```json
{
  "name": "PostgreSQL",
  "vault": "default",
  "limit": 20
}
```

---

### `muninn_entities`

Lists all known entities in a vault sorted by mention count descending.
Optionally filter by lifecycle state.

**Parameters:** `vault`, `limit` (default 50), `state` (filter: `active`,
`deprecated`, `merged`, `resolved`).

```json
{
  "vault": "default",
  "state": "active",
  "limit": 50
}
```

---

### `muninn_entity_timeline`

Returns a chronological view of when an entity first appeared and how it
evolved. Shows all engrams mentioning the entity sorted by creation time
(oldest first).

**Parameters:** `entity_name` (required), `vault`, `limit` (1–50, default 10).

```json
{
  "entity_name": "Auth Service",
  "vault": "default",
  "limit": 10
}
```

---

### `muninn_entity_clusters`

Returns entity pairs that frequently co-occur in the same engrams. Reads the
`0x24` co-occurrence index directly — O(pairs) with no engram scanning.

**Parameters:** `vault`, `min_count` (minimum co-occurrence count, default 2),
`top_n` (max pairs returned sorted by count descending, default 20).

```json
{
  "vault": "default",
  "min_count": 3,
  "top_n": 20
}
```

---

### `muninn_similar_entities`

Finds entity name pairs in a vault that are likely duplicates based on trigram
similarity. Returns pairs with similarity at or above the threshold, sorted by
similarity descending. Use `muninn_merge_entity` to merge confirmed duplicates.

**Parameters:** `vault`, `threshold` (0.0–1.0, default 0.85), `top_n` (default
20).

```json
{
  "vault": "default",
  "threshold": 0.85,
  "top_n": 20
}
```

---

### `muninn_entity_state`

Sets the lifecycle state of a named entity. When setting `state=merged`,
`merged_into` must also be provided with the canonical entity name.

**Parameters:** `entity_name` (required), `state` (required: `active`,
`deprecated`, `merged`, `resolved`), `merged_into` (required when
`state=merged`), `vault`.

```json
{
  "entity_name": "Postgres",
  "state": "merged",
  "merged_into": "PostgreSQL",
  "vault": "default"
}
```

---

### `muninn_merge_entity`

Merges `entity_a` into `entity_b` (the canonical entity). Sets `entity_a`
state to `merged`, relinks all engrams in the vault from `entity_a` to
`entity_b`, and updates `entity_b` mention count. Use `dry_run=true` to
preview without writing.

**Parameters:** `entity_a` (required), `entity_b` (required), `vault`,
`dry_run` (default false).

```json
{
  "entity_a": "Postgres",
  "entity_b": "PostgreSQL",
  "vault": "default",
  "dry_run": true
}
```

After confirming with `dry_run=true`, run without `dry_run` to apply.

---

### `muninn_export_graph`

Exports the entity relationship graph for a vault as JSON-LD or GraphML. Nodes
are named entities; edges are typed entity-to-entity relationships extracted
from engrams. `ExportGraph` deduplicates relationships by max-weight per
`(from, rel_type, to)` triple when multiple engrams assert the same
relationship.

**Parameters:** `vault`, `format` (`json-ld` (default) or `graphml`),
`include_engrams` (when true, enriches entity types from the `EntityRecord`
table, default false).

```json
{
  "vault": "default",
  "format": "graphml",
  "include_engrams": true
}
```

---

## See Also

- [`engram.md`](engram.md) — engram structure, storage, and lifecycle
- [`retrieval-design.md`](retrieval-design.md) — how recall queries use entity indexes
- [`key-space-schema.md`](key-space-schema.md) — full key-space prefix table including `0x22` and all other prefixes
- [`plugins.md`](plugins.md) — enrichment pipeline and LLM-based entity extraction
- [`feature-reference.md`](feature-reference.md) — complete tool reference for all 35 MCP tools
