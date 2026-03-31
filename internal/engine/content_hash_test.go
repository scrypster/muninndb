package engine

import (
	"context"
	"testing"

	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// TestWriteDuplicateContentReturnsExistingID verifies that writing the same
// content twice returns the original engram ID with a duplicate_content hint.
func TestWriteDuplicateContentReturnsExistingID(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()

	ctx := context.Background()

	// First write — should succeed normally.
	resp1, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:   "default",
		Content: "the quick brown fox jumps over the lazy dog",
		Concept: "test-concept",
	})
	if err != nil {
		t.Fatalf("first write: %v", err)
	}
	if resp1.Hint != "" {
		t.Errorf("first write should have no hint, got %q", resp1.Hint)
	}

	// Second write with identical content — should return original ID + hint.
	resp2, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:   "default",
		Content: "the quick brown fox jumps over the lazy dog",
		Concept: "different-concept",
	})
	if err != nil {
		t.Fatalf("second write: %v", err)
	}
	if resp2.ID != resp1.ID {
		t.Errorf("duplicate content should return original ID %q, got %q", resp1.ID, resp2.ID)
	}
	if resp2.Hint != "duplicate_content" {
		t.Errorf("duplicate content should have hint 'duplicate_content', got %q", resp2.Hint)
	}
}

// TestWriteDifferentContentCreatesNewEngram verifies that different content
// produces distinct engrams (no false dedup).
func TestWriteDifferentContentCreatesNewEngram(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()

	ctx := context.Background()

	resp1, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:   "default",
		Content: "content alpha",
	})
	if err != nil {
		t.Fatalf("first write: %v", err)
	}

	resp2, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:   "default",
		Content: "content beta",
	})
	if err != nil {
		t.Fatalf("second write: %v", err)
	}
	if resp2.ID == resp1.ID {
		t.Error("different content should produce different engram IDs")
	}
	if resp2.Hint == "duplicate_content" {
		t.Error("different content should not trigger duplicate_content hint")
	}
}

// TestWriteDuplicateContentAfterSoftDeleteAllowsRecreation verifies that
// soft-deleted content can be re-stored (the hash slot is cleared).
func TestWriteDuplicateContentAfterSoftDeleteAllowsRecreation(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()

	ctx := context.Background()
	vault := "default"
	content := "ephemeral content that will be deleted"

	// Write original.
	resp1, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:   vault,
		Content: content,
	})
	if err != nil {
		t.Fatalf("first write: %v", err)
	}

	// Soft-delete the engram.
	wsPrefix := eng.store.ResolveVaultPrefix(vault)
	id1, err := storage.ParseULID(resp1.ID)
	if err != nil {
		t.Fatalf("parse ULID: %v", err)
	}
	if err := eng.store.SoftDelete(ctx, wsPrefix, id1); err != nil {
		t.Fatalf("soft delete: %v", err)
	}

	// Re-write same content — should succeed with a NEW engram ID.
	resp2, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:   vault,
		Content: content,
	})
	if err != nil {
		t.Fatalf("re-write after soft-delete: %v", err)
	}
	if resp2.ID == resp1.ID {
		t.Error("re-write after soft-delete should create a new engram, got same ID")
	}
	if resp2.Hint == "duplicate_content" {
		t.Error("re-write after soft-delete should not have duplicate_content hint")
	}
}

// TestWriteDuplicateContentCrossVaultNoDedup verifies that the same content
// in different vaults does not trigger dedup (hash is per-vault).
func TestWriteDuplicateContentCrossVaultNoDedup(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()

	ctx := context.Background()
	content := "shared content across vaults"

	resp1, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:   "vault-a",
		Content: content,
	})
	if err != nil {
		t.Fatalf("write to vault-a: %v", err)
	}

	resp2, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:   "vault-b",
		Content: content,
	})
	if err != nil {
		t.Fatalf("write to vault-b: %v", err)
	}
	if resp2.ID == resp1.ID {
		t.Error("same content in different vaults should produce different engram IDs")
	}
	if resp2.Hint == "duplicate_content" {
		t.Error("same content in different vaults should not trigger duplicate_content hint")
	}
}
