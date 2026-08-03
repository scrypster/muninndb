package engine

import (
	"log/slog"
	"math/rand"
	"time"

	"github.com/scrypster/muninndb/internal/plugin"
	"github.com/scrypster/muninndb/internal/storage"
)

// evolveRepairVersion is written to the per-vault 0x2B watermark after a clean
// full pass. Bump it if the repair logic ever changes in a way that must
// re-visit already-marked vaults.
const evolveRepairVersion uint8 = 1

// runEvolveEntityLinkRepair is a one-shot startup pass that heals the damage
// left by the pre-fix Evolve path (#622): supersede-successors born with zero
// entity links even though their predecessor has them. Soft-delete preserves
// the predecessor's 0x20/0x23/0x21 entries, so the links are copied forward
// rather than reconstructed.
//
// The scan is driven from the soft-deleted side: every evolve predecessor is
// soft-deleted, and the soft-deleted population is small relative to the
// vault. For each soft-deleted engram with entity links, any engram that
// supersedes it and has no entity links of its own receives a copy of the
// links and relationship records in one atomic batch per successor
// (storage.RepairEvolveEntityCarry), so a kill mid-repair can never strand a
// partially-repaired successor for the skip test to mistake for intact.
//
// Supersede chains where every hop was stripped (A→B→C, only A has links) are
// healed by repeating the pass per vault until it fixes nothing — each round
// carries links one hop further along the chain.
//
// A vault that completes a clean pass is watermarked (0x2B) and skipped on
// later boots, so a healed vault does not pay the full soft-deleted scan
// forever. The skip's deliberate cost: damage created AFTER the watermark is
// never repaired. That cannot happen while this build runs (the write-path
// carry is atomic with the evolve), but a rollback to a pre-fix binary that
// evolves memories would strand new stripped successors; recovering from that
// requires bumping evolveRepairVersion (or deleting the 0x2B marks). The pass waits out a startup delay with jitter first — matching
// runPruneWorker — rather than competing with HNSW load and first traffic.
//
// Limitation, by design: a successor with at least one entity link is assumed
// intact and skipped. The repair never merges into or overwrites an existing
// link set (which may be caller-curated), so "repaired=N" counts successors
// that went from zero links to the predecessor's full set, not every
// divergence between predecessor and successor.
func (e *Engine) runEvolveEntityLinkRepair() {
	defer close(e.evolveRepairDone)
	defer func() {
		if r := recover(); r != nil {
			if !storage.IsClosedPanic(r) {
				slog.Error("engine: evolve entity-link repair panicked", "panic", r)
			}
		}
	}()

	delay := e.evolveRepairDelay
	timer := time.NewTimer(delay)
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
			slog.Warn("engine: evolve entity-link repair: list vaults", "err", err)
		}
		return
	}

	for _, vaultName := range vaults {
		if ctx.Err() != nil {
			return
		}
		ws := e.store.ResolveVaultPrefix(vaultName)

		mark, err := e.store.GetEvolveRepairMark(ctx, ws)
		if err != nil {
			slog.Warn("engine: evolve entity-link repair: read watermark", "vault", vaultName, "err", err)
			continue
		}
		if mark >= evolveRepairVersion {
			continue
		}

		repairedEngrams, repairedLinks, clean := e.sweepEvolveRepairVault(vaultName, ws)
		// Log unconditionally — a zero is a signed claim that the vault was
		// swept and found whole, not silence. "repaired" counts successors
		// healed from zero links; successors with any pre-existing link are
		// assumed intact and skipped (see the limitation note above).
		slog.Info("engine: evolve entity-link repair swept vault",
			"vault", vaultName, "repaired", repairedEngrams, "links", repairedLinks, "clean", clean)
		if clean {
			if err := e.store.SetEvolveRepairMark(ctx, ws, evolveRepairVersion); err != nil {
				slog.Warn("engine: evolve entity-link repair: write watermark", "vault", vaultName, "err", err)
			}
		}
	}
}

// sweepEvolveRepairVault runs repair rounds over one vault to fixpoint and
// reports whether the vault may be watermarked. clean is true ONLY when a
// round completed with nothing left to repair — an error or exhausting the
// round cap while rounds were still repairing must NOT count as clean, or the
// watermark would strand the unrepaired tail forever, the exact failure class
// the atomic per-successor repair exists to close.
//
// The cap exists for chains whose successor ULIDs sort BELOW their
// predecessors (clock regression during the vault's pre-fix history): the
// 0x0B scan walks in ULID order, so such a chain heals only one hop per
// round. Ascending chains heal in one round, and supersede cycles
// self-terminate via the has-links skip, so the cap binding at all is rare —
// when it does, the vault simply repairs further on later boots until the
// pass converges and the watermark is finally written.
func (e *Engine) sweepEvolveRepairVault(vaultName string, ws [8]byte) (int, int, bool) {
	repairedEngrams, repairedLinks := 0, 0
	converged := false
	for round := 0; round < 32 && !converged; round++ {
		n, links, err := e.repairEvolveEntityLinksOnce(ws)
		if err != nil {
			if e.stopCtx.Err() == nil {
				slog.Warn("engine: evolve entity-link repair failed", "vault", vaultName, "err", err)
			}
			return repairedEngrams, repairedLinks, false
		}
		repairedEngrams += n
		repairedLinks += links
		converged = n == 0
	}
	return repairedEngrams, repairedLinks, converged
}

// repairEvolveEntityLinksOnce runs one repair round over a vault and reports
// how many successors it healed and how many entity links it copied.
func (e *Engine) repairEvolveEntityLinksOnce(ws [8]byte) (int, int, error) {
	ctx := e.stopCtx
	repaired, copied := 0, 0
	err := e.store.ScanEngramsByState(ctx, ws, storage.StateSoftDeleted, func(oldID storage.ULID) error {
		var predEntities []string
		if err := e.store.ScanEngramEntities(ctx, ws, oldID, func(name string) error {
			predEntities = append(predEntities, name)
			return nil
		}); err != nil {
			return err
		}
		if len(predEntities) == 0 {
			return nil
		}

		var predRels []storage.RelationshipRecord
		if err := e.store.ScanEngramRelationships(ctx, ws, oldID, func(rec storage.RelationshipRecord) error {
			predRels = append(predRels, rec)
			return nil
		}); err != nil {
			return err
		}

		revs, err := e.store.GetReverseAssociations(ctx, ws, oldID, 0)
		if err != nil {
			return err
		}
		for _, rev := range revs {
			if rev.RelType != storage.RelSupersedes {
				continue
			}
			succID := rev.TargetID // GetReverseAssociations: TargetID is the SOURCE engram
			// Existence, the has-links skip test, the batch write and the
			// ledger funding all happen under the successor's stripe lock
			// inside RepairEvolveEntityCarry, atomically per successor.
			did, err := e.store.RepairEvolveEntityCarry(ctx, ws, succID, predEntities, predRels, plugin.DigestEntities)
			if err != nil {
				return err
			}
			if did {
				repaired++
				copied += len(predEntities)
			}
		}
		return nil
	})
	return repaired, copied, err
}

// defaultEvolveRepairDelay returns the startup delay for the repair pass:
// 60s plus jitter, matching runPruneWorker.
func defaultEvolveRepairDelay() time.Duration {
	return 60*time.Second + time.Duration(rand.Intn(10))*time.Second
}
