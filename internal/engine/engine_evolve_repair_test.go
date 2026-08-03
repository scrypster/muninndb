package engine

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/scrypster/muninndb/internal/engine/activation"
	"github.com/scrypster/muninndb/internal/engine/trigger"
	"github.com/scrypster/muninndb/internal/index/fts"
	"github.com/scrypster/muninndb/internal/plugin"
	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// simulateStrippedEvolve reproduces what the pre-fix Evolve left on disk:
// successor engram + supersedes association, predecessor soft-deleted,
// successor with zero entity links.
func simulateStrippedEvolve(t *testing.T, eng *Engine, ws [8]byte, oldULID storage.ULID, content string) storage.ULID {
	t.Helper()
	ctx := context.Background()
	newULID := storage.NewULID()
	now := time.Now()
	batch := eng.store.NewBatch()
	defer batch.Discard()
	require.NoError(t, batch.WriteEngram(ctx, ws, &storage.Engram{
		ID: newULID, Concept: "stripped", Content: content,
		Confidence: 1.0, Stability: 30.0, State: storage.StateActive,
		CreatedAt: now, UpdatedAt: now, LastAccess: now,
	}))
	require.NoError(t, batch.WriteAssociation(ctx, ws, newULID, oldULID, &storage.Association{
		TargetID: oldULID, RelType: storage.RelSupersedes, Weight: 1.0,
		Confidence: 1.0, CreatedAt: now, LastActivated: int32(now.Unix()),
	}))
	require.NoError(t, batch.UpdateEngramState(ctx, ws, oldULID, storage.StateSoftDeleted))
	require.NoError(t, batch.Commit())
	return newULID
}

// TestEvolveEntityLinkRepair_HealsStrippedSuccessor: the repair pass copies
// links forward onto damage created by the pre-fix Evolve, atomically per
// successor, and funds the mention-count and co-occurrence ledgers exactly as
// the write-path carry does.
func TestEvolveEntityLinkRepair_HealsStrippedSuccessor(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	resp, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault: "test", Concept: "victim", Content: "Carl planted rosemary",
		Entities: []mbp.InlineEntity{
			{Name: "Carl", Type: "person"},
			{Name: "rosemary", Type: "concept"},
		},
	})
	require.NoError(t, err)
	ws := eng.store.ResolveVaultPrefix("test")
	oldULID, err := storage.ParseULID(resp.ID)
	require.NoError(t, err)

	succID := simulateStrippedEvolve(t, eng, ws, oldULID, "Carl planted rosemary (evolved)")
	require.Empty(t, engramEntities(t, eng, ws, succID), "precondition: successor is stripped")

	repaired, links, err := eng.repairEvolveEntityLinksOnce(ws)
	require.NoError(t, err)
	require.Equal(t, 1, repaired)
	require.Equal(t, 2, links)

	require.ElementsMatch(t, []string{"Carl", "rosemary"}, engramEntities(t, eng, ws, succID))
	require.NotEmpty(t, engramRelationships(t, eng, ws, succID),
		"relationship records must be copied forward too")

	flags, err := eng.store.GetDigestFlags(ctx, plugin.ULID(succID))
	require.NoError(t, err)
	require.NotZero(t, flags&plugin.DigestEntities)

	// The repaired links are funded: write gave 1, the repair's carry gives 2,
	// and hard-deleting the predecessor must return 1 with the pair intact —
	// the same conservation the write-path carry maintains.
	rec, err := eng.store.GetEntityRecord(ctx, ws, "carl")
	require.NoError(t, err)
	require.NotNil(t, rec)
	require.Equal(t, int32(2), rec.MentionCount, "repair must fund the carried link")
	require.Equal(t, 2, coOccurrenceCount(t, eng, ws, "Carl", "rosemary"))

	require.NoError(t, eng.store.DeleteEngram(ctx, ws, oldULID))
	rec, err = eng.store.GetEntityRecord(ctx, ws, "carl")
	require.NoError(t, err)
	require.NotNil(t, rec)
	require.Equal(t, int32(1), rec.MentionCount)
	require.Equal(t, 1, coOccurrenceCount(t, eng, ws, "Carl", "rosemary"),
		"pair key must survive predecessor pruning after a repair-funded carry")

	// Idempotent: a second round finds nothing to do.
	repaired, _, err = eng.repairEvolveEntityLinksOnce(ws)
	require.NoError(t, err)
	require.Zero(t, repaired)
}

