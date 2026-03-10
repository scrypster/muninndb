package config

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// ClusterConfig holds configuration for multi-node cluster mode.
type ClusterConfig struct {
	Enabled       bool     `yaml:"enabled" json:"enabled"`
	NodeID        string   `yaml:"node_id" json:"node_id"`
	BindAddr      string   `yaml:"bind_addr" json:"bind_addr"`
	Seeds         []string `yaml:"seeds" json:"seeds"`
	ClusterSecret string   `yaml:"cluster_secret" json:"cluster_secret"`
	// "auto" | "primary" | "replica" | "sentinel" | "observer"
	// Default: "auto" — node participates in election and takes whichever role it wins
	Role string `yaml:"role" json:"role"`
	// LeaseTTL is how long a Cortex holds leadership before renewal is required (seconds)
	LeaseTTL int `yaml:"lease_ttl" json:"lease_ttl"`
	// HeartbeatMS is the MSP heartbeat interval in milliseconds
	HeartbeatMS int `yaml:"heartbeat_ms" json:"heartbeat_ms"`
	// SDOWNBeats is the number of missed heartbeats before marking a peer SDOWN. Default: 3.
	SDOWNBeats int `yaml:"sdown_beats" json:"sdown_beats"`
	// CCSIntervalS is the interval in seconds between cross-cluster consistency checks. Default: 30.
	CCSIntervalS int `yaml:"ccs_interval_seconds" json:"ccs_interval_seconds"`
	// ReconcileHeal controls whether reconciliation runs after a partition heals. Default: true.
	ReconcileHeal bool `yaml:"reconcile_on_heal" json:"reconcile_on_heal"`
	// TLS holds mutual-TLS settings for inter-node connections.
	TLS TLSConfig `yaml:"tls" json:"tls"`
	// QuorumLossTimeoutSec is how long a Cortex tolerates lost quorum before
	// self-demoting. Default: 5.
	QuorumLossTimeoutSec int `yaml:"quorum_loss_timeout_sec" json:"quorum_loss_timeout_sec"`
	// JoinTokenTTLMin is the lifetime of join tokens in minutes. Default: 15.
	JoinTokenTTLMin int `yaml:"join_token_ttl_min" json:"join_token_ttl_min"`
	// FailoverConvergenceTimeoutSec is how long graceful failover waits for
	// all Lobes to catch up. Default: 30.
	FailoverConvergenceTimeoutSec int `yaml:"failover_convergence_timeout_sec" json:"failover_convergence_timeout_sec"`
	// HandoffAckTimeoutSec is how long to wait for a HANDOFF_ACK during
	// graceful failover. Default: 5.
	HandoffAckTimeoutSec int `yaml:"handoff_ack_timeout_sec" json:"handoff_ack_timeout_sec"`
	// PruneIntervalSec is how often the Cortex prunes fully-replicated WAL
	// segments. Default: 60.
	PruneIntervalSec int `yaml:"prune_interval_sec" json:"prune_interval_sec"`
	// ReconDelayMs is how long to wait after a Lobe reconnects before
	// triggering reconciliation (ms). Default: 2000.
	ReconDelayMs int `yaml:"recon_delay_ms" json:"recon_delay_ms"`
}

// muninnYAML is the shape of muninn.yaml — only the cluster section is used here.
type muninnYAML struct {
	Cluster ClusterConfig `yaml:"cluster"`
}

// ClusterDefaults returns a ClusterConfig with all default values applied.
// Use this as the base when constructing a new cluster config programmatically
// to avoid accidentally persisting zero-values for required fields such as
// LeaseTTL and HeartbeatMS.
func ClusterDefaults() ClusterConfig { return clusterDefaults() }

// clusterDefaults returns a ClusterConfig with default values applied.
func clusterDefaults() ClusterConfig {
	return ClusterConfig{
		Enabled:                       false,
		Role:                          "auto",
		LeaseTTL:                      10,
		HeartbeatMS:                   1000,
		SDOWNBeats:                    3,
		CCSIntervalS:                  30,
		ReconcileHeal:                 true,
		QuorumLossTimeoutSec:          5,
		JoinTokenTTLMin:               15,
		FailoverConvergenceTimeoutSec: 30,
		HandoffAckTimeoutSec:          5,
		PruneIntervalSec:              60,
		ReconDelayMs:                  2000,
	}
}

