package engine

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/scrypster/muninndb/internal/auth"
)

// reclaimExpiredWorkflowVaults reclaims workflow vaults whose configured
// expiry has passed (#973): muninn_create_workflow_vault stamps an expiry on
// the vault config, the working preset evaporates the engrams after 7 days,
// and this sweep removes what is left — the name registration, the config,
// and every capability — so the vault registry cannot grow forever.
//
// It is a method, not a block inside runPruneWorker, for the same reason as
// decayAllVaults: the gate chain below must be unit-testable directly. Do not
// test this through the 60s timer.
//
// Per vault, ALL of these must hold before anything is deleted:
//   - auth.IsWorkflowVaultName — the wf- prefix is the structural guard; an
//     operator vault ("default", "production", ...) is incapable of matching.
//   - the vault config carries a non-nil ExpiresAt. An ABSENT expiry is never
//     a delete reason (fail-safe, not fail-open).
//   - the expiry has passed (now is not before it).
//   - the vault holds zero engrams (VaultHasEngrams: a key-only scan of the
//     0x01 range, limit 1 — NOT GetVaultCount, whose cached count can read
//     stale, and not any time-bounded id range, which a caller-backdated
//     created_at can escape; either false-empty would delete real data).
//     Expiry alone never outranks content — a vault past expiry with data in
//     it survives whole.
//   - no active clone/merge job targets the vault.
//
// Ordering inside a reclaim: capabilities first, vault delete last. Each
// RevokeCapability gates on refuseNonLeaderWrite and DeleteVault on
// refuseWrite, so a cluster follower refuses at the first mutation and no
// data is lost; the delete is last and idempotent, so a partial failure
// (some caps revoked, delete not reached) retries cleanly on the next tick.
// A capability that vanished mid-sweep is already-done, not a failure.
//
// Cluster residual, pre-existing and shared with the admin REST delete: auth
// writes (config, capability records) ride the replication log (auth
// SetReplicator), but the 0x0E/0x0F vault-name keys and ClearVault's range
// tombstones do not — a follower learns the config and caps are gone, not
// that the name was. Closing that is not this change's job.
func (e *Engine) reclaimExpiredWorkflowVaults(ctx context.Context, vaults []string) {
	// Single-writer gate (#596), same as decayAllVaults: on a Lobe this sweep
	// must not attempt the writes at all — the leader's reclamation reaches
	// followers through replication. Gating up front also keeps a follower
	// from WARNing every tick about refusals that are normal operation.
	if err := e.refuseNonLeaderWrite(); err != nil {
		slog.Debug("workflow vault reclaim skipped: not the cluster leader", "err", err)
		return
	}
	if e.authStore == nil {
		return
	}
	for _, name := range vaults {
		if !auth.IsWorkflowVaultName(name) {
			continue
		}
		cfg, err := e.authStore.GetVaultConfig(name)
		if err != nil {
			slog.Warn("workflow vault reclaim: config read failed", "vault", name, "err", err)
			continue
		}
		if cfg.ExpiresAt == nil {
			continue // absent expiry is never a delete reason
		}
		if time.Now().Before(*cfg.ExpiresAt) {
			continue
		}
		has, err := e.store.VaultHasEngrams(ctx, e.store.ResolveVaultPrefix(name))
		if err != nil {
			// A probe that cannot run must not delete: skip this vault this
			// tick and let the failure say itself (one WARN per attempt).
			slog.Warn("workflow vault reclaim: emptiness probe failed", "vault", name, "err", err)
			continue
		}
		if has {
			continue // data present: expiry does not outrank content
		}
		if e.jobManager != nil && e.jobManager.HasActiveJobTargeting(name) {
			continue
		}

		caps, err := e.authStore.ListCapabilities(name)
		if err != nil {
			slog.Warn("workflow vault reclaim: listing capabilities failed", "vault", name, "err", err)
			continue
		}
		revoked := 0
		aborted := false
		for _, c := range caps {
			if err := e.authStore.RevokeCapability(name, c.ID); err != nil {
				if errors.Is(err, auth.ErrCapabilityNotFound) {
					continue // already gone — idempotent
				}
				// Abort the vault, not the sweep: deleting now would orphan
				// the unrevoked cap records. Next tick re-lists what is left.
				slog.Warn("workflow vault reclaim: capability revoke failed", "vault", name, "cap", c.ID, "err", err)
				aborted = true
				break
			}
			revoked++
		}
		if aborted {
			continue
		}
		if err := e.DeleteVault(ctx, name); err != nil {
			slog.Warn("workflow vault reclaim: delete failed", "vault", name, "err", err)
			continue
		}
		slog.Info("reclaimed workflow vault",
			"vault", name,
			"reason", "expiry passed and vault holds zero engrams",
			"capabilities_revoked", revoked)
	}
}
