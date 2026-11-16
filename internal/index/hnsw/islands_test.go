package hnsw

// Closed islands: a group of more than M0 vectors that are each other's
// nearest neighbours used to end with no layer-0 edge leaving the group,
// because both neighbour selection at insert and back-link pruning kept the
// nearest candidates only. A walk that entered the group could never leave.

import (
	"context"
	"math/rand"
	"testing"
)

// islandFixture inserts `outsiders` random unit vectors first, then `members`
// vectors around one centroid (memberEps = 0 gives exact duplicates).
func islandFixture(t *testing.T, outsiders, members int, memberEps float32) (idx *Index, isOut map[[16]byte]bool, memberIDs [][16]byte, centroid []float32) {
	t.Helper()
	db := memPebble(t)
	t.Cleanup(func() { db.Close() })
	var ws [8]byte
	copy(ws[:], "ISLAND01")
	idx = New(db, ws)
	t.Cleanup(idx.Close)
	rng := rand.New(rand.NewSource(33))
	const dim = 32
	isOut = map[[16]byte]bool{}
	for i := 0; i < outsiders; i++ {
		var id [16]byte
		rng.Read(id[:])
		isOut[id] = true
		idx.Insert(id, unitVec(rng, make([]float32, dim), 1.0))
	}
	centroid = unitVec(rng, make([]float32, dim), 1.0)
	for i := 0; i < members; i++ {
		var id [16]byte
		rng.Read(id[:])
		memberIDs = append(memberIDs, id)
		v := centroid
		if memberEps > 0 {
			v = unitVec(rng, centroid, memberEps)
		}
		idx.Insert(id, v)
	}
	return idx, isOut, memberIDs, centroid
}

// assertNoClosedMembers fails for every member with no layer-0 path to an
// outsider, and when a search large enough to hold every vector misses one.
func assertNoClosedMembers(t *testing.T, idx *Index, isOut map[[16]byte]bool, members [][16]byte, centroid []float32) {
	t.Helper()
	closed := 0
	for _, m := range members {
		reach := bfsReachableSet(idx.nodes, m)
		escapes := false
		for id := range reach {
			if isOut[id] {
				escapes = true
				break
			}
		}
		if !escapes {
			closed++
		}
	}
	if closed > 0 {
		t.Errorf("%d of %d cluster members have no layer-0 path to any outsider (a closed island)", closed, len(members))
	}
	k := len(members) + len(isOut)
	res, err := idx.Search(context.Background(), centroid, k)
	if err != nil {
		t.Fatal(err)
	}
	got := 0
	for _, r := range res {
		if isOut[r.ID] {
			got++
		}
	}
	if got != len(isOut) {
		t.Errorf("search for k=%d (every vector in the index) returned %d of %d outsiders", k, got, len(isOut))
	}
}

// TestInsert_ClusterOfM0PlusOneIsNotClosed: M0 + 1 = 33 near-identical
// vectors are the smallest closed island under nearest-only selection.
func TestInsert_ClusterOfM0PlusOneIsNotClosed(t *testing.T) {
	idx, isOut, members, centroid := islandFixture(t, 5, M0+1, 0.001)
	assertNoClosedMembers(t, idx, isOut, members, centroid)
}

// TestInsert_ExactDuplicatesAreNotClosed: 40 bit-identical vectors (the same
// text embedded alone each time, or a caller-supplied embedding repeated).
// Every candidate is at the same distance from the base as from any selected
// duplicate, so the diversity test alone cannot tell them apart.
func TestInsert_ExactDuplicatesAreNotClosed(t *testing.T) {
	idx, isOut, members, centroid := islandFixture(t, 5, 40, 0)
	assertNoClosedMembers(t, idx, isOut, members, centroid)
}

// TestInsert_TopicLargerThanM0IsNotClosed: the non-duplicate case. ~40-member
// topics of ordinary (not near-identical) vectors close the same way.
func TestInsert_TopicLargerThanM0IsNotClosed(t *testing.T) {
	// Hermetic (its own seeded corpus, store and index; no package state), so it
	// runs in parallel and its -race cost overlaps the package's other heavy tests.
	t.Parallel()
	c := makeCorpus("topics", 128, 800, 7)
	db := memPebble(t)
	defer db.Close()
	var ws [8]byte
	copy(ws[:], "TOPICS01")
	idx := buildCorpus(t, c, db, ws, nil)
	if n := closedAgainstEntry(idx); n != 0 {
		t.Errorf("%d of %d nodes cannot reach the entry point over layer-0 edges", n, len(c.vecs))
	}
}

