package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	toml "github.com/pelletier/go-toml/v2"
)

// tokenPath returns the path to the MCP bearer token file.
func tokenPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".muninn", "mcp.token")
}

// generateToken creates a new random 24-byte (48 hex char) token.
func generateToken() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	return "mdb_" + hex.EncodeToString(b), nil
}

// loadOrGenerateToken reads mcp.token if it exists; otherwise generates and saves one.
// Returns (token, isNew, error).
func loadOrGenerateToken(dataDir string) (string, bool, error) {
	path := filepath.Join(filepath.Dir(dataDir), "mcp.token")

	existing, err := os.ReadFile(path)
	if err == nil {
		tok := strings.TrimSpace(string(existing))
		if tok != "" {
			// Warn if world-readable
			info, _ := os.Stat(path)
			if info != nil && info.Mode().Perm()&0o044 != 0 {
				fmt.Fprintf(os.Stderr, "  warning: %s is world-readable — consider: chmod 600 %s\n", path, path)
			}
			return tok, false, nil
		}
	}

	tok, err := generateToken()
	if err != nil {
		return "", false, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return "", false, err
	}
	if err := os.WriteFile(path, []byte(tok+"\n"), 0600); err != nil {
		return "", false, fmt.Errorf("save token: %w", err)
	}
	return tok, true, nil
}

// readTokenFile reads the token from the standard location.
// Returns "" if no token file exists (MCP is open).
func readTokenFile() string {
	home, _ := os.UserHomeDir()
	path := filepath.Join(home, ".muninn", "mcp.token")
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// AIToolConfig describes how to configure a specific AI tool.
type AIToolConfig struct {
	// Name is the human-readable tool name shown in output.
	Name string
	// ConfigPath returns the target config file path, or "" if manual only.
	ConfigPath func() string
	// MergeConfig merges muninn into the given config map.
	MergeConfig func(cfg map[string]any, mcpURL, token string)
	// ManualInstructions is shown instead of (or after) auto-config.
	ManualInstructions func(mcpURL, token string)
}

// writeAIToolConfig performs an atomic read-merge-backup-write of a JSON config file.
// The merge function receives the current (possibly empty) config map and should mutate it.
// Returns a human-readable summary of what changed, or an error.
func writeAIToolConfig(path string, mergeFn func(cfg map[string]any)) (string, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return "", fmt.Errorf("create config directory: %w", err)
	}

	// Check write permission before attempting anything
	dir := filepath.Dir(path)
	if f, err := os.CreateTemp(dir, ".muninn_write_test"); err != nil {
		return "", fmt.Errorf("no write permission for %s: %w", dir, err)
	} else {
		f.Close()
		os.Remove(f.Name())
	}

	// Read existing config
	existing, readErr := os.ReadFile(path)
	cfg := map[string]any{}
	if readErr == nil && len(existing) > 0 {
		if err := json.Unmarshal(existing, &cfg); err != nil {
			return "", fmt.Errorf("existing config at %s contains invalid JSON: %w\n  (backup at %s.bak if you want to recover)", path, err, path)
		}
	}

	// Backup before modification
	if readErr == nil && len(existing) > 0 {
		var origMode os.FileMode = 0644
		if info, err := os.Stat(path); err == nil {
			origMode = info.Mode().Perm()
		}
		if err := os.WriteFile(path+".bak", existing, origMode); err != nil {
			fmt.Fprintf(os.Stderr, "  warning: could not create backup %s.bak: %v\n", path, err)
		}
	}

	// Track which top-level keys existed before
	hadMCPServers := cfg["mcpServers"] != nil

	// Apply merge
	mergeFn(cfg)

	// Validate merged result
	out, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal merged config: %w", err)
	}
	var check map[string]any
	if err := json.Unmarshal(out, &check); err != nil {
		return "", fmt.Errorf("merged config validation failed: %w", err)
	}

	// Atomic write: temp file + rename.
	// os.CreateTemp generates an unpredictable filename, preventing a
	// symlink-based attack that could redirect the write to an arbitrary path.
	tmpFile, err := os.CreateTemp(filepath.Dir(path), ".muninn_cfg_*.tmp")
	if err != nil {
		return "", fmt.Errorf("create temp file: %w", err)
	}
	defer os.Remove(tmpFile.Name()) // no-op if rename succeeded
	if _, err := tmpFile.Write(out); err != nil {
		tmpFile.Close()
		return "", fmt.Errorf("write temp file: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		return "", fmt.Errorf("close temp file: %w", err)
	}
	if err := os.Chmod(tmpFile.Name(), 0600); err != nil {
		return "", fmt.Errorf("chmod temp file: %w", err)
	}
	if err := os.Rename(tmpFile.Name(), path); err != nil {
		return "", fmt.Errorf("atomic rename: %w", err)
	}

	if hadMCPServers {
		return "updated mcpServers.muninn in existing config (other servers preserved)", nil
	}
	return "added mcpServers.muninn to config", nil
}

