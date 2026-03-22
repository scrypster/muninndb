//go:build integration

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// muninnBin is the path to the built binary, set by TestMain.
var muninnBin string

// TestMain builds the muninn binary once and runs all integration tests.
// It exits 0 without running tests if muninn is already listening on :8750,
// since these tests require exclusive use of that port.
func TestMain(m *testing.M) {
	// Guard: skip if something is already on the MCP port.
	if resp, err := http.Get("http://127.0.0.1:8750/mcp/health"); err == nil {
		resp.Body.Close()
		fmt.Fprintln(os.Stderr, "integration: muninn already running on :8750 — stop it first")
		os.Exit(0)
	}

	// Build the binary into a temp file.
	tmp, err := os.CreateTemp("", "muninn-integ-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "integration: create temp: %v\n", err)
		os.Exit(1)
	}
	tmp.Close()
	muninnBin = tmp.Name()

	out, err := exec.Command("go", "build", "-tags", "localassets", "-o", muninnBin, ".").CombinedOutput()
	if err != nil {
		fmt.Fprintf(os.Stderr, "integration: build failed: %v\n%s\n", err, out)
		os.Remove(muninnBin)
		os.Exit(1)
	}

	code := m.Run()
	os.Remove(muninnBin)
	os.Exit(code)
}

// muninnCmd creates a command with MUNINNDB_DATA set to the given directory.
func muninnCmd(dataDir string, args ...string) *exec.Cmd {
	cmd := exec.Command(muninnBin, args...)
	cmd.Env = append(os.Environ(), "MUNINNDB_DATA="+dataDir)
	return cmd
}

// waitForHealth polls 127.0.0.1:8750/mcp/health until 200 or timeout.
func waitForHealth(timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := http.Get("http://127.0.0.1:8750/mcp/health")
		if err == nil && resp.StatusCode == 200 {
			resp.Body.Close()
			return true
		}
		time.Sleep(200 * time.Millisecond)
	}
	return false
}

// waitForDead polls until 127.0.0.1:8750 refuses connections (port is free).
func waitForDead(timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := http.Get("http://127.0.0.1:8750/mcp/health")
		if err != nil {
			return true // connection refused — port is free
		}
		resp.Body.Close()
		time.Sleep(100 * time.Millisecond)
	}
	return false
}

// startDaemon runs `muninn start`, waits for the health endpoint, and
// registers a cleanup that stops the daemon and waits for the port to free.
func startDaemon(t *testing.T, dataDir string) {
	t.Helper()
	out, err := muninnCmd(dataDir, "start").CombinedOutput()
	if err != nil {
		t.Fatalf("muninn start: %v\n%s", err, out)
	}
	if !waitForHealth(10 * time.Second) {
		t.Fatal("daemon did not become healthy within 10s")
	}
	t.Cleanup(func() {
		muninnCmd(dataDir, "stop").Run() //nolint:errcheck — ignore if already stopped
		waitForDead(5 * time.Second)    // ensure port is free before the next test
	})
}

// TestHelpExitsZero verifies that `muninn help` exits 0 and produces output.
func TestHelpExitsZero(t *testing.T) {
	out, err := exec.Command(muninnBin, "help").CombinedOutput()
	if err != nil {
		t.Fatalf("muninn help: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "muninn") {
		t.Errorf("help output missing 'muninn':\n%s", out)
	}
}

// TestUnknownCommandExitsOne verifies that unknown subcommands exit non-zero
// and print a helpful error.
func TestUnknownCommandExitsOne(t *testing.T) {
	cmd := exec.Command(muninnBin, "boguscommand-xyz")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected non-zero exit for unknown command, got success\n%s", out)
	}
	if !strings.Contains(string(out), "Unknown command") {
		t.Errorf("expected 'Unknown command' in output:\n%s", out)
	}
}

// TestInitNoStart verifies that `muninn init --yes --no-start --no-token` exits
// 0 and does not start a daemon.
func TestInitNoStart(t *testing.T) {
	dataDir := t.TempDir()
	out, err := muninnCmd(dataDir, "init", "--yes", "--no-start", "--no-token").CombinedOutput()
	if err != nil {
		t.Fatalf("muninn init --yes --no-start --no-token: %v\n%s", err, out)
	}
	// Port 8750 must still be closed.
	resp, hErr := http.Get("http://127.0.0.1:8750/mcp/health")
	if hErr == nil {
		resp.Body.Close()
		t.Error("daemon should not be running after --no-start, but :8750 responded")
	}
}

