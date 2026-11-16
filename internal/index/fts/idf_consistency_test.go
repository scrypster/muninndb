package fts

import (
	"context"
	"fmt"
	"testing"

	"github.com/cockroachdb/pebble"
	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/storage/keys"
)

// These tests pin one property: a search's scores are a function of the
// COMMITTED index and the query, and of nothing else — not of what this process
// searched earlier, and not of where a concurrent write happened to be.
//
// The index used to memoize each term's IDF per (vault, term). An IDF is a
// function of two numbers, the corpus size N and the term's document frequency
// df, and the memo was dropped only when a document containing THAT term was
// indexed. Two things followed.
//
//  1. A search reads N first and df later, with no snapshot across the two, and
//     IndexEngram commits postings+df in one batch and N in a second write. A
//     search that straddles an index of a document holding the term therefore
//     sees df = N+1. For a term every document so far contains, that makes
//     log((N−df+0.5)/(df+0.5)+1) NEGATIVE, and a non-positive IDF is read as
//     "the corpus has never seen this word": no postings are scanned and the
//     word is charged idfMax in the denominator. Memoized, that verdict outlived
//     the write that caused it — every later search in that process scored the
//     word as absent, until another document containing it was indexed or the
//     process restarted. The write path does exactly this search: the
//     auto-association worker searches each new memory's tags while the FTS
//     worker is indexing that same memory.
//
//  2. Without any race at all, a memoized IDF kept the N it was computed at
//     while the corpus grew, so the same vault scored the same query
//     differently before and after a restart.
//
// Measured on the deployed build, from an empty data directory, with 40
// invented memories per vault written through MCP (8 sharing a tag, written
// first): the tag word ended with NO full-text match in 5 of 520 vault builds,
// and held a stale positive IDF in 3 more. The on-disk index was complete and
// correct in every one of them, and a restart cleared it.

const idfTestTerm = "velmora"

func idfTestDoc(i int) (id [16]byte, concept, content string, tags []string) {
	id = [16]byte{0xA0, byte(i)}
	return id, fmt.Sprintf("Velmora ledger %d", i),
		fmt.Sprintf("Port Velmora keeps ledger number %d on the east quay.", i),
		[]string{"velmora"}
}

func idfTestIndex(t *testing.T, idx *Index, ws [8]byte, from, to int) {
	t.Helper()
	for i := from; i <= to; i++ {
		id, concept, content, tags := idfTestDoc(i)
		if err := idx.IndexEngram(ws, id, concept, "", content, tags); err != nil {
			t.Fatalf("IndexEngram(%d): %v", i, err)
		}
	}
}

func requireAllFound(t *testing.T, idx *Index, ws [8]byte, query string, want int, when string) {
	t.Helper()
	res, err := idx.Search(context.Background(), ws, query, 100)
	if err != nil {
		t.Fatalf("%s: Search(%q): %v", when, query, err)
	}
	if len(res) != want {
		t.Fatalf("%s: Search(%q) matched %d documents, want %d — the word is in all %d and has %d postings on disk",
			when, query, len(res), want, want, want)
	}
	for _, r := range res {
		if r.Score <= 0 {
			t.Fatalf("%s: Search(%q) scored a matching document %v, want > 0", when, query, r.Score)
		}
	}
}

// TestSearch_StraddlingAnIndexDoesNotPoisonTheTerm forces the interleaving the
// trial hit, with real code on both sides and no sleeps: the reader's two reads
// (scoreAll reads N, then getIDF reads df) with a complete IndexEngram of a
// document holding the term in between. Every search after that, on a now
// quiescent vault, must find the term.
func TestSearch_StraddlingAnIndexDoesNotPoisonTheTerm(t *testing.T) {
	db, cleanup := openTestDB(t)
	defer cleanup()

	idx := New(db)
	ws := keys.VaultPrefix("straddle")

	idfTestIndex(t, idx, ws, 1, 7)

	// Reader, first read: scoreAll takes N before it looks at any term.
	staleN := float64(idx.readStats(ws).TotalEngrams)
	if staleN != 7 {
		t.Fatalf("setup: N = %v, want 7", staleN)
	}

	// Writer: the eighth document is indexed while the reader is between reads.
	idfTestIndex(t, idx, ws, 8, 8)

	// Reader, second read: the term's df, now 8, against the N it already holds.
	_ = idx.getIDF(ws, idfTestTerm, staleN)

	// The vault is quiescent from here on: 8 documents, df = 8, N = 8.
	requireAllFound(t, idx, ws, idfTestTerm, 8, "first search after the straddle")

	// ...and it stays found when unrelated documents arrive (nothing that holds
	// the term is indexed again, so nothing used to refresh the memo).
	for i := 0; i < 5; i++ {
		if err := idx.IndexEngram(ws, [16]byte{0xB0, byte(i)}, "Thrennic kiln", "", "The Thrennic glassworks fires a kiln.", []string{"glassworks"}); err != nil {
			t.Fatal(err)
		}
	}
	requireAllFound(t, idx, ws, idfTestTerm, 8, "search after unrelated writes")
}

