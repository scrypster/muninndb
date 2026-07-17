package storage

import (
	"context"
	"testing"
	"time"
)

// TestListByStateInRange writes 3 engrams with CreatedAt values spanning a range,
// then calls ListByStateInRange with a window that covers only 2 of them.
// Verifies that exactly 2 IDs are returned.
func TestListByStateInRange(t *testing.T) {
	store := openTestStore(t)
	ws := store.VaultPrefix("range-test")
	ctx := context.Background()

	now := time.Now()

	// Three timestamps: old (before window), in-window-1, in-window-2.
	tOld := now.Add(-3 * time.Hour)
	tIn1 := now.Add(-2 * time.Hour)
	tIn2 := now.Add(-1 * time.Hour)

	// Write engram outside the window.
	engOld := &Engram{
		Concept:   "old engram",
		Content:   "outside the range window",
		CreatedAt: tOld,
	}
	_, err := store.WriteEngram(ctx, ws, engOld)
	if err != nil {
		t.Fatalf("WriteEngram (old): %v", err)
	}

	// Write two engrams inside the window.
	engIn1 := &Engram{
		Concept:   "in-window engram 1",
		Content:   "inside the range window first",
		CreatedAt: tIn1,
	}
	id1, err := store.WriteEngram(ctx, ws, engIn1)
	if err != nil {
		t.Fatalf("WriteEngram (in1): %v", err)
	}

	engIn2 := &Engram{
		Concept:   "in-window engram 2",
		Content:   "inside the range window second",
		CreatedAt: tIn2,
	}
	id2, err := store.WriteEngram(ctx, ws, engIn2)
	if err != nil {
		t.Fatalf("WriteEngram (in2): %v", err)
	}

	// The state index defaults to StateActive (0) for newly written engrams.
	// ListByStateInRange with [tIn1-1ms, tIn2+1ms] should return both in-window IDs.
	since := tIn1.Add(-time.Millisecond)
	until := tIn2.Add(time.Millisecond)

	ids, err := store.ListByStateInRange(ctx, ws, StateActive, since, until, 100)
	if err != nil {
		t.Fatalf("ListByStateInRange: %v", err)
	}

	if len(ids) != 2 {
		t.Errorf("ListByStateInRange returned %d IDs, want 2", len(ids))
	}

	// Verify the returned IDs are the two in-window engrams.
	found := make(map[ULID]bool)
	for _, id := range ids {
		found[id] = true
	}
	if !found[id1] {
		t.Errorf("in-window engram 1 (%v) not in results", id1)
	}
	if !found[id2] {
		t.Errorf("in-window engram 2 (%v) not in results", id2)
	}
}

// TestCountEngrams writes 3 engrams and verifies CountEngrams returns at least 3.
func TestCountEngrams(t *testing.T) {
	store := openTestStore(t)
	ws := store.VaultPrefix("count-engrams-test")
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		if _, err := store.WriteEngram(ctx, ws, &Engram{
			Concept: "concept",
			Content: "content",
		}); err != nil {
			t.Fatalf("WriteEngram[%d]: %v", i, err)
		}
	}

	count, err := store.CountEngrams(ctx)
	if err != nil {
		t.Fatalf("CountEngrams: %v", err)
	}
	if count < 3 {
		t.Errorf("CountEngrams: got %d, want >= 3", count)
	}
}

// TestEngramIDsByCreatedRange writes engrams at distinct timestamps and verifies
// that EngramIDsByCreatedRange returns only those within the specified window.
func TestEngramIDsByCreatedRange(t *testing.T) {
	store := openTestStore(t)
	ws := store.VaultPrefix("ids-by-range-test")
	ctx := context.Background()

	now := time.Now()
	tOld := now.Add(-3 * time.Hour)
	tIn1 := now.Add(-2 * time.Hour)
	tIn2 := now.Add(-1 * time.Hour)

	// Write one engram outside the window.
	if _, err := store.WriteEngram(ctx, ws, &Engram{
		Concept:   "old",
		Content:   "outside window",
		CreatedAt: tOld,
	}); err != nil {
		t.Fatalf("WriteEngram (old): %v", err)
	}

	// Write two engrams inside the window.
	id1, err := store.WriteEngram(ctx, ws, &Engram{
		Concept:   "in-window-1",
		Content:   "first in window",
		CreatedAt: tIn1,
	})
	if err != nil {
		t.Fatalf("WriteEngram (in1): %v", err)
	}
	id2, err := store.WriteEngram(ctx, ws, &Engram{
		Concept:   "in-window-2",
		Content:   "second in window",
		CreatedAt: tIn2,
	})
	if err != nil {
		t.Fatalf("WriteEngram (in2): %v", err)
	}

	since := tIn1.Add(-time.Millisecond)
	until := tIn2.Add(time.Millisecond)

	ids, err := store.EngramIDsByCreatedRange(ctx, ws, since, until, 100)
	if err != nil {
		t.Fatalf("EngramIDsByCreatedRange: %v", err)
	}
	if len(ids) != 2 {
		t.Errorf("EngramIDsByCreatedRange returned %d IDs, want 2", len(ids))
	}

	found := make(map[ULID]bool)
	for _, id := range ids {
		found[id] = true
	}
	if !found[id1] {
		t.Errorf("id1 (%v) not returned by EngramIDsByCreatedRange", id1)
	}
	if !found[id2] {
		t.Errorf("id2 (%v) not returned by EngramIDsByCreatedRange", id2)
	}
}

