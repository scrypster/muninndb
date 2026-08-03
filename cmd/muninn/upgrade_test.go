package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestNewerVersionAvailable(t *testing.T) {
	cases := []struct {
		current, latest string
		want            bool
	}{
		{"v1.0.0", "v1.0.1", true},
		{"v1.0.0", "v1.0.0", false},
		{"v1.0.1", "v1.0.0", false},
		{"v1.2.0", "v2.0.0", true},
		{"dev", "v1.0.0", false},
		{"", "v1.0.0", false},
		{"v1.0.0", "", false},
	}
	for _, tc := range cases {
		got := newerVersionAvailable(tc.current, tc.latest)
		if got != tc.want {
			t.Errorf("newerVersionAvailable(%q, %q) = %v, want %v", tc.current, tc.latest, got, tc.want)
		}
	}
}

func TestIsHomebrewInstall(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"/opt/homebrew/Cellar/muninn/1.0.0/bin/muninn", true},
		{"/usr/local/opt/muninn/bin/muninn", true},
		{"/opt/homebrew/bin/muninn", true},
		{"/usr/local/Cellar/muninn/1.0.0/bin/muninn", true},
		{"/home/user/.local/bin/muninn", false},
		{"/usr/local/bin/muninn", false},
		{"/tmp/muninn", false},
	}
	for _, tc := range cases {
		got := isHomebrewInstallPath(tc.path)
		if got != tc.want {
			t.Errorf("isHomebrewInstallPath(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

func TestReleaseAssetURL(t *testing.T) {
	url := releaseAssetURL("v1.2.3", "darwin", "arm64")
	want := "https://github.com/scrypster/muninndb/releases/download/v1.2.3/muninn_v1.2.3_darwin_arm64.tar.gz"
	if url != want {
		t.Errorf("got %q, want %q", url, want)
	}

	url = releaseAssetURL("v1.2.3", "linux", "amd64")
	want = "https://github.com/scrypster/muninndb/releases/download/v1.2.3/muninn_v1.2.3_linux_amd64.tar.gz"
	if url != want {
		t.Errorf("got %q, want %q", url, want)
	}

	url = releaseAssetURL("v1.2.3", "windows", "amd64")
	want = "https://github.com/scrypster/muninndb/releases/download/v1.2.3/muninn_v1.2.3_windows_amd64.zip"
	if url != want {
		t.Errorf("got %q, want %q", url, want)
	}
}

func TestDownloadAndExtractBinary(t *testing.T) {
	// Build a minimal tar.gz containing a fake "muninn" binary
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	content := []byte("#!/bin/sh\necho fake-binary")
	hdr := &tar.Header{
		Name: "muninn",
		Mode: 0755,
		Size: int64(len(content)),
	}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(content); err != nil {
		t.Fatal(err)
	}
	tw.Close()
	gw.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", buf.Len()))
		w.Write(buf.Bytes())
	}))
	defer srv.Close()

	dest, err := downloadAndExtractBinary(srv.URL, "muninn")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer os.Remove(dest)

	data, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("cannot read extracted file: %v", err)
	}
	if string(data) != string(content) {
		t.Errorf("content mismatch: got %q, want %q", data, content)
	}
}

func TestVerifyBinary(t *testing.T) {
	// Use the current test binary — it's always a real executable
	err := verifyBinary(os.Args[0], "")
	if err != nil {
		t.Errorf("verifyBinary with real binary: %v", err)
	}
}

func TestVerifyBinary_NotExecutable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chmod has no effect on Windows — execute bit check is skipped there")
	}
	f, err := os.CreateTemp("", "muninn-test-*")
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("not a binary")
	f.Close()
	defer os.Remove(f.Name())

	// Make it non-executable
	os.Chmod(f.Name(), 0600)

	err = verifyBinary(f.Name(), "")
	if err == nil {
		t.Error("expected error for non-executable file, got nil")
	}
}

