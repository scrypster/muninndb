package engine

import (
	"context"
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

	if err := eng.UpdateTags(ctx, vault, id, []string{"antelope"}); err != nil {
		t.Fatalf("UpdateTags: %v", err)
	}
	awaitFTS(t, eng)

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
	awaitFTS(t, eng)
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
	awaitFTS(t, eng)

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
