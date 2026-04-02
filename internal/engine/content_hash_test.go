package engine

import (
	"context"
	"testing"
	"time"

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

// TestHardDeleteThenRewriteSameContent verifies that hard-deleting an engram
// frees its content-hash slot so the same content can be stored again.
func TestHardDeleteThenRewriteSameContent(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()

	ctx := context.Background()
	vault := "default"
	content := "content that will be hard-deleted"

	// Write original.
	resp1, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:   vault,
		Content: content,
		Concept: "original",
	})
	if err != nil {
		t.Fatalf("first write: %v", err)
	}

	// Hard-delete the engram.
	_, err = eng.Forget(ctx, &mbp.ForgetRequest{
		ID:    resp1.ID,
		Hard:  true,
		Vault: vault,
	})
	if err != nil {
		t.Fatalf("hard delete: %v", err)
	}

	// Re-write same content — should succeed with a NEW engram ID (hash slot freed).
	resp2, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:   vault,
		Content: content,
		Concept: "recreated",
	})
	if err != nil {
		t.Fatalf("re-write after hard-delete: %v", err)
	}
	if resp2.ID == resp1.ID {
		t.Error("re-write after hard-delete should create a new engram, got same ID")
	}
	if resp2.Hint == "duplicate_content" {
		t.Error("re-write after hard-delete should not have duplicate_content hint")
	}
}

// TestEvolveThenWriteOldContentNoDup verifies that evolving engram A→B frees
// the content-hash slot for A's content, so writing A again creates a new engram.
func TestEvolveThenWriteOldContentNoDup(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()

	ctx := context.Background()
	vault := "default"
	contentA := "original content alpha"
	contentB := "evolved content beta"

	// Write content A.
	resp1, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:   vault,
		Content: contentA,
		Concept: "concept-A",
	})
	if err != nil {
		t.Fatalf("write A: %v", err)
	}

	// Evolve A → B (soft-deletes A, creates B).
	_, err = eng.Evolve(ctx, vault, resp1.ID, contentB, "update", nil)
	if err != nil {
		t.Fatalf("evolve: %v", err)
	}

	// Write content A again — should NOT dedup since A was evolved away.
	resp3, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:   vault,
		Content: contentA,
		Concept: "concept-A-v2",
	})
	if err != nil {
		t.Fatalf("re-write A: %v", err)
	}
	if resp3.ID == resp1.ID {
		t.Error("re-write of evolved-away content should create a new engram, got original ID")
	}
	if resp3.Hint == "duplicate_content" {
		t.Error("re-write of evolved-away content should not trigger duplicate_content hint")
	}
}

// TestWriteClearVaultThenRewrite verifies that writing content, clearing the vault,
// and re-writing the same content succeeds (no stale hash blocks the re-write).
func TestWriteClearVaultThenRewrite(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()

	ctx := context.Background()
	vault := "clear-hash-test"
	content := "content that will survive vault clear"

	// Write original.
	resp1, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:   vault,
		Content: content,
		Concept: "original",
	})
	if err != nil {
		t.Fatalf("first write: %v", err)
	}
	if resp1.Hint != "" {
		t.Errorf("first write should have no hint, got %q", resp1.Hint)
	}

	// Clear the vault — must remove 0x28 content-hash keys.
	if err := eng.ClearVault(ctx, vault); err != nil {
		t.Fatalf("ClearVault: %v", err)
	}

	// Re-write same content — should succeed with a NEW engram ID (hash was cleared).
	resp2, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:   vault,
		Content: content,
		Concept: "recreated",
	})
	if err != nil {
		t.Fatalf("re-write after clear: %v", err)
	}
	if resp2.ID == resp1.ID {
		t.Error("re-write after ClearVault should create a new engram, got same ID")
	}
	if resp2.Hint == "duplicate_content" {
		t.Error("re-write after ClearVault should not have duplicate_content hint")
	}
}

