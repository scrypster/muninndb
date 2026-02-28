package storage

import (
	"context"
	"testing"

	"github.com/cockroachdb/pebble"
	"github.com/scrypster/muninndb/internal/storage/keys"
)

// ---------------------------------------------------------------------------
// ClearEmbedFlagsForVault
// ---------------------------------------------------------------------------

func TestClearEmbedFlagsForVault(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("embed-flags-test")

	// Write several engrams.
	ids := make([]ULID, 5)
	for i := range ids {
		eng := &Engram{
			Concept: "concept",
			Content: "content",
		}
		id, err := store.WriteEngram(ctx, ws, eng)
		if err != nil {
			t.Fatalf("WriteEngram[%d]: %v", i, err)
		}
		ids[i] = id
	}

	// Set DigestEmbed (0x02) flag on each.
	const DigestEmbed uint8 = 0x02
	for i, id := range ids {
		if err := store.SetDigestFlag(ctx, id, DigestEmbed); err != nil {
			t.Fatalf("SetDigestFlag[%d]: %v", i, err)
		}
	}

	// Verify flags are set before clearing.
	for i, id := range ids {
		flags, err := store.GetDigestFlags(ctx, id)
		if err != nil {
			t.Fatalf("GetDigestFlags[%d] before clear: %v", i, err)
		}
		if flags&DigestEmbed == 0 {
			t.Fatalf("expected DigestEmbed set on engram %d before clear", i)
		}
	}

	// Call ClearEmbedFlagsForVault.
	cleared, err := store.ClearEmbedFlagsForVault(ctx, ws)
	if err != nil {
		t.Fatalf("ClearEmbedFlagsForVault: %v", err)
	}

	// Verify: returned count matches number of flags cleared.
	if cleared != int64(len(ids)) {
		t.Errorf("cleared = %d, want %d", cleared, len(ids))
	}

	// Verify: GetDigestFlags for each engram no longer has bit 0x02 set.
	for i, id := range ids {
		flags, err := store.GetDigestFlags(ctx, id)
		if err != nil {
			t.Fatalf("GetDigestFlags[%d] after clear: %v", i, err)
		}
		if flags&DigestEmbed != 0 {
			t.Errorf("engram %d still has DigestEmbed set after clear", i)
		}
	}

	// Verify: calling ClearEmbedFlagsForVault again returns 0 (idempotent).
	cleared2, err := store.ClearEmbedFlagsForVault(ctx, ws)
	if err != nil {
		t.Fatalf("ClearEmbedFlagsForVault (second call): %v", err)
	}
	if cleared2 != 0 {
		t.Errorf("second ClearEmbedFlagsForVault = %d, want 0 (idempotent)", cleared2)
	}
}

// ---------------------------------------------------------------------------
// ClearHNSWForVault
// ---------------------------------------------------------------------------

func TestClearHNSWForVault(t *testing.T) {
	store := newTestStore(t)
	ws := store.VaultPrefix("hnsw-clear-test")

	// Write some 0x07 HNSW keys directly to the DB.
	hnswIDs := make([][16]byte, 3)
	for i := range hnswIDs {
		hnswIDs[i] = [16]byte(NewULID())
		k := keys.HNSWNodeKey(ws, hnswIDs[i], 0)
		if err := store.db.Set(k, []byte("fake-neighbors"), pebble.Sync); err != nil {
			t.Fatalf("write HNSW key[%d]: %v", i, err)
		}
	}

	// Verify keys exist before clearing.
	count := countHNSWKeys(t, store, ws)
	if count != len(hnswIDs) {
		t.Fatalf("before clear: HNSW key count = %d, want %d", count, len(hnswIDs))
	}

	// Call ClearHNSWForVault.
	if err := store.ClearHNSWForVault(ws); err != nil {
		t.Fatalf("ClearHNSWForVault: %v", err)
	}

	// Verify: the keys are gone.
	count = countHNSWKeys(t, store, ws)
	if count != 0 {
		t.Errorf("after clear: HNSW key count = %d, want 0", count)
	}
}

// countHNSWKeys scans the 0x07 range for the vault and returns the count.
func countHNSWKeys(t *testing.T, store *PebbleStore, ws [8]byte) int {
	t.Helper()
	wsPlus, err := keys.IncrementWSPrefix(ws)
	if err != nil {
		t.Fatalf("IncrementWSPrefix: %v", err)
	}
	lo := make([]byte, 9)
	lo[0] = 0x07
	copy(lo[1:], ws[:])
	hi := make([]byte, 9)
	hi[0] = 0x07
	copy(hi[1:], wsPlus[:])

	iter, err := store.db.NewIter(&pebble.IterOptions{
		LowerBound: lo,
		UpperBound: hi,
	})
	if err != nil {
		t.Fatalf("NewIter for HNSW scan: %v", err)
	}
	defer iter.Close()

	n := 0
	for valid := iter.First(); valid; valid = iter.Next() {
		n++
	}
	return n
}

// ---------------------------------------------------------------------------
// GetEmbedModel / SetEmbedModel
// ---------------------------------------------------------------------------

func TestGetSetEmbedModel(t *testing.T) {
	store := newTestStore(t)
	ws := store.VaultPrefix("embed-model-test")

	// GetEmbedModel on fresh vault returns "".
	model, err := store.GetEmbedModel(ws)
	if err != nil {
		t.Fatalf("GetEmbedModel (fresh): %v", err)
	}
	if model != "" {
		t.Errorf("fresh GetEmbedModel = %q, want empty", model)
	}

	// SetEmbedModel("bge-small-en-v1.5").
	if err := store.SetEmbedModel(ws, "bge-small-en-v1.5"); err != nil {
		t.Fatalf("SetEmbedModel: %v", err)
	}

	// GetEmbedModel returns "bge-small-en-v1.5".
	model, err = store.GetEmbedModel(ws)
	if err != nil {
		t.Fatalf("GetEmbedModel (after set): %v", err)
	}
	if model != "bge-small-en-v1.5" {
		t.Errorf("GetEmbedModel = %q, want %q", model, "bge-small-en-v1.5")
	}

	// SetEmbedModel("") clears it.
	if err := store.SetEmbedModel(ws, ""); err != nil {
		t.Fatalf("SetEmbedModel (clear): %v", err)
	}

	// GetEmbedModel returns "" again.
	model, err = store.GetEmbedModel(ws)
	if err != nil {
		t.Fatalf("GetEmbedModel (after clear): %v", err)
	}
	if model != "" {
		t.Errorf("GetEmbedModel after clear = %q, want empty", model)
	}
}
