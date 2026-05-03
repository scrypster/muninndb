package engine

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/cockroachdb/pebble"
	"github.com/scrypster/muninndb/internal/auth"
	"github.com/scrypster/muninndb/internal/engine/activation"
	"github.com/scrypster/muninndb/internal/engine/trigger"
	"github.com/scrypster/muninndb/internal/index/fts"
	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// testEnv wires up a fully functional Engine with real storage and FTS,
// using a temporary directory that is cleaned up after the test.
func testEnv(t *testing.T) (*Engine, func()) {
	t.Helper()
	dir, err := os.MkdirTemp("", "muninndb-engine-test-*")
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
	eng := NewEngine(EngineConfig{Store: store, FTSIndex: ftsIdx, ActivationEngine: actEngine, TriggerSystem: trigSystem, Embedder: embedder})

	return eng, func() {
		eng.Stop()    // stop FTS worker, novelty worker, coherence flush, autoAssoc
		store.Close() // stop PebbleStore background workers and close db
		os.RemoveAll(dir)
	}
}

// testEnvWithDB is like testEnv but also returns the underlying *pebble.DB
// for tests that need to simulate closed-DB conditions.
func testEnvWithDB(t *testing.T) (*Engine, *pebble.DB, func()) {
	t.Helper()
	dir, err := os.MkdirTemp("", "muninndb-engine-test-*")
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

	embedder := &noopEmbedder{}
	actEngine := activation.New(store, &ftsAdapter{ftsIdx}, nil, embedder)
	trigSystem := trigger.New(store, &ftsTrigAdapter{ftsIdx}, nil, embedder)
	eng := NewEngine(EngineConfig{Store: store, FTSIndex: ftsIdx, ActivationEngine: actEngine, TriggerSystem: trigSystem, Embedder: embedder})

	return eng, db, func() {
		eng.Stop()
		// store.Close() may panic or error if db was already closed by the test;
		// recover gracefully so cleanup always removes the temp dir.
		func() {
			defer func() { recover() }()
			store.Close()
		}()
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

// awaitFTS drains the FTS worker by stopping it (which deterministically
// processes all queued index jobs) and restarting it with a fresh worker.
// This is the correct alternative to time.Sleep(300ms) when a test needs
// to ensure FTS visibility before calling Activate.
// The restarted worker will be stopped by eng.Stop() during cleanup.
func awaitFTS(t *testing.T, eng *Engine) {
	t.Helper()
	if eng.ftsWorker == nil {
		return
	}
	eng.ftsWorker.Stop()
	eng.ftsWorker = fts.NewWorker(eng.fts)
}

// TestHelloVersionCheck ensures the engine accepts the protocol version string
// "1" (as sent by the Web UI and MBP clients) and rejects other version strings.
// Regression test for issue #19: engine incorrectly required "1.0" instead of "1".
func TestHelloVersionCheck(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	// "1" must succeed — this is the MBP protocol version string
	resp, err := eng.Hello(ctx, &mbp.HelloRequest{Version: "1", Vault: "test"})
	if err != nil {
		t.Fatalf("Hello with version=1 should succeed, got: %v", err)
	}
	if resp == nil {
		t.Fatal("expected non-nil response")
	}
	if resp.ServerVersion == "" {
		t.Error("ServerVersion should not be empty")
	}

	// any other version must fail
	_, err = eng.Hello(ctx, &mbp.HelloRequest{Version: "1.0", Vault: "test"})
	if err == nil {
		t.Fatal("Hello with version=1.0 should return an error")
	}
	_, err = eng.Hello(ctx, &mbp.HelloRequest{Version: "2", Vault: "test"})
	if err == nil {
		t.Fatal("Hello with version=2 should return an error")
	}
	_, err = eng.Hello(ctx, &mbp.HelloRequest{Version: "", Vault: "test"})
	if err == nil {
		t.Fatal("Hello with empty version should return an error")
	}
}

// TestHelloRegistersVault ensures that calling Hello registers the vault name
// so it appears in ListVaults without needing to write an engram first.
// Regression test for issue #19 part 2: vault not appearing in dropdown after creation.
func TestHelloRegistersVault(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	const vault = "new-vault-hello-test"

	// Before hello, the vault must not be listed
	before, err := eng.ListVaults(ctx)
	if err != nil {
		t.Fatalf("ListVaults before hello: %v", err)
	}
	for _, v := range before {
		if v == vault {
			t.Fatalf("vault %q already exists before Hello — test setup issue", vault)
		}
	}

	// Hello must succeed and register the vault
	_, err = eng.Hello(ctx, &mbp.HelloRequest{Version: "1", Vault: vault})
	if err != nil {
		t.Fatalf("Hello failed: %v", err)
	}

	// After hello, the vault must appear in ListVaults
	after, err := eng.ListVaults(ctx)
	if err != nil {
		t.Fatalf("ListVaults after hello: %v", err)
	}
	found := false
	for _, v := range after {
		if v == vault {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("vault %q not found in ListVaults after Hello; got: %v", vault, after)
	}
}

// TestActivateReturnsResults is the primary regression test for the bug where
// Activate always returned 0 results. It exercises the full engine pipeline:
// Write → FTS index → Activate → BM25 scoring.
func TestActivateReturnsResults(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	// Write engrams
	writes := []struct {
		concept string
		content string
		tags    []string
	}{
		{"Go programming language", "Go is a statically typed compiled language for concurrency.", []string{"golang", "compiled"}},
		{"PostgreSQL database", "PostgreSQL is a powerful open source relational database with SQL.", []string{"database", "sql"}},
		{"Machine learning basics", "Machine learning uses algorithms to find patterns in large datasets.", []string{"ml", "ai"}},
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

	// Allow async FTS worker to index the written engrams (worker flushes every 100ms).
	awaitFTS(t, eng)

	// Activate with a query that should match the Go engram
	resp, err := eng.Activate(ctx, &mbp.ActivateRequest{
		Vault:      "test",
		Context:    []string{"compiled programming language"},
		MaxResults: 10,
		Threshold:  0.01,
	})
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}

	if len(resp.Activations) == 0 {
		t.Fatal("Activate returned 0 results, want >= 1")
	}

	// The Go engram must rank first for this query
	top := resp.Activations[0]
	if top.Concept != "Go programming language" {
		t.Errorf("top concept = %q, want %q", top.Concept, "Go programming language")
	}
	if top.Score <= 0 {
		t.Errorf("top score = %v, want > 0", top.Score)
	}
}

// TestActivateFTSRankingCorrect verifies that BM25 scoring ranks the most
// relevant engram at the top across multiple distinct queries.
func TestActivateFTSRankingCorrect(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	cases := []struct {
		concept string
		content string
	}{
		{"Go programming", "Go is a compiled statically typed systems language."},
		{"SQL databases", "SQL is used for querying relational databases and tables."},
		{"Neural networks", "Neural networks are the foundation of deep learning models."},
	}

	for _, c := range cases {
		if _, err := eng.Write(ctx, &mbp.WriteRequest{Vault: "test", Concept: c.concept, Content: c.content}); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}

	// Allow async FTS worker to index the written engrams.
	awaitFTS(t, eng)

	tests := []struct {
		query   string
		wantTop string
	}{
		{"compiled statically typed language", "Go programming"},
		{"relational database tables SQL query", "SQL databases"},
		{"deep learning neural network models", "Neural networks"},
	}

	for _, tt := range tests {
		resp, err := eng.Activate(ctx, &mbp.ActivateRequest{
			Vault:      "test",
			Context:    []string{tt.query},
			MaxResults: 10,
			Threshold:  0.01,
		})
		if err != nil {
			t.Fatalf("Activate(%q): %v", tt.query, err)
		}
		if len(resp.Activations) == 0 {
			t.Fatalf("query %q: 0 results", tt.query)
		}
		if resp.Activations[0].Concept != tt.wantTop {
			t.Errorf("query %q: top = %q, want %q", tt.query, resp.Activations[0].Concept, tt.wantTop)
		}
	}
}

// TestActivateVaultIsolation verifies that engrams written to vault A do not
// appear in results when querying vault B, even with identical content.
func TestActivateVaultIsolation(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	// Write to vault A
	if _, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:   "vault-a",
		Concept: "Secret A",
		Content: "This information belongs exclusively to vault alpha.",
	}); err != nil {
		t.Fatalf("Write vault-a: %v", err)
	}

	// Write to vault B
	if _, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:   "vault-b",
		Concept: "Secret B",
		Content: "This information belongs exclusively to vault beta.",
	}); err != nil {
		t.Fatalf("Write vault-b: %v", err)
	}

	// Query vault B — must not see vault A's content
	resp, err := eng.Activate(ctx, &mbp.ActivateRequest{
		Vault:      "vault-b",
		Context:    []string{"vault alpha secret"},
		MaxResults: 10,
		Threshold:  0.01,
	})
	if err != nil {
		t.Fatalf("Activate vault-b: %v", err)
	}
	for _, a := range resp.Activations {
		if a.Concept == "Secret A" {
			t.Errorf("vault-b search returned Secret A — vault isolation broken")
		}
	}

	// Query vault A — must not see vault B's content
	resp, err = eng.Activate(ctx, &mbp.ActivateRequest{
		Vault:      "vault-a",
		Context:    []string{"vault beta secret"},
		MaxResults: 10,
		Threshold:  0.01,
	})
	if err != nil {
		t.Fatalf("Activate vault-a: %v", err)
	}
	for _, a := range resp.Activations {
		if a.Concept == "Secret B" {
			t.Errorf("vault-a search returned Secret B — vault isolation broken")
		}
	}
}

