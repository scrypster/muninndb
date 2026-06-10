package consolidation

import (
	"context"
	"testing"

	"github.com/scrypster/muninndb/internal/storage"
)

// TestDedup_DoesNotMutateCachedRepresentative pins the contract that dedup's
// frequency-absorption must NOT mutate the representative engram in place.
// GetEngrams returns pointers into the L1 cache that concurrent recalls may be
// reading; mutating representative.AccessCount there is a data race. The new
// access count must be computed locally and only persisted via UpdateMetadata
// (which invalidates the cache), leaving any already-handed-out pointer intact.
func TestDedup_DoesNotMutateCachedRepresentative(t *testing.T) {
	store, db, cleanup := testStoreWithDB(t)
	defer cleanup()

	ctx := context.Background()
	vault := "dedup_no_mutate"
	wsPrefix := store.ResolveVaultPrefix(vault)

	embed := []float32{1, 0, 0, 0}
	const startAccess uint32 = 5

	repID := writeEngramWithEmbedding(t, ctx, store, db, wsPrefix, &storage.Engram{
		Concept: "rep", Content: "rep content", Confidence: 0.9, Relevance: 0.9,
		Stability: 30, Embedding: embed, AccessCount: startAccess,
	})
	for i := 0; i < 3; i++ {
		writeEngramWithEmbedding(t, ctx, store, db, wsPrefix, &storage.Engram{
			Concept: "dup", Content: "dup content", Confidence: 0.5, Relevance: 0.5,
			Stability: 30, Embedding: embed, AccessCount: 0,
		})
	}

	// Pre-warm the L1 cache and hold the representative pointer, exactly as a
	// concurrent recall would. dedup will fetch the same pointer from the cache.
	held, err := store.GetEngrams(ctx, wsPrefix, []storage.ULID{repID})
	if err != nil || len(held) != 1 || held[0] == nil {
		t.Fatalf("pre-warm GetEngrams: %v (n=%d)", err, len(held))
	}
	heldRep := held[0]
	if heldRep.AccessCount != startAccess {
		t.Fatalf("setup: held AccessCount = %d, want %d", heldRep.AccessCount, startAccess)
	}

	mock := &mockEngineInterface{store: store}
	w := &Worker{Engine: mock, MaxDedup: 100, MaxTransitive: 100}
	if err := w.runPhase2Dedup(ctx, store, wsPrefix, &ConsolidationReport{}, vault); err != nil {
		t.Fatal(err)
	}

	// The pointer handed out before dedup must be unchanged — dedup must not
	// have mutated the shared cache struct in place.
	if heldRep.AccessCount != startAccess {
		t.Errorf("dedup mutated the cached representative in place: AccessCount = %d, want %d (unchanged)",
			heldRep.AccessCount, startAccess)
	}

	// The persisted value must still reflect the absorbed duplicates (cache was
	// invalidated by UpdateMetadata, so this re-reads from storage).
	rep, err := store.GetEngram(ctx, wsPrefix, repID)
	if err != nil {
		t.Fatal(err)
	}
	if rep.AccessCount != startAccess+3 {
		t.Errorf("persisted AccessCount = %d, want %d (5 starting + 3 absorbed)", rep.AccessCount, startAccess+3)
	}
}