// LoadClusterConfig reads the cluster configuration from dataDir.
// It looks for a cluster: section in {dataDir}/muninn.yaml first, then
// falls back to {dataDir}/cluster.yaml. If neither file exists the defaults
// are returned — cluster mode is simply disabled. Environment variables are
// applied after the file (env vars always take priority).
func LoadClusterConfig(dataDir string) (ClusterConfig, error) {
	cfg := clusterDefaults()

	// Try muninn.yaml (cluster: section) then cluster.yaml (top-level).
	if loaded, ok, err := tryLoadMuninnYAML(dataDir); err != nil {
		return cfg, err
	} else if ok {
		cfg = loaded
	} else if loaded, ok, err := tryLoadClusterYAML(dataDir); err != nil {
		return cfg, err
	} else if ok {
		cfg = loaded
	}

	applyEnvOverrides(&cfg)
	autoNodeID(&cfg, dataDir)

	// Default AutoGenDir to dataDir/cluster-tls if TLS is enabled but no dir specified.
	if cfg.TLS.Enabled && cfg.TLS.AutoGenDir == "" {
		cfg.TLS.AutoGenDir = filepath.Join(dataDir, "cluster-tls")
	}

	return cfg, nil
}

// SaveClusterConfig writes cfg to {dataDir}/cluster.yaml with mode 0600.
// Sensitive fields (ClusterSecret) are written as-is; ensure the file
// has appropriate filesystem permissions.
func SaveClusterConfig(dataDir string, cfg ClusterConfig) error {
	if dataDir == "" {
		return fmt.Errorf("config: SaveClusterConfig: dataDir is empty")
	}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("config: marshal cluster config: %w", err)
	}
	path := filepath.Join(dataDir, "cluster.yaml")
	if err := os.WriteFile(path, data, 0600); err != nil {
		return fmt.Errorf("config: write cluster.yaml: %w", err)
	}
	return nil
}

// tryLoadMuninnYAML attempts to read the cluster: stanza from muninn.yaml.
// Returns (config, true, nil) on success, (defaults, false, nil) if file absent.
func tryLoadMuninnYAML(dataDir string) (ClusterConfig, bool, error) {
	path := filepath.Join(dataDir, "muninn.yaml")
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return clusterDefaults(), false, nil
	}
	if err != nil {
		return clusterDefaults(), false, err
	}
	var doc muninnYAML
	// Seed the cluster section with defaults before unmarshalling so that
	// fields absent from the file keep their default values.
	doc.Cluster = clusterDefaults()
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return clusterDefaults(), false, fmt.Errorf("parse %s: %w", path, err)
	}
	return doc.Cluster, true, nil
}

// tryLoadClusterYAML attempts to read a standalone cluster.yaml file.
// Returns (config, true, nil) on success, (defaults, false, nil) if file absent.
func tryLoadClusterYAML(dataDir string) (ClusterConfig, bool, error) {
	path := filepath.Join(dataDir, "cluster.yaml")
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return clusterDefaults(), false, nil
	}
	if err != nil {
		return clusterDefaults(), false, err
	}
	cfg := clusterDefaults()
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return clusterDefaults(), false, fmt.Errorf("parse %s: %w", path, err)
	}
	return cfg, true, nil
}

