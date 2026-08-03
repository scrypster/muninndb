package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestClusterConfig_Defaults(t *testing.T) {
	cfg, err := LoadClusterConfig(t.TempDir())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Enabled {
		t.Error("expected Enabled=false")
	}
	if cfg.Role != "auto" {
		t.Errorf("expected Role=auto, got %q", cfg.Role)
	}
	if cfg.LeaseTTL != 10 {
		t.Errorf("expected LeaseTTL=10, got %d", cfg.LeaseTTL)
	}
	if cfg.HeartbeatMS != 1000 {
		t.Errorf("expected HeartbeatMS=1000, got %d", cfg.HeartbeatMS)
	}
}

func TestClusterConfig_EnvOverride(t *testing.T) {
	t.Setenv("MUNINN_CLUSTER_ENABLED", "true")
	t.Setenv("MUNINN_CLUSTER_NODE_ID", "test-node")
	t.Setenv("MUNINN_CLUSTER_SEEDS", "10.0.0.1:8474,10.0.0.2:8474")
	t.Setenv("MUNINN_CLUSTER_SECRET", "secret")

	cfg, err := LoadClusterConfig(t.TempDir())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !cfg.Enabled {
		t.Error("expected Enabled=true")
	}
	if cfg.NodeID != "test-node" {
		t.Errorf("expected NodeID=test-node, got %q", cfg.NodeID)
	}
	if len(cfg.Seeds) != 2 {
		t.Errorf("expected 2 seeds, got %d: %v", len(cfg.Seeds), cfg.Seeds)
	} else {
		if cfg.Seeds[0] != "10.0.0.1:8474" {
			t.Errorf("unexpected seed[0]: %q", cfg.Seeds[0])
		}
		if cfg.Seeds[1] != "10.0.0.2:8474" {
			t.Errorf("unexpected seed[1]: %q", cfg.Seeds[1])
		}
	}
	if cfg.ClusterSecret != "secret" {
		t.Errorf("expected ClusterSecret=secret, got %q", cfg.ClusterSecret)
	}
}

func TestClusterConfig_Validate_Valid(t *testing.T) {
	cfg := ClusterConfig{
		Enabled:     true,
		NodeID:      "node-1",
		Seeds:       []string{"seed:8474"},
		Role:        "auto",
		LeaseTTL:    10,
		HeartbeatMS: 1000,
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("expected nil error, got: %v", err)
	}
}

func TestClusterConfig_Validate_MissingSeeds(t *testing.T) {
	cfg := ClusterConfig{
		Enabled:     true,
		NodeID:      "node-1",
		Seeds:       []string{},
		Role:        "auto",
		LeaseTTL:    10,
		HeartbeatMS: 1000,
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "seeds") {
		t.Errorf("expected error to contain 'seeds', got: %v", err)
	}
}

func TestClusterConfig_Validate_InvalidRole(t *testing.T) {
	cfg := ClusterConfig{
		Enabled:     true,
		NodeID:      "node-1",
		Seeds:       []string{"s:8474"},
		Role:        "master",
		LeaseTTL:    10,
		HeartbeatMS: 1000,
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "role") {
		t.Errorf("expected error to contain 'role', got: %v", err)
	}
}

