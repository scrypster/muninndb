package main

import (
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/cockroachdb/pebble"
	"github.com/scrypster/muninndb/internal/storage"
)

// osExit is os.Exit by default; tests override it to avoid killing the test process.
var osExit = os.Exit

// runBackup performs an offline point-in-time backup of the MuninnDB data directory.
// It uses Pebble's Checkpoint to create a consistent, hardlinked snapshot of the
// database, then copies auxiliary files (WAL segments, auth_secret).
//
// If the server is running (PID file present and process alive), it errors out
// and directs the user to the online backup REST endpoint instead.
func runBackup(args []string) {
	var dataDir, outputDir string

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--data-dir":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "error: --data-dir requires an argument")
				osExit(1)
				return
			}
			i++
			dataDir = args[i]
		case "--output":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "error: --output requires an argument")
				osExit(1)
				return
			}
			i++
			outputDir = args[i]
		default:
			fmt.Fprintf(os.Stderr, "error: unknown flag %q\n", args[i])
			fmt.Fprintln(os.Stderr, "Usage: muninn backup --output <dir> [--data-dir <dir>]")
			osExit(1)
			return
		}
	}

	if outputDir == "" {
		fmt.Fprintln(os.Stderr, "error: --output is required")
		fmt.Fprintln(os.Stderr, "Usage: muninn backup --output <dir> [--data-dir <dir>]")
		osExit(1)
		return
	}

	if dataDir == "" {
		dataDir = defaultDataDir()
	}

	if _, err := os.Stat(dataDir); os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "error: data directory %q does not exist\n", dataDir)
		osExit(1)
		return
	}

	// Refuse to overwrite an existing output directory.
	if _, err := os.Stat(outputDir); err == nil {
		fmt.Fprintf(os.Stderr, "error: output directory %q already exists (refusing to overwrite)\n", outputDir)
		osExit(1)
		return
	}

	// Check if the server is running — Pebble uses a lock file, so we can't
	// open the database while another process has it.
	pidPath := filepath.Join(dataDir, "muninn.pid")
	if pid, err := readPID(pidPath); err == nil && isProcessRunning(pid) {
		fmt.Fprintf(os.Stderr, "error: muninn is running (pid %d) — cannot perform offline backup\n", pid)
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "Use the online backup endpoint instead:")
		fmt.Fprintf(os.Stderr, "  curl -X POST http://127.0.0.1:8475/api/admin/backup -d '{\"output_dir\": \"%s\"}'\n", outputDir)
		osExit(1)
		return
	}

	start := time.Now()
	slog.Info("starting backup", "data_dir", dataDir, "output", outputDir)

	if err := os.MkdirAll(outputDir, 0700); err != nil {
		fmt.Fprintf(os.Stderr, "error: failed to create output directory: %v\n", err)
		osExit(1)
		return
	}

	// 1. Checkpoint the Pebble database.
	pebbleDir := filepath.Join(dataDir, "pebble")
	checkpointDir := filepath.Join(outputDir, "pebble")

	db, err := storage.OpenPebble(pebbleDir, storage.DefaultOptions())
	if err != nil {
		os.RemoveAll(outputDir)
		fmt.Fprintf(os.Stderr, "error: failed to open pebble database: %v\n", err)
		osExit(1)
		return
	}

	if err := db.Checkpoint(checkpointDir); err != nil {
		db.Close()
		os.RemoveAll(outputDir)
		fmt.Fprintf(os.Stderr, "error: pebble checkpoint failed: %v\n", err)
		osExit(1)
		return
	}
	db.Close()
	slog.Info("pebble checkpoint complete", "dir", checkpointDir)

	// 1b. Verify the checkpoint is actually openable before proceeding.
	if err := verifyCheckpoint(checkpointDir); err != nil {
		os.RemoveAll(outputDir)
		fmt.Fprintf(os.Stderr, "error: backup verification failed: %v\n", err)
		osExit(1)
		return
	}
	slog.Info("backup verified: checkpoint is readable", "dir", checkpointDir)

	// 2. Copy WAL (MOL) directory.
	walSrc := filepath.Join(dataDir, "wal")
	walDst := filepath.Join(outputDir, "wal")
	if info, err := os.Stat(walSrc); err == nil && info.IsDir() {
		if err := copyDir(walSrc, walDst); err != nil {
			fmt.Fprintf(os.Stderr, "warning: failed to copy wal directory: %v\n", err)
		} else {
			slog.Info("wal directory copied", "src", walSrc, "dst", walDst)
		}
	}

	// 3. Copy auth_secret if it exists.
	secretSrc := filepath.Join(dataDir, "auth_secret")
	secretDst := filepath.Join(outputDir, "auth_secret")
	if _, err := os.Stat(secretSrc); err == nil {
		if err := copyFile(secretSrc, secretDst); err != nil {
			fmt.Fprintf(os.Stderr, "warning: failed to copy auth_secret: %v\n", err)
		} else {
			slog.Info("auth_secret copied")
		}
	}

	elapsed := time.Since(start)
	size := dirSizeBytes(outputDir)
	fmt.Printf("Backup complete: %s (%s, %s)\n", outputDir, formatBytes(size), elapsed.Round(time.Millisecond))
}

// copyFile copies a single file, preserving permissions.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	info, err := in.Stat()
	if err != nil {
		return err
	}

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode())
	if err != nil {
		return err
	}
	defer out.Close()

	_, err = io.Copy(out, in)
	return err
}

// copyDir recursively copies a directory tree.
func copyDir(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)

		if d.IsDir() {
			return os.MkdirAll(target, 0700)
		}
		return copyFile(path, target)
	})
}

// dirSizeBytes walks a directory tree and sums file sizes.
func dirSizeBytes(dir string) int64 {
	var total int64
	filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		total += info.Size()
		return nil
	})
	return total
}

// verifyCheckpoint opens a Pebble checkpoint in read-only mode and scans a
// few keys to confirm the data is not corrupted. Returns nil on success.
func verifyCheckpoint(checkpointDir string) error {
	verifyDB, err := pebble.Open(checkpointDir, &pebble.Options{ReadOnly: true})
	if err != nil {
		return fmt.Errorf("open checkpoint: %w", err)
	}
	defer verifyDB.Close()

	iter, err := verifyDB.NewIter(nil)
	if err != nil {
		return fmt.Errorf("create iterator: %w", err)
	}
	defer iter.Close()

	const maxScan = 10
	n := 0
	for iter.First(); iter.Valid() && n < maxScan; iter.Next() {
		n++
	}
	if err := iter.Error(); err != nil {
		return fmt.Errorf("scan keys: %w", err)
	}
	return nil
}

func formatBytes(b int64) string {
	const (
		KB = 1024
		MB = KB * 1024
		GB = MB * 1024
	)
	switch {
	case b >= GB:
		return fmt.Sprintf("%.1f GB", float64(b)/float64(GB))
	case b >= MB:
		return fmt.Sprintf("%.1f MB", float64(b)/float64(MB))
	case b >= KB:
		return fmt.Sprintf("%.1f KB", float64(b)/float64(KB))
	default:
		return fmt.Sprintf("%d B", b)
	}
}
