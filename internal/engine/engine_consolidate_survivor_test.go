package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// Consolidate archived its own survivor and destroyed the fact.
//
// Consolidate writes the merged content as a new engram, then soft-deletes every
// input id. But Write is exact-content deduplicated: if mergedContent matches one
// of the inputs byte-for-byte — the NATURAL case, because the obvious merged text
// for a set of near-duplicates is one of them verbatim — Write returns that
// input's EXISTING id instead of creating a new engram. The archive loop then
// soft-deletes the very engram just handed back as the result.
//
// Reproduced live before this fix:
//
//	consolidate(ids=[A,B], merged_content=<A's exact content>)
//	  -> {"id":"...13WAB4","archived":["...13WAB4","...SWZ5QG"]}
//	                ^ the survivor              ^ archived anyway
//	muninn_read A  -> soft_deleted
//	muninn_read B  -> soft_deleted
//	recall(...)    -> 0 results
//
// The caller used the merge tool exactly as documented and lost the fact, with a
// success response naming an id that no longer resolves. Same class as the
// contradiction data loss: doing the right thing destroys data, and the response
// asserts something that is not true.
//
// The fix keeps both behaviours (dedup is correct; archiving inputs is correct)
// and removes the contradiction between them: an input that IS the merged result
// is never archived.
func TestConsolidate_NeverArchivesItsOwnSurvivor(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()
	const vault = "default"
	const shared = "Priya Raman is the tech lead for the scheduler team."

	a, err := eng.Write(ctx, &mbp.WriteRequest{Vault: vault, Content: shared, Concept: "scheduler tech lead"})
	if err != nil {
		t.Fatalf("write A: %v", err)
	}
	b, err := eng.Write(ctx, &mbp.WriteRequest{Vault: vault, Content: "The scheduler team tech lead is Priya Raman.", Concept: "scheduler lead"})
	if err != nil {
		t.Fatalf("write B: %v", err)
	}

	// The natural merged text for a set of near-duplicates is one of them
	// verbatim — which is exactly what trips the dedup path.
	res, err := eng.Consolidate(ctx, vault, []string{a.ID, b.ID}, shared)
	if err != nil {
		t.Fatalf("consolidate: %v", err)
	}
	merged := res.MergedID.String()

	for _, id := range res.Archived {
		if id == merged {
			t.Errorf("consolidate archived its own survivor %s: the merged write hit exact-content "+
				"dedup and returned an existing input id, which the archive loop then soft-deleted. "+
				"archived=%v", merged, res.Archived)
		}
	}

	// The survivor must still be readable and live — the response named it as the
	// result, so a response that resolves to a soft-deleted engram is a lie.
	got, err := eng.GetEngram(ctx, vault, res.MergedID)
	if err != nil {
		t.Fatalf("read merged survivor %s: %v (consolidate returned an id that does not resolve)", merged, err)
	}
	if got == nil {
		t.Fatalf("merged survivor %s does not resolve", merged)
	}
	if strings.EqualFold(got.State.String(), "soft_deleted") {
		t.Errorf("merged survivor %s is soft_deleted — the fact was destroyed by the merge tool", merged)
	}
}

// The guard must hold for CASE-INSENSITIVE id forms too: ULID parsing accepts
// lowercase and Forget accepts lowercase, so a string comparison let a
// lowercase input slip past the guard and archive the survivor anyway
// (adversarial review of #754, finding 7).
func TestConsolidate_SurvivorGuardIsCaseInsensitive(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()
	const vault = "default"
	const shared = "Priya Raman is the tech lead for the scheduler team."

	a, err := eng.Write(ctx, &mbp.WriteRequest{Vault: vault, Content: shared, Concept: "lead"})
	if err != nil {
		t.Fatalf("write A: %v", err)
	}
	b, err := eng.Write(ctx, &mbp.WriteRequest{Vault: vault, Content: "The scheduler team tech lead is Priya Raman.", Concept: "lead2"})
	if err != nil {
		t.Fatalf("write B: %v", err)
	}

	res, err := eng.Consolidate(ctx, vault, []string{strings.ToLower(a.ID), b.ID}, shared)
	if err != nil {
		t.Fatalf("consolidate: %v", err)
	}
	merged := res.MergedID.String()
	for _, id := range res.Archived {
		if strings.EqualFold(id, merged) {
			t.Errorf("lowercase input id bypassed the survivor guard: archived=%v merged=%s", res.Archived, merged)
		}
	}
	got, err := eng.GetEngram(ctx, vault, res.MergedID)
	if err != nil || got == nil {
		t.Fatalf("survivor unreadable after lowercase-id consolidate: %v", err)
	}
	if strings.EqualFold(got.State.String(), "soft_deleted") {
		t.Error("survivor soft-deleted via the lowercase bypass")
	}
}

// The ordinary case must keep working: when the merged content is genuinely new,
// a new engram is created and every input is archived. This pins that the fix
// does not smuggle in "stop archiving inputs".
func TestConsolidate_DistinctMergedContentStillArchivesAllInputs(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()
	const vault = "default"

	a, err := eng.Write(ctx, &mbp.WriteRequest{Vault: vault, Content: "The cache holds 64 entries.", Concept: "cache size"})
	if err != nil {
		t.Fatalf("write A: %v", err)
	}
	b, err := eng.Write(ctx, &mbp.WriteRequest{Vault: vault, Content: "Cache capacity is sixty-four.", Concept: "cache capacity"})
	if err != nil {
		t.Fatalf("write B: %v", err)
	}

	res, err := eng.Consolidate(ctx, vault, []string{a.ID, b.ID},
		"The cache holds 64 entries (capacity confirmed).")
	if err != nil {
		t.Fatalf("consolidate: %v", err)
	}
	merged := res.MergedID.String()
	if merged == a.ID || merged == b.ID {
		t.Fatalf("distinct merged content should create a NEW engram, got an input id %s", merged)
	}
	if len(res.Archived) != 2 {
		t.Errorf("both inputs must be archived when the merge is genuinely new; archived=%v", res.Archived)
	}
}
