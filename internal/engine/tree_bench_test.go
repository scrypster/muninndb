package engine

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/scrypster/muninndb/internal/engine/activation"
	"github.com/scrypster/muninndb/internal/engine/trigger"
	"github.com/scrypster/muninndb/internal/index/fts"
	"github.com/scrypster/muninndb/internal/storage"
)

// testEnvTB creates a test engine environment that accepts testing.TB so it can
// be used from both *testing.T and *testing.B. The underlying testEnv only accepts
// *testing.T, so we replicate the setup logic here.
func testEnvTB(tb testing.TB) (*Engine, func()) {
	tb.Helper()
	dir, err := os.MkdirTemp("", "muninndb-engine-bench-*")
	if err != nil {
		tb.Fatal(err)
	}

	db, err := storage.OpenPebble(dir, storage.DefaultOptions())
	if err != nil {
		os.RemoveAll(dir)
		tb.Fatal(err)
	}

	store := storage.NewPebbleStore(db, storage.PebbleStoreConfig{CacheSize: 1000})
	ftsIdx := fts.New(db)

	embedder := &noopEmbedder{}
	actEngine := activation.New(store, &ftsAdapter{ftsIdx}, nil, embedder)
	trigSystem := trigger.New(store, &ftsTrigAdapter{ftsIdx}, nil, embedder)
	eng := NewEngine(EngineConfig{Store: store, FTSIndex: ftsIdx, ActivationEngine: actEngine, TriggerSystem: trigSystem, Embedder: embedder})

	return eng, func() {
		eng.Stop()
		store.Close()
		os.RemoveAll(dir)
	}
}

// BenchmarkRememberTree_10Nodes benchmarks writing a tree with 1 root + 9 children.
func BenchmarkRememberTree_10Nodes(b *testing.B) {
	eng, cleanup := testEnvTB(b)
	defer cleanup()
	ctx := context.Background()

	children := make([]TreeNodeInput, 9)
	for i := 0; i < 9; i++ {
		children[i] = TreeNodeInput{
			Concept: fmt.Sprintf("Child%d", i+1),
			Content: fmt.Sprintf("child content %d", i+1),
		}
	}

	req := &RememberTreeRequest{
		Vault: "bench-10",
		Root: TreeNodeInput{
			Concept:  "BenchRoot",
			Content:  "benchmark root",
			Children: children,
		},
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = eng.RememberTree(ctx, req)
	}
}

// BenchmarkRecallTree_50Nodes benchmarks recalling a tree with ~51 nodes
// (root → 5 phases → 9 tasks each = 1 + 5 + 45 = 51 nodes).
func BenchmarkRecallTree_50Nodes(b *testing.B) {
	eng, cleanup := testEnvTB(b)
	defer cleanup()
	ctx := context.Background()
	vault := "bench-50"

	const numPhases = 5
	const tasksPerPhase = 9

	phases := make([]TreeNodeInput, numPhases)
	for p := 0; p < numPhases; p++ {
		tasks := make([]TreeNodeInput, tasksPerPhase)
		for tt := 0; tt < tasksPerPhase; tt++ {
			tasks[tt] = TreeNodeInput{
				Concept: fmt.Sprintf("Phase%d_Task%d", p+1, tt+1),
				Content: fmt.Sprintf("task %d of phase %d", tt+1, p+1),
			}
		}
		phases[p] = TreeNodeInput{
			Concept:  fmt.Sprintf("Phase%d", p+1),
			Content:  fmt.Sprintf("phase %d", p+1),
			Children: tasks,
		}
	}

	// Write the tree once before the timer starts.
	result, err := eng.RememberTree(ctx, &RememberTreeRequest{
		Vault: vault,
		Root: TreeNodeInput{
			Concept:  "BenchRecallRoot",
			Content:  "root for recall benchmark",
			Children: phases,
		},
	})
	if err != nil {
		b.Fatalf("RememberTree setup: %v", err)
	}
	rootID := result.RootID

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = eng.RecallTree(ctx, vault, rootID, 0, 0, true)
	}
}
