package engine

import (
	"context"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// TestAsOf_ReadsEvolvedPredecessor is the bitemporal headline guarantee:
// evolve SUPERSEDES a fact (soft-delete = "no longer current"), it does not
// ERASE it. An explicit historical query — as_of a timestamp inside the
// predecessor's validity window, or include_invalid — must still see it, while
// DEFAULT recall must still lead with the successor and never name the
// predecessor.
//
// Evolve's own code says so (engine.go EvolveAt: "Supersede = soft-delete
// (hidden from the present) + ValidUntil stamp (re-opens the record for as_of
// time-travel only)"), and muninn_guide promises it to agents. Before the fix
// the phase-6 lifecycle cut dropped soft-deleted engrams unconditionally,
// BEFORE and INDEPENDENT of the valid-time gate, so the promise was false.
//
// Control (already pinned by TestRecall_ValidTimeGate): forget(not_true_since)
// stamps ValidUntil WITHOUT soft-deleting, and as_of/include_invalid see that
// record fine — which is what isolates the lifecycle cut, not the validity
// axis, as the defect.
func TestAsOf_ReadsEvolvedPredecessor(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	const vault = "asof-history"

	// The predecessor was true for a past interval and was evolved one hour
	// ago, so its window is [-3h, -1h) and "two hours ago" sits inside it.
	createdAt := time.Now().Add(-3 * time.Hour).UTC()
	oldResp, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:     vault,
		Concept:   "asof history subject",
		Content:   "asof history probe content first revision",
		CreatedAt: &createdAt,
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	oldID := oldResp.ID

	effectiveAt := time.Now().Add(-time.Hour).UTC()
	newULID, err := eng.EvolveAt(ctx, vault, oldID,
		"asof history probe content second revision", "revision", nil, "", nil, nil, effectiveAt)
	if err != nil {
		t.Fatalf("EvolveAt: %v", err)
	}
	newID := newULID.String()

	awaitFTS(t, eng)

	// Bookkeeping precondition: the predecessor is soft-deleted AND carries a
	// closed ValidUntil at the evolve boundary. If this fails the rest of the
	// test is meaningless.
	ws := eng.store.ResolveVaultPrefix(vault)
	oldULID, err := storage.ParseULID(oldID)
	if err != nil {
		t.Fatalf("ParseULID: %v", err)
	}
	stored, err := eng.store.GetEngram(ctx, ws, oldULID)
	if err != nil || stored == nil {
		t.Fatalf("GetEngram(predecessor): %v", err)
	}
	if stored.State != storage.StateSoftDeleted {
		t.Fatalf("precondition: predecessor state = %v, want soft-deleted", stored.State)
	}
	if !stored.ValidUntil.Equal(effectiveAt) {
		t.Fatalf("precondition: predecessor ValidUntil = %v, want %v", stored.ValidUntil, effectiveAt)
	}

	baseReq := func() *mbp.ActivateRequest {
		return &mbp.ActivateRequest{
			Vault:      vault,
			Context:    []string{"asof history probe content"},
			MaxResults: 10,
			Threshold:  0.0,
		}
	}

	// (a) as_of INSIDE the predecessor's window returns the predecessor.
	asOf := time.Now().Add(-2 * time.Hour).UTC()
	reqAsOf := baseReq()
	reqAsOf.AsOf = &asOf
	got := activateIDs(t, eng, reqAsOf)
	if _, ok := got[oldID]; !ok {
		t.Error("as_of inside the predecessor's validity window did not return the evolved predecessor — " +
			"soft-delete erased history instead of demoting it")
	}
	if _, ok := got[newID]; ok {
		t.Error("as_of returned the successor, whose ValidFrom is after T")
	}

	// (b) include_invalid returns the predecessor, annotated expired.
	reqInv := baseReq()
	reqInv.IncludeInvalid = true
	got = activateIDs(t, eng, reqInv)
	item, ok := got[oldID]
	if !ok {
		t.Fatal("include_invalid did not return the evolved predecessor")
	}
	if !item.Expired {
		t.Error("include_invalid result for the evolved predecessor lacks expired=true")
	}
	if item.ValidUntil == 0 {
		t.Error("include_invalid result for the evolved predecessor lacks valid_until")
	}

	// (c) DEFAULT recall never names the predecessor, and the successor leads.
	got = activateIDs(t, eng, baseReq())
	if _, ok := got[oldID]; ok {
		t.Error("default recall returned the superseded predecessor — soft-delete must still mean 'not current'")
	}
	if _, ok := got[newID]; !ok {
		t.Error("default recall dropped the current fact")
	}
	resp, err := eng.Activate(ctx, baseReq())
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if len(resp.Activations) == 0 || resp.Activations[0].ID != newID {
		t.Errorf("default recall must lead with the current fact; got %+v", resp.Activations)
	}
}

// TestAsOf_SoftDeletedByForgetStaysHidden guards the blast radius of the fix.
// A plain (non-supersession) soft-delete carries NO ValidUntil stamp, so its
// window is open: as_of a time inside that open window must not resurrect a
// forgotten memory into a historical view as a CURRENT fact of that view...
// except that history is exactly what include_invalid asks for. This test pins
// the part that must not move: default recall never returns it, and an as_of
// BEFORE the memory existed never returns it.
func TestAsOf_SoftDeletedByForgetStaysHidden(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	const vault = "asof-forgotten"

	createdAt := time.Now().Add(-time.Hour).UTC()
	resp, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:     vault,
		Concept:   "asof forgotten subject",
		Content:   "asof forgotten probe content",
		CreatedAt: &createdAt,
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if _, err := eng.Forget(ctx, &mbp.ForgetRequest{Vault: vault, ID: resp.ID, Hard: false}); err != nil {
		t.Fatalf("Forget: %v", err)
	}
	awaitFTS(t, eng)

	baseReq := func() *mbp.ActivateRequest {
		return &mbp.ActivateRequest{
			Vault:      vault,
			Context:    []string{"asof forgotten probe content"},
			MaxResults: 10,
			Threshold:  0.0,
		}
	}

	if got := activateIDs(t, eng, baseReq()); len(got) != 0 {
		t.Errorf("default recall returned a soft-deleted engram: %v", got)
	}

	// as_of BEFORE the memory existed: the view predates it entirely.
	before := createdAt.Add(-time.Hour)
	reqBefore := baseReq()
	reqBefore.AsOf = &before
	if _, ok := activateIDs(t, eng, reqBefore)[resp.ID]; ok {
		t.Error("as_of before the engram's ValidFrom returned it")
	}
}
