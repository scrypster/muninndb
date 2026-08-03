package mcp

import (
	"context"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// ── muninn_decide: a decision, not a fact ────────────────────────────────────

// TestDecideAdapter_ReportsStoredConcept: muninn_decide returned
// {"concept":""} while storing the concept correctly. Two independent
// evaluators concluded data was being lost. Response-serialization bug.
//
// RED without the fix: the adapter builds WriteResult{ID, Warnings} only.
func TestDecideAdapter_ReportsStoredConcept(t *testing.T) {
	eng, adapter, cleanup := newRealEngineAdapter(t)
	defer cleanup()
	ctx := context.Background()

	const decision = "Adopt trunk-based development for the sandbox repo"
	res, err := adapter.Decide(ctx, "decide", decision, "Long-lived branches were stalling review", nil, nil)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if res.Concept != decision {
		t.Errorf("decide response concept = %q, want %q", res.Concept, decision)
	}
	// And it really is what was stored.
	id, err := storage.ParseULID(res.ID)
	if err != nil {
		t.Fatalf("parse id: %v", err)
	}
	stored, err := eng.GetEngram(ctx, "decide", id)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if stored.Concept != decision {
		t.Errorf("stored concept = %q, want %q", stored.Concept, decision)
	}
}

// ── muninn_evolve: response must report the stored concept ───────────────────

// TestEvolveAdapter_ReportsStoredConcept: same response-serialization bug on
// evolve — the successor's concept (carried from the predecessor, or the
// caller's override) was stored but never returned.
//
// RED without the fix: the adapter builds WriteResult{ID} only.
func TestEvolveAdapter_ReportsStoredConcept(t *testing.T) {
	eng, adapter, cleanup := newRealEngineAdapter(t)
	defer cleanup()
	ctx := context.Background()

	w, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:   "evolve",
		Concept: "Deploy cadence",
		Content: "We ship on Fridays",
	})
	if err != nil {
		t.Fatalf("seed write: %v", err)
	}

	// Carried concept.
	res, err := adapter.Evolve(ctx, "evolve", w.ID, "We ship on Tuesdays", "Friday deploys caused weekend pages", nil, "", nil, nil, timeZero())
	if err != nil {
		t.Fatalf("Evolve: %v", err)
	}
	if res.Concept != "Deploy cadence" {
		t.Errorf("evolve response concept = %q, want the carried %q", res.Concept, "Deploy cadence")
	}

	// Explicit concept override.
	res2, err := adapter.Evolve(ctx, "evolve", res.ID, "We ship continuously", "Moved to CD", nil, "Deploy cadence (CD)", nil, nil, timeZero())
	if err != nil {
		t.Fatalf("Evolve(2): %v", err)
	}
	if res2.Concept != "Deploy cadence (CD)" {
		t.Errorf("evolve response concept = %q, want the override %q", res2.Concept, "Deploy cadence (CD)")
	}
}

// timeZero keeps the Evolve call sites readable.
func timeZero() time.Time { return time.Time{} }
