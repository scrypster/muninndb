package mcp

// TestMCPEngineAdapterExplain_SetsConcept guards a third live instance of the
// #721 shape (after Evolve and Decide): internal/mcp/engine_adapter.go's
// Explain method copied five of engine.ExplainData's six fields
// (EngramID, FinalScore, WouldReturn, Threshold, Components — skipping only
// Concept) into ExplainResult, so muninn_explain always reported
// concept: "" on MCP even though REST's equivalent adapter
// (internal/transport/rest/engine_adapter.go) has set it correctly all
// along. ExplainResult.Concept has no `omitempty` (internal/mcp/types.go),
// so the zero value serializes as an explicit concept: "" on the wire.
//
// engine.ExplainData.Concept is only populated inside engine.Explain's
// WouldReturn branch (internal/engine/query.go) — it walks the activation
// results looking for the requested engram ID and only fills Concept (and the
// rest of the score breakdown) if it finds it there. So this test must seed a
// query that genuinely matches the engram, not just assert WouldReturn==true
// as a side effect.

import (
	"context"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/transport/mbp"
)

func TestMCPEngineAdapterExplain_SetsConcept(t *testing.T) {
	a, cleanup := newConceptAdapterEnv(t)
	defer cleanup()

	ctx := context.Background()
	seed, err := a.eng.Write(ctx, &mbp.WriteRequest{
		Vault:   "default",
		Concept: "Bob the baker",
		Content: "Bob bakes sourdough bread every morning at the corner bakery",
	})
	if err != nil {
		t.Fatalf("seed Write: %v", err)
	}

	req := &ExplainRequest{
		EngramID: seed.ID,
		Query:    []string{"sourdough", "bakery"},
	}

	// The environment's embedder is a noop (activation.NewNoopEmbedder in
	// newConceptAdapterEnv), so FTS is the only path that can make
	// WouldReturn true here (docs/internals/testing-hermeticity.md, async
	// source #3) — and FTS indexing runs off an async worker, not inline
	// with Write. package mcp has no access to the unexported
	// engine.waitWriteTimeIdle drain (white-box, package engine/activation
	// only), so this polls Explain's WouldReturn with a hard deadline
	// instead. This is a bounded READINESS wait, not a synchronization
	// primitive — it does not order two goroutines, it just bounds how long
	// we tolerate the indexer being behind, and it fails loudly (t.Fatalf)
	// rather than silently asserting on a stale/empty result if the deadline
	// is reached.
	deadline := time.Now().Add(5 * time.Second)
	var got *ExplainResult
	for {
		got, err = a.Explain(ctx, "default", req)
		if err != nil {
			t.Fatalf("Explain: %v", err)
		}
		if got.WouldReturn {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("Explain: WouldReturn never became true within the readiness deadline (last result: %+v)", got)
		}
		time.Sleep(10 * time.Millisecond)
	}

	if got.Concept != "Bob the baker" {
		t.Errorf("Concept = %q, want %q", got.Concept, "Bob the baker")
	}
}
