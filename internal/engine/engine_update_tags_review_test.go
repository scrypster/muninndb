package engine

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/storage/keys"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// TestUpdateTags_EvolvePredecessorKeepsPostings pins the trash-versus-history
// gate in Engine.UpdateTags (#720 review, finding 1).
//
// Evolve soft-deletes its predecessor AND stamps a closed ValidUntil in one
// batch, and — unlike Forget — deliberately leaves the predecessor's FTS
// postings in place, because those postings are what a query phrased in the OLD
// wording still matches; COG-28 then resolves that evidence forward to the chain
// head. activation.PassesLifecycle admits exactly this record under as_of /
// include_invalid.
//
// The earlier gate keyed on State alone, so a retag of that predecessor took the
// DeleteEngram arm and destroyed the postings permanently — recoverable only by
// reindex-fts. A metadata edit silently removing the keyword path to a
// superseded fact is the silently-wrong class: nothing errors, nothing warns,
// and the loss only shows up as an as_of query that used to work.
//
// RED (gate restored to `state == StateSoftDeleted || state == StateArchived`):
//
//	predecessor FTS hits for "brachiosaur" after retag = 0, want 1
func TestUpdateTags_EvolvePredecessorKeepsPostings(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()
	const vault = "retag-evolve-predecessor"

	resp, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:   vault,
		Concept: "capacity note",
		Content: "the brachiosaur cluster runs hot during the nightly compaction",
		// Tags with NO shared token: fts.Search is disjunctive over the query's
		// terms, so "owner:ops" → "owner:platform" would still match on the
		// retained "owner" token and the removal assertion below would be vacuous.
		Tags: []string{"ceratopsian"},
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	predID, err := storage.ParseULID(resp.ID)
	if err != nil {
		t.Fatalf("ParseULID(%q): %v", resp.ID, err)
	}
	ws := eng.store.ResolveVaultPrefix(vault)
	awaitFTS(t, eng)

	if n := ftsHitCount(t, eng, ws, "brachiosaur", predID); n != 1 {
		t.Fatalf("precondition: predecessor FTS hits before evolve = %d, want 1", n)
	}

	if _, err := eng.Evolve(ctx, vault, resp.ID,
		"the sauropod cluster runs hot during the nightly compaction",
		"renamed the cluster", nil, ""); err != nil {
		t.Fatalf("Evolve: %v", err)
	}
	awaitFTS(t, eng)

	// Precondition on the mechanism this test depends on: evolve must produce
	// the supersession signature (soft-deleted + CLOSED stamp) and must NOT have
	// de-indexed the predecessor. If either changes, the assertion below is
	// testing nothing and should fail loudly here instead.
	pred, err := eng.store.GetEngram(ctx, ws, predID)
	if err != nil {
		t.Fatalf("GetEngram(predecessor): %v", err)
	}
	if pred.State != storage.StateSoftDeleted {
		t.Fatalf("precondition: predecessor state = %v, want soft-deleted", pred.State)
	}
	if pred.ValidUntil.IsZero() {
		t.Fatalf("precondition: predecessor ValidUntil is open, want a closed supersession stamp")
	}
	if n := ftsHitCount(t, eng, ws, "brachiosaur", predID); n != 1 {
		t.Fatalf("precondition: evolve itself dropped the predecessor's postings (hits=%d, want 1) — "+
			"this test can no longer detect the retag regression", n)
	}

	// The edit under test: a pure metadata change on the predecessor.
	if err := eng.UpdateTags(ctx, vault, predID, []string{"hadrosaur"}); err != nil {
		t.Fatalf("UpdateTags(predecessor): %v", err)
	}

	if n := ftsHitCount(t, eng, ws, "brachiosaur", predID); n != 1 {
		t.Errorf("predecessor FTS hits for a CONTENT term after retag = %d, want 1 — "+
			"retagging superseded history destroyed the keyword path as_of is contracted to reach", n)
	}
	if n := ftsHitCount(t, eng, ws, "hadrosaur", predID); n != 1 {
		t.Errorf("predecessor FTS hits for the NEW tag after retag = %d, want 1", n)
	}
	if n := ftsHitCount(t, eng, ws, "ceratopsian", predID); n != 0 {
		t.Errorf("predecessor FTS hits for the REMOVED tag after retag = %d, want 0", n)
	}
}

