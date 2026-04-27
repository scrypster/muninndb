package auth_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/scrypster/muninndb/internal/auth"
)

// TestBootstrap_FirstRun verifies that Bootstrap on a fresh store creates an admin
// user, writes a secret file, and configures a default vault.
func TestBootstrap_FirstRun(t *testing.T) {
	store := newTestStore(t)
	tmpDir := t.TempDir()
	secretPath := filepath.Join(tmpDir, "auth_secret")

	secret, err := auth.Bootstrap(store, secretPath)
	if err != nil {
		t.Fatalf("Bootstrap first run: %v", err)
	}
	if len(secret) == 0 {
		t.Error("expected non-empty secret")
	}

	// Admin should exist after bootstrap.
	if !store.AdminExists() {
		t.Error("expected admin to exist after bootstrap")
	}

	// Secret file should have been written.
	data, err := os.ReadFile(secretPath)
	if err != nil {
		t.Fatalf("expected secret file to exist: %v", err)
	}
	if len(data) == 0 {
		t.Error("expected non-empty secret file")
	}

	// Default vault config should be set (public=true on first run).
	cfg, err := store.GetVaultConfig("default")
	if err != nil {
		t.Fatalf("GetVaultConfig: %v", err)
	}
	if !cfg.Public {
		t.Error("expected default vault to be public after bootstrap")
	}
}

// TestBootstrap_Idempotent verifies that calling Bootstrap twice succeeds and
// does not overwrite the admin password set on the first run.
func TestBootstrap_Idempotent(t *testing.T) {
	store := newTestStore(t)
	tmpDir := t.TempDir()
	secretPath := filepath.Join(tmpDir, "auth_secret")

	// First bootstrap — creates root/password admin.
	if _, err := auth.Bootstrap(store, secretPath); err != nil {
		t.Fatalf("Bootstrap first run: %v", err)
	}

	// Change the admin password so we can detect an overwrite.
	if err := store.ChangeAdminPassword("root", "newpassword"); err != nil {
		t.Fatalf("ChangeAdminPassword: %v", err)
	}

	// Second bootstrap — should be a no-op for the admin account.
	if _, err := auth.Bootstrap(store, secretPath); err != nil {
		t.Fatalf("Bootstrap second run: %v", err)
	}

	// The password changed above should still be valid (not overwritten).
	if err := store.ValidateAdmin("root", "newpassword"); err != nil {
		t.Errorf("expected updated password to remain valid after second bootstrap: %v", err)
	}
	// The original default password should no longer work.
	if err := store.ValidateAdmin("root", "password"); err == nil {
		t.Error("expected original default password to be invalid after manual change")
	}
}

// TestBootstrap_EnvPasswordAppliedOnExistingInstance is a regression test for
// MUNINN_ADMIN_PASSWORD being silently ignored on existing (non-first-run) instances.
// When the env var is set at startup and an admin already exists, Bootstrap must
// update the root password so that containerised deployments can rotate credentials.
func TestBootstrap_EnvPasswordAppliedOnExistingInstance(t *testing.T) {
	store := newTestStore(t)
	tmpDir := t.TempDir()
	secretPath := filepath.Join(tmpDir, "auth_secret")

	// First bootstrap — creates root with default "password".
	if _, err := auth.Bootstrap(store, secretPath); err != nil {
		t.Fatalf("Bootstrap first run: %v", err)
	}

	// Simulate operator setting MUNINN_ADMIN_PASSWORD before restarting an
	// existing instance.
	t.Setenv("MUNINN_ADMIN_PASSWORD", "my-secure-password")

	// Second bootstrap — existing instance with env var set.
	if _, err := auth.Bootstrap(store, secretPath); err != nil {
		t.Fatalf("Bootstrap second run: %v", err)
	}

	// The new password from the env var must now be valid.
	if err := store.ValidateAdmin("root", "my-secure-password"); err != nil {
		t.Errorf("regression: MUNINN_ADMIN_PASSWORD not applied to existing instance: %v", err)
	}
	// The old default password must no longer work.
	if err := store.ValidateAdmin("root", "password"); err == nil {
		t.Error("regression: old default password still valid after MUNINN_ADMIN_PASSWORD was set")
	}
}

// TestBootstrap_EnvPasswordNotSetIdempotent verifies that when MUNINN_ADMIN_PASSWORD
// is NOT set, a second bootstrap does not overwrite a manually-changed password.
func TestBootstrap_EnvPasswordNotSetIdempotent(t *testing.T) {
	store := newTestStore(t)
	tmpDir := t.TempDir()
	secretPath := filepath.Join(tmpDir, "auth_secret")

	if _, err := auth.Bootstrap(store, secretPath); err != nil {
		t.Fatalf("Bootstrap first run: %v", err)
	}
	if err := store.ChangeAdminPassword("root", "manual-change"); err != nil {
		t.Fatalf("ChangeAdminPassword: %v", err)
	}

	// Env var is not set — second bootstrap must not touch the password.
	if _, err := auth.Bootstrap(store, secretPath); err != nil {
		t.Fatalf("Bootstrap second run: %v", err)
	}

	if err := store.ValidateAdmin("root", "manual-change"); err != nil {
		t.Errorf("manually-set password was overwritten when MUNINN_ADMIN_PASSWORD is unset: %v", err)
	}
}

// TestBootstrap_SecretFileExists verifies that if a secret file already exists
// Bootstrap reuses it without overwriting.
func TestBootstrap_SecretFileExists(t *testing.T) {
	store := newTestStore(t)
	tmpDir := t.TempDir()
	secretPath := filepath.Join(tmpDir, "auth_secret")

	// Write a known secret before calling Bootstrap.
	existingSecret := []byte("pre-existing-secret-data")
	if err := os.WriteFile(secretPath, existingSecret, 0600); err != nil {
		t.Fatalf("write secret file: %v", err)
	}

	secret, err := auth.Bootstrap(store, secretPath)
	if err != nil {
		t.Fatalf("Bootstrap with existing secret file: %v", err)
	}

	// The returned secret must match what was pre-written.
	if string(secret) != string(existingSecret) {
		t.Errorf("expected Bootstrap to reuse existing secret %q, got %q",
			string(existingSecret), string(secret))
	}

	// The file on disk must be unchanged.
	data, err := os.ReadFile(secretPath)
	if err != nil {
		t.Fatalf("read secret file: %v", err)
	}
	if string(data) != string(existingSecret) {
		t.Errorf("expected secret file to be unchanged, got %q", string(data))
	}
}
