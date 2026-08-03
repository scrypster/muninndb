package storage_test

import (
	"context"
	"encoding/binary"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cockroachdb/pebble"
	"github.com/scrypster/muninndb/internal/prefix"
	"github.com/scrypster/muninndb/internal/replication"
	"github.com/scrypster/muninndb/internal/storage"
)

// openUpgradeTestDB opens a PebbleStore at the given directory. The caller is
// responsible for closing the returned store (which also closes the underlying
// Pebble DB).
func openUpgradeTestDB(t *testing.T, dir string) (*storage.PebbleStore, *pebble.DB) {
	t.Helper()
	db, err := storage.OpenPebble(dir, storage.DefaultOptions())
	if err != nil {
		t.Fatalf("OpenPebble(%s): %v", dir, err)
	}
	store := storage.NewPebbleStore(db, storage.PebbleStoreConfig{CacheSize: 0})
	return store, db
}

// TestUpgradeTest_DataSurvivesRestart writes multiple engrams with varying
// concepts, tags, and content, closes the DB, reopens it, and verifies all
// data is intact.
func TestUpgradeTest_DataSurvivesRestart(t *testing.T) {
	dir, err := os.MkdirTemp("", "muninndb-upgrade-data-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	ctx := context.Background()

	// --- Phase 1: write data ---
	store, _ := openUpgradeTestDB(t, dir)
	ws := store.VaultPrefix("test")

	engrams := []*storage.Engram{
		{Concept: "go-concurrency", Content: "Goroutines are lightweight threads", Tags: []string{"go", "concurrency"}},
		{Concept: "rust-ownership", Content: "Rust enforces ownership at compile time", Tags: []string{"rust", "memory"}},
		{Concept: "sql-joins", Content: "INNER JOIN returns matching rows", Tags: []string{"sql", "databases"}},
	}

	ids := make([]storage.ULID, len(engrams))
	for i, eng := range engrams {
		id, err := store.WriteEngram(ctx, ws, eng)
		if err != nil {
			t.Fatalf("WriteEngram[%d]: %v", i, err)
		}
		ids[i] = id
	}

	store.Close()

	// --- Phase 2: reopen and verify ---
	store2, _ := openUpgradeTestDB(t, dir)
	defer store2.Close()
	ws2 := store2.VaultPrefix("test")

	for i, id := range ids {
		got, err := store2.GetEngram(ctx, ws2, id)
		if err != nil {
			t.Fatalf("GetEngram[%d] after reopen: %v", i, err)
		}
		if got.Concept != engrams[i].Concept {
			t.Errorf("engram[%d] concept = %q, want %q", i, got.Concept, engrams[i].Concept)
		}
		if got.Content != engrams[i].Content {
			t.Errorf("engram[%d] content = %q, want %q", i, got.Content, engrams[i].Content)
		}
		if len(got.Tags) != len(engrams[i].Tags) {
			t.Errorf("engram[%d] tags = %v, want %v", i, got.Tags, engrams[i].Tags)
		}
	}
}

// TestUpgradeTest_SchemaVersionSet writes data, closes the DB, reopens it, and
// verifies that CheckAndSetSchemaVersion succeeds (same version round-trip).
func TestUpgradeTest_SchemaVersionSet(t *testing.T) {
	dir, err := os.MkdirTemp("", "muninndb-upgrade-schema-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	ctx := context.Background()

	// --- Phase 1: open, set schema version, write an engram ---
	store, db := openUpgradeTestDB(t, dir)
	if err := replication.CheckAndSetSchemaVersion(db); err != nil {
		t.Fatalf("CheckAndSetSchemaVersion (first open): %v", err)
	}
	ws := store.VaultPrefix("test")
	_, err = store.WriteEngram(ctx, ws, &storage.Engram{Concept: "schema-test", Content: "data"})
	if err != nil {
		t.Fatalf("WriteEngram: %v", err)
	}
	store.Close()

	// --- Phase 2: reopen and verify schema version is still accepted ---
	_, db2 := openUpgradeTestDB(t, dir)
	defer db2.Close()

	if err := replication.CheckAndSetSchemaVersion(db2); err != nil {
		t.Fatalf("CheckAndSetSchemaVersion (reopen): %v", err)
	}
}

// TestUpgradeTest_AssociationsSurviveRestart writes engrams with associations,
// closes the DB, reopens it, and verifies associations are readable.
func TestUpgradeTest_AssociationsSurviveRestart(t *testing.T) {
	dir, err := os.MkdirTemp("", "muninndb-upgrade-assoc-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	ctx := context.Background()

	// --- Phase 1: write engrams and associations ---
	store, _ := openUpgradeTestDB(t, dir)
	ws := store.VaultPrefix("test")

	id1, err := store.WriteEngram(ctx, ws, &storage.Engram{Concept: "node-a", Content: "first node"})
	if err != nil {
		t.Fatalf("WriteEngram(1): %v", err)
	}
	id2, err := store.WriteEngram(ctx, ws, &storage.Engram{Concept: "node-b", Content: "second node"})
	if err != nil {
		t.Fatalf("WriteEngram(2): %v", err)
	}
	id3, err := store.WriteEngram(ctx, ws, &storage.Engram{Concept: "node-c", Content: "third node"})
	if err != nil {
		t.Fatalf("WriteEngram(3): %v", err)
	}

	if err := store.WriteAssociation(ctx, ws, id1, id2, &storage.Association{TargetID: id2, Weight: 0.8}); err != nil {
		t.Fatalf("WriteAssociation(1->2): %v", err)
	}
	if err := store.WriteAssociation(ctx, ws, id1, id3, &storage.Association{TargetID: id3, Weight: 0.6}); err != nil {
		t.Fatalf("WriteAssociation(1->3): %v", err)
	}

	store.Close()

	// --- Phase 2: reopen and verify ---
	store2, _ := openUpgradeTestDB(t, dir)
	defer store2.Close()
	ws2 := store2.VaultPrefix("test")

	assocMap, err := store2.GetAssociations(ctx, ws2, []storage.ULID{id1}, 100)
	if err != nil {
		t.Fatalf("GetAssociations after reopen: %v", err)
	}

	assocs := assocMap[id1]
	if len(assocs) != 2 {
		t.Fatalf("expected 2 associations for id1, got %d", len(assocs))
	}

	targets := map[storage.ULID]float32{}
	for _, a := range assocs {
		targets[a.TargetID] = a.Weight
	}
	if w, ok := targets[id2]; !ok {
		t.Error("association to id2 not found after reopen")
	} else if w != 0.8 {
		t.Errorf("association to id2 weight = %v, want 0.8", w)
	}
	if w, ok := targets[id3]; !ok {
		t.Error("association to id3 not found after reopen")
	} else if w != 0.6 {
		t.Errorf("association to id3 weight = %v, want 0.6", w)
	}
}

// TestUpgradeTest_IndexesSurviveRestart writes engrams with tags, closes the DB,
// reopens it, and verifies tag indexes and state indexes still work.
func TestUpgradeTest_IndexesSurviveRestart(t *testing.T) {
	dir, err := os.MkdirTemp("", "muninndb-upgrade-idx-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	ctx := context.Background()

	// --- Phase 1: write engrams with tags and known states ---
	store, _ := openUpgradeTestDB(t, dir)
	ws := store.VaultPrefix("test")

	id1, err := store.WriteEngram(ctx, ws, &storage.Engram{
		Concept: "tagged-a",
		Content: "content a",
		Tags:    []string{"alpha", "shared"},
	})
	if err != nil {
		t.Fatalf("WriteEngram(1): %v", err)
	}

	id2, err := store.WriteEngram(ctx, ws, &storage.Engram{
		Concept: "tagged-b",
		Content: "content b",
		Tags:    []string{"beta", "shared"},
	})
	if err != nil {
		t.Fatalf("WriteEngram(2): %v", err)
	}

	// Soft-delete id2 to test state index
	if err := store.SoftDelete(ctx, ws, id2); err != nil {
		t.Fatalf("SoftDelete: %v", err)
	}

	store.Close()

	// --- Phase 2: reopen and verify indexes ---
	store2, _ := openUpgradeTestDB(t, dir)
	defer store2.Close()
	ws2 := store2.VaultPrefix("test")

	// Tag index: "shared" should return both engrams
	epoch := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	far := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	tagIDs, err := store2.ListByTagInRange(ctx, ws2, "shared", epoch, far, 100)
	if err != nil {
		t.Fatalf("ListByTagInRange(shared) after reopen: %v", err)
	}
	if len(tagIDs) != 2 {
		t.Errorf("expected 2 engrams with tag 'shared', got %d", len(tagIDs))
	}

	// Tag index: "alpha" should return only id1
	alphaIDs, err := store2.ListByTagInRange(ctx, ws2, "alpha", epoch, far, 100)
	if err != nil {
		t.Fatalf("ListByTagInRange(alpha) after reopen: %v", err)
	}
	if len(alphaIDs) != 1 {
		t.Errorf("expected 1 engram with tag 'alpha', got %d", len(alphaIDs))
	} else if alphaIDs[0] != id1 {
		t.Errorf("tag 'alpha' returned id %v, want %v", alphaIDs[0], id1)
	}

	// State index: active should contain id1
	activeIDs, err := store2.ListByState(ctx, ws2, storage.StateActive, 100)
	if err != nil {
		t.Fatalf("ListByState(active) after reopen: %v", err)
	}
	found := false
	for _, id := range activeIDs {
		if id == id1 {
			found = true
			break
		}
	}
	if !found {
		t.Error("id1 not found in active state index after reopen")
	}

	// State index: soft-deleted should contain id2
	deletedIDs, err := store2.ListByState(ctx, ws2, storage.StateSoftDeleted, 100)
	if err != nil {
		t.Fatalf("ListByState(soft-deleted) after reopen: %v", err)
	}
	found = false
	for _, id := range deletedIDs {
		if id == id2 {
			found = true
			break
		}
	}
	if !found {
		t.Error("id2 not found in soft-deleted state index after reopen")
	}
}

// TestUpgradeTest_DowngradeBlocked writes a schema version, closes the DB,
// manually bumps the stored schema version to a higher number, and verifies
// that CheckAndSetSchemaVersion returns an error on the next open.
func TestUpgradeTest_DowngradeBlocked(t *testing.T) {
	dir, err := os.MkdirTemp("", "muninndb-upgrade-downgrade-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	// --- Phase 1: set current schema version ---
	db, err := storage.OpenPebble(dir, storage.DefaultOptions())
	if err != nil {
		t.Fatalf("OpenPebble: %v", err)
	}
	if err := replication.CheckAndSetSchemaVersion(db); err != nil {
		t.Fatalf("CheckAndSetSchemaVersion (initial): %v", err)
	}
	db.Close()

	// --- Phase 2: reopen and bump stored version to simulate a newer binary ---
	db2, err := storage.OpenPebble(dir, storage.DefaultOptions())
	if err != nil {
		t.Fatalf("OpenPebble (phase 2): %v", err)
	}

	// Key matches schemaVersionKey() in internal/replication/keys.go. It moved
	// from 0x19 0x03 "schema_v" to prefix.Replication|0x02|"schema_version" with
	// #726; the live constructor is unexported, so this copy is hand-written and
	// the migration's copy is pinned separately by
	// TestMigrationV5KeysMatchReplicationPackage.
	schemaKey := append([]byte{prefix.Replication, 0x02}, "schema_version"...)
	futureVersion := uint64(999)
	buf := make([]byte, 8)
	binary.BigEndian.PutUint64(buf, futureVersion)
	if err := db2.Set(schemaKey, buf, pebble.Sync); err != nil {
		t.Fatalf("manually set future schema version: %v", err)
	}
	db2.Close()

	// --- Phase 3: reopen and verify downgrade is blocked ---
	db3, err := storage.OpenPebble(dir, storage.DefaultOptions())
	if err != nil {
		t.Fatalf("OpenPebble (phase 3): %v", err)
	}
	defer db3.Close()

	err = replication.CheckAndSetSchemaVersion(db3)
	if err == nil {
		t.Fatal("expected error for downgrade, got nil")
	}
	if !strings.Contains(err.Error(), "newer binary") {
		t.Errorf("error = %q, want it to contain 'newer binary'", err.Error())
	}
}