// mcpServerEntry returns the JSON map for muninn's HTTP MCP server entry.
// Used by Cursor, Windsurf, and the VS Code manual snippet — clients that
// natively support HTTP/SSE transport via a url field.
//
// Note: "type" is intentionally omitted. Claude Desktop v1.1.4010+ crashes
// on startup with a TypeError if "type":"http" is present in any mcpServers
// entry. Claude Desktop uses the stdio bridge instead (see desktopMCPEntry).
func mcpServerEntry(mcpURL, token string) map[string]any {
	entry := map[string]any{
		"url": mcpURL,
	}
	if token != "" {
		entry["headers"] = map[string]any{
			"Authorization": "Bearer " + token,
		}
	}
	return entry
}

// mergeMCPServers adds/updates muninn in the mcpServers map of cfg.
func mergeMCPServers(cfg map[string]any, mcpURL, token string) {
	servers, ok := cfg["mcpServers"].(map[string]any)
	if !ok {
		servers = map[string]any{}
	}
	servers["muninn"] = mcpServerEntry(mcpURL, token)
	cfg["mcpServers"] = servers
}

// openCodeMCPEntry returns the JSON map for muninn's OpenCode MCP entry.
// OpenCode requires type "remote", explicit oauth:false, and uses a
// file-template for auth so the token is read from disk at startup.
func openCodeMCPEntry(mcpURL, token string) map[string]any {
	entry := map[string]any{
		"type":  "remote",
		"url":   mcpURL,
		"oauth": false,
	}
	if token != "" {
		entry["headers"] = map[string]any{
			"Authorization": "Bearer {file:~/.muninn/mcp.token}",
		}
	}
	return entry
}

// mergeOpenCodeMCP upserts muninn into cfg["mcp"]["muninn"],
// preserving all other entries under the "mcp" top-level key.
func mergeOpenCodeMCP(cfg map[string]any, mcpURL, token string) {
	mcp, ok := cfg["mcp"].(map[string]any)
	if !ok {
		mcp = map[string]any{}
	}
	mcp["muninn"] = openCodeMCPEntry(mcpURL, token)
	cfg["mcp"] = mcp
}

// claudeCodeMCPEntry returns the JSON map for muninn's Claude Code MCP entry.
// Claude Code requires "type":"http" for schema validation; this is distinct from
// Claude Desktop which crashes if "type" is present (see mcpServerEntry).
func claudeCodeMCPEntry(mcpURL, token string) map[string]any {
	entry := map[string]any{
		"type": "http",
		"url":  mcpURL,
	}
	if token != "" {
		entry["headers"] = map[string]any{
			"Authorization": "Bearer " + token,
		}
	}
	return entry
}

