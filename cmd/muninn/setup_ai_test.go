package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestGenerateToken verifies token format and uniqueness.
func TestGenerateToken(t *testing.T) {
	tok1, err := generateToken()
	if err != nil {
		t.Fatalf("generateToken: %v", err)
	}
	if !strings.HasPrefix(tok1, "mdb_") {
		t.Errorf("token should start with mdb_, got %q", tok1)
	}
	// prefix (4) + 48 hex chars = 52 total
	if len(tok1) != 52 {
		t.Errorf("expected token length 52, got %d (%s)", len(tok1), tok1)
	}
	tok2, _ := generateToken()
	if tok1 == tok2 {
		t.Error("two generated tokens should not be equal")
	}
}

// TestLoadOrGenerateToken_NewToken verifies a fresh token is created when none exists.
func TestLoadOrGenerateToken_NewToken(t *testing.T) {
	dir := t.TempDir()
	tok, isNew, err := loadOrGenerateToken(filepath.Join(dir, "data"))
	if err != nil {
		t.Fatalf("loadOrGenerateToken: %v", err)
	}
	if !isNew {
		t.Error("expected isNew=true for fresh token")
	}
	if !strings.HasPrefix(tok, "mdb_") {
		t.Errorf("token should start with mdb_, got %q", tok)
	}
	// Verify file was written
	tokenFile := filepath.Join(dir, "mcp.token")
	b, err := os.ReadFile(tokenFile)
	if err != nil {
		t.Fatalf("token file not written: %v", err)
	}
	if strings.TrimSpace(string(b)) != tok {
		t.Errorf("token file content mismatch")
	}
	// Verify file permissions (Windows doesn't support Unix permissions)
	if runtime.GOOS != "windows" {
		info, _ := os.Stat(tokenFile)
		if info.Mode().Perm() != 0600 {
			t.Errorf("token file should be 0600, got %o", info.Mode().Perm())
		}
	}
}

// TestLoadOrGenerateToken_ExistingToken verifies an existing token is reused.
func TestLoadOrGenerateToken_ExistingToken(t *testing.T) {
	dir := t.TempDir()
	tokenFile := filepath.Join(dir, "mcp.token")
	os.WriteFile(tokenFile, []byte("mdb_existingtoken1234567890abcdef1234567890abcde\n"), 0600)

	tok, isNew, err := loadOrGenerateToken(filepath.Join(dir, "data"))
	if err != nil {
		t.Fatalf("loadOrGenerateToken: %v", err)
	}
	if isNew {
		t.Error("expected isNew=false when token file already exists")
	}
	if tok != "mdb_existingtoken1234567890abcdef1234567890abcde" {
		t.Errorf("unexpected token: %q", tok)
	}
}

// TestWriteAIToolConfig_NewFile verifies config creation when no file exists.
func TestWriteAIToolConfig_NewFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "claude_desktop_config.json")

	summary, err := writeAIToolConfig(path, func(cfg map[string]any) {
		mergeMCPServers(cfg, "http://127.0.0.1:8750/mcp", "mdb_testtoken")
	})
	if err != nil {
		t.Fatalf("writeAIToolConfig: %v", err)
	}
	if !strings.Contains(summary, "mcpServers.muninn") {
		t.Errorf("unexpected summary: %q", summary)
	}

	// Read back and verify
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("config file not written: %v", err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(b, &cfg); err != nil {
		t.Fatalf("config file is not valid JSON: %v", err)
	}
	servers, ok := cfg["mcpServers"].(map[string]any)
	if !ok {
		t.Fatal("mcpServers not found in config")
	}
	entry, ok := servers["muninn"].(map[string]any)
	if !ok {
		t.Fatal("muninn entry not found in mcpServers")
	}
	if entry["url"] != "http://127.0.0.1:8750/mcp" {
		t.Errorf("unexpected URL in config: %v", entry["url"])
	}
}

// TestWriteAIToolConfig_PreservesExistingServers verifies other mcpServers are not clobbered.
func TestWriteAIToolConfig_PreservesExistingServers(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")

	// Write existing config with another server
	existing := map[string]any{
		"mcpServers": map[string]any{
			"other-tool": map[string]any{"url": "http://other:9999"},
		},
		"someOtherKey": "someValue",
	}
	b, _ := json.Marshal(existing)
	os.WriteFile(path, b, 0600)

	_, err := writeAIToolConfig(path, func(cfg map[string]any) {
		mergeMCPServers(cfg, "http://127.0.0.1:8750/mcp", "")
	})
	if err != nil {
		t.Fatalf("writeAIToolConfig: %v", err)
	}

	// Read back
	b2, _ := os.ReadFile(path)
	var cfg map[string]any
	json.Unmarshal(b2, &cfg)

	servers := cfg["mcpServers"].(map[string]any)
	if _, ok := servers["other-tool"]; !ok {
		t.Error("other-tool server was removed")
	}
	if _, ok := servers["muninn"]; !ok {
		t.Error("muninn server not added")
	}
	if cfg["someOtherKey"] != "someValue" {
		t.Error("top-level key was removed")
	}
}

// TestWriteAIToolConfig_InvalidExistingJSON verifies graceful error on corrupt config.
func TestWriteAIToolConfig_InvalidExistingJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	os.WriteFile(path, []byte("this is not json {{{{"), 0644)

	_, err := writeAIToolConfig(path, func(cfg map[string]any) {
		mergeMCPServers(cfg, "http://127.0.0.1:8750/mcp", "")
	})
	if err == nil {
		t.Fatal("expected error for invalid JSON, got nil")
	}
	if !strings.Contains(err.Error(), "invalid JSON") {
		t.Errorf("expected 'invalid JSON' in error, got: %v", err)
	}
}

// TestWriteAIToolConfig_CreatesParentDir verifies missing parent directories are created.
func TestWriteAIToolConfig_CreatesParentDir(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "Claude", "claude_desktop_config.json")
	// Parent dir does NOT exist yet

	_, err := writeAIToolConfig(path, func(cfg map[string]any) {
		mergeMCPServers(cfg, "http://127.0.0.1:8750/mcp", "")
	})
	if err != nil {
		t.Fatalf("writeAIToolConfig should create parent dirs: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Error("config file not created")
	}
}

// TestWriteAIToolConfig_BackupCreated verifies .bak is created for existing files.
func TestWriteAIToolConfig_BackupCreated(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	original := []byte(`{"mcpServers":{}}`)
	os.WriteFile(path, original, 0644)

	writeAIToolConfig(path, func(cfg map[string]any) {
		mergeMCPServers(cfg, "http://127.0.0.1:8750/mcp", "")
	})

	bak, err := os.ReadFile(path + ".bak")
	if err != nil {
		t.Fatal("backup file not created")
	}
	if string(bak) != string(original) {
		t.Error("backup content does not match original")
	}
}

