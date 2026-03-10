package backup

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync/atomic"
	"testing"
	"time"
)

// stubCheckpointer is a test double for Checkpointer.
// It writes a marker file inside destDir to confirm the call happened.
type stubCheckpointer struct {
	called    atomic.Int32
	markerErr error // if non-nil, Checkpoint returns this error
}

func (s *stubCheckpointer) Checkpoint(destDir string) error {
	s.called.Add(1)
	if s.markerErr != nil {
		return s.markerErr
	}
	if err := os.MkdirAll(destDir, 0700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(destDir, "checkpoint.marker"), []byte("ok"), 0600)
}

// TestNew_DisabledWhenNoInterval verifies that New returns nil when Interval is zero.
func TestNew_DisabledWhenNoInterval(t *testing.T) {
	stub := &stubCheckpointer{}
	sched := New(Config{Interval: 0, BackupDir: "/tmp/backups", Retain: 5}, stub)
	if sched != nil {
		t.Fatalf("expected nil scheduler when Interval is zero, got non-nil")
	}
}

// TestRunOnce_CreatesCheckpoint verifies that runOnce creates a backup directory
// with the expected timestamp-based name and calls Checkpoint inside it.
func TestRunOnce_CreatesCheckpoint(t *testing.T) {
	dir := t.TempDir()
	stub := &stubCheckpointer{}

	sched := New(Config{
		Interval:  time.Hour, // won't tick in test
		BackupDir: dir,
		Retain:    5,
		DataDir:   "", // no aux files
	}, stub)
	if sched == nil {
		t.Fatal("expected non-nil scheduler")
	}

	before := time.Now().UTC()
	sched.runOnce()
	after := time.Now().UTC()

	if stub.called.Load() != 1 {
		t.Fatalf("expected Checkpoint called once, got %d", stub.called.Load())
	}

	// Verify a backup directory was created.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read backup dir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 backup dir, got %d", len(entries))
	}

	name := entries[0].Name()

	// Name must begin with "backup-" and contain a valid timestamp.
	const prefix = "backup-"
	if len(name) <= len(prefix) {
		t.Fatalf("unexpected backup dir name: %q", name)
	}
	tsStr := name[len(prefix):]
	ts, err := time.Parse("20060102-150405", tsStr)
	if err != nil {
		t.Fatalf("backup dir timestamp parse error for %q: %v", tsStr, err)
	}

	// The parsed timestamp should fall within the run window (allow 1s slack).
	if ts.Before(before.Add(-time.Second)) || ts.After(after.Add(time.Second)) {
		t.Fatalf("timestamp %v not in expected range [%v, %v]", ts, before, after)
	}

	// Verify the pebble sub-directory and marker exist.
	markerPath := filepath.Join(dir, name, "pebble", "checkpoint.marker")
	if _, err := os.Stat(markerPath); err != nil {
		t.Fatalf("marker file missing at %s: %v", markerPath, err)
	}
}

// TestPruneOldBackups_KeepsRetainCount creates 7 backup directories, sets
// retain=3, runs runOnce (which adds an 8th), and verifies only 3 remain.
func TestPruneOldBackups_KeepsRetainCount(t *testing.T) {
	dir := t.TempDir()
	stub := &stubCheckpointer{}

	const retain = 3

	// Pre-create 7 backup dirs with ascending timestamps so pruning removes
	// the oldest ones first.
	for i := 0; i < 7; i++ {
		ts := time.Date(2024, 1, i+1, 12, 0, 0, 0, time.UTC)
		name := fmt.Sprintf("backup-%s", ts.Format("20060102-150405"))
		if err := os.MkdirAll(filepath.Join(dir, name), 0700); err != nil {
			t.Fatalf("mkdir %s: %v", name, err)
		}
	}

	sched := New(Config{
		Interval:  time.Hour,
		BackupDir: dir,
		Retain:    retain,
		DataDir:   "",
	}, stub)

	sched.runOnce()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read backup dir: %v", err)
	}

	var names []string
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	if len(names) != retain {
		t.Fatalf("expected %d backup dirs after prune, got %d: %v", retain, len(names), names)
	}
}