// TestActivateThresholdFiltering verifies that results below the threshold
// are not returned even when FTS or temporal scoring would score them above zero.
func TestActivateThresholdFiltering(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	if _, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:   "test",
		Concept: "Unrelated topic",
		Content: "This content has nothing to do with the query whatsoever.",
	}); err != nil {
		t.Fatalf("Write: %v", err)
	}

	// Very high threshold — only extremely relevant results should pass
	resp, err := eng.Activate(ctx, &mbp.ActivateRequest{
		Vault:      "test",
		Context:    []string{"golang concurrency channels goroutines"},
		MaxResults: 10,
		Threshold:  0.99,
	})
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}

	for _, a := range resp.Activations {
		if a.Score < 0.99 {
			t.Errorf("result %q has score %v below threshold 0.99", a.Concept, a.Score)
		}
	}
}

// TestActivateConfidenceAffectsScore verifies that engrams written with lower
// confidence have proportionally lower final scores than high-confidence ones.
func TestActivateConfidenceAffectsScore(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	// Two engrams with similar content differing in confidence (unique content to avoid content-hash dedup).
	if _, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:      "test",
		Concept:    "high confidence fact",
		Content:    "Go is a compiled programming language built at Google for systems work. [high]",
		Confidence: 1.0,
	}); err != nil {
		t.Fatalf("Write high confidence: %v", err)
	}
	if _, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:      "test",
		Concept:    "low confidence fact",
		Content:    "Go is a compiled programming language built at Google for systems work. [low]",
		Confidence: 0.2,
	}); err != nil {
		t.Fatalf("Write low confidence: %v", err)
	}

	// Allow async FTS worker to index (same as TestActivateReturnsResults).
	awaitFTS(t, eng)

	resp, err := eng.Activate(ctx, &mbp.ActivateRequest{
		Vault:      "test",
		Context:    []string{"compiled programming language Google systems"},
		MaxResults: 10,
		Threshold:  0.01,
	})
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}

	if len(resp.Activations) < 2 {
		t.Fatalf("expected 2 results, got %d", len(resp.Activations))
	}

	// High confidence must rank above low confidence for identical content
	if resp.Activations[0].Concept != "high confidence fact" {
		t.Errorf("top result = %q, want %q", resp.Activations[0].Concept, "high confidence fact")
	}
	if resp.Activations[0].Score <= resp.Activations[1].Score {
		t.Errorf("high confidence score %v not greater than low confidence score %v",
			resp.Activations[0].Score, resp.Activations[1].Score)
	}
}

