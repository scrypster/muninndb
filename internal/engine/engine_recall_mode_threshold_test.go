package engine

import (
	"context"
	"testing"

	"github.com/scrypster/muninndb/internal/auth"
	"github.com/scrypster/muninndb/internal/transport/mbp"
	"github.com/stretchr/testify/require"
)

// These tests pin the #704 fix: recall-mode presets were a SECOND threshold
// decider (after the COG-6 default coerce) that still applied ACT-R-calibrated
// values (semantic 0.3, recent 0.2, deep 0.1) to rrf-scored requests, silently
// emptying rrf vaults that carry a recall mode — the exact bug class R1 (#705)
// killed on the no-mode default path. The fix makes the engine the single
// preset-threshold decider, keyed on the effective scoring mode: under rrf
// fusion the preset threshold ABSTAINS and the mode-aware default stands
// (Threshold 0 → activation.Run()'s rrf floor, 0.001). Non-threshold preset
// fields (hops, weights) are scale-free and unchanged.

// TestActivate_RRFVaultDefaultRecallMode_NotSilentlyEmpty pins the
// vault-default half of #704: an rrf vault whose config sets
// recall_mode: "deep" must not have deep's ACT-R-calibrated 0.1 threshold
// clobber the leave-0-for-rrf decision.
//
// RED CHECK: without the fix (the vault-default preset block applying
// preset.Threshold regardless of scoring mode), this fails with 0 results —
// every rrf final (~<0.05) is filtered by deep's 0.1 floor.
func TestActivate_RRFVaultDefaultRecallMode_NotSilentlyEmpty(t *testing.T) {
	eng, as, _, cleanup := testEnvWithAuth(t)
	defer cleanup()
	ctx := context.Background()

	const vault = "rrf-mode-vault-default"
	require.NoError(t, as.SetVaultConfig(auth.VaultConfig{
		Name:   vault,
		Public: true,
		Plasticity: &auth.PlasticityConfig{
			ScoringFusion: ptr("rrf"),
			RecallMode:    ptr("deep"),
		},
	}))

	relevantIDs := writeRRFThresholdFixture(t, eng, ctx, vault)

	// No explicit mode, no explicit threshold — the vault default decides.
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
	}
	t.Logf("recall@10 = %d/10 relevant engrams surfaced (total returned: %d)", hit, len(resp.Activations))
	require.GreaterOrEqualf(t, hit, 9,
		"rrf vault with vault-default recall_mode=deep must not be silently empty (#704): got %d/10", hit)
}

