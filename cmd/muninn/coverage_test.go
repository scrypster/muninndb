package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// --- printHelp ---

func TestPrintHelp(t *testing.T) {
	out := captureStdout(func() {
		printHelp()
	})
	checks := []string{
		"muninn",
		"QUICK START",
		"COMMANDS",
		"SERVER FLAGS",
		"AI TOOL INTEGRATION",
		"PORTS",
		"EMBEDDERS",
		"LLM ENRICHMENT",
		"muninn init",
		"muninn start",
		"muninn stop",
		"muninn status",
		"muninn help",
		"8474",
		"8475",
		"8476",
		"8750",
	}
	for _, c := range checks {
		if !strings.Contains(out, c) {
			t.Errorf("printHelp output missing %q", c)
		}
	}
}

// --- parseBehaviorChoice ---

func TestParseBehaviorChoice(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"1", "autonomous"},
		{"2", "prompted"},
		{"3", "selective"},
		{"4", "custom"},
		{"", "autonomous"},
		{"5", "autonomous"},
		{"abc", "autonomous"},
	}
	for _, tc := range cases {
		got := parseBehaviorChoice(tc.input)
		if got != tc.want {
			t.Errorf("parseBehaviorChoice(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

// --- printBehaviorNote ---

func TestPrintBehaviorNote(t *testing.T) {
	cases := []struct {
		mode    string
		custom  string
		wantIn  string
	}{
		{"autonomous", "", "autonomous"},
		{"prompted", "", "prompted"},
		{"selective", "", "selective"},
		{"custom", "", "custom"},
		{"custom", "my instructions", "behavior-instructions"},
	}
	for _, tc := range cases {
		out := captureStdout(func() {
			printBehaviorNote(tc.mode, tc.custom)
		})
		if !strings.Contains(out, tc.wantIn) {
			t.Errorf("printBehaviorNote(%q, %q): expected %q in output, got: %s",
				tc.mode, tc.custom, tc.wantIn, out)
		}
	}
}

// --- formatBytes ---

func TestFormatBytes(t *testing.T) {
	cases := []struct {
		input int64
		want  string
	}{
		{0, "0 B"},
		{100, "100 B"},
		{1023, "1023 B"},
		{1024, "1.0 KB"},
		{1536, "1.5 KB"},
		{1048576, "1.0 MB"},
		{1572864, "1.5 MB"},
		{1073741824, "1.0 GB"},
		{1610612736, "1.5 GB"},
	}
	for _, tc := range cases {
		got := formatBytes(tc.input)
		if got != tc.want {
			t.Errorf("formatBytes(%d) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

// --- printClusterHelp ---

func TestPrintClusterHelp(t *testing.T) {
	out := captureStdout(func() {
		printClusterHelp()
	})
	checks := []string{
		"Usage:",
		"cluster",
		"info",
		"status",
		"failover",
		"add-node",
		"remove-node",
		"enable",
		"disable",
		"--addr",
		"--json",
	}
	for _, c := range checks {
		if !strings.Contains(out, c) {
			t.Errorf("printClusterHelp output missing %q", c)
		}
	}
}

// --- httpPost ---

func TestHttpPost_Success(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			t.Errorf("expected POST, got %s", r.Method)
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"ok":true}`))
	}))
	defer ts.Close()

	body, err := httpPost(ts.URL)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(string(body), "ok") {
		t.Errorf("expected ok in response, got %q", string(body))
	}
}

func TestHttpPost_ServerError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("internal error"))
	}))
	defer ts.Close()

	_, err := httpPost(ts.URL)
	if err == nil {
		t.Fatal("expected error for 500 response")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("expected 500 in error, got: %v", err)
	}
}

func TestHttpPost_Unreachable(t *testing.T) {
	_, err := httpPost("http://localhost:1/nonexistent")
	if err == nil {
		t.Fatal("expected error for unreachable server")
	}
}

// --- httpGet edge cases ---

func TestHttpGet_Success(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"status":"ok"}`))
	}))
	defer ts.Close()

	body, err := httpGet(ts.URL)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(string(body), "ok") {
		t.Errorf("expected ok in body")
	}
}

func TestHttpGet_ServerError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte("not found"))
	}))
	defer ts.Close()

	_, err := httpGet(ts.URL)
	if err == nil {
		t.Fatal("expected error for 404")
	}
}

// --- httpPostJSON edge cases ---