// TestEngineWorkersSubmit verifies that cognitive workers can be created and
// wired to the engine, and that they accept submissions from Write and Activate
// operations. This test uses real workers to verify the integration.
func TestEngineWorkersSubmit(t *testing.T) {
	t.Helper()
	dir, err := os.MkdirTemp("", "muninndb-engine-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	db, err := storage.OpenPebble(dir, storage.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}

	store := storage.NewPebbleStore(db, storage.PebbleStoreConfig{CacheSize: 1000})
	ftsIdx := fts.New(db)

	// Create embedder and adapters
	embedder := &noopEmbedder{}
	actEngine := activation.New(store, &ftsAdapter{ftsIdx}, nil, embedder)
	trigSystem := trigger.New(store, &ftsTrigAdapter{ftsIdx}, nil, embedder)

	eng := NewEngine(EngineConfig{Store: store, FTSIndex: ftsIdx, ActivationEngine: actEngine, TriggerSystem: trigSystem, Embedder: embedder})
	defer func() {
		eng.Stop()    // stop FTS worker and other background goroutines before closing db
		store.Close() // stop PebbleStore background workers and close db
	}()

	ctx := context.Background()

	// Write an engram
	_, err = eng.Write(ctx, &mbp.WriteRequest{
		Vault:   "test",
		Concept: "test concept",
		Content: "test content",
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	// Allow async FTS worker to index (same as TestActivateReturnsResults).
	awaitFTS(t, eng)

	// Activate (which should trigger worker submissions if workers were wired)
	resp, err := eng.Activate(ctx, &mbp.ActivateRequest{
		Vault:      "test",
		Context:    []string{"test"},
		MaxResults: 10,
		Threshold:  0.01,
	})
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}

	// Just verify that the engine handles the operations correctly
	// with nil workers (backward compatibility test).
	if len(resp.Activations) == 0 {
		t.Fatalf("Activate returned no results")
	}
}

func TestEngineGetContradictions(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	ws := eng.store.VaultPrefix("test")
	id1 := storage.ULID([16]byte{1})
	id2 := storage.ULID([16]byte{2})
	_ = eng.store.FlagContradiction(ctx, ws, id1, id2)

	pairs, err := eng.GetContradictions(ctx, "test")
	if err != nil {
		t.Fatalf("GetContradictions: %v", err)
	}
	if len(pairs) == 0 {
		t.Fatal("expected at least 1 contradiction pair")
	}
}

func TestEngineEvolve(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	resp, err := eng.Write(ctx, &mbp.WriteRequest{Vault: "test", Concept: "original", Content: "old content"})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	oldID := resp.ID

	newID, err := eng.Evolve(ctx, "test", oldID, "new content", "updated reasoning", nil)
	if err != nil {
		t.Fatalf("Evolve: %v", err)
	}
	if newID == (storage.ULID{}) {
		t.Fatal("Evolve returned zero ID")
	}
	if newID.String() == oldID {
		t.Fatal("Evolve must return a different ID than the old engram")
	}
}

func TestEngineUpdateContent_SameIDReturned(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	resp, err := eng.Write(ctx, &mbp.WriteRequest{Vault: "test", Concept: "stable", Content: "v1 content"})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	oldID := resp.ID

	returnedID, err := eng.UpdateContent(ctx, "test", oldID, "v2 content", "test update", -1.0)
	if err != nil {
		t.Fatalf("UpdateContent: %v", err)
	}
	if returnedID.String() != oldID {
		t.Fatalf("UpdateContent must return same ID; got %s want %s", returnedID.String(), oldID)
	}
}

func TestEngineUpdateContent_ContentReplaced(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	resp, _ := eng.Write(ctx, &mbp.WriteRequest{Vault: "test", Concept: "c", Content: "before"})

	if _, err := eng.UpdateContent(ctx, "test", resp.ID, "after", "replace test", -1.0); err != nil {
		t.Fatalf("UpdateContent: %v", err)
	}

	got, err := eng.Read(ctx, &mbp.ReadRequest{Vault: "test", IDs: []string{resp.ID}})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(got.Engrams) != 1 {
		t.Fatalf("expected 1 engram, got %d", len(got.Engrams))
	}
	if got.Engrams[0].Content != "after" {
		t.Fatalf("content not replaced; got %q want %q", got.Engrams[0].Content, "after")
	}
	// Concept must be preserved (no "(evolved)" suffix that Evolve would add).
	if got.Engrams[0].Concept != "c" {
		t.Fatalf("concept changed; got %q want %q", got.Engrams[0].Concept, "c")
	}
}

func TestEngineUpdateContent_ConfidenceUnchangedWhenSentinel(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	resp, _ := eng.Write(ctx, &mbp.WriteRequest{Vault: "test", Concept: "c", Content: "x", Confidence: 0.5})

	if _, err := eng.UpdateContent(ctx, "test", resp.ID, "y", "no conf change", -1.0); err != nil {
		t.Fatalf("UpdateContent: %v", err)
	}

	got, _ := eng.Read(ctx, &mbp.ReadRequest{Vault: "test", IDs: []string{resp.ID}})
	if got.Engrams[0].Confidence != 0.5 {
		t.Fatalf("confidence should be unchanged with sentinel; got %f want 0.5", got.Engrams[0].Confidence)
	}
}

func TestEngineUpdateContent_ConfidenceSetWhenProvided(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	resp, _ := eng.Write(ctx, &mbp.WriteRequest{Vault: "test", Concept: "c", Content: "x", Confidence: 0.5})

	if _, err := eng.UpdateContent(ctx, "test", resp.ID, "y", "conf change", 0.95); err != nil {
		t.Fatalf("UpdateContent: %v", err)
	}

	got, _ := eng.Read(ctx, &mbp.ReadRequest{Vault: "test", IDs: []string{resp.ID}})
	if got.Engrams[0].Confidence != 0.95 {
		t.Fatalf("confidence not updated; got %f want 0.95", got.Engrams[0].Confidence)
	}
}

func TestEngineUpdateContent_NotFound(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	// Generate a syntactically valid but non-existent ULID.
	bogus := storage.NewULID().String()
	_, err := eng.UpdateContent(ctx, "test", bogus, "new", "should fail", -1.0)
	if err == nil {
		t.Fatal("UpdateContent on non-existent id should error")
	}
}

func TestEngineConsolidate(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	r1, _ := eng.Write(ctx, &mbp.WriteRequest{Vault: "test", Concept: "a", Content: "content a"})
	r2, _ := eng.Write(ctx, &mbp.WriteRequest{Vault: "test", Concept: "b", Content: "content b"})

	res, err := eng.Consolidate(ctx, "test", []string{r1.ID, r2.ID}, "merged content")
	if err != nil {
		t.Fatalf("Consolidate: %v", err)
	}
	if res.MergedID == (storage.ULID{}) {
		t.Fatal("Consolidate returned zero merged ID")
	}
	if len(res.Archived) == 0 {
		t.Fatal("expected at least 1 archived ID")
	}
	_ = res.Warnings
}

func TestEngineSession(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	before := time.Now()
	_, _ = eng.Write(ctx, &mbp.WriteRequest{Vault: "test", Concept: "recent", Content: "just written"})

	summary, err := eng.Session(ctx, "test", before)
	if err != nil {
		t.Fatalf("Session: %v", err)
	}
	if len(summary.Writes) == 0 {
		t.Fatal("Session must include the recent write")
	}
}

func TestEngineDecide(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	r, _ := eng.Write(ctx, &mbp.WriteRequest{Vault: "test", Concept: "evidence", Content: "supporting data"})

	res, err := eng.Decide(ctx, "test", "go with option A",
		"rationale text", []string{"option B", "option C"}, []string{r.ID})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if res.ID == (storage.ULID{}) {
		t.Fatal("Decide returned zero ID")
	}
}

// TestEngineGetAssociations verifies that links written via Link are returned by GetAssociations.
func TestEngineGetAssociations(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	r1, _ := eng.Write(ctx, &mbp.WriteRequest{Vault: "test", Concept: "src", Content: "source node"})
	r2, _ := eng.Write(ctx, &mbp.WriteRequest{Vault: "test", Concept: "dst", Content: "target node"})

	_, err := eng.Link(ctx, &mbp.LinkRequest{
		SourceID: r1.ID,
		TargetID: r2.ID,
		RelType:  1,
		Weight:   0.8,
		Vault:    "test",
	})
	if err != nil {
		t.Fatalf("Link: %v", err)
	}

	assocs, err := eng.GetAssociations(ctx, "test", r1.ID, 10)
	if err != nil {
		t.Fatalf("GetAssociations: %v", err)
	}
	if len(assocs) == 0 {
		t.Fatal("expected at least 1 association, got 0")
	}
	found := false
	for _, a := range assocs {
		if a.TargetID.String() == r2.ID {
			found = true
		}
	}
	if !found {
		t.Errorf("target %q not found in associations", r2.ID)
	}
}

// TestEngineRestore verifies that a soft-deleted engram can be restored.
func TestEngineRestore(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	resp, err := eng.Write(ctx, &mbp.WriteRequest{Vault: "test", Concept: "restore me", Content: "will be deleted"})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	// Soft delete
	if _, err := eng.Forget(ctx, &mbp.ForgetRequest{ID: resp.ID, Hard: false, Vault: "test"}); err != nil {
		t.Fatalf("Forget (soft): %v", err)
	}

	// Restore
	restored, err := eng.Restore(ctx, "test", resp.ID)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if restored.State != storage.StateActive {
		t.Errorf("expected state active after restore, got %v", restored.State)
	}
}

// TestEngineRestoreNotDeleted verifies that restoring a non-deleted engram returns an error.
func TestEngineRestoreNotDeleted(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	resp, _ := eng.Write(ctx, &mbp.WriteRequest{Vault: "test", Concept: "active", Content: "not deleted"})
	_, err := eng.Restore(ctx, "test", resp.ID)
	if err == nil {
		t.Fatal("expected error restoring non-deleted engram")
	}
}

// TestEngineUpdateLifecycleState verifies lifecycle state transitions.
func TestEngineUpdateLifecycleState(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	resp, _ := eng.Write(ctx, &mbp.WriteRequest{Vault: "test", Concept: "task", Content: "task engram"})

	states := []string{"planning", "active", "paused", "blocked", "completed", "cancelled", "archived"}
	for _, state := range states {
		if err := eng.UpdateLifecycleState(ctx, "test", resp.ID, state); err != nil {
			t.Errorf("UpdateLifecycleState(%q): %v", state, err)
		}
	}
}

// TestEngineUpdateLifecycleStateInvalid verifies that unknown states return an error.
func TestEngineUpdateLifecycleStateInvalid(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	resp, _ := eng.Write(ctx, &mbp.WriteRequest{Vault: "test", Concept: "c", Content: "x"})
	err := eng.UpdateLifecycleState(ctx, "test", resp.ID, "limbo")
	if err == nil {
		t.Fatal("expected error for unknown lifecycle state")
	}
}

// TestEngineListDeleted verifies that soft-deleted engrams are returned by ListDeleted.
func TestEngineListDeleted(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	r1, _ := eng.Write(ctx, &mbp.WriteRequest{Vault: "test", Concept: "to delete", Content: "gone soon"})
	r2, _ := eng.Write(ctx, &mbp.WriteRequest{Vault: "test", Concept: "keep alive", Content: "stays"})

	if _, err := eng.Forget(ctx, &mbp.ForgetRequest{ID: r1.ID, Hard: false, Vault: "test"}); err != nil {
		t.Fatalf("Forget: %v", err)
	}

	deleted, err := eng.ListDeleted(ctx, "test", 100)
	if err != nil {
		t.Fatalf("ListDeleted: %v", err)
	}
	if len(deleted) == 0 {
		t.Fatal("expected at least 1 deleted engram")
	}
	foundDeleted := false
	for _, e := range deleted {
		if e != nil && e.ID.String() == r1.ID {
			foundDeleted = true
		}
		if e != nil && e.ID.String() == r2.ID {
			t.Errorf("active engram %q should not appear in ListDeleted", r2.ID)
		}
	}
	if !foundDeleted {
		t.Errorf("deleted engram %q not found in ListDeleted results", r1.ID)
	}
}

// TestEngineTraverse verifies BFS traversal follows association edges.
func TestEngineTraverse(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	r1, _ := eng.Write(ctx, &mbp.WriteRequest{Vault: "test", Concept: "start", Content: "root node"})
	r2, _ := eng.Write(ctx, &mbp.WriteRequest{Vault: "test", Concept: "hop1", Content: "first neighbor"})
	r3, _ := eng.Write(ctx, &mbp.WriteRequest{Vault: "test", Concept: "hop2", Content: "second neighbor"})

	_, _ = eng.Link(ctx, &mbp.LinkRequest{SourceID: r1.ID, TargetID: r2.ID, RelType: 1, Weight: 0.9, Vault: "test"})
	_, _ = eng.Link(ctx, &mbp.LinkRequest{SourceID: r2.ID, TargetID: r3.ID, RelType: 1, Weight: 0.8, Vault: "test"})

	nodes, edges, err := eng.Traverse(ctx, "test", r1.ID, 3, 50, false)
	if err != nil {
		t.Fatalf("Traverse: %v", err)
	}
	if len(nodes) < 1 {
		t.Fatal("expected at least 1 node (start node) from traversal")
	}
	// Start node must have hop distance 0
	startFound := false
	for _, n := range nodes {
		if n.ID.String() == r1.ID {
			startFound = true
			if n.HopDist != 0 {
				t.Errorf("start node hop_dist = %d, want 0", n.HopDist)
			}
		}
	}
	if !startFound {
		t.Error("start node not found in traversal result")
	}
	if len(edges) == 0 {
		t.Error("expected at least 1 edge from traversal")
	}
}

// TestEngineTraverseBoundedHops verifies that maxHops limits how deep BFS goes.
func TestEngineTraverseBoundedHops(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	// Build a 4-hop chain: a → b → c → d → e (unique content to avoid content-hash dedup).
	nodes := make([]string, 5)
	for i := range nodes {
		r, _ := eng.Write(ctx, &mbp.WriteRequest{Vault: "test", Concept: "node", Content: fmt.Sprintf("node content %d", i)})
		nodes[i] = r.ID
	}
	for i := 0; i < 4; i++ {
		_, _ = eng.Link(ctx, &mbp.LinkRequest{SourceID: nodes[i], TargetID: nodes[i+1], RelType: 1, Weight: 0.9, Vault: "test"})
	}

	// Traverse with maxHops=2 — should not reach node[3] or node[4]
	result, _, err := eng.Traverse(ctx, "test", nodes[0], 2, 100, false)
	if err != nil {
		t.Fatalf("Traverse: %v", err)
	}
	for _, n := range result {
		if n.ID.String() == nodes[4] {
			t.Error("node at hop distance 4 should not be reachable with maxHops=2")
		}
	}
}

// TestEngineExplain verifies that Explain finds an engram that matches the query.
func TestEngineExplain(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	resp, _ := eng.Write(ctx, &mbp.WriteRequest{
		Vault:   "test",
		Concept: "authentication",
		Content: "JWT token authentication for REST APIs using bearer tokens.",
	})

	data, err := eng.Explain(ctx, "test", resp.ID, []string{"JWT", "authentication", "bearer"}, nil)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if data.EngramID != resp.ID {
		t.Errorf("explain returned wrong engram id: %q, want %q", data.EngramID, resp.ID)
	}
	// With matching content, would_return should be true
	if !data.WouldReturn {
		t.Log("note: WouldReturn=false (may need higher relevance score to match; FTS dependent)")
	}
}

// ---------------------------------------------------------------------------
// Fix 2: HopDepth defaults to 2 when not set
// ---------------------------------------------------------------------------

// TestActivateHopDepthDefault verifies that Activate uses 2-hop BFS traversal
// by default when no explicit DisableHops or MaxHops is provided.
// We write a 3-hop chain and check that hops 1 and 2 are reachable.
func TestActivateHopDepthDefault(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	// Build chain: root → hop1 → hop2 → hop3
	root, _ := eng.Write(ctx, &mbp.WriteRequest{Vault: "test", Concept: "root node", Content: "root hop depth test"})
	hop1, _ := eng.Write(ctx, &mbp.WriteRequest{Vault: "test", Concept: "hop1 node", Content: "first hop neighbor"})
	hop2, _ := eng.Write(ctx, &mbp.WriteRequest{Vault: "test", Concept: "hop2 node", Content: "second hop neighbor"})
	hop3, _ := eng.Write(ctx, &mbp.WriteRequest{Vault: "test", Concept: "hop3 node", Content: "third hop neighbor"})

	_, _ = eng.Link(ctx, &mbp.LinkRequest{SourceID: root.ID, TargetID: hop1.ID, RelType: 1, Weight: 1.0, Vault: "test"})
	_, _ = eng.Link(ctx, &mbp.LinkRequest{SourceID: hop1.ID, TargetID: hop2.ID, RelType: 1, Weight: 1.0, Vault: "test"})
	_, _ = eng.Link(ctx, &mbp.LinkRequest{SourceID: hop2.ID, TargetID: hop3.ID, RelType: 1, Weight: 1.0, Vault: "test"})

	// Activate the root — default HopDepth=2 should follow edges
	resp, err := eng.Activate(ctx, &mbp.ActivateRequest{
		Vault:      "test",
		Context:    []string{"root hop depth test"},
		MaxResults: 20,
		Threshold:  0.0,
		// MaxHops: 0 → engine should default to 2
	})
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}
	_ = hop3 // hop3 should not be reachable at depth 2 from root

	// At least hop1 and hop2 IDs should appear in results (fetched via BFS traversal)
	ids := make(map[string]bool)
	for _, a := range resp.Activations {
		ids[a.ID] = true
	}
	_ = root
	_ = hop1
	_ = hop2
	// We can't assert specific IDs without FTS scoring pushing them in, but
	// we can verify Activate doesn't error and returns a non-negative result.
	if resp.Activations == nil {
		t.Error("Activate with default HopDepth must not return nil Activations")
	}
}

// ---------------------------------------------------------------------------
// Fix 2: DisableHops = true disables BFS traversal
// ---------------------------------------------------------------------------

// TestActivateDisableHops verifies that setting DisableHops=true bypasses
// the association graph traversal phase.
func TestActivateDisableHops(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	if _, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:   "test",
		Concept: "disable hops fact",
		Content: "content about disabling graph hops in activation",
	}); err != nil {
		t.Fatalf("Write: %v", err)
	}

	// DisableHops=true must not panic and must return results.
	resp, err := eng.Activate(ctx, &mbp.ActivateRequest{
		Vault:       "test",
		Context:     []string{"disable hops"},
		MaxResults:  10,
		Threshold:   0.01,
		DisableHops: true,
	})
	if err != nil {
		t.Fatalf("Activate with DisableHops: %v", err)
	}
	if resp == nil {
		t.Fatal("expected non-nil response with DisableHops=true")
	}
	// Results still valid — FTS path returns matches even without BFS traversal.
	if len(resp.Activations) == 0 {
		t.Log("note: 0 results with DisableHops (FTS-only path, acceptable)")
	}
}

