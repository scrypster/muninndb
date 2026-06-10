//go:build integration

package main

import (
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
	"time"

	muninn "github.com/scrypster/muninndb"
)

// execOp runs `muninn exec --data-dir <dir> <args...>` and returns stdout,
// stderr, and the exit code. muninnBin is set by TestMain in integration_test.go.
func execOp(t *testing.T, dataDir string, args ...string) (stdout, stderr string, exitCode int) {
	t.Helper()
	allArgs := append([]string{"exec", "--data-dir", dataDir}, args...)
	cmd := exec.Command(muninnBin, allArgs...)
	var outBuf, errBuf strings.Builder
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	err := cmd.Run()
	exitCode = 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		}
	}
	return outBuf.String(), errBuf.String(), exitCode
}

func TestExec_Remember_Recall_Read_Forget(t *testing.T) {
	dir := t.TempDir()

	// remember
	stdout, stderr, code := execOp(t, dir,
		"remember", "--concept", "test concept", "--content", "test content")
	if code != execExitSuccess {
		t.Fatalf("remember exit %d, stderr: %s", code, stderr)
	}
	var remResult map[string]string
	if err := json.Unmarshal([]byte(stdout), &remResult); err != nil {
		t.Fatalf("remember: not valid JSON: %v\noutput: %s", err, stdout)
	}
	id := remResult["id"]
	if id == "" {
		t.Fatalf("remember: no id in response: %s", stdout)
	}

	// read by ID
	stdout, stderr, code = execOp(t, dir, "read", "--id", id)
	if code != execExitSuccess {
		t.Fatalf("read exit %d, stderr: %s", code, stderr)
	}
	var readResult map[string]any
	if err := json.Unmarshal([]byte(stdout), &readResult); err != nil {
		t.Fatalf("read: not valid JSON: %v\noutput: %s", err, stdout)
	}
	if readResult["content"] != "test content" {
		t.Errorf("read: got content %q, want %q", readResult["content"], "test content")
	}

	// recall — FTS is async, poll until visible
	deadline := time.Now().Add(10 * time.Second)
	for {
		stdout, stderr, code = execOp(t, dir, "recall", "--query", "test content", "--limit", "5")
		if code != execExitSuccess {
			t.Fatalf("recall exit %d, stderr: %s", code, stderr)
		}
		var results []any
		if err := json.Unmarshal([]byte(stdout), &results); err != nil {
			t.Fatalf("recall: not valid JSON: %v\noutput: %s", err, stdout)
		}
		if len(results) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("recall: no results after 10s")
		}
		time.Sleep(100 * time.Millisecond)
	}

	// forget
	stdout, stderr, code = execOp(t, dir, "forget", "--id", id)
	if code != execExitSuccess {
		t.Fatalf("forget exit %d, stderr: %s", code, stderr)
	}
	var forgetResult map[string]any
	if err := json.Unmarshal([]byte(stdout), &forgetResult); err != nil {
		t.Fatalf("forget: not valid JSON: %v\noutput: %s", err, stdout)
	}
	if forgetResult["ok"] != true {
		t.Errorf("forget: expected ok=true, got: %v", forgetResult)
	}

	// read after forget → exit 3 (not found)
	_, _, code = execOp(t, dir, "read", "--id", id)
	if code != execExitNotFound {
		t.Errorf("read after forget: want exit %d, got %d", execExitNotFound, code)
	}
}

func TestExec_Read_NotFound(t *testing.T) {
	dir := t.TempDir()
	_, stderr, code := execOp(t, dir, "read", "--id", "01ARZ3NDEKTSV4RRFFQ69G5FAV")
	if code != execExitNotFound {
		t.Errorf("want exit %d, got %d (stderr: %s)", execExitNotFound, code, stderr)
	}
}

func TestExec_MissingRequiredFlags(t *testing.T) {
	dir := t.TempDir()

	cases := [][]string{
		{"remember", "--concept", "only concept"}, // missing --content
		{"remember", "--content", "only content"}, // missing --concept
		{"recall"}, // missing --query
		{"read"},   // missing --id
		{"forget"}, // missing --id
	}
	for _, args := range cases {
		_, _, code := execOp(t, dir, args...)
		if code != execExitUsage {
			t.Errorf("args %v: want exit %d (usage), got %d", args, execExitUsage, code)
		}
	}
}

func TestExec_UnknownOperation(t *testing.T) {
	dir := t.TempDir()
	_, stderr, code := execOp(t, dir, "doesnotexist")
	if code != execExitUsage {
		t.Errorf("want exit %d, got %d (stderr: %s)", execExitUsage, code, stderr)
	}
}

func TestExec_AlreadyLocked(t *testing.T) {
	dir := t.TempDir()

	// Hold the Pebble lock in THIS process via muninn.Open so the subprocess
	// is guaranteed to find the lock held for its entire duration.
	db, err := muninn.Open(dir)
	if err != nil {
		t.Fatalf("open holder: %v", err)
	}
	defer db.Close()

	// Subprocess tries to open the same data dir — must fail with exit 2.
	_, stderr, code := execOp(t, dir,
		"remember", "--concept", "test", "--content", "should fail")
	if code != execExitError {
		t.Errorf("locked: want exit %d, got %d (stderr: %s)", execExitError, code, stderr)
	}
	// Confirm the failure is lock contention, not some unrelated runtime error.
	if !strings.Contains(stderr, "lock") && !strings.Contains(stderr, "LOCK") &&
		!strings.Contains(stderr, "held by another process") &&
		!strings.Contains(stderr, "already in use") &&
		!strings.Contains(stderr, "resource temporarily unavailable") {
		t.Errorf("stderr should mention lock contention, got: %s", stderr)
	}
}
