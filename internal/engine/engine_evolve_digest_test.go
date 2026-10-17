package engine

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cockroachdb/pebble"
	"github.com/stretchr/testify/require"

	"github.com/scrypster/muninndb/internal/plugin"
	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// digestFlags reads the digest flags for id. An engram with no flag key yet
// reads as 0, as the retroactive processor treats it.
func digestFlags(t *testing.T, eng *Engine, id storage.ULID) uint16 {
	t.Helper()
	flags, err := eng.store.GetDigestFlags(context.Background(), plugin.ULID(id))
	if errors.Is(err, pebble.ErrNotFound) {
		return 0
	}
	require.NoError(t, err)
	return flags
}

// TestEvolve_InheritedTypeFlagsClassification is the core #902 regression
// test: the successor inherits MemoryType and TypeLabel, so its classification
// stage is complete and must be flagged. Fails on the pre-fix Evolve, whose
// successor GetEnrichmentCandidates reported as missing classification forever.
func TestEvolve_InheritedTypeFlagsClassification(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	resp, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault: "test", Concept: "db choice", Content: "PostgreSQL runs payments",
		MemoryType: uint8(storage.TypeDecision), TypeLabel: "decision",
	})
	require.NoError(t, err)

	newID, err := eng.Evolve(ctx, "test", resp.ID, "PostgreSQL 17 runs payments now", "version bump", nil, "")
	require.NoError(t, err)

	ws := eng.store.ResolveVaultPrefix("test")
	succ, err := eng.store.GetEngram(ctx, ws, newID)
	require.NoError(t, err)
	require.Equal(t, "decision", succ.TypeLabel, "precondition: the successor inherits the type")
	require.NotZero(t, digestFlags(t, eng, newID)&plugin.DigestClassified,
		"an inherited type is a completed classification stage")
}

// TestEvolve_CarriedRelationshipsFlagRelationships: caller-supplied
// relationship records are carried into the successor (#622), so its
// relationships stage is complete and must be flagged. Fails on the pre-fix
// Evolve.
func TestEvolve_CarriedRelationshipsFlagRelationships(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	resp, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault: "test", Concept: "cache", Content: "Redis caches PostgreSQL reads",
		Entities: []mbp.InlineEntity{
			{Name: "Redis", Type: "database"},
			{Name: "PostgreSQL", Type: "database"},
		},
		EntityRelationships: []mbp.InlineEntityRelationship{
			{FromEntity: "Redis", ToEntity: "PostgreSQL", RelType: "caches", Weight: 0.9},
		},
	})
	require.NoError(t, err)

	newID, err := eng.Evolve(ctx, "test", resp.ID, "Redis 8 caches PostgreSQL reads", "version bump", nil, "")
	require.NoError(t, err)

	ws := eng.store.ResolveVaultPrefix("test")
	carriedCaches := false
	for _, rec := range engramRelationships(t, eng, ws, newID) {
		if rec.RelType == "caches" {
			carriedCaches = true
		}
	}
	require.True(t, carriedCaches, "precondition: the caller's relationship is carried")
	require.NotZero(t, digestFlags(t, eng, newID)&plugin.DigestRelationships,
		"carried relationship records are a completed relationships stage")
}

// TestEvolve_CoOccurrenceAloneFlagsNoRelationships: Write records co_occurs_with
// for any two inline entities without completing the relationships stage, and
// Evolve carries those records. They are engine-generated side effects, so the
// successor's relationships stage must stay open for enrichment. A guard on the
// fix: it passes on the pre-fix Evolve too, which flagged nothing.
func TestEvolve_CoOccurrenceAloneFlagsNoRelationships(t *testing.T) {
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

	newID, err := eng.Evolve(ctx, "test", resp.ID, "Ada pairs with Grace on Fridays", "detail", nil, "")
	require.NoError(t, err)

	ws := eng.store.ResolveVaultPrefix("test")
	recs := engramRelationships(t, eng, ws, newID)
	require.NotEmpty(t, recs, "precondition: the co-occurrence record is carried")
	for _, rec := range recs {
		require.Equal(t, "co_occurs_with", rec.RelType, "precondition: only co-occurrence is carried")
	}
	require.Zero(t, digestFlags(t, eng, newID)&plugin.DigestRelationships,
		"co-occurrence alone completes no relationships stage")
}

// TestEvolve_InlineEntitiesLeaveRelationshipsStage: inline entities replace
// the carry, so no relationship records are carried and the relationships
// stage stays open for enrichment over the new set, while the inherited type
// still flags classification. The classification assertion fails on the
// pre-fix Evolve; the relationships one guards the fix.
func TestEvolve_InlineEntitiesLeaveRelationshipsStage(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	resp, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault: "test", Concept: "owner", Content: "Alice owns the deploy and reports to Carol",
		MemoryType: uint8(storage.TypeFact), TypeLabel: "fact",
		Entities: []mbp.InlineEntity{
			{Name: "Alice", Type: "person"},
			{Name: "Carol", Type: "person"},
		},
		EntityRelationships: []mbp.InlineEntityRelationship{
			{FromEntity: "Alice", ToEntity: "Carol", RelType: "reports_to", Weight: 0.9},
		},
	})
	require.NoError(t, err)

	newID, err := eng.EvolveAt(ctx, "test", resp.ID, "Bob owns the deploy now", "handover", nil, "",
		[]mbp.InlineEntity{{Name: "Bob", Type: "person"}}, nil, time.Time{})
	require.NoError(t, err)

	flags := digestFlags(t, eng, newID)
	require.Zero(t, flags&plugin.DigestRelationships,
		"replaced entities leave the relationships stage for enrichment")
	require.NotZero(t, flags&plugin.DigestClassified, "the inherited type still flags classification")
}

// TestEvolve_UntypedWithoutRelationshipsFlagsNothing is the control: a
// predecessor with no type and no relationship records hands the successor
// nothing to flag, so both stages stay open for enrichment. Guards against
// flagging a stage whose data is absent.
func TestEvolve_UntypedWithoutRelationshipsFlagsNothing(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	resp, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault: "test", Concept: "raw", Content: "an untyped note with no entities",
	})
	require.NoError(t, err)

	newID, err := eng.Evolve(ctx, "test", resp.ID, "an untyped note, revised", "edit", nil, "")
	require.NoError(t, err)

	ws := eng.store.ResolveVaultPrefix("test")
	succ, err := eng.store.GetEngram(ctx, ws, newID)
	require.NoError(t, err)
	require.Empty(t, succ.TypeLabel, "precondition: the successor has no type")
	require.Empty(t, engramRelationships(t, eng, ws, newID), "precondition: nothing is carried")
	flags := digestFlags(t, eng, newID)
	require.Zero(t, flags&plugin.DigestClassified, "no type means no classification flag")
	require.Zero(t, flags&plugin.DigestRelationships, "no relationships means no relationships flag")
}