// TestWriteAIToolConfig_AtomicTempCleaned verifies temp file is cleaned up after success.
func TestWriteAIToolConfig_AtomicTempCleaned(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")

	writeAIToolConfig(path, func(cfg map[string]any) {
		mergeMCPServers(cfg, "http://127.0.0.1:8750/mcp", "")
	})

	// No temp files should remain
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("temp file not cleaned up: %s", e.Name())
		}
	}
}

// TestMCPServerEntry_WithToken verifies token is included when provided.
func TestMCPServerEntry_WithToken(t *testing.T) {
	entry := mcpServerEntry("http://127.0.0.1:8750/mcp", "mdb_abc123")
	if entry["url"] != "http://127.0.0.1:8750/mcp" {
		t.Errorf("unexpected url: %v", entry["url"])
	}
	if _, ok := entry["type"]; ok {
		t.Errorf("type field must not be present (causes Claude Desktop crash on startup)")
	}
	headers, ok := entry["headers"].(map[string]any)
	if !ok {
		t.Fatal("headers not found")
	}
	if headers["Authorization"] != "Bearer mdb_abc123" {
		t.Errorf("unexpected Authorization: %v", headers["Authorization"])
	}
}

// TestMCPServerEntry_NoToken verifies no headers when token is empty.
func TestMCPServerEntry_NoToken(t *testing.T) {
	entry := mcpServerEntry("http://127.0.0.1:8750/mcp", "")
	if _, ok := entry["headers"]; ok {
		t.Error("headers should not be present when token is empty")
	}
	if _, ok := entry["type"]; ok {
		t.Errorf("type field must not be present (causes Claude Desktop crash on startup)")
	}
}

// TestClaudeCodeMCPEntry_HasType verifies that the Claude Code entry includes "type":"http".
func TestClaudeCodeMCPEntry_HasType(t *testing.T) {
	entry := claudeCodeMCPEntry("http://127.0.0.1:8750/mcp", "")
	if entry["type"] != "http" {
		t.Errorf(`type = %v, want "http" — Claude Code schema requires this field`, entry["type"])
	}
	if entry["url"] != "http://127.0.0.1:8750/mcp" {
		t.Errorf("url = %v, want http://127.0.0.1:8750/mcp", entry["url"])
	}
}

// TestClaudeCodeMCPEntry_WithToken verifies token is included under headers.
func TestClaudeCodeMCPEntry_WithToken(t *testing.T) {
	entry := claudeCodeMCPEntry("http://127.0.0.1:8750/mcp", "mdb_tok")
	headers, ok := entry["headers"].(map[string]any)
	if !ok {
		t.Fatal("headers missing")
	}
	if headers["Authorization"] != "Bearer mdb_tok" {
		t.Errorf("Authorization = %v, want Bearer mdb_tok", headers["Authorization"])
	}
}

// TestClaudeCodeMCPEntry_NoToken verifies no headers when token is empty.
func TestClaudeCodeMCPEntry_NoToken(t *testing.T) {
	entry := claudeCodeMCPEntry("http://127.0.0.1:8750/mcp", "")
	if _, ok := entry["headers"]; ok {
		t.Error("headers should not be present when token is empty")
	}
}

// TestMergeClaudeCodeMCP_SetsTypeField verifies mergeClaudeCodeMCP writes "type":"http".
func TestMergeClaudeCodeMCP_SetsTypeField(t *testing.T) {
	cfg := map[string]any{}
	mergeClaudeCodeMCP(cfg, "http://127.0.0.1:8750/mcp", "tok")
	servers, ok := cfg["mcpServers"].(map[string]any)
	if !ok {
		t.Fatal("mcpServers missing")
	}
	muninn, ok := servers["muninn"].(map[string]any)
	if !ok {
		t.Fatal("mcpServers.muninn missing")
	}
	if muninn["type"] != "http" {
		t.Errorf(`type = %v, want "http"`, muninn["type"])
	}
}

// TestParseToolNumbers verifies tool selection parsing.
func TestParseToolNumbers(t *testing.T) {
	tests := []struct {
		input    string
		expected []int
	}{
		{"1", []int{1}},
		{"1 2 3", []int{1, 2, 3}},
		{"1,2,3", []int{1, 2, 3}},
		{"1 1 2", []int{1, 2}}, // deduplication
		{"", nil},
		{"6 7 8", []int{6, 7, 8}}, // valid range is 1-9
		{"abc", nil},              // non-numeric
	}
	for _, tt := range tests {
		got := parseToolNumbers(tt.input)
		if len(got) != len(tt.expected) {
			t.Errorf("parseToolNumbers(%q): got %v, want %v", tt.input, got, tt.expected)
			continue
		}
		for i := range got {
			if got[i] != tt.expected[i] {
				t.Errorf("parseToolNumbers(%q)[%d]: got %d, want %d", tt.input, i, got[i], tt.expected[i])
			}
		}
	}
}

// TestOpenCodeConfigPath verifies OpenCode config path is absolute and contains "opencode".
func TestOpenCodeConfigPath(t *testing.T) {
	path := openCodeConfigPath()
	if !filepath.IsAbs(path) {
		t.Errorf("path %q should be absolute", path)
	}
	if !strings.Contains(path, "opencode") {
		t.Errorf("path %q should contain 'opencode'", path)
	}
	if !strings.HasSuffix(path, "opencode.json") {
		t.Errorf("path %q should end with opencode.json", path)
	}
}

// TestOpenClawConfigPath verifies OpenClaw config path points to openclaw.json (not mcp.json).
func TestOpenClawConfigPath(t *testing.T) {
	path := openClawConfigPath()
	if path == "" {
		t.Error("openClawConfigPath returned empty string")
	}
	if !filepath.IsAbs(path) {
		t.Errorf("path %q should be absolute", path)
	}
	if !strings.HasSuffix(path, "openclaw.json") {
		t.Errorf("path %q should end with openclaw.json", path)
	}
	// Must contain the openclaw directory component (case-insensitive for Windows).
	if !strings.Contains(strings.ToLower(path), "openclaw") {
		t.Errorf("path %q should contain an openclaw directory component", path)
	}
}

// --- OpenClaw bad config cleanup (migration from v0.3.13-alpha) ---

func TestCleanupOpenClawBadConfig_RemovesMuninnEntry(t *testing.T) {
	_, cleanup := withTempHome(t)
	defer cleanup()

	path := openClawConfigPath()
	os.MkdirAll(filepath.Dir(path), 0755)
	// Simulate the broken v0.3.13-alpha config.
	os.WriteFile(path, []byte(`{"provider":{"mcpServers":{"muninn":{"transport":"streamable-http","url":"http://127.0.0.1:8750/mcp"}}},"topKey":"kept"}`), 0644)

	cleanupOpenClawBadConfig()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("config file missing after cleanup: %v", err)
	}
	var cfg map[string]any
	json.Unmarshal(data, &cfg)
	if _, ok := cfg["provider"]; ok {
		t.Error("provider key should be removed when mcpServers.muninn was the only entry")
	}
	if cfg["topKey"] != "kept" {
		t.Error("unrelated top-level key must be preserved")
	}
}