// TestEvolveEntityLinkRepair_HealsChain: A→B→C where both hops were stripped.
// Each round carries links one hop further; the fixpoint loop heals the chain.
func TestEvolveEntityLinkRepair_HealsChain(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()

	resp, err := eng.Write(context.Background(), &mbp.WriteRequest{
		Vault: "test", Concept: "root", Content: "timshel",
		Entities: []mbp.InlineEntity{{Name: "timshel", Type: "concept"}},
	})
	require.NoError(t, err)
	ws := eng.store.ResolveVaultPrefix("test")
	aID, err := storage.ParseULID(resp.ID)
	require.NoError(t, err)

	bID := simulateStrippedEvolve(t, eng, ws, aID, "timshel v2")
	// Middle hop is itself superseded and soft-deleted.
	cID := simulateStrippedEvolve(t, eng, ws, bID, "timshel v3")

	for round := 0; round < 4; round++ {
		n, _, err := eng.repairEvolveEntityLinksOnce(ws)
		require.NoError(t, err)
		if n == 0 {
			break
		}
	}

	require.Equal(t, []string{"timshel"}, engramEntities(t, eng, ws, bID))
	require.Equal(t, []string{"timshel"}, engramEntities(t, eng, ws, cID),
		"links must travel the whole chain to the live successor")
}

// repairTestEnv builds a store the test can boot several engines over, so the
// repair can be exercised through the real NewEngine/Stop lifecycle.
func repairTestEnv(t *testing.T) (*storage.PebbleStore, *fts.Index, func()) {
	t.Helper()
	dir, err := os.MkdirTemp("", "muninndb-evolve-repair-test-*")
	require.NoError(t, err)
	db, err := storage.OpenPebble(dir, storage.DefaultOptions())
	if err != nil {
		os.RemoveAll(dir)
		t.Fatal(err)
	}
	store := storage.NewPebbleStore(db, storage.PebbleStoreConfig{CacheSize: 1000})
	return store, fts.New(db), func() {
		store.Close()
		os.RemoveAll(dir)
	}
}

func newRepairEngine(store *storage.PebbleStore, ftsIdx *fts.Index, delay time.Duration) *Engine {
	embedder := &noopEmbedder{}
	return NewEngine(EngineConfig{
		Store: store, FTSIndex: ftsIdx,
		ActivationEngine:  activation.New(store, &ftsAdapter{ftsIdx}, nil, embedder),
		TriggerSystem:     trigger.New(store, &ftsTrigAdapter{ftsIdx}, nil, embedder),
		Embedder:          embedder,
		EvolveRepairDelay: &delay,
	})
}