// TestStartStop verifies the full start → health check → stop lifecycle.
func TestStartStop(t *testing.T) {
	dataDir := t.TempDir()
	startDaemon(t, dataDir)

	// PID file must exist while the daemon is running.
	pidPath := filepath.Join(dataDir, "muninn.pid")
	if _, err := os.Stat(pidPath); err != nil {
		t.Errorf("PID file missing at %s: %v", pidPath, err)
	}

	// `muninn status` must exit 0 and eventually report "running".
	// runStart only waits for the MCP port (8750); the REST API (8475) used
	// by the database probe may still be warming up. Poll until settled.
	var statusOut string
	statusDeadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(statusDeadline) {
		out, err := muninnCmd(dataDir, "status").CombinedOutput()
		if err != nil {
			t.Errorf("muninn status returned non-zero: %v\n%s", err, out)
			break
		}
		statusOut = string(out)
		if strings.Contains(statusOut, "running") {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if !strings.Contains(statusOut, "running") {
		t.Errorf("status never reached 'running' (last output):\n%s", statusOut)
	}
}

// TestRestartNoRace verifies that `muninn restart` cleanly stops the old
// process before starting a new one (no "address already in use" race).
func TestRestartNoRace(t *testing.T) {
	dataDir := t.TempDir()
	startDaemon(t, dataDir)

	// `muninn restart` should stop, then start, then return healthy.
	out, err := muninnCmd(dataDir, "restart").CombinedOutput()
	if err != nil {
		t.Fatalf("muninn restart: %v\n%s", err, out)
	}

	// Confirm the daemon is healthy after the restart.
	if !waitForHealth(10 * time.Second) {
		t.Fatal("daemon did not become healthy after restart")
	}
	// Cleanup registered by startDaemon will stop the restarted daemon.
}

// mcpTool sends a JSON-RPC 2.0 tools/call request to the local MCP server and
// returns the parsed text payload. token may be empty if auth is not configured.
func mcpTool(t *testing.T, token, toolName string, args map[string]any) map[string]any {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      toolName,
			"arguments": args,
		},
	})
	req, _ := http.NewRequest("POST", "http://127.0.0.1:8750/mcp", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("mcpTool %s: %v", toolName, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("mcpTool %s: HTTP %d", toolName, resp.StatusCode)
	}
	rawBody, _ := io.ReadAll(resp.Body)
	// result is {"content":[{"type":"text","text":"<json>"}]} (MCP textContent envelope)
	var rpcResp struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(bytes.NewReader(rawBody)).Decode(&rpcResp); err != nil {
		t.Fatalf("mcpTool %s: decode: %v\nbody: %s", toolName, err, rawBody)
	}
	if rpcResp.Error != nil {
		t.Fatalf("mcpTool %s: RPC error %d: %s", toolName, rpcResp.Error.Code, rpcResp.Error.Message)
	}
	if len(rpcResp.Result.Content) == 0 {
		t.Fatalf("mcpTool %s: empty result.content array\nbody: %s", toolName, rawBody)
	}
	var result map[string]any
	if err := json.Unmarshal([]byte(rpcResp.Result.Content[0].Text), &result); err != nil {
		t.Fatalf("mcpTool %s: parse text payload: %v", toolName, err)
	}
	return result
}

