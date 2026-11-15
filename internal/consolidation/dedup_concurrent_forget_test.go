package consolidation

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/scrypster/muninndb/internal/storage"
)

// TestDedup_ConcurrentForgetOfRepresentativeSticks: dedup reads every engram
// at the start of the pass and, after archiving a cluster's duplicates, used
// to write the representative's WHOLE metadata back from that early copy to
// bump AccessCount. A forget of the representative committed in between was
// reverted — the memory came back active. The forget is landed by the cache
// fill seam, i.e. after dedup's read of the representative and before
// anything is written.
func TestDedup_ConcurrentForgetOfRepresentativeSticks(t *testing.T) {
	store, db, cleanup := testStoreWithDB(t)
	defer cleanup()

	ctx := context.Background()
	vault := "dedup_concurrent_forget"
	wsPrefix := store.ResolveVaultPrefix(vault)
	embed := []float32{0, 1, 0, 0}

	repID := writeEngramWithEmbedding(t, ctx, store, db, wsPrefix, &storage.Engram{
		Concept: "rep", Content: "hive scale reading logged", Confidence: 0.9, Relevance: 0.9,
		Stability: 30, Embedding: embed, AccessCount: 5,
	})
	for i := 0; i < 3; i++ {
		writeEngramWithEmbedding(t, ctx, store, db, wsPrefix, &storage.Engram{
			Concept: fmt.Sprintf("dup-%c", 'a'+i), Content: "hive scale reading logged",
			Confidence: 0.5, Relevance: 0.5, Stability: 30, Embedding: embed,
		})
	}
	// The fixture rewrote 0x01 behind the store; start from cold caches so
	// dedup's read is a Pebble read that passes the fill seam.
	store.InvalidateAllCaches()

	var fired atomic.Bool
	restore := storage.SetCacheFillHookForTesting(func(ws [8]byte, id storage.ULID) {
		if ws != wsPrefix || id != repID || !fired.CompareAndSwap(false, true) {
			return
		}
		if err := store.SoftDelete(ctx, wsPrefix, repID); err != nil {
			t.Errorf("SoftDelete: %v", err)
		}
	})
	w := &Worker{Engine: &mockEngineInterface{store: store}, MaxDedup: 100, MaxTransitive: 100}
	err := w.runPhase2Dedup(ctx, store, wsPrefix, &ConsolidationReport{}, vault)
	restore()
	if err != nil {
		t.Fatal(err)
	}
	if !fired.Load() {
		t.Fatal("fixture: dedup never read the representative through the fill seam")
	}

	rep, err := store.GetEngramUncached(ctx, wsPrefix, repID)
	if err != nil {
		t.Fatal(err)
	}
	if rep.State != storage.StateSoftDeleted {
		t.Fatalf("representative state = %v after a forget committed during the pass: dedup's frequency bump reverted it", rep.State)
	}
	if rep.AccessCount != 5+3 {
		t.Fatalf("representative AccessCount = %d, want 8", rep.AccessCount)
	}
}
