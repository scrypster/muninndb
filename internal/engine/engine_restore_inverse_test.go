package engine

import (
	"context"
	"testing"

	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// ftsScoreOrZero returns id's BM25 score for query in ws (0 if it is not a hit).
func ftsScoreOrZero(t *testing.T, eng *Engine, ws [8]byte, query string, id storage.ULID) float64 {
	t.Helper()
	hits, err := eng.fts.Search(context.Background(), ws, query, 50)
	if err != nil {
		t.Fatalf("fts.Search(%q): %v", query, err)
	}
	for _, h := range hits {
		if storage.ULID(h.ID) == id {
			return h.Score
		}
	}
	return 0
}

// recallFinds reports whether a keyword-only recall (the test engine's
// embedder is a no-op, so FTS is the only candidate source) returns id.
func recallFinds(t *testing.T, eng *Engine, vault, query string, id storage.ULID) bool {
	t.Helper()
	resp, err := eng.Activate(context.Background(), &mbp.ActivateRequest{
		Vault:      vault,
		Context:    []string{query},
		MaxResults: 20,
	})
	if err != nil {
		t.Fatalf("Activate(%q): %v", query, err)
	}
	for _, a := range resp.Activations {
		if a.ID == id.String() {
			return true
		}
	}
	return false
}

// TestRestore_ForgottenMemoryIsFindableByKeywordAgain is item 1: forget drops
// the memory's keyword postings and restore never put them back, so a
// restored memory was findable by vector but never by keyword, at every later
// recall. Restore is now the exact inverse of forget for the keyword index:
// the postings come back and the corpus statistics do not move (the score
// for the memory's own word is what it was before the forget).
func TestRestore_ForgottenMemoryIsFindableByKeywordAgain(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()
	const vault = "apiary-restore"
	id := writeApiaryCorpus(t, eng, vault)
	ws := eng.store.ResolveVaultPrefix(vault)

	before := ftsScoreOrZero(t, eng, ws, "smokerwick", id)
	if before == 0 || !recallFinds(t, eng, vault, "smokerwick", id) {
		t.Fatal("precondition: the memory is not findable by its own word before the forget")
	}
	if _, err := eng.Forget(ctx, &mbp.ForgetRequest{Vault: vault, ID: id.String()}); err != nil {
		t.Fatal(err)
	}
	if ftsScoreOrZero(t, eng, ws, "smokerwick", id) != 0 {
		t.Fatal("precondition: forget left the memory's postings in place")
	}
	if _, err := eng.Restore(ctx, vault, id.String()); err != nil {
		t.Fatal(err)
	}

	if !recallFinds(t, eng, vault, "smokerwick", id) {
		t.Fatal("a restored memory is not returned by a keyword-only recall for a word only it contains")
	}
	if after := ftsScoreOrZero(t, eng, ws, "smokerwick", id); after != before {
		t.Fatalf("keyword score after forget+restore = %v, before = %v: restore is not the exact inverse of forget", after, before)
	}
}

// TestRestore_AfterRetagWhileDeleted_MatchesALiveRetag: muninn_update_tags on
// a forgotten memory keeps it out of keyword search (the guide promised the
// postings would be "rebuilt after restore" only by retagging again). Restore
// now rebuilds them from the current tags, and the end state of
// forget -> retag -> restore is the same keyword index as a plain retag in an
// identical vault: same score, new tag findable, old tag not.
func TestRestore_AfterRetagWhileDeleted_MatchesALiveRetag(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	const live, cycled = "apiary-live-retag", "apiary-cycled-retag"
	liveID := writeApiaryCorpus(t, eng, live)
	cycledID := writeApiaryCorpus(t, eng, cycled)
	liveWS := eng.store.ResolveVaultPrefix(live)
	cycledWS := eng.store.ResolveVaultPrefix(cycled)

	newTags := []string{"queenline-frames"}
	if err := eng.UpdateTags(ctx, live, liveID, newTags); err != nil {
		t.Fatal(err)
	}

	if _, err := eng.Forget(ctx, &mbp.ForgetRequest{Vault: cycled, ID: cycledID.String()}); err != nil {
		t.Fatal(err)
	}
	if err := eng.UpdateTags(ctx, cycled, cycledID, newTags); err != nil {
		t.Fatal(err)
	}
	if ftsScoreOrZero(t, eng, cycledWS, "queenline", cycledID) != 0 {
		t.Fatal("precondition: a retag of a forgotten memory put it back into keyword search")
	}
	if _, err := eng.Restore(ctx, cycled, cycledID.String()); err != nil {
		t.Fatal(err)
	}

	if !recallFinds(t, eng, cycled, "queenline", cycledID) {
		t.Fatal("after forget -> retag -> restore the memory is not findable by its new tag")
	}
	if ftsScoreOrZero(t, eng, cycledWS, "apiary", cycledID) != 0 {
		t.Fatal("after forget -> retag -> restore the memory is still findable by the tag it no longer has")
	}
	for _, q := range []string{"queenline", "smokerwick"} {
		want := ftsScoreOrZero(t, eng, liveWS, q, liveID)
		if got := ftsScoreOrZero(t, eng, cycledWS, q, cycledID); got != want {
			t.Fatalf("score for %q after forget -> retag -> restore = %v, after a live retag = %v: the keyword statistics differ", q, got, want)
		}
	}
}
