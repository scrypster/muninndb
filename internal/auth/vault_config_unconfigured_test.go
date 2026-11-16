package auth

import (
	"bytes"
	"log/slog"
	"reflect"
	"strings"
	"testing"

	"github.com/cockroachdb/pebble"
	"github.com/cockroachdb/pebble/vfs"
)

func openNoticeTestDB(t *testing.T) *pebble.DB {
	t.Helper()
	db, err := pebble.Open("", &pebble.Options{FS: vfs.NewMem()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestUnconfiguredVaults_ListsOnlyVaultsWithNoStoredConfig(t *testing.T) {
	s := NewStore(openNoticeTestDB(t))
	for _, cfg := range []VaultConfig{{Name: "orchard", Public: true}, {Name: "ferry", Public: false}} {
		if err := s.SetVaultConfig(cfg); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.UnconfiguredVaults([]string{"quarry", "orchard", "lantern", "ferry"})
	if err != nil {
		t.Fatal(err)
	}
	// A vault configured as locked (ferry) HAS an explicit policy; only vaults
	// with no stored config are reported, sorted.
	if want := []string{"lantern", "quarry"}; !reflect.DeepEqual(got, want) {
		t.Errorf("UnconfiguredVaults = %v, want %v", got, want)
	}
	if got, err := s.UnconfiguredVaults(nil); err != nil || len(got) != 0 {
		t.Errorf("no vaults: got %v, %v", got, err)
	}
}

// Locked-by-default is the secure state. Reading an unconfigured vault's config
// must not raise a WARN: at WARN it printed one line per vault on every start,
// a permanent alarm for a state that is by design. The fail-closed answer
// itself is unchanged.
func TestGetVaultConfig_UnconfiguredVaultIsLockedAndNotAWarning(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(prev)

	s := NewStore(openNoticeTestDB(t))
	const vault = "unconfigured-notice-fixture"
	warnedUnconfiguredVaults.Delete(vault)
	cfg, err := s.GetVaultConfig(vault)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Public {
		t.Fatal("an unconfigured vault must default to locked (fail-closed)")
	}
	out := buf.String()
	if strings.Contains(out, "level=WARN") || strings.Contains(out, "level=ERROR") {
		t.Errorf("unconfigured vault raised a warning; want DEBUG at most:\n%s", out)
	}
	if !strings.Contains(out, "level=DEBUG") || !strings.Contains(out, vault) {
		t.Errorf("the per-vault line should still exist at DEBUG and name the vault:\n%s", out)
	}
}
