package cognitive

import (
	"context"

	"github.com/scrypster/muninndb/internal/storage"
)

// hebbianStoreAdapter adapts storage.EngineStore to cognitive.HebbianStore.
type hebbianStoreAdapter struct {
	store storage.EngineStore
}

// NewHebbianStoreAdapter returns a HebbianStore backed by the given EngineStore.
func NewHebbianStoreAdapter(store storage.EngineStore) HebbianStore {
	return &hebbianStoreAdapter{store: store}
}

func (a *hebbianStoreAdapter) GetAssocWeight(ctx context.Context, ws [8]byte, src, dst [16]byte) (float32, error) {
	return a.store.GetAssocWeight(ctx, ws, storage.ULID(src), storage.ULID(dst))
}

func (a *hebbianStoreAdapter) UpdateAssocWeight(ctx context.Context, ws [8]byte, src, dst [16]byte, weight float32) error {
	// This path is only used outside of processBatch (e.g., tests, manual adjustments).
	// CountDelta is 0 because this is a weight-only update — co-activation count
	// is accumulated exclusively through UpdateAssocWeightBatch in processBatch.
	return a.store.UpdateAssocWeight(ctx, ws, storage.ULID(src), storage.ULID(dst), weight, 0)
}

func (a *hebbianStoreAdapter) DecayAssocWeights(ctx context.Context, ws [8]byte, decayFactor float64, minWeight float32, archiveThreshold float64) (int, error) {
	return a.store.DecayAssocWeights(ctx, ws, decayFactor, minWeight, archiveThreshold)
}

func (a *hebbianStoreAdapter) UpdateAssocWeightBatch(ctx context.Context, updates []AssocWeightUpdate) error {
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

// decayStoreAdapter adapts storage.EngineStore to cognitive.DecayStore.
type decayStoreAdapter struct {
	store storage.EngineStore
}

// NewDecayStoreAdapter returns a DecayStore backed by the given EngineStore.
func NewDecayStoreAdapter(store storage.EngineStore) DecayStore {
	return &decayStoreAdapter{store: store}
}

func (a *decayStoreAdapter) GetMetadataBatch(ctx context.Context, ws [8]byte, ids [][16]byte) ([]DecayMeta, error) {
	ulidIDs := make([]storage.ULID, len(ids))
	for i, id := range ids {
		ulidIDs[i] = storage.ULID(id)
	}
	metas, err := a.store.GetMetadata(ctx, ws, ulidIDs)
	if err != nil {
		return nil, err
	}
	result := make([]DecayMeta, len(metas))
	for i, meta := range metas {
		if meta != nil {
			result[i] = DecayMeta{
				ID:          [16]byte(meta.ID),
				LastAccess:  meta.LastAccess,
				AccessCount: meta.AccessCount,
				Stability:   meta.Stability,
				Relevance:   meta.Relevance,
			}
		}
	}
	return result, nil
}

func (a *decayStoreAdapter) UpdateRelevance(ctx context.Context, ws [8]byte, id [16]byte, relevance, stability float32) error {
	return a.store.UpdateRelevance(ctx, ws, storage.ULID(id), relevance, stability)
}

// contradictStoreAdapter adapts storage.EngineStore to cognitive.ContradictionStore.
type contradictStoreAdapter struct {
	store storage.EngineStore
}

// NewContradictStoreAdapter returns a ContradictionStore backed by the given EngineStore.
func NewContradictStoreAdapter(store storage.EngineStore) ContradictionStore {
	return &contradictStoreAdapter{store: store}
}

func (a *contradictStoreAdapter) FlagContradiction(ctx context.Context, ws [8]byte, engramA, engramB [16]byte) error {
	return a.store.FlagContradiction(ctx, ws, storage.ULID(engramA), storage.ULID(engramB))
}

// confidenceStoreAdapter adapts storage.EngineStore to cognitive.ConfidenceStore.
type confidenceStoreAdapter struct {
	store storage.EngineStore
}

// NewConfidenceStoreAdapter returns a ConfidenceStore backed by the given EngineStore.
func NewConfidenceStoreAdapter(store storage.EngineStore) ConfidenceStore {
	return &confidenceStoreAdapter{store: store}
}

func (a *confidenceStoreAdapter) GetConfidence(ctx context.Context, ws [8]byte, id [16]byte) (float32, error) {
	return a.store.GetConfidence(ctx, ws, storage.ULID(id))
}

func (a *confidenceStoreAdapter) UpdateConfidence(ctx context.Context, ws [8]byte, id [16]byte, confidence float32) error {
	return a.store.UpdateConfidence(ctx, ws, storage.ULID(id), confidence)
}
