package engine

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"

	"github.com/scrypster/muninndb/internal/auth"
)

// ErrVaultNotFound is returned when an operation references a vault that does not exist.
// Use errors.Is to check for this error in callers.
var ErrVaultNotFound = errors.New("vault not found")

// ErrEngramNotFound is returned when an operation references an engram that does not exist.
// Use errors.Is to check for this error in callers.
var ErrEngramNotFound = errors.New("engram not found")

// ErrInvalidArgument is returned when a caller-supplied scalar is out of the
// representable range (NaN, Inf, etc.). REST/gRPC handlers map it to 400/INVALID_ARGUMENT.
// Use errors.Is to check for this error in callers.
var ErrInvalidArgument = errors.New("invalid argument")

// ErrSelfContradiction is returned when AdjustConfidence is asked to mark an
// engram as contradicting itself (id == other with hasContra=true). The
// contradiction graph is irreflexive by construction.
var ErrSelfContradiction = errors.New("self-contradiction rejected")

// ErrEngramSoftDeleted is returned when an operation targets an engram that has
// been soft-deleted. Use errors.Is to check for this error in callers.
var ErrEngramSoftDeleted = errors.New("engram is soft-deleted")

// ErrEngramArchived is returned when an operation targets an engram that has
// been archived. Use errors.Is to check for this error in callers.
var ErrEngramArchived = errors.New("engram is archived")

// ErrVaultNameCollision is returned when a rename or clone targets a vault name
// that already exists. Use errors.Is to check for this error in callers.
var ErrVaultNameCollision = errors.New("vault name already exists")

// ErrInvalidID is returned when a caller passes an ID that cannot be parsed as
// a valid ULID. Use errors.Is to check for this error in callers; REST handlers
// map it to HTTP 400 Bad Request.
var ErrInvalidID = errors.New("invalid engram id")

// ErrInvalidRequest is returned when a caller passes a field value that is
// syntactically valid but semantically out of range (e.g. a CreatedAt timestamp
// that is before the project epoch or too far in the future). REST handlers map
// it to HTTP 422 Unprocessable Entity.
var ErrInvalidRequest = errors.New("invalid request")

// ErrAppendForbidden is returned when an append-mode credential attempts an
// operation that modifies or deletes an EXISTING engram, entity, or lease.
// Append-mode may create new memories and read; it may not destroy or overwrite.
// Enforced here at the engine (via refuseAppend) so the guarantee holds on every
// transport — the MCP dispatch gate alone would leave REST/gRPC/MBP open.
var ErrAppendForbidden = errors.New("append-mode credential cannot modify or delete existing memories")

// refuseAppend returns ErrAppendForbidden when the request credential is
// append-mode. EVERY engine method that modifies or deletes an existing engram/
// entity/lease/enrichment must call this at its top. It is the single chokepoint
// for the append guarantee at the engine layer; TestAppendMode_RefusesEveryDestructiveOp
// exercises each such method so a newly-added destructive method missing this
// call fails CI (principle #6: pin the derived guarantee, don't trust a blacklist).
//
// Accepted residuals — all reinforcement, never destruction. refuseAppend guards
// the destructive surface (overwrite/evolve/archive/re-trust/delete of existing
// content/confidence/state/tags/lifecycle); it deliberately does NOT guard the
// "strengthens with use" side effects that both the write and read paths drive on
// EXISTING engrams:
//   - additive remember on a content-hash duplicate → TouchAccess (#682).
//   - the read path (Read/Activate/recall) under an append credential is NOT
//     forced read-only (resolveReadOnly forces it only for observe), so it drives
//     RecordFeedback (per-vault adaptive scoring weights), TouchAccess (access
//     count / last-access), and Hebbian LTP + PAS on existing associations.
//
// None overwrite content/confidence/state/tags/lifecycle or delete — they only
// reinforce, which is the product promise's first word. Documented identically on
// auth.ModeAppend. (If future work needs append reads to leave existing state
// byte-identical, force resolveReadOnly for append too; today it does not.)
//
// Scope note on the census: TestAppendMode_MethodCensus forces every exported
// method into a bucket (real anti-rot — it caught Restore/PruneVault), but it does
// not behaviorally prove the read-only/infra members are non-destructive; that
// classification is a human judgment. More rot-resistant than a checklist, not
// structurally rot-proof.
func (e *Engine) refuseAppend(ctx context.Context) error {
	if auth.AppendFromContext(ctx) {
		return ErrAppendForbidden
	}
	return nil
}

