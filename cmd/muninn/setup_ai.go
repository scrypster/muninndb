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

// mcpServerEntry returns the JSON map for muninn's MCP server entry.
// Note: "type" is intentionally omitted. Claude Desktop v1.1.4010+ crashes
// on startup with a TypeError if "type":"http" is present in any mcpServers
// entry. The MCP client infers transport from the URL schema.
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
		mergeMCPServers(cfg, mcpURL, token)
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

// openClawConfigPath returns the path to OpenClaw's MCP config file.
func openClawConfigPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".openclaw", "mcp.json")
}

// configureClaudeDesktop writes the muninn MCP entry into Claude Desktop's config.
func configureClaudeDesktop(mcpURL, token string) error {
	path := claudeDesktopConfigPath()
	summary, err := writeAIToolConfig(path, func(cfg map[string]any) {
		mergeMCPServers(cfg, mcpURL, token)
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

// configureOpenClaw writes the muninn MCP entry into OpenClaw's mcp.json.
func configureOpenClaw(mcpURL, token string) error {
	path := openClawConfigPath()
	summary, err := writeAIToolConfig(path, func(cfg map[string]any) {
		mergeMCPServers(cfg, mcpURL, token)
	})
	if err != nil {
		return err
	}
	fmt.Printf("  ✓ OpenClaw: %s\n    %s\n", summary, path)
	fmt.Println("  → Restart OpenClaw to activate muninn memory")
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
