//go:build vectors

package bleve_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	blevesearch "github.com/blevesearch/bleve/v2"
	"github.com/scrypster/muninndb/internal/plugin"
	"github.com/scrypster/muninndb/internal/search"
	searchadapters "github.com/scrypster/muninndb/internal/search/adapters"
	searchbleve "github.com/scrypster/muninndb/internal/search/bleve"
	"github.com/scrypster/muninndb/internal/storage"
)

func bleveVecIndexPath(root string, ws [8]byte) string {
	return filepath.Join(root, "vec", bleveVaultHex(ws))
}

func bleveVaultHex(ws [8]byte) string {
	return string([]byte{
		hexChars[ws[0]>>4], hexChars[ws[0]&0xf],
		hexChars[ws[1]>>4], hexChars[ws[1]&0xf],
		hexChars[ws[2]>>4], hexChars[ws[2]&0xf],
		hexChars[ws[3]>>4], hexChars[ws[3]&0xf],
		hexChars[ws[4]>>4], hexChars[ws[4]&0xf],
		hexChars[ws[5]>>4], hexChars[ws[5]&0xf],
		hexChars[ws[6]>>4], hexChars[ws[6]&0xf],
		hexChars[ws[7]>>4], hexChars[ws[7]&0xf],
	})
}

var hexChars = []byte("0123456789abcdef")

func TestBackendBuildsPersistentBleveVectorIndex(t *testing.T) {
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
	if err := backend.IndexText(ctx, ws, &storage.Engram{ID: idA, Concept: "vector a", CreatedBy: "u1", CreatedAt: zeroTime()}); err != nil {
		t.Fatalf("IndexText A: %v", err)
	}
	if err := backend.IndexText(ctx, ws, &storage.Engram{ID: idB, Concept: "vector b", CreatedBy: "u1", CreatedAt: zeroTime()}); err != nil {
		t.Fatalf("IndexText B: %v", err)
	}
	// Vector must be indexed separately — FTS index does not store embeddings.
	if err := backend.IndexVector(ctx, ws, [16]byte(idA), []float32{0, 0, 0}); err != nil {
		t.Fatalf("IndexVector A: %v", err)
	}
	if err := backend.IndexVector(ctx, ws, [16]byte(idB), []float32{10, 10, 10}); err != nil {
		t.Fatalf("IndexVector B: %v", err)
	}
	hits, err := backend.SearchVector(ctx, ws, []float32{10, 10, 10}, search.VectorSearchOptions{TopK: 1})
	if err != nil {
		t.Fatalf("SearchVector: %v", err)
	}
	if len(hits) != 1 || hits[0].ID != [16]byte(idB) {
		t.Fatalf("SearchVector hits = %#v, want %x", hits, [16]byte(idB))
	}
	if err := backend.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	vaultPath := bleveVecIndexPath(indexPath, ws)
	assertBleveIndexFiles(t, vaultPath)
	reopened, err := blevesearch.Open(vaultPath)
	if err != nil {
		t.Fatalf("bleve.Open persisted vector index: %v", err)
	}
	defer reopened.Close()
	req := blevesearch.NewSearchRequest(blevesearch.NewMatchNoneQuery())
	req.AddKNN("embedding", []float32{10, 10, 10}, 1, 1.0)
	res, err := reopened.Search(req)
	if err != nil {
		t.Fatalf("reopened vector Search: %v", err)
	}
	if len(res.Hits) != 1 || res.Hits[0].ID == "" {
		t.Fatalf("reopened vector hits = %#v", res.Hits)
	}
}

