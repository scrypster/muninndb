package plugin

import (
	"context"
	"fmt"

	hnswpkg "github.com/scrypster/muninndb/internal/index/hnsw"
	"github.com/scrypster/muninndb/internal/storage"
)

// pluginStoreAdapter wraps *storage.PebbleStore and the HNSW registry to
// implement the plugin.PluginStore interface.
type pluginStoreAdapter struct {
	store *storage.PebbleStore
	hnsw  *hnswpkg.Registry
}

// NewStoreAdapter returns a PluginStore backed by the given PebbleStore and HNSW registry.
func NewStoreAdapter(store *storage.PebbleStore, hnsw *hnswpkg.Registry) PluginStore {
	return &pluginStoreAdapter{store: store, hnsw: hnsw}
}

func (a *pluginStoreAdapter) CountWithoutFlag(ctx context.Context, flag uint8) (int64, error) {
	return a.store.CountWithoutFlag(ctx, flag)
}

func (a *pluginStoreAdapter) ScanWithoutFlag(ctx context.Context, flag uint8) EngramIterator {
	iter := a.store.ScanWithoutFlag(ctx, flag)
	if iter == nil {
		// Prevent a typed-nil concrete pointer from being wrapped in a non-nil
		// interface value, which would cause a nil-dereference panic in the caller.
		return nil
	}
	return iter
}

func (a *pluginStoreAdapter) SetDigestFlag(ctx context.Context, id ULID, flag uint8) error {
	return a.store.SetDigestFlag(ctx, storage.ULID(id), flag)
}

func (a *pluginStoreAdapter) GetDigestFlags(ctx context.Context, id ULID) (uint8, error) {
	return a.store.GetDigestFlags(ctx, storage.ULID(id))
}

func (a *pluginStoreAdapter) UpdateEmbedding(ctx context.Context, id ULID, vec []float32) error {
	ws, ok := a.store.FindVaultPrefix(storage.ULID(id))
	if !ok {
		return fmt.Errorf("UpdateEmbedding: engram %s not found", id.String())
	}
	return a.store.UpdateEmbedding(ctx, ws, storage.ULID(id), vec)
}

func (a *pluginStoreAdapter) UpdateDigest(ctx context.Context, id ULID, result *EnrichmentResult) error {
	if result == nil {
		return fmt.Errorf("UpdateDigest: nil enrichment result")
	}
	return a.store.UpdateDigest(ctx, storage.ULID(id), result.Summary, result.KeyPoints, result.MemoryType, result.TypeLabel)
}

func (a *pluginStoreAdapter) UpsertEntity(ctx context.Context, entity ExtractedEntity) error {
	record := storage.EntityRecord{
		Name:       entity.Name,
		Type:       entity.Type,
		Confidence: entity.Confidence,
	}
	return a.store.UpsertEntityRecord(ctx, record, "plugin:enrich")
}

func (a *pluginStoreAdapter) LinkEngramToEntity(ctx context.Context, engramID ULID, entityName string) error {
	ws, ok := a.store.FindVaultPrefix(storage.ULID(engramID))
	if !ok {
		return fmt.Errorf("LinkEngramToEntity: engram %s not found", engramID.String())
	}
	return a.store.WriteEntityEngramLink(ctx, ws, storage.ULID(engramID), entityName)
}

func (a *pluginStoreAdapter) IncrementEntityCoOccurrence(ctx context.Context, engramID ULID, nameA, nameB string) error {
	ws, ok := a.store.FindVaultPrefix(storage.ULID(engramID))
	if !ok {
		return fmt.Errorf("IncrementEntityCoOccurrence: engram %s not found", engramID.String())
	}
	return a.store.IncrementEntityCoOccurrence(ctx, ws, nameA, nameB)
}

func (a *pluginStoreAdapter) UpsertRelationship(ctx context.Context, engramID ULID, rel ExtractedRelation) error {
	ws, ok := a.store.FindVaultPrefix(storage.ULID(engramID))
	if !ok {
		return fmt.Errorf("UpsertRelationship: engram %s not found", engramID.String())
	}
	record := storage.RelationshipRecord{
		FromEntity: rel.FromEntity,
		ToEntity:   rel.ToEntity,
		RelType:    rel.RelType,
		Weight:     rel.Weight,
		Source:     "plugin:enrich",
	}
	return a.store.UpsertRelationshipRecord(ctx, ws, storage.ULID(engramID), record)
}

func (a *pluginStoreAdapter) HNSWInsert(ctx context.Context, id ULID, vec []float32) error {
	ws, ok := a.store.FindVaultPrefix(storage.ULID(id))
	if !ok {
		return fmt.Errorf("HNSWInsert: engram %s not found", id.String())
	}
	return a.hnsw.Insert(ctx, ws, [16]byte(id), vec)
}

func (a *pluginStoreAdapter) AutoLinkByEmbedding(ctx context.Context, id ULID, vec []float32) error {
	ws, ok := a.store.FindVaultPrefix(storage.ULID(id))
	if !ok {
		return nil // not a fatal error
	}
	results, err := a.hnsw.Search(ctx, ws, vec, 5)
	if err != nil || len(results) == 0 {
		return nil
	}
	for _, r := range results {
		if r.ID == [16]byte(id) {
			continue // skip self
		}
		// Write a RELATES_TO association with weight = similarity * 0.8
		weight := float32(r.Score * 0.8)
		if weight <= 0 {
			continue
		}
		assoc := &storage.Association{
			TargetID: storage.ULID(r.ID),
			RelType:  storage.RelRelatesTo,
			Weight:   weight,
		}
		_ = a.store.WriteAssociation(ctx, ws, storage.ULID(id), storage.ULID(r.ID), assoc)
	}
	return nil
}
