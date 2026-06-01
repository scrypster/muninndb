//go:build bleve

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
	"github.com/scrypster/muninndb/internal/engine"
	"github.com/scrypster/muninndb/internal/engine/activation"
	"github.com/scrypster/muninndb/internal/engine/trigger"
	hnswpkg "github.com/scrypster/muninndb/internal/index/hnsw"
	"github.com/scrypster/muninndb/internal/search"
	searchadapters "github.com/scrypster/muninndb/internal/search/adapters"
	searchbleve "github.com/scrypster/muninndb/internal/search/bleve"
	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

func newBleveRESTEngine(t *testing.T, indexPath string, useBleveVector bool) (*engine.Engine, search.Backend, func()) {
	t.Helper()
	dir, err := os.MkdirTemp("", "muninndb-bleve-rest-store-*")
	if err != nil {
		t.Fatal(err)
	}
	db, err := storage.OpenPebble(dir, storage.DefaultOptions())
	if err != nil {
		os.RemoveAll(dir)
		t.Fatal(err)
	}
	store := storage.NewPebbleStore(db, storage.PebbleStoreConfig{CacheSize: 1000})
	hnswRegistry := hnswpkg.NewRegistry(db)
	searchBackend, err := searchbleve.Open(searchbleve.Config{Path: indexPath, DefaultAnalyzer: "standard", VectorDim: 3, Similarity: "l2_norm"})
	if err != nil {
		store.Close()
		os.RemoveAll(dir)
		t.Fatalf("open bleve backend: %v", err)
	}
	embedder := &testEmbedder{}
	var vectorIndex hnswpkg.RegistryIndex = hnswRegistry
	var actVector activation.HNSWIndex = activation.NewHNSWAdapter(hnswRegistry)
	var trigVector trigger.HNSWIndex = trigger.NewHNSWAdapter(hnswRegistry)
	if useBleveVector {
		vectorIndex = searchadapters.HNSWRegistry{B: searchBackend}
		actVector = searchadapters.ActivationVector{B: searchBackend}
		trigVector = searchadapters.TriggerVector{B: searchBackend}
	}
	actEngine := activation.New(store, searchadapters.ActivationFTS{B: searchBackend}, actVector, embedder)
	trigSystem := trigger.New(store, searchadapters.TriggerFTS{B: searchBackend}, trigVector, embedder)
	eng := engine.NewEngine(engine.EngineConfig{
		Store:            store,
		FTSIndex:         searchadapters.FTSIndex{B: searchBackend},
		ActivationEngine: actEngine,
		TriggerSystem:    trigSystem,
		Embedder:         embedder,
		HNSWRegistry:     vectorIndex,
	})
	return eng, searchBackend, func() {
		eng.Stop()
		store.Close()
		os.RemoveAll(dir)
	}
}

func TestBleveBackendWiringServesRESTActivationAndBuildsIndex(t *testing.T) {
	indexPath := filepath.Join(t.TempDir(), "search.bleve")
	t.Logf("bleve service integration index path: %s", indexPath)
	eng, backend, cleanup := newBleveRESTEngine(t, indexPath, false)
	defer cleanup()
	server := NewServer("127.0.0.1:0", NewEngineWrapper(eng, nil), nil, []byte("test-secret"), nil, EmbedInfo{}, EnrichInfo{}, nil, t.TempDir(), nil)
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()

	createBody := []byte(`{"vault":"default","concept":"bleve service integration","content":"bleve backend builds a persistent service level search index"}`)
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

	activateBody := []byte(`{"vault":"default","context":["persistent service search index"],"max_results":5,"disable_hops":true,"weights":{"full_text_relevance":1,"disable_actr":true}}`)
	activateResp, err := http.Post(httpServer.URL+"/api/activate", "application/json", bytes.NewReader(activateBody))
	if err != nil {
		t.Fatalf("POST /api/activate: %v", err)
	}
	defer activateResp.Body.Close()
	if activateResp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/activate status = %d, want %d", activateResp.StatusCode, http.StatusOK)
	}
	var activated mbp.ActivateResponse
	if err := json.NewDecoder(activateResp.Body).Decode(&activated); err != nil {
		t.Fatalf("decode activate response: %v", err)
	}
	if activated.TotalFound == 0 || len(activated.Activations) == 0 {
		t.Fatalf("REST activate returned no results: %#v", activated)
	}
	if activated.Activations[0].ID != created.ID {
		t.Fatalf("REST top activation ID = %s, want %s", activated.Activations[0].ID, created.ID)
	}
	if err := backend.Close(); err != nil {
		t.Fatalf("close bleve backend before inspection: %v", err)
	}
	assertBleveServiceIndex(t, indexPath)
}

