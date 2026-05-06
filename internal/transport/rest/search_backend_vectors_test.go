//go:build vectors

package rest

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	blevesearch "github.com/blevesearch/bleve/v2"
	"github.com/scrypster/muninndb/internal/search"
	"github.com/scrypster/muninndb/internal/storage"
)

func TestBleveBackendWiringServesRESTVectorActivationAndBuildsFAISSIndex(t *testing.T) {
	indexPath := filepath.Join(t.TempDir(), "search-faiss.bleve")
	t.Logf("bleve faiss service integration index path: %s", indexPath)
	eng, backend, cleanup := newBleveRESTEngine(t, indexPath, true)
	defer cleanup()
	server := NewServer("127.0.0.1:0", NewEngineWrapper(eng, nil), nil, []byte("test-secret"), nil, EmbedInfo{}, EnrichInfo{}, nil, t.TempDir(), nil)
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()

	createBody := []byte(`{"vault":"default","concept":"bleve faiss vector","content":"bleve stores vectors through the integrated KNN index","embedding":[10,10,10]}`)
	createResp, err := http.Post(httpServer.URL+"/api/engrams", "application/json", bytes.NewReader(createBody))
	if err != nil {
		t.Fatalf("POST /api/engrams: %v", err)
	}
	defer createResp.Body.Close()
	if createResp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /api/engrams status = %d, want %d", createResp.StatusCode, http.StatusCreated)
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(createResp.Body).Decode(&created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	time.Sleep(250 * time.Millisecond)

	ws := eng.Store().ResolveVaultPrefix("default")
	hits, err := backend.SearchVector(t.Context(), ws, []float32{10, 10, 10}, search.VectorSearchOptions{TopK: 1})
	if err != nil {
		t.Fatalf("direct bleve vector search: %v", err)
	}
	uid, err := storage.ParseULID(created.ID)
	if err != nil {
		t.Fatalf("ParseULID(%q): %v", created.ID, err)
	}
	if len(hits) == 0 || hits[0].ID != [16]byte(uid) {
		t.Fatalf("direct bleve vector search hits = %#v, want %x", hits, [16]byte(uid))
	}
	if err := backend.Close(); err != nil {
		t.Fatalf("close bleve backend before inspection: %v", err)
	}
	assertBleveServiceVectorIndex(t, indexPath)
}

func assertBleveServiceVectorIndex(t *testing.T, indexPath string) {
	t.Helper()
	indexPath = firstBleveVecIndexPath(t, indexPath)
	for _, name := range []string{"index_meta.json", "store"} {
		path := filepath.Join(indexPath, name)
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("expected Bleve vector index artifact %s: %v", path, err)
		}
	}
	idx, err := blevesearch.Open(indexPath)
	if err != nil {
		t.Fatalf("open persisted Bleve vector index: %v", err)
	}
	defer idx.Close()
	req := blevesearch.NewSearchRequest(blevesearch.NewMatchNoneQuery())
	req.AddKNN("embedding", []float32{10, 10, 10}, 1, 1.0)
	res, err := idx.Search(req)
	if err != nil {
		t.Fatalf("Bleve persisted vector Search: %v", err)
	}
	if len(res.Hits) == 0 {
		t.Fatal("Bleve persisted vector index returned no KNN hits")
	}
}
