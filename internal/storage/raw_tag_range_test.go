package storage

import (
	"context"
	"strings"
	"testing"

	"github.com/cockroachdb/pebble"
	"github.com/scrypster/muninndb/internal/storage/keys"
)

// TestRawTagRange_RejectsNulInValue verifies that a tag whose value (the part
// after the first ':') contains a 0x00 (NUL) byte is rejected at write time
// with an error, and that no 0x2B index entry is written for it — a NUL byte
// in the value would corrupt the value/id separator the raw-tag-range index
// depends on for ordering.
func TestRawTagRange_RejectsNulInValue(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("nul-reject-test")

	badTag := "due:2026\x0007-01" // NUL embedded in the value
	_, err := store.WriteEngram(ctx, ws, &Engram{
		Concept:    "bad tag engram",
		Content:    "content",
		Tags:       []string{badTag},
		Confidence: 1.0,
		Stability:  30,
	})
	if err == nil {
		t.Fatal("expected WriteEngram to reject a tag value containing a NUL byte, got nil error")
	}
	if !strings.Contains(err.Error(), "NUL") {
		t.Errorf("expected error to mention NUL byte, got: %v", err)
	}

	// No 0x2B entries should exist for this vault at all — the whole write
	// must have been rejected, not partially applied.
	lower := keys.RawTagRangePrefix(ws, keys.Hash("due"))
	upper := keys.PrefixUpperBound(lower)
	iter, err := store.db.NewIter(&pebble.IterOptions{LowerBound: lower, UpperBound: upper})
	if err != nil {
		t.Fatalf("new iter: %v", err)
	}
	defer iter.Close()
	if iter.First() {
		t.Errorf("unexpected 0x2B entry written despite rejected tag: %x", iter.Key())
	}
}

// TestRawTagRange_ClearVaultRemoves verifies that ClearVault removes all
// 0x2B raw-tag-range entries for the vault, and that recreating a vault with
// the SAME name afterward does not resurrect stale raw-tag entries under the
// reused workspace prefix (e.g. a stale "due:" entry causing a false-positive
// reminder hit for a brand-new, unrelated vault).
func TestRawTagRange_ClearVaultRemoves(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	const vaultName = "clearvault-rawtag-reuse"
	ws := store.VaultPrefix(vaultName)

	if err := store.WriteVaultName(ws, vaultName); err != nil {
		t.Fatalf("WriteVaultName: %v", err)
	}

	_, err := store.WriteEngram(ctx, ws, &Engram{
		Concept:    "reminder",
		Content:    "pay the invoice",
		Tags:       []string{"due:2026-01-01"},
		Confidence: 1.0,
		Stability:  30,
	})
	if err != nil {
		t.Fatalf("WriteEngram: %v", err)
	}

	// Sanity: the raw-tag entry exists before clearing.
	lower := keys.RawTagRangePrefix(ws, keys.Hash("due"))
	upper := keys.PrefixUpperBound(lower)
	if !hasAnyKeyInRange(t, store, lower, upper) {
		t.Fatal("expected a 0x2B entry to exist before ClearVault")
	}

	if _, err := store.ClearVault(ctx, ws); err != nil {
		t.Fatalf("ClearVault: %v", err)
	}

	if hasAnyKeyInRange(t, store, lower, upper) {
		t.Error("0x2B raw-tag-range entries survived ClearVault")
	}

	// Simulate vault-name reuse: same name, same derived workspace prefix,
	// write an engram with an UNRELATED tag under the same tagKey ("due").
	// If a stale entry had survived, a due:<=X scan on the reused vault could
	// resurrect the old, deleted engram's ID.
	_, err = store.WriteEngram(ctx, ws, &Engram{
		Concept:    "fresh vault, unrelated content",
		Content:    "brand new after reuse",
		Tags:       []string{"due:2030-12-31"},
		Confidence: 1.0,
		Stability:  30,
	})
	if err != nil {
		t.Fatalf("WriteEngram after reuse: %v", err)
	}

	ids, err := store.ScanRawTagRange(ctx, ws, "due", lower, upper, 0)
	if err != nil {
		t.Fatalf("ScanRawTagRange after reuse: %v", err)
	}
	if len(ids) != 1 {
		t.Errorf("expected exactly 1 raw-tag entry after clear+reuse (only the fresh write), got %d", len(ids))
	}
}

func hasAnyKeyInRange(t *testing.T, store *PebbleStore, lower, upper []byte) bool {
	t.Helper()
	iter, err := store.db.NewIter(&pebble.IterOptions{LowerBound: lower, UpperBound: upper})
	if err != nil {
		t.Fatalf("new iter: %v", err)
	}
	defer iter.Close()
	return iter.First()
}
