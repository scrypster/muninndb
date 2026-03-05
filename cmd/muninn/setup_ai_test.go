package main

import (
	"encoding/json"
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
		mergeMCPServers(cfg, "http://localhost:8750/mcp", "mdb_testtoken")
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
	if entry["url"] != "http://localhost:8750/mcp" {
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
		mergeMCPServers(cfg, "http://localhost:8750/mcp", "")
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
		mergeMCPServers(cfg, "http://localhost:8750/mcp", "")
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
		mergeMCPServers(cfg, "http://localhost:8750/mcp", "")
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
		mergeMCPServers(cfg, "http://localhost:8750/mcp", "")
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
		mergeMCPServers(cfg, "http://localhost:8750/mcp", "")
	})

	// No temp files should remain
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp.") {
			t.Errorf("temp file not cleaned up: %s", e.Name())
		}
	}
}

// TestMCPServerEntry_WithToken verifies token is included when provided.
func TestMCPServerEntry_WithToken(t *testing.T) {
	entry := mcpServerEntry("http://localhost:8750/mcp", "mdb_abc123")
	if entry["url"] != "http://localhost:8750/mcp" {
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
	entry := mcpServerEntry("http://localhost:8750/mcp", "")
	if _, ok := entry["headers"]; ok {
		t.Error("headers should not be present when token is empty")
	}
	if _, ok := entry["type"]; ok {
		t.Errorf("type field must not be present (causes Claude Desktop crash on startup)")
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
		{"abc", nil},    // non-numeric
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

// TestOpenClawConfigPath verifies OpenClaw config path is set correctly.
func TestOpenClawConfigPath(t *testing.T) {
	path := openClawConfigPath()
	if path == "" {
		t.Error("openClawConfigPath returned empty string")
	}
	home, _ := os.UserHomeDir()
	if !strings.HasPrefix(path, home) {
		t.Errorf("path %q should start with home dir", path)
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

// TestConfigureClaudeDesktopWritesConfig verifies Claude Desktop config is written at correct path with correct JSON.
func TestConfigureClaudeDesktopWritesConfig(t *testing.T) {
	home, cleanup := withTempHome(t)
	defer cleanup()

	mcpURL := "http://localhost:8750/mcp"
	token := "mdb_testtoken123"

	out := captureStdout(func() {
		err := configureClaudeDesktop(mcpURL, token)
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
	if muninn["url"] != mcpURL {
		t.Errorf("url = %v, want %q", muninn["url"], mcpURL)
	}
	headers, ok := muninn["headers"].(map[string]any)
	if !ok {
		t.Fatalf("headers not found when token supplied: %s", data)
	}
	if headers["Authorization"] != "Bearer "+token {
		t.Errorf("Authorization = %v, want %q", headers["Authorization"], "Bearer "+token)
	}

	// Output should contain success marker
	if !strings.Contains(out, "✓") {
		t.Errorf("output missing success marker '✓': %s", out)
	}
}

// TestConfigureClaudeDesktopNoToken verifies no auth header is written when token is empty.
func TestConfigureClaudeDesktopNoToken(t *testing.T) {
	_, cleanup := withTempHome(t)
	defer cleanup()

	captureStdout(func() {
		if err := configureClaudeDesktop("http://localhost:8750/mcp", ""); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	data, _ := os.ReadFile(claudeDesktopConfigPath())
	var cfg map[string]any
	json.Unmarshal(data, &cfg)
	servers := cfg["mcpServers"].(map[string]any)
	muninn := servers["muninn"].(map[string]any)

	if _, hasHeaders := muninn["headers"]; hasHeaders {
		t.Error("headers should not be present when token is empty")
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
		configureClaudeDesktop("http://localhost:8750/mcp", "tok123")
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
		if err := configureCursor("http://localhost:8750/mcp", "tok"); err != nil {
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
		if err := configureWindsurf("http://localhost:8750/mcp", "tok"); err != nil {
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

// TestConfigureOpenClawWritesConfig verifies OpenClaw config is written correctly.
func TestConfigureOpenClawWritesConfig(t *testing.T) {
	_, cleanup := withTempHome(t)
	defer cleanup()

	out := captureStdout(func() {
		if err := configureOpenClaw("http://localhost:8750/mcp", "tok"); err != nil {
			t.Fatalf("error: %v", err)
		}
	})

	data, err := os.ReadFile(openClawConfigPath())
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
		if err := configureCodex("http://localhost:8750/mcp", "mdb_testtoken"); err != nil {
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
	if !strings.Contains(content, "http://localhost:8750/mcp") {
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
		if err := configureCodex("http://localhost:8750/mcp", ""); err != nil {
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
	if !strings.Contains(content, "http://localhost:8750/mcp") {
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
		configureCodex("http://localhost:8750/mcp", "tok123")
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

	_, err := writeCodexTOMLConfig(path, "http://localhost:8750/mcp", "")
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

	writeCodexTOMLConfig(path, "http://localhost:8750/mcp", "")

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
		configureNamedTools([]string{"codex"}, "http://localhost:8750/mcp", "tok123")
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
		printVSCodeInstructions("http://localhost:8750/mcp", "mdb_mytoken")
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
		printVSCodeInstructions("http://localhost:8750/mcp", "")
	})
	if strings.Contains(out, "Bearer") {
		t.Errorf("should not have auth header without token: %s", out)
	}
}

// TestPrintManualInstructions verifies manual instructions contain required elements.
func TestPrintManualInstructions(t *testing.T) {
	out := captureStdout(func() {
		printManualInstructions("http://localhost:8750/mcp", "mdb_secrettoken")
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
		printManualInstructions("http://localhost:8750/mcp", "")
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
		configureNamedTools([]string{"claude"}, "http://localhost:8750/mcp", "tok123")
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
		configureNamedTools([]string{"claude-desktop"}, "http://localhost:8750/mcp", "tok")
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
		configureNamedTools([]string{"cursor"}, "http://localhost:8750/mcp", "tok123")
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
		configureNamedTools([]string{"windsurf"}, "http://localhost:8750/mcp", "tok123")
	})
	if !strings.Contains(out, "✓") {
		t.Errorf("expected success marker for windsurf tool, got: %s", out)
	}

	path := windsurfConfigPath()
	if _, err := os.ReadFile(path); err != nil {
		t.Errorf("windsurf config file not written: %v", err)
	}
}

// TestConfigureNamedToolsOpenClaw verifies openclaw tool configures OpenClaw.
func TestConfigureNamedToolsOpenClaw(t *testing.T) {
	_, cleanup := withTempHome(t)
	defer cleanup()

	out := captureStdout(func() {
		configureNamedTools([]string{"openclaw"}, "http://localhost:8750/mcp", "tok123")
	})
	if !strings.Contains(out, "✓") {
		t.Errorf("expected success marker for openclaw tool, got: %s", out)
	}

	path := openClawConfigPath()
	if _, err := os.ReadFile(path); err != nil {
		t.Errorf("openclaw config file not written: %v", err)
	}
}

// TestConfigureNamedToolsVSCode verifies vscode tool shows instructions.
func TestConfigureNamedToolsVSCode(t *testing.T) {
	_, cleanup := withTempHome(t)
	defer cleanup()

	out := captureStdout(func() {
		configureNamedTools([]string{"vscode"}, "http://localhost:8750/mcp", "")
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
		configureNamedTools([]string{"vs-code"}, "http://localhost:8750/mcp", "")
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
		configureNamedTools([]string{"manual"}, "http://localhost:8750/mcp", "")
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
		configureNamedTools([]string{"other"}, "http://localhost:8750/mcp", "")
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
		configureNamedTools([]string{"claude", "cursor"}, "http://localhost:8750/mcp", "tok123")
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
		configureNamedTools([]string{"nonexistent"}, "http://localhost:8750/mcp", "")
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
		if err := configureClaudeMD(); err != nil {
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
		if err := configureClaudeMD(); err != nil {
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
		if err := configureClaudeMD(); err != nil {
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
		if err := configureClaudeMD(); err != nil {
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
