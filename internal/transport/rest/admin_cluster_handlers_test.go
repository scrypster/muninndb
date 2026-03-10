package rest

import (
	"encoding/json"
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

// TestBug175_SettingsPersistAllFields is a regression test for GitHub issue #175.
// The settings endpoint accepted sdown_beats, ccs_interval_seconds, and reconcile_on_heal
// but applyAndPersistSettings only wrote heartbeat_ms to disk — the other three were silently
// dropped. This test proves the bug exists (fails before fix) and guards against regressions.
func TestBug175_SettingsPersistAllFields(t *testing.T) {
	dataDir := t.TempDir()
	s := NewServer("localhost:0", &MockEngine{}, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, dataDir, nil)

	// First enable cluster so there is a cluster.yaml on disk.
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/admin/cluster/enable",
		strings.NewReader(`{"role":"primary","bind_addr":"127.0.0.1:8474"}`))
	r.Header.Set("Content-Type", "application/json")
	s.mux.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("enable cluster: expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// Send all four settings fields.
	w = httptest.NewRecorder()
	r = httptest.NewRequest("PUT", "/api/admin/cluster/settings",
		strings.NewReader(`{"heartbeat_ms":750,"sdown_beats":5,"ccs_interval_seconds":60,"reconcile_on_heal":false}`))
	r.Header.Set("Content-Type", "application/json")
	s.mux.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("save settings: expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// Read back the saved config and verify all four fields are persisted.
	saved, err := config.LoadClusterConfig(dataDir)
	if err != nil {
		t.Fatalf("LoadClusterConfig: %v", err)
	}
	if saved.HeartbeatMS != 750 {
		t.Errorf("HeartbeatMS = %d, want 750", saved.HeartbeatMS)
	}
	// These three assertions fail before the fix (#175):
	if saved.SDOWNBeats != 5 {
		t.Errorf("SDOWNBeats = %d, want 5 (regression: issue #175)", saved.SDOWNBeats)
	}
	if saved.CCSIntervalS != 60 {
		t.Errorf("CCSIntervalS = %d, want 60 (regression: issue #175)", saved.CCSIntervalS)
	}
	if saved.ReconcileHeal != false {
		t.Errorf("ReconcileHeal = %v, want false (regression: issue #175)", saved.ReconcileHeal)
	}
}

// TestBug175_GetSettingsReturnsPersistedValues verifies that GET /api/admin/cluster/settings
// returns the values previously saved via PUT, not stale defaults. This is the server-side
// counterpart of the UI regression guard: if this fails, the form would always show defaults.
func TestBug175_GetSettingsReturnsPersistedValues(t *testing.T) {
	dataDir := t.TempDir()
	s := NewServer("localhost:0", &MockEngine{}, nil, nil, nil, EmbedInfo{}, EnrichInfo{}, nil, dataDir, nil)

	// Enable cluster so cluster.yaml exists.
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/admin/cluster/enable",
		strings.NewReader(`{"role":"primary","bind_addr":"127.0.0.1:8474"}`))
	r.Header.Set("Content-Type", "application/json")
	s.mux.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("enable cluster: %d: %s", w.Code, w.Body.String())
	}

	// PUT specific values.
	w = httptest.NewRecorder()
	r = httptest.NewRequest("PUT", "/api/admin/cluster/settings",
		strings.NewReader(`{"heartbeat_ms":800,"sdown_beats":7,"ccs_interval_seconds":45,"reconcile_on_heal":false}`))
	r.Header.Set("Content-Type", "application/json")
	s.mux.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT settings: %d: %s", w.Code, w.Body.String())
	}

	// GET and verify the response body reflects what was saved.
	w = httptest.NewRecorder()
	r = httptest.NewRequest("GET", "/api/admin/cluster/settings", nil)
	s.mux.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("GET settings: %d: %s", w.Code, w.Body.String())
	}
	var got map[string]any
	if err := json.NewDecoder(w.Body).Decode(&got); err != nil {
		t.Fatalf("decode GET response: %v", err)
	}
	checks := map[string]any{
		"heartbeat_ms":         float64(800),
		"sdown_beats":          float64(7),
		"ccs_interval_seconds": float64(45),
		"reconcile_on_heal":    false,
	}
	for field, want := range checks {
		if got[field] != want {
			t.Errorf("GET settings: %s = %v, want %v", field, got[field], want)
		}
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
