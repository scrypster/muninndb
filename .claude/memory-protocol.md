# Memory proposals — how a finding survives the session

A session produces knowledge. Most of it is recoverable from git, the PR, or the issue
tracker. A little of it is not, and that little is what memory is for.

The failure this protocol exists to fix: an agent emits a finding as prose, the prose goes
into a chat transcript, the transcript ends. Persistence that depends on someone
*remembering to remember* does not happen under load — this project measured that
(a 4.81% declaration rate that three separate interventions failed to move) and then
demonstrated it on itself, losing a day of findings while building the memory engine.

So: **do not print a memory. Append it to the ledger.** The ledger is a file, and files do
not evaporate.

## The bar — what qualifies

A proposal must be **durable, non-obvious, and not recoverable elsewhere.**

Propose:
- A measured result and the number (`decay was a 13.5-minute half-life since February`).
- A decision and the reason it beat the alternative, especially when the reasoning is what
  will be re-litigated later.
- An honest negative — a thing that did not work, with the evidence that killed it. These
  are among the most valuable memories the project has, because they stop the idea being
  re-proposed.
- A defect *pattern*, as distinct from a defect. "Three passes found thirteen findings and
  they are one bug" is durable; the thirteen are in the PR.
- Context about a person or a working relationship that a future session would otherwise
  have to rediscover.
- A trap: a thing that looks safe and is not.

Do **not** propose:
- Progress narration. "Ran the tests, they passed."
- A restatement of a diff, a commit message, or a PR body. Git has those.
- An issue's contents. The tracker has those.
- Anything you would have to look up again anyway to trust it.
- Five variations of one idea. One concept per memory, atomic. If it needs "and", it is
  probably two memories.

The bar exists because **a noisy vault is worse than a small one.** That is measured here
too: a pollution analysis found 74% of supersession pairs were template junk, and it
collapsed the analysis that depended on them. A drain with no bar is a pollution pump.

## How to append

**Use the helper.** It validates before it writes, fills in `vault`, and refuses a batch
rather than queueing a bad line:

```sh
node .claude/hooks/memory-propose.mjs <<'JSON'
{"concept":"short label","content":"the fact itself, self-contained, readable in a year","summary":"one line","type":"fact","tags":["…"],"source":"adversary"}
JSON
```

It takes a single object, a JSON array, or one object per line. `--check` validates without
appending. If a record is rejected, **nothing is appended** and the error names the field —
fix and re-send the whole batch.

Raw appends to `.claude/memory-proposals.jsonl` (gitignored) still work — never rewrite or
reorder the file, append only — and `ledger-guard.mjs` will flag a malformed one while the
session that wrote it can still fix it. But the helper is the path that cannot be wrong.

```jsonc
{
  "vault":    "muninndb",          // required — the helper fills this in
  "concept":  "short label",       // required
  "content":  "the fact itself",   // required — self-contained, >= 40 chars, readable in a year
  "summary":  "one line",          // strongly preferred
  "type":     "fact",              // fact|decision|observation|issue|procedure|constraint|…
  "tags":     ["…"],
  "entities": ["…"],               // bare names are fine
  "importance": 0.8,               // 0.7+ is protected from capacity pruning
  "source":   "adversary",         // which agent or session proposed it
  "supersedes_hint": "…"           // optional: a phrase or ULID this likely replaces
}
```

The schema lives in exactly one place — `.claude/hooks/memory-schema.mjs` — and the
producer, the guard, the drain and the migration all import it. It is enforced there
because prose did not enforce it here: 43 of the first 179 proposals were permanently
invalid (31 omitted `vault`, 12 used `title`/`body`), three incompatible schemas in one day
against a shape documented that same morning, and the failures came in *contiguous runs* —
one agent invocation getting it wrong for its whole batch.

Write it self-contained. A memory that only makes sense next to the conversation that
produced it is not a memory, it is a comment.

**Privacy is not relaxed here.** The ledger is a real artifact and it is subject to the same
rule as committed content: measure on real vaults, never name them. No vault names, no
client or tenant identifiers, no memory content copied out of someone's vault, no
commercial terms. The pre-commit guard scans it, and it is gitignored so it cannot land by
accident — but the guard is a backstop, not permission.

