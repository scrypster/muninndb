package bleve_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	blevesearch "github.com/blevesearch/bleve/v2"
	searchbleve "github.com/scrypster/muninndb/internal/search/bleve"
	"github.com/scrypster/muninndb/internal/storage"
)

func bleveFTSIndexPath(root string, ws [8]byte) string {
	return filepath.Join(root, "fts", fmt.Sprintf("%x", ws))
}

func TestBackendBuildsPersistentBleveIndex(t *testing.T) {
	ctx := context.Background()
	indexPath := filepath.Join(t.TempDir(), "search.bleve")
	t.Logf("bleve test index path: %s", indexPath)
	backend, err := searchbleve.Open(searchbleve.Config{Path: indexPath, DefaultAnalyzer: "standard"})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	ws := [8]byte{1, 2, 3, 4, 5, 6, 7, 8}
	id := storage.NewULID()
	eng := &storage.Engram{
		ID:        id,
		Concept:   "Bleve integration",
		Content:   "Bleve should build a persistent full text index",
		Tags:      []string{"bleve", "search"},
		CreatedBy: "test",
	}
	if err := backend.IndexText(ctx, ws, eng); err != nil {
		t.Fatalf("IndexText: %v", err)
	}
	hits, err := backend.SearchText(ctx, ws, "persistent full text", 10)
	if err != nil {
		t.Fatalf("SearchText: %v", err)
	}
	if len(hits) == 0 || hits[0].ID != [16]byte(id) {
		t.Fatalf("SearchText hits = %#v, want first ID %x", hits, [16]byte(id))
	}
	if err := backend.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	vaultPath := bleveFTSIndexPath(indexPath, ws)
	assertBleveIndexFiles(t, vaultPath)
	reopened, err := blevesearch.Open(vaultPath)
	if err != nil {
		t.Fatalf("bleve.Open persisted index: %v", err)
	}
	defer reopened.Close()
	count, err := reopened.DocCount()
	if err != nil {
		t.Fatalf("DocCount: %v", err)
	}
	if count != 1 {
		t.Fatalf("DocCount = %d, want 1", count)
	}
	req := blevesearch.NewSearchRequest(blevesearch.NewMatchQuery("persistent full text"))
	res, err := reopened.Search(req)
	if err != nil {
		t.Fatalf("reopened Search: %v", err)
	}
	if len(res.Hits) == 0 {
		t.Fatal("reopened index search returned no hits")
	}
}

func TestBackendAcceptsVectorIndexingWithoutVectorsTag(t *testing.T) {
	ctx := context.Background()
	indexPath := filepath.Join(t.TempDir(), "search-vector.bleve")
	t.Logf("bleve vector test index path: %s", indexPath)
	backend, err := searchbleve.Open(searchbleve.Config{Path: indexPath, DefaultAnalyzer: "standard", VectorDim: 3, Similarity: "l2_norm"})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	ws := [8]byte{8, 7, 6, 5, 4, 3, 2, 1}
	idA := storage.NewULID()
	idB := storage.NewULID()
	if err := backend.IndexText(ctx, ws, &storage.Engram{ID: idA, Concept: "vector a"}); err != nil {
		t.Fatalf("IndexText A: %v", err)
	}
	if err := backend.IndexText(ctx, ws, &storage.Engram{ID: idB, Concept: "vector b"}); err != nil {
		t.Fatalf("IndexText B: %v", err)
	}
	if err := backend.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	assertBleveIndexFiles(t, bleveFTSIndexPath(indexPath, ws))
	reopened, err := blevesearch.Open(bleveFTSIndexPath(indexPath, ws))
	if err != nil {
		t.Fatalf("bleve.Open persisted FTS index: %v", err)
	}
	defer reopened.Close()
	count, err := reopened.DocCount()
	if err != nil {
		t.Fatalf("DocCount: %v", err)
	}
	if count != 2 {
		t.Fatalf("DocCount = %d, want 2", count)
	}
}

func TestBackendKeepsSeparateIndexPerVault(t *testing.T) {
	ctx := context.Background()
	indexPath := filepath.Join(t.TempDir(), "search-vaults.bleve")
	backend, err := searchbleve.Open(searchbleve.Config{Path: indexPath, DefaultAnalyzer: "standard"})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer backend.Close()

	wsA := [8]byte{1}
	wsB := [8]byte{2}
	idA := storage.NewULID()
	idB := storage.NewULID()
	if err := backend.IndexText(ctx, wsA, &storage.Engram{ID: idA, Concept: "shared term", Content: "only vault a"}); err != nil {
		t.Fatalf("IndexText A: %v", err)
	}
	if err := backend.IndexText(ctx, wsB, &storage.Engram{ID: idB, Concept: "shared term", Content: "only vault b"}); err != nil {
		t.Fatalf("IndexText B: %v", err)
	}
	hitsA, err := backend.SearchText(ctx, wsA, "shared term", 10)
	if err != nil {
		t.Fatalf("SearchText A: %v", err)
	}
	if len(hitsA) != 1 || hitsA[0].ID != [16]byte(idA) {
		t.Fatalf("SearchText A hits = %#v, want only %x", hitsA, [16]byte(idA))
	}
	hitsB, err := backend.SearchText(ctx, wsB, "shared term", 10)
	if err != nil {
		t.Fatalf("SearchText B: %v", err)
	}
	if len(hitsB) != 1 || hitsB[0].ID != [16]byte(idB) {
		t.Fatalf("SearchText B hits = %#v, want only %x", hitsB, [16]byte(idB))
	}
	assertBleveIndexFiles(t, bleveFTSIndexPath(indexPath, wsA))
	assertBleveIndexFiles(t, bleveFTSIndexPath(indexPath, wsB))
}

func assertBleveIndexFiles(t *testing.T, indexPath string) {
	t.Helper()
	for _, name := range []string{"index_meta.json", "store"} {
		path := filepath.Join(indexPath, name)
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("expected Bleve index artifact %s: %v", path, err)
		}
	}
	entries, err := os.ReadDir(indexPath)
	if err != nil {
		t.Fatalf("ReadDir index path: %v", err)
	}
	if len(entries) < 2 {
		t.Fatalf("index path has %d entries, want persisted Bleve artifacts", len(entries))
	}
}
