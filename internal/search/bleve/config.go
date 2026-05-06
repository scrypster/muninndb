package bleve

// Config controls the Bleve-backed search implementation.
type Config struct {
	Path               string
	DefaultAnalyzer    string
	VectorDim          int
	Similarity         string
	VectorOptimizedFor string
}

func (c Config) analyzer() string {
	if c.DefaultAnalyzer != "" {
		return c.DefaultAnalyzer
	}
	return "standard"
}

func (c Config) similarity() string {
	if c.Similarity != "" {
		return c.Similarity
	}
	return "dot_product"
}

// VectorDimension reports the fixed embedding dimension configured for this backend.
func (b *Backend) VectorDimension() int {
	if b == nil {
		return 0
	}
	return b.cfg.VectorDim
}
