package fts

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cockroachdb/pebble"
	"github.com/cockroachdb/pebble/vfs"
	"github.com/cockroachdb/pebble/vfs/errorfs"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/scrypster/muninndb/internal/metrics"
	"github.com/scrypster/muninndb/internal/storage/keys"
)

// A search answers from ONE committed state of the index. Two things make that
// true, and each has a test here:
//
//   - the reader takes one Pebble snapshot and reads the document count, every
//     word's document frequency and every posting list from it;
//   - the writer commits a document's postings, document frequencies AND the
//     corpus statistics in one batch, so there is no committed state in which a
//     word is counted in a document the corpus size does not include.
//
// With both, a word counted in more documents than the vault holds is no longer
// something a search can see in passing. It means the statistics on disk
// disagree, and the third test pins that this is reported, not just survived.

// readGate counts sstable reads and can stop the caller inside the Nth one. It
// is the hook for forcing an interleaving without touching product code and
// without sleeping: the search is parked INSIDE one of its own reads while the
// test commits a write.
type readGate struct {
	count   atomic.Int64
	pauseAt atomic.Int64
	paused  chan struct{}
	release chan struct{}
}

func (g *readGate) maybeError(op errorfs.Op, path string) error {
	if op.OpKind() != errorfs.OpKindRead || !strings.HasSuffix(path, ".sst") {
		return nil
	}
	if n := g.count.Add(1); n == g.pauseAt.Load() {
		g.pauseAt.Store(0)
		g.paused <- struct{}{}
		<-g.release
	}
	return nil
}

