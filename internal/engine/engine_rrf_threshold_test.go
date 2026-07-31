package engine

import (
	"context"
	"fmt"
	"testing"

	"github.com/scrypster/muninndb/internal/auth"
	"github.com/scrypster/muninndb/internal/engine/activation"
	"github.com/scrypster/muninndb/internal/transport/mbp"
	"github.com/stretchr/testify/require"
)

// writeRRFThresholdFixture writes 20 engrams into vault: 10 "relevant" ones
// that strongly match the shared query phrase (via repeated terms, so FTS/BM25
// ranks them highest) and 10 "noise" ones sharing no terms with the query.
// Returns the IDs of the 10 relevant engrams.
func writeRRFThresholdFixture(t *testing.T, eng *Engine, ctx context.Context, vault string) []string {
	t.Helper()
	relevant := make([]string, 0, 10)
	for i := 0; i < 10; i++ {
		resp, err := eng.Write(ctx, &mbp.WriteRequest{
			Vault:   vault,
			Concept: fmt.Sprintf("thermal calibration widget procedure %d", i),
			Content: fmt.Sprintf("thermal calibration widget procedure step %d for device alignment; thermal calibration widget", i),
		})
		require.NoError(t, err)
		relevant = append(relevant, resp.ID)
	}
	for i := 0; i < 10; i++ {
		_, err := eng.Write(ctx, &mbp.WriteRequest{
			Vault:   vault,
			Concept: fmt.Sprintf("gardening notes %d", i),
			Content: fmt.Sprintf("unrelated gardening tips volume %d watering schedule notes", i),
		})
		require.NoError(t, err)
	}
	awaitFTS(t, eng)
	return relevant
}

// TestActivate_RRFDefaultThreshold_EndToEnd pins the R1 fix: default recall
// (Threshold left at its zero value, the shape every transport produces when
// the caller doesn't set one — REST, gRPC, MBP, and the embedded DB.Recall
// API all pass 0 through) against an rrf-fusion vault must actually surface
// results.
//
// BEFORE THE FIX: activateCore unconditionally coerced Threshold==0 to 0.1
// (a value calibrated to ACT-R's blended [0,1] scale) BEFORE
// activation.Run() ever saw the request. RRF finals are rank-based and
// typically land under ~0.05, so every result was filtered by the 0.1 floor
// -- default recall returned 0 of 20, on a vault full of relevant memories,
// with #590's mode-aware Run() default (rrf -> 0.001) unreachable dead code.
//
// The existing activation-layer regression test
// (TestRRF_ReturnsResultsWithDefaultThreshold, activation/activation_test.go)
// stayed green throughout, because it calls activation.Engine.Run() directly
// with Threshold: 0 -- a call shape production never produces (activateCore
// always coerces first). That asymmetry is exactly why this test is pinned
// at the ENGINE layer (Engine.Activate / activateCore), not the Run() layer.
//
// RED CHECK (documented, verified manually — see PR description): reverting
// engine.go's coerce to the old unconditional
//
//	if actReq.Threshold == 0 { actReq.Threshold = 0.1 }
//
// makes this test fail with recall@10 == 0.0, while
// TestRRF_ReturnsResultsWithDefaultThreshold keeps passing -- proving the
// old test could not have caught this bug.
func TestActivate_RRFDefaultThreshold_EndToEnd(t *testing.T) {
	eng, as, _, cleanup := testEnvWithAuth(t)
	defer cleanup()
	ctx := context.Background()

	const vault = "rrf-honesty"
	require.NoError(t, as.SetVaultConfig(auth.VaultConfig{
		Name:       vault,
		Public:     true,
		Plasticity: &auth.PlasticityConfig{ScoringFusion: ptr("rrf")},
	}))

	relevantIDs := writeRRFThresholdFixture(t, eng, ctx, vault)

	// Threshold left at its zero value -- the REST/gRPC/MBP/embedded shape.
	resp, err := eng.Activate(ctx, &mbp.ActivateRequest{
		Vault:      vault,
		Context:    []string{"thermal calibration widget"},
		Threshold:  0,
		MaxResults: 10,
	})
	require.NoError(t, err)

	relevantSet := make(map[string]struct{}, len(relevantIDs))
	for _, id := range relevantIDs {
		relevantSet[id] = struct{}{}
	}
	hit := 0
	for _, a := range resp.Activations {
		if _, ok := relevantSet[a.ID]; ok {
			hit++
		}
		require.GreaterOrEqualf(t, a.Score, float32(0.001),
			"every returned result must clear the genuine rrf floor, got %v for %s", a.Score, a.ID)
	}

	t.Logf("recall@10 = %d/10 relevant engrams surfaced (total returned: %d)", hit, len(resp.Activations))
	require.GreaterOrEqualf(t, hit, 9,
		"expected recall@10 >= 0.9 (>= 9 of 10 relevant engrams) under rrf default threshold, got %d/10 -- default recall under rrf is broken (the dead-#590-code bug)", hit)
}

