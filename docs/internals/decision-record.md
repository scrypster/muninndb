# Decision record

Why MuninnDB is the way it is. Each entry: the decision, its rationale, and the reusable
**principle** it established. Cite these when reviewing so contributors see the project has
a spine, not just opinions. Sourced from merged PRs, closed issues, and the CHANGELOG.

---

## Cognition & memory model

**Explicit config is never silently substituted (#582 → #585/#589).** A configured embed
provider that failed at boot used to silently substitute the bundled 384-dim model,
splitting the vault into mutually-invisible embedding spaces while reporting
`health: "good"`. Fixed: never substitute — fail, or degrade to a safe noop mode; #589
added a vault dimension guard. **Principle: silent, plausible-looking wrong answers are
the worst failure class. Honor explicit config or fail loudly.**

**Degrade loudly-but-gracefully (#577 → #578).** When the embed backend is unreachable,
recall degrades to FTS/BM25-only with a WARN rather than hard-failing. Paired with #582
this is the house degradation doctrine. **Principle: same-model degraded results are fine;
different-model or silently-empty results never are.**

**Tag filters must not silently miss (#607, green-lit).** `tags_all`/`tags_any` are
post-filters over the phase-2 candidate pool, and the maintained 0x0C tag index has zero
readers, so tag-scoped recall is non-deterministic. The chosen fix injects candidates from
the tag index — **explicitly reusing the precedent that `created_after/before` filters
already set** (`EngramIDsByCreatedRange` RRF-fused into phase 2). Maintainer pre-scoped two
traps (ACT-R scores injected candidates near zero; per-tag scan limits truncating before
intersection). **Principle: silent violation of a documented guarantee is high-severity;
extend proven in-tree mechanisms over new architecture; pre-scope the traps before the
contributor starts.**

**Entity boost was tightened, not removed (#569 → #581).** Ubiquitous entities flooded
recall via uncapped, threshold-bypassing injection. Fix: rarity-weight the boost, cap
accumulation, gate on threshold, keep the factor (0.15) below typical BFS weights.
**Principle: fix the flood with a cap and a gate; don't amputate the feature.**

**RRF preserves explicit thresholds (#590).** RRF fusion was clobbering a caller's explicit
threshold; the fix distinguishes "caller set a threshold" from "engine default applied."
**Principle: an explicit caller value survives internal re-scoring.**

**HNSW integrity is rebuilt from source, never trusted as-is (#471, #544/#545).** Four
graph defects silently degraded search to one reachable cluster. Established: a structurally
broken index is rebuilt from source-of-truth vectors on load (bfsReachable check), never
restored as-is. **Principle: a corrupt index is a rebuild trigger, not a served result.**

**Recall side-effects are an open design question (#598, open).** Recall mutates state
(co-activation bonds the result set, cache reads refresh recency, max-normalization turns
boosts into displacement). A read-only recall flag is proposed but not settled. **Treat
"read-only recall" as unresolved; don't ship changes that assume it exists.**

## Security & credentials

**Security properties are structural, not policy-checked (#612).** The obvious design
(mint a full-mode `mk_` key for a new workflow vault) was killed by the author's own
32-agent red-team: recursive credential minting + immortal orphan keys. Replaced with a
distinct `cap_` credential type — TTL-mandatory, structurally non-recursive (`cap_`
authenticates as `IsCapability`, never `IsAPIKey`, so it can't satisfy the mint gate),
`wf-*` namespaced, MCP-transport-only, opt-in default-off. A second pre-PR red-team caught
a cross-vault clobber and an SSE confused-deputy. **Principles: make the bad state
unrepresentable, not merely forbidden; self-red-team before review; document residual
risks, don't bury them.**

**Discovery filtering is not access control (#588 → #604).** #588 posed the mechanism
question (per-key attribute vs list parameter vs hybrid) and explicitly waited to align
before implementing. The maintainer settled it: the key layer is a security gate, the wrong
home for a presentation preference; a request header wins because clients attach headers at
connect while `tools/list` takes no params; filtering is advertisement-only (dispatch never
consults it) and fails **open** on unknown values ("a typo must never hide a client's
memory tools"). **Principles: align on mechanism before code when it touches the key model
or transport; fail open on presentation, fail closed on auth.**

**Match enforcement strength to the trust model (#548 → #576).** Work-queue semantics
(`compare_and_set` + an engram lease) for multi-agent shared memory. The lease is
deliberately **advisory** — it matches the cooperative-agent posture; fencing tokens were
deferred to v2. Staleness is a pure server-clock function at read time (no background
reaper). **Principle: don't over-build enforcement the threat model doesn't need; state the
residual explicitly.**

**Keyspace collisions get a migration, not an assertion (#611, green-lit).** auth and
storage both claim Pebble prefixes 0x11–0x14 in the shared DB. The reporter rated it "low";
independent adversarial analysis found the severity **understated** — a live admin-existence
false-positive, a flagged-engram miscount, and an O(all-associations) startup scan. Chosen:
full relocation + one-time migration ("we're still alpha, migration cost is as low as it'll
ever be") plus a cross-package disjointness test — not an assertion-only fix. **Principles:
verify reporter claims independently (severity may rise); fix the disk, not just the future;
guard the class of bug with a test.**

## Delivery & process

**Features land as minimal increments referencing their RFC (#597 → #599, #612, #617).**
The shared-working-vault RFC is grounded in verified code reading ("~90% already exists")
and delivered as numbered increments, each small and reviewable, with "out of scope"
tracked back on the RFC thread. **Principle: no sprawling PRs; each increment cites its RFC
and names what it defers.**

**Derived config is pinned by invariant tests (#599).** `working` = `default` + exactly
two deltas, pinned by a `reflect.DeepEqual` test so a future edit to `default` can't drift
it. `MultiUser` stayed a separate per-vault override — presets tune cognition, social flags
stay orthogonal. **Principle: pin derived truth with a test; keep orthogonal knobs
orthogonal.**

**A write verb rides the key trust boundary (#610, green-lit).** The proposed
`muninn remember --content-file` CLI client authenticates via the existing token-file
convention (`~/.muninn/mcp.token`, 0600) — the same trust boundary as other key-authed
clients — not the admin-session `-u/-p` pattern. **Principle: conventions beat new
mechanisms; a write verb uses the write trust boundary.**

**Negative results are first-class (#609, closed by author).** "carry," a sidecar salience
ledger built on MuninnDB, was offered upstream headlined by a negative result: 523
ambient-push deliveries, zero uptake — which killed ambient push, while explicitly saying
nothing about the pull-path decay/Hebbian primitives. Valued for its eligibility contract,
its "informed latest-wins" reversal-friction rule, and its scope discipline ("the store
stays the store; the caring travels separately"). **Principle: honest negative results and
tight scope are contributions, not disappointments.**

**Green-light with required changes (#608, the canonical example).** Principal-scoped keys
were accepted in principle with five numbered required changes: anchored/delimiter-respecting
glob matching, reserved-namespace (`wf-*`) exclusion, a real global key index so pattern
keys are listable/revocable, live matching for observe-only, and TTL-mandatory per the #612
precedent — plus an honest re-centering note (the strongest case is observe-only sweep keys).
The contributor accepted all five. **Principle: never a bare no; enumerate what holds, then
numbered requirements, then re-affirm the invite.**

---

## Direction (where the product is heading)

**Active / green-lit awaiting PRs:** RFC #597 remaining increments (#617: mk_ SSE
re-validation #615, coverage #614, `session.go` removal; then single-batch CAS atomicity,
TTL-driven cap reclamation, SSE auth dedup); #607 tag-index candidate injection; #610
`muninn remember` CLI verb; #611 prefix relocation + migration; #608 observe-only
sweep keys (five required changes agreed).

**Open RFCs / undirected:** #376 cross-vault recall (read-side complement to #597 — "fuse
reads vs share writes"); #598 read-only recall (+ #569/#573 scoring calibration); #605/#606
observability (embed-failure signal, BM25-fallback metrics); #596 replication divergence +
config replication; #600 upgrade integrity (checksum verification); recurring recall-quality
integrity reports.

**Explicitly deferred / rejected — do not reopen without new evidence:** write-path lease
enforcement, fencing tokens, CAS crash-atomicity (until a workflow demands them); write-mode
pattern keys (dropped in #608 v2); per-key toolset attribute (the key layer is not a
presentation layer, #604); ambient push (negative result, #609).

**`tag_prefix` seeds candidates via a new ordered index, not the hash index (S1).**
Superseded the earlier "stays a post-filter" call above: the 0x0C tag index is
hash-indexed and cannot range-scan, so `tag_prefix` (e.g. `due:<=today`) could only be
checked post-hoc in phase 6, after other indices had already decided the candidate pool —
exactly the #607 failure mode, but for range filters instead of exact ones. S1 adds 0x2B,
a SEPARATE index keyed on `Hash(tagKey)` with the raw tag VALUE sorted after it, so
`lte`/`gte`/`lt`/`gt`/`eq` become real bounded Pebble range scans that seed phase-2
candidates (`ActivationEngine.seedTagCandidates` / `storage.ScanRawTagRange`); phase 6's
`passesMetaFilter` remains the exactness gate. See `docs/internals/keyspace-registry.md`
0x2B for the key layout. **Principle: "stays a post-filter" is a permanent verdict only
until the seeding mechanism it was rejected for becomes cheap to build — re-litigate
when the cost equation changes, don't let an old call block a index built for it.**

### Calibration is per-vault, self-derived, never hardcoded from a sample vault (semantic floor / #712, 2026-07-29)

Reliable-colleague work surfaced a recurring trap: tuning a threshold/baseline/vocabulary on the
maintainer's own vault and shipping that constant as the universal default. #712 currency
(version-cluster) failed this twice — v1's entity anchor didn't fire on the entity-sparse real
vault, and v2's tag-marker vocabulary (`four-bucket`, `v2`, `final`) + document-frequency
thresholds were tuned to one vault's pricing history (and still false-positived on it, telling a
shipped fact it was superseded by an aspirational "planned" one). The semantic-abstention floor's
`b = μ+2σ` was derived from that vault's embedding anisotropy — a value that fits bge-small on that
corpus but misfloors a different vault or model. The maintainer's framing: *"you can calibrate my
vault to get better results, everyone can calibrate their own vault, but we shouldn't be defining
the calibration for others."* A maintainer/sample vault is for FINDING bugs and VALIDATING that a
feature survives messy real data — never for baking a constant into the product; a feature that
shines on the sample vault and does nothing (or misfires) elsewhere is a failure even when the
sample passes. **Principle (CLAUDE.md #11): a feature that needs a number derives it from each
vault's OWN data (self-calibrating — #711 weights IDF from the vault's own corpus; the semantic
floor should self-measure each vault's anisotropy baseline over its own embeddings) or exposes a
per-vault override; model/cold-start defaults are hints, never fixed law. Ship mechanisms and
hints, not other people's answers.**
