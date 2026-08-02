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

**Associative surprise: killed after four measured passes (2026-07-31).** The pitch was the
product's most-wanted feature: unprompted, surface a non-obvious cross-session connection the
calling LLM could not have made itself. Four independent attempts to make it fire, each one
measured on real data, and the mechanism is refuted **at its premise** rather than at its tuning.

- **Pass 1 — does it fire?** Over a real corpus: 1,525 focal engrams, 37,296 candidate edges,
  focality assumed always-true (the most generous possible assumption). **0 fires.** Removing the
  type gate entirely: still 0.
- **Pass 2 — is the anti-cosplay null sound?** No. The degree-preserving configuration-model null
  reduces to a fixed threshold on `coact*idf` and is blind to the candidate's own degree. With
  degree isolated as the only variable, hub-vs-genuine separation was +0.003 at n=74 and **-0.007
  at n=809** (sign flips), arms distinguished on **0/50 seeds**, and it *prefers* the popularity
  hub when the hub carries more weight. Its p-value also resolves to ~3 distinct values regardless
  of N. The null was inverted, not merely weak.
- **Pass 3 — is it substrate-starved?** No. A counterfactual on two identical clones (types and
  entities re-derived offline by a local model on the treatment arm; co-activation edges
  byte-identical across arms) lifted the graph-shaped substrate ~5x — JOINT typed-and-entity
  coverage 2.49% -> 12.29%, entity coverage 29.9% -> 98.6% — and produced **0 fires in BOTH arms**
  on the same 27,255 edges. Gate rejections *redistributed* rather than cleared (`type` 59.0% ->
  72.4%, `not-focal` 38.6% -> 12.2%): enrichment pushed candidates past focality straight into the
  next gate.
- **Pass 4 — is it over-gated?** Partly, but that is not the cause either. All 32 deterministic
  gate subsets were run over 37,169 candidate edges. **Four of the six gates are redundant** —
  `type`, `idf-floor`, `focal` and `valid` each reject *nothing* that another gate has not already
  rejected, and `valid` never fires at all. The earlier "the type gate absorbs everything"
  attribution was an artifact of evaluation ORDER.

**The actual refutation.** `non-obvious` is the only load-bearing gate, and it rejects **100.00%**
of 14,306 bridge-carrying candidates: 9.1% because the candidate is already in the recall set,
90.9% because its cosine to the recall set is at or above the ceiling. The distribution leaves no
room — min 0.500, p50 0.763, p95 0.877. Not one candidate in 14,306 sits below it.

The reason is structural: **co-activation edges form between memories recalled together, so a
focal engram's co-activation neighbourhood IS its semantic neighbourhood.** The feature's premise —
"a connection recall structurally could not have made" — is contradicted by its own substrate.
There is no such thing as a co-activation neighbour recall could not have reached. A tau sweep
confirms the shape: every survivor bought by raising the ceiling is a near-duplicate of what recall
already returned (the 152 candidates blocked only by `non-obvious` have median cosine 0.722 and
max 1.000 — restatements, not surprises).

**Principle: a mechanism can be refuted at its premise, and that is cheaper to learn than tuning
it.** Three of the four passes were spent improving the machinery — a better null, a better
substrate, fewer gates — when the disqualifying fact was available from the candidate-cosine
distribution alone. Ask what the mechanism assumes about its own inputs before optimising it.

**Also recorded (2026-07-31): the substrate hypothesis was too strong.** "Fix capture and the graph
features will fire" was stated confidently and is refuted by pass 3. Capture quality remains worth
fixing on its own merits — silently discarding a caller's `type`, entities or relation is
indefensible regardless — but it is a precondition, not the lever, and it unlocked nothing here.
See `docs/internals/agent-experience-findings.md` for the full evaluation.

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
presentation layer, #604); ambient push (negative result, #609); associative surprise
(negative result, refuted at its premise after four measured passes — a focal's
co-activation neighbourhood is its semantic neighbourhood, so 100% of real candidates sit at
or above the non-obviousness ceiling).

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

### A config value must denominate a unit someone can reason about (assoc decay / #762, 2026-08-01)

