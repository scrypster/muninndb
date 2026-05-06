//go:build vectors

package bleve

import "github.com/blevesearch/bleve/v2/mapping"

func addVectorMapping(doc *mapping.DocumentMapping, cfg Config) {
	if cfg.VectorDim <= 0 {
		return
	}
	vecField := mapping.NewVectorFieldMapping()
	vecField.Dims = cfg.VectorDim
	vecField.Similarity = cfg.similarity()
	vecField.VectorIndexOptimizedFor = cfg.VectorOptimizedFor
	doc.AddFieldMappingsAt("embedding", vecField)
}
