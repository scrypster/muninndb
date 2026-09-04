package migrate

import (
	"bytes"
	"testing"

	"github.com/cockroachdb/pebble"
	"github.com/cockroachdb/pebble/vfs"
	"github.com/scrypster/muninndb/internal/prefix"
	"github.com/scrypster/muninndb/internal/storage/keys"
)

// makeRelKey builds a raw LEGACY 42-byte 0x21 relationship key without going
// through the storage layer, matching the exact pre-#894 byte layout the
// migration reads from: 0x21 | ws(8) | engramID(16) | fromHash(8) |
// relTypeByte(1) | toHash(8). The live keys.RelationshipKey now emits the
// 49-byte predicate-hash layout, so v2's historical input must be hand-rolled
// (v6's legacyEntityKey precedent: migrations and their fixtures decode the
// format that was on disk, not whatever the live constructor becomes later).
func makeRelKey(ws [8]byte, engramID [16]byte, fromHash [8]byte, toHash [8]byte) []byte {
	k := make([]byte, 42)
	k[0] = prefix.Relationship
	copy(k[1:9], ws[:])
	copy(k[9:25], engramID[:])
	copy(k[25:33], fromHash[:])
	k[33] = 0x01 // any relTypeByte — v2 never reads this byte
	copy(k[34:42], toHash[:])
	return k
}

// hasRelEntityIndexKey returns true iff the 0x26 key for (ws, entityHash, engramID) exists in db.
func hasRelEntityIndexKey(t *testing.T, db *pebble.DB, ws [8]byte, entityHash [8]byte, engramID [16]byte) bool {
	t.Helper()
	k := keys.RelEntityIndexKey(ws, entityHash, engramID)
	_, closer, err := db.Get(k)
	if err == pebble.ErrNotFound {
		return false
	}
	if err != nil {
		t.Fatalf("db.Get 0x26 key: %v", err)
	}
	closer.Close()
	return true
}

// TestBackfillRelEntityIndex_WritesFromAndToEntries verifies that a single 0x21 key
// produces one 0x26 entry for fromHash and one for toHash.
func TestBackfillRelEntityIndex_WritesFromAndToEntries(t *testing.T) {
	db, err := pebble.Open("", &pebble.Options{FS: vfs.NewMem()})
	if err != nil {
		t.Fatalf("open pebble: %v", err)
	}
	defer db.Close()

	ws := [8]byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08}
	engramID := [16]byte{1}
	fromHash := [8]byte{0xAA, 0xAA, 0xAA, 0xAA, 0xAA, 0xAA, 0xAA, 0xAA}
	toHash := [8]byte{0xBB, 0xBB, 0xBB, 0xBB, 0xBB, 0xBB, 0xBB, 0xBB}

	relKey := makeRelKey(ws, engramID, fromHash, toHash)
	if err := db.Set(relKey, nil, pebble.Sync); err != nil {
		t.Fatalf("set 0x21 key: %v", err)
	}

	if err := BackfillRelEntityIndex(db); err != nil {
		t.Fatalf("BackfillRelEntityIndex: %v", err)
	}

	if !hasRelEntityIndexKey(t, db, ws, fromHash, engramID) {
		t.Error("missing 0x26 entry for fromHash after backfill")
	}
	if !hasRelEntityIndexKey(t, db, ws, toHash, engramID) {
		t.Error("missing 0x26 entry for toHash after backfill")
	}
}

// TestBackfillRelEntityIndex_Idempotent verifies that running the migration twice
// does not error and does not alter the existing 0x26 entries.
func TestBackfillRelEntityIndex_Idempotent(t *testing.T) {
	db, err := pebble.Open("", &pebble.Options{FS: vfs.NewMem()})
	if err != nil {
		t.Fatalf("open pebble: %v", err)
	}
	defer db.Close()

	ws := [8]byte{0x10}
	engramID := [16]byte{2}
	fromHash := [8]byte{0x11}
	toHash := [8]byte{0x22}

	relKey := makeRelKey(ws, engramID, fromHash, toHash)
	if err := db.Set(relKey, nil, pebble.Sync); err != nil {
		t.Fatalf("set 0x21 key: %v", err)
	}

	// First run.
	if err := BackfillRelEntityIndex(db); err != nil {
		t.Fatalf("BackfillRelEntityIndex (first): %v", err)
	}
	// Second run — must be a no-op, no error.
	if err := BackfillRelEntityIndex(db); err != nil {
		t.Fatalf("BackfillRelEntityIndex (second): %v", err)
	}

	// Both 0x26 entries must still exist.
	if !hasRelEntityIndexKey(t, db, ws, fromHash, engramID) {
		t.Error("0x26 fromHash entry missing after second run")
	}
	if !hasRelEntityIndexKey(t, db, ws, toHash, engramID) {
		t.Error("0x26 toHash entry missing after second run")
	}
}

// TestBackfillRelEntityIndex_SkipsMalformedKey verifies that 0x21-prefixed keys with
// the wrong length are silently skipped and do not cause an error.
func TestBackfillRelEntityIndex_SkipsMalformedKey(t *testing.T) {
	db, err := pebble.Open("", &pebble.Options{FS: vfs.NewMem()})
	if err != nil {
		t.Fatalf("open pebble: %v", err)
	}
	defer db.Close()

	// Write a truncated 0x21 key (wrong length).
	malformed := []byte{0x21, 0x00, 0x01, 0x02} // only 4 bytes, not 42
	if err := db.Set(malformed, nil, pebble.Sync); err != nil {
		t.Fatalf("set malformed key: %v", err)
	}

	// Must not return an error.
	if err := BackfillRelEntityIndex(db); err != nil {
		t.Fatalf("BackfillRelEntityIndex with malformed key: %v", err)
	}

	// No 0x26 entries should have been written (malformed key skipped).
	iter, iterErr := db.NewIter(&pebble.IterOptions{
		LowerBound: []byte{0x26},
		UpperBound: []byte{0x27},
	})
	if iterErr != nil {
		t.Fatalf("new iter for 0x26 scan: %v", iterErr)
	}
	defer iter.Close()
	if iter.First() {
		t.Errorf("unexpected 0x26 entry written for malformed 0x21 key: %x", iter.Key())
	}
}

