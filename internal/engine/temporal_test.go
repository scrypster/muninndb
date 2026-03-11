package engine

import (
	"context"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// TestTemporalFilter_CreatedAfter writes 3 engrams at different timestamps and
// verifies that activating with a created_after filter returns only engrams
// created at or after the filter time.
func TestTemporalFilter_CreatedAfter(t *testing.T) {
	t.Parallel()
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	now := time.Now()
	tOld := now.Add(-14 * 24 * time.Hour) // 2 weeks ago
	tMid := now.Add(-7 * 24 * time.Hour)  // 1 week ago
	tRecent := now.Add(-24 * time.Hour)   // yesterday

	// Write 3 engrams with distinct custom timestamps and distinct content
	oldResp, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:     "temporal-test",
		Concept:   "old memory",
		Content:   "this engram was written two weeks ago in the past",
		CreatedAt: &tOld,
	})
	if err != nil {
		t.Fatalf("Write old: %v", err)
	}

	midResp, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:     "temporal-test",
		Concept:   "mid memory",
		Content:   "this engram was written one week ago recently",
		CreatedAt: &tMid,
	})
	if err != nil {
		t.Fatalf("Write mid: %v", err)
	}

	recentResp, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:     "temporal-test",
		Concept:   "recent memory",
		Content:   "this engram was written yesterday very recently",
		CreatedAt: &tRecent,
	})
	if err != nil {
		t.Fatalf("Write recent: %v", err)
	}

	// Allow async FTS worker to index
	awaitFTS(t, eng)

	// Activate with created_after=tMid — should return mid and recent, NOT old
	// The passesMetaFilter logic: created_after passes if eng.CreatedAt.After(t)
	// So tMid.After(tMid) == false — tMid itself would fail with strict After.
	// Use tMid minus 1 second so both tMid and tRecent pass.
	filterTime := tMid.Add(-time.Second)
	resp, err := eng.Activate(ctx, &mbp.ActivateRequest{
		Vault:      "temporal-test",
		Context:    []string{"memory engram"},
		MaxResults: 100,
		Threshold:  0.001,
		Filters: []mbp.Filter{
			{
				Field: "created_after",
				Op:    ">=",
				Value: filterTime,
			},
		},
	})
	if err != nil {
		t.Fatalf("Activate with created_after: %v", err)
	}

	// Build a set of returned IDs
	returnedIDs := make(map[string]bool)
	for _, a := range resp.Activations {
		returnedIDs[a.ID] = true
	}

	// old must NOT be in results
	if returnedIDs[oldResp.ID] {
		t.Errorf("created_after filter failed: old engram (2 weeks ago) appeared in results but should have been excluded")
	}

	// mid and recent must be in results
	if !returnedIDs[midResp.ID] {
		t.Errorf("created_after filter failed: mid engram (1 week ago) missing from results — should have been included")
	}
	if !returnedIDs[recentResp.ID] {
		t.Errorf("created_after filter failed: recent engram (yesterday) missing from results — should have been included")
	}
}

