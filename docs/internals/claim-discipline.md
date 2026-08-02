# Claim discipline

`docs/internals/testing-hermeticity.md` exists because a test that passes both ways proves
nothing. This document is the same idea one domain over.

**A claim that would read the same whether or not it were true is the documentation form of
a test that passes both ways.** "Every writer funnels through this" reads identically
whether six writers bypass it or none do. So does "the two percentages agree", and "this is
collision-free rather than heuristic".

The evidence for writing this down: across roughly eighteen review findings in a single
day, almost every defect was a claim outrunning its mechanism — and the *code* was usually
right. The defects lived in the parts of the repo nothing executes: comments, invariant
text, census docstrings, commit messages, measurement records. Those parts are read by
every future contributor and by every agent that plans a change, and they are the project's
reasoning substrate. An overstated invariant does not stay in `invariants.md`; it
propagates into every decision made by someone who believed it.

The rules below are cheap. Each one comes from something that actually shipped wrong.

---

## 1. A claim that names a set must be regenerable from a mechanism

Prose that says "the writers are A, B and C", "all four call sites", "every handler" is a
set claim. It ages badly and it is never re-derived, because re-deriving looks like work
that has already been done.

**Derive the set from the thing that must be true, not from what sounds relevant.**

The worked example is the one that failed first. An invariant named an "enumerated writer
set" for association edges. It had been assembled by reading method names — the functions
that *sounded* like association writers. It omitted three engram writers that reach the
same keyspace through inline `Associations` on an engram, all of them client-reachable from
REST, gRPC and MBP. Re-deriving the same set from **the key builders** —

```
git grep -n 'AssocFwdKey\|AssocRevKey\|AssocWeightIndexKey\|ArchiveAssocKey'
```

— returned 18 sites and surfaced the omission immediately. The question "who writes these
bytes" has a mechanical answer; "which functions are association writers" has an
associative one.

Where prose names a set, **a test should regenerate it and fail on drift**. The repo
already does this in several places, and they are the pattern to copy:

- `prefix.All()` as the single source of truth for Pebble prefixes, with disjointness
  tests derived from it (`internal/prefix/prefix_test.go`).
- `TestAppendMode_MethodCensus` — reflection over `*Engine`'s method set, so a new exported
  method cannot escape classification.
- `TestToolClassification_CoversAllRegisteredHandlers` — the registered handler map, not a
  hand-list of tool names.
- `TestPointGetReadersAreCovered` (`internal/storage/read_error_census_test.go`) — every
  method whose body calls `ps.pointGet` must appear in the behavioural table.

A hand-maintained list in a comment is acceptable only when nothing mechanical defines the
set. Say which it is.

## 2. A guard must state what it does not catch

A guard that cannot articulate its own boundary is pinning an instance and calling it a
class — and it reads, to everyone downstream, as coverage it does not provide. That is
strictly worse than no guard, because it stops the next person looking.

Two failures of this kind, both real:

- A source census that pattern-matched a read-error-laundering idiom claimed to pin the
  class. A full reintroduction of the original defect passed it in **four of the five**
  natural ways to write the same bug — renaming the error variable, adding a log line
  before the return, inverting the condition, or splitting an `||` into two `if`s, which is
  the most likely refactor of the very code it guarded.
- A `RelType` census recognised **one of four** legal declaration forms. Two of the forms
  that escaped it are *annotated* — `(RelType)` in parentheses parses as a `ParenExpr`, not
  an `Ident`.

Recognising syntax is always one form behind, and it is not decidable. **Membership in a
list is decidable.** The shape that worked, twice, in one day:

> a **behavioural** table — inject the fault, assert the observable behaviour — plus a
> **narrow completeness census** over a decidable list, whose only job is to prove the
> table has a row for every member.

`read_error_census_test.go` is the reference implementation, including its own boundary
section: it names `FlagContradiction` as a reader outside the `pointGet` seam and therefore
outside the census, tracked separately, "so nobody reads more into a green run than it
means."

Write the boundary as a `DOES NOT CATCH:` paragraph in the guard itself. If you cannot
write one, you do not yet know what the guard covers.

## 3. Invariant strength vocabulary

This is the highest-leverage rule in this document, because `invariants.md` is a source of
truth.

