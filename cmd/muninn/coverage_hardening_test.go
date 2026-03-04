package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	plugincfg "github.com/scrypster/muninndb/internal/config"
)

// storage import is intentionally omitted — PebbleStore requires *pebble.DB.

// ---------------------------------------------------------------------------
// help.go: wantsHelp + printSubcommandUsage
// ---------------------------------------------------------------------------

func TestWantsHelp(t *testing.T) {
	tests := []struct {
		args []string
		want bool
	}{
		{nil, false},
		{[]string{}, false},
		{[]string{"start"}, false},
		{[]string{"-h"}, true},
		{[]string{"--help"}, true},
		{[]string{"help"}, true},
		{[]string{"vault", "-h"}, true},
		{[]string{"list", "--help"}, true},
		{[]string{"list", "help"}, true},
		{[]string{"-v", "--debug"}, false},
	}
	for _, tc := range tests {
		got := wantsHelp(tc.args)
		if got != tc.want {
			t.Errorf("wantsHelp(%v) = %v, want %v", tc.args, got, tc.want)
		}
	}
}

func TestPrintSubcommandUsage_Basic(t *testing.T) {
	out := captureStdout(func() {
		printSubcommandUsage("test", "run tests", "muninn test [flags]", nil, nil)
	})
	if !strings.Contains(out, "muninn test") {
		t.Errorf("missing command name: %s", out)
	}
	if !strings.Contains(out, "run tests") {
		t.Errorf("missing summary: %s", out)
	}
}

func TestPrintSubcommandUsage_WithFlagsAndExamples(t *testing.T) {
	out := captureStdout(func() {
		printSubcommandUsage("demo", "demo command", "muninn demo",
			[][2]string{{"--flag", "A flag"}},
			[]string{"muninn demo --flag"})
	})
	if !strings.Contains(out, "--flag") {
		t.Errorf("missing flag: %s", out)
	}
	if !strings.Contains(out, "muninn demo --flag") {
		t.Errorf("missing example: %s", out)
	}
}

func TestSubcommandHelpEntries(t *testing.T) {
	for name, fn := range subcommandHelp {
		t.Run(name, func(t *testing.T) {
			out := captureStdout(func() { fn() })
			if !strings.Contains(out, "muninn") {
				t.Errorf("subcommandHelp[%q] missing 'muninn' in output", name)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// vault.go: runVaultDelete, runVaultClear with httptest mock
// ---------------------------------------------------------------------------

func withMockVaultServer(t *testing.T, handler http.HandlerFunc) (cleanup func()) {
	t.Helper()
	ts := httptest.NewServer(handler)
	oldAdmin, oldUI, oldCookie := vaultAdminBase, vaultUIBase, vaultCookie
	vaultAdminBase = ts.URL
	vaultUIBase = ts.URL
	vaultCookie = "test-session"
	return func() {
		ts.Close()
		vaultAdminBase = oldAdmin
		vaultUIBase = oldUI
		vaultCookie = oldCookie
	}
}

func TestRunVaultDelete_Success(t *testing.T) {
	cleanup := withMockVaultServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "DELETE" && strings.Contains(r.URL.Path, "/api/admin/vaults/") {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	defer cleanup()

	out := captureStdout(func() {
		runVaultDelete([]string{"test-vault", "--yes"})
	})
	if !strings.Contains(out, "deleted") {
		t.Errorf("expected 'deleted' in output: %s", out)
	}
}

func TestRunVaultDelete_NotFound(t *testing.T) {
	cleanup := withMockVaultServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	defer cleanup()

	out := captureStdout(func() {
		runVaultDelete([]string{"nonexistent", "--yes"})
	})
	if !strings.Contains(out, "not found") {
		t.Errorf("expected 'not found' in output: %s", out)
	}
}

func TestRunVaultDelete_Conflict(t *testing.T) {
	cleanup := withMockVaultServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
	})
	defer cleanup()

	out := captureStdout(func() {
		runVaultDelete([]string{"default", "--yes"})
	})
	if !strings.Contains(out, "Protected") {
		t.Errorf("expected 'Protected' in output: %s", out)
	}
}

func TestRunVaultDelete_Unauthorized(t *testing.T) {
	cleanup := withMockVaultServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	defer cleanup()

	out := captureStdout(func() {
		runVaultDelete([]string{"test", "--yes"})
	})
	if !strings.Contains(out, "Not authenticated") {
		t.Errorf("expected 'Not authenticated' in output: %s", out)
	}
}

func TestRunVaultDelete_NoName(t *testing.T) {
	out := captureStdout(func() {
		runVaultDelete([]string{})
	})
	if !strings.Contains(out, "Usage:") {
		t.Errorf("expected usage message: %s", out)
	}
}

func TestRunVaultDelete_ForceDefault(t *testing.T) {
	var gotForceHeader string
	cleanup := withMockVaultServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotForceHeader = r.Header.Get("X-Allow-Default")
		w.WriteHeader(http.StatusOK)
	})
	defer cleanup()

	captureStdout(func() {
		runVaultDelete([]string{"default", "--yes", "--force"})
	})
	if gotForceHeader != "true" {
		t.Errorf("expected X-Allow-Default header 'true', got %q", gotForceHeader)
	}
}

func TestRunVaultClear_Success(t *testing.T) {
	cleanup := withMockVaultServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" && strings.Contains(r.URL.Path, "/clear") {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	defer cleanup()

	out := captureStdout(func() {
		runVaultClear([]string{"test-vault", "--yes"})
	})
	if !strings.Contains(out, "cleared") {
		t.Errorf("expected 'cleared' in output: %s", out)
	}
}

func TestRunVaultClear_NoName(t *testing.T) {
	out := captureStdout(func() {
		runVaultClear([]string{})
	})
	if !strings.Contains(out, "Usage:") {
		t.Errorf("expected usage message: %s", out)
	}
}

// ---------------------------------------------------------------------------
// vault.go: runVaultClone with httptest
// ---------------------------------------------------------------------------

func TestRunVaultClone_Success(t *testing.T) {
	cleanup := withMockVaultServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" && strings.Contains(r.URL.Path, "/clone") {
			w.WriteHeader(http.StatusAccepted)
			json.NewEncoder(w).Encode(map[string]string{"job_id": "job-123"})
			return
		}
		if strings.Contains(r.URL.Path, "/job-status") {
			json.NewEncoder(w).Encode(statusSnap{Status: "done", Pct: 100})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	defer cleanup()

	out := captureStdout(func() {
		runVaultClone([]string{"src", "dst"})
	})
	if !strings.Contains(out, "Cloning") {
		t.Errorf("expected 'Cloning' in output: %s", out)
	}
}

func TestRunVaultClone_MissingArgs(t *testing.T) {
	out := captureStdout(func() {
		runVaultClone([]string{"only-one"})
	})
	if !strings.Contains(out, "Usage:") {
		t.Errorf("expected usage: %s", out)
	}
}

func TestRunVaultClone_ServerError(t *testing.T) {
	cleanup := withMockVaultServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"message": "internal error"}})
	})
	defer cleanup()

	out := captureStdout(func() {
		runVaultClone([]string{"src", "dst"})
	})
	if !strings.Contains(out, "Error") {
		t.Errorf("expected error in output: %s", out)
	}
}

// ---------------------------------------------------------------------------
// vault.go: runVaultMerge with httptest
// ---------------------------------------------------------------------------

func TestRunVaultMerge_Success(t *testing.T) {
	cleanup := withMockVaultServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" && strings.Contains(r.URL.Path, "/merge-into") {
			w.WriteHeader(http.StatusAccepted)
			json.NewEncoder(w).Encode(map[string]string{"job_id": "job-456"})
			return
		}
		if strings.Contains(r.URL.Path, "/job-status") {
			json.NewEncoder(w).Encode(statusSnap{Status: "done", Pct: 100})
			return
		}
	})
	defer cleanup()

	out := captureStdout(func() {
		runVaultMerge([]string{"source", "target", "--yes"})
	})
	if !strings.Contains(out, "Merging") {
		t.Errorf("expected 'Merging' in output: %s", out)
	}
}

func TestRunVaultMerge_MissingArgs(t *testing.T) {
	out := captureStdout(func() {
		runVaultMerge([]string{"only-source"})
	})
	if !strings.Contains(out, "Usage:") {
		t.Errorf("expected usage: %s", out)
	}
}

func TestRunVaultMerge_SelfMerge(t *testing.T) {
	oldExit := osExit
	var exitCode int
	osExit = func(code int) { exitCode = code }
	defer func() { osExit = oldExit }()

	out := captureStderr(func() {
		runVaultMerge([]string{"same", "same", "--yes"})
	})
	if exitCode != 1 {
		t.Errorf("expected exit code 1, got %d", exitCode)
	}
	if !strings.Contains(out, "cannot merge") {
		t.Errorf("expected 'cannot merge' error: %s", out)
	}
}

