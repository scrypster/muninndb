package storage

import (
	"context"
	"testing"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// writeTestEngram writes a minimal valid Engram and returns its assigned ULID.
func writeTestEngram(t *testing.T, store *PebbleStore, ws [8]byte, concept, content string) ULID {
	t.Helper()
	eng := &Engram{
		Concept: concept,
		Content: content,
	}
	id, err := store.WriteEngram(context.Background(), ws, eng)
	if err != nil {
		t.Fatalf("WriteEngram(%q): %v", concept, err)
	}
	return id
}

// ---------------------------------------------------------------------------
// GetEngrams
// ---------------------------------------------------------------------------

// TestGetEngrams_Roundtrip writes 3 engrams and verifies GetEngrams returns all 3.
func TestGetEngrams_Roundtrip(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("getengrams-roundtrip")

	id1 := writeTestEngram(t, store, ws, "concept-a", "content-a")
	id2 := writeTestEngram(t, store, ws, "concept-b", "content-b")
	id3 := writeTestEngram(t, store, ws, "concept-c", "content-c")

	results, err := store.GetEngrams(ctx, ws, []ULID{id1, id2, id3})
	if err != nil {
		t.Fatalf("GetEngrams: %v", err)
	}

	if len(results) != 3 {
		t.Fatalf("expected 3 results, got %d", len(results))
	}

	// Each slot must be non-nil and must match the expected concept.
	expected := []struct {
		id      ULID
		concept string
	}{
		{id1, "concept-a"},
		{id2, "concept-b"},
		{id3, "concept-c"},
	}
	for i, e := range expected {
		if results[i] == nil {
			t.Errorf("slot %d: expected non-nil engram for id %v", i, e.id)
			continue
		}
		if results[i].ID != e.id {
			t.Errorf("slot %d: ID mismatch: got %v, want %v", i, results[i].ID, e.id)
		}
		if results[i].Concept != e.concept {
			t.Errorf("slot %d: Concept mismatch: got %q, want %q", i, results[i].Concept, e.concept)
		}
	}
}

// TestGetEngrams_EmptyInput verifies that an empty ULID slice returns an empty
// result slice with no error.
func TestGetEngrams_EmptyInput(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("getengrams-empty")

	results, err := store.GetEngrams(ctx, ws, []ULID{})
	if err != nil {
		t.Fatalf("GetEngrams(empty): %v", err)
	}
	if len(results) != 0 {
		t.Errorf("expected empty result, got %d elements", len(results))
	}
}

// TestGetEngrams_DanglingID verifies that a non-existent ID causes that slot to
// be nil while the other slots are populated correctly.
func TestGetEngrams_DanglingID(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("getengrams-dangling")

	id1 := writeTestEngram(t, store, ws, "real-concept", "real-content")
	dangling := NewULID() // never written

	// Request real ID first, then the dangling one.
	results, err := store.GetEngrams(ctx, ws, []ULID{id1, dangling})
	if err != nil {
		t.Fatalf("GetEngrams: %v", err)
	}

	if len(results) != 2 {
		t.Fatalf("expected 2 result slots, got %d", len(results))
	}
	if results[0] == nil {
		t.Error("slot 0 (real id): expected non-nil engram")
	}
	if results[1] != nil {
		t.Errorf("slot 1 (dangling id): expected nil, got %+v", results[1])
	}
}

// ---------------------------------------------------------------------------
// UpdateMetadata
// ---------------------------------------------------------------------------

// TestUpdateMetadata_ChangesConfidence writes an engram, updates its confidence
// via UpdateMetadata, and verifies the change is visible through GetEngram.
func TestUpdateMetadata_ChangesConfidence(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("updatemeta-confidence")

	id := writeTestEngram(t, store, ws, "meta-concept", "meta-content")

	// Read current metadata to build an updated copy.
	metas, err := store.GetMetadata(ctx, ws, []ULID{id})
	if err != nil {
		t.Fatalf("GetMetadata: %v", err)
	}
	if len(metas) == 0 || metas[0] == nil {
		t.Fatal("GetMetadata returned nothing for newly-written engram")
	}

	// Patch only confidence; copy all other fields.
	updated := *metas[0]
	updated.Confidence = 0.5

	if err := store.UpdateMetadata(ctx, ws, id, &updated); err != nil {
		t.Fatalf("UpdateMetadata: %v", err)
	}

	// Verify via GetEngram (bypasses cache because UpdateMetadata invalidates it).
	eng, err := store.GetEngram(ctx, ws, id)
	if err != nil {
		t.Fatalf("GetEngram after UpdateMetadata: %v", err)
	}
	if eng.Confidence != 0.5 {
		t.Errorf("Confidence: got %v, want 0.5", eng.Confidence)
	}
}

// TestUpdateMetadata_NotFound verifies that UpdateMetadata returns an error for
// a non-existent engram ID.
func TestUpdateMetadata_NotFound(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("updatemeta-notfound")

	ghost := NewULID()
	meta := &EngramMeta{
		ID:         ghost,
		Confidence: 0.9,
		State:      StateActive,
	}
	err := store.UpdateMetadata(ctx, ws, ghost, meta)
	if err == nil {
		t.Fatal("expected error when updating non-existent engram, got nil")
	}
}

// ---------------------------------------------------------------------------
// UpdateTags
// ---------------------------------------------------------------------------

// TestUpdateTags_ReplacesTags writes an engram with tags ["a","b"], then calls
// UpdateTags with ["c","d"], and verifies the new tags via GetEngram.
func TestUpdateTags_ReplacesTags(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("updatetags-replace")

	eng := &Engram{
		Concept: "tag-concept",
		Content: "tag-content",
		Tags:    []string{"a", "b"},
	}
	id, err := store.WriteEngram(ctx, ws, eng)
	if err != nil {
		t.Fatalf("WriteEngram: %v", err)
	}

	if err := store.UpdateTags(ctx, ws, id, []string{"c", "d"}); err != nil {
		t.Fatalf("UpdateTags: %v", err)
	}

	got, err := store.GetEngram(ctx, ws, id)
	if err != nil {
		t.Fatalf("GetEngram after UpdateTags: %v", err)
	}

	wantTags := map[string]bool{"c": true, "d": true}
	if len(got.Tags) != 2 {
		t.Errorf("expected 2 tags after update, got %d: %v", len(got.Tags), got.Tags)
	}
	for _, tag := range got.Tags {
		if !wantTags[tag] {
			t.Errorf("unexpected tag %q in updated engram", tag)
		}
	}
}

// TestUpdateTags_NotFound verifies that UpdateTags returns an error for a
// non-existent engram ID.
func TestUpdateTags_NotFound(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("updatetags-notfound")

	ghost := NewULID()
	err := store.UpdateTags(ctx, ws, ghost, []string{"tag1"})
	if err == nil {
		t.Fatal("expected error when updating tags of non-existent engram, got nil")
	}
}

// ---------------------------------------------------------------------------
// GetEmbedding
// ---------------------------------------------------------------------------

// TestGetEmbedding_NoEmbedding verifies that GetEmbedding returns nil (not an
// error) when no embedding was stored for an engram.
func TestGetEmbedding_NoEmbedding(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("getembedding-none")

	// Write an engram without an embedding.
	id := writeTestEngram(t, store, ws, "no-embed-concept", "no-embed-content")

	embedding, err := store.GetEmbedding(ctx, ws, id)
	if err != nil {
		t.Fatalf("GetEmbedding: %v", err)
	}
	if embedding != nil {
		t.Errorf("expected nil embedding, got slice of length %d", len(embedding))
	}
}

// TestGetEmbedding_Roundtrip writes an engram with an embedding and verifies
// that GetEmbedding returns values that are close to the originals.
// The storage layer uses quantized int8; values are approximate after round-trip.
func TestGetEmbedding_Roundtrip(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("getembedding-roundtrip")

	want := []float32{0.1, 0.5, -0.3, 0.9, -0.7}
	eng := &Engram{
		Concept:   "embed-concept",
		Content:   "embed-content",
		Embedding: want,
	}
	id, err := store.WriteEngram(ctx, ws, eng)
	if err != nil {
		t.Fatalf("WriteEngram with embedding: %v", err)
	}

	got, err := store.GetEmbedding(ctx, ws, id)
	if err != nil {
		t.Fatalf("GetEmbedding: %v", err)
	}
	if got == nil {
		t.Fatal("expected non-nil embedding, got nil")
	}
	if len(got) != len(want) {
		t.Fatalf("embedding length mismatch: got %d, want %d", len(got), len(want))
	}
	// int8 quantization introduces ~0.01 error; allow 0.02 tolerance.
	const tolerance = 0.02
	for i := range want {
		diff := got[i] - want[i]
		if diff < 0 {
			diff = -diff
		}
		if diff > tolerance {
			t.Errorf("embedding[%d]: got %v, want %v (diff %v > tolerance %v)", i, got[i], want[i], diff, tolerance)
		}
	}
}

// ---------------------------------------------------------------------------
// DeleteEngram
// ---------------------------------------------------------------------------

// TestDeleteEngram_RemovesRecord writes then hard-deletes an engram and verifies
// that a subsequent GetEngram returns an error.
func TestDeleteEngram_RemovesRecord(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("deleteengram-removes")

	id := writeTestEngram(t, store, ws, "to-delete", "will be gone")

	// Sanity check: the engram exists before deletion.
	if _, err := store.GetEngram(ctx, ws, id); err != nil {
		t.Fatalf("GetEngram before delete: %v", err)
	}

	if err := store.DeleteEngram(ctx, ws, id); err != nil {
		t.Fatalf("DeleteEngram: %v", err)
	}

	// After deletion, GetEngram must return an error.
	_, err := store.GetEngram(ctx, ws, id)
	if err == nil {
		t.Fatal("expected error from GetEngram after delete, got nil")
	}
}

// TestDeleteEngram_AutoCleansOrdinalKey verifies that DeleteEngram atomically removes
// any ordinal keys where the deleted engram is the child.
func TestDeleteEngram_AutoCleansOrdinalKey(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("auto-ordinal-clean")

	// Write parent and child engrams.
	parentEng := &Engram{Concept: "parent", Content: "root"}
	parentID, err := store.WriteEngram(ctx, ws, parentEng)
	if err != nil {
		t.Fatal(err)
	}
	childEng := &Engram{Concept: "child", Content: "leaf"}
	childID, err := store.WriteEngram(ctx, ws, childEng)
	if err != nil {
		t.Fatal(err)
	}

	// Write ordinal key: parent → child at ordinal 1.
	if err := store.WriteOrdinal(ctx, ws, parentID, childID, 1); err != nil {
		t.Fatal(err)
	}

	// Verify ordinal exists.
	_, found, err := store.ReadOrdinal(ctx, ws, parentID, childID)
	if err != nil || !found {
		t.Fatalf("ordinal should exist after write: err=%v found=%v", err, found)
	}

	// Hard-delete the child engram.
	if err := store.DeleteEngram(ctx, ws, childID); err != nil {
		t.Fatalf("DeleteEngram: %v", err)
	}

	// Ordinal key must be gone automatically.
	_, found, err = store.ReadOrdinal(ctx, ws, parentID, childID)
	if err != nil {
		t.Fatalf("ReadOrdinal after delete: %v", err)
	}
	if found {
		t.Error("ordinal key should be automatically removed when child engram is deleted")
	}
}

// TestDeleteEngram_AutoCleansParentOrdinalKeys verifies that DeleteEngram atomically
// removes all ordinal keys where the deleted engram was the parent
// (i.e. keys of the form 0x1E|ws|deletedID|childID).
func TestDeleteEngram_AutoCleansParentOrdinalKeys(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("auto-parent-ordinal-clean")

	// Write parent P and children C1, C2.
	parentID, err := store.WriteEngram(ctx, ws, &Engram{Concept: "parent", Content: "root"})
	if err != nil {
		t.Fatal(err)
	}
	c1ID, err := store.WriteEngram(ctx, ws, &Engram{Concept: "child1", Content: "leaf1"})
	if err != nil {
		t.Fatal(err)
	}
	c2ID, err := store.WriteEngram(ctx, ws, &Engram{Concept: "child2", Content: "leaf2"})
	if err != nil {
		t.Fatal(err)
	}

	// Register both children under P with ordinals.
	if err := store.WriteOrdinal(ctx, ws, parentID, c1ID, 1); err != nil {
		t.Fatal(err)
	}
	if err := store.WriteOrdinal(ctx, ws, parentID, c2ID, 2); err != nil {
		t.Fatal(err)
	}

	// Sanity check: both ordinal keys exist before deleting the parent.
	entries, err := store.ListChildOrdinals(ctx, ws, parentID)
	if err != nil {
		t.Fatalf("ListChildOrdinals before delete: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 child ordinals before delete, got %d", len(entries))
	}

	// Hard-delete the parent engram P.
	if err := store.DeleteEngram(ctx, ws, parentID); err != nil {
		t.Fatalf("DeleteEngram(parent): %v", err)
	}

	// All parent-role ordinal keys must be gone automatically.
	entries, err = store.ListChildOrdinals(ctx, ws, parentID)
	if err != nil {
		t.Fatalf("ListChildOrdinals after delete: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("expected 0 child ordinals after parent deleted, got %d", len(entries))
	}
}

// TestDeleteEngram_WithAssociations writes two engrams, links A->B, deletes A,
// and verifies that the association forward and reverse keys are removed from
// Pebble so GetAssociations (on a fresh, cache-cold store) returns no edges from A.
func TestDeleteEngram_WithAssociations(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("deleteengram-assoc")

	idA := writeTestEngram(t, store, ws, "engram-A", "content-A")
	idB := writeTestEngram(t, store, ws, "engram-B", "content-B")

	// Create a directed association A->B.
	assoc := &Association{
		TargetID: idB,
		Weight:   0.7,
	}
	if err := store.WriteAssociation(ctx, ws, idA, idB, assoc); err != nil {
		t.Fatalf("WriteAssociation: %v", err)
	}

	// Confirm the association exists before deleting A.
	pre, err := store.GetAssociations(ctx, ws, []ULID{idA}, 10)
	if err != nil {
		t.Fatalf("GetAssociations before delete: %v", err)
	}
	if len(pre[idA]) == 0 {
		t.Fatal("expected A->B association before delete, found none")
	}

	// Hard-delete A. This must cascade and remove association keys.
	if err := store.DeleteEngram(ctx, ws, idA); err != nil {
		t.Fatalf("DeleteEngram(A): %v", err)
	}

	// The engram A must be gone.
	if _, err := store.GetEngram(ctx, ws, idA); err == nil {
		t.Error("expected error from GetEngram(A) after delete, got nil")
	}

	// Open a fresh store instance sharing the same underlying DB so the
	// assocCache (TTL=2s) starts cold and reads straight from Pebble.
	// This confirms the physical association keys were actually removed.
	freshStore := newFreshStore(t, store.db)
	post, err := freshStore.GetAssociations(ctx, ws, []ULID{idA}, 10)
	if err != nil {
		t.Fatalf("GetAssociations after delete (fresh store): %v", err)
	}
	if len(post[idA]) != 0 {
		t.Errorf("expected 0 associations from A after delete, got %d", len(post[idA]))
	}
}

// TestGetMetadata_ReturnNilForMissing verifies that GetMetadata returns nil
// entries for non-existent engrams without error, allowing callers to
// distinguish missing from present engrams in a batch call.
func TestGetMetadata_ReturnNilForMissing(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("getmetadata-missing")

	// Write two engrams.
	id1 := writeTestEngram(t, store, ws, "exists-1", "content-1")
	id2 := writeTestEngram(t, store, ws, "exists-2", "content-2")

	// Create a non-existent ID (never written).
	missingID := NewULID()

	// Call GetMetadata with all three IDs (real, real, missing).
	metas, err := store.GetMetadata(ctx, ws, []ULID{id1, id2, missingID})
	if err != nil {
		t.Fatalf("GetMetadata: %v", err)
	}

	// Verify we get exactly 3 result slots.
	if len(metas) != 3 {
		t.Fatalf("expected 3 metadata results, got %d", len(metas))
	}

	// Slot 0 (id1): should be non-nil.
	if metas[0] == nil {
		t.Error("slot 0 (id1): expected non-nil metadata")
	} else if metas[0].ID != id1 {
		t.Errorf("slot 0: ID mismatch: got %v, want %v", metas[0].ID, id1)
	}

	// Slot 1 (id2): should be non-nil.
	if metas[1] == nil {
		t.Error("slot 1 (id2): expected non-nil metadata")
	} else if metas[1].ID != id2 {
		t.Errorf("slot 1: ID mismatch: got %v, want %v", metas[1].ID, id2)
	}

	// Slot 2 (missingID): should be nil (no error).
	if metas[2] != nil {
		t.Errorf("slot 2 (missingID): expected nil, got %+v", metas[2])
	}
}
