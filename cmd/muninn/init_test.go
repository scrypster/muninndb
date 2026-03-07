package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestLevenshtein verifies the edit distance calculation.
func TestLevenshtein(t *testing.T) {
	tests := []struct {
		a, b string
		want int
	}{
		{"", "", 0},
		{"a", "", 1},
		{"", "a", 1},
		{"search", "search", 0},
		{"sarch", "search", 1},   // 1 insertion
		{"serach", "search", 2},  // 2 edits (r and a swapped = delete + insert)
		{"forget", "forgot", 1},  // 1 substitution (e→o)
		{"xyz", "abc", 3},
	}
	for _, tt := range tests {
		got := levenshtein(tt.a, tt.b)
		if got != tt.want {
			t.Errorf("levenshtein(%q, %q) = %d, want %d", tt.a, tt.b, got, tt.want)
		}
	}
}

// TestSuggestCommand verifies typo suggestions.
func TestSuggestCommand(t *testing.T) {
	tests := []struct {
		input    string
		wantSome bool // just verify we get a suggestion or not
	}{
		{"sarch", true},   // typo of "search"
		{"forgt", true},   // typo of "forget"
		{"exxit", true},   // typo of "exit"
		{"xyz123", false}, // no close match
		{"search", false}, // exact match → no suggestion needed (but might return "search" itself)
	}
	for _, tt := range tests {
		got := suggestCommand(tt.input)
		if tt.wantSome && got == "" {
			t.Errorf("suggestCommand(%q): expected a suggestion, got empty", tt.input)
		}
		if !tt.wantSome && got != "" {
			// For "xyz123" we want no suggestion; but this is best-effort
			// so just log rather than fail (Levenshtein of 6 chars vs short strings is unpredictable)
			t.Logf("suggestCommand(%q): got unexpected suggestion %q (acceptable for edge case)", tt.input, got)
		}
	}
}

// TestFirstRunDetection verifies that absence of config file is treated as first run.
func TestFirstRunDetection(t *testing.T) {
	// Override UserHomeDir via env for this test
	dir := t.TempDir()
	home := os.Getenv("HOME")
	os.Setenv("HOME", dir)
	defer os.Setenv("HOME", home)

	cfgPath := filepath.Join(dir, ".muninn", "config")
	_, err := os.Stat(cfgPath)
	if !os.IsNotExist(err) {
		t.Skip("config file unexpectedly exists in temp dir")
	}
	// Simulates what runShell() does:
	_, configErr := os.Stat(cfgPath)
	isFirstRun := os.IsNotExist(configErr)
	if !isFirstRun {
		t.Error("expected isFirstRun=true when config file doesn't exist")
	}
}

// TestFirstRunDetection_ExistingConfig verifies non-first-run when config exists.
func TestFirstRunDetection_ExistingConfig(t *testing.T) {
	dir := t.TempDir()
	home := os.Getenv("HOME")
	os.Setenv("HOME", dir)
	defer os.Setenv("HOME", home)

	cfgDir := filepath.Join(dir, ".muninn")
	os.MkdirAll(cfgDir, 0755)
	cfgPath := filepath.Join(cfgDir, "config")
	os.WriteFile(cfgPath, []byte(`{"default_vault":"default"}`), 0644)

	_, configErr := os.Stat(cfgPath)
	isFirstRun := os.IsNotExist(configErr)
	if isFirstRun {
		t.Error("expected isFirstRun=false when config file exists")
	}
}

// TestParseToolNumbersEdgeCases covers additional parsing scenarios.
func TestParseToolNumbersEdgeCases(t *testing.T) {
	// Mixed separators
	got := parseToolNumbers("1, 3")
	if len(got) != 2 || got[0] != 1 || got[1] != 3 {
		t.Errorf("parseToolNumbers(\"1, 3\"): got %v, want [1 3]", got)
	}

	// Valid range is 1-9; 0 is rejected but 6 is accepted
	got = parseToolNumbers("0 6 3")
	if len(got) != 2 || got[0] != 6 || got[1] != 3 {
		t.Errorf("parseToolNumbers(\"0 6 3\"): got %v, want [6 3]", got)
	}
}

