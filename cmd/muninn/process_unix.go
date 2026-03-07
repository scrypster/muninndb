//go:build !windows

package main

import (
	"os"
	"os/exec"
	"syscall"
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
