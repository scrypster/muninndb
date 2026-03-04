package engine

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/auth"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// writeReq is a helper that builds a WriteRequest for the given vault, concept, and content.
func writeReq(vault, concept, content string) *mbp.WriteRequest {
	return &mbp.WriteRequest{
		Vault:   vault,
		Concept: concept,
		Content: content,
	}
}

// activateReq is a helper that builds an ActivateRequest for the given vault and query.
func activateReq(vault, query string) *mbp.ActivateRequest {
	return &mbp.ActivateRequest{
		Vault:      vault,
		Context:    []string{query},
		MaxResults: 20,
		Threshold:  0.0,
	}
}

func TestEngineClearVault_MemoriesGone(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	if _, err := eng.Write(ctx, writeReq("clear-me", "quantum entanglement", "some content about quantum")); err != nil {
		t.Fatal(err)
	}
	// Let FTS worker flush.
	time.Sleep(300 * time.Millisecond)

	if err := eng.ClearVault(ctx, "clear-me"); err != nil {
		t.Fatalf("ClearVault: %v", err)
	}

	resp, err := eng.Activate(ctx, activateReq("clear-me", "quantum"))
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Activations) > 0 {
		t.Errorf("expected 0 activations after ClearVault, got %d", len(resp.Activations))
	}

	// Also verify FTS posting lists are cleared
	ws := eng.store.VaultPrefix("clear-me")
	ftsResults, err := eng.fts.Search(context.Background(), ws, "quantum", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(ftsResults) > 0 {
		t.Errorf("expected FTS to return nothing after ClearVault, got %d results", len(ftsResults))
	}

	// Vault name should still be registered after ClearVault.
	vaults, err := eng.ListVaults(ctx)
	if err != nil {
		t.Fatalf("ListVaults: %v", err)
	}
	found := false
	for _, v := range vaults {
		if v == "clear-me" {
			found = true
		}
	}
	if !found {
		t.Error("ClearVault should preserve vault registration")
	}
}

func TestEngineDeleteVault_VaultNotListed(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	if _, err := eng.Write(ctx, writeReq("to-delete", "some concept", "content")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)

	if err := eng.DeleteVault(ctx, "to-delete"); err != nil {
		t.Fatalf("DeleteVault: %v", err)
	}

	vaults, err := eng.ListVaults(ctx)
	if err != nil {
		t.Fatalf("ListVaults: %v", err)
	}
	for _, v := range vaults {
		if v == "to-delete" {
			t.Error("deleted vault still appears in ListVaults")
		}
	}
}

func TestEngineDeleteVault_GlobalEngramCountDecreases(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	eng.Write(ctx, writeReq("vault-keep", "keep1", "c"))
	eng.Write(ctx, writeReq("vault-keep", "keep2", "c"))
	eng.Write(ctx, writeReq("vault-del", "del1", "c"))
	eng.Write(ctx, writeReq("vault-del", "del2", "c"))
	eng.Write(ctx, writeReq("vault-del", "del3", "c"))
	time.Sleep(300 * time.Millisecond)

	beforeCount := eng.engramCount.Load()
	if err := eng.DeleteVault(ctx, "vault-del"); err != nil {
		t.Fatalf("DeleteVault: %v", err)
	}
	afterCount := eng.engramCount.Load()

	// Global engramCount must strictly decrease after deleting a non-empty vault.
	if afterCount >= beforeCount {
		t.Errorf("engramCount should decrease after DeleteVault: before=%d after=%d", beforeCount, afterCount)
	}
	// Count must not go negative — the floor guard in ClearVault prevents this
	// even in crash-recovery scenarios where the persistent count may diverge.
	// We do not assert an exact -3 delta because the in-memory counter is seeded
	// from a persistent scan at startup and can skew if prior tests left state.
	if afterCount < 0 {
		t.Errorf("engramCount went negative after DeleteVault: %d (floor guard failed)", afterCount)
	}
}

func TestEngineClearVault_NotFound(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	err := eng.ClearVault(ctx, "does-not-exist")
	if err == nil {
		t.Error("expected error for unknown vault, got nil")
	}
}