// TestBackfillRelEntityIndex_MultiRecord verifies that multiple 0x21 keys across
// different vaults are all backfilled correctly.
func TestBackfillRelEntityIndex_MultiRecord(t *testing.T) {
	db, err := pebble.Open("", &pebble.Options{FS: vfs.NewMem()})
	if err != nil {
		t.Fatalf("open pebble: %v", err)
	}
	defer db.Close()

	type relEntry struct {
		ws       [8]byte
		engramID [16]byte
		fromHash [8]byte
		toHash   [8]byte
	}
	entries := []relEntry{
		{
			ws:       [8]byte{0x01},
			engramID: [16]byte{0x01},
			fromHash: [8]byte{0xAA},
			toHash:   [8]byte{0xBB},
		},
		{
			ws:       [8]byte{0x02},
			engramID: [16]byte{0x02},
			fromHash: [8]byte{0xCC},
			toHash:   [8]byte{0xDD},
		},
		{
			ws:       [8]byte{0x01}, // same vault, different engram
			engramID: [16]byte{0x03},
			fromHash: [8]byte{0xEE},
			toHash:   [8]byte{0xFF},
		},
	}

	for _, e := range entries {
		k := makeRelKey(e.ws, e.engramID, e.fromHash, e.toHash)
		if err := db.Set(k, nil, pebble.Sync); err != nil {
			t.Fatalf("set 0x21 key: %v", err)
		}
	}

	if err := BackfillRelEntityIndex(db); err != nil {
		t.Fatalf("BackfillRelEntityIndex: %v", err)
	}

	for _, e := range entries {
		if !hasRelEntityIndexKey(t, db, e.ws, e.fromHash, e.engramID) {
			t.Errorf("missing 0x26 fromHash for ws=%x engramID=%x", e.ws, e.engramID)
		}
		if !hasRelEntityIndexKey(t, db, e.ws, e.toHash, e.engramID) {
			t.Errorf("missing 0x26 toHash for ws=%x engramID=%x", e.ws, e.engramID)
		}
	}
}

// TestBackfillRelEntityIndex_EmptyDB verifies that an empty database is a no-op.
func TestBackfillRelEntityIndex_EmptyDB(t *testing.T) {
	db, err := pebble.Open("", &pebble.Options{FS: vfs.NewMem()})
	if err != nil {
		t.Fatalf("open pebble: %v", err)
	}
	defer db.Close()

	if err := BackfillRelEntityIndex(db); err != nil {
		t.Fatalf("BackfillRelEntityIndex on empty DB: %v", err)
	}
}

// TestBackfillRelEntityIndex_KeyLayout verifies that the 0x26 key byte layout
// matches the expected structure: 0x26 | ws(8) | entityHash(8) | engramID(16).
func TestBackfillRelEntityIndex_KeyLayout(t *testing.T) {
	db, err := pebble.Open("", &pebble.Options{FS: vfs.NewMem()})
	if err != nil {
		t.Fatalf("open pebble: %v", err)
	}
	defer db.Close()

	ws := [8]byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08}
	engramID := [16]byte{
		0x10, 0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17,
		0x18, 0x19, 0x1A, 0x1B, 0x1C, 0x1D, 0x1E, 0x1F,
	}
	fromHash := [8]byte{0xAA, 0xBB, 0xCC, 0xDD, 0xEE, 0xFF, 0x11, 0x22}
	toHash := [8]byte{0x33, 0x44, 0x55, 0x66, 0x77, 0x88, 0x99, 0x00}

	relKey := makeRelKey(ws, engramID, fromHash, toHash)
	if err := db.Set(relKey, nil, pebble.Sync); err != nil {
		t.Fatalf("set 0x21 key: %v", err)
	}

	if err := BackfillRelEntityIndex(db); err != nil {
		t.Fatalf("BackfillRelEntityIndex: %v", err)
	}

	// Verify raw byte layout of the fromHash 0x26 key.
	expectedFromKey := keys.RelEntityIndexKey(ws, fromHash, engramID)
	if len(expectedFromKey) != 33 {
		t.Fatalf("expected 0x26 key to be 33 bytes, got %d", len(expectedFromKey))
	}
	if expectedFromKey[0] != 0x26 {
		t.Errorf("key[0] = 0x%02X, want 0x26", expectedFromKey[0])
	}
	if !bytes.Equal(expectedFromKey[1:9], ws[:]) {
		t.Errorf("key[1:9] = %x, want ws %x", expectedFromKey[1:9], ws)
	}
	if !bytes.Equal(expectedFromKey[9:17], fromHash[:]) {
		t.Errorf("key[9:17] = %x, want fromHash %x", expectedFromKey[9:17], fromHash)
	}
	if !bytes.Equal(expectedFromKey[17:33], engramID[:]) {
		t.Errorf("key[17:33] = %x, want engramID %x", expectedFromKey[17:33], engramID)
	}

	// Confirm the key actually exists in the db.
	_, closer, err := db.Get(expectedFromKey)
	if err != nil {
		t.Fatalf("0x26 key not found after backfill: %v", err)
	}
	closer.Close()
}