// TestListByTagInRange_NewestFirst writes 5 engrams sharing one tag at ascending
// CreatedAt timestamps and verifies that ListByTagInRange returns them
// newest-first, that truncation sacrifices the oldest entries (not the newest),
// and that the since/until window bounds the scan.
func TestListByTagInRange_NewestFirst(t *testing.T) {
	store := openTestStore(t)
	ws := store.VaultPrefix("tag-range-test")
	ctx := context.Background()

	now := time.Now()
	// Oldest -> newest. ULID (and thus tag-index order) is derived from CreatedAt.
	times := []time.Time{
		now.Add(-5 * time.Hour),
		now.Add(-4 * time.Hour),
		now.Add(-3 * time.Hour),
		now.Add(-2 * time.Hour),
		now.Add(-1 * time.Hour),
	}
	ids := make([]ULID, len(times))
	for i, ts := range times {
		id, err := store.WriteEngram(ctx, ws, &Engram{
			Concept:   "tagged",
			Content:   "content",
			Tags:      []string{"t"},
			CreatedAt: ts,
		})
		if err != nil {
			t.Fatalf("WriteEngram[%d]: %v", i, err)
		}
		ids[i] = id
	}

	epoch := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	far := now.Add(time.Hour)

	// Full scan: all 5, newest-first.
	got, err := store.ListByTagInRange(ctx, ws, "t", epoch, far, 100)
	if err != nil {
		t.Fatalf("ListByTagInRange full: %v", err)
	}
	wantFull := []ULID{ids[4], ids[3], ids[2], ids[1], ids[0]}
	if len(got) != len(wantFull) {
		t.Fatalf("full scan: got %d IDs, want %d", len(got), len(wantFull))
	}
	for i := range wantFull {
		if got[i] != wantFull[i] {
			t.Errorf("full scan pos %d: got %v, want %v (expected newest-first)", i, got[i], wantFull[i])
		}
	}

	// Truncated scan (limit 3): the 3 NEWEST, newest-first. Truncation must drop
	// the oldest entries, never the newest.
	gotTrunc, err := store.ListByTagInRange(ctx, ws, "t", epoch, far, 3)
	if err != nil {
		t.Fatalf("ListByTagInRange truncated: %v", err)
	}
	wantTrunc := []ULID{ids[4], ids[3], ids[2]}
	if len(gotTrunc) != len(wantTrunc) {
		t.Fatalf("truncated scan: got %d IDs, want %d", len(gotTrunc), len(wantTrunc))
	}
	for i := range wantTrunc {
		if gotTrunc[i] != wantTrunc[i] {
			t.Errorf("truncated scan pos %d: got %v, want %v (truncation must keep the newest)", i, gotTrunc[i], wantTrunc[i])
		}
	}

	// Windowed scan: since/until excludes the two oldest, returns id2..id4 newest-first.
	since := now.Add(-3*time.Hour - time.Minute)
	until := now.Add(-30 * time.Minute)
	gotWin, err := store.ListByTagInRange(ctx, ws, "t", since, until, 100)
	if err != nil {
		t.Fatalf("ListByTagInRange windowed: %v", err)
	}
	wantWin := []ULID{ids[4], ids[3], ids[2]}
	if len(gotWin) != len(wantWin) {
		t.Fatalf("windowed scan: got %d IDs, want %d", len(gotWin), len(wantWin))
	}
	for i := range wantWin {
		if gotWin[i] != wantWin[i] {
			t.Errorf("windowed scan pos %d: got %v, want %v", i, gotWin[i], wantWin[i])
		}
	}
}

