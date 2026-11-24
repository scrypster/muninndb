package engine

import (
	"context"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/auth"
	"github.com/scrypster/muninndb/internal/storage"
)

// reclaimVault is the fixture one reclaim test needs: a registered vault name,
// an optional config expiry, an optional engram, and one minted capability
// (future expiry, so it is live until the sweep revokes it).
func reclaimVault(t *testing.T, eng *Engine, as *auth.Store, store *storage.PebbleStore, name string, cfgExpiresAt *time.Time, withEngram bool) (capID string) {
	t.Helper()
	ctx := context.Background()
	if err := store.WriteVaultName(store.VaultPrefix(name), name); err != nil {
		t.Fatalf("WriteVaultName %s: %v", name, err)
	}
	if err := as.SetVaultConfig(auth.VaultConfig{Name: name, Public: true, ExpiresAt: cfgExpiresAt}); err != nil {
		t.Fatalf("SetVaultConfig %s: %v", name, err)
	}
	if withEngram {
		if _, err := store.WriteEngram(ctx, store.ResolveVaultPrefix(name), &storage.Engram{
			Concept: "reclaim probe", Content: "must survive reclamation",
		}); err != nil {
			t.Fatalf("WriteEngram %s: %v", name, err)
		}
	}
	capExpiry := time.Now().Add(time.Hour) // live credential until revoked
	_, cap, err := as.GenerateCapability(name, "worker", auth.ModeFull, "workflow_vault", &capExpiry)
	if err != nil {
		t.Fatalf("GenerateCapability %s: %v", name, err)
	}
	return cap.ID
}

func vaultRegistered(t *testing.T, store *storage.PebbleStore, name string) bool {
	t.Helper()
	names, err := store.ListVaultNames()
	if err != nil {
		t.Fatalf("ListVaultNames: %v", err)
	}
	for _, n := range names {
		if n == name {
			return true
		}
	}
	return false
}

func vaultConfigured(t *testing.T, as *auth.Store, name string) bool {
	t.Helper()
	cfgs, err := as.ListVaultConfigs()
	if err != nil {
		t.Fatalf("ListVaultConfigs: %v", err)
	}
	for _, c := range cfgs {
		if c.Name == name {
			return true
		}
	}
	return false
}

func capabilityCount(t *testing.T, as *auth.Store, name string) int {
	t.Helper()
	caps, err := as.ListCapabilities(name)
	if err != nil {
		t.Fatalf("ListCapabilities %s: %v", name, err)
	}
	return len(caps)
}

// TestReclaimExpiredWorkflowVaults_DeletesStaleEmptyVault is the core #973
// behavior: a wf-* vault whose expiry has passed and which holds zero engrams
// is deleted — name registration, config, and every capability for it. The
// registry stops growing forever.
func TestReclaimExpiredWorkflowVaults_DeletesStaleEmptyVault(t *testing.T) {
	eng, as, store, cleanup := testEnvWithAuth(t)
	defer cleanup()
	ctx := context.Background()

	past := time.Now().Add(-2 * time.Hour)
	capID := reclaimVault(t, eng, as, store, "wf-gone", &past, false)
	if capabilityCount(t, as, "wf-gone") != 1 {
		t.Fatal("fixture: expected exactly one capability before reclaim")
	}

	eng.reclaimExpiredWorkflowVaults(ctx, []string{"wf-gone"})

	if vaultRegistered(t, store, "wf-gone") {
		t.Error("expired empty wf-* vault still registered after reclaim — the registry leak #973 describes")
	}
	if vaultConfigured(t, as, "wf-gone") {
		t.Error("expired empty wf-* vault config still present after reclaim")
	}
	if got := capabilityCount(t, as, "wf-gone"); got != 0 {
		t.Errorf("expired wf-* vault kept %d capabilities after reclaim, want 0 (cap %s)", got, capID)
	}
}

// TestReclaimExpiredWorkflowVaults_TTLStillActiveSurvives: a wf-* vault inside
// its TTL is not reclaimable, no matter how empty it is.
func TestReclaimExpiredWorkflowVaults_TTLStillActiveSurvives(t *testing.T) {
	eng, as, store, cleanup := testEnvWithAuth(t)
	defer cleanup()
	ctx := context.Background()

	future := time.Now().Add(48 * time.Hour)
	reclaimVault(t, eng, as, store, "wf-alive", &future, false)

	eng.reclaimExpiredWorkflowVaults(ctx, []string{"wf-alive"})

	if !vaultRegistered(t, store, "wf-alive") {
		t.Error("wf-* vault inside its TTL was deleted — only EXPIRED vaults are reclaimable")
	}
	if !vaultConfigured(t, as, "wf-alive") {
		t.Error("wf-* vault inside its TTL lost its config")
	}
	if got := capabilityCount(t, as, "wf-alive"); got != 1 {
		t.Errorf("wf-* vault inside its TTL: %d capabilities, want 1 (untouched)", got)
	}
}

