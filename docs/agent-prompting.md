# Proactive Agent Prompting

Getting an AI agent to use MuninnDB well is mostly a prompting problem. Agents that are only told "you have memory tools" tend to use them reactively — they'll recall when asked, and store when told to. That's useful, but it's not the same as an agent that saves continuously and arrives at sessions already loaded with context.

This guide covers the system prompt patterns that make the difference.

---

## The core insight

Most agents have an implicit filter: *"Is this important enough to save?"* That filter is the enemy of good memory behavior. It causes agents to hold back on things that seem minor in the moment but matter later. The framing that works is:

> **Saving is a mindset, not a checklist. When in doubt, save it.**

Removing the evaluation gate — replacing it with a bias toward saving — is the single biggest lever for improving proactive behavior.

---

## Recommended CLAUDE.md / system prompt

This pattern works well across Claude Code, Claude Desktop, Cursor, Codebuff, OpenClaw, and other MCP-connected agents. Replace `[vault name]` with your vault (usually `default`):

```markdown
# Memory: MuninnDB (Canonical)

MuninnDB (muninn MCP) is the canonical memory system. Never use local auto memory.

## Session Start — Always
Call `muninn_recall` with relevant context before beginning any work.
This loads prior context. Vault: [vault name].

## During Every Session
- Save to Muninn continuously — this is a mindset, not a checklist.
- Anything the user shares or that emerges from the work should be saved immediately.
- Do not evaluate whether it is "important enough".
- Do not wait to be asked. When in doubt, save it.

## Tools
- **Recall**: `muninn_recall` (vault, context)
- **Store**: `muninn_remember` (vault, concept, content)
- **Batch**: `muninn_remember_batch` (vault, memories[])
- **Read**: `muninn_read` (vault, id)
- **Link**: `muninn_link` (vault, source_id, target_id)
- **Guide**: `muninn_guide` — call on first connect to learn best practices

Vault: [vault name].
```

> **Credit:** This pattern was contributed by the community. Thanks to [@cmdillon](https://github.com/cmdillon) for sharing it.

---

## Why `muninn_guide` matters

`muninn_guide` returns vault-specific instructions at runtime — it knows your vault's behavior mode, whether enrichment is active, and any vault-level notes you've set. Including it in the system prompt ensures the agent picks it up on first connect rather than waiting to be told. Think of it as a self-describing configuration layer for memory behavior.

---

## What to save

Agents don't always know what's valuable. Being explicit helps:

- **Decisions** — architecture choices, approach selections, and the reasoning behind them
- **Preferences** — how the user likes things done, recurring instructions, style choices
- **Discoveries** — non-obvious patterns, gotchas, workarounds found while working
- **Project context** — tech stack, constraints, deployment setup, team conventions
- **Session state** — where you left off, what's in progress, what's blocked

---

## Recall at the start of every session

The recall-first pattern is as important as saving. An agent that saves diligently but doesn't recall at session start loses the benefit of continuity. The system prompt should make recall unconditional — not "if relevant" but "always, before beginning any work."

---

## Batch saves for efficiency

For agents that do a lot of work in one session, `muninn_remember_batch` is more efficient than individual saves — it writes multiple memories in a single round trip. Good agents will naturally group related saves at natural pause points.

---

## Troubleshooting proactive behavior

| Symptom | Likely cause | Fix |
|---------|-------------|-----|
| Agent only saves when asked | Missing system prompt guidance | Add the CLAUDE.md pattern above |
| Agent recalls but doesn't save | "important enough" filter active | Add the mindset framing explicitly |
| Agent saves but context doesn't persist | Wrong vault name | Confirm vault name in system prompt matches MuninnDB |
| Agent ignores `muninn_guide` | Tool not listed in system prompt | Add `muninn_guide` to the tools section |

---

## Related

- [How Memory Works](how-memory-works.md) — the cognitive engine behind storage and retrieval
- [Semantic Triggers](semantic-triggers.md) — subscribe to contexts and get push updates when relevance changes
- [Auth & Vaults](auth.md) — multi-vault setup, API keys, behavior modes
- [Codebuff Integration](integrations/codebuff.md)
- [GitHub Copilot Integration](integrations/github-copilot.md)
