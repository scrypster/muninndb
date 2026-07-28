package engine

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// writeValidityEngram writes an engram with optional validity bounds and
// returns its ID string.
func writeValidityEngram(t *testing.T, eng *Engine, vault, concept, content string, validFrom, validUntil *time.Time) string {
	t.Helper()
	resp, err := eng.Write(context.Background(), &mbp.WriteRequest{
		Vault:      vault,
		Concept:    concept,
		Content:    content,
		ValidFrom:  validFrom,
		ValidUntil: validUntil,
	})
	if err != nil {
		t.Fatalf("Write(%q): %v", concept, err)
	}
	return resp.ID
}

func activateIDs(t *testing.T, eng *Engine, req *mbp.ActivateRequest) map[string]*mbp.ActivationItem {
	t.Helper()
	resp, err := eng.Activate(context.Background(), req)
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}
	out := make(map[string]*mbp.ActivationItem, len(resp.Activations))
	for i := range resp.Activations {
		out[resp.Activations[i].ID] = &resp.Activations[i]
	}
	return out
}

// TestRecall_ValidTimeGate is the recall-side behavior matrix:
//   - default recall DROPS a fact with a closed ValidUntil <= now (COG-19)
//   - default recall KEEPS a fact with a future ValidFrom (not-yet-valid != expired)
//   - as_of=T RETURNS the expired fact whose window covers T (and drops the
//     fact whose window does not cover T)
//   - include_invalid returns the expired fact, annotated expired=true
func TestRecall_ValidTimeGate(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()

	const vault = "validtime-recall"
	now := time.Now().UTC()
	pastFrom := now.Add(-48 * time.Hour)
	pastUntil := now.Add(-24 * time.Hour) // closed window: expired
	futureFrom := now.Add(24 * time.Hour)

	expiredID := writeValidityEngram(t, eng, vault,
		"validity gate expired", "validity gate test content expired fact", &pastFrom, &pastUntil)
	currentID := writeValidityEngram(t, eng, vault,
		"validity gate current", "validity gate test content current fact", nil, nil)
	futureID := writeValidityEngram(t, eng, vault,
		"validity gate future", "validity gate test content future fact", &futureFrom, nil)

	awaitFTS(t, eng)

	baseReq := func() *mbp.ActivateRequest {
		return &mbp.ActivateRequest{
			Vault:      vault,
			Context:    []string{"validity gate test content"},
			MaxResults: 10,
			Threshold:  0.0,
		}
	}

	// Default: expired dropped, current + future kept.
	got := activateIDs(t, eng, baseReq())
	if _, ok := got[expiredID]; ok {
		t.Error("default recall returned an engram with ValidUntil <= now (COG-19 violation)")
	}
	if _, ok := got[currentID]; !ok {
		t.Error("default recall dropped the current fact")
	}
	if _, ok := got[futureID]; !ok {
		t.Error("default recall dropped a future-ValidFrom fact — only EXPIRED facts are gated")
	}

	// as_of inside the expired fact's window: expired returned, future dropped.
	asOf := now.Add(-36 * time.Hour)
	reqAsOf := baseReq()
	reqAsOf.AsOf = &asOf
	got = activateIDs(t, eng, reqAsOf)
	if _, ok := got[expiredID]; !ok {
		t.Error("as_of=T did not return the fact whose validity window covers T")
	}
	if _, ok := got[futureID]; ok {
		t.Error("as_of=T returned a fact whose ValidFrom is after T")
	}
	// The current fact was created ~now, so its window [CreatedAt, open) does
	// not cover T 36h ago — the full interval check applies under as_of.
	if _, ok := got[currentID]; ok {
		t.Error("as_of=T returned a fact created after T (interval check not applied)")
	}

	// include_invalid: everything back, expired annotated.
	reqInv := baseReq()
	reqInv.IncludeInvalid = true
	got = activateIDs(t, eng, reqInv)
	item, ok := got[expiredID]
	if !ok {
		t.Fatal("include_invalid did not return the expired fact")
	}
	if !item.Expired {
		t.Error("include_invalid result for expired fact lacks expired=true annotation")
	}
	if item.ValidUntil == 0 {
		t.Error("include_invalid result for expired fact lacks valid_until annotation")
	}
	if cur, ok := got[currentID]; ok && cur.Expired {
		t.Error("current fact wrongly annotated expired")
	}
}

