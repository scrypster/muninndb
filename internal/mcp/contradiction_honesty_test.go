package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/cognitive"
	"github.com/scrypster/muninndb/internal/engine"
	"github.com/scrypster/muninndb/internal/engine/activation"
	"github.com/scrypster/muninndb/internal/engine/trigger"
	"github.com/scrypster/muninndb/internal/index/fts"
	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// newContradictionServer wires a real engine behind the MCP server with a live
// ContradictWorker whose flush interval is compressed from the production 30s.
// The stub engine used by the other handler tests cannot catch the two places
// a contradiction field silently vanishes on this surface — the adapter's
// field mapping and the hand-built response maps — which is exactly what these
// tests cover (#764 risks 5 and the convert.go allocation predicate).
func newContradictionServer(t *testing.T, flushInterval time.Duration) (*engine.Engine, *MCPServer, func()) {
	t.Helper()
	dir, err := os.MkdirTemp("", "muninndb-mcp-contradiction-*")
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

	cw := cognitive.NewContradictWorker(cognitive.NewContradictStoreAdapter(store))
	cw.Worker.SetFlushInterval(flushInterval)
	ctx, cancel := context.WithCancel(context.Background())
	workerDone := make(chan struct{})
	go func() { defer close(workerDone); _ = cw.Worker.Run(ctx) }()

	eng := engine.NewEngine(engine.EngineConfig{
		Store: store, FTSIndex: ftsIdx, ActivationEngine: actEngine,
		TriggerSystem: trigSystem, Embedder: embedder,
		ContradictWorker: cw.Worker,
	})
	srv := newTestServerWith(NewEngineAdapter(eng, nil, nil))
	return eng, srv, func() {
		cancel()
		<-workerDone
		eng.Stop()
		store.Close()
		os.RemoveAll(dir)
	}
}

