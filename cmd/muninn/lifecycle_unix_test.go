//go:build !windows

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRunStop_StalePIDFileRemovalFails verifies that stop does not report the
// stale PID file as removed when the removal actually failed. A PID file that
// survives the "removed" message is worse than one that was never claimed to
// be gone: the next start still sees it and the operator has been told
// otherwise.
//
// Unix-only because the mechanism is: unlink is governed by write permission
// on the *directory*, not on the file, so making the directory read-only makes
// os.Remove fail while leaving everything else intact. Windows has no
// equivalent — os.Chmod there only toggles the read-only attribute, which does
// not stop a file inside the directory from being deleted, and os.Geteuid
// returns -1 so the root skip below would never fire.
func TestRunStop_StalePIDFileRemovalFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: directory write permissions do not apply")
	}
	dir := t.TempDir()
	t.Setenv("MUNINNDB_DATA", dir)

	pidPath := filepath.Join(dir, "muninn.pid")
	if err := writePID(pidPath, 99999999); err != nil { // dead PID
		t.Fatalf("writePID: %v", err)
	}
	if err := writeAddrsFile(dir, daemonAddrs{RestAddr: "127.0.0.1:8475"}); err != nil {
		t.Fatalf("writeAddrsFile: %v", err)
	}

	// Restore before t.TempDir's own cleanup runs (cleanups run LIFO).
	t.Cleanup(func() { os.Chmod(dir, 0o700) })
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	exitCode := stubExit(t)
	out := captureStdout(func() { runStop() })

	if strings.Contains(out, "removed stale PID file") {
		t.Errorf("stop claimed to have removed the PID file although removal failed; stdout = %q", out)
	}
	if *exitCode <= 0 {
		t.Errorf("runStop exit code = %d when sidecar removal failed; want non-zero", *exitCode)
	}
	if _, err := os.Stat(pidPath); err != nil {
		t.Errorf("PID file unexpectedly gone: stat err = %v", err)
	}
}
