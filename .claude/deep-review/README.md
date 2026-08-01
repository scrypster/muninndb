# Design records

One document per increment, written before the code. The `increment` skill
(`.claude/skills/increment/`) requires one for any non-trivial change: the mechanism, the
minimal first-increment scope with explicit deferrals, invariant impacts, the measurable
proof, and the top risks.

These are published deliberately. If you want to know why MuninnDB is shaped the way it is,
`docs/internals/decision-record.md` has the summary and these have the working.

## The triage rule — read before adding a file here

This directory is **public**. `private/` is gitignored.

A design record goes in `private/` if it contains any of:

- **A real person's name** — contributors, colleagues, clients, investors. Test fixtures may
  use invented names; use obviously-invented ones.
- **Client or tenant data** — vault names tied to a customer, fund/org identifiers, anything
  naming who the data belongs to.
- **Commercial terms** — pricing, rates, discounts, contract values, cost structure.
- **Competitive or go-to-market strategy** — positioning, moat analysis, pointed claims about
  other vendors.
- **Operational specifics of someone's machine** — hosts, ports, key material, deployment
  paths particular to one install.

Everything else — mechanisms, measurements, invariants, prior art, negative results,
adversarial findings — belongs in public. Honest negative results are first-class here; a
killed idea is a real result and worth publishing.

**Default new work to `private/` and promote it deliberately.** Promotion is a review;
demotion after the fact is a leak, and git remembers.

## Measuring on a real vault

Several records report measurements taken against a production vault. Refer to it as
"a production vault" and keep query examples generic — the measurement is the point, and the
corpus it ran on is not yours to publish.
