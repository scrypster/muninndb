package storage

import (
	"context"
	"testing"
)

// TestDeleteEvolveRepairMark_ResetsToZero and TestDeleteAssocWeightRepairMark_ResetsToZero
// pin #761's operator escape hatch: an operator must be able to force the next
// boot's startup pass to re-scan a vault instead of trusting a stale clean
// pass, without recompiling the repair-version constant or dropping the
// vault's data via ClearVault.
func TestDeleteEvolveRepairMark_ResetsToZero(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("evolve-watermark-reset")

	if err := store.SetEvolveRepairMark(ctx, ws, 3); err != nil {
		t.Fatalf("SetEvolveRepairMark: %v", err)
	}
	got, err := store.GetEvolveRepairMark(ctx, ws)
	if err != nil || got != 3 {
		t.Fatalf("mark before reset: got %d, err %v; want 3", got, err)
	}

	if err := store.DeleteEvolveRepairMark(ctx, ws); err != nil {
		t.Fatalf("DeleteEvolveRepairMark: %v", err)
	}

	got, err = store.GetEvolveRepairMark(ctx, ws)
	if err != nil {
		t.Fatalf("GetEvolveRepairMark after reset: %v", err)
	}
	if got != 0 {
		t.Errorf("mark after reset: got %d, want 0 (never repaired) so the next boot re-scans", got)
	}
}

func TestDeleteAssocWeightRepairMark_ResetsToZero(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("assoc-weight-watermark-reset")

	if err := store.SetAssocWeightRepairMark(ctx, ws, 2); err != nil {
		t.Fatalf("SetAssocWeightRepairMark: %v", err)
	}
	got, err := store.GetAssocWeightRepairMark(ctx, ws)
	if err != nil || got != 2 {
		t.Fatalf("mark before reset: got %d, err %v; want 2", got, err)
	}

	if err := store.DeleteAssocWeightRepairMark(ctx, ws); err != nil {
		t.Fatalf("DeleteAssocWeightRepairMark: %v", err)
	}

	got, err = store.GetAssocWeightRepairMark(ctx, ws)
	if err != nil {
		t.Fatalf("GetAssocWeightRepairMark after reset: %v", err)
	}
	if got != 0 {
		t.Errorf("mark after reset: got %d, want 0 (never repaired) so the next boot re-scans", got)
	}
}

// TestDeleteRepairMark_AbsentMarkIsNoOp: deleting a mark that was never set
// must not error — an operator resetting "all" vaults should not have to know
// in advance which ones actually need it.
func TestDeleteRepairMark_AbsentMarkIsNoOp(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("never-repaired-vault")

	if err := store.DeleteEvolveRepairMark(ctx, ws); err != nil {
		t.Errorf("DeleteEvolveRepairMark on an absent mark: %v, want nil", err)
	}
	if err := store.DeleteAssocWeightRepairMark(ctx, ws); err != nil {
		t.Errorf("DeleteAssocWeightRepairMark on an absent mark: %v, want nil", err)
	}
}
