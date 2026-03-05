package main

import (
	"context"

	"github.com/scrypster/muninndb/internal/cognitive"
	"github.com/scrypster/muninndb/internal/storage"
)

// benchHebbianAdapter adapts PebbleStore to the HebbianStore interface.
type benchHebbianAdapter struct{ store *storage.PebbleStore }

func (a *benchHebbianAdapter) GetAssocWeight(ctx context.Context, ws [8]byte, src, dst [16]byte) (float32, error) {
	return a.store.GetAssocWeight(ctx, ws, storage.ULID(src), storage.ULID(dst))
}
func (a *benchHebbianAdapter) UpdateAssocWeight(ctx context.Context, ws [8]byte, src, dst [16]byte, w float32) error {
	// This path is only used outside of processBatch (e.g., tests, manual adjustments).
	// CountDelta is 0 because this is a weight-only update — co-activation count
	// is accumulated exclusively through UpdateAssocWeightBatch in processBatch.
	return a.store.UpdateAssocWeight(ctx, ws, storage.ULID(src), storage.ULID(dst), w, 0)
}
func (a *benchHebbianAdapter) DecayAssocWeights(ctx context.Context, ws [8]byte, factor float64, min float32, archiveThreshold float64) (int, error) {
	return a.store.DecayAssocWeights(ctx, ws, factor, min, archiveThreshold)
}
func (a *benchHebbianAdapter) UpdateAssocWeightBatch(ctx context.Context, updates []cognitive.AssocWeightUpdate) error {
	storageUpdates := make([]storage.AssocWeightUpdate, len(updates))
	for i, u := range updates {
		storageUpdates[i] = storage.AssocWeightUpdate{
			WS:         u.WS,
			Src:        storage.ULID(u.Src),
			Dst:        storage.ULID(u.Dst),
			Weight:     u.Weight,
			CountDelta: u.CountDelta,
		}
	}
	return a.store.UpdateAssocWeightBatch(ctx, storageUpdates)
}

// benchConfidenceAdapter adapts PebbleStore to the ConfidenceStore interface.
type benchConfidenceAdapter struct{ store *storage.PebbleStore }

func (a *benchConfidenceAdapter) GetConfidence(ctx context.Context, ws [8]byte, id [16]byte) (float32, error) {
	return a.store.GetConfidence(ctx, ws, storage.ULID(id))
}
func (a *benchConfidenceAdapter) UpdateConfidence(ctx context.Context, ws [8]byte, id [16]byte, c float32) error {
	return a.store.UpdateConfidence(ctx, ws, storage.ULID(id), c)
}

// benchContradictAdapter adapts PebbleStore to the ContradictionStore interface.
type benchContradictAdapter struct{ store *storage.PebbleStore }

func (a *benchContradictAdapter) FlagContradiction(ctx context.Context, ws [8]byte, engramA, engramB [16]byte) error {
	return a.store.FlagContradiction(ctx, ws, storage.ULID(engramA), storage.ULID(engramB))
}
