package migrate

import (
	"testing"
	"time"

	"github.com/cockroachdb/pebble"
	"github.com/cockroachdb/pebble/vfs"
	"github.com/scrypster/muninndb/internal/storage/erf"
	"github.com/scrypster/muninndb/internal/storage/keys"
	"github.com/scrypster/muninndb/internal/types"
)

// makeTestERFBytes encodes a minimal valid ERF record for the given id with
// EmbedDim set to the given value. Used by migration tests.
func makeTestERFBytes(t *testing.T, id [16]byte, embedDim uint8) []byte {
	t.Helper()
	eng := &erf.Engram{
		Concept:    "test-concept",
		Content:    "test content",
		Confidence: 0.9,
		Relevance:  0.5,
		Stability:  30.0,
		State:      0x01, // StateActive
		EmbedDim:   embedDim,
		CreatedAt:  time.Now().Truncate(time.Nanosecond),
		UpdatedAt:  time.Now().Truncate(time.Nanosecond),
		LastAccess: time.Now().Truncate(time.Nanosecond),
	}
	copy(eng.ID[:], id[:])
	b, err := erf.Encode(eng)
	if err != nil {
		t.Fatalf("makeTestERFBytes: Encode: %v", err)
	}
	return b
}

func TestBackfillEmbedDim(t *testing.T) {
	db, err := pebble.Open("", &pebble.Options{FS: vfs.NewMem()})
	if err != nil {
		t.Fatalf("open pebble: %v", err)
	}
	defer db.Close()

	wsPrefix := [8]byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08}
	id := [16]byte{1}

	// Encode a minimal valid ERF record with EmbedDim = 0 (not set).
	eng := &erf.Engram{
		Concept:    "test-concept",
		Content:    "test content",
		Confidence: 0.9,
		Relevance:  0.5,
		Stability:  30.0,
		State:      0x01, // StateActive
		CreatedAt:  time.Now().Truncate(time.Nanosecond),
		UpdatedAt:  time.Now().Truncate(time.Nanosecond),
		LastAccess: time.Now().Truncate(time.Nanosecond),
		// EmbedDim intentionally left as zero (EmbedNone)
	}
	copy(eng.ID[:], id[:])

	erfBytes, err := erf.Encode(eng)
	if err != nil {
		t.Fatalf("Encode ERF: %v", err)
	}

	// Verify EmbedDim is 0 before migration.
	if erfBytes[erf.OffsetEmbedDim] != 0 {
		t.Fatalf("precondition: EmbedDim = %d, want 0", erfBytes[erf.OffsetEmbedDim])
	}

	// Write the ERF record at the 0x01 engram key.
	erfKey := keys.EngramKey(wsPrefix, id)
	if err := db.Set(erfKey, erfBytes, pebble.Sync); err != nil {
		t.Fatalf("set ERF: %v", err)
	}

	// Write the meta key at the 0x02 prefix.
	metaKey := keys.MetaKey(wsPrefix, id)
	if err := db.Set(metaKey, erf.MetaKeySlice(erfBytes), pebble.Sync); err != nil {
		t.Fatalf("set meta: %v", err)
	}

	// Write embedding at 0x18 key (384 dimensions = 384 + 8 = 392 bytes value).
	embedVal := make([]byte, 8+384)
	embedKey := keys.EmbeddingKey(wsPrefix, id)
	if err := db.Set(embedKey, embedVal, pebble.Sync); err != nil {
		t.Fatalf("set embedding: %v", err)
	}

	// Run migration.
	if err := BackfillEmbedDim(db); err != nil {
		t.Fatalf("BackfillEmbedDim: %v", err)
	}

	// Verify EmbedDim is now Embed384 in the ERF record.
	val, closer, err := db.Get(erfKey)
	if err != nil {
		t.Fatalf("get ERF after migration: %v", err)
	}
	defer closer.Close()
	if val[erf.OffsetEmbedDim] != uint8(types.Embed384) {
		t.Errorf("ERF EmbedDim = %d, want %d (Embed384)", val[erf.OffsetEmbedDim], types.Embed384)
	}

	// Verify the patched record is still decodable (CRC32 valid).
	buf := make([]byte, len(val))
	copy(buf, val)
	closer.Close()

	if _, err := erf.Decode(buf); err != nil {
		t.Errorf("Decode after migration: %v (CRC32 should still be valid)", err)
	}
}

