package adapters

import (
	"context"

	"github.com/scrypster/muninndb/internal/index/hnsw"
	"github.com/scrypster/muninndb/internal/search"
)

// HNSWRegistry adapts a search backend to hnsw.RegistryIndex.
type HNSWRegistry struct{ B search.Backend }

var _ hnsw.RegistryIndex = HNSWRegistry{}

// Insert delegates vector writes to the configured search backend.
func (a HNSWRegistry) Insert(ctx context.Context, ws [8]byte, id [16]byte, vec []float32) error {
	if a.B == nil {
		return nil
	}
	return a.B.IndexVector(ctx, ws, id, vec)
}

// Search delegates semantic-neighbor lookup to the configured search backend.
func (a HNSWRegistry) Search(ctx context.Context, ws [8]byte, vec []float32, topK int) ([]hnsw.ScoredID, error) {
	if a.B == nil {
		return nil, nil
	}
	hits, err := a.B.SearchVector(ctx, ws, vec, search.VectorSearchOptions{TopK: topK})
	if err != nil {
		return nil, err
	}
	out := make([]hnsw.ScoredID, len(hits))
	for i, hit := range hits {
		out[i] = hnsw.ScoredID{ID: hit.ID, Score: hit.Score}
	}
	return out, nil
}

// VaultEmbedDim reports a backend vector dimension when available.
func (a HNSWRegistry) VaultEmbedDim(ws [8]byte) int {
	if a.B == nil {
		return 0
	}
	if dimProvider, ok := a.B.(search.VaultVectorDimensionProvider); ok {
		return dimProvider.VaultVectorDimension(ws)
	}
	if dimProvider, ok := a.B.(search.VectorDimensionProvider); ok {
		return dimProvider.VectorDimension()
	}
	return 0
}

// VaultVectors is unavailable for generic search backends unless implemented separately.
func (a HNSWRegistry) VaultVectors([8]byte) int { return 0 }

// VaultVectorBytes is unavailable for generic search backends unless implemented separately.
func (a HNSWRegistry) VaultVectorBytes([8]byte) int64 { return 0 }

// TotalVectorBytes is unavailable for generic search backends unless implemented separately.
func (a HNSWRegistry) TotalVectorBytes() int64 { return 0 }

// ResetVault delegates vault lifecycle reset to the configured search backend.
func (a HNSWRegistry) ResetVault(ws [8]byte) {
	if a.B != nil {
		_ = a.B.ResetVault(context.Background(), ws)
	}
}

// TombstoneNode delegates vector deletion to the configured search backend.
func (a HNSWRegistry) TombstoneNode(ws [8]byte, id [16]byte) {
	if a.B != nil {
		_ = a.B.DeleteVector(context.Background(), ws, id)
	}
}
