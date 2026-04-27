//go:build !windows

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"
)

// stopProcess sends SIGTERM for graceful shutdown on Unix.
func stopProcess(proc *os.Process) error {
	return proc.Signal(syscall.SIGTERM)
}

// isProcessRunning checks whether a process with the given PID is still alive
// by sending signal 0 (the Unix existence check).
func isProcessRunning(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return proc.Signal(syscall.Signal(0)) == nil
}

// daemonSysProcAttr detaches the daemon into its own session so it is not
// terminated when the parent CLI process exits or loses its controlling TTY.
func daemonSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}

// daemonExtraSetup applies platform-specific settings to the daemon command.
func daemonExtraSetup(cmd *exec.Cmd) {}

// isPebbleLockHeld returns true if the Pebble LOCK file inside dataDir is
// currently held by another process.
//
// Pebble uses POSIX fcntl record locks (F_SETLK / F_WRLCK), not BSD flock.
// On Linux these two locking mechanisms are independent — a flock check would
// always succeed even while Pebble holds the database. We therefore probe with
// the same F_SETLK / F_WRLCK approach that Pebble uses.
//
// Returns false on any error (file absent, no pebble/ dir yet, permission
// denied) so that callers proceed and let Pebble itself produce a clear error.
// There is an inherent TOCTOU race between this check and spawning the child
// process; that is acceptable — this is a best-effort guard, and Pebble's own
// lock will cause the losing process to fail fast.
func isPebbleLockHeld(dataDir string) bool {
	lockPath := filepath.Join(dataDir, "pebble", "LOCK")
	f, err := os.OpenFile(lockPath, os.O_RDWR, 0600)
	if err != nil {
		// File does not exist or is otherwise inaccessible — not held.
		return false
	}
	defer f.Close()

	spec := unix.Flock_t{
		Type:   unix.F_WRLCK,
		Whence: 0, // SEEK_SET
		Start:  0,
		Len:    0, // whole file
	}
	// F_SETLK is non-blocking: returns EACCES or EAGAIN if already locked.
	err = unix.FcntlFlock(f.Fd(), unix.F_SETLK, &spec)
	if err != nil {
		// Could not acquire — another process holds the lock.
		return true
	}
	// We acquired it; release immediately and report not held.
	spec.Type = unix.F_UNLCK
	_ = unix.FcntlFlock(f.Fd(), unix.F_SETLK, &spec)
	return false
}
