package grpc_test

import (
	"context"
	"os"
	"testing"

	"github.com/scrypster/muninndb/internal/engine"
	"github.com/scrypster/muninndb/internal/engine/activation"
	"github.com/scrypster/muninndb/internal/index/fts"
	hnswpkg "github.com/scrypster/muninndb/internal/index/hnsw"
	"github.com/scrypster/muninndb/internal/storage"
	transportgrpc "github.com/scrypster/muninndb/internal/transport/grpc"
	"github.com/scrypster/muninndb/internal/transport/mbp"
	pb "github.com/scrypster/muninndb/proto/gen/go/muninn/v1"
)

// zeroVecEmbedder returns an all-zero vector: the engine treats that as an
// untrustworthy embed and degrades the recall to keyword-only.
type zeroVecEmbedder struct{}

func (zeroVecEmbedder) Embed(_ context.Context, _ []string) ([]float32, error) {
	return make([]float32, 8), nil
}
func (zeroVecEmbedder) Tokenize(text string) []string { return []string{text} }

// TestGRPCActivate_CarriesSemanticDegraded: a keyword-only recall must say
// so on gRPC too. Before this, the adapter dropped SemanticDegraded, so gRPC
// callers received BM25-only rows with no marker at all — and once the embed
// lanes turned "deadline behind a batch" from a hard error into a degraded
// success, that silence became the common outcome on this transport.
func TestGRPCActivate_CarriesSemanticDegraded(t *testing.T) {
	dir, err := os.MkdirTemp("", "muninndb-grpc-degraded-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	db, err := storage.OpenPebble(dir, storage.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	store := storage.NewPebbleStore(db, storage.PebbleStoreConfig{CacheSize: 128})
	defer store.Close()
	ftsIdx := fts.New(db)
	emb := zeroVecEmbedder{}
	actEngine := activation.New(store, activation.NewFTSAdapter(ftsIdx), activation.NewHNSWAdapter(hnswpkg.NewRegistry(db)), emb)
	eng := engine.NewEngine(engine.EngineConfig{Store: store, FTSIndex: ftsIdx, ActivationEngine: actEngine, Embedder: emb})
	defer eng.Stop()
	ctx := context.Background()
	if _, err := eng.Write(ctx, &mbp.WriteRequest{Vault: "default", Concept: "hive", Content: "the beekeeper added a honey super to the second hive"}); err != nil {
		t.Fatal(err)
	}
	eng.WaitWriteTimeIdle()

	resp, err := transportgrpc.TestableNewEngineAdapter(eng).Activate(ctx, &pb.ActivateRequest{Vault: "default", Context: []string{"honey super hive"}, MaxResults: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Activations) == 0 {
		t.Fatal("keyword fallback returned nothing; the fixture should match by BM25")
	}
	if !resp.SemanticDegraded || resp.SemanticDegradedReason != activation.DegradedEmbedZeroVector {
		t.Fatalf("gRPC ActivateResponse semantic_degraded=%v reason=%q, want true/%q — keyword-only rows served unmarked",
			resp.SemanticDegraded, resp.SemanticDegradedReason, activation.DegradedEmbedZeroVector)
	}
}
