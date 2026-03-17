package storage

import (
	"context"
	"testing"
	"time"
)

func TestCountEngramsByDay(t *testing.T) {
	store := openTestStore(t)
	ws := store.VaultPrefix("activity-test")
	ctx := context.Background()

	// Use a fixed reference time in UTC to avoid timezone ambiguity.
	ref := time.Date(2025, 3, 15, 12, 0, 0, 0, time.UTC)
	day0 := ref.Add(-2 * 24 * time.Hour) // 2025-03-13
	day1 := ref.Add(-1 * 24 * time.Hour) // 2025-03-14
	day2 := ref                          // 2025-03-15

	// Write two engrams on day0.
	for i := 0; i < 2; i++ {
		_, err := store.WriteEngram(ctx, ws, &Engram{
			Concept:   "day0 engram",
			Content:   "content",
			CreatedAt: day0.Add(time.Duration(i) * time.Hour),
		})
		if err != nil {
			t.Fatalf("WriteEngram day0[%d]: %v", i, err)
		}
	}
	// Write one engram on day1.
	_, err := store.WriteEngram(ctx, ws, &Engram{
		Concept:   "day1 engram",
		Content:   "content",
		CreatedAt: day1,
	})
	if err != nil {
		t.Fatalf("WriteEngram day1: %v", err)
	}
	// Write three engrams on day2, including one at the very start of day (boundary).
	for i := 0; i < 3; i++ {
		_, err := store.WriteEngram(ctx, ws, &Engram{
			Concept:   "day2 engram",
			Content:   "content",
			CreatedAt: day2.Truncate(24 * time.Hour).Add(time.Duration(i) * time.Minute),
		})
		if err != nil {
			t.Fatalf("WriteEngram day2[%d]: %v", i, err)
		}
	}

	t.Run("full range returns all days", func(t *testing.T) {
		since := day0.Truncate(24 * time.Hour)
		until := day2.Truncate(24 * time.Hour).Add(24*time.Hour - time.Millisecond)
		counts, err := store.CountEngramsByDay(ctx, ws, since, until)
		if err != nil {
			t.Fatalf("CountEngramsByDay: %v", err)
		}
		if got := counts["2025-03-13"]; got != 2 {
			t.Errorf("day0 count = %d, want 2", got)
		}
		if got := counts["2025-03-14"]; got != 1 {
			t.Errorf("day1 count = %d, want 1", got)
		}
		if got := counts["2025-03-15"]; got != 3 {
			t.Errorf("day2 count = %d, want 3", got)
		}
	})

	t.Run("boundary at exact since/until", func(t *testing.T) {
		// Range covers only day0 (since = start of day0, until = end of day0).
		since := day0.Truncate(24 * time.Hour)
		until := since.Add(24*time.Hour - time.Millisecond)
		counts, err := store.CountEngramsByDay(ctx, ws, since, until)
		if err != nil {
			t.Fatalf("CountEngramsByDay: %v", err)
		}
		if got := counts["2025-03-13"]; got != 2 {
			t.Errorf("day0 count = %d, want 2", got)
		}
		// day1 and day2 should not appear.
		if got := counts["2025-03-14"]; got != 0 {
			t.Errorf("day1 count = %d, want 0", got)
		}
		if got := counts["2025-03-15"]; got != 0 {
			t.Errorf("day2 count = %d, want 0", got)
		}
	})

	t.Run("empty range returns empty map", func(t *testing.T) {
		// A range entirely before any engrams.
		since := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
		until := time.Date(2020, 1, 2, 0, 0, 0, 0, time.UTC)
		counts, err := store.CountEngramsByDay(ctx, ws, since, until)
		if err != nil {
			t.Fatalf("CountEngramsByDay: %v", err)
		}
		if len(counts) != 0 {
			t.Errorf("expected empty map, got %v", counts)
		}
	})

	t.Run("context cancellation", func(t *testing.T) {
		cctx, cancel := context.WithCancel(ctx)
		cancel()
		_, err := store.CountEngramsByDay(cctx, ws, day0, day2)
		if err == nil {
			t.Error("expected context error, got nil")
		}
	})
}
