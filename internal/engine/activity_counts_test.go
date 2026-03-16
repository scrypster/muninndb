package engine

import (
	"context"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/storage"
)

func TestActivityCounts(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()

	ctx := context.Background()
	vault := "activity-test"

	// Use fixed UTC dates.
	day0 := time.Date(2025, 6, 10, 0, 0, 0, 0, time.UTC)
	day2 := time.Date(2025, 6, 12, 0, 0, 0, 0, time.UTC)
	day3 := time.Date(2025, 6, 13, 0, 0, 0, 0, time.UTC)

	ws := eng.store.VaultPrefix(vault)

	// Write engrams: 2 on day0, 0 on day1 (gap), 1 on day2, 0 on day3.
	for i := 0; i < 2; i++ {
		_, err := eng.store.WriteEngram(ctx, ws, &storage.Engram{
			Concept:   "day0",
			Content:   "content",
			CreatedAt: day0.Add(time.Duration(i) * time.Hour),
		})
		if err != nil {
			t.Fatalf("WriteEngram day0[%d]: %v", i, err)
		}
	}
	_, err := eng.store.WriteEngram(ctx, ws, &storage.Engram{
		Concept:   "day2",
		Content:   "content",
		CreatedAt: day2.Add(6 * time.Hour),
	})
	if err != nil {
		t.Fatalf("WriteEngram day2: %v", err)
	}

	t.Run("contiguous zero-filling", func(t *testing.T) {
		result, err := eng.ActivityCounts(ctx, vault, day0, day3)
		if err != nil {
			t.Fatalf("ActivityCounts: %v", err)
		}

		// Should get 4 days: day0, day1, day2, day3 (contiguous).
		if len(result) != 4 {
			t.Fatalf("expected 4 days, got %d: %v", len(result), result)
		}

		expected := []DailyCount{
			{Date: "2025-06-10", Count: 2},
			{Date: "2025-06-11", Count: 0}, // zero-filled
			{Date: "2025-06-12", Count: 1},
			{Date: "2025-06-13", Count: 0}, // zero-filled
		}
		for i, want := range expected {
			got := result[i]
			if got.Date != want.Date || got.Count != want.Count {
				t.Errorf("result[%d] = {%s, %d}, want {%s, %d}", i, got.Date, got.Count, want.Date, want.Count)
			}
		}
	})

	t.Run("single day range", func(t *testing.T) {
		// Use end-of-day for until so both engrams at 00:00 and 01:00 are included.
		endOfDay0 := day0.Add(24*time.Hour - time.Millisecond)
		result, err := eng.ActivityCounts(ctx, vault, day0, endOfDay0)
		if err != nil {
			t.Fatalf("ActivityCounts: %v", err)
		}
		if len(result) != 1 {
			t.Fatalf("expected 1 day, got %d", len(result))
		}
		if result[0].Date != "2025-06-10" || result[0].Count != 2 {
			t.Errorf("result[0] = {%s, %d}, want {2025-06-10, 2}", result[0].Date, result[0].Count)
		}
	})

	t.Run("range with no engrams", func(t *testing.T) {
		since := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
		until := time.Date(2025, 1, 3, 0, 0, 0, 0, time.UTC)
		result, err := eng.ActivityCounts(ctx, vault, since, until)
		if err != nil {
			t.Fatalf("ActivityCounts: %v", err)
		}
		// 3 days, all zero-filled.
		if len(result) != 3 {
			t.Fatalf("expected 3 days, got %d", len(result))
		}
		for _, r := range result {
			if r.Count != 0 {
				t.Errorf("expected 0 count for %s, got %d", r.Date, r.Count)
			}
		}
	})
}
