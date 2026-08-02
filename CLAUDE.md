# MuninnDB — working guide for AI agents

This file governs how AI agents (Claude Code and others) work in this repo. It auto-loads
for anyone using Claude Code here, contributor or maintainer. It is the constitution: what
MuninnDB *is*, the principles that must never be violated, and how to write and review
changes that fit the project.

The deep reference lives in `docs/internals/` — this file is the index and the
non-negotiables.

---

## 1. What MuninnDB is

MuninnDB is a **cognitive memory engine for AI agents**, written in Go over a single
shared [Pebble](https://github.com/cockroachdb/pebble) LSM key-value store. Its one-line
promise, from the README, is load-bearing — every change is measured against it:

> **Memory that strengthens with use, fades when unused, and pushes to you when it matters.**

It is accessible over **MCP, REST, gRPC, the MBP binary protocol, an embedded Go library,
and SDKs** (Python/Node/PHP/…). It is **alpha**, single-binary, zero-config by default.
Licensed BSL 1.1.

The three words in the promise map to real subsystems:
- **strengthens with use** → Hebbian co-activation learning, LTP, access-driven ACT-R base-level.
- **fades when unused** → Ebbinghaus/ACT-R temporal decay, the background pruner, association decay + archival.
- **pushes to you when it matters** → recall's predictive activation (PAS), entity/associative traversal, confidence-weighted scoring.

If a change makes memory behave less like this — recall that returns silently-wrong
results, decay that never fires, learning that displaces genuine matches — it is wrong even
if it compiles and the tests pass.

### Architecture map

| Subsystem | Packages | What it owns |
|---|---|---|
| Cognitive engine | `internal/engine/` (+ `activation/`), root `recall.go`/`remember.go`/`dream.go`/`muninn.go` | Recall pipeline, write-path learning, decay, pruning, scoring |
| Cognition support | `internal/cognitive/`, `scoring/`, `consolidation/`, `episodic/`, `working/`, `brief/`, `query/` | Decay math, Hebbian worker, dream consolidation, episodes, working-memory buffer |
| Storage | `internal/storage/` (+ `keys/`), `index/` (HNSW), `wal/`, `backup/`, `provenance/` | Pebble keyspace, engrams, associations, vector index, durability |
| Auth & credentials | `internal/auth/` | `mk_` API keys, `cap_` capability tokens, `mdb_` static token, admin users, plasticity config |
| Transports | `internal/mcp/`, `internal/transport/{rest,grpc,mbp}/` | Protocol surfaces + their auth enforcement |
| Cluster | `internal/replication/` | Leader election, log shipping, failover |
| Plasticity | `internal/auth/plasticity.go` | Per-vault cognition config, the 5 presets |
| Surfaces | `cmd/muninn/` (CLI), `web/` (console), `sdk/`, `proto/` | User-facing entry points |

Reference docs: `docs/internals/keyspace-registry.md` (the Pebble prefix table),
`docs/internals/invariants.md` (hard invariants), `docs/internals/decision-record.md` (why
the project is the way it is), `docs/internals/drift-and-obligations.md` (cross-surface
sync obligations + the CI budget).

---

## 2. Core principles (the lens for every change)

Distilled from real decisions across the project's history (each traced to its PR/issue in
`docs/internals/decision-record.md`).

