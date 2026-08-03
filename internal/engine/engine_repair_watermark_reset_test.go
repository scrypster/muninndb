package engine

import (
	"context"
	"errors"
	"testing"
)

// TestResetRepairWatermark_Evolve/AssocWeight/All are the #761 reproduction:
// before this method existed there was no operator path to re-arm a repair
// watermark short of bumping the repair-version constant (a recompile) or
// ClearVault (which drops the vault's data too).
func TestResetRepairWatermark_Evolve(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	const vaultName = "reset-watermark-evolve"
	ws := eng.store.VaultPrefix(vaultName)
	if err := eng.store.WriteVaultName(ws, vaultName); err != nil {
		t.Fatalf("WriteVaultName: %v", err)
	}
	if err := eng.store.SetEvolveRepairMark(ctx, ws, 5); err != nil {
		t.Fatalf("SetEvolveRepairMark: %v", err)
	}

	if err := eng.ResetRepairWatermark(ctx, vaultName, RepairWatermarkEvolve); err != nil {
		t.Fatalf("ResetRepairWatermark: %v", err)
	}

	got, err := eng.store.GetEvolveRepairMark(ctx, ws)
	if err != nil {
		t.Fatalf("GetEvolveRepairMark: %v", err)
	}
	if got != 0 {
		t.Errorf("evolve mark after reset: got %d, want 0", got)
	}
}

func TestResetRepairWatermark_AssocWeight(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	const vaultName = "reset-watermark-assocweight"
	ws := eng.store.VaultPrefix(vaultName)
	if err := eng.store.WriteVaultName(ws, vaultName); err != nil {
		t.Fatalf("WriteVaultName: %v", err)
	}
	if err := eng.store.SetAssocWeightRepairMark(ctx, ws, 4); err != nil {
		t.Fatalf("SetAssocWeightRepairMark: %v", err)
	}

	if err := eng.ResetRepairWatermark(ctx, vaultName, RepairWatermarkAssocWeight); err != nil {
		t.Fatalf("ResetRepairWatermark: %v", err)
	}

	got, err := eng.store.GetAssocWeightRepairMark(ctx, ws)
	if err != nil {
		t.Fatalf("GetAssocWeightRepairMark: %v", err)
	}
	if got != 0 {
		t.Errorf("assoc_weight mark after reset: got %d, want 0", got)
	}
}

func TestResetRepairWatermark_All(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	const vaultName = "reset-watermark-all"
	ws := eng.store.VaultPrefix(vaultName)
	if err := eng.store.WriteVaultName(ws, vaultName); err != nil {
		t.Fatalf("WriteVaultName: %v", err)
	}
	if err := eng.store.SetEvolveRepairMark(ctx, ws, 5); err != nil {
		t.Fatalf("SetEvolveRepairMark: %v", err)
	}
	if err := eng.store.SetAssocWeightRepairMark(ctx, ws, 4); err != nil {
		t.Fatalf("SetAssocWeightRepairMark: %v", err)
	}

	if err := eng.ResetRepairWatermark(ctx, vaultName, RepairWatermarkAll); err != nil {
		t.Fatalf("ResetRepairWatermark: %v", err)
	}

	if got, err := eng.store.GetEvolveRepairMark(ctx, ws); err != nil || got != 0 {
		t.Errorf("evolve mark after reset-all: got %d, err %v; want 0", got, err)
	}
	if got, err := eng.store.GetAssocWeightRepairMark(ctx, ws); err != nil || got != 0 {
		t.Errorf("assoc_weight mark after reset-all: got %d, err %v; want 0", got, err)
	}
}

// TestResetRepairWatermark_UnknownVault must refuse cleanly rather than
// silently succeed against a vault that was never registered.
func TestResetRepairWatermark_UnknownVault(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	err := eng.ResetRepairWatermark(ctx, "no-such-vault", RepairWatermarkAll)
	if !errors.Is(err, ErrVaultNotFound) {
		t.Fatalf("ResetRepairWatermark on unknown vault: err = %v, want ErrVaultNotFound", err)
	}
}

// TestResetRepairWatermark_UnknownTarget must refuse an unrecognized target
// string rather than silently doing nothing or doing "all".
func TestResetRepairWatermark_UnknownTarget(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	const vaultName = "reset-watermark-unknown-target"
	ws := eng.store.VaultPrefix(vaultName)
	if err := eng.store.WriteVaultName(ws, vaultName); err != nil {
		t.Fatalf("WriteVaultName: %v", err)
	}

	err := eng.ResetRepairWatermark(ctx, vaultName, RepairWatermarkKind("bogus"))
	if !errors.Is(err, ErrUnknownRepairWatermark) {
		t.Fatalf("ResetRepairWatermark with unknown target: err = %v, want ErrUnknownRepairWatermark", err)
	}
}
