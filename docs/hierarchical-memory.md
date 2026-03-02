# Hierarchical Memory

MuninnDB supports tree-structured memory — outlines, task lists, project plans, and any nested
hierarchy where the order and parent-child relationships between nodes carry meaning. Each node
in the tree is a first-class engram with full cognitive properties (scoring, associations,
entity extraction, contradiction detection). The tree structure is represented through
`is_part_of` associations and a separate ordinal index that preserves sibling order.

---

## MCP Tools

| Tool | Purpose |
|------|---------|
| `muninn_remember_tree` | Write an entire hierarchy in one call. Returns `root_id` and a `node_map` (concept → ULID). |
| `muninn_recall_tree` | Retrieve the full ordered hierarchy rooted at a given ULID. |
| `muninn_add_child` | Append or insert a single child node to an existing parent. |

These three tools have no REST or gRPC equivalents; hierarchical memory is MCP-only.

---

## How It Works

### Storage model

When `muninn_remember_tree` is called, every node in the input becomes a separate engram record
(key prefix `0x01`/`0x02`). Sibling relationships are expressed in two ways:

1. **`is_part_of` association** — each child holds an association pointing to its parent with
   `RelIsPartOf` relation type, weight `1.0`, confidence `1.0`. This is stored using the
   standard association forward/reverse key space (`0x03`/`0x04`).

2. **Ordinal index** — a dedicated key at prefix `0x1E` encodes sibling order:
   ```
   Key layout: 0x1E | wsPrefix(8 bytes) | parentID(16 bytes) | childID(16 bytes)  = 41 bytes
   Value:      uint32 ordinal (big-endian, 4 bytes)
   ```
   Ordinals are non-negative `int32` values. The root node has no parent and no ordinal entry.
   Children are assigned ordinals starting at `1` and incrementing by 1 per sibling.

### Write path

`RememberTree` uses a two-phase write to keep the engine from partially applying large trees:

1. **Phase 1** — all engram records are written in a single atomic Pebble batch via
   `batch.Commit()`. If this fails, no engrams are written.
2. **Phase 2** — `is_part_of` associations and ordinal index entries are written per-node.
   These writes happen after the engram batch succeeds.

Input validation runs before any writes: the engine rejects a tree that exceeds `maxTreeDepth`
(constant: `20`, defined in `internal/engine/tree.go:61`), contains an empty concept, or
contains duplicate concept strings anywhere in the tree (including across separate branches).

---

## Recall Mechanics

`muninn_recall_tree` does a depth-first traversal starting at the requested root ULID:

```
RecallTree(vault, root_id, max_depth, limit, include_completed)
```

### Parameters

| Parameter | Default | Cap | Behavior |
|-----------|---------|-----|---------|
| `root_id` | — | — | Required. ULID of the root engram. |
| `max_depth` | `10` | `50` | Maximum recursion depth. `0` = unlimited. Negative values normalize to `0`. |
| `limit` | `0` | `1000` | Max children returned per node per level. `0` = no limit. |
| `include_completed` | `true` | — | When `false`, completed and soft-deleted children are filtered out. |

The `max_depth` cap of `50` is enforced in `internal/mcp/handlers.go:767-768`. The default of
`10` is set at the handler level when the caller omits the parameter.

### Ghost node handling

Ordinal index entries can outlive their corresponding engrams — for example, if a node was
hard-deleted after the ordinal was written, or if ordinal cleanup was missed during a crash.
When the traversal encounters an ordinal entry whose engram is absent from storage, it returns
`(nil, nil)` and silently skips that entry rather than returning an error or panicking. This
allows a partially-cleaned tree to still return all surviving nodes.

### Soft-delete and completed filtering

When `include_completed=false`, the traversal:
1. Fetches metadata for all children of the current node in a single batch call (avoids N+1
   queries).
2. Filters out any child whose state is `StateCompleted` or `StateSoftDeleted`.
3. Skips children whose metadata is missing entirely (treated as a ghost, same as hard-deleted).

The root node is always returned regardless of its state — the caller explicitly requested it by
ID. The `include_completed` flag applies only to child nodes.

---

## Incremental Updates (`muninn_add_child`)

To add a node to an existing tree without resending the whole structure:

```
muninn_add_child(vault, parent_id, concept, content, [type], [tags], [ordinal])
```

### Parent validation

Before writing, the engine reads the parent engram and rejects the call if the parent:
- Does not exist (`nil` from storage)
- Has state `StateSoftDeleted`
- Has state `StateCompleted`

A parent in `active` or `archived` state is accepted.

### Ordinal assignment

- **Explicit ordinal** (`ordinal` parameter provided): The caller specifies the position.
  The engram, association, and ordinal key are written atomically in a single Pebble batch.
