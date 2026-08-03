package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
)

// TestReopenableFile_ReopenAfterRename is the exact scenario #850 was filed
// against: a rotator renames the file out from under the daemon, then
// signals it to reopen. Without a working Reopen, writes after the rename
// keep landing in the renamed (now-orphaned) inode and the file at the
// original path never receives anything.
//
// This is a POSIX rotation idiom: an open descriptor stays attached to its
// inode across a rename, so the daemon can keep writing to the (now
// unlinked-by-path) old file until Reopen swaps it for a fresh descriptor at
// the original path. Windows takes a mandatory lock on an open file, so
// os.Rename on a path the daemon still has open fails outright — there is no
// equivalent to keep working. That is not a gap this test papers over: the
// feature's *trigger* is SIGHUP, and SIGHUP has no Windows delivery mechanism
// either (see the signal.Notify comment in server.go), so rename-then-signal
// rotation is inherently POSIX-only, not merely untested on Windows. See
// docs/self-hosting.md for the operator-facing statement of that limitation.
func TestReopenableFile_ReopenAfterRename(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("rename-while-open rotation is a POSIX idiom: Windows takes a mandatory lock on open files, so this rename would fail outright, and SIGHUP — the trigger this mechanism relies on — cannot be delivered on Windows at all. See docs/self-hosting.md for the documented platform limitation.")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "muninn.log")

	rf, err := newReopenableFile(path)
	if err != nil {
		t.Fatalf("newReopenableFile: %v", err)
	}
	defer rf.Close()

	if _, err := rf.Write([]byte("before rotation\n")); err != nil {
		t.Fatalf("write before rotation: %v", err)
	}

	rotated := path + ".1"
	if err := os.Rename(path, rotated); err != nil {
		t.Fatalf("rename: %v", err)
	}

	if err := rf.Reopen(); err != nil {
		t.Fatalf("Reopen: %v", err)
	}

	if _, err := rf.Write([]byte("after rotation\n")); err != nil {
		t.Fatalf("write after rotation: %v", err)
	}

	rotatedContent, err := os.ReadFile(rotated)
	if err != nil {
		t.Fatalf("read rotated file: %v", err)
	}
	if !strings.Contains(string(rotatedContent), "before rotation") {
		t.Errorf("rotated file missing pre-rotation content: %q", rotatedContent)
	}
	if strings.Contains(string(rotatedContent), "after rotation") {
		t.Errorf("rotated file must NOT receive the post-reopen write — it did: %q", rotatedContent)
	}

	freshContent, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fresh file at original path: %v", err)
	}
	if !strings.Contains(string(freshContent), "after rotation") {
		t.Errorf("fresh file at the original path must receive the post-reopen write; got %q", freshContent)
	}
	if strings.Contains(string(freshContent), "before rotation") {
		t.Errorf("fresh file must NOT contain the pre-rotation write; got %q", freshContent)
	}
}

// spyReopener counts Reopen calls without touching the filesystem, so
// handleServerSignal's routing decision can be checked in isolation from
// reopenableFile's own mechanism (covered above).
type spyReopener struct {
	calls int
	err   error
}

func (s *spyReopener) Reopen() error {
	s.calls++
	return s.err
}

func TestHandleServerSignal_SIGHUP_ReopensWhenConfigured(t *testing.T) {
	spy := &spyReopener{}
	shutdown := false
	exited := -1
	handleServerSignal(syscall.SIGHUP, spy, func() {}, &shutdown, func(code int) { exited = code })

	if spy.calls != 1 {
		t.Errorf("expected Reopen to be called once, got %d", spy.calls)
	}
	if shutdown {
		t.Error("SIGHUP must not set shutdownRequested")
	}
	if exited != -1 {
		t.Errorf("SIGHUP must not exit the process, got exit code %d", exited)
	}
}

func TestHandleServerSignal_SIGHUP_NoOpWhenNoReopener(t *testing.T) {
	shutdown := false
	exited := -1
	// reopener is nil: no --log-file/MUNINN_LOG_FILE configured.
	handleServerSignal(syscall.SIGHUP, nil, func() {}, &shutdown, func(code int) { exited = code })

	if shutdown {
		t.Error("SIGHUP must not set shutdownRequested even with no reopener")
	}
	if exited != -1 {
		t.Errorf("SIGHUP must not exit the process, got exit code %d", exited)
	}
}

func TestHandleServerSignal_SIGTERM_GracefulThenForceExit(t *testing.T) {
	shutdown := false
	cancelCalls := 0
	exited := -1
	cancel := func() { cancelCalls++ }
	exitFn := func(code int) { exited = code }

	handleServerSignal(syscall.SIGTERM, nil, cancel, &shutdown, exitFn)
	if !shutdown {
		t.Fatal("first SIGTERM must set shutdownRequested")
	}
	if cancelCalls != 1 {
		t.Fatalf("first SIGTERM must call cancel exactly once, got %d", cancelCalls)
	}
	if exited != -1 {
		t.Fatalf("first SIGTERM must not exit, got %d", exited)
	}

	handleServerSignal(syscall.SIGTERM, nil, cancel, &shutdown, exitFn)
	if exited != 1 {
		t.Fatalf("second SIGTERM must force exit(1), got %d", exited)
	}
	if cancelCalls != 1 {
		t.Fatalf("second SIGTERM must not call cancel again, got %d calls", cancelCalls)
	}
}