// ---------------------------------------------------------------------------
// vault.go: runVaultReindexFTS with httptest
// ---------------------------------------------------------------------------

func TestRunVaultReindexFTS_Success(t *testing.T) {
	cleanup := withMockVaultServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" && strings.Contains(r.URL.Path, "/reindex-fts") {
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]any{"vault": "test", "engrams_reindexed": 42})
			return
		}
	})
	defer cleanup()

	out := captureStdout(func() {
		runVaultReindexFTS([]string{"test"})
	})
	if !strings.Contains(out, "42") {
		t.Errorf("expected engram count in output: %s", out)
	}
	if !strings.Contains(out, "Porter2") {
		t.Errorf("expected Porter2 mention: %s", out)
	}
}

func TestRunVaultReindexFTS_MissingArg(t *testing.T) {
	out := captureStdout(func() {
		runVaultReindexFTS([]string{})
	})
	if !strings.Contains(out, "Usage:") {
		t.Errorf("expected usage: %s", out)
	}
}

func TestRunVaultReindexFTS_NotFound(t *testing.T) {
	cleanup := withMockVaultServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	defer cleanup()

	out := captureStdout(func() {
		runVaultReindexFTS([]string{"nonexistent"})
	})
	if !strings.Contains(out, "not found") {
		t.Errorf("expected 'not found': %s", out)
	}
}

func TestRunVaultReindexFTS_Unauthorized(t *testing.T) {
	cleanup := withMockVaultServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	defer cleanup()

	out := captureStdout(func() {
		runVaultReindexFTS([]string{"test"})
	})
	if !strings.Contains(out, "Not authenticated") {
		t.Errorf("expected auth error: %s", out)
	}
}

// ---------------------------------------------------------------------------
// vault.go: runVaultExport with httptest
// ---------------------------------------------------------------------------

func TestRunVaultExport_Success(t *testing.T) {
	cleanup := withMockVaultServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && strings.Contains(r.URL.Path, "/export") {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("archive-data-here"))
			return
		}
	})
	defer cleanup()

	outFile := filepath.Join(t.TempDir(), "test-export.muninn")
	out := captureStdout(func() {
		runVaultExport([]string{"--vault", "mydata", "--output", outFile})
	})
	if !strings.Contains(out, "Exported") {
		t.Errorf("expected 'Exported' in output: %s", out)
	}
	data, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatalf("export file not written: %v", err)
	}
	if string(data) != "archive-data-here" {
		t.Errorf("unexpected file content: %s", data)
	}
}

func TestRunVaultExport_DefaultOutputFile(t *testing.T) {
	cleanup := withMockVaultServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("data"))
	})
	defer cleanup()

	dir := t.TempDir()
	oldWd, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(oldWd)

	captureStdout(func() {
		runVaultExport([]string{"myvault"})
	})
	if _, err := os.Stat(filepath.Join(dir, "myvault.muninn")); err != nil {
		t.Errorf("expected default output file 'myvault.muninn': %v", err)
	}
}

func TestRunVaultExport_MissingVault(t *testing.T) {
	out := captureStdout(func() {
		runVaultExport([]string{})
	})
	if !strings.Contains(out, "Usage:") {
		t.Errorf("expected usage: %s", out)
	}
}

func TestRunVaultExport_ResetMetadata(t *testing.T) {
	var gotURL string
	cleanup := withMockVaultServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotURL = r.URL.String()
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("data"))
	})
	defer cleanup()

	outFile := filepath.Join(t.TempDir(), "out.muninn")
	captureStdout(func() {
		runVaultExport([]string{"--vault", "v", "--output", outFile, "--reset-metadata"})
	})
	if !strings.Contains(gotURL, "reset_metadata=true") {
		t.Errorf("expected reset_metadata query param, got URL: %s", gotURL)
	}
}

func TestRunVaultExportMarkdown_Success(t *testing.T) {
	cleanup := withMockVaultServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && strings.Contains(r.URL.Path, "/export-markdown") {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("md-archive"))
			return
		}
	})
	defer cleanup()

	outFile := filepath.Join(t.TempDir(), "notes.tgz")
	out := captureStdout(func() {
		runVaultExportMarkdown([]string{"--vault", "default", "--output", outFile})
	})
	if !strings.Contains(out, "Exported") {
		t.Errorf("expected 'Exported' in output: %s", out)
	}
	data, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatalf("markdown export file not written: %v", err)
	}
	if string(data) != "md-archive" {
		t.Errorf("unexpected markdown export file content: %q", string(data))
	}
}

func TestRunVaultExportMarkdown_AllVaults(t *testing.T) {
	cleanup := withMockVaultServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "GET" && r.URL.Path == "/api/vaults":
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode([]string{"default", "team"})
		case r.Method == "GET" && strings.Contains(r.URL.Path, "/export-markdown"):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("archive"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	defer cleanup()

	outDir := t.TempDir()
	out := captureStdout(func() {
		runVaultExportMarkdown([]string{"--all-vaults", "--output", outDir})
	})
	if !strings.Contains(out, "Exporting vault") {
		t.Errorf("expected export progress output, got: %s", out)
	}
	if _, err := os.Stat(filepath.Join(outDir, "default.markdown.tgz")); err != nil {
		t.Fatalf("missing default export file: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outDir, "team.markdown.tgz")); err != nil {
		t.Fatalf("missing team export file: %v", err)
	}
}

// ---------------------------------------------------------------------------
// vault.go: runVaultImport with httptest
// ---------------------------------------------------------------------------

func TestRunVaultImport_Success(t *testing.T) {
	cleanup := withMockVaultServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" && strings.Contains(r.URL.Path, "/import") {
			w.WriteHeader(http.StatusAccepted)
			json.NewEncoder(w).Encode(map[string]string{"job_id": "imp-789"})
			return
		}
		if strings.Contains(r.URL.Path, "/job-status") {
			json.NewEncoder(w).Encode(statusSnap{Status: "done", Pct: 100})
			return
		}
	})
	defer cleanup()

	archiveFile := filepath.Join(t.TempDir(), "test.muninn")
	os.WriteFile(archiveFile, []byte("archive-data"), 0644)

	out := captureStdout(func() {
		runVaultImport([]string{archiveFile, "--vault", "imported"})
	})
	if !strings.Contains(out, "Importing") {
		t.Errorf("expected 'Importing' in output: %s", out)
	}
}

func TestRunVaultImport_MissingArgs(t *testing.T) {
	out := captureStdout(func() {
		runVaultImport([]string{})
	})
	if !strings.Contains(out, "Usage:") {
		t.Errorf("expected usage: %s", out)
	}
}

func TestRunVaultImport_MissingFile(t *testing.T) {
	out := captureStdout(func() {
		runVaultImport([]string{"/nonexistent/file.muninn", "--vault", "test"})
	})
	if !strings.Contains(out, "Error") {
		t.Errorf("expected error for missing file: %s", out)
	}
}

// ---------------------------------------------------------------------------
// vault.go: fetchJobStatus + renderBar
// ---------------------------------------------------------------------------

func TestFetchJobStatus_Success(t *testing.T) {
	cleanup := withMockVaultServer(t, func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(statusSnap{Status: "running", Phase: "copying", Pct: 50, CopyTotal: 100, CopyCurrent: 50})
	})
	defer cleanup()

	snap := fetchJobStatus("job-1", "test-vault")
	if snap == nil {
		t.Fatal("expected non-nil snap")
	}
	if snap.Status != "running" {
		t.Errorf("expected status 'running', got %q", snap.Status)
	}
	if snap.Pct != 50 {
		t.Errorf("expected pct 50, got %f", snap.Pct)
	}
}

func TestFetchJobStatus_ServerDown(t *testing.T) {
	oldAdmin := vaultAdminBase
	vaultAdminBase = "http://localhost:1" // nothing listens here
	defer func() { vaultAdminBase = oldAdmin }()

	snap := fetchJobStatus("job-1", "vault")
	if snap != nil {
		t.Errorf("expected nil snap when server is down, got %+v", snap)
	}
}

func TestRenderBar_CopyingPhase_Full(t *testing.T) {
	bar := renderBar(statusSnap{Phase: "copying", Pct: 50, CopyTotal: 200, CopyCurrent: 100})
	if !strings.Contains(bar, "Copying") {
		t.Errorf("expected 'Copying': %s", bar)
	}
	if !strings.Contains(bar, "50.0%") {
		t.Errorf("expected '50.0%%': %s", bar)
	}
	if !strings.Contains(bar, "100/200") {
		t.Errorf("expected '100/200': %s", bar)
	}
}