**Use *cannot* / *never* / *may only exist* only for structural unrepresentability**
(principle #3), and **state the structural reason inline** — the type system, the key
layout, a single writer, an interface that cannot satisfy the gate. "A `cap_` token cannot
mint a credential because it authenticates as `IsCapability`, never `IsAPIKey`, so it
cannot satisfy the mint gate" is the register: the claim carries its own mechanism, and a
reader can check it.

**Anything enforced by a check says *is refused unless*, and states its residual** — with a
number where one has been measured.

The instance: an invariant said an association edge "may only exist while both endpoints
live". The enforcement was check-then-write, and a forced-race harness measured **6–169
dangling rows per 400 races**. The wording claimed unrepresentability for a property with a
measured, non-zero residual. The corrected wording — "**is refused unless** BOTH of its
endpoints have a live 0x01 engram record" — is weaker, true, and tells the next reader that
a repair path is still needed.

A residual is not a weakness in the invariant. Hiding it is.

## 4. Measurement records carry denominator, N, machine, and null-or-not

Four rules, four instances:

- **A percentage without its denominator is not comparable to another percentage.** "The
  two percentages agree" was written about two numbers whose denominators differed by ~50×.
- **A result inside the run-to-run spread is a null and cannot corroborate an effect.** One
  of those same two numbers was explicitly annotated "inside the run-to-run spread" in the
  line above, and was then cited as corroboration.
- **A band from N runs on one machine is a sample, not a bound.** A benchmark band widened
  from 3 runs on one machine was exceeded by 5 of 6 independent runs.
- **State what the rate is the rate OF.** "~1 engram ID in 256" sat next to "on a delete
  path that is data loss". The 1/256 was the rate at which a *bound is loose*; actual loss
  additionally required a ~2⁻⁶⁴ ID coincidence. Both numbers were correct. Their adjacency
  was the claim, and it was false.

So: denominator, N, the machine, and whether the result is a null. Four extra words each.
And per CLAUDE.md, the corpus is "a production vault" — keep the numbers, drop the name.

`make corpora` exists so that "I re-ran the standing corpora and they did not move" is a
diff rather than an assertion. See below.

## 5. The general test

> **Would this sentence read the same if it were false?**

If yes, it is not carrying evidence. Add the mechanism, the denominator, or the boundary —
or weaken the sentence until it is true.

Applies to commit messages and PR descriptions too. "Every writer now funnels through the
guard" in a commit message is the same claim as in a comment, and it is the version future
readers find first.

---

## What this cannot catch

One of the day's findings was a test that had **pinned the defect as intended behaviour**,
with an accurate five-line comment explaining the exact bit pattern. Every claim in it was
true. Someone had traced the mechanism completely and concluded the code was correct. The
comment would not have read differently if it were false, because it was not false.

No amount of claim-testing catches that. It needed someone to ask **"should this be
true?"** rather than "is this true?" — a question about intent, which no census, no
denominator and no vocabulary rule reaches.

Stating this is not modesty. A convention that implied otherwise would be a claim
outrunning its own mechanism, in a document about claims outrunning their mechanisms.

Related and also uncovered: an explicit `//go:build ignore`, or a build tag nothing sets,
excludes a file just as completely as the filename constraint the guard below covers — and
it is visible in the file's own first lines, so nothing here looks for it.

---

## The guards this document ships with

Three mechanisms, all cheap. Each covers one way a green run meant nothing.

**`scripts/check-filename-build-constraints.sh`** (CI: `shellcheck` job; `make
check-filenames`). The Go toolchain excludes a file whose name ends in `_GOOS`, `_GOARCH`
or `_GOOS_GOARCH` — after stripping `_test` — on filename alone, with no `//go:build` line
and no diagnostic. In #814 a scratch file added as `zz_base_arm_test.go` was excluded on
every machine in the project, and `go test -run` reported

```
ok  	github.com/scrypster/muninndb/internal/prefix	0.386s [no tests to run]
```

with exit 0. **A RED-first check that runs zero tests is indistinguishable from a passing
baseline** — the "fails without the fix" evidence was produced by the toolchain, not
observed. The guard is an allowlist, not a pattern ban, because seven platform files here are
intentional; its token list is transcribed from the toolchain's own
`internal/syslist/syslist.go` and cross-checked against the installed toolchain on every
run, per §1. It is deliberately not a Go test: a Go test guarding build exclusion can
itself be build-excluded, and would then pass silently forever. Note `_unix` is **not** in
the token list — syslist.go says of `UnixOS`, verbatim, "This is not used for filename
matching", and `process_unix.go` is constrained by an explicit `//go:build` line instead.

**`cmd/muninn/integration_test.go`** (#812). `TestMain` used to print one line and
`os.Exit(0)` when something already held `:8750`. `go test -v` then produced zero `=== RUN`
lines and `ok`. On a maintainer machine with a daemon up, that is every run — so every
local "integration smoke: ok" for as long as it was believed. It now emits a real SKIP
record via a sentinel test, and exits non-zero when `MUNINN_REQUIRE_INTEGRATION` is set. CI
sets it: a runner has no daemon, so a busy port there means something is wrong.

**`make corpora`** (`scripts/run-corpora.sh`). "Re-run the standing corpora" was an
instruction with no referent. The standing corpora are the four measurement harnesses in
`internal/engine/activation`: `TestMeasureAbstentionGate`, `TestMeasureRecallQuerySet`,
`TestMeasureRelevanceBandHint`, `TestMeasureShadowPrecision_GraftedChain`. They already run
inside a full `go test ./...`, so the target adds no coverage — it produces the artifact.
Raw `go test -v` output is not diffable (interleaved timestamped INFO logs; source line
numbers that move whenever the harness is edited), so the script normalises those away.
Two runs of an unchanged tree are byte-identical; a change that moves a corpus shows up as
a numeric diff and nothing else. Measured once on an arm64 laptop: 22s wall-clock, 179 artifact lines; treat both as an
order of magnitude, not a bound. The
script also fails if any named harness did not run at all, which is the #814 lesson applied
to the script itself.

`testdata/bible/` was removed rather than populated. It held only a `.gitkeep`; its loader
(`scripts/eval-bible-setup.sh`) and consumer (`cmd/eval-bible/`) are gitignored internal
tooling, and `Makefile` already records why the targets went. An empty data directory reads
as "there is a corpus here" — the same class of claim — and it had been cited as if it were
the standing corpora.

---

## Audit list

Claims identified as needing correction on the day this document was written. **Do not
fix the branch-owned entries** — the owning branch has them, and a second fix means a
conflict in `invariants.md`, which three in-flight branches are already amending.

Verified at the commits named. Anything I could not verify is not listed.

| Claim | Where | Status |
|---|---|---|
| An association edge "may only exist while both endpoints live" — unrepresentability language for a check-then-write with a measured 6–169/400 residual | `docs/internals/invariants.md` STO-12 | **Corrected on `fix/803-dangling-association-edges` @71a54b6** — now "is refused unless BOTH of its endpoints have a live 0x01 engram record" |
| STO-12's "enumerated writer set", derived from method names, omitting three inline-`Associations` engram writers | same | **Corrected on the same branch** — `internal/storage/assoc_endpoints_writers_test.go` re-derives it "FROM THE KEY BUILDERS rather than from" names |
| "~1 engram ID in 256" adjacent to "on a delete path that is data loss" | `internal/storage/assoc_endpoints_cascade_test.go:207-211`, same branch | **Corrected on the same branch** — now states explicitly that 1-in-256 is the rate the bound is loose, ~2⁻⁶⁴ for loss |
| `normalizeEngramTimes` "every writer funnels through this"; six read-modify-write encoders do not | `internal/storage/`, `fix/810-clone-lastaccess-sentinel` @494c8b5 | **Corrected on that branch** — invariant text now scopes it to writers that CREATE timestamps, with `TestReadModifyWriteWriters_PerpetuateSentinel_DoNotHeal` |
| "collision-free rather than heuristic: … any timestamp inside UnixNano's defined range maps to itself" — self-refuting, the colliding instant is inside that range; duplicated in a second test's doc | same branch | **Corrected on that branch** — now "aliased, not collision-free", with the colliding instant named |
| A `RelType` census recognising 1 of 4 declaration forms, two of the escapees annotated | `internal/storage/ranking_neighbors_test.go`, `fix/800-assoc-symmetry-ranking` @1380560 | **Corrected on that branch** — `relTypeConstantNames` now anchors on the const block and unwraps `ParenExpr` |
| A source census claiming to pin the read-error-laundering class, caught 1 of 5 idioms | `internal/storage/read_error_census_test.go` | **Already corrected on `develop`** (#809, b272c74) — replaced by behavioural table + completeness census, and now the reference example for §2 |
| "The explicit guard below is LOAD-BEARING — on a delete path that is data loss", with two structurally identical unguarded delete loops nearby, one in the same function | reported against the association delete paths | **Owned by `fix/803-dangling-association-edges`** — its STO-11 text now enumerates all four destructive prefix scans and tables them |
| A benchmark band widened from 3 runs on one machine, exceeded by 5 of 6 independent runs | reported against a currency/latency benchmark | **Open, unowned.** Whoever restates the band: N, machine, and whether the widening was a null |
| **Two different `[STO-12]` invariants exist.** `fix/803-dangling-association-edges` @71a54b6 (`invariants.md:122`, association endpoints) and `fix/810-clone-lastaccess-sentinel` @494c8b5 (`invariants.md:122`, the ERF zero-time sentinel) each define STO-12 as a *different* invariant | `docs/internals/invariants.md`, two branches | **Open, unowned.** Both are correct in isolation; whichever merges second must renumber, and every reference to "STO-12" written before that merge is ambiguous. Found by grepping both worktrees, not by either branch |

The last row is itself an instance of §1: two branches each derived "the next free
invariant ID" from the file they had, and both derivations were locally sound.