## How it drains

It drains itself. `PreCompact`, `SessionEnd` and a debounced `Stop` are wired in
`.claude/settings.json`; that file's `_trigger_rationale` records the measurement behind the
choice (over 55 session transcripts, the two sessions holding 81.3% of all events fired
`SessionEnd` **zero** times and compacted 18 times between them, so `PreCompact` leads).

To run it by hand: `node .claude/hooks/memory-drain.mjs` (`--dry-run` to see what it would
do, `--max N` to cap a run).

**The drain is a pipe, not a curator.** Identity → write → archive → truncate. It performs
no recall, computes no relevance band, and holds nothing. Cost is O(proposals appended since
the last run).

Its contract, in the order the properties matter:

1. **Nothing is lost, including concurrently.** A proposal that cannot be written *this
   time* stays in the ledger; one that can *never* be written moves to
   `memory-proposals.deadletter.jsonl` with the reason, so the queue can reach empty; a
   proposal appended *during* a run survives it. The drain consumes only the byte range it
   read, splices anything appended past that offset back in verbatim, and refuses to
   rewrite at all if the region it consumed changed underneath it.

   The consumed range stops at the **last newline**, never at the file size: an
   unterminated trailing line is a writer mid-write, and dead-lettering it would rule a
   transient state permanently invalid. It stays in the tail until the rest of it arrives.

   A **producer never fails on a held lock** — it appends anyway and says so on stderr. An
   unlocked append lands past the offset the drain pinned, which is the tail the splice
   preserves; refusing to append would convert a held lock into a lost finding, which is the
   one failure this whole mechanism exists to prevent.
2. **Idempotent, exactly.** Every proposal carries a content-derived `op_id` and the server
   holds an idempotency receipt for it, so a re-drain returns the existing engram
   (`idempotent: true`) rather than minting a second. That is an O(1) exact check with no
   embedder — as opposed to asking a similarity score whether two things are the same
   thing. (Receipts expire after 30 days, which is far longer than a line can survive in the
   ledger now that the drain is wired.)

   **Identity is `vault` + `concept` + `content`, and nothing else.** `summary`, `type`,
   `entities`, `tags` and `importance` are annotations on that identity: they are written on
   a first write, and a re-proposal that *corrects* one of them does **not** apply it,
   because the op_id is unchanged and the server returns the existing engram untouched.
   Including them in identity would mint a rival near-duplicate for a one-word summary edit,
   which is exactly the pollution this design exists to avoid — so the drain instead says so
   out loud: a `NOT APPLIED` line, `counts.unapplied_annotations` in the receipt, and
   `annotations_not_applied` on the archive record, which also keeps the corrected text.
   **Correcting a live memory is `muninn_evolve`'s job, not a re-proposal's.**
