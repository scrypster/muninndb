package migrate

import (
	"testing"
	"time"

	"github.com/cockroachdb/pebble"
	"github.com/cockroachdb/pebble/vfs"
	"github.com/scrypster/muninndb/internal/storage/erf"
	"github.com/scrypster/muninndb/internal/storage/keys"
)

// writePreUpgradeEngram writes a raw 0x01 engram record directly (ERF-encoded,
// via erf.Encode) WITHOUT going through storage.PebbleStore.WriteEngram —
// simulating an engram that was written before the 0x2C raw-tag-range index
// existed. No 0x2C entry is written for it, exactly like data from a binary
// that predates S1.
func writePreUpgradeEngram(t *testing.T, db *pebble.DB, ws [8]byte, id [16]byte, tags []string) {
	t.Helper()
	eng := &erf.Engram{
		ID:         id,
		Concept:    "pre-upgrade engram",
		Content:    "content written before the raw tag range index existed",
		Tags:       tags,
		Confidence: 1.0,
		Stability:  30.0,
		CreatedAt:  time.Now().Add(-30 * 24 * time.Hour),
		UpdatedAt:  time.Now().Add(-30 * 24 * time.Hour),
		LastAccess: time.Now().Add(-30 * 24 * time.Hour),
		State:      1, // StateActive
	}
	encoded, err := erf.Encode(eng)
	if err != nil {
		t.Fatalf("erf.Encode: %v", err)
	}
	if err := db.Set(keys.EngramKey(ws, id), encoded, pebble.Sync); err != nil {
		t.Fatalf("set 0x01 engram key: %v", err)
	}
}

// TestRawTagRange_Backfill is the pre-upgrade acceptance case: engrams
// written before the 0x2C index existed (no raw-tag-range entries at all)
// must become queryable via ScanRawTagRange-style range bounds after running
// BackfillRawTagRange once. This proves the migration is EAGER (a one-time,
// version-gated backfill) rather than lazy — pre-existing due: tags must
// become queryable immediately, not only on next write.
func TestRawTagRange_Backfill(t *testing.T) {
	db, err := pebble.Open("", &pebble.Options{FS: vfs.NewMem()})
	if err != nil {
		t.Fatalf("open pebble: %v", err)
	}
	defer db.Close()

	ws := [8]byte{0xAA, 0xBB, 0xCC, 0xDD, 0xEE, 0xFF, 0x11, 0x22}
	id1 := [16]byte{1}
	id2 := [16]byte{2}
	id3 := [16]byte{3} // no colon tag — must NOT get a raw-tag entry

	writePreUpgradeEngram(t, db, ws, id1, []string{"due:2026-01-15", "important"})
	writePreUpgradeEngram(t, db, ws, id2, []string{"due:2026-02-20"})
	writePreUpgradeEngram(t, db, ws, id3, []string{"no-colon-tag"})

	// Before migration: no 0x2C entries exist at all (simulating pre-S1 data).
	tagKeyHash := keys.Hash("due")
	prefixBytes := keys.RawTagRangePrefix(ws, tagKeyHash)
	upperAll := keys.PrefixUpperBound(prefixBytes)
	iter, err := db.NewIter(&pebble.IterOptions{LowerBound: prefixBytes, UpperBound: upperAll})
	if err != nil {
		t.Fatalf("new iter: %v", err)
	}
	if iter.First() {
		t.Fatal("expected no 0x2C entries before migration (simulating pre-upgrade data)")
	}
	iter.Close()

	// Run the backfill migration.
	if err := BackfillRawTagRange(db); err != nil {
		t.Fatalf("BackfillRawTagRange: %v", err)
	}

	// After migration: a bounded range scan (due:<=2026-02-20) finds both
	// due-tagged engrams.
	lower, upper := keys.RawTagRangeBound(ws, tagKeyHash, "lte", []byte("2026-02-20"))
	iter2, err := db.NewIter(&pebble.IterOptions{LowerBound: lower, UpperBound: upper})
	if err != nil {
		t.Fatalf("new iter after migration: %v", err)
	}
	defer iter2.Close()
	found := make(map[[16]byte]bool)
	for valid := iter2.First(); valid; valid = iter2.Next() {
		k := iter2.Key()
		if len(k) < 16 {
			continue
		}
		var gotID [16]byte
		copy(gotID[:], k[len(k)-16:])
		found[gotID] = true
	}
	if !found[id1] {
		t.Error("expected id1 (due:2026-01-15) to be found after backfill")
	}
	if !found[id2] {
		t.Error("expected id2 (due:2026-02-20) to be found after backfill")
	}
	if found[id3] {
		t.Error("id3 has no key:value tag and must not appear in the due: range")
	}

	// The no-colon tag must never have produced a 0x2C entry anywhere.
	allIter, err := db.NewIter(&pebble.IterOptions{LowerBound: []byte{0x2C}, UpperBound: []byte{0x2D}})
	if err != nil {
		t.Fatalf("new iter over all 0x2C: %v", err)
	}
	defer allIter.Close()
	count := 0
	for valid := allIter.First(); valid; valid = allIter.Next() {
		count++
	}
	if count != 2 {
		t.Errorf("expected exactly 2 total 0x2C entries after backfill (one per due: tag), got %d", count)
	}
}

// TestRawTagRange_Backfill_Idempotent verifies running the migration twice
// does not error and does not change the result.
func TestRawTagRange_Backfill_Idempotent(t *testing.T) {
	db, err := pebble.Open("", &pebble.Options{FS: vfs.NewMem()})
	if err != nil {
		t.Fatalf("open pebble: %v", err)
	}
	defer db.Close()

	ws := [8]byte{0x01}
	id := [16]byte{1}
	writePreUpgradeEngram(t, db, ws, id, []string{"due:2026-03-01"})

	if err := BackfillRawTagRange(db); err != nil {
		t.Fatalf("BackfillRawTagRange (first): %v", err)
	}
	if err := BackfillRawTagRange(db); err != nil {
		t.Fatalf("BackfillRawTagRange (second): %v", err)
	}

	tagKeyHash := keys.Hash("due")
	prefixBytes := keys.RawTagRangePrefix(ws, tagKeyHash)
	upper := keys.PrefixUpperBound(prefixBytes)
	iter, err := db.NewIter(&pebble.IterOptions{LowerBound: prefixBytes, UpperBound: upper})
	if err != nil {
		t.Fatalf("new iter: %v", err)
	}
	defer iter.Close()
	count := 0
	for valid := iter.First(); valid; valid = iter.Next() {
		count++
	}
	if count != 1 {
		t.Errorf("expected exactly 1 entry after idempotent double-run, got %d", count)
	}
}
