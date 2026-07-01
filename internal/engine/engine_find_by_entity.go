package engine

import (
	"context"
	"errors"
	"fmt"

	"github.com/scrypster/muninndb/internal/storage"
)

// errLimitReached is the internal sentinel used to stop an entity reverse
// index scan once the caller's limit is satisfied.
var errLimitReached = errors.New("limit reached")

// FindByEntityResult is the result of a FindByEntity lookup.
type FindByEntityResult struct {
	// Engrams are the live engrams linked to MatchedEntity, up to the
	// caller's limit.
	Engrams []*storage.Engram
	// MatchedEntity is the entity name that actually served the results:
	// the query itself on an exact hit, a vault entity name on a fuzzy hit,
	// empty when nothing matched.
	MatchedEntity string
	// Fuzzy is true when MatchedEntity was resolved by token matching
	// rather than exact (normalized) lookup — the resolution is always
	// reported, never silent (issue #571).
	Fuzzy bool
	// Candidates are the remaining ranked fuzzy candidates that were not
	// used for retrieval, reported so the caller can see near-misses.
	Candidates []string
}

// FindByEntity returns engrams in vault that mention entityName, using the
// 0x23 reverse index for O(matches) lookup. The exact (normalized) name is
// tried first; when it yields no live engrams, the vault's entity names are
// fuzzy-resolved by token overlap (issue #571: `knock` vs `The Knock`,
// `dream cycle` vs `dream-cycle`, `TokenArcade` vs `Token Arcade`) and the
// best-ranked candidate with live engrams serves the results.
// Results are limited to limit entries (default 20, max 50).
func (e *Engine) FindByEntity(ctx context.Context, vault, entityName string, limit int) (*FindByEntityResult, error) {
	if entityName == "" {
		return nil, fmt.Errorf("find_by_entity: entity_name is required")
	}
	if limit <= 0 {
		limit = 20
	}
	if limit > 50 {
		limit = 50
	}
	ws := e.store.ResolveVaultPrefix(vault)

	engrams, err := e.entityEngrams(ctx, ws, entityName, limit)
	if err != nil {
		return nil, err
	}
	if len(engrams) > 0 {
		return &FindByEntityResult{Engrams: engrams, MatchedEntity: entityName}, nil
	}

	// Exact lookup found nothing live — fuzzy-resolve against the vault's
	// entity names and use the best-ranked candidate that has live engrams.
	candidates := e.resolveEntityFuzzy(ctx, ws, entityName)
	for i, name := range candidates {
		engrams, err = e.entityEngrams(ctx, ws, name, limit)
		if err != nil {
			return nil, err
		}
		if len(engrams) > 0 {
			return &FindByEntityResult{
				Engrams:       engrams,
				MatchedEntity: name,
				Fuzzy:         true,
				Candidates:    append([]string(nil), candidates[i+1:]...),
			}, nil
		}
	}
	return &FindByEntityResult{}, nil
}

// entityEngrams scans the 0x23 reverse index for live engrams in ws linked
// to entityName, up to limit entries.
func (e *Engine) entityEngrams(ctx context.Context, ws [8]byte, entityName string, limit int) ([]*storage.Engram, error) {
	var results []*storage.Engram
	err := e.store.ScanEntityEngrams(ctx, entityName, func(gotWS [8]byte, id storage.ULID) error {
		if gotWS != ws {
			return nil // different vault — skip
		}
		if len(results) >= limit {
			return errLimitReached // sentinel to stop scanning
		}
		eng, err := e.store.GetEngram(ctx, ws, id)
		if err != nil || eng == nil {
			return nil // skip missing/deleted
		}
		if eng.State == storage.StateSoftDeleted || eng.State == storage.StateArchived {
			return nil
		}
		results = append(results, eng)
		return nil
	})
	if err != nil && !errors.Is(err, errLimitReached) {
		return nil, fmt.Errorf("find_by_entity: scan: %w", err)
	}
	return results, nil
}