func TestEngineDeleteVault_NotFound(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	err := eng.DeleteVault(ctx, "does-not-exist")
	if err == nil {
		t.Error("expected error for unknown vault, got nil")
	}
}

// TestClearVault_Idempotent verifies that calling ClearVault twice on the same
// vault does not return an error on the second call and leaves the vault empty.
func TestClearVault_Idempotent(t *testing.T) {
	eng, _, store, cleanup := testEnvWithAuth(t)
	defer cleanup()
	ctx := context.Background()

	const vaultName = "idempotent-clear-vault"

	// Write 3 engrams using eng.Write — this also registers the vault name.
	for i := 0; i < 3; i++ {
		if _, err := eng.Write(ctx, writeReq(vaultName, "concept", "content")); err != nil {
			t.Fatalf("Write[%d]: %v", i, err)
		}
	}

	ws := store.VaultPrefix(vaultName)

	// Verify we have at least 3 engrams (may be more if engine background init wrote extras).
	if count := store.GetVaultCount(ctx, ws); count < 3 {
		t.Fatalf("expected at least 3 engrams before first ClearVault, got %d", count)
	}

	// First ClearVault — must succeed.
	if err := eng.ClearVault(ctx, vaultName); err != nil {
		t.Fatalf("first ClearVault: %v", err)
	}

	if count := store.GetVaultCount(ctx, ws); count != 0 {
		t.Errorf("expected 0 engrams after first ClearVault, got %d", count)
	}

	// Second ClearVault on an already-empty vault — must also succeed (idempotent).
	if err := eng.ClearVault(ctx, vaultName); err != nil {
		t.Fatalf("second ClearVault (idempotent): %v", err)
	}

	if count := store.GetVaultCount(ctx, ws); count != 0 {
		t.Errorf("expected 0 engrams after second ClearVault, got %d", count)
	}
}

// TestEngineRenameVault_Success verifies that a renamed vault's engrams are
// accessible under the new name.
func TestEngineRenameVault_Success(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	if _, err := eng.Write(ctx, writeReq("rename-src", "photosynthesis process", "plants convert light")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)

	if err := eng.RenameVault(ctx, "rename-src", "rename-dst"); err != nil {
		t.Fatalf("RenameVault: %v", err)
	}

	// New name should be listed.
	vaults, err := eng.ListVaults(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var foundNew, foundOld bool
	for _, v := range vaults {
		if v == "rename-dst" {
			foundNew = true
		}
		if v == "rename-src" {
			foundOld = true
		}
	}
	if !foundNew {
		t.Error("renamed vault not found in ListVaults")
	}
	if foundOld {
		t.Error("old vault name still in ListVaults after rename")
	}

	// Engrams should be accessible under new name.
	resp, err := eng.Activate(ctx, activateReq("rename-dst", "photosynthesis"))
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Activations) == 0 {
		t.Error("expected activations under new vault name, got 0")
	}
}

// TestEngineRenameVault_NotFound verifies that renaming a nonexistent vault returns an error.
func TestEngineRenameVault_NotFound(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	err := eng.RenameVault(ctx, "no-such-vault", "anything")
	if err == nil {
		t.Fatal("expected error for nonexistent vault, got nil")
	}
}

// TestEngineRenameVault_Collision verifies that renaming to an existing vault name fails.
func TestEngineRenameVault_Collision(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	eng.Write(ctx, writeReq("vault-x", "c", "d"))
	eng.Write(ctx, writeReq("vault-y", "e", "f"))
	time.Sleep(200 * time.Millisecond)

	err := eng.RenameVault(ctx, "vault-x", "vault-y")
	if err == nil {
		t.Fatal("expected collision error, got nil")
	}
}

// TestEngineRenameVault_SameName verifies behavior when old and new name are the same.
func TestEngineRenameVault_SameName(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	eng.Write(ctx, writeReq("same-vault", "c", "d"))
	time.Sleep(200 * time.Millisecond)

	err := eng.RenameVault(ctx, "same-vault", "same-vault")
	if err == nil {
		t.Fatal("expected error for same-name rename, got nil")
	}
}

