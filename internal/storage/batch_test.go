package storage

import (
	"context"
	"testing"
)

// newTestStoreForBatch creates a PebbleStore backed by a temp Pebble DB.
// store.Close() drains background goroutines, closes the DB, and removes the dir.
//
// Do NOT use openTestPebble here: PebbleStore.Close() already calls
// db.Close() internally. A second db.Close() from openTestPebble's cleanup
// would cause pebble to panic with "pebble: closed".
func newTestStoreForBatch(t *testing.T) *PebbleStore {
	t.Helper()
	return openTestStore(t)
}

// TestStoreBatch_CommitWritesTwoEngrams verifies that committing a batch with
// two engrams makes both readable via ReadEngram / GetEngram.
func TestStoreBatch_CommitWritesTwoEngrams(t *testing.T) {
	ctx := context.Background()
	store := newTestStoreForBatch(t)
	ws := store.VaultPrefix("batch-test")

	eng1 := &Engram{Concept: "Alpha", Content: "first"}
	eng2 := &Engram{Concept: "Beta", Content: "second"}

	batch := store.NewBatch()
	defer batch.Discard()

	if err := batch.WriteEngram(ctx, ws, eng1); err != nil {
		t.Fatalf("WriteEngram eng1: %v", err)
	}
	if err := batch.WriteEngram(ctx, ws, eng2); err != nil {
		t.Fatalf("WriteEngram eng2: %v", err)
	}
	if err := batch.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	// Verify both engrams are readable.
	got1, err := store.GetEngram(ctx, ws, eng1.ID)
	if err != nil {
		t.Fatalf("GetEngram eng1: %v", err)
	}
	if got1 == nil {
		t.Fatal("eng1 not found after commit")
	}
	if got1.Concept != "Alpha" {
		t.Errorf("eng1 concept: got %q want %q", got1.Concept, "Alpha")
	}

	got2, err := store.GetEngram(ctx, ws, eng2.ID)
	if err != nil {
		t.Fatalf("GetEngram eng2: %v", err)
	}
	if got2 == nil {
		t.Fatal("eng2 not found after commit")
	}
	if got2.Concept != "Beta" {
		t.Errorf("eng2 concept: got %q want %q", got2.Concept, "Beta")
	}
}

// TestStoreBatch_DiscardWritesNothing verifies that calling Discard (without
// Commit) leaves no engrams in the store.
func TestStoreBatch_DiscardWritesNothing(t *testing.T) {
	ctx := context.Background()
	store := newTestStoreForBatch(t)
	ws := store.VaultPrefix("discard-test")

	eng := &Engram{Concept: "Ephemeral", Content: "never committed"}

	batch := store.NewBatch()
	if err := batch.WriteEngram(ctx, ws, eng); err != nil {
		t.Fatalf("WriteEngram: %v", err)
	}
	// Discard without committing.
	batch.Discard()

	// Verify the engram was NOT written.
	// eng.ID is zero if WriteEngram never assigned one; that would also not exist.
	if eng.ID != (ULID{}) {
		got, err := store.GetEngram(ctx, ws, eng.ID)
		// GetEngram returns an error ("engram not found") or nil engram when absent.
		if err == nil && got != nil {
			t.Fatal("engram found after Discard — expected no write")
		}
	}
}

// TestStoreBatch_DiscardAfterCommit_IsIdempotent verifies that calling Discard
// after a successful Commit does not panic or error.
func TestStoreBatch_DiscardAfterCommit_IsIdempotent(t *testing.T) {
	ctx := context.Background()
	store := newTestStoreForBatch(t)
	ws := store.VaultPrefix("idempotent-test")

	eng := &Engram{Concept: "Safe", Content: "committed then discarded"}

	batch := store.NewBatch()
	if err := batch.WriteEngram(ctx, ws, eng); err != nil {
		t.Fatalf("WriteEngram: %v", err)
	}
	if err := batch.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	// Calling Discard after Commit must be a no-op (should not panic).
	batch.Discard()

	// Engram should still be readable.
	got, err := store.GetEngram(ctx, ws, eng.ID)
	if err != nil {
		t.Fatalf("GetEngram after double-discard: %v", err)
	}
	if got == nil {
		t.Fatal("engram not found after Commit + Discard")
	}
}

// TestStoreBatch_DefaultsApplied verifies that the batch applies the same
// field defaults (state, confidence, stability, timestamps) as WriteEngram.
func TestStoreBatch_DefaultsApplied(t *testing.T) {
	ctx := context.Background()
	store := newTestStoreForBatch(t)
	ws := store.VaultPrefix("defaults-test")

	eng := &Engram{Concept: "Defaulted", Content: "check defaults"}

	batch := store.NewBatch()
	defer batch.Discard()

	if err := batch.WriteEngram(ctx, ws, eng); err != nil {
		t.Fatalf("WriteEngram: %v", err)
	}
	if err := batch.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	got, err := store.GetEngram(ctx, ws, eng.ID)
	if err != nil {
		t.Fatalf("GetEngram: %v", err)
	}
	if got.State != StateActive {
		t.Errorf("state: got %v want StateActive", got.State)
	}
	if got.Confidence != 1.0 {
		t.Errorf("confidence: got %v want 1.0", got.Confidence)
	}
	if got.Stability != 30.0 {
		t.Errorf("stability: got %v want 30.0", got.Stability)
	}
	if got.CreatedAt.IsZero() {
		t.Error("CreatedAt should not be zero")
	}
}