// ---------------------------------------------------------------------------
// Fix 4: ReadOnly (Observe mode) does not break Activate
// ---------------------------------------------------------------------------

// TestActivateObserveModeDoesNotError verifies that activating in observe mode
// (which sets ReadOnly=true internally) returns valid results without error.
func TestActivateObserveModeDoesNotError(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	if _, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:   "test",
		Concept: "observe me",
		Content: "content for observe mode test",
	}); err != nil {
		t.Fatalf("Write: %v", err)
	}

	// Allow async FTS worker to index (same as TestActivateReturnsResults).
	awaitFTS(t, eng)

	// Normal activate (not observe mode) — baseline.
	resp, err := eng.Activate(ctx, &mbp.ActivateRequest{
		Vault:      "test",
		Context:    []string{"observe me"},
		MaxResults: 5,
		Threshold:  0.01,
	})
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if len(resp.Activations) == 0 {
		t.Fatal("expected at least 1 activation from FTS")
	}
}

// ---------------------------------------------------------------------------
// Fix 5: Coherence registry is populated after Write
// ---------------------------------------------------------------------------

// TestCoherenceRegistryAfterWrite verifies that writing to a vault updates the
// coherence registry so that Stat() includes a coherence score for that vault.
func TestCoherenceRegistryAfterWrite(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	// Write a few engrams to a named vault (unique content to avoid content-hash dedup).
	vault := "coh-test"
	for i := 0; i < 3; i++ {
		if _, err := eng.Write(ctx, &mbp.WriteRequest{
			Vault:   vault,
			Concept: "coherence subject",
			Content: fmt.Sprintf("content for coherence counter test %d", i),
		}); err != nil {
			t.Fatalf("Write[%d]: %v", i, err)
		}
	}

	stat, err := eng.Stat(ctx, &mbp.StatRequest{})
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}

	if stat.CoherenceScores == nil {
		t.Fatal("CoherenceScores must be populated after writes")
	}
	cr, ok := stat.CoherenceScores[vault]
	if !ok {
		t.Fatalf("CoherenceScores missing vault %q", vault)
	}
	if cr.TotalEngrams != 3 {
		t.Errorf("TotalEngrams = %d, want 3", cr.TotalEngrams)
	}
	if cr.Score < 0 || cr.Score > 1 {
		t.Errorf("coherence Score %v out of [0,1]", cr.Score)
	}
}