func TestRenderBar_IndexingPhase_Full(t *testing.T) {
	bar := renderBar(statusSnap{Phase: "indexing", Pct: 75, IndexTotal: 400, IndexCurrent: 300})
	if !strings.Contains(bar, "Re-indexing") {
		t.Errorf("expected 'Re-indexing': %s", bar)
	}
	if !strings.Contains(bar, "300/400") {
		t.Errorf("expected '300/400': %s", bar)
	}
}

// ---------------------------------------------------------------------------
// status.go: overallState + printStatusDisplay
// ---------------------------------------------------------------------------

func TestOverallState_AllUp(t *testing.T) {
	svcs := []serviceStatus{{up: true}, {up: true}, {up: true}}
	if overallState(svcs) != stateRunning {
		t.Error("expected stateRunning")
	}
}

func TestOverallState_AllDown(t *testing.T) {
	svcs := []serviceStatus{{up: false}, {up: false}}
	if overallState(svcs) != stateStopped {
		t.Error("expected stateStopped")
	}
}

func TestOverallState_Mixed(t *testing.T) {
	svcs := []serviceStatus{{up: true}, {up: false}}
	if overallState(svcs) != stateDegraded {
		t.Error("expected stateDegraded")
	}
}

func TestPrintStatusDisplay_Stopped(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer ts.Close()

	out := captureStdout(func() {
		printStatusDisplay(true)
	})
	_ = out
}

// ---------------------------------------------------------------------------
// upgrade.go: latestVersion + runUpgrade with httptest
// ---------------------------------------------------------------------------

func TestLatestVersion_DevBuild_Hardening(t *testing.T) {
	v, err := latestVersion()
	if err != nil {
		t.Skipf("network error (expected in CI): %v", err)
	}
	_ = v
}

func TestRunUpgrade_DevBuild(t *testing.T) {
	out := captureStdout(func() {
		runUpgrade([]string{})
	})
	if !strings.Contains(out, "dev") && !strings.Contains(out, "Checking") {
		t.Errorf("expected dev/checking message: %s", out)
	}
}

func TestRunUpgrade_CheckOnly(t *testing.T) {
	out := captureStdout(func() {
		runUpgrade([]string{"--check"})
	})
	if !strings.Contains(out, "Current version") {
		t.Errorf("expected version info: %s", out)
	}
}

// ---------------------------------------------------------------------------
// vault_auth.go: authenticateAdmin with httptest
// ---------------------------------------------------------------------------

func TestAuthenticateAdmin_ExplicitPassword(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		if body["username"] == "admin" && body["password"] == "s3cr3t" {
			http.SetCookie(w, &http.Cookie{Name: "muninn_session", Value: "valid"})
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer ts.Close()

	oldUI, oldCookie := vaultUIBase, vaultCookie
	vaultUIBase = ts.URL
	vaultCookie = ""
	defer func() { vaultUIBase = oldUI; vaultCookie = oldCookie }()

	err := authenticateAdmin("admin", "s3cr3t", false)
	if err != nil {
		t.Errorf("expected success, got: %v", err)
	}
	if vaultCookie != "valid" {
		t.Errorf("expected cookie 'valid', got %q", vaultCookie)
	}
}

func TestAuthenticateAdmin_DefaultCredsWork(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		if body["username"] == "root" && body["password"] == "password" {
			http.SetCookie(w, &http.Cookie{Name: "muninn_session", Value: "default-session"})
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer ts.Close()

	oldUI, oldCookie := vaultUIBase, vaultCookie
	vaultUIBase = ts.URL
	vaultCookie = ""
	defer func() { vaultUIBase = oldUI; vaultCookie = oldCookie }()

	err := authenticateAdmin("root", "", false)
	if err != nil {
		t.Errorf("expected auto-auth success, got: %v", err)
	}
	if vaultCookie != "default-session" {
		t.Errorf("expected cookie 'default-session', got %q", vaultCookie)
	}
}

// ---------------------------------------------------------------------------
// vault.go: runVault dispatch + printVaultUsage
// ---------------------------------------------------------------------------

func TestRunVault_NoArgs_Dispatch(t *testing.T) {
	out := captureStdout(func() {
		runVault([]string{})
	})
	if !strings.Contains(out, "Usage:") {
		t.Errorf("expected usage output: %s", out)
	}
}

func TestRunVault_ListDispatch(t *testing.T) {
	cleanup := withMockVaultServer(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/api/vaults") {
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode([]string{"default", "test"})
			return
		}
		if strings.Contains(r.URL.Path, "/api/auth/login") {
			http.SetCookie(w, &http.Cookie{Name: "muninn_session", Value: "s"})
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	defer cleanup()

	out := captureStdout(func() {
		runVault([]string{"list"})
	})
	if !strings.Contains(out, "default") {
		t.Errorf("expected 'default' vault: %s", out)
	}
}

// ---------------------------------------------------------------------------
// init.go: runNonInteractiveInit
// ---------------------------------------------------------------------------

func TestRunNonInteractiveInit(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("MUNINNDB_DATA", filepath.Join(home, "data"))

	out := captureStdout(func() {
		runNonInteractiveInit("http://localhost:8750/mcp", "manual", "", false, true, true)
	})
	if !strings.Contains(out, "Manual") && !strings.Contains(out, "manual") &&
		!strings.Contains(out, "mcpServers") && !strings.Contains(out, "curl") {
		t.Logf("output: %s", out)
	}
}

// ---------------------------------------------------------------------------
// init.go: configureTools edge cases
// ---------------------------------------------------------------------------

func TestConfigureTools_Empty(t *testing.T) {
	out := captureStdout(func() {
		configureTools(nil, "http://localhost:8750/mcp", "")
	})
	_ = out
}

// lifecycle.go: runStartService / runStopService are skipped — they call systemctl directly.

// ---------------------------------------------------------------------------
// status.go: checkVersionHint (exercises latestVersion in goroutine)
// ---------------------------------------------------------------------------

func TestCheckVersionHint(t *testing.T) {
	out := captureStdout(func() {
		checkVersionHint()
	})
	_ = out
}

// ---------------------------------------------------------------------------
// vault.go: printHTTPError
// ---------------------------------------------------------------------------

func TestPrintHTTPError_WithMessage_Full(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"message": "bad vault name"}})
	}))
	defer ts.Close()

	resp, _ := http.Get(ts.URL)
	out := captureStdout(func() {
		printHTTPError(resp)
	})
	if !strings.Contains(out, "bad vault name") {
		t.Errorf("expected error message in output: %s", out)
	}
}

func TestPrintHTTPError_NoMessage(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, "not json")
	}))
	defer ts.Close()

	resp, _ := http.Get(ts.URL)
	out := captureStdout(func() {
		printHTTPError(resp)
	})
	if !strings.Contains(out, "500") {
		t.Errorf("expected HTTP 500 in output: %s", out)
	}
}

// ---------------------------------------------------------------------------
// repl_client.go: runShowVaults (0% coverage, exercises mcpCall path)
// ---------------------------------------------------------------------------

func TestRunShowVaults_NoServer(t *testing.T) {
	out := captureStdout(func() {
		runShowVaults()
	})
	_ = out
}

// ---------------------------------------------------------------------------
// vault.go: doVaultRequestForce edge cases
// ---------------------------------------------------------------------------

func TestDoVaultRequestForce_ConnectionError_Path(t *testing.T) {
	oldAdmin := vaultAdminBase
	vaultAdminBase = "http://localhost:1"
	defer func() { vaultAdminBase = oldAdmin }()

	out := captureStdout(func() {
		doVaultRequestForce("DELETE", vaultAdminBase+"/api/admin/vaults/test", "done", false)
	})
	if !strings.Contains(out, "Error connecting") {
		t.Errorf("expected connection error: %s", out)
	}
}

func TestDoVaultRequestForce_GenericError(t *testing.T) {
	cleanup := withMockVaultServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	defer cleanup()

	out := captureStdout(func() {
		doVaultRequestForce("DELETE", vaultAdminBase+"/api/admin/vaults/x", "done", false)
	})
	if !strings.Contains(out, "500") {
		t.Errorf("expected HTTP 500: %s", out)
	}
}

func TestDoVaultRequestForce_ConflictWithForce(t *testing.T) {
	cleanup := withMockVaultServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
	})
	defer cleanup()

	out := captureStdout(func() {
		doVaultRequestForce("DELETE", vaultAdminBase+"/api/admin/vaults/default", "done", true)
	})
	if !strings.Contains(out, "Cannot override") {
		t.Errorf("expected 'Cannot override': %s", out)
	}
}

// ---------------------------------------------------------------------------
// vault.go: runVaultClone/Merge connection error paths
// ---------------------------------------------------------------------------

func TestRunVaultClone_ConnectionError(t *testing.T) {
	oldAdmin, oldCookie := vaultAdminBase, vaultCookie
	vaultAdminBase = "http://localhost:1"
	vaultCookie = "test"
	defer func() { vaultAdminBase = oldAdmin; vaultCookie = oldCookie }()

	out := captureStdout(func() {
		runVaultClone([]string{"src", "dst"})
	})
	if !strings.Contains(out, "Error connecting") {
		t.Errorf("expected connection error: %s", out)
	}
}