// ClearVault removes all memories from a vault. The vault name remains registered.
// It evicts all in-memory state (HNSW, FTS IDF cache, novelty fingerprints, coherence
// counters, activity tracking) and adjusts the global engramCount.
func (e *Engine) ClearVault(ctx context.Context, vaultName string) error {
	if err := e.refuseAppend(ctx); err != nil {
		return err
	}
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

	// Use ResolveVaultPrefix so renamed vaults (ws ≠ siphash(currentName))
	// are cleared at their actual workspace. With raw VaultPrefix(name) here,
	// ClearVault on a renamed vault would silently range-delete an empty
	// prefix and leave the real engrams orphaned.
	ws := e.store.ResolveVaultPrefix(vaultName)

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
// Note: ws is resolved via ResolveVaultPrefix BEFORE calling ClearVault,
// because ClearVault evicts vaultPrefixCache for the vault name. After
// eviction, a subsequent ResolveVaultPrefix call would still read the
// persisted 0x0F index — but for renamed vaults we need the index lookup
// (not raw SipHash) to find the real ws, so we capture it up front.
func (e *Engine) DeleteVault(ctx context.Context, vaultName string) error {
	if err := e.refuseAppend(ctx); err != nil {
		return err
	}
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
	// ResolveVaultPrefix so renamed vaults (ws ≠ siphash(currentName))
	// have their actual workspace passed to DeleteVaultNameOnly.
	ws := e.store.ResolveVaultPrefix(vaultName)

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

// ensureVaultRegistered checks whether vaultName appears in the Pebble 0x0E
// name index. If it does not, it checks the auth store — a vault created via
// `muninn vault create` only writes an auth config entry until the first engram
// is stored, leaving a gap between the auth registry and the Pebble index.
// When found exclusively in the auth store the name is written into the Pebble
// index so subsequent admin operations (reindex-fts, vault export, etc.) can
// find it. Returns false only when the vault is absent from both registries.
func (e *Engine) ensureVaultRegistered(name string) (bool, error) {
	names, err := e.store.ListVaultNames()
	if err != nil {
		return false, err
	}
	for _, n := range names {
		if n == name {
			return true, nil
		}
	}
	// Fall back to the auth store which only holds explicitly configured vaults.
	// GetVaultConfig is fail-closed (returns a default for unknown vaults), so use
	// ListVaultConfigs which exclusively returns persisted entries.
	if e.authStore != nil {
		cfgs, lErr := e.authStore.ListVaultConfigs()
		if lErr == nil {
			for _, cfg := range cfgs {
				if cfg.Name == name {
					ws := e.store.ResolveVaultPrefix(name)
					if wErr := e.store.WriteVaultName(ws, name); wErr != nil {
						slog.Warn("vault registry repair failed", "vault", name, "err", wErr)
					}
					return true, nil
				}
			}
		}
	}
	return false, nil
}

// RegisterVaultName writes the vault name into the Pebble 0x0E name index.
// Called by the admin REST handler when a vault is created so that admin
// operations (reindex-fts, vault export, vault clear) can find it immediately,
// even before the first engram has been written.
func (e *Engine) RegisterVaultName(name string) error {
	ws := e.store.ResolveVaultPrefix(name)
	return e.store.WriteVaultName(ws, name)
}

// VaultNameExists reports whether a vault with the given name is registered.
// Delegates to PebbleStore.VaultNameExists (0x0F name→prefix index lookup).
// Used by muninn_create_workflow_vault to reject names that already exist,
// preventing cross-vault config clobber (RFC #597 RedTeam finding).
func (e *Engine) VaultNameExists(name string) bool {
	return e.store.VaultNameExists(name)
}

// RenameVault atomically renames a vault. This is a metadata-only operation —
// no engram data is moved or modified. Returns ErrVaultNotFound if oldName
// doesn't exist, ErrVaultJobActive if a clone/merge job targets the vault,
// or an error if newName already exists.
func (e *Engine) RenameVault(ctx context.Context, oldName, newName string) error {
	if err := e.refuseAppend(ctx); err != nil {
		return err
	}
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
