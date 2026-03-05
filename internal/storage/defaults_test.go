package storage

import (
	"context"
	"os"
	"testing"
)

// TestWriteEngramDefaults verifies that WriteEngram applies sensible defaults
// for fields that must not be zero-valued for correct scoring.
//
// Regression: confidence=0 caused final score = raw * 0 = 0 (nothing returned
// from activate). stability=0 caused division-by-zero in decay = NaN.
// lastAccess=zero caused daysSince = ~738000 days → decayFactor = min floor.
func TestWriteEngramDefaults(t *testing.T) {
	dir, err := os.MkdirTemp("", "muninndb-defaults-*")
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

	// Write engram with no explicit confidence, stability, or lastAccess
	eng := &Engram{
		Concept: "test concept",
		Content: "test content body",
		Tags:    []string{"tag1"},
	}

	id, err := store.WriteEngram(ctx, ws, eng)
	if err != nil {
		t.Fatalf("WriteEngram: %v", err)
	}

	// Read back the stored engram
	stored, err := store.GetEngram(ctx, ws, id)
	if err != nil {
		t.Fatalf("GetEngram: %v", err)
	}

	// Confidence must default to 1.0 so final score = raw * confidence != 0
	if stored.Confidence != 1.0 {
		t.Errorf("Confidence = %v, want 1.0", stored.Confidence)
	}

	// Stability must be non-zero so decay factor is a valid float
	if stored.Stability <= 0 {
		t.Errorf("Stability = %v, want > 0", stored.Stability)
	}

	// LastAccess must be non-zero so daysSince is computed correctly
	if stored.LastAccess.IsZero() {
		t.Errorf("LastAccess is zero, want it set to CreatedAt")
	}

	// CreatedAt must be set
	if stored.CreatedAt.IsZero() {
		t.Errorf("CreatedAt is zero")
	}

	// LastAccess should be >= CreatedAt
	if stored.LastAccess.Before(stored.CreatedAt) {
		t.Errorf("LastAccess %v is before CreatedAt %v", stored.LastAccess, stored.CreatedAt)
	}
}

// TestWriteEngramExplicitValues verifies that explicit values are preserved.
func TestWriteEngramExplicitValues(t *testing.T) {
	dir, err := os.MkdirTemp("", "muninndb-explicit-*")
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

	eng := &Engram{
		Concept:    "test",
		Content:    "explicit values test",
		Confidence: 0.7,
		Stability:  14.0,
	}

	id, err := store.WriteEngram(ctx, ws, eng)
	if err != nil {
		t.Fatalf("WriteEngram: %v", err)
	}

	stored, err := store.GetEngram(ctx, ws, id)
	if err != nil {
		t.Fatalf("GetEngram: %v", err)
	}

	// Explicit confidence must be preserved, not overwritten
	if stored.Confidence != 0.7 {
		t.Errorf("Confidence = %v, want 0.7", stored.Confidence)
	}

	// Explicit stability must be preserved
	if stored.Stability != 14.0 {
		t.Errorf("Stability = %v, want 14.0", stored.Stability)
	}
}