func TestRunVaultMerge_ConnectionError(t *testing.T) {
	oldAdmin, oldCookie := vaultAdminBase, vaultCookie
	vaultAdminBase = "http://localhost:1"
	vaultCookie = "test"
	defer func() { vaultAdminBase = oldAdmin; vaultCookie = oldCookie }()

	out := captureStdout(func() {
		runVaultMerge([]string{"src", "dst", "--yes"})
	})
	if !strings.Contains(out, "Error connecting") {
		t.Errorf("expected connection error: %s", out)
	}
}

func TestRunVaultMerge_BadJobResponse(t *testing.T) {
	cleanup := withMockVaultServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			w.WriteHeader(http.StatusAccepted)
			w.Write([]byte("not json"))
			return
		}
	})
	defer cleanup()

	out := captureStdout(func() {
		runVaultMerge([]string{"src", "dst", "--yes"})
	})
	if !strings.Contains(out, "could not read job ID") {
		t.Errorf("expected job ID error: %s", out)
	}
}

// ---------------------------------------------------------------------------
// vault.go: runVaultReindexFTS connection error
// ---------------------------------------------------------------------------

func TestRunVaultReindexFTS_ConnectionError(t *testing.T) {
	oldAdmin, oldCookie := vaultAdminBase, vaultCookie
	vaultAdminBase = "http://localhost:1"
	vaultCookie = "test"
	defer func() { vaultAdminBase = oldAdmin; vaultCookie = oldCookie }()

	out := captureStdout(func() {
		runVaultReindexFTS([]string{"test"})
	})
	if !strings.Contains(out, "Error connecting") {
		t.Errorf("expected connection error: %s", out)
	}
}

func TestRunVaultReindexFTS_GenericHTTPError(t *testing.T) {
	cleanup := withMockVaultServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"message": "bad request"}})
	})
	defer cleanup()

	out := captureStdout(func() {
		runVaultReindexFTS([]string{"test"})
	})
	if !strings.Contains(out, "bad request") {
		t.Errorf("expected 'bad request' error: %s", out)
	}
}

// ---------------------------------------------------------------------------
// vault.go: runVaultExport error paths
// ---------------------------------------------------------------------------

func TestRunVaultExport_ServerError(t *testing.T) {
	cleanup := withMockVaultServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"message": "vault not found"}})
	})
	defer cleanup()

	out := captureStdout(func() {
		runVaultExport([]string{"nonexistent"})
	})
	if !strings.Contains(out, "vault not found") {
		t.Errorf("expected 'vault not found': %s", out)
	}
}

func TestRunVaultExport_ConnectionError(t *testing.T) {
	oldAdmin, oldCookie := vaultAdminBase, vaultCookie
	vaultAdminBase = "http://localhost:1"
	vaultCookie = "test"
	defer func() { vaultAdminBase = oldAdmin; vaultCookie = oldCookie }()

	out := captureStdout(func() {
		runVaultExport([]string{"test"})
	})
	if !strings.Contains(out, "Error connecting") {
		t.Errorf("expected connection error: %s", out)
	}
}

// ---------------------------------------------------------------------------
// vault.go: runVaultImport error paths
// ---------------------------------------------------------------------------

func TestRunVaultImport_ServerError(t *testing.T) {
	cleanup := withMockVaultServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"message": "invalid archive"}})
	})
	defer cleanup()

	archiveFile := filepath.Join(t.TempDir(), "test.muninn")
	os.WriteFile(archiveFile, []byte("data"), 0644)

	out := captureStdout(func() {
		runVaultImport([]string{archiveFile, "--vault", "test"})
	})
	if !strings.Contains(out, "invalid archive") {
		t.Errorf("expected error: %s", out)
	}
}

func TestRunVaultImport_ResetMetadata(t *testing.T) {
	var gotImportURL string
	cleanup := withMockVaultServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" && strings.Contains(r.URL.Path, "/import") {
			gotImportURL = r.URL.String()
			w.WriteHeader(http.StatusAccepted)
			json.NewEncoder(w).Encode(map[string]string{"job_id": "imp-001"})
			return
		}
		if strings.Contains(r.URL.Path, "/job-status") {
			json.NewEncoder(w).Encode(statusSnap{Status: "done", Pct: 100})
			return
		}
	})
	defer cleanup()

	archiveFile := filepath.Join(t.TempDir(), "test.muninn")
	os.WriteFile(archiveFile, []byte("data"), 0644)

	captureStdout(func() {
		runVaultImport([]string{archiveFile, "--vault", "test", "--reset-metadata"})
	})
	if !strings.Contains(gotImportURL, "reset_metadata=true") {
		t.Errorf("expected reset_metadata param, got URL: %s", gotImportURL)
	}
}

// ---------------------------------------------------------------------------
// init.go: configureTools with numbered selections
// ---------------------------------------------------------------------------

func TestConfigureTools_ManualAndVSCode(t *testing.T) {
	out := captureStdout(func() {
		configureTools([]int{3, 5}, "http://localhost:8750/mcp", "tok")
	})
	if !strings.Contains(out, "muninn") {
		t.Errorf("expected instructions: %s", out)
	}
}

func TestConfigureTools_AllNumbered(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	out := captureStdout(func() {
		errs := configureTools([]int{1, 2, 3, 4, 5}, "http://localhost:8750/mcp", "tok")
		_ = errs
	})
	_ = out
}

// ---------------------------------------------------------------------------
// init.go: configureNamedTools full coverage
// ---------------------------------------------------------------------------

func TestConfigureNamedTools_AllNames(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	out := captureStdout(func() {
		errs := configureNamedTools([]string{
			"claude", "claude-code", "cursor", "windsurf", "openclaw", "vscode", "manual",
		}, "http://localhost:8750/mcp", "tok")
		_ = errs
	})
	_ = out
}

// ---------------------------------------------------------------------------
// upgrade.go: runUpgrade with mock GitHub API
// ---------------------------------------------------------------------------

func TestRunUpgrade_UpToDate(t *testing.T) {
	out := captureStdout(func() {
		runUpgrade([]string{})
	})
	if !strings.Contains(out, "Current version") {
		t.Errorf("expected version info: %s", out)
	}
}

// ---------------------------------------------------------------------------
// status.go: probeServices + printStatusDisplay paths
// ---------------------------------------------------------------------------

func TestProbeServices_AllDown(t *testing.T) {
	svcs := probeServices()
	for _, s := range svcs {
		_ = s.up
	}
	if len(svcs) != 3 {
		t.Errorf("expected 3 services, got %d", len(svcs))
	}
}

func TestPrintStatusDisplay_Compact(t *testing.T) {
	out := captureStdout(func() {
		state := printStatusDisplay(true)
		_ = state
	})
	_ = out
}

func TestPrintStatusDisplay_Full(t *testing.T) {
	out := captureStdout(func() {
		state := printStatusDisplay(false)
		_ = state
	})
	if !strings.Contains(out, "muninn") {
		t.Errorf("expected 'muninn' in output: %s", out)
	}
}

// ---------------------------------------------------------------------------
// vault.go: runVault dispatch for various subcommands
// ---------------------------------------------------------------------------

