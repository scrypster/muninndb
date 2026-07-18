# Review rubric — the dependable-review protocol

This is the backbone of `code-reviewer`. It is written so that **even a weak model that
follows it literally produces a review you can trust**, because the judgment is offloaded to
mechanical evidence (the compiler, the race detector, the RED-sanity procedure) and to
objective routing rules, not to the model's insight. The model's job is to run the gates and
attach the evidence, or to escalate honestly when it can't.

Three ideas run the whole thing:
1. **Evidence, not opinion.** Every gate produces pasted command output or it did not happen.
2. **The system picks the depth.** Risk tier is decided by objective path/keyword rules.
3. **A hard confidence floor.** If a gate can't be satisfied with evidence, the verdict is
   DEFER, never approve-on-faith. Deferring is cheap and safe. A wrong merge is not.

---

## Step 0 — Confirm what you're reviewing (always)

- `git branch --show-current`, `git log --oneline -3`. Fetch the PR head into a fresh clone
  or worktree off the PR ref. Review THAT, not whatever the local checkout happens to be on.
- **Read the invariants and this rubric from `develop`, not from the PR branch** (the PR may
  predate a docs update).
- Record the head SHA in your review. If you can't confirm the SHA you built, stop.

## Step 1 — Pick the risk tier (objective, grep-driven)

Run the diff's file list through these rules, top to bottom. First match wins.

**TIER 3 — deep, mandatory second adversarial reviewer.** Any of:
- touches `internal/auth/`, `internal/replication/`, `internal/storage/migrate/`,
  `internal/storage/keys/`, or `internal/prefix/`
- touches `internal/mcp/context.go`, `internal/mcp/server.go`, or `internal/mcp/session.go`
  (the MCP auth / session / dispatch surface)
- touches an on-disk or wire format: anything under `internal/transport/mbp/` (the binary
  wire types, e.g. `frame.go`, `types.go`), `proto/`, or any change to a Pebble key encoding
- the diff text contains any of: `casLocks`, `CompareAndSet`, `stripedMutex`, `Lease`,
  `ValidateAPIKey`, `ValidateCapability`, `GenerateCapability`, `IsLeader`, `RepLogAppend`,
  `hmac`, `sha256`, `crypto/`, `migrate`
- adds or removes a `go.mod` dependency

**TIER 2 — standard.** Any other change to `.go` logic under `internal/`, `cmd/`, or root.

**TIER 1 — light.** Only docs (`*.md`, `docs/`), comments, test-only changes, additive
`omitempty` response fields, or `web/` copy — and nothing that matches Tier 3.

State the tier and the rule that triggered it at the top of your review.

## Step 2 — Evidence gates (run every gate in scope; attach real output)

Each gate is PASS only with pasted output. No output means the gate did not run, which means
you cannot APPROVE.

**G1 Build (all tiers).** `go build ./... && go vet ./... && gofmt -l .` (for non-Go PRs,
the equivalent: the docs render, the YAML parses, etc.). Any failure → **BLOCK** (needs-work).

**G2 Tests (Tier 2 and 3; Tier 1 if any test exists).** Run the touched packages' tests. Add
`-race` whenever the change touches storage, the Hebbian/PAS workers, the pruner,
replication, or MCP session state. Paste the tail. Any failure → **BLOCK**.

**G3 RED-sanity (any PR that claims to fix a bug, close a race, or add a guard).** Prove the
new test catches the thing:
- Revert ONLY the production fix in place (keep the new test), or check out the pre-fix
  state of the changed source file. Run the new test. It must go **RED**. Paste the failure.
- Restore the fix. Run it. It must go **GREEN**. Paste it.
- If you cannot produce red-then-green, the fix is **unproven** → you may not APPROVE
  (needs-work, or DEFER if you can't tell why). A test that passes both ways proves nothing.

**G4 Invariant check (Tier 2 and 3).** For each invariant in `invariants.md` whose files the
diff touches: grep the anchor in the LIVE code, confirm the invariant still describes the
code, then state PASS or FAIL for this change. If the invariant's own text disagrees with the
live code, the **code wins** — flag the stale invariant, don't enforce it.

**G5 Cross-surface drift (all tiers).** Walk `drift-and-obligations.md` for every touched
file. Any unmet obligation (registry smoke test, openapi.yaml, SDK types, preset web-UI,
embed cache keys, proto regen) → a **required change**, not a nit.

*Do this mechanically, don't eyeball it — the type-alias trap is the one weak models miss.*
If the diff changes a struct under `internal/transport/mbp/`, that struct may be re-exported
by a type alias on another surface, so the change silently landed there too. Grep for it:
`grep -rn "= mbp\.<StructName>" internal/transport/rest internal/transport/grpc`. If REST or
gRPC aliases the struct you changed (e.g. `rest.ActivationItem = mbp.ActivationItem`, which
`/recall` serializes straight to the client), then your fields hit that surface's response,
and its schema (`openapi.yaml`) or proto MUST be updated or it's field-drift. A G5 that marks
this PASS without running that grep has not actually checked G5.

**G6 Adversarial refute (TIER 3 ONLY — mandatory).** A second, independent reviewer runs the
gates again AND actively tries to break the change: what input, ordering, crash point, or
concurrent caller makes it wrong? For a security change: what's the bypass? The two reviews
are compared:
- Both reach APPROVE with consistent evidence → APPROVE stands.
- The refuter finds a real, evidenced failure → needs-work.
- They disagree on a security or correctness point and evidence can't settle it → **DEFER**.

## Step 3 — Verdict (bounded by the confidence floor)

- **APPROVE** — only if every in-scope gate PASSED with attached evidence, and (Tier 3) G6
  agreed. Nothing else earns an approve.
- **APPROVE WITH REQUIRED CHANGES** — gates pass but a G5 obligation or a small, named fix is
  outstanding. List them numbered.
- **NEEDS WORK** — a gate failed or a real defect was found.
- **DEFER (human)** — you cannot satisfy a gate with evidence, the Tier-3 panel is split, or
  the change turns on a domain a review can't settle: a novel cryptographic construction, a
  consensus/replication correctness argument that can't be reduced to a test, or a
  license/BSL-boundary decision. **When you can't reach APPROVE or NEEDS-WORK on evidence,
  DEFER. Never guess, never approve-on-faith.**

## Step 4 — Anti-hallucination self-check (before you emit the verdict)

Answer these literally. Any "no" downgrades the verdict to DEFER:
- Did I attach real pasted output for G1, and for G2/G3 where in scope?
- If I claim a fix works, did I show the RED-sanity red-then-green output?
- If Tier 3, did a genuinely independent second pass (G6) run, and do I state its verdict?
- Did I verify each invariant I cite against the live code, not just quote the doc?

A review that reaches APPROVE without the evidence its tier requires is itself invalid.
Re-run it or DEFER.

---

## Why this is safe enough to trust without a human on routine changes

The human doesn't disappear. The human is **escalated to only when the system is honestly not
confident** — a failed gate, a split adversarial panel, or a domain a review can't settle.
Everything else is approved on mechanical evidence that a weak model can produce as reliably
as a strong one: a compile either succeeds or it doesn't, a race either fires or it doesn't, a
reverted fix either reddens its test or it doesn't. The floor is what makes "you don't review
routine code anymore" a promise the system can keep instead of a hope.
