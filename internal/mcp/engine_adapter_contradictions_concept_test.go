package mcp

// TestMCPEngineAdapterContradictions_SetsConcepts guards the muninn_contradictions
// sibling of #721/#172 — the fourth live instance of the same defect shape
// (after Evolve, Decide, Explain): internal/mcp/engine_adapter.go's
// GetContradictions method built ContradictionPair{IDa, IDb} straight off the
// engine's [2]storage.ULID pairs and never populated ConceptA/ConceptB, so
// concept_a/concept_b always serialized as "" on MCP even though REST's
// equivalent (internal/transport/rest/engine_adapter.go, GetContradictions)
// has read both engrams back to populate them all along.
//
// engine.Engine has no public API that flags a contradiction — it is only
// ever written via storage.PebbleStore.FlagContradiction (the 0x0A marker),
// which production code never calls automatically. Seeding one here mirrors
// the exact pattern internal/engine/prospective_test.go's contradiction-notice
// test and internal/engine/engine_test.go's TestEngineGetContradictions
// already use: eng.Store().ResolveVaultPrefix + eng.Store().FlagContradiction,
// both exported. That is the narrowest available seeding path; there is no
// narrower one through the adapter or MCP surface.

import (
	"context"
	"testing"

	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

func TestMCPEngineAdapterContradictions_SetsConcepts(t *testing.T) {
	a, cleanup := newConceptAdapterEnv(t)
	defer cleanup()

	ctx := context.Background()
	seedA, err := a.eng.Write(ctx, &mbp.WriteRequest{
		Vault:   "default",
		Concept: "Deploys ship on Fridays",
		Content: "The team decided deploys are fine on Fridays given the on-call rotation",
	})
	if err != nil {
		t.Fatalf("seed Write A: %v", err)
	}
	seedB, err := a.eng.Write(ctx, &mbp.WriteRequest{
		Vault:   "default",
		Concept: "Deploys never ship on Fridays",
		Content: "The team decided Friday deploys are banned after last quarter's incident",
	})
	if err != nil {
		t.Fatalf("seed Write B: %v", err)
	}

	idA, err := storage.ParseULID(seedA.ID)
	if err != nil {
		t.Fatalf("ParseULID(a): %v", err)
	}
	idB, err := storage.ParseULID(seedB.ID)
	if err != nil {
		t.Fatalf("ParseULID(b): %v", err)
	}
	ws := a.eng.Store().ResolveVaultPrefix("default")
	if err := a.eng.Store().FlagContradiction(ctx, ws, idA, idB); err != nil {
		t.Fatalf("FlagContradiction: %v", err)
	}

	pairs, err := a.GetContradictions(ctx, "default")
	if err != nil {
		t.Fatalf("GetContradictions: %v", err)
	}
	if len(pairs) != 1 {
		t.Fatalf("GetContradictions: got %d pairs, want 1 (pairs=%+v)", len(pairs), pairs)
	}

	p := pairs[0]
	// FlagContradiction stores both directions and GetContradictions
	// canonicalizes by ULID ordering, so which of idA/idB lands in IDa vs
	// IDb is not under this test's control — assert by matching ID, not by
	// position.
	wantConcept := map[string]string{
		seedA.ID: "Deploys ship on Fridays",
		seedB.ID: "Deploys never ship on Fridays",
	}
	got := map[string]string{p.IDa: p.ConceptA, p.IDb: p.ConceptB}
	for id, wantC := range wantConcept {
		gotC, ok := got[id]
		if !ok {
			t.Fatalf("pair %+v does not reference expected id %s", p, id)
		}
		if gotC != wantC {
			t.Errorf("concept for id %s = %q, want %q", id, gotC, wantC)
		}
	}
}
