package engine

import (
	"context"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/auth"
	"github.com/scrypster/muninndb/internal/engine/activation"
	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// ---------------------------------------------------------------------------
// COG-31, engine side: Engine.Activate must forward the vault's resolved
// HebbianEnabled to the activation request, so a vault that disables Hebbian
// learning also stops being SCORED by Hebbian edges.
//
// This is the wiring half of the pair; the gate itself is pinned by
// internal/engine/activation/hebbian_gate_test.go. Both are needed: a bool
// defaulting to false is a silent behaviour change for any caller that forgets
// to set it, so the production constructor's assignment must be pinned by a
// test that fails if someone deletes the line.
//
// PRIVACY: every string below is synthetic and authored here.
// ---------------------------------------------------------------------------

// hebReadGateProbe writes a two-engram corpus into vaultName, links them with a
// saturated association, seeds the activation log with the partner, and returns
// the hebbian_boost the probe row reports.
func hebReadGateProbe(t *testing.T, eng *Engine, vaultName string) float64 {
	t.Helper()
	ctx := context.Background()

	probeResp, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:   vaultName,
		Concept: "kiln firing schedule",
		Content: "the bisque kiln ramps at eighty degrees an hour to cone zero four",
	})
	if err != nil {
		t.Fatalf("Write(probe): %v", err)
	}
	partnerResp, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:   vaultName,
		Concept: "glaze mixing ratio",
		Content: "the celadon glaze is mixed at one part ash to two parts feldspar",
	})
	if err != nil {
		t.Fatalf("Write(partner): %v", err)
	}
	awaitFTS(t, eng)

	probeID, err := storage.ParseULID(probeResp.ID)
	if err != nil {
		t.Fatalf("ParseULID(probe): %v", err)
	}
	partnerID, err := storage.ParseULID(partnerResp.ID)
	if err != nil {
		t.Fatalf("ParseULID(partner): %v", err)
	}

	ws := eng.store.ResolveVaultPrefix(vaultName)
	if err := eng.store.WriteAssociation(ctx, ws, probeID, partnerID, &storage.Association{
		TargetID:   partnerID,
		RelType:    storage.RelRelatesTo,
		Weight:     1.0,
		Confidence: 1.0,
		CreatedAt:  time.Now(),
	}); err != nil {
		t.Fatalf("WriteAssociation: %v", err)
	}

	// Deterministic seam: record the partner as just-co-activated directly
	// rather than issuing a prior Activate() and racing the drainLog goroutine.
	eng.activation.AssocLog().Record(activation.LogEntry{
		VaultID:   wsVaultID(ws),
		At:        time.Now(),
		EngramIDs: []storage.ULID{partnerID},
		Scores:    []float64{1.0},
	})

	resp, err := eng.Activate(ctx, &mbp.ActivateRequest{
		Vault:      vaultName,
		Context:    []string{"kiln firing schedule"},
		MaxResults: 10,
		Threshold:  0.001,
		IncludeWhy: true,
	})
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}
	for _, a := range resp.Activations {
		if a.ID == probeResp.ID {
			return float64(a.ScoreComponents.HebbianBoost)
		}
	}
	t.Fatalf("probe engram not among %d activations", len(resp.Activations))
	return 0
}

// TestActivateRequest_WiresHebbianEnabledFromPlasticity is the engine-level RED
// test for COG-31: a vault with hebbian_enabled:false must report a zero
// read-side boost even though a saturated edge and a fresh co-activation exist.
func TestActivateRequest_WiresHebbianEnabledFromPlasticity(t *testing.T) {
	eng, as, store, cleanup := testEnvWithAuth(t)
	defer cleanup()

	const vaultName = "hebbian-off-vault"
	if err := store.WriteVaultName(store.VaultPrefix(vaultName), vaultName); err != nil {
		t.Fatalf("WriteVaultName: %v", err)
	}
	no := false
	if err := as.SetVaultConfig(auth.VaultConfig{
		Name:       vaultName,
		Public:     true,
		Plasticity: &auth.PlasticityConfig{HebbianEnabled: &no},
	}); err != nil {
		t.Fatalf("SetVaultConfig: %v", err)
	}
	if r := auth.ResolvePlasticity(&auth.PlasticityConfig{HebbianEnabled: &no}); r.HebbianEnabled {
		t.Fatal("test setup: HebbianEnabled did not resolve to false")
	}

	if got := hebReadGateProbe(t, eng, vaultName); got != 0 {
		t.Errorf("hebbian_enabled:false vault reported hebbian_boost = %v, want 0 — "+
			"Engine.Activate must forward resolved.HebbianEnabled (COG-31)", got)
	}
}

// TestActivateRequest_HebbianEnabledVaultStillBoosts is the converse control on
// a default-preset vault: the wiring must pass `true` through, not hardcode
// false.
func TestActivateRequest_HebbianEnabledVaultStillBoosts(t *testing.T) {
	eng, as, store, cleanup := testEnvWithAuth(t)
	defer cleanup()

	const vaultName = "hebbian-on-vault"
	if err := store.WriteVaultName(store.VaultPrefix(vaultName), vaultName); err != nil {
		t.Fatalf("WriteVaultName: %v", err)
	}
	if err := as.SetVaultConfig(auth.VaultConfig{Name: vaultName, Public: true}); err != nil {
		t.Fatalf("SetVaultConfig: %v", err)
	}
	if r := auth.ResolvePlasticity(nil); !r.HebbianEnabled {
		t.Fatal("test setup: the default preset does not enable Hebbian")
	}

	if got := hebReadGateProbe(t, eng, vaultName); got <= 0 {
		t.Errorf("default vault reported hebbian_boost = %v, want > 0", got)
	}
}