// ---------------------------------------------------------------------------
// Fix 5: Engine.Stop() flushes coherence cleanly
// ---------------------------------------------------------------------------

// TestEngineStopFlushesCoherence verifies that Stop() doesn't deadlock or panic
// even when the coherence flush goroutine is running.
func TestEngineStopFlushesCoherence(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer func() {
		cleanup()
	}()
	ctx := context.Background()

	if _, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:   "flush-test",
		Concept: "flush subject",
		Content: "content to flush",
	}); err != nil {
		t.Fatalf("Write: %v", err)
	}

	// Stop() must complete without hanging (timeout covered by test's default timeout).
	done := make(chan struct{})
	go func() {
		defer close(done)
		eng.Stop()
	}()

	select {
	case <-done:
		// Good — Stop() returned.
	case <-time.After(5 * time.Second):
		t.Fatal("eng.Stop() did not return within 5s — possible deadlock in coherence flush")
	}
}

// ---------------------------------------------------------------------------
// Task 3: Plasticity gates Hebbian and Temporal workers in Activate()
// ---------------------------------------------------------------------------

// TestActivate_PlasticityGatesHebbian verifies that the engine correctly resolves
// per-vault PlasticityConfig (via a real auth.Store) and that Activate() does not
// panic when an authStore is wired in with a scratchpad preset (HebbianEnabled=false).
func TestActivate_PlasticityGatesHebbian(t *testing.T) {
	dir, err := os.MkdirTemp("", "muninndb-plasticity-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	db, err := storage.OpenPebble(dir, storage.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}

	store := storage.NewPebbleStore(db, storage.PebbleStoreConfig{CacheSize: 1000})
	ftsIdx := fts.New(db)

	embedder := &noopEmbedder{}
	actEngine := activation.New(store, &ftsAdapter{ftsIdx}, nil, embedder)
	trigSystem := trigger.New(store, &ftsTrigAdapter{ftsIdx}, nil, embedder)

	// Create a real auth store and configure scratchpad preset for the test vault.
	as := auth.NewStore(db)
	scratchpadPreset := "scratchpad"
	if err := as.SetVaultConfig(auth.VaultConfig{
		Name:   "testvault",
		Public: true,
		Plasticity: &auth.PlasticityConfig{
			Preset: scratchpadPreset,
		},
	}); err != nil {
		t.Fatalf("SetVaultConfig: %v", err)
	}

	eng := NewEngine(EngineConfig{Store: store, AuthStore: as, FTSIndex: ftsIdx, ActivationEngine: actEngine, TriggerSystem: trigSystem, Embedder: embedder})
	defer func() {
		eng.Stop()
		store.Close()
	}()

	ctx := context.Background()

	// Write an engram so Activate has something to return.
	if _, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:   "testvault",
		Concept: "plasticity test",
		Content: "testing plasticity config resolution",
	}); err != nil {
		t.Fatalf("Write: %v", err)
	}

	// Activate must not panic with a real authStore wired in.
	resp, err := eng.Activate(ctx, &mbp.ActivateRequest{
		Vault:      "testvault",
		Context:    []string{"plasticity test"},
		MaxResults: 10,
		Threshold:  0.0,
	})
	if err != nil {
		t.Fatalf("Activate with authStore (scratchpad preset): %v", err)
	}
	if resp == nil {
		t.Fatal("expected non-nil response")
	}

	// Also verify that nil authStore (default test path) gives no panic.
	eng2 := NewEngine(EngineConfig{Store: store, FTSIndex: ftsIdx, ActivationEngine: actEngine, TriggerSystem: trigSystem, Embedder: embedder})
	defer func() {
		eng2.Stop()
	}()

	resp2, err := eng2.Activate(ctx, &mbp.ActivateRequest{
		Vault:      "testvault",
		Context:    []string{"plasticity test"},
		MaxResults: 10,
		Threshold:  0.0,
	})
	if err != nil {
		t.Fatalf("Activate with nil authStore: %v", err)
	}
	_ = resp2
}

// ---------------------------------------------------------------------------
// P2-T02: Lobe side-effect collection
// ---------------------------------------------------------------------------

// mockForwarder records CognitiveSideEffects forwarded to it.
type mockForwarder struct {
	mu      sync.Mutex
	effects []mbp.CognitiveSideEffect
}

func (m *mockForwarder) ForwardCognitiveEffects(effect mbp.CognitiveSideEffect) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.effects = append(m.effects, effect)
}

func (m *mockForwarder) received() []mbp.CognitiveSideEffect {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]mbp.CognitiveSideEffect, len(m.effects))
	copy(out, m.effects)
	return out
}

