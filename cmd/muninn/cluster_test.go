package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// TestClusterCommands_InfoJSON verifies that cluster info --json outputs raw JSON.
func TestClusterCommands_InfoJSON(t *testing.T) {
	// Create a mock HTTP server
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/cluster/info" && r.Method == "GET" {
			info := map[string]interface{}{
				"node_id":       "node-1",
				"role":          "Cortex",
				"is_leader":     true,
				"epoch":         42,
				"fencing_token": 100,
				"cortex_id":     "cortex-1",
				"members": []map[string]interface{}{
					{
						"node_id":  "node-1",
						"addr":     "localhost:8474",
						"role":     "Cortex",
						"last_seq": 1000,
					},
					{
						"node_id":  "node-2",
						"addr":     "127.0.0.1:8475",
						"role":     "Lobe",
						"last_seq": 950,
					},
				},
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(info)
		}
	}))
	defer server.Close()

	// Redirect the cluster info command to our mock server
	oldStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	// Run the command
	runClusterInfo([]string{"--addr", server.URL, "--json"})

	w.Close()
	os.Stdout = oldStdout

	// Read output
	output := make([]byte, 4096)
	n, _ := r.Read(output)
	result := string(output[:n])

	// Verify JSON output contains expected fields
	if !strings.Contains(result, "node-1") {
		t.Errorf("Expected node-1 in output, got: %s", result)
	}
	if !strings.Contains(result, "Cortex") {
		t.Errorf("Expected Cortex in output, got: %s", result)
	}
}

// TestClusterCommands_HealthStatus verifies that cluster status works with a healthy response.
func TestClusterCommands_HealthStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/cluster/health" && r.Method == "GET" {
			health := map[string]interface{}{
				"status":          "ok",
				"role":            "Cortex",
				"is_leader":       true,
				"epoch":           42,
				"replication_lag": 10,
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(health)
		} else if r.URL.Path == "/v1/cluster/nodes" && r.Method == "GET" {
			nodes := map[string]interface{}{
				"nodes": []map[string]interface{}{
					{
						"node_id":  "node-1",
						"addr":     "localhost:8474",
						"role":     "Cortex",
						"last_seq": 1000,
					},
				},
				"count": 1,
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(nodes)
		}
	}))
	defer server.Close()

	oldStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	runClusterStatus([]string{"--addr", server.URL})

	w.Close()
	os.Stdout = oldStdout

	output := make([]byte, 4096)
	n, _ := r.Read(output)
	result := string(output[:n])

	if !strings.Contains(result, "ok") {
		t.Errorf("Expected 'ok' status in output")
	}
	if !strings.Contains(result, "Cortex") {
		t.Errorf("Expected Cortex role in output")
	}
}

// TestClusterCommands_ServerUnreachable verifies proper error handling when server is down.
func TestClusterCommands_ServerUnreachable(t *testing.T) {
	// Use a port that's unlikely to have anything listening
	unreachableAddr := "http://localhost:1"

	// Capture stderr
	oldStderr := os.Stderr
	_, w, _ := os.Pipe()
	os.Stderr = w

	// This should exit with error, so we catch the panic if os.Exit is called
	defer func() {
		os.Stderr = oldStderr
	}()

	// Try to make a request to an unreachable server
	_, err := httpGet(unreachableAddr + "/v1/cluster/info")
	if err == nil {
		t.Errorf("Expected error for unreachable server, got nil")
	}

	w.Close()
}

// TestClusterCommands_AddNode verifies add-node prints instructions.
func TestClusterCommands_AddNode(t *testing.T) {
	oldStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	runClusterAddNode([]string{})

	w.Close()
	os.Stdout = oldStdout

	output := make([]byte, 4096)
	n, _ := r.Read(output)
	result := string(output[:n])

	if !strings.Contains(result, "Adding a New Node") {
		t.Errorf("Expected 'Adding a New Node' in output")
	}
	if !strings.Contains(result, "cluster.yaml") {
		t.Errorf("Expected 'cluster.yaml' in output")
	}
	if !strings.Contains(result, "enabled: true") {
		t.Errorf("Expected 'enabled: true' in output")
	}
}

