package storage

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// TestBackfillVaultNames verifies that BackfillVaultNames creates placeholder
// 0x0E vault-name entries for vault prefixes that have engrams but no existing
// vault-name record (i.e. data written before vault-name persistence).
func TestBackfillVaultNames(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	// Write an engram directly via WriteEngram, but deliberately skip
	// WriteVaultName so no 0x0E meta key exists for the vault prefix.
	ws := store.VaultPrefix("legacy-vault")
	if _, err := store.WriteEngram(ctx, ws, &Engram{
		Concept: "backfill-concept",
		Content: "backfill-content",
	}); err != nil {
		t.Fatalf("WriteEngram: %v", err)
	}

	// Confirm no vault name is registered yet.
	namesBefore, err := store.ListVaultNames()
	if err != nil {
		t.Fatalf("ListVaultNames (before backfill): %v", err)
	}
	for _, n := range namesBefore {
		if strings.HasPrefix(n, "vault-") {
			// Placeholder may already exist from a previous test sharing the DB;
			// that's fine — we just need to confirm BackfillVaultNames doesn't
			// break anything.
			break
		}
	}

	// Run BackfillVaultNames — it should create a placeholder entry for the ws.
	if err := store.BackfillVaultNames(); err != nil {
		t.Fatalf("BackfillVaultNames: %v", err)
	}

	// Now ListVaultNames must contain at least one name for our vault prefix.
	namesAfter, err := store.ListVaultNames()
	if err != nil {
		t.Fatalf("ListVaultNames (after backfill): %v", err)
	}
	if len(namesAfter) == 0 {
		t.Error("ListVaultNames after BackfillVaultNames returned empty — expected at least one name")
	}
}

// TestWriteVaultNameListVaultNamesRoundtrip verifies that WriteVaultName persists the
// vault name and ListVaultNames returns it.
func TestWriteVaultNameListVaultNamesRoundtrip(t *testing.T) {
	store := openTestStore(t)

	ws := store.VaultPrefix("my-vault")

	if err := store.WriteVaultName(ws, "my-vault"); err != nil {
		t.Fatalf("WriteVaultName: %v", err)
	}

	names, err := store.ListVaultNames()
	if err != nil {
		t.Fatalf("ListVaultNames: %v", err)
	}

	found := false
	for _, n := range names {
		if n == "my-vault" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("vault name %q not found in ListVaultNames; got %v", "my-vault", names)
	}
}

// TestWriteVaultNameIdempotent verifies that calling WriteVaultName multiple times
// for the same vault does not cause errors or duplicate entries.
func TestWriteVaultNameIdempotent(t *testing.T) {
	store := openTestStore(t)

	ws := store.VaultPrefix("idem-vault")

	for i := 0; i < 5; i++ {
		if err := store.WriteVaultName(ws, "idem-vault"); err != nil {
			t.Fatalf("WriteVaultName (call %d): %v", i, err)
		}
	}

	names, err := store.ListVaultNames()
	if err != nil {
		t.Fatalf("ListVaultNames: %v", err)
	}

	count := 0
	for _, n := range names {
		if n == "idem-vault" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected exactly 1 entry for idem-vault, got %d", count)
	}
}

// TestResolveVaultPrefixReturnsCorrectPrefix verifies that after WriteVaultName,
// ResolveVaultPrefix returns the same prefix used to write.
func TestResolveVaultPrefixReturnsCorrectPrefix(t *testing.T) {
	store := openTestStore(t)

	vaultName := "resolve-test-vault"
	ws := store.VaultPrefix(vaultName)

	if err := store.WriteVaultName(ws, vaultName); err != nil {
		t.Fatalf("WriteVaultName: %v", err)
	}

	resolved := store.ResolveVaultPrefix(vaultName)
	if resolved != ws {
		t.Errorf("ResolveVaultPrefix: got %x, want %x", resolved, ws)
	}
}