// applyEnvOverrides overwrites cfg fields with environment variable values
// when those variables are set. Env vars always take priority over YAML.
func applyEnvOverrides(cfg *ClusterConfig) {
	if v := os.Getenv("MUNINN_CLUSTER_ENABLED"); v != "" {
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "true", "1":
			cfg.Enabled = true
		case "false", "0":
			cfg.Enabled = false
		}
	}
	if v := os.Getenv("MUNINN_CLUSTER_NODE_ID"); v != "" {
		cfg.NodeID = v
	}
	if v := os.Getenv("MUNINN_CLUSTER_BIND_ADDR"); v != "" {
		cfg.BindAddr = v
	}
	if v := os.Getenv("MUNINN_CLUSTER_SEEDS"); v != "" {
		var seeds []string
		for _, s := range strings.Split(v, ",") {
			if s = strings.TrimSpace(s); s != "" {
				seeds = append(seeds, s)
			}
		}
		cfg.Seeds = seeds
	}
	if v := os.Getenv("MUNINN_CLUSTER_SECRET"); v != "" {
		cfg.ClusterSecret = v
	}
	if v := os.Getenv("MUNINN_CLUSTER_ROLE"); v != "" {
		cfg.Role = v
	}
	if v := os.Getenv("MUNINN_CLUSTER_LEASE_TTL"); v != "" {
		trimmed := strings.TrimSpace(v)
		if n, err := strconv.Atoi(trimmed); err == nil {
			cfg.LeaseTTL = n
		} else {
			slog.Warn("config: invalid MUNINN_CLUSTER_LEASE_TTL env var, using default",
				"value", trimmed,
				"err", err,
				"default", cfg.LeaseTTL,
			)
		}
	}
	if v := os.Getenv("MUNINN_CLUSTER_HEARTBEAT_MS"); v != "" {
		trimmed := strings.TrimSpace(v)
		if n, err := strconv.Atoi(trimmed); err == nil {
			cfg.HeartbeatMS = n
		} else {
			slog.Warn("config: invalid MUNINN_CLUSTER_HEARTBEAT_MS env var, using default",
				"value", trimmed,
				"err", err,
				"default", cfg.HeartbeatMS,
			)
		}
	}
}

// autoNodeID auto-generates a stable NodeID when cluster is enabled but
// NodeID is not set. The format is {hostname}-{first8charsOfSHA256(dataDir)}.
func autoNodeID(cfg *ClusterConfig, dataDir string) {
	if !cfg.Enabled || cfg.NodeID != "" {
		return
	}
	hostname, err := os.Hostname()
	if err != nil {
		slog.Warn("autoNodeID: hostname lookup failed, falling back to \"node\"", "err", err)
		hostname = "node"
	}
	sum := sha256.Sum256([]byte(dataDir))
	shortHash := fmt.Sprintf("%x", sum[:4]) // 4 bytes = 8 hex chars
	cfg.NodeID = hostname + "-" + shortHash
}

var validRoles = map[string]bool{
	"auto":     true,
	"primary":  true,
	"replica":  true,
	"sentinel": true,
	"observer": true,
}

// Validate checks that the cluster configuration is internally consistent.
// If Enabled is false, Validate always returns nil.
func (c ClusterConfig) Validate() error {
	if !c.Enabled {
		return nil
	}
	if strings.TrimSpace(c.NodeID) == "" {
		return errors.New("cluster: node_id must not be empty when cluster is enabled")
	}
	// Seeds are only required for non-primary roles; a primary starting fresh doesn't need seeds
	if len(c.Seeds) == 0 && c.Role != "primary" {
		return errors.New("cluster: seeds must have at least one entry when cluster is enabled and role is not primary")
	}
	if !validRoles[c.Role] {
		return fmt.Errorf("cluster: invalid role %q — must be one of: auto, primary, replica, sentinel, observer", c.Role)
	}
	if c.LeaseTTL <= 0 {
		return errors.New("cluster: lease_ttl must be > 0")
	}
	if c.HeartbeatMS <= 0 {
		return errors.New("cluster: heartbeat_ms must be > 0")
	}
	if c.QuorumLossTimeoutSec < 0 {
		return errors.New("cluster: quorum_loss_timeout_sec must not be negative")
	}
	if c.JoinTokenTTLMin < 0 {
		return errors.New("cluster: join_token_ttl_min must not be negative")
	}
	if c.FailoverConvergenceTimeoutSec < 0 {
		return errors.New("cluster: failover_convergence_timeout_sec must not be negative")
	}
	if c.HandoffAckTimeoutSec < 0 {
		return errors.New("cluster: handoff_ack_timeout_sec must not be negative")
	}
	if c.PruneIntervalSec < 0 {
		return errors.New("cluster: prune_interval_sec must not be negative")
	}
	if c.ReconDelayMs < 0 {
		return errors.New("cluster: recon_delay_ms must not be negative")
	}
	if c.ClusterSecret == "" {
		slog.Warn("cluster: cluster_secret is empty — running in insecure dev mode")
	}
	return nil
}
