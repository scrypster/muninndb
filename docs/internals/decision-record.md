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

**Coverage is not correctness (#731 → #732).** `muninn_state` sat in `isReadOnlyTool`
while `handleState` reached `Engine.UpdateLifecycleState` → `store.CompareAndSet`, so an
observe-mode credential could archive any engram in its vault — out of recall on every
path — and every test stayed green. The exhaustive test proved each tool sat in exactly one
bucket, never that the bucket was right. Three things hid it: the adapter **renames** the
method (`UpdateState` → `UpdateLifecycleState`), so grepping `internal/mcp/` for the engine
method finds only the one-line forwarder and never the handler; `registeredToolNames()` was
a hand-maintained mirror of the dispatch map; and the classifiers were `switch` bodies,
which cannot be enumerated, so a dead or misspelled entry was invisible. Unlike SEC-15's
append guarantee — backstopped at the engine by `refuseAppend`, which is why append mode was
never exploitable — observe has **no engine-level refusal**. `ObserveFromContext` suppresses
COG-11 learning and `auth.ContextMode` gates SEC-14 trust; neither refuses an operation. The
classification WAS the enforcement. Fixed by reclassifying, deriving the registry from the
dispatch map, making the classifiers enumerable maps, and adding a per-tool census a human
must edit. A reviewing pass built an independent write-reachability oracle and confirmed
`muninn_state` was the sole outlier in both directions across all 44 tools — and found why
the obvious follow-up test does not work: `Read`, `Activate`, `Explain` and `Stat` also
reach writes and are correctly read-only, because the engine suppresses them via
`ObserveFromContext`. So "a read-only tool must not reach a write" is RED on day one against
correct code; the true invariant is "must not reach a write unguarded by `ObserveFromContext`",
which points at an engine-level `refuseObserve` rather than a test.
**Principles: fail closed on auth; an exhaustive test that pins shape rather than meaning is
not a gate; prefer the enumerable structure over the one that cannot be asserted about.**

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
or above the non-obviousness ceiling); cross-domain discovery as designed (held at a
multiple-comparisons ceiling, #706 — reopening requires a null with sub-1/T resolution or a
gated test set, not a larger draw count); dream phases enabled by default (measured
net-negative against no dream at all, #786 — the phase-GATING increment #785 remains open);
repairing `phase5Traverse`'s hop threshold (negative result, #801 — the gate has been above
the mechanism's ceiling since the initial commit, and at scale traversal is strictly
dominated by simply raising `CandidatesPerIndex`; no threshold formulation is supported, and
only the ACT-R-seeded variant is left open, under a pre-committed rule in this record).

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

### A permutation null's resolution is a design ceiling, not a tuning knob (cross-domain discover / #706, 2026-08-02)

`muninn_discover` — read-only cross-domain connection discovery — was built RED-first with a
four-assertion planted-vault proof and then **held, not merged**, when an adversarial refute
of its STATISTICS found the significance inflated and the design unable to clear a valid gate
at production scale. Recorded here because the plumbing is correct and should be reused, and
because the reason it cannot ship is a constraint any successor has to clear rather than a
number anyone can turn up.

