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
