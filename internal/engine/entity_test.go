package engine

import (
	"context"
	"testing"

	"github.com/scrypster/muninndb/internal/plugin"
	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/transport/mbp"
	"github.com/stretchr/testify/require"
)

func TestInlineEntities_StoredInEntityTable(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	resp, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:   "default",
		Content: "PostgreSQL is used for the payments service",
		Concept: "payments db choice",
		Entities: []mbp.InlineEntity{
			{Name: "PostgreSQL", Type: "database"},
		},
	})
	require.NoError(t, err)
	require.NotEmpty(t, resp.ID)

	// Entity should be in the entity table (lookup via normalized name).
	record, err := eng.store.GetEntityRecord(ctx, "postgresql")
	require.NoError(t, err)
	require.NotNil(t, record, "inline entity should be stored in entity table")
	require.Equal(t, "PostgreSQL", record.Name)
	require.Equal(t, "database", record.Type)
	require.Equal(t, "inline", record.Source)

	// DigestEntities flag should be set on the engram.
	id, parseErr := storage.ParseULID(resp.ID)
	require.NoError(t, parseErr)
	flags, err := eng.store.GetDigestFlags(ctx, plugin.ULID(id))
	require.NoError(t, err)
	require.NotZero(t, flags&plugin.DigestEntities, "DigestEntities flag should be set for caller-provided entities")
}

func TestInlineEntities_KeyPointsDoNotContainEntityNames(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	resp, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:   "default",
		Content: "Redis is used as a cache layer",
		Concept: "caching strategy",
		Entities: []mbp.InlineEntity{
			{Name: "Redis", Type: "cache"},
		},
	})
	require.NoError(t, err)

	// Read back the engram and verify KeyPoints does NOT contain the entity hack format.
	id, parseErr := storage.ParseULID(resp.ID)
	require.NoError(t, parseErr)
	wsPrefix := eng.store.ResolveVaultPrefix("default")
	engram, err := eng.store.GetEngram(ctx, wsPrefix, id)
	require.NoError(t, err)

	for _, kp := range engram.KeyPoints {
		require.NotContains(t, kp, "Redis (cache)", "KeyPoints should not contain entity names in old hack format")
	}
}

func TestInlineEntities_MultipleEntities(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	_, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:   "default",
		Content: "The payment service uses PostgreSQL and Redis",
		Concept: "payment service architecture",
		Entities: []mbp.InlineEntity{
			{Name: "PostgreSQL", Type: "database"},
			{Name: "Redis", Type: "cache"},
			{Name: "payment-service", Type: "service"},
		},
	})
	require.NoError(t, err)

	// All entities should be stored.
	for _, name := range []string{"postgresql", "redis", "payment-service"} {
		record, err := eng.store.GetEntityRecord(ctx, name)
		require.NoError(t, err)
		require.NotNilf(t, record, "entity %q should be stored in entity table", name)
	}
}