// TestTemporalFilter_CreatedBefore writes 3 engrams at different timestamps and
// verifies that activating with a created_before filter returns only engrams
// created before the filter time.
func TestTemporalFilter_CreatedBefore(t *testing.T) {
	t.Parallel()
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	now := time.Now()
	tOld := now.Add(-14 * 24 * time.Hour) // 2 weeks ago
	tMid := now.Add(-7 * 24 * time.Hour)  // 1 week ago
	tRecent := now.Add(-24 * time.Hour)   // yesterday

	oldResp, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:     "temporal-before-test",
		Concept:   "old memory",
		Content:   "this engram was written two weeks ago in the past",
		CreatedAt: &tOld,
	})
	if err != nil {
		t.Fatalf("Write old: %v", err)
	}

	midResp, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:     "temporal-before-test",
		Concept:   "mid memory",
		Content:   "this engram was written one week ago recently",
		CreatedAt: &tMid,
	})
	if err != nil {
		t.Fatalf("Write mid: %v", err)
	}

	recentResp, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:     "temporal-before-test",
		Concept:   "recent memory",
		Content:   "this engram was written yesterday very recently",
		CreatedAt: &tRecent,
	})
	if err != nil {
		t.Fatalf("Write recent: %v", err)
	}

	// Allow async FTS worker to index
	awaitFTS(t, eng)

	// Activate with created_before=tMid+1s — should return old and mid, NOT recent
	// passesMetaFilter: created_before passes if eng.CreatedAt.Before(t)
	// tMid.Before(tMid+1s) == true, tRecent.Before(tMid+1s) == false
	filterTime := tMid.Add(time.Second)
	resp, err := eng.Activate(ctx, &mbp.ActivateRequest{
		Vault:      "temporal-before-test",
		Context:    []string{"memory engram"},
		MaxResults: 100,
		Threshold:  0.001,
		Filters: []mbp.Filter{
			{
				Field: "created_before",
				Op:    "<=",
				Value: filterTime,
			},
		},
	})
	if err != nil {
		t.Fatalf("Activate with created_before: %v", err)
	}

	returnedIDs := make(map[string]bool)
	for _, a := range resp.Activations {
		returnedIDs[a.ID] = true
	}

	// recent must NOT be in results
	if returnedIDs[recentResp.ID] {
		t.Errorf("created_before filter failed: recent engram (yesterday) appeared in results but should have been excluded")
	}

	// old and mid must be in results
	if !returnedIDs[oldResp.ID] {
		t.Errorf("created_before filter failed: old engram (2 weeks ago) missing from results — should have been included")
	}
	if !returnedIDs[midResp.ID] {
		t.Errorf("created_before filter failed: mid engram (1 week ago) missing from results — should have been included")
	}
}

// TestTemporalFilter_CombinedRange verifies that using both created_after and
// created_before together correctly returns only the engram in the middle of the
// time range.
func TestTemporalFilter_CombinedRange(t *testing.T) {
	t.Parallel()
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	now := time.Now()
	tOld := now.Add(-14 * 24 * time.Hour) // 2 weeks ago
	tMid := now.Add(-7 * 24 * time.Hour)  // 1 week ago
	tRecent := now.Add(-24 * time.Hour)   // yesterday

	oldResp, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:     "temporal-range-test",
		Concept:   "old memory",
		Content:   "this engram was written two weeks ago in the past",
		CreatedAt: &tOld,
	})
	if err != nil {
		t.Fatalf("Write old: %v", err)
	}

	midResp, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:     "temporal-range-test",
		Concept:   "mid memory",
		Content:   "this engram was written one week ago recently",
		CreatedAt: &tMid,
	})
	if err != nil {
		t.Fatalf("Write mid: %v", err)
	}

	recentResp, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:     "temporal-range-test",
		Concept:   "recent memory",
		Content:   "this engram was written yesterday very recently",
		CreatedAt: &tRecent,
	})
	if err != nil {
		t.Fatalf("Write recent: %v", err)
	}

	// Allow async FTS worker to index
	awaitFTS(t, eng)

	// Activate with created_after=(tOld+1s) AND created_before=(tRecent-1s)
	// Only tMid should pass both filters.
	resp, err := eng.Activate(ctx, &mbp.ActivateRequest{
		Vault:      "temporal-range-test",
		Context:    []string{"memory engram"},
		MaxResults: 100,
		Threshold:  0.001,
		Filters: []mbp.Filter{
			{
				Field: "created_after",
				Op:    ">=",
				Value: tOld.Add(time.Second),
			},
			{
				Field: "created_before",
				Op:    "<=",
				Value: tRecent.Add(-time.Second),
			},
		},
	})
	if err != nil {
		t.Fatalf("Activate with combined range: %v", err)
	}

	returnedIDs := make(map[string]bool)
	for _, a := range resp.Activations {
		returnedIDs[a.ID] = true
	}

	// old and recent must NOT be in results
	if returnedIDs[oldResp.ID] {
		t.Errorf("combined range filter failed: old engram should be excluded")
	}
	if returnedIDs[recentResp.ID] {
		t.Errorf("combined range filter failed: recent engram should be excluded")
	}

	// mid must be in results
	if !returnedIDs[midResp.ID] {
		t.Errorf("combined range filter failed: mid engram missing from results — should be the only result")
	}
}

