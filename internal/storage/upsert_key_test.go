package storage

import (
	"context"
	"testing"
	"time"

	"github.com/cockroachdb/pebble"

	"github.com/scrypster/muninndb/internal/storage/keys"
)

// TestGetUpsertKey_Miss: no forward-index entry → returns (ULID{}, nil),
// so the upsert write path creates a fresh engram.
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

// TestGetUpsertKey_Hit: a seeded 0x2B entry resolves to the stored engram ID.
func TestGetUpsertKey_Hit(t *testing.T) {
	store := openTestStore(t)
	ws := [8]byte{1, 2, 3, 4, 5, 6, 7, 8}
	hash := [32]byte{0xBB}
	want := ULID{0x01, 0x23, 0x45, 0x67, 0x89, 0xAB, 0xCD, 0xEF, 0x10, 0x20,
		0x30, 0x40, 0x50, 0x60, 0x70, 0x80}

	// Seed the forward index directly: 0x2B | ws | sha256 → engramID(16).
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

// TestUpsertEngram_Miss_CreatesAndIndexes: a miss writes the engram, the 0x28
// content-hash entry, AND the 0x2B upsert-key entry in one atomic batch — all
// three resolve to the same engram ID afterward. This is the atomicity claim
// that distinguishes upsert from the default Write path (where PutContentHash
// is a separate, non-co-committed commit).
func TestUpsertEngram_Miss_CreatesAndIndexes(t *testing.T) {
	store := openTestStore(t)
	ws := [8]byte{1, 2, 3, 4, 5, 6, 7, 8}
	keyHash := [32]byte{0x11}
	eng := &Engram{Concept: "upsert-test", Content: "body", Tags: []string{"a"}}

	id, created, err := store.UpsertEngram(context.Background(), ws, eng, keyHash)
	if err != nil {
		t.Fatalf("UpsertEngram miss: %v", err)
	}
	if !created {
		t.Error("expected created=true on miss")
	}
	if id == (ULID{}) {
		t.Fatal("expected non-zero ULID")
	}

	// 0x2B upsert-key entry maps keyHash → id.
	got, err := store.GetUpsertKey(context.Background(), ws, keyHash)
	if err != nil {
		t.Fatalf("GetUpsertKey after miss: %v", err)
	}
	if got != id {
		t.Errorf("upsert-key entry: got %x, want %x", got[:], id[:])
	}

	// 0x28 content-hash entry maps ContentHash(content) → id.
	chID, err := store.GetContentHash(context.Background(), ws, ContentHash(eng.Content))
	if err != nil {
		t.Fatalf("GetContentHash after miss: %v", err)
	}
	if chID != id {
		t.Errorf("content-hash entry: got %x, want %x", chID[:], id[:])
	}

	// The engram itself is readable.
	gotEng, err := store.GetEngram(context.Background(), ws, id)
	if err != nil {
		t.Fatalf("GetEngram after miss: %v", err)
	}
	if gotEng == nil {
		t.Fatal("engram nil after upsert miss")
	}
	if gotEng.Content != "body" {
		t.Errorf("content: got %q, want %q", gotEng.Content, "body")
	}
}

// TestUpsertEngram_Hit_ChangedContent_MergesInPlace: a second upsert with the
// same key but changed content merges on the SAME ULID — cognitive fields
// preserved, content/tags overwritten, stale tag + content-hash indexes swept,
// new ones written. created=false.
func TestUpsertEngram_Hit_ChangedContent_MergesInPlace(t *testing.T) {
	store := openTestStore(t)
	ws := [8]byte{1, 2, 3, 4, 5, 6, 7, 8}
	keyHash := [32]byte{0x22}

	// First write: content "v1", tag "red", a marked Confidence to assert preservation.
	eng1 := &Engram{Concept: "c", Content: "v1", Tags: []string{"red"}, Confidence: 0.42}
	id1, created, err := store.UpsertEngram(context.Background(), ws, eng1, keyHash)
	if err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	if !created {
		t.Fatal("first: expected created=true")
	}

	// Second write: same key, content "v2", tag "green".
	eng2 := &Engram{Concept: "c", Content: "v2", Tags: []string{"green"}}
	id2, created2, err := store.UpsertEngram(context.Background(), ws, eng2, keyHash)
	if err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	if created2 {
		t.Fatal("second: expected created=false (merge in place)")
	}
	if id2 != id1 {
		t.Errorf("merge changed the ULID: got %x, want %x", id2[:], id1[:])
	}

	// Content overwritten; cognitive field preserved.
	got, err := store.GetEngram(context.Background(), ws, id1)
	if err != nil {
		t.Fatalf("GetEngram after merge: %v", err)
	}
	if got.Content != "v2" {
		t.Errorf("content: got %q, want %q", got.Content, "v2")
	}
	if got.Confidence != 0.42 {
		t.Errorf("Confidence not preserved: got %v, want 0.42", got.Confidence)
	}

	// Old tag index ("red") swept; new ("green") present.
	if _, closer, err := store.db.Get(keys.TagIndexKey(ws, keys.Hash("red"), [16]byte(id1))); err == nil {
		closer.Close()
		t.Error("stale tag index \"red\" still present after merge")
	}
	if _, closer, err := store.db.Get(keys.TagIndexKey(ws, keys.Hash("green"), [16]byte(id1))); err != nil {
		t.Errorf("new tag index \"green\" missing: %v", err)
	} else {
		closer.Close()
	}

	// Content-hash re-pointed: "v1" entry gone, "v2" entry → id1.
	if oldCH, _ := store.GetContentHash(context.Background(), ws, ContentHash("v1")); oldCH != (ULID{}) {
		t.Errorf("stale content-hash for \"v1\" still present: %x", oldCH[:])
	}
	if newCH, _ := store.GetContentHash(context.Background(), ws, ContentHash("v2")); newCH != id1 {
		t.Errorf("content-hash for \"v2\": got %x, want %x", newCH[:], id1[:])
	}

	// Upsert-key entry still points to id1 (unchanged on merge).
	if uk, _ := store.GetUpsertKey(context.Background(), ws, keyHash); uk != id1 {
		t.Errorf("upsert-key re-pointed on merge: got %x, want %x", uk[:], id1[:])
	}
}

// TestUpsertEngram_Hit_IdenticalContent_PreservesHash: a re-write with byte-
// identical content merges in place (same ULID) and leaves the content-hash
// entry intact (the merge must not delete what it then re-creates — a no-op
// content change must not drop the index even transiently).
func TestUpsertEngram_Hit_IdenticalContent_PreservesHash(t *testing.T) {
	store := openTestStore(t)
	ws := [8]byte{1, 2, 3, 4, 5, 6, 7, 8}
	keyHash := [32]byte{0x33}
	eng1 := &Engram{Concept: "c", Content: "same", Tags: []string{"a"}}
	id1, _, err := store.UpsertEngram(context.Background(), ws, eng1, keyHash)
	if err != nil {
		t.Fatalf("first: %v", err)
	}

	eng2 := &Engram{Concept: "c", Content: "same", Tags: []string{"a"}}
	id2, created, err := store.UpsertEngram(context.Background(), ws, eng2, keyHash)
	if err != nil {
		t.Fatalf("identical re-write: %v", err)
	}
	if created {
		t.Fatal("identical re-write: expected created=false (merge)")
	}
	if id2 != id1 {
		t.Errorf("ULID changed on identical merge: got %x, want %x", id2[:], id1[:])
	}

	ch, _ := store.GetContentHash(context.Background(), ws, ContentHash("same"))
	if ch != id1 {
		t.Errorf("content-hash for identical content not preserved: got %x, want %x", ch[:], id1[:])
	}
}

// TestUpsertEngram_Hit_SoftDeleted_Recreates: when the forward index points at
// a non-Active (soft-deleted) engram, the StateActive guard must treat it as a
// miss — mint a fresh ULID and re-point the index — never merge into the
// tombstone (RedTeam #556 Change-2: silent data loss otherwise).
func TestUpsertEngram_Hit_SoftDeleted_Recreates(t *testing.T) {
	store := openTestStore(t)
	ws := [8]byte{1, 2, 3, 4, 5, 6, 7, 8}
	keyHash := [32]byte{0x44}
	eng1 := &Engram{Concept: "c", Content: "v1"}
	id1, _, err := store.UpsertEngram(context.Background(), ws, eng1, keyHash)
	if err != nil {
		t.Fatalf("first: %v", err)
	}

	// Soft-delete the pinned engram → the 0x2B entry now points at a tombstone.
	b := store.NewBatch()
	if err := b.UpdateEngramState(context.Background(), ws, id1, StateSoftDeleted); err != nil {
		t.Fatalf("soft delete: %v", err)
	}
	if err := b.Commit(); err != nil {
		t.Fatalf("commit soft delete: %v", err)
	}

	eng2 := &Engram{Concept: "c", Content: "v2"}
	id2, created, err := store.UpsertEngram(context.Background(), ws, eng2, keyHash)
	if err != nil {
		t.Fatalf("recreate: %v", err)
	}
	if !created {
		t.Fatal("soft-deleted pin: expected created=true (StateActive guard recreate)")
	}
	if id2 == id1 {
		t.Fatal("recreate should mint a new ULID, not merge into the tombstone")
	}

	// The 0x2B entry is re-pointed to the fresh engram.
	uk, _ := store.GetUpsertKey(context.Background(), ws, keyHash)
	if uk != id2 {
		t.Errorf("upsert-key not re-pointed to the new id: got %x, want %x", uk[:], id2[:])
	}

	got, err := store.GetEngram(context.Background(), ws, id2)
	if err != nil {
		t.Fatalf("GetEngram new: %v", err)
	}
	if got.Content != "v2" {
		t.Errorf("new engram content: got %q, want %q", got.Content, "v2")
	}
}

// TestUpsertEngram_Hit_HardDeleted_Recreates: when the forward index points at
// a hard-deleted engram (0x01 gone — e.g. after Forget, or ClearVault left 0x2B
// dangling), the upsert must treat it as a stale pointer and create fresh, NOT
// error forever. Pre-fix this returned "load pinned engram: engram not found"
// because GetEngram returns (nil, ErrNotFound), not (nil, nil), so the
// `pinned == nil` guard was dead code.
func TestUpsertEngram_Hit_HardDeleted_Recreates(t *testing.T) {
	store := openTestStore(t)
	ws := [8]byte{1, 2, 3, 4, 5, 6, 7, 8}
	keyHash := [32]byte{0x55}
	eng1 := &Engram{Concept: "c", Content: "v1"}
	id1, _, err := store.UpsertEngram(context.Background(), ws, eng1, keyHash)
	if err != nil {
		t.Fatalf("first upsert: %v", err)
	}

	// Hard-delete the pinned engram. DeleteEngram does NOT sweep 0x2B, so the
	// forward-index entry → id1 now dangles (0x01 for id1 is gone).
	if err := store.DeleteEngram(context.Background(), ws, id1); err != nil {
		t.Fatalf("hard delete: %v", err)
	}

	// Re-upsert on the same key: the stale pointer must NOT error — recreate.
	eng2 := &Engram{Concept: "c", Content: "v2"}
	id2, created, err := store.UpsertEngram(context.Background(), ws, eng2, keyHash)
	if err != nil {
		t.Fatalf("recreate after hard-delete returned error (the dead-guard bug): %v", err)
	}
	if !created {
		t.Fatal("expected created=true (recreate from a stale/hard-deleted pointer)")
	}
	if id2 == id1 {
		t.Fatal("recreate should mint a new ULID, not reuse the deleted one")
	}

	// The 0x2B entry is re-pointed to the fresh engram.
	uk, _ := store.GetUpsertKey(context.Background(), ws, keyHash)
	if uk != id2 {
		t.Errorf("upsert-key not re-pointed to the new id: got %x, want %x", uk[:], id2[:])
	}
}

// TestUpsertEngram_MergeSerializesWithCASLock: the merge path must hold
// casLocks.For(pinnedID) across read→commit (invariant STO-2), so it serializes
// with a concurrent DeleteEngram / CompareAndSet / AdjustConfidence on the same
// ULID. Holding the stripe lock externally stands in for an in-flight CAS: a
// correct upsertMerge must block until it is released (the #594
// lost-update/resurrection race). Mirrors TestDeleteEngramSerializesWithCompareAndSet.
func TestUpsertEngram_MergeSerializesWithCASLock(t *testing.T) {
	store := openTestStore(t)
	ws := [8]byte{1, 2, 3, 4, 5, 6, 7, 8}
	keyHash := [32]byte{0x66}
	id1, _, err := store.UpsertEngram(context.Background(), ws, &Engram{Concept: "c", Content: "v1"}, keyHash)
	if err != nil {
		t.Fatalf("seed upsert: %v", err)
	}

	// Hold the CAS stripe lock for the pinned engram (stands in for an in-flight
	// concurrent DeleteEngram/CAS/AdjustConfidence on the same ULID).
	mu := store.casLocks.For(id1[:])
	mu.Lock()

	done := make(chan struct{})
	go func() {
		_, _, _ = store.UpsertEngram(context.Background(), ws, &Engram{Concept: "c", Content: "v2"}, keyHash)
		close(done)
	}()

	select {
	case <-done:
		mu.Unlock()
		t.Fatal("UpsertEngram merge completed while the pinned engram's CAS stripe lock was held; " +
			"it does not serialize with DeleteEngram/CompareAndSet — #594 race reopened")
	case <-time.After(100 * time.Millisecond):
		// Expected: the merge is blocked on the stripe lock.
	}

	mu.Unlock()

	select {
	case <-done:
		// merge completed after the lock was released — correct.
	case <-time.After(2 * time.Second):
		t.Fatal("UpsertEngram merge did not complete after releasing the CAS stripe lock")
	}
}

// TestUpsertEngram_Merge_OverwritesMemoryType: the request's MemoryType must
// overwrite the existing engram's on merge (a re-ingested doc can change type).
// Pre-fix upsertMerge copied every content field EXCEPT MemoryType, freezing
// the classification at the first write's value.
func TestUpsertEngram_Merge_OverwritesMemoryType(t *testing.T) {
	store := openTestStore(t)
	ws := [8]byte{1, 2, 3, 4, 5, 6, 7, 8}
	keyHash := [32]byte{0x77}
	id1, _, err := store.UpsertEngram(context.Background(), ws,
		&Engram{Content: "v1", MemoryType: MemoryType(3)}, keyHash)
	if err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	if _, _, err := store.UpsertEngram(context.Background(), ws,
		&Engram{Content: "v2", MemoryType: MemoryType(5)}, keyHash); err != nil {
		t.Fatalf("merge upsert: %v", err)
	}
	got, _ := store.GetEngram(context.Background(), ws, id1)
	if got.MemoryType != MemoryType(5) {
		t.Errorf("MemoryType not overwritten on merge: got %v, want 5", got.MemoryType)
	}
}
