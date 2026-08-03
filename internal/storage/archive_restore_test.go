package storage

import (
	"context"
	"testing"
	"time"

	"github.com/cockroachdb/pebble"
	"github.com/scrypster/muninndb/internal/storage/keys"
)

func TestRestoreArchivedEdges_RestoresTopN(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("restore-test")

	src := NewULID()
	dst1 := NewULID()
	dst2 := NewULID()
	seedEndpoints(t, store, ws, src, dst1, dst2)

	// Write two archive entries directly.
	// dst1: high consolidation score (peakWeight=0.9, coAct=10, lastAct=1 day ago)
	// dst2: lower score (peakWeight=0.5, coAct=2, lastAct=30 days ago)
	now := time.Now()
	writeArchive := func(dst ULID, peak float32, coAct uint32, daysAgo int) {
		key := keys.ArchiveAssocKey(ws, [16]byte(src), [16]byte(dst))
		val := encodeArchiveValue(RelSupports, 0.9, now.Add(-24*time.Hour), int32(now.Add(-time.Duration(daysAgo)*24*time.Hour).Unix()), peak, coAct, 0)
		_ = store.db.Set(key, val[:], pebble.Sync)
	}
	writeArchive(dst1, 0.9, 10, 1) // score = 0.9*10/1 = 9.0
	writeArchive(dst2, 0.5, 2, 30) // score = 0.5*2/30 = 0.033

	restored, err := store.RestoreArchivedEdges(ctx, ws, [16]byte(src), 10)
	if err != nil {
		t.Fatalf("RestoreArchivedEdges: %v", err)
	}
	if len(restored) != 2 {
		t.Errorf("expected 2 restored edges, got %d", len(restored))
	}

	// Both should now exist in live index (0x14 via GetAssocWeight).
	w1, err1 := store.GetAssocWeight(ctx, ws, src, dst1)
	w2, err2 := store.GetAssocWeight(ctx, ws, src, dst2)
	if err1 != nil || w1 <= 0 {
		t.Errorf("dst1 not restored to live index: w=%v err=%v", w1, err1)
	}
	if err2 != nil || w2 <= 0 {
		t.Errorf("dst2 not restored to live index: w=%v err=%v", w2, err2)
	}

	// Restore weight: peakWeight * 0.25
	if w1 < 0.20 || w1 > 0.25 { // 0.9 * 0.25 = 0.225
		t.Errorf("dst1 restore weight: got %v, want ~0.225", w1)
	}

	// Archive key should be gone.
	archKey := keys.ArchiveAssocKey(ws, [16]byte(src), [16]byte(dst1))
	_, closer, getErr := store.db.Get(archKey)
	if getErr == nil {
		closer.Close()
		t.Error("archive key should be deleted after restore")
	}
}

func TestRestoreArchivedEdges_RestoresTopByConsolidation(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("restore-consolidation")

	src := NewULID()
	dst1 := NewULID()
	dst2 := NewULID()
	seedEndpoints(t, store, ws, src, dst1, dst2)

	now := time.Now()
	// dst1: high consolidation score (peakWeight=0.9, coAct=10, lastAct=1 day ago)
	// dst2: lower score (peakWeight=0.5, coAct=2, lastAct=30 days ago)
	writeArchive := func(dst ULID, peak float32, coAct uint32, daysAgo int) {
		key := keys.ArchiveAssocKey(ws, [16]byte(src), [16]byte(dst))
		val := encodeArchiveValue(RelSupports, 0.9, now.Add(-24*time.Hour), int32(now.Add(-time.Duration(daysAgo)*24*time.Hour).Unix()), peak, coAct, 0)
		_ = store.db.Set(key, val[:], pebble.Sync)
	}
	writeArchive(dst1, 0.9, 10, 1) // score = 0.9*10/1 = 9.0
	writeArchive(dst2, 0.5, 2, 30) // score = 0.5*2/30 = 0.033

	restored, err := store.RestoreArchivedEdges(ctx, ws, [16]byte(src), 10)
	if err != nil {
		t.Fatalf("RestoreArchivedEdges: %v", err)
	}
	if len(restored) != 2 {
		t.Fatalf("expected 2 restored edges, got %d", len(restored))
	}

	// Verify restore weight is correct (peakWeight * 0.25).
	w1, err1 := store.GetAssocWeight(ctx, ws, src, dst1)
	if err1 != nil || w1 <= 0 {
		t.Errorf("dst1 not restored to live index: w=%v err=%v", w1, err1)
	}
	if w1 < 0.20 || w1 > 0.25 { // 0.9 * 0.25 = 0.225
		t.Errorf("dst1 restore weight: got %v, want ~0.225", w1)
	}

	// Verify restoredAt is stamped on the live write.
	_, _, _, _, _, _, restoredAt1, err := store.getAssocValueFull(ctx, ws, src, dst1)
	if err != nil {
		t.Fatalf("getAssocValueFull: %v", err)
	}
	if restoredAt1 == 0 {
		t.Error("restored edge should have restoredAt stamped")
	}
}

