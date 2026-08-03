package auth_test

import (
	"context"
	"encoding/binary"
	"path/filepath"
	"testing"

	"github.com/cockroachdb/pebble"
	"github.com/cockroachdb/pebble/vfs"
	"github.com/scrypster/muninndb/internal/auth"
	"github.com/scrypster/muninndb/internal/prefix"
	"github.com/scrypster/muninndb/internal/storage"
)

// TestLockoutRegression_AllThreeBugsClosed proves the #611 prefix-relocation
// fix (auth: 0x11–0x14 → 0x42–0x45) closes all three live bugs end-to-end over
// a real in-mem Pebble DB shared between auth.Store and storage.PebbleStore:
//
//  1. Lockout  — AdminExists (scanning 0x42) is NOT fooled by storage
//     DigestFlags records at 0x11 that used to collide with auth's pre-relocation
//     AdminUser prefix and falsely trip AdminExists, short-circuiting Bootstrap.
//  2. Real admin — Bootstrap creates root whose password actually validates
//     (not a false-positive AdminExists return that skips CreateAdmin).
//  3. Symmetric miscount — CountWithFlag over [0x11,0x12) returns exactly N,
//     not N+admin (admin JSON at 0x42 is outside the DigestFlags range).
//
// The test FAILS if anyone reverts auth back to 0x11–0x14: AdminExists would
// see the 0x11 DigestFlags records, return true, and Bootstrap would skip
// CreateAdmin — failing assertion (3); CountWithFlag would also walk the admin
// JSON, failing assertion (4).
func TestLockoutRegression_AllThreeBugsClosed(t *testing.T) {
	db, store, pebbleStore := setupLockoutRegressionDB(t)

	// Seed 3 DigestFlags records at 0x11 (the storage key space that used to
	// collide with auth's pre-relocation AdminUser prefix and falsely trip
	// AdminExists). Key: 0x11 | id(16) = 17 bytes; value: 1-byte flag.
	const numDigestFlags = 3
	const flag = uint16(0x01)
	for i := 0; i < numDigestFlags; i++ {
		var id [16]byte
		id[15] = byte(i)
		key := make([]byte, 1+16)
		key[0] = prefix.DigestFlags
		copy(key[1:], id[:])
		if err := db.Set(key, []byte{byte(flag)}, pebble.Sync); err != nil {
			t.Fatalf("seed DigestFlags[%d]: %v", i, err)
		}
	}

	// (1) AdminExists == false despite 3 records at 0x11 — the lockout bug.
	// Pre-relocation, AdminUser was at 0x11 and AdminExists scanned [0x11,0x12),
	// finding the digest-flag records and wrongly concluding an admin existed.
	if store.AdminExists() {
		t.Fatal("AdminExists=true with only 0x11 digest-flag records — lockout regression (#611)")
	}

	// (2) Bootstrap creates root — must succeed because AdminExists correctly
	// reports false. Pre-relocation, Bootstrap would skip CreateAdmin here.
	secretPath := filepath.Join(t.TempDir(), "auth_secret")
	if _, err := auth.Bootstrap(store, secretPath); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if !store.AdminExists() {
		t.Fatal("AdminExists=false after Bootstrap — root admin was not created")
	}

	// (3) ValidateAdmin("root", "password") — a real admin whose password
	// validates, proving Bootstrap actually invoked CreateAdmin (default
	// password "password") rather than no-oping on a false positive.
	if err := store.ValidateAdmin("root", "password"); err != nil {
		t.Fatalf("ValidateAdmin(root, default password) failed: %v — admin was not really created", err)
	}

	// (4) CountWithFlag over [0x11,0x12) returns exactly N — the symmetric-
	// miscount guard. Pre-relocation, admin JSON under 0x11 was incorrectly
	// walked by CountWithFlag, inflating the count.
	got, err := pebbleStore.CountWithFlag(context.Background(), flag)
	if err != nil {
		t.Fatalf("CountWithFlag: %v", err)
	}
	if got != int64(numDigestFlags) {
		t.Errorf("CountWithFlag = %d, want %d (admin JSON at 0x42 must not be counted as a digest flag)",
			got, numDigestFlags)
	}
}

// TestLockoutRegression_ListVaultConfigsPerfCliff proves that ListVaultConfigs
// (scanning [0x45,0x46)) no longer reads the AssocWeightIndex at 0x14 — the
// perf-cliff collision source. Pre-relocation, VaultConfig lived at 0x14 and
// every association-weight row was misread as vault-config JSON, blowing up
// json.Unmarshal and degrading list-vault operations to a full scan of the
// association index.
func TestLockoutRegression_ListVaultConfigsPerfCliff(t *testing.T) {
	db, store, _ := setupLockoutRegressionDB(t)
	_ = db

	// Seed 50 AssocWeightIndex records at 0x14 — the perf-cliff collision
	// source. Key: 0x14 | ws(8) | src(16) | dst(16) = 41 bytes; value: 4-byte
	// float32 weight.
	const numAssocWeightRows = 50
	for i := 0; i < numAssocWeightRows; i++ {
		key := make([]byte, 1+8+16+16)
		key[0] = prefix.AssocWeightIndex
		binary.BigEndian.PutUint64(key[1:9], uint64(i))
		// src[9:25] and dst[25:41] left zero — uniqueness comes from the ws slot.
		if err := db.Set(key, make([]byte, 4), pebble.Sync); err != nil {
			t.Fatalf("seed AssocWeightIndex[%d]: %v", i, err)
		}
	}

	// ListVaultConfigs must return an empty slice — 0x14 records must NOT
	// be read as VaultConfig JSON. Pre-relocation this returned 50 garbage
	// entries (or errored on json.Unmarshal of a 4-byte float32).
	cfgs, err := store.ListVaultConfigs()
	if err != nil {
		t.Fatalf("ListVaultConfigs: %v", err)
	}
	if len(cfgs) != 0 {
		t.Errorf("ListVaultConfigs = %d configs, want 0 (AssocWeightIndex at 0x14 must not be read as vault configs)",
			len(cfgs))
	}
}

// setupLockoutRegressionDB opens an in-mem Pebble DB and wraps it with both
// auth.Store and storage.PebbleStore so the test can drive the REAL production
// code paths for AdminExists/Bootstrap/ValidateAdmin (auth) and CountWithFlag
// (storage) over the same key space. The PebbleStore owns db.Close via
// t.Cleanup; auth.Store has no Close of its own.
func setupLockoutRegressionDB(t *testing.T) (*pebble.DB, *auth.Store, *storage.PebbleStore) {
	t.Helper()
	db, err := pebble.Open("", &pebble.Options{FS: vfs.NewMem()})
	if err != nil {
		t.Fatalf("open in-mem pebble: %v", err)
	}
	authStore := auth.NewStore(db)
	pebbleStore := storage.NewPebbleStore(db, storage.PebbleStoreConfig{CacheSize: 100})
	// PebbleStore.Close drains background goroutines (walSync, counterFlush,
	// provWork, transCache) AND closes the shared db via closeOnce — single
	// Close path, no double-close.
	t.Cleanup(func() { _ = pebbleStore.Close() })
	return db, authStore, pebbleStore
}