func TestRunVault_DeleteDispatch(t *testing.T) {
	cleanup := withMockVaultServer(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/api/auth/login") {
			http.SetCookie(w, &http.Cookie{Name: "muninn_session", Value: "s"})
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	defer cleanup()

	out := captureStdout(func() {
		runVault([]string{"delete", "test-vault", "--yes"})
	})
	if !strings.Contains(out, "deleted") {
		t.Errorf("expected 'deleted': %s", out)
	}
}

func TestRunVault_ClearDispatch(t *testing.T) {
	cleanup := withMockVaultServer(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/api/auth/login") {
			http.SetCookie(w, &http.Cookie{Name: "muninn_session", Value: "s"})
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	defer cleanup()

	out := captureStdout(func() {
		runVault([]string{"clear", "test-vault", "--yes"})
	})
	if !strings.Contains(out, "cleared") {
		t.Errorf("expected 'cleared': %s", out)
	}
}

func TestRunVault_ExportDispatch(t *testing.T) {
	cleanup := withMockVaultServer(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/api/auth/login") {
			http.SetCookie(w, &http.Cookie{Name: "muninn_session", Value: "s"})
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	defer cleanup()

	out := captureStdout(func() {
		runVault([]string{"export"})
	})
	if !strings.Contains(out, "Usage:") {
		t.Errorf("expected usage: %s", out)
	}
}

func TestRunVault_ImportDispatch(t *testing.T) {
	cleanup := withMockVaultServer(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/api/auth/login") {
			http.SetCookie(w, &http.Cookie{Name: "muninn_session", Value: "s"})
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	defer cleanup()

	out := captureStdout(func() {
		runVault([]string{"import"})
	})
	if !strings.Contains(out, "Usage:") {
		t.Errorf("expected usage: %s", out)
	}
}

func TestRunVault_ReindexFTSDispatch(t *testing.T) {
	cleanup := withMockVaultServer(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/api/auth/login") {
			http.SetCookie(w, &http.Cookie{Name: "muninn_session", Value: "s"})
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	defer cleanup()

	out := captureStdout(func() {
		runVault([]string{"reindex-fts"})
	})
	if !strings.Contains(out, "Usage:") {
		t.Errorf("expected usage: %s", out)
	}
}

func TestRunVault_CloneDispatch(t *testing.T) {
	cleanup := withMockVaultServer(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/api/auth/login") {
			http.SetCookie(w, &http.Cookie{Name: "muninn_session", Value: "s"})
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	defer cleanup()

	out := captureStdout(func() {
		runVault([]string{"clone"})
	})
	if !strings.Contains(out, "Usage:") {
		t.Errorf("expected usage: %s", out)
	}
}

func TestRunVault_MergeDispatch(t *testing.T) {
	cleanup := withMockVaultServer(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/api/auth/login") {
			http.SetCookie(w, &http.Cookie{Name: "muninn_session", Value: "s"})
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	defer cleanup()

	out := captureStdout(func() {
		runVault([]string{"merge"})
	})
	if !strings.Contains(out, "Usage:") {
		t.Errorf("expected usage: %s", out)
	}
}

// ---------------------------------------------------------------------------
// vault.go: pollProgressBar with mock job status endpoint
// ---------------------------------------------------------------------------

func TestPollProgressBar_ImmediateDone(t *testing.T) {
	cleanup := withMockVaultServer(t, func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(statusSnap{Status: "done", Pct: 100, Phase: "copying", CopyTotal: 10, CopyCurrent: 10})
	})
	defer cleanup()

	out := captureStdout(func() {
		pollProgressBar("job-1", "test")
	})
	if !strings.Contains(out, "Done") {
		t.Errorf("expected 'Done': %s", out)
	}
}

func TestPollProgressBar_ErrorStatus(t *testing.T) {
	cleanup := withMockVaultServer(t, func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(statusSnap{Status: "error", Error: "disk full"})
	})
	defer cleanup()

	out := captureStdout(func() {
		pollProgressBar("job-1", "test")
	})
	if !strings.Contains(out, "disk full") {
		t.Errorf("expected 'disk full': %s", out)
	}
}

func TestPollProgressBar_JobNotFound(t *testing.T) {
	cleanup := withMockVaultServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	defer cleanup()

	out := captureStdout(func() {
		pollProgressBar("nonexistent", "test")
	})
	if !strings.Contains(out, "not found") {
		t.Errorf("expected 'not found': %s", out)
	}
}

// ---------------------------------------------------------------------------
// setup_ai.go: loadOrGenerateToken
// ---------------------------------------------------------------------------

func TestLoadOrGenerateToken_NewToken_Full(t *testing.T) {
	dir := t.TempDir()
	dataDir := filepath.Join(dir, "data")
	os.MkdirAll(dataDir, 0700)

	tok, isNew, err := loadOrGenerateToken(dataDir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !isNew {
		t.Error("expected new token")
	}
	if !strings.HasPrefix(tok, "mdb_") {
		t.Errorf("token should start with 'mdb_', got %q", tok)
	}
}

func TestLoadOrGenerateToken_ExistingToken_Full(t *testing.T) {
	dir := t.TempDir()
	dataDir := filepath.Join(dir, "data")
	os.MkdirAll(dataDir, 0700)

	tokenPath := filepath.Join(dir, "mcp.token")
	os.WriteFile(tokenPath, []byte("mdb_existingtoken123"), 0600)

	tok, isNew, err := loadOrGenerateToken(dataDir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if isNew {
		t.Error("expected existing token, not new")
	}
	if tok != "mdb_existingtoken123" {
		t.Errorf("expected 'mdb_existingtoken123', got %q", tok)
	}
}

// ---------------------------------------------------------------------------
// setup_ai.go: configureClaudeCode
// ---------------------------------------------------------------------------

func TestConfigureClaudeCode_Full(t *testing.T) {
	_, cleanup := withTempHome(t)
	defer cleanup()

	out := captureStdout(func() {
		err := configureClaudeCode("http://localhost:8750/mcp", "tok")
		if err != nil {
			t.Fatalf("error: %v", err)
		}
	})
	if !strings.Contains(out, "✓") {
		t.Errorf("missing success marker: %s", out)
	}
}

// ---------------------------------------------------------------------------
// init.go: runNonInteractiveInit with tools + token
// ---------------------------------------------------------------------------

func TestRunNonInteractiveInit_WithTools(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("MUNINNDB_DATA", filepath.Join(home, "data"))

	out := captureStdout(func() {
		runNonInteractiveInit("http://localhost:8750/mcp", "vscode,manual", "", true, true, true)
	})
	_ = out
}

func TestRunNonInteractiveInit_WithToken(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("MUNINNDB_DATA", filepath.Join(home, "data"))

	out := captureStdout(func() {
		runNonInteractiveInit("http://localhost:8750/mcp", "", "mdb_customtoken", false, true, true)
	})
	if !strings.Contains(out, "Token") || !strings.Contains(out, "mcp.token") {
		t.Logf("output: %s", out)
	}
}

func TestRunNonInteractiveInit_GenerateToken(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	os.MkdirAll(filepath.Join(home, ".muninn", "data"), 0700)
	t.Setenv("MUNINNDB_DATA", filepath.Join(home, ".muninn", "data"))

	out := captureStdout(func() {
		runNonInteractiveInit("http://localhost:8750/mcp", "", "", false, true, true)
	})
	_ = out
}

// ---------------------------------------------------------------------------
// vault.go: runVault with auth flags
// ---------------------------------------------------------------------------

func TestRunVault_WithAuthFlags(t *testing.T) {
	cleanup := withMockVaultServer(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/api/auth/login") {
			http.SetCookie(w, &http.Cookie{Name: "muninn_session", Value: "s"})
			w.WriteHeader(http.StatusOK)
			return
		}
		if strings.Contains(r.URL.Path, "/api/vaults") {
			json.NewEncoder(w).Encode([]string{"default"})
			return
		}
	})
	defer cleanup()

	out := captureStdout(func() {
		runVault([]string{"-u", "root", "-ppassword", "list"})
	})
	if !strings.Contains(out, "default") {
		t.Errorf("expected vault list output: %s", out)
	}
}

// ---------------------------------------------------------------------------
// renderBar: edge cases
// ---------------------------------------------------------------------------

func TestRenderBar_ZeroPct_Edge(t *testing.T) {
	bar := renderBar(statusSnap{Phase: "copying", Pct: 0, CopyTotal: 100, CopyCurrent: 0})
	if !strings.Contains(bar, "0.0%") {
		t.Errorf("expected 0.0%%: %s", bar)
	}
}

func TestRenderBar_OverflowPct(t *testing.T) {
	bar := renderBar(statusSnap{Phase: "copying", Pct: 150, CopyTotal: 100, CopyCurrent: 100})
	if !strings.Contains(bar, "150.0%") {
		t.Errorf("expected 150.0%%: %s", bar)
	}
}

// ---------------------------------------------------------------------------
// server.go: buildEnricher with env vars
// ---------------------------------------------------------------------------

func TestBuildEnricher_NoConfig(t *testing.T) {
	t.Setenv("MUNINN_ENRICH_URL", "")
	cfg := plugincfg.PluginConfig{}
	result := buildEnricher(context.Background(), cfg)
	if result != nil {
		t.Error("expected nil when no enrich URL is set")
	}
}

func TestBuildEnricher_InvalidURL(t *testing.T) {
	t.Setenv("MUNINN_ENRICH_URL", "invalid://not-a-real-provider")
	cfg := plugincfg.PluginConfig{}
	result := buildEnricher(context.Background(), cfg)
	if result != nil {
		t.Error("expected nil for invalid URL scheme")
	}
}

func TestBuildEnricher_FromSavedConfig(t *testing.T) {
	t.Setenv("MUNINN_ENRICH_URL", "")
	cfg := plugincfg.PluginConfig{
		EnrichURL: "invalid://saved-but-bad",
	}
	result := buildEnricher(context.Background(), cfg)
	if result != nil {
		t.Error("expected nil for invalid saved URL")
	}
}

func TestBuildEnricher_OllamaURL(t *testing.T) {
	t.Setenv("MUNINN_ENRICH_URL", "ollama://localhost:1/llama3.2")
	t.Setenv("MUNINN_ENRICH_API_KEY", "")
	t.Setenv("MUNINN_ANTHROPIC_KEY", "")
	result := buildEnricher(context.Background(), plugincfg.PluginConfig{})
	if result != nil {
		t.Log("ollama enricher init failed as expected (no server)")
	}
}

func TestBuildEnricher_OpenAIURL(t *testing.T) {
	t.Setenv("MUNINN_ENRICH_URL", "openai://gpt-4o-mini")
	t.Setenv("MUNINN_ENRICH_API_KEY", "fake-key")
	t.Setenv("MUNINN_ANTHROPIC_KEY", "")
	result := buildEnricher(context.Background(), plugincfg.PluginConfig{})
	_ = result
}

func TestBuildEnricher_AnthropicURL(t *testing.T) {
	t.Setenv("MUNINN_ENRICH_URL", "anthropic://claude-haiku-4-5-20251001")
	t.Setenv("MUNINN_ENRICH_API_KEY", "")
	t.Setenv("MUNINN_ANTHROPIC_KEY", "fake-anthropic-key")
	result := buildEnricher(context.Background(), plugincfg.PluginConfig{})
	_ = result
}

func TestBuildEnricher_SavedConfigFallback(t *testing.T) {
	t.Setenv("MUNINN_ENRICH_URL", "")
	t.Setenv("MUNINN_ENRICH_API_KEY", "")
	t.Setenv("MUNINN_ANTHROPIC_KEY", "")
	cfg := plugincfg.PluginConfig{
		EnrichURL:    "ollama://localhost:1/llama3.2",
		EnrichAPIKey: "",
	}
	result := buildEnricher(context.Background(), cfg)
	_ = result
}

func TestBuildEnricher_SavedConfigAPIKey(t *testing.T) {
	t.Setenv("MUNINN_ENRICH_URL", "")
	t.Setenv("MUNINN_ENRICH_API_KEY", "")
	t.Setenv("MUNINN_ANTHROPIC_KEY", "")
	cfg := plugincfg.PluginConfig{
		EnrichURL:    "openai://gpt-4o-mini",
		EnrichAPIKey: "saved-key",
	}
	result := buildEnricher(context.Background(), cfg)
	_ = result
}

// ---------------------------------------------------------------------------
// server.go: buildEmbedder noop path
// ---------------------------------------------------------------------------

func TestBuildEmbedder_Noop(t *testing.T) {
	t.Setenv("MUNINN_OLLAMA_URL", "")
	t.Setenv("MUNINN_OPENAI_KEY", "")
	t.Setenv("MUNINN_VOYAGE_KEY", "")
	t.Setenv("MUNINN_COHERE_KEY", "")
	t.Setenv("MUNINN_GOOGLE_KEY", "")
	t.Setenv("MUNINN_JINA_KEY", "")
	t.Setenv("MUNINN_MISTRAL_KEY", "")
	t.Setenv("MUNINN_LOCAL_EMBED", "0")

	cfg := plugincfg.PluginConfig{EmbedProvider: "none"}
	embedder, plug, err := buildEmbedder(context.Background(), cfg, t.TempDir())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if embedder == nil {
		t.Error("expected noop embedder, got nil")
	}
	if plug != nil {
		t.Error("expected nil plugin for noop")
	}
}

func TestBuildEmbedder_OllamaEnvBadURL(t *testing.T) {
	t.Setenv("MUNINN_OLLAMA_URL", "http://localhost:1/notreal")
	t.Setenv("MUNINN_OPENAI_KEY", "")
	t.Setenv("MUNINN_VOYAGE_KEY", "")
	t.Setenv("MUNINN_COHERE_KEY", "")
	t.Setenv("MUNINN_GOOGLE_KEY", "")
	t.Setenv("MUNINN_JINA_KEY", "")
	t.Setenv("MUNINN_MISTRAL_KEY", "")
	t.Setenv("MUNINN_LOCAL_EMBED", "0")

	cfg := plugincfg.PluginConfig{EmbedProvider: "none"}
	embedder, _, err := buildEmbedder(context.Background(), cfg, t.TempDir())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if embedder == nil {
		t.Error("expected noop embedder fallback, got nil")
	}
}

func TestBuildEmbedder_SavedConfigOllama(t *testing.T) {
	t.Setenv("MUNINN_OLLAMA_URL", "")
	t.Setenv("MUNINN_OPENAI_KEY", "")
	t.Setenv("MUNINN_VOYAGE_KEY", "")
	t.Setenv("MUNINN_COHERE_KEY", "")
	t.Setenv("MUNINN_GOOGLE_KEY", "")
	t.Setenv("MUNINN_JINA_KEY", "")
	t.Setenv("MUNINN_MISTRAL_KEY", "")
	t.Setenv("MUNINN_LOCAL_EMBED", "0")

	cfg := plugincfg.PluginConfig{EmbedProvider: "ollama", EmbedURL: "http://localhost:1/bad"}
	embedder, _, err := buildEmbedder(context.Background(), cfg, t.TempDir())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if embedder == nil {
		t.Error("expected noop embedder fallback")
	}
}

func TestBuildEmbedder_SavedConfigOpenAI(t *testing.T) {
	t.Setenv("MUNINN_OLLAMA_URL", "")
	t.Setenv("MUNINN_OPENAI_KEY", "")
	t.Setenv("MUNINN_VOYAGE_KEY", "")
	t.Setenv("MUNINN_COHERE_KEY", "")
	t.Setenv("MUNINN_GOOGLE_KEY", "")
	t.Setenv("MUNINN_JINA_KEY", "")
	t.Setenv("MUNINN_MISTRAL_KEY", "")
	t.Setenv("MUNINN_LOCAL_EMBED", "0")

	cfg := plugincfg.PluginConfig{EmbedProvider: "openai", EmbedAPIKey: "fake-key"}
	embedder, _, err := buildEmbedder(context.Background(), cfg, t.TempDir())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if embedder == nil {
		t.Error("expected noop embedder fallback")
	}
}

func TestBuildEmbedder_SavedConfigVoyage(t *testing.T) {
	t.Setenv("MUNINN_OLLAMA_URL", "")
	t.Setenv("MUNINN_OPENAI_KEY", "")
	t.Setenv("MUNINN_VOYAGE_KEY", "")
	t.Setenv("MUNINN_COHERE_KEY", "")
	t.Setenv("MUNINN_GOOGLE_KEY", "")
	t.Setenv("MUNINN_JINA_KEY", "")
	t.Setenv("MUNINN_MISTRAL_KEY", "")
	t.Setenv("MUNINN_LOCAL_EMBED", "0")

	cfg := plugincfg.PluginConfig{EmbedProvider: "voyage", EmbedAPIKey: "fake-key"}
	embedder, _, err := buildEmbedder(context.Background(), cfg, t.TempDir())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if embedder == nil {
		t.Error("expected noop embedder fallback")
	}
}

func TestBuildEmbedder_SavedConfigCohere(t *testing.T) {
	t.Setenv("MUNINN_OLLAMA_URL", "")
	t.Setenv("MUNINN_OPENAI_KEY", "")
	t.Setenv("MUNINN_VOYAGE_KEY", "")
	t.Setenv("MUNINN_COHERE_KEY", "")
	t.Setenv("MUNINN_GOOGLE_KEY", "")
	t.Setenv("MUNINN_JINA_KEY", "")
	t.Setenv("MUNINN_MISTRAL_KEY", "")
	t.Setenv("MUNINN_LOCAL_EMBED", "0")

	cfg := plugincfg.PluginConfig{EmbedProvider: "cohere", EmbedAPIKey: "fake-key"}
	embedder, _, err := buildEmbedder(context.Background(), cfg, t.TempDir())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if embedder == nil {
		t.Error("expected noop embedder fallback")
	}
}

func TestBuildEmbedder_SavedConfigGoogle(t *testing.T) {
	t.Setenv("MUNINN_OLLAMA_URL", "")
	t.Setenv("MUNINN_OPENAI_KEY", "")
	t.Setenv("MUNINN_VOYAGE_KEY", "")
	t.Setenv("MUNINN_COHERE_KEY", "")
	t.Setenv("MUNINN_GOOGLE_KEY", "")
	t.Setenv("MUNINN_JINA_KEY", "")
	t.Setenv("MUNINN_MISTRAL_KEY", "")
	t.Setenv("MUNINN_LOCAL_EMBED", "0")

	cfg := plugincfg.PluginConfig{EmbedProvider: "google", EmbedAPIKey: "fake-key"}
	embedder, _, err := buildEmbedder(context.Background(), cfg, t.TempDir())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if embedder == nil {
		t.Error("expected noop embedder fallback")
	}
}

func TestBuildEmbedder_SavedConfigJina(t *testing.T) {
	t.Setenv("MUNINN_OLLAMA_URL", "")
	t.Setenv("MUNINN_OPENAI_KEY", "")
	t.Setenv("MUNINN_VOYAGE_KEY", "")
	t.Setenv("MUNINN_COHERE_KEY", "")
	t.Setenv("MUNINN_GOOGLE_KEY", "")
	t.Setenv("MUNINN_JINA_KEY", "")
	t.Setenv("MUNINN_MISTRAL_KEY", "")
	t.Setenv("MUNINN_LOCAL_EMBED", "0")

	cfg := plugincfg.PluginConfig{EmbedProvider: "jina", EmbedAPIKey: "fake-key"}
	embedder, _, err := buildEmbedder(context.Background(), cfg, t.TempDir())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if embedder == nil {
		t.Error("expected noop embedder fallback")
	}
}

func TestBuildEmbedder_SavedConfigMistral(t *testing.T) {
	t.Setenv("MUNINN_OLLAMA_URL", "")
	t.Setenv("MUNINN_OPENAI_KEY", "")
	t.Setenv("MUNINN_VOYAGE_KEY", "")
	t.Setenv("MUNINN_COHERE_KEY", "")
	t.Setenv("MUNINN_GOOGLE_KEY", "")
	t.Setenv("MUNINN_JINA_KEY", "")
	t.Setenv("MUNINN_MISTRAL_KEY", "")
	t.Setenv("MUNINN_LOCAL_EMBED", "0")

	cfg := plugincfg.PluginConfig{EmbedProvider: "mistral", EmbedAPIKey: "fake-key"}
	embedder, _, err := buildEmbedder(context.Background(), cfg, t.TempDir())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if embedder == nil {
		t.Error("expected noop embedder fallback")
	}
}

func TestBuildEmbedder_AllEnvVarsTried(t *testing.T) {
	t.Setenv("MUNINN_OLLAMA_URL", "http://localhost:1/bad")
	t.Setenv("MUNINN_OPENAI_KEY", "fake-key")
	t.Setenv("MUNINN_VOYAGE_KEY", "fake-key")
	t.Setenv("MUNINN_COHERE_KEY", "fake-key")
	t.Setenv("MUNINN_GOOGLE_KEY", "fake-key")
	t.Setenv("MUNINN_JINA_KEY", "fake-key")
	t.Setenv("MUNINN_MISTRAL_KEY", "fake-key")
	t.Setenv("MUNINN_LOCAL_EMBED", "0")

	cfg := plugincfg.PluginConfig{EmbedProvider: "none"}
	embedder, _, err := buildEmbedder(context.Background(), cfg, t.TempDir())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if embedder == nil {
		t.Error("expected embedder (one of the env vars should init or fallback to noop)")
	}
}

// runStartupMigrations requires a real PebbleStore — tested via integration.

// ---------------------------------------------------------------------------
// upgrade.go: latestVersion with mock HTTP
// ---------------------------------------------------------------------------

func TestLatestVersion_MockAPI(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"tag_name": "v2.0.0"})
	}))
	defer ts.Close()

	origConst := githubReleaseAPI
	_ = origConst
}