// TestRestoreThenRewriteIsDuplicate verifies that restoring a soft-deleted engram
// re-adds its content-hash mapping, so a subsequent write of the same content
// returns the restored engram as a duplicate.
func TestRestoreThenRewriteIsDuplicate(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()

	ctx := context.Background()
	vault := "restore-hash-test"
	content := "content to restore and re-check"

	// Write original.
	resp1, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:   vault,
		Content: content,
		Concept: "original",
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

	// Verify the hash slot is now stale — writing same content should create new engram.
	resp2, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:   vault,
		Content: content,
		Concept: "after-soft-delete",
	})
	if err != nil {
		t.Fatalf("write after soft-delete: %v", err)
	}
	if resp2.ID == resp1.ID {
		t.Error("write after soft-delete should create a new engram, got original ID")
	}

	// Soft-delete the second engram too.
	id2, err := storage.ParseULID(resp2.ID)
	if err != nil {
		t.Fatalf("parse ULID: %v", err)
	}
	if err := eng.store.SoftDelete(ctx, wsPrefix, id2); err != nil {
		t.Fatalf("soft delete second: %v", err)
	}

	// Restore the second engram — this should re-add the content-hash mapping.
	_, err = eng.Restore(ctx, vault, resp2.ID)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}

	// Now writing the same content should detect the restored engram as a duplicate.
	resp3, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:   vault,
		Content: content,
		Concept: "after-restore",
	})
	if err != nil {
		t.Fatalf("write after restore: %v", err)
	}
	if resp3.ID != resp2.ID {
		t.Errorf("write after restore should return restored engram ID %q, got %q", resp2.ID, resp3.ID)
	}
	if resp3.Hint != "duplicate_content" {
		t.Errorf("write after restore should have hint 'duplicate_content', got %q", resp3.Hint)
	}
}

// TestDuplicateWriteReinforcesAccessCount verifies that writing duplicate content
// increments the existing engram's access count and updates LastAccess.
func TestDuplicateWriteReinforcesAccessCount(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()

	ctx := context.Background()
	vault := "reinforce-test"
	content := "content to be reinforced"

	// Write original.
	resp1, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:   vault,
		Content: content,
		Concept: "original",
	})
	if err != nil {
		t.Fatalf("first write: %v", err)
	}

	// Read the engram to get its initial access count.
	wsPrefix := eng.store.ResolveVaultPrefix(vault)
	id1, err := storage.ParseULID(resp1.ID)
	if err != nil {
		t.Fatalf("parse ULID: %v", err)
	}
	engBefore, err := eng.store.GetEngram(ctx, wsPrefix, id1)
	if err != nil {
		t.Fatalf("get engram before: %v", err)
	}
	accessBefore := engBefore.AccessCount
	lastAccessBefore := engBefore.LastAccess

	// Small sleep to ensure LastAccess will differ.
	time.Sleep(5 * time.Millisecond)

	// Write duplicate — should reinforce.
	resp2, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:   vault,
		Content: content,
		Concept: "duplicate",
	})
	if err != nil {
		t.Fatalf("duplicate write: %v", err)
	}
	if resp2.Hint != "duplicate_content" {
		t.Fatalf("expected duplicate_content hint, got %q", resp2.Hint)
	}

	// Read the engram again — access count should be incremented.
	engAfter, err := eng.store.GetEngram(ctx, wsPrefix, id1)
	if err != nil {
		t.Fatalf("get engram after: %v", err)
	}
	if engAfter.AccessCount != accessBefore+1 {
		t.Errorf("access count should be %d, got %d", accessBefore+1, engAfter.AccessCount)
	}
	if !engAfter.LastAccess.After(lastAccessBefore) {
		t.Error("LastAccess should be updated after duplicate write")
	}
}

// TestWriteBatchDedup verifies that WriteBatch detects duplicate content
// against existing engrams and returns the existing ID with a hint.
func TestWriteBatchDedup(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()

	ctx := context.Background()
	vault := "default"
	content := "batch dedup test content"

	// Write original via single Write.
	resp1, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:   vault,
		Content: content,
		Concept: "original",
	})
	if err != nil {
		t.Fatalf("single write: %v", err)
	}

	// WriteBatch with one duplicate and one unique item.
	reqs := []*mbp.WriteRequest{
		{Vault: vault, Content: content, Concept: "dup-in-batch"},
		{Vault: vault, Content: "unique batch content", Concept: "unique"},
	}
	responses, errs := eng.WriteBatch(ctx, reqs)

	// Duplicate item should return existing ID with hint.
	if errs[0] != nil {
		t.Fatalf("batch item 0 error: %v", errs[0])
	}
	if responses[0] == nil {
		t.Fatal("batch item 0 response is nil")
	}
	if responses[0].ID != resp1.ID {
		t.Errorf("batch dup should return original ID %q, got %q", resp1.ID, responses[0].ID)
	}
	if responses[0].Hint != "duplicate_content" {
		t.Errorf("batch dup should have hint 'duplicate_content', got %q", responses[0].Hint)
	}

	// Unique item should succeed normally.
	if errs[1] != nil {
		t.Fatalf("batch item 1 error: %v", errs[1])
	}
	if responses[1] == nil {
		t.Fatal("batch item 1 response is nil")
	}
	if responses[1].ID == resp1.ID {
		t.Error("unique batch item should have a different ID from the original")
	}
	if responses[1].Hint == "duplicate_content" {
		t.Error("unique batch item should not have duplicate_content hint")
	}
}
