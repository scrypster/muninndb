package engine

import (
	"github.com/scrypster/muninndb/internal/auth"
	"github.com/scrypster/muninndb/internal/engine/activation"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// presetCarriesWeights reports whether a recall-mode preset expresses any
// scoring-weight intent (semantic/recent do; deep and balanced don't). A
// weight-carrying preset defines a FULL legacy weight vector for the wire-mode
// zero-base branch in activateCore — the RecallModePreset zero-value scheme
// cannot express "force zero", so semantic's implicit decay=hebbian=recency=0
// is representable only by starting from the zero struct, never by overlaying
// a pre-filled one (overlaying resolved defaults lets a fresh, recently-active
// engram sum ~0.7 from decay+recency alone under legacy scoring and outrank
// genuine matches above semantic's own 0.3 threshold).
func presetCarriesWeights(p auth.RecallModePreset) bool {
	return p.SemanticSimilarity > 0 || p.FullTextRelevance > 0 || p.Recency > 0 || p.DisableACTR
}

// applyRecallModePreset applies the resolved recall-mode preset to the
// activation request. It runs AFTER the weights block and the COG-6 default
// coerce, because its decisions are keyed on facts only available there: the
// effective scoring mode (actReq.Weights.UseRRFFusion) and the caller's wire
// request (what was genuinely set by the caller vs filled by resolution).
//
//   - Threshold: preset thresholds are ACT-R-calibrated (semantic 0.3, recent
//     0.2, deep 0.1). Under rrf fusion they sit far above the entire rrf score
//     range (finals typically < 0.05), so the preset ABSTAINS and the
//     mode-aware default stands (Threshold stays 0 → activation.Run() applies
//     its rrf floor) — otherwise a mode-carrying recall re-creates the
//     silently-empty-rrf-vault bug R1 fixed on the no-mode path (#704). On
//     ACT-R/weighted_sum the preset overrides the 0.1 coerce, as it always
//     has. An explicit caller threshold is never modified.
//   - Hops: applied only when the caller neither set MaxHops nor explicitly
//     disabled traversal — DisableHops is an explicit opt-out and explicit
//     config is never silently substituted.
//   - Weight scalars: filled ONLY when the caller sent a weights struct, and
//     only into fields left zero on the WIRE (caller-explicit fields always
//     win). When the caller sent no weights, explicit weight-carrying modes
//     were already given the preset's full zero-base vector by the weights
//     block, and vault-default modes deliberately keep the vault's resolved
//     weights — a background default tints, it does not respecify scoring.
//   - DisableACTR: a scoring-strategy bit, not a scalar — applies whenever the
//     preset carries it. Under rrf fusion it is inert (phase 6 dispatches on
//     UseRRFFusion first, and the rrf config branch sets the same bits).
func applyRecallModePreset(actReq *activation.ActivateRequest, req *mbp.ActivateRequest, preset auth.RecallModePreset) {
	if preset.Threshold > 0 && req.Threshold == 0 && !actReq.Weights.UseRRFFusion {
		actReq.Threshold = float64(preset.Threshold)
	}
	if preset.MaxHops > 0 && req.MaxHops == 0 && !req.DisableHops {
		actReq.HopDepth = preset.MaxHops
	}
	if w, callerW := actReq.Weights, req.Weights; w != nil && callerW != nil {
		if preset.SemanticSimilarity > 0 && callerW.SemanticSimilarity == 0 {
			w.SemanticSimilarity = preset.SemanticSimilarity
		}
		if preset.FullTextRelevance > 0 && callerW.FullTextRelevance == 0 {
			w.FullTextRelevance = preset.FullTextRelevance
		}
		if preset.Recency > 0 && callerW.Recency == 0 {
			w.Recency = preset.Recency
		}
	}
	if preset.DisableACTR && actReq.Weights != nil {
		actReq.Weights.DisableACTR = true
		actReq.Weights.UseACTR = false
	}
}
