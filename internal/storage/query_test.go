package storage

import (
	"context"
	"fmt"
	"testing"
)

func TestListByStateFrom_CursorPagination(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	ws := store.ResolveVaultPrefix("default")

	// Write 5 active engrams
	ids := make([]ULID, 5)
	for i := 0; i < 5; i++ {
		id, err := store.WriteEngram(ctx, ws, &Engram{
			Concept:    fmt.Sprintf("engram %d", i),
			Content:    fmt.Sprintf("content %d", i),
			State:      StateActive,
			Confidence: 0.9,
			Relevance:  0.8,
		})
		if err != nil {
			t.Fatalf("WriteEngram %d: %v", i, err)
		}
		ids[i] = id
	}

	// Page 1: get first 2
	page1, err := store.ListByStateFrom(ctx, ws, StateActive, ULID{}, 2)
	if err != nil {
		t.Fatalf("ListByStateFrom page1: %v", err)
	}
	if len(page1) != 2 {
		t.Fatalf("page1 len: got %d, want 2", len(page1))
	}

	// Page 2: continue from last ID of page 1
	cursor := page1[len(page1)-1]
	page2, err := store.ListByStateFrom(ctx, ws, StateActive, cursor, 2)
	if err != nil {
		t.Fatalf("ListByStateFrom page2: %v", err)
	}
	if len(page2) != 2 {
		t.Fatalf("page2 len: got %d, want 2", len(page2))
	}
	// Pages must not overlap
	for _, id := range page2 {
		if id == cursor {
			t.Errorf("page2 contains cursor ID %s", cursor)
		}
	}

	// Page 3: only 1 remaining
	cursor2 := page2[len(page2)-1]
	page3, err := store.ListByStateFrom(ctx, ws, StateActive, cursor2, 2)
	if err != nil {
		t.Fatalf("ListByStateFrom page3: %v", err)
	}
	if len(page3) != 1 {
		t.Fatalf("page3 len: got %d, want 1", len(page3))
	}

	// Exhausted: empty cursor (all-zero) means start from beginning
	fromZero, err := store.ListByStateFrom(ctx, ws, StateActive, ULID{}, 10)
	if err != nil {
		t.Fatalf("ListByStateFrom fromZero: %v", err)
	}
	if len(fromZero) != 5 {
		t.Fatalf("fromZero len: got %d, want 5", len(fromZero))
	}
}
