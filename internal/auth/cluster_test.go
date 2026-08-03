package auth

import (
	"errors"
	"testing"
	"time"

	"github.com/cockroachdb/pebble"
	"github.com/cockroachdb/pebble/vfs"
)

func newClusterTestStore(t *testing.T) *Store {
	t.Helper()
	db, err := pebble.Open("", &pebble.Options{FS: vfs.NewMem()})
	if err != nil {
		t.Fatalf("pebble.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return NewStore(db)
}

var errTestNotLeader = errors.New("test: not the cortex")

// TestAuthStore_FollowerRefusesConfigWrites is the #596 issue-2 / #631 claim-2
// half at the auth layer. Vault config (which carries per-vault plasticity),
// API keys and capability tokens all went straight to Pebble via s.db.Set /
// batch.Commit — bypassing PebbleStore, and therefore RepLogAppend — so a
// config write on a Lobe was committed locally and diverged permanently.
func TestAuthStore_FollowerRefusesConfigWrites(t *testing.T) {
	s := newClusterTestStore(t)
	s.SetWriteGate(func() error { return errTestNotLeader })

	exp := time.Now().Add(time.Hour)
	cases := map[string]func() error{
		"SetVaultConfig":      func() error { return s.SetVaultConfig(VaultConfig{Name: "probe", Public: true}) },
		"DeleteVaultConfig":   func() error { return s.DeleteVaultConfig("probe") },
		"RenameVaultConfig":   func() error { return s.RenameVaultConfig("probe", "probe2") },
		"GenerateAPIKey":      func() error { _, _, err := s.GenerateAPIKey("probe", "l", ModeFull, nil); return err },
		"RevokeAPIKey":        func() error { return s.RevokeAPIKey("probe", "AAAAAAAAAAA") },
		"GenerateCapability":  func() error { _, _, err := s.GenerateCapability("probe", "l", ModeFull, "o", &exp); return err },
		"RevokeCapability":    func() error { return s.RevokeCapability("probe", "AAAAAAAAAAA") },
		"ChangeAdminPassword": func() error { return s.ChangeAdminPassword("root", "hunter2") },
	}
	for name, fn := range cases {
		t.Run(name, func(t *testing.T) {
			if err := fn(); !errors.Is(err, errTestNotLeader) {
				t.Errorf("%s on a follower: err = %v, want the write gate's error — "+
					"this configuration change would be committed locally and never "+
					"replicated (#596 issue 2)", name, err)
			}
		})
	}

	// Nothing landed.
	if cfg, _ := s.GetVaultConfig("probe"); cfg.Public {
		t.Error("refused SetVaultConfig still landed: vault is Public")
	}
}

// TestAuthStore_ConfigWritesReplicate pins the other half: on the Cortex, a
// configuration write must ENTER the replication stream. Before the fix every
// auth write bypassed it, so engrams replicated and configuration did not —
// and a failover served the old defaults.
func TestAuthStore_ConfigWritesReplicate(t *testing.T) {
	s := newClusterTestStore(t)
	type entry struct {
		op  uint8
		key []byte
	}
	var log []entry
	s.SetReplicator(func(op uint8, key, value []byte) error {
		k := append([]byte(nil), key...)
		log = append(log, entry{op: op, key: k})
		return nil
	})

	if err := s.SetVaultConfig(VaultConfig{Name: "probe", Public: true}); err != nil {
		t.Fatalf("SetVaultConfig: %v", err)
	}
	if len(log) != 1 || log[0].op != opSet {
		t.Fatalf("SetVaultConfig produced %d replication entries (%+v), want 1 OpSet — "+
			"vault config (and the plasticity it carries) must travel the same "+
			"replication path as engrams (#596 issue 2)", len(log), log)
	}
	if string(log[0].key) != string(vaultConfigKey("probe")) {
		t.Errorf("replicated key = %x, want the vault-config key %x", log[0].key, vaultConfigKey("probe"))
	}

	log = nil
	if _, _, err := s.GenerateAPIKey("probe", "l", ModeFull, nil); err != nil {
		t.Fatalf("GenerateAPIKey: %v", err)
	}
	if len(log) != 1 || log[0].op != opBatch {
		t.Errorf("GenerateAPIKey produced %+v, want 1 OpBatch — a key minted on the "+
			"Cortex must reach the Lobes or it returns 401 there (#596 issue 2)", log)
	}

	log = nil
	if err := s.DeleteVaultConfig("probe"); err != nil {
		t.Fatalf("DeleteVaultConfig: %v", err)
	}
	if len(log) != 1 || log[0].op != opDelete {
		t.Errorf("DeleteVaultConfig produced %+v, want 1 OpDelete", log)
	}
}

// TestAuthStore_StandaloneUnaffected: with neither seam wired (every
// non-cluster server) the store behaves exactly as before.
func TestAuthStore_StandaloneUnaffected(t *testing.T) {
	s := newClusterTestStore(t)
	if err := s.SetVaultConfig(VaultConfig{Name: "probe", Public: true}); err != nil {
		t.Fatalf("standalone SetVaultConfig: %v", err)
	}
	cfg, err := s.GetVaultConfig("probe")
	if err != nil || !cfg.Public {
		t.Fatalf("GetVaultConfig = %+v, %v; want Public", cfg, err)
	}
}
