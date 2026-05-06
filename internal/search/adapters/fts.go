package adapters

import (
	"context"
	"time"

	"github.com/scrypster/muninndb/internal/index/fts"
	"github.com/scrypster/muninndb/internal/search"
	"github.com/scrypster/muninndb/internal/storage"
)

// FTSIndex adapts a search backend to fts.FullTextIndex.
type FTSIndex struct{ B search.Backend }

var _ fts.FullTextIndex = FTSIndex{}

// IndexEngram delegates text indexing to the configured search backend.
func (a FTSIndex) IndexEngram(ws [8]byte, id [16]byte, concept, createdBy, content string, tags []string, createdAt int64) error {
	if a.B == nil {
		return nil
	}
	return a.B.IndexText(context.Background(), ws, &storage.Engram{
		ID:        storage.ULID(id),
		Concept:   concept,
		CreatedBy: createdBy,
		Content:   content,
		Tags:      tags,
		CreatedAt: time.Unix(createdAt, 0).UTC(),
	})
}

// DeleteEngram delegates text deletion to the configured search backend.
func (a FTSIndex) DeleteEngram(ws [8]byte, id [16]byte, _, _, _ string, _ []string, _ int64) error {
	if a.B == nil {
		return nil
	}
	return a.B.DeleteText(context.Background(), ws, id)
}

// InvalidateIDFCache is a no-op for search backends that manage their own scoring caches.
func (a FTSIndex) InvalidateIDFCache() {}

// Search delegates full-text lookup to the configured search backend.
func (a FTSIndex) Search(ctx context.Context, ws [8]byte, query string, topK int) ([]fts.ScoredID, error) {
	if a.B == nil {
		return nil, nil
	}
	hits, err := a.B.SearchText(ctx, ws, query, topK)
	if err != nil {
		return nil, err
	}
	out := make([]fts.ScoredID, len(hits))
	for i, hit := range hits {
		out[i] = fts.ScoredID{ID: hit.ID, Score: hit.Score}
	}
	return out, nil
}
