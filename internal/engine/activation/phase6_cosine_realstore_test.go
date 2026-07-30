package activation_test

import (
	"context"
	"os"
	"testing"

	"github.com/scrypster/muninndb/internal/engine/activation"
	"github.com/scrypster/muninndb/internal/index/fts"
	"github.com/scrypster/muninndb/internal/storage"
)

// TestPhase6Score_FTSOnlyCandidate_RealStoreLoaderGap is the RED-first repro
// for the REAL #714-A2 bug against the ACTUAL production loader path -- no
// stub. TestPhase6Score_FTSOnlyCandidate_NonZeroCosine (helpers_test.go) and
// its stub-double companion TestPhase6Score_FTSOnlyCandidate_LoaderGap both
// use in-memory stores; this test proves the bug against a real
// *storage.PebbleStore, the same store type production wires into
// activation.New (see muninn.go, cmd/muninn/server.go).
//
// Root cause: an engram written with WriteEngram(ctx, ws, eng) (eng.Embedding
// set) lands as an ERF v2 record -- the 0x01 engram key holds no embedding
// bytes at all; the vector goes to the separate 0x18 key
// (storage.PebbleStore.WriteEngram, keys.EmbeddingKey). storage.PebbleStore.
// GetEngrams (the hot-path bulk loader phase6Score uses) never joins 0x18 --
// it returns Embedding empty for every engram, always. Only
// storage.PebbleStore.GetEmbedding does the 0x18 read.
//
// This engram is made an FTS-only candidate: it is indexed into a real
// fts.Index and no HNSW is wired (nil), so it can only ever enter the
// candidate pool via the FTS path with vectorScore == 0. The query embedding
// is supplied directly via ActivateRequest.Embedding (identical to the
// engram's own embedding => cosine ~= 1.0 after int8 quantization), bypassing
// the need for a real embedder.
//
// Before the fix, needsCosine's fixup reads eng.Embedding straight off the
// GetEngrams result -- empty on a real store -- and never calls
// GetEmbedding(), so SemanticSimilarity stays 0. This FAILS on current code
// (guard dead) and PASSES once phase6Score falls back to GetEmbedding().
func TestPhase6Score_FTSOnlyCandidate_RealStoreLoaderGap(t *testing.T) {
	dir, err := os.MkdirTemp("", "muninndb-fts-cosine-loader-*")
	if err != nil {
		t.Fatal(err)
	}
	db, err := storage.OpenPebble(dir, storage.DefaultOptions())
	if err != nil {
		os.RemoveAll(dir)
		t.Fatal(err)
	}
	store := storage.NewPebbleStore(db, storage.PebbleStoreConfig{CacheSize: 128})
	t.Cleanup(func() {
		// Close the store (which closes the underlying db) rather than the raw
		// db directly, so the counter-flush and other background workers stop
		// before Pebble closes -- otherwise the flush goroutine races the db
		// close and logs a recovered "unexpected panic in counter flush" WARN.
		store.Close()
		os.RemoveAll(dir)
	})
	idx := fts.New(db)

	ws := store.VaultPrefix("fts-cosine-loader-gap")

	// A distinctive query embedding -- what "the query" would have embedded to.
	queryVec := []float32{0.6, 0.8, 0.0, 0.0}

	eng := &storage.Engram{
		Concept:   "WidgetPipeline lifecycle",
		Content:   "WidgetPipeline lifecycle state machine handles pending, cleared, and reversed states.",
		Embedding: queryVec, // identical to the query vector => cosine ~= 1.0
	}
	id, err := store.WriteEngram(context.Background(), ws, eng)
	if err != nil {
		t.Fatalf("WriteEngram: %v", err)
	}
	eng.ID = id

	// Sanity: confirm the production loader split actually holds on this
	// vault BEFORE running the engine, so a future storage change that starts
	// joining embeddings into GetEngrams doesn't make this test meaningless.
	loaded, err := store.GetEngrams(context.Background(), ws, []storage.ULID{id})
	if err != nil {
		t.Fatalf("GetEngrams: %v", err)
	}
	if len(loaded) != 1 || loaded[0] == nil {
		t.Fatalf("GetEngrams: expected exactly one engram back, got %d", len(loaded))
	}
	if len(loaded[0].Embedding) != 0 {
		t.Fatalf("GetEngrams unexpectedly returned a non-empty Embedding (len=%d) -- this test's premise "+
			"(GetEngrams never joins the 0x18 key) no longer holds; re-verify against the current storage layer",
			len(loaded[0].Embedding))
	}
	direct, err := store.GetEmbedding(context.Background(), ws, id)
	if err != nil {
		t.Fatalf("GetEmbedding: %v", err)
	}
	if len(direct) == 0 {
		t.Fatalf("GetEmbedding returned no vector -- the embedding is not retrievable through ANY loader; " +
			"this would be a different, deeper problem than the GetEngrams join gap")
	}

	if err := idx.IndexEngram(ws, [16]byte(id), eng.Concept, "", eng.Content, eng.Tags); err != nil {
		t.Fatalf("IndexEngram: %v", err)
	}

	// No HNSW (nil): the candidate can only ever be FTS-only, vectorScore == 0
	// in the fused pool. No embedder needed: the query embedding is supplied
	// directly via ActivateRequest.Embedding.
	eng2 := activation.New(store, activation.NewFTSAdapter(idx), nil, activation.NewNoopEmbedder())
	defer eng2.Close()

	result, err := eng2.Run(context.Background(), &activation.ActivateRequest{
		VaultPrefix: ws,
		Context:     []string{"WidgetPipeline lifecycle state machine"},
		Embedding:   queryVec,
		Threshold:   0.01,
		MaxResults:  10,
		IncludeWhy:  true,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	var found bool
	for _, a := range result.Activations {
		if a.Engram.ID == id {
			found = true
			if a.Components.SemanticSimilarity <= 0 {
				t.Errorf("FTS-only candidate (real PebbleStore) SemanticSimilarity = %v, want > 0 -- "+
					"GetEngrams returned an empty Embedding for this ERF v2 record and phase6Score's "+
					"fixup must fall back to GetEmbedding() to recover it",
					a.Components.SemanticSimilarity)
			}
		}
	}
	if !found {
		t.Fatalf("FTS-only candidate did not survive to the final result set; full result count=%d",
			len(result.Activations))
	}
}