// TestEvolveEntityLinkRepair_EngineLifecycleAndWatermark drives the repair
// through the real engine lifecycle rather than calling the pass directly —
// deleting the NewEngine spawn line (or breaking the delay/stop plumbing)
// turns this red while the direct-call tests stay green.
//
// Boot 1 has damage on disk and a short repair delay: the pass must heal it
// and watermark the vault. Boot 2 faces NEW damage in the same vault: the
// watermark must make it skip, so the new stripped successor stays stripped.
// That skip is the documented cost of not paying a full soft-deleted scan on
// every boot — new damage cannot occur once the write-path carry is deployed.
func TestEvolveEntityLinkRepair_EngineLifecycleAndWatermark(t *testing.T) {
	store, ftsIdx, cleanup := repairTestEnv(t)
	defer cleanup()
	ctx := context.Background()

	// Seed damage with a long-delay engine whose repair pass never fires.
	seed := newRepairEngine(store, ftsIdx, time.Hour)
	resp, err := seed.Write(ctx, &mbp.WriteRequest{
		Vault: "test", Concept: "victim", Content: "Pi charts the river",
		Entities: []mbp.InlineEntity{{Name: "Pi", Type: "person"}},
	})
	require.NoError(t, err)
	ws := store.ResolveVaultPrefix("test")
	oldULID, err := storage.ParseULID(resp.ID)
	require.NoError(t, err)
	succ1 := simulateStrippedEvolve(t, seed, ws, oldULID, "Pi charts the river (evolved)")
	seed.Stop()

	// Boot 1: the startup pass must heal the damage and mark the vault.
	boot1 := newRepairEngine(store, ftsIdx, time.Millisecond)
	select {
	case <-boot1.evolveRepairDone:
	case <-time.After(10 * time.Second):
		t.Fatal("repair pass did not complete within 10s of engine start")
	}
	require.ElementsMatch(t, []string{"Pi"}, engramEntities(t, boot1, ws, succ1),
		"boot-time repair must heal the stripped successor")
	mark, err := store.GetEvolveRepairMark(ctx, ws)
	require.NoError(t, err)
	require.Equal(t, evolveRepairVersion, mark, "clean pass must watermark the vault")

	// New damage after the watermark.
	resp2, err := boot1.Write(ctx, &mbp.WriteRequest{
		Vault: "test", Concept: "victim2", Content: "Rowan walks the edge",
		Entities: []mbp.InlineEntity{{Name: "Rowan", Type: "person"}},
	})
	require.NoError(t, err)
	old2, err := storage.ParseULID(resp2.ID)
	require.NoError(t, err)
	succ2 := simulateStrippedEvolve(t, boot1, ws, old2, "Rowan walks the edge (evolved)")
	boot1.Stop()

	// Boot 2: the watermark short-circuits the vault; the pass completes
	// without touching the new damage.
	boot2 := newRepairEngine(store, ftsIdx, time.Millisecond)
	select {
	case <-boot2.evolveRepairDone:
	case <-time.After(10 * time.Second):
		t.Fatal("watermarked pass did not complete within 10s of engine start")
	}
	require.Empty(t, engramEntities(t, boot2, ws, succ2),
		"watermarked vault must be skipped — a full re-scan on every boot is what the watermark exists to prevent")
	boot2.Stop()
}

// stripedChainWithDescendingULIDs lays down a pre-fix stripped supersede
// chain whose successor ULIDs sort BELOW their predecessors. The 0x0B state
// scan walks in ULID order, so each repair round can only carry links one hop
// down such a chain — the shape that makes the round cap bind.
func strippedChainWithDescendingULIDs(t *testing.T, eng *Engine, ws [8]byte, hops int) []storage.ULID {
	t.Helper()
	ctx := context.Background()
	now := time.Now()

	ids := make([]storage.ULID, hops+1)
	for i := range ids {
		// Root gets the LARGEST ULID; each successor sorts below its predecessor.
		ids[i] = storage.ULID{0xF0 - byte(i), 0x01, byte(i)}
	}

	require.NoError(t, eng.store.UpsertEntityRecord(ctx, ws, storage.EntityRecord{
		Name: "deepchain", Type: "concept", Source: "inline",
	}, "inline"))

	// Root: has the entity link.
	_, err := eng.store.WriteEngram(ctx, ws, &storage.Engram{
		ID: ids[0], Concept: "root", Content: "deep chain root",
		Confidence: 1.0, Stability: 30.0, State: storage.StateActive,
		CreatedAt: now, UpdatedAt: now, LastAccess: now,
	})
	require.NoError(t, err)
	require.NoError(t, eng.store.WriteEntityEngramLink(ctx, ws, ids[0], "deepchain"))

	// Each hop: successor engram + supersedes association, predecessor
	// soft-deleted, successor stripped — what the pre-fix Evolve left behind.
	for i := 1; i <= hops; i++ {
		batch := eng.store.NewBatch()
		require.NoError(t, batch.WriteEngram(ctx, ws, &storage.Engram{
			ID: ids[i], Concept: "hop", Content: "deep chain hop",
			Confidence: 1.0, Stability: 30.0, State: storage.StateActive,
			CreatedAt: now, UpdatedAt: now, LastAccess: now,
		}))
		require.NoError(t, batch.WriteAssociation(ctx, ws, ids[i], ids[i-1], &storage.Association{
			TargetID: ids[i-1], RelType: storage.RelSupersedes, Weight: 1.0,
			Confidence: 1.0, CreatedAt: now, LastActivated: int32(now.Unix()),
		}))
		require.NoError(t, batch.UpdateEngramState(ctx, ws, ids[i-1], storage.StateSoftDeleted))
		require.NoError(t, batch.Commit())
		batch.Discard()
	}
	return ids
}