// TestGetStatus_ReflectsLastRun verifies that after runOnce completes the
// status fields are populated correctly.
func TestGetStatus_ReflectsLastRun(t *testing.T) {
	dir := t.TempDir()
	stub := &stubCheckpointer{}

	cfg := Config{
		Interval:  5 * time.Minute,
		BackupDir: dir,
		Retain:    5,
		DataDir:   "",
	}
	sched := New(cfg, stub)

	// Status before any run: enabled but no last-run info.
	before := sched.GetStatus()
	if !before.Enabled {
		t.Fatal("expected Enabled=true before first run")
	}
	if !before.LastRunAt.IsZero() {
		t.Fatalf("expected zero LastRunAt before first run, got %v", before.LastRunAt)
	}

	runStart := time.Now()
	sched.runOnce()
	runEnd := time.Now()

	st := sched.GetStatus()

	if !st.Enabled {
		t.Error("expected Enabled=true")
	}
	if st.Interval != cfg.Interval.String() {
		t.Errorf("expected Interval=%q, got %q", cfg.Interval.String(), st.Interval)
	}
	if st.BackupDir != cfg.BackupDir {
		t.Errorf("expected BackupDir=%q, got %q", cfg.BackupDir, st.BackupDir)
	}
	if st.Retain != cfg.Retain {
		t.Errorf("expected Retain=%d, got %d", cfg.Retain, st.Retain)
	}
	if !st.LastRunOK {
		t.Errorf("expected LastRunOK=true, got false (error: %s)", st.LastError)
	}
	if st.LastRunAt.Before(runStart) || st.LastRunAt.After(runEnd) {
		t.Errorf("LastRunAt %v outside run window [%v, %v]", st.LastRunAt, runStart, runEnd)
	}
	if st.LastElapsed == "" {
		t.Error("expected non-empty LastElapsed")
	}
	if st.LastError != "" {
		t.Errorf("expected empty LastError, got %q", st.LastError)
	}
}

// TestScheduler_Start verifies the scheduler fires through its goroutine via
// a very short interval and that the engine Checkpoint is called at least once.
func TestScheduler_Start(t *testing.T) {
	dir := t.TempDir()
	stub := &stubCheckpointer{}

	sched := New(Config{
		Interval:  50 * time.Millisecond,
		BackupDir: dir,
		Retain:    10,
		DataDir:   "",
	}, stub)

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	done := sched.Start(ctx)
	<-ctx.Done()
	<-done // wait for the goroutine to finish any in-progress runOnce before temp dir cleanup

	if stub.called.Load() == 0 {
		t.Fatal("expected Checkpoint to be called at least once by the scheduler goroutine")
	}
}

// TestRunOnce_CheckpointError verifies that when Checkpoint returns an error,
// the scheduler records LastRunOK=false and populates LastError.
func TestRunOnce_CheckpointError(t *testing.T) {
	dir := t.TempDir()
	stub := &stubCheckpointer{
		markerErr: fmt.Errorf("disk full"),
	}

	sched := New(Config{
		Interval:  time.Hour,
		BackupDir: dir,
		Retain:    5,
		DataDir:   "",
	}, stub)

	sched.runOnce()

	st := sched.GetStatus()
	if st.LastRunOK {
		t.Error("expected LastRunOK=false after checkpoint error")
	}
	if st.LastError == "" {
		t.Error("expected LastError to be set after checkpoint error")
	}
	if st.LastRunAt.IsZero() {
		t.Error("expected LastRunAt to be set after checkpoint error")
	}
}

// TestRunOnce_MkdirAllFailure verifies that when the backup directory cannot
// be created (e.g. BackupDir is an existing file, not a directory),
// the scheduler records an error without panicking.
func TestRunOnce_MkdirAllFailure(t *testing.T) {
	// Create a file where the backup dir should be — MkdirAll will fail.
	tmp := t.TempDir()
	blockingFile := filepath.Join(tmp, "blocker")
	if err := os.WriteFile(blockingFile, []byte("x"), 0600); err != nil {
		t.Fatalf("setup: %v", err)
	}

	stub := &stubCheckpointer{}
	sched := New(Config{
		Interval:  time.Hour,
		BackupDir: filepath.Join(blockingFile, "subdir"), // path through a file
		Retain:    5,
		DataDir:   "",
	}, stub)

	sched.runOnce()

	st := sched.GetStatus()
	if st.LastRunOK {
		t.Error("expected LastRunOK=false when MkdirAll fails")
	}
	if st.LastError == "" {
		t.Error("expected LastError to be non-empty when MkdirAll fails")
	}
	// Checkpoint must not have been attempted.
	if stub.called.Load() != 0 {
		t.Errorf("expected Checkpoint not called, got %d calls", stub.called.Load())
	}
}