// snapshotFixture builds the same small index on a fresh in-memory store every
// time, flushed to one sstable, so a search performs the same sequence of reads
// on every build. The target holds a rare word in its concept and a common word
// once in its content, so its score for "zarquon widget" moves with the corpus
// size and with the common word's document frequency.
func snapshotFixture(t *testing.T) (idx *Index, g *readGate, ws [8]byte, target [16]byte) {
	t.Helper()
	g = &readGate{paused: make(chan struct{}), release: make(chan struct{})}
	cache := pebble.NewCache(1) // one byte: every read reaches the file, and the gate
	db, err := pebble.Open("snapshot", &pebble.Options{
		FS:                          errorfs.Wrap(vfs.NewMem(), errorfs.InjectorFunc(g.maybeError)),
		Cache:                       cache,
		DisableAutomaticCompactions: true, // a background read must not take the gate
	})
	if err != nil {
		cache.Unref()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		db.Close()
		cache.Unref()
	})
	idx = New(db)
	ws = keys.VaultPrefix("snapshot")
	target = [16]byte{0xEE, 1}
	if err := idx.IndexEngram(ws, target, "zarquon", "", "one widget crate beside the east quay", nil); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 6; i++ {
		content := "the tide office logged a barge at dusk"
		if i%2 == 0 {
			content = "a widget crate was logged at dusk"
		}
		if err := idx.IndexEngram(ws, [16]byte{0xE0, byte(i)}, "harbor tally", "", content, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Flush(); err != nil {
		t.Fatal(err)
	}
	return idx, g, ws, target
}

func snapshotScore(t *testing.T, idx *Index, ws [8]byte, target [16]byte) float64 {
	res, err := idx.Search(context.Background(), ws, "zarquon widget", 100)
	if err != nil {
		t.Errorf("Search: %v", err)
		return -1
	}
	for _, r := range res {
		if r.ID == target {
			return r.Score
		}
	}
	t.Errorf("Search did not return the target")
	return -1
}

// TestSearch_AnswersFromOneCommittedStateAtEveryInterleaving parks a search
// inside each of its sstable reads in turn, commits one document while it is
// parked, and lets it finish. Whatever point it was parked at, its score for the
// target must be the score of a state the index was actually in — before that
// write or after it — and never a blend of the two.
func TestSearch_AnswersFromOneCommittedStateAtEveryInterleaving(t *testing.T) {
	// How many sstable reads does one warm search make on this fixture?
	idx, g, ws, target := snapshotFixture(t)
	snapshotScore(t, idx, ws, target) // warm: open the table
	g.count.Store(0)
	snapshotScore(t, idx, ws, target)
	reads := int(g.count.Load())
	if reads < 4 {
		t.Fatalf("fixture: a search made %d sstable reads; the test needs several to interleave at", reads)
	}

	blended := 0
	for p := 1; p <= reads; p++ {
		idx, g, ws, target := snapshotFixture(t)
		snapshotScore(t, idx, ws, target) // warm
		before := snapshotScore(t, idx, ws, target)

		g.count.Store(0)
		g.pauseAt.Store(int64(p))
		done := make(chan float64, 1)
		go func() { done <- snapshotScore(t, idx, ws, target) }()
		select {
		case <-g.paused:
		case got := <-done:
			t.Fatalf("read %d of %d: the search finished (score %v) without reaching it — the fixture is not deterministic", p, reads, got)
		}

		// The search is parked inside its p-th read. Commit a document holding
		// the common word: the corpus size and that word's frequency both move.
		// (Bounded, so a search that has started holding a lock the writer needs
		// fails here instead of hanging the suite.)
		wrote := make(chan error, 1)
		go func() {
			wrote <- idx.IndexEngram(ws, [16]byte{0xE1, byte(p)}, "harbor tally", "", "another widget crate was counted", nil)
		}()
		select {
		case err := <-wrote:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(10 * time.Second):
			g.release <- struct{}{}
			t.Fatalf("parked in read %d of %d: a write could not commit while the search was parked — the search is holding something a writer needs", p, reads)
		}
		after := snapshotScore(t, idx, ws, target)
		g.release <- struct{}{}
		got := <-done

		if before == after {
			t.Fatalf("fixture: the write did not move the target's score (%v), so a blend could not be seen", before)
		}
		if got != before && got != after {
			blended++
			t.Errorf("parked in read %d of %d: the search scored the target %.6f, which is neither the state before the write (%.6f) nor the state after it (%.6f)",
				p, reads, got, before, after)
		}
	}
	t.Logf("%d interleavings, %d blended scores", reads, blended)
}

// TestIndexEngram_CorpusStatisticsCommitWithThePostings cuts the power the
// moment an index returns: only what was SYNCED survives. The document count
// must have survived with the postings and document frequencies it belongs to.
func TestIndexEngram_CorpusStatisticsCommitWithThePostings(t *testing.T) {
	mem := vfs.NewStrictMem()
	open := func() *pebble.DB {
		// The store lives at the FS root: a subdirectory would need its PARENT
		// synced to survive the reset, which Pebble does not do.
		db, err := pebble.Open("", &pebble.Options{FS: mem})
		if err != nil {
			t.Fatal(err)
		}
		return db
	}
	ws := keys.VaultPrefix("crash")

	db := open()
	idfTestIndex(t, New(db), ws, 1, 3)
	mem.SetIgnoreSyncs(true) // nothing from here on reaches the disk
	db.Close()
	mem.ResetToSyncedState() // power loss: drop everything unsynced
	mem.SetIgnoreSyncs(false)

	db = open()
	defer db.Close()
	idx := New(db)
	df, _, err := readDF(db, ws, idfTestTerm)
	if err != nil {
		t.Fatal(err)
	}
	n := mustStats(t, idx, ws).TotalEngrams
	if df != 3 || n != 3 {
		t.Fatalf("after a crash right after the third index: df(%s) = %d, N = %d; want 3 and 3 — the document count must be as durable as the postings it counts",
			idfTestTerm, df, n)
	}
	requireAllFound(t, idx, ws, idfTestTerm, 3, "after the crash")
}

// TestSearch_ReportsStatisticsThatDisagree: a word counted in more documents
// than the vault holds is counted on every search that meets it and logged once
// per vault per interval, naming the vault prefix and never the word.
func TestSearch_ReportsStatisticsThatDisagree(t *testing.T) {
	db, cleanup := openTestDB(t)
	defer cleanup()
	idx := New(db)
	ctx := context.Background()

	buf := &bytes.Buffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(prev)
	const msg = "keyword statistics are inconsistent"

	// A consistent vault reports nothing.
	healthy := keys.VaultPrefix("stats-agree")
	idfTestIndex(t, idx, healthy, 1, 3)
	base := testutil.ToFloat64(metrics.FTSStatsInconsistent)
	requireAllFound(t, idx, healthy, idfTestTerm, 3, "consistent vault")
	if got := testutil.ToFloat64(metrics.FTSStatsInconsistent) - base; got != 0 || strings.Contains(buf.String(), msg) {
		t.Fatalf("a consistent vault was reported: counter +%v, log %q", got, buf.String())
	}

	// A vault whose document count is one behind a word's frequency.
	ws := keys.VaultPrefix("stats-disagree")
	idfTestIndex(t, idx, ws, 1, 3)
	st := mustStats(t, idx, ws)
	st.TotalEngrams = 2
	if err := db.Set(keys.FTSStatsKey(ws), encodeStats(st), pebble.Sync); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if res, err := idx.Search(ctx, ws, idfTestTerm, 10); err != nil || len(res) != 3 {
			t.Fatalf("Search = %d matches, err %v; want 3, nil", len(res), err)
		}
	}
	if got := testutil.ToFloat64(metrics.FTSStatsInconsistent) - base; got != 3 {
		t.Errorf("muninndb_fts_stats_inconsistent_total moved by %v over three searches, want 3", got)
	}
	logs := buf.String()
	if n := strings.Count(logs, msg); n != 1 {
		t.Errorf("logged the inconsistency %d times over three searches, want once per vault per interval. Log:\n%s", n, logs)
	}
	if !strings.Contains(logs, fmt.Sprintf("vault_prefix=%x", ws)) {
		t.Errorf("the WARN does not name the vault prefix. Log:\n%s", logs)
	}
	if strings.Contains(logs, idfTestTerm) {
		t.Errorf("the WARN carries the word itself, which is memory content. Log:\n%s", logs)
	}
}

