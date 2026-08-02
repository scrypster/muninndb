---
name: panel
description: >-
  Run competing positions against each other and come out with a DECISION, not a pile of
  opinions. Use when a genuine fork has to be resolved and the evidence does not obviously
  pick a side: a product-doctrine question, a design that returned two defensible shapes,
  or a measurement that contradicts something already landed. Independent panelists, a
  decision rule written before anyone runs, and a judge whose powers are deliberately
  bounded.
---

# panel — dialectical inquiry that terminates

Also called adversarial collaboration, red-teaming, or a judge panel. The value is not
"more opinions." It is **independence followed by reconciliation against a rule written in
advance**, which is what stops the exercise from becoming a summary nobody can act on.

## When it is allowed to run

Any ONE of these qualifies. Nothing else does:

1. A **Tier-3 or doctrine fork** — a decision that changes what the product means, is
   expensive to reverse, or contradicts an existing invariant.
2. **The designer returned two or more defensible shapes** and could not choose between
   them on the evidence available.
3. A **measurement contradicts a landed invariant or a decision-record entry.**

Anything else: the designer picks the defensible default and proceeds, exactly as the
`increment` skill already says. Three deep-reasoning runs plus a judge is a real cost, and
the gate has to be worth it. **Do not run a panel on trivia, and do not run one to avoid
making a call you could make.**

## The procedure

### 1. Write the question and the decision rule FIRST

Before any panelist runs, write to the design record (or a scratch file if no design record
exists yet):

- the exact question, phrased so it can be answered;
- what result picks A, what picks B, what would pick something else;
- what result means **the panel was the wrong instrument** and the question needs
  measurement or an owner decision instead.

This is the same pre-registration discipline the project already holds its measurements to.
A rule written after the arguments arrive is not a rule, it is a rationalization.

### 2. Three independent panelists, with different MANDATES

Not different personalities. Different jobs:

- **Argue A** on the strongest available evidence.
- **Argue B** on the strongest available evidence.
- **Reject both and find a third shape.**

That third mandate is not a courtesy. It is the one that has repeatedly produced the
answer: on the upsert-doctrine question, two independent runs both rejected the two options
on the table and converged on a third that nobody had proposed.

Run them **concurrently and blind** — no panelist sees another's output. Use different
models where you can; the independence is the point. Each panelist reads the real code and
the decision record, and cites `file:line`. A panelist that reasons from the brief alone is
producing an opinion, and opinions are what this procedure exists to avoid.

### 3. The judge, with bounded powers

The judge may do exactly three things:

1. **Mark each claim** verified, refuted, or unverifiable — against the live code and the
   decision record, not against plausibility. Unverifiable claims are **discarded**, not
   weighed.
2. **Apply the pre-written rule** to what survives.
3. **DEFER to the owner** when the rule does not discriminate, and say precisely what
   additional evidence would.

**The judge may NOT introduce a new option.** That is what panelist three was for. A judge
that invents an answer has skipped the independence the whole procedure is built on.

### 4. Output a decision, and keep the losing arguments

Append to the design record: the decision, the rule that produced it, and **the arguments
that lost, with their strongest points intact.** A panel that ends in a summary has failed.
The losing case is the most valuable artifact six months later, when someone asks why the
project went this way — and it is what makes the decision reversible on evidence rather
than on memory.

## Failure modes to watch for

- **Panelists converging because they read each other.** Run them blind or the result is one
  opinion wearing three hats.
- **A judge that splits the difference.** Most forks do not have a coherent midpoint;
  averaging two designs usually produces one that has neither's virtues.
- **A rule vague enough to justify anything.** If you cannot say in advance what would
  change your mind, you are not running a panel, you are collecting support.
- **Running it to diffuse responsibility.** The decision is still the owner's. The panel
  informs it; it does not launder it.
