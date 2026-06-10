package engine

import (
	"context"
	"testing"
)

// TestReindexFTSVault_RenamedVault is a regression test for issue #480, the
// sibling of #454: ReindexFTSVault derived the workspace prefix via raw
// store.VaultPrefix(name) (SipHash of the *current* name) instead of
// store.ResolveVaultPrefix(name) (the 0x0F index lookup). For a vault renamed
// via RenameVault, ws != siphash(currentName) — the rename only flips the
// index keys; the engram data stays at the original workspace. So a reindex of
// a renamed vault scanned an empty prefix and reported success while
// reindexing nothing.
func TestReindexFTSVault_RenamedVault(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	// Two engrams in the source vault.
	if _, err := eng.Write(ctx, writeReq("reindex-src", "photosynthesis process", "plants convert light to energy")); err != nil {
		t.Fatal(err)
	}
	if _, err := eng.Write(ctx, writeReq("reindex-src", "cellular respiration", "cells release energy from glucose")); err != nil {
		t.Fatal(err)
	}
	awaitFTS(t, eng)

	if err := eng.RenameVault(ctx, "reindex-src", "reindex-dst"); err != nil {
		t.Fatalf("RenameVault: %v", err)
	}

	// Reindex under the new name must process the vault's engrams, not silently
	// scan an empty prefix.
	count, err := eng.ReindexFTSVault(ctx, "reindex-dst")
	if err != nil {
		t.Fatalf("ReindexFTSVault(reindex-dst): %v", err)
	}
	if count != 2 {
		t.Fatalf("reindexed %d engrams, want 2 — a renamed vault must reindex at its real workspace", count)
	}

	// And the rebuilt index must still answer queries under the new name.
	resp, err := eng.Activate(ctx, activateReq("reindex-dst", "photosynthesis"))
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Activations) == 0 {
		t.Error("expected activations under the renamed vault after reindex, got 0")
	}
}
