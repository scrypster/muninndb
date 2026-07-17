package migrate

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/cockroachdb/pebble"
	"github.com/scrypster/muninndb/internal/auth"
	"github.com/scrypster/muninndb/internal/prefix"
)

// TestRelocateAuthPrefixes_RekeysAndLeavesStorage validates the four auth
// families are re-keyed from their pre-#611 colocated prefixes (0x11–0x14)
// onto the new dedicated auth range (0x42–0x45), while storage keys that
// share those prefix bytes are LEFT UNCHANGED.
//
// The >100-record apiKey seed is load-bearing: batchSize=100, so a 150-record
// seed forces the batch-reset path (commit at 100 → batch.Close+NewBatch →
// accumulate 101–150 → final commit). A 1-record seed could not catch the
// RT1 batch-reuse regression.
func TestRelocateAuthPrefixes_RekeysAndLeavesStorage(t *testing.T) {
	db := openTestDB(t)

	// 150 apiKeys at 0x12 (old Coherence byte) — hits the batch boundary at 100.
	for i := 0; i < 150; i++ {
		hash := make([]byte, 16)
		binary.BigEndian.PutUint64(hash, uint64(i))
		apiKeyJSON, _ := json.Marshal(auth.APIKey{
			ID:    fmt.Sprintf("k%d", i),
			Vault: "v",
			Mode:  auth.ModeFull,
		})
		oldKey := append([]byte{prefix.Coherence}, hash...)
		if err := db.Set(oldKey, apiKeyJSON, pebble.Sync); err != nil {
			t.Fatalf("seed apiKey %d: %v", i, err)
		}
	}

	// admin @0x11 — Username matches the key suffix (the predicate's round-trip check).
	adminJSON, _ := json.Marshal(auth.AdminUser{Username: "root", PassHash: []byte("ph")})
	adminOldKey := append([]byte{prefix.DigestFlags}, []byte("root")...)
	if err := db.Set(adminOldKey, adminJSON, pebble.Sync); err != nil {
		t.Fatalf("seed admin: %v", err)
	}

	// apiKeyVIdx @0x13 (old VaultWeights byte) — vault + 0x00 + 8-byte keyID, len >= 10.
	vidxSuffix := []byte("vault-x\x00abcdefgh") // 17 bytes → total key len 18
	vidxOldKey := append([]byte{prefix.VaultWeights}, vidxSuffix...)
	if err := db.Set(vidxOldKey, []byte("idx"), pebble.Sync); err != nil {
		t.Fatalf("seed vidx: %v", err)
	}

	// vaultCfg @0x14 (old AssocWeightIndex byte) — Name matches the key suffix.
	cfgJSON, _ := json.Marshal(auth.VaultConfig{Name: "default"})
	cfgOldKey := append([]byte{prefix.AssocWeightIndex}, []byte("default")...)
	if err := db.Set(cfgOldKey, cfgJSON, pebble.Sync); err != nil {
		t.Fatalf("seed cfg: %v", err)
	}

	// Storage keys that share the prefix ranges — MUST be LEFT UNCHANGED.
	// DigestFlags @0x11: 17-byte key (1 + 16 ULID), 1-byte value.
	digestKey := append([]byte{prefix.DigestFlags}, make([]byte, 16)...)
	digestVal := []byte{0x07}
	if err := db.Set(digestKey, digestVal, pebble.Sync); err != nil {
		t.Fatalf("seed digest: %v", err)
	}
	// AssocWeightIndex @0x14: 41-byte key, 4-byte big-endian float32 weight
	// (mirrors the real storage write at association.go:410-412). The migration
	// must not touch this — a 4-byte value is structurally impossible for any
	// vaultCfg JSON ('{"...":...}' is many B).
	awKey := append([]byte{prefix.AssocWeightIndex}, make([]byte, 40)...)
	awVal := binary.BigEndian.AppendUint32([]byte{}, 0x3F800000) // 1.0f32
	if err := db.Set(awKey, awVal, pebble.Sync); err != nil {
		t.Fatalf("seed aw: %v", err)
	}

	if err := RelocateAuthPrefixes(db); err != nil {
		t.Fatalf("relocate: %v", err)
	}

	// All 150 apiKeys now at 0x43 (new APIKey prefix); values intact; old keys gone.
	for i := 0; i < 150; i++ {
		hash := make([]byte, 16)
		binary.BigEndian.PutUint64(hash, uint64(i))
		oldKey := append([]byte{prefix.Coherence}, hash...)
		newKey := append([]byte{prefix.APIKey}, hash...)

		_, _, err := db.Get(oldKey)
		if !errors.Is(err, pebble.ErrNotFound) {
			t.Fatalf("apiKey %d: old key %x still present (err=%v)", i, oldKey, err)
		}
		v, closer, err := db.Get(newKey)
		if err != nil {
			t.Fatalf("apiKey %d: get new key: %v", i, err)
		}
		var parsed auth.APIKey
		if err := json.Unmarshal(v, &parsed); err != nil {
			t.Fatalf("apiKey %d: unmarshal: %v", i, err)
		}
		closer.Close()
		if parsed.ID != fmt.Sprintf("k%d", i) {
			t.Fatalf("apiKey %d: ID = %q, want k%d", i, parsed.ID, i)
		}
		if parsed.Mode != auth.ModeFull {
			t.Fatalf("apiKey %d: Mode = %q, want %q", i, parsed.Mode, auth.ModeFull)
		}
	}

	// admin now at 0x42; old admin key gone.
	newAdminKey := append([]byte{prefix.AdminUser}, []byte("root")...)
	v, closer, err := db.Get(newAdminKey)
	if err != nil {
		t.Fatalf("get admin at new prefix: %v", err)
	}
	var u auth.AdminUser
	if err := json.Unmarshal(v, &u); err != nil {
		t.Fatalf("unmarshal admin: %v", err)
	}
	closer.Close()
	if u.Username != "root" {
		t.Fatalf("admin username = %q, want root", u.Username)
	}
	_, _, err = db.Get(adminOldKey)
	if !errors.Is(err, pebble.ErrNotFound) {
		t.Fatalf("old admin key still present or wrong err: %v", err)
	}

	// apiKeyVIdx now at 0x44; old gone.
	newVidxKey := append([]byte{prefix.APIKeyVaultIdx}, vidxSuffix...)
	v, closer, err = db.Get(newVidxKey)
	if err != nil {
		t.Fatalf("get vidx at new prefix: %v", err)
	}
	if string(v) != "idx" {
		t.Fatalf("vidx value = %q, want idx", string(v))
	}
	closer.Close()
	_, _, err = db.Get(vidxOldKey)
	if !errors.Is(err, pebble.ErrNotFound) {
		t.Fatalf("old vidx key still present or wrong err: %v", err)
	}

	// vaultCfg now at 0x45; old gone.
	newCfgKey := append([]byte{prefix.VaultConfig}, []byte("default")...)
	v, closer, err = db.Get(newCfgKey)
	if err != nil {
		t.Fatalf("get cfg at new prefix: %v", err)
	}
	var c auth.VaultConfig
	if err := json.Unmarshal(v, &c); err != nil {
		t.Fatalf("unmarshal cfg: %v", err)
	}
	closer.Close()
	if c.Name != "default" {
		t.Fatalf("cfg name = %q, want default", c.Name)
	}
	_, _, err = db.Get(cfgOldKey)
	if !errors.Is(err, pebble.ErrNotFound) {
		t.Fatalf("old cfg key still present or wrong err: %v", err)
	}

	// Storage keys MUST be unchanged.
	v, closer, err = db.Get(digestKey)
	if err != nil {
		t.Fatalf("get digest (should be unchanged): %v", err)
	}
	if !bytes.Equal(v, digestVal) {
		t.Fatalf("digest value changed: got %x, want %x", v, digestVal)
	}
	closer.Close()

	v, closer, err = db.Get(awKey)
	if err != nil {
		t.Fatalf("get AssocWeightIndex (should be unchanged): %v", err)
	}
	if !bytes.Equal(v, awVal) {
		t.Fatalf("AssocWeightIndex value changed: got %x, want %x", v, awVal)
	}
	closer.Close()
}