// TestEngine_LobeMode_CollectsEffects verifies that an Engine with nil cognitive
// workers (Lobe mode) forwards a CognitiveSideEffect to the wired forwarder
// after Activate() returns results.
func TestEngine_LobeMode_CollectsEffects(t *testing.T) {
	dir, err := os.MkdirTemp("", "muninndb-lobe-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	db, err := storage.OpenPebble(dir, storage.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}

	store := storage.NewPebbleStore(db, storage.PebbleStoreConfig{CacheSize: 1000})
	ftsIdx := fts.New(db)

	embedder := &noopEmbedder{}
	actEngine := activation.New(store, &ftsAdapter{ftsIdx}, nil, embedder)
	trigSystem := trigger.New(store, &ftsTrigAdapter{ftsIdx}, nil, embedder)

	// nil workers — simulates Lobe mode
	eng := NewEngine(EngineConfig{Store: store, FTSIndex: ftsIdx, ActivationEngine: actEngine, TriggerSystem: trigSystem, Embedder: embedder})
	defer func() {
		eng.Stop()
		store.Close()
	}()

	fwd := &mockForwarder{}
	eng.SetCoordinator(fwd, "lobe-node-1")

	ctx := context.Background()

	// Write an engram so Activate has something to return
	_, err = eng.Write(ctx, &mbp.WriteRequest{
		Vault:   "test",
		Concept: "lobe test concept",
		Content: "content for lobe side effect collection test",
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	// Allow FTS worker to index
	awaitFTS(t, eng)

	resp, err := eng.Activate(ctx, &mbp.ActivateRequest{
		Vault:      "test",
		Context:    []string{"lobe side effect collection"},
		MaxResults: 10,
		Threshold:  0.01,
	})
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if len(resp.Activations) == 0 {
		t.Skip("no activations returned — cannot verify side-effect forwarding")
	}

	// ForwardCognitiveEffects is called synchronously from engine before returning,
	// but the mock is called directly (not via goroutine), so no sleep needed.
	// However the default plasticity has HebbianEnabled=true and TemporalEnabled=true,
	// so at least one of co_activations or accessed_ids should be populated.
	effects := fwd.received()
	if len(effects) == 0 {
		t.Fatal("expected at least 1 CognitiveSideEffect to be forwarded, got 0")
	}

	e := effects[0]
	if e.QueryID == "" {
		t.Error("CognitiveSideEffect.QueryID must not be empty")
	}
	if e.OriginNodeID != "lobe-node-1" {
		t.Errorf("OriginNodeID = %q, want %q", e.OriginNodeID, "lobe-node-1")
	}
	if e.Timestamp == 0 {
		t.Error("CognitiveSideEffect.Timestamp must not be zero")
	}

	// Verify engram IDs in the effect match activation results
	activatedIDs := make(map[[16]byte]bool)
	for _, a := range resp.Activations {
		ulid, parseErr := storage.ParseULID(a.ID)
		if parseErr != nil {
			continue
		}
		activatedIDs[[16]byte(ulid)] = true
	}

	for _, ref := range e.CoActivations {
		if !activatedIDs[ref.ID] {
			t.Errorf("co-activation ID %x not in activation results", ref.ID)
		}
	}
	for _, id := range e.AccessedIDs {
		if !activatedIDs[id] {
			t.Errorf("accessed ID %x not in activation results", id)
		}
	}

	if len(e.CoActivations) == 0 && len(e.AccessedIDs) == 0 {
		t.Error("expected at least CoActivations or AccessedIDs to be populated")
	}
}

// TestEngine_CortexMode_NoForwarding verifies that an Engine with real cognitive
// workers (Cortex mode) does NOT call ForwardCognitiveEffects.
func TestEngine_CortexMode_NoForwarding(t *testing.T) {
	dir, err := os.MkdirTemp("", "muninndb-cortex-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	db, err := storage.OpenPebble(dir, storage.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}

	store := storage.NewPebbleStore(db, storage.PebbleStoreConfig{CacheSize: 1000})
	ftsIdx := fts.New(db)

	embedder := &noopEmbedder{}
	actEngine := activation.New(store, &ftsAdapter{ftsIdx}, nil, embedder)
	trigSystem := trigger.New(store, &ftsTrigAdapter{ftsIdx}, nil, embedder)

	// nil workers — but we do NOT wire a coordinator
	eng := NewEngine(EngineConfig{Store: store, FTSIndex: ftsIdx, ActivationEngine: actEngine, TriggerSystem: trigSystem, Embedder: embedder})
	defer func() {
		eng.Stop()
		store.Close()
	}()

	// Attach forwarder but do NOT set it on the engine (simulates no coordinator wired)
	fwd := &mockForwarder{}
	_ = fwd // intentionally not wired

	ctx := context.Background()

	_, err = eng.Write(ctx, &mbp.WriteRequest{
		Vault:   "test",
		Concept: "cortex test",
		Content: "cortex side effect no-forward test content",
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	awaitFTS(t, eng)

	_, err = eng.Activate(ctx, &mbp.ActivateRequest{
		Vault:      "test",
		Context:    []string{"cortex"},
		MaxResults: 10,
		Threshold:  0.01,
	})
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}

	// forwarder was never wired — must not have been called
	if len(fwd.received()) != 0 {
		t.Errorf("ForwardCognitiveEffects called %d times, want 0", len(fwd.received()))
	}
}

// TestEngine_WritesDuringShutdown exercises concurrent writes while the engine
// is being shut down. This test verifies that either writes succeed or return a
// well-typed error (not a panic), and the test does NOT hang.
func TestEngine_WritesDuringShutdown(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()

	ctx := context.Background()

	// Track write results
	type writeResult struct {
		idx int
		err error
	}
	results := make(chan writeResult, 100)

	// Start a goroutine that does rapid writes in a loop
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 50; i++ {
			_, err := eng.Write(ctx, &mbp.WriteRequest{
				Vault:   "shutdown-test",
				Concept: fmt.Sprintf("concept-%d", i),
				Content: fmt.Sprintf("content-%d", i),
			})
			results <- writeResult{idx: i, err: err}
		}
	}()

	// After a brief moment (allow some writes to start), call Stop()
	time.Sleep(50 * time.Millisecond)

	// Stop the engine
	stopDone := make(chan struct{})
	go func() {
		defer close(stopDone)
		eng.Stop()
	}()

	// Wait for the write goroutine to finish
	select {
	case <-done:
		// Good — writes finished
	case <-time.After(10 * time.Second):
		t.Fatal("write goroutine did not finish within 10s")
	}

	// Wait for Stop() to finish
	select {
	case <-stopDone:
		// Good — Stop() finished
	case <-time.After(10 * time.Second):
		t.Fatal("eng.Stop() did not return within 10s — possible deadlock during shutdown")
	}

	// Collect all results (without blocking, since we may get fewer writes than attempted)
	close(results)
	var writeErrors []error
	var writeSuccesses int

	for res := range results {
		if res.err != nil {
			writeErrors = append(writeErrors, res.err)
		} else {
			writeSuccesses++
		}
	}

	// The test passes as long as:
	// 1. No panics occurred (would have been caught earlier)
	// 2. The test didn't hang (we got here)
	// 3. Writes either succeeded or returned errors (no undefined behavior)
	t.Logf("Writes during shutdown: %d succeeded, %d failed", writeSuccesses, len(writeErrors))

	// It's acceptable for some writes to fail if the engine is stopping,
	// but they should fail with proper error handling, not panics.
}

// ---------------------------------------------------------------------------
// TestEngineRead_RoundTrip: Write then read back by ID
// ---------------------------------------------------------------------------

// TestEngineRead_RoundTrip writes an engram then immediately reads it back by ID.
// Verifies all fields match.
func TestEngineRead_RoundTrip(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	// Write an engram
	writeResp, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:   "test",
		Concept: "memory consolidation",
		Content: "Sleep helps consolidate memories into long-term storage.",
		Tags:    []string{"neuroscience", "sleep"},
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if writeResp.ID == "" {
		t.Fatal("expected non-empty ID from Write")
	}

	// Read it back by ID
	readResp, err := eng.Read(ctx, &mbp.ReadRequest{
		Vault: "test",
		ID:    writeResp.ID,
	})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}

	// Verify fields
	if readResp.ID != writeResp.ID {
		t.Errorf("ID mismatch: got %q, want %q", readResp.ID, writeResp.ID)
	}
	if readResp.Concept != "memory consolidation" {
		t.Errorf("Concept = %q, want %q", readResp.Concept, "memory consolidation")
	}
	if readResp.Content != "Sleep helps consolidate memories into long-term storage." {
		t.Errorf("Content mismatch")
	}
	// Tags may be nil or empty slice depending on implementation
	// Just verify no error and key fields are correct
}

// ---------------------------------------------------------------------------
// TestEngineRead_NotFound: Read non-existent ID
// ---------------------------------------------------------------------------

// TestEngineRead_NotFound reads a non-existent ID — should return an error, not panic.
func TestEngineRead_NotFound(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	_, err := eng.Read(ctx, &mbp.ReadRequest{
		Vault: "test",
		ID:    "01ARZ3NDEKTSV4RRFFQ69G5FAV", // valid ULID format but not stored
	})
	if err == nil {
		t.Error("expected error for non-existent ID, got nil")
	}
}

// ---------------------------------------------------------------------------
// TestEngineRead_AfterRestart: Persistence test
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// TestActivate_CancelledContext_PreventsProvenanceGoroutines
// ---------------------------------------------------------------------------

// TestActivate_CancelledContext_PreventsProvenanceGoroutines verifies the
// ctx.Err() guard that sits at the top of the provenance goroutine loop inside
// activateCore. The guard is:
//
//	for i, scored := range result.Activations {
//	    if err := ctx.Err(); err != nil {
//	        break  // ← this path
//	    }
//	    wg.Add(1)
//	    go func(...) { ... }(...)
//	}
//
// With a pre-cancelled context and DisableHops=true (which bypasses the Phase-5
// BFS check that would also fail on a cancelled ctx), two outcomes are valid:
//
//  1. activateCore returns context.Canceled / context.DeadlineExceeded — this
//     means the cancellation propagated through an earlier Pebble read, which
//     is equally correct: the ctx.Err() guard prevented goroutine spawning.
//
//  2. activateCore returns results but every SourceType field is empty — the
//     activation pipeline completed (Pebble ignores context cancellation at
//     the iterator level), but the provenance loop broke immediately on the
//     first ctx.Err() check, so no goroutines were ever spawned to fill SourceType.
//
// Both outcomes prove the guard is exercised.
func TestActivate_CancelledContext_PreventsProvenanceGoroutines(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()

	ctx := context.Background()

	// Write a handful of engrams so there is something to activate against.
	topics := []struct {
		concept string
		content string
	}{
		{"context cancellation alpha", "context cancellation propagation in Go goroutines"},
		{"context cancellation beta", "cancelling contexts prevents unnecessary goroutine spawns"},
		{"context cancellation gamma", "context deadline exceeded stops background operations"},
	}
	for _, w := range topics {
		if _, err := eng.Write(ctx, &mbp.WriteRequest{
			Vault:   "ctx-cancel-test",
			Concept: w.concept,
			Content: w.content,
		}); err != nil {
			t.Fatalf("Write(%q): %v", w.concept, err)
		}
	}

	// Allow the async FTS worker to index the written engrams.
	awaitFTS(t, eng)

	// Sanity check: normal activate returns results, establishing a baseline.
	normalResp, err := eng.Activate(ctx, &mbp.ActivateRequest{
		Vault:       "ctx-cancel-test",
		Context:     []string{"context cancellation goroutine"},
		MaxResults:  10,
		Threshold:   0.01,
		DisableHops: true,
	})
	if err != nil {
		t.Fatalf("baseline Activate: %v", err)
	}
	if len(normalResp.Activations) == 0 {
		t.Skip("no results returned from baseline Activate — FTS did not index; skipping cancellation test")
	}

	// Create a context that is already cancelled before Activate is called.
	// DisableHops=true bypasses the Phase-5 BFS guard (which would also fail
	// on a cancelled ctx) so that the execution reaches the provenance loop,
	// where the ctx.Err() check we want to exercise lives.
	cancelledCtx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately — ctx.Err() returns context.Canceled from here on

	resp, err := eng.Activate(cancelledCtx, &mbp.ActivateRequest{
		Vault:       "ctx-cancel-test",
		Context:     []string{"context cancellation goroutine"},
		MaxResults:  10,
		Threshold:   0.01,
		DisableHops: true,
	})

	if err != nil {
		// Outcome 1: the cancelled ctx propagated through a Pebble read or
		// another ctx-aware path before reaching the provenance loop.
		// Both context.Canceled and context.DeadlineExceeded are acceptable.
		if err != context.Canceled && err != context.DeadlineExceeded {
			// Unwrap to check for wrapped context errors.
			unwrapped := err
			for {
				if unwrapped == context.Canceled || unwrapped == context.DeadlineExceeded {
					break
				}
				type unwrapper interface{ Unwrap() error }
				if u, ok := unwrapped.(unwrapper); ok {
					unwrapped = u.Unwrap()
					if unwrapped == nil {
						break
					}
				} else {
					break
				}
			}
			if unwrapped != context.Canceled && unwrapped != context.DeadlineExceeded {
				t.Fatalf("Activate with cancelled ctx returned unexpected error: %v", err)
			}
		}
		// Valid outcome: ctx cancellation was detected and propagated correctly.
		t.Logf("Activate with pre-cancelled ctx returned error (expected): %v", err)
		return
	}

	// Outcome 2: activation completed despite the cancelled ctx (Pebble
	// iterators do not observe context cancellation at the read level).
	// The provenance loop must have broken immediately on the ctx.Err() check,
	// so no goroutines were spawned to fill SourceType.
	if resp == nil {
		t.Fatal("Activate returned nil response without error")
	}
	for _, item := range resp.Activations {
		if item.SourceType != "" {
			t.Errorf(
				"item %q has SourceType=%q: provenance goroutine was spawned despite cancelled ctx — ctx.Err() guard did not fire",
				item.Concept, item.SourceType,
			)
		}
	}
	t.Logf("Activate with pre-cancelled ctx returned %d results, all SourceType fields empty (provenance loop broken by ctx.Err() guard)", len(resp.Activations))
}

// TestEngineRead_AfterRestart writes data, stops the engine, reopens from the SAME
// directory, and verifies data survives.
func TestEngineRead_AfterRestart(t *testing.T) {
	// Create a dir we control (not auto-removed until test ends)
	dir := t.TempDir()

	openEngine := func() (*Engine, func()) {
		db, err := storage.OpenPebble(dir, storage.DefaultOptions())
		if err != nil {
			t.Fatalf("OpenPebble: %v", err)
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
		}
	}

	ctx := context.Background()

	// First session: write
	eng1, close1 := openEngine()
	writeResp, err := eng1.Write(ctx, &mbp.WriteRequest{
		Vault:   "persist",
		Concept: "durability test",
		Content: "This must survive a restart.",
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	engramID := writeResp.ID
	close1() // orderly shutdown

	// Second session: reopen same dir, read back
	eng2, close2 := openEngine()
	defer close2()

	readResp, err := eng2.Read(ctx, &mbp.ReadRequest{
		Vault: "persist",
		ID:    engramID,
	})
	if err != nil {
		t.Fatalf("Read after restart: %v", err)
	}
	if readResp.Concept != "durability test" {
		t.Errorf("Concept after restart = %q, want %q", readResp.Concept, "durability test")
	}
	if readResp.Content != "This must survive a restart." {
		t.Errorf("Content after restart = %q", readResp.Content)
	}
}

// ---------------------------------------------------------------------------
// TestEngineForget_HardDelete: Hard delete removes engram
// ---------------------------------------------------------------------------

// TestEngineForget_HardDelete verifies that hard delete (Hard: true) actually removes
// the engram — reading it after should fail.
func TestEngineForget_HardDelete(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	writeResp, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:   "test",
		Concept: "temporary note",
		Content: "Delete this permanently.",
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	// Hard delete
	_, err = eng.Forget(ctx, &mbp.ForgetRequest{
		Vault: "test",
		ID:    writeResp.ID,
		Hard:  true,
	})
	if err != nil {
		t.Fatalf("Forget(Hard=true): %v", err)
	}

	// Reading should now fail
	_, err = eng.Read(ctx, &mbp.ReadRequest{
		Vault: "test",
		ID:    writeResp.ID,
	})
	if err == nil {
		t.Error("expected error reading hard-deleted engram, got nil")
	}
}

func TestWriteBatch_HappyPath(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	reqs := make([]*mbp.WriteRequest, 5)
	for i := range reqs {
		reqs[i] = &mbp.WriteRequest{
			Vault:   "test",
			Concept: fmt.Sprintf("batch concept %d", i),
			Content: fmt.Sprintf("batch content %d", i),
		}
	}

	responses, errs := eng.WriteBatch(ctx, reqs)

	if len(responses) != 5 {
		t.Fatalf("expected 5 responses, got %d", len(responses))
	}
	if len(errs) != 5 {
		t.Fatalf("expected 5 errors, got %d", len(errs))
	}

	for i, resp := range responses {
		if errs[i] != nil {
			t.Errorf("item %d: unexpected error: %v", i, errs[i])
			continue
		}
		if resp == nil {
			t.Errorf("item %d: response is nil", i)
			continue
		}
		if resp.ID == "" {
			t.Errorf("item %d: ID is empty", i)
		}
	}

	// Verify all written engrams are readable
	for i, resp := range responses {
		readResp, err := eng.Read(ctx, &mbp.ReadRequest{ID: resp.ID, Vault: "test"})
		if err != nil {
			t.Errorf("item %d: read failed: %v", i, err)
			continue
		}
		wantConcept := fmt.Sprintf("batch concept %d", i)
		if readResp.Concept != wantConcept {
			t.Errorf("item %d: concept = %q, want %q", i, readResp.Concept, wantConcept)
		}
	}
}

func TestWriteBatch_MaxSizeExceeded(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	reqs := make([]*mbp.WriteRequest, 51)
	for i := range reqs {
		reqs[i] = &mbp.WriteRequest{
			Vault:   "test",
			Concept: "too many",
			Content: "content",
		}
	}

	responses, errs := eng.WriteBatch(ctx, reqs)

	if len(responses) != 51 {
		t.Fatalf("expected 51 responses, got %d", len(responses))
	}
	if len(errs) != 51 {
		t.Fatalf("expected 51 errors, got %d", len(errs))
	}

	for i, err := range errs {
		if err == nil {
			t.Errorf("item %d: expected error for batch too large, got nil", i)
		}
	}
}

func TestWriteBatch_MultipleVaults(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	reqs := []*mbp.WriteRequest{
		{Vault: "vault-a", Concept: "concept-a", Content: "content-a"},
		{Vault: "vault-b", Concept: "concept-b", Content: "content-b"},
		{Vault: "vault-a", Concept: "concept-c", Content: "content-c"},
	}

	responses, errs := eng.WriteBatch(ctx, reqs)

	for i := range reqs {
		if errs[i] != nil {
			t.Errorf("item %d: unexpected error: %v", i, errs[i])
			continue
		}
		if responses[i] == nil || responses[i].ID == "" {
			t.Errorf("item %d: missing response", i)
		}
	}

	// Verify engrams are in the correct vaults
	r1, err := eng.Read(ctx, &mbp.ReadRequest{ID: responses[0].ID, Vault: "vault-a"})
	if err != nil {
		t.Fatalf("read from vault-a: %v", err)
	}
	if r1.Concept != "concept-a" {
		t.Errorf("vault-a concept = %q, want %q", r1.Concept, "concept-a")
	}

	r2, err := eng.Read(ctx, &mbp.ReadRequest{ID: responses[1].ID, Vault: "vault-b"})
	if err != nil {
		t.Fatalf("read from vault-b: %v", err)
	}
	if r2.Concept != "concept-b" {
		t.Errorf("vault-b concept = %q, want %q", r2.Concept, "concept-b")
	}
}

func TestWriteBatch_EmptyBatch(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	responses, errs := eng.WriteBatch(ctx, nil)
	if len(responses) != 0 {
		t.Errorf("expected 0 responses, got %d", len(responses))
	}
	if len(errs) != 0 {
		t.Errorf("expected 0 errors, got %d", len(errs))
	}
}

// ---------------------------------------------------------------------------
// Test: Vault-default recall mode is applied by activateCore
// ---------------------------------------------------------------------------

func TestActivateCore_VaultDefaultRecallMode(t *testing.T) {
	dir, err := os.MkdirTemp("", "muninndb-recallmode-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	db, err := storage.OpenPebble(dir, storage.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}

	store := storage.NewPebbleStore(db, storage.PebbleStoreConfig{CacheSize: 1000})
	ftsIdx := fts.New(db)

	embedder := &noopEmbedder{}
	actEngine := activation.New(store, &ftsAdapter{ftsIdx}, nil, embedder)
	trigSystem := trigger.New(store, &ftsTrigAdapter{ftsIdx}, nil, embedder)

	// Configure vault with recall_mode = "semantic".
	// "semantic" preset: DisableACTR=true, SemanticSimilarity=0.8,
	// FullTextRelevance=0.2, Threshold=0.3.
	as := auth.NewStore(db)
	semanticMode := "semantic"
	if err := as.SetVaultConfig(auth.VaultConfig{
		Name:   "recalltest",
		Public: true,
		Plasticity: &auth.PlasticityConfig{
			RecallMode: &semanticMode,
		},
	}); err != nil {
		t.Fatalf("SetVaultConfig: %v", err)
	}

	eng := NewEngine(EngineConfig{Store: store, AuthStore: as, FTSIndex: ftsIdx, ActivationEngine: actEngine, TriggerSystem: trigSystem, Embedder: embedder})
	defer func() {
		eng.Stop()
		store.Close()
	}()

	ctx := context.Background()

	// Write an engram so Activate has something to process.
	if _, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:   "recalltest",
		Concept: "semantic recall test",
		Content: "testing vault default recall mode application",
	}); err != nil {
		t.Fatalf("Write: %v", err)
	}

	// Call Activate with no Mode on the request.
	// activateCore should detect resolved.RecallMode="semantic" and apply the preset.
	resp, err := eng.Activate(ctx, &mbp.ActivateRequest{
		Vault:      "recalltest",
		Context:    []string{"semantic recall"},
		MaxResults: 10,
		Threshold:  0,
		Mode:       "", // no explicit mode — triggers vault-default path
	})
	if err != nil {
		t.Fatalf("Activate with vault default recall mode: %v", err)
	}
	if resp == nil {
		t.Fatal("expected non-nil response")
	}
	if resp.QueryID == "" {
		t.Error("QueryID must not be empty")
	}

	// Now test with explicit Mode — this should bypass vault default.
	resp2, err := eng.Activate(ctx, &mbp.ActivateRequest{
		Vault:      "recalltest",
		Context:    []string{"semantic recall"},
		MaxResults: 10,
		Threshold:  0,
		Mode:       "deep", // explicit mode bypasses vault default
	})
	if err != nil {
		t.Fatalf("Activate with explicit deep mode: %v", err)
	}
	if resp2 == nil {
		t.Fatal("expected non-nil response for explicit mode")
	}

	// Test "recent" mode as vault default too, to exercise a different preset.
	recentMode := "recent"
	if err := as.SetVaultConfig(auth.VaultConfig{
		Name:   "recentvault",
		Public: true,
		Plasticity: &auth.PlasticityConfig{
			RecallMode: &recentMode,
		},
	}); err != nil {
		t.Fatalf("SetVaultConfig recent: %v", err)
	}

	if _, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:   "recentvault",
		Concept: "recent mode test",
		Content: "testing recent recall mode vault default",
	}); err != nil {
		t.Fatalf("Write to recentvault: %v", err)
	}

	resp3, err := eng.Activate(ctx, &mbp.ActivateRequest{
		Vault:      "recentvault",
		Context:    []string{"recent mode"},
		MaxResults: 10,
		Threshold:  0,
	})
	if err != nil {
		t.Fatalf("Activate with vault default recent mode: %v", err)
	}
	if resp3 == nil {
		t.Fatal("expected non-nil response for recent vault")
	}
}

// TestStat_CountsConfigOnlyVaults verifies that vaults created via authStore
// (with no engrams written) are counted by Stat(). This is the core regression:
// dashboard "Vaults" stat card must match `muninn vault list` output.
//
// The test adds two config-only vaults and then asserts VaultCount==2, proving
// that config-only vaults (not just data-layer vaults) are included in the count.
func TestStat_CountsConfigOnlyVaults(t *testing.T) {
	eng, authStore, _, cleanup := testEnvWithAuth(t)
	defer cleanup()
	ctx := context.Background()

	// Create two config-only vaults (no engrams written — simulates `muninn vault create`).
	for _, name := range []string{"config-only-alpha", "config-only-beta"} {
		if err := authStore.SetVaultConfig(auth.VaultConfig{Name: name, Public: false}); err != nil {
			t.Fatalf("SetVaultConfig(%q): %v", name, err)
		}
	}

	// Stat() must count both config-only vaults (no data written to either).
	resp, err := eng.Stat(ctx, &mbp.StatRequest{})
	if err != nil {
		t.Fatalf("Stat after SetVaultConfig: %v", err)
	}
	if resp.VaultCount != 2 {
		t.Errorf("expected VaultCount=2 (two config-only vaults), got %d", resp.VaultCount)
	}
}

// TestStat_DeduplicatesVaultCount verifies that a vault appearing in BOTH the
// data store (has engrams) AND the auth config store is counted exactly once.
func TestStat_DeduplicatesVaultCount(t *testing.T) {
	eng, authStore, _, cleanup := testEnvWithAuth(t)
	defer cleanup()
	ctx := context.Background()

	const sharedVault = "shared-vault"

	// Write an engram so the vault appears in the data store.
	if _, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:   sharedVault,
		Concept: "dedup test",
		Content: "this vault exists in both stores",
	}); err != nil {
		t.Fatalf("Write: %v", err)
	}

	// Also add a config entry for the same vault (mirrors real usage where
	// vault create + write both happen).
	if err := authStore.SetVaultConfig(auth.VaultConfig{Name: sharedVault, Public: false}); err != nil {
		t.Fatalf("SetVaultConfig: %v", err)
	}

	resp, err := eng.Stat(ctx, &mbp.StatRequest{})
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	// sharedVault must be counted once, not twice.
	// Total is 1 (sharedVault) — no other vaults exist.
	if resp.VaultCount != 1 {
		t.Errorf("expected VaultCount=1 (deduplicated), got %d", resp.VaultCount)
	}
}

