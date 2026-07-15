package activation

import (
	"context"
	"testing"
)

// TestNoopEmbedderReturnsNoEmbedding pins the noop contract: no embedding at
// all, so recall takes the FTS-only fast path (len(embedding)==0 in phase2)
// instead of walking HNSW with a zero vector — and a fixed 384-dim stub can
// never trip the vault dimension guard (#582) on non-384-dim vaults.
func TestNoopEmbedderReturnsNoEmbedding(t *testing.T) {
	e := NewNoopEmbedder()
	texts := []string{"hello", "world", "test"}
	vecs, err := e.Embed(context.Background(), texts)
	if err != nil {
		t.Fatalf("Embed returned unexpected error: %v", err)
	}
	if len(vecs) != 0 {
		t.Fatalf("expected no embedding from noop embedder, got %d floats", len(vecs))
	}
}

func TestNoopEmbedderTokenize(t *testing.T) {
	e := NewNoopEmbedder()
	tokens := e.Tokenize("hello world  foo")
	expected := []string{"hello", "world", "foo"}
	if len(tokens) != len(expected) {
		t.Fatalf("expected %d tokens, got %d: %v", len(expected), len(tokens), tokens)
	}
	for i, tok := range tokens {
		if tok != expected[i] {
			t.Fatalf("token[%d]: expected %q, got %q", i, expected[i], tok)
		}
	}
}

func TestNoopEmbedderTokenizeEmpty(t *testing.T) {
	e := NewNoopEmbedder()
	tokens := e.Tokenize("")
	if len(tokens) != 0 {
		t.Fatalf("expected empty tokens for empty string, got %v", tokens)
	}
}

func TestNoopEmbedderZeroTexts(t *testing.T) {
	e := NewNoopEmbedder()
	vecs, err := e.Embed(context.Background(), []string{})
	if err != nil {
		t.Fatalf("Embed returned unexpected error: %v", err)
	}
	if len(vecs) != 0 {
		t.Fatalf("expected 0 floats for empty texts, got %d", len(vecs))
	}
}

func TestNewNoopEmbedderNotNil(t *testing.T) {
	e := NewNoopEmbedder()
	if e == nil {
		t.Fatal("NewNoopEmbedder returned nil")
	}
}
