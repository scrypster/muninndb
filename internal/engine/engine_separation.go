package engine

import (
	"context"
	"sort"

	"github.com/scrypster/muninndb/internal/engine/activation"
)

const (
	// separationSeedTopN is the number of top results whose entity links are
	// used as the "query entity context" for pattern separation scoring.
	// Matches entityBoostTopN so both features derive context from the same seeds.
	separationSeedTopN = 5
)

// applySeparation applies hippocampal pattern separation to post-scored results.
// It collects entities from the top-N seed results (the "query entity context"),
// then penalises candidates whose entity sets have low Jaccard overlap with the
// seeds. This reduces cross-context interference without modifying the HNSW index.
//
// No-op when e.separationScorer is nil (feature disabled).
func (e *Engine) applySeparation(ctx context.Context, ws [8]byte, results []activation.ScoredEngram) []activation.ScoredEngram {
	if e.separationScorer == nil || len(results) == 0 {
		return results
	}

	// Collect entities from the top-N seed results to form the query entity context.
	seedCount := len(results)
	if seedCount > separationSeedTopN {
		seedCount = separationSeedTopN
	}

	queryEntitySet := make(map[string]struct{})
	for _, seed := range results[:seedCount] {
		_ = e.store.ScanEngramEntities(ctx, ws, seed.Engram.ID, func(entityName string) error {
			queryEntitySet[entityName] = struct{}{}
			return nil
		})
	}

	// Flatten the entity set to a slice for the scorer.
	queryEntities := make([]string, 0, len(queryEntitySet))
	for name := range queryEntitySet {
		queryEntities = append(queryEntities, name)
	}

	// No entity context from seeds → no separation signal.
	if len(queryEntities) == 0 {
		return results
	}

	// Build seed ID set for filtering.
	seedIDs := make(map[[16]byte]struct{}, seedCount)
	for _, seed := range results[:seedCount] {
		seedIDs[[16]byte(seed.Engram.ID)] = struct{}{}
	}

	// Build candidate ID slice, excluding seeds to avoid double entity lookup.
	candidateIDs := make([][16]byte, 0, len(results)-seedCount)
	candidateIdx := make([]int, 0, len(results)-seedCount) // maps back to results index
	for i, r := range results {
		rid := [16]byte(r.Engram.ID)
		if _, isSeed := seedIDs[rid]; isSeed {
			continue
		}
		candidateIDs = append(candidateIDs, rid)
		candidateIdx = append(candidateIdx, i)
	}

	multipliers, err := e.separationScorer.ScoreSeparation(ctx, ws, queryEntities, candidateIDs)
	if err != nil {
		// On error, return results unmodified.
		return results
	}

	// Apply multipliers only to non-seed candidates.
	for j, ri := range candidateIdx {
		results[ri].Score *= multipliers[j]
	}

	// Re-sort descending by score after separation adjustments.
	sort.Slice(results, func(i, j int) bool {
		return results[i].Score > results[j].Score
	})

	return results
}
