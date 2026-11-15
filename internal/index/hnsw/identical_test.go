package hnsw

// Bit-identical vectors (the same text embedded alone each time; memories that
// share a long templated opening the embedder truncates to; a caller-supplied
// embedding repeated). Every distance among them ties, so a rule that orders
// ties by id points every list at the same few copies and the rest are never
// reached.

import (
	"context"
	"errors"
	"math/rand"
	"testing"

	"github.com/cockroachdb/pebble"
	"github.com/scrypster/muninndb/internal/rankorder"
	"github.com/scrypster/muninndb/internal/storage/keys"
)

// identicalCorpus: base random vectors (dim 32) plus copies bit-identical
// copies of one vector, in shuffled insertion order.
func identicalCorpus(base, copies int, seed int64) (c *corpus, dupVec []float32, dupIDs map[[16]byte]bool) {
	r := rand.New(rand.NewSource(seed))
	c = &corpus{dim: 32, clusterTp: -1}
	for i := 0; i < base; i++ {
		c.vecs = append(c.vecs, cUnit(cGauss(r, 32, 1)))
	}
	dupVec = cUnit(cGauss(r, 32, 1))
	for i := 0; i < copies; i++ {
		c.vecs = append(c.vecs, dupVec)
	}
	c.ids = make([][16]byte, len(c.vecs))
	dupIDs = map[[16]byte]bool{}
	for i := range c.ids {
		r.Read(c.ids[i][:])
		if i >= base {
			dupIDs[c.ids[i]] = true
		}
	}
	c.inCluster = make([]bool, len(c.vecs))
	c.topic = make([]int, len(c.vecs))
	r.Shuffle(len(c.vecs), func(i, j int) {
		c.vecs[i], c.vecs[j] = c.vecs[j], c.vecs[i]
		c.ids[i], c.ids[j] = c.ids[j], c.ids[i]
	})
	return c, dupVec, dupIDs
}

func assertAllIdenticalReachable(t *testing.T, idx *Index, dupVec []float32, dupIDs map[[16]byte]bool) {
	t.Helper()
	if n := closedAgainstEntry(idx); n != 0 {
		t.Errorf("%d nodes cannot reach the entry point", n)
	}
	idx.mu.RLock()
	fwd := bfsReachableSet(idx.nodes, idx.entryPoint)
	idx.mu.RUnlock()
	unreached := 0
	for id := range dupIDs {
		if !fwd[id] {
			unreached++
		}
	}
	if unreached > 0 {
		t.Errorf("%d of %d identical copies unreachable from the entry point", unreached, len(dupIDs))
	}
	res, err := idx.Search(context.Background(), dupVec, len(dupIDs))
	if err != nil {
		t.Fatal(err)
	}
	got := 0
	for i, r := range res {
		if dupIDs[r.ID] {
			got++
		}
		if i > 0 && res[i-1].Score == r.Score && !rankorder.Before(res[i-1].ID, r.ID) {
			t.Errorf("tied results %d and %d are not in newest-first order", i-1, i)
		}
	}
	if got != len(dupIDs) {
		t.Errorf("search for the copies with k=%d returned %d of %d", len(dupIDs), got, len(dupIDs))
	}
}

// TestIdentical_MoreThanM0CopiesAllReachable: 40, 100 and 200 bit-identical
// copies inside a vault, and a vault holding nothing else.
func TestIdentical_MoreThanM0CopiesAllReachable(t *testing.T) {
	for _, tc := range []struct {
		name         string
		base, copies int
	}{{"40-in-600", 600, 40}, {"100-in-600", 600, 100}, {"200-in-600", 600, 200}, {"vault-of-100", 0, 100}} {
		t.Run(tc.name, func(t *testing.T) {
			c, dupVec, dupIDs := identicalCorpus(tc.base, tc.copies, 41)
			var ws [8]byte
			copy(ws[:], "IDENT001")
			idx := buildCorpus(t, c, nil, ws, nil)
			assertAllIdenticalReachable(t, idx, dupVec, dupIDs)
		})
	}
}

