package engine

import (
	"context"
	"testing"

	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// Explicit link weights arrived as 0 in traverse.
//
// Two independent evaluation rounds observed it: muninn_decide's evidence
// links "supports edges ... weight 0, oddly", and an explicit
// link(contradicts, weight=1.0) whose traverse edge reported 0. The weight IS
// persisted (it lives in the association key as a complement and
// WriteAssociation stores it three ways), so the loss is on a read path — and
// a declared weight surfacing as 0 tells the caller their declaration was
// ignored, which is this project's silent-substitution class on the exact
// channel (declared edges) it treats as ground truth.
func TestTraverse_ReportsDeclaredLinkWeight(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()
	const vault = "traverse-weight"

	a, err := eng.Write(ctx, &mbp.WriteRequest{Vault: vault, Content: "the ingest queue is bounded at four thousand", Concept: "queue bound"})
	if err != nil {
		t.Fatalf("write A: %v", err)
	}
	b, err := eng.Write(ctx, &mbp.WriteRequest{Vault: vault, Content: "the ingest queue is unbounded", Concept: "queue unbounded"})
	if err != nil {
		t.Fatalf("write B: %v", err)
	}

	if _, err := eng.Link(ctx, &mbp.LinkRequest{
		Vault: vault, SourceID: b.ID, TargetID: a.ID,
		RelType: uint16(storage.RelContradicts), Weight: 1.0,
	}); err != nil {
		t.Fatalf("link: %v", err)
	}

	_, edges, err := eng.Traverse(ctx, vault, b.ID, 1, 10, false)
	if err != nil {
		t.Fatalf("traverse: %v", err)
	}
	aULID, _ := storage.ParseULID(a.ID)
	found := false
	for _, e := range edges {
		if e.To == aULID && e.RelType == storage.RelContradicts {
			found = true
			if e.Weight != 1.0 {
				t.Fatalf("declared contradicts link weight 1.0 surfaced as %v in traverse — the "+
					"caller's declaration is being silently zeroed on the read path", e.Weight)
			}
		}
	}
	if !found {
		t.Fatalf("contradicts edge not found in traverse at all; edges=%v", edges)
	}
}