// mergeClaudeCodeMCP upserts muninn into cfg["mcpServers"] using the Claude
// Code-specific entry format (includes "type":"http").
func mergeClaudeCodeMCP(cfg map[string]any, mcpURL, token string) {
	servers, ok := cfg["mcpServers"].(map[string]any)
	if !ok {
		servers = map[string]any{}
	}
	servers["muninn"] = claudeCodeMCPEntry(mcpURL, token)
	cfg["mcpServers"] = servers
}

// claudeCodeConfigPath returns the path to Claude Code's (claude CLI) config file.
// Claude Code reads ~/.claude.json for global MCP server configuration.
func claudeCodeConfigPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude.json")
}

// configureClaudeCode writes the muninn MCP entry into Claude Code's ~/.claude.json.
func configureClaudeCode(mcpURL, token string) error {
	path := claudeCodeConfigPath()
	summary, err := writeAIToolConfig(path, func(cfg map[string]any) {
		mergeClaudeCodeMCP(cfg, mcpURL, token)
	})
	if err != nil {
		return err
	}
	fmt.Printf("  ✓ Claude Code: %s\n    %s\n", summary, path)
	fmt.Println("  → No restart needed — Claude Code picks up MCP config automatically")
	return nil
}

// claudeDesktopConfigPath returns the path to Claude Desktop's config file on the current OS.
func claudeDesktopConfigPath() string {
	home, _ := os.UserHomeDir()
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(home, "Library", "Application Support", "Claude", "claude_desktop_config.json")
	case "windows":
		appData := os.Getenv("APPDATA")
		if appData == "" {
			appData = filepath.Join(home, "AppData", "Roaming")
		}
		return filepath.Join(appData, "Claude", "claude_desktop_config.json")
	default: // linux and others
		configDir := os.Getenv("XDG_CONFIG_HOME")
		if configDir == "" {
			configDir = filepath.Join(home, ".config")
		}
		return filepath.Join(configDir, "Claude", "claude_desktop_config.json")
	}
}

// cursorConfigPath returns the path to Cursor's MCP config file.
func cursorConfigPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".cursor", "mcp.json")
}

// windsurfConfigPath returns the path to Windsurf's MCP config file.
func windsurfConfigPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".codeium", "windsurf", "mcp_config.json")
}

// openClawConfigPath returns the path to OpenClaw's config file.
// macOS/Linux: ~/.openclaw/openclaw.json
// Windows:     %APPDATA%\OpenClaw\openclaw.json
func openClawConfigPath() string {
	if runtime.GOOS == "windows" {
		appData := os.Getenv("APPDATA")
		if appData == "" {
			home, _ := os.UserHomeDir()
			appData = filepath.Join(home, "AppData", "Roaming")
		}
		return filepath.Join(appData, "OpenClaw", "openclaw.json")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".openclaw", "openclaw.json")
}

// openCodeConfigPath returns the path to OpenCode's config file.
// macOS/Linux: ~/.config/opencode/opencode.json
// Windows:     %APPDATA%\opencode\opencode.json
func openCodeConfigPath() string {
	switch runtime.GOOS {
	case "windows":
		appData := os.Getenv("APPDATA")
		if appData == "" {
			home, _ := os.UserHomeDir()
			appData = filepath.Join(home, "AppData", "Roaming")
		}
		return filepath.Join(appData, "opencode", "opencode.json")
	default: // macOS and Linux — OpenCode uses XDG conventions on both
		if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
			return filepath.Join(xdg, "opencode", "opencode.json")
		}
		home, _ := os.UserHomeDir()
		return filepath.Join(home, ".config", "opencode", "opencode.json")
	}
}

