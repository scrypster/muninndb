package grpc

import (
	"context"
	"testing"

	"github.com/scrypster/muninndb/internal/engine"
	"github.com/scrypster/muninndb/internal/engine/activation"
	"github.com/scrypster/muninndb/internal/index/fts"
	"github.com/scrypster/muninndb/internal/metrics/latency"
	"github.com/scrypster/muninndb/internal/storage"
	pb "github.com/scrypster/muninndb/proto/gen/go/muninn/v1"
)

// noopEmbedderForTest returns a fixed zero vector — no ML model required.
// Mirrors internal/engine's own noopEmbedder test double (unexported there,
// so duplicated rather than imported across package boundaries).
type noopEmbedderForTest struct{}

func (noopEmbedderForTest) Embed(_ context.Context, texts []string) ([]float32, error) {
	return make([]float32, 384), nil
}
func (noopEmbedderForTest) Tokenize(text string) []string {
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

// ftsAdapterForTest converts fts.ScoredID to activation.ScoredID.
type ftsAdapterForTest struct{ idx *fts.Index }

func (a *ftsAdapterForTest) Search(ctx context.Context, ws [8]byte, query string, topK int) ([]activation.ScoredID, error) {
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

// TestAdapter_Write_SkipsSimilarExistingSelfQuery is the gRPC half of F2
// (712-currency fix round): pb.WriteResponse carries neither
// SimilarExisting nor SimilarExistingBasis (design record §4.5, MCP + MBP
// only), so a gRPC single Write must not pay COG-34's self-query Activate()
// call — there is nowhere on the wire for the result to go. Asserted via
// the latency instrumentation (a recorded "similar_existing" sample means
// Activate() actually ran for the hook), not absence of a field alone.
func TestAdapter_Write_SkipsSimilarExistingSelfQuery(t *testing.T) {
	dir := t.TempDir()
	db, err := storage.OpenPebble(dir, storage.DefaultOptions())
	if err != nil {
		t.Fatalf("open pebble: %v", err)
	}
	store := storage.NewPebbleStore(db, storage.PebbleStoreConfig{CacheSize: 1000})
	defer store.Close()

	ftsIdx := fts.New(db)
	embedder := noopEmbedderForTest{}
	actEngine := activation.New(store, &ftsAdapterForTest{ftsIdx}, nil, embedder)
	eng := engine.NewEngine(engine.EngineConfig{Store: store, FTSIndex: ftsIdx, ActivationEngine: actEngine, Embedder: embedder})
	defer eng.Stop()

	tracker := latency.New()
	eng.SetLatencyTracker(tracker)

	adapter := NewEngineAdapter(eng)
	ctx := context.Background()

	// A pre-existing candidate the self-query would otherwise band `strong`
	// against the second write (identical distinctive vocabulary).
	if _, err := adapter.Write(ctx, &pb.WriteRequest{
		Content: "the quarterly compliance filing lists every jurisdiction and deadline",
	}); err != nil {
		t.Fatalf("first adapter.Write: %v", err)
	}
	eng.WaitWriteTimeIdle()

	ws := store.ResolveVaultPrefix("")
	before := tracker.For(ws, "similar_existing").Count

	if _, err := adapter.Write(ctx, &pb.WriteRequest{
		Content: "the quarterly compliance filing lists every jurisdiction and deadline, revised",
	}); err != nil {
		t.Fatalf("second adapter.Write: %v", err)
	}
	eng.WaitWriteTimeIdle()

	after := tracker.For(ws, "similar_existing").Count
	if after != before {
		t.Fatalf("F2 violated: a gRPC Write paid the similar_existing self-query "+
			"(similar_existing samples %d -> %d) — the gRPC adapter must skip the "+
			"COG-34 hook entirely, pb.WriteResponse has no field for it", before, after)
	}
}
