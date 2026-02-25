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

// daemonSysProcAttr returns SysProcAttr for the daemon child process.
// On Unix, no special attributes are needed — the child survives the parent.
func daemonSysProcAttr() *syscall.SysProcAttr {
	return nil
}

// daemonExtraSetup applies platform-specific settings to the daemon command.
func daemonExtraSetup(cmd *exec.Cmd) {}
