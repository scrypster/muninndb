package storage

import (
	"context"
	"testing"

	"github.com/scrypster/muninndb/internal/storage/keys"
	"github.com/scrypster/muninndb/internal/types"
)

// digestFlagAll is a convenience flag covering all bits, used to identify any
// flagged engram in CountWithFlag tests.
const digestFlagAll = uint8(0xFF)

// TestCountWithoutFlag writes 3 engrams, sets the digest flag on 1, and
// verifies CountWithoutFlag returns 2 (the unflagged ones).
func TestCountWithoutFlag(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("count-without-flag-vault")

	var ids []ULID
	for i := 0; i < 3; i++ {
		id, err := store.WriteEngram(ctx, ws, &Engram{
			Concept: "concept",
			Content: "content",
		})
		if err != nil {
			t.Fatalf("WriteEngram[%d]: %v", i, err)
		}
		ids = append(ids, id)
	}

	// Flag only the first engram.
	const flag = uint8(0x01)
	if err := store.SetDigestFlag(ctx, ids[0], flag); err != nil {
		t.Fatalf("SetDigestFlag: %v", err)
	}

	count, err := store.CountWithoutFlag(ctx, flag)
	if err != nil {
		t.Fatalf("CountWithoutFlag: %v", err)
	}
	// 2 of 3 engrams lack the flag.
	if count != 2 {
		t.Errorf("CountWithoutFlag: got %d, want 2", count)
	}
}

// TestCountWithFlag writes 3 engrams, sets the digest flag on 1, and verifies
// CountWithFlag returns 1 (only the flagged one).
func TestCountWithFlag(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("count-with-flag-vault")

	var ids []ULID
	for i := 0; i < 3; i++ {
		id, err := store.WriteEngram(ctx, ws, &Engram{
			Concept: "concept",
			Content: "content",
		})
		if err != nil {
			t.Fatalf("WriteEngram[%d]: %v", i, err)
		}
		ids = append(ids, id)
	}

	// Flag only the second engram.
	const flag = uint8(0x01)
	if err := store.SetDigestFlag(ctx, ids[1], flag); err != nil {
		t.Fatalf("SetDigestFlag: %v", err)
	}

	count, err := store.CountWithFlag(ctx, flag)
	if err != nil {
		t.Fatalf("CountWithFlag: %v", err)
	}
	if count != 1 {
		t.Errorf("CountWithFlag: got %d, want 1", count)
	}
}

// TestFindVaultPrefix writes an engram in a known vault, then calls
// FindVaultPrefix with the engram's ULID and verifies the correct ws is returned.
func TestFindVaultPrefix(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("find-vault-prefix-vault")

	id, err := store.WriteEngram(ctx, ws, &Engram{
		Concept: "find-me",
		Content: "some content",
	})
	if err != nil {
		t.Fatalf("WriteEngram: %v", err)
	}

	gotWS, ok := store.FindVaultPrefix(id)
	if !ok {
		t.Fatal("FindVaultPrefix: expected ok=true, got false")
	}
	if gotWS != ws {
		t.Errorf("FindVaultPrefix: got ws %x, want %x", gotWS, ws)
	}
}

func TestDimFromLen(t *testing.T) {
	cases := []struct {
		n    int
		want types.EmbedDimension
	}{
		{0, types.EmbedNone},
		{384, types.Embed384},
		{768, types.Embed768},
		{1536, types.Embed1536},
		{3072, types.Embed3072},
		{512, types.EmbedOther},
		{1, types.EmbedOther},
	}
	for _, tc := range cases {
		got := DimFromLen(tc.n)
		if got != tc.want {
			t.Errorf("DimFromLen(%d) = %d, want %d", tc.n, got, tc.want)
		}
	}
}

// TestFindVaultPrefix_NotFound verifies that FindVaultPrefix returns ok=false
// for a ULID that was never written to any vault.
func TestFindVaultPrefix_NotFound(t *testing.T) {
	store := newTestStore(t)

	ghost := NewULID()
	_, ok := store.FindVaultPrefix(ghost)
	if ok {
		t.Error("FindVaultPrefix: expected ok=false for unwritten ULID, got true")
	}
}

func TestUpdateEmbedding_SetsDimInERF(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("embed-dim-test")

	id, err := store.WriteEngram(ctx, ws, &Engram{
		Concept: "test concept",
		Content: "test content",
	})
	if err != nil {
		t.Fatalf("WriteEngram: %v", err)
	}

	// EmbedDim should initially be 0.
	got, err := store.GetEngram(ctx, ws, id)
	if err != nil {
		t.Fatalf("GetEngram before update: %v", err)
	}
	if got.EmbedDim != EmbedNone {
		t.Errorf("initial EmbedDim = %d, want 0", got.EmbedDim)
	}

	// Update with a 384-dim vector.
	vec := make([]float32, 384)
	vec[0] = 0.1
	if err := store.UpdateEmbedding(ctx, ws, id, vec); err != nil {
		t.Fatalf("UpdateEmbedding: %v", err)
	}

	got, err = store.GetEngram(ctx, ws, id)
	if err != nil {
		t.Fatalf("GetEngram after update: %v", err)
	}
	if got.EmbedDim != EmbedDimension(types.Embed384) {
		t.Errorf("EmbedDim = %d, want %d (Embed384)", got.EmbedDim, types.Embed384)
	}
}

