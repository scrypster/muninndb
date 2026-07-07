package hnsw

import (
	"context"
	"errors"
	"testing"

	"github.com/cockroachdb/pebble"
)

func newDimTestRegistry(t *testing.T) (*Registry, *pebble.DB) {
	t.Helper()
	db, err := pebble.Open(t.TempDir(), &pebble.Options{})
	if err != nil {
		t.Fatalf("pebble.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return NewRegistry(db), db
}

// drainRegistry waits for async persistNode goroutines of the given vaults'
// indexes so a subsequent db.Close() cannot race them (see hnsw_test.go).
func drainRegistry(reg *Registry, wss ...[8]byte) {
	for _, ws := range wss {
		if idx := reg.get(ws); idx != nil {
			idx.Close()
		}
	}
}

func dimTestVec(dim int) []float32 {
	vec := make([]float32, dim)
	for i := range vec {
		vec[i] = float32(i%7) + 0.5
	}
	return vec
}

func TestRegistryInsertDimMismatch(t *testing.T) {
	reg, db := newDimTestRegistry(t)
	ctx := context.Background()
	ws := [8]byte{1}

	// An empty vault accepts any dimension; the first insert establishes it.
	if err := reg.Insert(ctx, ws, [16]byte{1}, dimTestVec(384)); err != nil {
		t.Fatalf("first insert into empty vault: %v", err)
	}
	if err := reg.Insert(ctx, ws, [16]byte{2}, dimTestVec(384)); err != nil {
		t.Fatalf("matching-dimension insert: %v", err)
	}

	// A mismatched vector is refused with a typed error.
	err := reg.Insert(ctx, ws, [16]byte{3}, dimTestVec(768))
	var dimErr *DimMismatchError
	if !errors.As(err, &dimErr) {
		t.Fatalf("expected *DimMismatchError, got %v", err)
	}
	if dimErr.Got != 768 || dimErr.Want != 384 {
		t.Fatalf("DimMismatchError = got %d want %d; expected got 768 want 384", dimErr.Got, dimErr.Want)
	}

	// The refused vector must not be persisted: both the live index and a
	// fresh registry reloading from Pebble see only the two valid vectors.
	if n := reg.VaultVectors(ws); n != 2 {
		t.Errorf("live index holds %d vectors, want 2", n)
	}
	reg2 := NewRegistry(db)
	if n := reg2.VaultVectors(ws); n != 2 {
		t.Errorf("reloaded index holds %d vectors, want 2", n)
	}

	// Other vaults establish their own dimension independently.
	if err := reg.Insert(ctx, [8]byte{2}, [16]byte{9}, dimTestVec(768)); err != nil {
		t.Fatalf("768-dim insert into a different empty vault: %v", err)
	}

	drainRegistry(reg, ws, [8]byte{2})
	drainRegistry(reg2, ws)
}

func TestRegistrySearchDimMismatch(t *testing.T) {
	reg, _ := newDimTestRegistry(t)
	ctx := context.Background()
	ws := [8]byte{1}

	// Searching an empty vault never dim-errors, regardless of query size.
	if _, err := reg.Search(ctx, ws, dimTestVec(768), 5); err != nil {
		t.Fatalf("search on empty vault: %v", err)
	}

	if err := reg.Insert(ctx, ws, [16]byte{1}, dimTestVec(384)); err != nil {
		t.Fatalf("insert: %v", err)
	}

	// Matching query dimension searches normally.
	if _, err := reg.Search(ctx, ws, dimTestVec(384), 5); err != nil {
		t.Fatalf("matching-dimension search: %v", err)
	}

	// Mismatched query dimension returns the typed error instead of silently
	// scoring 0 against every node.
	_, err := reg.Search(ctx, ws, dimTestVec(768), 5)
	var dimErr *DimMismatchError
	if !errors.As(err, &dimErr) {
		t.Fatalf("expected *DimMismatchError, got %v", err)
	}
	if dimErr.Got != 768 || dimErr.Want != 384 {
		t.Fatalf("DimMismatchError = got %d want %d; expected got 768 want 384", dimErr.Got, dimErr.Want)
	}

	drainRegistry(reg, ws)
}

// TestRegistryInsertRefusesOnLoadFailure pins that a vault whose graph failed
// to load refuses writes (its real dimension is unknown — failing open with
// Dim()==0 could recreate the #582 split) while reads keep the pre-existing
// degraded-empty behavior.
func TestRegistryInsertRefusesOnLoadFailure(t *testing.T) {
	reg, _ := newDimTestRegistry(t)
	ctx := context.Background()
	reg.loadErrHook = func() error { return errors.New("simulated pebble read error") }
	ws := [8]byte{1}

	if err := reg.Insert(ctx, ws, [16]byte{1}, dimTestVec(384)); err == nil {
		t.Fatal("expected Insert to refuse a vault whose graph failed to load")
	}
	// Reads stay degraded-not-erroring (#499 contract).
	if _, err := reg.Search(ctx, ws, dimTestVec(384), 5); err != nil {
		t.Fatalf("Search on a load-failed vault should degrade, got error: %v", err)
	}

	// Once the load succeeds again, inserts proceed.
	reg.loadErrHook = nil
	if err := reg.Insert(ctx, ws, [16]byte{1}, dimTestVec(384)); err != nil {
		t.Fatalf("Insert after load recovery: %v", err)
	}
	drainRegistry(reg, ws)
}

// TestRegistryConcurrentFirstInsertEstablishesOneDim pins that dimension
// check and first-insert establishment are atomic: of two concurrent first
// inserts with different dimensions, exactly one wins.
func TestRegistryConcurrentFirstInsertEstablishesOneDim(t *testing.T) {
	reg, _ := newDimTestRegistry(t)
	ctx := context.Background()
	ws := [8]byte{1}

	errs := make(chan error, 2)
	go func() { errs <- reg.Insert(ctx, ws, [16]byte{1}, dimTestVec(384)) }()
	go func() { errs <- reg.Insert(ctx, ws, [16]byte{2}, dimTestVec(768)) }()

	var refused, accepted int
	var dimErr *DimMismatchError
	for i := 0; i < 2; i++ {
		if err := <-errs; err == nil {
			accepted++
		} else if errors.As(err, &dimErr) {
			refused++
		} else {
			t.Fatalf("unexpected error type: %v", err)
		}
	}
	if accepted != 1 || refused != 1 {
		t.Fatalf("expected exactly one accepted and one refused insert, got accepted=%d refused=%d", accepted, refused)
	}
	if n := reg.VaultVectors(ws); n != 1 {
		t.Fatalf("vault holds %d vectors, want 1", n)
	}
	drainRegistry(reg, ws)
}
