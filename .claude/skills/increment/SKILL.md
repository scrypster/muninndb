---
name: increment
description: The repeatable build loop MuninnDB holds itself to — design, build RED-first, independently vet, adversarially refute, MEASURE real value on a vault, then PR/CI/land. Use for any non-trivial feature or fix ("build X", "add Y", "next increment"). Encodes the discipline that keeps us shipping real, measured value instead of smoke-and-mirrors.
---

# increment — the MuninnDB build loop

This is the loop that landed the reflex stack, valid-time, importance, and more, each
adversarially reviewed and measured. It is how we chase real, measurable value and refuse fluff.
The north star: **a sentient-FEELING memory engine** — if the user has to say "remember when
so-and-so told me X," it has failed. The bar for every increment: **world-class,
enterprise-grade, measured — no smoke-and-mirrors, no marketing.**

## When to use
Any non-trivial feature or fix. For a one-line mechanical edit, just do it. For anything that
touches the cognitive core, storage, recall, or a public surface, run the loop.

## The non-negotiable principles (the lens)
1. **Measure, don't claim.** Every increment ships with a before/after on a real or labs vault
   that quantifies value. "It works" is not a result; "8/8 important facts survived pruning that a
   recency pruner wipes 0/8" is. If you can't measure it, you don't understand it yet.
2. **Defer-if-buggy over ship-to-hit-a-milestone.** If a refute pass finds a real defect you can't
   cleanly fix, ship the clean core and DEFER the broken part to a properly-designed increment.
   (We deferred importance-modulated decay + reinforcement after the refute found scoring-mode
   bugs; shipped only the measured pruning-protection core.)
3. **Honest negative results are first-class.** If the measurement shows no meaningful value, or a
   gate can't be cleared, HOLD and report — don't dress up noise. A killed idea is a real result.
4. **Never trust a sub-agent's "green."** Independently re-run build/vet/gofmt/`-race` yourself.
5. **Minimal, reviewable increments referencing their design.** No sprawling PRs. Name what you
   defer. Design docs live in `.claude/deep-review/`.
6. **The DB surfaces evidence, the LLM concludes.** Muninn provides substrate + candidates WITH
   evidence (counts, lift, provenance, time-alignment); it never asserts causation or "the
   insight." Any "discovery" analytic is read-only and never writes association weights (that
   creates a self-confirming loop). A candidate without its denominator is a plausible-wrong
   answer — the worst failure class.

## The loop
1. **Design (Fable).** Spawn a design agent grounded in the REAL code (read, don't assume) plus the
   relevant neuroscience / prior-art / failure post-mortems (learn from what others learned from;
   don't reproduce their mistakes). It must deliver: the mechanism, the minimal first-increment
   scope with explicit deferrals, the composition with what's landed, invariant impacts, the
   MEASURABLE proof, and top risks. Save it to `.claude/deep-review/<date>-<name>-design.md`.
2. **Decide.** Read the design. Surface any genuine product-behavior fork to the owner; otherwise
   pick the defensible default and proceed. For a contested call, a second deep-reasoning pass
   (Fable) or a design panel can decide.
3. **Build, RED-first.** In a fresh worktree off `origin/develop` (avoid the stale-checkout trap;
   copy the embed assets in for `-tags localassets`). For every behavior change: write the test,
   show it FAILS without the code, then implement. `-race` for storage / Hebbian-PAS / pruner /
   replication / MCP-session paths. May be delegated to a fork/build agent.
4. **Vet (you, independently).** `go build -tags localassets ./... && go vet -tags localassets
   ./... && gofmt -l .` clean; full `go test -tags localassets ./...`; `-race` on the hot packages.
   RED-check the key guards discriminate (toggle off → fail) using a `cp` backup, NEVER
   `git checkout` on files with uncommitted work.
5. **Adversarial refute.** Spawn an independent reviewer with a REFUTE mandate (try to break it,
   find the bypass/failure-ordering/invariant-violation). **Tier-3 (auth / on-disk format /
   migration / concurrency / crypto / replication / mbp wire type) → the refuter runs on Opus and
   is mandatory.** Reconcile: both clean → stands; a real evidenced defect → fix it; genuine
   security/correctness split → DEFER to the owner. Fix every real finding (add RED-proven guards).
6. **Measure — the acceptance gate.** Prove real value on a vault with a NON-GAMEABLE test.
   Prefer a control that fluff can't pass: a RED check (mechanism disabled → effect gone), and
   where correlation/discovery is involved, a **timestamp-shuffle / permutation null** (real signal
   collapses to the noise floor; popularity artifacts don't). Report the number.
7. **Land.** PR into `develop` (title + body naming what shipped + what's deferred, referencing the
   design). Watch CI to green (a background `gh pr checks --watch`). Admin-merge on all-green when
   authorized (`gh pr merge --squash --admin`); otherwise hand off. If a gate is red or a finding
   is unfixed, HOLD and report — do not merge.

## Contributor PRs
Don't bounce nitpicks back to a strong contributor, especially after multiple round-trips. Either
adjust their branch yourself (`maintainerCanModify=true`: merge develop INTO their branch, resolve,
push — a merge commit, not a force-push) or merge and do a small follow-up PR. Still hold the bar
(Tier-3 refute, CI green). Serialize merges that touch the same hot signatures — a parallel merge
silently dropped a feature once.

(How review responses get drafted is separate maintainer tooling and is not part of this repo.)

## Labs vs live
Prove mechanisms in an isolated labs daemon (own data dir + ports, `-tags localassets` +
`MUNINN_LOCAL_EMBED=1`) before pointing them at a vault you care about. Back up first
(`muninn backup --data-dir <d> --output <b>`, server stopped via `launchctl bootout`), save the old
binary for rollback, and on macOS **ad-hoc code-sign the new binary**
(`codesign --force --sign - <bin>` — launchd rejects unsigned binaries with
`OS_REASON_CODESIGNING`), then `launchctl bootstrap`/`kickstart` and verify: embed provider up, no
corruption, a positive recall, and any startup migration ran clean.

## Pipelining
While one increment's build/refute runs, design the next (agents notify on completion). Keep the
owner's roadmap and any contributor backlog both advancing. Run several loops in sequence for a big
push; stay in the loop between them.
