package engine

import (
	"context"
	"fmt"
	"testing"

	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// TestUpdateTags_ReindexesFTS proves that an in-place retag leaves the BM25
// posting lists agreeing with the engram's real tag set.
//
// Why this is a correctness test and not a housekeeping nit: tags are tokenized
// into the posting lists under FieldTags (internal/index/fts/fts.go
// IndexEngram), and storage.UpdateTags only rewrites 0x02/0x03/0x0C/0x2C. The
// stale 0x0C/0x2C tag-index entries are harmless because
// activation.PassesMetaFilter re-checks tags_all/tags_any/tag_prefix against
// eng.Tags — a stale seeding entry cannot produce a false positive there. FTS
// has no such rescue: ftsScore feeds the RRF/ACT-R blend directly with nothing
// downstream to re-verify it. So without the delete-then-reindex pair in
// Engine.UpdateTags, recall scores the engram on a tag it does NOT have and
// fails to score it on the tag it DOES have (#720).
//
// RED (before the fix), measured:
//
//	engram tags on disk after retag: [antelope]
//	FTS hits for the REMOVED tag "zebrafish": 1 (want 0)
//	FTS hits for the ADDED   tag "antelope":  0 (want 1)
//
// The delete side must be keyed on the OLD tag set, captured BEFORE
// store.UpdateTags overwrites it — fts.DeleteEngram derives the terms to remove
// from the tags it is handed (same shape as the soft-delete cleanup path), so
// deleting with the new set would orphan the old postings anyway.
func TestUpdateTags_ReindexesFTS(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()
	const vault = "retag-fts"

	resp, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:   vault,
		Concept: "migration plan",
		Content: "the plan covers the staged rollout and the rollback drill",
		Tags:    []string{"zebrafish"},
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	id, err := storage.ParseULID(resp.ID)
	if err != nil {
		t.Fatalf("ParseULID(%q): %v", resp.ID, err)
	}
	ws := eng.store.ResolveVaultPrefix(vault)

	// The async FTS worker owns indexing; drain it rather than sleeping
	// (docs/internals/testing-hermeticity.md).
	awaitFTS(t, eng)

	// Sanity: the original tag really is searchable, so a later miss means the
	// retag broke it rather than tag indexing never having worked.
	if n := ftsHitCount(t, eng, ws, "zebrafish", id); n != 1 {
		t.Fatalf("precondition: FTS hits for the original tag %q = %d, want 1", "zebrafish", n)
	}

	// No awaitFTS after a retag: Engine.UpdateTags reindexes SYNCHRONOUSLY
	// through fts.ReindexEngram. It is the one FTS path that deletes before it
	// adds, so a job dropped by the worker's non-blocking Submit would leave the
	// engram with no postings at all rather than merely delayed ones — see
	// Engine.UpdateTags's doc comment. The absence of a drain here is part of what
	// this test asserts.
	if err := eng.UpdateTags(ctx, vault, id, []string{"antelope"}); err != nil {
		t.Fatalf("UpdateTags: %v", err)
	}

	// The record is the authority for what the tags ARE.
	got, err := eng.store.GetEngram(ctx, ws, id)
	if err != nil {
		t.Fatalf("GetEngram after UpdateTags: %v", err)
	}
	if len(got.Tags) != 1 || got.Tags[0] != "antelope" {
		t.Fatalf("engram tags on disk after retag = %v, want [antelope]", got.Tags)
	}

	// ...and the posting lists must agree with it in both directions.
	if n := ftsHitCount(t, eng, ws, "zebrafish", id); n != 0 {
		t.Errorf("FTS hits for the REMOVED tag %q = %d, want 0 (stale posting: recall would score this engram on a tag it does not have)", "zebrafish", n)
	}
	if n := ftsHitCount(t, eng, ws, "antelope", id); n != 1 {
		t.Errorf("FTS hits for the ADDED tag %q = %d, want 1 (missing posting: recall cannot score this engram on the tag it does have)", "antelope", n)
	}

	// The delete-then-reindex pair drops EVERY posting the engram had (all
	// fields, not just FieldTags) before re-adding them, so a content term must
	// survive a retag — otherwise the fix would trade a tag bug for a worse one.
	if n := ftsHitCount(t, eng, ws, "rollback", id); n != 1 {
		t.Errorf("FTS hits for the content term %q = %d, want 1 — retag must not drop content postings", "rollback", n)
	}

	// Clearing all tags must clear the postings too (the tool advertises an
	// empty array as clear-all).
	if err := eng.UpdateTags(ctx, vault, id, []string{}); err != nil {
		t.Fatalf("UpdateTags(clear): %v", err)
	}
	if n := ftsHitCount(t, eng, ws, "antelope", id); n != 0 {
		t.Errorf("FTS hits for %q after clearing all tags = %d, want 0", "antelope", n)
	}
	if n := ftsHitCount(t, eng, ws, "rollback", id); n != 1 {
		t.Errorf("FTS hits for the content term %q after clearing tags = %d, want 1", "rollback", n)
	}
}

// TestUpdateTags_ThenForget_LeavesNoOrphanPostings covers the sibling defect the
// reindex closes for free. Forget's soft-delete cleanup calls
// fts.DeleteEngram(..., eng.Tags) with the tags as read AT DELETE TIME, so
// before the reindex a retag left the index holding postings for the OLD tags
// that the delete's term set never mentioned — a soft-deleted engram stayed
// keyword-searchable under a tag it used to have. With Engine.UpdateTags keeping
// the postings equal to the record's tags, the delete's term set is exact again
// and nothing survives (#720).
func TestUpdateTags_ThenForget_LeavesNoOrphanPostings(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()
	const vault = "retag-then-forget"

	resp, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:   vault,
		Concept: "quarterly audit",
		Content: "the auditors reviewed the ledger and signed off",
		Tags:    []string{"zebrafish"},
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	id, err := storage.ParseULID(resp.ID)
	if err != nil {
		t.Fatalf("ParseULID(%q): %v", resp.ID, err)
	}
	ws := eng.store.ResolveVaultPrefix(vault)
	awaitFTS(t, eng)

	if err := eng.UpdateTags(ctx, vault, id, []string{"antelope"}); err != nil {
		t.Fatalf("UpdateTags: %v", err)
	}

	if _, err := eng.Forget(ctx, &mbp.ForgetRequest{Vault: vault, ID: resp.ID}); err != nil {
		t.Fatalf("Forget: %v", err)
	}
	awaitFTS(t, eng)

	for _, term := range []string{"zebrafish", "antelope", "auditors"} {
		if n := ftsHitCount(t, eng, ws, term, id); n != 0 {
			t.Errorf("after retag + soft delete, FTS hits for %q = %d, want 0 (orphan posting)", term, n)
		}
	}
}

// TestUpdateTags_AfterForget_DoesNotReindexSoftDeleted covers the OTHER ordering:
// forget first, then retag. It is not symmetric with retag→forget, because the
// retag is the thing that writes postings — reindexing unconditionally would undo
// the soft delete's own FTS cleanup, whose whole stated purpose (fts.DeleteEngram's
// header) is "to prevent soft-deleted engrams from appearing in search results".
//
// It is contained rather than catastrophic: activation's phase-6 filter drops
// StateSoftDeleted/StateArchived before returning candidates, so a resurrected
// posting cannot become a visible false positive. But it burns candidate slots
// and skews the corpus statistics for a document that is not even active — and
// this branch documents soft-deleted engrams as retaggable, so it is a reachable
// state, not a hypothetical one (#720).
//
// SCOPE: this is the TRASH case specifically. The drop is gated on trash versus
// history, not on State alone — a plain Forget leaves ValidUntil OPEN, which is
// what makes this record discarded rather than superseded, and Forget has
// already de-indexed it. An evolve predecessor carries a CLOSED ValidUntil and
// is still indexed on purpose, so it takes the reindex arm instead; see
// TestUpdateTags_EvolvePredecessorKeepsPostings and
// activation.CarriesSupersessionSignature (#720 review, finding 1). Archived
// engrams likewise reindex: nothing de-indexes on archival, so dropping them
// here destroyed live postings.
func TestUpdateTags_AfterForget_DoesNotReindexSoftDeleted(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()
	const vault = "forget-then-retag"

	resp, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:   vault,
		Concept: "decommission runbook",
		Content: "the runbook drains the node and revokes its certificates",
		Tags:    []string{"zebrafish"},
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	id, err := storage.ParseULID(resp.ID)
	if err != nil {
		t.Fatalf("ParseULID(%q): %v", resp.ID, err)
	}
	ws := eng.store.ResolveVaultPrefix(vault)
	awaitFTS(t, eng)

	if n := ftsHitCount(t, eng, ws, "zebrafish", id); n != 1 {
		t.Fatalf("precondition: FTS hits for %q = %d, want 1", "zebrafish", n)
	}

	if _, err := eng.Forget(ctx, &mbp.ForgetRequest{Vault: vault, ID: resp.ID}); err != nil {
		t.Fatalf("Forget: %v", err)
	}
	awaitFTS(t, eng)
	if n := ftsHitCount(t, eng, ws, "runbook", id); n != 0 {
		t.Fatalf("precondition: soft delete left %d postings for %q, want 0", n, "runbook")
	}

	// The retag itself must still succeed — a soft-deleted memory stays
	// retaggable, because Restore exists and refusing to fix a label on a
	// recoverable memory is the worse contract.
	if err := eng.UpdateTags(ctx, vault, id, []string{"antelope"}); err != nil {
		t.Fatalf("UpdateTags on a soft-deleted engram: %v", err)
	}
	if got, err := eng.store.GetEngram(ctx, ws, id); err != nil {
		t.Fatalf("GetEngram after retagging a soft-deleted engram: %v", err)
	} else if len(got.Tags) != 1 || got.Tags[0] != "antelope" {
		t.Errorf("soft-deleted engram tags after retag = %v, want [antelope]", got.Tags)
	}

	// ...but nothing may come back into the index.
	for _, term := range []string{"antelope", "zebrafish", "runbook", "decommission"} {
		if n := ftsHitCount(t, eng, ws, term, id); n != 0 {
			t.Errorf("after forget → retag, FTS hits for %q = %d, want 0 — a retag must not reindex a soft-deleted engram", term, n)
		}
	}
}

// TestUpdateTags_RepeatedRetagsDoNotDecayFTSScore is the end-to-end half of the
// stats-neutrality pin in internal/index/fts/reindex_test.go: it goes through
// Engine.UpdateTags, so it proves the ENGINE is on the stats-neutral path and not
// just that the path exists.
//
// The symptom being pinned: with the delete-then-index pair, every retag
// incremented the document frequency of every term the engram contained (and the
// corpus size), so the engram's own score for a query on its own UNCHANGED
// content decayed monotonically — 8.1% from one retag, below a 0.3 default
// threshold by ten, 82.6% by a hundred against a 10.0% drop for an unretagged
// control. muninn_update_tags advertises `due:<ISO-date>` maintenance, so ten
// retags is under two weeks of ordinary use, and nothing surfaces the loss: the
// memory simply stops being found, BECAUSE its owner curated it (#720).
func TestUpdateTags_RepeatedRetagsDoNotDecayFTSScore(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()
	const vault = "retag-score-decay"

	// A background corpus, so N and the common terms' DF are not degenerate.
	for i := range 12 {
		if _, err := eng.Write(ctx, &mbp.WriteRequest{
			Vault:   vault,
			Concept: "release notes",
			Content: fmt.Sprintf("the release notes describe deployment step %d", i),
		}); err != nil {
			t.Fatalf("Write(background %d): %v", i, err)
		}
	}

	resp, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:   vault,
		Concept: "capacitor recalibration",
		Content: "the zorbfluke procedure needs a torque wrench and a steady hand",
		Tags:    []string{"due:2026-01-01"},
	})
	if err != nil {
		t.Fatalf("Write(target): %v", err)
	}
	id, err := storage.ParseULID(resp.ID)
	if err != nil {
		t.Fatalf("ParseULID(%q): %v", resp.ID, err)
	}
	ws := eng.store.ResolveVaultPrefix(vault)
	awaitFTS(t, eng)

	// One rare term the target really has, one the corpus has never seen — the
	// shape COG-24 calibrates against.
	const query = "zorbfluke plarfnog"

	// Baseline after the first retag, so the comparison isn't confounded by the
	// tag field's token count changing shape on the first edit.
	if err := eng.UpdateTags(ctx, vault, id, []string{"due:2026-02-02"}); err != nil {
		t.Fatalf("UpdateTags(baseline): %v", err)
	}
	baseline := ftsScore(t, eng, ws, query, id)

	for i := range 20 {
		tag := fmt.Sprintf("due:2026-03-%02d", i+1)
		if err := eng.UpdateTags(ctx, vault, id, []string{tag}); err != nil {
			t.Fatalf("UpdateTags(%s): %v", tag, err)
		}
	}
	after := ftsScore(t, eng, ws, query, id)

	t.Logf("full_text score after 1 retag = %.4f; after 21 retags = %.4f", baseline, after)
	if after < baseline*0.99 {
		t.Errorf("score decayed %.4f → %.4f over 20 retags (%.1f%% loss) — curating a memory's tags must not make it harder to find by its own unchanged content",
			baseline, after, 100*(baseline-after)/baseline)
	}
}

