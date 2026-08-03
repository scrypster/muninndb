package rest

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/scrypster/muninndb/internal/config"
)

// TestClusterEnable_RequiresRestartAndBuildsNoCoordinator pins the #628
// resolution. Clustering cannot be turned on inside a running process: the
// storage layer's replication hook is captured when the PebbleStore is built at
// boot and only when cluster.yaml already said Enabled. A coordinator created
// after that point is attached to a replication log nothing appends to — the
// node calls itself clustered, hands joining Lobes a snapshot, and then
// replicates none of its subsequent writes.
//
// So the endpoint persists the configuration and says so: 202, enabled=false,
// restart_required=true, and no coordinator on the server.
func TestClusterEnable_RequiresRestartAndBuildsNoCoordinator(t *testing.T) {
	dir := t.TempDir()
	srv := &Server{dataDir: dir}

	body := `{"role":"primary","bind_addr":"127.0.0.1:7946","cluster_secret":"synthetic-secret"}`
	req := httptest.NewRequest(http.MethodPost, "/api/admin/cluster/enable", strings.NewReader(body))
	w := httptest.NewRecorder()
	srv.handleAdminClusterEnable(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("status: got %d, want 202 Accepted (200 claims a cluster that is not running): %s",
			w.Code, w.Body.String())
	}

	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got, ok := resp["restart_required"].(bool); !ok || !got {
		t.Fatalf("restart_required: got %v, want true — the operator must be told (body: %s)",
			resp["restart_required"], w.Body.String())
	}
	if got, ok := resp["enabled"].(bool); !ok || got {
		t.Fatalf("enabled: got %v, want false — clustering is configured, not running", resp["enabled"])
	}
	if got, ok := resp["configured"].(bool); !ok || !got {
		t.Fatalf("configured: got %v, want true", resp["configured"])
	}

	if srv.coordinator != nil {
		t.Fatal("runtime enable constructed a coordinator; that coordinator cannot replicate this node's writes (#628)")
	}

	saved, err := config.LoadClusterConfig(dir)
	if err != nil {
		t.Fatalf("load persisted cluster config: %v", err)
	}
	if !saved.Enabled {
		t.Error("configuration was not persisted as enabled; the restart would not start clustering")
	}
	if saved.Role != "primary" || saved.BindAddr != "127.0.0.1:7946" {
		t.Errorf("persisted config mismatch: role=%q bind=%q", saved.Role, saved.BindAddr)
	}
	if saved.NodeID == "" {
		t.Error("persisted config has no NodeID; the node could not rejoin after restart")
	}
}

// TestEnableClusterRuntime_NeverStartsACoordinator is the structural half: the
// only way to get a coordinator is the boot path. There is no factory seam to
// wire, by construction — this test exists so a future reintroduction of one
// has to delete an assertion that explains why it must not come back.
func TestEnableClusterRuntime_NeverStartsACoordinator(t *testing.T) {
	dir := t.TempDir()
	srv := &Server{dataDir: dir}

	if err := srv.enableClusterRuntime(context.Background(), config.ClusterConfig{
		Enabled:  true,
		NodeID:   "node-synthetic",
		BindAddr: "127.0.0.1:7946",
		Role:     "primary",
	}); err != nil {
		t.Fatalf("enableClusterRuntime: %v", err)
	}
	if srv.coordinator != nil {
		t.Fatal("enableClusterRuntime started a coordinator outside the boot path (#628)")
	}
	saved, _ := config.LoadClusterConfig(dir)
	if !saved.Enabled {
		t.Error("expected config to be persisted as enabled")
	}
}
