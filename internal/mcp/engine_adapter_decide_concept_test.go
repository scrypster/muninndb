package mcp

// TestMCPEngineAdapterDecide_SetsConcept guards the muninn_decide sibling of
// #721: internal/mcp/engine_adapter.go's Decide method returned
// &WriteResult{ID: ..., Warnings: ...} without ever setting Concept, so every
// muninn_decide response also reported concept: "". Unlike Evolve there is no
// store read-back precedence to apply — engine.Decide writes
// Concept: decision verbatim (internal/engine/engine.go) — so a plain field
// assignment in the adapter is the entire fix, and this test is what proves
// the wiring, not just the intent.

import (
	"context"
	"testing"
)

func TestMCPEngineAdapterDecide_SetsConcept(t *testing.T) {
	a, cleanup := newConceptAdapterEnv(t)
	defer cleanup()

	ctx := context.Background()
	got, err := a.Decide(ctx, "default", "Use PostgreSQL for the primary store",
		"It handles our write throughput and has good ecosystem support", nil, nil)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if got.Concept != "Use PostgreSQL for the primary store" {
		t.Errorf("Concept = %q, want %q", got.Concept, "Use PostgreSQL for the primary store")
	}
}
