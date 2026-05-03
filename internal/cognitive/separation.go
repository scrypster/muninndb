package cognitive

import (
	"context"
)

// SeparationStore abstracts entity lookups needed by the pattern separation scorer.
// The storage.PebbleStore satisfies this interface via ScanEngramEntities.
type SeparationStore interface {
	// GetEngramEntities returns entity names linked to an engram.
	GetEngramEntities(ctx context.Context, ws [8]byte, engramID [16]byte) ([]string, error)
}

// SeparationScorer applies hippocampal pattern separation to ACTIVATE results.
// For each candidate, it computes entity Jaccard similarity against the query's
// entity context and returns a score multiplier in [0.1, 1.0]. Candidates that
// share no entity context with the query receive the maximum penalty.
type SeparationScorer struct {
	store  SeparationStore
	config SeparationConfig
}

// NewSeparationScorer creates a scorer with the given store and config.
func NewSeparationScorer(store SeparationStore, config SeparationConfig) *SeparationScorer {
	return &SeparationScorer{
		store:  store,
		config: config,
	}
}

// ScoreSeparation returns a multiplier [0.1, 1.0] for each candidate.
// 1.0 = same context (no penalty), lower = different context (penalised).
//
// If queryEntities is empty, all candidates receive 1.0 (no penalty) because
// there is no entity context to compare against.
func (s *SeparationScorer) ScoreSeparation(ctx context.Context, ws [8]byte, queryEntities []string, candidateIDs [][16]byte) ([]float64, error) {
	multipliers := make([]float64, len(candidateIDs))

	// No query entities → no separation signal; return all 1.0.
	if len(queryEntities) == 0 {
		for i := range multipliers {
			multipliers[i] = 1.0
		}
		return multipliers, nil
	}

	// Build a set from query entities for O(1) lookup.
	querySet := make(map[string]struct{}, len(queryEntities))
	for _, e := range queryEntities {
		querySet[e] = struct{}{}
	}

	alpha := s.config.RepulsionAlpha
	if alpha < 0 || alpha >= 1 {
		alpha = 0.3
	}

	for i, cid := range candidateIDs {
		candEntities, err := s.store.GetEngramEntities(ctx, ws, cid)
		if err != nil {
			// On error, be lenient — no penalty.
			multipliers[i] = 1.0
			continue
		}

		jaccard := JaccardSimilarity(querySet, candEntities)
		mult := 1.0 - alpha*(1.0-jaccard)

		// Clamp to [0.1, 1.0].
		if mult < 0.1 {
			mult = 0.1
		}
		if mult > 1.0 {
			mult = 1.0
		}
		multipliers[i] = mult
	}

	return multipliers, nil
}

// JaccardSimilarity computes |A ∩ B| / |A ∪ B| between a set A (map) and
// a slice B. Returns 0.0 if both are empty, 1.0 if identical.
func JaccardSimilarity(setA map[string]struct{}, sliceB []string) float64 {
	if len(setA) == 0 && len(sliceB) == 0 {
		return 0.0
	}

	setB := make(map[string]struct{}, len(sliceB))
	for _, e := range sliceB {
		setB[e] = struct{}{}
	}

	// |A ∩ B|
	intersection := 0
	for k := range setA {
		if _, ok := setB[k]; ok {
			intersection++
		}
	}

	// |A ∪ B| = |A| + |B| - |A ∩ B|
	union := len(setA) + len(setB) - intersection
	if union == 0 {
		return 0.0
	}

	return float64(intersection) / float64(union)
}
