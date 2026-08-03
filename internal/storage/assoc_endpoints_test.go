package storage

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/storage/keys"
)

// TestSTO12_SoftDeletedAndArchivedEndpointsKeepTheirEdges is the data-loss
// guard on the guard.
//
// SoftDelete batch.Sets the 0x01 key with StateSoftDeleted, and archiving is
// also a state (StateArchived) on a present record — a soft-deleted or
// archived engram IS present in 0x01 and is restorable. If the liveness
// predicate were lifecycle-state-aware rather than existence-based, this fix
// would silently destroy the edges of every restorable engram, which is a far
// worse outcome than the leak it closes.
func TestSTO12_SoftDeletedAndArchivedEndpointsKeepTheirEdges(t *testing.T) {
	for _, tc := range []struct {
		name  string
		apply func(t *testing.T, ps *PebbleStore, ws [8]byte, id ULID)
	}{
		{"soft-deleted", func(t *testing.T, ps *PebbleStore, ws [8]byte, id ULID) {
			if err := ps.SoftDelete(context.Background(), ws, id); err != nil {
				t.Fatalf("SoftDelete: %v", err)
			}
		}},
		{"archived", func(t *testing.T, ps *PebbleStore, ws [8]byte, id ULID) {
			eng, err := ps.GetEngram(context.Background(), ws, id)
			if err != nil {
				t.Fatalf("GetEngram: %v", err)
			}
			eng.State = StateArchived
			if _, err := ps.WriteEngram(context.Background(), ws, eng); err != nil {
				t.Fatalf("WriteEngram(archived): %v", err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ps := newTestStore(t)
			ctx := context.Background()
			ws := ps.VaultPrefix("sto31-" + tc.name)
			src, dst := NewULID(), NewULID()
			seedEndpoints(t, ps, ws, src, dst)

			tc.apply(t, ps, ws, dst)

			if !ps.engramExists(ws, [16]byte(dst)) {
				t.Fatalf("%s engram must still be present in 0x01 — it is restorable", tc.name)
			}
			if err := ps.WriteAssociation(ctx, ws, src, dst, &Association{
				TargetID: dst, RelType: RelSupports, Weight: 0.4, Confidence: 1, CreatedAt: time.Now(),
			}); err != nil {
				t.Fatalf("WriteAssociation to a %s endpoint must be allowed, got %v", tc.name, err)
			}
			w, err := ps.GetAssocWeight(ctx, ws, src, dst)
			if err != nil || w == 0 {
				t.Fatalf("edge to a %s endpoint was not written: w=%v err=%v", tc.name, w, err)
			}
		})
	}
}

// TestSTO12_WriteAssociationRefusesDeadEndpoints pins the primitive's refusal
// in both directions.
func TestSTO12_WriteAssociationRefusesDeadEndpoints(t *testing.T) {
	ps := newTestStore(t)
	ctx := context.Background()
	ws := ps.VaultPrefix("sto31-refuse")

	live, dead := NewULID(), NewULID()
	seedEndpoints(t, ps, ws, live)

	for _, tc := range []struct {
		name     string
		src, dst ULID
	}{
		{"dead target", live, dead},
		{"dead source", dead, live},
		{"both dead", dead, NewULID()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ps.WriteAssociation(ctx, ws, tc.src, tc.dst, &Association{
				TargetID: tc.dst, RelType: RelRelatesTo, Weight: 0.3, Confidence: 1, CreatedAt: time.Now(),
			})
			if !errors.Is(err, ErrDanglingEndpoint) {
				t.Fatalf("want ErrDanglingEndpoint, got %v", err)
			}
			// And nothing was written: a refused write must be a no-op, not a
			// partial one.
			if _, closer, gerr := ps.db.Get(keys.AssocWeightIndexKey(ws, [16]byte(tc.src), [16]byte(tc.dst))); gerr == nil {
				_ = closer.Close()
				t.Fatal("refused write still left a 0x14 weight-index row")
			}
		})
	}
}

// TestSTO12_UpdateAssocWeightBatchRefusesDeadEndpoints pins the second creator.
//
// #809 made a read FAILURE skip the pair, but read ABSENCE is a normal fact
// that returns a nil error and a zero tuple — so before this fix a Hebbian
// flush naming two ULIDs with no engram and no edge fell through both reads
// and Set a brand-new 0x03/0x04/0x14 row, returning nil.
func TestSTO12_UpdateAssocWeightBatchRefusesDeadEndpoints(t *testing.T) {
	ps := newTestStore(t)
	ctx := context.Background()
	ws := ps.VaultPrefix("sto31-hebbian")
	src, dst := NewULID(), NewULID()

	err := ps.UpdateAssocWeightBatch(ctx, []AssocWeightUpdate{
		{WS: ws, Src: [16]byte(src), Dst: [16]byte(dst), Weight: 0.42, CountDelta: 1},
	})
	var skip *AssocBatchSkipError
	if !errors.As(err, &skip) {
		t.Fatalf("want *AssocBatchSkipError, got %v", err)
	}
	if skip.Applied != 0 || len(skip.Skipped) != 1 {
		t.Fatalf("want 1 skipped / 0 applied, got %d skipped / %d applied", len(skip.Skipped), skip.Applied)
	}
	if !errors.Is(err, ErrDanglingEndpoint) {
		t.Fatalf("skip cause should name the dangling endpoint, got %v", err)
	}
	if w, _ := ps.GetAssocWeight(ctx, ws, src, dst); w != 0 {
		t.Fatalf("an edge was created between two ULIDs with no engram record: w=%v", w)
	}
}

// TestSTO12_BatchWriteAssociationAcceptsSameBatchEngram pins the exception the
// batch variant must make. RememberChild and Evolve queue WriteEngram and
// WriteAssociation into the SAME uncommitted batch so they land atomically —
// the engram's 0x01 key is not readable from Pebble until Commit, so a
// DB-read-only guard would reject every atomic child-append and every evolve.
func TestSTO12_BatchWriteAssociationAcceptsSameBatchEngram(t *testing.T) {
	ps := newTestStore(t)
	ctx := context.Background()
	ws := ps.VaultPrefix("sto31-batch")

	parent := NewULID()
	seedEndpoints(t, ps, ws, parent)

	child := NewULID()
	batch := ps.NewBatch()
	defer batch.Discard()
	if err := batch.WriteEngram(ctx, ws, &Engram{ID: child, Concept: "child", Content: "child content"}); err != nil {
		t.Fatalf("batch WriteEngram: %v", err)
	}
	if err := batch.WriteAssociation(ctx, ws, child, parent, &Association{
		TargetID: parent, RelType: RelIsPartOf, Weight: 1, Confidence: 1, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("batch WriteAssociation for an engram queued in the same batch must succeed, got %v", err)
	}

	// A target that exists nowhere is still refused.
	if err := batch.WriteAssociation(ctx, ws, child, NewULID(), &Association{
		TargetID: NewULID(), RelType: RelRelatesTo, Weight: 0.3, Confidence: 1, CreatedAt: time.Now(),
	}); !errors.Is(err, ErrDanglingEndpoint) {
		t.Fatalf("want ErrDanglingEndpoint for a target in neither the batch nor the DB, got %v", err)
	}

	if err := batch.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if w, _ := ps.GetAssocWeight(ctx, ws, child, parent); w == 0 {
		t.Fatal("the atomic child->parent edge was not committed")
	}
}

// TestSTO12_DeleteEngramCascadesArchivedEdges pins the 0x25 cascade in both
// directions. Before this fix DeleteEngram scanned 0x03 and 0x04 only.
//
// THIS TEST IS THE ONLY PIN ON THAT CASCADE. Do not assume the engine-level
// table covers it: ablating BOTH PrefixIterator loops in DeleteEngram leaves
// ./internal/engine entirely green, including
// TestSTO12_NoDanglingAssociationEdges' "archived edge whose target is
// hard-deleted" case — because RestoreArchivedEdges reaps the unrestorable 0x25
// row per target on the way past, so the end-to-end census comes back clean
// even with the cascade removed. The layer is pinned; the table case is not its
// pin. Same masking shape as the FTS cascade, which is why
// TestSTO12_HardDeletePathsCleanTheSearchIndexes exists separately.
func TestSTO12_DeleteEngramCascadesArchivedEdges(t *testing.T) {
	ps := newTestStore(t)
	ctx := context.Background()
	ws := ps.VaultPrefix("sto31-archive-cascade")

	victim, other := NewULID(), NewULID()
	seedEndpoints(t, ps, ws, victim, other)

	now := time.Now()
	lastAct := int32(now.Add(-24 * time.Hour).Unix())
	av := encodeArchiveValue(RelSupports, 0.9, now.Add(-72*time.Hour), lastAct, 0.9, 10, 0)
	// victim as archive SOURCE and as archive TARGET.
	if err := ps.db.Set(keys.ArchiveAssocKey(ws, [16]byte(victim), [16]byte(other)), av[:], nil); err != nil {
		t.Fatal(err)
	}
	if err := ps.db.Set(keys.ArchiveAssocKey(ws, [16]byte(other), [16]byte(victim)), av[:], nil); err != nil {
		t.Fatal(err)
	}

	if err := ps.DeleteEngram(ctx, ws, victim); err != nil {
		t.Fatalf("DeleteEngram: %v", err)
	}

	for _, k := range [][]byte{
		keys.ArchiveAssocKey(ws, [16]byte(victim), [16]byte(other)),
		keys.ArchiveAssocKey(ws, [16]byte(other), [16]byte(victim)),
	} {
		if _, closer, err := ps.db.Get(k); err == nil {
			_ = closer.Close()
			t.Errorf("0x25 archive row survived the hard delete of one of its endpoints: %x", k)
		}
	}
}

// TestSTO12_DeleteEngramInvalidatesTheAssocCacheOfItsSources closes the one
// member of the class STO-12's row-level wording does not cover.
//
// The invariant is about ROWS, and DeleteEngram's cascade removes every row —
// but GetAssociations serves a 2s-TTL in-memory cache keyed by source engram,
// and DeleteEngram never touched it. So for up to two seconds after a hard
// delete the SERVED graph still names the dead engram: traversal walks to an ID
// whose 0x01 is gone, wasting the hop. It cannot leak content (0x01 is gone, so
// the ID can never materialise into an engram) and it self-heals on expiry,
// which is why this is a wasted-work bug and not a correctness one — but the
// reverse pass already has every source ID in hand, so the invalidation is free.
//
// Deterministic by construction: the cache is warmed by an explicit read, not
// by waiting for a worker.
func TestSTO12_DeleteEngramInvalidatesTheAssocCacheOfItsSources(t *testing.T) {
	ps := newTestStore(t)
	ctx := context.Background()
	ws := ps.VaultPrefix("sto12-assoccache")

	src, victim := NewULID(), NewULID()
	seedEndpoints(t, ps, ws, src, victim)
	if err := ps.WriteAssociation(ctx, ws, src, victim, &Association{
		TargetID: victim, RelType: RelSupports, Weight: 0.6, Confidence: 1, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("WriteAssociation: %v", err)
	}

	// Warm the cache for src.
	got, err := ps.GetAssociations(ctx, ws, []ULID{src}, 10)
	if err != nil {
		t.Fatalf("GetAssociations (warm): %v", err)
	}
	if len(got[src]) != 1 {
		t.Fatalf("precondition: src should have 1 association, got %d", len(got[src]))
	}

	if err := ps.DeleteEngram(ctx, ws, victim); err != nil {
		t.Fatalf("DeleteEngram: %v", err)
	}

	got, err = ps.GetAssociations(ctx, ws, []ULID{src}, 10)
	if err != nil {
		t.Fatalf("GetAssociations (post-delete): %v", err)
	}
	for _, a := range got[src] {
		if a.TargetID == victim {
			t.Fatal("the association cache still serves an edge to a hard-deleted engram")
		}
	}
}

// TestSTO12_RestoreArchivedEdgesDoesNotResurrectDeadTargets pins the third
// creator. A restore stamps restoredAt, and GCArchivedEdges requires
// restoredAt == 0 — so a restored-then-re-archived dangling edge would be
// PERMANENTLY exempt from collection.
func TestSTO12_RestoreArchivedEdgesDoesNotResurrectDeadTargets(t *testing.T) {
	ps := newTestStore(t)
	ctx := context.Background()
	ws := ps.VaultPrefix("sto31-restore")

	src, deadDst := NewULID(), NewULID()
	seedEndpoints(t, ps, ws, src)

	now := time.Now()
	av := encodeArchiveValue(RelSupports, 0.9, now.Add(-72*time.Hour),
		int32(now.Add(-24*time.Hour).Unix()), 0.9, 10, 0)
	if err := ps.db.Set(keys.ArchiveAssocKey(ws, [16]byte(src), [16]byte(deadDst)), av[:], nil); err != nil {
		t.Fatal(err)
	}
	ps.archiveBloom.Add([16]byte(src))

	restored, err := ps.RestoreArchivedEdges(ctx, ws, [16]byte(src), 10)
	if err != nil {
		t.Fatalf("RestoreArchivedEdges: %v", err)
	}
	if len(restored) != 0 {
		t.Fatalf("restored %d edge(s) to an engram that does not exist", len(restored))
	}
	if w, _ := ps.GetAssocWeight(ctx, ws, src, deadDst); w != 0 {
		t.Fatalf("a dangling live edge was written by the restore path: w=%v", w)
	}
	// The unrecoverable archive row is reaped, not left to be rescanned on
	// every future recall.
	if _, closer, gerr := ps.db.Get(keys.ArchiveAssocKey(ws, [16]byte(src), [16]byte(deadDst))); gerr == nil {
		_ = closer.Close()
		t.Error("archive row whose target can never exist again was left in place")
	}
}