// seedContradictionOverMCP writes two rival facts and declares the
// contradiction through muninn_link, returning the two IDs.
func seedContradictionOverMCP(t *testing.T, eng *engine.Engine, srv *MCPServer, vault string) (string, string) {
	t.Helper()
	ctx := context.Background()
	a, err := eng.Write(ctx, &mbp.WriteRequest{Vault: vault, Concept: "request timeout limit",
		Content: "the request timeout limit is 180ms"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := eng.Write(ctx, &mbp.WriteRequest{Vault: vault, Concept: "request timeout limit revised",
		Content: "the request timeout limit is 320ms"})
	if err != nil {
		t.Fatal(err)
	}
	link := fmt.Sprintf(`{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_link","arguments":{"vault":%q,"source_id":%q,"target_id":%q,"relation":"contradicts"}}}`,
		vault, b.ID, a.ID)
	if rec := postRPC(t, srv, link); rec.Code != 200 {
		t.Fatalf("muninn_link: HTTP %d: %s", rec.Code, rec.Body.String())
	}
	return a.ID, b.ID
}

func contradictionsOverMCP(t *testing.T, srv *MCPServer, vault string) map[string]any {
	t.Helper()
	rpc := fmt.Sprintf(`{"jsonrpc":"2.0","method":"tools/call","id":2,"params":{"name":"muninn_contradictions","arguments":{"vault":%q}}}`, vault)
	return extractInnerJSON(t, decodeResp(t, postRPC(t, srv, rpc).Body.String()))
}

// TestContradictionsOverMCP_DeclaredNotPending is R2 for #764 D1/F1.3.
//
// A declared contradiction is durable at muninn_link return and honored by
// recall on the very next query, so the surface must not label it
// "pending_detection" — both round-7 evaluators read that string as "your
// declaration has not taken effect", polled it, and concluded the feature was
// dead. What IS outstanding is the asynchronous confidence penalty, and it is
// reported as its own field so it cannot be confused with the declaration.
func TestContradictionsOverMCP_DeclaredNotPending(t *testing.T) {
	eng, srv, cleanup := newContradictionServer(t, 150*time.Millisecond)
	defer cleanup()

	seedContradictionOverMCP(t, eng, srv, "default")

	got := contradictionsOverMCP(t, srv, "default")
	list, _ := got["contradictions"].([]any)
	if len(list) != 1 {
		t.Fatalf("contradictions = %v, want exactly one pair immediately after the link", got["contradictions"])
	}
	entry, _ := list[0].(map[string]any)
	if entry["status"] != "declared" {
		t.Errorf("status = %v, want \"declared\" — the declaration is not pending anything", entry["status"])
	}
	if entry["confidence_penalty"] != "pending" {
		t.Errorf("confidence_penalty = %v, want \"pending\"", entry["confidence_penalty"])
	}

	// ...and the penalty flips to applied once the worker flushes, without the
	// status changing: provenance is not readiness.
	deadline := time.Now().Add(5 * time.Second)
	for {
		got = contradictionsOverMCP(t, srv, "default")
		list, _ = got["contradictions"].([]any)
		entry, _ = list[0].(map[string]any)
		if entry["confidence_penalty"] == "applied" {
			break
		}
		if time.Now().After(deadline) {
			b, _ := json.Marshal(got)
			t.Fatalf("confidence_penalty never became \"applied\": %s", b)
		}
		time.Sleep(25 * time.Millisecond)
	}
	if entry["status"] != "declared" {
		t.Errorf("status = %v after detection, want \"declared\" (provenance, not readiness)", entry["status"])
	}
}

// TestRecallOverMCP_ConflictBlockAndAnnotations is R4 for #764 D2.
//
// It exists specifically for the two places a contradiction field silently
// disappears on this surface, neither of which the engine-level test can see:
// convert.go's "should I allocate annotations at all" predicate (a row
// carrying no OTHER annotation would drop the field entirely), and
// handleRecall's hand-built result map, which is NOT a mirror of
// mbp.ActivateResponse — a field added to the struct alone reaches REST and
// vanishes here.
func TestRecallOverMCP_ConflictBlockAndAnnotations(t *testing.T) {
	eng, srv, cleanup := newContradictionServer(t, time.Hour) // no worker flush needed
	defer cleanup()

	a, b := seedContradictionOverMCP(t, eng, srv, "default")

	// Drain the write-time async workers (FTS indexing included) through the
	// engine's exported seam instead of polling a wall-clock deadline — the
	// 10s poll this replaces flaked at ~10% under -race on a loaded machine
	// (#722 doctrine: drain deterministically, never sleep past a race).
	eng.WaitWriteTimeIdle()

	rpc := `{"jsonrpc":"2.0","method":"tools/call","id":3,"params":{"name":"muninn_recall","arguments":{"vault":"default","context":"what is the request timeout limit","limit":5,"threshold":0.001}}}`
	got := extractInnerJSON(t, decodeResp(t, postRPC(t, srv, rpc).Body.String()))
	if mem, _ := got["memories"].([]any); len(mem) < 2 {
		t.Fatalf("both sides of the conflict not retrievable after the write-time drain: %v", got)
	}

	// The response-level block survived the hand-built map.
	block, ok := got["conflict"].(map[string]any)
	if !ok {
		t.Fatalf("muninn_recall response carries no top-level \"conflict\" key: %v", got)
	}
	if block["unresolved"] != true {
		t.Errorf("conflict.unresolved = %v, want true", block["unresolved"])
	}
	if w, _ := block["warning"].(string); w == "" {
		t.Error("conflict block carries no warning naming the resolution actions")
	}
	if pairs, _ := block["pairs"].([]any); len(pairs) != 1 {
		t.Errorf("conflict.pairs = %v, want exactly one pair", block["pairs"])
	}

	// Both rows carry the per-row annotation — and neither carries any OTHER
	// annotation, so this is exactly the convert.go allocation-predicate case.
	memories, _ := got["memories"].([]any)
	if len(memories) < 2 {
		t.Fatalf("want both sides of the conflict returned, got %d memories", len(memories))
	}
	seen := map[string]bool{}
	for _, raw := range memories {
		m, _ := raw.(map[string]any)
		id, _ := m["id"].(string)
		if id != a && id != b {
			continue
		}
		ann, ok := m["annotations"].(map[string]any)
		if !ok {
			t.Fatalf("row %s has no annotations object at all — the allocation predicate dropped it", id)
		}
		c, ok := ann["unresolved_contradiction"].(map[string]any)
		if !ok {
			t.Fatalf("row %s: annotations carry no unresolved_contradiction: %v", id, ann)
		}
		wantPartner := a
		if id == a {
			wantPartner = b
		}
		if c["with"] != wantPartner {
			t.Errorf("row %s names partner %v, want %s", id, c["with"], wantPartner)
		}
		if c["partner_in_results"] != true {
			t.Errorf("row %s: partner_in_results = %v, want true", id, c["partner_in_results"])
		}
		if s, _ := c["side"].(string); s != "asserted" && s != "challenged" {
			t.Errorf("row %s: side = %v", id, c["side"])
		}
		seen[id] = true
	}
	if !seen[a] || !seen[b] {
		t.Errorf("annotation missing on one side: a=%v b=%v", seen[a], seen[b])
	}
}
