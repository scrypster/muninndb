package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildDaemonArgs(t *testing.T) {
	cases := []struct {
		name           string
		dataDir        string
		dev            bool
		osArgs         []string
		listenHostEnv  string
		corsOriginsEnv string
		wantContains   []string
		wantAbsent     []string
	}{
		{
			name:       "default listen-host not forwarded",
			dataDir:    "/tmp/data",
			osArgs:     []string{},
			wantAbsent: []string{"--listen-host"},
		},
		{
			name:         "non-default listen-host forwarded",
			dataDir:      "/tmp/data",
			osArgs:       []string{"--listen-host", "0.0.0.0"},
			wantContains: []string{"--listen-host", "0.0.0.0"},
		},
		{
			name:         "cors-origins flag in osArgs forwarded",
			dataDir:      "/tmp/data",
			osArgs:       []string{"--cors-origins", "http://flag.local"},
			wantContains: []string{"--cors-origins", "http://flag.local"},
		},
		{
			name:           "cors-origins from env when no flag",
			dataDir:        "/tmp/data",
			osArgs:         []string{},
			corsOriginsEnv: "http://env.local",
			wantContains:   []string{"--cors-origins", "http://env.local"},
		},
		{
			name:           "flag wins over env for cors-origins",
			dataDir:        "/tmp/data",
			osArgs:         []string{"--cors-origins", "http://flag.local"},
			corsOriginsEnv: "http://env.local",
			wantContains:   []string{"--cors-origins", "http://flag.local"},
			wantAbsent:     []string{"http://env.local"},
		},
		{
			name:       "neither cors flag nor env not forwarded",
			dataDir:    "/tmp/data",
			osArgs:     []string{},
			wantAbsent: []string{"--cors-origins"},
		},
		{
			name:         "dev true forwarded",
			dataDir:      "/tmp/data",
			dev:          true,
			osArgs:       []string{},
			wantContains: []string{"--dev"},
		},
		{
			// Token must NEVER appear in daemon argv — it is read from disk by the daemon.
			// This prevents the token from leaking via `ps ax` or /proc/<pid>/cmdline.
			name:       "mcp-token never in daemon args",
			dataDir:    "/tmp/data",
			osArgs:     []string{},
			wantAbsent: []string{"--mcp-token"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := buildDaemonArgs(tc.dataDir, tc.dev, tc.osArgs, tc.listenHostEnv, tc.corsOriginsEnv)

			for _, want := range tc.wantContains {
				found := false
				for _, arg := range got {
					if arg == want {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("expected %q in args %v", want, got)
				}
			}

			for _, absent := range tc.wantAbsent {
				for _, arg := range got {
					if arg == absent {
						t.Errorf("expected %q to be absent from args %v", absent, got)
						break
					}
				}
			}
		})
	}
}

// TestIsProcessRunningCurrentProcess checks if the current process is identified as running.
func TestIsProcessRunningCurrentProcess(t *testing.T) {
	pid := os.Getpid()
	if !isProcessRunning(pid) {
		t.Errorf("current process (pid %d) should be running", pid)
	}
}

// TestIsProcessRunningDeadProcess checks if a non-existent PID is correctly identified as not running.
func TestIsProcessRunningDeadProcess(t *testing.T) {
	// PID 99999999 almost certainly doesn't exist
	if isProcessRunning(99999999) {
		t.Error("pid 99999999 should not be running")
	}
}

// TestIsProcessRunningNegativePID checks that negative PIDs are handled gracefully.
func TestIsProcessRunningNegativePID(t *testing.T) {
	// Negative PID — should not panic, should return false
	if isProcessRunning(-1) {
		t.Error("negative pid should not be running")
	}
}

// TestIsProcessRunningZeroPID checks that PID 0 is handled correctly.
func TestIsProcessRunningZeroPID(t *testing.T) {
	// PID 0 is special — should return false
	if isProcessRunning(0) {
		t.Error("pid 0 should not be running")
	}
}

// TestDefaultDataDir checks that defaultDataDir returns a valid path under the home directory.
func TestDefaultDataDir(t *testing.T) {
	dir := defaultDataDir()
	if dir == "" {
		t.Error("defaultDataDir returned empty string")
	}
	home, _ := os.UserHomeDir()
	if !strings.HasPrefix(dir, home) {
		t.Errorf("defaultDataDir %q should be under home %q", dir, home)
	}
	if !strings.HasSuffix(dir, "data") {
		t.Errorf("defaultDataDir %q should end with 'data'", dir)
	}
}

// TestDefaultDataDirEnvOverride checks that MUNINNDB_DATA environment variable is respected.
func TestDefaultDataDirEnvOverride(t *testing.T) {
	oldVal := os.Getenv("MUNINNDB_DATA")
	defer os.Setenv("MUNINNDB_DATA", oldVal)

	testDir := "/tmp/test-muninn-data"
	os.Setenv("MUNINNDB_DATA", testDir)

	dir := defaultDataDir()
	if dir != testDir {
		t.Errorf("defaultDataDir = %q, want %q (from MUNINNDB_DATA)", dir, testDir)
	}
}

// TestRunStop_StalePIDFile verifies that a stale PID file — left behind when the
// daemon crashed or was kill -9'd without cleanup — does not make 'muninn stop'
// fail with "process already finished". Stop must detect the dead PID, remove the
// stale sidecars, and succeed.
func TestRunStop_StalePIDFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MUNINNDB_DATA", dir)

	pidPath := filepath.Join(dir, "muninn.pid")
	if err := writePID(pidPath, 99999999); err != nil { // dead PID, per TestIsProcessRunningDeadProcess
		t.Fatalf("writePID: %v", err)
	}
	addrsPath := filepath.Join(dir, addrsFileName)
	if err := writeAddrsFile(dir, daemonAddrs{RestAddr: "127.0.0.1:8475"}); err != nil {
		t.Fatalf("writeAddrsFile: %v", err)
	}

	origExit := osExit
	exitCode := -1
	osExit = func(code int) { exitCode = code }
	defer func() { osExit = origExit }()

	runStop()

	if exitCode != -1 {
		t.Errorf("runStop exited with code %d on a stale PID file; want no exit", exitCode)
	}
	if _, err := os.Stat(pidPath); !os.IsNotExist(err) {
		t.Errorf("stale PID file not removed: stat err = %v", err)
	}
	if _, err := os.Stat(addrsPath); !os.IsNotExist(err) {
		t.Errorf("stale addrs file not removed: stat err = %v", err)
	}
}