// TestListByTagsAllInRange_AllTruncated is the counterexample that breaks a
// per-tag-window seeding heuristic: tags A and B each have more single-tag
// members than the limit and all newer than the target, so every per-tag
// newest-first window truncates BEFORE reaching the target. The target carries
// both tags. A K-way stream intersection must still find it, because the limit
// bounds intersection OUTPUT, not the per-stream input scan.
func TestListByTagsAllInRange_AllTruncated(t *testing.T) {
	store := openTestStore(t)
	ws := store.VaultPrefix("tags-all-truncated")
	ctx := context.Background()

	base := time.Now().Add(-24 * time.Hour)

	// Oldest engram, carries both tags — the true positive.
	target, err := store.WriteEngram(ctx, ws, &Engram{
		Concept: "target", Content: "old, both tags", Tags: []string{"A", "B"}, CreatedAt: base,
	})
	if err != nil {
		t.Fatalf("WriteEngram(target): %v", err)
	}

	// Three newer A-only and three newer B-only decoys. With limit 3, each tag's
	// newest-first window is entirely decoys and never reaches the target.
	for i := 0; i < 3; i++ {
		if _, err := store.WriteEngram(ctx, ws, &Engram{
			Concept: "a-decoy", Content: "A only", Tags: []string{"A"}, CreatedAt: base.Add(time.Duration(i+1) * time.Hour),
		}); err != nil {
			t.Fatalf("WriteEngram(a-decoy %d): %v", i, err)
		}
		if _, err := store.WriteEngram(ctx, ws, &Engram{
			Concept: "b-decoy", Content: "B only", Tags: []string{"B"}, CreatedAt: base.Add(time.Duration(i+1) * time.Hour),
		}); err != nil {
			t.Fatalf("WriteEngram(b-decoy %d): %v", i, err)
		}
	}

	epoch := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	far := time.Now().Add(time.Hour)

	got, err := store.ListByTagsAllInRange(ctx, ws, []string{"A", "B"}, epoch, far, 3)
	if err != nil {
		t.Fatalf("ListByTagsAllInRange: %v", err)
	}
	if len(got) != 1 || got[0] != target {
		t.Errorf("ListByTagsAllInRange = %v, want [%v] (target must survive all-window truncation)", got, target)
	}
}

// TestListByTagsAllInRange_LimitOnOutput verifies that limit bounds the number of
// emitted intersection results (the newest ones), not the per-tag input scan.
func TestListByTagsAllInRange_LimitOnOutput(t *testing.T) {
	store := openTestStore(t)
	ws := store.VaultPrefix("tags-all-limit")
	ctx := context.Background()

	base := time.Now().Add(-24 * time.Hour)

	// Five engrams carrying BOTH tags, ascending in time.
	both := make([]ULID, 5)
	for i := 0; i < 5; i++ {
		id, err := store.WriteEngram(ctx, ws, &Engram{
			Concept: "both", Content: "carries both", Tags: []string{"A", "B"}, CreatedAt: base.Add(time.Duration(i) * time.Hour),
		})
		if err != nil {
			t.Fatalf("WriteEngram(both %d): %v", i, err)
		}
		both[i] = id
	}
	// Some single-tag decoys interleaved so the streams are not pure.
	for i := 0; i < 4; i++ {
		if _, err := store.WriteEngram(ctx, ws, &Engram{
			Concept: "a-only", Content: "A only", Tags: []string{"A"}, CreatedAt: base.Add(time.Duration(i)*time.Hour + 30*time.Minute),
		}); err != nil {
			t.Fatalf("WriteEngram(a-only %d): %v", i, err)
		}
	}

	epoch := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	far := time.Now().Add(time.Hour)

	got, err := store.ListByTagsAllInRange(ctx, ws, []string{"A", "B"}, epoch, far, 3)
	if err != nil {
		t.Fatalf("ListByTagsAllInRange: %v", err)
	}
	// Expect the 3 NEWEST both-tag engrams, newest-first: both[4], both[3], both[2].
	want := []ULID{both[4], both[3], both[2]}
	if len(got) != len(want) {
		t.Fatalf("got %d IDs, want %d (limit must bound output to the newest matches)", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("pos %d: got %v, want %v (newest-first, limited output)", i, got[i], want[i])
		}
	}
}

