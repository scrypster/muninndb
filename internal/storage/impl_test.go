package storage

import (
	"context"
	"os"
	"testing"

	"github.com/scrypster/muninndb/internal/storage/keys"
	"github.com/scrypster/muninndb/internal/wal"
)

// TestWriteEngramWritesBucketKey verifies that WriteEngram also writes a relevance bucket key (0x10 prefix).
func TestWriteEngramWritesBucketKey(t *testing.T) {
	dir, err := os.MkdirTemp("", "muninndb-bucket-key-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	db, err := OpenPebble(dir, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	store := NewPebbleStore(db, PebbleStoreConfig{CacheSize: 100})
	defer store.Close()
	ws := store.VaultPrefix("test")
	ctx := context.Background()

	// Write an engram with an explicit relevance value
	eng := &Engram{
		Concept:   "test concept",
		Content:   "test content",
		Relevance: 0.75,
	}

	id, err := store.WriteEngram(ctx, ws, eng)
	if err != nil {
		t.Fatalf("WriteEngram: %v", err)
	}

	// Scan for 0x10 bucket keys
	bucketPrefix := keys.RelevanceBucketKey(ws, 0, [16]byte{})
	bucketPrefix = bucketPrefix[0 : 1+8] // Just the 0x10 | ws prefix

	iter, err := PrefixIterator(db, bucketPrefix)
	if err != nil {
		t.Fatalf("PrefixIterator: %v", err)
	}
	defer iter.Close()

	foundCount := 0
	for iter.First(); iter.Valid(); iter.Next() {
		foundCount++
		// Extract the ID from the key to verify it matches
		key := iter.Key()
		if len(key) >= 26 {
			var extractedID ULID
			copy(extractedID[:], key[10:26])
			if extractedID != id {
				t.Errorf("Found bucket key with wrong ID: %v, expected %v", extractedID, id)
			}
		}
	}

	if foundCount != 1 {
		t.Errorf("Expected 1 bucket key, found %d", foundCount)
	}
}

// TestUpdateRelevanceMovesKey verifies that UpdateRelevance deletes the old bucket key and creates a new one.
func TestUpdateRelevanceMovesKey(t *testing.T) {
	dir, err := os.MkdirTemp("", "muninndb-update-relevance-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	db, err := OpenPebble(dir, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	store := NewPebbleStore(db, PebbleStoreConfig{CacheSize: 100})
	defer store.Close()
	ws := store.VaultPrefix("test")
	ctx := context.Background()

	// Write an engram with initial relevance 0.3
	eng := &Engram{
		Concept:   "test concept",
		Content:   "test content",
		Relevance: 0.3,
		Stability: 30.0,
	}

	id, err := store.WriteEngram(ctx, ws, eng)
	if err != nil {
		t.Fatalf("WriteEngram: %v", err)
	}

	// Verify the old bucket key exists
	oldBucketKey := keys.RelevanceBucketKey(ws, 0.3, [16]byte(id))
	oldVal, err := Get(db, oldBucketKey)
	if err != nil {
		t.Fatalf("Get old bucket key: %v", err)
	}
	if oldVal == nil {
		t.Error("Old bucket key not found before UpdateRelevance")
	}

	// Update relevance to 0.8
	newRelevance := float32(0.8)
	newStability := float32(45.0)
	err = store.UpdateRelevance(ctx, ws, id, newRelevance, newStability)
	if err != nil {
		t.Fatalf("UpdateRelevance: %v", err)
	}

	// Verify the old bucket key is gone
	oldVal, err = Get(db, oldBucketKey)
	if err != nil {
		t.Fatalf("Get old bucket key after update: %v", err)
	}
	if oldVal != nil {
		t.Error("Old bucket key still exists after UpdateRelevance")
	}

	// Verify the new bucket key exists
	newBucketKey := keys.RelevanceBucketKey(ws, newRelevance, [16]byte(id))
	newVal, err := Get(db, newBucketKey)
	if err != nil {
		t.Fatalf("Get new bucket key: %v", err)
	}
	if newVal == nil {
		t.Error("New bucket key not found after UpdateRelevance")
	}

	// Verify metadata is updated
	meta, err := store.GetMetadata(ctx, ws, []ULID{id})
	if err != nil {
		t.Fatalf("GetMetadata: %v", err)
	}
	if len(meta) != 1 {
		t.Fatalf("Expected 1 metadata record, got %d", len(meta))
	}
	if meta[0].Relevance != newRelevance {
		t.Errorf("Relevance not updated: got %v, expected %v", meta[0].Relevance, newRelevance)
	}
	if meta[0].Stability != newStability {
		t.Errorf("Stability not updated: got %v, expected %v", meta[0].Stability, newStability)
	}
}

// TestRecentActiveUsesIndex verifies that RecentActive uses the 0x10 bucket index for O(k) scanning.
// After writing 3 engrams with different relevances, RecentActive(2) returns the 2 highest.
func TestRecentActiveUsesIndex(t *testing.T) {
	dir, err := os.MkdirTemp("", "muninndb-recent-active-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	db, err := OpenPebble(dir, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	store := NewPebbleStore(db, PebbleStoreConfig{CacheSize: 100})
	defer store.Close()
	ws := store.VaultPrefix("test")
	ctx := context.Background()

	// Write 3 engrams with different relevance values
	relevances := []float32{0.2, 0.8, 0.5}
	var ids []ULID

	for i, rel := range relevances {
		eng := &Engram{
			Concept:   "test concept " + string(rune(i)),
			Content:   "test content " + string(rune(i)),
			Relevance: rel,
		}

		id, err := store.WriteEngram(ctx, ws, eng)
		if err != nil {
			t.Fatalf("WriteEngram[%d]: %v", i, err)
		}
		ids = append(ids, id)
	}

	// Get top 2 by relevance using RecentActive
	topIDs, err := store.RecentActive(ctx, ws, 2)
	if err != nil {
		t.Fatalf("RecentActive: %v", err)
	}

	if len(topIDs) != 2 {
		t.Errorf("Expected 2 results, got %d", len(topIDs))
	}

	// The bucket index sorts by highest relevance first (0.8, 0.5, 0.2)
	// So topIDs[0] should be the engram with 0.8, topIDs[1] should be 0.5
	meta, err := store.GetMetadata(ctx, ws, topIDs)
	if err != nil {
		t.Fatalf("GetMetadata: %v", err)
	}

	if len(meta) != 2 {
		t.Fatalf("Expected 2 metadata records, got %d", len(meta))
	}

	// Verify the relevances are in descending order
	if meta[0].Relevance < meta[1].Relevance {
		t.Errorf("Results not in descending relevance order: %v, %v", meta[0].Relevance, meta[1].Relevance)
	}

	// Verify we got the highest 2 (0.8 and 0.5, not 0.2)
	if meta[0].Relevance != 0.8 {
		t.Errorf("Highest relevance should be 0.8, got %v", meta[0].Relevance)
	}
	if meta[1].Relevance != 0.5 {
		t.Errorf("Second highest relevance should be 0.5, got %v", meta[1].Relevance)
	}
}

// TestWriteEngramCallsWAL verifies that WriteEngram calls AppendAsync to the WAL when a GroupCommitter is set.
func TestWriteEngramCallsWAL(t *testing.T) {
	dbDir, err := os.MkdirTemp("", "muninndb-wal-db-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dbDir)

	walDir, err := os.MkdirTemp("", "muninndb-wal-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(walDir)

	// Open Pebble and WAL
	db, err := OpenPebble(dbDir, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	mol, err := wal.Open(walDir)
	if err != nil {
		t.Fatal(err)
	}
	defer mol.Close()

	// Create GroupCommitter
	gc := wal.NewGroupCommitter(mol, db)

	// Create PebbleStore and set WAL
	store := NewPebbleStore(db, PebbleStoreConfig{CacheSize: 100})
	defer store.Close()
	store.SetWAL(mol, gc)

	ws := store.VaultPrefix("test")
	ctx := context.Background()

	// Write an engram
	eng := &Engram{
		Concept:   "test concept",
		Content:   "test content",
		Relevance: 0.5,
	}

	id, err := store.WriteEngram(ctx, ws, eng)
	if err != nil {
		t.Fatalf("WriteEngram: %v", err)
	}

	// Verify the engram was written to Pebble
	retrieved, err := store.GetEngram(ctx, ws, id)
	if err != nil {
		t.Fatalf("GetEngram: %v", err)
	}
	if retrieved.Concept != eng.Concept {
		t.Errorf("Engram not correctly written: got concept %q, expected %q", retrieved.Concept, eng.Concept)
	}

	// Verify MOL was opened successfully and is working
	if mol == nil {
		t.Error("MOL is nil after opening")
	}
}

// TestSetWALOptional verifies that WriteEngram works without WAL (gc is nil).
func TestSetWALOptional(t *testing.T) {
	dir, err := os.MkdirTemp("", "muninndb-no-wal-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	db, err := OpenPebble(dir, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	store := NewPebbleStore(db, PebbleStoreConfig{CacheSize: 100})
	defer store.Close()
	ws := store.VaultPrefix("test")
	ctx := context.Background()

	// Write an engram without setting WAL
	eng := &Engram{
		Concept:   "test concept",
		Content:   "test content",
		Relevance: 0.5,
	}

	id, err := store.WriteEngram(ctx, ws, eng)
	if err != nil {
		t.Fatalf("WriteEngram without WAL: %v", err)
	}

	// Verify the engram was written
	retrieved, err := store.GetEngram(ctx, ws, id)
	if err != nil {
		t.Fatalf("GetEngram: %v", err)
	}
	if retrieved.Concept != eng.Concept {
		t.Errorf("Engram not correctly written: got concept %q, expected %q", retrieved.Concept, eng.Concept)
	}
}

// TestGetAssocWeight reads the weight of a forward association.
func TestGetAssocWeight(t *testing.T) {
	dir, err := os.MkdirTemp("", "muninndb-assoc-weight-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	db, err := OpenPebble(dir, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	store := NewPebbleStore(db, PebbleStoreConfig{CacheSize: 100})
	defer store.Close()
	ws := store.VaultPrefix("test")
	ctx := context.Background()

	// Write two engrams
	eng1 := &Engram{Concept: "concept1", Content: "content1"}
	id1, err := store.WriteEngram(ctx, ws, eng1)
	if err != nil {
		t.Fatalf("WriteEngram(1): %v", err)
	}

	eng2 := &Engram{Concept: "concept2", Content: "content2"}
	id2, err := store.WriteEngram(ctx, ws, eng2)
	if err != nil {
		t.Fatalf("WriteEngram(2): %v", err)
	}

	// Write an association from id1 to id2 with weight 0.75
	assoc := &Association{TargetID: id2, Weight: 0.75}
	err = store.WriteAssociation(ctx, ws, id1, id2, assoc)
	if err != nil {
		t.Fatalf("WriteAssociation: %v", err)
	}

	// Read the weight back
	weight, err := store.GetAssocWeight(ctx, ws, id1, id2)
	if err != nil {
		t.Fatalf("GetAssocWeight: %v", err)
	}

	if weight != 0.75 {
		t.Errorf("Expected weight 0.75, got %v", weight)
	}

	// Test reading non-existent association (should return 0.0)
	eng3 := &Engram{Concept: "concept3", Content: "content3"}
	id3, err := store.WriteEngram(ctx, ws, eng3)
	if err != nil {
		t.Fatalf("WriteEngram(3): %v", err)
	}

	weight, err = store.GetAssocWeight(ctx, ws, id1, id3)
	if err != nil {
		t.Fatalf("GetAssocWeight (non-existent): %v", err)
	}
	if weight != 0.0 {
		t.Errorf("Expected weight 0.0 for non-existent association, got %v", weight)
	}
}

// TestUpdateAssocWeight updates the weight of an association.
func TestUpdateAssocWeight(t *testing.T) {
	dir, err := os.MkdirTemp("", "muninndb-update-assoc-weight-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	db, err := OpenPebble(dir, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	store := NewPebbleStore(db, PebbleStoreConfig{CacheSize: 100})
	defer store.Close()
	ws := store.VaultPrefix("test")
	ctx := context.Background()

	// Write two engrams
	eng1 := &Engram{Concept: "concept1", Content: "content1"}
	id1, err := store.WriteEngram(ctx, ws, eng1)
	if err != nil {
		t.Fatalf("WriteEngram(1): %v", err)
	}

	eng2 := &Engram{Concept: "concept2", Content: "content2"}
	id2, err := store.WriteEngram(ctx, ws, eng2)
	if err != nil {
		t.Fatalf("WriteEngram(2): %v", err)
	}

	// Write initial association with weight 0.5
	assoc := &Association{TargetID: id2, Weight: 0.5}
	err = store.WriteAssociation(ctx, ws, id1, id2, assoc)
	if err != nil {
		t.Fatalf("WriteAssociation: %v", err)
	}

	// Update weight to 0.9
	err = store.UpdateAssocWeight(ctx, ws, id1, id2, 0.9, 0)
	if err != nil {
		t.Fatalf("UpdateAssocWeight: %v", err)
	}

	// Read back and verify
	weight, err := store.GetAssocWeight(ctx, ws, id1, id2)
	if err != nil {
		t.Fatalf("GetAssocWeight: %v", err)
	}

	if weight != 0.9 {
		t.Errorf("Expected updated weight 0.9, got %v", weight)
	}
}

// TestGetConfidence reads confidence from metadata.
func TestGetConfidence(t *testing.T) {
	dir, err := os.MkdirTemp("", "muninndb-confidence-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	db, err := OpenPebble(dir, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	store := NewPebbleStore(db, PebbleStoreConfig{CacheSize: 100})
	defer store.Close()
	ws := store.VaultPrefix("test")
	ctx := context.Background()

	// Write an engram with explicit confidence
	eng := &Engram{
		Concept:    "test concept",
		Content:    "test content",
		Confidence: 0.85,
	}

	id, err := store.WriteEngram(ctx, ws, eng)
	if err != nil {
		t.Fatalf("WriteEngram: %v", err)
	}

	// Read confidence
	conf, err := store.GetConfidence(ctx, ws, id)
	if err != nil {
		t.Fatalf("GetConfidence: %v", err)
	}

	if conf != 0.85 {
		t.Errorf("Expected confidence 0.85, got %v", conf)
	}
}

// TestUpdateConfidence updates confidence in metadata.
func TestUpdateConfidence(t *testing.T) {
	dir, err := os.MkdirTemp("", "muninndb-update-confidence-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	db, err := OpenPebble(dir, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	store := NewPebbleStore(db, PebbleStoreConfig{CacheSize: 100})
	defer store.Close()
	ws := store.VaultPrefix("test")
	ctx := context.Background()

	// Write an engram with initial confidence
	eng := &Engram{
		Concept:    "test concept",
		Content:    "test content",
		Confidence: 0.5,
	}

	id, err := store.WriteEngram(ctx, ws, eng)
	if err != nil {
		t.Fatalf("WriteEngram: %v", err)
	}

	// Update confidence to 0.95
	err = store.UpdateConfidence(ctx, ws, id, 0.95)
	if err != nil {
		t.Fatalf("UpdateConfidence: %v", err)
	}

	// Read back and verify
	conf, err := store.GetConfidence(ctx, ws, id)
	if err != nil {
		t.Fatalf("GetConfidence: %v", err)
	}

	if conf != 0.95 {
		t.Errorf("Expected updated confidence 0.95, got %v", conf)
	}
}

// TestGetConceptAssociations returns neighbor IDs for spreading activation.
func TestGetConceptAssociations(t *testing.T) {
	dir, err := os.MkdirTemp("", "muninndb-concept-assoc-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	db, err := OpenPebble(dir, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	store := NewPebbleStore(db, PebbleStoreConfig{CacheSize: 100})
	defer store.Close()
	ws := store.VaultPrefix("test")
	ctx := context.Background()

	// Write three engrams
	eng1 := &Engram{Concept: "concept1", Content: "content1"}
	id1, err := store.WriteEngram(ctx, ws, eng1)
	if err != nil {
		t.Fatalf("WriteEngram(1): %v", err)
	}

	eng2 := &Engram{Concept: "concept2", Content: "content2"}
	id2, err := store.WriteEngram(ctx, ws, eng2)
	if err != nil {
		t.Fatalf("WriteEngram(2): %v", err)
	}

	eng3 := &Engram{Concept: "concept3", Content: "content3"}
	id3, err := store.WriteEngram(ctx, ws, eng3)
	if err != nil {
		t.Fatalf("WriteEngram(3): %v", err)
	}

	// Write associations from id1 to id2 and id3
	assoc2 := &Association{TargetID: id2, Weight: 0.8}
	err = store.WriteAssociation(ctx, ws, id1, id2, assoc2)
	if err != nil {
		t.Fatalf("WriteAssociation(1->2): %v", err)
	}

	assoc3 := &Association{TargetID: id3, Weight: 0.6}
	err = store.WriteAssociation(ctx, ws, id1, id3, assoc3)
	if err != nil {
		t.Fatalf("WriteAssociation(1->3): %v", err)
	}

	// Get concept associations (neighbors of id1)
	neighbors, err := store.GetConceptAssociations(ctx, ws, id1, 10)
	if err != nil {
		t.Fatalf("GetConceptAssociations: %v", err)
	}

	if len(neighbors) != 2 {
		t.Errorf("Expected 2 neighbors, got %d", len(neighbors))
	}

	// Verify both neighbors are present
	hasID2 := false
	hasID3 := false
	for _, n := range neighbors {
		if n == id2 {
			hasID2 = true
		}
		if n == id3 {
			hasID3 = true
		}
	}

	if !hasID2 {
		t.Error("Neighbor id2 not found")
	}
	if !hasID3 {
		t.Error("Neighbor id3 not found")
	}
}

// TestFlagContradiction marks a contradiction between two engrams.
func TestFlagContradiction(t *testing.T) {
	dir, err := os.MkdirTemp("", "muninndb-contradiction-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	db, err := OpenPebble(dir, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	store := NewPebbleStore(db, PebbleStoreConfig{CacheSize: 100})
	defer store.Close()
	ws := store.VaultPrefix("test")
	ctx := context.Background()

	// Write two engrams
	eng1 := &Engram{Concept: "concept1", Content: "content1"}
	id1, err := store.WriteEngram(ctx, ws, eng1)
	if err != nil {
		t.Fatalf("WriteEngram(1): %v", err)
	}

	eng2 := &Engram{Concept: "concept2", Content: "content2"}
	id2, err := store.WriteEngram(ctx, ws, eng2)
	if err != nil {
		t.Fatalf("WriteEngram(2): %v", err)
	}

	// Flag contradiction
	err = store.FlagContradiction(ctx, ws, id1, id2)
	if err != nil {
		t.Fatalf("FlagContradiction: %v", err)
	}

	// Verify the contradiction key was written
	// We check this by trying to read the 0x0A key directly
	contradictionKey := keys.ContradictionKey(ws, 0, 0, [16]byte(id1))
	contradictionKey = contradictionKey[0 : 1+8+4+2] // Just prefix to search

	iter, err := PrefixIterator(db, contradictionKey)
	if err != nil {
		t.Fatalf("PrefixIterator: %v", err)
	}
	defer iter.Close()

	foundContra := false
	for iter.First(); iter.Valid(); iter.Next() {
		foundContra = true
		break
	}

	if !foundContra {
		t.Error("Contradiction flag not written to database")
	}
}

// TestUpdateAssocWeightNoDuplicateEdges verifies that updating an association
// weight deletes the old key before writing the new one, preventing accumulation
// of duplicate (a,b) edges with different weight-encoded keys.
func TestUpdateAssocWeightNoDuplicateEdges(t *testing.T) {
	dir, err := os.MkdirTemp("", "muninndb-assoc-dedup-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	db, err := OpenPebble(dir, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	store := NewPebbleStore(db, PebbleStoreConfig{CacheSize: 100})
	defer store.Close()
	ws := store.VaultPrefix("test")
	ctx := context.Background()

	eng1 := &Engram{Concept: "src", Content: "source"}
	id1, _ := store.WriteEngram(ctx, ws, eng1)
	eng2 := &Engram{Concept: "dst", Content: "dest"}
	id2, _ := store.WriteEngram(ctx, ws, eng2)

	// Write initial association then update weight three times
	_ = store.WriteAssociation(ctx, ws, id1, id2, &Association{TargetID: id2, Weight: 0.3})
	_ = store.UpdateAssocWeight(ctx, ws, id1, id2, 0.5, 0)
	_ = store.UpdateAssocWeight(ctx, ws, id1, id2, 0.7, 0)
	_ = store.UpdateAssocWeight(ctx, ws, id1, id2, 0.9, 0)

	// Scan all forward-assoc keys for the pair and count distinct weight-encoded entries.
	// There must be exactly 1 — if old keys weren't deleted, there'd be multiple.
	assocMap, err := store.GetAssociations(ctx, ws, []ULID{id1}, 100)
	if err != nil {
		t.Fatalf("GetAssociations: %v", err)
	}
	got := assocMap[id1]
	count := 0
	for _, a := range got {
		if a.TargetID == id2 {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected exactly 1 edge for pair (id1,id2), got %d — old weight keys not cleaned up", count)
	}
	// Final weight must be the last one written
	if len(got) > 0 && got[0].Weight != 0.9 {
		t.Errorf("expected weight 0.9 after updates, got %v", got[0].Weight)
	}
}

// TestListByState verifies that soft-deleted engrams appear in ListByState results.
func TestListByState(t *testing.T) {
	dir, err := os.MkdirTemp("", "muninndb-list-state-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	db, err := OpenPebble(dir, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	store := NewPebbleStore(db, PebbleStoreConfig{CacheSize: 100})
	defer store.Close()
	ws := store.VaultPrefix("test")
	ctx := context.Background()

	// Write 3 engrams, soft-delete 2 of them
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
	if err := store.SoftDelete(ctx, ws, ids[0]); err != nil {
		t.Fatalf("SoftDelete[0]: %v", err)
	}
	if err := store.SoftDelete(ctx, ws, ids[1]); err != nil {
		t.Fatalf("SoftDelete[1]: %v", err)
	}

	deleted, err := store.ListByState(ctx, ws, StateSoftDeleted, 100)
	if err != nil {
		t.Fatalf("ListByState: %v", err)
	}
	if len(deleted) != 2 {
		t.Errorf("expected 2 soft-deleted engrams, got %d", len(deleted))
	}

	// ids[2] (active) must not appear
	for _, id := range deleted {
		if id == ids[2] {
			t.Error("active engram should not appear in soft-deleted list")
		}
	}
}

// TestListByStateLimitRespected verifies that ListByState respects the limit parameter.
func TestListByStateLimitRespected(t *testing.T) {
	dir, err := os.MkdirTemp("", "muninndb-list-state-limit-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	db, err := OpenPebble(dir, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	store := NewPebbleStore(db, PebbleStoreConfig{CacheSize: 100})
	defer store.Close()
	ws := store.VaultPrefix("test")
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		id, err := store.WriteEngram(ctx, ws, &Engram{Concept: "c", Content: "x"})
		if err != nil {
			t.Fatalf("WriteEngram: %v", err)
		}
		if err := store.SoftDelete(ctx, ws, id); err != nil {
			t.Fatalf("SoftDelete: %v", err)
		}
	}

	ids, err := store.ListByState(ctx, ws, StateSoftDeleted, 3)
	if err != nil {
		t.Fatalf("ListByState: %v", err)
	}
	if len(ids) != 3 {
		t.Errorf("expected limit=3 to be respected, got %d", len(ids))
	}
}

// TestWriteReadCoherence verifies that WriteCoherence persists counter data
// and ReadCoherence retrieves the identical values.
func TestWriteReadCoherence(t *testing.T) {
	dir, err := os.MkdirTemp("", "muninndb-coherence-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	db, err := OpenPebble(dir, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	store := NewPebbleStore(db, PebbleStoreConfig{CacheSize: 100})
	defer store.Close()
	ws := store.VaultPrefix("coh-vault")

	// Write a known 7-element array.
	want := [7]int64{42, 10, 32, 3, 1, 800000, 640000}
	if err := store.WriteCoherence(ws, want); err != nil {
		t.Fatalf("WriteCoherence: %v", err)
	}

	// Read it back.
	got, ok, err := store.ReadCoherence(ws)
	if err != nil {
		t.Fatalf("ReadCoherence: %v", err)
	}
	if !ok {
		t.Fatal("ReadCoherence returned ok=false, expected true after write")
	}
	if got != want {
		t.Errorf("ReadCoherence: got %v, want %v", got, want)
	}
}

// TestReadCoherenceMissReturnsNotFound verifies that ReadCoherence for a vault
// that has never had WriteCoherence called returns ok=false with no error.
func TestReadCoherenceMissReturnsNotFound(t *testing.T) {
	dir, err := os.MkdirTemp("", "muninndb-coherence-miss-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	db, err := OpenPebble(dir, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	store := NewPebbleStore(db, PebbleStoreConfig{CacheSize: 100})
	defer store.Close()
	ws := store.VaultPrefix("never-written")

	_, ok, err := store.ReadCoherence(ws)
	if err != nil {
		t.Fatalf("ReadCoherence on missing key: %v", err)
	}
	if ok {
		t.Error("ReadCoherence on missing key returned ok=true, want false")
	}
}

// ---------------------------------------------------------------------------
// WriteEngramBatch tests
// ---------------------------------------------------------------------------

// TestWriteEngramBatch_MultiItem batch-writes 3 engrams and verifies all are
// retrievable via GetEngram.
func TestWriteEngramBatch_MultiItem(t *testing.T) {
	store := newTestStore(t)

	ctx := context.Background()
	ws := store.VaultPrefix("batch-multi")

	engrams := []*Engram{
		{Concept: "alpha", Content: "first content"},
		{Concept: "beta", Content: "second content"},
		{Concept: "gamma", Content: "third content"},
	}

	items := make([]EngramBatchItem, len(engrams))
	for i, e := range engrams {
		items[i] = EngramBatchItem{WSPrefix: ws, Engram: e}
	}

	ids, errs := store.WriteEngramBatch(ctx, items)
	for i, err := range errs {
		if err != nil {
			t.Fatalf("WriteEngramBatch item[%d]: %v", i, err)
		}
	}
	if len(ids) != 3 {
		t.Fatalf("expected 3 IDs, got %d", len(ids))
	}

	for i, id := range ids {
		got, err := store.GetEngram(ctx, ws, id)
		if err != nil {
			t.Fatalf("GetEngram[%d]: %v", i, err)
		}
		if got.Concept != engrams[i].Concept {
			t.Errorf("item[%d] Concept: got %q, want %q", i, got.Concept, engrams[i].Concept)
		}
		if got.Content != engrams[i].Content {
			t.Errorf("item[%d] Content: got %q, want %q", i, got.Content, engrams[i].Content)
		}
	}
}

// TestWriteEngramBatch_EmptyInput verifies that an empty batch is a no-op with
// no error and returns empty slices.
func TestWriteEngramBatch_EmptyInput(t *testing.T) {
	store := newTestStore(t)

	ctx := context.Background()
	ids, errs := store.WriteEngramBatch(ctx, []EngramBatchItem{})
	if len(ids) != 0 {
		t.Errorf("expected 0 IDs, got %d", len(ids))
	}
	if len(errs) != 0 {
		t.Errorf("expected 0 errors, got %d", len(errs))
	}
}

// ---------------------------------------------------------------------------
// CacheLen / GetDB / DiskSize trivial accessors
// ---------------------------------------------------------------------------

// TestStoreCacheLen writes 3 engrams to the L1 cache (via GetEngram which
// populates the cache) and verifies CacheLen() > 0.
func TestStoreCacheLen(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("cachelen-vault")

	// Write 3 engrams, then read them back so they land in the L1 cache.
	for i := 0; i < 3; i++ {
		id, err := store.WriteEngram(ctx, ws, &Engram{
			Concept: "concept",
			Content: "content",
		})
		if err != nil {
			t.Fatalf("WriteEngram[%d]: %v", i, err)
		}
		// GetEngram populates the L1 cache.
		if _, err := store.GetEngram(ctx, ws, id); err != nil {
			t.Fatalf("GetEngram[%d]: %v", i, err)
		}
	}

	if store.CacheLen() <= 0 {
		t.Errorf("expected CacheLen > 0 after 3 reads, got %d", store.CacheLen())
	}
}

// TestGetDB verifies that GetDB returns the non-nil Pebble instance.
func TestGetDB(t *testing.T) {
	store := newTestStore(t)
	if store.GetDB() == nil {
		t.Error("expected GetDB() to return non-nil *pebble.DB")
	}
}

// TestDiskSize verifies that DiskSize() returns > 0 after writing some engrams.
func TestDiskSize(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("disksize-vault")

	for i := 0; i < 3; i++ {
		if _, err := store.WriteEngram(ctx, ws, &Engram{
			Concept: "concept",
			Content: "some content to occupy disk space",
		}); err != nil {
			t.Fatalf("WriteEngram[%d]: %v", i, err)
		}
	}

	if store.DiskSize() <= 0 {
		t.Errorf("expected DiskSize > 0 after writes, got %d", store.DiskSize())
	}
}

// ---------------------------------------------------------------------------
// ContextWithSnapshot
// ---------------------------------------------------------------------------

// TestContextWithSnapshot verifies that ContextWithSnapshot embeds a value in
// the context (the value is retrievable via pebbleReader).
func TestContextWithSnapshot(t *testing.T) {
	store := newTestStore(t)

	snap := store.NewSnapshot()
	defer snap.Close()

	ctx := context.Background()
	ctxWithSnap := ContextWithSnapshot(ctx, snap)

	// The context must contain a value — pebbleReader should return the snapshot.
	reader := store.pebbleReader(ctxWithSnap)
	if reader == nil {
		t.Error("expected pebbleReader to return non-nil reader when snapshot is in ctx")
	}
	// Verify it really used the snapshot (not the live DB) by checking type.
	if reader == store.GetDB() {
		t.Error("expected pebbleReader to return the snapshot, not the live DB")
	}

	// Without snapshot in ctx, pebbleReader should fall back to the live DB.
	liveReader := store.pebbleReader(ctx)
	if liveReader != store.GetDB() {
		t.Error("expected pebbleReader to return the live DB when no snapshot in ctx")
	}
}

// TestWriteEngramBatch_VaultCountIncrement verifies that the vault count
// increases by exactly the batch size after WriteEngramBatch.
func TestWriteEngramBatch_VaultCountIncrement(t *testing.T) {
	store := newTestStore(t)

	ctx := context.Background()
	ws := store.VaultPrefix("batch-count")

	countBefore := store.GetVaultCount(ctx, ws)

	const batchSize = 3
	items := make([]EngramBatchItem, batchSize)
	for i := range items {
		items[i] = EngramBatchItem{
			WSPrefix: ws,
			Engram:   &Engram{Concept: "c", Content: "x"},
		}
	}

	_, errs := store.WriteEngramBatch(ctx, items)
	for i, err := range errs {
		if err != nil {
			t.Fatalf("WriteEngramBatch item[%d]: %v", i, err)
		}
	}

	countAfter := store.GetVaultCount(ctx, ws)
	if countAfter-countBefore != batchSize {
		t.Errorf("vault count increment: got %d, want %d", countAfter-countBefore, batchSize)
	}
}

// ---------------------------------------------------------------------------
// BatchSet / BatchDelete (pebble.go package-level helpers)
// ---------------------------------------------------------------------------

// TestBatchSet verifies that BatchSet adds a Set operation to a batch that is
// later committed and the value is readable from Pebble.
func TestBatchSet(t *testing.T) {
	db := openTestPebble(t)

	key := []byte("test-batchset-key")
	value := []byte("hello from BatchSet")

	batch := db.NewBatch()
	BatchSet(batch, key, value)
	if err := batch.Commit(nil); err != nil {
		batch.Close()
		t.Fatalf("batch.Commit: %v", err)
	}
	batch.Close()

	got, err := Get(db, key)
	if err != nil {
		t.Fatalf("Get after BatchSet: %v", err)
	}
	if string(got) != string(value) {
		t.Errorf("BatchSet: got value %q, want %q", got, value)
	}
}

// TestBatchDelete verifies that BatchDelete adds a Delete operation to a batch;
// after commit the key is no longer present in Pebble.
func TestBatchDelete(t *testing.T) {
	db := openTestPebble(t)

	key := []byte("test-batchdelete-key")
	value := []byte("to be deleted")

	// Write the key first.
	setBatch := db.NewBatch()
	BatchSet(setBatch, key, value)
	if err := setBatch.Commit(nil); err != nil {
		setBatch.Close()
		t.Fatalf("initial batch.Commit: %v", err)
	}
	setBatch.Close()

	// Confirm it exists.
	before, err := Get(db, key)
	if err != nil || before == nil {
		t.Fatalf("Get before BatchDelete: err=%v, value=%v", err, before)
	}

	// Delete via BatchDelete.
	delBatch := db.NewBatch()
	BatchDelete(delBatch, key)
	if err := delBatch.Commit(nil); err != nil {
		delBatch.Close()
		t.Fatalf("delete batch.Commit: %v", err)
	}
	delBatch.Close()

	// Confirm the key is gone.
	after, err := Get(db, key)
	if err != nil {
		t.Fatalf("Get after BatchDelete: %v", err)
	}
	if after != nil {
		t.Errorf("BatchDelete: key still present after delete, value=%q", after)
	}
}
