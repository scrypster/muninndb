// Package factory provides build-tag-gated creation of search backends.
// This file is always compiled and holds the BleveConfig type shared across
// factory implementations.
package factory

// BleveConfig holds the subset of Bleve backend configuration needed by callers.
// The factory translates this to the upstream bleve.Config.
type BleveConfig struct {
	Path               string
	DefaultAnalyzer    string
	VectorDim          int
	Similarity         string
	VectorOptimizedFor string
}
