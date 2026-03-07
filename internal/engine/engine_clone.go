package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/cockroachdb/pebble"
	"github.com/scrypster/muninndb/internal/engine/vaultjob"
	"github.com/scrypster/muninndb/internal/index/fts"
	"github.com/scrypster/muninndb/internal/storage"
)

// StartClone starts an async job to clone sourceVault to a new vault named newName.
// Returns the job immediately (202 pattern). The clone runs in a background goroutine.
// Returns an error if sourceVault does not exist or newName already exists.
func (e *Engine) StartClone(ctx context.Context, sourceVault, newName string) (*vaultjob.Job, error) {
	// I3: Hold vaultOpsMu for the entire check+reserve window so that a
	// concurrent clone/delete cannot race between the existence check and the
	// WriteVaultName reservation.
	e.vaultOpsMu.Lock()
	defer e.vaultOpsMu.Unlock()

	names, err := e.store.ListVaultNames()
	if err != nil {
		return nil, fmt.Errorf("start clone: list vaults: %w", err)
	}
	sourceFound := false
	targetExists := false
	for _, n := range names {
		if n == sourceVault {
			sourceFound = true
		}
		if n == newName {
			targetExists = true
		}
	}
	if !sourceFound {
		return nil, fmt.Errorf("start clone: source vault %q: %w", sourceVault, ErrVaultNotFound)
	}
	if targetExists {
		return nil, fmt.Errorf("start clone: target vault %q: %w", newName, ErrVaultNameCollision)
	}

	// Reserve the target vault name before releasing the mutex.
	// CloneVaultData no longer calls WriteVaultName; we do it here atomically.
	wsTarget := e.store.VaultPrefix(newName)
	if err := e.store.WriteVaultName(wsTarget, newName); err != nil {
		return nil, fmt.Errorf("start clone: reserve vault name: %w", err)
	}

	job, err := e.jobManager.Create("clone", sourceVault, newName)
	if err != nil {
		// Clean up the reserved vault name so it can be used in future clone attempts.
		if cleanupErr := e.store.DeleteVaultNameOnly(ctx, newName, wsTarget); cleanupErr != nil {
			slog.Error("start clone: failed to clean up reserved vault name after job creation failure",
				"vault", newName, "err", cleanupErr)
		}
		return nil, fmt.Errorf("start clone: %w", err)
	}

	wsSource := e.store.VaultPrefix(sourceVault)

	// Count engrams in source to set CopyTotal/IndexTotal for progress tracking.
	sourceCount := e.store.GetVaultCount(ctx, wsSource)
	job.CopyTotal = sourceCount
	job.IndexTotal = sourceCount

	if !e.spawnJob(func() { e.runClone(job, wsSource, wsTarget, newName) }) {
		e.jobManager.Fail(job, fmt.Errorf("engine is shutting down"))
		if cleanupErr := e.store.DeleteVaultNameOnly(context.Background(), newName, wsTarget); cleanupErr != nil {
			slog.Warn("start clone: failed to clean up reserved vault name during shutdown",
				"vault", newName, "err", cleanupErr)
		}
		return job, nil // job is already failed; return it so the caller can report the job_id
	}
	return job, nil
}