func TestEngineClearVault_CoherenceGone(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	eng.Write(ctx, writeReq("coh-vault", "test concept", "content"))
	time.Sleep(200 * time.Millisecond)

	// Confirm coherence entry exists before clearing (Snapshots returns entries for known vaults).
	var hadEntry bool
	if eng.coherence != nil {
		for _, snap := range eng.coherence.Snapshots() {
			if snap.VaultName == "coh-vault" {
				hadEntry = true
				break
			}
		}
		if !hadEntry {
			t.Log("coherence entry not yet populated — may be due to timing; test continues")
		}
	}

	eng.ClearVault(ctx, "coh-vault")

	// After ClearVault the coherence entry should be absent from Snapshots.
	if eng.coherence != nil {
		for _, snap := range eng.coherence.Snapshots() {
			if snap.VaultName == "coh-vault" {
				t.Error("coherence entry should be removed after ClearVault")
			}
		}
	}
}

// TestEngineRenameVault_AuthConfigMoved verifies that RenameVault moves the
// auth vault config from the old name to the new name.
func TestEngineRenameVault_AuthConfigMoved(t *testing.T) {
	eng, authStore, _, cleanup := testEnvWithAuth(t)
	defer cleanup()
	ctx := context.Background()

	// Write an engram to register the vault.
	if _, err := eng.Write(ctx, writeReq("auth-rename-src", "concept", "content")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)

	// Set a vault config on the source vault.
	if err := authStore.SetVaultConfig(auth.VaultConfig{Name: "auth-rename-src", Public: true}); err != nil {
		t.Fatalf("SetVaultConfig: %v", err)
	}

	// Rename the vault.
	if err := eng.RenameVault(ctx, "auth-rename-src", "auth-rename-dst"); err != nil {
		t.Fatalf("RenameVault: %v", err)
	}

	// Verify the config was moved to the new name.
	cfg, err := authStore.GetVaultConfig("auth-rename-dst")
	if err != nil {
		t.Fatalf("GetVaultConfig(dst): %v", err)
	}
	if cfg.Name != "auth-rename-dst" {
		t.Errorf("expected config Name=%q, got %q", "auth-rename-dst", cfg.Name)
	}
	if !cfg.Public {
		t.Error("expected config Public=true after rename, got false")
	}
}