// ftsScore returns id's raw FTS score for query, failing if the engram is absent
// from the results entirely.
func ftsScore(t *testing.T, eng *Engine, ws [8]byte, query string, id storage.ULID) float64 {
	t.Helper()
	if eng.fts == nil {
		t.Fatal("test engine has no FTS index wired")
	}
	hits, err := eng.fts.Search(context.Background(), ws, query, 50)
	if err != nil {
		t.Fatalf("fts.Search(%q): %v", query, err)
	}
	for _, h := range hits {
		if storage.ULID(h.ID) == id {
			return h.Score
		}
	}
	t.Fatalf("engram %s absent from FTS results for %q", id.String(), query)
	return 0
}

// ftsHitCount returns how many times id appears in the raw FTS results for
// query. It queries the index directly (not recall) so the assertion is about
// the posting lists themselves, with no scoring threshold in the way.
func ftsHitCount(t *testing.T, eng *Engine, ws [8]byte, query string, id storage.ULID) int {
	t.Helper()
	if eng.fts == nil {
		t.Fatal("test engine has no FTS index wired")
	}
	hits, err := eng.fts.Search(context.Background(), ws, query, 50)
	if err != nil {
		t.Fatalf("fts.Search(%q): %v", query, err)
	}
	n := 0
	for _, h := range hits {
		if storage.ULID(h.ID) == id {
			n++
		}
	}
	return n
}