// TestUpdateTags_TrashIsStillDeIndexed is the negative control for the gate
// above: narrowing the drop to open-stamp soft-deletes must not stop dropping
// them. A plain muninn_forget leaves ValidUntil open — trash, not history — and
// a retag of that record must not put it back in the candidate pool.
func TestUpdateTags_TrashIsStillDeIndexed(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()
	const vault = "retag-trash"

	resp, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:   vault,
		Concept: "scratch note",
		Content: "the pterodactyl draft was abandoned",
		Tags:    []string{"draft"},
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	id, err := storage.ParseULID(resp.ID)
	if err != nil {
		t.Fatalf("ParseULID: %v", err)
	}
	ws := eng.store.ResolveVaultPrefix(vault)
	awaitFTS(t, eng)

	if _, err := eng.Forget(ctx, &mbp.ForgetRequest{Vault: vault, ID: resp.ID}); err != nil {
		t.Fatalf("Forget: %v", err)
	}
	got, err := eng.store.GetEngram(ctx, ws, id)
	if err != nil {
		t.Fatalf("GetEngram after forget: %v", err)
	}
	if !got.ValidUntil.IsZero() {
		t.Fatalf("precondition: plain forget produced a CLOSED stamp, so this is no longer the trash case")
	}

	if err := eng.UpdateTags(ctx, vault, id, []string{"triaged"}); err != nil {
		t.Fatalf("UpdateTags: %v", err)
	}
	if n := ftsHitCount(t, eng, ws, "triaged", id); n != 0 {
		t.Errorf("FTS hits for the new tag on FORGOTTEN trash = %d, want 0 — a retag must not resurrect it", n)
	}
	if n := ftsHitCount(t, eng, ws, "pterodactyl", id); n != 0 {
		t.Errorf("FTS hits for a content term on FORGOTTEN trash = %d, want 0", n)
	}
}

// TestUpdateTags_RetiresStaleTagIndexEntries pins the 0x0C/0x2C removal diff
// (#720 review, finding 2).
//
// The orphans cannot produce a false POSITIVE — phase-6's passesMetaFilter
// re-checks the real tag on the engram — but the 0x0C/0x2C indexes are candidate
// SEEDS drawn against a bounded budget, so every orphan burns a slot that a
// genuinely matching engram needed. That is a false negative at the pool
// boundary. Due-date tags are the workload this tool's own description and
// muninn_guide recommend, and a recurring task is retagged over and over, so the
// residual concentrates on exactly the intended use case.
//
// RED (removal block deleted): raw-tag range scan for "due" returns 6 entries
// for a single engram carrying ONE due tag, instead of 1.
func TestUpdateTags_RetiresStaleTagIndexEntries(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()
	const vault = "retag-orphans"

	resp, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:   vault,
		Concept: "recurring standup",
		Content: "weekly sync",
		Tags:    []string{"due:2026-01-05"},
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	id, err := storage.ParseULID(resp.ID)
	if err != nil {
		t.Fatalf("ParseULID: %v", err)
	}
	ws := eng.store.ResolveVaultPrefix(vault)

	// Roll the due date forward the way a recurring task actually behaves.
	dates := []string{"2026-01-12", "2026-01-19", "2026-01-26", "2026-02-02", "2026-02-09"}
	for _, d := range dates {
		if err := eng.UpdateTags(ctx, vault, id, []string{"due:" + d}); err != nil {
			t.Fatalf("UpdateTags(due:%s): %v", d, err)
		}
	}

	// One tag on the record ⇒ exactly one 0x2C seed entry, not one per retag.
	lower, upper := keys.RawTagRangeBound(ws, keys.Hash("due"), "gte", []byte(""))
	ids, err := eng.store.ScanRawTagRange(ctx, ws, "due", lower, upper, 0)
	if err != nil {
		t.Fatalf("ScanRawTagRange: %v", err)
	}
	mine := 0
	for _, got := range ids {
		if got == id {
			mine++
		}
	}
	if mine != 1 {
		t.Errorf("0x2C raw-tag seed entries for one engram after %d retags = %d, want 1 — "+
			"each orphan burns a candidate seed slot the tag-filter budget owes a real match",
			len(dates), mine)
	}

	// The surviving entry must be the CURRENT due date, not a stale one: a
	// removal pass that deleted the wrong side would also produce a count of 1.
	lowerCur, upperCur := keys.RawTagRangeBound(ws, keys.Hash("due"), "gte", []byte("2026-02-09"))
	curIDs, err := eng.store.ScanRawTagRange(ctx, ws, "due", lowerCur, upperCur, 0)
	if err != nil {
		t.Fatalf("ScanRawTagRange(current): %v", err)
	}
	found := false
	for _, got := range curIDs {
		if got == id {
			found = true
		}
	}
	if !found {
		t.Errorf("the surviving 0x2C entry is not the current due date — the removal diff kept the wrong side")
	}
}