// TestRelocateAuthPrefixes_LengthCollisionDoesNotOrphanAuth is the RT regression
// test for the isAuthKey length-short-circuit bug: when a real auth record's
// KEY length happens to equal the storage family's key length on the SAME
// prefix byte, the length-only short-circuit misclassified it as storage and
// silently LEFT it at the old prefix (orphaned).
//
// Two collisions are covered:
//
//  1. DigestFlags @0x11: storage key is 1+16=17B; an admin with a 16-char
//     username is 1+16=17B — same length, different value shape (storage value
//     is 1 byte; admin JSON is multi-byte starting with '{').
//  2. AssocWeightIndex @0x14: storage key is 41B; a vaultCfg with a 40-char
//     vault name is 1+40=41B — same length, different value shape (storage
//     value is 4-byte weight; vaultCfg JSON is multi-byte starting with '{').
//
// Post-migration the orphaned admin would be invisible to AdminExists (which
// scans 0x42), causing Bootstrap to create default root/password — silent
// lockout + security regression. This test would have caught both.
func TestRelocateAuthPrefixes_LengthCollisionDoesNotOrphanAuth(t *testing.T) {
	db := openTestDB(t)

	// 16-char username admin at 0x11 — key length 1+16=17, COLLIDES with
	// storage DigestFlags (1+16-byte ULID). The pre-fix length short-circuit
	// returned "storage, leave it" and silently orphaned this admin.
	const adminUser = "administrator123" // exactly 16 chars (13+3)
	if len(adminUser) != 16 {
		t.Fatalf("admin seed length = %d, want 16", len(adminUser))
	}
	adminJSON, _ := json.Marshal(auth.AdminUser{Username: adminUser, PassHash: []byte("ph")})
	adminOldKey := append([]byte{prefix.DigestFlags}, []byte(adminUser)...)
	if err := db.Set(adminOldKey, adminJSON, pebble.Sync); err != nil {
		t.Fatalf("seed 16-char admin: %v", err)
	}

	// 40-char vault name vaultCfg at 0x14 — key length 1+40=41, COLLIDES with
	// storage AssocWeightIndex (41B). Same silent-orphan failure mode.
	const vaultName = "abcdefghijklmnopqrstuvwxyz0123456789abcd" // exactly 40 chars
	if len(vaultName) != 40 {
		t.Fatalf("vault name seed length = %d, want 40", len(vaultName))
	}
	cfgJSON, _ := json.Marshal(auth.VaultConfig{Name: vaultName})
	cfgOldKey := append([]byte{prefix.AssocWeightIndex}, []byte(vaultName)...)
	if err := db.Set(cfgOldKey, cfgJSON, pebble.Sync); err != nil {
		t.Fatalf("seed 40-char vaultCfg: %v", err)
	}

	if err := RelocateAuthPrefixes(db); err != nil {
		t.Fatalf("relocate (length-collision case): %v", err)
	}

	// The 16-char admin MUST now be at 0x42 (AdminUser) — NOT orphaned at 0x11.
	newAdminKey := append([]byte{prefix.AdminUser}, []byte(adminUser)...)
	v, closer, err := db.Get(newAdminKey)
	if err != nil {
		t.Fatalf("16-char admin NOT relocated to 0x42 (orphaned at 0x11): %v", err)
	}
	var u auth.AdminUser
	if err := json.Unmarshal(v, &u); err != nil {
		t.Fatalf("unmarshal 16-char admin: %v", err)
	}
	closer.Close()
	if u.Username != adminUser {
		t.Fatalf("admin username round-trip = %q, want %q", u.Username, adminUser)
	}
	if _, _, err := db.Get(adminOldKey); !errors.Is(err, pebble.ErrNotFound) {
		t.Fatalf("16-char admin old key at 0x11 still present (expected relocated, not orphaned): %v", err)
	}

	// The 40-char vaultCfg MUST now be at 0x45 (VaultConfig) — NOT orphaned at 0x14.
	newCfgKey := append([]byte{prefix.VaultConfig}, []byte(vaultName)...)
	v, closer, err = db.Get(newCfgKey)
	if err != nil {
		t.Fatalf("40-char vaultCfg NOT relocated to 0x45 (orphaned at 0x14): %v", err)
	}
	var c auth.VaultConfig
	if err := json.Unmarshal(v, &c); err != nil {
		t.Fatalf("unmarshal 40-char vaultCfg: %v", err)
	}
	closer.Close()
	if c.Name != vaultName {
		t.Fatalf("vaultCfg name round-trip = %q, want %q", c.Name, vaultName)
	}
	if _, _, err := db.Get(cfgOldKey); !errors.Is(err, pebble.ErrNotFound) {
		t.Fatalf("40-char vaultCfg old key at 0x14 still present (expected relocated, not orphaned): %v", err)
	}
}