func TestClusterConfig_AutoNodeID(t *testing.T) {
	t.Setenv("MUNINN_CLUSTER_ENABLED", "true")
	// Do not set MUNINN_CLUSTER_NODE_ID

	dataDir := t.TempDir()

	cfg1, err := LoadClusterConfig(dataDir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg1.NodeID == "" {
		t.Fatal("expected non-empty auto-generated NodeID")
	}

	// Same dataDir must produce the same NodeID.
	cfg2, err := LoadClusterConfig(dataDir)
	if err != nil {
		t.Fatalf("unexpected error on second load: %v", err)
	}
	if cfg1.NodeID != cfg2.NodeID {
		t.Errorf("expected stable NodeID: got %q then %q", cfg1.NodeID, cfg2.NodeID)
	}
}

// TestClusterConfig_AutoNodeID_StableAcrossHostnameChange reproduces #630: a
// hostname change (macOS mDNS renaming, a container getting a fresh hostname
// on restart) must not mint a new node identity for the same data directory.
// A node_id that changes with the hostname mints a "ghost" registration that
// out-lives the old identity, poisoning quorum counting and pinning
// MinReplicatedSeq at its last ack forever.
func TestClusterConfig_AutoNodeID_StableAcrossHostnameChange(t *testing.T) {
	t.Setenv("MUNINN_CLUSTER_ENABLED", "true")
	// Do not set MUNINN_CLUSTER_NODE_ID — exercise auto-derivation.

	dataDir := t.TempDir()

	orig := hostnameFn
	defer func() { hostnameFn = orig }()

	hostnameFn = func() (string, error) { return "MacBookPro-abcd1234", nil }
	cfg1, err := LoadClusterConfig(dataDir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg1.NodeID == "" {
		t.Fatal("expected non-empty auto-generated NodeID")
	}

	// Same machine, same data directory, but the hostname flapped (a real,
	// observed sequence: MacBookPro-<hash> -> Jareds-MacBook-Pro.local-<hash>).
	hostnameFn = func() (string, error) { return "Jareds-MacBook-Pro.local", nil }
	cfg2, err := LoadClusterConfig(dataDir)
	if err != nil {
		t.Fatalf("unexpected error on second load: %v", err)
	}

	if cfg1.NodeID != cfg2.NodeID {
		t.Errorf("node identity must survive a hostname change: got %q then %q", cfg1.NodeID, cfg2.NodeID)
	}
}

func TestClusterConfig_DisabledAlwaysValid(t *testing.T) {
	cfg := ClusterConfig{
		Enabled: false,
		Seeds:   []string{},
		NodeID:  "",
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("expected nil error for disabled cluster, got: %v", err)
	}
}

func TestClusterConfig_MuninnYAML(t *testing.T) {
	dataDir := t.TempDir()
	content := `cluster:
  enabled: true
  node_id: yaml-node
  bind_addr: "0.0.0.0:8474"
  seeds:
    - seed1:8474
    - seed2:8474
  cluster_secret: yamlsecret
  role: primary
  lease_ttl: 15
  heartbeat_ms: 500
`
	if err := os.WriteFile(filepath.Join(dataDir, "muninn.yaml"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadClusterConfig(dataDir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !cfg.Enabled {
		t.Error("expected Enabled=true")
	}
	if cfg.NodeID != "yaml-node" {
		t.Errorf("expected NodeID=yaml-node, got %q", cfg.NodeID)
	}
	if cfg.Role != "primary" {
		t.Errorf("expected Role=primary, got %q", cfg.Role)
	}
	if cfg.LeaseTTL != 15 {
		t.Errorf("expected LeaseTTL=15, got %d", cfg.LeaseTTL)
	}
	if cfg.HeartbeatMS != 500 {
		t.Errorf("expected HeartbeatMS=500, got %d", cfg.HeartbeatMS)
	}
	if len(cfg.Seeds) != 2 {
		t.Errorf("expected 2 seeds, got %d", len(cfg.Seeds))
	}
}

func TestSaveAndLoadClusterConfig(t *testing.T) {
	dir := t.TempDir()
	cfg := ClusterConfig{
		Enabled:       true,
		NodeID:        "node-1",
		BindAddr:      "127.0.0.1:7777",
		Role:          "primary",
		ClusterSecret: "secret",
		HeartbeatMS:   500,
		Seeds:         []string{"10.0.0.2:7777"},
	}
	if err := SaveClusterConfig(dir, cfg); err != nil {
		t.Fatalf("save: %v", err)
	}

	// Check file permissions
	info, err := os.Stat(filepath.Join(dir, "cluster.yaml"))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Fatalf("expected mode 0600, got %04o", perm)
	}

	loaded, err := LoadClusterConfig(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if loaded.NodeID != cfg.NodeID {
		t.Errorf("NodeID: got %q want %q", loaded.NodeID, cfg.NodeID)
	}
	if loaded.BindAddr != cfg.BindAddr {
		t.Errorf("BindAddr: got %q want %q", loaded.BindAddr, cfg.BindAddr)
	}
	if loaded.Enabled != cfg.Enabled {
		t.Errorf("Enabled: got %v want %v", loaded.Enabled, cfg.Enabled)
	}
	if loaded.Role != cfg.Role {
		t.Errorf("Role: got %q want %q", loaded.Role, cfg.Role)
	}
	if loaded.ClusterSecret != cfg.ClusterSecret {
		t.Errorf("ClusterSecret mismatch")
	}
	if loaded.HeartbeatMS != cfg.HeartbeatMS {
		t.Errorf("HeartbeatMS: got %d want %d", loaded.HeartbeatMS, cfg.HeartbeatMS)
	}
	if len(loaded.Seeds) != len(cfg.Seeds) || (len(cfg.Seeds) > 0 && loaded.Seeds[0] != cfg.Seeds[0]) {
		t.Errorf("Seeds: got %v want %v", loaded.Seeds, cfg.Seeds)
	}
}

func TestSaveClusterConfig_EmptyDataDir(t *testing.T) {
	err := SaveClusterConfig("", ClusterConfig{})
	if err == nil {
		t.Fatal("expected error for empty dataDir")
	}
}

func TestClusterConfig_Validate_NegativeTimeoutFields(t *testing.T) {
	base := ClusterConfig{
		Enabled:     true,
		NodeID:      "node-1",
		Seeds:       []string{"seed:8474"},
		Role:        "auto",
		LeaseTTL:    10,
		HeartbeatMS: 1000,
	}

	fields := []struct {
		name string
		set  func(*ClusterConfig)
	}{
		{"quorum_loss_timeout_sec", func(c *ClusterConfig) { c.QuorumLossTimeoutSec = -1 }},
		{"join_token_ttl_min", func(c *ClusterConfig) { c.JoinTokenTTLMin = -1 }},
		{"failover_convergence_timeout_sec", func(c *ClusterConfig) { c.FailoverConvergenceTimeoutSec = -1 }},
		{"handoff_ack_timeout_sec", func(c *ClusterConfig) { c.HandoffAckTimeoutSec = -1 }},
		{"prune_interval_sec", func(c *ClusterConfig) { c.PruneIntervalSec = -1 }},
		{"recon_delay_ms", func(c *ClusterConfig) { c.ReconDelayMs = -1 }},
	}

	for _, f := range fields {
		t.Run(f.name, func(t *testing.T) {
			cfg := base
			f.set(&cfg)
			err := cfg.Validate()
			if err == nil {
				t.Fatalf("expected error for negative %s, got nil", f.name)
			}
			if !strings.Contains(err.Error(), f.name) {
				t.Errorf("expected error to mention %q, got: %v", f.name, err)
			}
		})
	}
}

func TestClusterConfig_Validate_ZeroTimeoutFieldsOK(t *testing.T) {
	cfg := ClusterConfig{
		Enabled:                       true,
		NodeID:                        "node-1",
		Seeds:                         []string{"seed:8474"},
		Role:                          "auto",
		LeaseTTL:                      10,
		HeartbeatMS:                   1000,
		QuorumLossTimeoutSec:          0,
		JoinTokenTTLMin:               0,
		FailoverConvergenceTimeoutSec: 0,
		HandoffAckTimeoutSec:          0,
		PruneIntervalSec:              0,
		ReconDelayMs:                  0,
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("expected zero values to be valid, got: %v", err)
	}
}

func TestClusterConfig_Validate_PositiveTimeoutFieldsOK(t *testing.T) {
	cfg := ClusterConfig{
		Enabled:                       true,
		NodeID:                        "node-1",
		Seeds:                         []string{"seed:8474"},
		Role:                          "auto",
		LeaseTTL:                      10,
		HeartbeatMS:                   1000,
		QuorumLossTimeoutSec:          5,
		JoinTokenTTLMin:               15,
		FailoverConvergenceTimeoutSec: 30,
		HandoffAckTimeoutSec:          5,
		PruneIntervalSec:              60,
		ReconDelayMs:                  2000,
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("expected positive values to be valid, got: %v", err)
	}
}

func TestClusterConfig_InvalidEnvVars(t *testing.T) {
	// Test that bad env vars log warnings but use defaults
	t.Setenv("MUNINN_CLUSTER_LEASE_TTL", "not-a-number")
	t.Setenv("MUNINN_CLUSTER_HEARTBEAT_MS", "also-bad")

	cfg, err := LoadClusterConfig(t.TempDir())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should use defaults, not the bad values
	if cfg.LeaseTTL != 10 {
		t.Errorf("expected LeaseTTL=10 (default), got %d", cfg.LeaseTTL)
	}
	if cfg.HeartbeatMS != 1000 {
		t.Errorf("expected HeartbeatMS=1000 (default), got %d", cfg.HeartbeatMS)
	}
}
