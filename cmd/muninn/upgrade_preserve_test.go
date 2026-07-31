package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestBackupBinaryPath_NamesByVersion(t *testing.T) {
	got := backupBinaryPath("/usr/local/bin/muninn", "v0.9.0")
	want := "/usr/local/bin/muninn.v0.9.0.bak"
	if got != want {
		t.Errorf("backupBinaryPath = %q, want %q", got, want)
	}
}

func TestBackupBinaryPath_SanitizesVersion(t *testing.T) {
	// The version is stamped in by -ldflags at build time, so it is only as
	// trustworthy as whoever built the binary. It must not be able to steer the
	// backup out of the executable's own directory.
	//
	// The exe path is built with filepath rather than written as a literal so the
	// assertions hold on Windows too, where filepath.Dir normalizes to backslashes
	// and would not equal a hardcoded "/usr/local/bin".
	exe := filepath.Join(string(filepath.Separator), "usr", "local", "bin", "muninn")
	got := backupBinaryPath(exe, "../../etc/cron.d/x")

	if filepath.Dir(got) != filepath.Dir(exe) {
		t.Errorf("backup escaped the binary's directory: %q (want it in %q)",
			got, filepath.Dir(exe))
	}
	if base := filepath.Base(got); strings.Contains(base, "..") {
		t.Errorf("a traversal token survived sanitization: %q", base)
	}
}

func TestBackupBinaryPath_EmptyVersion(t *testing.T) {
	got := backupBinaryPath("/usr/local/bin/muninn", "")
	if got != "/usr/local/bin/muninn.previous.bak" {
		t.Errorf("backupBinaryPath with empty version = %q", got)
	}
}

// The invariant that decides link-vs-move: after the outgoing binary is preserved,
// it must still be at its own path. A move would leave a window in which the machine
// has no muninn at all — and no binary whatsoever if the install then failed.
func TestPreserveBinary_LeavesOriginalInPlace(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "muninn")
	if err := os.WriteFile(exe, []byte("OLD BINARY"), 0755); err != nil {
		t.Fatal(err)
	}
	bak := backupBinaryPath(exe, "v0.9.0")

	if err := preserveBinary(exe, bak); err != nil {
		t.Fatalf("preserveBinary: %v", err)
	}

	orig, err := os.ReadFile(exe)
	if err != nil {
		t.Fatalf("original binary is gone after preserve: %v", err)
	}
	if string(orig) != "OLD BINARY" {
		t.Errorf("original content changed: %q", orig)
	}
	saved, err := os.ReadFile(bak)
	if err != nil {
		t.Fatalf("rollback copy missing: %v", err)
	}
	if string(saved) != "OLD BINARY" {
		t.Errorf("rollback copy content = %q, want the outgoing binary", saved)
	}
}

// The reason the rollback copy exists: it must still hold the old bytes after the
// new binary has replaced the original, which is what makes it a rollback path.
func TestPreserveBinary_SurvivesTheInstallRename(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "muninn")
	if err := os.WriteFile(exe, []byte("OLD BINARY"), 0755); err != nil {
		t.Fatal(err)
	}
	bak := backupBinaryPath(exe, "v0.9.0")
	if err := preserveBinary(exe, bak); err != nil {
		t.Fatalf("preserveBinary: %v", err)
	}

	// Mirror selfUpdate's install step: rename the downloaded binary over the old one.
	incoming := filepath.Join(dir, ".muninn-upgrade-tmp")
	if err := os.WriteFile(incoming, []byte("NEW BINARY"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(incoming, exe); err != nil {
		t.Fatal(err)
	}

	saved, err := os.ReadFile(bak)
	if err != nil {
		t.Fatalf("rollback copy missing after install: %v", err)
	}
	if string(saved) != "OLD BINARY" {
		t.Errorf("rollback copy = %q, want the pre-upgrade binary", saved)
	}
	installed, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	if string(installed) != "NEW BINARY" {
		t.Errorf("installed binary = %q, want the new one", installed)
	}
}

// A retried upgrade must not fail because the previous attempt already left a .bak.
// os.Link refuses an existing destination, so this covers the clear-first step.
func TestPreserveBinary_ReplacesStaleBackup(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "muninn")
	if err := os.WriteFile(exe, []byte("OLD BINARY"), 0755); err != nil {
		t.Fatal(err)
	}
	bak := backupBinaryPath(exe, "v0.9.0")
	if err := os.WriteFile(bak, []byte("STALE"), 0755); err != nil {
		t.Fatal(err)
	}

	if err := preserveBinary(exe, bak); err != nil {
		t.Fatalf("preserveBinary over an existing backup: %v", err)
	}
	saved, err := os.ReadFile(bak)
	if err != nil {
		t.Fatal(err)
	}
	if string(saved) != "OLD BINARY" {
		t.Errorf("stale backup was not replaced: %q", saved)
	}
}

func TestCopyExecutable_ProducesAnExecutableCopy(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "muninn")
	dst := filepath.Join(dir, "muninn.bak")
	if err := os.WriteFile(src, []byte("OLD BINARY"), 0755); err != nil {
		t.Fatal(err)
	}

	if err := copyExecutable(src, dst); err != nil {
		t.Fatalf("copyExecutable: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "OLD BINARY" {
		t.Errorf("copy content = %q", got)
	}
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(dst)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode()&0111 == 0 {
			// A non-executable rollback copy cannot be rolled back to.
			t.Errorf("copy is not executable: mode %v", fi.Mode())
		}
	}
}

func TestPreserveBinary_ReportsAnUnreadableSource(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "does-not-exist")
	bak := backupBinaryPath(exe, "v0.9.0")

	if err := preserveBinary(exe, bak); err == nil {
		t.Error("preserveBinary succeeded with no source binary; the upgrade would " +
			"proceed believing it had a rollback path")
	}
}
