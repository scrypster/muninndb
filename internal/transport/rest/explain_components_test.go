package rest

import (
	"context"
	"testing"

	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// TestRESTExplain_ReportsComponents: POST /api/explain is what the web
// console's "Score Components" panel renders. The adapter returned only id,
// concept, final score, would_return and threshold — components were never
// mapped, so the panel showed 0.0000 for every factor on every memory,
// including a fully-matching one.
func TestRESTExplain_ReportsComponents(t *testing.T) {
	eng, cleanup := newRESTRetryEnrichEnv(t)
	defer cleanup()
	ctx := context.Background()
	w, err := eng.Write(ctx, &mbp.WriteRequest{Vault: "default", Concept: "waterbar drainage", Content: "the waterbar on switchback four drains left"})
	if err != nil {
		t.Fatal(err)
	}
	eng.WaitWriteTimeIdle()
	wrapper := &RESTEngineWrapper{engine: eng}
	resp, err := wrapper.Explain(ctx, "default", &ExplainRequest{EngramID: w.ID, Query: []string{"waterbar drainage"}})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Components.FullTextRelevance <= 0 || resp.Components.Confidence <= 0 {
		t.Fatalf("REST explain components not mapped: %+v", resp.Components)
	}
	if resp.Components.DecayFactor != 1 {
		t.Errorf("ACT-R decay_factor = %.4f, applied value is 1", resp.Components.DecayFactor)
	}
}
