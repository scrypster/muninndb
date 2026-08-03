package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteReadLogDestFile_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	if err := writeLogDestFile(dir, "/var/log/muninn/custom.log"); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := readLogDestFile(dir)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got != "/var/log/muninn/custom.log" {
		t.Errorf("got %q, want the written path", got)
	}
}

func TestResolveLogPath_ConcretePathIsAuthoritative(t *testing.T) {
	dir := t.TempDir()
	custom := filepath.Join(dir, "elsewhere.log")
	if err := writeLogDestFile(dir, custom); err != nil {
		t.Fatal(err)
	}
	path, inherited := resolveLogPath(dir)
	if inherited {
		t.Fatal("expected inherited=false for a concrete recorded path")
	}
	if path != custom {
		t.Errorf("got path %q, want %q", path, custom)
	}
}

func TestResolveLogPath_InheritedRefusesAPath(t *testing.T) {
	dir := t.TempDir()
	if err := writeLogDestFile(dir, logDestInherited); err != nil {
		t.Fatal(err)
	}
	_, inherited := resolveLogPath(dir)
	if !inherited {
		t.Fatal("expected inherited=true when the sidecar records stderr:inherited")
	}
}

func TestResolveLogPath_FallsBackWhenSidecarAbsent(t *testing.T) {
	dir := t.TempDir()
	path, inherited := resolveLogPath(dir)
	if inherited {
		t.Fatal("expected inherited=false when no sidecar exists at all (pre-fix daemon or nothing has run)")
	}
	want := filepath.Join(dir, "muninn.log")
	if path != want {
		t.Errorf("got %q, want historical default %q", path, want)
	}
}

// TestRunLogs_DoesNotShowStaleContentWhenDaemonLogsElsewhere is the exact
// #852 reproduction: a stale muninn.log from a previous CLI-forked run sits
// in the data dir, but the daemon that wrote muninn.logdest at its last
// startup recorded that its real output is inherited stderr (e.g. it is now
// running under a supervisor). `muninn logs` must not present the fossil
// file's content as if it were current.
func TestRunLogs_DoesNotShowStaleContentWhenDaemonLogsElsewhere(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MUNINNDB_DATA", dir)

	stale := filepath.Join(dir, "muninn.log")
	if err := os.WriteFile(stale, []byte("time=2026-08-02T18:44:21 level=INFO msg=\"shutdown complete\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := writeLogDestFile(dir, logDestInherited); err != nil {
		t.Fatal(err)
	}

	out := captureStdout(func() {
		runLogs([]string{"--no-follow"})
	})

	if strings.Contains(out, "shutdown complete") {
		t.Errorf("must not print stale fossil content as current; got: %s", out)
	}
	if !strings.Contains(out, "inherited") {
		t.Errorf("expected guidance explaining the log destination is inherited; got: %s", out)
	}
}
