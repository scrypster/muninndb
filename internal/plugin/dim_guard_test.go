package plugin

import (
	"context"
	"errors"
	"testing"

	"github.com/cockroachdb/pebble"
	hnswpkg "github.com/scrypster/muninndb/internal/index/hnsw"
	"github.com/scrypster/muninndb/internal/storage"
)

// openTestStoreWithHNSW returns a PebbleStore, an HNSW registry sharing the
// same Pebble database (as wired in production by NewStoreAdapter), and that
// database for building additional registries against the same storage.
func openTestStoreWithHNSW(t *testing.T) (*storage.PebbleStore, *hnswpkg.Registry, *pebble.DB) {
	t.Helper()
	db, err := storage.OpenPebble(t.TempDir(), storage.DefaultOptions())
	if err != nil {
		t.Fatalf("open pebble: %v", err)
	}
	store := storage.NewPebbleStore(db, storage.PebbleStoreConfig{CacheSize: 100})
	t.Cleanup(func() { store.Close() })
	return store, hnswpkg.NewRegistry(db), db
}

func guardTestVec(dim int) []float32 {
	vec := make([]float32, dim)
	for i := range vec {
		vec[i] = float32(i%7) + 0.5
	}
	return vec
}

// TestStoreAdapter_UpdateEmbeddingDimGuard pins that a mismatched embedding is
// refused BEFORE it is persisted (issue #582): no 0x18 key is written and the
// HNSW backstop refuses it too.
func TestStoreAdapter_UpdateEmbeddingDimGuard(t *testing.T) {
	store, reg, storeDB := openTestStoreWithHNSW(t)
	adapter := NewStoreAdapter(store, reg)
	ctx := context.Background()
	ws := store.VaultPrefix("dim-guard-vault")

	id1, err := store.WriteEngram(ctx, ws, &storage.Engram{Concept: "a", Content: "a"})
	if err != nil {
		t.Fatalf("WriteEngram: %v", err)
	}
	id2, err := store.WriteEngram(ctx, ws, &storage.Engram{Concept: "b", Content: "b"})
	if err != nil {
		t.Fatalf("WriteEngram: %v", err)
	}

	// Establish the vault dimension at 384 through the normal embed sequence.
	if err := adapter.UpdateEmbedding(ctx, ULID(id1), guardTestVec(384)); err != nil {
		t.Fatalf("UpdateEmbedding (first, establishes dim): %v", err)
	}
	if err := adapter.HNSWInsert(ctx, ULID(id1), guardTestVec(384)); err != nil {
		t.Fatalf("HNSWInsert: %v", err)
	}

	// A 768-dim embedding must now be refused with the typed error.
	err = adapter.UpdateEmbedding(ctx, ULID(id2), guardTestVec(768))
	var dimErr *hnswpkg.DimMismatchError
	if !errors.As(err, &dimErr) {
		t.Fatalf("expected *DimMismatchError from UpdateEmbedding, got %v", err)
	}
	if dimErr.Got != 768 || dimErr.Want != 384 {
		t.Fatalf("DimMismatchError = got %d want %d; expected got 768 want 384", dimErr.Got, dimErr.Want)
	}

	// Nothing was persisted for id2.
	if vec, err := store.GetEmbedding(ctx, ws, storage.ULID(id2)); err == nil && len(vec) > 0 {
		t.Fatalf("refused embedding was persisted anyway (len %d)", len(vec))
	}

	// The HNSW backstop refuses the same vector as well.
	err = adapter.HNSWInsert(ctx, ULID(id2), guardTestVec(768))
	if !errors.As(err, &dimErr) {
		t.Fatalf("expected *DimMismatchError from HNSWInsert, got %v", err)
	}

	// A matching embedding still goes through.
	if err := adapter.UpdateEmbedding(ctx, ULID(id2), guardTestVec(384)); err != nil {
		t.Fatalf("matching-dimension UpdateEmbedding: %v", err)
	}

	// The guard must also hold when the vault's HNSW index is NOT in memory:
	// a fresh registry (cold cache) falls back to the on-disk dimension —
	// one Pebble seek — without loading the graph.
	coldAdapter := NewStoreAdapter(store, hnswpkg.NewRegistry(storeDB))
	err = coldAdapter.UpdateEmbedding(ctx, ULID(id2), guardTestVec(768))
	if !errors.As(err, &dimErr) {
		t.Fatalf("expected *DimMismatchError from cold-cache UpdateEmbedding, got %v", err)
	}
}

