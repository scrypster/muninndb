package engine

import (
	"errors"
	"fmt"
	"io"
	"log/slog"

	"github.com/cockroachdb/pebble"
	"github.com/scrypster/muninndb/internal/engine/vaultjob"
	"github.com/scrypster/muninndb/internal/metrics"
	"github.com/scrypster/muninndb/internal/storage"
	"golang.org/x/net/context"
)

// ExportVault synchronously exports the named vault to w as a .muninn archive.
// Returns an ExportResult with engram count and total key count.
// Returns ErrVaultNotFound if the vault does not exist.
func (e *Engine) ExportVault(ctx context.Context, vaultName, embedderModel string, dimension int, resetMeta bool, w io.Writer) (*storage.ExportResult, error) {
	names, err := e.store.ListVaultNames()
	if err != nil {
		return nil, fmt.Errorf("export vault: list vaults: %w", err)
	}
	found := false
	for _, n := range names {
		if n == vaultName {
			found = true
			break
		}
	}
	if !found {
		return nil, fmt.Errorf("export vault %q: %w", vaultName, ErrVaultNotFound)
	}

	ws := e.store.VaultPrefix(vaultName)
	opts := storage.ExportOpts{
		EmbedderModel: embedderModel,
		Dimension:     dimension,
		ResetMetadata: resetMeta,
	}
	result, err := e.store.ExportVaultData(ctx, ws, vaultName, opts, w)
	if err != nil {
		return nil, fmt.Errorf("export vault %q: %w", vaultName, err)
	}
	return result, nil
}

// StartImport starts an async job to import a .muninn archive into a new vault
// named vaultName. The data is read from r.
// Returns the job immediately (202 pattern).
// Returns an error if vaultName already exists.
func (e *Engine) StartImport(ctx context.Context, vaultName, embedderModel string, dimension int, resetMeta bool, r io.Reader) (*vaultjob.Job, error) {
	e.vaultOpsMu.Lock()

	names, err := e.store.ListVaultNames()
	if err != nil {
		e.vaultOpsMu.Unlock()
		return nil, fmt.Errorf("start import: list vaults: %w", err)
	}
	for _, n := range names {
		if n == vaultName {
			e.vaultOpsMu.Unlock()
			return nil, fmt.Errorf("start import: vault %q: %w", vaultName, ErrVaultNameCollision)
		}
	}

	// Reserve the vault name before releasing the lock.
	wsTarget := e.store.VaultPrefix(vaultName)
	if err := e.store.WriteVaultName(wsTarget, vaultName); err != nil {
		e.vaultOpsMu.Unlock()
		return nil, fmt.Errorf("start import: reserve vault name: %w", err)
	}

	e.vaultOpsMu.Unlock()

	job, err := e.jobManager.Create("import", "", vaultName)
	if err != nil {
		// Clean up the reserved vault name.
		if cleanupErr := e.store.DeleteVaultNameOnly(ctx, vaultName, wsTarget); cleanupErr != nil {
			slog.Error("start import: failed to clean up reserved vault name after job creation failure",
				"vault", vaultName, "err", cleanupErr)
		}
		return nil, fmt.Errorf("start import: %w", err)
	}

	opts := storage.ImportOpts{
		ResetMetadata:     resetMeta,
		ExpectedModel:     embedderModel,
		ExpectedDimension: dimension,
	}
	if !e.spawnJob(func() { e.runImport(job, wsTarget, vaultName, r, opts) }) {
		e.jobManager.Fail(job, fmt.Errorf("engine is shutting down"))
		// Do NOT call DeleteVaultNameOnly here: the engine is shutting down and
		// Pebble may already be closed, which would panic. The orphaned vault name
		// entry is harmless — an incomplete import target with no engrams will
		// simply appear as an empty vault until cleaned up by the operator.
		return job, nil // job is already failed; return it so the caller can report the job_id
	}
	return job, nil
}

func (e *Engine) runImport(job *vaultjob.Job, wsTarget [8]byte, vaultName string, r io.Reader, opts storage.ImportOpts) {
	// Use engine lifecycle context so the goroutine exits when Stop() is called.
	ctx := e.stopCtx

	defer func() {
		if rec := recover(); rec != nil {
			// Swallow closed-DB panics — can occur if the 30s Stop() timeout
			// expires and Pebble is closed before this goroutine exits.
			if err, ok := rec.(error); ok && errors.Is(err, pebble.ErrClosed) {
				e.jobManager.Fail(job, fmt.Errorf("engine closed during job"))
				return
			}
			metrics.ImportJobsTotal.WithLabelValues("failed").Inc()
			e.jobManager.Fail(job, fmt.Errorf("import job panicked: %v", rec))
			slog.Error("import job panicked", "job_id", job.ID, "vault", vaultName, "panic", rec)
		}
	}()

	// Phase 1: import data from archive.
	result, err := e.store.ImportVaultData(ctx, wsTarget, vaultName, opts, r)
	if err != nil {
		metrics.ImportJobsTotal.WithLabelValues("failed").Inc()
		e.jobManager.Fail(job, fmt.Errorf("import phase: %w", err))
		return
	}
	job.CopyCurrent.Store(result.EngramCount)
	job.CopyTotal = result.EngramCount

	// Phase 2: Re-index FTS and HNSW for imported vault.
	job.SetPhase(vaultjob.PhaseIndexing)
	job.IndexTotal = result.EngramCount
	if err := e.reindexVault(ctx, wsTarget, job); err != nil {
		metrics.ImportJobsTotal.WithLabelValues("failed").Inc()
		e.jobManager.Fail(job, fmt.Errorf("index phase: %w", err))
		return
	}

	// Update global engram count.
	e.engramCount.Add(result.EngramCount)

	metrics.ImportJobsTotal.WithLabelValues("completed").Inc()
	e.jobManager.Complete(job)
}
