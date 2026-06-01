//go:build bleve

package factory

import (
	"github.com/scrypster/muninndb/internal/search"
	searchbleve "github.com/scrypster/muninndb/internal/search/bleve"
)

// OpenBleve creates a new Bleve-backed search backend from the provided configuration.
// Only available when built with -tags bleve.
func OpenBleve(cfg BleveConfig) (search.Backend, error) {
	return searchbleve.Open(searchbleve.Config{
		Path:               cfg.Path,
		DefaultAnalyzer:    cfg.DefaultAnalyzer,
		VectorDim:          cfg.VectorDim,
		Similarity:         cfg.Similarity,
		VectorOptimizedFor: cfg.VectorOptimizedFor,
	})
}
