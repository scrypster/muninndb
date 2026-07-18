# Hard invariants

Testable assertions a PR must never violate. Anchors are on `develop`. When an anchor's
line number drifts, the symbol name is the source of truth — re-locate by grep, don't
trust the number. If the live code disagrees with an assertion here, the code wins: flag
the stale invariant, don't enforce it.

Format: **[INV-n]** assertion — `file:anchor` — *why it matters / what breaks if violated.*

---

## Cognition invariants

- **[COG-1]** Unknown preset name falls back to `default`, never errors — `internal/auth/plasticity.go` `ResolvePlasticity`. *Config can never wedge the engine.*
- **[COG-2]** All plasticity overrides are clamped, never rejected (HopDepth 0–8, weights 0–1, ACTRDecay 0.01–2.0, HebScale 0–50, PASMax 0–10) — `plasticity.go`. *Out-of-range config is impossible, not merely discouraged.*
- **[COG-3]** The `working` preset equals `default` plus exactly `{RetentionDays:7, BehaviorMode:selective}` — pinned by `TestResolvePlasticity_WorkingEqualsDefaultPlusTwoDeltas` (`plasticity_test.go`). *Editing `default` must not silently drift `working`.*
- **[COG-4]** All presets share `RecallMode="balanced"`, `ArchiveThreshold=0.05`, `InlineEnrichment="caller_preferred"`, `EnrichmentEnabled=true`, `MaxEngrams=0`, `ScoringFusion=""` — `plasticity.go` preset table + the preset-invariant test suites. *These are cross-preset guarantees other code relies on.*
- **[COG-5]** The ACT-R content gate is `contentMatch = w.SemanticSimilarity*vectorScore + w.FullTextRelevance*normalizedFTS`, where `w` is the **resolved** weights (`resolvedWeights` — from the request/plasticity, falling back to `DefaultWeights{Semantic:0.35, FullText:0.25}` only when `req.Weights == nil`) — `internal/engine/activation/engine.go` `computeACTR` + `resolveWeights`. *The content gate is weight-driven, not a fixed constant. When no weights are supplied, the 0.35/0.25 defaults apply and unused components' budget is redistributed (the `1/0.65` scale). A change to `DefaultWeights` or to the resolve/redistribute logic reshapes scoring for every request that doesn't pass explicit weights — verify against the live `resolveWeights` before asserting what the effective weights are.*
- **[COG-6]** RRF mode forces threshold to 0.001 when the caller threshold ≥ 0.01 — `activation/engine.go` (RRF scores live in [0,0.05]). *Preserves #590's fix: an explicit caller threshold under RRF is respected, not clobbered.*
- **[COG-7]** ACT-R base-level is capped at ≈1.489 (ln(e^1.693−1)); Hebbian/transition boosts may exceed it, base-level alone may not — `activation/engine.go`. *"threshold=0.3 means the same thing in a fresh and a mature vault."*
- **[COG-8]** In RRF mode, BFS-traversed candidates must carry a non-zero rrfScore or they vanish from results — `activation/engine.go`. *Traversal reachability must not silently drop.*
- **[COG-9]** Soft-deleted/archived engrams are always filtered in phase-6 scoring and in entity boost — `activation/engine.go`, `engine_entity_boost.go`. *HNSW has no delete; the filter is the only guard.*
- **[COG-10]** Co-activation (recall-time Hebbian) never updates confidence — only user feedback and contradictions do — `engine.go`. *"Evidence of relevance, not evidence of truth."*
- **[COG-11]** Observe mode (`ReadOnly`) skips activation-log, Hebbian, and PAS writes — `engine.go`, `activation/engine.go`. *A read must not mutate learning state.* (Known wound: archive-restore in phase-4.75 still writes even for observe — see soft spots.)
- **[COG-12]** `Recall`/`Activate` never bumps engram AccessCount — only explicit `RecordAccess` and content-dedup reinforcement do — `engine.go`. *Recall is not an access for decay purposes.*
- **[COG-13]** Idempotency check precedes content-hash dedup on the write path — `engine.go`. *Re-submitting a known op_id with drifted content returns the original, deliberately.*
- **[COG-14]** Caller-supplied embedding ⇒ `DigestEmbed` flag set + inline HNSW insert; the retroactive embedder must never overwrite it — `engine.go`. *Don't re-embed what the caller already embedded.*
- **[COG-15]** The pruner only ever **hard-deletes**, and runs only for vaults with `MaxEngrams>0` or `RetentionDays>0` — `engine.go` PruneVault / runPruneWorker. *Soft-delete would leave relevance-index entries → infinite prune loop.*
- **[COG-16]** Association decay requires `HebbianEnabled && AssocDecayFactor>0` — `engine.go`. *Decay of learned edges is gated on learning being on.*
- **[COG-17]** Hebbian weight updates are log-space multiplicative, capped at 1.0, seeded at 0.01 — `internal/cognitive/hebbian.go`. *Prevents overflow and runaway weights.*
- **[COG-18]** Entity-boost factor (0.15) stays below typical BFS weights, seeds TopN=5, and results are re-truncated to MaxResults after boosting — `engine_entity_boost.go`, `engine.go` (tightened in #581). *Ubiquitous entities must not flood recall (#569).*

## Storage & durability invariants

- **[STO-1]** A new Pebble prefix must be disjoint from the keyspace registry and documented where the registry lives; the disjointness cross-check must cover it. On current `develop` that means `keys/keys.go` doc comments + bumping `storageMaxPrefix` in `keys_test.go` if ≥ 0x2B; **once PR #618 (#611) merges** it means adding the prefix to `internal/prefix/prefix.All()` (the test auto-tightens, no constant to bump). Check `ls internal/prefix/` to see which world you're in. — see `keyspace-registry.md`.
- **[STO-2]** Any path mutating engram **state or lease** must hold `casLocks.For(id)` across read→commit, as `CompareAndSet` (`storage/lease.go`) and `DeleteEngram` (`storage/engram.go`) do. *A new unlocked state-mutating path reopens the #594 resurrection race.*
- **[STO-3]** CAS state changes go through `UpdateMetadata` (keeps 0x0B index, caches, provenance, replication consistent) — never a direct 0x02 write — `storage/lease.go`.
- **[STO-4]** The engram lease (0x2A) is **advisory**: recall/claim paths consult it; normal writes must not start consulting it without an RFC — `storage/lease.go`, `engine/engine_lease.go`.
- **[STO-5]** `DeleteEngram` deletes association keys from **live Pebble scans**, not the engram's inline ERF list (the Hebbian worker may have moved weights), and must include the 0x2A LeaseKey in its batch — `storage/engram.go`.
- **[STO-6]** `ClearVault` ordering is load-bearing: point-delete 0x15 count → evict in-memory counter → drain coalescer → drain provenance → tombstone `DeleteRange`. Any new vault-scoped prefix MUST be added to its `dataPrefixes` list — `storage/vault_lifecycle.go`.
- **[STO-7]** MOL WAL recovery hard-fails on CRC mismatch, never skip-and-continue; replay functions must stay idempotent (last-seq is written NoSync) — `internal/wal/mol.go` `Recover`.
- **[STO-8]** HNSW: entry-point promotion happens only after the node is linked; `LoadFromPebble` validates entry-point reachability via `bfsReachable` and rebuilds if disconnected; `Registry.Insert` rolls back the orphan vector if graph insertion fails — `internal/index/hnsw/` (guards PR #471's four defects).
- **[STO-9]** Vault dimension: all insert/search paths go through `CheckDim`; a failed graph load sets `loadErr` and refuses inserts (never reports dim 0 = "empty vault" and recreates a split embedding space) — `internal/index/hnsw/registry.go` (#582/#589).
- **[STO-10]** Idempotency receipts give **no** concurrency guarantee — `WriteIdempotency`/`CheckIdempotency` are unlocked Get/Set. A caller needing exactly-once under concurrency must add its own lock. A PR claiming idempotency is race-safe is wrong.

## Security & transport invariants

- **[SEC-1]** A `cap_` token must never set `IsAPIKey` — exactly one of `IsAPIKey`/`IsCapability` is set — `internal/mcp/context.go`, `internal/mcp/types.go`. *This is what makes the recursion guard structural.*
- **[SEC-2]** Any privileged MCP tool (one that mints/grants/creates) gates on `IsAPIKey && Mode==ModeFull && <opt-in env>` before dispatch — `internal/mcp/server.go` (`muninn_create_workflow_vault` is the template). *New privileged tools must copy this triple gate.*
- **[SEC-3]** `ValidateAPIKey` accepts only the `mk_` prefix; REST/gRPC/MBP call **only** `ValidateAPIKey`, never `ValidateCapability` — `internal/auth/keys_store.go`, `middleware.go`, `grpc/server.go`, `mbp/server.go`. *Keeps cap_ MCP-only, structurally.*
- **[SEC-4]** Capabilities carry a non-nil TTL at both mint (`GenerateCapability` rejects nil) and validate (nil/past expiry = expired) — `internal/auth/capability_store.go`. *No immortal credentials.*
- **[SEC-5]** Invalid `mk_`/`cap_` on MCP fails closed — never falls through to static-token or open-server auth — `internal/mcp/context.go`.
- **[SEC-6]** Every registered MCP tool is in exactly one of `isMutatingTool`/`isReadOnlyTool`; unknown tools blocked for observe AND write modes; unknown key modes rejected — `internal/mcp/context.go`, `server.go` (enforced by `TestToolClassification_CoversAllRegisteredHandlers`). *A new tool with no classification silently escapes mode enforcement.*
- **[SEC-7]** A vault-pinned credential rejects any differing `vault` argument and never echoes the pinned name — `internal/mcp/context.go` (also REST/gRPC/MBP). *No cross-vault action, no name leak.*
- **[SEC-8]** Unconfigured vaults are locked (fail-closed, `Public:false`) on REST/gRPC/MBP — `internal/auth/vault_config.go`.
- **[SEC-9]** Toolset filtering is **advertisement-only**; dispatch never consults it; unknown values fail **open to full** — `internal/mcp/toolset.go`. *Presentation preference, not a security boundary.*
- **[SEC-10]** SSE per-POST: a cached `cap_` session credential is re-validated before dispatch — `internal/mcp/server.go`. *Closes the #612 confused-deputy.* **Known open gap: the symmetric `mk_` re-validation does NOT exist (#615/#617) — a revoked mk_ SSE session can survive. A PR touching the SSE POST path should fix or at least not widen this.**
- **[SEC-11]** Vault names are `[a-z0-9_-]{1,64}` everywhere via `IsValidVaultName` — `internal/auth/token.go`.
- **[SEC-12]** Static-token compares are constant-time with a length cap; keys/caps are looked up by SHA-256(secret) — raw secrets are never stored — `internal/auth/token.go`, `keys_store.go`, `capability_store.go`.
- **[SEC-13]** Any new client write path in a cluster deployment must either gate on `IsLeader()`/Cortex-origin or replicate its mutation — no transport does this today, which is exactly bug #596. Background workers that mutate replicated state (e.g. the pruner) inherit the same obligation.

---

## Known open wounds

Several of these invariants sit next to known-imperfect code. The individual issues are
tracked publicly (#596, #605, #607, #611, #615/#617, …); a reviewer should recognize when a
PR is touching one and either fix or at least not widen it. The consolidated map of soft
spots and trust-surface weaknesses is kept in the maintainer-private tier
(`.claude/maintainer/soft-spots.md`) rather than concentrated here — cite the individual
issue, not a one-stop list.
