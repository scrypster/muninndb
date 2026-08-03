package engine

import (
	"context"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/provenance"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// waitForProvenance polls the audit trail until an entry with the given
// operation appears. The provenance worker is async with no exported drain, so
// this polls the observable itself (bounded) rather than sleeping a guess.
func waitForProvenance(t *testing.T, eng *Engine, vault, id, op string) provenance.ProvenanceEntry {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		entries, err := eng.GetProvenance(ctx, vault, id)
		if err != nil {
			t.Fatalf("GetProvenance: %v", err)
		}
		for _, e := range entries {
			if e.Operation == op {
				return e
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("no provenance entry with operation %q for %s within 5s", op, id)
	return provenance.ProvenanceEntry{}
}

// TestEvolve_ProvenanceRecordsWhatChangedAndWhy is the defect test.
// muninn_provenance promises "who wrote it, what changed, and why"; for the one
// operation whose entire meaning is what-changed-and-why, the entry carried a
// timestamp, a source and the verb "evolve" and nothing else. The successor's
// entry must carry the predecessor it replaced, the caller's reason, and the
// valid-time boundary the change took effect at.
//
// The successor is the side that records it: the successor is the live engram a
// reader holds after an evolve (the predecessor is soft-deleted and hidden from
// the present), and predecessor_id mirrors read's superseded_by pointing the
// other way, so the pair of records is symmetric and neither invents a fact.
func TestEvolve_ProvenanceRecordsWhatChangedAndWhy(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	resp, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault: "test", Concept: "Deploy target", Content: "deploys go to us-west-1",
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	effectiveAt := time.Now().Add(1 * time.Minute).UTC().Truncate(time.Millisecond)
	reason := "region migration completed"

	newID, err := eng.EvolveAt(ctx, "test", resp.ID, "deploys go to us-east-2", reason, nil, "", nil, nil, effectiveAt)
	if err != nil {
		t.Fatalf("EvolveAt: %v", err)
	}

	entry := waitForProvenance(t, eng, "test", newID.String(), "evolve")
	if entry.Details == nil {
		t.Fatal("evolve provenance entry carries no Details — no predecessor, no reason, no effective_at")
	}
	if entry.Details.PredecessorID != resp.ID {
		t.Errorf("PredecessorID = %q, want %q", entry.Details.PredecessorID, resp.ID)
	}
	if entry.Details.Reason != reason {
		t.Errorf("Reason = %q, want %q", entry.Details.Reason, reason)
	}
	if entry.Details.EffectiveAt == nil {
		t.Fatal("EffectiveAt is absent, want the valid-time boundary")
	}
	if !entry.Details.EffectiveAt.Equal(effectiveAt) {
		t.Errorf("EffectiveAt = %v, want %v", entry.Details.EffectiveAt.UTC(), effectiveAt)
	}
}

// TestEvolve_ProvenanceOmitsEmptyReason pins that a reason-less evolve records
// absence as absence: predecessor and effective_at are still known and recorded,
// the reason field stays empty rather than being invented.
func TestEvolve_ProvenanceOmitsEmptyReason(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	resp, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault: "test", Concept: "Coffee order", Content: "flat white",
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	newID, err := eng.Evolve(ctx, "test", resp.ID, "cortado", "", nil, "")
	if err != nil {
		t.Fatalf("Evolve: %v", err)
	}

	entry := waitForProvenance(t, eng, "test", newID.String(), "evolve")
	if entry.Details == nil {
		t.Fatal("evolve provenance entry carries no Details")
	}
	if entry.Details.PredecessorID != resp.ID {
		t.Errorf("PredecessorID = %q, want %q", entry.Details.PredecessorID, resp.ID)
	}
	if entry.Details.Reason != "" {
		t.Errorf("Reason = %q, want empty — no reason was given", entry.Details.Reason)
	}
	if entry.Details.EffectiveAt == nil {
		t.Error("EffectiveAt is absent — evolve always has a boundary, defaulted to the evolve moment")
	}
}

// TestWrite_ProvenanceHasNoDetails pins the other half: a plain create records
// no details, and must not gain an empty ones.
func TestWrite_ProvenanceHasNoDetails(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	resp, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault: "test", Concept: "Plain", Content: "nothing changed here",
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	entry := waitForProvenance(t, eng, "test", resp.ID, "create")
	if entry.Details != nil {
		t.Errorf("create entry carries Details %+v, want nil", entry.Details)
	}
}