func TestBackfillEmbedDim_Idempotent(t *testing.T) {
	db, err := pebble.Open("", &pebble.Options{FS: vfs.NewMem()})
	if err != nil {
		t.Fatalf("open pebble: %v", err)
	}
	defer db.Close()

	wsPrefix := [8]byte{0xAA, 0xBB}
	id := [16]byte{2}

	eng := &erf.Engram{
		Concept:    "already-set",
		Content:    "content",
		Confidence: 0.8,
		Relevance:  0.6,
		Stability:  20.0,
		State:      0x01,
		EmbedDim:   0x02, // Embed768 — already set
		CreatedAt:  time.Now().Truncate(time.Nanosecond),
		UpdatedAt:  time.Now().Truncate(time.Nanosecond),
		LastAccess: time.Now().Truncate(time.Nanosecond),
	}
	copy(eng.ID[:], id[:])

	erfBytes, err := erf.Encode(eng)
	if err != nil {
		t.Fatalf("Encode ERF: %v", err)
	}

	erfKey := keys.EngramKey(wsPrefix, id)
	if err := db.Set(erfKey, erfBytes, pebble.Sync); err != nil {
		t.Fatalf("set ERF: %v", err)
	}

	// Write embedding with 384 dimensions (would map to Embed384 if overwritten).
	embedVal := make([]byte, 8+384)
	embedKey := keys.EmbeddingKey(wsPrefix, id)
	if err := db.Set(embedKey, embedVal, pebble.Sync); err != nil {
		t.Fatalf("set embedding: %v", err)
	}

	// Run migration — should skip because EmbedDim is already set.
	if err := BackfillEmbedDim(db); err != nil {
		t.Fatalf("BackfillEmbedDim: %v", err)
	}

	// Verify EmbedDim remains Embed768, not overwritten with Embed384.
	val, closer, err := db.Get(erfKey)
	if err != nil {
		t.Fatalf("get ERF after migration: %v", err)
	}
	defer closer.Close()
	if val[erf.OffsetEmbedDim] != uint8(types.Embed768) {
		t.Errorf("EmbedDim = %d, want %d (Embed768, unchanged)", val[erf.OffsetEmbedDim], types.Embed768)
	}
}

// TestBackfillEmbedDim_MetaKeyPatched verifies that BackfillEmbedDim patches
// EmbedDim in both the 0x01 ERF key and the 0x02 meta key.
func TestBackfillEmbedDim_MetaKeyPatched(t *testing.T) {
	db, err := pebble.Open("", &pebble.Options{FS: vfs.NewMem()})
	if err != nil {
		t.Fatalf("open pebble: %v", err)
	}
	defer db.Close()

	wsPrefix := [8]byte{0x10}
	id := [16]byte{2}

	erfBytes := makeTestERFBytes(t, id, 0 /* EmbedDim = 0, needs patching */)

	erfKey := keys.EngramKey(wsPrefix, id)
	if err := db.Set(erfKey, erfBytes, pebble.Sync); err != nil {
		t.Fatalf("set ERF: %v", err)
	}

	metaKey := keys.MetaKey(wsPrefix, id)
	if err := db.Set(metaKey, erf.MetaKeySlice(erfBytes), pebble.Sync); err != nil {
		t.Fatalf("set meta: %v", err)
	}

	// 768-dim embedding: 8 bytes params + 768 bytes quantized.
	embedVal := make([]byte, 8+768)
	embedKey := keys.EmbeddingKey(wsPrefix, id)
	if err := db.Set(embedKey, embedVal, pebble.Sync); err != nil {
		t.Fatalf("set embedding: %v", err)
	}

	if err := BackfillEmbedDim(db); err != nil {
		t.Fatalf("BackfillEmbedDim: %v", err)
	}

	// Verify the 0x02 meta key has EmbedDim = Embed768.
	metaVal, metaCloser, err := db.Get(metaKey)
	if err != nil {
		t.Fatalf("get meta key after migration: %v", err)
	}
	defer metaCloser.Close()
	if metaVal[erf.OffsetEmbedDim] != uint8(types.Embed768) {
		t.Errorf("meta EmbedDim = %d, want %d (Embed768)", metaVal[erf.OffsetEmbedDim], types.Embed768)
	}

	// Also verify the 0x01 ERF key was patched.
	erfVal, erfCloser, err := db.Get(erfKey)
	if err != nil {
		t.Fatalf("get ERF key after migration: %v", err)
	}
	defer erfCloser.Close()
	if erfVal[erf.OffsetEmbedDim] != uint8(types.Embed768) {
		t.Errorf("ERF EmbedDim = %d, want %d (Embed768)", erfVal[erf.OffsetEmbedDim], types.Embed768)
	}
}