// TestUpdateTags_ConcurrentRetagsDoNotStrandPostings pins the serialization
// (#720 review, finding 3).
//
// Storage being atomic is not enough: the FTS delta is derived from the tag set
// read BEFORE the write, so two concurrent retags could interleave
// read/write/reindex and leave the loser's postings behind. Measured at 36 of 40
// trials before the fix, with no artificial delay — and because df_t is
// corpus-wide, the double-decrement also moved an uninvolved third engram's
// full-text score by 6.5%.
//
// Cooperative agents retagging the same memory concurrently is ordinary, which
// is why this is a test rather than a documented residual.
//
// RED (engine.UpdateTags's LockEngram/UpdateTagsLocked pair reverted to a plain
// store.UpdateTags call): fails under -race with postings for a tag the record
// does not carry.
func TestUpdateTags_ConcurrentRetagsDoNotStrandPostings(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()
	const vault = "retag-concurrent"

	resp, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:   vault,
		Concept: "contended note",
		Content: "the ammonite record is edited by several agents at once",
		Tags:    []string{"initial"},
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	id, err := storage.ParseULID(resp.ID)
	if err != nil {
		t.Fatalf("ParseULID: %v", err)
	}
	ws := eng.store.ResolveVaultPrefix(vault)
	awaitFTS(t, eng)

	const writers = 8
	candidates := make([]string, writers)
	for i := range candidates {
		candidates[i] = fmt.Sprintf("writer%d", i)
	}

	var wg sync.WaitGroup
	for _, tag := range candidates {
		wg.Add(1)
		go func(tag string) {
			defer wg.Done()
			if err := eng.UpdateTags(ctx, vault, id, []string{tag}); err != nil {
				t.Errorf("UpdateTags(%s): %v", tag, err)
			}
		}(tag)
	}
	wg.Wait()

	// Whichever writer landed last, the index must agree with the RECORD: the
	// winner's tag present exactly once, every loser's tag absent. A stranded
	// posting means recall scores this engram on a tag it does not have.
	got, err := eng.store.GetEngram(ctx, ws, id)
	if err != nil {
		t.Fatalf("GetEngram: %v", err)
	}
	if len(got.Tags) != 1 {
		t.Fatalf("record tags after concurrent retags = %v, want exactly one", got.Tags)
	}
	winner := got.Tags[0]

	for _, tag := range candidates {
		want := 0
		if tag == winner {
			want = 1
		}
		if n := ftsHitCount(t, eng, ws, tag, id); n != want {
			t.Errorf("FTS hits for %q = %d, want %d (record carries %q) — "+
				"a concurrent retag stranded postings the record does not back", tag, n, want, winner)
		}
	}
	// The original tag is a loser too, and content must survive regardless.
	if n := ftsHitCount(t, eng, ws, "initial", id); n != 0 {
		t.Errorf("FTS hits for the pre-contention tag = %d, want 0", n)
	}
	if n := ftsHitCount(t, eng, ws, "ammonite", id); n != 1 {
		t.Errorf("FTS hits for a content term = %d, want 1 — contention must not cost content postings", n)
	}
}