// TestEvolveAt_StampsPredecessorValidUntil verifies evolve's atomic batch stamps
// old.ValidUntil = new.ValidFrom while KEEPING the soft-delete, and that the
// successor carries ValidFrom = effectiveAt.
func TestEvolveAt_StampsPredecessorValidUntil(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()
	const vault = "validtime-evolve"

	// Predecessor was true from 5h ago; the change took effect 2h ago (after the
	// fact's start — a real interval, not an inverted window).
	createdAt := time.Now().Add(-5 * time.Hour).UTC()
	resp, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault: vault, Concept: "runway", Content: "runway is 12 months (May figure)", CreatedAt: &createdAt,
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	effectiveAt := time.Now().Add(-2 * time.Hour).UTC()
	newID, err := eng.EvolveAt(ctx, vault, resp.ID, "runway is 9 months (July figure)", "updated figure", nil, "", effectiveAt)
	if err != nil {
		t.Fatalf("EvolveAt: %v", err)
	}

	ws := eng.store.ResolveVaultPrefix(vault)
	oldULID, _ := storage.ParseULID(resp.ID)
	old, err := eng.store.GetEngram(ctx, ws, oldULID)
	if err != nil {
		t.Fatalf("GetEngram old: %v", err)
	}
	if old.State != storage.StateSoftDeleted {
		t.Errorf("old.State = %v, want StateSoftDeleted (evolve KEEPS soft-deleting)", old.State)
	}
	if !old.ValidUntil.Equal(effectiveAt) {
		t.Errorf("old.ValidUntil = %v, want effectiveAt %v (predecessor stamp missing)", old.ValidUntil, effectiveAt)
	}

	newEng, err := eng.store.GetEngram(ctx, ws, newID)
	if err != nil {
		t.Fatalf("GetEngram new: %v", err)
	}
	if !newEng.ValidFrom.Equal(effectiveAt) {
		t.Errorf("new.ValidFrom = %v, want effectiveAt %v (windows must meet exactly)", newEng.ValidFrom, effectiveAt)
	}
	if !newEng.ValidUntil.IsZero() {
		t.Errorf("new.ValidUntil = %v, want open", newEng.ValidUntil)
	}
}

// TestEvolve_DefaultStampsNow verifies the no-effective_at path stamps the
// predecessor with the evolve time (== successor CreatedAt).
func TestEvolve_DefaultStampsNow(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()
	const vault = "validtime-evolve-default"

	resp, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault: vault, Concept: "status", Content: "status alpha",
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	newID, err := eng.Evolve(ctx, vault, resp.ID, "status beta", "changed", nil, "")
	if err != nil {
		t.Fatalf("Evolve: %v", err)
	}

	ws := eng.store.ResolveVaultPrefix(vault)
	oldULID, _ := storage.ParseULID(resp.ID)
	old, _ := eng.store.GetEngram(ctx, ws, oldULID)
	newEng, _ := eng.store.GetEngram(ctx, ws, newID)
	if old.ValidUntil.IsZero() {
		t.Fatal("old.ValidUntil is zero — default evolve did not stamp the predecessor")
	}
	if !old.ValidUntil.Equal(newEng.CreatedAt) {
		t.Errorf("old.ValidUntil = %v, want new.CreatedAt %v", old.ValidUntil, newEng.CreatedAt)
	}
}

