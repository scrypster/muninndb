# Test hermeticity in `internal/engine` — drain async before asserting

How to write a test that asserts on state produced by the engine's async workers without
it flaking under `-race` on constrained CI cores. Captures a multi-round CI-flake hunt
(PR #722, 2026-07) so the next person doesn't repeat it — including a wrong turn: the
initial hypothesis (wall-clock scoring time) was disproven by instrumentation, and the
real fifth source turned out to be the activation/Hebbian log drain. Both are recorded
below because the wrong turn is itself the lesson.

## The failure mode

`internal/engine` tests that assert on state produced by the engine's async workers flake
under `go test -race` on constrained CI cores (2 vCPU). The tell: a test passes locally at
`-count=1` but fails intermittently in CI, and different tests fail on different runs
(`TestProspectiveAcceptance_Gates1to3` one run, `TestFeedbackUseful_BumpsAccessCount` the
next). A recovered `WARN autoassoc job failed err="context canceled"` at teardown is the
smoking gun: a fire-and-forget worker straggled past `eng.Stop()`, i.e. the test asserted
before the async work landed.

**Root cause, always the same:** the test used `time.Sleep(200ms)` (or a single bounded
poll) as a stand-in for synchronization. `time.Sleep` is NOT a synchronization primitive —
under `-race` scheduling perturbation on busy cores, the awaited goroutine simply isn't
scheduled in time. Production tolerates this (eventual consistency by design); tests must
not.

## The complete catalog of async sources on the write/scoring path

A recall/score/count assertion in a test may depend on any of these; drain each. This is
the table as it shipped in #722 — verified against `develop`, not the draft assumption.

| # | Async source | What it feeds | Deterministic await |
|---|---|---|---|
| 1 | Association workers (autoAssoc, neighborWorker, goalLinkWorker) — fire-and-forget off `Write()`; create RelSupports/associations | `phase5Traverse` BFS candidate pool | `WaitIdle()` on each (`jobWG`-tracked) — folded into `Engine.waitWriteTimeIdle()` (`internal/engine/engine.go`) |
| 2 | Fire-and-forget goroutines (`RecordFeedback` TouchAccess/scoring, `Read` reinforcement, #682), tracked by `Engine.fireAndForgetWG` | AccessCount, reinforcement signal | `Engine.waitFireAndForgetIdle()` (`fireAndForgetWG.Wait()`) — separate from `waitWriteTimeIdle` because it drains a different WaitGroup |
| 3 | Async FTS indexer (`ftsWorker`, decoupled from write hot path) | FTS candidate search — the ONLY recall path when embeddings are noop/zero | `ftsWorker.Flush(timeout)` — folded into `waitWriteTimeIdle()` |
| 4 | Counter-coalescer (100ms flush ticker, `internal/storage/counter_coalescer.go`) | AccessCount/vault-count persistence → ACT-R base-level | white-box `counterCoalescer.flush()` called directly in `internal/storage` package tests; at the engine layer, `store.Close()` (not `db.Close()`) drains it before the store closes |
| 5 | Activation/Hebbian log drain (`ActivationEngine.logCh` → `drainLog` → `assocLog`, read by `phase4HebbianBoost` on the NEXT call) | Hebbian co-activation boost applied to candidates associated with something recently activated | `ActivationEngine.WaitLogIdle()` (`internal/engine/activation/engine.go`) — folded into `Engine.waitWriteTimeIdle()`. To reset the log between independent scripted calls (not just drain it), call `Engine.resetActivationLogForVault` (`internal/engine/engine.go`), which calls `ActivationEngine.ResetLog` → `activation.ActivationLog.ResetVault`, **after** `waitWriteTimeIdle`, never before — a reset issued while a drain is still in flight is silently undone by the late-arriving entry. |

The Push/prospective-acceptance harness needed 1+3+5; the reinforcement tests needed 2+4.
A test that touches recall ranking of freshly-written data plausibly needs all five —
call `waitWriteTimeIdle()` (which covers 1, 3, 5) and, if the assertion also depends on
feedback/reinforcement or persisted counts, `waitFireAndForgetIdle()` and a counter flush
too.

**A hypothesis that didn't survive verification:** the #722 draft initially attributed the
fifth flake source to wall-clock scoring time — the idea that `now := time.Now()` inside
`computeACTR`'s base-level/recency math could nondeterministically push a candidate across
the recall threshold between a write and a scripted `Activate()` a few milliseconds later,
and that the fix would be an injectable clock. Instrumentation disproved this: there is no
injectable clock in `internal/engine/activation` — production still calls `time.Now()`
directly (`engine.go`) — and freezing time did not make the flake go away. The actual
culprit, found by tracing which goroutine was still running when the assertion fired, was
source #5 above: `phase4HebbianBoost` reading `assocLog` before `drainLog` had applied the
previous call's entry, nondeterministically including or excluding a Hebbian boost that
decided which of two near-tied candidates ranked first. **The lesson generalizes past this
one bug: verify a flake's cause by instrumentation before building the fix — a plausible
timing story (wall-clock races) can be wrong, and the fix for the wrong cause (a clock
injection nobody needed) would have shipped complexity while leaving the real flake in
place.**

## The helper pattern (how to add a drain correctly)

- **WaitGroup discipline**: `Add(1)` in the SAME non-blocking `select` as the enqueue
  send, so a dropped job (default branch) does `Add`+`Done` net-zero and a sent job gets
  exactly one `Done`. Put `Done()` (and the job context's `cancel()`) in a **`defer` inside
  a per-job function** — a bare `Done()` in the run loop is skipped if the handler panics,
  and since these loops have no `recover()`, that would hang a later `Wait()` (a leaked
  `Add`). Extract the per-job body so the defer scopes per-iteration.
- **Test-only + UNEXPORTED**: expose the drain as an unexported method
  (`waitWriteTimeIdle`, `waitFireAndForgetIdle`, `WaitLogIdle` — the last is exported
  because `activation` is a separate package from its white-box `engine` callers, but its
  doc comment says plainly that production never calls it) called from white-box
  `package engine`/`package activation` tests. An EXPORTED `*Engine` method trips the
  append-mode invariant (`append_mode_test.go` requires every exported method be
  classified into `guardedOps`/`appendAdditive`/`appendInfra`/`appendReadOnly`) — don't add
  test-only sync to the public `*Engine` API.
- **Zero production behavior change**: these WaitGroups/channels are `Add`/`Done`'d (or
  sent/drained) on the hot path already; the drain only adds the ability to `Wait()`,
  which production never calls. Injectable clocks, where they don't exist, should not be
  invented just to chase a flake — confirm the real cause first (see above).

## Proving it — and the runaway to avoid

Verify with a BOUNDED run: `GOMAXPROCS=2 go test -race -run <TheTest> -count=50` (≤2 min
for a ~2s test). `-race` alone is sufficient scheduling stress.

**Do NOT** prove hermeticity by piling artificial load: a prior attempt ran `-count=20`
under 8× background `yes` processes, repeatedly, leaking the `yes` processes each round
until 132 accumulated and load average hit 185 (~10x the box), starving everything. Rules:
no background load generators, `-count` ≤ 50, one test invocation at a time. If a bounded
run still flakes, find the missing async source in the table above — don't crank the
load, and don't reach for a fix (like an injectable clock) before instrumentation confirms
which source is actually racing.

## Standing obligation

See `drift-and-obligations.md` obligation 12: any new async worker or fire-and-forget path
on the write or scoring path needs a `WaitIdle`/`Flush` seam, folded into
`waitWriteTimeIdle()` (or documented here as its own drain), and any test asserting on its
output must drain it deterministically — never `time.Sleep`.
