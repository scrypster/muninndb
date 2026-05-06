package search

import (
	"context"
	"io"

	"github.com/scrypster/muninndb/internal/storage"
)

// Hit is a scored search result returned by a search backend.
type Hit struct {
	ID    [16]byte
	Score float64
}

// TextIndexer writes and removes text-search documents.
type TextIndexer interface {
	IndexText(ctx context.Context, ws [8]byte, eng *storage.Engram) error
	DeleteText(ctx context.Context, ws [8]byte, id [16]byte) error
}

// TextSearcher finds documents by text query.
type TextSearcher interface {
	SearchText(ctx context.Context, ws [8]byte, query string, topK int) ([]Hit, error)
}

// VectorIndexer writes and removes vector-search documents.
type VectorIndexer interface {
	IndexVector(ctx context.Context, ws [8]byte, id [16]byte, vec []float32) error
	DeleteVector(ctx context.Context, ws [8]byte, id [16]byte) error
}

// Filter is a query filter applied by the search backend (e.g. bleve pre-filter).
type Filter struct {
	Field string
	Op    string
	Value any
}

// VectorSearchOptions controls a vector search request.
type VectorSearchOptions struct {
	TopK    int
	Filters []Filter
}

// VectorSearcher finds documents by vector similarity.
type VectorSearcher interface {
	SearchVector(ctx context.Context, ws [8]byte, vec []float32, opts VectorSearchOptions) ([]Hit, error)
}

// VectorDimensionProvider optionally reports the fixed vector dimension for a backend.
type VectorDimensionProvider interface {
	VectorDimension() int
}

// VaultVectorDimensionProvider optionally reports vector dimension per vault.
type VaultVectorDimensionProvider interface {
	VaultVectorDimension(ws [8]byte) int
}

// VaultLifecycle resets or rebuilds per-vault search state.
type VaultLifecycle interface {
	ResetVault(ctx context.Context, ws [8]byte) error
	ReindexVault(ctx context.Context, ws [8]byte, scan func(func(*storage.Engram) error) error) error
}

// Backend is the complete search backend boundary used by engine wiring.
type Backend interface {
	TextIndexer
	TextSearcher
	VectorIndexer
	VectorSearcher
	VaultLifecycle
	io.Closer
}