// TestLink_SupersedesStampsTargetValidUntil verifies an explicit RelSupersedes
// link stamps the target's ValidUntil (write-time closure), skipping targets
// whose window is already closed.
func TestLink_SupersedesStampsTargetValidUntil(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()
	const vault = "validtime-link"

	oldResp, err := eng.Write(ctx, &mbp.WriteRequest{Vault: vault, Concept: "old fact", Content: "the old fact"})
	if err != nil {
		t.Fatalf("Write old: %v", err)
	}
	newResp, err := eng.Write(ctx, &mbp.WriteRequest{Vault: vault, Concept: "new fact", Content: "the new fact"})
	if err != nil {
		t.Fatalf("Write new: %v", err)
	}

	before := time.Now()
	if _, err := eng.Link(ctx, &mbp.LinkRequest{
		Vault: vault, SourceID: newResp.ID, TargetID: oldResp.ID,
		RelType: uint16(storage.RelSupersedes), Weight: 1.0,
	}); err != nil {
		t.Fatalf("Link: %v", err)
	}

	ws := eng.store.ResolveVaultPrefix(vault)
	oldULID, _ := storage.ParseULID(oldResp.ID)
	old, err := eng.store.GetEngram(ctx, ws, oldULID)
	if err != nil {
		t.Fatalf("GetEngram: %v", err)
	}
	if old.ValidUntil.IsZero() {
		t.Fatal("RelSupersedes link did not stamp target.ValidUntil")
	}
	if old.ValidUntil.Before(before) || old.ValidUntil.After(time.Now().Add(time.Second)) {
		t.Errorf("target.ValidUntil = %v, want ~now", old.ValidUntil)
	}
	if old.State == storage.StateSoftDeleted {
		t.Error("RelSupersedes link soft-deleted the target — stamp, never delete")
	}
	firstStamp := old.ValidUntil

	// A second supersedes link must not move the already-closed window.
	thirdResp, err := eng.Write(ctx, &mbp.WriteRequest{Vault: vault, Concept: "third fact", Content: "the third fact"})
	if err != nil {
		t.Fatalf("Write third: %v", err)
	}
	if _, err := eng.Link(ctx, &mbp.LinkRequest{
		Vault: vault, SourceID: thirdResp.ID, TargetID: oldResp.ID,
		RelType: uint16(storage.RelSupersedes), Weight: 1.0,
	}); err != nil {
		t.Fatalf("Link (second): %v", err)
	}
	old, _ = eng.store.GetEngram(ctx, ws, oldULID)
	if !old.ValidUntil.Equal(firstStamp) {
		t.Errorf("second supersedes link moved ValidUntil from %v to %v — already-closed windows are preserved", firstStamp, old.ValidUntil)
	}
}

// TestForget_NotTrueSince_StampsInsteadOfDelete verifies forget with
// not_true_since invalidates on the valid-time axis: ValidUntil stamped,
// engram stays active (COG-19: never a delete).
func TestForget_NotTrueSince_StampsInsteadOfDelete(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()
	const vault = "validtime-forget"

	// The fact was true for a past interval: created two hours ago, stopped being
	// true one hour ago. (not_true_since must be after the fact's start — an
	// invalidation before the fact existed is an inverted window, rejected.)
	createdAt := time.Now().Add(-2 * time.Hour).UTC()
	resp, err := eng.Write(ctx, &mbp.WriteRequest{Vault: vault, Concept: "belief", Content: "a belief that stopped being true", CreatedAt: &createdAt})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	notTrueSince := time.Now().Add(-time.Hour).UTC()
	if _, err := eng.Forget(ctx, &mbp.ForgetRequest{
		Vault: vault, ID: resp.ID, NotTrueSince: &notTrueSince,
	}); err != nil {
		t.Fatalf("Forget(not_true_since): %v", err)
	}

	ws := eng.store.ResolveVaultPrefix(vault)
	id, _ := storage.ParseULID(resp.ID)
	got, err := eng.store.GetEngram(ctx, ws, id)
	if err != nil {
		t.Fatalf("GetEngram: %v", err)
	}
	if got.State == storage.StateSoftDeleted {
		t.Error("forget(not_true_since) soft-deleted the engram — must stamp instead")
	}
	if !got.ValidUntil.Equal(notTrueSince) {
		t.Errorf("ValidUntil = %v, want %v", got.ValidUntil, notTrueSince)
	}
}

// TestRestore_ClearsValidUntil verifies muninn_restore re-opens the validity
// window — otherwise a restore is a recall no-op.
func TestRestore_ClearsValidUntil(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()
	const vault = "validtime-restore"

	resp, err := eng.Write(ctx, &mbp.WriteRequest{Vault: vault, Concept: "restorable", Content: "restorable content"})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	// Evolve stamps + soft-deletes the predecessor.
	if _, err := eng.Evolve(ctx, vault, resp.ID, "restorable content v2", "update", nil, ""); err != nil {
		t.Fatalf("Evolve: %v", err)
	}

	restored, err := eng.Restore(ctx, vault, resp.ID)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if !restored.ValidUntil.IsZero() {
		t.Errorf("Restore returned ValidUntil = %v, want zero", restored.ValidUntil)
	}

	ws := eng.store.ResolveVaultPrefix(vault)
	id, _ := storage.ParseULID(resp.ID)
	got, _ := eng.store.GetEngram(ctx, ws, id)
	if got.State != storage.StateActive {
		t.Errorf("State = %v, want StateActive", got.State)
	}
	if !got.ValidUntil.IsZero() {
		t.Errorf("ValidUntil after restore = %v, want zero (stamp must be cleared)", got.ValidUntil)
	}
}

