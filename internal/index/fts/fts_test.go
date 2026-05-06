package fts

import (
	"context"
	"os"
	"testing"

	"github.com/cockroachdb/pebble"
	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/storage/keys"
)

func openTestDB(t *testing.T) (*pebble.DB, func()) {
	t.Helper()
	dir, err := os.MkdirTemp("", "muninndb-fts-*")
	if err != nil {
		t.Fatal(err)
	}
	db, err := storage.OpenPebble(dir, storage.DefaultOptions())
	if err != nil {
		os.RemoveAll(dir)
		t.Fatal(err)
	}
	return db, func() {
		db.Close()
		os.RemoveAll(dir)
	}
}

// TestIndexEngramUpdatesStats verifies that IndexEngram updates per-term document
// frequency and global stats so that BM25 search returns non-zero scores.
//
// Regression: IndexEngram wrote posting keys but did not write TermStats (df)
// or call UpdateStats. This caused getIDF to return 0 for all terms, making
// every BM25 score 0 and Search return no results.
func TestIndexEngramUpdatesStats(t *testing.T) {
	db, cleanup := openTestDB(t)
	defer cleanup()

	idx := New(db)
	store := storage.NewPebbleStore(db, storage.PebbleStoreConfig{CacheSize: 100})
	ws := store.VaultPrefix("test")
	ctx := context.Background()

	id := [16]byte{1}
	err := idx.IndexEngram(ws, id, "Go programming language", "", "Go is a compiled language", []string{"golang"}, 0)
	if err != nil {
		t.Fatalf("IndexEngram: %v", err)
	}

	// Stats must be updated
	stats := idx.readStats(ws)
	if stats.TotalEngrams == 0 {
		t.Errorf("TotalEngrams = 0, want >= 1 after IndexEngram")
	}
	if stats.AvgDocLen == 0 {
		t.Errorf("AvgDocLen = 0, want > 0 after IndexEngram")
	}

	// Search must return results with non-zero scores
	results, err := idx.Search(ctx, ws, "compiled language", 10)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("Search returned 0 results, want >= 1")
	}
	if results[0].Score <= 0 {
		t.Errorf("results[0].Score = %v, want > 0", results[0].Score)
	}
}

// TestFTSRankingOrder verifies that the most relevant document ranks first.
func TestFTSRankingOrder(t *testing.T) {
	db, cleanup := openTestDB(t)
	defer cleanup()

	idx := New(db)
	store := storage.NewPebbleStore(db, storage.PebbleStoreConfig{CacheSize: 100})
	ws := store.VaultPrefix("test")
	ctx := context.Background()

	id1 := [16]byte{1}
	id2 := [16]byte{2}
	id3 := [16]byte{3}

	_ = idx.IndexEngram(ws, id1, "Go programming language", "", "Go is a statically typed compiled language", []string{"golang", "compiled"}, 0)
	_ = idx.IndexEngram(ws, id2, "PostgreSQL database", "", "PostgreSQL is a relational database system", []string{"database", "sql"}, 0)
	_ = idx.IndexEngram(ws, id3, "Machine learning", "", "Machine learning algorithms learn from data", []string{"ml", "ai"}, 0)

	// Query about compiled language should rank Go first
	results, err := idx.Search(ctx, ws, "compiled programming language", 10)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("Search returned 0 results")
	}
	if results[0].ID != id1 {
		t.Errorf("top result ID = %x, want %x (Go engram)", results[0].ID, id1)
	}

	// Query about database should rank PostgreSQL first
	results, err = idx.Search(ctx, ws, "relational database SQL", 10)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("Search returned 0 results")
	}
	if results[0].ID != id2 {
		t.Errorf("top result ID = %x, want %x (PostgreSQL engram)", results[0].ID, id2)
	}
}

