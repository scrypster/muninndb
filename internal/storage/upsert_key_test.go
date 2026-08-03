package storage

import (
	"context"
	"testing"

	"github.com/cockroachdb/pebble"

	"github.com/scrypster/muninndb/internal/storage/keys"
)

// TestGetUpsertKey_Miss: no forward-index entry → returns (ULID{}, nil),
// so the upsert orchestrator takes the create branch.
func TestGetUpsertKey_Miss(t *testing.T) {
	store := openTestStore(t)
	ws := [8]byte{1, 2, 3, 4, 5, 6, 7, 8}
	hash := [32]byte{0xAA}

	id, err := store.GetUpsertKey(context.Background(), ws, hash)
	if err != nil {
		t.Fatalf("GetUpsertKey miss: unexpected error: %v", err)
	}
	if id != (ULID{}) {
		t.Errorf("expected zero ULID on miss, got %x", id[:])
	}
}

// TestGetUpsertKey_Hit: a seeded 0x2F entry resolves to the stored engram ID.
func TestGetUpsertKey_Hit(t *testing.T) {
	store := openTestStore(t)
	ws := [8]byte{1, 2, 3, 4, 5, 6, 7, 8}
	hash := [32]byte{0xBB}
	want := ULID{0x01, 0x23, 0x45, 0x67, 0x89, 0xAB, 0xCD, 0xEF, 0x10, 0x20,
		0x30, 0x40, 0x50, 0x60, 0x70, 0x80}

	// Seed the forward index directly: 0x2F | ws | sha256 → engramID(16).
	if err := store.db.Set(keys.UpsertKeyKey(ws, hash), want[:], pebble.Sync); err != nil {
		t.Fatalf("seed upsert key: %v", err)
	}

	id, err := store.GetUpsertKey(context.Background(), ws, hash)
	if err != nil {
		t.Fatalf("GetUpsertKey hit: unexpected error: %v", err)
	}
	if id != want {
		t.Errorf("got %x, want %x", id[:], want[:])
	}
}

// TestGetUpsertKey_CorruptValue: a value of the wrong length is an error,
// never a silent zero (fail loud — a short/long value means the index is
// corrupted, not that the key is absent).
func TestGetUpsertKey_CorruptValue(t *testing.T) {
	store := openTestStore(t)
	ws := [8]byte{1, 2, 3, 4, 5, 6, 7, 8}
	hash := [32]byte{0xCC}

	// Write a short (8-byte) value — not a valid ULID.
	if err := store.db.Set(keys.UpsertKeyKey(ws, hash), []byte("short!!!"), pebble.Sync); err != nil {
		t.Fatalf("seed corrupt value: %v", err)
	}

	_, err := store.GetUpsertKey(context.Background(), ws, hash)
	if err == nil {
		t.Fatal("expected error on corrupt (non-16-byte) value, got nil")
	}
}

// TestPutUpsertKey_WritesAndReadsBack: PutUpsertKey writes the 0x2F pointer
// that GetUpsertKey then resolves — the simple, non-batched mirror of
// PutContentHash. This is the primitive the upsert orchestrator's create branch
// uses to pin a freshly created engram under sha256(idempotent_id). The
// atomic-with-evolve re-point path is exercised by the engine-level tests
// (StoreBatch.RepointUpsertKey inside evolveAtInternal's batch).
func TestPutUpsertKey_WritesAndReadsBack(t *testing.T) {
	store := openTestStore(t)
	ws := [8]byte{1, 2, 3, 4, 5, 6, 7, 8}
	hash := [32]byte{0xDD}
	want := ULID{0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88, 0x99, 0xAA,
		0xBB, 0xCC, 0xDD, 0xEE, 0xFF, 0x00}

	if err := store.PutUpsertKey(context.Background(), ws, hash, want); err != nil {
		t.Fatalf("PutUpsertKey: %v", err)
	}

	got, err := store.GetUpsertKey(context.Background(), ws, hash)
	if err != nil {
		t.Fatalf("GetUpsertKey after Put: %v", err)
	}
	if got != want {
		t.Errorf("round-trip mismatch: got %x, want %x", got[:], want[:])
	}

	// PutUpsertKey is idempotent / overwriting — a second put with a new ID
	// replaces the first (the create branch relies on this when re-pointing a
	// stale pointer at a fresh ULID).
	want2 := ULID{0xFF, 0xEE, 0xDD, 0xCC, 0xBB, 0xAA, 0x99, 0x88, 0x77, 0x66,
		0x55, 0x44, 0x33, 0x22, 0x11, 0x00}
	if err := store.PutUpsertKey(context.Background(), ws, hash, want2); err != nil {
		t.Fatalf("PutUpsertKey overwrite: %v", err)
	}
	got2, _ := store.GetUpsertKey(context.Background(), ws, hash)
	if got2 != want2 {
		t.Errorf("overwrite round-trip: got %x, want %x", got2[:], want2[:])
	}
}