// TestWrite_ContentHashDedup_ExpiredHitWritesNewEngram is the COG-13 valid-time
// fix: re-remembering content identical to an EXPIRED engram must NOT reinforce
// the expired record — two same-content facts with disjoint windows are two facts.
func TestWrite_ContentHashDedup_ExpiredHitWritesNewEngram(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()
	const vault = "validtime-dedup"
	const content = "the office is in building 7"

	// Backdated so the later not_true_since (~now) is after the fact's start.
	firstCreated := time.Now().Add(-time.Hour).UTC()
	first, err := eng.Write(ctx, &mbp.WriteRequest{Vault: vault, Concept: "office", Content: content, CreatedAt: &firstCreated})
	if err != nil {
		t.Fatalf("Write first: %v", err)
	}

	// Sanity: while current, identical content dedups to the same ID.
	dup, err := eng.Write(ctx, &mbp.WriteRequest{Vault: vault, Concept: "office", Content: content})
	if err != nil {
		t.Fatalf("Write dup: %v", err)
	}
	if dup.ID != first.ID {
		t.Fatalf("pre-expiry dedup broken: got %s, want %s", dup.ID, first.ID)
	}

	// Expire the first fact on the valid-time axis.
	notTrueSince := time.Now().Add(-time.Minute).UTC()
	if _, err := eng.Forget(ctx, &mbp.ForgetRequest{Vault: vault, ID: first.ID, NotTrueSince: &notTrueSince}); err != nil {
		t.Fatalf("Forget(not_true_since): %v", err)
	}

	ws := eng.store.ResolveVaultPrefix(vault)
	firstULID, _ := storage.ParseULID(first.ID)
	beforeMeta, _ := eng.store.GetMetadata(ctx, ws, []storage.ULID{firstULID})

	// Re-remember the identical content: must create a NEW engram.
	again, err := eng.Write(ctx, &mbp.WriteRequest{Vault: vault, Concept: "office", Content: content})
	if err != nil {
		t.Fatalf("Write again: %v", err)
	}
	if again.ID == first.ID {
		t.Fatal("re-remembering content identical to an EXPIRED engram reinforced the expired record instead of writing a new engram")
	}
	if again.Hint == "duplicate_content" {
		t.Error("expired-hit write still reported duplicate_content")
	}

	// The expired record must not have been reinforced.
	afterMeta, _ := eng.store.GetMetadata(ctx, ws, []storage.ULID{firstULID})
	if beforeMeta[0] != nil && afterMeta[0] != nil && afterMeta[0].AccessCount != beforeMeta[0].AccessCount {
		t.Errorf("expired engram AccessCount changed %d -> %d — expired records must not be reinforced by dedup",
			beforeMeta[0].AccessCount, afterMeta[0].AccessCount)
	}

	// And the hash now points at the new, current engram.
	third, err := eng.Write(ctx, &mbp.WriteRequest{Vault: vault, Concept: "office", Content: content})
	if err != nil {
		t.Fatalf("Write third: %v", err)
	}
	if third.ID != again.ID {
		t.Errorf("content hash not repointed: third write returned %s, want %s", third.ID, again.ID)
	}
}

// TestCOG19_Pin pins invariant COG-19: invalidation is always a ValidUntil
// stamp, never a delete, and default recall never returns an engram whose
// ValidUntil <= now — including candidates injected after phase-6 (entity
// boost now gates injections itself, but supersession still injects past
// PassesMetaFilter, so the final gate must hold regardless).
func TestCOG19_Pin(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()
	const vault = "cog19-pin"

	cog19Created := time.Now().Add(-time.Hour).UTC()
	resp, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault: vault, Concept: "cog19 fact", Content: "cog19 pinned fact content", CreatedAt: &cog19Created,
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	awaitFTS(t, eng)

	// Every invalidation surface is a stamp: the record must survive it.
	nts := time.Now().Add(-time.Second).UTC()
	if _, err := eng.Forget(ctx, &mbp.ForgetRequest{Vault: vault, ID: resp.ID, NotTrueSince: &nts}); err != nil {
		t.Fatalf("Forget(not_true_since): %v", err)
	}
	ws := eng.store.ResolveVaultPrefix(vault)
	id, _ := storage.ParseULID(resp.ID)
	got, err := eng.store.GetEngram(ctx, ws, id)
	if err != nil || got == nil {
		t.Fatalf("invalidation deleted the record (err=%v) — COG-19 requires a stamp", err)
	}
	if got.State != storage.StateActive {
		t.Errorf("invalidation changed lifecycle state to %v — the valid-time axis is orthogonal to state", got.State)
	}
	if got.ValidUntil.IsZero() {
		t.Fatal("no ValidUntil stamp written")
	}

	// Default recall must never return it.
	respAct, err := eng.Activate(ctx, &mbp.ActivateRequest{
		Vault: vault, Context: []string{"cog19 pinned fact content"}, MaxResults: 10, Threshold: 0.0,
	})
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}
	for _, a := range respAct.Activations {
		if a.ID == resp.ID {
			t.Fatal("COG-19 violated: default recall returned an engram whose ValidUntil <= now")
		}
	}
}

