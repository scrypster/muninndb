package engine

import (
	"context"
	"testing"

	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// TestReindexFTS_DoesNotMakeForgottenMemoriesKeywordFindable: the vault
// keyword rebuild indexed every record, so a full rebuild put a forgotten
// memory's postings back. It now leaves a forgotten (trash) memory exactly as
// Forget leaves it — no postings, its term frequencies still counted, so a
// later Restore is the same exact inverse — while live and archived memories
// are indexed as before.
func TestReindexFTS_DoesNotMakeForgottenMemoriesKeywordFindable(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()
	const vault = "apiary-reindex"
	id := writeApiaryCorpus(t, eng, vault)
	ws := eng.store.ResolveVaultPrefix(vault)
	before := ftsScoreOrZero(t, eng, ws, "smokerwick", id)

	if _, err := eng.Forget(ctx, &mbp.ForgetRequest{Vault: vault, ID: id.String()}); err != nil {
		t.Fatal(err)
	}
	if _, err := eng.ReindexFTSVault(ctx, vault); err != nil {
		t.Fatal(err)
	}
	if s := ftsScoreOrZero(t, eng, ws, "smokerwick", id); s != 0 {
		t.Fatalf("after a keyword rebuild a forgotten memory is keyword-findable again (score %v)", s)
	}
	if _, err := eng.Restore(ctx, vault, id.String()); err != nil {
		t.Fatal(err)
	}
	if after := ftsScoreOrZero(t, eng, ws, "smokerwick", id); after != before {
		t.Fatalf("score after forget -> rebuild -> restore = %v, before = %v", after, before)
	}
}

// TestObservability_CountsLiveMemoriesWithoutKeywordPostings: memories
// restored before the restore fix are live but have no keyword postings, and
// nothing told an operator whether a vault had any. The admin observability
// snapshot now reports the count per vault (forgotten memories, which have
// no postings by design, are not counted), and a keyword rebuild clears it.
func TestObservability_CountsLiveMemoriesWithoutKeywordPostings(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()
	const vault = "apiary-coverage"
	id := writeApiaryCorpus(t, eng, vault)
	ws := eng.store.ResolveVaultPrefix(vault)

	// A second memory forgotten normally: no postings, and correctly so.
	resp, err := eng.Write(ctx, &mbp.WriteRequest{Vault: vault, Concept: "old hive stand", Content: "the cedar hive stand was retired"})
	if err != nil {
		t.Fatal(err)
	}
	awaitFTS(t, eng)
	if _, err := eng.Forget(ctx, &mbp.ForgetRequest{Vault: vault, ID: resp.ID}); err != nil {
		t.Fatal(err)
	}

	// The old bug's end state for `id`: live record, postings gone.
	eng2, err := eng.store.GetEngramUncached(ctx, ws, id)
	if err != nil {
		t.Fatal(err)
	}
	if err := eng.fts.DeleteEngram(ws, [16]byte(id), eng2.Concept, eng2.CreatedBy, eng2.Content, eng2.Tags); err != nil {
		t.Fatal(err)
	}

	snap, err := eng.Observability(ctx, "test", 1)
	if err != nil {
		t.Fatal(err)
	}
	if got := snap.Vaults[vault].KeywordUnindexedLive; got != 1 {
		t.Fatalf("keyword_unindexed_live = %d, want 1 (one live memory without postings; the forgotten one does not count)", got)
	}

	if _, err := eng.ReindexFTSVault(ctx, vault); err != nil {
		t.Fatal(err)
	}
	snap, err = eng.Observability(ctx, "test", 1)
	if err != nil {
		t.Fatal(err)
	}
	if got := snap.Vaults[vault].KeywordUnindexedLive; got != 0 {
		t.Fatalf("keyword_unindexed_live after a rebuild = %d, want 0", got)
	}
}