func TestDownloadAndExtractBinary_Progress(t *testing.T) {
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	content := make([]byte, 1024) // 1KB fake binary
	hdr := &tar.Header{Name: "muninn", Mode: 0755, Size: int64(len(content))}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatal(err)
	}
	tw.Write(content)
	tw.Close()
	gw.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", buf.Len()))
		w.Write(buf.Bytes())
	}))
	defer srv.Close()

	var lastReported int64
	dest, _, err := downloadAndExtractBinaryProgress(srv.URL, "muninn", func(downloaded, total int64) {
		lastReported = downloaded
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer os.Remove(dest)
	if lastReported == 0 {
		t.Error("progress callback was never called")
	}
}

// ============================================================================
// NEW HARDENING TESTS
// ============================================================================

func TestNewerVersionAvailable_PreRelease(t *testing.T) {
	cases := []struct {
		current, latest string
		want            bool
	}{
		// Pre-release suffix is stripped — same numeric triple → not newer
		{"v1.0.0-rc.1", "v1.0.0", false},
		{"v1.0.0", "v1.0.0-rc.1", false},
		// Build metadata stripped — same triple → not newer
		{"v1.0.0+build.1", "v1.0.0", false},
		// Minor bump still detected
		{"v1.0.0-rc.1", "v1.1.0", true},
	}
	for _, tc := range cases {
		got := newerVersionAvailable(tc.current, tc.latest)
		if got != tc.want {
			t.Errorf("newerVersionAvailable(%q, %q) = %v, want %v", tc.current, tc.latest, got, tc.want)
		}
	}
}

func TestDownloadAndExtractBinary_NonOKStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	_, err := downloadAndExtractBinary(srv.URL, "muninn")
	if err == nil {
		t.Fatal("expected error for HTTP 404, got nil")
	}
	if !strings.Contains(err.Error(), "HTTP 404") {
		t.Errorf("expected 'HTTP 404' in error, got %q", err.Error())
	}
}

func TestDownloadAndExtractBinary_BinaryNotFound(t *testing.T) {
	// Archive contains "other-file", not "muninn"
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	content := []byte("not the right binary")
	hdr := &tar.Header{Name: "other-file", Mode: 0755, Size: int64(len(content))}
	tw.WriteHeader(hdr)
	tw.Write(content)
	tw.Close()
	gw.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(buf.Bytes())
	}))
	defer srv.Close()

	_, err := downloadAndExtractBinary(srv.URL, "muninn")
	if err == nil {
		t.Fatal("expected error when binary not found in archive, got nil")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("expected 'not found' in error, got %q", err.Error())
	}
}

func TestDownloadAndExtractBinary_DirectoryPrefix(t *testing.T) {
	// Archive entry is "muninn-v1.2.3/muninn" — filepath.Base should match "muninn"
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	content := []byte("#!/bin/sh\necho prefixed")
	hdr := &tar.Header{
		Name: "muninn-v1.2.3/muninn",
		Mode: 0755,
		Size: int64(len(content)),
	}
	tw.WriteHeader(hdr)
	tw.Write(content)
	tw.Close()
	gw.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(buf.Bytes())
	}))
	defer srv.Close()

	dest, err := downloadAndExtractBinary(srv.URL, "muninn")
	if err != nil {
		t.Fatalf("expected directory-prefixed entry to be found, got error: %v", err)
	}
	defer os.Remove(dest)

	data, _ := os.ReadFile(dest)
	if string(data) != string(content) {
		t.Errorf("content mismatch: got %q, want %q", data, content)
	}
}

func TestDownloadAndExtractBinary_CorruptGzip(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("this is not a valid gzip stream"))
	}))
	defer srv.Close()

	_, err := downloadAndExtractBinary(srv.URL, "muninn")
	if err == nil {
		t.Fatal("expected error for corrupt gzip, got nil")
	}
}

func TestProgressReader_NilFn(t *testing.T) {
	pr := &progressReader{
		r:     bytes.NewReader([]byte("hello")),
		total: 5,
		fn:    nil,
	}
	buf := make([]byte, 10)
	n, err := pr.Read(buf)
	if err != nil && err.Error() != "EOF" {
		t.Errorf("unexpected error: %v", err)
	}
	if n == 0 {
		t.Error("expected bytes read, got 0")
	}
}

func TestUpgradeStep_Error(t *testing.T) {
	// Capture stdout to avoid polluting test output
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	called := false
	sentinel := fmt.Errorf("sentinel error")
	err := upgradeStep("Test step...", func() error {
		called = true
		return sentinel
	})

	w.Close()
	os.Stdout = old
	r.Close()

	if !called {
		t.Error("expected fn to be called")
	}
	if err != sentinel {
		t.Errorf("expected sentinel error, got %v", err)
	}
}