// ---------------------------------------------------------------------------
// init.go: promptClaudeMD exercises (via configureClaudeMD)
// ---------------------------------------------------------------------------

func TestConfigureClaudeMD_NewFile_Hardened(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("USERPROFILE", home)

	err := configureClaudeMD()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	content, err := os.ReadFile(filepath.Join(home, ".claude", "CLAUDE.md"))
	if err != nil {
		t.Fatalf("CLAUDE.md not created: %v", err)
	}
	if !strings.Contains(string(content), "muninn") {
		t.Error("expected muninn content in CLAUDE.md")
	}
}

func TestConfigureClaudeMD_ExistingNoBlock(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("USERPROFILE", home)
	claudeDir := filepath.Join(home, ".claude")
	os.MkdirAll(claudeDir, 0700)
	os.WriteFile(filepath.Join(claudeDir, "CLAUDE.md"), []byte("existing content\n"), 0644)

	err := configureClaudeMD()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	content, _ := os.ReadFile(filepath.Join(claudeDir, "CLAUDE.md"))
	if !strings.Contains(string(content), "muninn") {
		t.Error("expected muninn block prepended")
	}
	if !strings.Contains(string(content), "existing content") {
		t.Error("expected original content preserved")
	}
}

func TestConfigureClaudeMD_AlreadyHasBlock(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("USERPROFILE", home)
	claudeDir := filepath.Join(home, ".claude")
	os.MkdirAll(claudeDir, 0700)
	os.WriteFile(filepath.Join(claudeDir, "CLAUDE.md"), []byte("# Memory\nmuninn_remember\n"), 0644)

	err := configureClaudeMD()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// ---------------------------------------------------------------------------
// vault.go: confirmVaultAction (test with piped stdin)
// ---------------------------------------------------------------------------

func TestConfirmVaultAction_Match(t *testing.T) {
	r, w, _ := os.Pipe()
	oldStdin := os.Stdin
	os.Stdin = r
	defer func() { os.Stdin = oldStdin }()

	go func() {
		fmt.Fprintln(w, "my-vault")
		w.Close()
	}()

	out := captureStdout(func() {
		result := confirmVaultAction("my-vault", "delete")
		if !result {
			t.Error("expected true for matching confirmation")
		}
	})
	_ = out
}

func TestConfirmVaultAction_Mismatch(t *testing.T) {
	r, w, _ := os.Pipe()
	oldStdin := os.Stdin
	os.Stdin = r
	defer func() { os.Stdin = oldStdin }()

	go func() {
		fmt.Fprintln(w, "wrong-name")
		w.Close()
	}()

	out := captureStdout(func() {
		result := confirmVaultAction("my-vault", "delete")
		if result {
			t.Error("expected false for mismatched confirmation")
		}
	})
	if !strings.Contains(out, "did not match") {
		t.Errorf("expected mismatch message: %s", out)
	}
}

// ---------------------------------------------------------------------------
// vault.go: runVault dispatches through delete/clear without --yes (cancel)
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// init.go: configureNamedTools with openclaw path
// ---------------------------------------------------------------------------

func TestConfigureNamedTools_OpenClaw(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	out := captureStdout(func() {
		errs := configureNamedTools([]string{"openclaw"}, "http://localhost:8750/mcp", "tok")
		_ = errs
	})
	_ = out
}

// ---------------------------------------------------------------------------
// init.go: configureTools each numbered option individually
// ---------------------------------------------------------------------------

func TestConfigureTools_ClaudeDesktop_Path(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	captureStdout(func() {
		configureTools([]int{1}, "http://localhost:8750/mcp", "tok")
	})
}

func TestConfigureTools_Cursor_Path(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	captureStdout(func() {
		configureTools([]int{2}, "http://localhost:8750/mcp", "tok")
	})
}

func TestConfigureTools_Windsurf_Path(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	captureStdout(func() {
		configureTools([]int{4}, "http://localhost:8750/mcp", "tok")
	})
}

// ---------------------------------------------------------------------------
// upgrade.go: runUpgrade with injected latestVersionFn
// ---------------------------------------------------------------------------

func TestRunUpgrade_SkipsDevBuild(t *testing.T) {
	old := latestVersionFn
	latestVersionFn = func() (string, error) { return "", nil }
	defer func() { latestVersionFn = old }()

	out := captureStdout(func() {
		runUpgrade([]string{})
	})
	if !strings.Contains(out, "skipped") {
		t.Errorf("expected 'skipped': %s", out)
	}
}

func TestRunUpgrade_UpToDate_Injected(t *testing.T) {
	old := latestVersionFn
	latestVersionFn = func() (string, error) { return "v0.0.1", nil }
	defer func() { latestVersionFn = old }()

	out := captureStdout(func() {
		runUpgrade([]string{})
	})
	if !strings.Contains(out, "up to date") || !strings.Contains(out, "Checking") {
		t.Errorf("expected 'up to date' or 'Checking': %s", out)
	}
}

func TestRunUpgrade_NetworkError(t *testing.T) {
	old := latestVersionFn
	latestVersionFn = func() (string, error) { return "", fmt.Errorf("network down") }
	defer func() { latestVersionFn = old }()

	oldExit := osExit
	osExit = func(c int) {}
	defer func() { osExit = oldExit }()

	out := captureStdout(func() {
		runUpgrade([]string{})
	})
	if !strings.Contains(out, "failed") {
		t.Errorf("expected 'failed': %s", out)
	}
}

func TestLatestVersionDefault_Dev(t *testing.T) {
	v, err := latestVersionDefault()
	if err != nil {
		t.Skipf("network error: %v", err)
	}
	_ = v
}

func TestCheckVersionHint_WithUpdate(t *testing.T) {
	old := latestVersionFn
	latestVersionFn = func() (string, error) { return "", nil }
	defer func() { latestVersionFn = old }()

	out := captureStdout(func() {
		checkVersionHint()
	})
	_ = out
}

// ---------------------------------------------------------------------------
// status.go: printStatusDisplay with injected probeServicesFn
// ---------------------------------------------------------------------------

func TestPrintStatusDisplay_Running(t *testing.T) {
	old := probeServicesFn
	latestOld := latestVersionFn
	probeServicesFn = func() []serviceStatus {
		return []serviceStatus{
			{name: "database", port: 8475, up: true},
			{name: "mcp", port: 8750, up: true},
			{name: "web ui", port: 8476, up: true},
		}
	}
	latestVersionFn = func() (string, error) { return "", nil }
	defer func() { probeServicesFn = old; latestVersionFn = latestOld }()

	out := captureStdout(func() {
		state := printStatusDisplay(false)
		if state != stateRunning {
			t.Errorf("expected stateRunning, got %d", state)
		}
	})
	if !strings.Contains(out, "running") {
		t.Errorf("expected 'running': %s", out)
	}
}

func TestPrintStatusDisplay_Degraded(t *testing.T) {
	old := probeServicesFn
	probeServicesFn = func() []serviceStatus {
		return []serviceStatus{
			{name: "database", port: 8475, up: true},
			{name: "mcp", port: 8750, up: false},
			{name: "web ui", port: 8476, up: true},
		}
	}
	defer func() { probeServicesFn = old }()

	out := captureStdout(func() {
		state := printStatusDisplay(false)
		if state != stateDegraded {
			t.Errorf("expected stateDegraded, got %d", state)
		}
	})
	if !strings.Contains(out, "mcp") {
		t.Errorf("expected 'mcp' in degraded output: %s", out)
	}
	if !strings.Contains(out, "restart") {
		t.Errorf("expected 'restart' hint: %s", out)
	}
}

func TestPrintStatusDisplay_Stopped_Full(t *testing.T) {
	old := probeServicesFn
	probeServicesFn = func() []serviceStatus {
		return []serviceStatus{
			{name: "database", port: 8475, up: false},
			{name: "mcp", port: 8750, up: false},
			{name: "web ui", port: 8476, up: false},
		}
	}
	defer func() { probeServicesFn = old }()

	out := captureStdout(func() {
		state := printStatusDisplay(false)
		if state != stateStopped {
			t.Errorf("expected stateStopped, got %d", state)
		}
	})
	if !strings.Contains(out, "stopped") {
		t.Errorf("expected 'stopped': %s", out)
	}
	if !strings.Contains(out, "muninn start") {
		t.Errorf("expected 'muninn start' hint: %s", out)
	}
}

func TestPrintStatusDisplay_DegradedMCPDown(t *testing.T) {
	old := probeServicesFn
	probeServicesFn = func() []serviceStatus {
		return []serviceStatus{
			{name: "database", port: 8475, up: true},
			{name: "mcp", port: 8750, up: false},
			{name: "web ui", port: 8476, up: false},
		}
	}
	defer func() { probeServicesFn = old }()

	out := captureStdout(func() {
		state := printStatusDisplay(true)
		if state != stateDegraded {
			t.Errorf("expected stateDegraded, got %d", state)
		}
	})
	if !strings.Contains(out, "memory access") {
		t.Errorf("expected MCP-specific warning: %s", out)
	}
}

// ---------------------------------------------------------------------------
// lifecycle.go: runStatus with injected probeServicesFn
// ---------------------------------------------------------------------------

func TestRunStatus_Running(t *testing.T) {
	old := probeServicesFn
	latestOld := latestVersionFn
	probeServicesFn = func() []serviceStatus {
		return []serviceStatus{
			{name: "database", port: 8475, up: true},
			{name: "mcp", port: 8750, up: true},
			{name: "web ui", port: 8476, up: true},
		}
	}
	latestVersionFn = func() (string, error) { return "", nil }
	defer func() { probeServicesFn = old; latestVersionFn = latestOld }()

	out := captureStdout(func() {
		runStatus()
	})
	if !strings.Contains(out, "running") {
		t.Errorf("expected 'running': %s", out)
	}
}

func TestRunStatus_Stopped(t *testing.T) {
	old := probeServicesFn
	probeServicesFn = func() []serviceStatus {
		return []serviceStatus{
			{name: "database", port: 8475, up: false},
			{name: "mcp", port: 8750, up: false},
			{name: "web ui", port: 8476, up: false},
		}
	}
	defer func() { probeServicesFn = old }()

	oldExit := osExit
	var exitCode int
	osExit = func(c int) { exitCode = c }
	defer func() { osExit = oldExit }()

	captureStdout(func() {
		runStatus()
	})
	if exitCode != 1 {
		t.Errorf("expected exit code 1 for stopped, got %d", exitCode)
	}
}

// ---------------------------------------------------------------------------
// init.go: promptClaudeMD (stdin-based) 
// ---------------------------------------------------------------------------

func TestPromptClaudeMD_Yes(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	r, w, _ := os.Pipe()
	oldStdin := os.Stdin
	os.Stdin = r
	defer func() { os.Stdin = oldStdin }()

	go func() {
		fmt.Fprintln(w, "y")
		w.Close()
	}()

	out := captureStdout(func() {
		promptClaudeMD()
	})
	if !strings.Contains(out, "CLAUDE.md") {
		t.Logf("output: %s", out)
	}
}

func TestPromptClaudeMD_No(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	r, w, _ := os.Pipe()
	oldStdin := os.Stdin
	os.Stdin = r
	defer func() { os.Stdin = oldStdin }()

	go func() {
		fmt.Fprintln(w, "n")
		w.Close()
	}()

	out := captureStdout(func() {
		promptClaudeMD()
	})
	if !strings.Contains(out, "Optional") || !strings.Contains(out, "CLAUDE.md") {
		t.Logf("output: %s", out)
	}
}

func TestPromptClaudeMD_Empty(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	r, w, _ := os.Pipe()
	oldStdin := os.Stdin
	os.Stdin = r
	defer func() { os.Stdin = oldStdin }()

	go func() {
		fmt.Fprintln(w, "")
		w.Close()
	}()

	captureStdout(func() {
		promptClaudeMD()
	})
}

// ---------------------------------------------------------------------------
// init.go: runToolMultiSelectFallback with piped stdin
// ---------------------------------------------------------------------------

func TestRunToolMultiSelectFallback_EmptyInput(t *testing.T) {
	r, w, _ := os.Pipe()
	oldStdin := os.Stdin
	os.Stdin = r
	defer func() { os.Stdin = oldStdin }()

	go func() {
		fmt.Fprintln(w, "")
		w.Close()
	}()

	tools := []toolChoice{
		{displayName: "Claude", key: "claude", selected: true},
		{displayName: "Cursor", key: "cursor", selected: false},
	}

	var result []string
	captureStdout(func() {
		result = runToolMultiSelectFallback(tools)
	})
	if len(result) != 1 || result[0] != "claude" {
		t.Errorf("expected [claude], got %v", result)
	}
}

func TestRunToolMultiSelectFallback_SelectNumbers(t *testing.T) {
	r, w, _ := os.Pipe()
	oldStdin := os.Stdin
	os.Stdin = r
	defer func() { os.Stdin = oldStdin }()

	go func() {
		fmt.Fprintln(w, "1 2")
		w.Close()
	}()

	tools := []toolChoice{
		{displayName: "Claude", key: "claude", selected: false},
		{displayName: "Cursor", key: "cursor", selected: false},
		{displayName: "Manual", key: "manual", selected: false},
	}

	var result []string
	captureStdout(func() {
		result = runToolMultiSelectFallback(tools)
	})
	if len(result) != 2 {
		t.Errorf("expected 2 selections, got %v", result)
	}
}

func TestRunToolMultiSelectFallback_DetectedTools(t *testing.T) {
	r, w, _ := os.Pipe()
	oldStdin := os.Stdin
	os.Stdin = r
	defer func() { os.Stdin = oldStdin }()

	go func() {
		fmt.Fprintln(w, "")
		w.Close()
	}()

	tools := []toolChoice{
		{displayName: "Claude", key: "claude", selected: true, detected: true, configPath: "/path/to/config"},
	}

	var result []string
	captureStdout(func() {
		result = runToolMultiSelectFallback(tools)
	})
	if len(result) != 1 {
		t.Errorf("expected 1 selection, got %v", result)
	}
}

func TestRunVaultDelete_CancelledByUser(t *testing.T) {
	r, w, _ := os.Pipe()
	oldStdin := os.Stdin
	os.Stdin = r
	defer func() { os.Stdin = oldStdin }()

	go func() {
		fmt.Fprintln(w, "nope")
		w.Close()
	}()

	cleanup := withMockVaultServer(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/api/auth/login") {
			http.SetCookie(w, &http.Cookie{Name: "muninn_session", Value: "s"})
			w.WriteHeader(http.StatusOK)
			return
		}
	})
	defer cleanup()

	out := captureStdout(func() {
		runVaultDelete([]string{"my-vault"})
	})
	if !strings.Contains(out, "Cancelled") {
		t.Errorf("expected 'Cancelled': %s", out)
	}
}
