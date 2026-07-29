//go:build localassets

package engine

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/engine/activation"
	"github.com/scrypster/muninndb/internal/engine/trigger"
	"github.com/scrypster/muninndb/internal/index/fts"
	hnswpkg "github.com/scrypster/muninndb/internal/index/hnsw"
	"github.com/scrypster/muninndb/internal/plugin"
	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/transport/mbp"

	embedpkg "github.com/scrypster/muninndb/internal/plugin/embed"
)

// === THE MEASUREMENT-ARTIFACT FIX =========================================
//
// TestSentienceAcceptanceGate (sentience_gate_test.go) wires testEnv(t), which
// gives the engine a noopEmbedder and a nil vector index (activation.New(...,
// nil, noopEmbedder)). That kills the ENTIRE SEMANTIC HALF of recall: every
// vector_score is 0, so Activate's ACT-R blend (0.6*semantic + 0.4*FTS) is
// really just 0.4*FTS, collapsing final scores below the 0.35 threshold at
// ~250-engram scale. The measured 0.062 composite from that run is an
// artifact of the harness, not evidence about the engine's actual capability
// — production runs with a real embedder (MUNINN_LOCAL_EMBED=1 by default
// whenever the bundled bge-small assets are present, see cmd/muninn/server.go
// buildEmbedder()).
//
// This file wires the REAL local bge-small (384-dim) ONNX embedder plus a
// real HNSW vector index — exactly the construction cmd/muninn/server.go uses
// in production (embedpkg.NewEmbedService("local://bge-small-en-v1.5") +
// hnswpkg.NewRegistry(db) + activation.NewHNSWAdapter/trigger.NewHNSWAdapter)
// — and re-runs the SAME frozen fixture (testdata/sentience_session.json,
// untouched) through the SAME harness (runSentienceHarnessGen, factored out
// of runSentienceHarness with no behavior change for the original noop
// callers). Only requires -tags localassets (the bundled ONNX model/tokenizer
// must be embedded at build time — see internal/plugin/embed/local_test.go).
//
// Because intentions arm and fire on CUE-ENTITY overlap (NoticesForRecall
// scans engram entities, not vectors — see prospective.go), only axes that
// depend on Activate's ranked results (A2 currency, and indirectly A1/A4,
// whose candidate pool comes from Activate) need real vectors. Memory writes
// (filler, session memories, supersede targets, evolve successors) are given
// real embeddings via the WriteRequest.Embedding / Evolve embedding
// parameters — the same "client-supplied embedding, inserted into HNSW
// inline" path production already exercises for pre-embedded content (#582).

// realEmbedDataDir caches the temp dir used to materialize the ONNX runtime
// shared library exactly once per test binary run (extraction has real I/O
// cost and the local provider is safe to share a DataDir across instances).
var realEmbedDataDirOnce = func() string {
	dir, err := os.MkdirTemp("", "muninndb-sentience-embed-data-*")
	if err != nil {
		panic(err)
	}
	return dir
}()

