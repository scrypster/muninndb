//go:build !vectors

package bleve_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/scrypster/muninndb/internal/search"
	searchbleve "github.com/scrypster/muninndb/internal/search/bleve"
	"github.com/scrypster/muninndb/internal/storage"
)

func TestBackendVectorOperationsRequireVectorsBuildTag(t *testing.T) {
	backend, err := searchbleve.Open(searchbleve.Config{Path: filepath.Join(t.TempDir(), "search-no-vector.bleve"), DefaultAnalyzer: "standard", VectorDim: 3})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer backend.Close()

	ctx := context.Background()
	if err := backend.IndexVector(ctx, [8]byte{}, [16]byte(storage.NewULID()), []float32{1, 2, 3}); !errors.Is(err, searchbleve.ErrVectorSearchUnavailable) {
		t.Fatalf("IndexVector error = %v, want ErrVectorSearchUnavailable", err)
	}
	if _, err := backend.SearchVector(ctx, [8]byte{}, []float32{1, 2, 3}, search.VectorSearchOptions{TopK: 1}); !errors.Is(err, searchbleve.ErrVectorSearchUnavailable) {
		t.Fatalf("SearchVector error = %v, want ErrVectorSearchUnavailable", err)
	}
}