// TestSearch_TermWithPostingsIsNeverScoredAsAbsent covers the same window as it
// looks ON DISK: postings and df committed, N one behind. That is what every
// reader can observe between IndexEngram's two writes, and what a crash between
// them leaves behind for good (the posting batch is synced, the stats write is
// not). A fresh process — no memo of any kind — must still find the word.
func TestSearch_TermWithPostingsIsNeverScoredAsAbsent(t *testing.T) {
	db, cleanup := openTestDB(t)
	defer cleanup()

	idx := New(db)
	ws := keys.VaultPrefix("stats-behind")

	idfTestIndex(t, idx, ws, 1, 3)

	// Roll N back by one document: the state after the third document's posting
	// batch and before its stats write.
	st := idx.readStats(ws)
	if st.TotalEngrams != 3 {
		t.Fatalf("setup: N = %d, want 3", st.TotalEngrams)
	}
	st.TotalEngrams = 2
	if err := db.Set(keys.FTSStatsKey(ws), encodeStats(st), pebble.Sync); err != nil {
		t.Fatal(err)
	}

	fresh := New(db)
	requireAllFound(t, fresh, ws, idfTestTerm, 3, "df one ahead of N")

	// A multi-word query must not charge the held word as corpus-absent either:
	// "velmora" is in all three, so a document matching it must outscore the
	// zero a wholly unknown query gets.
	res, err := fresh.Search(context.Background(), ws, "velmora ledger", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 3 {
		t.Fatalf("Search(\"velmora ledger\") matched %d documents, want 3", len(res))
	}
}

// TestSearch_ScoreDoesNotDependOnSearchHistory needs no race: one process
// searches early, the corpus grows, and the same committed index must score a
// query the same for that process as for one that has just started.
//
// The target holds a rare word in its concept (full coverage) and a commoner
// word once in its content (partial coverage), so the two words' relative IDF
// decides the score. An IDF remembered from a one-document corpus weighs the
// rare word like a common one.
func TestSearch_ScoreDoesNotDependOnSearchHistory(t *testing.T) {
	db, cleanup := openTestDB(t)
	defer cleanup()

	store := storage.NewPebbleStore(db, storage.PebbleStoreConfig{CacheSize: 100})
	ws := store.VaultPrefix("history")
	ctx := context.Background()

	longLived := New(db)
	target := [16]byte{0xC0, 1}
	if err := longLived.IndexEngram(ws, target, "zarquon", "", "one widget crate was counted beside the east quay ledger today", nil); err != nil {
		t.Fatal(err)
	}

	// An early search, when the corpus is one document.
	if _, err := longLived.Search(ctx, ws, "zarquon", 10); err != nil {
		t.Fatal(err)
	}

	// The corpus grows; none of the new documents holds "zarquon", a third hold
	// "widget".
	for i := 0; i < 12; i++ {
		content := "the tide office logged a barge at dusk"
		if i%3 == 0 {
			content = "a widget crate was logged at dusk"
		}
		if err := longLived.IndexEngram(ws, [16]byte{0xC1, byte(i)}, "harbor tally", "", content, nil); err != nil {
			t.Fatal(err)
		}
	}

	const q = "zarquon widget"
	scoreOf := func(idx *Index) float64 {
		res, err := idx.Search(ctx, ws, q, 100)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range res {
			if r.ID == target {
				return r.Score
			}
		}
		t.Fatalf("Search(%q) did not return the target document", q)
		return 0
	}

	got, want := scoreOf(longLived), scoreOf(New(db))
	if got != want {
		t.Fatalf("Search(%q) scored the same document %.4f in a process that searched earlier and %.4f in one that just started — the score must be a function of the committed index only", q, got, want)
	}
}
