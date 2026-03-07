# Codebuff + MuninnDB

Codebuff is an AI coding agent that supports MCP servers. Connecting MuninnDB gives Codebuff persistent memory across sessions — it can store decisions, patterns, and context it discovers while working, and recall them automatically in future sessions.

---

## Prerequisites

- MuninnDB running locally (`muninn start`)
- Codebuff installed and configured
- Your MuninnDB admin token (printed on first start, or found in `muninn.env`)

---

## MCP configuration

Add MuninnDB as an MCP server in your Codebuff config. The exact config file location depends on your Codebuff version — check the [Codebuff docs](https://codebuff.com/docs) for the current path. The structure is:

```json
{
  "mcpServers": {
    "muninn": {
      "type": "http",
      "url": "http://127.0.0.1:8750/mcp",
      "headers": {
        "Authorization": "Bearer YOUR_ADMIN_TOKEN"
      }
    }
  }
}
```

**No token configured?** Omit the `headers` block:

```json
{
  "mcpServers": {
    "muninn": {
      "type": "http",
      "url": "http://127.0.0.1:8750/mcp"
    }
  }
}
```

---

## System prompt / AGENT.md

For Codebuff to use memory proactively — storing useful things it discovers, not just when explicitly asked — add this to your project's `AGENT.md` or system prompt:

```markdown
## Memory (MuninnDB)

You have access to persistent memory via the `muninn_*` MCP tools.

**Store** things worth remembering:
- Architecture decisions and the reasoning behind them
- Non-obvious patterns, gotchas, and workarounds discovered while coding
- User preferences and recurring instructions
- Important project context (tech stack, deployment setup, constraints)

**Recall** at the start of relevant sessions:
- Call `muninn_recall` with context about what you're working on
- Call `muninn_guide` on first connect for vault-specific instructions

**Don't ask** — just store. If you discover something useful, write it to memory without waiting to be told.
```

This is the key to getting proactive memory behavior. Without it, most agents only use memory when explicitly prompted.

---

## Verify the connection

Start a Codebuff session and ask it to:

```
Call muninn_guide and tell me what you see.
```

MuninnDB will return vault-aware usage instructions. If you see tool output, the connection is working.

---

## What Codebuff can remember

Once connected, Codebuff can use MuninnDB to persist:

- **Architectural decisions** — why you chose this structure, what alternatives were rejected
- **Codebase quirks** — non-obvious patterns, footguns, things that bit you once
- **Project conventions** — naming, formatting, testing approach
- **Refactor history** — what was changed and why
- **Session handoffs** — where you left off on a long-running task

These persist across sessions, across machines (if using a remote MuninnDB instance), and across context resets. The cognitive engine ages them naturally — frequently accessed memories stay sharp, unused ones fade.

---

## Troubleshooting

| Symptom | Fix |
|---------|-----|
| `connection refused` | Ensure `muninn start` is running |
| Tools not appearing | Restart Codebuff after editing the config |
| Agent not storing proactively | Add the `AGENT.md` / system prompt instructions above |
| Token not known | Check `~/.muninn/muninn.env` — token is printed on first `muninn start` |