// TestRetroactiveProcessor_DimMismatchSkipsBeforeInference pins the #582
// pre-scan contract: engrams of a vault whose dimension does not match the
// active embedder are skipped BEFORE inference (Embed is never called), are
// NOT failure-flagged (DigestEmbedFailed shares bit 0x80 with
// DigestEnrichFailed and the condition is vault-level, resolved by `vault
// reembed`), and remain pending for the next pass.
func TestRetroactiveProcessor_DimMismatchSkipsBeforeInference(t *testing.T) {
	eng := &Engram{Concept: "vec", Content: "data"}
	iter := &mockIterator{engrams: []*Engram{eng}}

	store := &mockPluginStore{
		countResult: 1,
		scanResult:  iter,
		checkDimErr: &hnswpkg.DimMismatchError{Got: 384, Want: 1024},
	}
	embedPlugin := &mockEmbedPlugin{
		mockPlugin: mockPlugin{name: "embed-dim", tier: TierEmbed},
	}
	rp := NewRetroactiveProcessor(store, embedPlugin, DigestEmbed)

	if ok := rp.processBatch(context.Background()); !ok {
		t.Error("processBatch should return true")
	}

	if store.checkDimCalls != 1 {
		t.Errorf("expected 1 CheckEmbedDim call, got %d", store.checkDimCalls)
	}
	if embedPlugin.embedCalls != 0 {
		t.Errorf("expected 0 Embed calls for a mismatched vault, got %d", embedPlugin.embedCalls)
	}
	if store.updateEmbedCalls != 0 || store.hnswInsertCalls != 0 {
		t.Errorf("expected no embedding writes, got update=%d insert=%d", store.updateEmbedCalls, store.hnswInsertCalls)
	}
	if len(store.setFlags) != 0 {
		t.Errorf("expected no digest flags on a dimension skip, flags set: %v", store.setFlags)
	}
}

// TestRetroactiveProcessor_DimMismatchAtWriteLeftPending pins the backstop
// behavior when the mismatch only surfaces at UpdateEmbedding (a race the
// pre-scan cannot see): the engram is left pending — no DigestEmbedFailed,
// no processed flag — and counted as an error.
func TestRetroactiveProcessor_DimMismatchAtWriteLeftPending(t *testing.T) {
	eng := &Engram{Concept: "vec", Content: "data"}
	iter := &mockIterator{engrams: []*Engram{eng}}

	store := &mockPluginStore{
		countResult:    1,
		scanResult:     iter,
		updateEmbedErr: &hnswpkg.DimMismatchError{Got: 384, Want: 1024},
	}
	embedPlugin := &mockEmbedPlugin{
		mockPlugin: mockPlugin{name: "embed-dim-race", tier: TierEmbed},
	}
	rp := NewRetroactiveProcessor(store, embedPlugin, DigestEmbed)

	if ok := rp.processBatch(context.Background()); !ok {
		t.Error("processBatch should return true")
	}

	if store.hnswInsertCalls != 0 {
		t.Errorf("expected 0 HNSWInsert calls after refused embedding, got %d", store.hnswInsertCalls)
	}
	if len(store.setFlags) != 0 {
		t.Errorf("expected no digest flags on a dimension refusal, flags set: %v", store.setFlags)
	}
	if stats := rp.Stats(); stats.Errors != 1 {
		t.Errorf("expected Errors=1, got %d", stats.Errors)
	}
}
