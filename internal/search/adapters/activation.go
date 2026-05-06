package adapters

import (
	"context"

	"github.com/scrypster/muninndb/internal/engine/activation"
	"github.com/scrypster/muninndb/internal/search"
	"github.com/scrypster/muninndb/internal/storage"
)

// ActivationFTS adapts search.TextSearcher to activation.FTSIndex.
type ActivationFTS struct{ B search.TextSearcher }

var _ activation.FTSIndex = ActivationFTS{}

// Search delegates activation FTS search to a search backend.
func (a ActivationFTS) Search(ctx context.Context, ws [8]byte, query string, topK int) ([]activation.ScoredID, error) {
	if a.B == nil {
		return nil, nil
	}
	hits, err := a.B.SearchText(ctx, ws, query, topK)
	if err != nil {
		return nil, err
	}
	out := make([]activation.ScoredID, len(hits))
	for i, h := range hits {
		out[i] = activation.ScoredID{ID: storage.ULID(h.ID), Score: h.Score}
	}
	return out, nil
}

// ActivationVector adapts search.VectorSearcher to activation.HNSWIndex.
type ActivationVector struct{ B search.VectorSearcher }

var _ activation.HNSWIndex = ActivationVector{}

// Search delegates activation vector search to a search backend.
func (a ActivationVector) Search(ctx context.Context, ws [8]byte, vec []float32, topK int, filters []activation.Filter) ([]activation.ScoredID, error) {
	if a.B == nil {
		return nil, nil
	}
	sf := make([]search.Filter, len(filters))
	for i, f := range filters {
		sf[i] = search.Filter{Field: f.Field, Op: f.Op, Value: f.Value}
	}
	hits, err := a.B.SearchVector(ctx, ws, vec, search.VectorSearchOptions{TopK: topK, Filters: sf})
	if err != nil {
		return nil, err
	}
	out := make([]activation.ScoredID, len(hits))
	for i, h := range hits {
		out[i] = activation.ScoredID{ID: storage.ULID(h.ID), Score: h.Score}
	}
	return out, nil
}
