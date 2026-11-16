package activation

import "github.com/scrypster/muninndb/internal/rankorder"

// Content-first order (COG-35): "evidence of match orders; evidence of
// association annotates".
//
// On the default ACT-R path a recall's rows are ordered by their ABSOLUTE
// content score — min(ContentMatch, 1) x Confidence, the same quantity the
// abstention gate and the relevance bands key on — and a row's `score` IS
// that number. Association evidence (entity boost, Hebbian co-activation,
// predictive-activation transitions) is still computed and reported per row,
// and still INJECTS associated rows the query did not match, but it no longer
// changes order except to break an EXACT content tie. A row with no content
// score (an injection) has score 0 and therefore comes after every scored row,
// ordered by its association evidence, then newest-first.
//
// Why: measured on a production install (150 model-written rewordings of live
// memory titles, three vaults), ordering the engine's own rows by the content
// score alone put the right memory at rank 1 in 62/150 against 15/150 for the
// boosted order (MRR 0.495 vs 0.238, paired difference +0.257, 95% CI
// [+0.199, +0.318]); no strength of the entity boost measured better than
// none. The rule is structural (match evidence first), not a tuned weight —
// see docs/internals/decision-record.md.
//
// rrf, weighted_sum and CGDN keep their boosted order: their scores are not
// absolute content scores, so there is nothing to order "by content" on.

// ContentFirstOrder reports whether a recall scored under w is ordered
// content-first. It mirrors resolveWeights + phase 6's path selection exactly
// (nil weights = ACT-R; rrf takes precedence over CGDN; CGDN over ACT-R), so
// the engine's post-pipeline phases and phase 6 cannot disagree about which
// order a response is in.
func ContentFirstOrder(w *Weights) bool {
	if legacyBoostOrder() {
		return false
	}
	if w == nil {
		return true
	}
	return !w.UseRRFFusion && !w.UseCGDN && !w.DisableACTR
}

// AssociationEvidence is a row's total association evidence: the entity,
// Hebbian and predictive-activation contributions it carries. Under
// content-first order it breaks exact content ties and orders rows with no
// content score; it never moves a row past a different content score.
func AssociationEvidence(c ScoreComponents) float64 {
	return c.EntityBoost + c.HebbianBoost + c.TransitionBoost
}

// ResultBefore is the ONE comparator every re-sort of a recall's rows uses
// (phase 6 and the engine's post-pipeline phases), so no layer can reorder
// what another ordered. Score descending; under content-first order an exact
// score tie is then broken by association evidence; the final key is the
// newest-first rule (rankorder.Before). With contentFirst=false it is exactly
// the trunk's (Score, newest-first) order, so rrf / weighted_sum / CGDN are
// unchanged.
func ResultBefore(a, b *ScoredEngram, contentFirst bool) bool {
	if a.Score != b.Score {
		return a.Score > b.Score
	}
	if contentFirst {
		ea, eb := AssociationEvidence(a.Components), AssociationEvidence(b.Components)
		if ea != eb {
			return ea > eb
		}
	}
	return rankorder.Before(a.Engram.ID, b.Engram.ID)
}
