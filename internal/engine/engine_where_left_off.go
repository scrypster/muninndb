package engine

import (
	"context"
	"fmt"

	"github.com/scrypster/muninndb/internal/storage"
)

// WhereLeftOff returns the most recently accessed non-deleted, non-completed engrams
// sorted by LastAccess descending. Uses the 0x22 LastAccess index for O(limit) lookup.
func (e *Engine) WhereLeftOff(ctx context.Context, vault string, limit int) ([]*storage.Engram, error) {
	if limit <= 0 {
		limit = 10
	}
	if limit > 50 {
		limit = 50
	}
	ws := e.store.ResolveVaultPrefix(vault)
	var results []*storage.Engram
	err := e.store.ScanLastAccessDesc(ctx, ws, func(id storage.ULID, _ int64) error {
		if len(results) >= limit {
			return fmt.Errorf("limit reached")
		}
		eng, err := e.store.GetEngram(ctx, ws, id)
		if err != nil || eng == nil {
			return nil
		}
		if eng.State == storage.StateSoftDeleted || eng.State == storage.StateCompleted || eng.State == storage.StateArchived {
			return nil
		}
		results = append(results, eng)
		return nil
	})
	if err != nil && err.Error() != "limit reached" {
		return nil, fmt.Errorf("where_left_off: scan: %w", err)
	}
	return results, nil
}
