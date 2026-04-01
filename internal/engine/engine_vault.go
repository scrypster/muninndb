package engine

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
)

// ErrVaultNotFound is returned when an operation references a vault that does not exist.
// Use errors.Is to check for this error in callers.
var ErrVaultNotFound = errors.New("vault not found")

// ErrEngramNotFound is returned when an operation references an engram that does not exist.
// Use errors.Is to check for this error in callers.
var ErrEngramNotFound = errors.New("engram not found")

// ErrEngramSoftDeleted is returned when an operation targets an engram that has
// been soft-deleted. Use errors.Is to check for this error in callers.
var ErrEngramSoftDeleted = errors.New("engram is soft-deleted")

// ErrEngramArchived is returned when an operation targets an engram that has
// been archived. Use errors.Is to check for this error in callers.
var ErrEngramArchived = errors.New("engram is archived")

// ErrVaultNameCollision is returned when a rename or clone targets a vault name
// that already exists. Use errors.Is to check for this error in callers.
var ErrVaultNameCollision = errors.New("vault name already exists")

// ClearVault removes all memories from a vault. The vault name remains registered.
// It evicts all in-memory state (HNSW, FTS IDF cache, novelty fingerprints, coherence
// counters, activity tracking) and adjusts the global engramCount.
func (e *Engine) ClearVault(ctx context.Context, vaultName string) error {
	if !e.beginVaultOp() {
		return fmt.Errorf("engine is shutting down")
	}
	defer e.endVaultOp()

	opCtx, stop := e.vaultOpContext(ctx)
	defer stop()

	return e.clearVault(opCtx, vaultName)
}