**The defect, traced to the line.** `circularShiftPValue` returned
`(exceed+1)/(NullIters+1)`, but the null draws come from `deterministicShiftOffsets`, which
walks `[1, T-1]` — a permutation space of exactly **T-1 members**. At T=365 both the
production default N=500 and the proof's N=4000 yield the same **364 distinct rotations**;
every extra draw recomputes a rotation already computed, producing a bit-identical statistic
and no new information. Dividing by N+1 anyway reports a resolution the data cannot contain:
`1/4001 ≈ 0.00025` against an exact floor of `1/365 ≈ 0.00274`, an **11x** overstatement
(the production default's `1/501` is 1.4x). Fixed by deduplicating the offsets and dividing
by the size of the distinct set — which also bounds the work at the size of the permutation
space instead of growing linearly in a knob.

**With the honest p, the planted signal fails.** At the proof's m=128 tests, BH q at rank 1
is `p·m`: `1/365 · 128 = 0.35` against the reported `0.032`. It misses q≤0.05 by 7x. The
proof's `nullItersForTest = 4000` had been chosen to push `1/(N+1)` under `0.05/128` — the
inflation *was* the gate. Re-run at production defaults on the same planted vault, the signal
never appears at all: `[lag7/N500] candidates=0`, `[lag1/N500] candidates=0`,
`[lag7/N4000] candidates=0`; only `[lag1/N4000]`, a regime `handleDiscover` never uses,
surfaces it. **Landing the p-value fix on the held branch turns its own proof RED, and that
red is the truth** — `TestDiscover_PlantedSignalProof` reports
`tested_pairs=128 dropped_fdr=128` with an empty top-3, and
`TestDiscover_COG22_ReadOnly_FailOnMutate`'s deliberate non-vacuity guard ("a no-op read-only
stub would also pass trivially") fires too, which is exactly the assertion earning its keep.
Neither was softened.

**The ceiling, which is the entry's whole point.** With a valid floor `max(1/T, 1/N)`, the
largest m a *lone* true positive can survive under BH at q≤0.05 is `0.05/p_floor`: about **18**
tests on a 365-day window, and about **25** even on a ten-year vault (where N=500 becomes the
binding constraint rather than T). Production defaults test 200 entities per domain across 8
lags — up to 320,000 tests. A handful of genuine signals **structurally cannot** clear FDR
there. This is inherent to (day-bucket circular-shift null, ~T rotations) × (BH-FDR over all
pairs); honest p makes the feature a no-op at scale, inflated p makes it a liar. **No draw
count, support floor or q threshold moves it** — a successor must change the ceiling itself:
enlarge the permutation space below 1/T resolution (finer buckets, block permutation), adopt a
null with genuine sub-1/T resolution (analytic Poisson/binomial, a non-rotation bootstrap), or
shrink m before correction (hierarchical/gated testing over a small pre-registered candidate
set, or discover-on-half/confirm-on-half instead of BH over the full pair×lag grid). Its
acceptance test must assert the planted signal survives **at `handleDiscover`'s real
defaults**, never a bespoke regime.

**Correct — do not rebuild.** Five pieces survive the refute intact and belong in any
successor: (1) **COG-22 read-only structural enforcement** — the `discoverReader` interface
plus `mutationCanaryStore` make a write unrepresentable rather than forbidden (principle #3);
(2) the lift formula `k·T/(n_a·n_b)` computed on the **`EffectiveValidFrom` event clock**, not
`CreatedAt`, with per-day presence dedup and cross-domain self-pair drop; (3) **BH-FDR over
ALL tests**, never just the survivors, with correct step-up monotonicity; (4) the
circular-shift-over-IID null choice and its anti-conservative unit tests — an IID shuffle
destroys the autocorrelation instead of the alignment and makes a merely-bursty independent
pair look significant; (5) the **non-gameable shuffle RED** — permuting the market domain's
timestamps leaves every count identical and collapses the planted pair to lift≈1.1, p≈0.28.
Plus the evidence contract (non-optional support/lift/p/q/marginals) and its strict
no-causation language.

**Principle: the resolution of a permutation test is a property of its permutation space, and
no draw count buys resolution the space does not have. Before designing a multiple-comparison
gate, compute `alpha / p_floor` — if that number is smaller than the number of tests you
intend to run, the design is a no-op or a liar and no amount of tuning changes which.**

### One vault finds bugs; two vaults tell you which finding generalizes (entity sparsity / #716, 2026-08-02)

A measurement on a 3,296-memory production vault found that many memories carried **zero**
extracted entities — **12.8%** entity coverage in the post-enum window — and concluded that
*real vaults are entity-sparse*, that the tag layer is the actual organizing signal, and that
entity-anchored features (entity-boost, entity-scoped recall, #712 version-clustering,
cross-domain discovery) will under-fire on real data. #712 was re-anchored onto tags partly on
the strength of that claim.

**It did not survive a second vault.** @johanneshauer measured an independently-used
production vault — 2,892 engrams, ~5 months, multi-user, external Ollama `bge-m3` (1024-dim)
rather than the bundled bge-small — read-only against a backup snapshot, aggregate counts only:
**95.1%** coverage whole-vault, 97.6% pre-enum, and **92.6%** in the *same calendar window on
the same server version* that produced the 12.8%.

So **"real vaults are entity-sparse" is not a property of real vaults. It is a property of how
a vault is written.** The batch-vs-single control replicates the causal mechanism on both
corpora: the batch-shaped path (which never received the strict entity-type enum) stays flat
while the enum'd single-write path drops — **DiD +10.8pp** there against **+65.7pp** here, same
sign, same shape, on a different embedder and a different domain. What differs is magnitude
(−9.5pp against −63.4pp), and the magnitude tracks **how well the writing client already knew
the entity-type vocabulary**: an operator-primed caller that had been told the 14 recognized
types inline had almost nothing left for the enum to reject. Dose-response for exactly the
mechanism the first vault identified — the enum's cost is a function of caller priming, not of
the data — and substantially fixed upstream by #743, which demotes the enum to a description so
an unknown type degrades to `other` instead of dropping the whole entity. A follow-up 144-run
paired harness put a **lower bound** on the priming effect itself (+0.383 entities, +0.574
relationships per write from one added operator paragraph, both CIs clear of zero) and could
not build a genuinely unprimed arm, which is stated as a limitation rather than papered over.

**What still stands, and is a standing design constraint.** The tag layer *is* a first-class
organizing signal on some real vaults; entity-anchored features *do* under-fire where extraction
never populated; a feature must not assume an entity-rich graph. `ExcludeTags` shipping
per-vault is that principle already in practice. What does **not** stand is treating
entity-sparsity as the steady state of real vaults, or as evidence against the entity anchor in
general — the evidence base for that was a single vault written before #743 landed. It is worth
re-measuring coverage on a post-#743 corpus before either anchor is called the right one. This
binds #712, #789 (whose per-vault contradiction lexicons face the same question) and cross-domain
discovery alike: each must work on both shapes, because both are real.

**Principle: ONE vault is for finding bugs. TWO vaults are for knowing which finding
generalizes.** A single-corpus measurement can establish that a mechanism is real (the DiD
replicated) and simultaneously mislead about its population (the headline inverted, 12.8% to
95.1%). This is CLAUDE.md #11 earning itself from the other direction: the rule against baking a
sample vault's constant into the product is the same rule as the one against generalizing a
sample vault's *shape* into a claim about everyone. A finding phrased as "real vaults are X" has
a denominator of one until a second, differently-written corpus says otherwise — and the honest
version of the original finding is "a vault written this way is X", which is both smaller and
true.

### Dream consolidation's phases did not beat doing nothing (#786 / #367, 2026-08-02)

@5queezer ran the ablation that settles how dream ships: **50 Optuna trials (TPE sampler) over
255 dream-phase combinations**, scored on PersonaChat + MultiWOZ, followed by a full
vault-isolated 6-dataset run with three arms.

| arm | composite |
|---|---|
| baseline — no dream at all | **0.489** |
| all phases enabled | 0.374 |
| Optuna-best subset (1,2,5) | 0.322 |

**No phase combination in the study beat doing nothing.** Per-phase deltas: transitive inference
**+0.022** and orient **+0.007** help, semantic dedup +0.006 is marginal; relevance decay
**−0.011**, LLM adjudication **−0.011** and bidirectional stability **−0.014** are net-negative.
Follow-up LocOMo/LongMemEval work (read-only evaluation, predeclared config, median-of-fifteen)
came out neutral for conservative ERC, mean ≈ **+0.00048** — and the contributor's own reading is
the right one: *"I would not sell this as a large LocOMo win,"* treat it as diagnostic
infrastructure, not positive evidence.

**This is why dream ships opt-in with conservative defaults rather than on by default**, and it
belongs where people read *why* rather than in a closed thread. It sits beside ambient push
(#609) and associative surprise (#751) as a measured negative that shaped the product —
principle #10, and a particularly clean instance of it: the contributor published the table that
killed his own feature's defaults, and volunteered the null follow-up rather than the arm that
agreed with him.

**The MECHANISM half stays OPEN.** This record closes the *claim*, not the work. **#785** —
`MUNINN_DREAM_PHASES` selection with a conservative default phase set `{0,2,5}` (orient, semantic
dedup, transitive inference), `parseDreamPhases` returning the safe set on empty OR invalid input
with a WARN (fail-open-to-safe, principle #4), landed as **per-vault plasticity config** rather
than a process-wide constant (principle #11 — a fixed phase set is exactly the kind of one-vault
answer we do not ship as law) — is real unbuilt code and does not close with this entry. The
measurement above is precisely what its defaults should encode: the three phases that were not
measured harmful. Its preset obligations apply in full — web console cards and a
`reflect.DeepEqual` pinning test (principle #6).

**Principle: a negative result retires a claim, not the mechanism it was measured on. Record
which half died — and say plainly, in the same entry, which half is still open — or the next
reader will close both.**

---

**A symmetric relation gets a read-side union in a SEPARATE method, never in the shared
reader — and never write-side mirroring (#800).** Every association writer picked an edge
direction, and they picked different ones: the Hebbian worker canonicalises each
co-activated pair older→newer, the neighbour and autoassoc workers write newer→older.
Recall's two ranking phases read only the 0x03 forward index, so the SAME single
relationship boosted a candidate at full strength from one endpoint and by exactly zero
from the other. Every DIRECTIONAL relation in the codebase (supersession, currency, the
contradiction gate) was already being read from both endpoints correctly; only the
symmetric ones were half-blind. The classification was inverted, and it had never been
written down anywhere.

Three placements were on the table and two were killed on evidence:

*Killed — unioning inside `GetAssociations`.* Its consumers include a WRITER (dream's
transitive inference persists what it infers) and direction-presenting surfaces
(`Engine.Traverse`, REST `/associations`). Unioning the shared reader made dream persist
manufactured transitive facts and made REST report "the OLD version supersedes the NEW
one" — with a green suite. Both failures become structurally unrepresentable when the
union lives in a sibling method, which is COG-22's `NameableAsLineage` shape reused.

*Killed — write-side mirroring (writing both `fwd(a,b)` and `fwd(b,a)` for symmetric
types).* `UpdateAssocWeightBatch` stamps `lastActivated` on the canonical key only, and
COG-27 makes decay a pure function of `(peakWeight, lastActivated, now)` per 0x03 key. A
mirrored edge would therefore decay while its primary did not: ~50% divergence at 30 days,
the 5% floor at ~130 days, a 20x direction skew that no reader can detect and no test
would catch. Making it correct means dual-keying every weight write atomically, which
drags in `GetAssocWeight`'s single-value contract, the #756 0x2E repair pass,
`deleteLegacyFullWeightKeys`, archive/restore, export/import and the replicated batch — a
Tier 3 on-disk change with a migration, in exchange for a fix that leaves every existing
vault broken. The read-side union, by contrast, fixes every existing vault the moment it
lands, because 0x04 has been fully maintained all along.

**Principle: when a shared reader has both a writer and a presenter downstream, do not
widen it — add a sibling with a narrower contract and name its only legitimate consumers.
And prefer the fix that repairs existing data over the one that only helps new data.**

**A graceful-degradation fallback can reinstate the very defect the change fixes, and the
half that is preserved is the half that gets written down (#800).** `phase4HebbianBoost`
swallowed its read error with a bare `return`, and the union gave it a second source for
one, so the first repair was the obvious pairing: warn, then fall back to the forward-only
`GetAssociations`. It preserves the forward half's absolute signal — and it is not
uniformly better than the bare return it replaced. Measured on the fixture that IS #800's
root cause (one recent engram, two candidates, one `RelCoActivated` edge of identical
weight each, differing only in the orientation their writer picked): healthy union 0.5/0.5,
forward-only fallback 0.5/0.0, bare return 0.0/0.0. `hebbianBoost` MULTIPLIES the RRF score,
so the fallback opens a 33% final-score gap between two candidates the corpus says are
equal, while the thing it replaced preserved their (correct) tie. The fallback trades
tie-preservation for signal-preservation, and only the winning half of that trade was in
the commit message. Resolved by dropping the Hebbian term entirely on a failed union and
warning — the same shape as an unreachable embed backend degrading to BM25-only rather than
to a half-applied vector score. **Principle: a fallback that keeps PART of a signal keeps
part of that signal's biases too. Before adding one, score the fixture the bug was filed
about — if the fallback re-enters the failure mode, uniform loss beats partial, biased
retention. And pin the RELATIVE ORDER, not just the magnitudes: every magnitude assertion
here passed on the defective fallback.**

The counter-argument, recorded because a decision record that carries only the winning
half is the same shape as a benchmark that only measures the arm that agrees with it:
**dropping the whole Hebbian term costs related-vs-unrelated discrimination for ALL
candidates in that recall, and that aggregate quality loss may well exceed the orientation
bias among the subset of pairs that happen to be symmetric.** Neither quantity is measured,
and measuring them needs a relevance-judged corpus this project does not have. The decision
still stands on shape rather than magnitude — the whole-term loss is UNIFORM, so nothing is
ranked above anything else on a fabricated basis, and principles #1/#2 rank silent
wrongness above visible degradation — but "the revert is right" is a judgement here, not a
measurement. Two things narrow the counter-argument's practical reach: the term is a
ranking MODIFIER, not a channel, so its absence changes an ordering and not what any score
asserts; and `GetRankingNeighbors` errors if EITHER half fails, so a forward-half failure
would have failed the fallback too — the fallback only ever helped on a
reverse-scan-specific failure, a strictly narrower set than "a failed union".

**The degradation is also undetectable downstream, deliberately (#800).**
`ActivationResult` carries `SemanticDegraded` for the semantic channel and has no analogue
for the Hebbian one, so a dropped Hebbian term is visible only in the server log — a
caller cannot tell a recall that lost it from one that never had it. Accepted rather than
overlooked: a channel flag tells a caller that a score MEANS something different (a BM25-only
score is not a hybrid score), whereas a missing ranking modifier leaves every score meaning
exactly what it says and only reorders them. Adding a second flag would also make the wire
shape imply the two are peers. Recorded as a deliberate asymmetry so it is not rediscovered
as an oversight.

**The "loudly" half of degrade-loudly-but-gracefully is a behaviour, and it was unpinned
everywhere (#800).** Deleting both `slog.Warn` calls from `phase4HebbianBoost` left
`./internal/engine/... ./internal/storage/` fully green: nothing in the repo asserted on a
WARN string, on a change whose stated justification is principle #2. A four-line
`captureWarn(t, fn) string` test helper (swap `slog.Default()` for a buffer, restore) makes
asserting on the log as cheap as asserting on a return value. **Principle: if the log line
IS the user-visible behaviour of a degradation path, it needs a test like any other
behaviour — otherwise "loudly" survives exactly until someone tidies up.**

**A cost model that says "one more bounded scan, like the one next to it" must check
whether the one next to it is cached (#800).** The design sized the reverse read against
the forward read and left it uncached, reasoning that one extra bounded Pebble iterator
was affordable. It was not: the forward half is served from `assocCache`, so the reverse
half was paying ~50 fresh seeks on every recall. Measured on a synthetic 200-engram vault
at 10 edges/node, a 50-candidate read cost ~11µs forward-only and ~152µs for the union,
which moved whole-recall p50 15-20% — past the increment's own pre-committed kill
threshold. Giving the reverse half a cache of the same shape, and replacing a per-candidate
dedup map with a linear scan over a list bounded by `maxPerNode`, brought the union to
~41µs and whole-recall p50 to +1.7% (paired median, 12 rounds of the increment's own
harness, whose whole-recall p50 is ~0.5 ms — no embedder in the path). The +1.3% in the
entry below is a SECOND, independent attempt at the same quantity, and the two do NOT
corroborate each other, in either direction: the denominators differ ~50× (~0.5 ms here,
~26 ms there), so equal percentages would be absolute costs 50× apart — +1.3% of 26 ms is
~340 µs, roughly 8× the ~41 µs measured here — and the +1.3% is itself inside its own
run-to-run spread, i.e. a null equally consistent with 0%. The honest reading: the
small-denominator harness measured the effect, the end-to-end harness was underpowered for
it and cleared the gate without resolving it. Neither harness is committed, so neither
number is reproducible from the tree; both are recorded as what was observed, not as a
result anyone can re-derive here. **Principle: two percentages of two different
denominators are not two measurements of one number — convert to absolute cost before
claiming agreement, and a result inside the noise band corroborates nothing.** **Principle: "symmetric
cost to an adjacent operation" is a claim about the adjacent operation's implementation,
not its signature — and a per-item map allocation on a path that runs 50 times per query
is usually the largest line in the profile.**

**A number cited from a benchmark must be producible BY that benchmark — check which arm
you read (#800).** The extra copy in `mergeRankingNeighbors`' no-reverse-edges shortcut was
recorded as "~1µs per call, measured with `BenchmarkPhase4Read`". That benchmark builds a
RING: at `edges > 0` every node has both outbound and inbound edges, so `len(rev) > 0` and
the shortcut is never reached; at `edges = 0` no node has any edge, so the forward list is
empty and the branch returns `nil` without copying. **No arm of it performs the copy**, and
the ~1µs was machine noise — it moves in the same band with the copy reverted (degree-0 arm,
five runs each: 9.5–10.8 µs with the copy, 9.4–10.3 µs without, fully overlapping). Re-measured on a fixture that does pay
(`BenchmarkPhase4Read_ForwardOnlyFan`: 50 candidates fanning out to sinks that are never
themselves candidates, so nothing points AT a candidate), the median cost is +4.1 µs at
forward degree 2, +6.8 µs at 10 and +13.2 µs at the `maxPerNode` cap of 20 — an order of
magnitude above the recorded figure, and structural rather than noise: allocations go
62 → 112, exactly one per candidate. The decision is unchanged (~13 µs against a ~26 ms
whole-recall p50 is ~0.05%, and uniform slice ownership is worth it), only the claim.
The repair was to ADD the arm rather than to soften the prose, so the doc's citation is
regenerable from the committed mechanism, and the fixture's shape is asserted in the CI
gate by `TestForwardOnlyFanFixture_TakesTheMergeCopyShortcut` rather than assumed — the
whole failure was a number taken from an arm nobody checked. **Principle: when a claim
names a measurement, the named measurement must be able to produce it. If the benchmark
you cite has no arm that exercises the code you are pricing, adding the arm is the fix;
restating the prose leaves the next person measuring the same wrong thing.**

**A latency budget is only meaningful with its denominator attached (#800).** The COG-31
increment pre-committed a whole-recall p50 kill threshold expressed against a ~0.5 ms
figure. Re-measured end to end through `Engine.Activate` with the real embedder — 60
recalls per arm, 4 runs per commit, an independent attempt at the +1.7% above rather
than a revision of it — cold p50 moved 26.14 ms → 26.49 ms (+1.3%, inside the
run-to-run spread), and p99 on IDENTICAL code varied 47.5–261.6 ms across four runs, so p99
is not a usable gate at this sample size. The gate is cleared, but the number that cleared
it is **embedder-dominated**: whole-recall p50 is ~26 ms, not ~0.5 ms. A storage-layer cost
of a few hundred microseconds is ~1% of that and ~55% of the other, and **a deployment that
supplies caller-side embeddings sits at the other one** — no embedder in the path, so the
same absolute cost is a large fraction of the call. **Principle: a percentage-of-p50 budget
silently encodes a deployment shape. State the absolute cost and the denominator you
measured it against, or a cleared gate will be read as "negligible everywhere".**

**Two caches keyed alike are still two caches: never share one dedup set between them
(#800).** `UpdateAssocWeightBatch` invalidated the forward cache on each update's `Src` and
the reverse cache on its `Dst`, deduplicating both through ONE `seen` set. The keys are the
same 24-byte `(vault, engramID)` shape, so an engram appearing in BOTH roles inside one
batch had its second eviction suppressed and one cache served pre-batch weights for the
rest of the 2s TTL. That is the common case, not a corner: `HebbianWorker.processBatch`
emits every C(n,2) pair of a co-activated set, so any three co-activated engrams X<Y<Z put
Y in both roles — every recall returning ≥3 results. It also regressed a path the increment
did not touch (`GetAssociations`, correct at the parent commit), which is the general
hazard: **adding a second cache to an existing invalidation site is a change to the FIRST
cache's coherence, even when the reader is byte-for-byte unmodified.** Dedup sets are
per-cache, and the pin belongs on both sides.

**`revAssocScanCap` bounds accepted edges, not keys scanned — deliberately (#800).** An
inbound edge failing `BidirectionalForRanking` is skipped without consuming a cap slot, so
one cold `GetRankingNeighbors` for a hub is O(inbound degree): measured ~4 µs at degree 0,
~65 µs at 1,000 and ~0.5 ms at 5,000 directional inbound edges, returning ZERO edges for
that cost, against a few µs for the pre-change forward-only read; the symmetric arm stays
flat (~15 µs) because there the cap binds. **Quote the RATIO, not the microseconds.** Across
more than a dozen runs of the committed benchmark on two machine classes, the degree-5,000
figure landed anywhere from ~390 to ~570 µs — a spread wider than any effect this code
could have, on a benchmark that purges both caches inside the loop, so it is machine
variance and not fixture noise. An earlier revision of this entry quoted a three-run band
("389-493 µs") and five of the next six runs fell outside it: **a band from a handful of
runs on one machine is a sample, not a bound, and writing it down as a range makes it look
like the latter.** What reproduces is that a degree-5,000 directional hub costs roughly half
a millisecond, ~100× (observed ~90-140×) the same call at degree 0, and grows linearly in
inbound degree. Turning it into a scanned-key budget was considered
and rejected: reverse keys arrive weight-descending and the two edge classes do not share a
weight distribution — explicit directional relations are written once at a high fixed
confidence weight, while the `RelCoActivated` edges this union exists to surface start low
and grow with use — so a key budget on a directional hub fills with directional edges and
systematically hides exactly the Hebbian edges the feature was built to reach. **Principle:
a "work bound" that truncates an ordered stream is only neutral if the ordering is
uncorrelated with what you are filtering for. Here it is anti-correlated, so the bound
would trade a bounded latency win for a silent, biased loss of real neighbours.** A
relType-aware reverse index, or a per-engram directional-degree hint, is its own increment.

### Associative traversal has never fired, and no threshold repairs it (#801, 2026-08-02)

`phase5Traverse` seeds each hop from the raw RRF score and gates the propagated score on
`minHopScore = 0.05` (`internal/engine/activation/engine.go:1568`, `:1596`, `:1676`).
**Both constants date to the initial commit, and at that commit the gate was above the
mechanism's theoretical ceiling.** The fusion then summed three lists — HNSW k=40, FTS
k=60, decay k=120 — so the best conceivable seed, rank 1 in all three, scored
`1/41 + 1/61 + 1/121 = 0.049048`, and the best conceivable hop off it scored
`0.049048 × 1.0 × 0.7 = 0.034334` against a 0.05 gate. No input, no configuration, no edge
weight and no seed rank could produce a hop. The phase was dead on arrival and has emitted
nothing for the life of the product.

It became *theoretically* reachable later only by accident, when three more RRF lists were
added (PAS transitions k=50, time k=100, tag k=100) and lifted the ceiling to `0.088458`.
That ceiling is not real: the `time` and `tag` lists are only populated when the request
carries a time or tag filter, and an ordinary recall carries neither. Measured list
membership over real recalls on two vaults (460 seeds) found **zero seeds in 5 or 6 lists**,
so the unfiltered ceiling is `1/41 + 1/61 + 1/121 + 1/51 = 0.0686559` and clearing the gate
needs `weight × boost ≥ 1.041`. Maximum edge weight is 1.0 and the `default` profile only
dampens (`profiles.go:82` — every entry ≤ 1.0), so `boost ≤ 1.0`. **Under the default
profile on an unfiltered recall the phase cannot fire at any edge weight whatsoever.**

**Measured, not only derived.** On a real 3,458-engram production vault with 127,798
association edges (plus a 736-engram second vault), 150 queries each, real pipeline, real
bundled embedder, a fresh clone per arm:

- **0 hops on 150/150 queries, both vaults.** Observed maximum seed score `0.04078` =
  exactly rank-1 HNSW + rank-1 FTS; no seed ever appeared in more than two lists. At that
  seed a hop needs `weight × boost ≥ 1.751`.
- **Reachability was not the constraint.** The candidate pool covered 2.60% of vault A
  (p50), with **121 novel engrams one hop from a median query's seeds**. An earlier
  small-vault pass had stalled because the pool covered 100% of the vault; this substrate
  removed that blocker.
- **The instrument was verified non-blind**: the shipped `phase5Traverse`, on the same real
  edges, produces 64 hops the moment seed scores are scaled up.
- **There is no knee, there is a cliff.** Gate 0.05 → 0/150 queries with a hop; 0.001 →
  8/150; 0.0005 → 142/150 at a median of 15 hops; 0 → 125 hops median. Silence and flooding
  are a factor of ~2 in gate value apart with nothing usable between.
- **Weights are pinned 20× below their write-time constants.** Association decay clamps to
  `peakWeight × 0.05` (`storage/association.go`), and a full census finds the live
  population sitting on that floor: `RelRelatesTo` p50 **0.01500** (= 0.3 × 0.05),
  `RelCoActivated` p50 **0.00050** (= the 0.01 cold-start seed × 0.05). Only 829 of 127,798
  edges (0.65%) exceed 0.05 at all, and none approaches 1.04.

**The decisive control is not the obvious one.** Seed↔hop cosine was computed and then
**discarded as circular**: `internal/engine/autoassoc/neighbor.go:185` writes `RelRelatesTo`
edges *from an HNSW kNN search* with `weight = similarity × 0.5`, and 99.2% of reachable
hops are that type — scoring them by cosine measures the construction rule, not retrieval
quality. The non-circular control instead asks what traversal buys **against the cheapest
alternative**: for each query, compare the N hops a budget cap admits with HNSW ranks
`31..30+N`, i.e. what simply raising `CandidatesPerIndex` from 30 gives away for free.

| cap N | A: traversal wins / raise-k wins | Δ p50 | sign test p | vault B |
|---|---|---|---|---|
| 1 | 2 / 146 | −0.0083 | 6e-41 | 2 / 122 |
| 2 | **0 / 150** | −0.0163 | 1.4e-45 | 0 / 133 |
| 5 | **0 / 150** | −0.0218 | 1.4e-45 | 0 / 140 |
| 10 | **0 / 150** | −0.0229 | 1.4e-45 | 1 / 147 |

**Strictly dominated on essentially every query, in both vaults, at every setting.** 82.7%
of top-1 hops already sit inside the query's own top-400 vector ranking; the median top-1
hop sits at HNSW rank 98 — traversal's single best offer is worse than the 31st vector
candidate. The pre-committed usability rule (*median hops in [1,10] AND median admitted-hop
cosine ≥ background p95 = 0.855*) was met by no setting; best observed admitted-hop cosine
0.785 against a background of 0.705.

**Hebbian edges cannot participate, and the ones that would are noise.** Not one of 77,408
`RelCoActivated` edges reaches 0.05 (max **0.01009**, 20× below the gate, ~1,700× below what
a hop needs). At a fully-open gate the 81 that get through measure query-cosine **0.706
against a background of 0.705** — indistinguishable from random pairs — and any gate low
enough to admit one admits a median of 49–125 neighbour hops.

**No threshold formulation is supported**, and the alternatives died for stated reasons, not
for lack of tuning. A lower absolute constant has no knee to find and would encode one
vault's decay history and write-time constants (principle #11, the #762 shape exactly). A
relative-to-seed form is a **category error**: since `propagated = base × w × boost × 0.7`,
the condition `propagated ≥ α × base` reduces to `w × boost ≥ α/0.7` — the seed cancels
exactly, so a fraction-of-own-score threshold is an edge-weight test wearing a score's
clothing, and every `relown`/`reltop` arm admitted either 0 hops or all of them. A
rank/budget cap bounds the count but loses the sign test at p < 1e-40. Per-RelType treatment
fails because the only high-volume type is kNN-derived and therefore **redundant with the
vector index by construction**, while the Hebbian arm sits at background.

**Why nobody noticed, for the life of the product.** There was never a "before" in which
hops worked, so no regression signal ever existed; the only detectable symptom would have
been the absence of something that had never been present. Every gate the project has —
tests, invariants, the review agent — checks that a mechanism behaves correctly when
exercised, and the traversal tests do exactly that: they drive `phase5Traverse` with seed
scores (0.1, 1.0) that phase 3 cannot produce, so they proved the BFS is correct and could
not have revealed that nothing ever reaches it. Meanwhile the neighbour worker, the
autoassoc worker, dream consolidation, the Hebbian worker, decay, archival and the #756
weight-repair pass all kept maintaining a structure that this phase never productively read.
**Principle: a unit test that supplies its own inputs cannot tell you the input is
unreachable. When a mechanism has a threshold, pin the threshold against the measured
distribution of what actually arrives at it, not against a value chosen to exercise the
branch.**

**What this does NOT establish, stated because the negative is strong.** Traversal is not
*harmful* — it never fires, so it costs one (usually cache-warm) `GetRankingNeighbors` call
per recall and then terminates at level 1. Non-default profiles (`causal`, `adversarial`,
boosts to 1.3) were not run; the arithmetic still says no (`1.041/1.3` needs weight ≥ 0.80,
which one edge in 127,798 meets) but it is arithmetic, not a measurement. Filtered recall,
where `time`/`tag` populate and a seed could reach 3–4 lists, was not run. PAS was measured
as *absent* rather than as zero-contribution — the harness cannot populate the in-process
activation log — though its bound (+1/51) changes no conclusion. Queries were in-vault
`Concept` strings rather than a real query log, which makes seeds unusually strong and
therefore **favours** traversal; the null is conservative.

#### The one caveat the data cannot speak to: ACT-R-seeded traversal

Traversal seeded on the **final ACT-R score** instead of `rrfScore` is a genuinely different
mechanism and was never measured. It is not a tuning variant: ACT-R finals are order ~0.5
rather than order ~0.04, so the same 0.05 gate becomes `weight ≥ ~0.14` and the phase would
start emitting. Saying otherwise would be reasoning past the evidence, so it is recorded
here rather than asserted away — and it is recorded here rather than filed as an issue,
because it is a rider on a killed mechanism, not work in its own right.

The prior is strongly against it, with a mechanism: seeding changes only *which* members of
the reachable set are admitted and in what order, never the set itself, and that set is
99.2% kNN-derived edges — the vector index's own neighbourhood of the seeds, 82.7% of it
already inside the query's top-400.

So the arm worth running is not "the ACT-R variant" but **the oracle bound on every possible
seeding rule**, which is the same cost and kills or clears the entire family in one pass.
Pre-committed here, before any number is looked at:

- **Setup.** Same two production vault clones, same 150 queries per vault, same pipeline,
  gate fully open so admission is not the variable. For each query `q`, let `H(q)` be the
  novel one-hop targets reachable from the phase-3 top-20 seeds under the `default` profile
  (p50 121 on vault A). Any seeding rule that leaves the seed *set* alone — ACT-R included —
  can only re-order and re-admit members of `H(q)`; a variant that also re-selects the 20
  seeds needs `H(q)` recomputed over that seed set, a one-line harness change.
- **Arm.** For each `q` and cap `N ∈ {1,2,5,10}`, take the `N` members of `H(q)` that
  *maximise* #801's comparison statistic — an oracle no implementable seeding rule can beat
  — and compare against HNSW ranks `31..30+N`, the same raise-k control and the same
  two-sided sign test over 150 queries.
- **SHIPS** (build and measure a real ACT-R-seeded arm, which must then clear the same bar
  non-oracularly): the oracle beats raise-k on **≥ 90/150 queries at cap 2 with sign-test
  p < 0.01 in vault A, and the same sign in vault B**.
- **KILLS the whole family** (ACT-R seeding, rank seeding, and any future reseeding, without
  running any of them): the oracle wins on **≤ 75/150 at cap 2**, i.e. the sign test does not
  favour it at p < 0.05, in either vault.
- **UNDERPOWERED — a measurement problem, not a verdict** (rerun wider; do not read it as
  either outcome): oracle wins on 76–89 of 150, or the two vaults disagree in sign, or fewer
  than 120 queries have `|H(q)| ≥ N`. At n=150 against a 0.5 null a two-sided sign test
  resolves a 60/40 split at p < 0.01 with power ~0.85, and #801's own arms resolved at
  p < 1e-40, so an ambiguous result here is a genuine null region rather than a resolution
  limit.
- **Cost.** One offline pass on a throwaway clone. No product code, no CI minutes. Nothing
  above depends on it: it can only revive a killed mechanism, never rescue the shipped one.

#### Code disposition: kept, pinned, and labelled — not deleted, not flagged

`phase5Traverse` and its call site (`internal/engine/activation/engine.go:783`) **stay**,
with the inertness recorded at the constant and pinned by a test
(`TestPhase5Traverse_InertAtTheMeasuredSeedCeiling`) that drives the real phase at the
measured unfiltered ceiling and the maximum representable edge weight and asserts zero hops.
The RED control for that test already exists next to it:
`TestPhase5Traverse_ReachesSymmetricEdgeFromEitherEndpoint` drives the same function at
`rrfScore = 1.0` and gets hops, so the pin fails if the phase becomes live and fails if the
BFS breaks — it cannot pass vacuously.

Deletion was the tempting call, and this project has been bitten repeatedly by dead code
everyone believes works. It loses on two concrete counts.

- **It would convert an inert mechanism into a silently-ignored explicit config**, which is
  principle #1's failure class rather than a cleanup. `hop_depth` is a documented per-vault
  plasticity value (`internal/auth/plasticity.go:20`, presets at `:235/:262/:289/:316/:346`)
  surfaced on REST admin + `openapi.yaml`, gRPC (`proto/muninn/v1/service.proto`), MBP
  (`internal/transport/mbp/types.go`), the CLI (`cmd/muninn/vault.go`), the web console
  (`web/static/js/app.js`, `web/templates/index.html`) and `docs/auth.md` /
  `docs/feature-reference.md`; `disable_hops` and `hop_path` are request/response fields on
  those same surfaces. Removing the phase without disposing of all of them leaves a number an
  operator sets and a field a caller reads that mean nothing at all. Doing it properly is a
  cross-surface increment of its own, and it should be decided on the ACT-R/oracle result
  rather than ahead of it — deletion is the one option that cannot be undone cheaply.
- **COG-31 landed four days earlier and names `phase5Traverse` as one of exactly two
  legitimate consumers of `GetRankingNeighbors`**, with the traversal half pinned by its own
  test. Deleting the phase rewrites a large, freshly-reasoned invariant to remove a consumer
  whose read is correct.

An explicitly-off flag was rejected too: a toggle advertises "we might turn this on", which
the measurement contradicts, and it buys a per-vault config surface (preset drift, the web
UI, a pinning test) for a mechanism measured as strictly dominated.

**Named deferral, folded in here rather than filed: `hop_depth` is accepted everywhere and
cannot change any result.** Nothing is silently *substituted* — but it is silently inert,
which is the same disappointment from the operator's side. The honest options are to mark it
advisory-on-every-surface pending the oracle arm, or to remove it together with the phase.
Not decided here; it must not be rediscovered as a bug report.

#### What this implies for adjacent work

- **#805** (CGDN's `epsilon = 0.01` floor sitting 20× above the steady-state Hebbian weight
  of 0.0005) and **#816** (the over-inclusive `keys.PrefixUpperBound` guard, which covers the
  association range scans this BFS reads) both touch a phase that emits nothing. Neither is
  wasted — `phase4HebbianBoost` shares the same read path and *does* contribute — but any
  claim either makes about traversal specifically is a claim about a no-op.
- **#800**'s traversal arm is in the same position: the symmetric read it fixed is correct
  and load-bearing for phase 4, and inert for phase 5. That is why its test says so at the
  top rather than presenting a user-visible win.
- **The general pattern is the same shape as #762 and #805**: a threshold chosen against
  write-time constants, applied to a quantity that decays 20× before anyone reads it. Any
  future constant compared against an association weight must be checked against the
  **steady-state floor** (`peakWeight × 0.05`), not against the value the writer wrote.

**Principle: "the association graph is our differentiator" was a claim about a subsystem, and
it was true of the wrong half of it. Phase 4 reads the graph and contributes; phase 5 reads
it and has never emitted a row. When a claim spans two mechanisms, measure them separately —
and when one of them turns out to be redundant with a cheaper index it is built on top of,
the finding is "delete the claim", not "tune the constant".**

### CGDN has never executed, and the class now has a census (#768/#805, 2026-08-03)

`computeComponents` — the component producer the CGDN scoring path uses — never sets
`ScoreComponents.ContentMatch` (only `computeACTR` does; `computeComponents` is shared with
the legacy weighted-sum path, which never reads that field, so the gap was invisible there).
CGDN's abstention gate is

	absolute := min(min(Raw, ContentMatch), 1.0) * Confidence
	if absolute < req.Threshold && !inTagPool { continue }

so `absolute` is exactly `0.0` for every non-tag-pool candidate on this path, and every
`req.Threshold > 0` drops the entire result set. CGDN has never returned a live result in a
passing configuration for the life of the feature — the same shape as #801, discovered by
the same #763 review panel that found traversal, and deliberately not fixed there either.

**The unclamped-`r` claim in #768 does not hold, and the record needs correcting rather than
repeating it.** The issue also asserted the live ratio `r = a(d)^n / denom` was unbounded,
citing a measured `8649.0`. It is not: the Pass-2 loop that computes `r` for LIVE candidates
only runs `if len(cgdnCands) > 0`, and inside that branch `denom = sigma^n + sum(a_i^n)`
where the sum runs over every candidate in the pool — including the one whose ratio is being
computed. `denom >= a(d)^n` for that candidate by construction, so `r <= 1` always, on the
live path, for every configuration. The measured `8649.0` came from the COG-28 SHADOW pass
evaluated against an EMPTY live pool (`denom` degenerating to `sigma^n` alone at the 0.01
fallback) — a path the code already clamps explicitly (`math.Min(r, 1.0)`) with a comment
naming exactly this trap. Verified independently: read the loop guard and the shadow clamp
in `internal/engine/activation/engine.go` before trusting either claim on faith (principle #9
— severity and correctness both need independent verification, in either direction).

**#805 is not a separate defect — it is the same phase's SECOND, independent reason nothing
comes out, folded in here rather than tracked apart.** CGDN's Hebbian rescue term in
`computeGatedActivation` is `rescue(d) = max(0, hebbianBoost - epsilon) * lambda`, with
`epsilon = 0.01`. Association edge weights are clamped to `peakWeight * 0.05` in steady
state (`internal/storage/association.go`), and a census of two production vault clones put
the entire live `RelCoActivated` (Hebbian) population at a p50 of **0.0005** — twenty times
below epsilon. So even after #768 is repaired, `hebbianBoost - epsilon` is negative for
essentially every live edge and the rescue mechanism the function exists to provide is a
no-op on its own terms — the same #801/#762 shape: a constant chosen against a write-time
value (Hebbian cold-start seed 0.01, which epsilon was presumably set to match), applied to
a quantity a decay pass moves 20x before anyone reads it.

#### Disposition: kept, labelled INERT at both sites, not deleted, not flagged

`computeComponents`, the CGDN branch of `phase6Score`, and `computeGatedActivation` **stay**,
with the inertness recorded at the constant/gate in each case and pinned by
`TestPhase6Score_CGDN_InertAtAnyPositiveThreshold`, which drives the real `phase6Score` CGDN
path with a maximally strong candidate (perfect vector cosine, perfect FTS coverage) at the
smallest representable positive threshold and asserts zero activations. Its RED control is
the neighbouring `TestPhase6Score_CGDNPath`, which drives the SAME function over an
equivalent fixture at `Threshold: 0.0` and DOES get output — proving the pin is not vacuous:
raise the control's threshold above zero and it starts failing like the pin; lower the pin's
threshold to zero and it passes trivially like the control. The two tests bound the defect
exactly at zero, which is where it actually sits.

This follows #801's disposition rule exactly, because the same two facts hold:

- **CGDN has real, live per-vault configuration and transport surfaces**, checked before
  deciding: `experimental_cgdn` (plasticity config, `internal/auth/plasticity.go`, all five
  presets), `use_cgdn`/`cgdn_alpha`/`cgdn_beta`/`cgdn_power` (REST `openapi.yaml` and MBP
  wire types, `internal/transport/mbp/types.go`), and `--mode cgdn` (the CLI REPL,
  `cmd/muninn/repl_client.go`, with its own passthrough test). Deleting the scorer without
  disposing of all of them turns an inert mechanism into silently-ignored explicit config —
  principle #1's failure class arrived at by way of a cleanup, which is exactly what #801
  named as the reason traversal was kept. (CGDN has no gRPC/proto or MCP surface today —
  checked and confirmed absent — so this is a real but narrower surface footprint than
  traversal's six; still enough to make deletion the wrong call.)
- **Deletion cannot be undone cheaply, and repair is not free either.** Wiring
  `ContentMatch` alone would surface a scorer whose OWN Hebbian floor (#805) still discards
  essentially its entire live Hebbian-linked population — an honest fix is a coupled change
  to both constants together, re-measured against a real vault, not a one-line patch. That
  work is not done here; this entry records the finding and the disposition, not a repair.

Comments at three sites carry the finding forward for the next reader: the CGDN branch in
`phase6Score` (the gate itself, the corrected `r <= 1` proof, and why not to "fix"
`ContentMatch` alone), `computeComponents`'s doc comment (the missing field, and why
weighted-sum never noticed), and `computeGatedActivation`'s epsilon constant (the #805
floor, restated at the site rather than only in this record).

`docs/feature-reference.md` and `docs/retrieval-design.md` previously described CGDN as
simply "experimental" with no caveat about output; both now state the inertness and cite
this entry, mirroring what #801 did for `docs/feature-reference.md`'s traversal row.

#### The census: a units/scale check for "constant vs. a decaying quantity", not "every threshold"

#805 asked, as its own machine check: *"a units/scale census over every constant compared
against an association weight: assert each is either derived per-vault or documented with
the steady-state range it is meant to discriminate."* Built as
`TestWeightGateCensus`/`TestWeightGateCensusMatcherFixture`
(`internal/engine/activation/weight_gate_census_test.go`), in the shape of
`TestLastAccessElapsedCensus` (`internal/storage/lastaccess_census_test.go`) — an AST walk
of the whole module, a named-sites floor, and a SEPARATE matcher self-check driven by a
synthetic fixture, because (as that census's own history records) a vacuity guard on a bare
count can pass while the walk or the matcher has silently gone blind.

**Scope, deliberately narrow.** "Every threshold in the engine" was tried and rejected as
too broad to be useful. The rule that shipped: a **named, hardcoded numeric `const`**
(function-local or file-scoped — never a variable, a struct field, or a plasticity-derived
value, which are already per-vault by construction) compared (`<`,`<=`,`>`,`>=`) or
subtracted against a quantity that is, or is transitively assigned from, a `.Weight`
selector or the identifier `hebbianBoost`/`HebbianBoost` (case-insensitive) — this
codebase's vocabulary for association-edge-derived strength. Two things were tried and
explicitly rejected during development, both because they produced real noise measured
against the actual tree, not hypothetically:

- Matching bare numeric literals (not only named consts) — this additionally flagged
  `update.Weight > restoreWeight*1.5` and `newCoAct >= existingCoAct+3` in
  `internal/storage/association.go`, neither of which is the defect: a named constant is
  specifically the signal that a human chose a number once and is unlikely to ever revisit
  it, which is how both #801 and #805 went unnoticed since the initial commit.
- Matching the substring "boost" instead of the exact `hebbianBoost` identifier — this
  caught `entityBoostNoiseFloor` (`internal/engine/engine_entity_boost.go`), a named float
  constant genuinely compared against a derived quantity (`contribution`), but that quantity
  is `entityBoostFactor * idf` — an entity-rarity IDF score with no decay pass touching it,
  not an association weight. A FALSE POSITIVE, kept in the matcher fixture
  (`notAssociationWeight`) as the permanent regression case for that vocabulary choice.

**Result on the real tree: three sites, zero false positives.** `minHopScore` (#801) and
`epsilon` (#805) were the two the record already knew about. The census found a **third**,
previously undocumented in this class: `internal/consolidation/transitive.go:minWeight`
(`runPhase5TransitiveInference`, gating transitive-edge inference at
`max(Weight, PeakWeight) >= 0.7`). Inspection showed it is the **healthy** counter-example:
its own comment already reasons about the achievable range — autoassoc mints edges at 0.3,
archive restore returns them at `peakWeight * 0.25`, so neither route can reach 0.7, which is
exactly why the threshold was chosen there and why a dangling edge (the risk the comment
names) cannot climb to it. It is recorded in `weightGateKnownSites` alongside the two dead
ones specifically so a reader sees both outcomes of doing the range-check: two constants
that were never checked and are dead, one that was checked and is fine. The census asserts a
site is DOCUMENTED, not that it is healthy — both `weightGateKnownSites` entries `minHopScore`
and `epsilon` remain intentionally live and undeleted per this entry's own disposition,
while `minWeight` needed no change at all.

**RED-verified, not merely asserted.** Neutering the transitive-taint fixpoint (so only a
direct `.Weight` selector — never a locally-assigned intermediate like `propagated` or
`effectiveAB` — counts as tainted) was tried as the sabotage: `TestWeightGateCensus` lost 2
of its 3 known sites and failed loudly naming which; `TestWeightGateCensusMatcherFixture`
failed on the exact fixture case exercising that shape. Restoring the fixpoint made both
pass again. The census is not vacuous with respect to its own matcher, by demonstration, not
assumption — the same discipline `TestLastAccessCensusMatcherSeesLaunderedCopies` established
after that census's own count-only guard was shown to lose 5 of 6 sites silently.

**What this does NOT claim.** The census does not, and cannot, cover #768's actual defect —
a field that is never SET, not a constant compared against the wrong range. That shape (an
uninitialized/never-populated component silently zeroing a downstream gate) is a different
mechanism from "a hardcoded number vs. a decaying quantity", and is caught here only by the
behavioural pin (`TestPhase6Score_CGDN_InertAtAnyPositiveThreshold`), not by the AST census.
Naming this boundary matters more than pretending one machine check now covers the whole
class — see the census's own "what it does NOT catch" section, which states four further
gaps (inter-function taint, non-`.Weight`/non-Hebbian vocabulary, and others) rather than
implying completeness.

**Principle: the same review that measured #801 correctly getting the arithmetic right also
carried forward an overstated corollary (`r` unbounded) that a second, independent look
disproved — severity and correctness both need independent re-verification in EITHER
direction (principle #9), and a corrected record is worth more than a consistent one. And
the class both defects share — a constant a person chose once, compared against a quantity
that a decay pass moves 20x before anyone reads it — is now a standing, self-checking
machine test rather than something that needs a third independent analysis pass to
rediscover a third time.**
