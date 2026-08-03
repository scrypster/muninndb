package engine

import (
	"context"
	"testing"

	"github.com/scrypster/muninndb/internal/storage"
	"github.com/stretchr/testify/require"
)

// Entity records (0x1F) must be vault-scoped (#683). Before the fix the record
// was keyed 0x1F|nameHash with no workspace prefix, so every vault that ever
// mentioned an entity of that name shared one record: one mention_count summed
// across tenants, and a lookup from a vault with no links to the entity still
// returned the other tenant's metadata.
//
// Both tests below are RED on the pre-#683 keyspace.

// TestEntityRecord_NoCrossVaultPhantom is the tenancy half: an entity that
// exists ONLY in vault B must be invisible from vault A. Pre-fix,
// GetEntityAggregate returned B's record (name, type, confidence, first_seen,
// mention_count) with an empty engram list — an existence oracle over another
// tenant's entity vocabulary.
func TestEntityRecord_NoCrossVaultPhantom(t *testing.T) {
	t.Parallel()
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	const vaultA = "scope-tenant-a"
	const vaultB = "scope-tenant-b"
	const entity = "Thornwick Ledger"

	wsB := eng.store.ResolveVaultPrefix(vaultB)

	// Vault B alone knows this entity.
	idB, err := eng.store.WriteEngram(ctx, wsB, &storage.Engram{
		Concept: "b-only", Content: "content that mentions the ledger",
	})
	require.NoError(t, err)
	require.NoError(t, eng.store.UpsertEntityRecord(ctx, wsB, storage.EntityRecord{
		Name: entity, Type: "system", Confidence: 0.91, Source: "inline",
	}, "inline"))
	require.NoError(t, eng.store.WriteEntityEngramLink(ctx, wsB, idB, entity))

	// Vault A must not see it at all.
	aggA, err := eng.GetEntityAggregate(ctx, vaultA, entity, 10)
	require.NoError(t, err)
	require.Nil(t, aggA, "vault A leaked vault B's entity record: %+v", aggA)

	// Vault B still sees its own.
	aggB, err := eng.GetEntityAggregate(ctx, vaultB, entity, 10)
	require.NoError(t, err)
	require.NotNil(t, aggB)
	require.Equal(t, entity, aggB.Record.Name)
	require.Len(t, aggB.Engrams, 1)
}

// TestEntityRecord_MentionCountIsPerVault is the correctness half: two vaults
// that mention the same entity name keep independent mention counts. Pre-fix a
// single record accumulated both vaults' upserts, so vault A reported vault
// B's mentions as its own.
func TestEntityRecord_MentionCountIsPerVault(t *testing.T) {
	t.Parallel()
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	const vaultA = "count-tenant-a"
	const vaultB = "count-tenant-b"
	const entity = "Marlowe Index"

	wsA := eng.store.ResolveVaultPrefix(vaultA)
	wsB := eng.store.ResolveVaultPrefix(vaultB)

	// Vault A: one mention.
	idA, err := eng.store.WriteEngram(ctx, wsA, &storage.Engram{Concept: "a1", Content: "a one"})
	require.NoError(t, err)
	require.NoError(t, eng.store.UpsertEntityRecord(ctx, wsA, storage.EntityRecord{
		Name: entity, Type: "concept", Source: "inline",
	}, "inline"))
	require.NoError(t, eng.store.WriteEntityEngramLink(ctx, wsA, idA, entity))

	// Vault B: three mentions.
	for _, concept := range []string{"b1", "b2", "b3"} {
		idB, err := eng.store.WriteEngram(ctx, wsB, &storage.Engram{Concept: concept, Content: "b " + concept})
		require.NoError(t, err)
		require.NoError(t, eng.store.UpsertEntityRecord(ctx, wsB, storage.EntityRecord{
			Name: entity, Type: "concept", Source: "inline",
		}, "inline"))
		require.NoError(t, eng.store.WriteEntityEngramLink(ctx, wsB, idB, entity))
	}

	listA, err := eng.ListEntities(ctx, vaultA, 50, "")
	require.NoError(t, err)
	require.Len(t, listA, 1)
	require.EqualValues(t, 1, listA[0].MentionCount,
		"vault A's mention_count absorbed vault B's mentions")

	listB, err := eng.ListEntities(ctx, vaultB, 50, "")
	require.NoError(t, err)
	require.Len(t, listB, 1)
	require.EqualValues(t, 3, listB[0].MentionCount,
		"vault B's mention_count absorbed vault A's mentions")
}

// TestSetEntityState_CannotReachAnotherVault is the write half of #683.
// SetEntityState took no vault and wrote the global 0x1F record, so an agent
// authenticated for vault A could deprecate — or mark merged — an entity that
// exists only in vault B, and B's next read would see it.
func TestSetEntityState_CannotReachAnotherVault(t *testing.T) {
	t.Parallel()
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	const vaultA = "state-tenant-a"
	const vaultB = "state-tenant-b"
	const entity = "Ashgrove Relay"

	wsB := eng.store.ResolveVaultPrefix(vaultB)
	idB, err := eng.store.WriteEngram(ctx, wsB, &storage.Engram{Concept: "b", Content: "relay content"})
	require.NoError(t, err)
	require.NoError(t, eng.store.UpsertEntityRecord(ctx, wsB, storage.EntityRecord{
		Name: entity, Type: "system", Confidence: 1, State: "active",
	}, "inline"))
	require.NoError(t, eng.store.WriteEntityEngramLink(ctx, wsB, idB, entity))

	// Vault A must not be able to touch it — it does not exist there.
	err = eng.SetEntityState(ctx, vaultA, entity, "deprecated", "", "")
	require.Error(t, err, "vault A wrote lifecycle state onto an entity it cannot see")

	recB, err := eng.store.GetEntityRecord(ctx, wsB, entity)
	require.NoError(t, err)
	require.NotNil(t, recB)
	require.Equal(t, "active", recB.State, "vault A's write reached vault B's entity record")
}
