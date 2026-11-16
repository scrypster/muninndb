package hnsw_test

import (
	"context"
	"testing"

	"github.com/scrypster/muninndb/internal/index/hnsw"
)

// TestSearch_ExactDistanceTiesAreNewestFirst: identical vectors score exactly
// the same distance to any query, so which of them survive the top-k cut is
// decided by the tie rule alone. Recall's one tie rule (honest clock,
// increment 1) is newest-first: ULID descending. With every node inside the
// beam (n <= ef), the result must be the k highest ids, highest first, on
// every call. (With more exact duplicates than the beam holds, which tied
// nodes the approximate walk VISITS depends on graph order; the rule then
// applies among those visited.)
func TestSearch_ExactDistanceTiesAreNewestFirst(t *testing.T) {
	db := newTestDB(t)
	idx := hnsw.New(db, testWS())
	defer idx.Close()

	vec := []float32{0.3, 0.4, 0.5, 0.7}
	const n, k = 30, 5
	for i := 0; i < n; i++ {
		id := [16]byte{0x01, byte(i)}
		insertVector(t, idx, db, id, vec)
	}
	for call := 0; call < 10; call++ {
		got, err := idx.Search(context.Background(), vec, k)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != k {
			t.Fatalf("got %d results, want %d", len(got), k)
		}
		for i, r := range got {
			if want := ([16]byte{0x01, byte(n - 1 - i)}); r.ID != want {
				t.Fatalf("call %d: tied result %d = %x, want %x (the %d-th highest id: ties break newest-first)", call, i, r.ID, want, i)
			}
		}
	}
}
