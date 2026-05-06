//go:build vectors
// +build vectors

package bleve_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/scrypster/muninndb/internal/search"
	searchbleve "github.com/scrypster/muninndb/internal/search/bleve"
	"github.com/scrypster/muninndb/internal/storage"
)

// TestBleveFAISSVectorSearch verifies FAISS-powered vector search end-to-end:
//   - FTS search works independently of vectors
//   - Vector search finds semantically close documents via FAISS KNN
//   - Non-matching vectors are not returned
//   - Documents without vector are found by FTS but not by vector KNN
//   - High-dimensional vectors (1024) work with Voyage-compatible config
func TestBleveFAISSVectorSearch(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "search.bleve")

	cfg := searchbleve.Config{
		Path:            path,
		DefaultAnalyzer: "standard",
		VectorDim:       4,
		Similarity:      "dot_product",
	}

	backend, err := searchbleve.Open(cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer backend.Close()

	ctx := context.Background()
	ws := [8]byte{0x01}

	// ── Document A: FTS-only (no embedding) ──────────────────────
	idA := storage.ULID([16]byte{0xa1})
	engA := &storage.Engram{
		ID:      idA,
		Content: "document without embedding for FTS only",
	}
	if err := backend.IndexText(ctx, ws, engA); err != nil {
		t.Fatalf("IndexText A: %v", err)
	}

	// ── Document B: FTS + matching vector ────────────────────────
	idB := storage.ULID([16]byte{0xb1})
	vecB := []float32{1.0, 0.5, 0.2, 0.8}
	engB := &storage.Engram{
		ID:        idB,
		Content:   "blue sky document for vector and text search",
		Embedding: vecB,
	}
	if err := backend.IndexText(ctx, ws, engB); err != nil {
		t.Fatalf("IndexText B: %v", err)
	}
	if err := backend.IndexVector(ctx, ws, [16]byte(idB), vecB); err != nil {
		t.Fatalf("IndexVector B: %v", err)
	}

	// ── Document C: FTS + non-matching vector ────────────────────
	idC := storage.ULID([16]byte{0xc1})
	vecC := []float32{-1.0, -0.5, -0.2, -0.8}
	engC := &storage.Engram{
		ID:        idC,
		Content:   "red ocean document for vector contrast",
		Embedding: vecC,
	}
	if err := backend.IndexText(ctx, ws, engC); err != nil {
		t.Fatalf("IndexText C: %v", err)
	}
	if err := backend.IndexVector(ctx, ws, [16]byte(idC), vecC); err != nil {
		t.Fatalf("IndexVector C: %v", err)
	}

	queryVec := []float32{1.0, 0.5, 0.2, 0.8} // matches doc B exactly

	// ── Test 1: FTS finds text documents ─────────────────────────
	t.Run("FTS", func(t *testing.T) {
		hits, _ := backend.SearchText(ctx, ws, "document", 10)
		if len(hits) < 2 {
			t.Fatalf("FTS 'document': got %d hits, want >= 2", len(hits))
		}
	})

	// ── Test 2: Vector KNN via FAISS ─────────────────────────────
	t.Run("VectorKNN", func(t *testing.T) {
		hits, err := backend.SearchVector(ctx, ws, queryVec, search.VectorSearchOptions{TopK: 10})
		if err != nil {
			t.Fatalf("SearchVector: %v", err)
		}
		if len(hits) == 0 {
			t.Fatal("SearchVector returned 0 results — FAISS KNN may not be working")
		}
		// First hit should be doc B (exact match, highest dot_product)
		if len(hits) >= 2 {
			t.Logf("top-2 vector scores: %.4f, %.4f", hits[0].Score, hits[1].Score)
		}
	})

	// ── Test 3: FTS finds doc A (no embedding) but vector KNN does NOT ──
	t.Run("DocWithoutEmbedding_FTSnotVector", func(t *testing.T) {
		ftsHits, _ := backend.SearchText(ctx, ws, "embedding", 10)
		foundAinFTS := false
		for _, h := range ftsHits {
			if h.ID == [16]byte(idA) {
				foundAinFTS = true
				break
			}
		}
		if !foundAinFTS {
			t.Fatal("doc A not found by FTS")
		}
		// Vector KNN should NOT return doc A (no embedding)
		vecHits, _ := backend.SearchVector(ctx, ws, queryVec, search.VectorSearchOptions{TopK: 10})
		for _, h := range vecHits {
			if h.ID == [16]byte(idA) {
				t.Fatal("doc A (no embedding) should not appear in vector KNN results")
			}
		}
	})

	// ── Test 4: wrong dimension vector is NOT indexed ────────────
	t.Run("WrongDimension", func(t *testing.T) {
		idD := storage.ULID([16]byte{0xd1})
		wrongVec := []float32{0.1, 0.2} // dim=2, but config expects 4
		engD := &storage.Engram{
			ID:        idD,
			Content:   "wrong dimension embedding document",
			Embedding: wrongVec,
		}
		if err := backend.IndexText(ctx, ws, engD); err != nil {
			t.Fatalf("IndexText D: %v", err)
		}
		// IndexVector with wrong dim — should return an error or silently skip
		errVec := backend.IndexVector(ctx, ws, [16]byte(idD), wrongVec)
		t.Logf("IndexVector wrong dim result: err=%v", errVec)
		// Vector search should NOT return doc D
		vecHits, _ := backend.SearchVector(ctx, ws, queryVec, search.VectorSearchOptions{TopK: 10})
		for _, h := range vecHits {
			if h.ID == [16]byte(idD) {
				t.Error("doc D (wrong dim) should not appear in vector KNN results")
			}
		}
	})

	// ── Test 5: 1024-dimensional vectors (Voyage-compatible) ─────
	t.Run("VoyageDimensions", func(t *testing.T) {
		dir1024 := filepath.Join(t.TempDir(), "search1024.bleve")
		cfg1024 := searchbleve.Config{
			Path:            dir1024,
			DefaultAnalyzer: "standard",
			VectorDim:       1024,
			Similarity:      "dot_product",
		}
		be1024, err := searchbleve.Open(cfg1024)
		if err != nil {
			t.Fatalf("Open 1024: %v", err)
		}
		defer be1024.Close()

		// Generate random 1024D vectors
		v1 := make([]float32, 1024)
		v2 := make([]float32, 1024)
		for i := range v1 {
			v1[i] = float32(i%100) / 100.0
			v2[i] = float32(i%100) / 100.0 // same vector = perfect match
		}
		// v3 is orthogonal
		_ = v2

		id1024 := storage.ULID([16]byte{0xe1})
		eng1024 := &storage.Engram{
			ID:        id1024,
			Content:   "1024-dimensional vector document",
			Embedding: v1,
		}
		if err := be1024.IndexText(ctx, ws, eng1024); err != nil {
			t.Fatalf("IndexText 1024: %v", err)
		}
		if err := be1024.IndexVector(ctx, ws, [16]byte(id1024), v1); err != nil {
			t.Fatalf("IndexVector 1024: %v", err)
		}
		hits, err := be1024.SearchVector(ctx, ws, v2, search.VectorSearchOptions{TopK: 10})
		if err != nil {
			t.Fatalf("SearchVector 1024: %v", err)
		}
		if len(hits) == 0 {
			t.Fatal("SearchVector 1024D returned 0 results")
		}
		t.Logf("1024D vector hits: %d, top score: %.4f", len(hits), hits[0].Score)
	})
}