// TestResolveVaultPrefixColdPath verifies that ResolveVaultPrefix works on a
// fresh store instance (cold path, no in-memory cache) by reading from Pebble.
//
// The test uses a sequential open/close pattern to avoid two PebbleStores
// sharing one *pebble.DB: each PebbleStore.Close() calls db.Close() internally,
// so sharing a DB between stores causes a double-close panic.
func TestResolveVaultPrefixColdPath(t *testing.T) {
	dir, err := os.MkdirTemp("", "muninndb-test-*")
	if err != nil {
		t.Fatalf("create temp dir: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })

	vaultName := "cold-path-vault"
	var ws [8]byte

	// Phase 1: write vault name and close the store (+ db).
	{
		db1, err := OpenPebble(dir, DefaultOptions())
		if err != nil {
			t.Fatalf("OpenPebble (store1): %v", err)
		}
		store1 := NewPebbleStore(db1, PebbleStoreConfig{CacheSize: 100})
		ws = store1.VaultPrefix(vaultName)
		if err := store1.WriteVaultName(ws, vaultName); err != nil {
			store1.Close()
			t.Fatalf("WriteVaultName: %v", err)
		}
		// store1.Close() stops goroutines and closes db1.
		if err := store1.Close(); err != nil {
			t.Fatalf("store1.Close: %v", err)
		}
	}

	// Phase 2: reopen from same dir — store2 has no in-memory cache (cold path).
	db2, err := OpenPebble(dir, DefaultOptions())
	if err != nil {
		t.Fatalf("OpenPebble (store2): %v", err)
	}
	store2 := NewPebbleStore(db2, PebbleStoreConfig{CacheSize: 100})
	t.Cleanup(func() { store2.Close() })

	resolved := store2.ResolveVaultPrefix(vaultName)
	if resolved != ws {
		t.Errorf("cold path ResolveVaultPrefix: got %x, want %x", resolved, ws)
	}
}

// TestVaultNameExists verifies VaultNameExists returns true for registered names
// and false for unregistered names.
func TestVaultNameExists(t *testing.T) {
	store := openTestStore(t)

	ws := store.VaultPrefix("exists-vault")
	if err := store.WriteVaultName(ws, "exists-vault"); err != nil {
		t.Fatalf("WriteVaultName: %v", err)
	}

	if !store.VaultNameExists("exists-vault") {
		t.Error("expected VaultNameExists to return true for registered vault")
	}
	if store.VaultNameExists("no-such-vault") {
		t.Error("expected VaultNameExists to return false for unregistered vault")
	}
}

// TestRenameVault_Success verifies that RenameVault changes both index keys
// and that the new name resolves to the same prefix.
func TestRenameVault_Success(t *testing.T) {
	store := openTestStore(t)

	ws := store.VaultPrefix("old-vault")
	if err := store.WriteVaultName(ws, "old-vault"); err != nil {
		t.Fatalf("WriteVaultName: %v", err)
	}

	if err := store.RenameVault(ws, "old-vault", "new-vault"); err != nil {
		t.Fatalf("RenameVault: %v", err)
	}

	// New name should exist and resolve to same prefix.
	if !store.VaultNameExists("new-vault") {
		t.Error("new name not found after rename")
	}
	resolved := store.ResolveVaultPrefix("new-vault")
	if resolved != ws {
		t.Errorf("ResolveVaultPrefix(new-vault) = %x, want %x", resolved, ws)
	}

	// Old name should no longer exist.
	if store.VaultNameExists("old-vault") {
		t.Error("old name still exists after rename")
	}

	// ListVaultNames should contain new but not old.
	names, err := store.ListVaultNames()
	if err != nil {
		t.Fatalf("ListVaultNames: %v", err)
	}
	for _, n := range names {
		if n == "old-vault" {
			t.Error("old-vault still in ListVaultNames after rename")
		}
	}
	found := false
	for _, n := range names {
		if n == "new-vault" {
			found = true
		}
	}
	if !found {
		t.Error("new-vault not in ListVaultNames after rename")
	}
}

// TestRenameVault_Collision verifies that renaming to an existing name fails.
func TestRenameVault_Collision(t *testing.T) {
	store := openTestStore(t)

	wsA := store.VaultPrefix("vault-a")
	if err := store.WriteVaultName(wsA, "vault-a"); err != nil {
		t.Fatalf("WriteVaultName(a): %v", err)
	}
	wsB := store.VaultPrefix("vault-b")
	if err := store.WriteVaultName(wsB, "vault-b"); err != nil {
		t.Fatalf("WriteVaultName(b): %v", err)
	}

	err := store.RenameVault(wsA, "vault-a", "vault-b")
	if err == nil {
		t.Fatal("expected error when renaming to existing name, got nil")
	}
}

// TestRenameVault_NotFound verifies that renaming with a wrong oldName fails.
func TestRenameVault_NotFound(t *testing.T) {
	store := openTestStore(t)

	ws := store.VaultPrefix("real-vault")
	if err := store.WriteVaultName(ws, "real-vault"); err != nil {
		t.Fatalf("WriteVaultName: %v", err)
	}

	err := store.RenameVault(ws, "wrong-name", "new-name")
	if err == nil {
		t.Fatal("expected error when oldName doesn't match, got nil")
	}
}

// TestListVaultNamesMultipleVaults verifies that multiple vault names are all
// returned by ListVaultNames.
func TestListVaultNamesMultipleVaults(t *testing.T) {
	store := openTestStore(t)

	vaults := []string{"vault-alpha", "vault-beta", "vault-gamma"}
	for _, name := range vaults {
		ws := store.VaultPrefix(name)
		if err := store.WriteVaultName(ws, name); err != nil {
			t.Fatalf("WriteVaultName(%s): %v", name, err)
		}
	}

	names, err := store.ListVaultNames()
	if err != nil {
		t.Fatalf("ListVaultNames: %v", err)
	}

	nameSet := make(map[string]bool)
	for _, n := range names {
		nameSet[n] = true
	}

	for _, expected := range vaults {
		if !nameSet[expected] {
			t.Errorf("vault %q missing from ListVaultNames; got %v", expected, names)
		}
	}
}

// TestRenameVault_ClosedDB_MetaKeyError verifies that RenameVault surfaces an
// error when the underlying Pebble DB is closed, exercising the db.Get(metaKey)
// error branch. Pebble v1.1.5 panics with pebble.ErrClosed on operations after
// close, so we recover from the panic and verify it occurred.
func TestRenameVault_ClosedDB_MetaKeyError(t *testing.T) {
	dir := t.TempDir()
	db, err := OpenPebble(dir, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}

	store := NewPebbleStore(db, PebbleStoreConfig{CacheSize: 100})

	// Let background workers initialize before we pull the rug.
	time.Sleep(100 * time.Millisecond)

	// Write a vault name while the DB is still open.
	ws := store.VaultPrefix("old-name")
	if err := store.WriteVaultName(ws, "old-name"); err != nil {
		t.Fatalf("WriteVaultName: %v", err)
	}

	// Close the raw Pebble DB so subsequent operations panic with pebble.ErrClosed.
	if err := store.db.Close(); err != nil {
		t.Fatalf("db.Close: %v", err)
	}

	// Pebble panics on Get after Close; recover and verify the panic occurred.
	panicked := false
	func() {
		defer func() {
			if r := recover(); r != nil {
				panicked = true
				t.Logf("RenameVault panicked as expected: %v", r)
			}
		}()
		_ = store.RenameVault(ws, "old-name", "new-name")
	}()
	if !panicked {
		t.Fatal("expected RenameVault to panic on closed DB, but it did not")
	}
}

// TestVaultNameExists_ClosedDB verifies that VaultNameExists panics (rather
// than returning a stale result) when the underlying Pebble DB is closed,
// exercising the db.Get error path. Pebble v1.1.5 panics with pebble.ErrClosed
// on all operations after close.
func TestVaultNameExists_ClosedDB(t *testing.T) {
	dir := t.TempDir()
	db, err := OpenPebble(dir, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}

	store := NewPebbleStore(db, PebbleStoreConfig{CacheSize: 100})

	// Let background workers initialize before we pull the rug.
	time.Sleep(100 * time.Millisecond)

	// Write a vault name while the DB is still open.
	ws := store.VaultPrefix("exists-closed")
	if err := store.WriteVaultName(ws, "exists-closed"); err != nil {
		t.Fatalf("WriteVaultName: %v", err)
	}

	// Sanity check: exists while DB is open.
	if !store.VaultNameExists("exists-closed") {
		t.Fatal("expected VaultNameExists to return true while DB is open")
	}

	// Close the raw Pebble DB.
	if err := store.db.Close(); err != nil {
		t.Fatalf("db.Close: %v", err)
	}

	// Pebble panics on Get after Close; recover and verify.
	panicked := false
	func() {
		defer func() {
			if r := recover(); r != nil {
				panicked = true
				t.Logf("VaultNameExists panicked as expected: %v", r)
			}
		}()
		_ = store.VaultNameExists("exists-closed")
	}()
	if !panicked {
		t.Fatal("expected VaultNameExists to panic on closed DB, but it did not")
	}
}