// TestFTSMultipleEngrams verifies df increments correctly across multiple engrams.
func TestFTSMultipleEngrams(t *testing.T) {
	db, cleanup := openTestDB(t)
	defer cleanup()

	idx := New(db)
	store := storage.NewPebbleStore(db, storage.PebbleStoreConfig{CacheSize: 100})
	ws := store.VaultPrefix("test")
	ctx := context.Background()

	// Index 3 engrams all containing the word "system"
	for i := 0; i < 3; i++ {
		id := [16]byte{byte(i + 1)}
		_ = idx.IndexEngram(ws, id, "system concept", "", "this is a system component", nil, 0)
	}

	stats := idx.readStats(ws)
	if stats.TotalEngrams != 3 {
		t.Errorf("TotalEngrams = %d, want 3", stats.TotalEngrams)
	}

	results, err := idx.Search(ctx, ws, "system component", 10)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 3 {
		t.Errorf("Search returned %d results, want 3", len(results))
	}
}

// TestFTS_DualPathSearch verifies that Search handles both stemmed and
// unstemmed query tokens, finding engrams indexed under stemmed keys.
//
// Test A: querying the unstemmed form ("running") finds an engram indexed as "run".
// Test B: querying the already-stemmed form ("run") also finds the same engram.
// Test C: a posting entry written directly under the unstemmed key ("running") for
// a second engram is also returned — proving the dual-path union logic is active.
func TestFTS_DualPathSearch(t *testing.T) {
	db, cleanup := openTestDB(t)
	defer cleanup()

	idx := New(db)
	store := storage.NewPebbleStore(db, storage.PebbleStoreConfig{CacheSize: 100})
	ws := store.VaultPrefix("dualpath")
	ctx := context.Background()

	// id1: indexed via IndexEngram — uses stemmed tokens internally.
	// "running" → Porter2 stem → "run"; "dogs" → "dog".
	var id1 [16]byte
	id1[15] = 1

	err := idx.IndexEngram(ws, id1, "running dogs", "", "they run fast", nil, 0)
	if err != nil {
		t.Fatalf("IndexEngram: %v", err)
	}

	// Test A: searching "running" (unstemmed form) should find id1 indexed as "run".
	// Search tokenizes "running" → stemmed "run" AND unstemmed "running".
	// The union of both token forms is used; "run" has a posting → id1 is found.
	resultsA, err := idx.Search(ctx, ws, "running", 5)
	if err != nil {
		t.Fatalf("Search A: %v", err)
	}
	if len(resultsA) == 0 {
		t.Error("Test A: search for 'running' should find engram indexed as 'run', got 0 results")
	} else {
		found := false
		for _, r := range resultsA {
			if r.ID == id1 {
				found = true
			}
		}
		if !found {
			t.Error("Test A: id1 not found when searching 'running'")
		}
	}

	// Test B: searching the already-stemmed form "run" should also find id1.
	resultsB, err := idx.Search(ctx, ws, "run", 5)
	if err != nil {
		t.Fatalf("Search B: %v", err)
	}
	if len(resultsB) == 0 {
		t.Error("Test B: search for 'run' should find engram indexed as 'run', got 0 results")
	} else {
		found := false
		for _, r := range resultsB {
			if r.ID == id1 {
				found = true
			}
		}
		if !found {
			t.Error("Test B: id1 not found when searching 'run'")
		}
	}

	// Test C: write a posting entry directly under the unstemmed token "running"
	// for a second engram (id2). This simulates a legacy / un-migrated index entry.
	// Search("running") must return id2 via the raw-token path in the union.
	var id2 [16]byte
	id2[15] = 2

	// Import keys package to write the raw posting directly.
	// We write a minimal 7-byte posting value: TF=1.0, Field=FieldContent(0x03), DocLen=5.
	import_keys := func() {
		// The posting key format is: 0x05 | ws(8) | term | 0x00 | id(16)
		// We can construct it with the exported FTSPostingKey helper.
	}
	_ = import_keys // used only to document the format; actual write is below.

	// Build and write the raw posting key for the unstemmed token "running" + id2.
	// We use Pebble directly to bypass IndexEngram's stemming.
	{
		// Construct key manually: same layout as keys.FTSPostingKey
		// Format: 0x05 | ws[8] | term | 0x00 | field[1] | id[16]
		term := "running"
		termBytes := []byte(term)
		key := make([]byte, 1+8+len(termBytes)+1+1+16)
		key[0] = 0x05
		copy(key[1:9], ws[:])
		copy(key[9:9+len(termBytes)], termBytes)
		key[9+len(termBytes)] = 0x00
		key[10+len(termBytes)] = 0x03 // FieldContent
		copy(key[11+len(termBytes):], id2[:])

		// Encode a minimal 7-byte PostingValue: TF=1, Field=0x03 (content), DocLen=5.
		import_pv := make([]byte, 7)
		// TF float32(1.0) = 0x3F800000 big-endian
		import_pv[0] = 0x3F
		import_pv[1] = 0x80
		import_pv[2] = 0x00
		import_pv[3] = 0x00
		import_pv[4] = 0x03 // FieldContent
		import_pv[5] = 0x00 // DocLen high byte
		import_pv[6] = 0x05 // DocLen low byte = 5

		if err := db.Set(key, import_pv, nil); err != nil {
			t.Fatalf("db.Set raw posting for id2: %v", err)
		}

		// Also update stats so id2 is counted.
		if err := idx.UpdateStats(ws, 5); err != nil {
			t.Fatalf("UpdateStats for id2: %v", err)
		}

		// Update term-stats (df) for "running" so IDF is non-zero.
		import_tkey := make([]byte, 1+8+len(term))
		import_tkey[0] = 0x09
		copy(import_tkey[1:9], ws[:])
		copy(import_tkey[9:], []byte(term))

		// Write df=1 for "running".
		import_df := make([]byte, 8)
		import_df[0] = 0x00
		import_df[1] = 0x00
		import_df[2] = 0x00
		import_df[3] = 0x01 // df=1 big-endian uint32
		if err := db.Set(import_tkey, import_df, nil); err != nil {
			t.Fatalf("db.Set term-stats for 'running': %v", err)
		}
		// Invalidate IDF cache so stale values are not returned.
		idx.InvalidateIDFCache()
	}

	// Now search "running" — dual-path must include both stemmed "run" (→ id1)
	// and unstemmed "running" (→ id2) in the result set.
	resultsC, err := idx.Search(ctx, ws, "running", 10)
	if err != nil {
		t.Fatalf("Search C: %v", err)
	}
	if len(resultsC) == 0 {
		t.Error("Test C: search for 'running' returned 0 results after writing id2 under unstemmed key")
	}

	// id2 must appear (via the raw "running" posting key path).
	foundID2 := false
	for _, r := range resultsC {
		if r.ID == id2 {
			foundID2 = true
		}
	}
	if !foundID2 {
		t.Error("Test C: id2 not found — dual-path unstemmed token lookup did not return it")
	}
}

