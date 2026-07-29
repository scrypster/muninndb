package engine

import (
	"testing"

	"github.com/scrypster/muninndb/internal/auth"
	"github.com/scrypster/muninndb/internal/engine/activation"
	"github.com/scrypster/muninndb/internal/transport/mbp"
	"github.com/stretchr/testify/require"
)

func presetFor(t *testing.T, name string) auth.RecallModePreset {
	t.Helper()
	p, err := auth.LookupRecallMode(name)
	require.NoError(t, err)
	return p
}

func TestApplyRecallModePreset_ThresholdAbstainsUnderRRF(t *testing.T) {
	actReq := &activation.ActivateRequest{Weights: &activation.Weights{UseRRFFusion: true}}
	applyRecallModePreset(actReq, &mbp.ActivateRequest{}, presetFor(t, "deep"))
	require.Zero(t, actReq.Threshold,
		"preset threshold must abstain under rrf fusion (#704) — 0 lets Run() apply its rrf floor")
}

func TestApplyRecallModePreset_ThresholdAppliesOnACTR(t *testing.T) {
	// Simulates the post-coerce state: COG-6 already coerced 0 -> 0.1; the
	// preset overrides it because the CALLER left the threshold unset.
	actReq := &activation.ActivateRequest{Threshold: 0.1, Weights: &activation.Weights{}}
	applyRecallModePreset(actReq, &mbp.ActivateRequest{}, presetFor(t, "semantic"))
	require.InDelta(t, 0.3, actReq.Threshold, 1e-6)
}

func TestApplyRecallModePreset_ExplicitCallerThresholdWins(t *testing.T) {
	actReq := &activation.ActivateRequest{Threshold: 0.7, Weights: &activation.Weights{}}
	applyRecallModePreset(actReq, &mbp.ActivateRequest{Threshold: 0.7}, presetFor(t, "semantic"))
	require.InDelta(t, 0.7, actReq.Threshold, 1e-6, "an explicit caller threshold is never modified")
}

// TestApplyRecallModePreset_DisableHopsWins pins the explicit-opt-out rule:
// a caller's DisableHops must not be overridden by a preset's MaxHops
// (explicit config is never silently substituted).
//
// RED CHECK: removing `&& !req.DisableHops` from the hops guard fails this.
func TestApplyRecallModePreset_DisableHopsWins(t *testing.T) {
	actReq := &activation.ActivateRequest{HopDepth: 0, Weights: &activation.Weights{}}
	applyRecallModePreset(actReq, &mbp.ActivateRequest{DisableHops: true}, presetFor(t, "deep"))
	require.Zero(t, actReq.HopDepth, "preset hops must not override an explicit DisableHops")
}

func TestApplyRecallModePreset_HopsApplyWhenUnset(t *testing.T) {
	actReq := &activation.ActivateRequest{Weights: &activation.Weights{}}
	applyRecallModePreset(actReq, &mbp.ActivateRequest{}, presetFor(t, "deep"))
	require.Equal(t, 4, actReq.HopDepth)
}

func TestApplyRecallModePreset_ScalarsFillOnlyCallerZeroFields(t *testing.T) {
	// Caller sent a weights struct with an explicit SemanticSimilarity; the
	// preset may fill the fields the caller left zero, never the set one.
	actReq := &activation.ActivateRequest{Weights: &activation.Weights{SemanticSimilarity: 0.5}}
	callerW := &mbp.Weights{SemanticSimilarity: 0.5}
	applyRecallModePreset(actReq, &mbp.ActivateRequest{Weights: callerW}, presetFor(t, "semantic"))
	require.InDelta(t, 0.5, actReq.Weights.SemanticSimilarity, 1e-6, "caller-set field must win")
	require.InDelta(t, 0.2, actReq.Weights.FullTextRelevance, 1e-6, "caller-zero field takes the preset value")
	require.True(t, actReq.Weights.DisableACTR)
	require.False(t, actReq.Weights.UseACTR)
}

func TestApplyRecallModePreset_NoCallerWeights_ScalarsDoNotOverlayResolved(t *testing.T) {
	// With no caller weights struct, scalar application is NOT this
	// function's job: explicit weight-carrying modes got the zero-base vector
	// in the weights block, and vault-default modes keep the vault's resolved
	// weights (a background default tints, it does not respecify scoring).
	// Only the DisableACTR strategy bit applies here.
	actReq := &activation.ActivateRequest{Weights: &activation.Weights{
		SemanticSimilarity: 0.6, FullTextRelevance: 0.4, DecayFactor: 0.4, HebbianBoost: 0.5, Recency: 0.3, UseACTR: true,
	}}
	applyRecallModePreset(actReq, &mbp.ActivateRequest{}, presetFor(t, "semantic"))
	require.InDelta(t, 0.6, actReq.Weights.SemanticSimilarity, 1e-6)
	require.InDelta(t, 0.4, actReq.Weights.FullTextRelevance, 1e-6)
	require.InDelta(t, 0.3, actReq.Weights.Recency, 1e-6)
	require.True(t, actReq.Weights.DisableACTR)
}

func TestPresetCarriesWeights(t *testing.T) {
	require.True(t, presetCarriesWeights(presetFor(t, "semantic")))
	require.True(t, presetCarriesWeights(presetFor(t, "recent")))
	require.False(t, presetCarriesWeights(presetFor(t, "deep")))
	require.False(t, presetCarriesWeights(presetFor(t, "balanced")))
}