- **Append mode** (`ordinal` omitted): The engine must read the current max ordinal and assign
  `max + 1`. To prevent concurrent appends from producing duplicate ordinals, the engine
  acquires a **per-parent mutex** (`getChildMutex(parentID)`) before the read-modify-write
  sequence. The mutex is keyed on the parent ULID string and stored in a `sync.Map` on the
  engine. The batch commit happens while the mutex is held; the mutex is released immediately
  after.

In both cases, all three writes (engram record, `is_part_of` association, ordinal key) are
committed atomically in a single Pebble batch.

---

## Limits

| Limit | Value | Source |
|-------|-------|--------|
| Write-time max tree depth | `maxTreeDepth = 20` | `internal/engine/tree.go:61` |
| Recall `max_depth` cap | `50` | `internal/mcp/handlers.go:767` |
| Recall `limit` cap (per-node children) | `1000` | `internal/mcp/handlers.go` |
| Concept must be non-empty | yes | validated before any writes |
| Duplicate concepts within one tree | rejected | validated across all branches |
| Ordinal values | non-negative `int32` | `WriteOrdinal` rejects negative values |

---

## Durability

Ordinal index writes (`WriteOrdinal`, `DeleteOrdinal`) use `pebble.NoSync` commits. They are
covered by the WAL syncer (`internal/storage/wal_syncer.go`), which calls
`db.LogData(nil, pebble.Sync)` every `walSyncInterval` (constant: `10ms`) to durably flush all
preceding `NoSync` writes. Maximum data loss window on a crash is 10 milliseconds — equivalent
to `MySQL innodb_flush_log_at_trx_commit=2`.

Engram records and `is_part_of` associations (written via `batch.Commit()` in the two-phase
path) use the engram batch durability tier, which defaults to `pebble.Sync` (immediate fsync)
unless the store is configured with `noSyncEngrams=true`.

The ordinal index is used only for child ordering; losing an ordinal entry does not destroy the
engram or its `is_part_of` association. The worst-case effect of an ordinal loss is that
`recall_tree` returns siblings in an unpredictable order or omits a ghost entry entirely.

---

## Examples

### Write a project outline

```json
{
  "tool": "muninn_remember_tree",
  "arguments": {
    "vault": "default",
    "root": {
      "concept": "Q3 Launch Plan",
      "content": "All tasks required for Q3 product launch.",
      "type": "goal",
      "tags": ["q3", "launch"],
      "children": [
        {
          "concept": "Backend work",
          "content": "API changes and database migrations.",
          "type": "task",
          "children": [
            {
              "concept": "Write migration scripts",
              "content": "Pebble schema migrations for new fields.",
              "type": "task"
            },
            {
              "concept": "Update REST endpoints",
              "content": "Add v2 endpoints for new payload shapes.",
              "type": "task"
            }
          ]
        },
        {
          "concept": "Frontend work",
          "content": "UI changes for new features.",
          "type": "task"
        }
      ]
    }
  }
}
```

Response:
```json
{
  "root_id": "01HXYZ...",
  "node_map": {
    "Q3 Launch Plan": "01HXYZ...",
    "Backend work": "01HABC...",
    "Write migration scripts": "01HDEF...",
    "Update REST endpoints": "01HGHI...",
    "Frontend work": "01HJKL..."
  }
}
```

### Recall the tree

```json
{
  "tool": "muninn_recall_tree",
  "arguments": {
    "vault": "default",
    "root_id": "01HXYZ...",
    "max_depth": 10,
    "include_completed": false
  }
}
```

Returns a nested `TreeNode` structure. Each node includes `id`, `concept`, `state`,
`last_accessed`, `ordinal` (sibling position), and `children` (ordered by ordinal ascending).
Completed and soft-deleted nodes are excluded when `include_completed=false`.

### Add a child to an existing node

```json
{
  "tool": "muninn_add_child",
  "arguments": {
    "vault": "default",
    "parent_id": "01HABC...",
    "concept": "Write OpenAPI spec",
    "content": "Document all v2 endpoints in openapi.yaml.",
    "type": "task",
    "tags": ["api", "docs"]
  }
}
```

Ordinal is omitted, so the engine appends the child at `max_existing_ordinal + 1` under a
per-parent mutex. Returns `{ "child_id": "...", "ordinal": 3 }`.

---

## REST Equivalents

There are no REST or gRPC equivalents for `muninn_remember_tree`, `muninn_recall_tree`, or
`muninn_add_child`. Tree operations are available through the MCP interface only.

---

## See Also

- [engram.md](engram.md) — engram structure, lifecycle states, memory types
- [durability-guarantees.md](durability-guarantees.md) — Sync vs NoSync tiers, WAL syncer
- [feature-reference.md](feature-reference.md) — complete tool reference
- [key-space-schema.md](key-space-schema.md) — `0x1E` ordinal key layout and other key prefixes
