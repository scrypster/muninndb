package engine

import (
	"context"
	"testing"

	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// TestSoftDelete_FTSCleanup verifies that a soft-deleted engram does not appear
// in FTS search results after deletion.
func TestSoftDelete_FTSCleanup(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	// 1. Write an engram with distinctive content.
	writeResp, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:   "test",
		Concept: "purple elephant database",
		Content: "The purple elephant is a unique identifier for this test engram.",
		Tags:    []string{"purple", "elephant"},
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	// Allow async FTS worker to index the written engram.
	awaitFTS(t, eng)

	// 2. Search FTS for "purple elephant" — should find it before deletion.
	respBefore, err := eng.Activate(ctx, &mbp.ActivateRequest{
		Vault:      "test",
		Context:    []string{"purple elephant"},
		MaxResults: 10,
		Threshold:  0.01,
	})
	if err != nil {
		t.Fatalf("Activate (before soft delete): %v", err)
	}
	if len(respBefore.Activations) == 0 {
		t.Fatal("expected Activate to find engram before soft delete, got 0 results")
	}

	// 3. Soft-delete the engram.
	_, err = eng.Forget(ctx, &mbp.ForgetRequest{
		Vault: "test",
		ID:    writeResp.ID,
		Hard:  false,
	})
	if err != nil {
		t.Fatalf("Forget (soft delete): %v", err)
	}

	// 4. Search FTS for "purple elephant" again — should return 0 results.
	respAfter, err := eng.Activate(ctx, &mbp.ActivateRequest{
		Vault:      "test",
		Context:    []string{"purple elephant"},
		MaxResults: 10,
		Threshold:  0.01,
	})
	if err != nil {
		t.Fatalf("Activate (after soft delete): %v", err)
	}
	if len(respAfter.Activations) > 0 {
		t.Errorf("expected 0 results after soft delete, got %d; top concept = %q",
			len(respAfter.Activations), respAfter.Activations[0].Concept)
	}
}