func (e *Engine) clearVault(ctx context.Context, vaultName string) error {
	mu := e.getVaultMutex(vaultName)
	mu.Lock()
	defer mu.Unlock()

	// Verify the vault exists in the registered name list.
	names, err := e.store.ListVaultNames()
	if err != nil {
		return fmt.Errorf("clear vault: list vault names: %w", err)
	}
	found := false
	for _, n := range names {
		if n == vaultName {
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("vault %q: %w", vaultName, ErrVaultNotFound)
	}

	ws := e.store.VaultPrefix(vaultName)

	// NOTE: Jobs already mid-flush may write ghost FTS entries after the range
	// tombstones land. This is harmless — activation filtering skips engrams
	// with no metadata, and ghost posting list entries are reclaimed by Pebble
	// compaction. A drain barrier was considered but rejected as disproportionate
	// complexity for a microsecond race with no correctness impact.

	// Prevent FTS worker from re-creating keys during the range delete.
	if e.ftsWorker != nil {
		e.ftsWorker.SetClearing(ws, true)
		defer e.ftsWorker.SetClearing(ws, false)
	}

	vaultCount, err := e.store.ClearVault(ctx, ws)
	if err != nil {
		return fmt.Errorf("clear vault %q: %w", vaultName, err)
	}

	e.engramCount.Add(-vaultCount)

	// Floor at zero — guards against counter skew in crash recovery scenarios.
	for {
		cur := e.engramCount.Load()
		if cur >= 0 {
			break
		}
		if e.engramCount.CompareAndSwap(cur, 0) {
			break
		}
	}

	if e.hnswRegistry != nil {
		e.hnswRegistry.ResetVault(ws)
	}
	if e.fts != nil {
		e.fts.InvalidateIDFCache()
	}
	if e.noveltyDet != nil {
		e.noveltyDet.PurgeVault(binary.BigEndian.Uint32(ws[:4]))
	}
	if e.coherence != nil {
		e.coherence.DeleteVault(vaultName)
	}
	if e.activity != nil {
		e.activity.Evict(ws)
	}
	return nil
}

// ErrVaultJobActive is returned by DeleteVault when a clone or merge job is
// currently running against the target vault.
var ErrVaultJobActive = fmt.Errorf("vault has an active clone/merge job in progress")

// DeleteVault removes all memories and the vault name registration.
// Returns ErrVaultJobActive if any clone/merge job is currently running against this vault.
// It calls ClearVault (which adjusts engramCount and in-memory state),
// then deletes the vault name keys from storage.
//
// Note: ws must be captured BEFORE calling ClearVault, because ClearVault
// evicts vaultPrefixCache for the vault name. After ClearVault,
// store.VaultPrefix would still return the SipHash but the name is no longer
// registered — DeleteVaultNameOnly needs the ws captured before eviction.
func (e *Engine) DeleteVault(ctx context.Context, vaultName string) error {
	if !e.beginVaultOp() {
		return fmt.Errorf("engine is shutting down")
	}
	defer e.endVaultOp()

	opCtx, stop := e.vaultOpContext(ctx)
	defer stop()

	return e.deleteVault(opCtx, vaultName)
}

func (e *Engine) deleteVault(ctx context.Context, vaultName string) error {
	// Reject deletion if a clone/merge job is actively writing into this vault
	// (i.e., the vault is the Target of a running job). Deleting a vault that is
	// a Source is allowed — the merge's own post-copy cleanup calls DeleteVault
	// on the source and must not be blocked.
	if e.jobManager != nil && e.jobManager.HasActiveJobTargeting(vaultName) {
		return fmt.Errorf("delete vault %q: %w", vaultName, ErrVaultJobActive)
	}

	// Capture ws BEFORE ClearVault evicts the in-memory name cache.
	ws := e.store.VaultPrefix(vaultName)

	if err := e.clearVault(ctx, vaultName); err != nil {
		return fmt.Errorf("delete vault (clear phase): %w", err)
	}

	if err := e.store.DeleteVaultNameOnly(ctx, vaultName, ws); err != nil {
		// Data is already gone (ClearVault succeeded). Only the name registration
		// remains. Retry of DeleteVault is idempotent — it will re-clear (0 engrams)
		// then attempt DeleteVaultNameOnly again.
		slog.Warn("vault data cleared but name registration not removed",
			"vault", vaultName, "err", err)
		return fmt.Errorf("delete vault (name cleanup): %w", err)
	}

	// NOTE: vaultMu.Delete runs outside the per-vault lock. Any concurrent
	// caller that reaches getVaultMutex after DeleteVaultNameOnly returns will
	// find the vault name gone from storage and abort via ErrVaultNotFound
	// before it ever uses the mutex. The re-insertion/deletion race window is
	// therefore harmless in practice.
	e.vaultMu.Delete(vaultName)

	// Auth config: remove config entry if present.
	if e.authStore != nil {
		if err := e.authStore.DeleteVaultConfig(vaultName); err != nil {
			slog.Warn("delete vault: auth config cleanup failed", "vault", vaultName, "err", err)
		}
	}

	return nil
}

// RenameVault atomically renames a vault. This is a metadata-only operation —
// no engram data is moved or modified. Returns ErrVaultNotFound if oldName
// doesn't exist, ErrVaultJobActive if a clone/merge job targets the vault,
// or an error if newName already exists.
func (e *Engine) RenameVault(ctx context.Context, oldName, newName string) error {
	if !e.beginVaultOp() {
		return fmt.Errorf("engine is shutting down")
	}
	defer e.endVaultOp()

	e.vaultOpsMu.Lock()
	defer e.vaultOpsMu.Unlock()

	// Validate oldName exists and newName doesn't.
	names, err := e.store.ListVaultNames()
	if err != nil {
		return fmt.Errorf("rename vault: list names: %w", err)
	}
	var oldFound, newFound bool
	for _, n := range names {
		if n == oldName {
			oldFound = true
		}
		if n == newName {
			newFound = true
		}
	}
	if !oldFound {
		return fmt.Errorf("vault %q: %w", oldName, ErrVaultNotFound)
	}
	if newFound {
		return fmt.Errorf("vault %q: %w", newName, ErrVaultNameCollision)
	}

	// Reject if a clone/merge job is targeting this vault.
	if e.jobManager != nil && e.jobManager.HasActiveJobTargeting(oldName) {
		return fmt.Errorf("rename vault %q: %w", oldName, ErrVaultJobActive)
	}

	ws := e.store.ResolveVaultPrefix(oldName)

	// Storage: atomic batch rename.
	if err := e.store.RenameVault(ws, oldName, newName); err != nil {
		return fmt.Errorf("rename vault storage: %w", err)
	}

	// Auth config: move config entry if present.
	if e.authStore != nil {
		if err := e.authStore.RenameVaultConfig(oldName, newName); err != nil {
			slog.Warn("rename vault: auth config rename failed", "err", err)
		}
	}

	// Coherence: move counters.
	if e.coherence != nil {
		e.coherence.RenameVault(oldName, newName)
	}

	// Per-vault mutex: move entry from old to new name.
	if mu, ok := e.vaultMu.LoadAndDelete(oldName); ok {
		e.vaultMu.Store(newName, mu)
	}

	return nil
}
