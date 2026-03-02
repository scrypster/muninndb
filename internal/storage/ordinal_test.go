package storage

import (
	"context"
	"strings"
	"testing"
)

func TestOrdinalWriteAndRead(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("ordinal-write-read")
	parent := NewULID()
	child := NewULID()

	if err := store.WriteOrdinal(ctx, ws, parent, child, 3); err != nil {
		t.Fatal(err)
	}
	ord, found, err := store.ReadOrdinal(ctx, ws, parent, child)
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("expected ordinal to be found after write")
	}
	if ord != 3 {
		t.Fatalf("ordinal: got %d, want 3", ord)
	}
}

func TestOrdinalRead_NotFound(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("ordinal-not-found")
	parent := NewULID()
	child := NewULID()

	_, found, err := store.ReadOrdinal(ctx, ws, parent, child)
	if err != nil {
		t.Fatal(err)
	}
	if found {
		t.Fatal("expected ordinal not found for unwritten key")
	}
}

func TestOrdinalDelete(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("ordinal-delete")
	parent := NewULID()
	child := NewULID()

	if err := store.WriteOrdinal(ctx, ws, parent, child, 1); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteOrdinal(ctx, ws, parent, child); err != nil {
		t.Fatal(err)
	}
	_, found, err := store.ReadOrdinal(ctx, ws, parent, child)
	if err != nil {
		t.Fatal(err)
	}
	if found {
		t.Fatal("expected ordinal to be gone after delete")
	}
}

func TestListChildOrdinals_OrderedByOrdinal(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("ordinal-list-sorted")
	parent := NewULID()
	c1, c2, c3 := NewULID(), NewULID(), NewULID()

	// Write out of ordinal order intentionally.
	_ = store.WriteOrdinal(ctx, ws, parent, c3, 30)
	_ = store.WriteOrdinal(ctx, ws, parent, c1, 10)
	_ = store.WriteOrdinal(ctx, ws, parent, c2, 20)

	entries, err := store.ListChildOrdinals(ctx, ws, parent)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(entries))
	}
	if entries[0].Ordinal != 10 || entries[1].Ordinal != 20 || entries[2].Ordinal != 30 {
		t.Errorf("wrong ordinal order: got %v %v %v", entries[0].Ordinal, entries[1].Ordinal, entries[2].Ordinal)
	}
	if entries[0].ChildID != c1 || entries[1].ChildID != c2 || entries[2].ChildID != c3 {
		t.Errorf("wrong child IDs in sorted order")
	}
}

func TestListChildOrdinals_Empty(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("ordinal-list-empty")
	parent := NewULID()

	entries, err := store.ListChildOrdinals(ctx, ws, parent)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected 0 entries for parent with no children, got %d", len(entries))
	}
}

func TestDeleteEngramOrdinal(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("delete-ordinal-test")

	parent := NewULID()
	child := NewULID()

	// Write ordinal.
	if err := store.WriteOrdinal(ctx, ws, parent, child, 1); err != nil {
		t.Fatal(err)
	}
	// Verify it exists.
	_, found, err := store.ReadOrdinal(ctx, ws, parent, child)
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("ordinal should exist after write")
	}

	// Delete via DeleteEngramOrdinal.
	if err := store.DeleteEngramOrdinal(ctx, ws, parent, child); err != nil {
		t.Fatalf("DeleteEngramOrdinal: %v", err)
	}

	// Verify gone.
	_, found, err = store.ReadOrdinal(ctx, ws, parent, child)
	if err != nil {
		t.Fatal(err)
	}
	if found {
		t.Error("ordinal should be removed after DeleteEngramOrdinal")
	}
}

// TestWriteOrdinal_RejectsNegative verifies that WriteOrdinal rejects
// negative ordinal values with a clear error message.
func TestWriteOrdinal_RejectsNegative(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("ordinal-reject-negative")

	parent := NewULID()
	child := NewULID()

	// Attempt to write a negative ordinal.
	err := store.WriteOrdinal(ctx, ws, parent, child, -1)
	if err == nil {
		t.Fatal("expected error when writing negative ordinal, got nil")
	}

	// Verify error message contains guidance about non-negative requirement.
	errMsg := err.Error()
	if !strings.Contains(errMsg, "non-negative") {
		t.Errorf("expected error message to mention 'non-negative', got: %q", errMsg)
	}
}
