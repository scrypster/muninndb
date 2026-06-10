package hnsw_test

import (
	"context"
	"math"
	"math/rand"
	"testing"

	"github.com/cockroachdb/pebble"
	"github.com/scrypster/muninndb/internal/index/hnsw"
	"github.com/scrypster/muninndb/internal/storage/keys"
)

// newTestDB opens a Pebble instance in a temp dir and registers cleanup.
func newTestDB(t *testing.T) *pebble.DB {
	t.Helper()
	db, err := pebble.Open(t.TempDir(), &pebble.Options{})
	if err != nil {
		t.Fatalf("pebble.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// testWS returns a fixed 8-byte workspace prefix for tests.
func testWS() [8]byte {
	return keys.VaultPrefix("hnsw-test")
}

// newID returns a random 16-byte ID.
func newID(rng *rand.Rand) [16]byte {
	var id [16]byte
	rng.Read(id[:])
	return id
}

// randomUnitVector generates a random unit vector of the given dimension using rng.
func randomUnitVector(rng *rand.Rand, dim int) []float32 {
	v := make([]float32, dim)
	var norm float64
	for i := range v {
		x := rng.NormFloat64()
		v[i] = float32(x)
		norm += x * x
	}
	norm = math.Sqrt(norm)
	if norm > 0 {
		for i := range v {
			v[i] = float32(float64(v[i]) / norm)
		}
	}
	return v
}

// insertVector is a convenience helper: stores the vector in Pebble then calls Insert.
func insertVector(t *testing.T, idx *hnsw.Index, db *pebble.DB, id [16]byte, vec []float32) {
	t.Helper()
	if err := idx.StoreVector(id, vec); err != nil {
		t.Fatalf("StoreVector: %v", err)
	}
	idx.Insert(id, vec)
}

// TestCosineSimilarityIdentical verifies that the similarity of a vector with
// itself is 1.0.
func TestCosineSimilarityIdentical(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	v := randomUnitVector(rng, 8)
	sim := hnsw.CosineSimilarity(v, v)
	if math.Abs(float64(sim)-1.0) > 1e-5 {
		t.Errorf("expected cosine similarity 1.0 for identical vectors, got %v", sim)
	}
}

// TestCosineSimilarityOrthogonal verifies that two orthogonal unit vectors
// have similarity 0.0.
func TestCosineSimilarityOrthogonal(t *testing.T) {
	a := []float32{1, 0, 0, 0}
	b := []float32{0, 1, 0, 0}
	sim := hnsw.CosineSimilarity(a, b)
	if math.Abs(float64(sim)) > 1e-6 {
		t.Errorf("expected cosine similarity 0.0 for orthogonal vectors, got %v", sim)
	}
}

// TestCosineSimilarityZeroVector verifies that a zero vector input returns 0.0
// without panicking.
func TestCosineSimilarityZeroVector(t *testing.T) {
	zero := []float32{0, 0, 0, 0}
	other := []float32{1, 0, 0, 0}

	sim := hnsw.CosineSimilarity(zero, other)
	if sim != 0.0 {
		t.Errorf("expected 0.0 for zero vector input, got %v", sim)
	}

	sim = hnsw.CosineSimilarity(other, zero)
	if sim != 0.0 {
		t.Errorf("expected 0.0 when second vector is zero, got %v", sim)
	}

	sim = hnsw.CosineSimilarity(zero, zero)
	if sim != 0.0 {
		t.Errorf("expected 0.0 for both-zero input, got %v", sim)
	}
}

// TestInsertAndSearch inserts 20 vectors — one "spike" unit vector plus 19
// random ones — then searches for the spike and verifies it ranks #1.
// The spike vector (all mass in dim 0) has cosine similarity ~0 with any
// random unit vector, so it is always the unambiguous nearest neighbour and
// HNSW's greedy descent reliably finds it even in small graphs.
func TestInsertAndSearch(t *testing.T) {
	db := newTestDB(t)
	ws := testWS()
	idx := hnsw.New(db, ws)
	t.Cleanup(idx.Close) // drain persistNode goroutines before db.Close() fires
	rng := rand.New(rand.NewSource(42))
	ctx := context.Background()

	const dim = 16
	const n = 20
	const target = 5

	ids := make([][16]byte, n)
	vecs := make([][]float32, n)
	for i := 0; i < n; i++ {
		ids[i] = newID(rng)
		if i == target {
			// One-hot unit vector: clearly closest to itself, ~0 to any random vec.
			v := make([]float32, dim)
			v[0] = 1.0
			vecs[i] = v
		} else {
			vecs[i] = randomUnitVector(rng, dim)
		}
		insertVector(t, idx, db, ids[i], vecs[i])
	}

	results, err := idx.Search(ctx, vecs[target], 1)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("Search returned no results")
	}
	if results[0].ID != ids[target] {
		t.Errorf("expected top result to be ids[%d], got a different ID", target)
	}
	if results[0].Score < 0.99 {
		t.Errorf("expected similarity > 0.99 for exact match, got %v", results[0].Score)
	}
}

// TestSearchReturnsKResults inserts 50 vectors, searches with k=10 and verifies
// exactly 10 results are returned.
func TestSearchReturnsKResults(t *testing.T) {
	db := newTestDB(t)
	ws := testWS()
	idx := hnsw.New(db, ws)
	t.Cleanup(idx.Close)
	rng := rand.New(rand.NewSource(99))
	ctx := context.Background()

	const dim = 16
	const n = 50
	const k = 10

	for i := 0; i < n; i++ {
		id := newID(rng)
		vec := randomUnitVector(rng, dim)
		insertVector(t, idx, db, id, vec)
	}

	query := randomUnitVector(rng, dim)
	results, err := idx.Search(ctx, query, k)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != k {
		t.Errorf("expected %d results, got %d", k, len(results))
	}
}

// TestPersistenceRoundTrip inserts 10 nodes into an index, waits for async
// persistence, loads a fresh Index from the same Pebble DB and verifies that
// searching on the fresh index returns a result matching the original insert.
func TestPersistenceRoundTrip(t *testing.T) {
	db := newTestDB(t)
	ws := testWS()
	ctx := context.Background()
	rng := rand.New(rand.NewSource(7))

	const dim = 8
	const n = 10

	ids := make([][16]byte, n)
	vecs := make([][]float32, n)

	// First index: insert nodes.
	idx1 := hnsw.New(db, ws)
	for i := 0; i < n; i++ {
		ids[i] = newID(rng)
		vecs[i] = randomUnitVector(rng, dim)
		insertVector(t, idx1, db, ids[i], vecs[i])
	}

	// Block until all persistNode goroutines finish writing to Pebble,
	// then load a fresh index from the persisted state.
	idx1.Close()

	// Second index: load from Pebble.
	idx2 := hnsw.New(db, ws)
	t.Cleanup(idx2.Close)
	if err := idx2.LoadFromPebble(); err != nil {
		t.Fatalf("LoadFromPebble: %v", err)
	}

	// Search on the fresh index for the first vector; should get a valid result.
	// We ask for k=3 to improve the chance of finding the exact vector.
	results, err := idx2.Search(ctx, vecs[0], 3)
	if err != nil {
		t.Fatalf("Search on loaded index: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("Search on loaded index returned no results")
	}

	// Build a set of all inserted IDs for membership validation.
	insertedIDs := make(map[[16]byte]bool, n)
	for _, id := range ids {
		insertedIDs[id] = true
	}

	// Every returned result must be a node we actually inserted.
	for i, r := range results {
		if !insertedIDs[r.ID] {
			t.Errorf("result[%d] ID %v was not in the original inserted set", i, r.ID)
		}
	}

	// The score must be positive (valid cosine similarity range for a result from the index).
	if results[0].Score <= 0 {
		t.Errorf("expected positive similarity score on persistence round-trip, got %v", results[0].Score)
	}
}

// TestSearchEmptyIndex verifies that searching an empty index returns nil/empty
// without panicking.
func TestSearchEmptyIndex(t *testing.T) {
	db := newTestDB(t)
	ws := testWS()
	idx := hnsw.New(db, ws)
	ctx := context.Background()

	query := []float32{1, 0, 0, 0}
	results, err := idx.Search(ctx, query, 5)
	if err != nil {
		t.Fatalf("Search on empty index returned unexpected error: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("expected 0 results on empty index, got %d", len(results))
	}
}

// TestInsertSameIDOverwrite inserts the same ID twice with different vectors.
// After the overwrite, searching for the new vector must include the overwritten
// node in the results and its score must reflect the new embedding (similarity ≈ 1.0).
//
// Strategy: use a 16-node graph (enough for reliable HNSW connectivity) and
// search with k equal to the full graph size so the overwritten node is guaranteed
// to be visited regardless of where the entry-point lands.
func TestInsertSameIDOverwrite(t *testing.T) {
	db := newTestDB(t)
	ws := testWS()
	idx := hnsw.New(db, ws)
	t.Cleanup(idx.Close)
	rng := rand.New(rand.NewSource(13))
	ctx := context.Background()

	const dim = 16
	const extra = 15 // random filler nodes after the overwrite

	// One-hot unit vector in dim 0 for vecA, dim 1 for vecB.
	// They are orthogonal, so Pebble always returns the correct "current" vector.
	var id [16]byte
	rng.Read(id[:])

	vecA := make([]float32, dim)
	vecA[0] = 1.0
	vecB := make([]float32, dim)
	vecB[1] = 1.0 // clearly different from vecA; only vecB matches the query

	// Insert vecA, then overwrite with vecB.
	if err := idx.StoreVector(id, vecA); err != nil {
		t.Fatalf("StoreVector(vecA): %v", err)
	}
	idx.Insert(id, vecA)

	if err := idx.StoreVector(id, vecB); err != nil {
		t.Fatalf("StoreVector(vecB): %v", err)
	}
	idx.Insert(id, vecB)

	// Fill the graph with extra random vectors (each uses dims 2-15 mostly,
	// so none is as close to vecB as id itself).
	for i := 0; i < extra; i++ {
		otherID := newID(rng)
		otherVec := randomUnitVector(rng, dim)
		insertVector(t, idx, db, otherID, otherVec)
	}

	// Search with k = extra+2 (cover the whole graph) so the overwritten node
	// is found even if HNSW's greedy start drifts away from it.
	k := extra + 2
	results, err := idx.Search(ctx, vecB, k)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("Search returned no results")
	}

	// Verify: id must appear in the result set and have similarity ≈ 1.0 with vecB.
	found := false
	for _, r := range results {
		if r.ID == id {
			found = true
			if r.Score < 0.99 {
				t.Errorf("overwritten node score = %v, want > 0.99 (should reflect vecB)", r.Score)
			}
			break
		}
	}
	if !found {
		t.Errorf("overwritten node (id) not found in top-%d results", k)
	}

	// The top result must also be id (vecB is clearly the closest to itself).
	if results[0].ID != id {
		t.Errorf("expected id as top-1 result (vecB is unique direction), got different ID")
	}
}
