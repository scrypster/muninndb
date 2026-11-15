package hnsw_test

import (
	"context"
	"math"
	"math/rand"
	"reflect"
	"testing"

	"github.com/cockroachdb/pebble"
	"github.com/cockroachdb/pebble/vfs"
	"github.com/scrypster/muninndb/internal/index/hnsw"
)

const liveDim = 64

func memDB(t *testing.T) *pebble.DB {
	t.Helper()
	db, err := pebble.Open("hnsw-live", &pebble.Options{FS: vfs.NewMem()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func nearVec(rng *rand.Rand, base []float32, eps float32) []float32 {
	n := randomUnitVector(rng, len(base))
	v := make([]float32, len(base))
	var s float64
	for i := range v {
		v[i] = base[i] + eps*n[i]
		s += float64(v[i]) * float64(v[i])
	}
	return normalize(v, s)
}

func normalize(v []float32, s float64) []float32 {
	if s == 0 {
		return v
	}
	n := float32(1 / math.Sqrt(s))
	for i := range v {
		v[i] *= n
	}
	return v
}

// starvationIndex builds bg random vectors, rel relevant ones around a topic
// (eps 0.8) and dup near-duplicates on the topic (eps 0.25) — the shape an
// evolve chain of dup versions leaves in the graph. It returns the index, the
// relevant ids, the duplicate ids and a query on the topic.
func starvationIndex(t *testing.T, bg, rel, dup int) (*hnsw.Index, map[[16]byte]bool, map[[16]byte]bool, []float32) {
	t.Helper()
	db := memDB(t)
	idx := hnsw.NewWithParams(db, testWS(), 64, 0)
	t.Cleanup(idx.Close)
	rng := rand.New(rand.NewSource(42))
	topic := randomUnitVector(rng, liveDim)
	for i := 0; i < bg; i++ {
		idx.Insert(newID(rng), randomUnitVector(rng, liveDim))
	}
	relIDs := map[[16]byte]bool{}
	for i := 0; i < rel; i++ {
		id := newID(rng)
		relIDs[id] = true
		idx.Insert(id, nearVec(rng, topic, 0.8))
	}
	dupIDs := map[[16]byte]bool{}
	for i := 0; i < dup; i++ {
		id := newID(rng)
		dupIDs[id] = true
		idx.Insert(id, nearVec(rng, topic, 0.25))
	}
	return idx, relIDs, dupIDs, nearVec(rand.New(rand.NewSource(7)), topic, 0.25)
}

func countIn(hits []hnsw.ScoredID, set map[[16]byte]bool) int {
	n := 0
	for _, h := range hits {
		if set[h.ID] {
			n++
		}
	}
	return n
}

// With nothing retired, SearchLive is Search: same ids, same order, same
// scores, for every k. (The engine does not even call SearchLive for a vault
// with an empty retired set; this pins that the two walks agree anyway.)
func TestSearchLive_NoRetiredMatchesSearch(t *testing.T) {
	idx, _, _, _ := starvationIndex(t, 1500, 0, 0)
	rng := rand.New(rand.NewSource(9))
	ctx := context.Background()
	for q := 0; q < 40; q++ {
		query := randomUnitVector(rng, liveDim)
		for _, k := range []int{10, 45, 200} {
			want, err := idx.Search(ctx, query, k)
			if err != nil {
				t.Fatal(err)
			}
			got, shadows, stats, err := idx.SearchLive(ctx, query, k, func([16]byte) bool { return false }, 16)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("query %d k=%d: SearchLive != Search\n live   %v\n search %v", q, k, got, want)
			}
			if len(shadows) != 0 || stats.RetiredSeen != 0 || stats.ExactScan {
				t.Fatalf("query %d k=%d: no retired nodes, yet shadows=%d stats=%+v", q, k, len(shadows), stats)
			}
		}
	}
}

// The index-level form of the slot defect, and its fix. 500 retired near-
// duplicates on the query's topic: Search(k=45) returns almost nothing but
// them; SearchLive returns all 20 relevant live vectors, keeps the nearest
// retired ones as shadows on their own budget, and stays inside its bound.
func TestSearchLive_RetiredClusterDoesNotTakeSlots(t *testing.T) {
	idx, rel, dup, q := starvationIndex(t, 1980, 20, 500)
	ctx := context.Background()
	retired := func(id [16]byte) bool { return dup[id] }

	plain, err := idx.Search(ctx, q, 45)
	if err != nil {
		t.Fatal(err)
	}
	live, shadows, stats, err := idx.SearchLive(ctx, q, 45, retired, 16)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("Search(k=45): %d relevant, %d retired | SearchLive: %d relevant, %d retired in live, %d shadows, stats=%+v",
		countIn(plain, rel), countIn(plain, dup), countIn(live, rel), countIn(live, dup), len(shadows), stats)

	if countIn(live, dup) != 0 {
		t.Errorf("SearchLive returned %d retired ids among live results", countIn(live, dup))
	}
	if got := countIn(live, rel); got != 20 {
		t.Errorf("SearchLive reached %d of 20 relevant live vectors", got)
	}
	if len(live) != 45 {
		t.Errorf("SearchLive returned %d live hits, want k=45", len(live))
	}
	if len(shadows) != 16 || countIn(shadows, dup) != 16 {
		t.Errorf("shadows = %d (%d retired), want the nearest 16 retired", len(shadows), countIn(shadows, dup))
	}
	for i := 1; i < len(shadows); i++ {
		if shadows[i].Score > shadows[i-1].Score {
			t.Errorf("shadows not nearest-first at %d", i)
		}
	}
	if stats.RetiredExpanded > stats.RetiredBudget {
		t.Errorf("retired expansions %d exceeded the budget %d", stats.RetiredExpanded, stats.RetiredBudget)
	}
	// The budget is derived from the index: max(ef, N/M0).
	want := (1980 + 20 + 500) / hnsw.M0
	if want < hnsw.EfSearch {
		want = hnsw.EfSearch
	}
	if stats.RetiredBudget != want {
		t.Errorf("retired budget = %d, want max(ef, N/M0) = %d", stats.RetiredBudget, want)
	}
}

// Tombstoned (hard-deleted) nodes route but are never results and never
// shadows.
func TestSearchLive_TombstonedNeverReturned(t *testing.T) {
	idx, _, dup, q := starvationIndex(t, 300, 20, 40)
	for id := range dup {
		idx.Tombstone(id)
	}
	live, shadows, _, err := idx.SearchLive(context.Background(), q, 30, func([16]byte) bool { return false }, 16)
	if err != nil {
		t.Fatal(err)
	}
	if countIn(live, dup) != 0 || len(shadows) != 0 {
		t.Errorf("tombstoned ids surfaced: %d live, %d shadows", countIn(live, dup), len(shadows))
	}
	if len(live) != 30 {
		t.Errorf("got %d live hits, want 30", len(live))
	}
}