// TestActivate_ACTRDefaultThreshold_Unaffected is the ACT-R control for the
// R1 fix: it pins that the majority-path (ACT-R / weighted_sum, i.e. any
// vault NOT configured for rrf fusion) default-threshold behavior is
// untouched by the mode-aware coerce. Threshold==0 must still coerce to the
// same 0.1 it always has -- proven here by asserting the zero-threshold call
// returns byte-identical IDs, in the same order, with the same scores, as an
// explicit Threshold: 0.1 call (which is what 0 always resolved to, before
// and after R1, on this path).
func TestActivate_ACTRDefaultThreshold_Unaffected(t *testing.T) {
	eng, as, _, cleanup := testEnvWithAuth(t)
	defer cleanup()
	ctx := context.Background()

	const vault = "actr-control"
	// No Plasticity override -- default resolves to ACT-R (ScoringFusion == "").
	require.NoError(t, as.SetVaultConfig(auth.VaultConfig{Name: vault, Public: true}))

	writeRRFThresholdFixture(t, eng, ctx, vault)

	// STEADY-STATE WARMUP. The WeightComplement(1.0) fix made full-weight
	// association edges readable for the first time — before it, the fixture's
	// declared weight-1.0 relationships round-tripped to 0, so recall-to-recall
	// transition boosts had nothing to act on and any two calls in a session
	// were accidentally identical. Post-fix, the FIRST recall's session learning
	// re-ranks the second (verified: run1 differs, run2==run3 — "strengthens
	// with use" working as promised). These tests assert THRESHOLD-RESOLUTION
	// equivalence, not learning behaviour, so they compare in the steady state
	// a warmup call establishes.
	if _, err := eng.Activate(ctx, &mbp.ActivateRequest{Vault: vault,
		Context: []string{"thermal calibration widget"}, Threshold: 0.1, MaxResults: 10}); err != nil {
		t.Fatalf("warmup: %v", err)
	}

	zeroResp, err := eng.Activate(ctx, &mbp.ActivateRequest{
		Vault:      vault,
		Context:    []string{"thermal calibration widget"},
		Threshold:  0,
		MaxResults: 10,
	})
	require.NoError(t, err)

	explicitResp, err := eng.Activate(ctx, &mbp.ActivateRequest{
		Vault:      vault,
		Context:    []string{"thermal calibration widget"},
		Threshold:  0.1,
		MaxResults: 10,
	})
	require.NoError(t, err)

	require.Equal(t, len(explicitResp.Activations), len(zeroResp.Activations),
		"ACT-R default (Threshold=0) must resolve to the same result count as explicit Threshold=0.1 -- R1 must not touch this path")
	for i := range zeroResp.Activations {
		require.Equal(t, explicitResp.Activations[i].ID, zeroResp.Activations[i].ID,
			"result order/identity at index %d must be unchanged for the ACT-R path", i)
		require.InDelta(t, explicitResp.Activations[i].Score, zeroResp.Activations[i].Score, 1e-9,
			"score at index %d must be unchanged for the ACT-R path", i)
	}
	require.NotEmpty(t, zeroResp.Activations, "sanity: the ACT-R control fixture must return results at all")
}

