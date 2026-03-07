# GitHub Copilot + MuninnDB

GitHub Copilot supports MCP servers via VS Code's `.vscode/mcp.json` configuration. This lets Copilot read and write memories through MuninnDB just like any other MCP-capable agent.

---

## Prerequisites

- MuninnDB running locally (`muninn start`) or on a remote host
- VS Code with the GitHub Copilot extension (v1.250+)
- Your MuninnDB admin token (printed on first start, or found in `muninn.env`)

---

## Configuration

Add a `.vscode/mcp.json` file to your workspace (or user settings):

```json
{
  "servers": {
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

Replace `YOUR_ADMIN_TOKEN` with the token from your `muninn.env` file (`MUNINN_ADMIN_TOKEN`).

**No token configured?** If you started MuninnDB without a token, omit the `headers` block entirely:

```json
{
  "servers": {
    "muninn": {
      "type": "http",
      "url": "http://127.0.0.1:8750/mcp"
    }
  }
}
```

---

## Remote host

If MuninnDB is running on a remote server, replace `127.0.0.1` with the server's IP or hostname:

```json
{
  "servers": {
    "muninn": {
      "type": "http",
      "url": "http://your-server:8750/mcp",
      "headers": {
        "Authorization": "Bearer YOUR_ADMIN_TOKEN"
      }
    }
  }
}
```

> **Tip:** Use `muninn start --listen-host 0.0.0.0` on the remote server so it accepts connections from outside localhost.

---

## Verify the connection

Once configured, open VS Code's Copilot chat panel and ask:

```
@muninn call muninn_guide
```

MuninnDB will respond with vault-aware instructions for how to use memory effectively. If you see tool results, you're connected.

---

## OAuth error?

If Copilot shows:

```
OAuth Client Credentials Required
This server requires OAuth but doesn't support automatic client registration.
```

This means Copilot is trying to use OAuth discovery before checking the `Authorization` header. The fix is to ensure the `headers` block is present in your config — Copilot will use the bearer token directly and skip the OAuth flow.

---

## Troubleshooting

| Symptom | Fix |
|---------|-----|
| `connection refused` | Make sure `muninn start` is running and port 8750 is reachable |
| OAuth error | Add the `headers` block with your admin token |
| Tools not appearing | Restart VS Code after editing `mcp.json` |
| Token not known | Check `~/.muninn/muninn.env` or re-run `muninn start` — the token is printed on first run |
