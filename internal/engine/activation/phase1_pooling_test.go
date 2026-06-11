package activation

import (
	"context"
	"strings"
	"testing"
)

// flatStubEmbedder mimics the real embedder contract: Embed returns a flat
// len(texts)*dim slice (per-phrase vectors concatenated), NOT a single vector.
type flatStubEmbedder struct{ dim int }

func (s flatStubEmbedder) Embed(_ context.Context, texts []string) ([]float32, error) {
	out := make([]float32, len(texts)*s.dim)
	for i := range out {
		out[i] = 1 // non-zero so pooling produces a non-zero, normalizable vector
	}
	return out, nil
}
func (s flatStubEmbedder) Tokenize(text string) []string { return strings.Fields(text) }

// stubHNSW just needs to be non-nil so phase1 computes the query embedding.
type stubHNSW struct{}

func (stubHNSW) Search(context.Context, [8]byte, []float32, int) ([]ScoredID, error) {
	return nil, nil
}

// TestPhase1_MultiPhraseEmbedding_PooledToIndexDim is the regression test for
// #498: a multi-phrase context must yield a single dim-sized query vector. The
// embedder returns len(context)*dim concatenated; phase1 previously used that
// raw N*dim slice as the query vector, so HNSW's CosineSimilarity length guard
// returned 0 for every node and semantic recall silently returned nothing for
// any context with 2+ phrases.
func TestPhase1_MultiPhraseEmbedding_PooledToIndexDim(t *testing.T) {
	const dim = 8
	e := &ActivationEngine{embedder: flatStubEmbedder{dim: dim}, hnsw: stubHNSW{}}
	ctx := context.Background()

	single, err := e.phase1(ctx, &ActivateRequest{Context: []string{"one phrase"}})
	if err != nil {
		t.Fatalf("phase1 single: %v", err)
	}
	if len(single.embedding) != dim {
		t.Fatalf("single-phrase embedding len = %d, want %d", len(single.embedding), dim)
	}

	for _, n := range []int{2, 3, 5} {
		ctxPhrases := make([]string, n)
		for i := range ctxPhrases {
			ctxPhrases[i] = "phrase"
		}
		multi, err := e.phase1(ctx, &ActivateRequest{Context: ctxPhrases})
		if err != nil {
			t.Fatalf("phase1 %d-phrase: %v", n, err)
		}
		if len(multi.embedding) != dim {
			t.Errorf("%d-phrase embedding len = %d, want %d (must be pooled to index dim, not N*dim)",
				n, len(multi.embedding), dim)
		}
	}
}
