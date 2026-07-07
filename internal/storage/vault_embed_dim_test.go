package storage

import (
	"context"
	"testing"
)

func TestVaultEmbedDimOnDisk(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("embed-dim-on-disk")

	// No stored embeddings → 0.
	dim, err := store.VaultEmbedDimOnDisk(ws)
	if err != nil {
		t.Fatalf("VaultEmbedDimOnDisk (empty): %v", err)
	}
	if dim != 0 {
		t.Fatalf("empty vault dim = %d, want 0", dim)
	}

	id, err := store.WriteEngram(ctx, ws, &Engram{Concept: "c", Content: "c"})
	if err != nil {
		t.Fatalf("WriteEngram: %v", err)
	}
	vec := make([]float32, 384)
	for i := range vec {
		vec[i] = float32(i%7) + 0.5
	}
	if err := store.UpdateEmbedding(ctx, ws, id, vec); err != nil {
		t.Fatalf("UpdateEmbedding: %v", err)
	}

	// The quantized on-disk layout (8 param bytes + 1 byte/component) must
	// yield the original dimension.
	dim, err = store.VaultEmbedDimOnDisk(ws)
	if err != nil {
		t.Fatalf("VaultEmbedDimOnDisk: %v", err)
	}
	if dim != 384 {
		t.Fatalf("vault dim = %d, want 384", dim)
	}

	// Other vaults are unaffected.
	other, err := store.VaultEmbedDimOnDisk(store.VaultPrefix("some-other-vault"))
	if err != nil {
		t.Fatalf("VaultEmbedDimOnDisk (other): %v", err)
	}
	if other != 0 {
		t.Fatalf("other vault dim = %d, want 0", other)
	}
}
