//go:build windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
)

// Windows process creation flags for daemon detachment.
const (
	createNewProcessGroup = 0x00000200
	detachedProcess       = 0x00000008
)

// stopProcess terminates the daemon on Windows.
// Windows does not support Unix signals. We use Kill (TerminateProcess)
// because os.Interrupt only works if the process shares our console,
// which detached daemons do not.
// MuninnDB's storage layer (Pebble) uses atomic writes, so a hard kill is safe.
func stopProcess(proc *os.Process) error {
	return proc.Kill()
}

// isProcessRunning checks whether a process with the given PID exists on Windows.
// The Unix signal-0 trick is not available, so we query the system task list.
func isProcessRunning(pid int) bool {
	if pid <= 0 {
		return false
	}
	out, err := exec.Command(
		"tasklist", "/FI", fmt.Sprintf("PID eq %d", pid), "/NH", "/FO", "CSV",
	).Output()
	if err != nil {
		return false
	}
	return strings.Contains(string(out), fmt.Sprintf("\"%d\"", pid))
}

// daemonSysProcAttr returns SysProcAttr that detaches the daemon from the
// parent's console so it survives terminal close.
func daemonSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{
		CreationFlags: createNewProcessGroup | detachedProcess,
	}
}

// daemonExtraSetup applies Windows-specific settings to the daemon command.
// On Windows, HideWindow prevents a console flash when spawning the daemon.
func daemonExtraSetup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.HideWindow = true
}

// isPebbleLockHeld is not implemented on Windows; always returns false.
func isPebbleLockHeld(_ string) bool { return false }
