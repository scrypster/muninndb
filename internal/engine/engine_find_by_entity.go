package engine

import (
	"context"
	"fmt"

	"github.com/scrypster/muninndb/internal/storage"
)

// FindByEntity returns all engrams in vault that mention entityName,
// using the 0x23 reverse index for O(matches) lookup.
// Results are limited to limit entries (default 20, max 50).
func (e *Engine) FindByEntity(ctx context.Context, vault, entityName string, limit int) ([]*storage.Engram, error) {
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
	var results []*storage.Engram
	err := e.store.ScanEntityEngrams(ctx, entityName, func(gotWS [8]byte, id storage.ULID) error {
		if gotWS != ws {
			return nil // different vault — skip
		}
		if len(results) >= limit {
			return fmt.Errorf("limit reached") // sentinel to stop scanning
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
	if err != nil && err.Error() != "limit reached" {
		return nil, fmt.Errorf("find_by_entity: scan: %w", err)
	}
	return results, nil
}