func TestRestoreArchivedEdges_Transitive(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("restore-transitive")

	src := NewULID()
	neighbor := NewULID()
	deepNeighbor := NewULID()
	seedEndpoints(t, store, ws, src, neighbor, deepNeighbor)

	now := time.Now()
	lastAct := int32(now.Add(-24 * time.Hour).Unix())

	// Archive src -> neighbor
	arc1 := encodeArchiveValue(RelSupports, 0.9, now.Add(-72*time.Hour), lastAct, 0.9, 10, 0)
	store.db.Set(keys.ArchiveAssocKey(ws, [16]byte(src), [16]byte(neighbor)), arc1[:], nil)

	// Archive neighbor -> deepNeighbor
	arc2 := encodeArchiveValue(RelRelatesTo, 0.7, now.Add(-72*time.Hour), lastAct, 0.7, 5, 0)
	store.db.Set(keys.ArchiveAssocKey(ws, [16]byte(neighbor), [16]byte(deepNeighbor)), arc2[:], nil)

	store.archiveBloom.Add(src)
	store.archiveBloom.Add(neighbor)

	restored, err := store.RestoreArchivedEdgesTransitive(ctx, ws, src, 10, 5)
	if err != nil {
		t.Fatalf("RestoreArchivedEdgesTransitive: %v", err)
	}

	// src->neighbor should be restored.
	w1, _ := store.GetAssocWeight(ctx, ws, src, neighbor)
	if w1 == 0 {
		t.Error("src->neighbor should be restored")
	}

	// neighbor->deepNeighbor should also be restored (transitive).
	w2, _ := store.GetAssocWeight(ctx, ws, neighbor, deepNeighbor)
	if w2 == 0 {
		t.Error("neighbor->deepNeighbor should be restored (transitive)")
	}

	if len(restored) != 2 {
		t.Errorf("expected 2 restored ULIDs (neighbor + deepNeighbor), got %d", len(restored))
	}
}

// --- #806: archived associations restorable from either endpoint ---
//
// Before the fix, DecayAssocWeights archived an edge only under ws|src|dst,
// so RestoreArchivedEdges (called per recall CANDIDATE by
// phase4_75ArchiveRestore) could only ever find it when recall happened to
// touch the endpoint that was its Src. Since the Hebbian worker's
// canonicalPair always makes the OLDER engram Src, that meant the NEWER of a
// co-activated pair — exactly what a recent session is most likely to touch
// — could never trigger its own edge's restoration.