func TestUpdateEmbedding_Dim3072(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("embed-dim-3072")

	id, err := store.WriteEngram(ctx, ws, &Engram{
		Concept: "gemini test",
		Content: "3072-dim embedding test",
	})
	if err != nil {
		t.Fatalf("WriteEngram: %v", err)
	}

	vec := make([]float32, 3072)
	vec[0] = 0.5
	if err := store.UpdateEmbedding(ctx, ws, id, vec); err != nil {
		t.Fatalf("UpdateEmbedding: %v", err)
	}

	got, err := store.GetEngram(ctx, ws, id)
	if err != nil {
		t.Fatalf("GetEngram: %v", err)
	}
	if got.EmbedDim != EmbedDimension(types.Embed3072) {
		t.Errorf("EmbedDim = %d, want %d (Embed3072)", got.EmbedDim, types.Embed3072)
	}
}

// TestUpdateEmbedding_MetaKeyPatched verifies that UpdateEmbedding patches EmbedDim
// in the 0x02 meta key so that GetMetadata (used by list endpoints) also reflects
// the correct embedding dimension — not just the full engram read path.
func TestUpdateEmbedding_MetaKeyPatched(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("embed-meta-test")

	id, err := store.WriteEngram(ctx, ws, &Engram{
		Concept: "meta test",
		Content: "meta key patch test",
	})
	if err != nil {
		t.Fatalf("WriteEngram: %v", err)
	}

	vec := make([]float32, 768)
	vec[0] = 0.3
	if err := store.UpdateEmbedding(ctx, ws, id, vec); err != nil {
		t.Fatalf("UpdateEmbedding: %v", err)
	}

	// Verify via GetMetadata (reads from 0x02 meta key / metaCache path).
	metas, err := store.GetMetadata(ctx, ws, []ULID{id})
	if err != nil {
		t.Fatalf("GetMetadata: %v", err)
	}
	if len(metas) != 1 || metas[0] == nil {
		t.Fatalf("GetMetadata returned unexpected results: len=%d", len(metas))
	}
	if metas[0].EmbedDim != EmbedDimension(types.Embed768) {
		t.Errorf("meta EmbedDim = %d, want %d (Embed768)", metas[0].EmbedDim, types.Embed768)
	}
}

// TestUpdateEmbedding_MissingERF verifies that UpdateEmbedding does not return an
// error when the ERF record (0x01 key) does not exist for the given ID, and that
// the embedding key (0x18) is still written successfully.
func TestUpdateEmbedding_MissingERF(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("embed-missing-erf")

	// Call UpdateEmbedding for an ID that was never written as an engram.
	id := NewULID()
	vec := make([]float32, 384)
	vec[0] = 0.1

	// Should not return an error even though the ERF record doesn't exist.
	if err := store.UpdateEmbedding(ctx, ws, id, vec); err != nil {
		t.Errorf("UpdateEmbedding with missing ERF should not error: %v", err)
	}

	// The embedding key should still be written (0x18 prefix).
	embedKey := keys.EmbeddingKey(ws, [16]byte(id))
	val, closer, err := store.db.Get(embedKey)
	if err != nil {
		t.Errorf("embedding key should be written even when ERF missing: %v", err)
	} else {
		closer.Close()
		// quantized: 8 bytes params + 384 bytes quantized values
		if len(val) != 8+384 {
			t.Errorf("embedding val len = %d, want %d", len(val), 8+384)
		}
	}
}

// TestUpdateEmbedding_EmbedOther verifies that a vector with an unknown dimension
// (not 384, 768, 1536, or 3072) is stored with EmbedDim = EmbedOther (255).
func TestUpdateEmbedding_EmbedOther(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("embed-other-test")

	id, err := store.WriteEngram(ctx, ws, &Engram{
		Concept: "unknown dim",
		Content: "512-dim embedding",
	})
	if err != nil {
		t.Fatalf("WriteEngram: %v", err)
	}

	vec := make([]float32, 512) // not in known enum → EmbedOther
	vec[0] = 0.7
	if err := store.UpdateEmbedding(ctx, ws, id, vec); err != nil {
		t.Fatalf("UpdateEmbedding: %v", err)
	}

	got, err := store.GetEngram(ctx, ws, id)
	if err != nil {
		t.Fatalf("GetEngram: %v", err)
	}
	if got.EmbedDim != EmbedDimension(types.EmbedOther) {
		t.Errorf("EmbedDim = %d, want %d (EmbedOther=255)", got.EmbedDim, types.EmbedOther)
	}
}