// TestIdentical_RestartCyclesRepairAtMostOnce: a vault with identical copies,
// written by the new rule and by the old one, restarted five times. The first
// start may repair an old graph; no later start repairs anything.
func TestIdentical_RestartCyclesRepairAtMostOnce(t *testing.T) {
	for _, tc := range []struct {
		name   string
		copies int
		legacy bool
	}{{"new-40", 40, false}, {"new-200", 200, false}, {"old-40", 40, true}, {"old-200", 200, true}} {
		t.Run(tc.name, func(t *testing.T) {
			c, dupVec, dupIDs := identicalCorpus(300, tc.copies, 43)
			db := memPebble(t)
			defer db.Close()
			var ws [8]byte
			copy(ws[:], "IDENT002")
			persistCorpus(t, c, db, ws, tc.legacy)
			for boot := 1; boot <= 5; boot++ {
				reg := NewRegistry(db)
				_ = reg.VaultVectors(ws)
				reg.WaitRepairs()
				s := reg.RebuildStatus(ws)
				if boot > 1 && s != nil {
					t.Errorf("boot %d repaired again: %+v", boot, s)
				}
				if boot == 5 {
					assertAllIdenticalReachable(t, reg.get(ws), dupVec, dupIDs)
				}
				reg.Close()
			}
		})
	}
}

// TestInsert_ReinsertEntryPointKeepsGraphSearchable: writing the entry
// point's id again (a re-embed) must not leave the walk starting at a node
// that links to nothing.
func TestInsert_ReinsertEntryPointKeepsGraphSearchable(t *testing.T) {
	c := makeCorpus("uniform", 32, 300, 5)
	var ws [8]byte
	copy(ws[:], "REENTRY1")
	idx := buildCorpus(t, c, nil, ws, nil)
	ep := idx.entryPoint
	r := newRand(6)
	idx.Insert(ep, cUnit(cGauss(r, 32, 1)))
	for i := 0; i < 20; i++ {
		res, err := idx.Search(context.Background(), cUnit(cGauss(r, 32, 1)), 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(res) != 10 {
			t.Fatalf("after re-inserting the entry point a search returned %d of 10", len(res))
		}
	}
}

// TestLoad_VectorlessLeftoversIgnoredAndCleaned: layer keys with no vector
// (written back into a cleared vault, or left by a failed insert) are not
// adopted as nodes — never as the entry point — and are deleted; a vault
// whose only keys are leftovers starts empty and works after new inserts.
func TestLoad_VectorlessLeftoversIgnoredAndCleaned(t *testing.T) {
	db := memPebble(t)
	defer db.Close()
	var ws [8]byte
	copy(ws[:], "LEFTOVR1")
	r := newRand(9)
	junk := func(n int) {
		for i := 0; i < n; i++ {
			var id [16]byte
			r.Read(id[:])
			if i == 0 {
				id[0] = 0 // smallest id, highest layer: the old entry rule's favourite
			}
			for l := 0; l <= 5; l++ {
				if err := db.Set(keys.HNSWNodeKey(ws, id, uint8(l)), encodeNeighbors([][16]byte{id}), pebble.Sync); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	junk(30)
	reg := NewRegistry(db)
	for i := 0; i < 60; i++ {
		var id [16]byte
		r.Read(id[:])
		id[0] |= 0x80
		if err := reg.Insert(context.Background(), ws, id, cUnit(cGauss(r, 16, 1))); err != nil {
			t.Fatal(err)
		}
	}
	res, err := reg.Search(context.Background(), ws, cUnit(cGauss(r, 16, 1)), 10)
	if err != nil || len(res) != 10 {
		t.Errorf("vault whose only prior keys were leftovers: search returned %d results (err %v), want 10", len(res), err)
	}
	reg.Close()
	if n := len(graphKeys(t, db, ws)); n > 60*8 {
		t.Errorf("%d graph keys after the load; the leftovers were not deleted", n)
	}
	reg2 := NewRegistry(db)
	_ = reg2.VaultVectors(ws)
	reg2.WaitRepairs()
	idx := reg2.get(ws)
	if n := idx.Len(); n != 60 {
		t.Errorf("reloaded index holds %d nodes, want 60 (leftovers adopted)", n)
	}
	reg2.Close()
}

// TestSearch_EntryWithoutVectorIsALoudError: a graph whose entry point has no
// vector cannot answer; that must be an error (callers degrade loudly), not
// an empty result that looks like "nothing matched".
func TestSearch_EntryWithoutVectorIsALoudError(t *testing.T) {
	var ws [8]byte
	copy(ws[:], "NOENTRY1")
	idx := New(nil, ws)
	idx.Insert([16]byte{0x90}, []float32{1, 0, 0, 0})
	idx.mu.Lock()
	idx.nodes[[16]byte{0x01}] = &HNSWNode{id: [16]byte{0x01}, layers: [][][16]byte{{}}}
	idx.entryPoint = [16]byte{0x01}
	idx.mu.Unlock()
	_, err := idx.Search(context.Background(), []float32{1, 0, 0, 0}, 5)
	if !errors.Is(err, ErrEntryWithoutVector) {
		t.Errorf("search from a vector-less entry point: err = %v, want ErrEntryWithoutVector", err)
	}
}
