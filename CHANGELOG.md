# Changelog

All notable changes to MuninnDB are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

---

## [Unreleased]

### Fixed

- **An association edge can no longer outlive its endpoints** (#803). Hard
  deletes left the dead engram in the FTS and vector indexes, so the automatic
  association workers kept finding it and minting fresh edges to an ID that no
  longer existed — growing with use, and never reaped, because association decay
  does not read engram records. Both hard-delete callers now clean both search
  indexes, `DeleteEngram` cascades archived (0x25) edges in both directions, and
  every association writer refuses an edge whose endpoint has no engram record.
  The delete cascade also missed every edge at weight ≤ ~1/256 (and at the
  legacy full-weight key position) because of a scan-bound bug; those edges were
  unreapable, and the startup key-repair pass could promote one into a live
  dangling edge. Both are closed.

  **Client-visible change:** a write whose `associations[].target_id` names a
  memory that has been hard-deleted is now **rejected** instead of silently
  accepted. The row it used to create pointed at nothing. How the rejection
  surfaces is per-transport: REST's single-write path returns **400**; REST's
  batch endpoint still returns 201 with `status: "error"` on that item only, its
  siblings committing as before; gRPC and MBP return their existing error for an
  invalid ID. `relationships[]` is unchanged in this
  release — it still logs a warning and succeeds (#817).

---

## [0.10.0] - 2026-08-01

The trust release. Nine rounds of blind hands-on evaluation by AI agents drove
this cycle: each round's top complaint was fixed, adversarially reviewed, and
re-verified live by the evaluator that filed it.

### The cognitive substrate, made honest

- **Full-confidence learning restored** (#757): a key-encoding overflow had sent
  every weight-1.0 association (declared links, decide evidence, LTP-saturated
  learning) to the wrong key position since inception, where it read back as 0.
  "Strengthens with use" works for the first time. A startup repair (#759)
  relocates identifiable pre-fix keys, watermarked and gated so decay cannot
  destroy the evidence first.
- **Association decay is a real forgetting curve** (#766): decay was a per-pass
  multiplier on a 60-second tick (a 13.5-minute half-life) since February; every
  learned edge hit the floor within an hour of last use. Now a peak-anchored
  elapsed-time ceiling (default 30-day half-life, `assoc_half_life_days`),
  cadence-independent by construction, no on-disk format change. A legacy
  `assoc_decay_factor` in (0,1) is reinterpreted per-day with a one-time loud
  WARN; a factor of 1.0 or above skips loudly instead of silently enabling.
- **Contradictions stopped destroying data** (#747) and started mattering
  (#772): declaring a `contradicts` link now changes what recall returns — both
  sides demoted and annotated, a response-level `conflict` block, and every
  resolution path (evolve, forget(not_true_since), link(supersedes)) actually
  clears it, immediately, in recall and in the report. The shared worker's flush
  ticker was a debounce that starved detection (and the confidence penalty)
  under any active session; fixed.

### Recall that tells the truth

- **Version chains resolve to their head** (#767): a query phrased in a
  superseded fact's old wording returns the current version, attributed
  (`substituted_for`, `substitution_basis`), with fork/cycle refusal and loud
  truncation. Evolve now wakes the embed processor, closing a ~3-minute window
  where a fresh successor was semantically invisible.
- **A first-class relevance band** (#778): every recall row carries
  `relevance_band` (strong/moderate/weak/filter_match/uncalibrated) derived
  from the absolute score against the vault's own calibration — the per-query
  score renormalization can no longer dress a weak neighbor as certainty.
  `absolute_score` and `content_match` are now visible on the wire. The
  response-level "nothing strongly matched" hint deliberately did not ship: it
  failed its pre-committed acceptance rule (16.7% vs 70%), and that record is
  pinned in CI.
- **Calibrated abstention** (#715, #718, #754): recall can honestly abstain
  (`abstained`, `abstained_reason`) with an anisotropy-calibrated semantic
  floor, self-measured per model; embedding failure degrades loudly
  (`semantic_degraded`, #740). RRF vaults no longer return silently-empty
  default recall (#705); recall-mode presets no longer bypass the mode-aware
  threshold (#710); top-N ordering is deterministic (#699).
- **Currency advisories stopped lying** (#738, #758): the version-cluster
  advisory ships with a universal version-marker gate and declared-chain
  suppression — a deliberate near-silencing on vaults without version
  vocabulary, because a false "possibly superseded" on a live fact is worse
  than silence.

### The write path and the agent experience

- **The Push, increment 1** (#694): armed intentions with focal-cue notices
  over MCP, behind `MUNINN_PROSPECTIVE`.
- **Curator reflex in the on-connect surface** (#741) and evolve-first guidance
  (#723); six silent-substitution defects from hands-on evaluation fixed in one
  pass (#746); unrecognized memory types (#742) and link relations (#745) are
  never silently swallowed; the entity-type enum that cost 64 points of entity
  coverage is gone (#743); evolve records its real write verb in provenance
  (#739); optional inline entities on evolve (#680); importance dimension with
  pruning protection (#689); per-vault exclude-tags (#735).
- **muninn_remember names the vault it resolved to** when the caller omits one
  (#772 rider).

### Operations, safety, durability

- Consolidate no longer loses data under concurrent writes (#754). Stale PID
  files no longer break `muninn stop` (#650). Trigger events carry the full
  vault prefix (#697). Backup/import test hardening (#753), hermeticity
  doctrine for async assertions (#727), and four CI timing flakes fixed at the
  cause. Privacy: design records triaged with a public-repo naming rule
  enforced by a reviewer-level check (#775, #734).

### Known and named

- The recall gate's answerability ceiling is measured, in-tree, and honest:
  topically-adjacent unanswerable queries pass at high rates because they carry
  real evidence; no scalar gate fixes this (#757's labeled query set). The
  abstain-on-weak default both evaluators now ask for is the next increment's
  question, gated by that measurement.
- Pre-#766 decay damage is not retroactively undone: floored edges re-learn
  from the floor rather than being resurrected by fiat.
- Open, filed, and tracked: consolidation's embed-lag recall hole (#779), stale
  concept after evolve (#769), entity co-occurrence staleness after evolve
  (#780), the same-key relType replacement footgun (#771), CGDN's dead
  experimental path (#768).

---

## [0.9.0] - 2026-07-20

Adds an agent-oriented credential and multi-agent workflow layer (capability
tokens and self-provisioned workflow vaults), a `working` plasticity preset,
recall-event calibration ground truth, and a `muninn remember` CLI write verb,
alongside a keyspace-collision fix that ships a one-time on-disk migration.

> **Upgrade note — on-disk migration.** This release relocates the auth
> subsystem's internal Pebble key prefixes to resolve a latent collision with
> storage keys (#611). A one-time v3 migration rewrites existing admin-user,
> API-key, and vault-config records on the first start after upgrade. It is
> idempotent and crash-safe, but **back up your data directory before
> upgrading**, as with any storage-format change. No action is needed beyond
> starting the new binary.

### Added

- **Capability tokens (`cap_`) and agent-provisioned workflow vaults.** A new
  TTL-bound, MCP-only credential type plus a `muninn_create_workflow_vault` tool
  that lets an agent mint a scoped, expiring `wf-*` vault for a fleet.
  Capabilities are structurally non-recursive (a capability can never mint
  another) and the tool is opt-in (`MUNINN_AGENT_VAULT_CREATE`, default off).
  (#612, RFC #597)
- **Toolset profiles for `tools/list`.** A `core`/`full` switch via the
  `MUNINN_MCP_TOOLSET` env var or a per-connection `X-Muninn-Toolset` header
  trims the advertised tool set to cut per-session schema overhead. Advertisement
  only — every tool stays callable. Defaults to `full`, fails open on unknown
  values. (#604, implements #588)
- **`working` plasticity preset.** `default` cognition (Hebbian + PAS on) plus a
  7-day retention window, so a shared workflow vault auto-evaporates through the
  background pruner. (#599, RFC #597)
- **Recall-event sink.** The set of engrams surfaced by each recall is persisted
  as calibration ground truth, behind a purpose-gated read allowlist so it can
  never leak back into recall, replicated like every other write, and cleared
  with its vault. (#574, closes #573)
- **`muninn remember` CLI verb.** Store a memory against a running daemon over
  the REST surface, with `--content-file` for large or JSON-fragile payloads,
  authenticating from a `0600` key file. (#613, closes #610)
- **Stored memory type surfaced on reads.** `read`, `recall`, `where_left_off`,
  and `find_by_entity` now return the stored `memory_type` and `type_label`.
  (#616)
- **Search Scoring setting** in the management console. (#590)

### Changed

- **Auth Pebble key prefixes relocated (`0x11`–`0x14` → `0x42`–`0x45`),**
  resolving a latent collision with storage keys in the shared database (see the
  upgrade note above). Ships a v3 on-disk migration and a cross-package
  prefix-disjointness test so the collision class can't recur. (#618, closes
  #611)

### Fixed

- **Tag-scoped recall no longer silently misses engrams.** `tags_all` /
  `tags_any` were post-filters over the candidate pool; they now seed candidates
  from the tag index, so a tagged engram can no longer be structurally absent
  from a tag-scoped recall. (#619, closes #607)
- **Revoked or expired `mk_` API keys can no longer keep dispatching on an open
  MCP SSE session.** The cached session credential is re-validated on every POST,
  symmetric with the existing `cap_` re-validation. (#617, RFC #597; see
  Security)
- **Late HNSW inserts stay reachable.** A node born with zero in-edges could be
  permanently unfindable; the fresh back-edge is now protected during prune and
  small orphan sets are repaired on load. (#621, closes #620)
- **Explicit recall threshold preserved under RRF fusion** instead of being
  clobbered to the RRF floor. (#590)
- **`DeleteEngram` serialized with `CompareAndSet`'s per-engram stripe lock,**
  closing a race that could resurrect a just-deleted engram. (#594)

### Security

- **Revoked `mk_` MCP SSE sessions closed.** A revoked or expired API key could
  keep serving a long-lived SSE session; it is now re-validated on every POST
  before dispatch. (#617)

---

## [0.8.0] - 2026-07-09

### Added

- **Atomic compare-and-set + ownership lease on engrams.** A low-level CAS
  primitive (`PebbleStore.CompareAndSet`), layered into an advisory ownership
  lease (`Claim`/`Release`), giving a fleet of agents sharing a vault
  work-queue semantics — no two agents grab the same item. Closes a
  pre-existing lifecycle-state TOCTOU as a side effect. (#576, closes #548)
- **Fuzzy entity resolve behind exact `find_by_entity` lookup.** Exact match
  is tried first; fuzzy token-set-containment matching is the fallback, not
  the default, avoiding over-eager matches. (#571, #572, tightened in #581)
- **Vault dimension guard.** Each vault's embedding dimension is established
  atomically on first insert and enforced on every write path. On mismatch,
  recall degrades to BM25-only instead of silently mixing embedding spaces.
  (#589, closes #582)
- **User-supplied local ONNX embedding model.** New `embed_model_path` /
  `embed_tokenizer_path` config lets operators point at their own model
  instead of the bundled one; dimension is probed via real inference at
  init, never hardcoded. (#589, closes #583)
- **`vault plasticity` CLI command** to get/set per-vault plasticity
  settings. (#551)
- **`multi_user` vault setting** with shared-vault session-start guidance.
  (#575)
- **gRPC `ListVaults` and `BatchForget` RPCs**, with per-item vault
  resolution and auth requirements. (#557, #558)

### Fixed

- **Embed provider silent fallback eliminated.** An explicitly configured
  embed provider is never silently substituted for a different model. (#585)
- **BM25 fallback when the embed backend is unreachable**, with precise
  `-32602` error messages instead of an opaque failure. (#578)
- **Plasticity panel reset.** Advanced plasticity overrides no longer leak
  stale values into the save payload when the panel is collapsed. (#579)
- **CLI joined `exec:`/`logs:` tokens** now route to their handlers instead
  of being misparsed. (#580)
- **Idempotent write paths.** Idempotency keys are wired through gRPC and
  REST write paths and deduped within a batch. (#560, plus follow-up fix)
- **Entity scan dedup** by normalized identity instead of raw casing.
- **HNSW graph rebuild** from vectors when the restored structure is
  disconnected.
- **`vault create` registration** in the Pebble index, repairing a
  split-brain window in reindex-fts/export. (#547)
- **Google enrichment truncation.** The Google provider's `MaxOutputTokens`
  was raised from 4096 to 8192, and LLM parse failures are now counted in the
  stats, so long enrichments are no longer silently truncated.

### Security

- Bump Go toolchain to 1.26.5, fixing [GO-2026-5856](https://pkg.go.dev/vuln/GO-2026-5856)
  (Encrypted Client Hello privacy leak in `crypto/tls`), reachable via the gRPC
  server, MCP server, WAL recovery, `doctor` cert dial, and plugin transport.
  (#591)

> **Correction (see #602):** the originally-tagged 0.8.0 changelog listed an
> entity-boost recall-flood fix (#569) under Fixed. That fix did not make the
> 0.8.0 cut — #569 remains open and its PR (#570) is still in review — so the
> entry has been removed here. The fuzzy-entity work that did ship (#581) is
> covered under "Fuzzy entity resolve" above.

---

## [0.7.0] - 2026-06-12

The headline of this release is a complete overhaul of the cluster subsystem.
The Cortex/Lobe replication layer existed in previous releases but was not
reliably functional in real multi-node deployments. Every known correctness
issue has been addressed and Docker-validated end-to-end. High-availability
deployments are now production-ready.

### Added

- **Automatic failover.** When the Cortex goes down, Lobes detect SDOWN via
  gossip, accumulate votes, and the first node with quorum wins a jittered
  Raft-style election. Failover completes without operator intervention. (#532)
- **Returning-primary deference.** A restarted former Cortex probes the cluster
  before asserting leadership. If a failover leader is already in place, it
  defers, receives a snapshot, and follows — no split-brain, no data loss. (#537)
- **PeerHello discovery mesh.** Nodes with no join relationship (two primaries,
  sentinels) dial configured seeds and exchange authenticated `PeerHello` frames
  to establish identified connections, feeding MSP liveness and elections. (#530)
- **Equal-epoch tie-break.** When two primaries discover each other at the same
  epoch (split-brain bootstrap), the lower node-id keeps leadership and the
  other demotes cleanly. (#530)
- **Periodic quorum-loss self-demotion.** A Cortex that loses quorum demotes
  itself on a configurable interval. Recovery is automatic — once quorum is
  restored, it re-elects at a fresh epoch. Gated by a `hadQuorum` latch to
  prevent false demotion during bootstrap. (#527)
- **`muninn_evolve` concept rename.** The `concept` field is now an optional
  parameter. Omit to inherit the predecessor's label verbatim (existing
  behavior); supply a new string to rename it. Fixes the class of orientation
  bugs where a concept encoding mutable state ("answer owed", "PR blocked")
  could never be corrected without destroying the ULID lineage. (#483)
- **Server-side tag filters on recall.** `tags_all`, `tags_any`, and
  `tag_filter` (key-prefix + lexical bounds) are now first-class params on
  `muninn_recall`. Composes with semantic search and temporal filters. (#479)
- **SSE push re-evaluation on embed.** A `PushOnWrite` subscription that
  couldn't match at write time (vector score was 0, embedding not yet ready)
  is re-evaluated once the retroactive processor inserts the embedding. Fires
  exactly once; deduplicates against write-time deliveries. (#512)

### Fixed

- **Demotion zombie eliminated.** After demotion the Cortex goroutine previously
  parked at `<-ctx.Done()`, leaving the node in a half-leader state. Replaced
  with a supervisor state machine (`modeLeading / modeFollowing /
  modeWaitingQuorum`) that drives correct transitions. (#526)
- **Convergent failover election.** Stale connections from the old topology were
  not evicted after a failover, causing new join attempts to land on dead peers.
  Leader-gated joins, `ClearLeader` on ODOWN, and `EvictIfConn` on connection
  death bring the cluster to a stable single-leader state reliably. (#535, #536)
- **Lobe identity reconciliation on join.** A Lobe that reconnected under a
  different address was registered as a new member while the old entry persisted,
  inflating membership counts and breaking quorum calculations. (#524)
- **Voter count in `HandleVoteResponse`** — only registered voters counted,
  resolving phantom-voter quorum inflation. (#525)
- **Cluster topology in `/v1/cluster/info`** was reporting stale or incorrect
  member lists after topology changes. (#518)
- **Lobe replication stream.** A Lobe was closing the join connection
  immediately after handshake; the Cortex streams replication entries over the
  same connection, so the stream was immediately broken. (#515)
- **JoinRequest.Role HMAC coverage.** The join HMAC covered only `node_id`,
  leaving the `Role` field unauthenticated. Protocol v2 covers `nodeID + role`;
  v1 nodes remain accepted during rolling upgrades. (#539)

### Internal

- MSP `odownFired` latch prevents duplicate ODOWN callbacks per episode.
- `connKind` priority ordering (`kindJoin > kindHello > kindSeed`) prevents
  inferior connections from evicting established ones.
- TCP keepalive (15 s) set on all adopted peer connections.
- MBP protocol version bumped to 2.

---

## [0.6.3] - 2026-06-11

Hotfix release for issues found in production immediately after v0.6.2. Every
fix ships with a regression test.

### Fixed
- **Multi-phrase semantic recall returned 0 results.** For a `context` of 2+
  phrases, the query embedding was the flat N×dim concatenation of the
  per-phrase vectors, fed to the dim-sized HNSW index — so the cosine length
  guard zeroed every score. The query embedding is now mean-pooled into a single
  dim-sized vector. Single-phrase recall was unaffected. (#498)
- **`merge_entity` on case-variant names destroyed engram links.** Entity names
  are hashed case-insensitively, so merging "Foo" into "foo" made source and
  target the same key; the relink batch's Set+Delete then silently removed the
  link to both casings. Now guarded at both the merge and relink layers. (#503)
- **SSE push events were never delivered to SDK clients.** The server read
  `on_write` while all SDKs send `push_on_write`; the status recorder also
  lacked `Unwrap()`, killing SSE streams at the write deadline. Both fixed, plus
  the Python SDK now parses the nested `engram` push payload. (#437)
- **Inline-enriched memories were reported as un-enriched forever.**
  Caller-supplied summary/relationships/type now set the matching digest flags,
  so the enrichment-candidates query no longer returns them. (#500)
- **HNSW: a failed index load was cached as a permanently empty index.** The
  load is now retried on the next access; load outcomes are logged; the restore
  iterator is scoped to the vault. (#499)
- **`muninn_recall` serialization.** `content` no longer duplicates `summary`,
  lifecycle `state` is populated, and scores no longer carry float32 noise. (#502)
- **Local embedder mislabeled.** It was labeled `all-MiniLM-L6-v2` but is
  `bge-small-en-v1.5`; additionally the Windows release binary shipped MiniLM
  bytes under BGE pooling. Both corrected. (#455)
- **Failed LLM-enrichment init now surfaces the real error** instead of
  reporting only "Not configured". (#453)
- **Entity-type validation is now consistent** across `remember`,
  `entity_state`, and `apply_enrichment` (normalize + coerce on every
  user-facing path). (#501)

---

## [0.6.2] - 2026-06-10

### Security
- **Vault isolation on the binary transports** — MBP (8474) and gRPC (8477) now enforce the same fail-closed vault model as REST/MCP: a keyed session is pinned to its key's vault (cross-vault access rejected, even to a public vault), an unauthenticated session may reach only public vaults, and a missing auth store fails closed (#484).
- **LLM provider API keys masked** in the admin plugin-config API; a retyped key is saved, an untouched (masked) field is preserved (#488).
- **Installer checksum verification** — releases now publish `checksums.txt`; `install.sh` / `install.ps1` verify the downloaded binary and refuse to install on mismatch (#489).
- **Startup warning** when bound to a non-loopback address while the admin still has the default password (#490).

### Added
- TLS is now a first-class mode (epic #443): TLS setup in `muninn init` (#465), `muninn doctor` self-describes TLS state / bind addresses / cert details (#463), startup cert-expiry warning (#456), `docs/tls.md` + TLS-aware systemd unit (#466), Web UI host derived from the cert DNS SAN (#467).

### Changed
- Scheme-aware CLI URLs and clients throughout — printed URLs, generated AI-tool configs, and admin/vault HTTP clients honour `https` under TLS (#468, #469, #478); `muninn status` distinguishes a TLS trust failure from a dead server and no longer reads an all-cert-failure as "stopped" (#477, #481).
- `muninn.env` is loaded before every subcommand, so lifecycle/status commands share the daemon's config (#476).
- Activity chart buckets by the viewer's local calendar day (#458).
- `go` directive bumped to 1.26.4 to clear govulncheck stdlib advisories (#464).

### Fixed
- **HNSW graph integrity** — link-before-promote, distance-based neighbor pruning, vault-scoped index load, and back-edge persistence; repairs silent degradation of semantic recall to a single reachable cluster (#471, also resolves #462).
- `Evolve` no longer appends ` (evolved)` to the concept; lineage stays in the supersedes graph (#459).
- Renamed-vault correctness — bulk vault operations and FTS reindex resolve the stored workspace prefix instead of the SipHash of the current name (#454, #480).
- Consolidation dedup no longer mutates the cache-shared representative engram in place — was a data race against concurrent recalls (#492).
- Decay/recency scoring clamps clock skew — a future `LastAccess`/`CreatedAt` no longer pushes retention above 1 (#493).
- Memory detail panel "Created: Invalid Date" for search results (#461).

### Internal
- `storage.ErrNotFound` sentinel replaces `strings.Contains(err, "not found")` matching at the engine boundary (#491).
- De-flaked the WAL syncer timing tests (#486).

### Upgrade notes
- The HNSW fix (#471) repairs the indexing algorithm but not graphs already degraded on disk by the old defects. If semantic recall has been returning too few results, run one `muninn vault reembed <vault>` per affected vault on this build to rebuild a correct graph.

---

## [0.6.1] - 2026-05-26

### Fixed
- `fix(cluster)` — defer the `OnLobeJoined` callback until the `JoinResponse` + snapshot are fully on the wire, so the streamer no longer races the handshake and corrupts the lobe-side parser (#449, #448 Bug 1).
- `fix(cli)` — auto-detect TLS in `muninn status` / `muninn start` health probes (#444).

### Changed
- `feat(consolidation)` — the representative node absorbs the `AccessCount` of merged duplicates during dedup (#447).
- `feat(enrichment)` — Gemini 2.5 Flash added as a Google enrichment option and promoted to the default Google model (#450, #452).
- `chore(consolidation)` — dedup metadata-update errors are now surfaced in the consolidation report (#451).

---

## [0.6.0] - 2026-05-20

### Added
- **Audit logging** — structured audit trail with file, stdout, syslog, and webhook sinks; `audit tail/export/stats` CLI commands (#418).
- **Retrieval annotations** — staleness, conflict, and trust metadata on recall responses (#388).
- **MCP `initialize` instructions** response.

### Fixed
- `fix(fts)` — auto-restart worker goroutines after a panic; include the field byte in the BM25 posting key (multi-field terms were silently overwritten); scope the IDF cache per `(vault, term)` (#430).
- `fix(storage)` — vault deletion now clears all per-vault prefixes and entity-graph data and prunes orphaned global entity records (#435, #436, #438).
- `fix(cli)` — `muninn status` / `start` probes honour `MUNINNDB_{ADMIN,MCP,UI}_URL` (#439, #440).
- `fix(engine)` — content-hash dedup race, enrichment ghost-queue deadlock, trigger nil-metadata crash.
- `fix(auth)` — validate the Bearer token before parsing the body to prevent DoS amplification (#416).
- `fix(import)` — pipe deadlock and orphaned vault name on a failed import (#412).

### Security
- gRPC bumped to v1.79.3; govulncheck added to CI.

---

## [0.5.1] - 2026-05-06

### Fixed
- `fix(fts)` — auto-restart FTS worker goroutines after a panic (a panicked worker was never replaced, eventually making all new writes unsearchable until restart); include the field byte in the BM25 posting key; scope the IDF cache by `(vault, term)` (#430).

---

## [0.5.0] - 2026-04-27

### Added
- **Per-engram trust/taint labels** (#387) — `TrustLevel` (`verified`/`inferred`/`external`/`untrusted`) stored at a fixed ERF offset (zero-migration); all writes auto-stamp `inferred`; trust is visible in `muninn_read`/`muninn_recall`; new `muninn_trust` MCP tool; `ExcludeUntrusted` per-vault plasticity option.
- **Cursor pagination** for `muninn_get_enrichment_candidates` so large vaults no longer miss candidates (#362).

### Fixed
- `fix(engine)` — 400 for invalid inline association target IDs (#399).
- `fix(rest)` — 400 instead of 500 for invalid engram IDs in `/api/link` (#395).
- `fix(enrich)` — prevent infinite retry loops that deadlocked the circuit breaker (#390).
- `fix(trigger)` — guard against nil metadata in `sweepVault` / `handleCognitive` (#393).
- `fix(activation)` — restore the RRF score for BFS-traversed candidates in the ACT-R/CGDN paths.
- `fix(rest)` — delete phantom vaults that existed only in auth config.

### Internal
- `refactor(auth)` — extract `ParseBearerToken`, `ValidateStaticToken`, `IsValidVaultName` into the shared `internal/auth` package.

---

## [0.4.12-alpha] - 2026-04-06

### Fixed
- **MCP vault-isolation bypass** — `mk_` vault-scoped keys now enforce vault pinning in open-server mode (no static token); previously any MCP caller could reach any vault by naming it. Invalid/revoked `mk_` keys fail closed; SSE message-endpoint auth re-validation tightened (#368).

---

## [0.4.11-alpha] - 2026-04-05

### Added
- **Long-Term Potentiation (LTP)** — Hebbian associations strengthen over repeated co-activation; configurable via plasticity config.
- **Reciprocal Rank Fusion (RRF)** scoring strategy, selectable alongside ACT-R and Ebbinghaus.
- **Content-hash deduplication** at write time.
- **Agent-managed enrichment via MCP** — `muninn_get_enrichment_candidates` / `muninn_apply_enrichment`.
- **`X-Client-Name: MuninnDB`** header on outbound LLM (embed/enrich) requests.

### Fixed
- **Cluster join handshake (4 bugs)** — register the live `net.Conn` before responding; remove the epoch guard so a Cortex restart re-triggers election; accept both `secret` and `cluster_secret` JSON fields; honour `MUNINN_ADMIN_PASSWORD` at bootstrap.

---

## [0.4.10] - 2026-04-02

### Added
- Dashboard activity panel overhaul: selectable timeframe presets (7d–180d, capped at 180 days), end-date picker, dynamic x-axis tick grouping based on chart width, and a raw data table toggle with copy-to-clipboard. Includes loading, error, and empty-state feedback.
- `GET /api/activity-counts` endpoint returning per-day engram creation counts for a vault. Accepts `days` (1–180, default 7) and optional `until` (YYYY-MM-DD) query parameters. Malformed or out-of-range values return 400. Backed by an efficient ULID key-header scan with zero-filled contiguous day ranges.

### Changed
- Web UI: unified tab navigation across Memories, Graph, and Settings pages with a consistent bordered-tab style replacing the previous mix of underline, button, and pill patterns.
- Public vault unauthenticated access now runs in `full` mode. Previously, requests to an open vault with no API key ran as `observe`, silently preventing cognitive-state writes. Public vaults are now genuinely open — callers get `full` access unless they present an explicit `observe` key.

### Fixed
- Native `<select>` dropdowns unreadable in dark mode — `--bg-card` CSS variable was referenced but never defined; added it to both themes and added global select/option styling for proper dark/light rendering.
- Sidebar nav items are now scrollable when viewport height is too small, keeping the logo and footer pinned.
- Collapsed sidebar footer icons no longer overflow into the right border; icons render borderless when collapsed and bordered when expanded.
- "New Vault" action moved from sidebar footer into the vault picker modal to reclaim vertical space for nav items.
- Sidebar footer icons (theme toggle, keyboard shortcuts) replaced with consistent SVG icons matching the existing icon family.
- Version label merged into the footer icon row instead of occupying its own line.
- Sidebar footer padding and gaps tightened to maximize nav item visibility on short viewports.
- Memories page search-mode segmented control (Balanced/Semantic/Recent/Deep) now matches adjacent button height and font size, includes dividers between options, and preserves padding when Alpine.js re-renders dynamic styles.
- Enrich now accepts OpenAI-compatible JSON responses returned in `message.reasoning` when `message.content` is empty, including structured reasoning payloads.
- Retry and retroactive enrichment now only mark entity and relationship stages complete after successful persistence, avoiding partial-state retries, nil-result crashes, and silent graph-write failures.
- Entity and relationship response parsing now rejects nested wrapper keys like `meta.entities` / `meta.relationships` instead of treating them as valid empty results.
- Vault-scoped REST routes now resolve non-default vaults consistently from authenticated request bodies as well as `?vault=`, and reject mismatched query/body vaults.
- Vault-scoped REST routes are setup to deprecate vault passed in the body in a later release.
- REST read responses now include `memory_type: 0` for fact-classified memories instead of omitting the field.
- Observe-mode API keys now return `403` on semantically mutating REST and gRPC routes while preserving access to read-like POST endpoints such as activation, traversal, explanation, and batch link reads.
- ACT-R scoring: `bLevelCap` prevents base-level saturation in fresh vaults; two-pass per-query normalization ensures scores stay in [0, 1] range.
- Archived engrams (dream engine) now filtered at all retrieval points — query, find-by-entity, trigger worker sweeps.
- Dormant flag now gated on `!UseACTR`; in ACT-R mode the flag is derived from activation score rather than the Ebbinghaus relevance field.
- Web UI: form class consistency, segmented control hover state, uniform input/button sizing, memory filter bar density, page title branding, logs page full-width layout, observability view hash routing.
- SSE keepalive uses spec-compliant comment frame (`: keepalive`) to prevent proxy idle timeouts.
- Entity type allowlist expanded from 8 to 14 types; unknown types pass through without coercion.
- Clipboard API guarded by secure-context check with `execCommand` fallback for HTTP installs.
- Pebble `ErrNotFound` distinguished from other errors in embed migration path.

---

## [0.2.6] - 2026-02-28

### Added
- Native TLS support via `--tls-cert` and `--tls-key` flags on all 5 client-facing servers
- OpenAPI 3.0 spec served at `GET /api/openapi.yaml` (60+ routes documented)
- API key TTL — optional `expires` field on key creation (`"90d"`, `"1y"`, RFC3339)
- Query timeout enforcement — 30s activation deadline with BFS short-circuit (`MUNINN_ACTIVATE_TIMEOUT`)
- Automated backup scheduler (`--backup-interval`, `--backup-dir`, `--backup-retain`)
- Vault rename — metadata-only rename across storage, engine, REST, CLI, and Web UI
- Contradiction resolution — Keep A, Keep B, Merge, Dismiss actions in Web UI
- CLI: `muninn vault create`, `muninn api-key create|list|revoke`, `muninn admin change-password`
- Web UI: engram edit/evolve, new vault creation, manual link/association creation
- Web UI: vault export/import, FTS reindex, lifecycle state transitions
- Web UI: explain scores ("Why?" button), consolidate, record decision modals
- Web UI: memory filtering and sorting (created/accessed, tags, state, confidence, date range)
- Web UI: keyboard shortcuts (`/` search, `n` new, `?` help), tooltips, prev/next navigation
- Web UI: per-engram embedding status indicator, API key expiry column, backup trigger
- Graph: orphan node filtering, zoom controls (+/−/Fit)
- Observability tab in Web UI with live polling
- `GET /api/admin/observability` REST endpoint with full system snapshot
- Per-vault latency tracker with percentile reporting (p50/p95/p99)
- Vault-labeled Prometheus histograms for write/activate/read latency
- `vault reembed` command (CLI, REST, Web UI)
- CHANGELOG.md, encryption at rest documentation, CI OpenAPI spec validation
- PR template with release checklist, hookify drift detection rules
- Branch protection on main (PR + approval + CI) and develop (CI)
- Node SDK publish workflow (OIDC trusted publishing)
- Patent notice (U.S. Provisional Patent Application No. 63/991,402)

### Fixed
- ListEngrams now uses passive Pebble scan — no Hebbian side effects on browse
- Explain runs in observe mode — no cognitive mutations on "Why?" clicks
- Session click fetches full engram data + updates URL hash
- Atomic auth config rename (Pebble batch instead of separate Set+Delete)
- Sentinel error `ErrVaultNameCollision` replaces fragile string matching across clone/import/rename
- `parseKeyExpiry` rejects past dates at creation time
- Backup test data race (atomic counter for stubCheckpointer)
- Windowed average calculation in latency tracker
- Unconditional Prometheus metric recording and reembed vault response handling
- MCP vault default fix

---

## [0.2.5] - 2026-02-27

### Added
- `bge-small-en-v1.5` embedder support as an alternative to the default ONNX embedder
- Recall mode presets exposed in CLI, REST, and Web UI

### Fixed
- Arrow key navigation in the `init` wizard multi-select and single-select prompts

---

## [0.2.4] - 2026-02-26

### Added
- Hebbian edge pruning — low-weight associative edges are automatically pruned over time
- Activation snapshot isolation so snapshots cannot observe mid-propagation state
- Auto-sync of the PHP SDK to the `muninndb-php` repository on tag push (CI)

### Changed
- License switched to Business Source License (BSL) 1.1
- Added provisional patent notice

---

## [0.2.3] - 2026-02-26

### Added
- Node.js and PHP SDKs alongside the existing Python SDK
- Expanded REST API surface to support new SDK operations
- Server version displayed on the login screen and sidebar in the Web UI

### Fixed
- Temporal scoring accuracy and activation precision
- Stale `dist/` artifacts that blocked PyPI publish in CI
- Test mocks and temporal test thresholds updated for correctness

### Changed
- Added Apache 2.0 license, NOTICE file, and Contributor License Agreement (CLA)

---

## [0.2.2] - 2026-02-25

### Fixed
- Dashboard CSS 404 error on first load
- CLI `init` interactive prompts not rendering correctly

---

## [0.2.1] - 2026-02-25

### Fixed
- Windows binary missing from GitHub release archive
- PyPI auto-publish not triggering on tag push (CI)

---

## [0.2.0] - 2026-02-25

### Added
- Windows support — `install.ps1`, embedded ORT DLL, daemon lifecycle, and CI pipeline
- gRPC export transport
- REST backup and restore handler
- Replication coordinator and WAL improvements
- CLI `backup` / `restore` commands and vault authentication
- MCP server guided onboarding flow and Codex support
- Cohere, Google, Jina, and Mistral embedding provider plugins
- PAS (Passive-Active-Sleep) state transitions with checkpoints and migration
- Bundled ONNX embedder is always-on with async ready notification
- Default vault is public on first run; default `root` / `password` credentials auto-provisioned
- Vault export and import as `.muninn` archives (CLI, REST, engine)

### Changed
- Production hardening across storage, engine, and transport layers
- Improved engine lifecycle logging and error handling

### Fixed
- Data race in `tailLog` tests under the `-race` detector
- Vault dispatch tests that required a running server (now properly mocked)
- Flaky integration test for the temporal filter
- Windows CI smoke test failures

### Removed
- Internal eval harnesses and setup scripts

---

## [0.1.0] - 2026-02-23

### Added
- Initial public release of MuninnDB — the cognitive database
- Core memory engine with semantic write, activate, and recall operations
- Associative graph with Hebbian-inspired edge weighting
- Novelty detection with async worker pipeline
- Bundled ONNX sentence-embedding model (no external embedding service required)
- REST API server with vault-based multi-tenancy and JWT authentication
- MCP (Model Context Protocol) server for AI agent integration
- Web UI with dashboard and vault management
- Python SDK with optional LangChain `BaseMemory` integration
- CLI (`muninn init`, `muninn start`, `muninn stop`, and related commands)
- Homebrew tap and Docker image publishing via CI
- Race-detector-clean test suite with CLI integration tests

---

## Comparison Links

[Unreleased]: https://github.com/scrypster/muninndb/compare/v0.4.10...HEAD
[0.4.10]: https://github.com/scrypster/muninndb/compare/v0.4.9-alpha...v0.4.10
[0.2.6]: https://github.com/scrypster/muninndb/compare/v0.2.5...v0.2.6
[0.2.5]: https://github.com/scrypster/muninndb/compare/v0.2.4...v0.2.5
[0.2.4]: https://github.com/scrypster/muninndb/compare/v0.2.3...v0.2.4
[0.2.3]: https://github.com/scrypster/muninndb/compare/v0.2.2...v0.2.3
[0.2.2]: https://github.com/scrypster/muninndb/compare/v0.2.1...v0.2.2
[0.2.1]: https://github.com/scrypster/muninndb/compare/v0.2.0...v0.2.1
[0.2.0]: https://github.com/scrypster/muninndb/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/scrypster/muninndb/releases/tag/v0.1.0
