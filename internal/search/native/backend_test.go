package native_test

import (
	"context"
	"os"
	"testing"

	"github.com/scrypster/muninndb/internal/index/fts"
	"github.com/scrypster/muninndb/internal/index/hnsw"
	"github.com/scrypster/muninndb/internal/search"
	"github.com/scrypster/muninndb/internal/search/native"
	"github.com/scrypster/muninndb/internal/storage"
)

func newTestBackend(t *testing.T) (*native.Backend, *storage.PebbleStore, [8]byte) {
	t.Helper()
	dir, err := os.MkdirTemp("", "muninndb-search-native-*")
	if err != nil {
		t.Fatalf("create temp dir: %v", err)
	}
	db, err := storage.OpenPebble(dir, storage.DefaultOptions())
	if err != nil {
		os.RemoveAll(dir)
		t.Fatalf("open pebble: %v", err)
	}
	store := storage.NewPebbleStore(db, storage.PebbleStoreConfig{CacheSize: 100})
	t.Cleanup(func() {
		store.Close()
		os.RemoveAll(dir)
	})
	backend := native.New(fts.New(db), hnsw.NewRegistry(db))
	return backend, store, store.VaultPrefix("native-search-test")
}

func TestBackendIndexAndSearchText(t *testing.T) {
	backend, _, ws := newTestBackend(t)
	ctx := context.Background()
	id := storage.ULID{1}
	eng := &storage.Engram{
		ID:        id,
		Concept:   "Go search backend",
		CreatedBy: "tester",
		Content:   "Native backend indexes full text through Pebble FTS",
		Tags:      []string{"search", "native"},
	}

	if err := backend.IndexText(ctx, ws, eng); err != nil {
		t.Fatalf("IndexText: %v", err)
	}
	hits, err := backend.SearchText(ctx, ws, "full text search", 10)
	if err != nil {
		t.Fatalf("SearchText: %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("SearchText returned no hits")
	}
	if hits[0].ID != [16]byte(id) {
		t.Fatalf("top hit ID = %x, want %x", hits[0].ID, [16]byte(id))
	}
	if hits[0].Score <= 0 {
		t.Fatalf("top hit score = %v, want > 0", hits[0].Score)
	}
}

func TestBackendIndexSearchAndDeleteVector(t *testing.T) {
	backend, _, ws := newTestBackend(t)
	ctx := context.Background()
	id := [16]byte{2}
	vec := []float32{1, 0, 0, 0}

	if err := backend.IndexVector(ctx, ws, id, vec); err != nil {
		t.Fatalf("IndexVector: %v", err)
	}
	hits, err := backend.SearchVector(ctx, ws, vec, search.VectorSearchOptions{TopK: 1})
	if err != nil {
		t.Fatalf("SearchVector: %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("SearchVector returned no hits")
	}
	if hits[0].ID != id {
		t.Fatalf("top vector hit ID = %x, want %x", hits[0].ID, id)
	}
	if hits[0].Score < 0.99 {
		t.Fatalf("top vector score = %v, want >= 0.99", hits[0].Score)
	}

	if err := backend.DeleteVector(ctx, ws, id); err != nil {
		t.Fatalf("DeleteVector: %v", err)
	}
	hits, err = backend.SearchVector(ctx, ws, vec, search.VectorSearchOptions{TopK: 1})
	if err != nil {
		t.Fatalf("SearchVector after DeleteVector: %v", err)
	}
	if len(hits) != 0 {
		t.Fatalf("SearchVector after DeleteVector returned %d hits, want 0", len(hits))
	}
}

func TestBackendReindexVaultIndexesTextAndVector(t *testing.T) {
	backend, _, ws := newTestBackend(t)
	ctx := context.Background()
	id := storage.ULID{3}
	eng := &storage.Engram{
		ID:        id,
		Concept:   "Reindex native backend",
		Content:   "Reindex callback rebuilds text and vector search state",
		Embedding: []float32{0, 1, 0, 0},
	}
	scan := func(fn func(*storage.Engram) error) error {
		return fn(eng)
	}

	if err := backend.ReindexVault(ctx, ws, scan); err != nil {
		t.Fatalf("ReindexVault: %v", err)
	}
	textHits, err := backend.SearchText(ctx, ws, "rebuilds text", 10)
	if err != nil {
		t.Fatalf("SearchText: %v", err)
	}
	if len(textHits) == 0 || textHits[0].ID != [16]byte(id) {
		t.Fatalf("SearchText hits = %#v, want first ID %x", textHits, [16]byte(id))
	}
	vectorHits, err := backend.SearchVector(ctx, ws, eng.Embedding, search.VectorSearchOptions{TopK: 1})
	if err != nil {
		t.Fatalf("SearchVector: %v", err)
	}
	if len(vectorHits) == 0 || vectorHits[0].ID != [16]byte(id) {
		t.Fatalf("SearchVector hits = %#v, want first ID %x", vectorHits, [16]byte(id))
	}
}

func TestBackendDeleteTextRemovesFTSIndexEntries(t *testing.T) {
	backend, _, ws := newTestBackend(t)
	ctx := context.Background()
	id := storage.ULID{4}
	eng := &storage.Engram{
		ID:      id,
		Concept: "Delete text works",
		Content: "Native DeleteText now receives text fields and removes FTS posting entries",
	}

	if err := backend.IndexText(ctx, ws, eng); err != nil {
		t.Fatalf("IndexText: %v", err)
	}
	// Verify the engram is searchable before deletion.
	hits, err := backend.SearchText(ctx, ws, "posting entries", 10)
	if err != nil {
		t.Fatalf("SearchText before delete: %v", err)
	}
	if len(hits) == 0 || hits[0].ID != [16]byte(id) {
		t.Fatalf("expected engram to be searchable before DeleteText, got hits=%v", hits)
	}

	if err := backend.DeleteText(ctx, ws, eng); err != nil {
		t.Fatalf("DeleteText: %v", err)
	}
	hits, err = backend.SearchText(ctx, ws, "posting entries", 10)
	if err != nil {
		t.Fatalf("SearchText after delete: %v", err)
	}
	if len(hits) != 0 {
		t.Fatalf("DeleteText should have removed FTS entries, but got %d hits", len(hits))
	}
}