// TestCopyFile_Roundtrip creates a source file, copies it, and verifies the
// destination has identical content and correct permissions.
func TestCopyFile_Roundtrip(t *testing.T) {
	src := filepath.Join(t.TempDir(), "source.txt")
	dst := filepath.Join(t.TempDir(), "dest.txt")

	content := []byte("hello muninndb backup")
	if err := os.WriteFile(src, content, 0640); err != nil {
		t.Fatalf("write src: %v", err)
	}

	if err := copyFile(src, dst); err != nil {
		t.Fatalf("copyFile: %v", err)
	}

	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read dst: %v", err)
	}
	if string(got) != string(content) {
		t.Errorf("content mismatch: got %q, want %q", got, content)
	}

	// Verify file mode is preserved.
	srcInfo, _ := os.Stat(src)
	dstInfo, _ := os.Stat(dst)
	if srcInfo.Mode() != dstInfo.Mode() {
		t.Errorf("mode mismatch: src %v, dst %v", srcInfo.Mode(), dstInfo.Mode())
	}
}

// TestCopyFile_SrcMissing verifies copyFile returns an error when the source
// does not exist.
func TestCopyFile_SrcMissing(t *testing.T) {
	src := filepath.Join(t.TempDir(), "nonexistent.txt")
	dst := filepath.Join(t.TempDir(), "dest.txt")

	if err := copyFile(src, dst); err == nil {
		t.Error("expected error when source file is missing, got nil")
	}
}

// TestCopyDir_Roundtrip creates a source directory tree with files at multiple
// levels, copies it, and verifies all files are present with correct content.
func TestCopyDir_Roundtrip(t *testing.T) {
	srcDir := t.TempDir()
	dstDir := filepath.Join(t.TempDir(), "copy")

	files := map[string]string{
		"a.txt":        "alpha",
		"sub/b.txt":    "beta",
		"sub/deep/c.txt": "gamma",
	}

	for relPath, content := range files {
		full := filepath.Join(srcDir, relPath)
		if err := os.MkdirAll(filepath.Dir(full), 0700); err != nil {
			t.Fatalf("mkdir %s: %v", filepath.Dir(full), err)
		}
		if err := os.WriteFile(full, []byte(content), 0600); err != nil {
			t.Fatalf("write %s: %v", full, err)
		}
	}

	if err := copyDir(srcDir, dstDir); err != nil {
		t.Fatalf("copyDir: %v", err)
	}

	for relPath, wantContent := range files {
		dstFile := filepath.Join(dstDir, relPath)
		got, err := os.ReadFile(dstFile)
		if err != nil {
			t.Errorf("missing file %s: %v", dstFile, err)
			continue
		}
		if string(got) != wantContent {
			t.Errorf("file %s: got %q, want %q", relPath, got, wantContent)
		}
	}
}

// TestDirSize verifies that dirSize correctly totals the byte sizes of all
// files in a directory tree.
func TestDirSize(t *testing.T) {
	dir := t.TempDir()

	files := map[string][]byte{
		"file1.txt":     []byte("hello"),        // 5 bytes
		"sub/file2.txt": []byte("world!!"),      // 7 bytes
	}
	var expectedTotal int64
	for relPath, content := range files {
		full := filepath.Join(dir, relPath)
		if err := os.MkdirAll(filepath.Dir(full), 0700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(full, content, 0600); err != nil {
			t.Fatalf("write %s: %v", full, err)
		}
		expectedTotal += int64(len(content))
	}

	got := dirSize(dir)
	if got != expectedTotal {
		t.Errorf("dirSize: got %d, want %d", got, expectedTotal)
	}
}

// TestDirSize_EmptyDir verifies that dirSize returns 0 for an empty directory.
func TestDirSize_EmptyDir(t *testing.T) {
	dir := t.TempDir()
	if got := dirSize(dir); got != 0 {
		t.Errorf("expected 0 for empty dir, got %d", got)
	}
}

// TestPruneOldBackups_EmptyDir verifies that pruneOldBackups returns 0 when
// the backup directory contains no subdirectories.
func TestPruneOldBackups_EmptyDir(t *testing.T) {
	dir := t.TempDir()
	sched := New(Config{
		Interval:  time.Hour,
		BackupDir: dir,
		Retain:    3,
	}, &stubCheckpointer{})

	pruned := sched.pruneOldBackups()
	if pruned != 0 {
		t.Errorf("expected 0 pruned for empty dir, got %d", pruned)
	}
}

// TestPruneOldBackups_UnderRetain verifies that when the number of backup
// directories is less than or equal to cfg.Retain, nothing is deleted.
func TestPruneOldBackups_UnderRetain(t *testing.T) {
	dir := t.TempDir()
	const retain = 5

	// Create 3 backup dirs — fewer than retain.
	for i := 0; i < 3; i++ {
		ts := time.Date(2025, 1, i+1, 10, 0, 0, 0, time.UTC)
		name := fmt.Sprintf("backup-%s", ts.Format("20060102-150405"))
		if err := os.MkdirAll(filepath.Join(dir, name), 0700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}

	sched := New(Config{
		Interval:  time.Hour,
		BackupDir: dir,
		Retain:    retain,
	}, &stubCheckpointer{})

	pruned := sched.pruneOldBackups()
	if pruned != 0 {
		t.Errorf("expected 0 pruned when under retain threshold, got %d", pruned)
	}

	// All 3 directories must still exist.
	entries, _ := os.ReadDir(dir)
	var dirs []string
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, e.Name())
		}
	}
	if len(dirs) != 3 {
		t.Errorf("expected 3 dirs to remain, got %d: %v", len(dirs), dirs)
	}
}