// TestFTS_DeleteEngram verifies that after deleting an engram from the FTS index
// it no longer appears in search results.
func TestFTS_DeleteEngram(t *testing.T) {
	db, cleanup := openTestDB(t)
	defer cleanup()

	idx := New(db)
	store := storage.NewPebbleStore(db, storage.PebbleStoreConfig{CacheSize: 100})
	ws := store.VaultPrefix("del-test")
	ctx := context.Background()

	id := [16]byte{0xDE, 0xAD, 0xBE, 0xEF}
	concept := "deletable engram concept"
	createdBy := "test-author"
	content := "this engram contains unique deletable content for search"
	tags := []string{"delete", "test"}

	// Step 1: Index the engram.
	if err := idx.IndexEngram(ws, id, concept, createdBy, content, tags, 0); err != nil {
		t.Fatalf("IndexEngram: %v", err)
	}

	// Step 2: Search — expect to find the engram (at least 1 result containing id).
	results, err := idx.Search(ctx, ws, "deletable content", 10)
	if err != nil {
		t.Fatalf("Search before delete: %v", err)
	}
	foundBefore := false
	for _, r := range results {
		if r.ID == id {
			foundBefore = true
			break
		}
	}
	if !foundBefore {
		t.Fatal("expected to find engram before DeleteEngram, but it was not in results")
	}

	// Step 3: Delete the engram from the FTS index.
	if err := idx.DeleteEngram(ws, id, concept, createdBy, content, tags, 0); err != nil {
		t.Fatalf("DeleteEngram: %v", err)
	}

	// Step 4: Search again — expect 0 results for that id.
	results, err = idx.Search(ctx, ws, "deletable content", 10)
	if err != nil {
		t.Fatalf("Search after delete: %v", err)
	}
	for _, r := range results {
		if r.ID == id {
			t.Errorf("engram id %x still appears in search results after DeleteEngram", id)
		}
	}
}

