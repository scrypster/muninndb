package engine

import (
	"context"
	"testing"

	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

func TestEngine_GetAnnotations_Contradicts(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	respA, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault: "default", Concept: "claim A", Content: "the sky is green",
	})
	if err != nil {
		t.Fatalf("Write A: %v", err)
	}
	respB, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault: "default", Concept: "claim B", Content: "the sky is blue",
	})
	if err != nil {
		t.Fatalf("Write B: %v", err)
	}

	_, err = eng.Link(ctx, &mbp.LinkRequest{
		Vault:    "default",
		SourceID: respA.ID,
		TargetID: respB.ID,
		RelType:  uint16(storage.RelContradicts),
		Weight:   1.0,
	})
	if err != nil {
		t.Fatalf("Link: %v", err)
	}

	ann, err := eng.GetAnnotations(ctx, "default", respA.ID, nil)
	if err != nil {
		t.Fatalf("GetAnnotations: %v", err)
	}
	if len(ann.ConflictsWith) != 1 || ann.ConflictsWith[0] != respB.ID {
		t.Errorf("ConflictsWith = %v, want [%s]", ann.ConflictsWith, respB.ID)
	}
	if ann.SupersededBy != "" {
		t.Errorf("SupersededBy should be empty, got %q", ann.SupersededBy)
	}
}

func TestEngine_GetAnnotations_SupersededBy(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	respOld, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault: "default", Concept: "old version", Content: "old content",
	})
	if err != nil {
		t.Fatalf("Write old: %v", err)
	}
	respNew, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault: "default", Concept: "new version", Content: "new content",
	})
	if err != nil {
		t.Fatalf("Write new: %v", err)
	}

	// new supersedes old: respNew → respOld with RelSupersedes
	_, err = eng.Link(ctx, &mbp.LinkRequest{
		Vault:    "default",
		SourceID: respNew.ID,
		TargetID: respOld.ID,
		RelType:  uint16(storage.RelSupersedes),
		Weight:   1.0,
	})
	if err != nil {
		t.Fatalf("Link: %v", err)
	}

	// GetAnnotations on the OLD engram — SupersededBy should be respNew.ID
	ann, err := eng.GetAnnotations(ctx, "default", respOld.ID, nil)
	if err != nil {
		t.Fatalf("GetAnnotations: %v", err)
	}
	if ann.SupersededBy != respNew.ID {
		t.Errorf("SupersededBy = %q, want %q", ann.SupersededBy, respNew.ID)
	}
}

func TestEngine_GetAnnotations_NoData(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	resp, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault: "default", Concept: "plain", Content: "no associations",
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	ann, err := eng.GetAnnotations(ctx, "default", resp.ID, nil)
	if err != nil {
		t.Fatalf("GetAnnotations: %v", err)
	}
	if len(ann.ConflictsWith) != 0 {
		t.Errorf("ConflictsWith should be empty, got %v", ann.ConflictsWith)
	}
	if ann.SupersededBy != "" {
		t.Errorf("SupersededBy should be empty, got %q", ann.SupersededBy)
	}
}

// #700: GetAnnotations must route ConflictsWith/SupersededBy targets through
// the SAME visibilityGate.Nameable the ranking phase's own annotation uses
// (COG-22), not a raw reverse-edge lookup. A candidate under a live foreign
// lease is exactly the case #548 already hides everywhere else in recall —
// this pins that GetAnnotations does not leak its ID back out through the
// annotate:true fallback.
func TestEngine_GetAnnotations_LeaseHiddenConflictTargetNotNamed(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	respA, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault: "default", Concept: "claim A", Content: "the sky is green",
	})
	if err != nil {
		t.Fatalf("Write A: %v", err)
	}
	respB, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault: "default", Concept: "claim B", Content: "the sky is blue",
	})
	if err != nil {
		t.Fatalf("Write B: %v", err)
	}
	_, err = eng.Link(ctx, &mbp.LinkRequest{
		Vault: "default", SourceID: respA.ID, TargetID: respB.ID,
		RelType: uint16(storage.RelContradicts), Weight: 1.0,
	})
	if err != nil {
		t.Fatalf("Link: %v", err)
	}

	// B is checked out by someone else — a live foreign lease, work-queue
	// visibility's #548 signal.
	if _, err := eng.Claim(ctx, "default", respB.ID, "someone-else", 300); err != nil {
		t.Fatalf("Claim: %v", err)
	}

	// A caller with no matching owner (empty CallerOwner) must not see B's ID
	// surface via A's ConflictsWith annotation.
	ann, err := eng.GetAnnotations(ctx, "default", respA.ID, &mbp.ActivateRequest{Vault: "default"})
	if err != nil {
		t.Fatalf("GetAnnotations: %v", err)
	}
	if len(ann.ConflictsWith) != 0 {
		t.Fatalf("ConflictsWith leaked a lease-hidden target: %v (want empty — the target is checked out by another owner)", ann.ConflictsWith)
	}
}
