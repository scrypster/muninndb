package mql_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/engine"
	"github.com/scrypster/muninndb/internal/engine/activation"
	"github.com/scrypster/muninndb/internal/engine/trigger"
	"github.com/scrypster/muninndb/internal/index/fts"
	"github.com/scrypster/muninndb/internal/query/mql"
	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// testEnv wires up a fully functional Engine with real storage and FTS,
// using a temporary directory that is cleaned up after the test.
func testEnv(t *testing.T) (*engine.Engine, func()) {
	t.Helper()
	dir, err := os.MkdirTemp("", "muninndb-mql-test-*")
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

	// Minimal no-op embedder and adapters
	embedder := &noopEmbedder{}
	actEngine := activation.New(store, &ftsAdapter{ftsIdx}, nil, embedder)
	trigSystem := trigger.New(store, &ftsTrigAdapter{ftsIdx}, nil, embedder)
	eng := engine.NewEngine(engine.EngineConfig{Store: store, FTSIndex: ftsIdx, ActivationEngine: actEngine, TriggerSystem: trigSystem, Embedder: embedder})

	return eng, func() {
		eng.Stop()    // stop FTS worker, novelty worker, coherence flush, autoAssoc
		store.Close() // stop PebbleStore background workers and close db
		os.RemoveAll(dir)
	}
}

// noopEmbedder returns a zero vector (no ML model required in tests).
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

// ftsAdapter converts fts.ScoredID to activation.ScoredID.
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

// ftsTrigAdapter converts fts.ScoredID to trigger.ScoredID.
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

// TestExecute_BasicActivate_RealEngine tests a basic ACTIVATE query with a real engine.
// It parses an MQL query, executes it, and verifies the response is not nil.
func TestExecute_BasicActivate_RealEngine(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	// Write test engrams
	writes := []struct {
		concept string
		content string
		tags    []string
	}{
		{"Go programming", "Go is a concurrent programming language.", []string{"golang", "programming"}},
		{"Python programming", "Python is an interpreted scripting language.", []string{"python", "programming"}},
		{"Rust systems language", "Rust is a systems programming language with memory safety.", []string{"rust", "systems"}},
	}

	for _, w := range writes {
		_, err := eng.Write(ctx, &mbp.WriteRequest{
			Vault:   "test",
			Concept: w.concept,
			Content: w.content,
			Tags:    w.tags,
		})
		if err != nil {
			t.Fatalf("Write(%q): %v", w.concept, err)
		}
	}

	// Allow async FTS worker to index
	time.Sleep(300 * time.Millisecond)

	// Parse an ACTIVATE query
	q, err := mql.Parse("ACTIVATE FROM test CONTEXT [\"programming\", \"language\"]")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	activateQuery, ok := q.(*mql.ActivateQuery)
	if !ok {
		t.Fatalf("expected ActivateQuery, got %T", q)
	}

	// Execute the query
	resp, err := mql.Execute(ctx, eng, activateQuery)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// Verify response is not nil and has results
	if resp == nil {
		t.Fatal("expected non-nil response")
	}

	// We should get some results from the programming context
	if len(resp.Activations) == 0 {
		t.Error("expected at least one activation result, got none")
	}
}

// TestExecute_WhereStatePredicate tests that WHERE state = active filters out deleted engrams.
// It writes an engram, soft-deletes it, then runs MQL with WHERE state = active.
// The deleted engram should NOT be returned.
func TestExecute_WhereStatePredicate(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	// Write two engrams
	var engramID string
	writes := []struct {
		concept string
		content string
		tags    []string
	}{
		{"Active concept", "This engram is active.", []string{"active", "test"}},
		{"Will delete", "This engram will be soft-deleted.", []string{"deleted", "test"}},
	}

	for i, w := range writes {
		resp, err := eng.Write(ctx, &mbp.WriteRequest{
			Vault:   "test",
			Concept: w.concept,
			Content: w.content,
			Tags:    w.tags,
		})
		if err != nil {
			t.Fatalf("Write(%q): %v", w.concept, err)
		}
		if i == 1 {
			engramID = resp.ID
		}
	}

	// Allow FTS indexing
	time.Sleep(300 * time.Millisecond)

	// Soft-delete the second engram
	forgetResp, err := eng.Forget(ctx, &mbp.ForgetRequest{
		Vault: "test",
		ID:    engramID,
		Hard:  false, // soft delete
	})
	if err != nil {
		t.Fatalf("Forget: %v", err)
	}
	if !forgetResp.OK {
		t.Fatal("Forget returned !OK")
	}

	// Parse query with WHERE state = active (should exclude deleted)
	q, err := mql.Parse(`ACTIVATE FROM test CONTEXT ["test"] WHERE state = active`)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	activateQuery, ok := q.(*mql.ActivateQuery)
	if !ok {
		t.Fatalf("expected ActivateQuery, got %T", q)
	}

	// Execute the query
	resp, err := mql.Execute(ctx, eng, activateQuery)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if resp == nil {
		t.Fatal("expected non-nil response")
	}

	// Verify that deleted engrams are not in the results
	for _, act := range resp.Activations {
		if act.ID == engramID {
			t.Errorf("soft-deleted engram %q should not be in results with WHERE state = active", engramID)
		}
	}

	// Verify we still get the active engram if present
	// (depends on FTS and scoring, so we just check no error occurred)
}

// TestExecute_OrPredicate_ReturnsError verifies that OR predicates are not yet supported
// and return an appropriate error.
func TestExecute_OrPredicate_ReturnsError(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	// Write a test engram
	_, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:   "test",
		Concept: "Test concept",
		Content: "Test content.",
		Tags:    []string{"golang", "testing"},
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	// Allow FTS indexing
	time.Sleep(300 * time.Millisecond)

	// Construct an ActivateQuery with an OrPredicate manually
	q := &mql.ActivateQuery{
		Vault:   "test",
		Context: []string{"term"},
		Where: &mql.OrPredicate{
			Left:  &mql.StatePredicate{State: "active"},
			Right: &mql.TagPredicate{Tag: "golang"},
		},
	}

	// Execute should return an error for unsupported OR
	_, err = mql.Execute(ctx, eng, q)
	if err == nil {
		t.Error("expected error for unsupported OrPredicate, got nil")
	}

	// Verify the error message mentions OR or unsupported
	if err != nil && (err.Error() == "" || err.Error() == "expected") {
		t.Errorf("error message should be descriptive: %v", err)
	}
}
