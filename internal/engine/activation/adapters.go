package activation

import (
	"context"

	"github.com/scrypster/muninndb/internal/index/fts"
	hnswpkg "github.com/scrypster/muninndb/internal/index/hnsw"
	"github.com/scrypster/muninndb/internal/storage"
	"strings"
)

// hnswActivationAdapter adapts *hnswpkg.Registry to activation.HNSWIndex.
type hnswActivationAdapter struct{ reg *hnswpkg.Registry }

func (a *hnswActivationAdapter) Search(ctx context.Context, ws [8]byte, vec []float32, topK int, _ []Filter) ([]ScoredID, error) {
	results, err := a.reg.Search(ctx, ws, vec, topK)
	if err != nil {
		return nil, err
	}
	out := make([]ScoredID, len(results))
	for i, r := range results {
		out[i] = ScoredID{ID: storage.ULID(r.ID), Score: r.Score}
	}
	return out, nil
}

// NewHNSWAdapter returns an HNSWIndex that delegates to the given registry.
func NewHNSWAdapter(reg *hnswpkg.Registry) HNSWIndex {
	return &hnswActivationAdapter{reg: reg}
}

// ftsActivationAdapter adapts *fts.Index to activation.FTSIndex.
type ftsActivationAdapter struct{ idx *fts.Index }

func (a *ftsActivationAdapter) Search(ctx context.Context, ws [8]byte, query string, topK int) ([]ScoredID, error) {
	results, err := a.idx.Search(ctx, ws, query, topK)
	if err != nil {
		return nil, err
	}
	out := make([]ScoredID, len(results))
	for i, r := range results {
		out[i] = ScoredID{ID: storage.ULID(r.ID), Score: r.Score}
	}
	return out, nil
}

// NewFTSAdapter returns an FTSIndex that delegates to the given index.
func NewFTSAdapter(idx *fts.Index) FTSIndex {
	return &ftsActivationAdapter{idx: idx}
}

// noopEmbedder is a stub embedder that returns zero vectors.
type noopEmbedder struct{}

func (e *noopEmbedder) Embed(ctx context.Context, texts []string) ([]float32, error) {
	return make([]float32, len(texts)*384), nil
}

func (e *noopEmbedder) Tokenize(text string) []string {
	return strings.Fields(text)
}

// NewNoopEmbedder returns an Embedder that returns zero vectors.
func NewNoopEmbedder() Embedder {
	return &noopEmbedder{}
}