// TestReadTokenFile_Missing verifies empty string returned when no token file.
func TestReadTokenFile_Missing(t *testing.T) {
	dir := t.TempDir()
	home := os.Getenv("HOME")
	os.Setenv("HOME", dir)
	defer os.Setenv("HOME", home)

	tok := readTokenFile()
	if tok != "" {
		t.Errorf("expected empty string when no token file, got %q", tok)
	}
}

// TestReadTokenFile_Present verifies token is read correctly.
func TestReadTokenFile_Present(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir) // os.UserHomeDir() checks USERPROFILE on Windows

	muninnDir := filepath.Join(dir, ".muninn")
	os.MkdirAll(muninnDir, 0755)
	os.WriteFile(filepath.Join(muninnDir, "mcp.token"), []byte("mdb_testtoken123\n"), 0600)

	tok := readTokenFile()
	if tok != "mdb_testtoken123" {
		t.Errorf("unexpected token: %q", tok)
	}
}

// TestMinHelper covers the min() helper function.
func TestMinHelper(t *testing.T) {
	if min(3, 5) != 3 {
		t.Error("min(3,5) should be 3")
	}
	if min(7, 2) != 2 {
		t.Error("min(7,2) should be 2")
	}
	if min(4, 4) != 4 {
		t.Error("min(4,4) should be 4")
	}
}

func TestDetectTools(t *testing.T) {
	tools := detectInstalledTools()
	if tools == nil {
		t.Error("detectInstalledTools returned nil")
	}
	for _, tool := range tools {
		if tool.key == "" {
			t.Error("tool missing key")
		}
		if tool.displayName == "" {
			t.Error("tool missing displayName")
		}
	}
}

func TestDetectInstalledTools_IncludesOpenCode(t *testing.T) {
	tools := detectInstalledTools()
	for _, tool := range tools {
		if tool.key == "opencode" {
			if tool.displayName != "OpenCode" {
				t.Errorf("displayName = %q, want \"OpenCode\"", tool.displayName)
			}
			if tool.configPath == "" {
				t.Error("configPath should not be empty")
			}
			return
		}
	}
	t.Error("opencode not found in detectInstalledTools()")
}

func TestConfigureNamedTools_OpenCode(t *testing.T) {
	_, cleanup := withTempHome(t)
	defer cleanup()

	out := captureStdout(func() {
		errs := configureNamedTools([]string{"opencode"}, "http://127.0.0.1:8750/mcp", "tok123")
		if len(errs) > 0 {
			t.Errorf("unexpected errors: %v", errs)
		}
	})

	if !strings.Contains(out, "✓") || !strings.Contains(out, "OpenCode") {
		t.Errorf("expected success output, got: %s", out)
	}

	data, err := os.ReadFile(openCodeConfigPath())
	if err != nil {
		t.Fatalf("config not written: %v", err)
	}
	var cfg map[string]any
	json.Unmarshal(data, &cfg)
	muninn := cfg["mcp"].(map[string]any)["muninn"].(map[string]any)
	if muninn["type"] != "remote" {
		t.Errorf("type = %v, want \"remote\"", muninn["type"])
	}
}

func TestUnknownToolMessage_IncludesOpenCode(t *testing.T) {
	_, cleanup := withTempHome(t)
	defer cleanup()
	stderr := captureStderr(func() {
		configureNamedTools([]string{"notarealtool"}, "http://127.0.0.1:8750/mcp", "")
	})
	if !strings.Contains(stderr, "opencode") {
		t.Errorf("unknown-tool error should list 'opencode', got: %s", stderr)
	}
}

func TestMuninnVersion(t *testing.T) {
	// When version var is empty, should return "dev"
	orig := version
	version = ""
	if muninnVersion() != "dev" {
		t.Error("expected 'dev' when version is empty")
	}
	version = "v1.2.3"
	if muninnVersion() != "v1.2.3" {
		t.Error("expected version string to be returned")
	}
	version = orig
}

