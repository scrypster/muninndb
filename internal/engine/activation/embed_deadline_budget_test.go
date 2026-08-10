package activation_test

import (
	"context"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/engine/activation"
	"github.com/scrypster/muninndb/internal/storage"
)

// blockingEmbedder simulates an embed backend that hangs until its context is
// cancelled — e.g. an Ollama model runner stuck reloading (#658) — and then
// surfaces the context error, exactly like a real HTTP client would.
type blockingEmbedder struct{}

func (e *blockingEmbedder) Embed(ctx context.Context, _ []string) ([]float32, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func (e *blockingEmbedder) Tokenize(text string) []string { return []string{text} }

// TestRun_EmbedTimeoutLeavesBudgetForBM25Fallback pins #658: when the embed
// backend hangs until the CALLER's context deadline, BM25-only recall must
// still complete within that same deadline — the whole reason the fallback
// (#577/#578) exists is to degrade gracefully, and a fallback that can never
// be reached is not a fallback. Before the fix, phase1 handed the embed call
// the caller's context unmodified: by the time Embed returned
// context.DeadlineExceeded, the shared context was already expired, so the
// early deadline check in Run's traversal phase (the same HopDepth>0 path a
// production recall resolves via vault plasticity) returned ctx.Err()
// directly — the exact "activation: context deadline exceeded" reported in
// the issue — before phase6 ever scored the FTS results phase2 already held.
func TestRun_EmbedTimeoutLeavesBudgetForBM25Fallback(t *testing.T) {
	store := newStubStore()
	eng := &storage.Engram{
		Concept: "reachable via bm25", Content: "content",
		Confidence: 1.0, Stability: 30.0, Relevance: 0.8,
	}
	store.writeEngram(eng)

	fts := &stubFTS{results: []activation.ScoredID{{ID: eng.ID, Score: 0.9}}}
	e := activation.New(store, fts, &emptyHNSW{}, &blockingEmbedder{})

	// A short overall budget mirrors a real MCP request deadline. The embed
	// backend in this test hangs until ctx itself expires, exactly the
	// reported failure mode — no separate embed-specific timeout is set here
	// so the fix must come from the pipeline reserving its own sub-budget.
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()

	result, err := e.Run(ctx, &activation.ActivateRequest{
		Context:    []string{"reachable via bm25"},
		Threshold:  0.0,
		MaxResults: 5,
		HopDepth:   1,
	})
	if err != nil {
		t.Fatalf("Run returned an error instead of degrading to BM25-only recall: %v", err)
	}
	if !result.SemanticDegraded {
		t.Errorf("SemanticDegraded = false, want true — the embed backend never returned before its sub-budget expired")
	}
	found := false
	for _, a := range result.Activations {
		if a.Engram.ID == eng.ID {
			found = true
		}
	}
	if !found {
		t.Errorf("BM25/FTS candidate was not returned — the embed timeout consumed the entire caller deadline, leaving none for the fallback")
	}
}
