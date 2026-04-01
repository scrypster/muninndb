package engine

import (
	"context"
	"fmt"
	"sort"

	"github.com/scrypster/muninndb/internal/storage"
)

const (
	defaultEntityEngramLimit   = 20
	defaultListEntitiesLimit   = 50
	entityCoOccurrenceMinCount = 2
	entityCoOccurrenceTopN     = 20
)

// EntityCoOccEntry is a named type for co-occurrence entries.
type EntityCoOccEntry struct {
	Name  string
	Count int
}

// EntityAggregateData holds the full aggregate view for a named entity.
// Used as the engine-layer return type; MCP adapter projects it to mcp.EntityAggregate.
type EntityAggregateData struct {
	Record      *storage.EntityRecord
	Engrams     []*storage.Engram
	Relations   []storage.RelationshipRecord
	CoOccurring []EntityCoOccEntry
}

// GetEntityAggregate returns the full aggregate view for a named entity.
func (e *Engine) GetEntityAggregate(ctx context.Context, vault, entityName string, limit int) (*EntityAggregateData, error) {
	if limit <= 0 {
		limit = defaultEntityEngramLimit
	}

	// 1. Entity metadata record (global, vault-agnostic)
	rec, err := e.store.GetEntityRecord(ctx, entityName)
	if err != nil {
		return nil, err
	}
	if rec == nil {
		return nil, nil // not found
	}

	ws := e.store.ResolveVaultPrefix(vault)

	// 2. Engrams that mention this entity (vault-scoped via ScanEntityEngrams reverse index)
	var engrams []*storage.Engram
	scanErr := e.store.ScanEntityEngrams(ctx, entityName, func(gotWS [8]byte, id storage.ULID) error {
		if gotWS != ws {
			return nil // different vault — skip
		}
		if len(engrams) >= limit {
			return fmt.Errorf("limit reached") // sentinel to stop scanning
		}
		eng, err := e.store.GetEngram(ctx, ws, id)
		if err != nil || eng == nil {
			return nil // skip missing/deleted
		}
		if eng.State == storage.StateSoftDeleted || eng.State == storage.StateArchived {
			return nil
		}
		engrams = append(engrams, eng)
		return nil
	})
	if scanErr != nil && scanErr.Error() != "limit reached" {
		return nil, scanErr
	}

	// 3. Relationships involving this entity (vault-scoped).
	// ScanEntityRelationships uses the 0x26 index for O(engrams-referencing-entity) lookup
	// instead of the O(all vault relationships) full scan that ScanRelationships would do.
	var rels []storage.RelationshipRecord
	err = e.store.ScanEntityRelationships(ctx, ws, entityName, func(r storage.RelationshipRecord) error {
		rels = append(rels, r)
		return nil
	})
	if err != nil {
		return nil, err
	}

	// 4. Co-occurring entities (vault-scoped), top-N by count
	var coEntries []EntityCoOccEntry
	err = e.store.ScanEntityClusters(ctx, ws, entityCoOccurrenceMinCount, func(nameA, nameB string, count int) error {
		if nameA == entityName {
			coEntries = append(coEntries, EntityCoOccEntry{Name: nameB, Count: count})
		} else if nameB == entityName {
			coEntries = append(coEntries, EntityCoOccEntry{Name: nameA, Count: count})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(coEntries, func(i, j int) bool { return coEntries[i].Count > coEntries[j].Count })
	if len(coEntries) > entityCoOccurrenceTopN {
		coEntries = coEntries[:entityCoOccurrenceTopN]
	}

	return &EntityAggregateData{
		Record:      rec,
		Engrams:     engrams,
		Relations:   rels,
		CoOccurring: coEntries,
	}, nil
}

// ListEntities returns EntityRecord summaries sorted by mention_count descending.
func (e *Engine) ListEntities(ctx context.Context, vault string, limit int, state string) ([]storage.EntityRecord, error) {
	if limit <= 0 {
		limit = defaultListEntitiesLimit
	}

	ws := e.store.ResolveVaultPrefix(vault)

	var records []storage.EntityRecord
	err := e.store.ScanVaultEntityNames(ctx, ws, func(name string) error {
		rec, err := e.store.GetEntityRecord(ctx, name)
		if err != nil || rec == nil {
			return nil // skip missing
		}
		if state != "" && rec.State != state {
			return nil // filter by state
		}
		records = append(records, *rec)
		return nil
	})
	if err != nil {
		return nil, err
	}

	sort.Slice(records, func(i, j int) bool {
		return records[i].MentionCount > records[j].MentionCount
	})
	if len(records) > limit {
		records = records[:limit]
	}
	return records, nil
}