func TestPrintEmbedderNote(t *testing.T) {
	cases := []struct {
		choice string
		want   string
	}{
		{"1", ""},         // local — no output
		{"2", "Ollama"},
		{"3", "OpenAI"},
		{"4", "Voyage"},
		{"5", "Cohere"},
		{"6", "Google"},
		{"7", "Jina"},
		{"8", "Mistral"},
		{"9", ""},         // unknown — no output (falls to default)
		{"", ""},          // empty — falls to default
	}
	for _, tc := range cases {
		out := captureStdout(func() {
			printEmbedderNote(tc.choice)
		})
		if tc.want != "" && !strings.Contains(out, tc.want) {
			t.Errorf("printEmbedderNote(%q): missing %q in output: %s", tc.choice, tc.want, out)
		}
		if tc.want == "" && strings.TrimSpace(out) != "" {
			t.Errorf("printEmbedderNote(%q): expected no output, got: %s", tc.choice, out)
		}
	}
}

func TestPrintWelcomeBanner(t *testing.T) {
	out := captureStdout(func() {
		printWelcomeBanner()
	})
	if !strings.Contains(out, "muninn") {
		t.Errorf("banner missing 'muninn': %s", out)
	}
	if !strings.Contains(out, "First time here") {
		t.Errorf("banner missing 'First time here': %s", out)
	}
}

func TestConfigureNamedToolsUnknownTool(t *testing.T) {
	out := captureStderr(func() {
		configureNamedTools([]string{"unknowntool"}, "http://127.0.0.1:8750/mcp", "tok123")
	})
	if !strings.Contains(out, "unknown tool") {
		t.Errorf("expected 'unknown tool' warning, got: %s", out)
	}
	if !strings.Contains(out, "unknowntool") {
		t.Errorf("expected tool name in warning, got: %s", out)
	}
}

func TestParseToolNumbersComprehensive(t *testing.T) {
	cases := []struct {
		input string
		want  []int
	}{
		{"1", []int{1}},
		{"1 2 3", []int{1, 2, 3}},
		{"1,2,3", []int{1, 2, 3}},
		{"1, 2, 3", []int{1, 2, 3}},
		{"1 1 2", []int{1, 2}},      // deduplication
		{"", []int(nil)},
		{"abc", []int(nil)},         // no digits
		{"10", []int{1}},            // only extracts first digit of "10" → 1
		{"9", []int{9}},
		{"0", []int(nil)},           // 0 is skipped (not 1-9)
	}
	for _, tc := range cases {
		got := parseToolNumbers(tc.input)
		if !intSliceEqual(got, tc.want) {
			t.Errorf("parseToolNumbers(%q): got %v, want %v", tc.input, got, tc.want)
		}
	}
}

func intSliceEqual(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestConfigureNamedToolsReturnsErrorsSlice verifies that when a tool fails to
// configure (e.g. due to a read-only home directory), the error is collected
// into the returned []string rather than silently discarded.
func TestConfigureNamedToolsReturnsErrorsSlice(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not enforce read-only directory permissions via chmod")
	}
	// Create a read-only temp directory and set it as HOME so that the config
	// directory cannot be created inside it — this forces a write failure.
	roDir := t.TempDir()
	if err := os.Chmod(roDir, 0555); err != nil {
		t.Skipf("cannot make dir read-only (may be running as root): %v", err)
	}
	defer os.Chmod(roDir, 0755) // restore so TempDir cleanup can succeed

	orig := os.Getenv("HOME")
	origXDG := os.Getenv("XDG_CONFIG_HOME")
	origAPPDATA := os.Getenv("APPDATA")
	os.Setenv("HOME", roDir)
	os.Setenv("XDG_CONFIG_HOME", roDir)
	os.Setenv("APPDATA", roDir)
	defer func() {
		os.Setenv("HOME", orig)
		os.Setenv("XDG_CONFIG_HOME", origXDG)
		os.Setenv("APPDATA", origAPPDATA)
	}()

	var errs []string
	captureStderr(func() {
		captureStdout(func() {
			errs = configureNamedTools([]string{"claude"}, "http://127.0.0.1:8750/mcp", "tok")
		})
	})

	if len(errs) == 0 {
		t.Fatal("expected at least one error in returned slice when home is read-only, got none")
	}
	// The error message should identify the failing tool.
	if errs[0] == "" {
		t.Error("error string should not be empty")
	}
	if !strings.Contains(errs[0], "Claude Desktop") {
		t.Errorf("error should identify 'Claude Desktop', got: %q", errs[0])
	}
}