3. **Observable from outside.** *Every* invocation writes `memory-drain-receipt.json` and
   appends to `memory-drain-receipts.jsonl` — including the no-op, debounced, locked and
   failure paths — carrying timestamp, trigger, items considered, items acted on, and
   per-outcome counts. Before this, the archive was appended only on success, so "never
   invoked", "invoked, ledger empty" and "invoked, everything held" were the identical
   filesystem state: no file. Three sessions could only answer "has this ever run?" by
   inference.

   A receipt nobody reads is not observability, though — it is a file. So
   `memory-freshness.mjs` runs at `SessionStart`, reads the receipt and the queue, and says
   something **only when something is wrong**: an unhealthy last outcome, proposals still
   queued, or a ledger with no receipt at all. It never blocks and always exits 0. Without
   it the receipt was write-only, and a seconds-old receipt reading `debounced` while
   findings rot looks like an answer rather than the absence of one.

   Two bounds keep the receipt honest under failure: every daemon call has a per-request
   timeout (`--timeout`, default 10 s) and the run has a deadline (`--deadline`, default
   45 s, under the hooks' 60 s kill), and `SIGTERM`/`SIGINT`/`SIGHUP` release the lock and
   write an `interrupted` receipt. Before that, a daemon that accepted the connection and
   never answered hung the drain until the harness killed it — leaving the lock on disk and
   **no** receipt, so `stat` returned the previous one, fresh-looking and wrong, and the
   leaked lock blocked producers for the ~10 minutes until the stale breaker fired.
   `SIGKILL` remains unrecoverable by construction; the stale-lock breaker is what covers it.

   The receipt's `last_run_at` — the debounce watermark — advances **only** on an outcome
   that actually consumed from the ledger (`ok`, `partial`, `empty`, `no-ledger`,
   `rewrite-refused`). It does not advance on `locked`, `unreachable`, `error`,
   `interrupted` or `debounced`. It only ever moves forward and an untouched ledger's mtime
   does not move at all, so one failed run that advanced it disabled the debounced `Stop`
   trigger *permanently* — for exactly the crash/kill case `Stop` exists to cover.
4. **Quiet in a hook, loud by hand.** Invoked as a hook it always exits 0 and the receipt
   carries the truth — a hook that reports failure at every session close is a hook people
   disable, which is the failure this mechanism exists to prevent. Invoked manually it exits
   non-zero when a write genuinely failed.

### What the drain deliberately no longer does

It used to recall the neighbourhood before each write and **HOLD** anything with a
strong-banded match, so a script would never mint a rival copy. That claim is retracted: the
gate never executed once in its lifetime, and four defects lived in it —

- it cost ~0.9 s/proposal, which is what made the drain unusable in a hook;
- it was structurally dead on any vault whose fusion mode is `rrf` or `weighted_sum`, and
  whenever semantic search is degraded, because the engine then bands every row
  `uncalibrated` and `strong` is never emitted. The drain wrote everything while reporting
  "0 held" — indistinguishable from "0 duplicates found". (Generalisable: any consumer
  branching on `relevance_band` must read `uncalibrated` and `filter_match` as *absence of
  measurement*, never as a low band.)
- a kill mid-loop left engrams written and the ledger unchanged, so on the next run each
  proposal recalled its own freshly-written self, banded strong, and was HELD **forever**;
- HELD was unbounded and grew *because* the drain succeeded, since every write densifies the
  neighbourhood the next proposal is tested against.

The generalisable shape: **a gate whose discriminating variable moves at a rate correlated
with the phenomenon it suppresses fails anti-correlated with need.**

### Curation — deferred, not dropped

"Does this finding supersede one already in the vault?" is a real question and a good one.
It is a *curation* decision, it requires a human, and putting it on the critical path of a
mechanism whose entire purpose is to not require one was the bug.

So it becomes a separate, explicitly human-run pass that reads the **vault**, not the
ledger — near-duplicate clusters among recently-written engrams, adjudicated with
`muninn_evolve`. **That pass is not built yet.** Until it is, the vault can accumulate
near-duplicates from the drain, and that is the accepted cost of a durability path that
actually runs. Nothing about the pipe blocks it: `memory-proposals.drained.jsonl` records
every engram id the drain produced, which is exactly the candidate set such a pass needs.

### Worktrees — one ledger per repository

**There is exactly one ledger per repository: the main checkout's.** A linked worktree's
`.git` is a file pointing at `<main>/.git/worktrees/<name>`, and `paths()` resolves through
it, so a proposal appended with a worktree as the working directory queues in the main
checkout where a drain will actually look. `MUNINN_LEDGER_ROOT` overrides it.

This corrects a claim that was false when it was written. The previous version said each
worktree "drains its own ledger… no ledger is invisible to the process that would drain
it". It was invisible: `CLAUDE_PROJECT_DIR` is **empty in a Bash tool environment**, so
`paths()` fell back to `cwd`, and an append made from a scratch worktree landed there — but
no session ever has a scratch worktree as its project root, so nothing ever drained it and
`ledger-guard.mjs` never saw it either. 17 real proposals were found stranded across two
worktrees, silently, while this document called the arrangement correct. A per-worktree
queue only works if a drain runs per worktree, and none ever will.
