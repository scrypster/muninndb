package plugin

import (
	"context"
	"os"
	"testing"

	"github.com/scrypster/muninndb/internal/storage"
)

// openTestStore returns a PebbleStore backed by a temp directory.
// It registers t.Cleanup to close and remove the directory.
func openTestStore(t *testing.T) *storage.PebbleStore {
	t.Helper()
	dir, err := os.MkdirTemp("", "muninndb-plugin-test-*")
	if err != nil {
		t.Fatalf("create temp dir: %v", err)
	}
	db, err := storage.OpenPebble(dir, storage.DefaultOptions())
	if err != nil {
		os.RemoveAll(dir)
		t.Fatalf("open pebble: %v", err)
	}
	store := storage.NewPebbleStore(db, storage.PebbleStoreConfig{CacheSize: 100})
	t.Cleanup(func() {
		store.Close()
		os.RemoveAll(dir)
	})
	return store
}

func TestStoreAdapter_Methods(t *testing.T) {
	store := openTestStore(t)
	adapter := &pluginStoreAdapter{store: store}
	ctx := context.Background()

	// Write a real engram first so FindVaultPrefix works.
	ws := store.VaultPrefix("test-vault")
	id, err := store.WriteEngram(ctx, ws, &storage.Engram{
		Concept: "test concept",
		Content: "test content",
	})
	if err != nil {
		t.Fatalf("WriteEngram: %v", err)
	}

	// UpsertEntity should store an entity record.
	if err := adapter.UpsertEntity(ctx, ExtractedEntity{Name: "PostgreSQL", Type: "database", Confidence: 0.9}); err != nil {
		t.Errorf("UpsertEntity should return nil, got %v", err)
	}

	// Verify the entity was stored.
	record, err := store.GetEntityRecord(ctx, "postgresql")
	if err != nil {
		t.Fatalf("GetEntityRecord: %v", err)
	}
	if record == nil {
		t.Fatal("entity record should not be nil after UpsertEntity")
	}
	if record.Name != "PostgreSQL" {
		t.Errorf("entity name = %q, want %q", record.Name, "PostgreSQL")
	}

	// LinkEngramToEntity should link a real engram.
	if err := adapter.LinkEngramToEntity(ctx, ULID(id), "PostgreSQL"); err != nil {
		t.Errorf("LinkEngramToEntity should return nil, got %v", err)
	}

	// UpsertRelationship should store a relationship record.
	if err := adapter.UpsertRelationship(ctx, ULID(id), ExtractedRelation{FromEntity: "payment-service", ToEntity: "PostgreSQL", RelType: "uses", Weight: 0.9}); err != nil {
		t.Errorf("UpsertRelationship should return nil, got %v", err)
	}

	// UpdateDigest should update engram fields.
	result := &EnrichmentResult{
		Summary:    "PostgreSQL is used for payments",
		KeyPoints:  []string{"database", "payments"},
		MemoryType: "decision",
	}
	if err := adapter.UpdateDigest(ctx, ULID(id), result); err != nil {
		t.Errorf("UpdateDigest should return nil, got %v", err)
	}

	// Verify digest was updated.
	eng, err := store.GetEngram(ctx, ws, id)
	if err != nil {
		t.Fatalf("GetEngram: %v", err)
	}
	if eng.Summary != "PostgreSQL is used for payments" {
		t.Errorf("engram Summary = %q, want %q", eng.Summary, "PostgreSQL is used for payments")
	}
}

func TestStoreAdapter_LinkEngramToEntity_NotFound(t *testing.T) {
	store := openTestStore(t)
	adapter := &pluginStoreAdapter{store: store}
	ctx := context.Background()

	// Use a random ULID that doesn't exist in the store.
	var missingID ULID
	err := adapter.LinkEngramToEntity(ctx, missingID, "SomeEntity")
	if err == nil {
		t.Error("LinkEngramToEntity should return error for unknown engram ID")
	}
}

func TestStoreAdapter_UpsertRelationship_NotFound(t *testing.T) {
	store := openTestStore(t)
	adapter := &pluginStoreAdapter{store: store}
	ctx := context.Background()

	var missingID ULID
	err := adapter.UpsertRelationship(ctx, missingID, ExtractedRelation{FromEntity: "a", ToEntity: "b"})
	if err == nil {
		t.Error("UpsertRelationship should return error for unknown engram ID")
	}
}

func TestNewStoreAdapter(t *testing.T) {
	adapter := NewStoreAdapter(nil, nil)
	if adapter == nil {
		t.Fatal("NewStoreAdapter returned nil")
	}
}

func TestStoreAdapter_ScanWithoutFlagNilStore(t *testing.T) {
	// Verify the interface compliance.
	var _ PluginStore = &pluginStoreAdapter{}
}