// desktopMCPEntry returns a stdio MCP entry for Claude Desktop.
//
// Claude Desktop's config file (claude_desktop_config.json) only supports stdio
// transports — any "type":"http" or "type":"sse" field crashes the app on startup.
// The entry spawns the muninn binary as a subprocess; the built-in mcp proxy
// bridges stdin/stdout JSON-RPC to the running MuninnDB daemon over HTTP.
//
// The Bearer token and server URL are NOT embedded in the config: the proxy
// reads the token from ~/.muninn/mcp.token and connects to the default daemon
// port at runtime, so the config never needs to change after daemon restarts.
//
// binPath should be the absolute path to the muninn binary (from os.Executable),
// which avoids PATH lookup failures when Desktop spawns the subprocess.
func desktopMCPEntry(binPath string) map[string]any {
	return map[string]any{
		"command": binPath,
		"args":    []any{"mcp"},
	}
}

// mergeDesktopMCP upserts the muninn stdio entry into cfg["mcpServers"].
func mergeDesktopMCP(cfg map[string]any, binPath string) {
	servers, ok := cfg["mcpServers"].(map[string]any)
	if !ok {
		servers = map[string]any{}
	}
	servers["muninn"] = desktopMCPEntry(binPath)
	cfg["mcpServers"] = servers
}

// configureClaudeDesktop writes the muninn stdio MCP entry into Claude Desktop's config.
// mcpURL and token are accepted for interface compatibility but are not embedded in the
// config — the muninn mcp proxy reads them from disk at runtime.
func configureClaudeDesktop(_, _ string) error {
	// Resolve the absolute path to this binary so Desktop can spawn it without
	// relying on PATH, which is often minimal in GUI app environments.
	binPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve binary path: %w", err)
	}

	path := claudeDesktopConfigPath()
	summary, err := writeAIToolConfig(path, func(cfg map[string]any) {
		mergeDesktopMCP(cfg, binPath)
	})
	if err != nil {
		return err
	}
	fmt.Printf("  ✓ Claude Desktop: %s\n    %s\n", summary, path)
	fmt.Println("  → Restart Claude Desktop to activate MuninnDB memory")
	return nil
}

// configureCursor writes the muninn MCP entry into Cursor's mcp.json.
func configureCursor(mcpURL, token string) error {
	path := cursorConfigPath()
	summary, err := writeAIToolConfig(path, func(cfg map[string]any) {
		mergeMCPServers(cfg, mcpURL, token)
	})
	if err != nil {
		return err
	}
	fmt.Printf("  ✓ Cursor: %s\n    %s\n", summary, path)
	fmt.Println("  → Restart Cursor or reload MCP servers to activate")
	return nil
}

// configureWindsurf writes the muninn MCP entry into Windsurf's mcp_config.json.
func configureWindsurf(mcpURL, token string) error {
	path := windsurfConfigPath()
	summary, err := writeAIToolConfig(path, func(cfg map[string]any) {
		mergeMCPServers(cfg, mcpURL, token)
	})
	if err != nil {
		return err
	}
	fmt.Printf("  ✓ Windsurf: %s\n    %s\n", summary, path)
	fmt.Println("  → Restart Windsurf to activate MuninnDB memory")
	return nil
}

// openClawMCPEntry returns the JSON map for muninn's OpenClaw stdio MCP entry.
// OpenClaw spawns this as a local subprocess; the muninn binary handles
// auth internally by reading ~/.muninn/mcp.token at runtime.
func openClawMCPEntry() map[string]any {
	return map[string]any{
		"command":   "muninn",
		"args":      []any{"mcp"},
		"transport": "stdio",
	}
}

// mergeOpenClawMCP upserts muninn into the root-level cfg["mcpServers"] map,
// preserving all other entries. OpenClaw reads root-level mcpServers for
// stdio server definitions.
func mergeOpenClawMCP(cfg map[string]any) {
	servers, ok := cfg["mcpServers"].(map[string]any)
	if !ok {
		servers = map[string]any{}
	}
	servers["muninn"] = openClawMCPEntry()
	cfg["mcpServers"] = servers
}

