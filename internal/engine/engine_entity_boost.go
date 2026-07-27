package engine

import (
	"context"
	"sort"

	"github.com/scrypster/muninndb/internal/engine/activation"
	"github.com/scrypster/muninndb/internal/storage"
)

const (
	// entityBoostFactor is the score added to engrams that share a named entity
	// with a top-N BFS result. Kept well below typical BFS association weights
	// (~0.3–0.9) so the boost surfaces related content without dominating.
	entityBoostFactor = float64(0.15)

	// entityBoostTopN is the number of top BFS results whose entity links are
	// used as seeds for the spread-activation pass.
	entityBoostTopN = 5
)

// applyEntityBoost performs a post-BFS spread-activation pass using named
// entities. It takes the top-N results from the BFS activation, collects
// every entity linked to those engrams via the 0x20 forward index, then
// finds all other engrams in the same vault that mention those entities via
// the 0x23 reverse index. Each such engram receives a score boost of
// entityBoostFactor (or is added to the result set with that score if it was
// not already returned by BFS). Results are re-sorted by score descending.
//
// filters and excludeUntrusted carry the request's exclusion rules. They must be
// applied to engrams injected here, because this pass runs after activation.Run
// has returned and therefore after phase 6, the pipeline's correctness gate.
// Engrams already in results need no re-check — they passed phase 6 on the way
// in — so only the injection path consults them (issue #654).
func (e *Engine) applyEntityBoost(ctx context.Context, ws [8]byte, results []activation.ScoredEngram, filters []activation.Filter, excludeUntrusted bool) []activation.ScoredEngram {
	if len(results) == 0 {
		return results
	}

	// Seed the boost from at most entityBoostTopN top results.
	seedCount := len(results)
	if seedCount > entityBoostTopN {
		seedCount = entityBoostTopN
	}
	seeds := results[:seedCount]

	// Build a reverse lookup: ULID → index in results slice.
	seenInResults := make(map[storage.ULID]int, len(results))
	for i, r := range results {
		seenInResults[r.Engram.ID] = i
	}

	// Candidates rejected once stay rejected: the fetch and every exclusion
	// check depend only on the candidate, never on which seed reached it. A
	// candidate sharing entities with several seeds would otherwise be
	// re-fetched and re-checked per seed — common under restrictive filters,
	// where most of a dense entity neighbourhood is rejected.
	rejected := make(map[storage.ULID]struct{})

	// For each seed engram, iterate its entity links (0x20 forward index).
	for _, topEng := range seeds {
		_ = e.store.ScanEngramEntities(ctx, ws, topEng.Engram.ID, func(entityName string) error {
			// For each entity, scan all engrams that mention it (0x23 reverse index).
			return e.store.ScanEntityEngrams(ctx, entityName, func(entityWS [8]byte, engramID storage.ULID) error {
				if entityWS != ws {
					return nil // skip other vaults
				}
				// Skip the seed itself — it already has its BFS score.
				if engramID == topEng.Engram.ID {
					return nil
				}

				if idx, found := seenInResults[engramID]; found {
					// Boost existing result.
					results[idx].Score += entityBoostFactor
				} else {
					if _, wasRejected := rejected[engramID]; wasRejected {
						return nil
					}
					// Fetch engram and add as a new entity-boosted result.
					eng, err := e.store.GetEngram(ctx, ws, engramID)
					if err != nil || eng == nil {
						rejected[engramID] = struct{}{}
						return nil
					}
					if eng.State == storage.StateSoftDeleted || eng.State == storage.StateArchived {
						rejected[engramID] = struct{}{}
						return nil
					}
					// This engram was never in the pipeline's candidate pool,
					// so it has not been through phase 6. Apply the request's
					// exclusion rules before surfacing it (issue #654).
					//
					// The trust check mirrors phase 6 exactly: only
					// TrustUntrusted is excluded. TrustUnset is the zero-value
					// backward-compat alias for TrustInferred and passes
					// through, as it does in the pipeline.
					if excludeUntrusted && eng.Trust == storage.TrustUntrusted {
						rejected[engramID] = struct{}{}
						return nil
					}
					if !activation.PassesMetaFilter(eng, filters) {
						rejected[engramID] = struct{}{}
						return nil
					}
					results = append(results, activation.ScoredEngram{
						Engram: eng,
						Score:  entityBoostFactor,
					})
					seenInResults[engramID] = len(results) - 1
				}
				return nil
			})
		})
	}

	// Re-sort descending by score after boost adjustments.
	sort.Slice(results, func(i, j int) bool {
		return results[i].Score > results[j].Score
	})

	return results
}
