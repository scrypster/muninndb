package storage

import (
	"context"
	"errors"
	"os"
	"testing"
)

// TestScanEngrams_ErrorPropagation verifies that ScanEngrams stops iterating and
// returns the error returned by fn, without calling fn for subsequent engrams.
func TestScanEngrams_ErrorPropagation(t *testing.T) {
	dir, err := os.MkdirTemp("", "muninndb-scan-err-*")
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
	ws := store.VaultPrefix("scan-test")
	ctx := context.Background()

	// Write 5 engrams.
	for i := 0; i < 5; i++ {
		eng := &Engram{
			Concept: "concept",
			Content: "content",
		}
		if _, err := store.WriteEngram(ctx, ws, eng); err != nil {
			t.Fatalf("WriteEngram[%d]: %v", i, err)
		}
	}

	sentinel := errors.New("stop here")
	var callCount int

	fn := func(_ *Engram) error {
		callCount++
		if callCount == 3 {
			return sentinel
		}
		return nil
	}

	scanErr := store.ScanEngrams(ctx, ws, fn)

	if !errors.Is(scanErr, sentinel) {
		t.Errorf("ScanEngrams returned %v, want sentinel error %v", scanErr, sentinel)
	}

	if callCount != 3 {
		t.Errorf("fn called %d times, want exactly 3 (engrams 4 and 5 must not be visited)", callCount)
	}
}
