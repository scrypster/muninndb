package engine

import (
	"context"
	"fmt"
	"sort"

	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/storage/keys"
)

// FindByConcept returns engrams in vault whose Concept exactly matches the
// given string. Uses the 0x2B concept index for O(matches) lookup.
//
// Concepts in MuninnDB are NOT unique — multiple writes with the same
// Concept produce distinct ULIDs. This function returns ALL matching
// engrams (excluding soft-deleted and archived), sorted newest-first by
// ULID timestamp, capped to limit. Default limit 1, max 50.
//
// Hash collisions are filtered by hydrating each candidate and comparing
// the full Concept string. This is the same pattern used by FindByEntity
// for soft-deleted/archived filtering — secondary indexes are advisory,
// the engram body is source of truth.
func (e *Engine) FindByConcept(ctx context.Context, vault, concept string, limit int) ([]*storage.Engram, error) {
	if concept == "" {
		return nil, fmt.Errorf("find_by_concept: concept is required")
	}
	if limit <= 0 {
		limit = 1
	}
	if limit > 50 {
		limit = 50
	}
	ws := e.store.ResolveVaultPrefix(vault)
	conceptHash := keys.Hash(concept)

	// Step 1: collect candidate engram IDs from the 0x2B prefix scan.
	var candidates []storage.ULID
	err := e.store.ScanConceptIndex(ctx, ws, conceptHash, func(id storage.ULID) error {
		candidates = append(candidates, id)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("find_by_concept: scan: %w", err)
	}

	// Step 2: hydrate, filter hash collisions, filter soft-deleted/archived.
	var results []*storage.Engram
	for _, id := range candidates {
		eng, err := e.store.GetEngram(ctx, ws, id)
		if err != nil || eng == nil {
			continue
		}
		if eng.Concept != concept {
			continue // hash collision — concept hash matched but full string didn't
		}
		if eng.State == storage.StateSoftDeleted || eng.State == storage.StateArchived {
			continue
		}
		results = append(results, eng)
	}

	// Step 3: sort newest-first by ULID (ULIDs are time-ordered).
	sort.Slice(results, func(i, j int) bool {
		return results[i].ID.String() > results[j].ID.String()
	})

	// Step 4: cap to limit.
	if len(results) > limit {
		results = results[:limit]
	}
	return results, nil
}