// TestActivate_RRFExplicitWireMode_ThresholdAbstains pins the wire-mode half
// of #704 at the engine layer: a request carrying Mode explicitly (the shape
// transports produce once they forward the mode instead of stamping the
// preset threshold) must not get the preset's ACT-R-scale threshold under
// rrf. Guards the fixed engine against regressing into applying preset
// thresholds blindly.
func TestActivate_RRFExplicitWireMode_ThresholdAbstains(t *testing.T) {
	eng, as, _, cleanup := testEnvWithAuth(t)
	defer cleanup()
	ctx := context.Background()

	const vault = "rrf-mode-explicit"
	require.NoError(t, as.SetVaultConfig(auth.VaultConfig{
		Name:       vault,
		Public:     true,
		Plasticity: &auth.PlasticityConfig{ScoringFusion: ptr("rrf")},
	}))

	relevantIDs := writeRRFThresholdFixture(t, eng, ctx, vault)

	// semantic carries the highest preset threshold (0.3) — the worst case.
	resp, err := eng.Activate(ctx, &mbp.ActivateRequest{
		Vault:      vault,
		Context:    []string{"thermal calibration widget"},
		Mode:       "semantic",
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
	}
	t.Logf("recall@10 = %d/10 relevant engrams surfaced (total returned: %d)", hit, len(resp.Activations))
	require.GreaterOrEqualf(t, hit, 9,
		"rrf vault with explicit wire mode=semantic must not be silently empty (#704): got %d/10", hit)
}

// TestActivate_ACTRVaultDefaultRecallMode_PresetThresholdUnchanged is the
// ACT-R control: on a non-rrf vault, the vault-default preset threshold must
// keep applying exactly as before the #704 fix — pinned by asserting the
// zero-threshold call is byte-identical (IDs, order, scores) to an explicit
// call at the preset's threshold. Changing preset thresholds shifts result
// sets for existing users; this proves the majority path did not move.
func TestActivate_ACTRVaultDefaultRecallMode_PresetThresholdUnchanged(t *testing.T) {
	eng, as, _, cleanup := testEnvWithAuth(t)
	defer cleanup()
	ctx := context.Background()

	const vault = "actr-mode-control"
	require.NoError(t, as.SetVaultConfig(auth.VaultConfig{
		Name:   vault,
		Public: true,
		Plasticity: &auth.PlasticityConfig{
			// ScoringFusion unset — default ACT-R.
			RecallMode: ptr("deep"),
		},
	}))

	writeRRFThresholdFixture(t, eng, ctx, vault)

	zeroResp, err := eng.Activate(ctx, &mbp.ActivateRequest{
		Vault:      vault,
		Context:    []string{"thermal calibration widget"},
		Threshold:  0,
		MaxResults: 10,
	})
	require.NoError(t, err)

	// deep = Threshold 0.1, MaxHops 4 — the explicit equivalent.
	explicitResp, err := eng.Activate(ctx, &mbp.ActivateRequest{
		Vault:      vault,
		Context:    []string{"thermal calibration widget"},
		Threshold:  0.1,
		MaxHops:    4,
		MaxResults: 10,
	})
	require.NoError(t, err)

	require.Equal(t, len(explicitResp.Activations), len(zeroResp.Activations),
		"ACT-R vault-default mode must resolve to the preset's explicit equivalent — #704 must not touch non-rrf vaults")
	for i := range zeroResp.Activations {
		require.Equal(t, explicitResp.Activations[i].ID, zeroResp.Activations[i].ID,
			"result order/identity at index %d must be unchanged for the ACT-R path", i)
		require.InDelta(t, explicitResp.Activations[i].Score, zeroResp.Activations[i].Score, 1e-9,
			"score at index %d must be unchanged for the ACT-R path", i)
	}
	require.NotEmpty(t, zeroResp.Activations, "sanity: the ACT-R control fixture must return results at all")
}

// TestActivate_ACTRExplicitWireMode_AppliesPreset pins the engine-side preset
// application for explicit wire modes on non-rrf vaults: Mode="deep" must
// resolve byte-identically to its explicit equivalent (Threshold 0.1,
// MaxHops 4). Before the fix the engine ignored req.Mode entirely —
// transports stamped presets client-side and raw MBP/gRPC callers' Mode was
// silently dropped. deep is used because it carries no weight fields, so the
// explicit-equivalent request resolves identical weights and the comparison
// is exact; the rrf-abstention of weight-carrying modes is pinned by
// TestActivate_RRFExplicitWireMode_ThresholdAbstains above, and the
// transports-no-longer-stamp half by the MCP/REST wire tests.
func TestActivate_ACTRExplicitWireMode_AppliesPreset(t *testing.T) {
	eng, as, _, cleanup := testEnvWithAuth(t)
	defer cleanup()
	ctx := context.Background()

	const vault = "actr-mode-explicit"
	require.NoError(t, as.SetVaultConfig(auth.VaultConfig{Name: vault, Public: true}))

	writeRRFThresholdFixture(t, eng, ctx, vault)

	modeResp, err := eng.Activate(ctx, &mbp.ActivateRequest{
		Vault:      vault,
		Context:    []string{"thermal calibration widget"},
		Mode:       "deep",
		Threshold:  0,
		MaxResults: 10,
	})
	require.NoError(t, err)

	explicitResp, err := eng.Activate(ctx, &mbp.ActivateRequest{
		Vault:      vault,
		Context:    []string{"thermal calibration widget"},
		Threshold:  0.1,
		MaxHops:    4,
		MaxResults: 10,
	})
	require.NoError(t, err)

	require.Equal(t, len(explicitResp.Activations), len(modeResp.Activations),
		"explicit wire Mode=deep must resolve to deep's explicit equivalent")
	for i := range modeResp.Activations {
		require.Equal(t, explicitResp.Activations[i].ID, modeResp.Activations[i].ID,
			"result order/identity at index %d must match the explicit-equivalent call", i)
		require.InDelta(t, explicitResp.Activations[i].Score, modeResp.Activations[i].Score, 1e-9,
			"score at index %d must match the explicit-equivalent call", i)
	}
	require.NotEmpty(t, modeResp.Activations, "sanity: the fixture must return results at all")
}

// TestActivate_ACTRExplicitSemanticMode_MatchesStampedEquivalent pins the
// weight semantics of explicit weight-carrying modes (the Defect-1 class from
// this increment's adversarial review): Mode="semantic" with no caller
// weights must resolve byte-identically to the stamped-request shape
// transports used to produce — Weights{0.8, 0.2, DisableACTR} from the ZERO
// base, threshold 0.3. Overlaying the preset onto resolved default weights
// instead lets a fresh, recently-active engram score ~0.7 under legacy
// scoring (0.4·decay + 0.3·recency + 0.5·hebbian terms) with zero content
// match — above semantic's own threshold, inverting the mode's precision
// contract.
//
// RED CHECK: with the zero-base branch disabled (preset overlaid on resolved
// defaults), the mode call returns recency-polluted results the explicit
// call does not — the fixture's 20 just-written engrams all carry
// decayFactor≈1 and high recency.
func TestActivate_ACTRExplicitSemanticMode_MatchesStampedEquivalent(t *testing.T) {
	eng, as, _, cleanup := testEnvWithAuth(t)
	defer cleanup()
	ctx := context.Background()

	const vault = "actr-semantic-zerobase"
	require.NoError(t, as.SetVaultConfig(auth.VaultConfig{Name: vault, Public: true}))

	writeRRFThresholdFixture(t, eng, ctx, vault)

	modeResp, err := eng.Activate(ctx, &mbp.ActivateRequest{
		Vault:      vault,
		Context:    []string{"thermal calibration widget"},
		Mode:       "semantic",
		Threshold:  0,
		MaxResults: 20,
	})
	require.NoError(t, err)

	explicitResp, err := eng.Activate(ctx, &mbp.ActivateRequest{
		Vault:      vault,
		Context:    []string{"thermal calibration widget"},
		Threshold:  0.3,
		MaxResults: 20,
		Weights: &mbp.Weights{
			SemanticSimilarity: 0.8,
			FullTextRelevance:  0.2,
			DisableACTR:        true,
		},
	})
	require.NoError(t, err)

	require.Equal(t, len(explicitResp.Activations), len(modeResp.Activations),
		"Mode=semantic must resolve to its stamped explicit equivalent — resolved-default weight pollution changes the result set")
	for i := range modeResp.Activations {
		require.Equal(t, explicitResp.Activations[i].ID, modeResp.Activations[i].ID,
			"result order/identity at index %d must match the stamped-equivalent call", i)
		require.InDelta(t, explicitResp.Activations[i].Score, modeResp.Activations[i].Score, 1e-9,
			"score at index %d must match the stamped-equivalent call", i)
	}
}