func TestBleveTextBackendUsesNativeHNSWWhenVectorBackendDisabled(t *testing.T) {
	indexPath := filepath.Join(t.TempDir(), "search-text-native-hnsw.bleve")
	eng, _, cleanup := newBleveRESTEngine(t, indexPath, false)
	defer cleanup()
	server := NewServer("127.0.0.1:0", NewEngineWrapper(eng, nil), nil, []byte("test-secret"), nil, EmbedInfo{}, EnrichInfo{}, nil, t.TempDir(), nil)
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()

	createBody := []byte(`{"vault":"default","concept":"bleve text native hnsw","content":"text backend keeps native vector index without vectors build","embedding":[1,2,3]}`)
	createResp, err := http.Post(httpServer.URL+"/api/engrams", "application/json", bytes.NewReader(createBody))
	if err != nil {
		t.Fatalf("POST /api/engrams: %v", err)
	}
	defer createResp.Body.Close()
	if createResp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /api/engrams status = %d, want %d", createResp.StatusCode, http.StatusCreated)
	}
	if dim := eng.GetVaultEmbedDim(t.Context(), "default"); dim != 3 {
		t.Fatalf("GetVaultEmbedDim = %d, want native HNSW dim 3", dim)
	}
}

func assertBleveServiceIndex(t *testing.T, indexPath string) {
	t.Helper()
	indexPath = firstBleveVaultIndexPath(t, indexPath)
	for _, name := range []string{"index_meta.json", "store"} {
		path := filepath.Join(indexPath, name)
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("expected Bleve index artifact %s: %v", path, err)
		}
	}
	idx, err := blevesearch.Open(indexPath)
	if err != nil {
		t.Fatalf("open persisted Bleve index: %v", err)
	}
	defer idx.Close()
	count, err := idx.DocCount()
	if err != nil {
		t.Fatalf("Bleve DocCount: %v", err)
	}
	if count != 1 {
		t.Fatalf("Bleve DocCount = %d, want 1", count)
	}
	res, err := idx.Search(blevesearch.NewSearchRequest(blevesearch.NewMatchQuery("persistent service search index")))
	if err != nil {
		t.Fatalf("Bleve persisted Search: %v", err)
	}
	if len(res.Hits) == 0 {
		t.Fatal("Bleve persisted index search returned no hits")
	}
}

func firstBleveVaultIndexPath(t *testing.T, root string) string {
	t.Helper()
	// New split layout: {root}/fts/{vault_hex}/index_meta.json
	// Also check old flat layout for backward compatibility.
	for _, sub := range []string{"fts", ""} {
		searchRoot := root
		if sub != "" {
			searchRoot = filepath.Join(root, sub)
		}
		entries, err := os.ReadDir(searchRoot)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			t.Fatalf("ReadDir Bleve root %s: %v", searchRoot, err)
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			path := filepath.Join(searchRoot, entry.Name())
			if _, err := os.Stat(filepath.Join(path, "index_meta.json")); err == nil {
				return path
			}
		}
	}
	t.Fatalf("no per-vault Bleve index found under %s", root)
	return ""
}

// firstBleveVecIndexPath finds the first per-vault vector index directory under root.
func firstBleveVecIndexPath(t *testing.T, root string) string {
	t.Helper()
	vecRoot := filepath.Join(root, "vec")
	entries, err := os.ReadDir(vecRoot)
	if err != nil {
		t.Fatalf("ReadDir Bleve vec root %s: %v", vecRoot, err)
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		path := filepath.Join(vecRoot, entry.Name())
		if _, err := os.Stat(filepath.Join(path, "index_meta.json")); err == nil {
			return path
		}
	}
	t.Fatalf("no per-vault Bleve vector index found under %s/vec", root)
	return ""
}