// TestApplyEntityBoost_SkippedUnderRRFFusion pins R1.4: the entity-boost
// pass is now an EXPLICIT no-op under rrf fusion (UseRRFFusion), preserving
// today's effective (previously accidental) behavior on purpose. Its
// factor/cap (entityBoostFactor=0.15, entityBoostCap=0.30) are calibrated to
// ACT-R's [0,1] scale; letting the pass run under rrf (whose finals are
// typically <0.05) would let an entity-linked-only engram, with zero content
// relevance to the query, outrank every genuine content match.
//
// The ACT-R leg (reqACTR below) is the positive control proving the fixture
// really does clear the injection threshold when the skip is absent -- i.e.
// the RRF leg's empty injection is because of the explicit skip, not because
// the evidence was too weak to qualify.
//
// RED CHECK (documented, verified manually — see PR description): removing
// the
//
//	if req.Weights != nil && req.Weights.UseRRFFusion { return results }
//
// guard from applyEntityBoost makes the RRF leg of this test fail (it starts
// injecting the target engram, same as the ACT-R leg) -- i.e. an injected
// engram outranks/enters alongside genuine content matches under RRF.
func TestApplyEntityBoost_SkippedUnderRRFFusion(t *testing.T) {
	t.Parallel()
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	const vault = "rrf-boost-skip"
	ws := eng.store.ResolveVaultPrefix(vault)

	// df=3 in a corpus of n=100 -- the exact rarity used by the existing
	// TestEntityBoost_SameEntityViaMultipleSeedsCountsOnce, known to clear a
	// 0.05 threshold via entity evidence alone (entityBoostFactor * idf(3,100)
	// ~= 0.114).
	upsertEntityTimes(t, eng, "RareEnt", 3)
	seed := writeLinkedEngram(t, eng, ws, "boost-skip-seed", "RareEnt")
	target := writeLinkedEngram(t, eng, ws, "boost-skip-target", "RareEnt")

	fullSeed, err := eng.store.GetEngram(ctx, ws, seed)
	require.NoError(t, err)
	seedResults := []activation.ScoredEngram{{Engram: fullSeed, Score: 0.8}}

	// RRF leg: the skip must fire, leaving results untouched.
	reqRRF := &activation.ActivateRequest{Threshold: 0.05, Weights: &activation.Weights{UseRRFFusion: true}}
	boostedRRF := eng.applyEntityBoost(ctx, ws, 100, seedResults, reqRRF)
	require.Len(t, boostedRRF, 1, "entity boost must be a no-op under rrf fusion -- no injection")
	require.Equal(t, seed, boostedRRF[0].Engram.ID)
	require.Zero(t, boostedRRF[0].Components.EntityBoost, "no boost may be applied to the seed under rrf fusion")

	// ACT-R (positive) control: identical evidence, no UseRRFFusion -- the
	// target must be injected, proving the RRF leg's silence above is the
	// explicit skip at work, not weak evidence.
	reqACTR := &activation.ActivateRequest{Threshold: 0.05}
	boostedACTR := eng.applyEntityBoost(ctx, ws, 100, seedResults, reqACTR)
	require.Len(t, boostedACTR, 2, "ACT-R control: entity evidence alone should inject the target")
	found := false
	for _, r := range boostedACTR {
		if r.Engram.ID == target {
			found = true
			require.Greater(t, r.Components.EntityBoost, 0.0)
		}
	}
	require.True(t, found, "ACT-R control: target must be injected via entity boost")
}
