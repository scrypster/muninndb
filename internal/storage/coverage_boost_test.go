package storage

// coverage_boost_test.go — targeted tests to push coverage from 73.3% → 75%+.
// Covers: EngramsByCreatedSince (pagination), CloneVaultData (empty source),
// MergeVaultData (empty source), ListByCreatorInRange (0% function),
// ParseULID, ParseLifecycleState, ParseMemoryType, MemoryType.String.

import (
	"context"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// EngramsByCreatedSince — pagination (offset/limit) path
// ---------------------------------------------------------------------------

// TestEngramsByCreatedSince_Pagination writes 5 engrams and verifies that
// offset and limit parameters correctly page through the results.
func TestEngramsByCreatedSince_Pagination(t *testing.T) {
	store := openTestStore(t)
	ws := store.VaultPrefix("since-page-test")
	ctx := context.Background()

	base := time.Now().Add(-10 * time.Minute)

	// Write 5 engrams with distinct (spaced) timestamps so ULID order is stable.
	for i := 0; i < 5; i++ {
		if _, err := store.WriteEngram(ctx, ws, &Engram{
			Concept:   "concept",
			Content:   "body",
			CreatedAt: base.Add(time.Duration(i) * time.Second),
		}); err != nil {
			t.Fatalf("WriteEngram[%d]: %v", i, err)
		}
	}

	since := base.Add(-time.Second) // include all 5

	// No offset: first 3.
	page1, err := store.EngramsByCreatedSince(ctx, ws, since, 0, 3)
	if err != nil {
		t.Fatalf("EngramsByCreatedSince page1: %v", err)
	}
	if len(page1) != 3 {
		t.Errorf("page1: got %d engrams, want 3", len(page1))
	}

	// Offset 3, limit 10: should get the remaining 2.
	page2, err := store.EngramsByCreatedSince(ctx, ws, since, 3, 10)
	if err != nil {
		t.Fatalf("EngramsByCreatedSince page2: %v", err)
	}
	if len(page2) != 2 {
		t.Errorf("page2: got %d engrams, want 2", len(page2))
	}

	// Offset beyond all results: should return empty.
	page3, err := store.EngramsByCreatedSince(ctx, ws, since, 10, 10)
	if err != nil {
		t.Fatalf("EngramsByCreatedSince page3: %v", err)
	}
	if len(page3) != 0 {
		t.Errorf("page3: got %d engrams, want 0", len(page3))
	}

	// Default limit (0) should clamp to 50.
	pageDefault, err := store.EngramsByCreatedSince(ctx, ws, since, 0, 0)
	if err != nil {
		t.Fatalf("EngramsByCreatedSince default limit: %v", err)
	}
	if len(pageDefault) != 5 {
		t.Errorf("default limit: got %d engrams, want 5", len(pageDefault))
	}
}

// TestEngramsByCreatedSince_EmptyVault verifies EngramsByCreatedSince on a
// vault with no engrams returns an empty slice without error.
func TestEngramsByCreatedSince_EmptyVault(t *testing.T) {
	store := openTestStore(t)
	ws := store.VaultPrefix("since-empty-vault")
	ctx := context.Background()

	engrams, err := store.EngramsByCreatedSince(ctx, ws, time.Time{}, 0, 100)
	if err != nil {
		t.Fatalf("EngramsByCreatedSince empty: %v", err)
	}
	if len(engrams) != 0 {
		t.Errorf("expected 0 engrams, got %d", len(engrams))
	}
}

// ---------------------------------------------------------------------------
// CloneVaultData — empty source vault
// ---------------------------------------------------------------------------

// TestCloneVaultData_EmptySource clones an empty vault and verifies that the
// target is also empty (count = 0, no error).
func TestCloneVaultData_EmptySource(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	wsSource := store.VaultPrefix("empty-src")
	wsTarget := store.VaultPrefix("empty-dst")

	if err := store.WriteVaultName(wsTarget, "empty-dst"); err != nil {
		t.Fatalf("WriteVaultName: %v", err)
	}

	copied, err := store.CloneVaultData(ctx, wsSource, wsTarget, nil)
	if err != nil {
		t.Fatalf("CloneVaultData empty source: %v", err)
	}
	if copied != 0 {
		t.Errorf("expected 0 engrams copied, got %d", copied)
	}

	// In-memory vault count must be 0.
	count := store.GetVaultCount(ctx, wsTarget)
	if count != 0 {
		t.Errorf("target vault count = %d, want 0", count)
	}
}

// TestCloneVaultData_WithOnCopyCallback verifies that the onCopy callback is
// invoked after the batch commit and receives the correct running total.
func TestCloneVaultData_WithOnCopyCallback(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	wsSource := store.VaultPrefix("cb-src")
	wsTarget := store.VaultPrefix("cb-dst")

	// Write 2 engrams so the callback fires at least once.
	for i := 0; i < 2; i++ {
		if _, err := store.WriteEngram(ctx, wsSource, &Engram{
			Concept: "concept",
			Content: "body",
		}); err != nil {
			t.Fatalf("WriteEngram[%d]: %v", i, err)
		}
	}

	if err := store.WriteVaultName(wsTarget, "cb-dst"); err != nil {
		t.Fatalf("WriteVaultName: %v", err)
	}

	var lastCallbackVal int64
	callbackCount := 0
	cb := func(n int64) {
		lastCallbackVal = n
		callbackCount++
	}

	copied, err := store.CloneVaultData(ctx, wsSource, wsTarget, cb)
	if err != nil {
		t.Fatalf("CloneVaultData: %v", err)
	}
	if copied != 2 {
		t.Errorf("expected 2 engrams copied, got %d", copied)
	}
	if callbackCount == 0 {
		t.Error("onCopy callback was never called")
	}
	if lastCallbackVal != 2 {
		t.Errorf("last callback value = %d, want 2", lastCallbackVal)
	}
}

// ---------------------------------------------------------------------------
// MergeVaultData — empty source into non-empty target
// ---------------------------------------------------------------------------

// TestMergeVaultData_EmptySource verifies that merging an empty source vault
// into a non-empty target leaves the target unchanged.
func TestMergeVaultData_EmptySource(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	wsSource := store.VaultPrefix("merge-empty-src")
	wsTarget := store.VaultPrefix("merge-empty-dst")

	// Write 2 engrams to target.
	for i := 0; i < 2; i++ {
		if _, err := store.WriteEngram(ctx, wsTarget, &Engram{
			Concept: "target concept",
			Content: "target body",
		}); err != nil {
			t.Fatalf("WriteEngram target[%d]: %v", i, err)
		}
	}

	merged, err := store.MergeVaultData(ctx, wsSource, wsTarget, nil)
	if err != nil {
		t.Fatalf("MergeVaultData empty source: %v", err)
	}
	if merged != 0 {
		t.Errorf("expected 0 engrams merged, got %d", merged)
	}

	// Target count should be unchanged (whatever it was after the writes) — at least 2.
	count := store.GetVaultCount(ctx, wsTarget)
	if count < 2 {
		t.Errorf("target vault count = %d, want >= 2 (should not decrease after empty merge)", count)
	}
}

// TestMergeVaultData_WithOnCopyCallback verifies the onCopy callback fires
// with the correct running total during a non-trivial merge.
func TestMergeVaultData_WithOnCopyCallback(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	wsSrc := store.VaultPrefix("merge-cb-src")
	wsDst := store.VaultPrefix("merge-cb-dst")

	// Write 3 engrams to source.
	for i := 0; i < 3; i++ {
		if _, err := store.WriteEngram(ctx, wsSrc, &Engram{
			Concept: "src concept",
			Content: "src body",
		}); err != nil {
			t.Fatalf("WriteEngram src[%d]: %v", i, err)
		}
	}

	var lastCB int64
	cbCalls := 0
	merged, err := store.MergeVaultData(ctx, wsSrc, wsDst, func(n int64) {
		lastCB = n
		cbCalls++
	})
	if err != nil {
		t.Fatalf("MergeVaultData: %v", err)
	}
	if merged != 3 {
		t.Errorf("expected 3 engrams merged, got %d", merged)
	}
	if cbCalls == 0 {
		t.Error("onCopy callback was never called")
	}
	if lastCB != 3 {
		t.Errorf("last callback value = %d, want 3", lastCB)
	}
}

// ---------------------------------------------------------------------------
// ListByCreatorInRange — 0% coverage
// ---------------------------------------------------------------------------

// TestListByCreatorInRange writes engrams with different CreatedBy values and
// verifies that ListByCreatorInRange filters by creator and time window.
func TestListByCreatorInRange(t *testing.T) {
	store := openTestStore(t)
	ws := store.VaultPrefix("creator-range-test")
	ctx := context.Background()

	now := time.Now()
	tIn1 := now.Add(-2 * time.Hour)
	tIn2 := now.Add(-1 * time.Hour)
	tOut := now.Add(-3 * time.Hour)

	// Write engram outside the window for the target creator.
	if _, err := store.WriteEngram(ctx, ws, &Engram{
		Concept:   "outside window",
		Content:   "body",
		CreatedBy: "alice",
		CreatedAt: tOut,
	}); err != nil {
		t.Fatalf("WriteEngram out: %v", err)
	}

	// Write two in-window engrams for "alice".
	id1, err := store.WriteEngram(ctx, ws, &Engram{
		Concept:   "in-window 1",
		Content:   "body",
		CreatedBy: "alice",
		CreatedAt: tIn1,
	})
	if err != nil {
		t.Fatalf("WriteEngram in1: %v", err)
	}

	id2, err := store.WriteEngram(ctx, ws, &Engram{
		Concept:   "in-window 2",
		Content:   "body",
		CreatedBy: "alice",
		CreatedAt: tIn2,
	})
	if err != nil {
		t.Fatalf("WriteEngram in2: %v", err)
	}

	// Write an in-window engram for a different creator — must not appear.
	if _, err := store.WriteEngram(ctx, ws, &Engram{
		Concept:   "bob engram",
		Content:   "body",
		CreatedBy: "bob",
		CreatedAt: tIn1,
	}); err != nil {
		t.Fatalf("WriteEngram bob: %v", err)
	}

	since := tIn1.Add(-time.Millisecond)
	until := tIn2.Add(time.Millisecond)

	ids, err := store.ListByCreatorInRange(ctx, ws, "alice", since, until, 100)
	if err != nil {
		t.Fatalf("ListByCreatorInRange: %v", err)
	}

	if len(ids) != 2 {
		t.Errorf("ListByCreatorInRange returned %d IDs, want 2", len(ids))
	}

	found := make(map[ULID]bool)
	for _, id := range ids {
		found[id] = true
	}
	if !found[id1] {
		t.Errorf("id1 (%v) not returned", id1)
	}
	if !found[id2] {
		t.Errorf("id2 (%v) not returned", id2)
	}
}

// TestListByCreatorInRange_DefaultLimit verifies the default-limit path (limit <= 0).
func TestListByCreatorInRange_DefaultLimit(t *testing.T) {
	store := openTestStore(t)
	ws := store.VaultPrefix("creator-deflimit-test")
	ctx := context.Background()

	now := time.Now()
	for i := 0; i < 3; i++ {
		if _, err := store.WriteEngram(ctx, ws, &Engram{
			Concept:   "concept",
			Content:   "body",
			CreatedBy: "charlie",
			CreatedAt: now.Add(time.Duration(i) * time.Second),
		}); err != nil {
			t.Fatalf("WriteEngram[%d]: %v", i, err)
		}
	}

	// limit 0 should default to 50.
	ids, err := store.ListByCreatorInRange(ctx, ws, "charlie", now.Add(-time.Minute), now.Add(time.Minute), 0)
	if err != nil {
		t.Fatalf("ListByCreatorInRange 0 limit: %v", err)
	}
	if len(ids) != 3 {
		t.Errorf("got %d IDs, want 3", len(ids))
	}
}

// ---------------------------------------------------------------------------
// ParseULID — 0% coverage
// ---------------------------------------------------------------------------

func TestParseULID_ValidAndInvalid(t *testing.T) {
	// Generate a valid ULID and round-trip it through string → ParseULID.
	original := NewULID()
	s := original.String()

	parsed, err := ParseULID(s)
	if err != nil {
		t.Fatalf("ParseULID valid string: %v", err)
	}
	if parsed != original {
		t.Errorf("ParseULID round-trip mismatch: got %v, want %v", parsed, original)
	}

	// Invalid string should return an error.
	_, err = ParseULID("not-a-valid-ulid")
	if err == nil {
		t.Error("ParseULID invalid string: expected error, got nil")
	}
}

// ---------------------------------------------------------------------------
// ParseLifecycleState — 0% coverage
// ---------------------------------------------------------------------------

func TestParseLifecycleState(t *testing.T) {
	cases := []struct {
		input string
		want  LifecycleState
		ok    bool
	}{
		{"active", StateActive, true},
		{"planning", StatePlanning, true},
		{"paused", StatePaused, true},
		{"blocked", StateBlocked, true},
		{"completed", StateCompleted, true},
		{"cancelled", StateCancelled, true},
		{"archived", StateArchived, true},
		{"unknown-xyz", 0, false},
	}
	for _, tc := range cases {
		got, err := ParseLifecycleState(tc.input)
		if tc.ok {
			if err != nil {
				t.Errorf("ParseLifecycleState(%q): unexpected error %v", tc.input, err)
			}
			if got != tc.want {
				t.Errorf("ParseLifecycleState(%q) = %v, want %v", tc.input, got, tc.want)
			}
		} else {
			if err == nil {
				t.Errorf("ParseLifecycleState(%q): expected error, got nil", tc.input)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// ParseMemoryType and MemoryType.String — both 0% coverage
// ---------------------------------------------------------------------------

func TestParseMemoryType(t *testing.T) {
	cases := []struct {
		input string
		want  MemoryType
		ok    bool
	}{
		{"fact", TypeFact, true},
		{"decision", TypeDecision, true},
		{"observation", TypeObservation, true},
		{"preference", TypePreference, true},
		{"issue", TypeIssue, true},
		{"bugfix", TypeIssue, true},
		{"bug_report", TypeIssue, true},
		{"task", TypeTask, true},
		{"procedure", TypeProcedure, true},
		{"event", TypeEvent, true},
		{"experience", TypeEvent, true},
		{"goal", TypeGoal, true},
		{"constraint", TypeConstraint, true},
		{"identity", TypeIdentity, true},
		{"reference", TypeReference, true},
		{"not-a-type", TypeFact, false},
	}
	for _, tc := range cases {
		got, ok := ParseMemoryType(tc.input)
		if ok != tc.ok {
			t.Errorf("ParseMemoryType(%q) ok=%v, want %v", tc.input, ok, tc.ok)
		}
		if tc.ok && got != tc.want {
			t.Errorf("ParseMemoryType(%q) = %v, want %v", tc.input, got, tc.want)
		}
	}
}

func TestMemoryTypeString(t *testing.T) {
	cases := []struct {
		mt   MemoryType
		want string
	}{
		{TypeFact, "fact"},
		{TypeDecision, "decision"},
		{TypeObservation, "observation"},
		{TypePreference, "preference"},
		{TypeIssue, "issue"},
		{TypeTask, "task"},
		{TypeProcedure, "procedure"},
		{TypeEvent, "event"},
		{TypeGoal, "goal"},
		{TypeConstraint, "constraint"},
		{TypeIdentity, "identity"},
		{TypeReference, "reference"},
		{MemoryType(255), "fact"}, // default / unknown
	}
	for _, tc := range cases {
		got := tc.mt.String()
		if got != tc.want {
			t.Errorf("MemoryType(%d).String() = %q, want %q", tc.mt, got, tc.want)
		}
	}
}