func TestUpgradeStep_Success(t *testing.T) {
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	err := upgradeStep("Test step...", func() error {
		return nil
	})

	w.Close()
	os.Stdout = old
	r.Close()

	if err != nil {
		t.Errorf("expected nil error, got %v", err)
	}
}

func TestVerifyBinary_NotExist(t *testing.T) {
	err := verifyBinary("/nonexistent/path/to/muninn", "")
	if err == nil {
		t.Error("expected error for non-existent path, got nil")
	}
}

func TestRunUpgrade_AlreadyUpToDate(t *testing.T) {
	orig := latestReleaseFn
	latestReleaseFn = func() (releaseInfo, error) { return releaseInfo{TagName: "v1.0.0"}, nil }
	defer func() { latestReleaseFn = orig }()

	origVersion := version
	version = "v1.0.0"
	defer func() { version = origVersion }()

	runUpgrade([]string{})
}

func TestRunUpgrade_CheckOnly_UpdateAvailable(t *testing.T) {
	orig := latestReleaseFn
	latestReleaseFn = func() (releaseInfo, error) { return releaseInfo{TagName: "v2.0.0"}, nil }
	defer func() { latestReleaseFn = orig }()

	origVersion := version
	version = "v1.0.0"
	defer func() { version = origVersion }()

	origExit := osExit
	var exitCode int
	osExit = func(code int) { exitCode = code }
	defer func() { osExit = origExit }()

	runUpgrade([]string{"--check"})

	if exitCode != 1 {
		t.Errorf("expected exit code 1 for --check with update available, got %d", exitCode)
	}
}

// TestWaitForProcessExit_AlreadyDead verifies that waitForProcessExit returns
// nil immediately for a PID that does not exist.
func TestWaitForProcessExit_AlreadyDead(t *testing.T) {
	// PID 99999999 is astronomically unlikely to exist on any real system.
	if err := waitForProcessExit(99999999, 5*time.Second); err != nil {
		t.Errorf("expected nil for dead PID, got: %v", err)
	}
}

// TestWaitForProcessExit_Timeout verifies that waitForProcessExit returns an
// error when the process is still alive after the timeout elapses.
func TestWaitForProcessExit_Timeout(t *testing.T) {
	// os.Getpid() is always alive — guaranteed to trigger the timeout path.
	err := waitForProcessExit(os.Getpid(), 300*time.Millisecond)
	if err == nil {
		t.Error("expected error for alive PID (current process), got nil")
	}
}

// Compile-time check: runStart must return error.
// If this does not compile, the signature regression is caught immediately.
var _ func(bool) error = runStart

// TestStopDaemonForUpgrade_ForeignLiveProcess verifies that the Homebrew
// upgrade path does not delete the PID file of a daemon it failed to stop.
// A daemon started with elevated privileges cannot be signalled by an
// unprivileged upgrade; deleting its PID file would leave it running against
// the database with nothing on disk to find it by, and the upgrade would then
// report the stop as successful.
func TestStopDaemonForUpgrade_ForeignLiveProcess(t *testing.T) {
	dir := t.TempDir()
	pidPath := filepath.Join(dir, "muninn.pid")
	if err := writePID(pidPath, 99999999); err != nil {
		t.Fatalf("writePID: %v", err)
	}

	// A process that stays alive however hard we signal it — what an
	// unprivileged stop of a root-owned daemon looks like.
	withProbe(t, processRunning)

	if stopDaemonForUpgrade(pidPath, 200*time.Millisecond) {
		t.Error("stopDaemonForUpgrade reported success for a daemon that is still running")
	}
	if _, err := os.Stat(pidPath); err != nil {
		t.Errorf("PID file of a running daemon was removed: stat err = %v", err)
	}
}