// configureOpenClaw writes the muninn stdio MCP entry into OpenClaw's openclaw.json.
// The mcpURL and token parameters are accepted for interface compatibility but are
// not embedded in the config — the muninn mcp proxy reads the token at runtime.
func configureOpenClaw(_, _ string) error {
	path := openClawConfigPath()
	summary, err := writeAIToolConfig(path, func(cfg map[string]any) {
		mergeOpenClawMCP(cfg)
	})
	if err != nil {
		return err
	}
	fmt.Printf("  ✓ OpenClaw: %s\n    %s\n", summary, path)
	fmt.Println("  → Restart OpenClaw to activate MuninnDB memory")
	return nil
}

// openClawSkillContent is the SKILL.md content that teaches OpenClaw how to
// use MuninnDB for persistent memory across sessions.
const openClawSkillContent = `# MuninnDB Memory

MuninnDB is your persistent memory system, available via the "muninn" MCP server.

## When to use memory

- Store important facts, decisions, user preferences, and project context
- Recall relevant memories at the start of each conversation
- Be proactive — if the user shares something worth remembering, store it without being asked

## Available tools

- **muninn_remember** — store a memory (vault, concept, content)
- **muninn_recall** — search memories by context (vault, context)
- **muninn_read** — read a specific memory by ID (vault, id)
- **muninn_link** — link two related memories (vault, source_id, target_id)
- **muninn_guide** — learn MuninnDB best practices (call this on first connect)
- **muninn_remember_batch** — store multiple memories in one call (vault, memories[])

## Usage pattern

At the start of each session, call muninn_recall with relevant context to surface
what you know. When the user shares preferences, facts, or decisions, call
muninn_remember. Use vault "default" for general memories.
`

// openClawSkillPath returns the path to the muninn SKILL.md for OpenClaw.
// macOS/Linux: ~/.openclaw/skills/muninn/SKILL.md
// Windows:     %APPDATA%\OpenClaw\skills\muninn\SKILL.md
func openClawSkillPath() string {
	if runtime.GOOS == "windows" {
		appData := os.Getenv("APPDATA")
		if appData == "" {
			home, _ := os.UserHomeDir()
			appData = filepath.Join(home, "AppData", "Roaming")
		}
		return filepath.Join(appData, "OpenClaw", "skills", "muninn", "SKILL.md")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".openclaw", "skills", "muninn", "SKILL.md")
}

// configureOpenClawSkill writes the MuninnDB SKILL.md into OpenClaw's skills directory.
func configureOpenClawSkill() error {
	path := openClawSkillPath()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return fmt.Errorf("create skill directory: %w", err)
	}
	if err := os.WriteFile(path, []byte(openClawSkillContent), 0644); err != nil {
		return fmt.Errorf("write SKILL.md: %w", err)
	}
	fmt.Printf("  ✓ OpenClaw skill: wrote SKILL.md\n    %s\n", path)
	return nil
}

// configureOpenCode writes the muninn MCP entry into OpenCode's opencode.json.
func configureOpenCode(mcpURL, token string) error {
	path := openCodeConfigPath()

	// Capture whether "mcp" key exists before writeAIToolConfig runs,
	// so we can print an accurate summary (writeAIToolConfig hardcodes "mcpServers").
	hadMCP := false
	if existing, err := os.ReadFile(path); err == nil {
		var peek map[string]any
		if json.Unmarshal(existing, &peek) == nil {
			hadMCP = peek["mcp"] != nil
		}
	}

	_, err := writeAIToolConfig(path, func(cfg map[string]any) {
		mergeOpenCodeMCP(cfg, mcpURL, token)
	})
	if err != nil {
		return err
	}

	var summary string
	if hadMCP {
		summary = "updated mcp.muninn in existing config (other servers preserved)"
	} else {
		summary = "added mcp.muninn to config"
	}

	fmt.Printf("  ✓ OpenCode: %s\n    %s\n", summary, path)
	fmt.Println("  → Restart OpenCode to activate MuninnDB memory")
	return nil
}

// codexConfigPath returns the path to OpenAI Codex CLI's config file.
// Codex uses ~/.codex/config.toml for global MCP server configuration.
func codexConfigPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".codex", "config.toml")
}

