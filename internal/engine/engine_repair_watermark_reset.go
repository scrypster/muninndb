package engine

import (
	"context"
	"fmt"
)

// RepairWatermarkKind names which per-vault repair watermark #761's operator
// reset targets.
type RepairWatermarkKind string

const (
	// RepairWatermarkEvolve is the 0x2B evolve entity-link repair mark (#681).
	RepairWatermarkEvolve RepairWatermarkKind = "evolve"
	// RepairWatermarkAssocWeight is the 0x2E pre-fix full-weight association
	// repair mark (#756).
	RepairWatermarkAssocWeight RepairWatermarkKind = "assoc_weight"
	// RepairWatermarkAll resets both.
	RepairWatermarkAll RepairWatermarkKind = "all"
)

// ErrUnknownRepairWatermark reports an unrecognized RepairWatermarkKind.
var ErrUnknownRepairWatermark = fmt.Errorf("unknown repair watermark target, want %q, %q, or %q",
	RepairWatermarkEvolve, RepairWatermarkAssocWeight, RepairWatermarkAll)

// ResetRepairWatermark deletes the named repair watermark(s) for vaultName so
// the next boot's startup pass re-scans the vault instead of trusting a prior
// clean pass (#761).
//
// Both 0x2B and 0x2E share the same documented rollback residual: running a
// pre-fix binary after the watermark is written lets new damage accrue that
// the watermark then masks, and until now the only recovery on record was
// bumping the repair-version constant (a recompile) or ClearVault, which drops
// the vault's data along with the mark. Deleting the mark is neither: both
// repair passes are idempotent by design (a re-run over an already-clean vault
// is a fast no-op scan, verified by test in both #681 and #756), so this is
// always safe to call, whether or not the vault actually needs it.
//
// Deliberately NOT gated by the cluster single-writer check: both repair
// passes are documented as node-LOCAL — "every node running this build does
// its own startup pass" (evolve_repair.go) — so an operator resetting a
// watermark is repairing THIS node's own copy, not originating a replicated
// client write. In a cluster, an operator who suspects several nodes are
// affected calls this once per node.
func (e *Engine) ResetRepairWatermark(ctx context.Context, vaultName string, which RepairWatermarkKind) error {
	if !e.beginVaultOp() {
		return fmt.Errorf("engine is shutting down")
	}
	defer e.endVaultOp()

	mu := e.getVaultMutex(vaultName)
	mu.Lock()
	defer mu.Unlock()

	found, err := e.ensureVaultRegistered(vaultName)
	if err != nil {
		return fmt.Errorf("reset repair watermark: vault lookup: %w", err)
	}
	if !found {
		return fmt.Errorf("vault %q: %w", vaultName, ErrVaultNotFound)
	}

	ws := e.store.ResolveVaultPrefix(vaultName)
	switch which {
	case RepairWatermarkEvolve:
		return e.store.DeleteEvolveRepairMark(ctx, ws)
	case RepairWatermarkAssocWeight:
		return e.store.DeleteAssocWeightRepairMark(ctx, ws)
	case RepairWatermarkAll:
		if err := e.store.DeleteEvolveRepairMark(ctx, ws); err != nil {
			return fmt.Errorf("reset repair watermark: evolve: %w", err)
		}
		if err := e.store.DeleteAssocWeightRepairMark(ctx, ws); err != nil {
			return fmt.Errorf("reset repair watermark: assoc_weight: %w", err)
		}
		return nil
	default:
		return fmt.Errorf("%w: got %q", ErrUnknownRepairWatermark, which)
	}
}
