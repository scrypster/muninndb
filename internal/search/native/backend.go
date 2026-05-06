package native

import (
	"context"
	"fmt"

	"github.com/scrypster/muninndb/internal/index/fts"
	"github.com/scrypster/muninndb/internal/index/hnsw"
	"github.com/scrypster/muninndb/internal/search"
	"github.com/scrypster/muninndb/internal/storage"
)

// Backend adapts MuninnDB's existing Pebble FTS and HNSW registry to search.Backend.
type Backend struct {
	FTS  *fts.Index
	HNSW *hnsw.Registry
}

var _ search.Backend = (*Backend)(nil)

// New returns a native search backend that preserves existing index behavior.
func New(ftsIndex *fts.Index, hnswRegistry *hnsw.Registry) *Backend {
	return &Backend{FTS: ftsIndex, HNSW: hnswRegistry}
}

// IndexText indexes the searchable text fields for an engram.
func (b *Backend) IndexText(_ context.Context, ws [8]byte, eng *storage.Engram) error {
	if b == nil || b.FTS == nil || eng == nil {
		return nil
	}
	return b.FTS.IndexEngram(ws, [16]byte(eng.ID), eng.Concept, eng.CreatedBy, eng.Content, eng.Tags, 0)
}

// IndexEngram preserves compatibility with the existing FTS worker boundary.
func (b *Backend) IndexEngram(ws [8]byte, id [16]byte, concept, createdBy, content string, tags []string, _ int64) error {
	if b == nil || b.FTS == nil {
		return nil
	}
	return b.FTS.IndexEngram(ws, id, concept, createdBy, content, tags, 0)
}

// DeleteText removes the text index entries for id when enough source text is available.
func (b *Backend) DeleteText(_ context.Context, ws [8]byte, id [16]byte) error {
	// Native FTS deletion needs the original tokenized fields to remove exact posting keys.
	// Existing engine deletion still calls fts.Index.DeleteEngram with that context.
	return nil
}

// SearchText delegates to the native FTS index.
func (b *Backend) SearchText(ctx context.Context, ws [8]byte, query string, topK int) ([]search.Hit, error) {
	if b == nil || b.FTS == nil {
		return nil, nil
	}
	results, err := b.FTS.Search(ctx, ws, query, topK)
	if err != nil {
		return nil, err
	}
	return ftsHits(results), nil
}

// IndexVector delegates to the native HNSW registry.
func (b *Backend) IndexVector(ctx context.Context, ws [8]byte, id [16]byte, vec []float32) error {
	if b == nil || b.HNSW == nil || len(vec) == 0 {
		return nil
	}
	return b.HNSW.Insert(ctx, ws, id, vec)
}

// DeleteVector tombstones the vector in the native HNSW registry.
func (b *Backend) DeleteVector(_ context.Context, ws [8]byte, id [16]byte) error {
	if b == nil || b.HNSW == nil {
		return nil
	}
	b.HNSW.TombstoneNode(ws, id)
	return nil
}

// SearchVector delegates to the native HNSW registry. Filters are ignored (native
// uses post-retrieval filtering via passesMetaFilter).
func (b *Backend) SearchVector(ctx context.Context, ws [8]byte, vec []float32, opts search.VectorSearchOptions) ([]search.Hit, error) {
	if b == nil || b.HNSW == nil {
		return nil, nil
	}
	results, err := b.HNSW.Search(ctx, ws, vec, opts.TopK)
	if err != nil {
		return nil, err
	}
	return hnswHits(results), nil
}

// VaultVectorDimension reports the native HNSW dimension for a vault.
func (b *Backend) VaultVectorDimension(ws [8]byte) int {
	if b == nil || b.HNSW == nil {
		return 0
	}
	return b.HNSW.VaultEmbedDim(ws)
}

// ResetVault resets in-memory native search state for a vault.
func (b *Backend) ResetVault(_ context.Context, ws [8]byte) error {
	if b == nil {
		return nil
	}
	if b.FTS != nil {
		b.FTS.InvalidateIDFCache()
	}
	if b.HNSW != nil {
		b.HNSW.ResetVault(ws)
	}
	return nil
}

// ReindexVault rebuilds native text and vector indexes from the provided scan callback.
func (b *Backend) ReindexVault(ctx context.Context, ws [8]byte, scan func(func(*storage.Engram) error) error) error {
	if scan == nil {
		return nil
	}
	return scan(func(eng *storage.Engram) error {
		if eng == nil {
			return nil
		}
		if err := b.IndexText(ctx, ws, eng); err != nil {
			return fmt.Errorf("native search: index text: %w", err)
		}
		if len(eng.Embedding) > 0 {
			if err := b.IndexVector(ctx, ws, [16]byte(eng.ID), eng.Embedding); err != nil {
				return fmt.Errorf("native search: index vector: %w", err)
			}
		}
		return nil
	})
}

// Close closes backend resources owned by this adapter.
func (b *Backend) Close() error { return nil }

func ftsHits(results []fts.ScoredID) []search.Hit {
	out := make([]search.Hit, len(results))
	for i, r := range results {
		out[i] = search.Hit{ID: r.ID, Score: r.Score}
	}
	return out
}

func hnswHits(results []hnsw.ScoredID) []search.Hit {
	out := make([]search.Hit, len(results))
	for i, r := range results {
		out[i] = search.Hit{ID: r.ID, Score: r.Score}
	}
	return out
}
