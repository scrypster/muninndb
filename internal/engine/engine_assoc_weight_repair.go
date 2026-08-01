package engine

import (
	"context"
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
// an ordering rather than a race between two startup timers.
//
// # Failure policy: a non-clean pass parks decay for the process lifetime
//
// The gate channel closes ONLY on a clean pass. If any vault's repair errored
// (or the vault list could not be read, or the pass panicked), the gate stays
// shut until the process restarts and a later pass completes cleanly.
//
// That is deliberately conservative, and it is the asymmetry that decides it:
// letting decay run over a vault that is still damaged destroys the 0x14
// evidence PERMANENTLY and UNIDENTIFIABLY — the next boot has no watermark, but
// also no pre-clamped edges left to find, so the damage becomes unrecoverable
// and uncountable. Parking decay costs one process lifetime of decay throughput
// on a background sweep whose whole job is slow forgetting. Cheap and
// reversible against permanent and silent: park.
//
// Shutdown is handled separately from completion, via assocWeightRepairExited,
// which the goroutine closes unconditionally. Stop() waits on THAT, so a parked
// gate can never hang shutdown or leak the goroutine.
//
// # Replication and upgrade order
//
// Nothing this pass writes is shipped to followers (see the storage docstring):
// every node repairs its own store, matching the #681 evolve-repair precedent.
// The repair is deterministic and idempotent, so the nodes converge. Two
// rolling-upgrade residuals follow from that, and both are operational, not
// code-fixable here:
//
//   - An old-binary node's prune worker has NO gate, so it can decay its own
//     store and destroy its own evidence before it is ever upgraded. Ordering
//     the rollout leader-first does not save it; only upgrading followers
//     PROMPTLY does. That is the operational guidance.
//   - An upgraded follower that has self-repaired can be re-damaged by an old
//     LEADER: the old leader's Hebbian write ships a delete computed by the
//     buggy encoder, which misses the repaired key position, so the follower is
//     left holding a phantom full-weight edge. The only mitigation is upgrading
//     the LEADER FIRST — that is the documented upgrade order for this change.
func (e *Engine) runLegacyFullWeightAssocRepair() {
	clean := false
	// Declared first so it runs LAST: the recover below must have run before
	// the gate decision is made, so a panic leaves clean == false.
	defer func() {
		if clean {
			close(e.assocWeightRepairDone)
		}
		close(e.assocWeightRepairExited)
	}()
	defer func() {
		if r := recover(); r != nil {
			if !storage.IsClosedPanic(r) {
				slog.Error("engine: legacy full-weight association repair panicked", "panic", r)
			}
		}
	}()
	clean = e.assocWeightRepairPass()
}

// assocWeightRepairPass performs the sweep and reports whether it completed
// CLEANLY over every vault — the sole condition for opening the decay gate.
// Shutdown mid-pass is not clean (the sweep did not finish), but it also does
// not matter: the prune worker is shutting down with it.
func (e *Engine) assocWeightRepairPass() bool {
	timer := time.NewTimer(e.assocWeightRepairDelay)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-e.stopCtx.Done():
		return false
	}

	ctx := e.stopCtx
	vaults, err := e.ListVaults(ctx)
	if err != nil {
		if ctx.Err() == nil {
			logAssocRepairParked("could not list vaults", nil, err)
		}
		return false
	}

	var unrepaired []string
	for _, vaultName := range vaults {
		if ctx.Err() != nil {
			return false
		}
		ws := e.store.ResolveVaultPrefix(vaultName)

		mark, err := e.store.GetAssocWeightRepairMark(ctx, ws)
		if err != nil {
			slog.Warn("engine: legacy full-weight association repair: read watermark", "vault", vaultName, "err", err)
			unrepaired = append(unrepaired, vaultName)
			continue
		}
		if mark >= assocWeightRepairVersion {
			continue
		}

		repaired, err := e.repairAssocWeights(ctx, ws)
		clean := err == nil
		if err != nil && ctx.Err() == nil {
			slog.Warn("engine: legacy full-weight association repair failed", "vault", vaultName, "err", err)
		}
		// Log unconditionally — a zero is a signed claim that the vault was
		// swept and found whole, not silence. "repaired" counts pre-fix
		// full-weight pairs actually relocated and committed. It does NOT count
		// edges a decay pass already clamped or deleted, nor pre-0x14-era pairs
		// with no weight index at all: those are unrepairable AND
		// unidentifiable (#756 correction), so no count is claimed for them.
		// On a non-clean sweep the number is what was relocated before the
		// error, i.e. "attempted", not a completed total.
		slog.Info("engine: legacy full-weight association repair swept vault",
			"vault", vaultName, "repaired", repaired, "clean", clean)
		if !clean {
			unrepaired = append(unrepaired, vaultName)
			continue
		}
		// A watermark-write failure does NOT park decay: the vault's keys are
		// repaired, so decay over it is safe. The only cost is re-scanning it
		// on the next boot, which the pass is built to tolerate.
		if err := e.store.SetAssocWeightRepairMark(ctx, ws, assocWeightRepairVersion); err != nil {
			slog.Warn("engine: legacy full-weight association repair: write watermark", "vault", vaultName, "err", err)
		}
	}

	if len(unrepaired) > 0 {
		logAssocRepairParked("vaults left unrepaired", unrepaired, nil)
		return false
	}
	return true
}

// repairAssocWeights dispatches to the store unless a test has installed a
// double via repairAssocWeightsFn.
func (e *Engine) repairAssocWeights(ctx context.Context, ws [8]byte) (int, error) {
	if e.repairAssocWeightsFn != nil {
		return e.repairAssocWeightsFn(ctx, ws)
	}
	return e.store.RepairLegacyFullWeightAssocKeys(ctx, ws)
}

// logAssocRepairParked emits the one message an operator must not miss: the
// repair did not complete cleanly, so association decay is parked for the rest
// of this process's life to keep the damaged edges identifiable.
func logAssocRepairParked(reason string, vaults []string, err error) {
	slog.Error("engine: legacy full-weight association repair did not complete cleanly — association decay is PARKED for this process lifetime so the repairable evidence is not destroyed; restart to retry the pass",
		"reason", reason, "vaults", vaults, "err", err)
}

// assocWeightRepairComplete reports whether the one-shot full-weight repair
// pass has finished CLEANLY. runPruneWorker gates association decay on it:
// decaying an unrepaired vault permanently destroys the repair's disambiguator,
// so the ordering is enforced rather than raced. A pass that errored never
// opens this gate — see the failure policy on runLegacyFullWeightAssocRepair.
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
