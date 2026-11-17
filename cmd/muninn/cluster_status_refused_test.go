package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// runStatusAgainst runs `muninn cluster status` against a server that answers
// /v1/cluster/health with the given status and body, and returns what the
// command wrote to stderr and the code it exited with (0 when it did not exit).
func runStatusAgainst(t *testing.T, healthStatus int, healthBody any) (stderr string, exitCode int) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/cluster/health":
			w.WriteHeader(healthStatus)
			_ = json.NewEncoder(w).Encode(healthBody)
		case "/v1/cluster/nodes":
			_ = json.NewEncoder(w).Encode(map[string]any{"nodes": []any{}, "count": 0})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	oldExit, oldStderr, oldStdout := osExit, os.Stderr, os.Stdout
	osExit = func(code int) { exitCode = code }
	errR, errW, _ := os.Pipe()
	outR, outW, _ := os.Pipe()
	os.Stderr, os.Stdout = errW, outW
	defer func() { osExit, os.Stderr, os.Stdout = oldExit, oldStderr, oldStdout }()

	runClusterStatus([]string{"--addr", server.URL})

	errW.Close()
	outW.Close()
	errBytes, _ := io.ReadAll(errR)
	_, _ = io.ReadAll(outR)
	return string(errBytes), exitCode
}

// TestClusterStatus_RefusedReplicaSaysWhy: a replica its leader has refused
// answers /v1/cluster/health with 503 and a reason. The command used to print
// "failed to reach server ... HTTP 503" for it, which points an operator at the
// network when the server is up and is saying exactly what is wrong.
func TestClusterStatus_RefusedReplicaSaysWhy(t *testing.T) {
	const reason = "replication: this node holds an entry the cortex does not: invented detail"
	stderr, code := runStatusAgainst(t, http.StatusServiceUnavailable, map[string]any{
		"status": "down", "role": "replica", "epoch": 2, "reason": reason,
	})

	if code != 1 {
		t.Errorf("exit code = %d, want 1: a node that is not replicating is not healthy", code)
	}
	if want := "node is refusing to replicate: " + reason; !strings.Contains(stderr, want) {
		t.Errorf("stderr does not say %q:\n%s", want, stderr)
	}
	if strings.Contains(stderr, "failed to reach server") {
		t.Errorf("stderr still says the server was not reached:\n%s", stderr)
	}
}

// TestClusterStatus_A503WithoutAReasonIsNotAnUnreachableServer: a node with no
// role yet, or too far behind, answers 503 with no reason. The server was
// reached and it answered. Saying it was not reached sends an operator to look
// at the network.
func TestClusterStatus_A503WithoutAReasonIsNotAnUnreachableServer(t *testing.T) {
	stderr, code := runStatusAgainst(t, http.StatusServiceUnavailable, map[string]any{"status": "down", "role": "unknown"})
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr, "answered HTTP 503") {
		t.Errorf("stderr does not say the server answered 503:\n%s", stderr)
	}
	for _, wrong := range []string{"failed to reach server", "refusing to replicate"} {
		if strings.Contains(stderr, wrong) {
			t.Errorf("stderr says %q for a server that answered 503 with no reason:\n%s", wrong, stderr)
		}
	}
	for _, want := range []string{`"status":"down"`, `"role":"unknown"`} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr does not carry the body's %s:\n%s", want, stderr)
		}
	}
}

func TestHealthRefusalReason(t *testing.T) {
	for body, want := range map[string]string{
		`{"status":"down","reason":"invented"}`: "invented",
		`{"status":"down"}`:                     "",
		`{"status":"down","reason":""}`:         "",
		`not json`:                              "",
		``:                                      "",
	} {
		if got := healthRefusalReason([]byte(body)); got != want {
			t.Errorf("healthRefusalReason(%q) = %q, want %q", body, got, want)
		}
	}
}