// withProbe substitutes the process probe for the duration of a test, so the
// decisions runStop makes can be exercised for every probe outcome without
// needing a process in that state to exist.
func withProbe(t *testing.T, state processState) {
	t.Helper()
	orig := probeProcess
	probeProcess = func(int) processState { return state }
	t.Cleanup(func() { probeProcess = orig })
}

// stubExit captures the exit code instead of terminating the test binary.
// It returns a pointer to the captured code; -1 means runStop never exited.
func stubExit(t *testing.T) *int {
	t.Helper()
	code := -1
	orig := osExit
	osExit = func(c int) { code = c }
	t.Cleanup(func() { osExit = orig })
	return &code
}

// TestRunStop_IndeterminateProbe verifies that a probe which cannot determine
// the state of the PID does not count as "no such process". Removing the
// sidecars on an indeterminate answer would orphan a daemon that is in fact
// still running: the PID file and the address file are the only way to find
// it again.
func TestRunStop_IndeterminateProbe(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MUNINNDB_DATA", dir)

	pidPath := filepath.Join(dir, "muninn.pid")
	if err := writePID(pidPath, 4242); err != nil {
		t.Fatalf("writePID: %v", err)
	}
	addrsPath := filepath.Join(dir, addrsFileName)
	if err := writeAddrsFile(dir, daemonAddrs{RestAddr: "127.0.0.1:8475"}); err != nil {
		t.Fatalf("writeAddrsFile: %v", err)
	}

	withProbe(t, processUnknown)
	exitCode := stubExit(t)

	runStop()

	if *exitCode <= 0 {
		t.Errorf("runStop exit code = %d on an indeterminate probe; want non-zero", *exitCode)
	}
	if _, err := os.Stat(pidPath); err != nil {
		t.Errorf("PID file removed on an indeterminate probe: stat err = %v", err)
	}
	if _, err := os.Stat(addrsPath); err != nil {
		t.Errorf("addrs file removed on an indeterminate probe: stat err = %v", err)
	}
}

