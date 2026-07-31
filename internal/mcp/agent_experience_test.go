package mcp

import (
	"context"
	"os"
	"testing"

	"github.com/scrypster/muninndb/internal/engine"
	"github.com/scrypster/muninndb/internal/engine/activation"
	"github.com/scrypster/muninndb/internal/engine/trigger"
	"github.com/scrypster/muninndb/internal/index/fts"
	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// newRealEngineAdapter wires a real *engine.Engine (live PebbleStore + FTS)
// behind the MCP adapter using only exported constructors — the engine
// package's test helpers are internal, so the shape is duplicated here (same
// as internal/transport/grpc/engine_adapter_confidence_test.go). The stub
// engine used by the handler tests cannot catch adapter-level field-mapping
// bugs, which is exactly the class these tests cover.
func newRealEngineAdapter(t *testing.T) (*engine.Engine, EngineInterface, func()) {
	t.Helper()
	dir, err := os.MkdirTemp("", "muninndb-mcp-agentexp-*")
	if err != nil {
		t.Fatal(err)
	}
	db, err := storage.OpenPebble(dir, storage.DefaultOptions())
	if err != nil {
		os.RemoveAll(dir)
		t.Fatal(err)
	}
	store := storage.NewPebbleStore(db, storage.PebbleStoreConfig{CacheSize: 128})
	ftsIdx := fts.New(db)
	embedder := activation.NewNoopEmbedder()
	actEngine := activation.New(store, activation.NewFTSAdapter(ftsIdx), nil, embedder)
	trigSystem := trigger.New(store, trigger.NewFTSAdapter(ftsIdx), nil, embedder)
	eng := engine.NewEngine(engine.EngineConfig{
		Store: store, FTSIndex: ftsIdx, ActivationEngine: actEngine,
		TriggerSystem: trigSystem, Embedder: embedder,
	})
	return eng, NewEngineAdapter(eng, nil, nil), func() {
		eng.Stop()
		store.Close()
		os.RemoveAll(dir)
	}
}

// ── muninn_explain: real numbers, or an explicit "unknown" ───────────────────

// TestExplainAdapter_ReportsStoredConfidence: the MCP surface reported
// components.confidence == 0 for every engram, including one whose stored
// confidence was 1.0, because the adapter never mapped the field (and
// mbp.ScoreComponents has no confidence at all). 0 is an impossible confidence
// for a live engram, so the number was not merely stale — it was invented.
//
// RED without the fix: Confidence is a plain float64 that nothing sets → 0.
func TestExplainAdapter_ReportsStoredConfidence(t *testing.T) {
	eng, adapter, cleanup := newRealEngineAdapter(t)
	defer cleanup()
	ctx := context.Background()

	w, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:      "explain",
		Concept:    "Redis eviction policy",
		Content:    "allkeys-lru with a 2GB cap",
		Confidence: 1.0,
	})
	if err != nil {
		t.Fatalf("seed write: %v", err)
	}

	res, err := adapter.Explain(ctx, "explain", &ExplainRequest{
		EngramID: w.ID,
		Query:    []string{"Redis", "eviction", "policy"},
	})
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if !res.Found {
		t.Fatal("found = false for an engram that exists")
	}
	if res.Components.Confidence == nil {
		t.Fatal("components.confidence is null for an engram that exists — it is always knowable")
	}
	if *res.Components.Confidence != 1.0 {
		t.Errorf("components.confidence = %v, want 1.0 (the stored value)", *res.Components.Confidence)
	}
	if res.Concept == "" {
		t.Error("concept is empty in the explain response")
	}
	if res.Threshold == 0 {
		t.Error("threshold = 0: explain must report the real recall bar, not a hardcoded zero")
	}
}

// TestExplainAdapter_UnscoredComponentsAreNullNotZero: when the query never
// scored the engram, the query-dependent components do not exist. Serializing
// them as 0 tells the operator "your semantic similarity is zero" when the
// truth is "nothing measured it" — the silent-substitution class. They must be
// JSON null, with a note saying why.
//
// RED without the fix: every component serializes as 0 and there is no note.
func TestExplainAdapter_UnscoredComponentsAreNullNotZero(t *testing.T) {
	eng, adapter, cleanup := newRealEngineAdapter(t)
	defer cleanup()
	ctx := context.Background()

	w, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:      "explain",
		Concept:    "Redis eviction policy",
		Content:    "allkeys-lru with a 2GB cap",
		Confidence: 1.0,
	})
	if err != nil {
		t.Fatalf("seed write: %v", err)
	}

	res, err := adapter.Explain(ctx, "explain", &ExplainRequest{
		EngramID: w.ID,
		Query:    []string{"kangaroo", "zeppelin"},
	})
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if res.Scored {
		t.Fatal("scored = true for a query that never reached the engram")
	}
	if res.Note == "" {
		t.Error("note is empty: an unscored explain must state that its components are absent")
	}
	if res.Components.SemanticSimilarity != nil {
		t.Errorf("semantic_similarity = %v, want null (not computed)", *res.Components.SemanticSimilarity)
	}
	if res.Components.FullTextRelevance != nil {
		t.Errorf("full_text_relevance = %v, want null (not computed)", *res.Components.FullTextRelevance)
	}
	// Query-independent facts survive.
	if res.Components.Confidence == nil || *res.Components.Confidence != 1.0 {
		t.Error("confidence must still be reported for an unscored engram")
	}
	if res.Concept == "" {
		t.Error("concept must still be reported for an unscored engram")
	}
}
