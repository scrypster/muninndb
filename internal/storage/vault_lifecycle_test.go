package storage

import (
	"context"
	"testing"

	"github.com/cockroachdb/pebble"
	"github.com/scrypster/muninndb/internal/storage/keys"
)

func TestClearVault_AllPrefixesGone(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("test-clear")

	// Write vault name
	if err := store.WriteVaultName(ws, "test-clear"); err != nil {
		t.Fatalf("WriteVaultName: %v", err)
	}

	// Write an engram
	id, err := store.WriteEngram(ctx, ws, &Engram{Concept: "test", Content: "content", Confidence: 1.0, Stability: 30})
	if err != nil {
		t.Fatal(err)
	}
	_ = id

	// Clear
	n, err := store.ClearVault(ctx, ws)
	if err != nil {
		t.Fatalf("ClearVault: %v", err)
	}
	if n < 1 {
		t.Errorf("expected vault count >= 1, got %d", n)
	}

	// All vault-scoped prefixes must be empty
	vaultPrefixes := []byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08,
		0x09, 0x0A, 0x0B, 0x0C, 0x0D, 0x10, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17,
		0x22, 0x25, 0x27}
	for _, p := range vaultPrefixes {
		lo := make([]byte, 9)
		lo[0] = p
		copy(lo[1:], ws[:])
		wsPlus, err := incrementWS(ws)
		if err != nil {
			t.Fatalf("incrementWS: %v", err)
		}
		hi := make([]byte, 9)
		hi[0] = p
		copy(hi[1:], wsPlus[:])
		iter, err := store.db.NewIter(&pebble.IterOptions{LowerBound: lo, UpperBound: hi})
		if err != nil {
			t.Fatalf("NewIter for prefix 0x%02X: %v", p, err)
		}
		if iter.First() {
			t.Errorf("prefix 0x%02X still has keys after ClearVault", p)
		}
		iter.Close()
	}

	// 0x0E vault meta must still exist (Clear preserves name)
	names, _ := store.ListVaultNames()
	found := false
	for _, nm := range names {
		if nm == "test-clear" {
			found = true
		}
	}
	if !found {
		t.Error("ClearVault should preserve vault name registration")
	}
}

func TestClearVault_CrossVaultSafety(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	wsA := store.VaultPrefix("vault-a")
	wsB := store.VaultPrefix("vault-b")

	engA, _ := store.WriteEngram(ctx, wsA, &Engram{Concept: "A", Content: "a", Confidence: 1.0, Stability: 30})
	engB, _ := store.WriteEngram(ctx, wsB, &Engram{Concept: "B", Content: "b", Confidence: 1.0, Stability: 30})

	if _, err := store.ClearVault(ctx, wsA); err != nil {
		t.Fatal(err)
	}

	// vault-A engram must be gone
	if _, err := store.GetEngram(ctx, wsA, engA); err == nil {
		t.Error("expected vault-A engram to be gone after ClearVault")
	}

	// vault-B engram must still exist
	got, err := store.GetEngram(ctx, wsB, engB)
	if err != nil {
		t.Fatalf("vault-B engram should still exist: %v", err)
	}
	if got.Concept != "B" {
		t.Errorf("unexpected concept: %q", got.Concept)
	}
}

