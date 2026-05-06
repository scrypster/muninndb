package engine

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/scrypster/muninndb/internal/metrics"
	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/storage/keys"
)

// ReindexFTSVault clears all FTS posting lists for the named vault and rebuilds
// them using the current (Porter2-stemmed) tokenizer. Sets the FTS schema version
// marker (0x1B key) to 1 upon successful completion so dual-path fallback can be
// skipped for this vault in future queries.
//
// Returns the number of engrams re-indexed, or an error.
//
// NOTE: This method does NOT rebuild HNSW embeddings — it is FTS-only.
// The vault must exist in the registered name list or ErrVaultNotFound is returned.
func (e *Engine) ReindexFTSVault(ctx context.Context, vaultName string) (int64, error) {
	if !e.beginVaultOp() {
		return 0, fmt.Errorf("engine is shutting down")
	}
	defer e.endVaultOp()

	opCtx, stop := e.vaultOpContext(ctx)
	defer stop()

	mu := e.getVaultMutex(vaultName)
	mu.Lock()
	defer mu.Unlock()

	names, err := e.store.ListVaultNames()
	if err != nil {
		return 0, fmt.Errorf("reindex-fts: list vault names: %w", err)
	}
	found := false
	for _, n := range names {
		if n == vaultName {
			found = true
			break
		}
	}
	if !found {
		return 0, fmt.Errorf("vault %q: %w", vaultName, ErrVaultNotFound)
	}

	ws := e.store.VaultPrefix(vaultName)

	// Prevent the FTS worker from submitting new jobs during the re-index window.
	if e.ftsWorker != nil {
		e.ftsWorker.SetClearing(ws, true)
		defer e.ftsWorker.SetClearing(ws, false)
	}

	// Step 1: Delete existing FTS keys for this vault via range tombstones.
	// Prefixes cleared: 0x05 (posting lists), 0x06 (trigrams),
	//                   0x08 (FTS global stats), 0x09 (per-term stats).
	wsPlus, err := keys.IncrementWSPrefix(ws)
	if err != nil {
		return 0, fmt.Errorf("reindex-fts: increment ws: %w", err)
	}

	if err := e.store.ClearFTSKeys(ws, wsPlus); err != nil {
		return 0, fmt.Errorf("reindex-fts: clear keys: %w", err)
	}

	// Invalidate in-memory IDF cache so stale scores are not carried forward.
	if e.fts != nil {
		e.fts.InvalidateIDFCache()
	}

	// Step 2: Re-index all engrams using the current stemmed tokenizer.
	var indexed int64
	scanErr := e.store.ScanEngrams(opCtx, ws, func(eng *storage.Engram) error {
		if e.fts != nil {
			if err := e.fts.IndexEngram(ws, [16]byte(eng.ID), eng.Concept, eng.CreatedBy, eng.Content, eng.Tags, eng.CreatedAt.Unix()); err != nil {
				slog.Warn("reindex-fts: IndexEngram failed", "vault", vaultName, "id", eng.ID, "err", err)
				metrics.FTSIndexFailures.WithLabelValues(vaultName).Inc()
			}
		}
		indexed++
		return nil
	})
	if scanErr != nil {
		return indexed, fmt.Errorf("reindex-fts: scan engrams: %w", scanErr)
	}

	// Step 3: Set the FTS version marker to 1 (fully re-indexed with stemming).
	if err := e.store.SetFTSVersionMarker(ws, 0x01); err != nil {
		return indexed, fmt.Errorf("reindex-fts: set version marker: %w", err)
	}

	slog.Info("reindex-fts: complete", "vault", vaultName, "engrams", indexed)
	return indexed, nil
}