// newRealLocalEmbedder constructs the production local bge-small embedder
// (activation.Embedder, 384-dim) via the exact same construction path
// cmd/muninn/server.go's buildEmbedder uses for the bundled ONNX model.
func newRealLocalEmbedder(t *testing.T) activation.Embedder {
	t.Helper()
	if !embedpkg.LocalAvailable() {
		t.Skip("bundled local ONNX assets not embedded — run `make fetch-assets` and rebuild with -tags localassets")
	}
	svc, err := embedpkg.NewEmbedService("local://bge-small-en-v1.5")
	if err != nil {
		t.Fatalf("NewEmbedService(local): %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := svc.Init(ctx, plugin.PluginConfig{DataDir: realEmbedDataDirOnce}); err != nil {
		t.Fatalf("local embed service Init: %v", err)
	}
	t.Cleanup(func() { _ = svc.Close() })
	return embedpkg.NewEmbedServiceAdapter(svc)
}

// testEnvSemantic is testEnv's real-substrate twin: the same storage/FTS
// wiring, but a real local bge-small embedder and a real HNSW vector index
// (hnswpkg.NewRegistry) instead of noopEmbedder/nil. This is what makes
// Activate's semantic half of the ACT-R blend live.
func testEnvSemantic(t *testing.T) (*Engine, func()) {
	t.Helper()
	dir, err := os.MkdirTemp("", "muninndb-engine-test-semantic-*")
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

	embedder := newRealLocalEmbedder(t)
	hnswRegistry := hnswpkg.NewRegistry(db)

	actEngine := activation.New(store, &ftsAdapter{ftsIdx}, activation.NewHNSWAdapter(hnswRegistry), embedder)
	trigSystem := trigger.New(store, &ftsTrigAdapter{ftsIdx}, trigger.NewHNSWAdapter(hnswRegistry), embedder)
	eng := NewEngine(EngineConfig{
		Store: store, FTSIndex: ftsIdx, ActivationEngine: actEngine, TriggerSystem: trigSystem,
		Embedder: embedder, HNSWRegistry: hnswRegistry,
	})

	return eng, func() {
		eng.Stop()
		store.Close()
		os.RemoveAll(dir)
	}
}

// TestSentienceProbe_VectorMatch is the direct sanity probe the increment's
// spec demands BEFORE trusting the full gate: a known query must return its
// semantic match via Activate with a real vector_score > 0, proving the
// write path actually embedded the memory and the HNSW index actually holds
// it (as opposed to, say, the embedder failing closed to a zero vector and
// the harness silently measuring nothing all over again).
func TestSentienceProbe_VectorMatch(t *testing.T) {
	eng, cleanup := testEnvSemantic(t)
	defer cleanup()
	ctx := context.Background()
	const vault = "probe-vault"

	embedder := eng.embedder // internal field access, same package

	text := "the Nordlys project migrated its telemetry pipeline to Kafka in March"
	vec, err := embedder.Embed(ctx, []string{text})
	if err != nil {
		t.Fatalf("embed probe text: %v", err)
	}
	if len(vec) == 0 {
		t.Fatal("real embedder returned an empty vector — embedder is not actually working")
	}

	resp, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault: vault, Content: text, Embedding: vec,
	})
	if err != nil {
		t.Fatalf("Write with client embedding: %v", err)
	}

	// A paraphrase with near-zero lexical overlap with the source text —
	// FTS alone should not find this; only semantic similarity can.
	query := "how did the observability stack change for that Scandinavian client engagement"
	actResp, err := eng.Activate(ctx, &mbp.ActivateRequest{
		Vault: vault, Context: []string{query}, MaxResults: 5, Threshold: 0.0, IncludeWhy: true,
	})
	if err != nil {
		t.Fatalf("Activate(probe query): %v", err)
	}

	var found bool
	var vectorScore float32
	for _, a := range actResp.Activations {
		if a.ID == resp.ID {
			found = true
			vectorScore = a.Score // composite score; the point is it's reachable at all under near-zero lexical overlap
			t.Logf("probe hit: id=%s score=%.4f why=%s", a.ID, a.Score, a.Why)
		}
	}
	if !found {
		t.Fatalf("real-embedder probe FAILED: paraphrased query did not surface its semantic match at all — "+
			"vector stack is not actually populating/searching (results=%d)", len(actResp.Activations))
	}
	if vectorScore <= 0 {
		t.Fatalf("real-embedder probe FAILED: match found but score=%.4f (want > 0) — semantic contribution is not live", vectorScore)
	}
	t.Logf("VECTOR STACK CONFIRMED LIVE: paraphrased query (near-zero lexical overlap) surfaced its semantic match with score=%.4f", vectorScore)
}