Association decay was `w ← w × AssocDecayFactor`, applied "once per prune pass" at 0.95.
Nobody ever wrote down what a pass was. The git history says why: `runPruneWorker` and its
60 s period arrived in `e51b985` as an *engram* sweep, where 60 s is a responsiveness
choice; `79605b2` bolted association decay into the same loop the next day and edited only
the doc comment. So the unit of decay became an interval owned by an unrelated worker —
1329 passes/day, a 14.6-minute half-life, `0.95^1329 ≈ 2.4e-30` per day. The observable in
#762 was the store-wide **maximum** association weight sitting at 0.05, the pruning floor.
"Fades when unused" was implemented as "fades unless used every five minutes," and the
5-minute grace window turned the curve into a cliff. Reinforcement could not compete:
holding one edge flat needed ~6,850 signal units/day, roughly 20–70 co-retrievals per
minute forever.

The fix decays against **elapsed wall-clock time from state the edge already carries**:
`w_new = min(w_old, max(peak·0.05, peak·2^(−Δt/H)))`, `Δt = now − lastActivated` (COG-27).
Both inputs were already in the 26-byte association value, so it cost no keyspace entry, no
value-format change, and no migration — which is why this shape won over the obvious
alternative (`w ← w·f^(elapsed/interval)` with a persisted per-vault last-decay watermark).
That alternative needed a new Pebble prefix, a Tier-3 keyspace review, `ClearVault`
plumbing, a downtime-debt cap, and per-node watermarks that never converge — and it stays
path-dependent, so #760 gets worse rather than better.

Three things this settles beyond the immediate bug:

