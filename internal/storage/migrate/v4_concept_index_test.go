package migrate

import (
	"testing"

	"github.com/cockroachdb/pebble"
	"github.com/scrypster/muninndb/internal/prefix"
	"github.com/scrypster/muninndb/internal/storage/erf"
	"github.com/scrypster/muninndb/internal/storage/keys"
)

func writeMigrationEngram(t *testing.T, db *pebble.DB, vault [8]byte, id [16]byte, concept string) {
	t.Helper()
	value, err := erf.EncodeV2(&erf.Engram{ID: id, Concept: concept})
	if err != nil {
		t.Fatalf("encode engram: %v", err)
	}
	if err := db.Set(keys.EngramKey(vault, id), value, pebble.Sync); err != nil {
		t.Fatalf("write engram: %v", err)
	}
}

func hasConceptIndexKey(t *testing.T, db *pebble.DB, vault [8]byte, id [16]byte, concept string) bool {
	t.Helper()
	_, closer, err := db.Get(keys.ConceptIndexKey(vault, keys.Hash(concept), id))
	if err == pebble.ErrNotFound {
		return false
	}
	if err != nil {
		t.Fatalf("read concept index: %v", err)
	}
	closer.Close()
	return true
}

func countConceptIndexKeys(t *testing.T, db *pebble.DB) int {
	t.Helper()
	iter, err := db.NewIter(&pebble.IterOptions{
		LowerBound: []byte{prefix.ConceptIndex},
		UpperBound: []byte{prefix.ConceptIndex + 1},
	})
	if err != nil {
		t.Fatalf("scan concept index: %v", err)
	}
	defer iter.Close()
	count := 0
	for valid := iter.First(); valid; valid = iter.Next() {
		count++
	}
	if err := iter.Error(); err != nil {
		t.Fatalf("scan concept index: %v", err)
	}
	return count
}

func TestBackfillConceptIndex_WritesExistingRecords(t *testing.T) {
	db := openTestDB(t)
	vault := [8]byte{1}
	first := [16]byte{1}
	second := [16]byte{2}
	writeMigrationEngram(t, db, vault, first, "policy.margin")
	writeMigrationEngram(t, db, vault, second, "policy.margin")

	if err := BackfillConceptIndex(db); err != nil {
		t.Fatalf("BackfillConceptIndex: %v", err)
	}
	if !hasConceptIndexKey(t, db, vault, first, "policy.margin") {
		t.Fatal("first concept index key missing")
	}
	if !hasConceptIndexKey(t, db, vault, second, "policy.margin") {
		t.Fatal("second concept index key missing")
	}
}

func TestBackfillConceptIndex_Idempotent(t *testing.T) {
	db := openTestDB(t)
	vault := [8]byte{1}
	id := [16]byte{1}
	writeMigrationEngram(t, db, vault, id, "policy.margin")

	if err := BackfillConceptIndex(db); err != nil {
		t.Fatalf("first backfill: %v", err)
	}
	if err := BackfillConceptIndex(db); err != nil {
		t.Fatalf("second backfill: %v", err)
	}
	if got := countConceptIndexKeys(t, db); got != 1 {
		t.Fatalf("concept index key count = %d, want 1", got)
	}
}

func TestBackfillConceptIndex_SkipsEmptyConcept(t *testing.T) {
	db := openTestDB(t)
	writeMigrationEngram(t, db, [8]byte{1}, [16]byte{1}, "")

	if err := BackfillConceptIndex(db); err != nil {
		t.Fatalf("BackfillConceptIndex: %v", err)
	}
	if got := countConceptIndexKeys(t, db); got != 0 {
		t.Fatalf("concept index key count = %d, want 0", got)
	}
}

func TestBackfillConceptIndex_IsolatesVaults(t *testing.T) {
	db := openTestDB(t)
	firstVault := [8]byte{1}
	secondVault := [8]byte{2}
	firstID := [16]byte{1}
	secondID := [16]byte{2}
	writeMigrationEngram(t, db, firstVault, firstID, "shared.concept")
	writeMigrationEngram(t, db, secondVault, secondID, "shared.concept")

	if err := BackfillConceptIndex(db); err != nil {
		t.Fatalf("BackfillConceptIndex: %v", err)
	}
	if !hasConceptIndexKey(t, db, firstVault, firstID, "shared.concept") {
		t.Fatal("first vault concept index key missing")
	}
	if !hasConceptIndexKey(t, db, secondVault, secondID, "shared.concept") {
		t.Fatal("second vault concept index key missing")
	}
	if hasConceptIndexKey(t, db, firstVault, secondID, "shared.concept") {
		t.Fatal("second vault ID leaked into first vault index")
	}
}

func TestBackfillConceptIndex_RepairsPartialIndex(t *testing.T) {
	db := openTestDB(t)
	vault := [8]byte{1}
	first := [16]byte{1}
	second := [16]byte{2}
	writeMigrationEngram(t, db, vault, first, "first")
	writeMigrationEngram(t, db, vault, second, "second")
	if err := db.Set(keys.ConceptIndexKey(vault, keys.Hash("first"), first), nil, pebble.Sync); err != nil {
		t.Fatalf("seed partial index: %v", err)
	}

	if err := BackfillConceptIndex(db); err != nil {
		t.Fatalf("BackfillConceptIndex: %v", err)
	}
	if !hasConceptIndexKey(t, db, vault, first, "first") || !hasConceptIndexKey(t, db, vault, second, "second") {
		t.Fatal("partial index was not fully repaired")
	}
	if got := countConceptIndexKeys(t, db); got != 2 {
		t.Fatalf("concept index key count = %d, want 2", got)
	}
}