// TestForget_NotTrueSince_RejectsEpochSentinel is the refute-pass blocking guard:
// a stamp at the Unix epoch (UnixNano()==0) would encode to raw 0 and decode as
// the ERF "open/current" sentinel — silently NOT invalidating while the handler
// reports success (a truth inversion). It must be rejected loudly.
func TestForget_NotTrueSince_RejectsEpochSentinel(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()
	const vault = "validtime-epoch"

	resp, err := eng.Write(ctx, &mbp.WriteRequest{Vault: vault, Concept: "c", Content: "content"})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	epoch := time.Unix(0, 0).UTC() // 1970 — UnixNano()==0, but IsZero()==false
	_, err = eng.Forget(ctx, &mbp.ForgetRequest{Vault: vault, ID: resp.ID, NotTrueSince: &epoch})
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("epoch not_true_since: err = %v, want ErrInvalidRequest", err)
	}
	// The fact must remain valid (NOT silently invalidated).
	ws := eng.store.ResolveVaultPrefix(vault)
	id, _ := storage.ParseULID(resp.ID)
	got, _ := eng.store.GetEngram(ctx, ws, id)
	if !got.ValidUntil.IsZero() {
		t.Error("rejected stamp must leave the fact open/current, not partially invalidated")
	}
}

// TestEvolveAt_RejectsEpochAndInverted guards the evolve.effective_at stamp path.
func TestEvolveAt_RejectsEpochAndInverted(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()
	const vault = "validtime-evolveat"

	resp, err := eng.Write(ctx, &mbp.WriteRequest{Vault: vault, Concept: "c", Content: "original"})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	epoch := time.Unix(0, 0).UTC()
	if _, err := eng.EvolveAt(ctx, vault, resp.ID, "new", "reason", nil, "", epoch); !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("epoch effective_at: err = %v, want ErrInvalidRequest", err)
	}
	// Inverted: effective_at before the predecessor's start (created ~now).
	past := time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)
	if _, err := eng.EvolveAt(ctx, vault, resp.ID, "new", "reason", nil, "", past); !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("inverted effective_at: err = %v, want ErrInvalidRequest", err)
	}
}

// TestWhereLeftOff_DropsInvalidatedActive pins the refute-pass gap fix: a fact
// invalidated via forget.not_true_since stays ACTIVE, so where_left_off (which
// bypasses the recall gate) must drop it explicitly — the session-orientation
// surface must never lead with something the user marked "no longer true".
func TestWhereLeftOff_DropsInvalidatedActive(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()
	const vault = "validtime-wlo"

	createdAt := time.Now().Add(-3 * time.Hour).UTC()
	resp, err := eng.Write(ctx, &mbp.WriteRequest{Vault: vault, Concept: "stale", Content: "no longer true fact", CreatedAt: &createdAt})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	// Also write a still-current fact so the surface has something to return.
	if _, err := eng.Write(ctx, &mbp.WriteRequest{Vault: vault, Concept: "live", Content: "still true fact"}); err != nil {
		t.Fatalf("Write current: %v", err)
	}
	notTrueSince := time.Now().Add(-1 * time.Hour).UTC()
	if _, err := eng.Forget(ctx, &mbp.ForgetRequest{Vault: vault, ID: resp.ID, NotTrueSince: &notTrueSince}); err != nil {
		t.Fatalf("Forget(not_true_since): %v", err)
	}

	got, err := eng.WhereLeftOff(ctx, vault, 10, nil)
	if err != nil {
		t.Fatalf("WhereLeftOff: %v", err)
	}
	for _, e := range got {
		if e.ID.String() == resp.ID {
			t.Error("where_left_off returned a fact marked no-longer-true (COG-19 leak)")
		}
	}
}