func TestBackendAddsVectorAfterTextOnlyIndexing(t *testing.T) {
	ctx := context.Background()
	backend, err := searchbleve.Open(searchbleve.Config{Path: filepath.Join(t.TempDir(), "search-late-vector.bleve"), DefaultAnalyzer: "standard", VectorDim: 3, Similarity: "l2_norm"})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer backend.Close()

	ws := [8]byte{1, 1, 2, 3, 5, 8, 13, 21}
	id := storage.NewULID()
	if err := backend.IndexText(ctx, ws, &storage.Engram{ID: id, Concept: "late embedding", Content: "text exists before embedding", CreatedBy: "u1", CreatedAt: zeroTime()}); err != nil {
		t.Fatalf("IndexText: %v", err)
	}
	if err := backend.IndexVector(ctx, ws, [16]byte(id), []float32{4, 5, 6}); err != nil {
		t.Fatalf("IndexVector: %v", err)
	}

	vectorHits, err := backend.SearchVector(ctx, ws, []float32{4, 5, 6}, search.VectorSearchOptions{TopK: 1})
	if err != nil {
		t.Fatalf("SearchVector: %v", err)
	}
	if len(vectorHits) != 1 || vectorHits[0].ID != [16]byte(id) {
		t.Fatalf("SearchVector hits = %#v, want %x", vectorHits, [16]byte(id))
	}
	textHits, err := backend.SearchText(ctx, ws, "text exists", 1)
	if err != nil {
		t.Fatalf("SearchText: %v", err)
	}
	if len(textHits) != 1 || textHits[0].ID != [16]byte(id) {
		t.Fatalf("SearchText hits = %#v, want %x", textHits, [16]byte(id))
	}
}

func TestBackendReindexVaultIndexesTextAndVector(t *testing.T) {
	ctx := context.Background()
	backend, err := searchbleve.Open(searchbleve.Config{Path: filepath.Join(t.TempDir(), "search-reindex-vector.bleve"), DefaultAnalyzer: "standard", VectorDim: 3, Similarity: "l2_norm"})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer backend.Close()

	ws := [8]byte{2, 4, 6, 8, 10, 12, 14, 16}
	id := storage.NewULID()
	scan := func(yield func(*storage.Engram) error) error {
		return yield(&storage.Engram{ID: id, Concept: "reindexed vector", Content: "reindex restores text and embedding", CreatedBy: "u1", CreatedAt: zeroTime(), Embedding: []float32{7, 8, 9}})
	}
	if err := backend.ReindexVault(ctx, ws, scan); err != nil {
		t.Fatalf("ReindexVault: %v", err)
	}
	vectorHits, err := backend.SearchVector(ctx, ws, []float32{7, 8, 9}, search.VectorSearchOptions{TopK: 1})
	if err != nil {
		t.Fatalf("SearchVector: %v", err)
	}
	if len(vectorHits) != 1 || vectorHits[0].ID != [16]byte(id) {
		t.Fatalf("SearchVector hits = %#v, want %x", vectorHits, [16]byte(id))
	}
}

func zeroTime() time.Time {
	return time.Time{}
}

func TestPluginStoreHNSWInsertAddsBleveVectorAfterTextOnlyIndexing(t *testing.T) {
	ctx := context.Background()
	db, err := storage.OpenPebble(t.TempDir(), storage.DefaultOptions())
	if err != nil {
		t.Fatalf("OpenPebble: %v", err)
	}
	store := storage.NewPebbleStore(db, storage.PebbleStoreConfig{CacheSize: 1000})
	defer store.Close()
	backend, err := searchbleve.Open(searchbleve.Config{Path: filepath.Join(t.TempDir(), "search-reembed-vector.bleve"), DefaultAnalyzer: "standard", VectorDim: 3, Similarity: "l2_norm"})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer backend.Close()

	ws := store.ResolveVaultPrefix("reembed")
	id, err := store.WriteEngram(ctx, ws, &storage.Engram{Concept: "reembed vector", Content: "embedding arrives later"})
	if err != nil {
		t.Fatalf("WriteEngram: %v", err)
	}
	if err := backend.IndexText(ctx, ws, &storage.Engram{ID: id, Concept: "reembed vector", Content: "embedding arrives later", CreatedBy: "u1", CreatedAt: zeroTime()}); err != nil {
		t.Fatalf("IndexText: %v", err)
	}
	pluginStore := plugin.NewStoreAdapter(store, searchadapters.HNSWRegistry{B: backend})
	if err := pluginStore.HNSWInsert(ctx, plugin.ULID(id), []float32{1, 3, 5}); err != nil {
		t.Fatalf("HNSWInsert: %v", err)
	}

	hits, err := backend.SearchVector(ctx, ws, []float32{1, 3, 5}, search.VectorSearchOptions{TopK: 1})
	if err != nil {
		t.Fatalf("SearchVector: %v", err)
	}
	if len(hits) != 1 || hits[0].ID != [16]byte(id) {
		t.Fatalf("SearchVector hits = %#v, want %x", hits, [16]byte(id))
	}
}