// TestEngineRenameVault_CoherenceCountersMoved verifies that RenameVault
// transfers coherence counters from old name to new name.
func TestEngineRenameVault_CoherenceCountersMoved(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	// Write a few engrams to build up coherence counters.
	for i := 0; i < 3; i++ {
		if _, err := eng.Write(ctx, writeReq("coh-rename-src", "concept", "content")); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(300 * time.Millisecond)

	if eng.coherence == nil {
		t.Skip("coherence is nil — cannot test coherence rename")
	}

	// Snapshot the source vault's coherence counters.
	srcSnap := eng.coherence.GetOrCreate("coh-rename-src").Snapshot("coh-rename-src")
	if srcSnap.TotalEngrams <= 0 {
		t.Fatalf("expected TotalEngrams > 0 before rename, got %d", srcSnap.TotalEngrams)
	}
	beforeEngrams := srcSnap.TotalEngrams

	// Rename the vault.
	if err := eng.RenameVault(ctx, "coh-rename-src", "coh-rename-dst"); err != nil {
		t.Fatalf("RenameVault: %v", err)
	}

	// Verify the counters are now under the new name.
	dstSnap := eng.coherence.GetOrCreate("coh-rename-dst").Snapshot("coh-rename-dst")
	if dstSnap.TotalEngrams != beforeEngrams {
		t.Errorf("expected TotalEngrams=%d after rename, got %d", beforeEngrams, dstSnap.TotalEngrams)
	}
}

// TestEngineRenameVault_VaultMuMoved verifies that the per-vault mutex is
// moved from the old name to the new name during a rename, so that subsequent
// operations on the new vault name succeed.
func TestEngineRenameVault_VaultMuMoved(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	// Write an engram to register the vault.
	if _, err := eng.Write(ctx, writeReq("mu-rename-src", "concept", "content")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)

	// Force a mutex entry to exist for the source vault by calling ClearVault
	// (which calls getVaultMutex internally). Write again to re-populate data.
	if err := eng.ClearVault(ctx, "mu-rename-src"); err != nil {
		t.Fatalf("ClearVault (pre-rename): %v", err)
	}
	if _, err := eng.Write(ctx, writeReq("mu-rename-src", "concept2", "content2")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)

	// Capture the original mutex pointer so we can verify identity after rename.
	origMu := eng.getVaultMutex("mu-rename-src")

	// Rename the vault.
	if err := eng.RenameVault(ctx, "mu-rename-src", "mu-rename-dst"); err != nil {
		t.Fatalf("RenameVault: %v", err)
	}

	// Verify the mutex was moved (same pointer), not recreated.
	newMu := eng.getVaultMutex("mu-rename-dst")
	if newMu != origMu {
		t.Error("expected renamed vault mutex to be the same pointer as the original")
	}

	// Verify ClearVault works on the new name — this exercises the mutex path.
	if err := eng.ClearVault(ctx, "mu-rename-dst"); err != nil {
		t.Fatalf("ClearVault on renamed vault: %v", err)
	}
}

// TestEngineRenameVault_ClosedDB verifies that RenameVault fails when the
// underlying Pebble DB is closed. Pebble panics with ErrClosed rather than
// returning an error, so we recover and verify the panic value.
func TestEngineRenameVault_ClosedDB(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup() // safe — PebbleStore.Close is idempotent via sync.Once
	ctx := context.Background()

	// Write an engram to register the vault.
	if _, err := eng.Write(ctx, writeReq("db-close-vault", "c", "d")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)

	// Stop background workers first to avoid panics from closed DB.
	eng.Stop()
	// Close the underlying Pebble DB so that ListVaultNames will fail.
	eng.store.Close()

	// Pebble panics (rather than returning an error) when the DB is closed,
	// so we recover the panic and verify it contains the expected message.
	var panicked bool
	var panicVal any
	func() {
		defer func() {
			if r := recover(); r != nil {
				panicked = true
				panicVal = r
			}
		}()
		eng.RenameVault(ctx, "db-close-vault", "new-vault")
	}()

	if !panicked {
		t.Fatal("expected panic on closed DB, but RenameVault returned normally")
	}
	msg := fmt.Sprintf("%v", panicVal)
	if !strings.Contains(msg, "closed") {
		t.Errorf("expected panic message containing 'closed', got: %v", panicVal)
	}
}

// TestDeleteVault_CleansAuthStore verifies that after DeleteVault, the vault
// name no longer appears in authStore.ListVaultConfigs. This is the primary
// regression test for the ghost vault bug: DeleteVault must clean up both the
// engine data store and the auth config store.
func TestDeleteVault_CleansAuthStore(t *testing.T) {
	eng, authStore, _, cleanup := testEnvWithAuth(t)
	defer cleanup()
	ctx := context.Background()

	const vaultName = "auth-delete-vault"

	// Register the vault by writing an engram.
	if _, err := eng.Write(ctx, writeReq(vaultName, "concept", "content")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	time.Sleep(300 * time.Millisecond)

	// Explicitly configure the vault in authStore so there is an entry to clean up.
	if err := authStore.SetVaultConfig(auth.VaultConfig{Name: vaultName, Public: true}); err != nil {
		t.Fatalf("SetVaultConfig: %v", err)
	}

	// Confirm the config entry exists before deletion.
	cfgs, err := authStore.ListVaultConfigs()
	if err != nil {
		t.Fatalf("ListVaultConfigs before delete: %v", err)
	}
	found := false
	for _, c := range cfgs {
		if c.Name == vaultName {
			found = true
		}
	}
	if !found {
		t.Fatal("expected vault config to be present before DeleteVault")
	}

	// Delete the vault.
	if err := eng.DeleteVault(ctx, vaultName); err != nil {
		t.Fatalf("DeleteVault: %v", err)
	}

	// The auth config entry must be gone after DeleteVault.
	cfgs, err = authStore.ListVaultConfigs()
	if err != nil {
		t.Fatalf("ListVaultConfigs after delete: %v", err)
	}
	for _, c := range cfgs {
		if c.Name == vaultName {
			t.Errorf("vault config still present in authStore after DeleteVault — ghost vault bug")
		}
	}
}

// TestDeleteVault_NotFoundAfterDelete verifies that calling DeleteVault a second
// time on an already-deleted vault returns an error, not "vault still exists".
// This tests the observable symptom of the ghost vault bug: if authStore cleanup
// is missing, the vault would re-appear in ListVaults and the second delete would
// either succeed (masking the ghost) or fail in an unexpected way.
func TestDeleteVault_NotFoundAfterDelete(t *testing.T) {
	eng, _, _, cleanup := testEnvWithAuth(t)
	defer cleanup()
	ctx := context.Background()

	const vaultName = "double-delete-vault"

	// Register and delete the vault.
	if _, err := eng.Write(ctx, writeReq(vaultName, "concept", "content")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	time.Sleep(300 * time.Millisecond)

	if err := eng.DeleteVault(ctx, vaultName); err != nil {
		t.Fatalf("first DeleteVault: %v", err)
	}

	// Second delete must return an error (vault not found).
	err := eng.DeleteVault(ctx, vaultName)
	if err == nil {
		t.Fatal("expected error on second DeleteVault, got nil")
	}

	// The vault must not appear in ListVaults — no ghost entry.
	vaults, listErr := eng.ListVaults(ctx)
	if listErr != nil {
		t.Fatalf("ListVaults: %v", listErr)
	}
	for _, v := range vaults {
		if v == vaultName {
			t.Errorf("deleted vault still appears in ListVaults — ghost vault bug")
		}
	}
}

// TestDeleteVault_VaultMuEntryRemoved verifies that the vaultMu sync.Map entry
// is deleted when a vault is deleted, preventing unbounded map growth.
func TestDeleteVault_VaultMuEntryRemoved(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	const vaultName = "vaultmu-delete-vault"

	if _, err := eng.Write(ctx, writeReq(vaultName, "concept", "content")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	time.Sleep(300 * time.Millisecond)

	// Populate the vaultMu entry.
	eng.getVaultMutex(vaultName)
	if _, ok := eng.vaultMu.Load(vaultName); !ok {
		t.Fatal("expected vaultMu entry to exist after getVaultMutex")
	}

	if err := eng.DeleteVault(ctx, vaultName); err != nil {
		t.Fatalf("DeleteVault: %v", err)
	}

	if _, ok := eng.vaultMu.Load(vaultName); ok {
		t.Error("expected vaultMu entry to be removed after DeleteVault")
	}
}

// TestEngineRenameVault_JobActive verifies that renaming a vault with an
// active clone/merge job targeting it returns ErrVaultJobActive.
func TestEngineRenameVault_JobActive(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	// Write an engram to register the vault.
	if _, err := eng.Write(ctx, writeReq("job-active-vault", "concept", "content")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)

	// Create a running job that targets this vault (simulates an in-progress clone).
	job, err := eng.jobManager.Create("clone", "some-source", "job-active-vault")
	if err != nil {
		t.Fatalf("jobManager.Create: %v", err)
	}
	// Don't Complete the job — leave it running.

	// Attempt to rename — should be rejected.
	err = eng.RenameVault(ctx, "job-active-vault", "new-name")
	if err == nil {
		t.Fatal("expected ErrVaultJobActive, got nil")
	}
	if !strings.Contains(err.Error(), "active clone/merge job") {
		t.Errorf("expected ErrVaultJobActive in error, got: %v", err)
	}

	// Clean up: complete the job so the engine can shut down cleanly.
	eng.jobManager.Complete(job)
}