1. **Explicit config is never silently substituted.** A configured embed provider, model,
   vault dimension, or preset is honored or fails loudly. Silent, plausible-looking wrong
   answers are the worst failure class in the project (#582/#585/#589).

2. **Degrade loudly-but-gracefully, never silently-wrong.** Embed backend down → fall back
   to BM25 with a WARN, never hard-fail *and* never pretend nothing changed (#578).
   Same-model degraded results are acceptable; different-model or silently-empty results are not.

3. **Security properties are made structurally impossible, not policy-checked.** A `cap_`
   token *cannot* mint another credential because it authenticates as `IsCapability`, never
   `IsAPIKey`, so it can't satisfy the mint gate — not because a rule says "don't" (#612).
   Prefer designs where the bad state is unrepresentable.

4. **Fail closed on auth, fail open on presentation.** Invalid credential → denied, always.
   Unknown *toolset* value (a display preference) → fall back to full so a typo never hides
   someone's memory tools (#604). Know which side of that line a change is on.

5. **Features land as minimal, reviewable increments referencing their RFC.** Big work ships
   as numbered increments (RFC #597 → #599 → #612 → #617), each small, each naming what it
   defers. Don't write or accept sprawling PRs.

6. **Derived config is pinned by invariant tests, not copy-paste.** The `working` preset is
   `default` + exactly two deltas, pinned by a `reflect.DeepEqual` test so a future edit to
   `default` can't silently drift it (#599).

7. **Extend proven in-tree mechanisms over inventing new architecture.** The tag-filter fix
   (#607) reuses the candidate-pool injection that time filters already do. Find the
   precedent first.

8. **Match enforcement strength to the actual trust model.** The engram lease is *advisory*
   because the agents sharing a vault are cooperative (#576). Don't over-build enforcement
   the threat model doesn't need — but say so and document the residual.

9. **Verify claims independently; severity can go up, not just down.** #611 was filed as
   "low risk"; analysis found live bugs and raised it. Never take a claim — a reporter's or
   a PR description's — on faith.

10. **Honest negative results and scope discipline are first-class.** #609 reported that
    ambient-push memory had zero uptake and killed the idea — that's valued, not dismissed.

11. **Calibration is per-vault and self-derived, never hardcoded from one dataset.** A
    threshold, baseline, or vocabulary a feature needs must be derived from each vault's OWN
    data (self-calibrating, like #711's per-corpus IDF) or exposed as a per-vault override —
    with model/cold-start defaults as hints only, never fixed constants that define someone
    else's data for them. A number tuned on one sample vault imposes that vault's shape on
    every other (a code vault versions with git SHAs; a notes app has no version tags; a
    medical vault is different again) where it is wrong or a silent no-op. A sample vault
    (e.g. a maintainer's own) is for FINDING bugs and VALIDATING generalization on messy real
    data — never for tuning a constant into the product. Everyone calibrates their own vault;
    we ship mechanisms and hints, not their answers. (The semantic-abstention floor self-
    measures each vault's embedding-anisotropy baseline instead of shipping bge-small's; #712
    currency was held partly because its version-marker vocabulary was one-vault-specific.)

---

## 3. How we work

**Verify, don't assume.** For any non-trivial change:

1. **Confirm the commit you're on.** The working checkout can be on a stale branch. Run
   `git branch --show-current` / `git log` and diff against `develop` before asserting what
   the code does. When in doubt, work in a fresh worktree off `origin/develop`. (This bit
   the project's own tooling build — a whole analysis pass ran against a branch missing six
   merged PRs.)

2. **Build and test the actual change**, not just the diff — and **keep `-tags localassets`**:
   `go build -tags localassets ./... && go vet -tags localassets ./... && gofmt -l .` plus the
   relevant `go test -tags localassets`. CI builds, vets, and tests everything with that tag
   (obligation #9), so a bare `go build ./...` exercises a different code path than the gate
   you have to pass. It needs the embed assets — `make fetch-assets` once. Use `-race` for
   anything touching storage, the Hebbian/PAS workers, the pruner, replication, or MCP
   session state.

3. **RED-sanity-check bug fixes.** A test for a fixed bug or closed race must be shown to
   *fail without the fix*. A test that passes both ways proves nothing. If a test asserts
   on state produced by an async worker, drain it deterministically instead of
   `time.Sleep` — see `docs/internals/testing-hermeticity.md` (#722).

4. **Honor the invariants and cross-surface obligations.** Check `docs/internals/invariants.md`,
   the keyspace registry, and `docs/internals/drift-and-obligations.md`. Touching an MCP
   handler means updating the registry smoke test; adding a Pebble prefix means checking the
   collision guard; changing a preset means updating the web UI and adding a pinning test; etc.
   Four of those obligations now warn automatically via `.claude/hooks/drift-guard.mjs`
   (marked 🪝 in that doc) — a reminder, not a gate, and no substitute for walking the list.

**This repository is public. Measure on real vaults; never name them.** Mechanisms get proven
against real corpora — that is the point, and a measurement without a real substrate is not
worth much. But the vault a measurement ran on is not ours to publish. It belongs to a user, a
client, or another product, and naming it links them to this project permanently.

The rule, in committed content — source, tests, code comments, design records, commit messages,
**and filenames**:

- Refer to a measurement corpus as **"a production vault"**. Keep the numbers; drop the name.
  "Measured on a real 3,296-memory production vault" carries every bit of the evidence that
  naming it would, and costs nothing.
- Use invented names in fixtures and examples. Not a real colleague, customer, or contact —
  and not a real product's module names either.
- No client, tenant, employer, or fund identifiers. No pricing, rates, or commercial terms.
- If a design record can't make its point without those, it isn't publishable. Keep it local;
  `.claude/deep-review/README.md` has the triage rule and the `private/` convention.

This is not hypothetical. A vault name reached `origin/develop` in #715 and was scrubbed at the
tip a day later by #734 — but a public repo's history is public forever, and a scrub of the tip
is not a scrub. **Getting it right before the commit is the only version of this that works.**
Maintainers additionally run a local pre-commit guard; do not rely on it, and note that
contributors don't have it.

**Keep CI fast and cheap.** The full gate must stay **under ~10 minutes** (baseline ~6–7
min; job map in `drift-and-obligations.md`). Unit and invariant tests are nearly free —
prefer them. Integration, Playwright, `-race`, and asset-gated tests cost real minutes;
reach for end-to-end proof only when a change genuinely needs it (protocol conformance, UI
flows, "the button is actually wired" behavior). Never balloon the pipeline to prove a point
a table-driven unit test could make.

**Communicate like the project does.** Reviews and responses are evidence-first, specific,
and never a rubber stamp: say what you verified and how, give specific rather than generic
feedback, and deliver pushback as "here's what holds up, here's what needs to change, and
yes please send it." Warmth and rigor, together.

---

## 4. Findings that outlive the session

**This applies to you, the main session, not only to subagents.** `.claude/memory-protocol.md`
was named in five agent definitions and nowhere in this file, so the only actor with a shell —
the only one that could ever drain the queue — had never been told the queue existed. Two
disjoint persistence protocols split by agent type with no bridge. This is the bridge.

If a session produces something **durable, non-obvious, and not recoverable from git, the PR,
or the tracker** — a measured number, a decision and why it beat the alternative, an honest
negative, a defect *pattern* rather than a defect, a trap that looks safe — propose it:

```sh
node .claude/hooks/memory-propose.mjs <<'JSON'
{"concept":"short label","content":"the fact itself, self-contained, readable in a year","summary":"one line","type":"fact","source":"main"}
JSON
```

Read `.claude/memory-protocol.md` for the bar (a noisy vault is worse than a small one, and
the "do not propose" list is as load-bearing as the "do"). The ledger is gitignored and
subject to the same privacy rule as committed content — no vault names, no client
identifiers.

Direct `muninn_remember` over MCP is not wrong and stays available; the ledger is what makes
the finding survive a session that has no MCP access, a subagent with no credentials, or a
context that ends before anyone thinks to write it down. `.claude/hooks/memory-drain.mjs`
moves the queue into the vault on `PreCompact` / `SessionEnd` / a debounced `Stop`. Every
invocation that is not `SIGKILL`ed leaves a receipt at `.claude/memory-drain-receipt.json`,
and `memory-freshness.mjs` reads it back at `SessionStart` and speaks up when the queue is
stale — so "has this ever run?" needs neither a `stat` nor an inference. That distinction is
the whole point: the first version of this mechanism never ran once and nobody could tell.

There is **one ledger per repository**, in the main checkout: appending from a linked
worktree resolves through `.git` to the same file, because a per-worktree queue is one no
drain ever visits (17 proposals were found stranded that way).

Its tests are `node --test .claude/hooks/tests/*.test.mjs` — a few seconds, no daemon, not in
CI. Run them if you touch the drain. Use the glob; `node --test .claude/hooks/tests/` finds
nothing and exits 1 in ~30 ms, because the runner skips dot-directories.

---

## 5. The code-review agent

`.claude/agents/code-reviewer.md` is the repo's resident reviewer — correctness, the
cognitive/storage/security invariants, and cross-surface drift, with its own
verify-build-test protocol. Use it (or the `/code-review` skill) when reviewing a change,
and proactively before opening a PR. It routes by what the diff touches and cites
`docs/internals/`.

Maintainers may have additional local-only tooling (issue triage, review orchestration) that
is not part of this repo.

---

## 6. Attribution

Do not add "Generated with Claude" / Anthropic attribution to any PR body, commit message,
issue, or code comment.
