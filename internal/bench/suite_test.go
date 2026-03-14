package bench_test

import (
	"context"
	"os"
	"testing"

	"github.com/scrypster/muninndb/internal/auth"
	"github.com/scrypster/muninndb/internal/engine"
	"github.com/scrypster/muninndb/internal/engine/activation"
	"github.com/scrypster/muninndb/internal/engine/trigger"
	"github.com/scrypster/muninndb/internal/index/fts"
	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// noopEmbedder returns a zero vector. No ML model required in benchmarks.
type noopEmbedder struct{}

func (e *noopEmbedder) Embed(_ context.Context, texts []string) ([]float32, error) {
	return make([]float32, 384), nil
}

func (e *noopEmbedder) Tokenize(text string) []string {
	var tokens []string
	word := ""
	for _, r := range text {
		if r == ' ' || r == '\t' {
			if word != "" {
				tokens = append(tokens, word)
				word = ""
			}
		} else {
			word += string(r)
		}
	}
	if word != "" {
		tokens = append(tokens, word)
	}
	return tokens
}

// ftsAdapter wraps fts.Index for activation.Engine.
type ftsAdapter struct{ idx *fts.Index }

func (a *ftsAdapter) Search(ctx context.Context, ws [8]byte, query string, topK int) ([]activation.ScoredID, error) {
	results, err := a.idx.Search(ctx, ws, query, topK)
	if err != nil {
		return nil, err
	}
	out := make([]activation.ScoredID, len(results))
	for i, r := range results {
		out[i] = activation.ScoredID{ID: storage.ULID(r.ID), Score: r.Score}
	}
	return out, nil
}

// ftsTrigAdapter wraps fts.Index for trigger.System.
type ftsTrigAdapter struct{ idx *fts.Index }

func (a *ftsTrigAdapter) Search(ctx context.Context, ws [8]byte, query string, topK int) ([]trigger.ScoredID, error) {
	results, err := a.idx.Search(ctx, ws, query, topK)
	if err != nil {
		return nil, err
	}
	out := make([]trigger.ScoredID, len(results))
	for i, r := range results {
		out[i] = trigger.ScoredID{ID: storage.ULID(r.ID), Score: r.Score}
	}
	return out, nil
}

// benchEnv creates a fully functional Engine for benchmarks.
// Returns the engine and a teardown function.
func benchEnv(b *testing.B) (*engine.Engine, func()) {
	b.Helper()
	dir, err := os.MkdirTemp("", "muninndb-bench-*")
	if err != nil {
		b.Fatal(err)
	}

	db, err := storage.OpenPebble(dir, storage.DefaultOptions())
	if err != nil {
		os.RemoveAll(dir)
		b.Fatal(err)
	}

	store := storage.NewPebbleStore(db, storage.PebbleStoreConfig{CacheSize: 1000})
	ftsIdx := fts.New(db)
	embedder := &noopEmbedder{}
	actEngine := activation.New(store, &ftsAdapter{ftsIdx}, nil, embedder)
	trigSystem := trigger.New(store, &ftsTrigAdapter{ftsIdx}, nil, embedder)
	as := auth.NewStore(db)
	eng := engine.NewEngine(engine.EngineConfig{Store: store, AuthStore: as, FTSIndex: ftsIdx, ActivationEngine: actEngine, TriggerSystem: trigSystem, Embedder: embedder})

	return eng, func() {
		eng.Stop()
		store.Close()
		os.RemoveAll(dir)
	}
}

// BenchmarkE2EWrite measures single-engram write throughput end-to-end.
func BenchmarkE2EWrite(b *testing.B) {
	eng, cleanup := benchEnv(b)
	defer cleanup()

	ctx := context.Background()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		_, _ = eng.Write(ctx, &mbp.WriteRequest{
			Vault:   "bench",
			Concept: "test",
			Content: "benchmark content",
		})
	}
}

// BenchmarkE2EActivate measures activation (memory retrieval) throughput.
func BenchmarkE2EActivate(b *testing.B) {
	eng, cleanup := benchEnv(b)
	defer cleanup()

	ctx := context.Background()

	// Seed the vault with engrams before benchmarking.
	for i := 0; i < 10; i++ {
		_, _ = eng.Write(ctx, &mbp.WriteRequest{
			Vault:   "bench",
			Concept: "test",
			Content: "benchmark content for activation",
		})
	}

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		_, _ = eng.Activate(ctx, &mbp.ActivateRequest{
			Vault:   "bench",
			Context: []string{"test"},
		})
	}
}

// BenchmarkE2EFTS measures full-text search throughput via Activate.
func BenchmarkE2EFTS(b *testing.B) {
	eng, cleanup := benchEnv(b)
	defer cleanup()

	ctx := context.Background()

	// Index several engrams before benchmarking.
	for i := 0; i < 10; i++ {
		_, _ = eng.Write(ctx, &mbp.WriteRequest{
			Vault:   "bench",
			Concept: "benchmark",
			Content: "benchmark content for full-text search indexing",
		})
	}

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		_, _ = eng.Activate(ctx, &mbp.ActivateRequest{
			Vault:   "bench",
			Context: []string{"benchmark"},
		})
	}
}
