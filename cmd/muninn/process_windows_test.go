//go:build windows

package main

import (
	"os"
	"testing"
)

// TestProbeProcessNative_CurrentProcess crosses the real tasklist boundary.
// TestTasklistState covers the decision table with synthetic output; this
// verifies that an actual tasklist invocation produces output that table
// recognises — the CSV shape, the quoting, and the process being found at all.
func TestProbeProcessNative_CurrentProcess(t *testing.T) {
	if got := probeProcessNative(os.Getpid()); got != processRunning {
		t.Errorf("probeProcessNative(own pid) = %v, want processRunning (%v)", got, processRunning)
	}
}

// TestProbeProcessNative_DeadProcess verifies the same boundary reports a PID
// that does not exist as dead rather than unknown — stop must still be able to
// clear a stale PID file on Windows.
func TestProbeProcessNative_DeadProcess(t *testing.T) {
	if got := probeProcessNative(99999999); got != processDead {
		t.Errorf("probeProcessNative(99999999) = %v, want processDead (%v)", got, processDead)
	}
}