// TestRelocateAuthPrefixes_FailLoudOnCorruptedAuthValue proves the RT4 fail-loud
// path: a value at 0x11 that is NOT a valid admin JSON document causes the
// migration to return a non-nil error and refuse to silently orphan it.
// The migration version is NOT stamped 3 in this case; the operator runs
// recovery (Task 7b), fixes the value, and re-runs.
func TestRelocateAuthPrefixes_FailLoudOnCorruptedAuthValue(t *testing.T) {
	db := openTestDB(t)
	corruptKey := append([]byte{prefix.DigestFlags}, []byte("root")...)
	if err := db.Set(corruptKey, []byte("{not json"), pebble.Sync); err != nil {
		t.Fatalf("seed corrupt: %v", err)
	}
	if err := RelocateAuthPrefixes(db); err == nil {
		t.Fatal("expected error on corrupted admin value at 0x11, got nil (silent orphan risk)")
	}
	// The corrupted record MUST still be at the old prefix — migration refused to touch it.
	v, closer, err := db.Get(corruptKey)
	if err != nil {
		t.Fatalf("corrupt record missing after failed migration: %v", err)
	}
	closer.Close()
	if string(v) != "{not json" {
		t.Fatalf("corrupt value mutated: got %q", v)
	}
}

// TestRelocateAuthPrefixes_Idempotent proves re-running the migration on an
// already-migrated DB is a no-op: nothing commits, nothing breaks.
func TestRelocateAuthPrefixes_Idempotent(t *testing.T) {
	db := openTestDB(t)

	// Seed one record per auth family, run once.
	adminJSON, _ := json.Marshal(auth.AdminUser{Username: "root"})
	if err := db.Set(append([]byte{prefix.DigestFlags}, []byte("root")...), adminJSON, pebble.Sync); err != nil {
		t.Fatal(err)
	}
	apiKeyJSON, _ := json.Marshal(auth.APIKey{ID: "k0", Vault: "v", Mode: auth.ModeFull})
	hash := make([]byte, 16)
	apiKeyOldKey := append([]byte{prefix.Coherence}, hash...)
	if err := db.Set(apiKeyOldKey, apiKeyJSON, pebble.Sync); err != nil {
		t.Fatal(err)
	}

	if err := RelocateAuthPrefixes(db); err != nil {
		t.Fatalf("first run: %v", err)
	}
	// Second run: old-prefix ranges are empty; must succeed with no mutations.
	if err := RelocateAuthPrefixes(db); err != nil {
		t.Fatalf("second run (idempotent): %v", err)
	}

	// Records still at the new prefixes after the second run.
	v, closer, err := db.Get(append([]byte{prefix.AdminUser}, []byte("root")...))
	if err != nil {
		t.Fatalf("admin missing after second run: %v", err)
	}
	closer.Close()
	v, closer, err = db.Get(append([]byte{prefix.APIKey}, hash...))
	if err != nil {
		t.Fatalf("apiKey missing after second run: %v", err)
	}
	closer.Close()
	if string(v) != string(apiKeyJSON) {
		t.Fatalf("apiKey value mutated on idempotent re-run")
	}
}
