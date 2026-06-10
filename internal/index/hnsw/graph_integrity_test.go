package hnsw

import (
	"context"
	"fmt"
	"math/rand"
	"testing"

	"github.com/cockroachdb/pebble"
)

func TestBulkInsertSelfQuery(t *testing.T) {
	dir := t.TempDir()
	db, err := pebble.Open(dir, &pebble.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	reg := NewRegistry(db)
	var ws [8]byte
	copy(ws[:], []byte("REPROTST"))

	// Sized to finish well inside CI's package timeout under -race while still
	// overflowing layer-0 maxConn so the pruning path is exercised. The original
	// repro (N=1000, dim=1536) showed 51/1000 self-hits pre-fix; this size still
	// fails pre-fix for the same reason.
	rng := rand.New(rand.NewSource(42))
	const N = 400
	const dim = 128
	ids := make([][16]byte, N)
	vecs := make([][]float32, N)
	ctx := context.Background()
	for i := 0; i < N; i++ {
		var id [16]byte
		rng.Read(id[:])
		v := make([]float32, dim)
		for j := range v {
			v[j] = rng.Float32()*2 - 1
		}
		ids[i], vecs[i] = id, v
		if err := reg.Insert(ctx, ws, id, v); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}

	hist := map[int]int{}
	selfHits := 0
	top1 := map[[16]byte]int{}
	for i := 0; i < N; i++ {
		res, err := reg.Search(ctx, ws, vecs[i], 10)
		if err != nil {
			t.Fatal(err)
		}
		hist[len(res)]++
		for _, r := range res {
			if r.ID == ids[i] {
				selfHits++
				break
			}
		}
		if len(res) > 0 {
			top1[res[0].ID]++
		}
	}
	fmt.Printf("N=%d selfHits=%d hist=%v distinctTop1=%d\n", N, selfHits, hist, len(top1))
	if selfHits < N*9/10 {
		t.Errorf("self-hit rate %d/%d — graph is not navigable", selfHits, N)
	}
}

func TestBulkInsertReloadSelfQuery(t *testing.T) {
	dir := t.TempDir()
	db, err := pebble.Open(dir, &pebble.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	reg := NewRegistry(db)
	var ws [8]byte
	copy(ws[:], []byte("RELOADTS"))

	rng := rand.New(rand.NewSource(7))
	const N = 500
	const dim = 256
	ids := make([][16]byte, N)
	vecs := make([][]float32, N)
	ctx := context.Background()
	for i := 0; i < N; i++ {
		var id [16]byte
		rng.Read(id[:])
		v := make([]float32, dim)
		for j := range v {
			v[j] = rng.Float32()*2 - 1
		}
		ids[i], vecs[i] = id, v
		if err := reg.Insert(ctx, ws, id, v); err != nil {
			t.Fatal(err)
		}
	}
	// Wait for async persistence, then reload into a fresh registry.
	reg.mu.RLock()
	for _, idx := range reg.indexes {
		idx.Close()
	}
	reg.mu.RUnlock()

	reg2 := NewRegistry(db)
	if got := reg2.VaultVectors(ws); got != N {
		t.Fatalf("reloaded %d vectors, want %d", got, N)
	}
	selfHits := 0
	for i := 0; i < N; i++ {
		res, err := reg2.Search(ctx, ws, vecs[i], 10)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range res {
			if r.ID == ids[i] {
				selfHits++
				break
			}
		}
	}
	fmt.Printf("reload selfHits=%d/%d\n", selfHits, N)
	if selfHits < N*9/10 {
		t.Errorf("post-reload self-hit rate %d/%d — persisted graph not navigable", selfHits, N)
	}
}