- **A rate needs a unit.** `assoc_decay_factor` was kept as the enable/disable switch (COG-16
  unchanged, `scratchpad`'s 0 still disables with no special case) and the rate moved to
  `assoc_half_life_days`. An explicit legacy factor in (0,1) is reinterpreted **per-day**
  with a one-time WARN naming the derived half-life — because "per pass" was never a
  meaning, only a number, and preserving the observed behaviour would mean preserving the
  bug. An explicit factor ≥ 1 carries no rate at all ("on, but weights never move"): it
  resolves half-life 0 and decay skips with its own truthful WARN — falling through to the
  preset's 30 days would run decay at a rate the operator explicitly declined and log a
  "derived" value that was never derived.
- **Prefer the clamp you cannot write around.** "Decay never raises a weight" is a pair of
  guards, not a policy comment (principle #3): the drop guard (`drop = w_old − ceiling`,
  write nothing unless `drop ≥ ε`) covers every path where the ceiling is the new weight,
  and a post-floor guard (`newW ≥ oldW` after the floor/archive block ⇒ no write) covers
  the floor branch, which assigns `dynamicFloor` and — the adversarial refute proved it
  with an executable test — could raise a sub-floor weight (0.02 → 0.045) and rewrite a
  floored edge's 5 keys every pass forever. The first draft claimed the drop guard alone
  made an increase unrepresentable; it did not, because the floor branch runs after it.
  Two earlier lessons repeat here: the first draft also had the `min` and the epsilon skip
  as separate expressions of the same clamp, and deleting the `min` left the test suite
  green because the epsilon check silently covered for it. A guard whose removal nothing
  notices is not a guard — and a claim of "no branch can do X" is only as good as the last
  branch added below it.
- **The damage is not retroactively repaired, on purpose.** Edges already at the floor stay
  there and re-learn through Hebbian growth. A one-shot re-anchoring pass (`w ← ceiling` for
  floored edges with co-activations) would fabricate weights that were never earned. Left as
  a separate, opt-in decision, not shipped by default.

**Principle: a configuration value must denominate a unit the operator can reason about. If
the unit is an implementation detail of an unrelated component, the value is not a setting —
it is a coincidence, and changing that component silently changes the behaviour of the
system.**

### Recall resolves a declared version chain to its head before ranking (#763, 2026-08-01)

Round-6 hands-on evaluation, call 18: immediately after `muninn_evolve`, the natural
question about the evolved decision returned only an adjacent memory and omitted the
freshly evolved decision entirely. A rephrase in deep mode found it. The evaluator's
framing — *"a memory system that stores truth but fails to retrieve it under a natural
phrasing is not dependable enough to drive autonomous decisions"* — made this the only
blocker between that evaluator and primary-memory use.

**The diagnosis was an ORDERING bug, not a scoring, threshold or embedding bug**, and
saying so mattered: three plausible-looking fixes were rejected on it. `EvolveAt`
soft-deletes its predecessor, but nothing removes that predecessor from HNSW ("HNSW has no
delete method") or from FTS — its vector and its postings are exactly what the user's OLD
wording matches. Phase 6's lifecycle cut discards it *before it is ever scored*, so the
relevance the stale wording earned is thrown away rather than redirected, and
`applySupersession` — which already does the right thing for the visible stale case — says
so in its own doc comment: *"evolve() soft-deletes its predecessor, so those never reach
here."* The visibility cut runs before the substitution phase, and the substitution phase
can only see what survived the cut.

**Rejected alternatives, each for a stated reason.**

- *Substitute at candidate assembly (phase 2), as the issue's sketch says literally.* At
  phase 2 there is no evidence, no score and no visibility resolution — we would inject a
  head for every superseded engram any index happened to return.
- *Re-score the head against the query and gate it on its own absolute.* The successor's
  whole purpose is that its wording changed; gating it on its own absolute reproduces call
  18 exactly, one layer deeper.
- *Inject at `shadow.Final − ε`, or cap the head below rank 1.* Superficially conservative,
  actually incoherent: the existing ε orders a head against its own VISIBLE stale twin,
  which here is not in the set. A ranking penalty on a fact the author declared current is
  a silent statement that we trust the declaration less than we say we do. If the
  declaration is untrustworthy the substitution should not happen at all, not at a discount.
- *Inherit the predecessor's embedding onto the successor to close the fresh-evolve
  window.* **Refuted, not deferred.** It would make the successor semantically
  indistinguishable from the fact it replaces (matching the OLD wording forever, silently,
  because a vector carries no provenance), swap the vector mid-life when the real embedding
  lands so identical queries return different results with nothing explaining it, and
  poison every downstream consumer of the vector — dedup, consolidation similarity,
  `similar_entities`, Hebbian neighbour selection — with a value that is not a measurement
  of that engram's text. Substitution already covers the window correctly and for free.
- *A per-vault plasticity kill-switch.* Deliberately not offered: a toggle for a
  correctness invariant invites "turn it off when it misfires" instead of fixing the
  misfire, and presets are a hand-duplicated drift surface. If the precision gates cannot
  be met, the design is wrong and must not ship behind a flag.

**Two findings that came out of building it, both raising scope rather than lowering it
(principle #9).**

1. *`EvolveAt` never woke the retroactive embed processor.* `Write` calls the `onWrite`
   hook after commit; evolve was the one write path that did not, so the successor waited
   for the processor's ticker, which backs off geometrically to a 3-minute ceiling on an
   idle vault. On a quiet vault a freshly evolved memory could be semantically unindexed
   for up to three minutes after the commit — the largest single contributor to the
   fresh-evolve retrieval window carried since round 4, and a one-line fix.
2. *Multi-hop evolve chains could never resolve at all.* The design doc asserted A→B→C
   returns C, and separately asserted that a soft-deleted successor voids the supersession.
   Both are in the shared walker, and for evolve chains they contradict: every intermediate
   an evolve leaves behind IS soft-deleted, so the walk read the first one as "retracted"
   and voided the whole chain — A→B→C returned nothing, which is #763 again with an extra
   hop. Resolved by distinguishing the two states with the same closed-`ValidUntil`
   signature the rest of the increment uses, which is exact rather than heuristic:
   supersession soft-deletes AND stamps atomically, a plain `muninn_forget` leaves the
   stamp open, and `forget(not_true_since)` stamps without soft-deleting.

**Principle: when the fix for a retrieval miss is "return something we did not retrieve",
the burden of proof is the false-positive rate, and it must be measured on the corpora
that already exist rather than argued.** The precision half is pinned by zero shadows
across 16 nonsense probes with a declared chain grafted into the abstention corpus, an
adjacent-topic corpus with a positive control, and an exact-equality detector for
normalization leakage — because a substitution that fires on the wrong topic is the
silently-wrong class this project ranks worst, arriving at the score the RIGHT topic earned.
