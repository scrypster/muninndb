package main

import (
	"errors"
	"testing"
)

// TestTasklistState covers the Windows probe's decision table. A tasklist
// invocation that failed says nothing about the process — the query did not
// run — and must not be reported as absence, or stop would delete the PID
// file of a running daemon whenever tasklist is missing from PATH or the
// process is denied access to it.
//
// The table is kept out of the windows-tagged file so it is exercised on
// every platform; the real tasklist boundary is crossed by
// TestProbeProcessNative_CurrentProcess in process_windows_test.go.
// TestProbeProcess_NonPositivePID pins the classification of a PID no process
// can have. It must be processDead and not processUnknown: a PID file holding
// 0 or a negative number is garbage, and stop has to be able to clear it.
// Both platform probes short-circuit before querying the system.
func TestProbeProcess_NonPositivePID(t *testing.T) {
	for _, pid := range []int{0, -1} {
		if got := probeProcessNative(pid); got != processDead {
			t.Errorf("probeProcessNative(%d) = %v, want processDead (%v)", pid, got, processDead)
		}
	}
}

func TestTasklistState(t *testing.T) {
	cases := []struct {
		name string
		pid  int
		out  string
		err  error
		want processState
	}{
		{
			name: "listed pid is running",
			pid:  4242,
			out:  "\"muninn.exe\",\"4242\",\"Console\",\"1\",\"12,345 K\"\r\n",
			want: processRunning,
		},
		{
			name: "no matching row is dead",
			pid:  4242,
			out:  "INFO: No tasks are running which match the specified criteria.\r\n",
			want: processDead,
		},
		{
			name: "failed invocation is unknown",
			pid:  4242,
			out:  "",
			err:  errors.New(`exec: "tasklist": executable file not found in %PATH%`),
			want: processUnknown,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tasklistState(tc.pid, []byte(tc.out), tc.err); got != tc.want {
				t.Errorf("tasklistState(%d, %q, %v) = %v, want %v", tc.pid, tc.out, tc.err, got, tc.want)
			}
		})
	}
}
