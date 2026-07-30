package main

import (
	"fmt"
	"strings"
)

// processState is the result of probing whether a PID belongs to a live
// process. The probe is deliberately three-valued: on some platforms and for
// some ownership combinations the operating system can tell us that a process
// exists, or that it does not, or nothing at all. Callers that act
// destructively on "not running" must distinguish the last case from the
// second.
type processState int

const (
	// processUnknown means the probe could not determine the state. It is not
	// evidence of absence — never treat it as such.
	processUnknown processState = iota
	// processRunning means a process with this PID exists.
	processRunning
	// processDead means no process with this PID exists.
	processDead
)

// probeProcess reports the liveness of a PID. It is a variable so tests can
// substitute a probe without needing a process in a particular ownership or
// privilege state.
var probeProcess = probeProcessNative

// tasklistState maps the outcome of a Windows `tasklist` query to a process
// state. It lives here rather than in the windows-only file so the decision
// table is exercised on every platform; the exec call itself stays there.
func tasklistState(pid int, out []byte, err error) processState {
	if err != nil {
		// The query did not run — that is not evidence the process is gone.
		return processUnknown
	}
	if strings.Contains(string(out), fmt.Sprintf("\"%d\"", pid)) {
		return processRunning
	}
	return processDead
}

// isProcessRunning reports whether a process with the given PID is alive.
// An indeterminate probe is reported as not running; callers that remove
// state on this answer must use probeProcess directly and handle
// processUnknown.
func isProcessRunning(pid int) bool {
	return probeProcess(pid) == processRunning
}
