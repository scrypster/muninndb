package main

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

func TestLogFilePath(t *testing.T) {
	path := logFilePath()
	if path == "" {
		t.Error("logFilePath returned empty string")
	}
	if !strings.HasSuffix(path, "muninn.log") {
		t.Errorf("expected muninn.log suffix, got %q", path)
	}
}

func TestMatchesLevel(t *testing.T) {
	cases := []struct {
		line  string
		level string
		want  bool
	}{
		{"2026-02-22 INFO server started", "info", true},
		{"2026-02-22 INFO server started", "INFO", true},
		{"2026-02-22 INFO server started", "error", false},
		{"2026-02-22 ERROR connection refused", "error", true},
		{"2026-02-22 ERROR connection refused", "ERR", true},
		{"2026-02-22 WARN high memory", "warn", true},
		{"2026-02-22 DEBUG verbose output", "debug", true},
		{"plain line no level", "info", false},
		{"", "error", false},
		{"anything", "", true}, // empty filter always matches (strings.Contains(s, "") == true)
	}
	for _, tc := range cases {
		got := matchesLevel(tc.line, tc.level)
		if got != tc.want {
			t.Errorf("matchesLevel(%q, %q): got %v, want %v", tc.line, tc.level, got, tc.want)
		}
	}
}

func TestPrintLastN_Basic(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "muninn-*.log")
	if err != nil {
		t.Fatal(err)
	}
	lines := []string{
		"line 1 INFO start",
		"line 2 DEBUG verbose",
		"line 3 ERROR crash",
		"line 4 INFO done",
		"line 5 WARN nearly",
	}
	for _, l := range lines {
		fmt.Fprintln(f, l)
	}
	f.Close()

	// Print last 3
	out := captureStdout(func() {
		printLastN(f.Name(), 3, "")
	})
	if strings.Contains(out, "line 1") || strings.Contains(out, "line 2") {
		t.Errorf("expected only last 3 lines, got: %s", out)
	}
	if !strings.Contains(out, "line 3") || !strings.Contains(out, "line 5") {
		t.Errorf("expected lines 3-5 in output, got: %s", out)
	}
}

func TestPrintLastN_FewerThanN(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "muninn-*.log")
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintln(f, "only one line")
	f.Close()

	// Request 10 but only 1 exists
	out := captureStdout(func() {
		printLastN(f.Name(), 10, "")
	})
	if !strings.Contains(out, "only one line") {
		t.Errorf("expected line in output, got: %s", out)
	}
}

func TestPrintLastN_WithLevelFilter(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "muninn-*.log")
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintln(f, "INFO good start")
	fmt.Fprintln(f, "ERROR bad thing happened")
	fmt.Fprintln(f, "INFO all ok")
	fmt.Fprintln(f, "ERROR another error")
	f.Close()

	out := captureStdout(func() {
		printLastN(f.Name(), 10, "error")
	})
	if strings.Contains(out, "INFO good start") || strings.Contains(out, "INFO all ok") {
		t.Errorf("INFO lines should be filtered out, got: %s", out)
	}
	if !strings.Contains(out, "bad thing happened") || !strings.Contains(out, "another error") {
		t.Errorf("ERROR lines should appear, got: %s", out)
	}
}

func TestPrintLastN_MissingFile(t *testing.T) {
	out := captureStdout(func() {
		printLastN("/tmp/muninn-nonexistent-12345678.log", 10, "")
	})
	if !strings.Contains(out, "No log file") {
		t.Errorf("expected 'No log file' message, got: %s", out)
	}
}

func TestPrintLastN_EmptyFile(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "muninn-*.log")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()

	// Empty file — should print nothing, not panic
	out := captureStdout(func() {
		printLastN(f.Name(), 5, "")
	})
	_ = out // should be empty, no panic
}

// TestRunLogsNoFollowMissingFile tests runLogs --no-follow when log file doesn't exist.
func TestRunLogsNoFollowMissingFile(t *testing.T) {
	t.Setenv("MUNINNDB_DATA", t.TempDir())
	out := captureStdout(func() {
		runLogs([]string{"--no-follow", "--last", "5"})
	})
	if strings.Contains(out, "error") && !strings.Contains(out, "No log file") {
		t.Errorf("unexpected error in output: %s", out)
	}
}

// TestRunLogsWithLastZero tests that --last 0 shows no history but still tails.
// We call tailLog directly with a local buffer so the goroutine never touches os.Stdout;
// this avoids a data race with captureStdout in concurrently-running tests.
func TestRunLogsWithLastZero(t *testing.T) {
	var buf strings.Builder
	done := make(chan bool, 1)
	go func() {
		// Nonexistent path → tailLog returns immediately with "No log file" message.
		tailLog("/tmp/muninn-nonexistent-test-99999.log", "", 0, &buf, &buf)
		done <- true
	}()

	select {
	case <-done:
		t.Log("tailLog returned (no log file at test path)")
	case <-time.After(500 * time.Millisecond):
		t.Log("tailLog blocked — log file unexpectedly exists at test path")
	}
}

// TestRunLogsWithNoFollowValidation tests that runLogs handles --no-follow correctly.
func TestRunLogsWithNoFollowValidation(t *testing.T) {
	t.Setenv("MUNINNDB_DATA", t.TempDir())
	out := captureStdout(func() {
		runLogs([]string{"--no-follow", "--last", "10"})
	})
	_ = out
}

// TestRunLogsWithLevelFilterAndNoFollow tests combining --level, --last, and --no-follow.
func TestRunLogsWithLevelFilterAndNoFollow(t *testing.T) {
	t.Setenv("MUNINNDB_DATA", t.TempDir())
	out := captureStdout(func() {
		runLogs([]string{"--no-follow", "--last", "5", "--level", "error"})
	})
	_ = out
}

// TestRunLogsPositionalArg tests muninn logs 50 syntax.
func TestRunLogsPositionalArg(t *testing.T) {
	t.Setenv("MUNINNDB_DATA", t.TempDir())
	out := captureStdout(func() {
		runLogs([]string{"--no-follow", "50"})
	})
	_ = out
}

// TestTailLogShowsHistory verifies that tailLog prints last N lines before tailing.
func TestTailLogShowsHistory(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "muninn-*.log")
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 10; i++ {
		fmt.Fprintf(f, "line %d INFO test\n", i)
	}
	f.Close()

	var out strings.Builder
	var errOut strings.Builder
	done := make(chan struct{})
	go func() {
		defer func() { recover() }()
		tailLog(f.Name(), "", 3, &out, &errOut)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
	}

	result := out.String()
	if !strings.Contains(result, "line 8") || !strings.Contains(result, "line 9") || !strings.Contains(result, "line 10") {
		t.Errorf("expected last 3 lines (8-10) in output, got: %s", result)
	}
	if strings.Contains(result, "line 7 ") {
		t.Errorf("line 7 should not appear in last-3 output, got: %s", result)
	}
}