// TestRunStop_ForeignLiveProcess covers what an unprivileged 'muninn stop'
// sees when the daemon was started with elevated privileges: a PID file naming
// a process that is alive but cannot be signalled. Stop must refuse rather
// than declare the PID file stale — the daemon still holds the database lock,
// and removing its PID and address files would leave nothing on disk to find
// it by.
//
// The probe is substituted rather than pointed at a real foreign process:
// runStop signals whatever the probe reports as running, and a test that sends
// SIGTERM to a real PID it does not own would terminate that process wherever
// the ownership assumption fails to hold. The EPERM mapping this stands in for
// is verified against the real syscall in
// TestProbeProcessNative_ForeignLiveProcess.
func TestRunStop_ForeignLiveProcess(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MUNINNDB_DATA", dir)

	pidPath := filepath.Join(dir, "muninn.pid")
	if err := writePID(pidPath, 99999999); err != nil {
		t.Fatalf("writePID: %v", err)
	}
	withProbe(t, processRunning)
	addrsPath := filepath.Join(dir, addrsFileName)
	if err := writeAddrsFile(dir, daemonAddrs{RestAddr: "127.0.0.1:8475"}); err != nil {
		t.Fatalf("writeAddrsFile: %v", err)
	}

	exitCode := stubExit(t)

	runStop()

	if *exitCode <= 0 {
		t.Errorf("runStop exit code = %d for a live process owned by another user; want non-zero", *exitCode)
	}
	if _, err := os.Stat(pidPath); err != nil {
		t.Errorf("PID file of a running daemon was removed: stat err = %v", err)
	}
	if _, err := os.Stat(addrsPath); err != nil {
		t.Errorf("addrs file of a running daemon was removed: stat err = %v", err)
	}
}

// TestRunStart_IndeterminateProbe verifies that start applies the same rule as
// stop: a PID whose state could not be established is not a stale PID. Start
// would otherwise delete the PID file and spawn a second daemon against a data
// directory that may still be in use — and on Windows the Pebble lock guard
// that catches this on Unix always reports "not held".
func TestRunStart_IndeterminateProbe(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MUNINNDB_DATA", dir)

	pidPath := filepath.Join(dir, "muninn.pid")
	if err := writePID(pidPath, 4242); err != nil {
		t.Fatalf("writePID: %v", err)
	}

	withProbe(t, processUnknown)

	if err := runStart(false); err == nil {
		t.Error("runStart returned nil on an indeterminate probe; want an error")
	}
	// Assert the contents, not just the presence: the old behavior removed the
	// PID file and forked a daemon, which writes a fresh PID file of its own.
	// A file existing afterwards proves nothing on its own.
	if got, err := readPID(pidPath); err != nil || got != 4242 {
		t.Errorf("PID file after runStart = (%d, %v), want (4242, nil) — the original file must be untouched", got, err)
	}
}

func TestMCPPortFromArgs_Default(t *testing.T) {
	if got := mcpPortFromArgs(nil); got != defaultMCPPort {
		t.Errorf("nil args: got %q, want %q", got, defaultMCPPort)
	}
}

func TestMCPPortFromArgs_CustomPort(t *testing.T) {
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"--mcp-addr", ":8760"}, "8760"},
		{[]string{"--mcp-addr", "0.0.0.0:8760"}, "8760"},
		{[]string{"--mcp-addr", "127.0.0.1:8750"}, "8750"},
		{[]string{"--other-flag", "value"}, defaultMCPPort},
		{[]string{"--mcp-addr=:9000"}, "9000"},
	}
	for _, c := range cases {
		got := mcpPortFromArgs(c.args)
		if got != c.want {
			t.Errorf("mcpPortFromArgs(%v) = %q, want %q", c.args, got, c.want)
		}
	}
}