// TestStopDaemonForUpgrade_DeadProcess verifies the ordinary path still works:
// a daemon that is gone has its PID file cleared and is reported as stopped.
func TestStopDaemonForUpgrade_DeadProcess(t *testing.T) {
	dir := t.TempDir()
	pidPath := filepath.Join(dir, "muninn.pid")
	if err := writePID(pidPath, 99999999); err != nil { // dead PID
		t.Fatalf("writePID: %v", err)
	}

	if !stopDaemonForUpgrade(pidPath, 200*time.Millisecond) {
		t.Error("stopDaemonForUpgrade reported failure for a dead daemon")
	}
	if _, err := os.Stat(pidPath); !os.IsNotExist(err) {
		t.Errorf("stale PID file not removed: stat err = %v", err)
	}
}

// withRunningDaemonPIDs substitutes runningDaemonPIDs for the duration of a
// test, so exe-scan-only daemon detection (no PID file, e.g. a
// service-managed process that never wrote muninn.pid — #792) can be
// exercised without a real matching process on disk.
func withRunningDaemonPIDs(t *testing.T, pids []int) {
	t.Helper()
	orig := runningDaemonPIDs
	runningDaemonPIDs = func() []int { return pids }
	t.Cleanup(func() { runningDaemonPIDs = orig })
}

// TestStopDaemonInstances_NoPIDFile_ExeScanFindsLiveProcess proves the core
// #792 defect: a daemon with no muninn.pid (launchd, a systemd Type=simple
// unit execing the server directly, a bare `--daemon` process) is invisible
// to the PID-file check alone, so a stop step that only reads the PID file
// stops nothing and — under the pre-fix upgradeStep contract — still prints
// "Stopping daemon... ✓". stopDaemonInstances must instead find this process
// via the exe-scan and refuse to report success while it stays alive.
func TestStopDaemonInstances_NoPIDFile_ExeScanFindsLiveProcess(t *testing.T) {
	dir := t.TempDir()
	pidPath := filepath.Join(dir, "muninn.pid") // deliberately never written

	// A process that stays alive however hard we signal it — same shape
	// TestStopDaemonForUpgrade_ForeignLiveProcess uses for the PID-file case.
	withProbe(t, processRunning)
	withRunningDaemonPIDs(t, []int{99999999})

	stopped, err := stopDaemonInstances(pidPath, 200*time.Millisecond)
	if err == nil {
		t.Fatal("expected stopDaemonInstances to refuse success — a live process running this binary was found via exe-scan and never died")
	}
	if !stopped {
		t.Error("expected stoppedAny=true — a stop was attempted on the exe-scan PID")
	}
	if !strings.Contains(err.Error(), "99999999") {
		t.Errorf("error does not name the stuck pid: %v", err)
	}
}

// TestStopDaemonInstances_NoPIDFile_NoLiveProcess is the negative control:
// with no PID file and no exe-scan hits, there is genuinely nothing to stop,
// and reporting stoppedAny=false with a nil error is honest, not a false
// positive.
func TestStopDaemonInstances_NoPIDFile_NoLiveProcess(t *testing.T) {
	dir := t.TempDir()
	pidPath := filepath.Join(dir, "muninn.pid")
	withRunningDaemonPIDs(t, nil)

	stopped, err := stopDaemonInstances(pidPath, 200*time.Millisecond)
	if err != nil {
		t.Fatalf("expected no error when nothing is running, got: %v", err)
	}
	if stopped {
		t.Error("expected stoppedAny=false when nothing is running")
	}
}

// TestStopDaemonInstances_PIDFileDeadProcess_ExeScanConfirmsGone verifies the
// ordinary case still works: a stale PID file naming a dead process, and the
// exe-scan agreeing nothing is running, reports success and clears the file.
func TestStopDaemonInstances_PIDFileDeadProcess_ExeScanConfirmsGone(t *testing.T) {
	dir := t.TempDir()
	pidPath := filepath.Join(dir, "muninn.pid")
	if err := writePID(pidPath, 99999999); err != nil {
		t.Fatalf("writePID: %v", err)
	}
	withRunningDaemonPIDs(t, nil)

	stopped, err := stopDaemonInstances(pidPath, 200*time.Millisecond)
	if err != nil {
		t.Fatalf("expected no error for an already-dead daemon, got: %v", err)
	}
	if stopped {
		t.Error("expected stoppedAny=false for a PID file naming an already-dead process")
	}
	if _, statErr := os.Stat(pidPath); !os.IsNotExist(statErr) {
		t.Errorf("stale PID file not removed: stat err = %v", statErr)
	}
}
