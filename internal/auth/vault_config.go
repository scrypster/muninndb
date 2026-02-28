package auth

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"

	"github.com/cockroachdb/pebble"
)

var warnedUnconfiguredVaults sync.Map

// GetVaultConfig returns the config for a vault.
//
// SECURITY NOTE — FAIL-CLOSED BEHAVIOR:
// When no config exists for a vault (pebble.ErrNotFound), this function returns
// {Public: false}. This is a FAIL-CLOSED default: any vault that has never been
// explicitly configured requires an API key for access.
//
// Bootstrap pre-configures the "default" vault as Public: true so that
// first-time users can connect any MCP client without needing an API key.
// Any additional vault created by an operator starts locked and must be
// explicitly opened via SetVaultConfig.
//
// To allow unauthenticated access to a vault, explicitly set Public: true:
//
//	store.SetVaultConfig(auth.VaultConfig{Name: "myvault", Public: true})
func (s *Store) GetVaultConfig(vault string) (VaultConfig, error) {
	data, closer, err := s.db.Get(vaultConfigKey(vault))
	if err != nil {
		// Fail-closed: any vault that has never been explicitly configured
		// requires an API key. Operators should call SetVaultConfig to
		// establish an explicit policy for each vault.
		if _, already := warnedUnconfiguredVaults.LoadOrStore(vault, struct{}{}); !already {
			slog.Warn("vault has no explicit config — defaulting to locked access (fail-closed); call SetVaultConfig to set an explicit policy",
				"vault", vault,
			)
		}
		return VaultConfig{Name: vault, Public: false}, nil
	}
	defer closer.Close()

	var cfg VaultConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return VaultConfig{}, fmt.Errorf("corrupt vault config: %w", err)
	}
	return cfg, nil
}

// SetVaultConfig persists the vault configuration.
func (s *Store) SetVaultConfig(cfg VaultConfig) error {
	data, err := json.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshal vault config: %w", err)
	}
	return s.db.Set(vaultConfigKey(cfg.Name), data, pebble.Sync)
}

// RenameVaultConfig moves a vault's config from oldName to newName.
// If no config exists for oldName, this is a no-op (returns nil).
func (s *Store) RenameVaultConfig(oldName, newName string) error {
	cfg, err := s.GetVaultConfig(oldName)
	if err != nil {
		return nil // no config → no-op
	}
	// Check if config was explicitly stored (not the fail-closed default).
	_, closer, getErr := s.db.Get(vaultConfigKey(oldName))
	if getErr != nil {
		return nil // no persisted config → no-op
	}
	closer.Close()

	cfg.Name = newName
	if err := s.SetVaultConfig(cfg); err != nil {
		return fmt.Errorf("rename vault config: write new: %w", err)
	}
	if err := s.db.Delete(vaultConfigKey(oldName), pebble.Sync); err != nil {
		return fmt.Errorf("rename vault config: delete old: %w", err)
	}
	return nil
}

// ListVaultConfigs returns all explicitly configured vaults.
func (s *Store) ListVaultConfigs() ([]VaultConfig, error) {
	lower := []byte{prefixVaultCfg}
	upper := vaultConfigUpperBound()

	iter, err := s.db.NewIter(&pebble.IterOptions{
		LowerBound: lower,
		UpperBound: upper,
	})
	if err != nil {
		return nil, fmt.Errorf("new iter: %w", err)
	}
	defer iter.Close()

	var cfgs []VaultConfig
	for iter.First(); iter.Valid(); iter.Next() {
		var cfg VaultConfig
		if err := json.Unmarshal(iter.Value(), &cfg); err == nil {
			cfgs = append(cfgs, cfg)
		}
	}
	return cfgs, iter.Error()
}
