package auth

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"

	"github.com/cockroachdb/pebble"
)

// Which unconfigured vaults have been warned about, so each is warned about
// once (noteUnconfiguredVault). The set is bounded: see there.
var (
	warnedUnconfiguredVaults sync.Map     // name (cut to maxWarnedVaultNameBytes) -> struct{}
	warnedUnconfiguredCount  atomic.Int64 // names in the set, or reserved for it
	warnedUnconfiguredFull   atomic.Bool  // the "no more will be logged" notice was written
)

const (
	// maxWarnedUnconfiguredVaults is how many distinct unconfigured vault
	// names are remembered and logged. The warning exists to tell an operator
	// that one of THEIR vaults has no explicit policy, and a deployment has a
	// handful of those. Names past this many are not an operator's vaults, they
	// are whatever callers are sending.
	maxWarnedUnconfiguredVaults = 256

	// maxWarnedVaultNameBytes is how much of a name is remembered and logged:
	// the longest a vault name may be (IsValidVaultName). A longer name cannot
	// belong to a vault; it is remembered and shown by its first bytes.
	maxWarnedVaultNameBytes = 64

	msgUnconfiguredVaultLimit = "more vaults with no explicit config have been asked about than are remembered; further ones are treated as locked without being logged"
)

// noteUnconfiguredVault warns, once per vault, that a vault has no explicit
// configuration and is being treated as locked.
//
// What it keeps is bounded, because the name comes from the caller and is
// looked up BEFORE the caller has shown a credential: a request with no key
// names a vault, on any transport, and the name comes here to find out whether
// that vault is public. Remembering every name asked about, and logging each,
// let any caller grow the server's memory and its log without limit — 2,000
// handshakes naming distinct 32 KiB vaults left 80 MiB behind. So at most
// maxWarnedUnconfiguredVaults names are remembered, each cut to
// maxWarnedVaultNameBytes, which is at most 16 KiB and 257 log lines for the
// life of the process: one per name, and one saying no more will be logged.
//
// The price is that once the set is full, an operator's own unconfigured vault
// asked about for the first time is not named in the log. It is still treated
// as locked; only the hint is lost.
func noteUnconfiguredVault(vault string) {
	name := vault
	if len(name) > maxWarnedVaultNameBytes {
		name = name[:maxWarnedVaultNameBytes] + "…"
	}
	if _, seen := warnedUnconfiguredVaults.Load(name); seen {
		return
	}
	// Take a place before storing, so the set never holds more than the limit
	// however many callers arrive at once.
	if warnedUnconfiguredCount.Add(1) > maxWarnedUnconfiguredVaults {
		warnedUnconfiguredCount.Add(-1)
		if warnedUnconfiguredFull.CompareAndSwap(false, true) {
			slog.Warn(msgUnconfiguredVaultLimit, "remembered", maxWarnedUnconfiguredVaults)
		}
		return
	}
	if _, already := warnedUnconfiguredVaults.LoadOrStore(name, struct{}{}); already {
		warnedUnconfiguredCount.Add(-1)
		return
	}
	slog.Warn("vault has no explicit config — defaulting to locked access (fail-closed); call SetVaultConfig to set an explicit policy",
		"vault", name,
	)
}

// resetUnconfiguredVaultWarnings forgets every name. Tests only.
func resetUnconfiguredVaultWarnings() {
	warnedUnconfiguredVaults.Range(func(k, _ any) bool {
		warnedUnconfiguredVaults.Delete(k)
		return true
	})
	warnedUnconfiguredCount.Store(0)
	warnedUnconfiguredFull.Store(false)
}

// unconfiguredVaultWarningsHeld reports how many names are remembered and how
// many bytes they hold. Tests only.
func unconfiguredVaultWarningsHeld() (names, bytes int) {
	warnedUnconfiguredVaults.Range(func(k, _ any) bool {
		names++
		bytes += len(k.(string))
		return true
	})
	return names, bytes
}

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
		noteUnconfiguredVault(vault)
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
//
// This is where per-vault plasticity lives, so it is the write behind
// PUT /api/admin/vault/{v}/plasticity. On a Lobe it is refused (#596); on the
// Cortex it now enters the replication stream like any engram write.
func (s *Store) SetVaultConfig(cfg VaultConfig) error {
	if err := s.refuseNonLeaderWrite(); err != nil {
		return err
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshal vault config: %w", err)
	}
	return s.set(vaultConfigKey(cfg.Name), data)
}

// RenameVaultConfig moves a vault's config from oldName to newName.
// If no config exists for oldName, this is a no-op (returns nil).
func (s *Store) RenameVaultConfig(oldName, newName string) error {
	if gerr := s.refuseNonLeaderWrite(); gerr != nil {
		return gerr
	}
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
	data, err := json.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("rename vault config: marshal: %w", err)
	}

	// Atomic batch: write new key + delete old key in a single commit.
	// Prevents duplicate config entries if the process crashes mid-operation.
	batch := s.db.NewBatch()
	batch.Set(vaultConfigKey(newName), data, nil)
	batch.Delete(vaultConfigKey(oldName), nil)
	if err := s.commit(batch); err != nil {
		batch.Close()
		return fmt.Errorf("rename vault config: commit: %w", err)
	}
	batch.Close()
	return nil
}

// DeleteVaultConfig removes the vault configuration for the named vault.
// If no config exists for the vault, this is a no-op and returns nil (idempotent).
func (s *Store) DeleteVaultConfig(name string) error {
	if err := s.refuseNonLeaderWrite(); err != nil {
		return err
	}
	return s.del(vaultConfigKey(name))
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