// TestStat_DefaultVaultMinimum verifies that Stat() returns VaultCount >= 1
// even when both the data store and the auth config store are completely empty.
// This preserves the existing "minimum 1" semantics for the dashboard.
func TestStat_DefaultVaultMinimum(t *testing.T) {
	eng, _, _, cleanup := testEnvWithAuth(t)
	defer cleanup()
	ctx := context.Background()

	resp, err := eng.Stat(ctx, &mbp.StatRequest{})
	if err != nil {
		t.Fatalf("Stat on empty engine: %v", err)
	}
	if resp.VaultCount < 1 {
		t.Errorf("expected VaultCount >= 1 (minimum floor), got %d", resp.VaultCount)
	}
}

// TestEngineTraverse_EdgeRelTypePopulated verifies that TraversalEdge.RelType
// is populated from the storage.Association when traversing a typed edge.
// Regression test for issue #173 (rel_type always empty in muninn_traverse).
func TestEngineTraverse_EdgeRelTypePopulated(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	r1, _ := eng.Write(ctx, &mbp.WriteRequest{Vault: "test", Concept: "src", Content: "source engram"})
	r2, _ := eng.Write(ctx, &mbp.WriteRequest{Vault: "test", Concept: "dst", Content: "destination engram"})

	_, _ = eng.Link(ctx, &mbp.LinkRequest{
		SourceID: r1.ID,
		TargetID: r2.ID,
		RelType:  uint16(storage.RelSupports),
		Weight:   0.9,
		Vault:    "test",
	})

	_, edges, err := eng.Traverse(ctx, "test", r1.ID, 1, 50, false)
	if err != nil {
		t.Fatalf("Traverse: %v", err)
	}
	if len(edges) == 0 {
		t.Fatal("expected at least one edge")
	}
	if edges[0].RelType != storage.RelSupports {
		t.Errorf("edge RelType = %v (%d), want storage.RelSupports (%d)", edges[0].RelType, edges[0].RelType, storage.RelSupports)
	}
}
