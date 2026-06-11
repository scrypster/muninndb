package config

import "testing"

// #522 Step 0: AdvertiseAddr defaults to BindAddr, and an explicit value (env)
// wins — so wildcard binds (0.0.0.0) in containers can advertise a routable addr.
func TestClusterConfig_AdvertiseAddr_DefaultsToBindAddr(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MUNINN_CLUSTER_ENABLED", "true")
	t.Setenv("MUNINN_CLUSTER_NODE_ID", "n1")
	t.Setenv("MUNINN_CLUSTER_BIND_ADDR", "0.0.0.0:8479")

	cfg, err := LoadClusterConfig(dir)
	if err != nil {
		t.Fatalf("LoadClusterConfig: %v", err)
	}
	if cfg.AdvertiseAddr != "0.0.0.0:8479" {
		t.Errorf("AdvertiseAddr should default to BindAddr, got %q", cfg.AdvertiseAddr)
	}
}

func TestClusterConfig_AdvertiseAddr_ExplicitWins(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MUNINN_CLUSTER_ENABLED", "true")
	t.Setenv("MUNINN_CLUSTER_NODE_ID", "n1")
	t.Setenv("MUNINN_CLUSTER_BIND_ADDR", "0.0.0.0:8479")
	t.Setenv("MUNINN_CLUSTER_ADVERTISE_ADDR", "node1:8479")

	cfg, err := LoadClusterConfig(dir)
	if err != nil {
		t.Fatalf("LoadClusterConfig: %v", err)
	}
	if cfg.AdvertiseAddr != "node1:8479" {
		t.Errorf("explicit AdvertiseAddr not honored, got %q", cfg.AdvertiseAddr)
	}
}
