package fts

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cockroachdb/pebble"
	"github.com/cockroachdb/pebble/vfs"
	"github.com/cockroachdb/pebble/vfs/errorfs"
	"github.com/scrypster/muninndb/internal/storage/keys"
)

// A read that fails is not a read that found nothing. These tests inject Pebble
// read errors (pebble's own vfs/errorfs) and pin both directions of that:
//
//   - a SEARCH whose reads fail returns the error. It used to return zero
//     matches and a nil error — corpus statistics defaulted, every word scored
//     as never seen, a failed posting scan discarded — which is a confident
//     empty answer the caller cannot tell from a real miss.
//   - a WRITE whose read-modify-write cannot read does not write. It used to
//     treat the unreadable count as zero and commit 1 over it: one transient
//     read error took a word's document frequency from 6 to 1 and the vault's
//     document count from 6 to 1, durably, and reported success.

// openFaultyDB returns an in-memory Pebble whose sstable reads fail while the
// returned switch is on. The block cache is one byte so reads reach the file.
func openFaultyDB(t *testing.T) (*pebble.DB, *atomic.Bool) {
	t.Helper()
	armed := new(atomic.Bool)
	inj := errorfs.InjectorFunc(func(op errorfs.Op, path string) error {
		if armed.Load() && op.OpKind() == errorfs.OpKindRead && strings.HasSuffix(path, ".sst") {
			return errorfs.ErrInjected
		}
		return nil
	})
	cache := pebble.NewCache(1)
	db, err := pebble.Open("faulty", &pebble.Options{FS: errorfs.Wrap(vfs.NewMem(), inj), Cache: cache})
	if err != nil {
		cache.Unref()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		armed.Store(false)
		db.Close()
		cache.Unref()
	})
	return db, armed
}

func TestSearch_ReadErrorIsReturnedNotReportedAsNoMatch(t *testing.T) {
	db, armed := openFaultyDB(t)
	idx := New(db)
	ws := keys.VaultPrefix("read-error")
	ctx := context.Background()

	idfTestIndex(t, idx, ws, 1, 6)
	if err := db.Flush(); err != nil {
		t.Fatal(err)
	}
	const q = "velmora ledger"
	if res, err := idx.Search(ctx, ws, q, 100); err != nil || len(res) != 6 {
		t.Fatalf("setup: Search = %d matches, err %v; want 6, nil", len(res), err)
	}

	armed.Store(true)
	res, err := idx.Search(ctx, ws, q, 100)
	live, shadows, liveErr := idx.SearchLive(ctx, ws, q, 100, func([16]byte) bool { return false }, 3)
	armed.Store(false)

	if err == nil {
		t.Errorf("Search with failing reads returned %d matches and a nil error — a failed read reported as \"no match\"", len(res))
	} else if !errors.Is(err, errorfs.ErrInjected) {
		t.Errorf("Search error = %v, want it to wrap the read error", err)
	}
	if len(res) != 0 {
		t.Errorf("Search with failing reads returned %d matches alongside its error, want none", len(res))
	}
	if liveErr == nil {
		t.Errorf("SearchLive with failing reads returned %d+%d matches and a nil error", len(live), len(shadows))
	}

	// The failure is not sticky: nothing was remembered from the failed search.
	if res, err := idx.Search(ctx, ws, q, 100); err != nil || len(res) != 6 {
		t.Errorf("after the reads recover: Search = %d matches, err %v; want 6, nil", len(res), err)
	}
}

