package engine

import (
	"log/slog"
	"math/rand"
	"time"

	"github.com/scrypster/muninndb/internal/storage"
)

// assocWeightRepairVersion is written to the per-vault 0x2E watermark after a
// clean full pass. Bump it if the repair logic ever changes in a way that must
// re-visit already-marked vaults.
const assocWeightRepairVersion uint8 = 1

// runLegacyFullWeightAssocRepair is a one-shot startup pass that relocates
// pre-fix weight-1.0 association keys from the weight-0.0 key position the
// overflowed WeightComplement wrote them at (#756) to the true 1.0 position.
// The per-pair rule and its evidence live on
// storage.RepairLegacyFullWeightAssocKeys.
//
// A vault that completes a clean pass is watermarked (0x2E) and skipped on
// later boots, so a healed vault does not pay a full 0x03 scan forever. A
// one-shot watermark is sound here in a way it is not for most repairs: the
// fixed encoder CANNOT create new damage of this kind, so there is nothing for
// a later pass to find. The residual is the same as #681's: rolling back to a
// pre-fix binary that writes 1.0 edges would strand new ones, and recovering
// would require bumping assocWeightRepairVersion (or deleting the 0x2E marks).
//
// The startup delay is deliberately SHORT — much shorter than the #681 evolve
// repair's 60s, which mirrors runPruneWorker. This repair is racing that very
// worker: the first DecayAssocWeights pass over an unrepaired vault destroys
// the disambiguator the repair depends on (see the storage docstring), and the
// prune worker's first tick is at 60s+jitter. The delay is a courtesy to HNSW
// load, not the safety mechanism; the safety mechanism is runPruneWorker's
// hard gate on assocWeightRepairDone, which makes "decay ran before the repair"
// structurally impossible rather than merely unlikely.
func (e *Engine) runLegacyFullWeightAssocRepair() {
	defer close(e.assocWeightRepairDone)
	defer func() {
		if r := recover(); r != nil {
			if !storage.IsClosedPanic(r) {
				slog.Error("engine: legacy full-weight association repair panicked", "panic", r)
			}
		}
	}()

	timer := time.NewTimer(e.assocWeightRepairDelay)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-e.stopCtx.Done():
		return
	}

	ctx := e.stopCtx
	vaults, err := e.ListVaults(ctx)
	if err != nil {
		if ctx.Err() == nil {
			slog.Warn("engine: legacy full-weight association repair: list vaults", "err", err)
		}
		return
	}

	for _, vaultName := range vaults {
		if ctx.Err() != nil {
			return
		}
		ws := e.store.ResolveVaultPrefix(vaultName)

		mark, err := e.store.GetAssocWeightRepairMark(ctx, ws)
		if err != nil {
			slog.Warn("engine: legacy full-weight association repair: read watermark", "vault", vaultName, "err", err)
			continue
		}
		if mark >= assocWeightRepairVersion {
			continue
		}

		repaired, err := e.store.RepairLegacyFullWeightAssocKeys(ctx, ws)
		clean := err == nil
		if err != nil && ctx.Err() == nil {
			slog.Warn("engine: legacy full-weight association repair failed", "vault", vaultName, "err", err)
		}
		// Log unconditionally — a zero is a signed claim that the vault was
		// swept and found whole, not silence. "repaired" counts pre-fix
		// full-weight pairs relocated. It does NOT count edges a decay pass
		// already clamped or deleted: those are unrepairable AND unidentifiable
		// (#756 correction), so no count is claimed for them.
		slog.Info("engine: legacy full-weight association repair swept vault",
			"vault", vaultName, "repaired", repaired, "clean", clean)
		if !clean {
			continue
		}
		if err := e.store.SetAssocWeightRepairMark(ctx, ws, assocWeightRepairVersion); err != nil {
			slog.Warn("engine: legacy full-weight association repair: write watermark", "vault", vaultName, "err", err)
		}
	}
}

// assocWeightRepairComplete reports whether the one-shot full-weight repair
// pass has finished (cleanly, in error, or by shutdown). runPruneWorker gates
// association decay on it: decaying an unrepaired vault permanently destroys
// the repair's disambiguator, so the ordering is enforced rather than raced.
//
// A nil channel means the engine was constructed without the pass (direct
// struct construction in some tests) — report complete so decay is never
// gated off forever by a channel nobody will close.
func (e *Engine) assocWeightRepairComplete() bool {
	if e.assocWeightRepairDone == nil {
		return true
	}
	select {
	case <-e.assocWeightRepairDone:
		return true
	default:
		return false
	}
}

// defaultAssocWeightRepairDelay returns the startup delay for the repair pass:
// 5s plus jitter. Short on purpose — see runLegacyFullWeightAssocRepair.
func defaultAssocWeightRepairDelay() time.Duration {
	return 5*time.Second + time.Duration(rand.Intn(3))*time.Second
}