// writeCodexTOMLConfig performs a read-merge-backup-write of Codex's TOML config.
// It preserves all existing keys and only adds/updates [mcp_servers.muninn].
func writeCodexTOMLConfig(path, mcpURL, token string) (string, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return "", fmt.Errorf("create config directory: %w", err)
	}

	existing, readErr := os.ReadFile(path)
	cfg := map[string]any{}
	if readErr == nil && len(existing) > 0 {
		if err := toml.Unmarshal(existing, &cfg); err != nil {
			return "", fmt.Errorf("existing config at %s contains invalid TOML: %w", path, err)
		}
	}

	// Backup before modification
	if readErr == nil && len(existing) > 0 {
		var origMode os.FileMode = 0644
		if info, err := os.Stat(path); err == nil {
			origMode = info.Mode().Perm()
		}
		if err := os.WriteFile(path+".bak", existing, origMode); err != nil {
			fmt.Fprintf(os.Stderr, "  warning: could not create backup %s.bak: %v\n", path, err)
		}
	}

	hadMCPServers := cfg["mcp_servers"] != nil

	servers, ok := cfg["mcp_servers"].(map[string]any)
	if !ok {
		servers = map[string]any{}
	}
	entry := map[string]any{
		"url": mcpURL,
	}
	if token != "" {
		entry["http_headers"] = map[string]any{
			"Authorization": "Bearer " + token,
		}
	}
	servers["muninn"] = entry
	cfg["mcp_servers"] = servers

	out, err := toml.Marshal(cfg)
	if err != nil {
		return "", fmt.Errorf("marshal merged config: %w", err)
	}

	tmpFile, err := os.CreateTemp(filepath.Dir(path), ".muninn_cfg_*.tmp")
	if err != nil {
		return "", fmt.Errorf("create temp file: %w", err)
	}
	defer os.Remove(tmpFile.Name())
	if _, err := tmpFile.Write(out); err != nil {
		tmpFile.Close()
		return "", fmt.Errorf("write temp file: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		return "", fmt.Errorf("close temp file: %w", err)
	}
	if err := os.Chmod(tmpFile.Name(), 0600); err != nil {
		return "", fmt.Errorf("chmod temp file: %w", err)
	}
	if err := os.Rename(tmpFile.Name(), path); err != nil {
		return "", fmt.Errorf("atomic rename: %w", err)
	}

	if hadMCPServers {
		return "updated mcp_servers.muninn in existing config (other servers preserved)", nil
	}
	return "added mcp_servers.muninn to config", nil
}

// configureCodex writes the muninn MCP entry into Codex's config.toml.
func configureCodex(mcpURL, token string) error {
	path := codexConfigPath()
	summary, err := writeCodexTOMLConfig(path, mcpURL, token)
	if err != nil {
		return err
	}
	fmt.Printf("  ✓ Codex: %s\n    %s\n", summary, path)
	fmt.Println("  → Restart Codex to activate MuninnDB memory")
	return nil
}

// printVSCodeInstructions prints manual setup steps for VS Code.
func printVSCodeInstructions(mcpURL, token string) {
	fmt.Println("  VS Code — add to your workspace .vscode/mcp.json:")
	snippet := map[string]any{
		"servers": map[string]any{
			"muninn": mcpServerEntry(mcpURL, token),
		},
	}
	b, _ := json.MarshalIndent(snippet, "    ", "  ")
	fmt.Printf("    %s\n", strings.ReplaceAll(string(b), "\n", "\n    "))
}

