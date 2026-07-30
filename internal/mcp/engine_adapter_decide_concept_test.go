package mcp

// TestMCPEngineAdapterDecide_SetsConcept guards the muninn_decide sibling of
// #721: internal/mcp/engine_adapter.go's Decide method returned
// &WriteResult{ID: ..., Warnings: ...} without ever setting Concept, so every
// muninn_decide response also reported concept: "". A plain field assignment
// of the caller's decision text looked like the entire fix, and the
// "fresh write" subtest below still passes under that simpler fix — but
// engine.Decide routes through e.Write, whose content-hash dedup
// short-circuit returns a PRE-EXISTING engram's ID when two decisions submit
// identical content (same rationale). The "dedup" subtest proves the
// response must report the STORED engram's concept, not the second caller's
// decision text, which a plain assignment gets wrong.

import (
	"context"
	"testing"

	"github.com/scrypster/muninndb/internal/storage"
)

func TestMCPEngineAdapterDecide_SetsConcept(t *testing.T) {
	a, cleanup := newConceptAdapterEnv(t)
	defer cleanup()

	ctx := context.Background()

	t.Run("fresh write", func(t *testing.T) {
		got, err := a.Decide(ctx, "default", "Use PostgreSQL for the primary store",
			"It handles our write throughput and has good ecosystem support", nil, nil)
		if err != nil {
			t.Fatalf("Decide: %v", err)
		}
		if got.Concept != "Use PostgreSQL for the primary store" {
			t.Errorf("Concept = %q, want %q", got.Concept, "Use PostgreSQL for the primary store")
		}
	})

	t.Run("dedup returns the stored engram's concept, not the second caller's decision text", func(t *testing.T) {
		const rationale = "It is the option the team already has operational experience running"

		r1, err := a.Decide(ctx, "default", "Use PostgreSQL", rationale, nil, nil)
		if err != nil {
			t.Fatalf("Decide (first): %v", err)
		}
		r2, err := a.Decide(ctx, "default", "Use MySQL", rationale, nil, nil)
		if err != nil {
			t.Fatalf("Decide (second): %v", err)
		}
		if r2.ID != r1.ID {
			t.Fatalf("expected content-hash dedup to collapse onto the same engram, got r1.ID=%s r2.ID=%s", r1.ID, r2.ID)
		}

		storedID, err := storage.ParseULID(r2.ID)
		if err != nil {
			t.Fatalf("ParseULID(%q): %v", r2.ID, err)
		}
		stored, err := a.eng.GetEngram(ctx, "default", storedID)
		if err != nil {
			t.Fatalf("GetEngram: %v", err)
		}

		if r2.Concept != stored.Concept {
			t.Errorf("Concept = %q, want the stored engram's concept %q", r2.Concept, stored.Concept)
		}
		if r2.Concept != "Use PostgreSQL" {
			t.Errorf("Concept = %q, want %q (the first call's decision — dedup returned that engram, not r2's \"Use MySQL\")", r2.Concept, "Use PostgreSQL")
		}
	})
}
