package storage

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// #683: the 0x1F entity record is vault-scoped. Two vaults that mention the
// same entity name own two independent records.

func TestEntityRecord_VaultsDoNotShareARecord(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	wsA := store.VaultPrefix("scope-a")
	wsB := store.VaultPrefix("scope-b")

	require.NoError(t, store.UpsertEntityRecord(ctx, wsA, EntityRecord{
		Name: "Fenwick Array", Type: "system", Confidence: 0.4,
	}, "test"))
	for range 3 {
		require.NoError(t, store.UpsertEntityRecord(ctx, wsB, EntityRecord{
			Name: "Fenwick Array", Type: "concept", Confidence: 0.9,
		}, "test"))
	}

	recA, err := store.GetEntityRecord(ctx, wsA, "Fenwick Array")
	require.NoError(t, err)
	require.NotNil(t, recA)
	require.EqualValues(t, 1, recA.MentionCount, "vault A absorbed vault B's upserts")
	require.Equal(t, "system", recA.Type, "vault B overwrote vault A's type")
	require.InDelta(t, 0.4, recA.Confidence, 0.0001, "vault B's confidence leaked into vault A")

	recB, err := store.GetEntityRecord(ctx, wsB, "Fenwick Array")
	require.NoError(t, err)
	require.NotNil(t, recB)
	require.EqualValues(t, 3, recB.MentionCount)
	require.Equal(t, "concept", recB.Type)
}

func TestEntityRecord_UnknownInAVaultThatNeverSawIt(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	wsA := store.VaultPrefix("oracle-a")
	wsB := store.VaultPrefix("oracle-b")

	require.NoError(t, store.UpsertEntityRecord(ctx, wsB, EntityRecord{
		Name: "Rookmoor Ledger", Type: "system", Confidence: 1,
	}, "test"))

	got, err := store.GetEntityRecord(ctx, wsA, "Rookmoor Ledger")
	require.NoError(t, err)
	require.Nil(t, got, "a vault answered an existence question about another vault's entity")
}

// TestDecrementEntityMentionCount_OrphanCheckIsVaultScoped: the 0x23 reverse
// index is keyed nameHash-then-ws, so an unbounded scan finds every tenant's
// links. If the orphan check used that, a vault's own record could never be
// reclaimed while any other vault still mentioned the name.
func TestDecrementEntityMentionCount_OrphanCheckIsVaultScoped(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	wsA := store.VaultPrefix("orphan-a")
	wsB := store.VaultPrefix("orphan-b")

	require.NoError(t, store.UpsertEntityRecord(ctx, wsA, EntityRecord{
		Name: "Sablewood", Type: "test", Confidence: 1,
	}, "test"))
	// Vault B keeps a live link to the same name. It must not hold vault A's
	// record open.
	require.NoError(t, store.WriteEntityEngramLink(ctx, wsB, NewULID(), "Sablewood"))

	require.NoError(t, store.DecrementEntityMentionCount(ctx, wsA, "Sablewood"))

	got, err := store.GetEntityRecord(ctx, wsA, "Sablewood")
	require.NoError(t, err)
	require.Nil(t, got, "vault A's orphaned record survived because another vault still links the name")
}
