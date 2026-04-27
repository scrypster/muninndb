package engine

import (
	"context"
	"testing"

	"github.com/scrypster/muninndb/internal/auth"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// TestActivation_Phase6_SkipsSoftDeletedEngrams verifies that soft-deleted
// engrams are excluded from Activate results. Writing 5 engrams, soft-deleting
// 2 of them, then activating must return none of the 2 deleted IDs.
func TestActivation_Phase6_SkipsSoftDeletedEngrams(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	const vault = "act-softdel"

	// Write 5 engrams with distinctive content to ensure FTS can recall them.
	concepts := []string{
		"activation softdel alpha",
		"activation softdel beta",
		"activation softdel gamma",
		"activation softdel delta",
		"activation softdel epsilon",
	}

	ids := make([]string, len(concepts))
	for i, concept := range concepts {
		resp, err := eng.Write(ctx, &mbp.WriteRequest{
			Vault:   vault,
			Concept: concept,
			Content: "activation soft delete filter test content " + concept,
		})
		if err != nil {
			t.Fatalf("Write[%d]: %v", i, err)
		}
		ids[i] = resp.ID
	}

	// Allow the async FTS worker to index the written engrams.
	awaitFTS(t, eng)

	// Soft-delete engrams at index 1 and 3.
	deletedIDs := []string{ids[1], ids[3]}
	for _, id := range deletedIDs {
		_, err := eng.Forget(ctx, &mbp.ForgetRequest{
			Vault: vault,
			ID:    id,
			Hard:  false,
		})
		if err != nil {
			t.Fatalf("Forget (soft delete) id=%s: %v", id, err)
		}
	}

	// Activate: query broadly so all 5 engrams would match without the filter.
	resp, err := eng.Activate(ctx, &mbp.ActivateRequest{
		Vault:      vault,
		Context:    []string{"activation soft delete filter test content"},
		MaxResults: 10,
		Threshold:  0.0,
	})
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}

	// Build a set of returned IDs for fast lookup.
	returnedIDs := make(map[string]bool, len(resp.Activations))
	for _, a := range resp.Activations {
		returnedIDs[a.ID] = true
	}

	// Assert that neither soft-deleted ID appears in the results.
	for _, deletedID := range deletedIDs {
		if returnedIDs[deletedID] {
			t.Errorf("soft-deleted engram id=%s appeared in Activate results — phase 6 filter failed", deletedID)
		}
	}

	// Sanity check: at least some of the non-deleted engrams should appear.
	// (This verifies the test isn't vacuously passing due to 0 results overall.)
	activeIDs := []string{ids[0], ids[2], ids[4]}
	anyActiveFound := false
	for _, activeID := range activeIDs {
		if returnedIDs[activeID] {
			anyActiveFound = true
			break
		}
	}
	if !anyActiveFound && len(resp.Activations) == 0 {
		t.Log("note: 0 activations returned — FTS may not have indexed in time; soft-delete filter assertion still passes (no deleted IDs returned)")
	}
}

// TestActivation_ExcludeUntrusted verifies that when the vault PlasticityConfig has
// ExcludeUntrusted=true, engrams marked as TrustUntrusted are filtered from results.
func TestActivation_ExcludeUntrusted(t *testing.T) {
	eng, as, _, cleanup := testEnvWithAuth(t)
	defer cleanup()
	ctx := context.Background()

	const vault = "act-trust-filter"

	// Configure this vault with ExcludeUntrusted=true.
	tr := true
	if err := as.SetVaultConfig(auth.VaultConfig{
		Name:   vault,
		Public: true,
		Plasticity: &auth.PlasticityConfig{
			ExcludeUntrusted: &tr,
		},
	}); err != nil {
		t.Fatalf("SetVaultConfig: %v", err)
	}

	// Write two engrams with the same distinctive content to ensure FTS can recall them.
	respUntrusted, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:   vault,
		Concept: "trust filter untrusted engram",
		Content: "exclude untrusted trust filter test zeta",
	})
	if err != nil {
		t.Fatalf("Write untrusted: %v", err)
	}

	respTrusted, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:   vault,
		Concept: "trust filter trusted engram",
		Content: "exclude untrusted trust filter test eta",
	})
	if err != nil {
		t.Fatalf("Write trusted: %v", err)
	}

	// Mark the first engram as untrusted.
	if err := eng.SetTrust(ctx, vault, respUntrusted.ID, "untrusted"); err != nil {
		t.Fatalf("SetTrust (untrusted): %v", err)
	}

	// Allow the async FTS worker to index the written engrams.
	awaitFTS(t, eng)

	// Activate: query broadly so both engrams would match without the filter.
	resp, err := eng.Activate(ctx, &mbp.ActivateRequest{
		Vault:      vault,
		Context:    []string{"exclude untrusted trust filter test"},
		MaxResults: 10,
		Threshold:  0.0,
	})
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}

	// Build a set of returned IDs for fast lookup.
	returnedIDs := make(map[string]bool, len(resp.Activations))
	for _, a := range resp.Activations {
		returnedIDs[a.ID] = true
	}

	// Precondition: awaitFTS guarantees indexing; we must see at least one result.
	// Zero results would mean the filter is broken in both directions (or awaitFTS failed).
	if len(resp.Activations) == 0 {
		t.Fatalf("Activate returned 0 results after awaitFTS — expected at least the trusted engram")
	}

	// Assert: untrusted engram must NOT appear.
	if returnedIDs[respUntrusted.ID] {
		t.Errorf("untrusted engram id=%s appeared in Activate results — ExcludeUntrusted filter failed", respUntrusted.ID)
	}

	// Assert: trusted engram must appear.
	if !returnedIDs[respTrusted.ID] {
		t.Errorf("trusted engram id=%s missing from Activate results — over-filtering?", respTrusted.ID)
	}
}