// archiveViaDecay writes a live association and runs it through
// DecayAssocWeights with parameters chosen to push it into the 0x25 archive
// (consolidation score above threshold, same shape as
// TestDecayAssocWeights_ArchivesStrongEdge), so the mirror-write behavior
// under test is exercised through the real production code path rather than
// hand-encoded fixture bytes.
func archiveViaDecay(t *testing.T, store *PebbleStore, ctx context.Context, ws [8]byte, src, dst ULID, relType RelType) {
	t.Helper()
	if err := store.WriteAssociation(ctx, ws, src, dst, &Association{
		TargetID:      dst,
		Weight:        0.8,
		RelType:       relType,
		LastActivated: int32(time.Now().Add(-48 * time.Hour).Unix()),
	}); err != nil {
		t.Fatalf("WriteAssociation: %v", err)
	}
	if _, err := store.DecayAssocWeights(ctx, ws, 24*time.Hour, 0.3, 0.05); err != nil {
		t.Fatalf("DecayAssocWeights: %v", err)
	}
	// Precondition: the edge actually left the live index.
	if w, _ := store.GetAssocWeight(ctx, ws, src, dst); w > 0 {
		t.Fatalf("precondition failed: edge is still live (w=%v), not archived", w)
	}
}

func archiveRowExists(t *testing.T, store *PebbleStore, ws [8]byte, a, b ULID) bool {
	t.Helper()
	_, closer, err := store.db.Get(keys.ArchiveAssocKey(ws, [16]byte(a), [16]byte(b)))
	if err != nil {
		return false
	}
	closer.Close()
	return true
}

// TestArchiveRestore_SymmetricEdgeMirroredOnArchive is the write-side half:
// a RelCoActivated edge (RelType.IsSymmetric()) must land under BOTH
// ws|src|dst and ws|dst|src the moment it is archived, with identical
// values, so a later scan from either endpoint's own prefix finds it.
func TestArchiveRestore_SymmetricEdgeMirroredOnArchive(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("archive-mirror-write")

	src, dst := NewULID(), NewULID()
	seedEndpoints(t, store, ws, src, dst)

	archiveViaDecay(t, store, ctx, ws, src, dst, RelCoActivated)

	if !archiveRowExists(t, store, ws, src, dst) {
		t.Error("primary archive row ws|src|dst missing")
	}
	if !archiveRowExists(t, store, ws, dst, src) {
		t.Error("mirror archive row ws|dst|src missing for a symmetric relation")
	}

	primaryVal, closer1, err := store.db.Get(keys.ArchiveAssocKey(ws, [16]byte(src), [16]byte(dst)))
	if err != nil {
		t.Fatalf("read primary: %v", err)
	}
	primaryCopy := append([]byte{}, primaryVal...)
	closer1.Close()
	mirrorVal, closer2, err := store.db.Get(keys.ArchiveAssocKey(ws, [16]byte(dst), [16]byte(src)))
	if err != nil {
		t.Fatalf("read mirror: %v", err)
	}
	defer closer2.Close()
	if string(primaryCopy) != string(mirrorVal) {
		t.Errorf("mirror value diverges from primary: primary=%x mirror=%x", primaryCopy, mirrorVal)
	}
}

// TestArchiveRestore_SymmetricEdgeRestorableFromEitherEndpoint is the
// end-to-end round trip #806 was filed over: archive a symmetric edge, then
// restore from the DESTINATION side (the side a recent session actually
// touches for a Hebbian pair) and confirm it comes back.
func TestArchiveRestore_SymmetricEdgeRestorableFromEitherEndpoint(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("archive-restore-either-end")

	src, dst := NewULID(), NewULID()
	seedEndpoints(t, store, ws, src, dst)

	archiveViaDecay(t, store, ctx, ws, src, dst, RelRelatesTo)

	// Restore triggered from the DESTINATION's own prefix — the case that
	// was silently impossible before #806.
	restored, err := store.RestoreArchivedEdges(ctx, ws, [16]byte(dst), 10)
	if err != nil {
		t.Fatalf("RestoreArchivedEdges from destination: %v", err)
	}
	if len(restored) != 1 {
		t.Fatalf("expected 1 edge restored from the destination side, got %d", len(restored))
	}

	// RestoreArchivedEdges always writes the live pair in (callerSrcID, c.dst)
	// order — here (dst, src), because the restore was triggered from dst's
	// own prefix. GetAssocWeight's 0x14 lookup is a raw, direction-sensitive
	// point read (unlike ranking, which unions 0x03/0x04 for symmetric
	// types), so the pair must be queried in the order it was actually
	// written; querying it (src, dst) is exactly the wrong-direction lookup
	// this is not testing.
	w, err := store.GetAssocWeight(ctx, ws, dst, src)
	if err != nil || w <= 0 {
		t.Errorf("edge not restored to live index after destination-side restore: w=%v err=%v", w, err)
	}
}