// TestEvolveEntityLinkRepair_RoundCapExhaustionIsNotClean pins the F-class
// defect the sweep refactor closed: a vault whose repair exhausts the round
// cap while rounds are still repairing must NOT report clean — a watermark
// written at that point would strand the unrepaired tail forever, on every
// future boot.
func TestEvolveEntityLinkRepair_RoundCapExhaustionIsNotClean(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()

	ws := eng.store.ResolveVaultPrefix("test")
	ids := strippedChainWithDescendingULIDs(t, eng, ws, 40)

	repaired, _, clean := eng.sweepEvolveRepairVault("test", ws)
	require.False(t, clean,
		"cap exhaustion with rounds still repairing must not count as clean")
	require.Equal(t, 32, repaired, "one hop per round up to the cap")
	require.Empty(t, engramEntities(t, eng, ws, ids[40]),
		"precondition of the defect: the tail is still stripped when the cap binds")

	// A later boot's sweep picks up where the cap stopped and converges.
	repaired2, _, clean2 := eng.sweepEvolveRepairVault("test", ws)
	require.True(t, clean2, "the follow-up sweep converges and may watermark")
	require.Equal(t, 8, repaired2)
	require.Equal(t, []string{"deepchain"}, engramEntities(t, eng, ws, ids[40]),
		"the tail heals across boots precisely because the first sweep withheld the watermark")
}

// TestEvolveEntityLinkRepair_SkipsAlreadyDigested pins the digest-set skip: a
// stripped successor that is already marked DigestEntities (as ReplayEnrichment,
// which takes no casLock, leaves it after funding the same successor in the repair
// window) must NOT be re-funded — the digest bit is the funded-marker. The
// successor is deliberately left link-less so the has-links skip does NOT fire and
// only the digest skip can prevent the double-fund.
func TestEvolveEntityLinkRepair_SkipsAlreadyDigested(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	resp, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault: "test", Concept: "victim", Content: "Dana shipped the release",
		Entities: []mbp.InlineEntity{
			{Name: "Dana", Type: "person"},
			{Name: "release", Type: "concept"},
		},
	})
	require.NoError(t, err)
	ws := eng.store.ResolveVaultPrefix("test")
	oldULID, err := storage.ParseULID(resp.ID)
	require.NoError(t, err)

	succID := simulateStrippedEvolve(t, eng, ws, oldULID, "Dana shipped the release (evolved)")
	require.Empty(t, engramEntities(t, eng, ws, succID), "precondition: successor is stripped (no links)")

	// Simulate ReplayEnrichment having already healed + marked this successor.
	require.NoError(t, eng.store.SetDigestFlag(ctx, plugin.ULID(succID), plugin.DigestEntities))

	mentionBefore, err := eng.store.GetEntityRecord(ctx, ws, "dana")
	require.NoError(t, err)
	require.NotNil(t, mentionBefore)

	repaired, _, err := eng.repairEvolveEntityLinksOnce(ws)
	require.NoError(t, err)
	require.Zero(t, repaired, "a digest-marked successor must be skipped, not re-funded")
	require.Empty(t, engramEntities(t, eng, ws, succID), "skip must write no links")

	mentionAfter, err := eng.store.GetEntityRecord(ctx, ws, "dana")
	require.NoError(t, err)
	require.Equal(t, mentionBefore.MentionCount, mentionAfter.MentionCount,
		"skip must not double-fund the mention ledger")
}
