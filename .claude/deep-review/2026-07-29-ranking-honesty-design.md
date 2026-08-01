# Ranking-honesty pack — design (DESIGN step of /increment)

**Date:** 2026-07-28 (doc filed under the 07-29 slot per task spec)
**Verified against:** `origin/develop` @ `28345a9` (fetched; local checkout `develop` @ `24f3b46` is 2 docs/test commits behind — none touch recall/scoring; every line reference below was read from the working tree and cross-checked against the two missing commits: `d31f58d` is a skill doc, `28345a9` adds `engine_entity_boost_test.go` coverage only).
**Scope guard honored:** nothing here touches `internal/prospective/`, notices/intend, or prefix 0x2D.

The north star: a result must never claim a confidence/score it didn't earn, and a
threshold must gate the number the user believes it gates (principles #1/#2).

---

## 1. Ground truth — how threshold, scoring, sort, and surfaced score actually work

### The pipeline (read, not assumed)

`internal/engine/activation/engine.go` `Run()` (line 433) is the 6-phase pipeline.
Phase 6 (`phase6Score`, line 1345) has **four scoring paths**, selected by resolved
weights: RRF fusion, CGDN (experimental-gated), ACT-R (production default), legacy
weighted-sum. In **every** path:

- The final score is a composite. ACT-R (line 1683–1692):
  `final = min(raw*scale, 1.0) * eng.Confidence` where
  `raw = contentMatch × softplus(B(M) + hebScale·hebbian + hebScale·transition) / 1.693`
  (`computeACTR`, line 1905). RRF (line 2008): `final = rrfScore × (1 + hebbian + transition) × confidence`.
- The threshold gate is `if final < req.Threshold && !c.inTagPool { continue }` —
  identical at all four cut-sites (lines 1543, 1633, 1687, 1708). **The threshold gates
  `final`, the blended composite — never raw cosine.**
- The sort key is `final` (`sort.Slice(... scored[i].final > scored[j].final)`,
  lines 1565, 1644, 1694, 1713).
- The surfaced score IS the sort key: `ScoredEngram.Score = s.final` (line 1760), with
  the full component breakdown (`ScoreComponents`, incl. `SemanticSimilarity` = true
  cosine) carried alongside.

Threshold plumbing, outermost-in:

1. **MCP** `handleRecall` (`internal/mcp/handlers.go:347`): `threshold := float32(0.5)`
   **unconditionally** when the arg is absent; clamped to [0,1]. Tool schema
   (`internal/mcp/tools.go:174`): `"Minimum relevance score 0.0-1.0 (default 0.5)"`.
2. **REST** (`internal/transport/rest/server.go:859`): passes the JSON value through
   (0 when absent), applying a recall-mode preset threshold only if set.
3. **Engine** `activateCore` (`internal/engine/engine.go:2183–2185`):
   ```go
   if actReq.Threshold == 0 {
       actReq.Threshold = 0.1
   }
   ```
   This runs **before** `activation.Run()` on every transport path (MCP, REST, gRPC,
   MBP, embedded `DB.Recall` — root `recall.go` passes no threshold at all → 0 → 0.1).
4. **`Run()`** (`activation/engine.go:440–448`, the #590 fix): mode-appropriate default
   **only when `Threshold <= 0`** — RRF → 0.001, otherwise 0.05 — and never tramples an
   explicit value.

### Verdict on suspected problem 1 — "threshold gates a blended score, not cosine"

**Partially refuted, and confirmed in a sharper form.**

- *Refuted as stated:* no surface promises "cosine ≥ X". The MCP schema says "minimum
  relevance score", and the number the threshold gates is exactly the number surfaced
  as `score` — gate, sort key, and surfaced score are the same value. The separately
  surfaced `vector_score` (`internal/mcp/convert.go:49`) is the true cosine, never
  conflated. (`muninn_similar_entities`' threshold at `handlers.go:1695` really is a
  trigram similarity and is documented as such — non-issue.)

- **Confirmed in a sharper form — #590's fix is dead code on every production path.**
  `activateCore` coerces `0 → 0.1` (engine.go:2183) *before* `Run()` is called, so
  `Run()`'s no-threshold branch — the entire #590 mechanism — is unreachable from MCP,
  REST, gRPC, MBP, and the embedded API. MCP additionally pre-fills 0.5. Consequence,
  for a vault with `scoring_fusion: "rrf"` (a first-class option, exposed in the web
  console's Search Scoring card **by #590 itself**):

  - RRF finals are rank-based: max ≈ `1/41 + 1/61 + 1/121 ≈ 0.049`, ×(1+boosts)×conf,
    realistically ≤ ~0.15 and typically < 0.05.
  - MCP default-threshold recall gates at **0.5** → zero results (except tag-pool
    bypass hits). REST/embedded default gates at **0.1** → near-zero results.
  - The regression test that guards this exact ship-blocker
    (`TestRRF_ReturnsResultsWithDefaultThreshold`, `activation_test.go:1456`) passes —
    because it calls `Run()` directly with `Threshold: 0`, a call shape production
    never produces. The test is green while the bug it documents is live end-to-end.

  This is silent-wrongness of the flagship kind: "no memories matched" from a vault
  full of relevant memories, with no warning, on a configuration the UI offers.

  Corroborating in-tree evidence that this is known-but-unowned:
  `internal/engine/engine_entity_boost.go:34–44` ("CALIBRATION CAVEAT: … On the
  activateCore path the request threshold is defaulted to 0.1 before activation.Run,
  so under RRF defaults no result clears the seed rule …").

  Also: **invariants.md COG-6 is stale and self-contradictory.** It reads "RRF mode
  forces threshold to 0.001 when the caller threshold ≥ 0.01" — that is the **pre-#590
  clobbering behavior** #590 removed — then in italics claims the opposite ("an
  explicit caller threshold under RRF is respected, not clobbered"). Neither clause
  describes what production does (production: RRF thresholds are 0.5/0.1 defaults that
  filter everything).

### Verdict on suspected problem 2 — "surfaced score doesn't match the sort key"

**Refuted. The ranking is honest here.**

- All four Phase-6 paths sort by `final` and surface `final` as `Score`.
- The post-pipeline entity-boost phase (`engine_entity_boost.go`, pass 2a ~line 205)
  adds the boost to `Score`, sets `Components.EntityBoost`, updates
  `Components.Final = Score` ("Keep the reported Final consistent with the adjusted
  Score"), re-sorts descending and re-truncates (pinned by COG-18).
- Supersession annotation does not reorder.
- One cosmetic dishonesty, noted for a deferral, not an increment: in RRF mode,
  BFS-traversed candidates route their propagated graph score into the
  `hebbianBoost` field (`activation/engine.go:1402–1414`, deliberate per COG-8), so
  the surfaced `HebbianBoost` component for those rows is not a Hebbian association
  strength. Ordering and `Score` remain honest; only that one component label lies.
  Defer (component-level relabel; zero ranking impact).

### Verdict on suspected problem 3 — "fabricated confidence/score numbers"

**Confirmed — one real confabulation, two non-issues.**

- **`internal/cognitive/contradict.go:100–103` — live confabulation with teeth.**
  ```go
  if severity <= 0 && a.RelType == b.RelType && a.TargetHash != b.TargetHash {
      // Same relation type pointing at different-concept targets.
      severity = 0.8
  }
  ```
  Three distinct lies stacked:
  1. The comment says "different-**concept**-hash targets"; the write path
     (`engine.go:1346`) actually populates `TargetHash = hashString(assoc.TargetID.String())`
     — the **target ID** hash. So the rule fires for *any* two same-RelType
     associations from a new engram to two different targets. Storing one memory with
     `references → A` and `references → B` (a completely ordinary shape) declares
     **A and B contradict each other**.
  2. The severity 0.8 is invented — no semantic signal of any kind was consulted.
  3. Consequences are real, not cosmetic: the pair is persisted as a 0x0A contradiction
     key (`FlagContradiction`, `storage/association.go:792`), surfaced by
     `muninn_contradictions` (`mcp/handlers.go:664`), forwarded to triggers with the
     mislabeled type `"semantic"` (`engine.go:1357`, `1901` — it is a relation-matrix
     heuristic, not semantic analysis), and — worst — both engrams take a **confidence
     penalty** via `ConfidenceUpdate{Evidence: EvidenceContradiction}` with evidence
     weight 0.1 (`cognitive/confidence.go:15`). A fabricated contradiction actively
     drags real memories' stored confidence down. This is #611-class: filed as a
     "number attached" cosmetic; verification raised it (severity can go up).
- Non-issue: `brief.Scorer{Threshold: 0.72}` (`engine.go:802`) — that threshold gates a
  genuine sentence-level cosine; the comment is accurate.
- Non-issue: `muninn_link` default `weight = 0.8` (`mcp/handlers.go:641`) — a documented
  parameter default the caller can override, not a claimed measurement.

---

## 2. The minimal first increment (and what it defers)

**Increment R1 — "the threshold gates what it says, in the mode you're in."**
Composes with #590: it makes #590's already-landed mode-aware defaulting *reachable*,
rather than redoing it.

1. **Make `activateCore`'s default mode-aware** (`internal/engine/engine.go:2183`):
   ```go
   if actReq.Threshold == 0 {
       if resolved.ScoringFusion == "rrf" {
           // leave 0 — Run() applies the RRF default (0.001), #590's mechanism
       } else {
           actReq.Threshold = 0.1 // unchanged production default for ACT-R/weighted_sum
       }
   }
   ```
   (Equivalent one-liner: skip the coerce when fusion is rrf.) ACT-R/weighted_sum
   default behavior is **bit-identical to today** — no silent substitution for the
   majority path.
2. **MCP stops pre-filling 0.5 for rrf vaults** (`internal/mcp/handlers.go:347`): when
   the `threshold` arg is absent, consult vault plasticity (the handler already calls
   `GetVaultPlasticity` for the empty-result hint at line 560); if
   `ScoringFusion == "rrf"`, send 0 (→ engine → `Run()` default 0.001). Otherwise keep
   0.5 exactly as today.
3. **Degrade loudly, never silently-empty** (principle #2): in `handleRecall`'s
   existing zero-results hint block (handlers.go:558–564), when the vault's fusion is
   rrf and the caller's *explicit* threshold ≥ 0.01, append: "this vault uses rrf
   (rank-based) scoring — scores rarely exceed ~0.15; a threshold of X filters
   everything; try ≤ 0.01." Explicit values are still honored (no clobbering — that is
   #590's contract and COG-6's intent).
4. **Keep entity boost inert under RRF *on purpose*** : today it is a no-op under RRF
   only *by accident* of the 0.1 threshold (its own comment says so, engine_entity_boost.go:34).
   Restoring the 0.001 default would activate a boost path whose cap (0.30) is
   calibrated to the ACT-R scale and would dominate genuine RRF scores (~0.05).
   Add an explicit `if UseRRFFusion { skip boost pass; debug-log }` so today's
   effective behavior is preserved deliberately and loudly. (Principle #8: say so and
   document the residual.)
5. **Docs that must move in the same PR**: rewrite stale COG-6 (see §3); fix the MCP
   tool description's threshold text; note the mode-dependent score scale in
   `muninn_guide` if it mentions thresholds.

**Increment R2 (separate PR) — "contradictions are found, not invented."**
Delete the `severity = 0.8` same-RelType/different-target rule in
`contradict.go:100–103` (relation-matrix contradictions — Supports↔Contradicts,
PrecededBy↔FollowedBy, and the explicit-`Contradicts`-link path at engine.go:2740 —
all remain). Rename the fabricated trigger type `"semantic"` at engine.go:1357/1901 to
`"relation_matrix"` (cross-surface: webhook/trigger consumers — flag in PR body).
Result: no more fabricated 0x0A flags, no more unearned confidence penalties.

**Explicit deferrals** (named, per principle #5):
- Unifying the divergent surface defaults (MCP 0.5 vs REST/embedded 0.1) — a real
  drift, but changing either shifts result sets for existing users; needs its own
  measured increment.
- Recall-mode preset thresholds (semantic=0.3 etc., ACT-R-calibrated) under rrf vaults
  — covered interim by the R1.3 hint.
- Entity-boost recalibration for RRF scale — already owned by the #570-review follow-up;
  R1.4 just makes the current state deliberate.
- RRF per-query score normalization to [0,1] — **considered and rejected**: dividing by
  the per-query max makes every query's top hit score 1.0 regardless of quality — a
  *new* fabricated number. ACT-R's rescale only triggers on saturation (>1.0); RRF has
  no such saturation semantics.
- The RRF-mode `HebbianBoost` component mislabel for BFS candidates (§1, problem 2).

---

## 3. Invariant impacts

- **COG-6 must be rewritten** — it currently describes the pre-#590 clobber. Proposed
  text: *"The effective default threshold is mode-aware and decided in exactly one
  place per path: transports/engine pass 0 ('unset') through for rrf vaults;
  `activation.Run()` applies rrf→0.001 / otherwise→0.05; `activateCore` applies 0.1
  for non-rrf. An explicit caller threshold is never modified. Pinned by
  `TestActivate_RRFDefaultThreshold_EndToEnd` (engine level, not Run() level)."*
  The new pin lives at the **engine** layer precisely because the existing Run()-level
  test stayed green while production was broken.
- **COG-5 (tag bypass)** — untouched; the `!inTagPool` bypass at all four cut-sites is
  unchanged by R1.
- **COG-18 (entity boost contract)** — R1.4 adds "and the boost pass is explicitly
  skipped under rrf fusion (scale mismatch, #570 follow-up)"; amend the invariant text.
- **COG-8 / COG-9 / COG-19** — unaffected (no change to traversed-candidate routing,
  soft-delete filtering, or the validity gate).
- **Keyspace registry** — no new prefixes; R2 only *stops writing* some 0x0A keys.
  (No cleanup/migration of previously fabricated 0x0A flags in R2 — `muninn_contradictions`
  pairs can be resolved via the existing `ResolveContradiction` path; note as residual.)
- **New pinned invariant needed?** Yes — the COG-6 rewrite above *is* it, plus a pin
  for R2: relation-matrix contradiction severities come only from `ContradictionSeverity`'s
  matrix (1.0 / 0.9), never synthesized elsewhere.

## 4. The measurable proof (non-gameable, CI-cheap)

All table-driven / in-memory; no integration tag, no Playwright, no -race requirement
(nothing concurrent changes).

**R1 metric — the lie, quantified.** New engine-level test (in `internal/engine`, using
the existing in-memory harness): synthetic vault, 20 engrams with FTS+vector signal,
vault plasticity `scoring_fusion: "rrf"`.
- **Before (HEAD):** `Engine.Activate` with `Threshold: 0` (the REST/embedded default
  shape) returns **0 of 20**; the MCP handler shape (threshold 0.5) returns **0 of 20**.
  The lie in one number: *default recall under a UI-offered scoring mode has recall@10
  = 0.0 on a vault where all 20 memories are relevant.*
- **After:** ≥ 9 of the top 10 expected engrams surface (recall@10 ≥ 0.9); every
  returned `Score` ≥ 0.001; and — the honesty half — the **ACT-R control**: identical
  request against an actr-default vault returns a result set byte-identical to HEAD's
  (pins "no silent substitution for the majority path").
- **RED check:** re-add the `actReq.Threshold = 0.1` coerce for rrf (one-line revert)
  → the end-to-end test fails with 0 results. Must be demonstrated in the PR (the
  existing Run()-level test does NOT fail under this revert — that asymmetry is the
  whole point and should be stated in the test comment).

**R2 metric.** Unit test on `ContradictWorker.processBatch` + engine-level write test:
- **Before (HEAD):** storing one engram with `references → A` and `references → B`
  produces 1 contradiction flag (severity 0.8, type "incompatible_relations") and a
  confidence drop on both A and B (assert `GetConfidence` decreases). *One ordinary
  write fabricates one contradiction and damages two memories' confidence.*
- **After:** 0 flags, confidence unchanged; **and** the honest positives still fire:
  Supports↔Contradicts pair still flags at 1.0, explicit `RelContradicts` link path
  still flags (M honest detections preserved — count asserted).
- **RED check:** restore the `severity = 0.8` branch → test fails.

## 5. Cross-surface obligations (per drift-and-obligations.md)

- **Obligation 1 (MCP handler touched):** run the registry-parity smoke
  (`go test -tags integration,localassets ./cmd/muninn/...`); no new tool, so
  `allMCPTools` is unchanged, but the run is still required. Update the
  `muninn_recall` threshold description in `tools.go:174` (drop the flat
  "default 0.5"; state the rrf-vault behavior).
- **Obligation 2 (REST):** `openapi.yaml` threshold param description — document the
  mode-aware default; `npx @redocly/cli lint`.
- **Obligation 3 (SDKs):** grep `sdk/python|node|php` for hardcoded threshold defaults
  or "0.5" doc strings; no wire change, so code parity is doc-only. 🪝 manual.
- **Obligation 4-class (web console):** the #590 Search Scoring card
  (`web/templates/index.html`, `web/static/js/app.js`) should gain one sentence on the
  rrf score scale so a user who flips the card understands why 0.5-style thresholds
  don't apply. No preset values change (COG-4 untouched).
- **muninn_guide** (`internal/mcp/guide.go`): add the score-scale note if thresholds
  are mentioned.
- **R2 trigger type string** `"semantic"` → `"relation_matrix"`: notify in PR body —
  webhook consumers may match on it (fail-open presentation change, but say it loudly).
- **docs/internals/invariants.md**: COG-6 rewrite + COG-18 amendment ship in the same
  PRs as the code they pin.

## 6. Top 2 risks & mitigations

1. **Behavior shift for existing rrf vaults** — vaults living with (unknowingly empty)
   rrf recall suddenly return results; any downstream consumer that "relied" on
   emptiness changes behavior. *Mitigation:* this is the fix working; call it out in
   release notes; the ACT-R control test pins that no other mode moves; the hint (R1.3)
   covers the explicit-threshold case without clobbering.
2. **Entity boost accidentally re-enabled under RRF** by the lower effective threshold,
   letting +0.30-scale boosts dominate ~0.05-scale content scores (the exact scenario
   the engine_entity_boost.go caveat warns about). *Mitigation:* R1.4's explicit skip,
   with its own test (boost pass not entered when `UseRRFFusion`), RED-checked by
   removing the skip and observing an injected engram outrank all content matches.

## 7. Mistakes NOT to reproduce

- **#582/#585/#589 (silent substitution):** never clobber an explicit threshold; the
  rrf mismatch is answered with a default-path fix plus a *loud hint*, not a rewrite of
  the user's number. #590 already litigated this — build on its contract.
- **#590's own gap:** a regression test at the wrong layer (Run()) stayed green while
  every production transport was broken. Pin at the layer where the bug lives
  (engine/MCP), and RED-check against the *production* call shape.
- **Per-query normalization as a "fix":** rescaling RRF to [0,1] would manufacture
  absolute-looking scores from relative ranks — trading one dishonesty for another.
- **Stale invariants:** COG-6 drifted into describing removed behavior. The rewrite
  ships in the same PR as R1; an invariant that contradicts the code is worse than none.
- **Taking the "low-risk cosmetic" framing on faith (#611):** the contradict.go item
  was flagged as "a number attached"; verification showed it corrupts stored
  confidence. Severity went up.

## 8. Tier

**Not Tier-3.** No auth surface, no on-disk format change (R2 writes *fewer* 0x0A keys,
same encoding), no migration, no wire-type change (the "ThresholdSet flag on
mbp.ActivateRequest" design was rejected partly to avoid touching the MBP wire struct),
no new concurrency (contradict worker change is a branch deletion inside an existing
single-worker batch loop). Standard build/test gate with `-tags localassets`; `-race`
not required by the touched paths, though the contradict-worker test will naturally run
under the existing cognitive-worker race jobs.
