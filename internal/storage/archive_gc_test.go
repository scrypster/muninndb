package storage

import (
	"context"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/storage/keys"
)

func TestGCArchivedEdges_PrunesQualifyingEdges(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("gc-test")

	src := NewULID()
	dstPrune := NewULID()
	dstKeep := NewULID()

	// dstPrune: qualifies for GC (peakWeight < 0.15, coAct < 3, >3 years old, never restored)
	fourYearsAgo := int32(time.Now().Add(-4 * 365 * 24 * time.Hour).Unix())
	arcPrune := encodeArchiveValue(RelSupports, 0.5, time.Now().Add(-4*365*24*time.Hour), fourYearsAgo, 0.10, 2, 0)
	store.db.Set(keys.ArchiveAssocKey(ws, [16]byte(src), [16]byte(dstPrune)), arcPrune[:], nil)

	// dstKeep: does NOT qualify (peakWeight > 0.15)
	arcKeep := encodeArchiveValue(RelSupports, 0.9, time.Now().Add(-4*365*24*time.Hour), fourYearsAgo, 0.50, 10, 0)
	store.db.Set(keys.ArchiveAssocKey(ws, [16]byte(src), [16]byte(dstKeep)), arcKeep[:], nil)

	store.archiveBloom.Add(src)

	pruned, err := store.GCArchivedEdges(ctx, ws)
	if err != nil {
		t.Fatalf("GCArchivedEdges: %v", err)
	}
	if pruned != 1 {
		t.Errorf("expected 1 pruned, got %d", pruned)
	}

	// dstPrune should be gone.
	val, _ := Get(store.db, keys.ArchiveAssocKey(ws, [16]byte(src), [16]byte(dstPrune)))
	if val != nil {
		t.Error("dstPrune should have been GC'd")
	}

	// dstKeep should still exist.
	val, _ = Get(store.db, keys.ArchiveAssocKey(ws, [16]byte(src), [16]byte(dstKeep)))
	if val == nil {
		t.Error("dstKeep should still exist")
	}
}