// printManualInstructions prints generic MCP connection info.
func printManualInstructions(mcpURL, token string) {
	fmt.Println("  MCP endpoint:", mcpURL)
	if token != "" {
		fmt.Println("  Authorization: Bearer", token)
	}
	fmt.Println()
	fmt.Println("  Add to your tool's MCP config:")
	snippet := map[string]any{
		"mcpServers": map[string]any{
			"muninn": mcpServerEntry(mcpURL, token),
		},
	}
	b, _ := json.MarshalIndent(snippet, "  ", "  ")
	fmt.Printf("  %s\n", strings.ReplaceAll(string(b), "\n", "\n  "))
	fmt.Println()
	fmt.Println("  Test it:")
	curlAuth := ""
	if token != "" {
		curlAuth = fmt.Sprintf(` -H "Authorization: Bearer %s"`, token)
	}
	fmt.Printf("    curl%s %s/mcp/health\n", curlAuth, mcpURL)
}

const claudeMDMemoryBlock = `# Memory Storage Preference

When asked to remember something — or when the user shares any preference, fact,
decision, or instruction worth remembering — **always use MuninnDB (muninn) MCP**.
Never use local auto memory. MuninnDB is the canonical memory system.

- **Store**: ` + "`mcp__muninn__muninn_remember`" + ` (vault, concept, content)
- **Recall**: ` + "`mcp__muninn__muninn_recall`" + ` (vault, context)
- **Read**: ` + "`mcp__muninn__muninn_read`" + ` (vault, id)
- **Link**: ` + "`mcp__muninn__muninn_link`" + ` (vault, source_id, target_id)
- **Guide**: ` + "`mcp__muninn__muninn_guide`" + ` — call this on first connect to learn best practices
- **Batch**: ` + "`mcp__muninn__muninn_remember_batch`" + ` (vault, memories[])

Use vault "default" unless the user specifies otherwise. Be proactive — if the user
shares something personal or important, store it without being asked.
`

// claudeMDPath returns the path to ~/.claude/CLAUDE.md.
func claudeMDPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude", "CLAUDE.md")
}

// configureClaudeMD writes the MuninnDB memory preference block into ~/.claude/CLAUDE.md.
// If the file already contains a MuninnDB block, it reports "already configured" and returns nil.
// If the file exists without one, the block is prepended. If missing, the file is created.
func configureClaudeMD() error {
	path := claudeMDPath()

	existing, err := os.ReadFile(path)
	if err == nil {
		if strings.Contains(string(existing), "mcp__muninn__muninn_remember") {
			fmt.Println("  ✓ CLAUDE.md already has MuninnDB memory preference")
			return nil
		}
		// Prepend the block to existing content.
		combined := claudeMDMemoryBlock + "\n---\n\n" + string(existing)
		if err := os.WriteFile(path, []byte(combined), 0644); err != nil {
			return fmt.Errorf("write %s: %w", path, err)
		}
		fmt.Printf("  ✓ CLAUDE.md updated: %s\n", path)
		return nil
	}

	// File doesn't exist — create directory and file.
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return fmt.Errorf("create directory: %w", err)
	}
	if err := os.WriteFile(path, []byte(claudeMDMemoryBlock), 0644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	fmt.Printf("  ✓ CLAUDE.md created: %s\n", path)
	return nil
}

// printClaudeMDInstructions prints manual instructions for configuring CLAUDE.md.
func printClaudeMDInstructions() {
	fmt.Println()
	fmt.Println("  ╭─ Optional: Claude Code memory preference ─────────────────╮")
	fmt.Println("  │                                                            │")
	fmt.Println("  │  To make Claude Code always use MuninnDB for memory,       │")
	fmt.Println("  │  add this to ~/.claude/CLAUDE.md:                          │")
	fmt.Println("  │                                                            │")
	fmt.Println("  │    # Memory Storage Preference                             │")
	fmt.Println("  │    Always use MuninnDB MCP for memory.                     │")
	fmt.Println("  │    Never use local auto memory.                            │")
	fmt.Println("  │                                                            │")
	fmt.Println("  │  Full block: muninn help claude-md                         │")
	fmt.Println("  │                                                            │")
	fmt.Println("  ╰────────────────────────────────────────────────────────────╯")
}
