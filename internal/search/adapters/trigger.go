package adapters

import (
	"context"

	"github.com/scrypster/muninndb/internal/engine/trigger"
	"github.com/scrypster/muninndb/internal/search"
	"github.com/scrypster/muninndb/internal/storage"
)

// TriggerFTS adapts search.TextSearcher to trigger.FTSIndex.
type TriggerFTS struct{ B search.TextSearcher }

var _ trigger.FTSIndex = TriggerFTS{}

// Search delegates trigger FTS search to a search backend.
func (a TriggerFTS) Search(ctx context.Context, ws [8]byte, query string, topK int) ([]trigger.ScoredID, error) {
	if a.B == nil {
		return nil, nil
	}
	hits, err := a.B.SearchText(ctx, ws, query, topK)
	if err != nil {
		return nil, err
	}
	out := make([]trigger.ScoredID, len(hits))
	for i, h := range hits {
		out[i] = trigger.ScoredID{ID: storage.ULID(h.ID), Score: h.Score}
	}
	return out, nil
}

// TriggerVector adapts search.VectorSearcher to trigger.HNSWIndex.
type TriggerVector struct{ B search.VectorSearcher }

var _ trigger.HNSWIndex = TriggerVector{}

// Search delegates trigger vector search to a search backend.
func (a TriggerVector) Search(ctx context.Context, ws [8]byte, vec []float32, topK int) ([]trigger.ScoredID, error) {
	if a.B == nil {
		return nil, nil
	}
	hits, err := a.B.SearchVector(ctx, ws, vec, search.VectorSearchOptions{TopK: topK})
	if err != nil {
		return nil, err
	}
	out := make([]trigger.ScoredID, len(hits))
	for i, h := range hits {
		out[i] = trigger.ScoredID{ID: storage.ULID(h.ID), Score: h.Score}
	}
	return out, nil
}