// TestSearch_CompletesWhileAWriterHoldsTheIndexLock pins that a search takes no
// index lock. A real IndexEngram is parked inside its critical section — in the
// document-frequency read it makes while holding idx.mu — and a search must run
// to completion before the writer is released. When searches took the lock,
// every one queued behind the writer's synced commit (median 17–20 ms against
// about 0.2 ms with one writer indexing continuously).
func TestSearch_CompletesWhileAWriterHoldsTheIndexLock(t *testing.T) {
	idx, g, ws, target := snapshotFixture(t)
	want := snapshotScore(t, idx, ws, target)

	g.count.Store(0)
	g.pauseAt.Store(1) // the writer's first sstable read is its df read, under idx.mu
	wrote := make(chan error, 1)
	go func() {
		wrote <- idx.IndexEngram(ws, [16]byte{0xE2, 1}, "harbor tally", "", "another widget crate was counted", nil)
	}()
	select {
	case <-g.paused:
	case err := <-wrote:
		t.Fatalf("fixture: IndexEngram finished (err %v) without an sstable read to park in", err)
	}
	if idx.mu.TryLock() {
		idx.mu.Unlock()
		t.Fatal("fixture: the writer is parked but does not hold idx.mu")
	}

	searched := make(chan float64, 1)
	go func() { searched <- snapshotScore(t, idx, ws, target) }()
	select {
	case got := <-searched:
		if got != want {
			t.Errorf("the search scored the target %v while the writer was parked mid-commit, want the committed state's %v", got, want)
		}
	case <-time.After(10 * time.Second):
		t.Error("a search did not complete while a writer held the index lock: searches must not take idx.mu")
	}

	g.release <- struct{}{}
	if err := <-wrote; err != nil {
		t.Fatal(err)
	}
}