// TestInitAndStart verifies that `muninn init --yes --no-token` (the first-run
// path without interactive prompts) successfully starts the daemon and all three
// services become healthy. This is the most important gap in first-time user
// confidence: init delegates to runStart internally, so if that path is broken
// the very first command a new user runs fails.
func TestInitAndStart(t *testing.T) {
	dataDir := t.TempDir()
	out, err := muninnCmd(dataDir, "init", "--yes", "--no-token").CombinedOutput()
	if err != nil {
		t.Fatalf("muninn init --yes --no-token: %v\n%s", err, out)
	}
	// init calls runStart internally and blocks until the MCP port is up, so
	// health should be nearly immediate — but give the REST API (8475) a moment.
	if !waitForHealth(10 * time.Second) {
		t.Fatal("daemon did not become healthy within 10s after init")
	}
	t.Cleanup(func() {
		muninnCmd(dataDir, "stop").Run() //nolint:errcheck
		waitForDead(5 * time.Second)
	})

	// Confirm status reports "running" (all 3 services up).
	var statusOut string
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		out, err := muninnCmd(dataDir, "status").CombinedOutput()
		if err != nil {
			t.Errorf("muninn status returned non-zero: %v\n%s", err, out)
			break
		}
		statusOut = string(out)
		if strings.Contains(statusOut, "running") {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !strings.Contains(statusOut, "running") {
		t.Errorf("status never reached 'running' after init (last output):\n%s", statusOut)
	}
}

// TestMCPRoundTrip verifies the core product path: store a memory via the MCP
// tool, then fetch it back by ID and confirm the content is preserved.
// This test exercises the full stack: MCP server → engine → storage → retrieval.
func TestMCPRoundTrip(t *testing.T) {
	dataDir := t.TempDir()
	startDaemon(t, dataDir)

	// Use whatever token the daemon was started with (readTokenFile reads the
	// same path as runStart, so they are always consistent).
	tok := readTokenFile()

	// Store a memory.
	writeResult := mcpTool(t, tok, "muninn_remember", map[string]any{
		"vault":   "default",
		"content": "integration test memory — hello from TestMCPRoundTrip",
		"concept": "integration test",
	})
	id, ok := writeResult["id"].(string)
	if !ok || id == "" {
		t.Fatalf("muninn_remember: expected non-empty id, got: %v", writeResult)
	}

	// Fetch it back by ID.
	readResult := mcpTool(t, tok, "muninn_read", map[string]any{
		"vault": "default",
		"id":    id,
	})
	content, _ := readResult["content"].(string)
	if content == "" {
		t.Fatalf("muninn_read: content field missing or empty: %v", readResult)
	}
	const want = "integration test memory"
	if !strings.Contains(content, want) {
		t.Errorf("muninn_read: content %q does not contain %q", content, want)
	}
}

// waitForIDs polls muninn_recall until all wantIDs appear in results or timeout.
func waitForIDs(t *testing.T, token string, args map[string]any, wantIDs []string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		result := mcpTool(t, token, "muninn_recall", args)
		memories, _ := result["memories"].([]any)
		found := 0
		for _, m := range memories {
			mem, _ := m.(map[string]any)
			id, _ := mem["id"].(string)
			for _, want := range wantIDs {
				if id == want {
					found++
				}
			}
		}
		if found >= len(wantIDs) {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("timeout after %v: did not find all IDs %v in recall results", timeout, wantIDs)
}

// extractIDs returns the set of IDs from a muninn_recall result.
func extractIDs(result map[string]any) map[string]bool {
	ids := make(map[string]bool)
	memories, _ := result["memories"].([]any)
	for _, m := range memories {
		mem, _ := m.(map[string]any)
		if id, ok := mem["id"].(string); ok {
			ids[id] = true
		}
	}
	return ids
}

// TestMCPTemporalFilter verifies that the since/before temporal filters on
// muninn_recall correctly include or exclude memories based on created_at.
func TestMCPTemporalFilter(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	dataDir := t.TempDir()
	startDaemon(t, dataDir)
	token := readTokenFile()

	now := time.Now().UTC()
	ago30 := now.Add(-30 * 24 * time.Hour).Format(time.RFC3339)
	ago3 := now.Add(-3 * 24 * time.Hour).Format(time.RFC3339)
	ago7 := now.Add(-7 * 24 * time.Hour).Format(time.RFC3339)

	// Use a unique marker so FTS can reliably find these memories.
	marker := fmt.Sprintf("temporal_test_%d", time.Now().UnixNano())

	// Write 3 memories with controlled timestamps.
	r1 := mcpTool(t, token, "muninn_remember", map[string]any{
		"vault":      "default",
		"concept":    "ancient " + marker,
		"content":    marker + " something that happened long ago",
		"created_at": ago30,
	})
	r2 := mcpTool(t, token, "muninn_remember", map[string]any{
		"vault":      "default",
		"concept":    "recent " + marker,
		"content":    marker + " something that happened recently",
		"created_at": ago3,
	})
	r3 := mcpTool(t, token, "muninn_remember", map[string]any{
		"vault":   "default",
		"concept": "current " + marker,
		"content": marker + " something happening now",
	})

	id1, _ := r1["id"].(string)
	id2, _ := r2["id"].(string)
	id3, _ := r3["id"].(string)
	if id1 == "" || id2 == "" || id3 == "" {
		t.Fatalf("failed to get IDs: %v %v %v", r1, r2, r3)
	}

	// Wait for all 3 memories to be indexed before testing temporal queries.
	recallArgs := map[string]any{
		"vault":     "default",
		"context":   []string{marker},
		"limit":     20,
		"threshold": 0.0,
	}
	waitForIDs(t, token, recallArgs, []string{id1, id2, id3}, 15*time.Second)

	// since=7d ago: should return recent (3d) + current, NOT ancient (30d).
	result := mcpTool(t, token, "muninn_recall", map[string]any{
		"vault":     "default",
		"context":   []string{marker},
		"since":     ago7,
		"limit":     20,
		"threshold": 0.0,
	})
	ids := extractIDs(result)
	if !ids[id2] {
		t.Errorf("expected recent event (3d ago) in since=7d results, got ids: %v", ids)
	}
	if !ids[id3] {
		t.Errorf("expected current event in since=7d results, got ids: %v", ids)
	}
	if ids[id1] {
		t.Errorf("ancient event (30d ago) should NOT appear in since=7d results")
	}

	// before=7d ago: should return ancient only.
	result2 := mcpTool(t, token, "muninn_recall", map[string]any{
		"vault":     "default",
		"context":   []string{marker},
		"before":    ago7,
		"limit":     20,
		"threshold": 0.0,
	})
	ids2 := extractIDs(result2)
	if !ids2[id1] {
		t.Errorf("expected ancient event in before=7d results, got ids: %v", ids2)
	}
	if ids2[id2] || ids2[id3] {
		t.Errorf("recent/current events should NOT appear in before=7d results")
	}
}

// TestMCPAssociationTraversal verifies that explicitly linked memories are
// surfaced via graph traversal when mode=deep is requested.
func TestMCPAssociationTraversal(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	dataDir := t.TempDir()
	startDaemon(t, dataDir)
	token := readTokenFile()

	// Write 3 related memories.
	rA := mcpTool(t, token, "muninn_remember", map[string]any{
		"vault":   "default",
		"concept": "quantum mechanics",
		"content": "quantum mechanics explains particle behavior through wave equations",
	})
	rB := mcpTool(t, token, "muninn_remember", map[string]any{
		"vault":   "default",
		"concept": "wave functions",
		"content": "wave functions describe quantum states in Hilbert space",
	})
	rC := mcpTool(t, token, "muninn_remember", map[string]any{
		"vault":   "default",
		"concept": "Copenhagen interpretation",
		"content": "Copenhagen interpretation resolves the quantum measurement problem",
	})

	idA, _ := rA["id"].(string)
	idB, _ := rB["id"].(string)
	idC, _ := rC["id"].(string)
	if idA == "" || idB == "" || idC == "" {
		t.Fatalf("failed to write memories: %v %v %v", rA, rB, rC)
	}

	// Link A→B and B→C explicitly.
	mcpTool(t, token, "muninn_link", map[string]any{
		"vault":     "default",
		"source_id": idA,
		"target_id": idB,
		"relation":  "relates_to",
		"weight":    0.8,
	})
	mcpTool(t, token, "muninn_link", map[string]any{
		"vault":     "default",
		"source_id": idB,
		"target_id": idC,
		"relation":  "relates_to",
		"weight":    0.8,
	})

	// Poll-retry: recall with mode=deep (MaxHops=4) until all 3 IDs appear.
	waitForIDs(t, token, map[string]any{
		"vault":   "default",
		"context": []string{"quantum mechanics"},
		"mode":    "deep",
		"limit":   20,
	}, []string{idA, idB, idC}, 10*time.Second)
}

// TestMCPTraverse verifies that muninn_traverse returns nodes and edges when
// traversing an explicitly linked memory graph.
func TestMCPTraverse(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	dataDir := t.TempDir()
	startDaemon(t, dataDir)
	token := readTokenFile()

	// Write 3 memories.
	rA := mcpTool(t, token, "muninn_remember", map[string]any{
		"vault":   "default",
		"concept": "Node-A",
		"content": "starting point",
	})
	rB := mcpTool(t, token, "muninn_remember", map[string]any{
		"vault":   "default",
		"concept": "Node-B",
		"content": "intermediate node",
	})
	rC := mcpTool(t, token, "muninn_remember", map[string]any{
		"vault":   "default",
		"concept": "Node-C",
		"content": "terminal node",
	})

	idA, _ := rA["id"].(string)
	idB, _ := rB["id"].(string)
	idC, _ := rC["id"].(string)
	if idA == "" || idB == "" || idC == "" {
		t.Fatalf("failed to write memories: %v %v %v", rA, rB, rC)
	}

	// Link A→B and B→C.
	mcpTool(t, token, "muninn_link", map[string]any{
		"vault":     "default",
		"source_id": idA,
		"target_id": idB,
		"relation":  "relates_to",
		"weight":    0.8,
	})
	mcpTool(t, token, "muninn_link", map[string]any{
		"vault":     "default",
		"source_id": idB,
		"target_id": idC,
		"relation":  "relates_to",
		"weight":    0.8,
	})

	// Traverse from A with max_hops=2.
	result := mcpTool(t, token, "muninn_traverse", map[string]any{
		"vault":     "default",
		"start_id":  idA,
		"max_hops":  2,
		"max_nodes": 20,
	})

	// Assert no error key in result.
	if errVal, hasErr := result["error"]; hasErr {
		t.Fatalf("muninn_traverse returned error: %v", errVal)
	}

	nodes, _ := result["nodes"].([]any)
	if len(nodes) < 3 {
		t.Errorf("expected at least 3 nodes in traverse result, got %d: %v", len(nodes), result)
	}

	edges, _ := result["edges"].([]any)
	if len(edges) < 2 {
		t.Errorf("expected at least 2 edges in traverse result, got %d: %v", len(edges), result)
	}
}

// TestMCPFullPipelineProof exercises the full pipeline: temporal filtering
// combined with graph traversal to verify that linked memories respect the
// since filter (alpha at 20d is excluded; beta at 10d and gamma at 2d appear).
func TestMCPFullPipelineProof(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	dataDir := t.TempDir()
	startDaemon(t, dataDir)
	token := readTokenFile()

	now := time.Now().UTC()
	ago20 := now.Add(-20 * 24 * time.Hour).Format(time.RFC3339)
	ago10 := now.Add(-10 * 24 * time.Hour).Format(time.RFC3339)
	ago2 := now.Add(-2 * 24 * time.Hour).Format(time.RFC3339)
	ago15 := now.Add(-15 * 24 * time.Hour).Format(time.RFC3339)

	// Write 3 memories at different timestamps.
	rAlpha := mcpTool(t, token, "muninn_remember", map[string]any{
		"vault":      "default",
		"concept":    "alpha physics discovery",
		"content":    "alpha particle physics discovery from ancient experiments",
		"created_at": ago20,
	})
	rBeta := mcpTool(t, token, "muninn_remember", map[string]any{
		"vault":      "default",
		"concept":    "beta physics experiment",
		"content":    "beta physics experiment confirming alpha findings",
		"created_at": ago10,
	})
	rGamma := mcpTool(t, token, "muninn_remember", map[string]any{
		"vault":      "default",
		"concept":    "gamma physics result",
		"content":    "gamma physics result proving the theory correct",
		"created_at": ago2,
	})

	idAlpha, _ := rAlpha["id"].(string)
	idBeta, _ := rBeta["id"].(string)
	idGamma, _ := rGamma["id"].(string)
	if idAlpha == "" || idBeta == "" || idGamma == "" {
		t.Fatalf("failed to write memories")
	}

	// Link the chain: alpha→beta→gamma.
	mcpTool(t, token, "muninn_link", map[string]any{
		"vault": "default", "source_id": idAlpha, "target_id": idBeta, "relation": "relates_to", "weight": 0.8,
	})
	mcpTool(t, token, "muninn_link", map[string]any{
		"vault": "default", "source_id": idBeta, "target_id": idGamma, "relation": "relates_to", "weight": 0.8,
	})

	// Recall with since=15d ago + mode=deep.
	// Should return: beta + gamma (in time window) via direct match + traversal.
	// Should NOT return: alpha (outside 15d window even though linked).
	waitForIDs(t, token, map[string]any{
		"vault":   "default",
		"context": []string{"physics"},
		"since":   ago15,
		"mode":    "deep",
		"limit":   20,
	}, []string{idBeta, idGamma}, 10*time.Second)

	// Verify alpha excluded.
	result := mcpTool(t, token, "muninn_recall", map[string]any{
		"vault":   "default",
		"context": []string{"physics"},
		"since":   ago15,
		"mode":    "deep",
		"limit":   20,
	})
	ids := extractIDs(result)
	if ids[idAlpha] {
		t.Errorf("alpha (20d ago) should NOT appear in since=15d results — temporal filter failed")
	}
	if !ids[idBeta] {
		t.Errorf("beta (10d ago) should appear in since=15d results")
	}
	if !ids[idGamma] {
		t.Errorf("gamma (2d ago) should appear in since=15d results")
	}
}

// TestVaultBehaviorPersistsAfterInit is an end-to-end regression test for issue #189.
// It starts a real daemon, calls applyBehaviorToVault (the function invoked during
// `muninn init`), then reads back the plasticity config via the REST API to confirm
// the mode was actually written to storage — not just acknowledged in memory.
func TestVaultBehaviorPersistsAfterInit(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	dataDir := t.TempDir()
	startDaemon(t, dataDir)

	// Wait for the REST admin API (8475) to be accepting connections.
	adminReady := false
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get("http://127.0.0.1:8475/api/vaults")
		if err == nil {
			resp.Body.Close()
			adminReady = true
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !adminReady {
		t.Fatal("REST API did not become available within 8s")
	}

	// Apply behavior via the same function called during init.
	// This uses the default vaultAdminBase (8475) and vaultUIBase (8476).
	out := captureStdout(func() {
		applyBehaviorToVault("selective", "")
	})
	if strings.Contains(out, "muninn vault behavior") {
		// Fallback message printed — something went wrong with apply.
		t.Fatalf("applyBehaviorToVault triggered fallback: %s", out)
	}
	if !strings.Contains(out, "selective") {
		t.Errorf("applyBehaviorToVault success message should contain 'selective', got: %q", out)
	}

	// Verify persistence via REST GET — login first to get a session cookie,
	// then hit the plasticity endpoint directly.
	loginResp, err := http.Post(
		"http://127.0.0.1:8476/api/auth/login",
		"application/json",
		strings.NewReader(`{"username":"root","password":"password"}`),
	)
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	defer loginResp.Body.Close()
	if loginResp.StatusCode != http.StatusOK {
		t.Fatalf("login: HTTP %d", loginResp.StatusCode)
	}

	// Collect session cookie.
	var sessionCookie *http.Cookie
	for _, c := range loginResp.Cookies() {
		if c.Name == "session" || c.Name == "muninn_session" || strings.Contains(c.Name, "session") {
			sessionCookie = c
			break
		}
	}
	if sessionCookie == nil {
		t.Fatal("login did not return a session cookie")
	}

	// GET plasticity and verify behavior_mode = "selective".
	getReq, err := http.NewRequest("GET", "http://127.0.0.1:8475/api/admin/vault/default/plasticity", nil)
	if err != nil {
		t.Fatalf("build GET: %v", err)
	}
	getReq.AddCookie(sessionCookie)
	getResp, err := (&http.Client{}).Do(getReq)
	if err != nil {
		t.Fatalf("GET plasticity: %v", err)
	}
	defer getResp.Body.Close()
	if getResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(getResp.Body)
		t.Fatalf("GET plasticity: HTTP %d: %s", getResp.StatusCode, body)
	}

	var result struct {
		Resolved struct {
			BehaviorMode string `json:"behavior_mode"`
		} `json:"resolved"`
	}
	if err := json.NewDecoder(getResp.Body).Decode(&result); err != nil {
		t.Fatalf("decode plasticity response: %v", err)
	}
	if result.Resolved.BehaviorMode != "selective" {
		t.Errorf("behavior_mode = %q after applyBehaviorToVault, want \"selective\"", result.Resolved.BehaviorMode)
	}
}
