package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestLoadEnvFile_SetsVars(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "muninn.env")
	os.WriteFile(path, []byte("MUNINN_TEST_VAR=hello\n"), 0600)

	t.Setenv("MUNINN_TEST_VAR", "")
	os.Unsetenv("MUNINN_TEST_VAR")
	loadEnvFileFrom(path)
	if got := os.Getenv("MUNINN_TEST_VAR"); got != "hello" {
		t.Errorf("expected hello, got %q", got)
	}
}

func TestLoadEnvFile_ShellEnvWins(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "muninn.env")
	os.WriteFile(path, []byte("MUNINN_TEST_WIN=from_file\n"), 0600)
	t.Setenv("MUNINN_TEST_WIN", "from_shell")

	loadEnvFileFrom(path)
	if got := os.Getenv("MUNINN_TEST_WIN"); got != "from_shell" {
		t.Errorf("shell env should win, got %q", got)
	}
}

func TestLoadEnvFile_NonMuninnKeyIgnored(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "muninn.env")
	os.WriteFile(path, []byte("PATH=/evil\nMUNINN_OK=yes\n"), 0600)
	origPath := os.Getenv("PATH")
	t.Setenv("MUNINN_OK", "")
	os.Unsetenv("MUNINN_OK")

	loadEnvFileFrom(path)
	if os.Getenv("PATH") != origPath {
		t.Error("PATH should not be modified")
	}
	if os.Getenv("MUNINN_OK") != "yes" {
		t.Error("MUNINN_OK should be set")
	}
}

func TestLoadEnvFile_CommentsAndBlanksSkipped(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "muninn.env")
	content := "# comment\n\nMUNINN_REAL=value\n# another comment\n"
	os.WriteFile(path, []byte(content), 0600)
	t.Setenv("MUNINN_REAL", "")
	os.Unsetenv("MUNINN_REAL")

	loadEnvFileFrom(path)
	if os.Getenv("MUNINN_REAL") != "value" {
		t.Error("MUNINN_REAL should be set")
	}
}

func TestLoadEnvFile_QuotedValues(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "muninn.env")
	os.WriteFile(path, []byte("MUNINN_Q1=\"hello world\"\nMUNINN_Q2='single'\n"), 0600)
	t.Setenv("MUNINN_Q1", "")
	os.Unsetenv("MUNINN_Q1")
	t.Setenv("MUNINN_Q2", "")
	os.Unsetenv("MUNINN_Q2")

	loadEnvFileFrom(path)
	if got := os.Getenv("MUNINN_Q1"); got != "hello world" {
		t.Errorf("double-quoted: got %q", got)
	}
	if got := os.Getenv("MUNINN_Q2"); got != "single" {
		t.Errorf("single-quoted: got %q", got)
	}
}

func TestLoadEnvFile_ExportPrefix(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "muninn.env")
	os.WriteFile(path, []byte("export MUNINN_EXPORTED=yes\n"), 0600)
	t.Setenv("MUNINN_EXPORTED", "")
	os.Unsetenv("MUNINN_EXPORTED")

	loadEnvFileFrom(path)
	if os.Getenv("MUNINN_EXPORTED") != "yes" {
		t.Error("export prefix should be stripped")
	}
}

func TestLoadEnvFile_MissingFileIsNoOp(t *testing.T) {
	// Should not panic or error
	loadEnvFileFrom("/nonexistent/path/muninn.env")
}

func TestLoadEnvFile_OversizedFileSkipped(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "muninn.env")

	// Build a file >64KB containing valid MUNINN lines at the start
	var content strings.Builder
	content.WriteString("MUNINN_OVERSIZED=should_not_be_set\n")
	// pad to exceed the 64KB limit
	for content.Len() < 65*1024 {
		content.WriteString("# padding\n")
	}
	os.WriteFile(path, []byte(content.String()), 0600)

	t.Setenv("MUNINN_OVERSIZED", "")
	os.Unsetenv("MUNINN_OVERSIZED")

	loadEnvFileFrom(path)
	if os.Getenv("MUNINN_OVERSIZED") != "" {
		t.Error("oversized file should be skipped entirely")
	}
}

func TestLoadEnvFile_SymlinkSkipped(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real.env")
	if err := os.WriteFile(real, []byte("MUNINN_SYM=bad\n"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "muninn.env")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MUNINN_SYM", "")
	os.Unsetenv("MUNINN_SYM")

	loadEnvFileFrom(link)
	if os.Getenv("MUNINN_SYM") != "" {
		t.Error("symlink should be rejected, MUNINN_SYM should not be set")
	}
}

func TestBuildEnvFileContent_OllamaEmbedder(t *testing.T) {
	content := buildEnvFileContent("ollama", "")
	if !strings.Contains(content, "MUNINN_OLLAMA_URL=ollama://") {
		t.Error("expected active MUNINN_OLLAMA_URL line")
	}
	if strings.Contains(content, "# MUNINN_OLLAMA_URL=") {
		t.Error("MUNINN_OLLAMA_URL should be active, not commented")
	}
}

func TestBuildEnvFileContent_OpenAIEmbedder(t *testing.T) {
	content := buildEnvFileContent("openai", "")
	if !strings.Contains(content, "MUNINN_OPENAI_KEY=") {
		t.Error("expected active MUNINN_OPENAI_KEY line")
	}
	if strings.Contains(content, "# MUNINN_OPENAI_KEY=") {
		t.Error("MUNINN_OPENAI_KEY should be active, not commented")
	}
}

func TestBuildEnvFileContent_LocalEmbedder(t *testing.T) {
	content := buildEnvFileContent("local", "")
	// local embedder — MUNINN_OLLAMA_URL should be commented
	if strings.Contains(content, "\nMUNINN_OLLAMA_URL=") {
		t.Error("MUNINN_OLLAMA_URL should be commented for local embedder")
	}
}

func TestBuildEnvFileContent_EnrichURL(t *testing.T) {
	content := buildEnvFileContent("local", "anthropic://claude-haiku-4-5-20251001")
	if !strings.Contains(content, "MUNINN_ENRICH_URL=anthropic://claude-haiku-4-5-20251001") {
		t.Error("expected active MUNINN_ENRICH_URL line")
	}
}

func TestWriteEnvFileTo_CreatesFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "muninn.env")

	created, err := writeEnvFileTo(path, "ollama", "")
	if err != nil {
		t.Fatalf("writeEnvFileTo: %v", err)
	}
	if !created {
		t.Error("expected created=true for new file")
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("file not created: %v", err)
	}
	// Windows does not support Unix-style permission bits; chmod is a no-op there.
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
		t.Errorf("expected 0600, got %o", info.Mode().Perm())
	}
	content, _ := os.ReadFile(path)
	if !strings.Contains(string(content), "MUNINN_OLLAMA_URL=ollama://") {
		t.Error("expected active MUNINN_OLLAMA_URL in file")
	}
}

func TestWriteEnvFileTo_NoOverwriteExisting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "muninn.env")
	original := []byte("# user customized\nMUNINN_CUSTOM=yes\n")
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}

	created, err := writeEnvFileTo(path, "ollama", "")
	if err != nil {
		t.Fatalf("writeEnvFileTo: %v", err)
	}
	if created {
		t.Error("expected created=false for existing file")
	}

	after, _ := os.ReadFile(path)
	if string(after) != string(original) {
		t.Error("existing muninn.env should not be overwritten")
	}
}
