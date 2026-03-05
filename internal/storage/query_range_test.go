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