func (e *Engine) runClone(job *vaultjob.Job, wsSource, wsTarget [8]byte, newName string) {
	// I1: Use engine lifecycle context so the goroutine exits when Stop() is called.
	ctx := e.stopCtx

	// Panic recovery: if goroutine panics, ensure running counter is decremented.
	defer func() {
		if r := recover(); r != nil {
			// Swallow closed-DB panics — can occur if the 30s Stop() timeout
			// expires and Pebble is closed before this goroutine exits.
			if err, ok := r.(error); ok && errors.Is(err, pebble.ErrClosed) {
				e.jobManager.Fail(job, fmt.Errorf("engine closed during job"))
				return
			}
			e.jobManager.Fail(job, fmt.Errorf("vault job panicked: %v", r))
			slog.Error("clone job panicked", "job_id", job.ID, "source_vault", wsSource, "target_vault", newName, "panic", r)
		}
	}()

	// Phase 1: Copy data via storage layer.
	// Note: WriteVaultName was already called under vaultOpsMu in StartClone.
	// I8: SetClearing(wsTarget) removed — reindexVault writes are idempotent
	// last-write-wins KV ops; duplicate FTS posting list entries are harmless.
	copied, err := e.store.CloneVaultData(ctx, wsSource, wsTarget, func(n int64) {
		job.CopyCurrent.Store(n)
	})
	if err != nil {
		e.jobManager.Fail(job, fmt.Errorf("copy phase: %w", err))
		return
	}
	job.CopyCurrent.Store(copied)

	// Phase 2: Re-index FTS and HNSW for target vault.
	job.SetPhase(vaultjob.PhaseIndexing)
	if err := e.reindexVault(ctx, wsTarget, job); err != nil {
		e.jobManager.Fail(job, fmt.Errorf("index phase: %w", err))
		return
	}

	// Update global engram count.
	e.engramCount.Add(copied)

	e.jobManager.Complete(job)
}

// reindexVault scans all engrams in a vault and rebuilds FTS and HNSW indexes.
// job may be nil (no progress tracking).
func (e *Engine) reindexVault(ctx context.Context, ws [8]byte, job *vaultjob.Job) error {
	var indexed int64

	err := e.store.ScanEngrams(ctx, ws, func(eng *storage.Engram) error {
		// Submit FTS re-index job to the async worker.
		// The worker checks clearingVaults internally — since we set clearing=true
		// before copying and clear it after indexing, we bypass the clearing guard
		// by calling the underlying fts.Index directly here.
		if e.fts != nil {
			if err := e.fts.IndexEngram(ws, [16]byte(eng.ID), eng.Concept, eng.CreatedBy, eng.Content, eng.Tags); err != nil {
				slog.Warn("reindex: FTS index failed for engram", "id", eng.ID, "err", err)
			}
		}

		// HNSW re-index (only if engram has an embedding).
		if e.hnswRegistry != nil && len(eng.Embedding) > 0 {
			if err := e.hnswRegistry.Insert(ctx, ws, [16]byte(eng.ID), eng.Embedding); err != nil {
				slog.Warn("reindex: HNSW insert failed for engram", "id", eng.ID, "err", err)
			}
		}

		indexed++
		if job != nil {
			job.IndexCurrent.Store(indexed)
		}
		return nil
	})
	return err
}

// StartMerge starts an async job to merge sourceVault into targetVault.
// If deleteSource is true, the source vault is deleted after the merge completes.
// Returns an error if source and target are the same, or if either vault does not exist.
func (e *Engine) StartMerge(ctx context.Context, sourceVault, targetVault string, deleteSource bool) (*vaultjob.Job, error) {
	if sourceVault == targetVault {
		return nil, fmt.Errorf("source and target vault must be different")
	}

	// I3: Hold vaultOpsMu during existence checks so a concurrent delete cannot
	// remove a vault between our check and the goroutine launch.
	e.vaultOpsMu.Lock()
	defer e.vaultOpsMu.Unlock()

	names, err := e.store.ListVaultNames()
	if err != nil {
		return nil, fmt.Errorf("start merge: list vaults: %w", err)
	}
	sourceFound := false
	targetFound := false
	for _, n := range names {
		if n == sourceVault {
			sourceFound = true
		}
		if n == targetVault {
			targetFound = true
		}
	}
	if !sourceFound {
		return nil, fmt.Errorf("start merge: source vault %q: %w", sourceVault, ErrVaultNotFound)
	}
	if !targetFound {
		return nil, fmt.Errorf("start merge: target vault %q: %w", targetVault, ErrVaultNotFound)
	}

	job, err := e.jobManager.Create("merge", sourceVault, targetVault)
	if err != nil {
		return nil, fmt.Errorf("start merge: %w", err)
	}

	wsSource := e.store.VaultPrefix(sourceVault)
	wsTarget := e.store.VaultPrefix(targetVault)

	sourceCount := e.store.GetVaultCount(ctx, wsSource)
	job.CopyTotal = sourceCount
	job.IndexTotal = sourceCount

	if !e.spawnJob(func() { e.runMerge(job, wsSource, wsTarget, sourceVault, targetVault, deleteSource) }) {
		e.jobManager.Fail(job, fmt.Errorf("engine is shutting down"))
		return job, nil // job is already failed; return it so the caller can report the job_id
	}
	return job, nil
}

