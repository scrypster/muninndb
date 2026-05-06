//go:build vectors

package bleve

import (
	"context"
	"fmt"
	"strings"

	blevesearch "github.com/blevesearch/bleve/v2"
	"github.com/scrypster/muninndb/internal/search"
	"github.com/scrypster/muninndb/internal/storage"
)

// VectorAvailable reports whether this build includes Bleve vector/KNN support.
func VectorAvailable() bool { return true }

// IndexVector indexes a vector into Bleve's integrated KNN/FAISS path.
func (b *Backend) IndexVector(ctx context.Context, ws [8]byte, id [16]byte, vec []float32) error {
	return b.indexVector(ctx, ws, id, vec)
}

// SearchVector searches Bleve's integrated KNN/FAISS path.
func (b *Backend) SearchVector(ctx context.Context, ws [8]byte, vec []float32, opts search.VectorSearchOptions) ([]search.Hit, error) {
	if len(vec) == 0 || opts.TopK <= 0 {
		return nil, nil
	}
	idx, err := b.vecForVault(ws)
	if err != nil || idx == nil {
		return nil, err
	}
	filterQuery := buildFilterQuery(opts.Filters)
	req := blevesearch.NewSearchRequest(blevesearch.NewMatchNoneQuery())
	if filterQuery != nil {
		req.AddKNNWithFilter("embedding", vec, int64(opts.TopK), 1.0, filterQuery)
	} else {
		req.AddKNN("embedding", vec, int64(opts.TopK), 1.0)
	}
	req.Size = opts.TopK
	res, err := idx.SearchInContext(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("bleve search vector: %w", err)
	}
	out := make([]search.Hit, 0, len(res.Hits))
	for _, h := range res.Hits {
		_, idText, ok := strings.Cut(h.ID, ":")
		if !ok {
			continue
		}
		id, err := storage.ParseULID(idText)
		if err == nil {
			out = append(out, search.Hit{ID: [16]byte(id), Score: h.Score})
		}
	}
	return out, nil
}
