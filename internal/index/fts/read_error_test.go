package fts

import (
	"context"
	"errors"
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