func TestCleanupOpenClawBadConfig_PreservesOtherServers(t *testing.T) {
	_, cleanup := withTempHome(t)
	defer cleanup()

	path := openClawConfigPath()
	os.MkdirAll(filepath.Dir(path), 0755)
	os.WriteFile(path, []byte(`{"provider":{"mcpServers":{"muninn":{"transport":"streamable-http"},"other":{"transport":"streamable-http","url":"http://x"}}}}`), 0644)

	cleanupOpenClawBadConfig()

	data, _ := os.ReadFile(path)
	var cfg map[string]any
	json.Unmarshal(data, &cfg)
	provider, ok := cfg["provider"].(map[string]any)
	if !ok {
		t.Fatal("provider key removed unexpectedly")
	}
	servers := provider["mcpServers"].(map[string]any)
	if _, ok := servers["muninn"]; ok {
		t.Error("muninn entry should be removed")
	}
	if _, ok := servers["other"]; !ok {
		t.Error("other server entry must be preserved")
	}
}

func TestCleanupOpenClawBadConfig_NoopWhenClean(t *testing.T) {
	_, cleanup := withTempHome(t)
	defer cleanup()
	// No file — should not error or create anything.
	cleanupOpenClawBadConfig()
	if _, err := os.ReadFile(openClawConfigPath()); err == nil {
		t.Error("cleanup should not create openclaw.json when it doesn't exist")
	}
}

// --- OpenClaw SKILL.md ---

func TestOpenClawSkillPath(t *testing.T) {
	_, cleanup := withTempHome(t)
	defer cleanup()
	path := openClawSkillPath()
	if !filepath.IsAbs(path) {
		t.Errorf("path %q should be absolute", path)
	}
	if !strings.HasSuffix(path, "SKILL.md") {
		t.Errorf("path %q should end with SKILL.md", path)
	}
	// Must be nested under a muninn skill directory.
	if !strings.Contains(strings.ToLower(path), "muninn") {
		t.Errorf("path %q should contain a muninn directory component", path)
	}
	// Must be nested under an openclaw directory.
	if !strings.Contains(strings.ToLower(path), "openclaw") {
		t.Errorf("path %q should contain an openclaw directory component", path)
	}
}

func TestConfigureOpenClawSkill_WritesFile(t *testing.T) {
	_, cleanup := withTempHome(t)
	defer cleanup()

	out := captureStdout(func() {
		if err := configureOpenClawSkill(""); err != nil {
			t.Fatalf("configureOpenClawSkill: %v", err)
		}
	})

	data, err := os.ReadFile(openClawSkillPath())
	if err != nil {
		t.Fatalf("SKILL.md not written: %v", err)
	}
	if !strings.Contains(string(data), "MuninnDB") {
		t.Error("SKILL.md should mention MuninnDB")
	}
	if !strings.Contains(string(data), "/api/engrams") {
		t.Error("SKILL.md should mention the REST API engrams endpoint")
	}
	if !strings.Contains(out, "SKILL.md") {
		t.Errorf("output should mention SKILL.md: %s", out)
	}
}

func TestConfigureOpenClawSkill_CreatesDirectory(t *testing.T) {
	_, cleanup := withTempHome(t)
	defer cleanup()

	dir := filepath.Dir(openClawSkillPath())
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Skip("directory already exists")
	}

	captureStdout(func() {
		if err := configureOpenClawSkill(""); err != nil {
			t.Fatalf("configureOpenClawSkill: %v", err)
		}
	})

	if _, err := os.Stat(dir); err != nil {
		t.Errorf("skill directory not created: %v", err)
	}
}

func TestOpenCodeMCPEntry_WithToken(t *testing.T) {
	entry := openCodeMCPEntry("http://127.0.0.1:8750/mcp", "mdb_testtoken123")
	if entry["type"] != "remote" {
		t.Errorf("type = %v, want \"remote\"", entry["type"])
	}
	if entry["oauth"] != false {
		t.Errorf("oauth = %v, want false", entry["oauth"])
	}
	headers, ok := entry["headers"].(map[string]any)
	if !ok {
		t.Fatal("headers not found when token supplied")
	}
	auth, _ := headers["Authorization"].(string)
	if auth != "Bearer {file:~/.muninn/mcp.token}" {
		t.Errorf("Authorization = %q, want file-template literal", auth)
	}
	if strings.Contains(fmt.Sprintf("%v", entry), "mdb_testtoken123") {
		t.Error("raw token value should NOT appear in entry")
	}
}

func TestOpenCodeMCPEntry_NoToken(t *testing.T) {
	entry := openCodeMCPEntry("http://127.0.0.1:8750/mcp", "")
	if entry["type"] != "remote" {
		t.Errorf("type = %v, want \"remote\"", entry["type"])
	}
	if entry["oauth"] != false {
		t.Errorf("oauth = %v, want false", entry["oauth"])
	}
	if _, ok := entry["headers"]; ok {
		t.Error("headers should not be present when token is empty")
	}
}

func TestMergeOpenCodeMCP_PreservesOtherEntries(t *testing.T) {
	cfg := map[string]any{
		"mcp": map[string]any{
			"other-tool": map[string]any{"type": "remote", "url": "http://other:9999"},
		},
		"topKey": "preserved",
	}
	mergeOpenCodeMCP(cfg, "http://127.0.0.1:8750/mcp", "tok")
	mcp := cfg["mcp"].(map[string]any)
	if _, ok := mcp["other-tool"]; !ok {
		t.Error("other-tool entry removed")
	}
	if _, ok := mcp["muninn"]; !ok {
		t.Error("muninn not added")
	}
	if cfg["topKey"] != "preserved" {
		t.Error("top-level key lost")
	}
}

func TestMergeOpenCodeMCP_EmptyConfig(t *testing.T) {
	cfg := map[string]any{}
	mergeOpenCodeMCP(cfg, "http://127.0.0.1:8750/mcp", "tok")
	mcp, ok := cfg["mcp"].(map[string]any)
	if !ok {
		t.Fatal("cfg[\"mcp\"] not a map")
	}
	if _, ok := mcp["muninn"]; !ok {
		t.Error("muninn not added")
	}
}

