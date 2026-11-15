//go:build localassets

package engine

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/cognitive"
	"github.com/scrypster/muninndb/internal/engine/activation"
	"github.com/scrypster/muninndb/internal/index/fts"
	hnswpkg "github.com/scrypster/muninndb/internal/index/hnsw"
	embedpkg "github.com/scrypster/muninndb/internal/plugin/embed"
	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// providerEmbedder adapts the bundled LocalProvider straight to
// activation.Embedder (production goes through EmbedService/BatchEmbedder,
// which pass the context — and so the lane mark — through unchanged), so a
// test can reach the provider's lanes.
type providerEmbedder struct{ p *embedpkg.LocalProvider }

func (a providerEmbedder) Embed(ctx context.Context, texts []string) ([]float32, error) {
	return a.p.EmbedBatch(ctx, texts)
}
func (a providerEmbedder) Tokenize(text string) []string { return strings.Fields(text) }

// laneEnv is realEmbedEnv over a LocalProvider the test holds directly.
func laneEnv(t *testing.T) (*Engine, *embedpkg.LocalProvider) {
	t.Helper()
	dir := t.TempDir()
	p := &embedpkg.LocalProvider{}
	if _, err := p.Init(context.Background(), embedpkg.ProviderHTTPConfig{DataDir: dir}); err != nil {
		t.Skipf("local embed provider init failed (assets missing? run make fetch-assets): %v", err)
	}
	db, err := storage.OpenPebble(dir+"/db", storage.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	store := storage.NewPebbleStore(db, storage.PebbleStoreConfig{CacheSize: 1000})
	ftsIdx := fts.New(db)
	emb := providerEmbedder{p}
	hnswReg := hnswpkg.NewRegistry(db)
	actEngine := activation.New(store, activation.NewFTSAdapter(ftsIdx), activation.NewHNSWAdapter(hnswReg), emb)
	eng := NewEngine(EngineConfig{
		Store: store, FTSIndex: ftsIdx, ActivationEngine: actEngine,
		Embedder: emb, HNSWRegistry: hnswReg, EmbedModelName: "bge-small-en-v1.5",
		ContradictWorker: (*cognitive.Worker[cognitive.ContradictItem])(nil),
	})
	t.Cleanup(func() {
		eng.Stop()
		store.Close()
		_ = p.Close()
		os.RemoveAll(dir)
	})
	return eng, p
}

// Invented fixture domain: a model railway club's layout notes.
var railwayNotes = [][2]string{
	{"turnout motors", "The turnout motors on the branch line are wired through the third relay board under the station."},
	{"track cleaning", "Track cleaning uses an abrasive block on the outer loop before every operating session."},
	{"signal timing", "The semaphore signals on the main line drop to danger two seconds after a train passes the sensor."},
	{"scenery glue", "Scenery foliage is fixed with diluted white glue and left to dry overnight before ballast."},
}

func writeRailwayNotes(t *testing.T, eng *Engine, p *embedpkg.LocalProvider) map[string]string {
	t.Helper()
	ids := map[string]string{}
	for _, n := range railwayNotes {
		vec, err := p.EmbedBatch(context.Background(), []string{n[0] + " " + n[1]})
		if err != nil {
			t.Fatal(err)
		}
		r, err := eng.Write(context.Background(), &mbp.WriteRequest{Vault: "default", Concept: n[0], Content: n[1], Embedding: vec})
		if err != nil {
			t.Fatal(err)
		}
		ids[n[0]] = r.ID
	}
	eng.waitWriteTimeIdle()
	return ids
}

// TestRecall_DegradesToBM25WhenEmbedLaneBusy is the reported defect end to
// end, with the real bundled embedder: recall's query lane is occupied past
// the embed budget. Before the lanes, the query embed waited on the shared
// mutex for the whole batch, then recall failed outright with "activation:
// context deadline exceeded", zero results and semantic_degraded=false. Now
// the embed gives up at its budget and recall answers keyword-only, flagged.
func TestRecall_DegradesToBM25WhenEmbedLaneBusy(t *testing.T) {
	eng, p := laneEnv(t)
	ids := writeRailwayNotes(t, eng, p)

	release := p.HoldLaneForTest(true) // an in-flight query-lane inference that does not finish in time
	defer release()
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	start := time.Now()
	resp, err := eng.Activate(ctx, &mbp.ActivateRequest{Vault: "default", Context: []string{"semaphore signals main line"}, MaxResults: 5})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("recall failed after %s with the query lane busy: %v — want a keyword-only degraded result", elapsed, err)
	}
	if !resp.SemanticDegraded || resp.SemanticDegradedReason != activation.DegradedEmbedTimeout {
		t.Fatalf("semantic_degraded=%v reason=%q, want true/%q", resp.SemanticDegraded, resp.SemanticDegradedReason, activation.DegradedEmbedTimeout)
	}
	if len(resp.Activations) == 0 || resp.Activations[0].ID != ids["signal timing"] {
		t.Fatalf("keyword fallback top result = %v, want the signal-timing note", resp.Activations)
	}
	t.Logf("recall with a busy query lane and a 400ms deadline: degraded keyword-only result in %s", elapsed.Round(time.Millisecond))
}

// TestRecall_ResultsUnchangedUnderConcurrentBatch: when recall is NOT
// degraded, a batch running beside it changes nothing about what it returns —
// same memories, same semantic similarities, bit for bit — because the query
// embed is the same inference on the same session, only scheduled differently.
func TestRecall_ResultsUnchangedUnderConcurrentBatch(t *testing.T) {
	eng, p := laneEnv(t)
	writeRailwayNotes(t, eng, p)
	query := []string{"how are the turnouts wired"}
	recall := func() map[string]float32 {
		t.Helper()
		resp, err := eng.Activate(context.Background(), &mbp.ActivateRequest{Vault: "default", Context: query, MaxResults: 10})
		if err != nil {
			t.Fatal(err)
		}
		if resp.SemanticDegraded {
			t.Fatalf("recall degraded (%s) with no deadline", resp.SemanticDegradedReason)
		}
		out := map[string]float32{}
		for _, a := range resp.Activations {
			out[a.ID] = a.ScoreComponents.SemanticSimilarity
		}
		eng.waitWriteTimeIdle()
		return out
	}
	idle := recall()

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { // a background batch loop, as the retroactive processor runs
		defer wg.Done()
		texts := make([]string, 8)
		for i := range texts {
			texts[i] = fmt.Sprintf("Layout log %d: ballast was laid along the yard throat and the fiddle yard was re-levelled.", i)
		}
		for {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := p.EmbedBatch(context.Background(), texts); err != nil {
				t.Error(err)
				return
			}
		}
	}()
	release := p.HoldLaneForTest(false) // and the batch lane occupied for certain during this recall
	busy := recall()
	release()
	close(stop)
	wg.Wait()

	if len(idle) == 0 || len(idle) != len(busy) {
		t.Fatalf("idle recall returned %d memories, under batch %d", len(idle), len(busy))
	}
	for id, sim := range idle {
		if got, ok := busy[id]; !ok || got != sim {
			t.Fatalf("memory %s: semantic similarity idle=%v under batch=%v (present=%v)", id, sim, got, ok)
		}
	}
}