func TestClearVault_RemovesEntityGraphForVault(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	wsA := store.VaultPrefix("entity-vault-a")
	wsB := store.VaultPrefix("entity-vault-b")

	engA, err := store.WriteEngram(ctx, wsA, &Engram{Concept: "A", Content: "a", Confidence: 1.0, Stability: 30})
	if err != nil {
		t.Fatal(err)
	}
	engB, err := store.WriteEngram(ctx, wsB, &Engram{Concept: "B", Content: "b", Confidence: 1.0, Stability: 30})
	if err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"Shared", "OnlyA", "Shared", "OnlyB"} {
		if err := store.UpsertEntityRecord(ctx, EntityRecord{Name: name, Type: "test", Confidence: 1}, "test"); err != nil {
			t.Fatalf("UpsertEntityRecord %q: %v", name, err)
		}
	}
	if err := store.WriteEntityEngramLink(ctx, wsA, engA, "Shared"); err != nil {
		t.Fatal(err)
	}
	if err := store.WriteEntityEngramLink(ctx, wsA, engA, "OnlyA"); err != nil {
		t.Fatal(err)
	}
	if err := store.WriteEntityEngramLink(ctx, wsB, engB, "Shared"); err != nil {
		t.Fatal(err)
	}
	if err := store.WriteEntityEngramLink(ctx, wsB, engB, "OnlyB"); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertRelationshipRecord(ctx, wsA, engA, RelationshipRecord{FromEntity: "Shared", ToEntity: "OnlyA", RelType: "uses", Weight: 1}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertRelationshipRecord(ctx, wsB, engB, RelationshipRecord{FromEntity: "Shared", ToEntity: "OnlyB", RelType: "uses", Weight: 1}); err != nil {
		t.Fatal(err)
	}
	if err := store.IncrementEntityCoOccurrence(ctx, wsA, "Shared", "OnlyA"); err != nil {
		t.Fatal(err)
	}
	if err := store.IncrementEntityCoOccurrence(ctx, wsB, "Shared", "OnlyB"); err != nil {
		t.Fatal(err)
	}

	if _, err := store.ClearVault(ctx, wsA); err != nil {
		t.Fatalf("ClearVault: %v", err)
	}

	for _, p := range []byte{0x20, 0x21, 0x24, 0x26} {
		assertNoVaultPrefixKeys(t, store, p, wsA)
	}
	assertNoEntityReverseIndexKeysForVault(t, store, wsA)

	var namesA []string
	if err := store.ScanVaultEntityNames(ctx, wsA, func(name string) error {
		namesA = append(namesA, name)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(namesA) != 0 {
		t.Fatalf("expected no entity names for cleared vault, got %v", namesA)
	}

	var sharedLinks [][8]byte
	if err := store.ScanEntityEngrams(ctx, "Shared", func(gotWS [8]byte, id ULID) error {
		sharedLinks = append(sharedLinks, gotWS)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(sharedLinks) != 1 || sharedLinks[0] != wsB {
		t.Fatalf("expected Shared to keep only vault B reverse link, got %v", sharedLinks)
	}

	onlyA, err := store.GetEntityRecord(ctx, "OnlyA")
	if err != nil {
		t.Fatal(err)
	}
	if onlyA != nil {
		t.Fatalf("expected orphaned entity OnlyA to be deleted, got %+v", onlyA)
	}
	shared, err := store.GetEntityRecord(ctx, "Shared")
	if err != nil {
		t.Fatal(err)
	}
	if shared == nil {
		t.Fatal("expected Shared entity record to survive because vault B still references it")
	}
}

func TestClearVault_L1CacheEvicted(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("cache-test")

	id, _ := store.WriteEngram(ctx, ws, &Engram{Concept: "cached", Content: "c", Confidence: 1.0, Stability: 30})
	store.GetEngram(ctx, ws, id) // populate L1 cache
	if _, ok := store.cache.Get(ws, id); !ok {
		t.Skip("cache did not populate — test not meaningful")
	}

	store.ClearVault(ctx, ws)

	if _, ok := store.cache.Get(ws, id); ok {
		t.Error("expected L1 cache miss after ClearVault")
	}
}

func TestClearVault_VaultCounterEvicted(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("counter-test")

	store.WriteEngram(ctx, ws, &Engram{Concept: "x", Content: "y", Confidence: 1.0, Stability: 30})
	store.GetVaultCount(ctx, ws) // seed in-memory counter

	store.ClearVault(ctx, ws)

	if _, ok := store.vaultCounters.Load(ws); ok {
		t.Error("vaultCounters entry should be evicted after ClearVault")
	}
}

func TestDeleteVaultNameOnly_RemovesNameRegistration(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("to-delete")
	store.WriteVaultName(ws, "to-delete")
	store.WriteEngram(ctx, ws, &Engram{Concept: "x", Content: "y", Confidence: 1.0, Stability: 30})

	// First clear data
	store.ClearVault(ctx, ws)

	// Then delete name registration
	if err := store.DeleteVaultNameOnly(ctx, "to-delete", ws); err != nil {
		t.Fatal(err)
	}

	names, _ := store.ListVaultNames()
	for _, nm := range names {
		if nm == "to-delete" {
			t.Error("vault name still registered after DeleteVaultNameOnly")
		}
	}
}

func TestDeleteVault_0x11OrphansNotDeleted(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("orphan-test")

	id, _ := store.WriteEngram(ctx, ws, &Engram{Concept: "x", Content: "y", Confidence: 1.0, Stability: 30})
	store.SetDigestFlag(ctx, id, 0x01)
	store.ClearVault(ctx, ws)
	store.DeleteVaultNameOnly(ctx, "orphan-test", ws)

	// 0x11 DigestFlag for this ULID must still exist (global, not vault-scoped)
	flags, err := store.GetDigestFlags(ctx, id)
	if err != nil {
		t.Fatalf("expected digest flag to survive vault deletion: %v", err)
	}
	if flags&0x01 == 0 {
		t.Error("digest flag should survive vault deletion")
	}
}

func assertNoVaultPrefixKeys(t *testing.T, store *PebbleStore, prefix byte, ws [8]byte) {
	t.Helper()
	wsPlus, err := incrementWS(ws)
	if err != nil {
		t.Fatalf("incrementWS: %v", err)
	}
	lo := make([]byte, 9)
	lo[0] = prefix
	copy(lo[1:], ws[:])
	hi := make([]byte, 9)
	hi[0] = prefix
	copy(hi[1:], wsPlus[:])
	iter, err := store.db.NewIter(&pebble.IterOptions{LowerBound: lo, UpperBound: hi})
	if err != nil {
		t.Fatalf("NewIter for prefix 0x%02X: %v", prefix, err)
	}
	defer iter.Close()
	if iter.First() {
		t.Fatalf("prefix 0x%02X still has vault keys after ClearVault", prefix)
	}
}

func assertNoEntityReverseIndexKeysForVault(t *testing.T, store *PebbleStore, ws [8]byte) {
	t.Helper()
	iter, err := store.db.NewIter(&pebble.IterOptions{LowerBound: []byte{0x23}, UpperBound: []byte{0x24}})
	if err != nil {
		t.Fatalf("NewIter for prefix 0x23: %v", err)
	}
	defer iter.Close()
	for valid := iter.First(); valid; valid = iter.Next() {
		k := iter.Key()
		if len(k) == 33 {
			var gotWS [8]byte
			copy(gotWS[:], k[9:17])
			if gotWS == ws {
				t.Fatal("entity reverse index still has vault keys after ClearVault")
			}
		}
	}
}

// TestClearVault_ClearsLastAccessArchiveAssocDreamState verifies that the three
// previously-missing prefixes (0x22 last-access, 0x25 archive-assoc, 0x27 dream state)
// are removed by ClearVault. Regression test for the pre-existing gap found during
// PR #436 review.
func TestClearVault_ClearsLastAccessArchiveAssocDreamState(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("test-missing-prefixes")
	_ = ctx

	wsPlus, err := incrementWS(ws)
	if err != nil {
		t.Fatalf("incrementWS: %v", err)
	}

	// Plant a 0x22 last-access key
	laKey := keys.LastAccessIndexKey(ws, 1000, [16]byte{1})
	if err := store.db.Set(laKey, nil, pebble.NoSync); err != nil {
		t.Fatalf("set 0x22: %v", err)
	}

	// Plant a 0x25 archive-assoc key
	archKey := keys.ArchiveAssocKey(ws, [16]byte{2}, [16]byte{3})
	if err := store.db.Set(archKey, nil, pebble.NoSync); err != nil {
		t.Fatalf("set 0x25: %v", err)
	}

	// Plant a 0x27 dream-state key
	dreamKey := keys.DreamStateKey(ws)
	if err := store.db.Set(dreamKey, []byte{0x01}, pebble.NoSync); err != nil {
		t.Fatalf("set 0x27: %v", err)
	}

	if _, err := store.ClearVault(context.Background(), ws); err != nil {
		t.Fatalf("ClearVault: %v", err)
	}

	for _, tc := range []struct {
		name   string
		prefix byte
	}{
		{"last-access (0x22)", 0x22},
		{"archive-assoc (0x25)", 0x25},
		{"dream-state (0x27)", 0x27},
	} {
		lo := make([]byte, 9)
		lo[0] = tc.prefix
		copy(lo[1:], ws[:])
		hi := make([]byte, 9)
		hi[0] = tc.prefix
		copy(hi[1:], wsPlus[:])
		iter, err := store.db.NewIter(&pebble.IterOptions{LowerBound: lo, UpperBound: hi})
		if err != nil {
			t.Fatalf("NewIter %s: %v", tc.name, err)
		}
		if iter.First() {
			t.Errorf("prefix %s still has keys after ClearVault", tc.name)
		}
		iter.Close()
	}
}
