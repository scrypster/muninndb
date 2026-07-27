package engine

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/scrypster/muninndb/internal/plugin"
	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// engramEntities returns the entity names linked to id via the 0x20 forward index.
func engramEntities(t *testing.T, eng *Engine, ws [8]byte, id storage.ULID) []string {
	t.Helper()
	var names []string
	err := eng.store.ScanEngramEntities(context.Background(), ws, id, func(name string) error {
		names = append(names, name)
		return nil
	})
	require.NoError(t, err)
	return names
}

// engramRelationships returns the 0x21 relationship records stored for id.
func engramRelationships(t *testing.T, eng *Engine, ws [8]byte, id storage.ULID) []storage.RelationshipRecord {
	t.Helper()
	var recs []storage.RelationshipRecord
	err := eng.store.ScanEngramRelationships(context.Background(), ws, id, func(rec storage.RelationshipRecord) error {
		recs = append(recs, rec)
		return nil
	})
	require.NoError(t, err)
	return recs
}

// coOccurrenceCount reads the 0x24 pair count for two entities, 0 if the key
// is absent.
func coOccurrenceCount(t *testing.T, eng *Engine, ws [8]byte, a, b string) int {
	t.Helper()
	count := 0
	err := eng.store.ScanEntityClusters(context.Background(), ws, 1, func(nameA, nameB string, c int) error {
		if (nameA == a && nameB == b) || (nameA == b && nameB == a) {
			count = c
		}
		return nil
	})
	require.NoError(t, err)
	return count
}

// TestEvolve_CarriesEntityLinks is the core #622 regression test: the
// supersede-successor must inherit the predecessor's entity links so it stays
// visible to FindByEntity. Fails on the pre-fix Evolve (successor had zero
// links).
func TestEvolve_CarriesEntityLinks(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	resp, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault: "test", Concept: "db choice", Content: "PostgreSQL runs payments",
		Entities: []mbp.InlineEntity{
			{Name: "PostgreSQL", Type: "product"},
			{Name: "payments", Type: "concept"},
		},
	})
	require.NoError(t, err)

	newID, err := eng.Evolve(ctx, "test", resp.ID, "PostgreSQL 17 runs payments now", "version bump", nil, "")
	require.NoError(t, err)

	ws := eng.store.ResolveVaultPrefix("test")
	names := engramEntities(t, eng, ws, newID)
	require.ElementsMatch(t, []string{"PostgreSQL", "payments"}, names,
		"successor must carry the predecessor's entity links")

	// The successor must be reachable through the 0x23 reverse index too.
	found := false
	err = eng.store.ScanEntityEngrams(ctx, "PostgreSQL", func(gotWS [8]byte, gotID storage.ULID) error {
		if gotWS == ws && gotID == newID {
			found = true
		}
		return nil
	})
	require.NoError(t, err)
	require.True(t, found, "successor must appear in the entity reverse index")

	// DigestEntities must be set so enrichment does not re-extract over the carry.
	flags, err := eng.store.GetDigestFlags(ctx, plugin.ULID(newID))
	require.NoError(t, err)
	require.NotZero(t, flags&plugin.DigestEntities)
}

// TestEvolve_MentionCountConservation pins the funding rule for the carried
// links. The ledger is one increment per link key created, one decrement per
// link key destroyed: DeleteEngram decrements unconditionally for every link
// it removes, so a carried link must be funded at carry time or the pruner
// cashes in the difference — hard-delete the predecessor (the recommended
// evolve-then-retention path) and MentionCount under-reads while the successor
// is still linked, and the co-occurrence pair key is destroyed at zero,
// removing the pair from ScanEntityClusters while both entities are still
// mentioned together.
//
// Conservation: write gives 1, evolve with carry gives 2, hard-deleting the
// predecessor gives 1 — and the pair key must survive the hard delete.
func TestEvolve_MentionCountConservation(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	resp, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault: "test", Concept: "pairing", Content: "Ada pairs with Grace",
		Entities: []mbp.InlineEntity{
			{Name: "Ada", Type: "person"},
			{Name: "Grace", Type: "person"},
		},
	})
	require.NoError(t, err)
	ws := eng.store.ResolveVaultPrefix("test")
	oldULID, err := storage.ParseULID(resp.ID)
	require.NoError(t, err)

	rec, err := eng.store.GetEntityRecord(ctx, "ada")
	require.NoError(t, err)
	require.NotNil(t, rec)
	require.Equal(t, int32(1), rec.MentionCount, "write funds one mention")
	require.Equal(t, 1, coOccurrenceCount(t, eng, ws, "Ada", "Grace"),
		"write funds one co-occurrence")

	newID, err := eng.Evolve(ctx, "test", resp.ID, "Ada still pairs with Grace", "refresh", nil, "")
	require.NoError(t, err)

	rec, err = eng.store.GetEntityRecord(ctx, "ada")
	require.NoError(t, err)
	require.NotNil(t, rec)
	require.Equal(t, int32(2), rec.MentionCount,
		"carry creates a second link key and must fund it")
	require.Equal(t, 2, coOccurrenceCount(t, eng, ws, "Ada", "Grace"),
		"carry creates a second pair mention and must fund it")

	// Hard-delete the predecessor — what retention pruning does to evolve
	// predecessors. Its decrement must be matched by the carry's increment.
	require.NoError(t, eng.store.DeleteEngram(ctx, ws, oldULID))

	rec, err = eng.store.GetEntityRecord(ctx, "ada")
	require.NoError(t, err)
	require.NotNil(t, rec, "entity record must survive predecessor pruning")
	require.Equal(t, int32(1), rec.MentionCount,
		"after pruning the predecessor, the count reflects the successor's link")
	require.Equal(t, 1, coOccurrenceCount(t, eng, ws, "Ada", "Grace"),
		"pair key must survive predecessor pruning — dropping to zero would remove the pair from clustering while the successor still mentions both")

	// The successor's links themselves are untouched by the predecessor delete.
	require.ElementsMatch(t, []string{"Ada", "Grace"}, engramEntities(t, eng, ws, newID))
}

// TestEvolve_CarriesRelationshipRecords: the predecessor's 0x21 relationship
// records travel with the links.
func TestEvolve_CarriesRelationshipRecords(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	resp, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault: "test", Concept: "pair", Content: "A talks to B",
		Entities: []mbp.InlineEntity{{Name: "A", Type: "concept"}, {Name: "B", Type: "concept"}},
	})
	require.NoError(t, err)

	ws := eng.store.ResolveVaultPrefix("test")
	oldULID, err := storage.ParseULID(resp.ID)
	require.NoError(t, err)
	require.NotEmpty(t, engramRelationships(t, eng, ws, oldULID),
		"precondition: inline entity pair produces a co_occurs_with record")

	newID, err := eng.Evolve(ctx, "test", resp.ID, "A still talks to B", "refresh", nil, "")
	require.NoError(t, err)

	recs := engramRelationships(t, eng, ws, newID)
	require.Len(t, recs, 1)
	require.Equal(t, "co_occurs_with", recs[0].RelType)
}