// TestBackfillEmbedDim_MultiRecord verifies BackfillEmbedDim across mixed states:
// a record that needs patching, one that is already set (idempotent), and one whose
// ERF is missing (orphaned embedding — should be skipped without error).
func TestBackfillEmbedDim_MultiRecord(t *testing.T) {
	db, err := pebble.Open("", &pebble.Options{FS: vfs.NewMem()})
	if err != nil {
		t.Fatalf("open pebble: %v", err)
	}
	defer db.Close()

	wsPrefix := [8]byte{0x20}

	// Record 1: needs patching (384-dim embedding, EmbedDim=0).
	id1 := [16]byte{1}
	erf1 := makeTestERFBytes(t, id1, 0 /* EmbedDim unset */)
	if err := db.Set(keys.EngramKey(wsPrefix, id1), erf1, pebble.Sync); err != nil {
		t.Fatalf("set ERF id1: %v", err)
	}
	if err := db.Set(keys.MetaKey(wsPrefix, id1), erf.MetaKeySlice(erf1), pebble.Sync); err != nil {
		t.Fatalf("set meta id1: %v", err)
	}
	embedVal1 := make([]byte, 8+384) // 384-dim → Embed384
	if err := db.Set(keys.EmbeddingKey(wsPrefix, id1), embedVal1, pebble.Sync); err != nil {
		t.Fatalf("set embedding id1: %v", err)
	}

	// Record 2: already has EmbedDim set (Embed768 = 2) — should be skipped.
	id2 := [16]byte{2}
	erf2 := makeTestERFBytes(t, id2, uint8(types.Embed768))
	if err := db.Set(keys.EngramKey(wsPrefix, id2), erf2, pebble.Sync); err != nil {
		t.Fatalf("set ERF id2: %v", err)
	}
	// Write a 384-dim embedding for id2; migration must NOT overwrite EmbedDim.
	embedVal2 := make([]byte, 8+384)
	if err := db.Set(keys.EmbeddingKey(wsPrefix, id2), embedVal2, pebble.Sync); err != nil {
		t.Fatalf("set embedding id2: %v", err)
	}

	// Record 3: embedding key exists but no ERF record (deleted engram — should skip).
	id3 := [16]byte{3}
	embedVal3 := make([]byte, 8+384)
	if err := db.Set(keys.EmbeddingKey(wsPrefix, id3), embedVal3, pebble.Sync); err != nil {
		t.Fatalf("set embedding id3: %v", err)
	}
	// No ERF record for id3 — BackfillEmbedDim must not error.

	if err := BackfillEmbedDim(db); err != nil {
		t.Fatalf("BackfillEmbedDim: %v", err)
	}

	// id1: should now have EmbedDim = Embed384.
	val1, c1, err := db.Get(keys.EngramKey(wsPrefix, id1))
	if err != nil {
		t.Fatalf("get ERF id1 after migration: %v", err)
	}
	got1 := val1[erf.OffsetEmbedDim]
	c1.Close()
	if got1 != uint8(types.Embed384) {
		t.Errorf("id1 ERF EmbedDim = %d, want %d (Embed384)", got1, types.Embed384)
	}

	// id2: should still have EmbedDim = Embed768 — not overwritten.
	val2, c2, err := db.Get(keys.EngramKey(wsPrefix, id2))
	if err != nil {
		t.Fatalf("get ERF id2 after migration: %v", err)
	}
	got2 := val2[erf.OffsetEmbedDim]
	c2.Close()
	if got2 != uint8(types.Embed768) {
		t.Errorf("id2 ERF EmbedDim = %d, want %d (Embed768, unchanged)", got2, types.Embed768)
	}

	// id3: no ERF written, migration must not have created one.
	_, c3, err := db.Get(keys.EngramKey(wsPrefix, id3))
	if err == nil {
		c3.Close()
		t.Error("id3 should have no ERF record — migration must skip orphaned embeddings")
	}
}
