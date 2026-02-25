package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cockroachdb/pebble"
	"github.com/scrypster/muninndb/internal/storage"
)

func TestBackupCommand_OfflineCheckpoint(t *testing.T) {
	dataDir := t.TempDir()
	pebbleDir := filepath.Join(dataDir, "pebble")
	walDir := filepath.Join(dataDir, "wal")

	if err := os.MkdirAll(walDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(walDir, "segment-001"), []byte("wal-data"), 0600); err != nil {
		t.Fatal(err)
	}

	secretPath := filepath.Join(dataDir, "auth_secret")
	if err := os.WriteFile(secretPath, []byte("s3cret"), 0600); err != nil {
		t.Fatal(err)
	}

	db, err := storage.OpenPebble(pebbleDir, storage.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Set([]byte("test-key"), []byte("test-value"), pebble.Sync); err != nil {
		t.Fatal(err)
	}
	db.Close()

	outputDir := filepath.Join(t.TempDir(), "backup-out")

	out := captureStdout(func() {
		runBackup([]string{"--output", outputDir, "--data-dir", dataDir})
	})

	if !strings.Contains(out, "Backup complete") {
		t.Fatalf("expected success message, got: %q", out)
	}

	// Verify checkpoint is openable and contains the written key.
	checkpointDB, err := storage.OpenPebble(filepath.Join(outputDir, "pebble"), storage.DefaultOptions())
	if err != nil {
		t.Fatalf("failed to open checkpoint: %v", err)
	}
	defer checkpointDB.Close()

	val, closer, err := checkpointDB.Get([]byte("test-key"))
	if err != nil {
		t.Fatalf("key not found in checkpoint: %v", err)
	}
	if string(val) != "test-value" {
		t.Fatalf("expected test-value, got %q", string(val))
	}
	closer.Close()

	// Verify WAL was copied.
	walCopy := filepath.Join(outputDir, "wal", "segment-001")
	data, err := os.ReadFile(walCopy)
	if err != nil {
		t.Fatalf("wal file not copied: %v", err)
	}
	if string(data) != "wal-data" {
		t.Fatalf("wal data mismatch: got %q", string(data))
	}

	// Verify auth_secret was copied.
	secretCopy := filepath.Join(outputDir, "auth_secret")
	data, err = os.ReadFile(secretCopy)
	if err != nil {
		t.Fatalf("auth_secret not copied: %v", err)
	}
	if string(data) != "s3cret" {
		t.Fatalf("auth_secret mismatch: got %q", string(data))
	}
}

func TestVerifyCheckpoint_Valid(t *testing.T) {
	dir := t.TempDir()
	db, err := pebble.Open(dir, &pebble.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Set([]byte("k"), []byte("v"), pebble.Sync); err != nil {
		t.Fatal(err)
	}
	db.Close()

	if err := verifyCheckpoint(dir); err != nil {
		t.Fatalf("expected valid checkpoint, got error: %v", err)
	}
}

func TestVerifyCheckpoint_InvalidDir(t *testing.T) {
	err := verifyCheckpoint(filepath.Join(t.TempDir(), "nonexistent"))
	if err == nil {
		t.Fatal("expected error for nonexistent checkpoint dir")
	}
}

func TestBackupCommand_OutputDirRequired(t *testing.T) {
	exitCode := -1
	origExit := osExit
	osExit = func(code int) { exitCode = code }
	defer func() { osExit = origExit }()

	stderr := captureStderr(func() {
		runBackup([]string{})
	})

	if exitCode != 1 {
		t.Fatalf("expected exit code 1, got %d", exitCode)
	}
	if !strings.Contains(stderr, "--output is required") {
		t.Fatalf("expected --output is required error, got: %q", stderr)
	}
}

func TestBackupCommand_OutputDirExists(t *testing.T) {
	dataDir := t.TempDir()
	pebbleDir := filepath.Join(dataDir, "pebble")

	db, err := storage.OpenPebble(pebbleDir, storage.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	db.Close()

	outputDir := t.TempDir() // already exists

	exitCode := -1
	origExit := osExit
	osExit = func(code int) { exitCode = code }
	defer func() { osExit = origExit }()

	stderr := captureStderr(func() {
		runBackup([]string{"--output", outputDir, "--data-dir", dataDir})
	})

	if exitCode != 1 {
		t.Fatalf("expected exit code 1, got %d", exitCode)
	}
	if !strings.Contains(stderr, "already exists") {
		t.Fatalf("expected 'already exists' error, got: %q", stderr)
	}
}
