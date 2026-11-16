package fts

import (
	"context"
	"fmt"
	"reflect"
	"testing"

	"github.com/scrypster/muninndb/internal/storage/keys"
)

func ftsID(i int) [16]byte {
	var id [16]byte
	id[0] = 0x01
	id[14] = byte(i >> 8)
	id[15] = byte(i)
	return id
}

// liveFixture indexes 20 live documents that mention the query term once in
// content and 60 retired versions that carry it twice in the concept, so every
// retired version out-scores every live document.
func liveFixture(t *testing.T) (*Index, [8]byte, map[[16]byte]bool, map[[16]byte]bool) {
	t.Helper()
	db, cleanup := openTestDB(t)
	t.Cleanup(cleanup)
	idx := New(db)
	ws := keys.VaultPrefix("fts-live")
	live, retired := map[[16]byte]bool{}, map[[16]byte]bool{}
	for i := 0; i < 20; i++ {
		id := ftsID(i)
		live[id] = true
		if err := idx.IndexEngram(ws, id, fmt.Sprintf("shelter note %d", i), "", fmt.Sprintf("the lantern at shelter %d needs a new mantle", i), nil); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 60; i++ {
		id := ftsID(1000 + i)
		retired[id] = true
		if err := idx.IndexEngram(ws, id, "lantern lantern roster", "", fmt.Sprintf("rota version %d", i), nil); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 40; i++ {
		if err := idx.IndexEngram(ws, ftsID(2000+i), fmt.Sprintf("culvert survey %d", i), "", "drainage check found the ditch clear", nil); err != nil {
			t.Fatal(err)
		}
	}
	return idx, ws, live, retired
}

func TestSearchLive_NoRetiredMatchesSearch(t *testing.T) {
	idx, ws, _, _ := liveFixture(t)
	ctx := context.Background()
	for _, q := range []string{"lantern", "lantern roster", "drainage ditch", "shelter mantle", "absentword"} {
		for _, k := range []int{5, 33, 0} {
			want, err := idx.Search(ctx, ws, q, k)
			if err != nil {
				t.Fatal(err)
			}
			got, shadows, err := idx.SearchLive(ctx, ws, q, k, func([16]byte) bool { return false }, 16)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) == 0 && len(want) == 0 {
				continue
			}
			if !reflect.DeepEqual(got, want) || len(shadows) != 0 {
				t.Fatalf("%q k=%d: SearchLive != Search\n live %v\n want %v", q, k, got, want)
			}
		}
	}
}

func TestSearchLive_RetiredPostingsDoNotTakeSlots(t *testing.T) {
	idx, ws, live, retired := liveFixture(t)
	ctx := context.Background()
	plain, err := idx.Search(ctx, ws, "lantern", 33)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, r := range plain {
		if live[r.ID] {
			n++
		}
	}
	got, shadows, err := idx.SearchLive(ctx, ws, "lantern", 33, func(id [16]byte) bool { return retired[id] }, 16)
	if err != nil {
		t.Fatal(err)
	}
	m := 0
	for _, r := range got {
		if retired[r.ID] {
			t.Fatalf("retired %x among live results", r.ID)
		}
		if live[r.ID] {
			m++
		}
	}
	t.Logf("Search(k=33): %d of 20 live | SearchLive: %d of 20 live, %d shadows", n, m, len(shadows))
	if m != 20 {
		t.Errorf("SearchLive returned %d of 20 live matches", m)
	}
	if len(shadows) != 16 {
		t.Errorf("shadows = %d, want the shadow budget 16", len(shadows))
	}
	for i := 1; i < len(shadows); i++ {
		if shadows[i].Score > shadows[i-1].Score {
			t.Errorf("shadows not best-first at %d", i)
		}
	}
	// Scores are the documents' own: a live document scores the same through
	// either entry point.
	want := map[[16]byte]float64{}
	all, _ := idx.Search(ctx, ws, "lantern", 0)
	for _, r := range all {
		want[r.ID] = r.Score
	}
	for _, r := range got {
		if r.Score != want[r.ID] {
			t.Errorf("live score changed for %x: %v vs %v", r.ID, r.Score, want[r.ID])
		}
	}
}