// TestFTS_DeleteEngram_NotIndexed verifies that calling DeleteEngram with an ID
// that was never indexed is idempotent and returns nil error.
// Tests the empty-token early-return path in DeleteEngram.
func TestFTS_DeleteEngram_NotIndexed(t *testing.T) {
	db, cleanup := openTestDB(t)
	defer cleanup()

	idx := New(db)
	store := storage.NewPebbleStore(db, storage.PebbleStoreConfig{CacheSize: 100})
	ws := store.VaultPrefix("del-noop-test")

	// Pass EMPTY strings for concept, createdBy, content and nil tags.
	// This exercises the early-return path: len(termSet) == 0 → return nil.
	// Tests the empty-token early-return path in DeleteEngram.
	neverIndexedID := [16]byte{0xFF, 0xEE, 0xDD, 0xCC}
	err := idx.DeleteEngram(ws, neverIndexedID, "", "", "", nil, 0)
	if err != nil {
		t.Errorf("DeleteEngram on never-indexed id with empty fields: expected nil error, got %v", err)
	}
}

func TestFTS_ReindexedVaultSkipsDualPath(t *testing.T) {
	db, cleanup := openTestDB(t)
	defer cleanup()
	idx := New(db)
	ctx := context.Background()

	ws := [8]byte{0xAA, 0xBB, 0xCC, 0xDD, 0xEE, 0xFF, 0x11, 0x22}
	id1 := [16]byte{0x01}
	id2 := [16]byte{0x02}

	// Index engram id1 properly via IndexEngram so TermStatsKey and FTSStatsKey are written.
	// "run quickly" → stemmed tokens include "run"; id1 will be findable via the stemmed path.
	if err := idx.IndexEngram(ws, id1, "run quickly", "", "", nil, 0); err != nil {
		t.Fatalf("IndexEngram: %v", err)
	}

	// Write a raw "running" posting for id2 ONLY — simulating legacy data.
	// This bypasses IndexEngram intentionally so there's no stemmed posting for id2.
	postingKey := keys.FTSPostingKey(ws, "running", FieldContent, id2)
	if err := db.Set(postingKey, []byte{}, pebble.NoSync); err != nil {
		t.Fatalf("Set raw posting: %v", err)
	}

	// Mark vault as reindexed (FTSVersionKey = 0x01).
	versionKey := keys.FTSVersionKey(ws)
	if err := db.Set(versionKey, []byte{0x01}, pebble.NoSync); err != nil {
		t.Fatalf("Set version key: %v", err)
	}

	// Search "running" — should stem to "run", find id1, but NOT find id2 (raw fallback skipped).
	results, err := idx.Search(ctx, ws, "running", 10)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	foundID1 := false
	foundID2 := false
	for _, r := range results {
		if [16]byte(r.ID) == id1 {
			foundID1 = true
		}
		if [16]byte(r.ID) == id2 {
			foundID2 = true
		}
	}
	if !foundID1 {
		t.Errorf("expected id1 (stemmed 'run' match) to be found, but it was not. Results: %+v", results)
	}
	if foundID2 {
		t.Errorf("expected id2 (raw 'running' only) to NOT be found after reindex, but it was. Raw fallback was not skipped.")
	}
}