// TestReclaimExpiredWorkflowVaults_ExpiredButNotEmptySurvives is the no-data-loss
// guard: expiry alone is not a delete reason. A wf-* vault past expiry that
// still holds engrams survives whole — name, config, data, capability.
func TestReclaimExpiredWorkflowVaults_ExpiredButNotEmptySurvives(t *testing.T) {
	eng, as, store, cleanup := testEnvWithAuth(t)
	defer cleanup()
	ctx := context.Background()

	past := time.Now().Add(-1 * time.Hour)
	name := "wf-keepsdata"
	reclaimVault(t, eng, as, store, name, &past, true)
	ws := store.ResolveVaultPrefix(name)

	eng.reclaimExpiredWorkflowVaults(ctx, []string{name})

	if !vaultRegistered(t, store, name) {
		t.Fatal("expired-but-non-empty wf-* vault was deleted — data loss: expiry never outranks content")
	}
	if got := store.GetVaultCount(ctx, ws); got != 1 {
		t.Errorf("engram count = %d after reclaim swept a non-empty vault, want 1", got)
	}
	if !vaultConfigured(t, as, name) {
		t.Error("expired-but-non-empty wf-* vault lost its config")
	}
}

// TestReclaimExpiredWorkflowVaults_OperatorVaultImmune: the wf- prefix is the
// structural guard. An operator vault with a past expiry and zero engrams —
// exactly the reclaimable shape minus the prefix — survives untouched, with
// or without an expiry set at all.
func TestReclaimExpiredWorkflowVaults_OperatorVaultImmune(t *testing.T) {
	eng, as, store, cleanup := testEnvWithAuth(t)
	defer cleanup()
	ctx := context.Background()

	past := time.Now().Add(-3 * time.Hour)
	reclaimVault(t, eng, as, store, "opsprod", &past, false) // past expiry, empty
	reclaimVault(t, eng, as, store, "opsplain", nil, false)  // nil expiry, empty
	reclaimVault(t, eng, as, store, "default", &past, false) // the canonical operator vault

	eng.reclaimExpiredWorkflowVaults(ctx, []string{"opsprod", "opsplain", "default"})

	for _, name := range []string{"opsprod", "opsplain", "default"} {
		if !vaultRegistered(t, store, name) {
			t.Errorf("operator vault %q was deleted — the wf- prefix is the guard; an operator vault must be structurally incapable of matching", name)
		}
		if !vaultConfigured(t, as, name) {
			t.Errorf("operator vault %q lost its config", name)
		}
		if got := capabilityCount(t, as, name); got != 1 {
			t.Errorf("operator vault %q: %d capabilities, want 1 (untouched)", name, got)
		}
	}
}

// TestReclaimExpiredWorkflowVaults_NilExpiryNeverDeleted: an absent expiry is
// not a delete reason, even on an empty wf-* vault. Fail-safe, not fail-open.
func TestReclaimExpiredWorkflowVaults_NilExpiryNeverDeleted(t *testing.T) {
	eng, as, store, cleanup := testEnvWithAuth(t)
	defer cleanup()
	ctx := context.Background()

	reclaimVault(t, eng, as, store, "wf-noexp", nil, false)

	eng.reclaimExpiredWorkflowVaults(ctx, []string{"wf-noexp"})

	if !vaultRegistered(t, store, "wf-noexp") {
		t.Error("wf-* vault with nil expiry was deleted — absent expiry must never be a delete reason")
	}
	if got := capabilityCount(t, as, "wf-noexp"); got != 1 {
		t.Errorf("wf-* vault with nil expiry: %d capabilities, want 1 (untouched)", got)
	}
}

// TestReclaimExpiredWorkflowVaults_OnlyTargetVaultTouched: the sweep deletes
// the stale vault and nothing else in the list it was handed.
func TestReclaimExpiredWorkflowVaults_OnlyTargetVaultTouched(t *testing.T) {
	eng, as, store, cleanup := testEnvWithAuth(t)
	defer cleanup()
	ctx := context.Background()

	past := time.Now().Add(-2 * time.Hour)
	future := time.Now().Add(24 * time.Hour)
	reclaimVault(t, eng, as, store, "wf-stale", &past, false)
	reclaimVault(t, eng, as, store, "wf-live", &future, false)
	reclaimVault(t, eng, as, store, "opsprod", &past, false)

	eng.reclaimExpiredWorkflowVaults(ctx, []string{"wf-stale", "wf-live", "opsprod"})

	if vaultRegistered(t, store, "wf-stale") {
		t.Error("stale empty wf-stale was not reclaimed")
	}
	for _, name := range []string{"wf-live", "opsprod"} {
		if !vaultRegistered(t, store, name) {
			t.Errorf("%s was deleted alongside wf-stale — the sweep must touch only its target", name)
		}
	}
}
