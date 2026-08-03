package storage

import (
	"context"
	"errors"
	"sort"
	"testing"

	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// ScanVaultEntityNames
// ---------------------------------------------------------------------------

// TestScanVaultEntityNames_Empty verifies that scanning an empty vault returns
// no entity names (the callback is never invoked).
func TestScanVaultEntityNames_Empty(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("entity-test")
	ws = store.VaultPrefix("scan-entity-names-empty")

	var names []string
	err := store.ScanVaultEntityNames(ctx, ws, func(name string) error {
		names = append(names, name)
		return nil
	})
	require.NoError(t, err)
	require.Empty(t, names, "expected no entity names in an empty vault")
}

// TestScanVaultEntityNames_MultipleEntities writes 3 entity-engram links and
// verifies that ScanVaultEntityNames returns all 3 distinct entity names.
func TestScanVaultEntityNames_MultipleEntities(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("entity-test")
	ws = store.VaultPrefix("scan-entity-names-multi")

	entityNames := []string{"PostgreSQL", "Redis", "Kafka"}

	// Write entity records and link them to distinct engrams in the vault.
	for _, name := range entityNames {
		require.NoError(t, store.UpsertEntityRecord(ctx, ws, EntityRecord{
			Name: name, Type: "technology", Confidence: 0.8,
		}, "test"))
		engramID := NewULID()
		require.NoError(t, store.WriteEntityEngramLink(ctx, ws, engramID, name))
	}

	var got []string
	err := store.ScanVaultEntityNames(ctx, ws, func(name string) error {
		got = append(got, name)
		return nil
	})
	require.NoError(t, err)

	sort.Strings(got)
	want := make([]string, len(entityNames))
	copy(want, entityNames)
	sort.Strings(want)

	require.Equal(t, want, got, "ScanVaultEntityNames must return all distinct entity names")
}

// TestScanVaultEntityNames_DeduplicatesAcrossEngrams verifies that if the same
// entity name is linked to multiple engrams, ScanVaultEntityNames still returns
// the name exactly once.
func TestScanVaultEntityNames_DeduplicatesAcrossEngrams(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("entity-test")
	ws = store.VaultPrefix("scan-entity-dedup")

	require.NoError(t, store.UpsertEntityRecord(ctx, ws, EntityRecord{
		Name: "Go", Type: "technology", Confidence: 0.9,
	}, "test"))

	// Link the same entity to 3 different engrams.
	for i := 0; i < 3; i++ {
		require.NoError(t, store.WriteEntityEngramLink(ctx, ws, NewULID(), "Go"))
	}

	var got []string
	err := store.ScanVaultEntityNames(ctx, ws, func(name string) error {
		got = append(got, name)
		return nil
	})
	require.NoError(t, err)
	require.Equal(t, []string{"Go"}, got, "duplicate entity links must be deduplicated by ScanVaultEntityNames")
}

// TestScanVaultEntityNames_DeduplicatesCaseVariants verifies that entity names
// differing only by case (or whitespace/NFKC) collapse to a single identity: they
// share one 0x1F record, so the scan must invoke fn exactly once. Guards the
// muninn_entities duplicate-row / similar_entities case-pair regression.
func TestScanVaultEntityNames_DeduplicatesCaseVariants(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("entity-test")
	ws = store.VaultPrefix("scan-entity-case-variants")

	require.NoError(t, store.UpsertEntityRecord(ctx, ws, EntityRecord{
		Name: "MuninnDB", Type: "database", Confidence: 1,
	}, "test"))

	// Two engrams link the same entity under different casings.
	require.NoError(t, store.WriteEntityEngramLink(ctx, ws, NewULID(), "MuninnDB"))
	require.NoError(t, store.WriteEntityEngramLink(ctx, ws, NewULID(), "muninndb"))

	var got []string
	require.NoError(t, store.ScanVaultEntityNames(ctx, ws, func(name string) error {
		got = append(got, name)
		return nil
	}))
	require.Len(t, got, 1, "case-variant entity links must collapse to one identity")
}

// TestScanVaultEntityNames_CallbackError verifies that ScanVaultEntityNames
// stops immediately and propagates the error returned by the callback.
func TestScanVaultEntityNames_CallbackError(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("entity-test")
	ws = store.VaultPrefix("scan-entity-names-err")

	for _, name := range []string{"A", "B", "C"} {
		require.NoError(t, store.UpsertEntityRecord(ctx, ws, EntityRecord{
			Name: name, Type: "t", Confidence: 0.5,
		}, "test"))
		require.NoError(t, store.WriteEntityEngramLink(ctx, ws, NewULID(), name))
	}

	sentinel := errors.New("stop scanning")
	callCount := 0
	err := store.ScanVaultEntityNames(ctx, ws, func(name string) error {
		callCount++
		return sentinel
	})
	require.ErrorIs(t, err, sentinel)
	require.Equal(t, 1, callCount, "callback must be invoked exactly once before error propagates")
}

// ---------------------------------------------------------------------------
// ScanEngramEntities
// ---------------------------------------------------------------------------

// TestScanEngramEntities_Empty verifies that scanning an engram that has no
// linked entities returns an empty result (callback never invoked).
func TestScanEngramEntities_Empty(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("entity-test")
	ws = store.VaultPrefix("scan-engram-entities-empty")

	engramID := NewULID() // never linked to any entity

	var names []string
	err := store.ScanEngramEntities(ctx, ws, engramID, func(entityName string) error {
		names = append(names, entityName)
		return nil
	})
	require.NoError(t, err)
	require.Empty(t, names, "expected no entities for an engram with no links")
}

// TestScanEngramEntities_LinkedEntities writes entity records, links them to a
// specific engram, then verifies ScanEngramEntities returns exactly those names.
func TestScanEngramEntities_LinkedEntities(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("entity-test")
	ws = store.VaultPrefix("scan-engram-entities-linked")

	engramID := NewULID()
	entityNames := []string{"payment-service", "PostgreSQL", "Redis"}

	for _, name := range entityNames {
		require.NoError(t, store.UpsertEntityRecord(ctx, ws, EntityRecord{
			Name: name, Type: "service", Confidence: 0.7,
		}, "test"))
		require.NoError(t, store.WriteEntityEngramLink(ctx, ws, engramID, name))
	}

	// Link a different entity to a different engram — must not appear in results.
	otherEngramID := NewULID()
	require.NoError(t, store.UpsertEntityRecord(ctx, ws, EntityRecord{
		Name: "unrelated", Type: "other", Confidence: 0.5,
	}, "test"))
	require.NoError(t, store.WriteEntityEngramLink(ctx, ws, otherEngramID, "unrelated"))

	var got []string
	err := store.ScanEngramEntities(ctx, ws, engramID, func(entityName string) error {
		got = append(got, entityName)
		return nil
	})
	require.NoError(t, err)

	sort.Strings(got)
	want := make([]string, len(entityNames))
	copy(want, entityNames)
	sort.Strings(want)

	require.Equal(t, want, got, "ScanEngramEntities must return exactly the linked entity names")
}

// TestScanEngramEntities_CallbackError verifies that ScanEngramEntities stops
// and propagates the error returned by the callback.
func TestScanEngramEntities_CallbackError(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("entity-test")
	ws = store.VaultPrefix("scan-engram-entities-err")

	engramID := NewULID()
	for _, name := range []string{"X", "Y", "Z"} {
		require.NoError(t, store.UpsertEntityRecord(ctx, ws, EntityRecord{
			Name: name, Type: "t", Confidence: 0.5,
		}, "test"))
		require.NoError(t, store.WriteEntityEngramLink(ctx, ws, engramID, name))
	}

	sentinel := errors.New("stop scanning entities")
	callCount := 0
	err := store.ScanEngramEntities(ctx, ws, engramID, func(entityName string) error {
		callCount++
		return sentinel
	})
	require.ErrorIs(t, err, sentinel)
	require.Equal(t, 1, callCount, "callback must be invoked exactly once before error propagates")
}

// ---------------------------------------------------------------------------
// ScanRelationships
// ---------------------------------------------------------------------------

// TestScanRelationships_Empty verifies that scanning a vault with no relationship
// records never invokes the callback and returns no error.
func TestScanRelationships_Empty(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("entity-test")
	ws = store.VaultPrefix("scan-rels-empty")

	called := false
	err := store.ScanRelationships(ctx, ws, func(r RelationshipRecord) error {
		called = true
		return nil
	})
	require.NoError(t, err)
	require.False(t, called, "callback must not be invoked when there are no relationship records")
}

// TestScanRelationships_WrittenRelationships writes several relationship records
// and verifies they are returned by ScanRelationships, including at least one
// co_occurs_with entry.
func TestScanRelationships_WrittenRelationships(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("entity-test")
	ws = store.VaultPrefix("scan-rels-written")

	engramID := NewULID()

	records := []RelationshipRecord{
		{FromEntity: "payment-service", ToEntity: "PostgreSQL", RelType: "uses", Weight: 0.9, Source: "test"},
		{FromEntity: "payment-service", ToEntity: "Redis", RelType: "co_occurs_with", Weight: 0.7, Source: "test"},
		{FromEntity: "auth-service", ToEntity: "Redis", RelType: "depends_on", Weight: 0.8, Source: "test"},
	}

	for _, rec := range records {
		require.NoError(t, store.UpsertRelationshipRecord(ctx, ws, engramID, rec))
	}

	var got []RelationshipRecord
	err := store.ScanRelationships(ctx, ws, func(r RelationshipRecord) error {
		got = append(got, r)
		return nil
	})
	require.NoError(t, err)
	require.Len(t, got, len(records), "ScanRelationships must return all written relationship records")

	// Verify that the co_occurs_with entry is present.
	var hasCoOccurs bool
	for _, r := range got {
		if r.RelType == "co_occurs_with" {
			hasCoOccurs = true
			break
		}
	}
	require.True(t, hasCoOccurs, "expected at least one co_occurs_with relationship in scan results")
}

// TestScanRelationships_IsolatedByVault verifies that ScanRelationships only
// returns records for the given vault workspace and not those from another vault.
func TestScanRelationships_IsolatedByVault(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	ws1 := store.VaultPrefix("scan-rels-vault1")
	ws2 := store.VaultPrefix("scan-rels-vault2")

	engramID := NewULID()

	require.NoError(t, store.UpsertRelationshipRecord(ctx, ws1, engramID, RelationshipRecord{
		FromEntity: "A", ToEntity: "B", RelType: "uses", Weight: 0.8, Source: "vault1",
	}))
	require.NoError(t, store.UpsertRelationshipRecord(ctx, ws2, engramID, RelationshipRecord{
		FromEntity: "C", ToEntity: "D", RelType: "uses", Weight: 0.6, Source: "vault2",
	}))

	var ws1Records []RelationshipRecord
	err := store.ScanRelationships(ctx, ws1, func(r RelationshipRecord) error {
		ws1Records = append(ws1Records, r)
		return nil
	})
	require.NoError(t, err)
	require.Len(t, ws1Records, 1, "ScanRelationships must only return records for the specified vault")
	require.Equal(t, "vault1", ws1Records[0].Source)
}

// TestScanRelationships_CallbackError verifies that ScanRelationships stops
// iteration and propagates the error returned by the callback.
func TestScanRelationships_CallbackError(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("entity-test")
	ws = store.VaultPrefix("scan-rels-err")

	engramID := NewULID()
	for i := 0; i < 3; i++ {
		require.NoError(t, store.UpsertRelationshipRecord(ctx, ws, engramID, RelationshipRecord{
			FromEntity: "from", ToEntity: "to", RelType: "uses", Weight: float32(i) * 0.1, Source: "test",
		}))
	}

	sentinel := errors.New("stop scanning relationships")
	callCount := 0
	err := store.ScanRelationships(ctx, ws, func(r RelationshipRecord) error {
		callCount++
		return sentinel
	})
	require.ErrorIs(t, err, sentinel)
	require.Equal(t, 1, callCount, "callback must be invoked exactly once before error propagates")
}
