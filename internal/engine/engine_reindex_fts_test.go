package engine

import (
	"context"
	"testing"

	"github.com/scrypster/muninndb/internal/storage"
)

// TestReindexFTSVault_SetsVersionMarker verifies that ReindexFTSVault writes
// the FTS version marker (0x01) to the FTSVersionKey for the vault.
func TestReindexFTSVault_SetsVersionMarker(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	const vaultName = "reindextest"
	store := eng.Store()
	ws := store.VaultPrefix(vaultName)

	// Register the vault name so ReindexFTSVault can find it in ListVaultNames.
	if err := store.WriteVaultName(ws, vaultName); err != nil {
		t.Fatalf("WriteVaultName: %v", err)
	}

	// Write 3 engrams to the vault.
	engrams := []struct {
		concept string
		content string
	}{
		{"running dogs", "they are running fast"},
		{"flying birds", "birds fly high in the sky"},
		{"swimming fish", "fish swim deep in the ocean"},
	}
	for i, e := range engrams {
		engram := &storage.Engram{
			Concept: e.concept,
			Content: e.content,
		}
		if _, err := store.WriteEngram(ctx, ws, engram); err != nil {
			t.Fatalf("WriteEngram[%d]: %v", i, err)
		}
	}

	// Run ReindexFTSVault.
	count, err := eng.ReindexFTSVault(ctx, vaultName)
	if err != nil {
		t.Fatalf("ReindexFTSVault: %v", err)
	}

	// Returned count must equal the number of written engrams.
	if count != 3 {
		t.Errorf("ReindexFTSVault returned %d, want 3", count)
	}

	// Verify the version marker was set to 0x01.
	ver, ok, err := store.FTSVersionMarker(ws)
	if err != nil {
		t.Fatalf("FTSVersionMarker: %v", err)
	}
	if !ok {
		t.Fatal("FTSVersionMarker not set after ReindexFTSVault")
	}
	if ver != 0x01 {
		t.Errorf("FTSVersionMarker = 0x%02X, want 0x01", ver)
	}
}

// TestReindexFTSVault_SearchWorksAfter verifies that after ReindexFTSVault,
// FTS search returns results for stemmed queries against re-indexed engrams.
func TestReindexFTSVault_SearchWorksAfter(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	const vaultName = "reindexsearchtest"
	store := eng.Store()
	ws := store.VaultPrefix(vaultName)

	if err := store.WriteVaultName(ws, vaultName); err != nil {
		t.Fatalf("WriteVaultName: %v", err)
	}

	// Write one engram with well-known content.
	engram := &storage.Engram{
		Concept: "running dogs",
		Content: "they are running fast",
	}
	if _, err := store.WriteEngram(ctx, ws, engram); err != nil {
		t.Fatalf("WriteEngram: %v", err)
	}

	// Allow the async FTS worker to process any pending jobs before reindex.
	awaitFTS(t, eng)

	count, err := eng.ReindexFTSVault(ctx, vaultName)
	if err != nil {
		t.Fatalf("ReindexFTSVault: %v", err)
	}
	if count != 1 {
		t.Errorf("ReindexFTSVault returned count %d, want 1", count)
	}

	// Use the engine's own FTS index to search — same index used during
	// ReindexFTSVault, so results are immediately visible.
	ftsIdx := eng.fts

	// Search for "run" — the Porter2 stem of "running".
	results, err := ftsIdx.Search(ctx, ws, "run", 5)
	if err != nil {
		t.Fatalf("FTS Search: %v", err)
	}
	if len(results) == 0 {
		t.Error("expected FTS search for 'run' to return the re-indexed engram, got 0 results")
	}
}
