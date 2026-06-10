package rest

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/scrypster/muninndb/internal/replication"
)

// TestClusterInfo_ClusterDisabled verifies {"enabled":false} when no coordinator.
func TestClusterInfo_ClusterDisabled(t *testing.T) {
	srv := newTestServer(t, nil)

	req := httptest.NewRequest("GET", "/v1/cluster/info", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if enabled, _ := resp["enabled"].(bool); enabled {
		t.Fatal("expected enabled=false when cluster disabled")
	}
}

// TestClusterInfo_WithCoordinator verifies full response is returned.
func TestClusterInfo_WithCoordinator(t *testing.T) {
	const testSecret = "test-cluster-secret"
	srv := newTestServer(t, nil)
	coord := newTestCoordinatorWithSecret(t, testSecret)
	srv.SetCoordinator(coord)

	req := httptest.NewRequest("GET", "/v1/cluster/info", nil)
	req.Header.Set("Authorization", "Bearer "+testSecret)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp clusterInfoResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.NodeID == "" {
		t.Error("expected non-empty node_id")
	}
	if resp.Role == "" {
		t.Error("expected non-empty role")
	}
	if resp.Members == nil {
		t.Error("expected members field in response")
	}
}

// TestClusterHealth_Standalone verifies {status:ok, role:standalone} when nil coordinator.
func TestClusterHealth_Standalone(t *testing.T) {
	srv := newTestServer(t, nil)

	req := httptest.NewRequest("GET", "/v1/cluster/health", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp clusterHealthResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Status != "ok" {
		t.Errorf("expected status=ok, got %q", resp.Status)
	}
	if resp.Role != "standalone" {
		t.Errorf("expected role=standalone, got %q", resp.Role)
	}
}

// TestClusterHealth_Healthy verifies status=ok for a primary node.
func TestClusterHealth_Healthy(t *testing.T) {
	const testSecret = "test-cluster-secret"
	srv := newTestServer(t, nil)
	coord := newTestCoordinatorWithSecret(t, testSecret)
	srv.SetCoordinator(coord)

	// Trigger election so the node becomes primary (lag=0).
	_ = coord.Election().StartElection(nil)

	req := httptest.NewRequest("GET", "/v1/cluster/health", nil)
	req.Header.Set("Authorization", "Bearer "+testSecret)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp clusterHealthResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Status != "ok" {
		t.Errorf("expected status=ok for primary, got %q", resp.Status)
	}
}

// TestClusterHealth_Down verifies 503 when role=Unknown.
func TestClusterHealth_Down(t *testing.T) {
	// clusterStatus with RoleUnknown should return "down".
	status := clusterStatus(replication.RoleUnknown, 0)
	if status != "down" {
		t.Errorf("expected down for unknown role, got %q", status)
	}
}

// TestClusterNodes_Empty verifies empty list when no coordinator.
func TestClusterNodes_Empty(t *testing.T) {
	srv := newTestServer(t, nil)

	req := httptest.NewRequest("GET", "/v1/cluster/nodes", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp clusterNodesResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Count != 0 {
		t.Errorf("expected count=0, got %d", resp.Count)
	}
	if len(resp.Nodes) != 0 {
		t.Errorf("expected empty nodes, got %d", len(resp.Nodes))
	}
}

// TestClusterNodes_WithMembers verifies member list is returned with coordinator.
func TestClusterNodes_WithMembers(t *testing.T) {
	const testSecret = "test-cluster-secret"
	srv := newTestServer(t, nil)
	coord := newTestCoordinatorWithSecret(t, testSecret)
	srv.SetCoordinator(coord)

	req := httptest.NewRequest("GET", "/v1/cluster/nodes", nil)
	req.Header.Set("Authorization", "Bearer "+testSecret)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp clusterNodesResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// The coordinator always includes self in the member list.
	if resp.Count < 1 {
		t.Errorf("expected at least 1 member (self), got %d", resp.Count)
	}
	if len(resp.Nodes) != resp.Count {
		t.Errorf("nodes length %d != count %d", len(resp.Nodes), resp.Count)
	}
}

// TestCognitiveConsistency_NoCoordinator verifies default excellent score when no coordinator.
func TestCognitiveConsistency_NoCoordinator(t *testing.T) {
	srv := newTestServer(t, nil)

	req := httptest.NewRequest("GET", "/v1/cluster/cognitive/consistency", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp cognitiveConsistencyResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Score != 1.0 {
		t.Errorf("expected score=1.0, got %f", resp.Score)
	}
	if resp.Assessment != "excellent" {
		t.Errorf("expected assessment=excellent, got %q", resp.Assessment)
	}
}

// TestCognitiveConsistency_WithCoordinator verifies the response reflects coordinator state.
func TestCognitiveConsistency_WithCoordinator(t *testing.T) {
	const testSecret = "test-cluster-secret"
	srv := newTestServer(t, nil)
	coord := newTestCoordinatorWithSecret(t, testSecret)
	srv.SetCoordinator(coord)

	req := httptest.NewRequest("GET", "/v1/cluster/cognitive/consistency", nil)
	req.Header.Set("Authorization", "Bearer "+testSecret)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp cognitiveConsistencyResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// Without a CCSProbe wired, coordinator returns default excellent.
	if resp.Score != 1.0 {
		t.Errorf("expected score=1.0, got %f", resp.Score)
	}
	if resp.Assessment != "excellent" {
		t.Errorf("expected assessment=excellent, got %q", resp.Assessment)
	}
	if resp.NodeScores == nil {
		t.Error("expected non-nil node_scores")
	}
}

// TestClusterStatus_Thresholds verifies the lag-based status logic for replicas.
func TestClusterStatus_Thresholds(t *testing.T) {
	cases := []struct {
		role     replication.NodeRole
		lag      uint64
		expected string
	}{
		{replication.RolePrimary, 0, "ok"},
		{replication.RolePrimary, 5000, "ok"}, // primary lag is always ok
		{replication.RoleReplica, 0, "ok"},
		{replication.RoleReplica, 999, "ok"},
		{replication.RoleReplica, 1000, "degraded"},
		{replication.RoleReplica, 9999, "degraded"},
		{replication.RoleReplica, 10000, "down"},
		{replication.RoleUnknown, 0, "down"},
	}
	for _, tc := range cases {
		got := clusterStatus(tc.role, tc.lag)
		if got != tc.expected {
			t.Errorf("clusterStatus(%v, %d) = %q, want %q", tc.role, tc.lag, got, tc.expected)
		}
	}
}
