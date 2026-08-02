package engine

import (
	"context"
	"testing"

	"time"

	"github.com/scrypster/muninndb/internal/auth"
	"github.com/scrypster/muninndb/internal/engine/vaultjob"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// TestClone_WeightedSumVault_FirstRecallNotSilentlyEmpty is the end-to-end
// regression for #810's severe symptom: on a `weighted_sum` vault, the FIRST
// recall after a clone returned ZERO results, silently.
//
// Mechanism: clone wrote uint64(time.Time{}.UnixNano()) into LastAccess, which
// decodes as 1754-08-30 — not IsZero(), so nothing caught it. computeComponents
// then saw daysSince ~= 740,000, giving recency=0 and decayFactor pinned at its
// 0.05 floor, scoring an otherwise-perfect row 0.42 against COG-6's 0.5
// weighted_sum default threshold.
//
// THE SHAPE OF THIS TEST IS THE TEST. Two properties are load-bearing and must
// not be "simplified":
//
//   - EXACTLY ONE recall against the clone. The second recall passes either way,
//     because the failed first recall itself populates the L1 cache and
//     computeComponents prefers the cache's lastAccessNs over eng.LastAccess.
//     That self-masking is precisely how this survived to production.
//   - A COLD cache for the target vault. The clone writes engram bytes straight
//     into Pebble, so the target workspace is naturally cold here — do not add a
//     read of the cloned vault before the assertion.
//
// Threshold is left at 0 so the engine's COG-6 fusion-aware coerce supplies the
// real production default (0.5 for weighted_sum). Hardcoding a low threshold
// here would make the test pass without the fix.
func TestClone_WeightedSumVault_FirstRecallNotSilentlyEmpty(t *testing.T) {
	eng, as, _, cleanup := testEnvWithAuth(t)
	defer cleanup()
	ctx := context.Background()

	const srcVault = "ws810-source"
	const dstVault = "ws810-clone"

	for _, name := range []string{srcVault, dstVault} {
		if err := as.SetVaultConfig(auth.VaultConfig{
			Name: name, Public: true,
			Plasticity: &auth.PlasticityConfig{ScoringFusion: ptr("weighted_sum")},
		}); err != nil {
			t.Fatalf("SetVaultConfig(%s): %v", name, err)
		}
	}

	concepts := []string{
		"heron migration routes",
		"heron nesting season",
		"heron feeding behaviour",
	}
	for _, c := range concepts {
		if _, err := eng.Write(ctx, &mbp.WriteRequest{
			Vault:   srcVault,
			Concept: c,
			Content: "field notes on " + c,
			Tags:    []string{"birds"},
		}); err != nil {
			t.Fatalf("Write %q: %v", c, err)
		}
	}
	awaitFTS(t, eng)

	job, err := eng.StartClone(ctx, srcVault, dstVault)
	if err != nil {
		t.Fatalf("StartClone: %v", err)
	}
	finalJob := waitForJob(t, eng, job.ID, 30*time.Second)
	if finalJob.GetStatus() != vaultjob.StatusDone {
		t.Fatalf("clone job status = %q (err: %s)", finalJob.GetStatus(), finalJob.GetErr())
	}
	awaitFTS(t, eng)

	// ---- THE recall: first, only, cold cache, production default threshold ----
	resp, err := eng.Activate(ctx, &mbp.ActivateRequest{
		Vault:      dstVault,
		Context:    []string{"heron"},
		MaxResults: 10,
		// Threshold intentionally 0 => engine applies the weighted_sum default (0.5).
	})
	if err != nil {
		t.Fatalf("Activate on cloned weighted_sum vault: %v", err)
	}
	if len(resp.Activations) == 0 {
		t.Fatalf("first recall on a freshly cloned weighted_sum vault returned ZERO results — the #810 silently-empty recall")
	}

	// Control: the source vault, same fusion mode, same query, must also match.
	// If this is empty the fixture is wrong, not the clone.
	srcResp, err := eng.Activate(ctx, &mbp.ActivateRequest{
		Vault:      srcVault,
		Context:    []string{"heron"},
		MaxResults: 10,
	})
	if err != nil {
		t.Fatalf("Activate on source vault: %v", err)
	}
	if len(srcResp.Activations) == 0 {
		t.Fatal("control: source weighted_sum vault returned zero results — fixture does not exercise the path")
	}
	if len(resp.Activations) != len(srcResp.Activations) {
		t.Errorf("clone returned %d activations, source returned %d — a clone must be recall-equivalent to its source",
			len(resp.Activations), len(srcResp.Activations))
	}
}
