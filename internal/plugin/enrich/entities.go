package enrich

import (
	"context"

	"github.com/scrypster/muninndb/internal/plugin"
)

// StoreEntities persists extracted entities and links them to an engram.
// After all entity-engram links are written, co-occurrence counts are incremented
// for every pair of entities that appear in the same engram.
func StoreEntities(ctx context.Context, store plugin.PluginStore, engramID plugin.ULID, entities []plugin.ExtractedEntity) error {
	var linked []string
	for _, entity := range entities {
		if err := store.UpsertEntity(ctx, engramID, entity); err != nil {
			return err
		}
		if err := store.LinkEngramToEntity(ctx, engramID, entity.Name); err != nil {
			return err
		}
		linked = append(linked, entity.Name)
	}
	// Write co-occurrence pairs for entities co-appearing in this engram.
	for i := 0; i < len(linked); i++ {
		for j := i + 1; j < len(linked); j++ {
			_ = store.IncrementEntityCoOccurrence(ctx, engramID, linked[i], linked[j])
		}
	}
	return nil
}

// StoreRelationships persists extracted relationships for an engram.
func StoreRelationships(ctx context.Context, store plugin.PluginStore, engramID plugin.ULID, relationships []plugin.ExtractedRelation) error {
	for _, rel := range relationships {
		if err := store.UpsertRelationship(ctx, engramID, rel); err != nil {
			return err
		}
	}
	return nil
}
