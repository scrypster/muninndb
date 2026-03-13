package engine

import (
	"context"
	"testing"

	"github.com/scrypster/muninndb/internal/transport/mbp"
	"github.com/stretchr/testify/require"
)

// writeEntityForStateTest writes one engram with an inline entity and returns the entity name.
func writeEntityForStateTest(t *testing.T, eng *Engine, vault, name, entityType string) {
	t.Helper()
	_, err := eng.Write(context.Background(), &mbp.WriteRequest{
		Vault:   vault,
		Content: name + " is a thing",
		Concept: "test entity",
		Entities: []mbp.InlineEntity{
			{Name: name, Type: entityType},
		},
	})
	require.NoError(t, err)
}

func TestSetEntityState_PreservesTypeWhenNotProvided(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	writeEntityForStateTest(t, eng, "default", "Modbus", "protocol")

	// Change state only — entityType param is empty.
	err := eng.SetEntityState(ctx, "Modbus", "deprecated", "", "")
	require.NoError(t, err)

	rec, err := eng.store.GetEntityRecord(ctx, "modbus")
	require.NoError(t, err)
	require.NotNil(t, rec)
	require.Equal(t, "deprecated", rec.State, "state should be updated")
	require.Equal(t, "protocol", rec.Type, "type should be preserved when not provided")
}

func TestSetEntityState_UpdatesTypeWhenProvided(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	// Entity created with wrong type from early enrichment.
	writeEntityForStateTest(t, eng, "default", "2014/53/EU", "other")

	// Correct the type while keeping state active.
	err := eng.SetEntityState(ctx, "2014/53/EU", "active", "", "directive")
	require.NoError(t, err)

	rec, err := eng.store.GetEntityRecord(ctx, "2014/53/eu")
	require.NoError(t, err)
	require.NotNil(t, rec)
	require.Equal(t, "active", rec.State)
	require.Equal(t, "directive", rec.Type, "type should be updated to the provided value")
}

func TestSetEntityState_UpdatesBothStateAndType(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	writeEntityForStateTest(t, eng, "default", "Bridge Dongle", "other")

	err := eng.SetEntityState(ctx, "Bridge Dongle", "deprecated", "", "module")
	require.NoError(t, err)

	rec, err := eng.store.GetEntityRecord(ctx, "bridge dongle")
	require.NoError(t, err)
	require.NotNil(t, rec)
	require.Equal(t, "deprecated", rec.State)
	require.Equal(t, "module", rec.Type)
}

func TestSetEntityState_NotFound(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	err := eng.SetEntityState(ctx, "ghost entity", "deprecated", "", "")
	require.Error(t, err, "should error for entity that does not exist")
}

func TestSetEntityStateBatch_AllSucceed(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	for _, e := range []struct{ name, typ string }{
		{"Modbus", "other"},
		{"2014/53/EU", "other"},
		{"Bridge Dongle", "other"},
	} {
		writeEntityForStateTest(t, eng, "default", e.name, e.typ)
	}

	ops := []EntityStateOp{
		{EntityName: "Modbus", State: "deprecated"},
		{EntityName: "2014/53/EU", State: "active", EntityType: "directive"},
		{EntityName: "Bridge Dongle", State: "deprecated", EntityType: "module"},
	}
	errs := eng.SetEntityStateBatch(ctx, ops)
	require.Len(t, errs, 3)
	for i, err := range errs {
		require.NoError(t, err, "op %d should succeed", i)
	}

	rec, _ := eng.store.GetEntityRecord(ctx, "2014/53/eu")
	require.Equal(t, "directive", rec.Type)
	require.Equal(t, "active", rec.State)
}

func TestSetEntityStateBatch_PartialFailure(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	writeEntityForStateTest(t, eng, "default", "Modbus", "protocol")
	writeEntityForStateTest(t, eng, "default", "Bridge Dongle", "module")

	ops := []EntityStateOp{
		{EntityName: "Modbus", State: "deprecated"},
		{EntityName: "ghost entity that does not exist", State: "deprecated"},
		{EntityName: "Bridge Dongle", State: "deprecated"},
	}
	errs := eng.SetEntityStateBatch(ctx, ops)
	require.Len(t, errs, 3)
	require.NoError(t, errs[0], "first op should succeed")
	require.Error(t, errs[1], "middle op should fail (entity not found)")
	require.NoError(t, errs[2], "third op should succeed despite middle failure")
}

func TestSetEntityStateBatch_ContextCancellation(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()

	writeEntityForStateTest(t, eng, "default", "Modbus", "protocol")
	writeEntityForStateTest(t, eng, "default", "Bridge Dongle", "module")

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately — all ops should get context.Canceled

	ops := []EntityStateOp{
		{EntityName: "Modbus", State: "deprecated"},
		{EntityName: "Bridge Dongle", State: "deprecated"},
	}
	errs := eng.SetEntityStateBatch(ctx, ops)
	require.Len(t, errs, 2)
	for i, err := range errs {
		require.ErrorIs(t, err, context.Canceled, "op %d should get context.Canceled", i)
	}
}

func TestSetEntityStateBatch_WithTypeCorrection(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	writeEntityForStateTest(t, eng, "default", "EN 62368-1", "other")

	ops := []EntityStateOp{
		{EntityName: "EN 62368-1", State: "active", EntityType: "standard"},
	}
	errs := eng.SetEntityStateBatch(ctx, ops)
	require.NoError(t, errs[0])

	rec, err := eng.store.GetEntityRecord(ctx, "en 62368-1")
	require.NoError(t, err)
	require.Equal(t, "standard", rec.Type)
	require.Equal(t, "active", rec.State)
}