func (e *Engine) runMerge(job *vaultjob.Job, wsSource, wsTarget [8]byte, sourceVault, targetVault string, deleteSource bool) {
	// I1: Use engine lifecycle context so the goroutine exits when Stop() is called.
	ctx := e.stopCtx

	// Panic recovery: if goroutine panics, ensure running counter is decremented.
	defer func() {
		if r := recover(); r != nil {
			// Swallow closed-DB panics — can occur if the 30s Stop() timeout
			// expires and Pebble is closed before this goroutine exits.
			if err, ok := r.(error); ok && errors.Is(err, pebble.ErrClosed) {
				e.jobManager.Fail(job, fmt.Errorf("engine closed during job"))
				return
			}
			e.jobManager.Fail(job, fmt.Errorf("vault job panicked: %v", r))
			slog.Error("merge job panicked", "job_id", job.ID, "source_vault", sourceVault, "target_vault", targetVault, "panic", r)
		}
	}()

	// Phase 1: Merge data via storage layer.
	// I8: SetClearing(wsTarget) removed — reindexVault writes are idempotent
	// last-write-wins KV ops; duplicate FTS posting list entries are harmless.
	merged, err := e.store.MergeVaultData(ctx, wsSource, wsTarget, func(n int64) {
		job.CopyCurrent.Store(n)
	})
	if err != nil {
		e.jobManager.Fail(job, fmt.Errorf("merge phase: %w", err))
		return
	}
	job.CopyCurrent.Store(merged)

	// Phase 2: Re-index FTS and HNSW for target vault.
	job.SetPhase(vaultjob.PhaseIndexing)
	if err := e.reindexVault(ctx, wsTarget, job); err != nil {
		e.jobManager.Fail(job, fmt.Errorf("index phase: %w", err))
		return
	}

	// Update global engram count with newly merged engrams.
	// Known limitation: if concurrent writes arrived at the source vault during
	// the merge window, DeleteVault (below) will subtract a higher vault count than
	// we add here, transiently depressing engramCount. ClearVault's floor guard
	// prevents going negative. The imprecision resolves on the next write or Stat call.
	e.engramCount.Add(merged)

	// Optionally delete the source vault after merge.
	if deleteSource {
		if err := e.DeleteVault(ctx, sourceVault); err != nil {
			slog.Warn("post-merge source vault deletion failed; source vault still exists",
				"vault", sourceVault, "err", err)
		}
	}

	e.jobManager.Complete(job)
}

// GetVaultJob returns the job with the given ID, or (nil, false) if not found.
func (e *Engine) GetVaultJob(jobID string) (*vaultjob.Job, bool) {
	return e.jobManager.Get(jobID)
}

// ftsIndexEngram is a helper that calls fts.Index.IndexEngram directly,
// bypassing the async worker. Used during re-index to avoid the clearing guard.
func ftsIndexEngram(idx interface {
	IndexEngram(ws [8]byte, id [16]byte, concept, createdBy, content string, tags []string) error
}, ws [8]byte, eng *storage.Engram) {
	_ = idx.IndexEngram(ws, [16]byte(eng.ID), eng.Concept, eng.CreatedBy, eng.Content, eng.Tags)
}

// ensure fts import is used (IndexJob is referenced only via worker.Submit in engine.go)
var _ = fts.IndexJob{}
