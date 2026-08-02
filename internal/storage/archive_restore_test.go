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
