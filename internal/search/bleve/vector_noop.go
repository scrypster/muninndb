//go:build !vectors

package bleve

import (
	"context"
	"errors"

	"github.com/scrypster/muninndb/internal/search"
)

// ErrVectorSearchUnavailable is returned by vector operations in builds without -tags vectors.
var ErrVectorSearchUnavailable = errors.New("bleve vector search requires -tags vectors")

// VectorAvailable reports whether this build includes Bleve vector/KNN support.
func VectorAvailable() bool { return false }

// IndexVector is unavailable unless built with -tags vectors.
func (b *Backend) IndexVector(context.Context, [8]byte, [16]byte, []float32) error {
	return ErrVectorSearchUnavailable
}

// SearchVector is unavailable unless built with -tags vectors.
func (b *Backend) SearchVector(context.Context, [8]byte, []float32, search.VectorSearchOptions) ([]search.Hit, error) {
	return nil, ErrVectorSearchUnavailable
}