// TestArchiveRestore_DirectionalEdgeNotMirrored: a directional relation
// (RelSupports is not in RelType.IsSymmetric()'s set) keeps the pre-#806
// behavior — restorable only from its Src. Mirroring it would let a restore
// MINT a live edge in a direction its writer never asserted, which COG-31
// forbids of a writer exactly as it forbids it of a presenter.
func TestArchiveRestore_DirectionalEdgeNotMirrored(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("archive-directional-not-mirrored")

	src, dst := NewULID(), NewULID()
	seedEndpoints(t, store, ws, src, dst)

	archiveViaDecay(t, store, ctx, ws, src, dst, RelSupports)

	if archiveRowExists(t, store, ws, dst, src) {
		t.Error("directional edge must not be mirrored under ws|dst|src")
	}

	restored, err := store.RestoreArchivedEdges(ctx, ws, [16]byte(dst), 10)
	if err != nil {
		t.Fatalf("RestoreArchivedEdges from destination: %v", err)
	}
	if len(restored) != 0 {
		t.Errorf("a directional edge was restored from its non-authored endpoint: %d", len(restored))
	}
}

// TestArchiveRestore_RestoreDeletesBothArchiveRows: restoring a symmetric
// edge from either side must retire BOTH archive rows in the restore's own
// commit, or the sibling survives as a stale duplicate with restoredAt still
// 0 — exactly the state GCArchivedEdges' restoredAt==0 criterion assumes
// cannot happen for an edge that is live again.
func TestArchiveRestore_RestoreDeletesBothArchiveRows(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("archive-restore-both-rows")

	src, dst := NewULID(), NewULID()
	seedEndpoints(t, store, ws, src, dst)

	archiveViaDecay(t, store, ctx, ws, src, dst, RelContradicts)

	if _, err := store.RestoreArchivedEdges(ctx, ws, [16]byte(src), 10); err != nil {
		t.Fatalf("RestoreArchivedEdges: %v", err)
	}

	if archiveRowExists(t, store, ws, src, dst) {
		t.Error("primary archive row survived a restore triggered from its own side")
	}
	if archiveRowExists(t, store, ws, dst, src) {
		t.Error("mirror archive row survived restore — a stale duplicate with restoredAt==0 remains")
	}
}

// TestArchiveRestore_ConcurrentRestoreAndGC exercises the ordering GC and
// restore can land in relative to each other, deterministically (no
// goroutines, per the project's testing-hermeticity rule): a GC pass that
// runs between the two archive rows being written and either being restored
// must not prune a row a restore is about to need, and a GC pass that runs
// AFTER a full restore must find nothing left to prune and must not error.
func TestArchiveRestore_ConcurrentRestoreAndGC(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("archive-restore-vs-gc")

	src, dst := NewULID(), NewULID()
	seedEndpoints(t, store, ws, src, dst)

	archiveViaDecay(t, store, ctx, ws, src, dst, RelCoActivated)

	// GC runs first. Both mirrored rows carry byte-identical values (fresh
	// LastActivated, peakWeight 0.8 >= the 0.15 GC floor), so neither
	// qualifies for pruning yet — GC must be a no-op here.
	pruned, err := store.GCArchivedEdges(ctx, ws)
	if err != nil {
		t.Fatalf("GCArchivedEdges (pre-restore): %v", err)
	}
	if pruned != 0 {
		t.Fatalf("GC pruned %d row(s) that a restore still needed", pruned)
	}

	restored, err := store.RestoreArchivedEdges(ctx, ws, [16]byte(dst), 10)
	if err != nil {
		t.Fatalf("RestoreArchivedEdges: %v", err)
	}
	if len(restored) != 1 {
		t.Fatalf("expected 1 restored edge, got %d", len(restored))
	}

	// GC runs again after the restore. Both archive rows are gone, so this
	// must find nothing and must not error over an already-live edge.
	pruned, err = store.GCArchivedEdges(ctx, ws)
	if err != nil {
		t.Fatalf("GCArchivedEdges (post-restore): %v", err)
	}
	if pruned != 0 {
		t.Errorf("GC found %d row(s) to prune after both archive rows were already retired by restore", pruned)
	}

	// The restored edge is genuinely live and unaffected by either GC pass.
	// Restore was triggered from dst's own prefix, so the live pair was
	// written (dst, src) — see the direction note in
	// TestArchiveRestore_SymmetricEdgeRestorableFromEitherEndpoint.
	if w, err := store.GetAssocWeight(ctx, ws, dst, src); err != nil || w <= 0 {
		t.Errorf("restored edge not live after both GC passes: w=%v err=%v", w, err)
	}
}