func TestConfigureOpenCode_WritesCorrectSchema(t *testing.T) {
	_, cleanup := withTempHome(t)
	defer cleanup()

	out := captureStdout(func() {
		if err := configureOpenCode("http://127.0.0.1:8750/mcp", "mdb_testtoken"); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	data, err := os.ReadFile(openCodeConfigPath())
	if err != nil {
		t.Fatalf("config file not written: %v", err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, data)
	}
	mcp := cfg["mcp"].(map[string]any)
	muninn := mcp["muninn"].(map[string]any)

	if muninn["type"] != "remote" {
		t.Errorf("type = %v, want \"remote\"", muninn["type"])
	}
	if muninn["oauth"] != false {
		t.Errorf("oauth = %v, want false", muninn["oauth"])
	}
	if !strings.Contains(out, "✓") || !strings.Contains(out, "OpenCode") {
		t.Errorf("output missing success marker: %s", out)
	}
	if !strings.Contains(out, "Restart OpenCode") {
		t.Errorf("output missing restart hint: %s", out)
	}
}

func TestConfigureOpenCode_NoToken(t *testing.T) {
	_, cleanup := withTempHome(t)
	defer cleanup()

	captureStdout(func() {
		configureOpenCode("http://127.0.0.1:8750/mcp", "")
	})

	data, _ := os.ReadFile(openCodeConfigPath())
	var cfg map[string]any
	json.Unmarshal(data, &cfg)
	muninn := cfg["mcp"].(map[string]any)["muninn"].(map[string]any)
	if _, ok := muninn["headers"]; ok {
		t.Error("headers should not be present without token")
	}
	if muninn["oauth"] != false {
		t.Error("oauth must be false even without token")
	}
}

func TestConfigureOpenCode_PreservesExistingEntries(t *testing.T) {
	_, cleanup := withTempHome(t)
	defer cleanup()

	path := openCodeConfigPath()
	os.MkdirAll(filepath.Dir(path), 0755)
	os.WriteFile(path, []byte(`{"mcp":{"other":{"type":"remote","url":"http://x"}},"topKey":"kept"}`), 0644)

	captureStdout(func() {
		configureOpenCode("http://127.0.0.1:8750/mcp", "tok")
	})

	data, _ := os.ReadFile(path)
	var cfg map[string]any
	json.Unmarshal(data, &cfg)
	if cfg["topKey"] != "kept" {
		t.Error("top-level key lost")
	}
	mcp := cfg["mcp"].(map[string]any)
	if _, ok := mcp["other"]; !ok {
		t.Error("other tool removed")
	}
	if _, ok := mcp["muninn"]; !ok {
		t.Error("muninn not added")
	}
}

func TestConfigureOpenCode_SummaryAdded(t *testing.T) {
	_, cleanup := withTempHome(t)
	defer cleanup()
	out := captureStdout(func() { configureOpenCode("http://127.0.0.1:8750/mcp", "tok") })
	if !strings.Contains(out, "added") {
		t.Errorf("expected 'added' in output for new config: %s", out)
	}
}

func TestConfigureOpenCode_SummaryUpdated(t *testing.T) {
	_, cleanup := withTempHome(t)
	defer cleanup()
	path := openCodeConfigPath()
	os.MkdirAll(filepath.Dir(path), 0755)
	os.WriteFile(path, []byte(`{"mcp":{"muninn":{"type":"remote","url":"http://127.0.0.1:8750/mcp","oauth":false}}}`), 0644)
	out := captureStdout(func() { configureOpenCode("http://127.0.0.1:8750/mcp", "tok") })
	if !strings.Contains(out, "updated") {
		t.Errorf("expected 'updated' in output for existing mcp: %s", out)
	}
}

// Helper to override HOME in tests
func withTempHome(t *testing.T) (string, func()) {
	t.Helper()
	tmp := t.TempDir()
	orig := os.Getenv("HOME")
	os.Setenv("HOME", tmp)
	// Also set XDG_CONFIG_HOME to temp dir for Linux tests
	origXDG := os.Getenv("XDG_CONFIG_HOME")
	os.Setenv("XDG_CONFIG_HOME", tmp)
	// Also set APPDATA for Windows tests
	origAPPDATA := os.Getenv("APPDATA")
	os.Setenv("APPDATA", tmp)
	// os.UserHomeDir() on Windows checks USERPROFILE, not HOME
	origUP := os.Getenv("USERPROFILE")
	os.Setenv("USERPROFILE", tmp)
	return tmp, func() {
		os.Setenv("HOME", orig)
		os.Setenv("XDG_CONFIG_HOME", origXDG)
		os.Setenv("APPDATA", origAPPDATA)
		os.Setenv("USERPROFILE", origUP)
	}
}

// TestConfigureClaudeDesktopWritesConfig verifies Claude Desktop config is written at the
// correct path with a stdio entry (command + args), not an HTTP entry (url + headers).
// Claude Desktop v1.1.4010+ crashes on startup if any mcpServers entry has type:"http".
func TestConfigureClaudeDesktopWritesConfig(t *testing.T) {
	home, cleanup := withTempHome(t)
	defer cleanup()

	out := captureStdout(func() {
		err := configureClaudeDesktop("http://127.0.0.1:8750/mcp", "mdb_testtoken123")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	// Path should be inside temp home
	configPath := claudeDesktopConfigPath()
	if !strings.HasPrefix(configPath, home) {
		t.Errorf("config path %q should be inside temp home %q", configPath, home)
	}

	// Read and verify the written JSON
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("config file not written: %v", err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("invalid JSON written: %v\ncontents: %s", err, data)
	}

	servers, ok := cfg["mcpServers"].(map[string]any)
	if !ok {
		t.Fatalf("mcpServers not found in config: %s", data)
	}
	muninn, ok := servers["muninn"].(map[string]any)
	if !ok {
		t.Fatalf("mcpServers.muninn not found: %s", data)
	}

	// Must be a stdio entry: command + args, no url/headers/type.
	if muninn["command"] == nil || muninn["command"] == "" {
		t.Errorf("command field missing or empty — Desktop config must use stdio transport: %s", data)
	}
	args, ok := muninn["args"].([]any)
	if !ok || len(args) == 0 || args[0] != "mcp" {
		t.Errorf("args should be [\"mcp\"], got %v: %s", muninn["args"], data)
	}
	if muninn["url"] != nil {
		t.Errorf("url must not be present in Desktop config (causes schema crash): %s", data)
	}
	if muninn["headers"] != nil {
		t.Errorf("headers must not be present in Desktop config: %s", data)
	}
	if muninn["type"] != nil {
		t.Errorf("type must not be present in Desktop config (causes crash): %s", data)
	}

	// Output should contain success marker
	if !strings.Contains(out, "✓") {
		t.Errorf("output missing success marker '✓': %s", out)
	}
}

// TestConfigureClaudeDesktopNoToken verifies Desktop config is still stdio even when no token.
// The token is read at runtime by muninn mcp, so it never appears in the config file.
func TestConfigureClaudeDesktopNoToken(t *testing.T) {
	_, cleanup := withTempHome(t)
	defer cleanup()

	captureStdout(func() {
		if err := configureClaudeDesktop("http://127.0.0.1:8750/mcp", ""); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	data, _ := os.ReadFile(claudeDesktopConfigPath())
	var cfg map[string]any
	json.Unmarshal(data, &cfg)
	servers := cfg["mcpServers"].(map[string]any)
	muninn := servers["muninn"].(map[string]any)

	// stdio entry: command present, no url/headers regardless of token presence.
	if muninn["command"] == nil {
		t.Error("command field missing — Desktop config must use stdio transport")
	}
	if muninn["url"] != nil {
		t.Error("url must not be present in Desktop config")
	}
	if muninn["headers"] != nil {
		t.Error("headers must not be present in Desktop config")
	}
}

// TestConfigureClaudeDesktopPreservesExistingKeys verifies existing config keys are not lost.
func TestConfigureClaudeDesktopPreservesExistingKeys(t *testing.T) {
	_, cleanup := withTempHome(t)
	defer cleanup()

	// Pre-populate with an existing MCP server
	path := claudeDesktopConfigPath()
	os.MkdirAll(filepath.Dir(path), 0755)
	existing := `{"mcpServers":{"other-tool":{"url":"http://other.example"}},"someOtherKey":"preserved"}`
	os.WriteFile(path, []byte(existing), 0644)

	captureStdout(func() {
		configureClaudeDesktop("http://127.0.0.1:8750/mcp", "tok123")
	})

	data, _ := os.ReadFile(path)
	var cfg map[string]any
	json.Unmarshal(data, &cfg)

	// Original key preserved
	if cfg["someOtherKey"] != "preserved" {
		t.Errorf("someOtherKey was lost: %s", data)
	}
	servers := cfg["mcpServers"].(map[string]any)
	// Original server preserved
	if _, ok := servers["other-tool"]; !ok {
		t.Errorf("other-tool was lost: %s", data)
	}
	// muninn added
	if _, ok := servers["muninn"]; !ok {
		t.Errorf("muninn not added: %s", data)
	}
}

// TestConfigureCursorWritesConfig verifies Cursor config is written correctly.
func TestConfigureCursorWritesConfig(t *testing.T) {
	_, cleanup := withTempHome(t)
	defer cleanup()

	out := captureStdout(func() {
		if err := configureCursor("http://127.0.0.1:8750/mcp", "tok"); err != nil {
			t.Fatalf("error: %v", err)
		}
	})

	data, err := os.ReadFile(cursorConfigPath())
	if err != nil {
		t.Fatalf("file not written: %v", err)
	}
	if !strings.Contains(string(data), `"muninn"`) {
		t.Errorf("muninn not in config: %s", data)
	}
	if !strings.Contains(string(data), "8750") {
		t.Errorf("MCP port not in config: %s", data)
	}
	if !strings.Contains(out, "✓") {
		t.Errorf("output missing success marker: %s", out)
	}
}

// TestConfigureWindsurfWritesConfig verifies Windsurf config is written correctly.
func TestConfigureWindsurfWritesConfig(t *testing.T) {
	_, cleanup := withTempHome(t)
	defer cleanup()

	out := captureStdout(func() {
		if err := configureWindsurf("http://127.0.0.1:8750/mcp", "tok"); err != nil {
			t.Fatalf("error: %v", err)
		}
	})

	data, err := os.ReadFile(windsurfConfigPath())
	if err != nil {
		t.Fatalf("file not written: %v", err)
	}
	if !strings.Contains(string(data), `"muninn"`) {
		t.Errorf("muninn not in config: %s", data)
	}
	if !strings.Contains(string(data), "8750") {
		t.Errorf("MCP port not in config: %s", data)
	}
	if !strings.Contains(out, "✓") {
		t.Errorf("output missing success marker: %s", out)
	}
}


// TestOpenClawSkillHasFrontmatter verifies the SKILL.md content includes valid YAML frontmatter
// so that OpenClaw recognizes and loads the skill.
func TestOpenClawSkillHasFrontmatter(t *testing.T) {
	if !strings.HasPrefix(buildOpenClawSkillContent(""), "---\n") {
		t.Error("SKILL.md must start with YAML frontmatter delimiter ---")
	}
	if !strings.Contains(buildOpenClawSkillContent(""), "name:") {
		t.Error("SKILL.md frontmatter must include name field")
	}
	if !strings.Contains(buildOpenClawSkillContent(""), "description:") {
		t.Error("SKILL.md frontmatter must include description field")
	}
	if !strings.Contains(buildOpenClawSkillContent(""), "metadata:") {
		t.Error("SKILL.md frontmatter must include metadata section")
	}
	if !strings.Contains(buildOpenClawSkillContent(""), "bins:") {
		t.Error("SKILL.md frontmatter must include requires.bins")
	}
	if !strings.Contains(buildOpenClawSkillContent(""), "- curl") {
		t.Error("SKILL.md frontmatter requires.bins must list curl (REST API uses curl)")
	}
}

// TestCodexConfigPath verifies Codex config path is set correctly.
func TestCodexConfigPath(t *testing.T) {
	path := codexConfigPath()
	if path == "" {
		t.Error("codexConfigPath returned empty string")
	}
	home, _ := os.UserHomeDir()
	if !strings.HasPrefix(path, home) {
		t.Errorf("path %q should start with home dir", path)
	}
	if !strings.HasSuffix(path, filepath.Join(".codex", "config.toml")) {
		t.Errorf("path %q should end with .codex/config.toml", path)
	}
}

// TestConfigureCodexWritesConfig verifies Codex config is written correctly as TOML.
func TestConfigureCodexWritesConfig(t *testing.T) {
	_, cleanup := withTempHome(t)
	defer cleanup()

	out := captureStdout(func() {
		if err := configureCodex("http://127.0.0.1:8750/mcp", "mdb_testtoken"); err != nil {
			t.Fatalf("error: %v", err)
		}
	})

	data, err := os.ReadFile(codexConfigPath())
	if err != nil {
		t.Fatalf("file not written: %v", err)
	}
	content := string(data)
	if !strings.Contains(content, "muninn") {
		t.Errorf("muninn not in config: %s", content)
	}
	if !strings.Contains(content, "http://127.0.0.1:8750/mcp") {
		t.Errorf("MCP URL not in config: %s", content)
	}
	if !strings.Contains(content, "Bearer mdb_testtoken") {
		t.Errorf("token not in config: %s", content)
	}
	if !strings.Contains(out, "✓") {
		t.Errorf("output missing success marker: %s", out)
	}
}

// TestConfigureCodexNoToken verifies no auth header when token is empty.
func TestConfigureCodexNoToken(t *testing.T) {
	_, cleanup := withTempHome(t)
	defer cleanup()

	captureStdout(func() {
		if err := configureCodex("http://127.0.0.1:8750/mcp", ""); err != nil {
			t.Fatalf("error: %v", err)
		}
	})

	data, _ := os.ReadFile(codexConfigPath())
	content := string(data)
	if strings.Contains(content, "Bearer") {
		t.Errorf("should not have auth header without token: %s", content)
	}
	if strings.Contains(content, "http_headers") {
		t.Errorf("should not have http_headers without token: %s", content)
	}
	if !strings.Contains(content, "http://127.0.0.1:8750/mcp") {
		t.Errorf("URL missing: %s", content)
	}
}

// TestConfigureCodexPreservesExistingKeys verifies existing TOML keys are not lost.
func TestConfigureCodexPreservesExistingKeys(t *testing.T) {
	_, cleanup := withTempHome(t)
	defer cleanup()

	path := codexConfigPath()
	os.MkdirAll(filepath.Dir(path), 0755)
	existing := `model = "o3-mini"

[mcp_servers.other-tool]
url = "http://other.example"
`
	os.WriteFile(path, []byte(existing), 0644)

	captureStdout(func() {
		configureCodex("http://127.0.0.1:8750/mcp", "tok123")
	})

	data, _ := os.ReadFile(path)
	content := string(data)

	if !strings.Contains(content, `o3-mini`) {
		t.Errorf("model key was lost: %s", content)
	}
	if !strings.Contains(content, "other-tool") {
		t.Errorf("other-tool server was lost: %s", content)
	}
	if !strings.Contains(content, "muninn") {
		t.Errorf("muninn not added: %s", content)
	}
}

// TestWriteCodexTOMLConfig_InvalidTOML verifies graceful error on corrupt config.
func TestWriteCodexTOMLConfig_InvalidTOML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	os.WriteFile(path, []byte("this is not valid toml = = = [[["), 0644)

	_, err := writeCodexTOMLConfig(path, "http://127.0.0.1:8750/mcp", "")
	if err == nil {
		t.Fatal("expected error for invalid TOML, got nil")
	}
	if !strings.Contains(err.Error(), "invalid TOML") {
		t.Errorf("expected 'invalid TOML' in error, got: %v", err)
	}
}

// TestWriteCodexTOMLConfig_BackupCreated verifies .bak is created for existing files.
func TestWriteCodexTOMLConfig_BackupCreated(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	original := []byte("[mcp_servers]\n")
	os.WriteFile(path, original, 0644)

	writeCodexTOMLConfig(path, "http://127.0.0.1:8750/mcp", "")

	bak, err := os.ReadFile(path + ".bak")
	if err != nil {
		t.Fatal("backup file not created")
	}
	if string(bak) != string(original) {
		t.Error("backup content does not match original")
	}
}

// TestConfigureNamedToolsCodex verifies codex tool configures Codex.
func TestConfigureNamedToolsCodex(t *testing.T) {
	_, cleanup := withTempHome(t)
	defer cleanup()

	out := captureStdout(func() {
		configureNamedTools([]string{"codex"}, "http://127.0.0.1:8750/mcp", "tok123", "")
	})
	if !strings.Contains(out, "✓") {
		t.Errorf("expected success marker for codex tool, got: %s", out)
	}

	path := codexConfigPath()
	if _, err := os.ReadFile(path); err != nil {
		t.Errorf("codex config file not written: %v", err)
	}
}

// TestPrintVSCodeInstructions verifies VS Code instructions contain required elements.
func TestPrintVSCodeInstructions(t *testing.T) {
	out := captureStdout(func() {
		printVSCodeInstructions("http://127.0.0.1:8750/mcp", "mdb_mytoken")
	})
	if !strings.Contains(out, `"muninn"`) {
		t.Errorf("missing muninn key: %s", out)
	}
	if !strings.Contains(out, "8750") {
		t.Errorf("missing MCP URL: %s", out)
	}
	if !strings.Contains(out, "mdb_mytoken") {
		t.Errorf("missing token: %s", out)
	}
	if !strings.Contains(out, "Bearer") {
		t.Errorf("missing Bearer auth: %s", out)
	}
	// VS Code uses "servers" not "mcpServers"
	if !strings.Contains(out, `"servers"`) {
		t.Errorf("VS Code format should use 'servers' key: %s", out)
	}
}

// TestPrintVSCodeInstructionsNoToken verifies no auth header without token.
func TestPrintVSCodeInstructionsNoToken(t *testing.T) {
	out := captureStdout(func() {
		printVSCodeInstructions("http://127.0.0.1:8750/mcp", "")
	})
	if strings.Contains(out, "Bearer") {
		t.Errorf("should not have auth header without token: %s", out)
	}
}

// TestPrintManualInstructions verifies manual instructions contain required elements.
func TestPrintManualInstructions(t *testing.T) {
	out := captureStdout(func() {
		printManualInstructions("http://127.0.0.1:8750/mcp", "mdb_secrettoken")
	})
	if !strings.Contains(out, "mcpServers") {
		t.Errorf("missing mcpServers: %s", out)
	}
	if !strings.Contains(out, "mdb_secrettoken") {
		t.Errorf("missing token: %s", out)
	}
	if !strings.Contains(out, "curl") {
		t.Errorf("missing curl test command: %s", out)
	}
	if !strings.Contains(out, "Bearer mdb_secrettoken") {
		t.Errorf("missing auth in curl: %s", out)
	}
}

// TestPrintManualInstructionsNoToken verifies curl command appears without token.
func TestPrintManualInstructionsNoToken(t *testing.T) {
	out := captureStdout(func() {
		printManualInstructions("http://127.0.0.1:8750/mcp", "")
	})
	if strings.Contains(out, "Bearer") {
		t.Errorf("should not have auth header without token: %s", out)
	}
	// curl command should still appear
	if !strings.Contains(out, "curl") {
		t.Errorf("missing curl command: %s", out)
	}
}

// TestConfigureNamedToolsClaudeDesktop verifies claude alias configures Claude Desktop.
func TestConfigureNamedToolsClaudeDesktop(t *testing.T) {
	_, cleanup := withTempHome(t)
	defer cleanup()

	out := captureStdout(func() {
		configureNamedTools([]string{"claude"}, "http://127.0.0.1:8750/mcp", "tok123", "")
	})
	if !strings.Contains(out, "✓") {
		t.Errorf("expected success marker for claude tool, got: %s", out)
	}

	// Verify file was written
	path := claudeDesktopConfigPath()
	if _, err := os.ReadFile(path); err != nil {
		t.Errorf("claude Desktop config file not written: %v", err)
	}
}

// TestConfigureNamedToolsClaudeDesktopAlias verifies claude-desktop alias works.
func TestConfigureNamedToolsClaudeDesktopAlias(t *testing.T) {
	_, cleanup := withTempHome(t)
	defer cleanup()

	out := captureStdout(func() {
		configureNamedTools([]string{"claude-desktop"}, "http://127.0.0.1:8750/mcp", "tok", "")
	})
	if !strings.Contains(out, "✓") {
		t.Errorf("claude-desktop alias should work: %s", out)
	}
}

// TestConfigureNamedToolsCursor verifies cursor tool configures Cursor.
func TestConfigureNamedToolsCursor(t *testing.T) {
	_, cleanup := withTempHome(t)
	defer cleanup()

	out := captureStdout(func() {
		configureNamedTools([]string{"cursor"}, "http://127.0.0.1:8750/mcp", "tok123", "")
	})
	if !strings.Contains(out, "✓") {
		t.Errorf("expected success marker for cursor tool, got: %s", out)
	}

	path := cursorConfigPath()
	if _, err := os.ReadFile(path); err != nil {
		t.Errorf("cursor config file not written: %v", err)
	}
}

// TestConfigureNamedToolsWindsurf verifies windsurf tool configures Windsurf.
func TestConfigureNamedToolsWindsurf(t *testing.T) {
	_, cleanup := withTempHome(t)
	defer cleanup()

	out := captureStdout(func() {
		configureNamedTools([]string{"windsurf"}, "http://127.0.0.1:8750/mcp", "tok123", "")
	})
	if !strings.Contains(out, "✓") {
		t.Errorf("expected success marker for windsurf tool, got: %s", out)
	}

	path := windsurfConfigPath()
	if _, err := os.ReadFile(path); err != nil {
		t.Errorf("windsurf config file not written: %v", err)
	}
}

// TestConfigureNamedToolsOpenClaw verifies openclaw tool installs the SKILL.md
// and does NOT write to openclaw.json (OpenClaw has no native MCP support).
func TestConfigureNamedToolsOpenClaw(t *testing.T) {
	_, cleanup := withTempHome(t)
	defer cleanup()

	out := captureStdout(func() {
		configureNamedTools([]string{"openclaw"}, "http://127.0.0.1:8750/mcp", "tok123", "")
	})
	if !strings.Contains(out, "✓") {
		t.Errorf("expected success marker for openclaw tool, got: %s", out)
	}

	// SKILL.md must be written.
	if _, err := os.ReadFile(openClawSkillPath()); err != nil {
		t.Errorf("SKILL.md not written: %v", err)
	}
	// openclaw.json must NOT be written — provider is not a valid OpenClaw key.
	if _, err := os.ReadFile(openClawConfigPath()); err == nil {
		t.Error("openclaw.json should not be written; OpenClaw has no native MCP support")
	}
}

// TestConfigureNamedToolsVSCode verifies vscode tool shows instructions.
func TestConfigureNamedToolsVSCode(t *testing.T) {
	_, cleanup := withTempHome(t)
	defer cleanup()

	out := captureStdout(func() {
		configureNamedTools([]string{"vscode"}, "http://127.0.0.1:8750/mcp", "", "")
	})
	if !strings.Contains(out, "VS Code") {
		t.Errorf("expected VS Code instructions, got: %s", out)
	}
	if !strings.Contains(out, `"servers"`) {
		t.Errorf("expected VS Code format with 'servers' key: %s", out)
	}
}

// TestConfigureNamedToolsVSCodeAlias verifies vs-code alias works.
func TestConfigureNamedToolsVSCodeAlias(t *testing.T) {
	_, cleanup := withTempHome(t)
	defer cleanup()

	out := captureStdout(func() {
		configureNamedTools([]string{"vs-code"}, "http://127.0.0.1:8750/mcp", "", "")
	})
	if !strings.Contains(out, "VS Code") {
		t.Errorf("expected VS Code instructions with vs-code alias: %s", out)
	}
}

// TestConfigureNamedToolsManual verifies manual tool shows manual instructions.
func TestConfigureNamedToolsManual(t *testing.T) {
	_, cleanup := withTempHome(t)
	defer cleanup()

	out := captureStdout(func() {
		configureNamedTools([]string{"manual"}, "http://127.0.0.1:8750/mcp", "", "")
	})
	if !strings.Contains(out, "mcpServers") {
		t.Errorf("expected manual instructions, got: %s", out)
	}
	if !strings.Contains(out, "curl") {
		t.Errorf("expected curl test command: %s", out)
	}
}

// TestConfigureNamedToolsOtherAlias verifies other alias works for manual.
func TestConfigureNamedToolsOtherAlias(t *testing.T) {
	_, cleanup := withTempHome(t)
	defer cleanup()

	out := captureStdout(func() {
		configureNamedTools([]string{"other"}, "http://127.0.0.1:8750/mcp", "", "")
	})
	if !strings.Contains(out, "mcpServers") {
		t.Errorf("expected manual instructions with 'other' alias: %s", out)
	}
}

// TestConfigureNamedToolsMultiple verifies multiple tools can be configured in one call.
func TestConfigureNamedToolsMultiple(t *testing.T) {
	_, cleanup := withTempHome(t)
	defer cleanup()

	out := captureStdout(func() {
		configureNamedTools([]string{"claude", "cursor"}, "http://127.0.0.1:8750/mcp", "tok123", "")
	})

	// Both should succeed
	if strings.Count(out, "✓") < 2 {
		t.Errorf("expected 2 success markers for 2 tools, got: %s", out)
	}

	// Both files should exist
	claudePath := claudeDesktopConfigPath()
	cursorPath := cursorConfigPath()
	if _, err := os.ReadFile(claudePath); err != nil {
		t.Errorf("claude config not written: %v", err)
	}
	if _, err := os.ReadFile(cursorPath); err != nil {
		t.Errorf("cursor config not written: %v", err)
	}
}

// TestConfigureNamedToolsUnknownToolSetupAI verifies unknown tools are gracefully ignored with error message.
func TestConfigureNamedToolsUnknownToolSetupAI(t *testing.T) {
	_, cleanup := withTempHome(t)
	defer cleanup()

	stderr := captureStderr(func() {
		configureNamedTools([]string{"nonexistent"}, "http://127.0.0.1:8750/mcp", "", "")
	})
	if !strings.Contains(stderr, "unknown tool") {
		t.Errorf("expected error for unknown tool, got stderr: %s", stderr)
	}
}

// TestConfigureClaudeMD_NewFile creates CLAUDE.md from scratch.
func TestConfigureClaudeMD_NewFile(t *testing.T) {
	_, cleanup := withTempHome(t)
	defer cleanup()

	out := captureStdout(func() {
		if err := configureClaudeMD(""); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	path := claudeMDPath()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("CLAUDE.md not created: %v", err)
	}
	content := string(data)
	if !strings.Contains(content, "mcp__muninn__muninn_remember") {
		t.Error("missing muninn_remember tool reference")
	}
	if !strings.Contains(content, "mcp__muninn__muninn_recall") {
		t.Error("missing muninn_recall tool reference")
	}
	if !strings.Contains(content, "mcp__muninn__muninn_guide") {
		t.Error("missing muninn_guide tool reference")
	}
	if !strings.Contains(content, "Memory Storage Preference") {
		t.Error("missing Memory Storage Preference header")
	}
	if !strings.Contains(out, "✓") {
		t.Errorf("output missing success marker: %s", out)
	}
	if !strings.Contains(out, "created") {
		t.Errorf("output should say 'created' for new file: %s", out)
	}
}

// TestConfigureClaudeMD_PrependsToExisting prepends the block to an existing CLAUDE.md.
func TestConfigureClaudeMD_PrependsToExisting(t *testing.T) {
	_, cleanup := withTempHome(t)
	defer cleanup()

	path := claudeMDPath()
	os.MkdirAll(filepath.Dir(path), 0755)
	existing := "# My Existing Instructions\n\nDo things my way.\n"
	os.WriteFile(path, []byte(existing), 0644)

	out := captureStdout(func() {
		if err := configureClaudeMD(""); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	data, _ := os.ReadFile(path)
	content := string(data)

	// MuninnDB block should be at the top
	if !strings.HasPrefix(content, "# Memory Storage Preference") {
		t.Error("MuninnDB block should be prepended to the top")
	}
	// Original content should still be there
	if !strings.Contains(content, "My Existing Instructions") {
		t.Error("original content should be preserved")
	}
	if !strings.Contains(content, "Do things my way.") {
		t.Error("original instructions should be preserved")
	}
	// Separator between sections
	if !strings.Contains(content, "---") {
		t.Error("should have a separator between MuninnDB block and existing content")
	}
	if !strings.Contains(out, "updated") {
		t.Errorf("output should say 'updated' for existing file: %s", out)
	}
}

// TestConfigureClaudeMD_AlreadyConfigured skips if MuninnDB block already exists.
func TestConfigureClaudeMD_AlreadyConfigured(t *testing.T) {
	_, cleanup := withTempHome(t)
	defer cleanup()

	path := claudeMDPath()
	os.MkdirAll(filepath.Dir(path), 0755)
	existing := "# Memory Storage Preference\n\nmcp__muninn__muninn_remember already here\n"
	os.WriteFile(path, []byte(existing), 0644)

	out := captureStdout(func() {
		if err := configureClaudeMD(""); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	// Should not modify the file
	data, _ := os.ReadFile(path)
	if string(data) != existing {
		t.Error("file should not be modified when already configured")
	}
	if !strings.Contains(out, "already") {
		t.Errorf("output should indicate already configured: %s", out)
	}
}

// TestConfigureClaudeMD_CreatesDirectory creates ~/.claude/ if it doesn't exist.
func TestConfigureClaudeMD_CreatesDirectory(t *testing.T) {
	_, cleanup := withTempHome(t)
	defer cleanup()

	captureStdout(func() {
		if err := configureClaudeMD(""); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	path := claudeMDPath()
	if _, err := os.Stat(path); err != nil {
		t.Errorf("CLAUDE.md should exist: %v", err)
	}
}

// TestHasClaudeCode verifies tool list detection.
func TestHasClaudeCode(t *testing.T) {
	tests := []struct {
		tools    []string
		expected bool
	}{
		{[]string{"claude-code"}, true},
		{[]string{"claudecode"}, true},
		{[]string{"claude", "claude-code", "cursor"}, true},
		{[]string{"claude", "cursor"}, false},
		{[]string{}, false},
		{nil, false},
	}
	for _, tt := range tests {
		got := hasClaudeCode(tt.tools)
		if got != tt.expected {
			t.Errorf("hasClaudeCode(%v) = %v, want %v", tt.tools, got, tt.expected)
		}
	}
}

// TestPrintClaudeMDInstructions verifies manual instructions are printed.
func TestPrintClaudeMDInstructions(t *testing.T) {
	out := captureStdout(func() {
		printClaudeMDInstructions()
	})
	if !strings.Contains(out, "CLAUDE.md") {
		t.Errorf("should mention CLAUDE.md: %s", out)
	}
	if !strings.Contains(out, "MuninnDB") {
		t.Errorf("should mention MuninnDB: %s", out)
	}
}

// TestClaudeMDPath verifies the path is under ~/.claude/.
func TestClaudeMDPath(t *testing.T) {
	path := claudeMDPath()
	if !strings.HasSuffix(path, filepath.Join(".claude", "CLAUDE.md")) {
		t.Errorf("unexpected path: %s", path)
	}
}

// ---------------------------------------------------------------------------
// Behavior mode content generation tests
// ---------------------------------------------------------------------------

// TestBuildClaudeMDMemoryBlock_ModeVariants verifies that each behavior mode
// produces distinct, non-empty content with the correct proactivity instruction.
func TestBuildClaudeMDMemoryBlock_ModeVariants(t *testing.T) {
	cases := []struct {
		mode   string
		wantIn string
	}{
		{"autonomous", "proactive"},
		{"prompted", "explicitly asks"},
		{"selective", "Automatically store decisions"},
		{"custom", "custom memory instructions"},
		{"", "proactive"}, // empty = autonomous default
	}
	for _, tc := range cases {
		got := buildClaudeMDMemoryBlock(tc.mode)
		if !strings.Contains(got, "MuninnDB") {
			t.Errorf("mode=%q: missing 'MuninnDB' in output", tc.mode)
		}
		if !strings.Contains(got, tc.wantIn) {
			t.Errorf("mode=%q: expected %q in output, got:\n%s", tc.mode, tc.wantIn, got)
		}
	}
	// Verify modes produce distinct outputs.
	autonomous := buildClaudeMDMemoryBlock("autonomous")
	prompted := buildClaudeMDMemoryBlock("prompted")
	selective := buildClaudeMDMemoryBlock("selective")
	if autonomous == prompted {
		t.Error("autonomous and prompted should produce different CLAUDE.md blocks")
	}
	if autonomous == selective {
		t.Error("autonomous and selective should produce different CLAUDE.md blocks")
	}
	if prompted == selective {
		t.Error("prompted and selective should produce different CLAUDE.md blocks")
	}
}

// TestBuildOpenClawSkillContent_ModeVariants verifies that each behavior mode
// produces distinct usage pattern text in the SKILL.md content.
func TestBuildOpenClawSkillContent_ModeVariants(t *testing.T) {
	cases := []struct {
		mode   string
		wantIn string
	}{
		{"autonomous", "Be proactive"},
		{"prompted", "ONLY store memories when the user explicitly asks"},
		{"selective", "Automatically store"},
		{"custom", "Be proactive"},   // custom falls through to proactive default
		{"", "Be proactive"},
	}
	for _, tc := range cases {
		got := buildOpenClawSkillContent(tc.mode)
		if !strings.HasPrefix(got, "---\n") {
			t.Errorf("mode=%q: must start with YAML frontmatter", tc.mode)
		}
		if !strings.Contains(got, tc.wantIn) {
			t.Errorf("mode=%q: expected %q in output, got:\n%s", tc.mode, tc.wantIn, got)
		}
	}
}

// TestBuildOpenClawSkillContent_AllModesHaveUsagePattern verifies the ## Usage pattern
// section is present for every mode.
func TestBuildOpenClawSkillContent_AllModesHaveUsagePattern(t *testing.T) {
	for _, mode := range []string{"", "autonomous", "prompted", "selective", "custom"} {
		got := buildOpenClawSkillContent(mode)
		if !strings.Contains(got, "## Usage pattern") {
			t.Errorf("mode=%q: missing '## Usage pattern' section", mode)
		}
		if !strings.Contains(got, "/api/engrams") {
			t.Errorf("mode=%q: missing REST API endpoint reference", mode)
		}
	}
}
