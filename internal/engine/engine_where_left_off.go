package engine

import (
	"context"
	"fmt"
	"time"

	"github.com/scrypster/muninndb/internal/storage"
)

// WhereLeftOff returns the most recently accessed non-deleted, non-completed engrams
// sorted by LastAccess descending. Uses the 0x22 LastAccess index for O(limit) lookup.
//
// excludeTypeLabels is an opt-in salience filter (S5): engrams whose TypeLabel
// is in the set are skipped, and the scan keeps going so the response still
// fills out limit with real memories. A nil or empty excludeTypeLabels is a
// no-op — the default, pre-S5 behavior — so existing callers see no change.
func (e *Engine) WhereLeftOff(ctx context.Context, vault string, limit int, excludeTypeLabels []string) ([]*storage.Engram, error) {
	if limit <= 0 {
		limit = 10
	}
	if limit > 50 {
		limit = 50
	}
	var exclude map[string]struct{}
	if len(excludeTypeLabels) > 0 {
		exclude = make(map[string]struct{}, len(excludeTypeLabels))
		for _, tl := range excludeTypeLabels {
			exclude[tl] = struct{}{}
		}
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
		// Drop facts explicitly invalidated on the valid-time axis (forget.not_true_since
		// / a supersede stamp leaves the engram ACTIVE), so this session-orientation
		// surface never leads with something the user marked "no longer true" (COG-19).
		if eng.IsExpired(time.Now()) {
			return nil
		}
		if exclude != nil {
			if _, skip := exclude[eng.TypeLabel]; skip {
				return nil
			}
		}
		results = append(results, eng)
		return nil
	})
	if err != nil && err.Error() != "limit reached" {
		return nil, fmt.Errorf("where_left_off: scan: %w", err)
	}
	return results, nil
}