// TestSTO12_ArchivedEdgesFromAHardDeletedSourceAreReaped covers
// reapArchivedEdgesFrom — a destructive, replicated, bulk-delete reachable from
// the recall read path, which shipped with no coverage of its own.
//
// The rows it deletes are provably unrestorable: RestoreArchivedEdges is the
// only reader of a 0x25 prefix and it needs the SOURCE's 0x01 record, which is
// gone. Scanning them on every recall forever is the alternative.
func TestSTO12_ArchivedEdgesFromAHardDeletedSourceAreReaped(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("sto12-reap-archive")

	src, dst1, dst2 := NewULID(), NewULID(), NewULID()
	// The TARGETS are alive; only the SOURCE is missing. That is what makes the
	// rows unrestorable rather than merely stale, and it stops the assertion
	// from passing for the wrong reason (the per-candidate target check).
	seedEndpoints(t, store, ws, dst1, dst2)

	now := time.Now()
	for _, dst := range []ULID{dst1, dst2} {
		val := encodeArchiveValue(RelSupports, 0.9, now.Add(-24*time.Hour), int32(now.Add(-24*time.Hour).Unix()), 0.9, 5, 0)
		if err := store.db.Set(keys.ArchiveAssocKey(ws, [16]byte(src), [16]byte(dst)), val[:], pebble.Sync); err != nil {
			t.Fatalf("seed archive row: %v", err)
		}
	}

	countArchive := func() int {
		prefix := keys.ArchiveAssocPrefixForID(ws, [16]byte(src))
		it, err := store.db.NewIter(&pebble.IterOptions{
			LowerBound: prefix, UpperBound: keys.PrefixUpperBound(append([]byte{}, prefix...)),
		})
		if err != nil {
			t.Fatalf("iter: %v", err)
		}
		defer it.Close()
		n := 0
		for it.First(); it.Valid(); it.Next() {
			n++
		}
		return n
	}
	if countArchive() != 2 {
		t.Fatalf("precondition: expected 2 archive rows, got %d", countArchive())
	}

	restored, err := store.RestoreArchivedEdges(ctx, ws, [16]byte(src), 10)
	if err != nil {
		t.Fatalf("RestoreArchivedEdges over a dead source must not error: %v", err)
	}
	if len(restored) != 0 {
		t.Errorf("edges were restored from a source with no engram record: %d", len(restored))
	}
	if n := countArchive(); n != 0 {
		t.Errorf("%d unrestorable 0x25 row(s) survived — recall rescans them on every candidate, forever", n)
	}
	for _, dst := range []ULID{dst1, dst2} {
		if w, _ := store.GetAssocWeight(ctx, ws, src, dst); w != 0 {
			t.Errorf("a live edge (w=%v) was created from an engram-less source", w)
		}
	}

	// Self-limiting: a second call finds nothing and commits nothing.
	if _, err := store.RestoreArchivedEdges(ctx, ws, [16]byte(src), 10); err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if n := countArchive(); n != 0 {
		t.Errorf("second pass left %d row(s)", n)
	}
}