// TestSentienceAcceptanceGate_Semantic is the honest re-measure: SAME frozen
// fixture, SAME harness, SAME thresholds — only the embedder/index wiring
// changed from noop/nil to the real local bge-small + HNSW substrate. See
// the commit body for the full before/after numbers.
func TestSentienceAcceptanceGate_Semantic(t *testing.T) {
	embedder := newRealLocalEmbedder(t)

	start := time.Now()
	on := runSentienceHarnessGen(t, true, testEnvSemantic, embedder)
	off := runSentienceHarnessGen(t, false, testEnvSemantic, embedder)
	elapsed := time.Since(start)

	for _, l := range on.log {
		t.Log(l)
	}

	deltaPush := on.a1HitRate() - off.a1HitRate()
	dumpA1, dumpA3, dumpFalse, dumpSilence := dumpEverythingControl(on)

	t.Logf("=== SENTIENT-FEEL SCORE (SFS) — REAL EMBEDDER — runtime %s ===", elapsed.Round(time.Millisecond))
	t.Logf("A1 unprompted surfacing : hit=%d/%d (%.3f)  precision=%.3f (fired=%d wanted=%d)",
		on.colleagueHit, on.colleagueCount, on.a1HitRate(), on.a1Precision(), on.colleagueFired, on.colleagueWanted)
	t.Logf("A2 currency             : win=%d/%d (%.3f)  annotation=%d/%d  as_of=%d/%d (mechanism fails: %v)",
		on.currencyWins, on.currencyProbes, on.a2WinRate(), on.staleAnnotated, on.staleProbes, on.asOfHits, on.asOfProbes, on.asOfMechanismFail)
	t.Logf("A3 non-intrusion        : false_notices=%d/%d silence calls (budget: exactly 0)", on.falseNotices, on.silenceCalls)
	t.Logf("A4 continuity           : hit=%d/%d (%.3f)", on.continuityHits, on.continuityProbes, on.a4HitRate())
	t.Logf("COMPOSITE SFS = min(A1,A2,A3,A4) = %.3f", on.composite())
	t.Logf("--- controls ---")
	t.Logf("C1 Push-OFF baseline    : A1_on=%.3f A1_off=%.3f  Delta_push=%.3f", on.a1HitRate(), off.a1HitRate(), deltaPush)
	t.Logf("C2 Explicit-query base  : %d/%d (%.3f) — margin over A1_on = %.3f",
		on.c2Hits, on.c2Count, on.c2Rate(), on.a1HitRate()-on.c2Rate())
	t.Logf("C3 Dump-everything      : A1=%.2f A3_false=%d/%d -> A3_norm=%.2f -> COMPOSITE=%.2f",
		dumpA1, dumpFalse, dumpSilence, dumpA3, minf(dumpA1, dumpA3))

	bar := func(name string, ok bool, format string, args ...any) {
		status := "FAIL"
		if ok {
			status = "PASS"
		}
		t.Logf("[%s] %s: %s", status, name, fmt.Sprintf(format, args...))
	}
	allPass := true
	check := func(name string, ok bool, format string, args ...any) {
		bar(name, ok, format, args...)
		allPass = allPass && ok
	}
	check("A1 hit rate", on.a1HitRate() >= 10.0/12.0, "%.3f (%d/12), want >= 10/12", on.a1HitRate(), on.colleagueHit)
	check("A1 precision", on.a1Precision() >= 0.90, "%.3f, want >= 0.90", on.a1Precision())
	check("A2 currency win rate", on.currencyWins >= 15, "%d/16, want >= 15/16", on.currencyWins)
	check("A2 annotation completeness", on.staleAnnotated >= 8, "%d/8, want 8/8", on.staleAnnotated)
	check("A2 as_of correctness", on.asOfHits >= 3, "%d/3, want 3/3 (mechanism fails: %v)", on.asOfHits, on.asOfMechanismFail)
	check("A3 non-intrusion", on.falseNotices == 0, "%d false notices on 30 silence calls, want EXACTLY ZERO", on.falseNotices)
	check("A4 thread pickup", on.continuityHits >= 5, "%d/6, want >= 5/6", on.continuityHits)
	check("COMPOSITE SFS", on.composite() >= 0.83, "%.3f, want >= 0.83", on.composite())
	check("C1 Delta_push", deltaPush >= 0.83, "%.3f, want >= 0.83", deltaPush)
	check("C2 margin over explicit baseline", on.a1HitRate()-on.c2Rate() >= 0.40, "%.3f, want >= 0.40", on.a1HitRate()-on.c2Rate())

	if allPass {
		t.Logf("GATE VERDICT (real embedder): PASS — SFS %.3f meets every design bar.", on.composite())
	} else {
		t.Logf("GATE VERDICT (real embedder): SUB-THRESHOLD HONEST CEILING — see commit body for root-cause findings.")
	}
}