// TestListByTagsAllInRange_DuplicateTagsEquivalent pins that duplicate tag values
// yield identical results to the distinct call. The fix is resource-safety (a
// duplicated tag must not open a redundant iterator over the same stream); there
// is no behavioral difference to assert, so this locks in result-equivalence.
func TestListByTagsAllInRange_DuplicateTagsEquivalent(t *testing.T) {
	store := openTestStore(t)
	ws := store.VaultPrefix("tags-all-dupe")
	ctx := context.Background()

	base := time.Now().Add(-24 * time.Hour)
	for i := 0; i < 3; i++ {
		if _, err := store.WriteEngram(ctx, ws, &Engram{
			Concept: "both", Content: "A and B", Tags: []string{"A", "B"}, CreatedAt: base.Add(time.Duration(i) * time.Hour),
		}); err != nil {
			t.Fatalf("WriteEngram(both %d): %v", i, err)
		}
		if _, err := store.WriteEngram(ctx, ws, &Engram{
			Concept: "a", Content: "A only", Tags: []string{"A"}, CreatedAt: base.Add(time.Duration(i)*time.Hour + 10*time.Minute),
		}); err != nil {
			t.Fatalf("WriteEngram(a %d): %v", i, err)
		}
	}

	epoch := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	far := time.Now().Add(time.Hour)

	eq := func(a, b []ULID) bool {
		if len(a) != len(b) {
			return false
		}
		for i := range a {
			if a[i] != b[i] {
				return false
			}
		}
		return true
	}

	// ["A","A"] must equal ["A"] (collapses to the single-tag path).
	single, err := store.ListByTagsAllInRange(ctx, ws, []string{"A"}, epoch, far, 100)
	if err != nil {
		t.Fatalf("ListByTagsAllInRange([A]): %v", err)
	}
	dupSingle, err := store.ListByTagsAllInRange(ctx, ws, []string{"A", "A"}, epoch, far, 100)
	if err != nil {
		t.Fatalf("ListByTagsAllInRange([A,A]): %v", err)
	}
	if !eq(single, dupSingle) {
		t.Errorf("[A,A] = %v, want == [A] = %v", dupSingle, single)
	}

	// ["A","A","B","B"] must equal ["A","B"].
	distinct, err := store.ListByTagsAllInRange(ctx, ws, []string{"A", "B"}, epoch, far, 100)
	if err != nil {
		t.Fatalf("ListByTagsAllInRange([A,B]): %v", err)
	}
	dup, err := store.ListByTagsAllInRange(ctx, ws, []string{"A", "A", "B", "B"}, epoch, far, 100)
	if err != nil {
		t.Fatalf("ListByTagsAllInRange([A,A,B,B]): %v", err)
	}
	if !eq(distinct, dup) {
		t.Errorf("[A,A,B,B] = %v, want == [A,B] = %v", dup, distinct)
	}
}

// TestLowestRelevanceIDs writes 5 engrams with distinct relevance scores, calls
// LowestRelevanceIDs(ctx, ws, 3), and verifies that 3 IDs are returned and they
// correspond to the 3 lowest-relevance engrams.
func TestLowestRelevanceIDs(t *testing.T) {
	store := openTestStore(t)
	ws := store.VaultPrefix("lowest-relevance-test")
	ctx := context.Background()

	// Write 5 engrams with clearly differentiated relevance scores.
	// Scores (ascending): 0.0, 0.1, 0.2, 0.7, 0.9
	engrams := []struct {
		relevance float32
		concept   string
	}{
		{0.0, "very low relevance"},
		{0.1, "low relevance"},
		{0.2, "below average relevance"},
		{0.7, "high relevance"},
		{0.9, "very high relevance"},
	}

	writtenIDs := make([]ULID, len(engrams))
	for i, e := range engrams {
		eng := &Engram{
			Concept:   e.concept,
			Content:   "content for relevance test",
			Relevance: e.relevance,
		}
		id, err := store.WriteEngram(ctx, ws, eng)
		if err != nil {
			t.Fatalf("WriteEngram[%d]: %v", i, err)
		}
		writtenIDs[i] = id
	}

	// Request the 3 lowest relevance IDs.
	ids, err := store.LowestRelevanceIDs(ctx, ws, 3)
	if err != nil {
		t.Fatalf("LowestRelevanceIDs: %v", err)
	}

	if len(ids) != 3 {
		t.Errorf("LowestRelevanceIDs returned %d IDs, want 3", len(ids))
	}

	// The 3 lowest relevance engrams are at indices 0, 1, 2 (scores 0.0, 0.1, 0.2).
	lowestSet := map[ULID]bool{
		writtenIDs[0]: true,
		writtenIDs[1]: true,
		writtenIDs[2]: true,
	}
	for _, id := range ids {
		if !lowestSet[id] {
			t.Errorf("ID %v is not among the 3 lowest-relevance engrams", id)
		}
	}

	// The 2 highest relevance engrams (indices 3, 4) must NOT appear in results.
	highSet := map[ULID]bool{
		writtenIDs[3]: true,
		writtenIDs[4]: true,
	}
	for _, id := range ids {
		if highSet[id] {
			t.Errorf("high-relevance engram %v appeared in lowest-relevance results", id)
		}
	}
}
