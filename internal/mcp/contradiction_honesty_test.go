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