// TestCustomCreatedAt_RoundTrip verifies that writing an engram with a custom
// CreatedAt timestamp preserves that timestamp when read back.
func TestCustomCreatedAt_RoundTrip(t *testing.T) {
	t.Parallel()
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	// Use a specific past time (truncated to second to avoid sub-second precision issues)
	pastTime := time.Now().Add(-48 * time.Hour).Truncate(time.Second)

	writeResp, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:     "createdAt-test",
		Concept:   "custom timestamp engram",
		Content:   "this engram has a custom creation timestamp from the past",
		CreatedAt: &pastTime,
	})
	if err != nil {
		t.Fatalf("Write with custom CreatedAt: %v", err)
	}
	if writeResp.ID == "" {
		t.Fatal("expected non-empty ID from Write")
	}

	// Read back the engram by ID
	readResp, err := eng.Read(ctx, &mbp.ReadRequest{
		Vault: "createdAt-test",
		ID:    writeResp.ID,
	})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}

	// ReadResponse.CreatedAt is UnixNano; convert back to time.Time for comparison
	gotCreatedAt := time.Unix(0, readResp.CreatedAt).Truncate(time.Second)

	if !gotCreatedAt.Equal(pastTime) {
		t.Errorf("CreatedAt mismatch: got %v, want %v (diff: %v)",
			gotCreatedAt, pastTime, gotCreatedAt.Sub(pastTime))
	}
}

// TestCustomCreatedAt_FilterUsesCustomTimestamp verifies that the temporal filter
// correctly uses the custom CreatedAt field (not write time) for filtering.
// This is the critical correctness test: a backdated engram must behave as if
// it was created at the custom timestamp, not at the time it was actually written.
func TestCustomCreatedAt_FilterUsesCustomTimestamp(t *testing.T) {
	t.Parallel()
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	now := time.Now()

	// Write an engram backdated to 10 days ago
	tenDaysAgo := now.Add(-10 * 24 * time.Hour)
	backdatedResp, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:     "backdated-test",
		Concept:   "backdated engram",
		Content:   "this engram has a custom backdated creation timestamp",
		CreatedAt: &tenDaysAgo,
	})
	if err != nil {
		t.Fatalf("Write backdated: %v", err)
	}

	// Write a current engram (no custom timestamp — uses write time which is ~now)
	currentResp, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:   "backdated-test",
		Concept: "current engram",
		Content: "this engram was written right now with default timestamp",
	})
	if err != nil {
		t.Fatalf("Write current: %v", err)
	}

	// Allow async FTS worker to index
	awaitFTS(t, eng)

	// Filter: created_before = 5 days ago
	// Only the backdated engram (10 days ago) should appear; the current engram (~now) should not.
	fiveDaysAgo := now.Add(-5 * 24 * time.Hour)
	resp, err := eng.Activate(ctx, &mbp.ActivateRequest{
		Vault:      "backdated-test",
		Context:    []string{"engram creation timestamp"},
		MaxResults: 100,
		Threshold:  0.001,
		Filters: []mbp.Filter{
			{
				Field: "created_before",
				Op:    "<=",
				Value: fiveDaysAgo,
			},
		},
	})
	if err != nil {
		t.Fatalf("Activate with created_before: %v", err)
	}

	returnedIDs := make(map[string]bool)
	for _, a := range resp.Activations {
		returnedIDs[a.ID] = true
	}

	// The current engram must NOT appear (it was written now, after the filter cutoff)
	if returnedIDs[currentResp.ID] {
		t.Errorf("custom CreatedAt test failed: current engram appeared but its write time (~now) is after the created_before cutoff (5 days ago)")
	}

	// The backdated engram MUST appear (it has CreatedAt = 10 days ago, before the cutoff)
	if !returnedIDs[backdatedResp.ID] {
		t.Errorf("custom CreatedAt test failed: backdated engram (10 days ago) missing from results — temporal filter should use the custom CreatedAt, not the actual write time")
	}
}
