package rest

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/cockroachdb/pebble"
	"github.com/scrypster/muninndb/internal/engine"
	"github.com/scrypster/muninndb/internal/engine/activation"
	"github.com/scrypster/muninndb/internal/engine/trigger"
	"github.com/scrypster/muninndb/internal/index/fts"
	hnswpkg "github.com/scrypster/muninndb/internal/index/hnsw"
	searchadapters "github.com/scrypster/muninndb/internal/search/adapters"
	"github.com/scrypster/muninndb/internal/search/native"
	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/storage/keys"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

type testEmbedder struct{}

func (e *testEmbedder) Embed(context.Context, []string) ([]float32, error) {
	return make([]float32, 384), nil
}

func (e *testEmbedder) Tokenize(text string) []string { return []string{text} }

func newSearchBackendRESTEngine(t *testing.T) (*engine.Engine, *pebble.DB, *storage.PebbleStore, func()) {
	t.Helper()
	dir, err := os.MkdirTemp("", "muninndb-search-backend-rest-*")
	if err != nil {
		t.Fatal(err)
	}
	db, err := storage.OpenPebble(dir, storage.DefaultOptions())
	if err != nil {
		os.RemoveAll(dir)
		t.Fatal(err)
	}
	store := storage.NewPebbleStore(db, storage.PebbleStoreConfig{CacheSize: 1000})
	ftsIdx := fts.New(db)
	hnswRegistry := hnswpkg.NewRegistry(db)
	searchBackend := native.New(ftsIdx, hnswRegistry)
	embedder := &testEmbedder{}
	actEngine := activation.New(store, searchadapters.ActivationFTS{B: searchBackend}, searchadapters.ActivationVector{B: searchBackend}, embedder)
	trigSystem := trigger.New(store, searchadapters.TriggerFTS{B: searchBackend}, searchadapters.TriggerVector{B: searchBackend}, embedder)
	eng := engine.NewEngine(engine.EngineConfig{
		Store:            store,
		FTSIndex:         ftsIdx,
		ActivationEngine: actEngine,
		TriggerSystem:    trigSystem,
		Embedder:         embedder,
		HNSWRegistry:     searchadapters.HNSWRegistry{B: searchBackend},
	})
	return eng, db, store, func() {
		eng.Stop()
		store.Close()
		os.RemoveAll(dir)
	}
}

func TestSearchBackendWiringServesRESTActivation(t *testing.T) {
	eng, db, store, cleanup := newSearchBackendRESTEngine(t)
	defer cleanup()
	server := NewServer("127.0.0.1:0", NewEngineWrapper(eng, nil), nil, []byte("test-secret"), nil, EmbedInfo{}, EnrichInfo{}, nil, t.TempDir(), nil)
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()

	createBody := []byte(`{"vault":"default","concept":"rest search backend","content":"service level integration reaches native search backend"}`)
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

	activateBody := []byte(`{"vault":"default","context":["service integration native backend"],"max_results":5,"disable_hops":true,"weights":{"full_text_relevance":1,"disable_actr":true}}`)
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
	assertNativeFTSIndexKeys(t, db, store, "default", created.ID)
}

func assertNativeFTSIndexKeys(t *testing.T, db *pebble.DB, store *storage.PebbleStore, vault, id string) {
	t.Helper()
	uid, err := storage.ParseULID(id)
	if err != nil {
		t.Fatalf("ParseULID(%q): %v", id, err)
	}
	ws := store.VaultPrefix(vault)
	if got := countKeysWithPrefix(t, db, []byte{0x05}, ws); got == 0 {
		t.Fatal("native FTS posting index has no 0x05 keys for vault")
	}
	if got := countKeysWithPrefix(t, db, []byte{0x06}, ws); got == 0 {
		t.Fatal("native FTS trigram index has no 0x06 keys for vault")
	}
	if got := countKeysWithPrefix(t, db, []byte{0x09}, ws); got == 0 {
		t.Fatal("native FTS term stats index has no 0x09 keys for vault")
	}
	if val, err := storage.Get(db, keys.FTSStatsKey(ws)); err != nil {
		t.Fatalf("read FTS stats key: %v", err)
	} else if len(val) < 8 || binary.BigEndian.Uint64(val[:8]) == 0 {
		t.Fatalf("FTS stats key missing indexed document count: %x", val)
	}
	if !hasPostingForID(t, db, ws, [16]byte(uid)) {
		t.Fatalf("native FTS posting index has no posting for created ID %s", id)
	}
}

func countKeysWithPrefix(t *testing.T, db *pebble.DB, namespace []byte, ws [8]byte) int {
	t.Helper()
	prefix := make([]byte, 1+len(ws))
	copy(prefix, namespace)
	copy(prefix[1:], ws[:])
	iter, err := storage.PrefixIterator(db, prefix)
	if err != nil {
		t.Fatalf("PrefixIterator %x: %v", prefix, err)
	}
	defer iter.Close()
	count := 0
	for iter.First(); iter.Valid(); iter.Next() {
		count++
	}
	if err := iter.Error(); err != nil {
		t.Fatalf("iterate prefix %x: %v", prefix, err)
	}
	return count
}

func hasPostingForID(t *testing.T, db *pebble.DB, ws [8]byte, id [16]byte) bool {
	t.Helper()
	prefix := make([]byte, 9)
	prefix[0] = 0x05
	copy(prefix[1:], ws[:])
	iter, err := storage.PrefixIterator(db, prefix)
	if err != nil {
		t.Fatalf("PrefixIterator postings: %v", err)
	}
	defer iter.Close()
	for iter.First(); iter.Valid(); iter.Next() {
		key := iter.Key()
		if len(key) >= 16 && bytes.Equal(key[len(key)-16:], id[:]) {
			return true
		}
	}
	if err := iter.Error(); err != nil {
		t.Fatalf("iterate postings: %v", err)
	}
	return false
}
