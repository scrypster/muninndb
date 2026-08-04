package engine

import (
	"context"
	"os"
	"sync/atomic"
	"testing"

	"github.com/scrypster/muninndb/internal/engine/activation"
	"github.com/scrypster/muninndb/internal/index/fts"
	"github.com/scrypster/muninndb/internal/index/hnsw"
	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// ---------------------------------------------------------------------------
// F7 (712-currency fix round): similarExisting's self-query called
// Activate(), which re-embedded the just-written text even when the write
// itself already carried an embedding — a full duplicate embedder inference
// on every successful remember, measured at ~100ms p95 against the v2 §6
// pre-registered ≤100ms bar. The fix threads the write's own
// eng.Embedding into the self-query's ActivateRequest.Embedding field,
// which activation's phase1 already treats as precomputed and skips
// re-embedding for.
//
// This is a call-count oracle, not a wall-clock assertion, per the
// coordinator's note: a counting embedder proves the self-query issues
// ZERO additional Embed() calls when the write supplied its own vector.
// ---------------------------------------------------------------------------

// countingEmbedder wraps a fixed-dimension zero-cost embed with a call
// counter, so a test can assert how MANY times the pipeline actually
// invoked it — not just that a result came back.
type countingEmbedder struct {
	calls atomic.Int64
	dim   int
}

func (c *countingEmbedder) Embed(_ context.Context, texts []string) ([]float32, error) {
	c.calls.Add(1)
	return make([]float32, c.dim*len(texts)), nil
}
func (c *countingEmbedder) Tokenize(text string) []string { return []string{text} }

// envWithRealHNSWAndCountingEmbedder wires a REAL hnsw.Registry for the
// write path's inline Insert (engine.go's `e.hnswRegistry.Insert`, taken
// whenever a write carries a client embedding) alongside a countingEmbedder
// as activation's HNSWIndex is satisfied by stubHNSW — phase1 only enters
// its embed branch when e.hnsw is non-nil (testEnvWithHNSW's own doc
// comment), and stubHNSW is the package's existing non-nil, do-nothing
// double for exactly that gate.
func envWithRealHNSWAndCountingEmbedder(t *testing.T, embedder activation.Embedder) (*Engine, func()) {
	t.Helper()
	dir, err := os.MkdirTemp("", "muninndb-similar-existing-embed-reuse-*")
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
	hnswReg := hnsw.NewRegistry(db)
	actEngine := activation.New(store, &ftsAdapter{ftsIdx}, stubHNSW{}, embedder)
	eng := NewEngine(EngineConfig{
		Store: store, FTSIndex: ftsIdx, HNSWRegistry: hnswReg,
		ActivationEngine: actEngine, Embedder: embedder,
	})
	return eng, func() {
		eng.Stop()
		store.Close()
		os.RemoveAll(dir)
	}
}

func TestSimilarExisting_F7_ReusesWriteEmbedding_ZeroExtraEmbedCalls(t *testing.T) {
	embedder := &countingEmbedder{dim: 8}
	eng, cleanup := envWithRealHNSWAndCountingEmbedder(t, embedder)
	defer cleanup()
	ctx := context.Background()
	const vault = "similar-existing-embed-reuse-probe"

	// A pre-existing candidate for the self-query to find. Its own write
	// also carries a client-supplied embedding, so this Write call itself
	// counts as one Embed() invocation exactly like the probe below — the
	// counter is read only around the probe write, so this one is outside
	// the measurement window.
	fixedVec := make([]float32, 8)
	if _, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault: vault, Concept: "onboarding checklist",
		Content:   "The onboarding checklist covers badge access, laptop setup, and the benefits portal.",
		Embedding: fixedVec,
	}); err != nil {
		t.Fatalf("write candidate: %v", err)
	}
	eng.WaitWriteTimeIdle()

	before := embedder.calls.Load()

	// The probe write ALSO carries a client-supplied embedding, so
	// eng.Embedding is populated by the time similarExisting runs — the
	// exact condition F7's reuse seam targets. Zero embedder calls are
	// expected across this entire Write(): none for the write path itself
	// (client supplied the vector, so nothing to compute) and none for the
	// self-query (reuses the same vector).
	if _, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault: vault, Concept: "onboarding checklist",
		Content:   "The onboarding checklist covers badge access, laptop setup, and the benefits portal, revised.",
		Embedding: fixedVec,
	}); err != nil {
		t.Fatalf("write probe: %v", err)
	}
	eng.WaitWriteTimeIdle()

	after := embedder.calls.Load()
	if after != before {
		t.Fatalf("F7 violated: similar_existing re-embedded text the write already carried an "+
			"embedding for — embedder.Embed call count %d -> %d, want no change", before, after)
	}
}
