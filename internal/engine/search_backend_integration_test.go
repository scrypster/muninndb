package engine

import (
	"context"
	"os"
	"testing"

	"github.com/scrypster/muninndb/internal/engine/activation"
	"github.com/scrypster/muninndb/internal/engine/trigger"
	"github.com/scrypster/muninndb/internal/index/fts"
	hnswpkg "github.com/scrypster/muninndb/internal/index/hnsw"
	searchadapters "github.com/scrypster/muninndb/internal/search/adapters"
	"github.com/scrypster/muninndb/internal/search/native"
	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

func newSearchBackendEngine(t *testing.T) (*Engine, *storage.PebbleStore, func()) {
	t.Helper()
	dir, err := os.MkdirTemp("", "muninndb-search-backend-engine-*")
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
	embedder := &noopEmbedder{}
	actEngine := activation.New(store, searchadapters.ActivationFTS{B: searchBackend}, searchadapters.ActivationVector{B: searchBackend}, embedder)
	trigSystem := trigger.New(store, searchadapters.TriggerFTS{B: searchBackend}, searchadapters.TriggerVector{B: searchBackend}, embedder)
	eng := NewEngine(EngineConfig{
		Store:            store,
		FTSIndex:         ftsIdx,
		ActivationEngine: actEngine,
		TriggerSystem:    trigSystem,
		Embedder:         embedder,
		HNSWRegistry:     hnswRegistry,
	})
	return eng, store, func() {
		eng.Stop()
		store.Close()
		os.RemoveAll(dir)
	}
}

func TestSearchBackendWiringActivatesThroughNativeBackend(t *testing.T) {
	eng, _, cleanup := newSearchBackendEngine(t)
	defer cleanup()
	ctx := context.Background()

	writeResp, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:   "default",
		Concept: "search backend integration",
		Content: "the native search backend should power activation full text recall",
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	awaitFTS(t, eng)

	activateResp, err := eng.Activate(ctx, &mbp.ActivateRequest{
		Vault:       "default",
		Context:     []string{"native search backend activation"},
		MaxResults:  5,
		DisableHops: true,
		Weights:     &mbp.Weights{FullTextRelevance: 1, DisableACTR: true},
	})
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if activateResp.TotalFound == 0 || len(activateResp.Activations) == 0 {
		t.Fatalf("Activate returned no results: %#v", activateResp)
	}
	if activateResp.Activations[0].ID != writeResp.ID {
		t.Fatalf("top activation ID = %s, want %s", activateResp.Activations[0].ID, writeResp.ID)
	}
}