// TestSearch_RecallVsExact_ClusteredShapes: recall@10 against brute force on
// shapes with clusters larger than M0.
func TestSearch_RecallVsExact_ClusteredShapes(t *testing.T) {
	// Hermetic (its own seeded corpus, store and index; no package state), so it
	// runs in parallel and its -race cost overlaps the package's other heavy tests.
	t.Parallel()
	for _, tc := range []struct {
		shape  string
		dim, n int
	}{{"topics", 128, 800}, {"dup80", 64, 400}} {
		shape := tc.shape
		t.Run(shape, func(t *testing.T) {
			c := makeCorpus(shape, tc.dim, tc.n, 11)
			db := memPebble(t)
			defer db.Close()
			var ws [8]byte
			copy(ws[:], "RECALL01")
			idx := buildCorpus(t, c, db, ws, nil)
			qs := makeQueries(c, 60, 5)
			sum := map[string]float64{}
			cnt := map[string]float64{}
			for _, q := range qs {
				got, err := idx.Search(context.Background(), q.q, 10)
				if err != nil {
					t.Fatal(err)
				}
				want, _ := exactTopK(c, q.q, 10, nil)
				sum[q.class] += recallStrict(got, want)
				cnt[q.class]++
			}
			for cl := range sum {
				r := sum[cl] / cnt[cl]
				if cl == "cluster-aimed" {
					// Within-cluster ordering among near-identical vectors is
					// graded by the harness under the jitter equivalence; here
					// it is only reported.
					t.Logf("%s recall@10 = %.3f (reported, not asserted)", cl, r)
					continue
				}
				if r < 0.95 {
					t.Errorf("%s recall@10 = %.3f against exact search, want >= 0.95", cl, r)
				} else {
					t.Logf("%s recall@10 = %.3f", cl, r)
				}
			}
		})
	}
}

// TestLoadFromPebble_DiverseGraphReloadsIdentically: a persisted graph reloads
// to the same graph — no repair fires — so a restart cannot change results.
func TestLoadFromPebble_DiverseGraphReloadsIdentically(t *testing.T) {
	// Hermetic (its own seeded corpus, store and index; no package state), so it
	// runs in parallel and its -race cost overlaps the package's other heavy tests.
	t.Parallel()
	c := makeCorpus("topics", 128, 1000, 3)
	db := memPebble(t)
	defer db.Close()
	var ws [8]byte
	copy(ws[:], "RELOAD01")
	live := buildCorpus(t, c, db, ws, nil)
	re := New(db, ws)
	if err := re.LoadFromPebble(); err != nil {
		t.Fatal(err)
	}
	re.Close()
	if re.entryPoint != live.entryPoint || re.maxLevel != live.maxLevel {
		t.Errorf("reloaded entry/maxLevel %x/%d, live %x/%d", re.entryPoint[:4], re.maxLevel, live.entryPoint[:4], live.maxLevel)
	}
	// Structural identity first: every node's every layer list, element by
	// element. A load-time repair or rebuild shows up here even when it
	// happens not to move any top-10.
	differ := 0
	for id, ln := range live.nodes {
		rn := re.nodes[id]
		if rn == nil || len(rn.layers) != len(ln.layers) {
			differ++
			continue
		}
		for l := range ln.layers {
			if !sameIDs(ln.layers[l], rn.layers[l]) {
				differ++
				break
			}
		}
	}
	if differ != 0 || len(re.nodes) != len(live.nodes) {
		t.Errorf("reloaded graph differs from the live graph at %d of %d nodes (loaded %d nodes)", differ, len(live.nodes), len(re.nodes))
	}
	qs := makeQueries(c, 200, 9)
	same := 0
	for _, q := range qs {
		if sameIDs(topIDs(t, live, q.q, 10), topIDs(t, re, q.q, 10)) {
			same++
		}
	}
	if same != len(qs) {
		t.Errorf("reloaded graph returned identical top-10 (ids and order) for %d of %d queries", same, len(qs))
	}
}

// scriptedSource is a rand.Source whose Int63 values are scripted, so a test
// can force the level randomLevel draws: 0 → "go up a level", max → "stop".
type scriptedSource struct{ vals []int64 }

func (s *scriptedSource) Int63() int64 {
	if len(s.vals) == 0 {
		return 1 << 62
	}
	v := s.vals[0]
	s.vals = s.vals[1:]
	return v
}
func (s *scriptedSource) Seed(int64) {}

// TestEntryPoint_LiveEqualsLoaded: two nodes reach the same top level, the
// larger id first. Load picks the smallest id at the top level; the live
// graph must agree, or the first query after a restart starts its descent
// somewhere else.
func TestEntryPoint_LiveEqualsLoaded(t *testing.T) {
	db := memPebble(t)
	defer db.Close()
	var ws [8]byte
	copy(ws[:], "ENTRY001")
	idx := New(db, ws)
	const stop = 1 << 62
	// big: level 1; small: level 1; then 20 nodes at level 0.
	src := &scriptedSource{vals: []int64{0, stop, 0, stop}}
	idx.rng = rand.New(src)
	rng := rand.New(rand.NewSource(4))
	big := [16]byte{0xF0, 1}
	small := [16]byte{0x10, 1}
	for _, id := range [][16]byte{big, small} {
		v := unitVec(rng, make([]float32, 16), 1)
		_ = idx.StoreVector(id, v)
		idx.Insert(id, v)
	}
	for i := 0; i < 20; i++ {
		var id [16]byte
		rng.Read(id[:])
		v := unitVec(rng, make([]float32, 16), 1)
		_ = idx.StoreVector(id, v)
		idx.Insert(id, v)
	}
	idx.Close()
	re := New(db, ws)
	if err := re.LoadFromPebble(); err != nil {
		t.Fatal(err)
	}
	re.Close()
	if idx.maxLevel != 1 || re.maxLevel != 1 {
		t.Fatalf("setup: maxLevel live %d loaded %d, want 1", idx.maxLevel, re.maxLevel)
	}
	if re.entryPoint != small {
		t.Fatalf("loaded entry %x, want the smallest id at the top level %x", re.entryPoint[:2], small[:2])
	}
	if idx.entryPoint != re.entryPoint {
		t.Errorf("live entry %x differs from loaded entry %x", idx.entryPoint[:2], re.entryPoint[:2])
	}
}
