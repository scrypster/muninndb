//go:build !windows

package main

import (
	"errors"
	"syscall"
	"testing"
)

// TestProbeProcessNative_ForeignLiveProcess checks that a live process this
// user may not signal is reported as running, not as dead. signal(0) fails
// with EPERM in that case: the kernel refuses the signal precisely *because*
// the process is there. Treating that refusal as absence makes stop remove the
// PID file of a daemon that is still holding the database lock.
//
// This goes through the real syscall rather than the injectable probe — the
// EPERM mapping is the behavior under test, and a substituted probe would
// assert nothing about it. The precondition is verified rather than assumed:
// as root, or wherever PID 1 happens to be owned by this user, signal(0)
// succeeds and the case cannot arise, which would make the assertion pass for
// the wrong reason.
func TestProbeProcessNative_ForeignLiveProcess(t *testing.T) {
	const foreignPID = 1 // always alive; normally owned by root
	if err := syscall.Kill(foreignPID, 0); !errors.Is(err, syscall.EPERM) {
		t.Skipf("pid %d is signalable by this user (err = %v) — EPERM is unreachable here", foreignPID, err)
	}
	if got := probeProcessNative(foreignPID); got != processRunning {
		t.Errorf("probeProcessNative(%d) = %v, want processRunning (%v)", foreignPID, got, processRunning)
	}
}
