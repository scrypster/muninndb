package storage

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestUpsertEntityRecord_RoundTrip(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	record := EntityRecord{
		Name:       "PostgreSQL",
		Type:       "database",
		Confidence: 0.95,
	}
	err := store.UpsertEntityRecord(ctx, record, "inline:test")
	require.NoError(t, err)

	// Normalized lookup (lowercase)
	got, err := store.GetEntityRecord(ctx, "postgresql")
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, "PostgreSQL", got.Name)
	require.Equal(t, "database", got.Type)
	require.Equal(t, "inline:test", got.Source)

	// Different case resolves to same record
	got2, err := store.GetEntityRecord(ctx, "POSTGRESQL")
	require.NoError(t, err)
	require.NotNil(t, got2)
	require.Equal(t, got.Name, got2.Name)
}

func TestGetEntityRecord_NotFound(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	got, err := store.GetEntityRecord(ctx, "nonexistent-entity")
	require.NoError(t, err)
	require.Nil(t, got)
}

func TestWriteEntityEngramLink(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	ws := store.VaultPrefix("entity-link-vault")

	// Write an entity first
	err := store.UpsertEntityRecord(ctx, EntityRecord{Name: "PostgreSQL", Type: "database", Confidence: 0.9}, "test")
	require.NoError(t, err)

	engramID := NewULID()

	// Write the link — should succeed without error
	err = store.WriteEntityEngramLink(ctx, ws, engramID, "PostgreSQL")
	require.NoError(t, err)
}

func TestUpsertRelationshipRecord(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	ws := store.VaultPrefix("relationship-vault")
	engramID := NewULID()

	record := RelationshipRecord{
		FromEntity: "payment-service",
		ToEntity:   "PostgreSQL",
		RelType:    "uses",
		Weight:     0.9,
		Source:     "plugin:enrich",
	}

	err := store.UpsertRelationshipRecord(ctx, ws, engramID, record)
	require.NoError(t, err)
}

func TestUpsertEntityRecord_UpdatePreservesName(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	// Write initial record
	err := store.UpsertEntityRecord(ctx, EntityRecord{
		Name:       "Redis",
		Type:       "cache",
		Confidence: 0.7,
	}, "inline:test")
	require.NoError(t, err)

	// Overwrite with higher confidence
	err = store.UpsertEntityRecord(ctx, EntityRecord{
		Name:       "Redis",
		Type:       "database",
		Confidence: 0.95,
	}, "plugin:enrich")
	require.NoError(t, err)

	got, err := store.GetEntityRecord(ctx, "redis")
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, "Redis", got.Name)
	require.Equal(t, "database", got.Type)
	require.Equal(t, "plugin:enrich", got.Source)
	require.InDelta(t, 0.95, got.Confidence, 0.001)
}