// TestMultiFieldPostingNoOverwrite verifies that a term appearing in multiple
// fields (concept AND content) contributes scores from both fields, not just
// the last one written. Before the fix, FTSPostingKey had no field byte and
// the last field's batch.Set overwrote all earlier ones.
func TestMultiFieldPostingNoOverwrite(t *testing.T) {
	db, cleanup := openTestDB(t)
	defer cleanup()

	idx := New(db)
	store := storage.NewPebbleStore(db, storage.PebbleStoreConfig{CacheSize: 100})
	ws := store.VaultPrefix("multifield")
	ctx := context.Background()

	// "rocket" appears in both concept and content — both fields should contribute.
	id1 := [16]byte{1}
	_ = idx.IndexEngram(ws, id1, "rocket science", "", "the rocket launches at dawn", nil)

	// id2 has "rocket" only in content.
	id2 := [16]byte{2}
	_ = idx.IndexEngram(ws, id2, "space travel", "", "the rocket propels us forward", nil)

	results, err := idx.Search(ctx, ws, "rocket", 10)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) < 2 {
		t.Fatalf("Search returned %d results, want 2", len(results))
	}
	// id1 has "rocket" in concept (weight 3.0) + content (weight 1.0) → higher score.
	// id2 has "rocket" only in content (weight 1.0) → lower score.
	if results[0].ID != id1 {
		t.Errorf("top result = %x, want id1 (%x); multi-field boost not applied", results[0].ID, id1)
	}
	if results[0].Score <= results[1].Score {
		t.Errorf("id1 score %v <= id2 score %v; expected multi-field boost", results[0].Score, results[1].Score)
	}
}

// TestIDFCacheVaultIsolation verifies that IDF values are cached per (vault, term)
// pair, not globally by term. Before the fix, idfCache was keyed by term only:
// vault A's cached IDF for "robot" would be returned for vault B's search,
// producing incorrect BM25 scores in multi-vault setups.
func TestIDFCacheVaultIsolation(t *testing.T) {
	db, cleanup := openTestDB(t)
	defer cleanup()

	idx := New(db)
	store := storage.NewPebbleStore(db, storage.PebbleStoreConfig{CacheSize: 100})
	wsA := store.VaultPrefix("vault-a")
	wsB := store.VaultPrefix("vault-b")
	ctx := context.Background()

	// vault-a: index 10 engrams, only 1 contains "robot" → high IDF (rare term)
	for i := 0; i < 9; i++ {
		id := [16]byte{byte(i + 1)}
		_ = idx.IndexEngram(wsA, id, "unrelated topic", "", "cooking baking gardening", nil)
	}
	idRobotA := [16]byte{10}
	_ = idx.IndexEngram(wsA, idRobotA, "robot", "", "robot automation", nil)

	// vault-b: index 3 engrams, all contain "robot" → low IDF (common term)
	for i := 0; i < 3; i++ {
		id := [16]byte{byte(i + 20)}
		_ = idx.IndexEngram(wsB, id, "robot", "", "robot", nil)
	}

	// Warm the IDF cache for vault-a by running a search.
	_, _ = idx.Search(ctx, wsA, "robot", 10)

	// vault-b IDF for "robot" must be lower than vault-a's (it's common in vault-b).
	idfA := idx.getIDF(wsA, "robot", float64(10))
	idfB := idx.getIDF(wsB, "robot", float64(3))

	if idfA <= idfB {
		t.Errorf("idfA (%v) should be > idfB (%v): robot is rare in vault-a but common in vault-b", idfA, idfB)
	}
}