func TestIndexWrites_ReadErrorDoesNotResetCounts(t *testing.T) {
	db, armed := openFaultyDB(t)
	idx := New(db)
	ws := keys.VaultPrefix("write-read-error")

	idfTestIndex(t, idx, ws, 1, 6)
	for i := 0; i < 3; i++ {
		if err := idx.IndexEngram(ws, [16]byte{0xB0, byte(i)}, "Thrennic kiln", "", "The Thrennic glassworks fires a kiln.", []string{"glassworks"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Flush(); err != nil {
		t.Fatal(err)
	}
	df := func(word string) uint32 {
		t.Helper()
		v, _, err := readDF(db, ws, Tokenize(word)[0])
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	if df("velmora") != 6 || df("glassworks") != 3 || mustStats(t, idx, ws).TotalEngrams != 9 {
		t.Fatalf("setup: df(velmora)=%d df(glassworks)=%d N=%d, want 6, 3, 9", df("velmora"), df("glassworks"), mustStats(t, idx, ws).TotalEngrams)
	}

	// A first index, a retag that brings in a word other documents hold, and a
	// bare stats update, each with every read failing.
	id7, concept, content, tags := idfTestDoc(7)
	id1, c1, ct1, tags1 := idfTestDoc(1)
	prev := Document{Concept: c1, Content: ct1, Tags: tags1}
	next := prev
	next.Tags = []string{"glassworks"}

	armed.Store(true)
	indexErr := idx.IndexEngram(ws, id7, concept, "", content, tags)
	reindexErr := idx.ReindexEngram(ws, id1, prev, next)
	statsErr := idx.UpdateStats(ws, 5)
	armed.Store(false)

	for name, err := range map[string]error{"IndexEngram": indexErr, "ReindexEngram": reindexErr, "UpdateStats": statsErr} {
		if err == nil {
			t.Errorf("%s with failing reads returned nil — it wrote counts derived from a read that failed", name)
		}
	}
	if got := df("velmora"); got != 6 {
		t.Errorf("df(velmora) = %d after the failed writes, want 6 unchanged", got)
	}
	if got := df("glassworks"); got != 3 {
		t.Errorf("df(glassworks) = %d after the failed retag, want 3 unchanged", got)
	}
	if got := mustStats(t, idx, ws).TotalEngrams; got != 9 {
		t.Errorf("N = %d after the failed writes, want 9 unchanged", got)
	}
	if has, err := idx.HasPostings(ws, id7, Document{Concept: concept, Content: content, Tags: tags}); err != nil || has {
		t.Errorf("the document whose index failed has postings=%v (err %v), want none: a failed index must not half-land", has, err)
	}
	if res, err := idx.Search(context.Background(), ws, "velmora", 100); err != nil || len(res) != 6 {
		t.Errorf("after the failed writes: Search(velmora) = %d matches, err %v; want 6, nil", len(res), err)
	}
}

// The two tests above fail EVERY read at once, so the first guarded read hides
// every guard behind it: five of the seven read-error checks could each be
// deleted with those tests still green. The sweeps below fail exactly ONE read —
// the k-th, for every k a search or a write makes — so each read's own guard is
// the only thing between its failure and a wrong answer.
//
// They count the reads K instead of assuming a number, and refuse to run on a
// K too small to mean anything, so a storage-engine upgrade that changes how
// many reads an operation makes cannot quietly leave them testing nothing.

// singleFault fails exactly the k-th sstable read after it is armed. With k = 0
// it only counts.
type singleFault struct {
	armed atomic.Bool
	n, k  atomic.Int64
	fired atomic.Bool
}

func (f *singleFault) inject(op errorfs.Op, path string) error {
	if !f.armed.Load() || op.OpKind() != errorfs.OpKindRead || !strings.HasSuffix(path, ".sst") {
		return nil
	}
	if n := f.n.Add(1); n == f.k.Load() {
		f.fired.Store(true)
		return errorfs.ErrInjected
	}
	return nil
}

func openSingleFaultDB(t *testing.T, f *singleFault) *pebble.DB {
	t.Helper()
	cache := pebble.NewCache(1)
	db, err := pebble.Open("single-fault", &pebble.Options{
		FS:                          errorfs.Wrap(vfs.NewMem(), errorfs.InjectorFunc(f.inject)),
		Cache:                       cache,
		DisableAutomaticCompactions: true, // a background read must not take the fault
	})
	if err != nil {
		cache.Unref()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		f.armed.Store(false)
		db.Close()
		cache.Unref()
	})
	return db
}

// singleFaultFixture indexes the same twelve documents on every call and flushes
// them to one sstable. Their words stem ("ledgers", "running", "crates"), so a
// search also makes the unstemmed-form reads of a vault with no schema marker.
func singleFaultFixture(t *testing.T, idx *Index, db *pebble.DB, ws [8]byte) {
	t.Helper()
	for i := 0; i < 12; i++ {
		content := "the tide office logged barges at dusk"
		if i%3 == 0 {
			content = "widget crates counted beside quays"
		}
		if err := idx.IndexEngram(ws, [16]byte{0xC0, byte(i)}, fmt.Sprintf("harbor ledgers running %d", i), "", content, []string{"kestrel"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Flush(); err != nil {
		t.Fatal(err)
	}
}

func storeDigest(t *testing.T, db *pebble.DB) [32]byte {
	t.Helper()
	it, err := db.NewIter(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer it.Close()
	h := sha256.New()
	for it.First(); it.Valid(); it.Next() {
		h.Write(it.Key())
		h.Write([]byte{0})
		h.Write(it.Value())
		h.Write([]byte{1})
	}
	if err := it.Error(); err != nil {
		t.Fatal(err)
	}
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

// singleFaultLegacyDoc adds one document the way a pre-stemming build indexed
// it: postings and frequency records under its UNSTEMMED words. It is what makes
// the unstemmed-form reads matter. Without it a failed unstemmed read that was
// taken for "no record" would be right by coincidence, and so would a failed
// schema-marker read taken for "no marker".
func singleFaultLegacyDoc(t *testing.T, idx *Index, db *pebble.DB, ws [8]byte) {
	t.Helper()
	id := [16]byte{0xCF, 1}
	batch := db.NewBatch()
	for _, raw := range []string{"ledgers", "running"} {
		if err := batch.Set(keys.FTSPostingKey(ws, raw, FieldConcept, id), encodePosting(PostingValue{TF: 1, Field: FieldConcept, DocLen: 2}), nil); err != nil {
			t.Fatal(err)
		}
		var df [8]byte
		df[3] = 1
		if err := batch.Set(keys.TermStatsKey(ws, raw), df[:], nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := batch.Commit(pebble.Sync); err != nil {
		t.Fatal(err)
	}
	if err := idx.UpdateStats(ws, 2); err != nil {
		t.Fatal(err)
	}
}

// TestSearch_AnySingleReadFaultIsAnErrorOrTheExactAnswer: whichever one read of
// a search fails, the search returns that error and no results. Never a partial
// result, never an empty success, never the right answer reached past an
// unchecked error, never an open snapshot left behind.
//
// Two vaults, because two of the reads only matter in one of them. In a vault
// with no schema marker the unstemmed-form frequency reads decide whether the
// legacy document matches. In a reindexed vault (marker = 1) the marker read
// decides whether those forms are consulted at all. Every search uses a fresh
// Index, as the first search of a process does, so the marker is really read.
func TestSearch_AnySingleReadFaultIsAnErrorOrTheExactAnswer(t *testing.T) {
	// Words in every document, in some, stemmed ones, and one the vault lacks.
	const q = "harbor ledgers running widget kestrel quays zarquonmissing"
	ctx := context.Background()

	for _, shape := range []struct {
		name        string
		reindexed   bool
		wantMatches int
	}{
		{name: "no schema marker (unstemmed forms are searched)", reindexed: false, wantMatches: 13},
		{name: "reindexed vault (schema marker 1)", reindexed: true, wantMatches: 12},
	} {
		t.Run(shape.name, func(t *testing.T) {
			f := &singleFault{}
			db := openSingleFaultDB(t, f)
			ws := keys.VaultPrefix("single-fault-search")
			singleFaultFixture(t, New(db), db, ws)
			singleFaultLegacyDoc(t, New(db), db, ws)
			if shape.reindexed {
				if err := db.Set(keys.FTSVersionKey(ws), []byte{0x01}, pebble.Sync); err != nil {
					t.Fatal(err)
				}
			}
			if err := db.Flush(); err != nil {
				t.Fatal(err)
			}

			base, err := New(db).Search(ctx, ws, q, 100)
			if err != nil || len(base) != shape.wantMatches {
				t.Fatalf("fixture: Search = %d matches, err %v; want %d, nil", len(base), err, shape.wantMatches)
			}
			f.n.Store(0)
			f.armed.Store(true)
			_, _ = New(db).Search(ctx, ws, q, 100)
			f.armed.Store(false)
			reads := int(f.n.Load())
			if reads < 8 {
				t.Fatalf("a search made %d sstable reads; this test needs the statistics, schema-marker, frequency and posting reads to be separate fault points", reads)
			}

			failed := 0
			for k := 1; k <= reads+2; k++ {
				idx := New(db)
				f.n.Store(0)
				f.k.Store(int64(k))
				f.fired.Store(false)
				f.armed.Store(true)
				var res []ScoredID
				var err error
				if k%2 == 0 {
					res, err = idx.Search(ctx, ws, q, 100)
				} else {
					var shadows []ScoredID
					res, shadows, err = idx.SearchLive(ctx, ws, q, 100, func(id [16]byte) bool { return id[1] == 3 }, 5)
					res = append(res, shadows...)
					sortScoredIDs(res)
				}
				f.armed.Store(false)

				if k <= reads && !f.fired.Load() {
					t.Fatalf("read %d of %d was never reached: the search does not make the same reads every time", k, reads)
				}
				switch {
				case f.fired.Load() && err != nil && len(res) == 0:
					failed++
					if !errors.Is(err, errorfs.ErrInjected) {
						t.Errorf("read %d of %d failed: the search returned %v, which does not wrap the read error", k, reads, err)
					}
				case !f.fired.Load() && err == nil && reflect.DeepEqual(res, base):
					// k is past the search's last read: nothing was injected
				default:
					t.Errorf("read %d of %d failed (injected: %v): the search returned %d results and err %v; want the error and no results — every read a search makes feeds its answer",
						k, reads, f.fired.Load(), len(res), err)
				}
				if open := db.Metrics().Snapshots.Count; open != 0 {
					t.Errorf("read %d of %d failed: %d snapshots left open", k, reads, open)
				}
			}
			if failed != reads {
				t.Errorf("%d of %d single read faults failed the search, want all of them", failed, reads)
			}
			t.Logf("%d sstable reads per search; failing each in turn: %d searches returned the error, none returned a wrong answer", reads, failed)
		})
	}
}

// TestIndexWrites_AnySingleReadFaultIsAnErrorAndLeavesTheStoreUnchanged: for
// each writer, whichever one of its reads fails, it returns an error and the
// store is byte-for-byte what it was. Not a changed store, and not a clean run
// reached past an unchecked error either: a writer that carries on after a read
// it could not make has decided something on a value it does not have.
func TestIndexWrites_AnySingleReadFaultIsAnErrorAndLeavesTheStoreUnchanged(t *testing.T) {
	ws := keys.VaultPrefix("single-fault-write")
	prev := Document{Concept: "harbor ledgers running 4", Content: "the tide office logged barges at dusk", Tags: []string{"kestrel"}}
	next := prev
	next.Tags = []string{"osprey", "widget"}

	for _, op := range []struct {
		name string
		run  func(idx *Index) error
	}{
		{"IndexEngram", func(idx *Index) error {
			return idx.IndexEngram(ws, [16]byte{0xD0, 1}, "harbor quays", "", "widget crates and a new word glimmerfen", []string{"kestrel"})
		}},
		{"ReindexEngram", func(idx *Index) error { return idx.ReindexEngram(ws, [16]byte{0xC0, 4}, prev, next) }},
		{"UpdateStats", func(idx *Index) error { return idx.UpdateStats(ws, 7) }},
	} {
		t.Run(op.name, func(t *testing.T) {
			// A clean run: how many reads, and what the store looks like after.
			f := &singleFault{}
			db := openSingleFaultDB(t, f)
			idx := New(db)
			singleFaultFixture(t, idx, db, ws)
			before := storeDigest(t, db)
			f.armed.Store(true)
			if err := op.run(idx); err != nil {
				t.Fatal(err)
			}
			f.armed.Store(false)
			reads := int(f.n.Load())
			clean := storeDigest(t, db)
			if reads < 1 || clean == before {
				t.Fatalf("fixture: a clean %s made %d sstable reads and changed the store: %v", op.name, reads, clean != before)
			}

			failed := 0
			for k := 1; k <= reads+1; k++ {
				f := &singleFault{}
				db := openSingleFaultDB(t, f)
				idx := New(db)
				singleFaultFixture(t, idx, db, ws)
				if storeDigest(t, db) != before {
					t.Fatal("fixture: the store is not the same on every build")
				}
				f.k.Store(int64(k))
				f.armed.Store(true)
				err := op.run(idx)
				f.armed.Store(false)
				got := storeDigest(t, db)

				if k <= reads && !f.fired.Load() {
					t.Fatalf("read %d of %d was never reached: %s does not make the same reads every time", k, reads, op.name)
				}
				switch {
				case f.fired.Load() && err != nil && got == before:
					failed++
				case !f.fired.Load() && err == nil && got == clean:
					// k is past the writer's last read: nothing was injected
				default:
					t.Errorf("read %d of %d failed (injected: %v): %s returned err %v, store changed: %v, store equals a clean run: %v; want an error and an unchanged store — every read a writer makes decides what it commits or reports",
						k, reads, f.fired.Load(), op.name, err, got != before, got == clean)
				}
			}
			if failed != reads {
				t.Errorf("%d of %d single read faults failed %s, want all of them", failed, reads, op.name)
			}
			t.Logf("%d sstable reads; failing each in turn: %d returned an error with the store unchanged, none left a changed store", reads, failed)
		})
	}
}