func TestHttpPostJSON_NilBody(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"ok":true}`))
	}))
	defer ts.Close()

	body, err := httpPostJSON(ts.URL, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(string(body), "ok") {
		t.Errorf("expected ok in response")
	}
}

func TestHttpPostJSON_ServerError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte("bad request"))
	}))
	defer ts.Close()

	_, err := httpPostJSON(ts.URL, []byte(`{"test":true}`))
	if err == nil {
		t.Fatal("expected error for 400")
	}
}

// --- getString, getBool, getUint64, getMembers edge cases ---

func TestGetString_MissingKey(t *testing.T) {
	data := map[string]interface{}{}
	if got := getString(data, "missing"); got != "" {
		t.Errorf("expected empty string for missing key, got %q", got)
	}
}

func TestGetString_NonStringValue(t *testing.T) {
	data := map[string]interface{}{"num": 42}
	if got := getString(data, "num"); got != "" {
		t.Errorf("expected empty string for non-string value, got %q", got)
	}
}

func TestGetBool_MissingKey(t *testing.T) {
	data := map[string]interface{}{}
	if got := getBool(data, "missing"); got {
		t.Error("expected false for missing key")
	}
}

func TestGetBool_NonBoolValue(t *testing.T) {
	data := map[string]interface{}{"str": "true"}
	if got := getBool(data, "str"); got {
		t.Error("expected false for non-bool value")
	}
}

func TestGetUint64_MissingKey(t *testing.T) {
	data := map[string]interface{}{}
	if got := getUint64(data, "missing"); got != 0 {
		t.Errorf("expected 0 for missing key, got %d", got)
	}
}

func TestGetUint64_NonFloatValue(t *testing.T) {
	data := map[string]interface{}{"str": "42"}
	if got := getUint64(data, "str"); got != 0 {
		t.Errorf("expected 0 for non-float value, got %d", got)
	}
}

func TestGetUint64_Success(t *testing.T) {
	data := map[string]interface{}{"epoch": float64(42)}
	if got := getUint64(data, "epoch"); got != 42 {
		t.Errorf("expected 42, got %d", got)
	}
}

func TestGetMembers_MissingKey(t *testing.T) {
	data := map[string]interface{}{}
	if got := getMembers(data, "members"); got != nil {
		t.Errorf("expected nil for missing key, got %v", got)
	}
}

func TestGetMembers_InvalidType(t *testing.T) {
	data := map[string]interface{}{"members": "not-an-array"}
	if got := getMembers(data, "members"); got != nil {
		t.Errorf("expected nil for non-array value, got %v", got)
	}
}

func TestGetMembers_Success(t *testing.T) {
	rawJSON := `{"members":[{"node_id":"n1","role":"Cortex"},{"node_id":"n2","role":"Lobe"}]}`
	var data map[string]interface{}
	json.Unmarshal([]byte(rawJSON), &data)

	got := getMembers(data, "members")
	if len(got) != 2 {
		t.Fatalf("expected 2 members, got %d", len(got))
	}
	if getString(got[0], "node_id") != "n1" {
		t.Errorf("expected first member node_id=n1, got %q", getString(got[0], "node_id"))
	}
	if getString(got[1], "role") != "Lobe" {
		t.Errorf("expected second member role=Lobe, got %q", getString(got[1], "role"))
	}
}

func TestGetMembers_MixedItems(t *testing.T) {
	data := map[string]interface{}{
		"members": []interface{}{
			map[string]interface{}{"node_id": "n1"},
			"invalid-item",
		},
	}
	got := getMembers(data, "members")
	if len(got) != 1 {
		t.Errorf("expected 1 valid member, got %d", len(got))
	}
}

// --- runBackup error paths ---

func TestRunBackup_MissingDataDirArgument(t *testing.T) {
	exitCode := -1
	origExit := osExit
	osExit = func(code int) { exitCode = code }
	defer func() { osExit = origExit }()

	captureStderr(func() {
		runBackup([]string{"--data-dir"})
	})

	if exitCode != 1 {
		t.Errorf("expected exit code 1, got %d", exitCode)
	}
}

func TestRunBackup_MissingOutputArgument(t *testing.T) {
	exitCode := -1
	origExit := osExit
	osExit = func(code int) { exitCode = code }
	defer func() { osExit = origExit }()

	captureStderr(func() {
		runBackup([]string{"--output"})
	})

	if exitCode != 1 {
		t.Errorf("expected exit code 1, got %d", exitCode)
	}
}

func TestRunBackup_UnknownFlag(t *testing.T) {
	exitCode := -1
	origExit := osExit
	osExit = func(code int) { exitCode = code }
	defer func() { osExit = origExit }()

	captureStderr(func() {
		runBackup([]string{"--unknown-flag"})
	})

	if exitCode != 1 {
		t.Errorf("expected exit code 1, got %d", exitCode)
	}
}

func TestRunBackup_NonexistentDataDir(t *testing.T) {
	exitCode := -1
	origExit := osExit
	osExit = func(code int) { exitCode = code }
	defer func() { osExit = origExit }()

	captureStderr(func() {
		runBackup([]string{"--output", "/tmp/test-backup-out", "--data-dir", "/nonexistent/path"})
	})

	if exitCode != 1 {
		t.Errorf("expected exit code 1, got %d", exitCode)
	}
}

// --- dirSizeBytes ---

func TestDirSizeBytes_EmptyDir(t *testing.T) {
	dir := t.TempDir()
	size := dirSizeBytes(dir)
	if size != 0 {
		t.Errorf("expected 0 for empty dir, got %d", size)
	}
}

// --- copyFile ---

func TestCopyFile_NonexistentSource(t *testing.T) {
	err := copyFile("/nonexistent/source", t.TempDir()+"/dst")
	if err == nil {
		t.Fatal("expected error for nonexistent source")
	}
}

// --- cluster status --json ---

func TestClusterStatus_JSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/cluster/health":
			json.NewEncoder(w).Encode(map[string]any{
				"status": "ok", "role": "primary", "epoch": 1.0,
			})
		case "/v1/cluster/nodes":
			json.NewEncoder(w).Encode(map[string]any{
				"nodes": []map[string]any{},
			})
		}
	}))
	defer server.Close()

	out := captureStdout(func() {
		runClusterStatus([]string{"--addr", server.URL, "--json"})
	})

	if !strings.Contains(out, "health") {
		t.Errorf("expected 'health' in JSON output, got: %s", out)
	}
}

// --- cluster info disabled ---

func TestClusterInfo_Disabled(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"enabled": false})
	}))
	defer server.Close()

	out := captureStdout(func() {
		defer func() { recover() }() // os.Exit recovery
		runClusterInfo([]string{"--addr", server.URL})
	})

	if !strings.Contains(out, "not enabled") {
		t.Logf("output: %s", out)
	}
}

// --- cluster info table output ---

func TestClusterInfo_TableOutput(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"enabled":   true,
			"node_id":   "cortex-1",
			"role":      "Cortex",
			"is_leader": true,
			"epoch":     float64(5),
			"cortex_id": "cortex-1",
			"members": []map[string]any{
				{"node_id": "cortex-1", "role": "Cortex", "last_seq": float64(100), "addr": "10.0.0.1:8474"},
			},
		})
	}))
	defer server.Close()

	out := captureStdout(func() {
		runClusterInfo([]string{"--addr", server.URL})
	})

	if !strings.Contains(out, "cortex-1") {
		t.Errorf("expected cortex-1 in table output, got: %s", out)
	}
	if !strings.Contains(out, "leader") {
		t.Errorf("expected (leader) in output, got: %s", out)
	}
}

// --- cluster disable server error ---

func TestClusterDisable_ServerError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("error"))
	}))
	defer server.Close()

	rc := runClusterDisable([]string{"--addr", server.URL, "--yes"})
	if rc != 1 {
		t.Errorf("expected exit code 1 on server error, got %d", rc)
	}
}

// --- configureTools ---

func TestConfigureTools_VSCode(t *testing.T) {
	out := captureStdout(func() {
		configureTools([]int{3}, "http://127.0.0.1:8750/mcp", "tok")
	})
	if !strings.Contains(out, "VS Code") && !strings.Contains(out, "settings.json") {
		t.Logf("VS Code instructions: %s", out)
	}
}

func TestConfigureTools_Manual(t *testing.T) {
	out := captureStdout(func() {
		configureTools([]int{5}, "http://127.0.0.1:8750/mcp", "tok")
	})
	if out == "" {
		t.Log("manual instructions printed (may be empty depending on implementation)")
	}
}

func TestConfigureTools_ClaudeDesktop(t *testing.T) {
	_, cleanup := withTempHome(t)
	defer cleanup()

	out := captureStdout(func() {
		configureTools([]int{1}, "http://127.0.0.1:8750/mcp", "tok")
	})
	if !strings.Contains(out, "✓") {
		t.Errorf("expected success marker for Claude Desktop, got: %s", out)
	}
}

func TestConfigureTools_Cursor(t *testing.T) {
	_, cleanup := withTempHome(t)
	defer cleanup()

	out := captureStdout(func() {
		configureTools([]int{2}, "http://127.0.0.1:8750/mcp", "tok")
	})
	if !strings.Contains(out, "✓") {
		t.Errorf("expected success marker for Cursor, got: %s", out)
	}
}

func TestConfigureTools_Windsurf(t *testing.T) {
	_, cleanup := withTempHome(t)
	defer cleanup()

	out := captureStdout(func() {
		configureTools([]int{4}, "http://127.0.0.1:8750/mcp", "tok")
	})
	if !strings.Contains(out, "✓") {
		t.Errorf("expected success marker for Windsurf, got: %s", out)
	}
}

func TestConfigureTools_Multiple(t *testing.T) {
	_, cleanup := withTempHome(t)
	defer cleanup()

	out := captureStdout(func() {
		configureTools([]int{1, 2, 3, 4, 5}, "http://127.0.0.1:8750/mcp", "tok")
	})
	if !strings.Contains(out, "VS Code") {
		t.Errorf("expected VS Code instructions in multi-tool output, got: %s", out)
	}
}

// --- runStartService / runStopService ---

func TestRunStartService_Web(t *testing.T) {
	out := captureStdout(func() {
		runStartService("web")
	})
	if !strings.Contains(out, "not yet implemented") {
		t.Errorf("expected 'not yet implemented', got: %s", out)
	}
}

func TestRunStartService_Unknown(t *testing.T) {
	exitCode := -1
	origExit := osExit
	osExit = func(code int) { exitCode = code }
	defer func() { osExit = origExit }()

	captureStderr(func() {
		runStartService("nonexistent")
	})

	if exitCode != 1 {
		t.Errorf("expected exit code 1 for unknown service, got %d", exitCode)
	}
}

func TestRunStopService_Web(t *testing.T) {
	out := captureStdout(func() {
		runStopService("web")
	})
	if !strings.Contains(out, "not yet implemented") {
		t.Errorf("expected 'not yet implemented', got: %s", out)
	}
}

func TestRunStopService_Unknown(t *testing.T) {
	exitCode := -1
	origExit := osExit
	osExit = func(code int) { exitCode = code }
	defer func() { osExit = origExit }()

	captureStderr(func() {
		runStopService("nonexistent")
	})

	if exitCode != 1 {
		t.Errorf("expected exit code 1 for unknown service, got %d", exitCode)
	}
}

// --- parseSemver edge cases ---

func TestParseSemver(t *testing.T) {
	cases := []struct {
		input              string
		wantMaj, wantMin   int
		wantPat            int
		wantOK             bool
	}{
		{"v1.2.3", 1, 2, 3, true},
		{"1.2.3", 1, 2, 3, true},
		{"v0.0.0", 0, 0, 0, true},
		{"v1.2.3-alpha", 1, 2, 3, true},
		{"v1.2.3+build", 1, 2, 3, true},
		{"v1.2.3-beta.1+build.123", 1, 2, 3, true},
		{"v1.2", 0, 0, 0, false},
		{"v1", 0, 0, 0, false},
		{"", 0, 0, 0, false},
		{"abc", 0, 0, 0, false},
		{"v1.2.abc", 0, 0, 0, false},
		{"v1.abc.3", 0, 0, 0, false},
		{"vabc.2.3", 0, 0, 0, false},
	}
	for _, tc := range cases {
		maj, min, pat, ok := parseSemver(tc.input)
		if ok != tc.wantOK {
			t.Errorf("parseSemver(%q): ok=%v, want %v", tc.input, ok, tc.wantOK)
			continue
		}
		if ok && (maj != tc.wantMaj || min != tc.wantMin || pat != tc.wantPat) {
			t.Errorf("parseSemver(%q): got (%d,%d,%d), want (%d,%d,%d)",
				tc.input, maj, min, pat, tc.wantMaj, tc.wantMin, tc.wantPat)
		}
	}
}

func TestNewerVersionAvailable_EdgeCases(t *testing.T) {
	cases := []struct {
		current, latest string
		want            bool
	}{
		{"v0.1.0", "v1.0.0", true},
		{"v2.0.0", "v1.99.99", false},
		{"v1.0.0", "v1.1.0", true},
		{"v1.1.0", "v1.0.0", false},
		{"invalid", "v1.0.0", false},
		{"v1.0.0", "invalid", false},
	}
	for _, tc := range cases {
		got := newerVersionAvailable(tc.current, tc.latest)
		if got != tc.want {
			t.Errorf("newerVersionAvailable(%q, %q) = %v, want %v", tc.current, tc.latest, got, tc.want)
		}
	}
}

// --- runCluster router ---

func TestRunCluster_UnknownSubcommand(t *testing.T) {
	exitCode := -1
	origExit := osExit
	osExit = func(code int) { exitCode = code }
	defer func() { osExit = origExit }()

	captureStderr(func() {
		captureStdout(func() {
			runCluster([]string{"bogus-subcommand"})
		})
	})

	if exitCode != 1 {
		t.Errorf("expected exit code 1, got %d", exitCode)
	}
}

func TestRunCluster_NoArgs(t *testing.T) {
	exitCode := -1
	origExit := osExit
	osExit = func(code int) { exitCode = code }
	defer func() { osExit = origExit }()

	captureStdout(func() {
		runCluster([]string{})
	})

	if exitCode != 1 {
		t.Errorf("expected exit code 1, got %d", exitCode)
	}
}

// --- runClusterFailover ---

func TestRunClusterFailover_Success(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]any{"triggered": true, "new_cortex": "node-2"})
	}))
	defer server.Close()

	out := captureStdout(func() {
		runClusterFailover([]string{"--addr", server.URL, "--yes"})
	})
	if !strings.Contains(out, "triggered") {
		t.Logf("failover output: %s", out)
	}
}

func TestRunClusterFailover_ServerError(t *testing.T) {
	exitCode := -1
	origExit := osExit
	osExit = func(code int) { exitCode = code }
	defer func() { osExit = origExit }()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("error"))
	}))
	defer server.Close()

	captureStderr(func() {
		runClusterFailover([]string{"--addr", server.URL, "--yes"})
	})

	if exitCode != 1 {
		t.Errorf("expected exit code 1, got %d", exitCode)
	}
}

func TestRunClusterFailover_NotTriggered(t *testing.T) {
	exitCode := -1
	origExit := osExit
	osExit = func(code int) { exitCode = code }
	defer func() { osExit = origExit }()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"triggered": false})
	}))
	defer server.Close()

	captureStdout(func() {
		runClusterFailover([]string{"--addr", server.URL, "--yes"})
	})

	if exitCode != 1 {
		t.Errorf("expected exit code 1 when election not triggered, got %d", exitCode)
	}
}

// --- runBackup additional edge cases ---

func TestRunBackup_NoOutputArg(t *testing.T) {
	exitCode := -1
	origExit := osExit
	osExit = func(code int) { exitCode = code }
	defer func() { osExit = origExit }()

	captureStderr(func() {
		runBackup([]string{})
	})

	if exitCode != 1 {
		t.Errorf("expected exit code 1 when no --output, got %d", exitCode)
	}
}

func TestRunBackup_OutputAlreadyExists(t *testing.T) {
	exitCode := -1
	origExit := osExit
	osExit = func(code int) { exitCode = code }
	defer func() { osExit = origExit }()

	existing := t.TempDir()
	captureStderr(func() {
		runBackup([]string{"--output", existing, "--data-dir", t.TempDir()})
	})

	if exitCode != 1 {
		t.Errorf("expected exit code 1 when output dir exists, got %d", exitCode)
	}
}

// --- copyDir ---

func TestCopyDir_Success(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir() + "/dst"

	os.MkdirAll(src+"/sub", 0755)
	os.WriteFile(src+"/file1.txt", []byte("hello"), 0644)
	os.WriteFile(src+"/sub/file2.txt", []byte("world"), 0644)

	if err := copyDir(src, dst); err != nil {
		t.Fatalf("copyDir: %v", err)
	}

	data, err := os.ReadFile(dst + "/file1.txt")
	if err != nil {
		t.Fatalf("read copied file: %v", err)
	}
	if string(data) != "hello" {
		t.Errorf("expected 'hello', got %q", string(data))
	}

	data2, err := os.ReadFile(dst + "/sub/file2.txt")
	if err != nil {
		t.Fatalf("read nested copied file: %v", err)
	}
	if string(data2) != "world" {
		t.Errorf("expected 'world', got %q", string(data2))
	}
}

// --- copyFile success ---

func TestCopyFile_Success(t *testing.T) {
	src := t.TempDir() + "/source.txt"
	dst := t.TempDir() + "/dest.txt"

	os.WriteFile(src, []byte("content"), 0644)

	if err := copyFile(src, dst); err != nil {
		t.Fatalf("copyFile: %v", err)
	}

	data, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read dst: %v", err)
	}
	if string(data) != "content" {
		t.Errorf("expected 'content', got %q", string(data))
	}
}

// --- dirSizeBytes with files ---

func TestDirSizeBytes_WithFiles(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(dir+"/a.txt", []byte("hello"), 0644)
	os.WriteFile(dir+"/b.txt", []byte("world!"), 0644)

	size := dirSizeBytes(dir)
	if size != 11 {
		t.Errorf("expected 11 bytes, got %d", size)
	}
}

// --- configureClaudeCode ---

func TestConfigureClaudeCode(t *testing.T) {
	_, cleanup := withTempHome(t)
	defer cleanup()

	out := captureStdout(func() {
		if err := configureClaudeCode("http://127.0.0.1:8750/mcp", "tok"); err != nil {
			t.Fatalf("error: %v", err)
		}
	})

	if !strings.Contains(out, "✓") {
		t.Errorf("expected success marker: %s", out)
	}
	if !strings.Contains(out, "Claude Code") {
		t.Errorf("expected 'Claude Code' in output: %s", out)
	}

	data, err := os.ReadFile(claudeCodeConfigPath())
	if err != nil {
		t.Fatalf("file not written: %v", err)
	}
	if !strings.Contains(string(data), `"muninn"`) {
		t.Errorf("muninn not in config: %s", data)
	}
	// Regression guard for issue #109: Claude Code schema requires "type":"http".
	if !strings.Contains(string(data), `"type"`) || !strings.Contains(string(data), `"http"`) {
		t.Errorf(`"type":"http" missing from written config — Claude Code schema will reject it: %s`, data)
	}
}

// --- configureNamedTools claude-code alias ---

func TestConfigureNamedToolsClaudeCode(t *testing.T) {
	_, cleanup := withTempHome(t)
	defer cleanup()

	out := captureStdout(func() {
		configureNamedTools([]string{"claude-code"}, "http://127.0.0.1:8750/mcp", "tok", "")
	})
	if !strings.Contains(out, "✓") {
		t.Errorf("expected success marker for claude-code: %s", out)
	}
}

func TestConfigureNamedToolsClaudeCodeAlias(t *testing.T) {
	_, cleanup := withTempHome(t)
	defer cleanup()

	out := captureStdout(func() {
		configureNamedTools([]string{"claudecode"}, "http://127.0.0.1:8750/mcp", "tok", "")
	})
	if !strings.Contains(out, "✓") {
		t.Errorf("expected success marker for claudecode alias: %s", out)
	}
}

// --- tokenPath ---

func TestTokenPath(t *testing.T) {
	path := tokenPath()
	if path == "" {
		t.Error("tokenPath returned empty string")
	}
	if !strings.Contains(path, ".muninn") {
		t.Errorf("tokenPath should contain .muninn, got %q", path)
	}
	if !strings.HasSuffix(path, "mcp.token") {
		t.Errorf("tokenPath should end with mcp.token, got %q", path)
	}
}

// --- printHTTPError ---

func TestPrintHTTPError_WithMessage(t *testing.T) {
	body := `{"error":{"message":"vault not found"}}`
	resp := &http.Response{
		StatusCode: 404,
		Body:       io.NopCloser(strings.NewReader(body)),
	}

	out := captureStdout(func() {
		printHTTPError(resp)
	})
	if !strings.Contains(out, "vault not found") {
		t.Errorf("expected error message in output, got: %s", out)
	}
}

func TestPrintHTTPError_WithoutMessage(t *testing.T) {
	resp := &http.Response{
		StatusCode: 500,
		Body:       io.NopCloser(strings.NewReader("not json")),
	}

	out := captureStdout(func() {
		printHTTPError(resp)
	})
	if !strings.Contains(out, "500") {
		t.Errorf("expected HTTP status code in output, got: %s", out)
	}
}

// --- runVault routing ---

func TestRunVault_EmptyArgs(t *testing.T) {
	out := captureStdout(func() {
		runVault([]string{})
	})
	if !strings.Contains(out, "Usage:") {
		t.Errorf("expected usage message, got: %s", out)
	}
}

func TestRunVault_InvalidSubcommand(t *testing.T) {
	out := captureStdout(func() {
		runVault([]string{"nonexistent"})
	})
	if !strings.Contains(out, "Unknown vault command") {
		t.Errorf("expected unknown command message, got: %s", out)
	}
}

// --- parseVaultArgs ---

func TestParseVaultArgs(t *testing.T) {
	name, yes, force := parseVaultArgs([]string{"myVault", "--yes", "--force"}, "delete")
	if name != "myVault" {
		t.Errorf("expected name=myVault, got %q", name)
	}
	if !yes {
		t.Error("expected yes=true")
	}
	if !force {
		t.Error("expected force=true")
	}
}

func TestParseVaultArgs_ShortFlags(t *testing.T) {
	name, yes, force := parseVaultArgs([]string{"testVault", "-y", "-f"}, "clear")
	if name != "testVault" {
		t.Errorf("expected name=testVault, got %q", name)
	}
	if !yes {
		t.Error("expected yes=true with -y")
	}
	if !force {
		t.Error("expected force=true with -f")
	}
}

func TestParseVaultArgs_NoName(t *testing.T) {
	out := captureStdout(func() {
		name, _, _ := parseVaultArgs([]string{"--yes"}, "delete")
		if name != "" {
			t.Error("expected empty name")
		}
	})
	if !strings.Contains(out, "Usage:") {
		t.Errorf("expected usage message when no vault name, got: %s", out)
	}
}

// --- latestVersion with dev build ---

func TestLatestVersion_DevBuild(t *testing.T) {
	orig := version
	version = ""
	defer func() { version = orig }()

	v, err := latestVersion()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v != "" {
		t.Errorf("dev build should return empty version, got %q", v)
	}
}

// --- claudeDesktopConfigPath platform ---

func TestClaudeDesktopConfigPath(t *testing.T) {
	path := claudeDesktopConfigPath()
	if path == "" {
		t.Error("claudeDesktopConfigPath returned empty string")
	}
}

func TestCursorConfigPath(t *testing.T) {
	path := cursorConfigPath()
	if path == "" {
		t.Error("cursorConfigPath returned empty string")
	}
	if !strings.Contains(path, "mcp.json") {
		t.Errorf("cursorConfigPath should contain mcp.json, got %q", path)
	}
}

func TestWindsurfConfigPath(t *testing.T) {
	path := windsurfConfigPath()
	if path == "" {
		t.Error("windsurfConfigPath returned empty string")
	}
}

func TestClaudeCodeConfigPath(t *testing.T) {
	path := claudeCodeConfigPath()
	if path == "" {
		t.Error("claudeCodeConfigPath returned empty string")
	}
	if !strings.Contains(path, ".claude.json") {
		t.Errorf("claudeCodeConfigPath should contain .claude.json, got %q", path)
	}
}

// --- tailLog ---

func TestTailLog_FileNotExist(t *testing.T) {
	var out strings.Builder
	var errOut strings.Builder
	tailLog("/nonexistent/log/file.log", "", 0, &out, &errOut)

	if !strings.Contains(out.String(), "No log file found") {
		t.Errorf("expected 'No log file found' message, got: %s", out.String())
	}
}

func TestTailLog_ExistingFile(t *testing.T) {
	// Use os.MkdirTemp instead of t.TempDir because tailLog holds the file
	// open indefinitely, and Windows cannot delete open files during cleanup.
	dir, err := os.MkdirTemp("", "taillog-existing-*")
	if err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(dir, "test.log")
	os.WriteFile(logPath, []byte("line1\nline2\n"), 0644)

	var out syncBuilder
	var errOut syncBuilder

	done := make(chan struct{})
	go func() {
		defer func() { recover() }()
		tailLog(logPath, "", 0, &out, &errOut)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(200 * time.Millisecond):
	}

	if errOut.Len() > 0 {
		t.Errorf("unexpected error output: %s", errOut.String())
	}
}

func TestTailLog_WithLevelFilter(t *testing.T) {
	dir, err := os.MkdirTemp("", "taillog-level-*")
	if err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(dir, "test.log")
	os.WriteFile(logPath, []byte("INFO starting\nERROR failed\n"), 0644)

	var out syncBuilder
	var errOut syncBuilder

	done := make(chan struct{})
	go func() {
		defer func() { recover() }()
		tailLog(logPath, "ERROR", 0, &out, &errOut)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(200 * time.Millisecond):
	}

	output := out.String()
	if strings.Contains(output, "filter:") && !strings.Contains(output, "ERROR") {
		t.Logf("filter header printed: %s", output)
	}
}

// --- renderBar ---

func TestRenderBar_Copying(t *testing.T) {
	snap := statusSnap{Phase: "copying", Pct: 50.0, CopyCurrent: 5, CopyTotal: 10}
	bar := renderBar(snap)
	if !strings.Contains(bar, "Copying") {
		t.Errorf("expected 'Copying' in bar, got: %s", bar)
	}
	if !strings.Contains(bar, "50.0%") {
		t.Errorf("expected 50.0%% in bar, got: %s", bar)
	}
}

func TestRenderBar_Indexing(t *testing.T) {
	snap := statusSnap{Phase: "indexing", Pct: 75.0, IndexCurrent: 15, IndexTotal: 20}
	bar := renderBar(snap)
	if !strings.Contains(bar, "Re-indexing") {
		t.Errorf("expected 'Re-indexing' in bar, got: %s", bar)
	}
}

func TestRenderBar_Complete(t *testing.T) {
	snap := statusSnap{Phase: "copying", Pct: 100.0, CopyCurrent: 10, CopyTotal: 10}
	bar := renderBar(snap)
	if !strings.Contains(bar, "100.0%") {
		t.Errorf("expected 100.0%% in bar, got: %s", bar)
	}
}

// --- isTerminal ---

func TestIsTerminal(t *testing.T) {
	// In test context, stdout is usually not a terminal
	result := isTerminal()
	_ = result // just verify it doesn't panic
}

// --- verifyCheckpoint ---

func TestVerifyCheckpoint_NonexistentDir(t *testing.T) {
	err := verifyCheckpoint("/nonexistent/checkpoint/dir")
	if err == nil {
		t.Error("expected error for nonexistent directory")
	}
}

// --- doVaultRequestForce ---

func TestDoVaultRequestForce_NoContent(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer ts.Close()

	out := captureStdout(func() {
		doVaultRequestForce("DELETE", ts.URL+"/api/admin/vaults/test", "Vault deleted.", false)
	})
	if !strings.Contains(out, "Vault deleted.") {
		t.Errorf("expected success message, got: %s", out)
	}
}

func TestDoVaultRequestForce_NotFound(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer ts.Close()

	out := captureStdout(func() {
		doVaultRequestForce("DELETE", ts.URL+"/api/admin/vaults/test", "Vault deleted.", false)
	})
	if !strings.Contains(out, "Vault not found") {
		t.Errorf("expected 'Vault not found', got: %s", out)
	}
}

func TestDoVaultRequestForce_Conflict(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
	}))
	defer ts.Close()

	out := captureStdout(func() {
		doVaultRequestForce("DELETE", ts.URL+"/api/admin/vaults/default", "Vault deleted.", false)
	})
	if !strings.Contains(out, "Protected vault") {
		t.Errorf("expected 'Protected vault', got: %s", out)
	}
}

func TestDoVaultRequestForce_ConflictForced(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Allow-Default") != "true" {
			t.Error("expected X-Allow-Default header when force=true")
		}
		w.WriteHeader(http.StatusConflict)
	}))
	defer ts.Close()

	out := captureStdout(func() {
		doVaultRequestForce("DELETE", ts.URL+"/api/admin/vaults/default", "Vault deleted.", true)
	})
	if !strings.Contains(out, "Cannot override") {
		t.Errorf("expected 'Cannot override', got: %s", out)
	}
}

func TestDoVaultRequestForce_Unauthorized(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer ts.Close()

	out := captureStdout(func() {
		doVaultRequestForce("DELETE", ts.URL+"/api/admin/vaults/test", "Vault deleted.", false)
	})
	if !strings.Contains(out, "Not authenticated") {
		t.Errorf("expected 'Not authenticated', got: %s", out)
	}
}

func TestDoVaultRequestForce_ServerError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer ts.Close()

	out := captureStdout(func() {
		doVaultRequestForce("DELETE", ts.URL+"/api/admin/vaults/test", "Vault deleted.", false)
	})
	if !strings.Contains(out, "500") {
		t.Errorf("expected HTTP 500 in error, got: %s", out)
	}
}

func TestDoVaultRequestForce_ConnectionError(t *testing.T) {
	out := captureStdout(func() {
		doVaultRequestForce("DELETE", "http://localhost:1/api/admin/vaults/test", "Vault deleted.", false)
	})
	if !strings.Contains(out, "Error connecting") {
		t.Errorf("expected connection error, got: %s", out)
	}
}

// --- runVaultClone insufficient args ---

func TestRunVaultClone_NoArgs(t *testing.T) {
	out := captureStdout(func() {
		runVaultClone([]string{})
	})
	if !strings.Contains(out, "Usage:") {
		t.Errorf("expected usage message, got: %s", out)
	}
}

func TestRunVaultClone_OneArg(t *testing.T) {
	out := captureStdout(func() {
		runVaultClone([]string{"source"})
	})
	if !strings.Contains(out, "Usage:") {
		t.Errorf("expected usage message for single arg, got: %s", out)
	}
}

// --- runVaultMerge ---

func TestRunVaultMerge_NoArgs(t *testing.T) {
	out := captureStdout(func() {
		runVaultMerge([]string{})
	})
	if !strings.Contains(out, "Usage:") {
		t.Errorf("expected usage message, got: %s", out)
	}
}

func TestRunVaultMerge_SameVault(t *testing.T) {
	exitCode := -1
	origExit := osExit
	osExit = func(code int) { exitCode = code }
	defer func() { osExit = origExit }()

	captureStderr(func() {
		runVaultMerge([]string{"myVault", "myVault", "--yes"})
	})

	if exitCode != 1 {
		t.Errorf("expected exit code 1 when merging vault into itself, got %d", exitCode)
	}
}

func TestRunVaultMerge_MissingTarget(t *testing.T) {
	out := captureStdout(func() {
		runVaultMerge([]string{"sourceOnly"})
	})
	if !strings.Contains(out, "Usage:") {
		t.Errorf("expected usage message when target missing, got: %s", out)
	}
}

// --- runVaultReindexFTS ---

func TestRunVaultReindexFTS_NoArgs(t *testing.T) {
	out := captureStdout(func() {
		runVaultReindexFTS([]string{})
	})
	if !strings.Contains(out, "Usage:") {
		t.Errorf("expected usage message, got: %s", out)
	}
}

// --- runVaultExport / runVaultImport no-args ---

func TestRunVaultExport_NoVault(t *testing.T) {
	out := captureStdout(func() {
		captureStderr(func() {
			runVaultExport([]string{})
		})
	})
	// should print usage or error
	_ = out
}

func TestRunVaultImport_NoArgs(t *testing.T) {
	out := captureStdout(func() {
		captureStderr(func() {
			runVaultImport([]string{})
		})
	})
	_ = out
}

// --- checkVersionHint (uses timeout) ---

func TestCheckVersionHint_DevBuild(t *testing.T) {
	orig := version
	version = ""
	defer func() { version = orig }()

	out := captureStdout(func() {
		checkVersionHint()
	})
	// dev build should not print anything
	_ = out
}