// TestPruneOldBackups_ZeroRetain verifies that pruneOldBackups skips pruning
// entirely when Retain is 0 (disabled), leaving all directories intact.
func TestPruneOldBackups_ZeroRetain(t *testing.T) {
	dir := t.TempDir()

	for i := 0; i < 4; i++ {
		ts := time.Date(2025, 3, i+1, 8, 0, 0, 0, time.UTC)
		name := fmt.Sprintf("backup-%s", ts.Format("20060102-150405"))
		if err := os.MkdirAll(filepath.Join(dir, name), 0700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}

	sched := New(Config{
		Interval:  time.Hour,
		BackupDir: dir,
		Retain:    0,
	}, &stubCheckpointer{})

	pruned := sched.pruneOldBackups()
	if pruned != 0 {
		t.Errorf("expected 0 pruned when Retain=0, got %d", pruned)
	}
}

// TestRunOnce_CopiesAuxFiles verifies that when DataDir contains a wal/
// subdirectory and an auth_secret file, runOnce copies both into the backup.
func TestRunOnce_CopiesAuxFiles(t *testing.T) {
	dataDir := t.TempDir()
	backupDir := t.TempDir()

	// Create wal/ directory with a log file.
	walDir := filepath.Join(dataDir, "wal")
	if err := os.MkdirAll(walDir, 0700); err != nil {
		t.Fatalf("mkdir wal: %v", err)
	}
	if err := os.WriteFile(filepath.Join(walDir, "000001.log"), []byte("wal data"), 0600); err != nil {
		t.Fatalf("write wal file: %v", err)
	}

	// Create auth_secret file.
	secretContent := []byte("supersecret")
	if err := os.WriteFile(filepath.Join(dataDir, "auth_secret"), secretContent, 0600); err != nil {
		t.Fatalf("write auth_secret: %v", err)
	}

	stub := &stubCheckpointer{}
	sched := New(Config{
		Interval:  time.Hour,
		BackupDir: backupDir,
		Retain:    5,
		DataDir:   dataDir,
	}, stub)

	sched.runOnce()

	// Locate the backup directory that was created.
	entries, err := os.ReadDir(backupDir)
	if err != nil {
		t.Fatalf("read backup dir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 backup dir, got %d", len(entries))
	}
	backupName := entries[0].Name()
	backupPath := filepath.Join(backupDir, backupName)

	// Verify wal/ was copied with its contents.
	copiedWalFile := filepath.Join(backupPath, "wal", "000001.log")
	gotWal, err := os.ReadFile(copiedWalFile)
	if err != nil {
		t.Errorf("wal file not copied: %v", err)
	} else if string(gotWal) != "wal data" {
		t.Errorf("wal file content mismatch: got %q", gotWal)
	}

	// Verify auth_secret was copied with the correct content.
	copiedSecret := filepath.Join(backupPath, "auth_secret")
	gotSecret, err := os.ReadFile(copiedSecret)
	if err != nil {
		t.Errorf("auth_secret not copied: %v", err)
	} else if string(gotSecret) != string(secretContent) {
		t.Errorf("auth_secret content mismatch: got %q, want %q", gotSecret, secretContent)
	}

	// Verify the overall run was marked as successful.
	st := sched.GetStatus()
	if !st.LastRunOK {
		t.Errorf("expected LastRunOK=true, got false: %s", st.LastError)
	}
}
