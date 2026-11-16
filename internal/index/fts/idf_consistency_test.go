package fts

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"sort"
	"testing"

	"github.com/cockroachdb/pebble"
	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/storage/keys"
)

// These tests pin that nothing a search works out is remembered for the next
// one, and that a word with a document-frequency record is never scored as if
// the vault had not seen it.
//
// The index used to memoize each term's IDF per (vault, term). An IDF is a
// function of two numbers, the corpus size N and the term's document frequency
// df, and the memo was dropped only when a document containing THAT term was
// indexed. Two things followed.
//
//  1. A search read N first and df later, with no snapshot across the two, and
//     IndexEngram committed postings+df in one batch and N in a second write. A
//     search that straddled an index of a document holding the term therefore
//     saw df = N+1. For a term every document so far contains, that makes
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
// Measured on the build that had the memo, from an empty data directory, 40
// invented memories per vault written through MCP by one to four builders at
// once, the first 8 sharing a tag: the tag word ended with NO full-text match
// in 12 of 820 vault builds, and held a stale positive IDF in 3 more. The
// on-disk index was complete and correct in every one, and a restart cleared
// it. An independent reproduction at this level — a real writer and a searcher
// that never pauses, no hooks — left the word unfindable in 300 of 300 trials
// with the memo and 0 of 300 without it.
//
// What these tests do NOT claim: that every word with POSTINGS is findable. A
// word can have postings and no document-frequency record at all — a memory
// whose first index never ran and that was later restored or retagged gets its
// postings from ReindexEngram, which adds no frequency for a word the document
// "already had". That word still matches nothing, on this build and the ones
// before it, and HasPostings reports the memory as indexed.

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

// TestSearch_StraddlingAnIndexDoesNotPoisonTheTerm replays the interleaving the
// trial hit, with real code on both sides and no sleeps: the reader's two reads
// (N, then the term's df) with a complete IndexEngram of a document holding the
// term in between. Every search after that, on a now quiescent vault, must find
// the term.
//
// A search can no longer produce this interleaving itself — it reads both
// numbers from one snapshot (TestSearch_AnswersFromOneCommittedStateAtEvery
// Interleaving) — so the two reads are made by hand here. What the test keeps
// pinning is the other half: an IDF worked out from a stale N is not kept.
func TestSearch_StraddlingAnIndexDoesNotPoisonTheTerm(t *testing.T) {
	db, cleanup := openTestDB(t)
	defer cleanup()

	idx := New(db)
	ws := keys.VaultPrefix("straddle")

	idfTestIndex(t, idx, ws, 1, 7)

	// Reader, first read: scoreAll takes N before it looks at any term.
	staleN := float64(mustStats(t, idx, ws).TotalEngrams)
	if staleN != 7 {
		t.Fatalf("setup: N = %v, want 7", staleN)
	}

	// Writer: the eighth document is indexed while the reader is between reads.
	idfTestIndex(t, idx, ws, 8, 8)

	// Reader, second read: the term's df, now 8, against the N it already holds.
	_ = mustIDF(t, idx, ws, idfTestTerm, staleN)

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

// TestSearch_WordWithAFrequencyRecordIsFoundWhenTheDocumentCountIsBehind covers
// the df = N+1 state as it looks ON DISK: postings and df committed, N one
// behind. IndexEngram no longer writes it (the count commits with the postings),
// but a store last written by a build that wrote N separately, and that crashed
// between the two writes, carries it for good. A fresh process — no memo of any
// kind — must still find a word whose frequency record says it is there.
//
// It is one point of the clamp's behaviour, end to end. The arithmetic is pinned
// across its whole domain by TestGetIDF_PositiveFiniteAndMonotone.
func TestSearch_WordWithAFrequencyRecordIsFoundWhenTheDocumentCountIsBehind(t *testing.T) {
	db, cleanup := openTestDB(t)
	defer cleanup()

	idx := New(db)
	ws := keys.VaultPrefix("stats-behind")

	idfTestIndex(t, idx, ws, 1, 3)

	// Roll N back by one document: what a crash between the posting batch and
	// the separate stats write left on disk, when they were separate.
	st := mustStats(t, idx, ws)
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

// TestGetIDF_PositiveFiniteAndMonotone pins getIDF over its whole numeric
// domain, not at one point: for every document frequency a record can hold and
// every corpus size a search can pass, the IDF of a word that HAS a record is
// positive and finite, never rises as the word gets commoner, and never falls
// as the corpus grows. Positive is what keeps the word from being read as
// "never seen"; the two monotonicities are what make it an IDF at all.
//
// df ranges over the uint32 the record stores, up to the value an unclamped
// decrement would wrap to. N includes values BELOW df, which is the case the
// clamp exists for, and 2^53, the last corpus size a float64 counts exactly.
func TestGetIDF_PositiveFiniteAndMonotone(t *testing.T) {
	db, cleanup := openTestDB(t)
	defer cleanup()
	idx := New(db)
	ws := keys.VaultPrefix("idf-table")
	const term = "zq"

	setDF := func(df uint32) {
		t.Helper()
		var b [8]byte
		binary.BigEndian.PutUint32(b[:4], df)
		if err := db.Set(keys.TermStatsKey(ws, term), b[:], pebble.NoSync); err != nil {
			t.Fatal(err)
		}
	}

	dfs := []uint32{0, 1, 1 << 31, math.MaxUint32}
	nsFor := func(df uint32) []float64 {
		d := float64(df)
		ns := []float64{0, 1}
		if df > 0 {
			ns = append(ns, d-1)
		}
		ns = append(ns, d, d+1, 1<<53)
		sort.Float64s(ns)
		return ns
	}
	// Every N that appears for any df, so monotonicity in df is checked at each.
	allNs := map[float64]bool{}
	for _, df := range dfs {
		for _, n := range nsFor(df) {
			allNs[n] = true
		}
	}

	at := func(df uint32, n float64) float64 {
		t.Helper()
		setDF(df)
		idf := mustIDF(t, idx, ws, term, n)
		if !(idf > 0) || math.IsInf(idf, 0) || math.IsNaN(idf) {
			t.Errorf("getIDF(N=%v, df=%d) = %v, want positive and finite — a word with a frequency record must never read as absent", n, df, idf)
		}
		return idf
	}

	for _, df := range dfs {
		prev := math.Inf(-1)
		for _, n := range nsFor(df) {
			idf := at(df, n)
			if idf < prev {
				t.Errorf("getIDF fell from %v to %v as N rose to %v at df=%d, want non-decreasing in N", prev, idf, n, df)
			}
			prev = idf
		}
	}
	for n := range allNs {
		prev := math.Inf(1)
		for _, df := range dfs {
			idf := at(df, n)
			if idf > prev {
				t.Errorf("getIDF rose from %v to %v as df rose to %d at N=%v, want non-increasing in df", prev, idf, df, n)
			}
			prev = idf
		}
	}

	// No record at all is the one input that returns 0.
	if err := db.Delete(keys.TermStatsKey(ws, term), pebble.NoSync); err != nil {
		t.Fatal(err)
	}
	if idf := mustIDF(t, idx, ws, term, 10); idf != 0 {
		t.Errorf("getIDF with no frequency record = %v, want 0 (the corpus has never counted the word)", idf)
	}
}
