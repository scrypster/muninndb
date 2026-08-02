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

One JSON object per line, appended to `.claude/memory-proposals.jsonl` (gitignored). Never
rewrite or reorder the file — append only.

```jsonc
{
  "vault":    "muninndb",          // required
  "concept":  "short label",       // required
  "content":  "the fact itself",   // required — self-contained, readable in a year
  "summary":  "one line",          // strongly preferred
  "type":     "fact",              // fact|decision|observation|issue|procedure|constraint|…
  "tags":     ["…"],
  "entities": ["…"],               // bare names are fine
  "importance": 0.8,               // 0.7+ is protected from capacity pruning
  "source":   "adversary",         // which agent or session proposed it
  "supersedes_hint": "…"           // optional: a phrase or ULID this likely replaces
}
```

Write it self-contained. A memory that only makes sense next to the conversation that
produced it is not a memory, it is a comment.

**Privacy is not relaxed here.** The ledger is a real artifact and it is subject to the same
rule as committed content: measure on real vaults, never name them. No vault names, no
client or tenant identifiers, no memory content copied out of someone's vault, no
commercial terms. The pre-commit guard scans it, and it is gitignored so it cannot land by
accident — but the guard is a backstop, not permission.

## How it drains

`node .claude/hooks/memory-drain.mjs` (add `--dry-run` to see what it would do).

Its contract, in the order the properties matter:

1. **Nothing is lost.** A proposal that cannot be written stays in the ledger. A failed
   write never consumes the line. If the drain dies halfway, re-running is safe.
2. **Idempotent, twice over.** Re-appending an already-drained proposal is caught by the
   neighbourhood check first (the memory it wrote is now a strong match, so it is HELD and
   reported rather than written again). If that check is ever bypassed, every proposal
   also carries a content-derived `op_id`, so the server returns the existing engram
   instead of minting a second. Re-running the drain cannot duplicate.
3. **It will not create a rival copy.** Before writing, it recalls the neighbourhood. If
   something already there matches strongly, the proposal is **HELD** in the ledger and
   reported rather than written, because that is a curation decision (evolve? supersede?
   leave alone?) and a script guessing it is how vaults fill with near-duplicates. The
   ledger is the durability floor; the vault is the quality bar. Held is not lost.
4. **Loud on failure.** A daemon that is down, a vault that is locked, a token that is
   missing: reported, non-zero exit, ledger untouched.

Curating the held proposals is a real task, not a formality. Recall what is there, decide
whether the new finding supersedes it, and use `muninn_evolve` when it does — that is the
whole reason the drain refuses to guess.
