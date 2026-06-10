package enrich

import (
	"context"
	"fmt"
	"testing"

	"github.com/scrypster/muninndb/internal/plugin"
	"github.com/scrypster/muninndb/internal/storage"
)

type mockPluginStore struct {
	upsertedEntities []plugin.ExtractedEntity
	linkedEngrams    []struct {
		engramID plugin.ULID
		entity   string
	}
	upsertedRels []struct {
		engramID plugin.ULID
		rel      plugin.ExtractedRelation
	}
	failOnUpsertEntity bool
	failOnLink         bool
	failOnUpsertRel    bool
}

func (m *mockPluginStore) UpsertEntity(_ context.Context, e plugin.ExtractedEntity) error {
	if m.failOnUpsertEntity {
		return fmt.Errorf("upsert entity failed")
	}
	m.upsertedEntities = append(m.upsertedEntities, e)
	return nil
}

func (m *mockPluginStore) LinkEngramToEntity(_ context.Context, id plugin.ULID, name string) error {
	if m.failOnLink {
		return fmt.Errorf("link failed")
	}
	m.linkedEngrams = append(m.linkedEngrams, struct {
		engramID plugin.ULID
		entity   string
	}{id, name})
	return nil
}

func (m *mockPluginStore) UpsertRelationship(_ context.Context, id plugin.ULID, rel plugin.ExtractedRelation) error {
	if m.failOnUpsertRel {
		return fmt.Errorf("upsert relationship failed")
	}
	m.upsertedRels = append(m.upsertedRels, struct {
		engramID plugin.ULID
		rel      plugin.ExtractedRelation
	}{id, rel})
	return nil
}

// Unused interface methods
func (m *mockPluginStore) CountWithoutFlag(context.Context, uint8, uint8) (int64, error) {
	return 0, nil
}
func (m *mockPluginStore) ScanWithoutFlag(context.Context, uint8, uint8) plugin.EngramIterator {
	return nil
}
func (m *mockPluginStore) SetDigestFlag(context.Context, plugin.ULID, uint8) error       { return nil }
func (m *mockPluginStore) GetDigestFlags(context.Context, plugin.ULID) (uint8, error)    { return 0, nil }
func (m *mockPluginStore) UpdateEmbedding(context.Context, plugin.ULID, []float32) error { return nil }
func (m *mockPluginStore) UpdateDigest(context.Context, plugin.ULID, *plugin.EnrichmentResult) error {
	return nil
}
func (m *mockPluginStore) HNSWInsert(context.Context, plugin.ULID, []float32) error { return nil }
func (m *mockPluginStore) AutoLinkByEmbedding(context.Context, plugin.ULID, []float32) error {
	return nil
}
func (m *mockPluginStore) IncrementEntityCoOccurrence(context.Context, plugin.ULID, string, string) error {
	return nil
}

func TestStoreEntities_Success(t *testing.T) {
	store := &mockPluginStore{}
	id := storage.NewULID()
	entities := []plugin.ExtractedEntity{
		{Name: "Go", Type: "language", Confidence: 0.9},
		{Name: "PostgreSQL", Type: "database", Confidence: 0.95},
	}

	err := StoreEntities(context.Background(), store, id, entities)
	if err != nil {
		t.Fatalf("StoreEntities failed: %v", err)
	}
	if len(store.upsertedEntities) != 2 {
		t.Fatalf("expected 2 upserted entities, got %d", len(store.upsertedEntities))
	}
	if len(store.linkedEngrams) != 2 {
		t.Fatalf("expected 2 links, got %d", len(store.linkedEngrams))
	}
}

func TestStoreEntities_Empty(t *testing.T) {
	store := &mockPluginStore{}
	err := StoreEntities(context.Background(), store, storage.NewULID(), nil)
	if err != nil {
		t.Fatalf("StoreEntities with nil should succeed: %v", err)
	}
}

func TestStoreEntities_UpsertError(t *testing.T) {
	store := &mockPluginStore{failOnUpsertEntity: true}
	entities := []plugin.ExtractedEntity{{Name: "X", Type: "tool", Confidence: 0.5}}

	err := StoreEntities(context.Background(), store, storage.NewULID(), entities)
	if err == nil {
		t.Fatal("expected error from UpsertEntity failure")
	}
}

func TestStoreEntities_LinkError(t *testing.T) {
	store := &mockPluginStore{failOnLink: true}
	entities := []plugin.ExtractedEntity{{Name: "X", Type: "tool", Confidence: 0.5}}

	err := StoreEntities(context.Background(), store, storage.NewULID(), entities)
	if err == nil {
		t.Fatal("expected error from LinkEngramToEntity failure")
	}
}

func TestStoreRelationships_Success(t *testing.T) {
	store := &mockPluginStore{}
	id := storage.NewULID()
	rels := []plugin.ExtractedRelation{
		{FromEntity: "app", ToEntity: "PostgreSQL", RelType: "uses", Weight: 0.9},
		{FromEntity: "Go", ToEntity: "app", RelType: "implements", Weight: 0.8},
	}

	err := StoreRelationships(context.Background(), store, id, rels)
	if err != nil {
		t.Fatalf("StoreRelationships failed: %v", err)
	}
	if len(store.upsertedRels) != 2 {
		t.Fatalf("expected 2 upserted rels, got %d", len(store.upsertedRels))
	}
}

func TestStoreRelationships_Empty(t *testing.T) {
	store := &mockPluginStore{}
	err := StoreRelationships(context.Background(), store, storage.NewULID(), nil)
	if err != nil {
		t.Fatalf("StoreRelationships with nil should succeed: %v", err)
	}
}

func TestStoreRelationships_Error(t *testing.T) {
	store := &mockPluginStore{failOnUpsertRel: true}
	rels := []plugin.ExtractedRelation{
		{FromEntity: "a", ToEntity: "b", RelType: "uses", Weight: 0.5},
	}

	err := StoreRelationships(context.Background(), store, storage.NewULID(), rels)
	if err == nil {
		t.Fatal("expected error from UpsertRelationship failure")
	}
}
