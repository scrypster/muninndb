package rest

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/scrypster/muninndb/internal/config"
)

func TestAdminCluster_GetToken_NoCoordinator(t *testing.T) {
	s := newTestServer(t, nil)
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/admin/cluster/token", nil)
	s.mux.ServeHTTP(w, r)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d: %s", w.Code, w.Body.String())
	}
}

func TestAdminCluster_Enable_MissingRole(t *testing.T) {
	s := newTestServer(t, nil)
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/admin/cluster/enable", strings.NewReader(`{"bind_addr":"127.0.0.1:7777"}`))
	r.Header.Set("Content-Type", "application/json")
	s.mux.ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestAdminCluster_Enable_MissingBindAddr(t *testing.T) {
	s := newTestServer(t, nil)
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/admin/cluster/enable", strings.NewReader(`{"role":"primary"}`))
	r.Header.Set("Content-Type", "application/json")
	s.mux.ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestAdminCluster_Settings_Validation_HeartbeatNegative(t *testing.T) {
	s := newTestServer(t, nil)
	w := httptest.NewRecorder()
	r := httptest.NewRequest("PUT", "/api/admin/cluster/settings", strings.NewReader(`{"heartbeat_ms":-1}`))
	r.Header.Set("Content-Type", "application/json")
	s.mux.ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

// TestAdminCluster_Enable_DefaultsWrittenToDisk is a regression test for
// GitHub issue #101: the cluster enable handler was writing lease_ttl: 0 and
// heartbeat_ms: 0 to cluster.yaml, causing a crash on the next restart.
func TestAdminCluster_Enable_DefaultsWrittenToDisk(t *testing.T) {
	dataDir := t.TempDir()
	s := NewServer("localhost:0", &MockEngine{}, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, dataDir, nil)

	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/admin/cluster/enable",
		strings.NewReader(`{"role":"primary","bind_addr":"127.0.0.1:8474"}`))
	r.Header.Set("Content-Type", "application/json")
	s.mux.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// Read back the persisted config and verify no zero-value timing fields.
	saved, err := config.LoadClusterConfig(dataDir)
	if err != nil {
		t.Fatalf("LoadClusterConfig: %v", err)
	}
	if !saved.Enabled {
		t.Error("cluster should be enabled in saved config")
	}
	if saved.LeaseTTL <= 0 {
		t.Errorf("LeaseTTL = %d, want > 0 (regression: issue #101)", saved.LeaseTTL)
	}
	if saved.HeartbeatMS <= 0 {
		t.Errorf("HeartbeatMS = %d, want > 0 (regression: issue #101)", saved.HeartbeatMS)
	}
	// Spot-check other timing defaults are also non-zero.
	if saved.QuorumLossTimeoutSec <= 0 {
		t.Errorf("QuorumLossTimeoutSec = %d, want > 0", saved.QuorumLossTimeoutSec)
	}
}

// TestClusterDefaults_NonZero verifies that ClusterDefaults returns sensible
// non-zero timing values so callers can use it as a safe base.
func TestClusterDefaults_NonZero(t *testing.T) {
	d := config.ClusterDefaults()
	if d.LeaseTTL <= 0 {
		t.Errorf("ClusterDefaults().LeaseTTL = %d, want > 0", d.LeaseTTL)
	}
	if d.HeartbeatMS <= 0 {
		t.Errorf("ClusterDefaults().HeartbeatMS = %d, want > 0", d.HeartbeatMS)
	}
}

func TestAdminCluster_Disable_NoCoordinator(t *testing.T) {
	s := newTestServer(t, nil)
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/admin/cluster/disable", nil)
	s.mux.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
}