// TestClusterCommands_RemoveNode verifies remove-node shows not-yet-implemented message.
func TestClusterCommands_RemoveNode(t *testing.T) {
	oldStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	runClusterRemoveNode([]string{"--node", "node-1"})

	w.Close()
	os.Stdout = oldStdout

	output := make([]byte, 4096)
	n, _ := r.Read(output)
	result := string(output[:n])

	if !strings.Contains(result, "not yet supported") {
		t.Errorf("Expected 'not yet supported' in output")
	}
	if !strings.Contains(result, "Workaround") {
		t.Errorf("Expected 'Workaround' in output")
	}
}

// TestClusterEnable_MissingBindAddr verifies that enable requires --bind-addr.
func TestClusterEnable_MissingBindAddr(t *testing.T) {
	oldStderr := os.Stderr
	_, w, _ := os.Pipe()
	os.Stderr = w
	defer func() {
		os.Stderr = oldStderr
		w.Close()
	}()

	rc := runClusterEnable([]string{"--yes"})
	if rc != 1 {
		t.Errorf("expected exit code 1 when --bind-addr missing, got %d", rc)
	}
}

// TestClusterEnable_Success verifies enable posts to /api/admin/cluster/enable.
func TestClusterEnable_Success(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/admin/cluster/enable" && r.Method == "POST" {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"enabled": true, "role": "primary"})
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	oldStdout := os.Stdout
	r, wp, _ := os.Pipe()
	os.Stdout = wp

	rc := runClusterEnable([]string{
		"--addr", server.URL,
		"--role", "primary",
		"--bind-addr", "127.0.0.1:7777",
		"--yes",
	})

	wp.Close()
	os.Stdout = oldStdout

	output := make([]byte, 4096)
	n, _ := r.Read(output)
	result := string(output[:n])

	if rc != 0 {
		t.Errorf("expected exit code 0, got %d", rc)
	}
	if !strings.Contains(result, "enabled") {
		t.Errorf("expected 'enabled' in output, got: %s", result)
	}
}

// TestClusterEnable_NonPrimaryMissingCortexAddr verifies that a non-primary role without --cortex-addr exits 1.
func TestClusterEnable_NonPrimaryMissingCortexAddr(t *testing.T) {
	rc := runClusterEnable([]string{
		"--role", "replica",
		"--bind-addr", "127.0.0.1:7777",
		"--yes",
	})
	if rc != 1 {
		t.Errorf("expected exit code 1 when replica role missing --cortex-addr, got %d", rc)
	}
}

// TestClusterEnable_ServerError verifies that a non-200 response from the server returns exit code 1.
func TestClusterEnable_ServerError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]any{"error": "coordinator not ready"})
	}))
	defer server.Close()

	rc := runClusterEnable([]string{
		"--addr", server.URL,
		"--role", "primary",
		"--bind-addr", "127.0.0.1:7777",
		"--yes",
	})
	if rc != 1 {
		t.Errorf("expected exit code 1 on server error, got %d", rc)
	}
}

// TestClusterDisable_Success verifies disable posts to /api/admin/cluster/disable.
func TestClusterDisable_Success(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/admin/cluster/disable" && r.Method == "POST" {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"enabled": false})
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	oldStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	rc := runClusterDisable([]string{"--addr", server.URL, "--yes"})

	w.Close()
	os.Stdout = oldStdout

	output := make([]byte, 4096)
	n, _ := r.Read(output)
	result := string(output[:n])

	if rc != 0 {
		t.Errorf("expected exit code 0, got %d", rc)
	}
	if !strings.Contains(result, "disabled") {
		t.Errorf("expected 'disabled' in output, got: %s", result)
	}
}
