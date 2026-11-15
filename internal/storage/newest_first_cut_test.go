package storage

import (
	"bytes"
	"context"
	"fmt"
	"testing"
	"time"
)

// One tie rule (honest clock, increment 1): wherever a candidate source cuts a
// list of EQUALLY-ranked entries at a limit, the newest (ULID descending)
// survive, newest first. These sources run before recall's final sort, so an
// oldest-first cut here silently hides the newest of a set of equals no matter
// what the final comparator does.

func ulidDesc(ids []ULID) []ULID {
	out := append([]ULID(nil), ids...)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && bytes.Compare(out[j][:], out[j-1][:]) > 0; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// TestRecentActive_TiedBucketCutIsNewestFirst: every engram written at the
// same relevance lands in the same 0x10 bucket; the topK cut inside that
// bucket used to walk ascending ULIDs and keep the OLDEST.
func TestRecentActive_TiedBucketCutIsNewestFirst(t *testing.T) {
	store := openTestStore(t)
	ws := store.VaultPrefix("recent-active-tie")
	ctx := context.Background()
	now := time.Now()
	var ids []ULID
	for i := 0; i < 30; i++ {
		created := now.Add(-time.Duration(30-i) * time.Hour)
		id, err := store.WriteEngram(ctx, ws, &Engram{Concept: "c", Content: "c", CreatedAt: created, LastAccess: created})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	got, err := store.RecentActive(ctx, ws, 8)
	if err != nil {
		t.Fatal(err)
	}
	want := ulidDesc(ids)[:8]
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("RecentActive over one tied bucket kept\n%v\nwant the 8 newest newest-first\n%v", got, want)
	}
}

// TestGetTopTransitions_TiedCountsAreNewestFirst covers both the Pebble scan
// and the in-memory TransitionCache: equal transition counts at the topK cut
// keep the newest targets.
func TestGetTopTransitions_TiedCountsAreNewestFirst(t *testing.T) {
	store := openTestStore(t)
	ws := store.VaultPrefix("transition-tie")
	ctx := context.Background()
	src := NewULID()
	now := time.Now()
	var dsts []ULID
	var sets []TransitionSet
	for i := 0; i < 10; i++ {
		d := NewULIDWithTime(now.Add(-time.Duration(10-i) * time.Hour))
		dsts = append(dsts, d)
		sets = append(sets, TransitionSet{WS: ws, Src: src, Dst: d, Count: 3})
	}
	if err := store.SetTransitionBatch(ctx, sets); err != nil {
		t.Fatal(err)
	}
	want := ulidDesc(dsts)[:4]
	render := func(ts []TransitionTarget) []ULID {
		out := make([]ULID, len(ts))
		for i, x := range ts {
			out[i] = ULID(x.ID)
		}
		return out
	}
	got, err := store.GetTopTransitions(ctx, ws, src, 4)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(render(got)) != fmt.Sprint(want) {
		t.Errorf("pebble GetTopTransitions tied cut kept %v, want %v", render(got), want)
	}
	tc := NewTransitionCache(store)
	defer tc.Close()
	for run := 0; run < 10; run++ {
		got, err := tc.GetTopTransitions(ctx, ws, src, 4)
		if err != nil {
			t.Fatal(err)
		}
		if fmt.Sprint(render(got)) != fmt.Sprint(want) {
			t.Fatalf("cache GetTopTransitions tied cut kept %v, want %v", render(got), want)
		}
	}
}
