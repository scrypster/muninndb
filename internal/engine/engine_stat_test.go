package engine

import (
	"context"
	"testing"

	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// TestStat_AfterWrites writes 3 engrams, calls Stat(), and verifies EngramCount > 0
// and StorageBytes >= 0.
// The request names a vault, so the answer is that vault's own (SEC-24): its
// engram count, and no on-disk size, which is a server-wide figure.
func TestStat_AfterWrites(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	writes := []struct {
		concept string
		content string
	}{
		{"stat test concept one", "content about first stat engram"},
		{"stat test concept two", "content about second stat engram"},
		{"stat test concept three", "content about third stat engram"},
	}

	for _, w := range writes {
		_, err := eng.Write(ctx, &mbp.WriteRequest{
			Vault:   "stat-vault",
			Concept: w.concept,
			Content: w.content,
		})
		if err != nil {
			t.Fatalf("Write(%q): %v", w.concept, err)
		}
	}

	stat, err := eng.Stat(ctx, &mbp.StatRequest{Vault: "stat-vault"})
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}

	if stat.EngramCount <= 0 {
		t.Errorf("EngramCount = %d, want > 0 after 3 writes", stat.EngramCount)
	}

	// StorageBytes may be 0 if Pebble hasn't flushed to disk yet; assert >= 0, not > 0.
	if stat.StorageBytes < 0 {
		t.Errorf("StorageBytes = %d, want >= 0", stat.StorageBytes)
	}
}

// TestStat_EmptyVaultField verifies that the server-wide Stat() with an empty
// Vault field returns the server's totals. Server-wide is asked for by name:
// an empty Vault alone is the default vault (SEC-24,
// TestStat_NoVaultNamedIsTheDefaultVaultNotTheServer).
func TestStat_EmptyVaultField(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	// Write one engram so we have something to count.
	_, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:   "any-vault",
		Concept: "empty vault field test",
		Content: "content for empty vault field stat test",
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	// Server-wide Stat with empty Vault field — must return global stats without error.
	stat, err := eng.Stat(ctx, &mbp.StatRequest{Vault: "", ServerWide: true})
	if err != nil {
		t.Fatalf("Stat(Vault=\"\"): %v", err)
	}

	if stat == nil {
		t.Fatal("Stat returned nil response")
	}

	// VaultCount must be >= 1 (the vault we wrote to).
	if stat.VaultCount < 1 {
		t.Errorf("VaultCount = %d, want >= 1", stat.VaultCount)
	}

	// EngramCount must reflect the written engram.
	if stat.EngramCount <= 0 {
		t.Errorf("EngramCount = %d, want > 0", stat.EngramCount)
	}

	// StorageBytes may be 0 if Pebble hasn't flushed yet; assert >= 0.
	if stat.StorageBytes < 0 {
		t.Errorf("StorageBytes = %d, want >= 0", stat.StorageBytes)
	}
}
