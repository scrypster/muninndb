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
