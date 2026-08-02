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
// COG-32, engine side: Engine.Activate must forward the vault's resolved
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
// test for COG-32: a vault with hebbian_enabled:false must report a zero
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
			"Engine.Activate must forward resolved.HebbianEnabled (COG-32)", got)
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

// ---------------------------------------------------------------------------
// The KILL SWITCH: actr_heb_scale = 0.
//
// The scalar multiplies BOTH hebbianBoost and transitionBoost inside softplus,
// so 0 means "no cognitive boost at all". Two layers used to substitute the
// 4.0 default for a configured 0 — Engine.Activate's `resolved.ACTRHebScale > 0`
// guard and activation's `req.ACTRHebScale > 0` guard — which made the
// documented switch a no-op in the hot path (principle #1).
//
// Asserted on the OBSERVABLE, not on the reported component: phase 4 still
// computes hebbian_boost at scale 0 (it is a measurement), but it must not move
// the score. So the same corpus is probed with and without the co-activation
// priming and the resulting scores must be bit-identical.
// ---------------------------------------------------------------------------

// hebScaleProbe is hebReadGateProbe's sibling: it returns the probe row's
// absolute score, optionally priming the activation log first.
func hebScaleProbe(t *testing.T, eng *Engine, vaultName string, prime bool) float64 {
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
	if prime {
		eng.activation.AssocLog().Record(activation.LogEntry{
			VaultID:   wsVaultID(ws),
			At:        time.Now(),
			EngramIDs: []storage.ULID{partnerID},
			Scores:    []float64{1.0},
		})
	}

	resp, err := eng.Activate(ctx, &mbp.ActivateRequest{
		Vault:      vaultName,
		Context:    []string{"kiln firing schedule"},
		MaxResults: 10,
		Threshold:  0.001,
	})
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}
	for _, a := range resp.Activations {
		if a.ID == probeResp.ID {
			// Raw, not AbsoluteScore: AbsoluteScore is
			// min(Raw, ContentMatch, 1) x Confidence, and under the no-op
			// embedder ContentMatch is capped at the FTS weight (0.4), which
			// clamps the prior back out of view. Raw is where the contextual
			// prior is observable.
			return float64(a.ScoreComponents.Raw)
		}
	}
	t.Fatalf("probe engram not among %d activations", len(resp.Activations))
	return 0
}

func hebScaleEnv(t *testing.T, vaultName string, scale *float64) (*Engine, func()) {
	t.Helper()
	eng, as, store, cleanup := testEnvWithAuth(t)
	if err := store.WriteVaultName(store.VaultPrefix(vaultName), vaultName); err != nil {
		t.Fatalf("WriteVaultName: %v", err)
	}
	if err := as.SetVaultConfig(auth.VaultConfig{
		Name:       vaultName,
		Public:     true,
		Plasticity: &auth.PlasticityConfig{ACTRHebScale: scale},
	}); err != nil {
		t.Fatalf("SetVaultConfig: %v", err)
	}
	return eng, cleanup
}

// TestACTRHebScaleZero_KillsTheCognitiveBoost is the end-to-end RED test for
// the kill switch.
func TestACTRHebScaleZero_KillsTheCognitiveBoost(t *testing.T) {
	zero := 0.0
	if r := auth.ResolvePlasticity(&auth.PlasticityConfig{ACTRHebScale: &zero}); r.ACTRHebScale != 0 {
		t.Fatalf("test setup: auth resolved actr_heb_scale %v, want 0", r.ACTRHebScale)
	}

	engPrimed, c1 := hebScaleEnv(t, "heb-scale-zero", &zero)
	defer c1()
	primed := hebScaleProbe(t, engPrimed, "heb-scale-zero", true)

	engCold, c2 := hebScaleEnv(t, "heb-scale-zero", &zero)
	defer c2()
	cold := hebScaleProbe(t, engCold, "heb-scale-zero", false)

	if primed != cold {
		t.Errorf("actr_heb_scale:0 — a co-activated neighbour still moved the score "+
			"(%.12f primed vs %.12f cold). An explicitly configured 0 must not be "+
			"substituted with the 4.0 default (principle #1).", primed, cold)
	}
}

// TestACTRHebScaleDefault_CognitiveBoostStillApplies is the converse control:
// the fix must honor an explicit 0, not neutralise the mechanism for everyone.
func TestACTRHebScaleDefault_CognitiveBoostStillApplies(t *testing.T) {
	engPrimed, c1 := hebScaleEnv(t, "heb-scale-default", nil)
	defer c1()
	primed := hebScaleProbe(t, engPrimed, "heb-scale-default", true)

	engCold, c2 := hebScaleEnv(t, "heb-scale-default", nil)
	defer c2()
	cold := hebScaleProbe(t, engCold, "heb-scale-default", false)

	if primed <= cold {
		t.Errorf("default actr_heb_scale — priming did not raise the score "+
			"(%.12f primed vs %.12f cold); the fixture proves nothing", primed, cold)
	}
}
