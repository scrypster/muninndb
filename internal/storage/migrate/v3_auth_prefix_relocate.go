package migrate

import (
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/cockroachdb/pebble"
	"github.com/scrypster/muninndb/internal/auth"
	"github.com/scrypster/muninndb/internal/prefix"
)

// RelocateAuthPrefixes is the v3 migration (#611): re-keys the four auth
// families from their pre-relocation colocated prefixes (0x11–0x14, where
// they sat inside storage's range) onto the new dedicated auth range
// 0x42–0x45 introduced by Task 2.
//
// Old → New:
//
//	0x11 (DigestFlags byte) → 0x42 AdminUser       (JSON name round-trip)
//	0x12 (Coherence byte)   → 0x43 APIKey          (length only: 1+16)
//	0x13 (VaultWeights byte)→ 0x44 APIKeyVaultIdx  (length only: >=10)
//	0x14 (AssocWeightIdx)   → 0x45 VaultConfig     (JSON name round-trip)
//
// Each old prefix byte is shared between storage and (legacy) auth, so the
// per-key discriminator isAuthKey decides for every key. The migration is
// idempotent: a re-run finds the old-prefix ranges empty → count=0, nothing
// commits, storage is never matched.
//
// On a corrupted auth value at 0x11/0x14 (JSON parse failure), the migration
// FAILS LOUD: the error is returned to the caller, the migration version is
// NOT stamped 3, and the operator runs recovery (Task 7b), fixes the value,
// and re-runs. The migration never silently orphans an auth record.
func RelocateAuthPrefixes(db *pebble.DB) error {
	if err := relocatePrefix(db, prefix.DigestFlags, prefix.AdminUser); err != nil {
		return fmt.Errorf("relocate auth prefixes: admin (0x11→0x42): %w", err)
	}
	if err := relocatePrefix(db, prefix.Coherence, prefix.APIKey); err != nil {
		return fmt.Errorf("relocate auth prefixes: apiKey (0x12→0x43): %w", err)
	}
	if err := relocatePrefix(db, prefix.VaultWeights, prefix.APIKeyVaultIdx); err != nil {
		return fmt.Errorf("relocate auth prefixes: apiKeyVIdx (0x13→0x44): %w", err)
	}
	if err := relocatePrefix(db, prefix.AssocWeightIndex, prefix.VaultConfig); err != nil {
		return fmt.Errorf("relocate auth prefixes: vaultCfg (0x14→0x45): %w", err)
	}
	return nil
}

// relocatePrefix scans every key under oldPfx, asks isAuthKey whether each one
// belongs to the auth family (vs. the storage family that shares the prefix
// byte), and if so writes it under newPfx and deletes the old key.
//
// [RT-FIX RT1] The batch is closed and re-allocated after every Commit —
// mirroring internal/storage/migrate/v2_rel_entity_index.go. Reusing a batch
// across commits grows its internal memtable without bound and triggers
// ErrBatchTooLarge on large auth sets (the original sketch's O(N²) bug).
//
// [RT-FIX RT4] isAuthKey returns (auth, parseErr). A JSON-parse failure on
// a prefix whose discriminator is JSON-based (0x11/0x14) is RETURNED from
// this function as a non-nil error, failing the migration loud. Storage
// keys in the shared range are silently skipped (counted in `skipped`).
func relocatePrefix(db *pebble.DB, oldPfx, newPfx byte) error {
	iter, err := db.NewIter(&pebble.IterOptions{
		LowerBound: []byte{oldPfx},
		UpperBound: []byte{oldPfx + 1},
	})
	if err != nil {
		return fmt.Errorf("new iter (0x%02x): %w", oldPfx, err)
	}
	defer iter.Close()

	const batchSize = 100
	batch := db.NewBatch()
	count, skipped := 0, 0

	commit := func() error {
		if err := batch.Commit(pebble.Sync); err != nil {
			return fmt.Errorf("commit batch (0x%02x): %w", oldPfx, err)
		}
		// [RT-FIX RT1] v2 idiom — do NOT reuse the batch across commits.
		batch.Close()
		batch = db.NewBatch()
		return nil
	}
	defer batch.Close()

	for valid := iter.First(); valid; valid = iter.Next() {
		oldKey := append([]byte(nil), iter.Key()...)
		val, err := iter.ValueAndErr()
		if err != nil {
			return fmt.Errorf("iter value (0x%02x) at %x: %w", oldPfx, oldKey, err)
		}

		isAuth, parseErr := isAuthKey(oldPfx, oldKey, val)
		if parseErr != nil {
			return fmt.Errorf("relocate 0x%02x: unparseable value at %x (possible corrupted auth record — refusing to silently orphan): %w", oldPfx, oldKey, parseErr)
		}
		if !isAuth {
			skipped++ // storage key in the shared range — leave it
			continue
		}

		newKey := append([]byte{newPfx}, oldKey[1:]...)
		batch.Set(newKey, append([]byte(nil), val...), nil)
		batch.Delete(oldKey, nil)
		count++

		if count%batchSize == 0 {
			if err := commit(); err != nil {
				return err
			}
		}
	}

	if err := iter.Error(); err != nil {
		return fmt.Errorf("iter error (0x%02x): %w", oldPfx, err)
	}

	if count%batchSize != 0 {
		if err := commit(); err != nil {
			return err
		}
	}

	slog.Info("relocate auth prefix",
		"from", fmt.Sprintf("0x%02x", oldPfx),
		"to", fmt.Sprintf("0x%02x", newPfx),
		"relocated", count,
		"skipped_storage", skipped,
	)
	return nil
}

// isAuthKey returns (isAuth, parseErr) for a key at the given old-prefix byte.
//
// parseErr is non-nil ONLY for a value that LOOKS like it should be an auth
// record (a key on the 0x11/0x14 prefix whose length is NOT the storage
// length) but fails to JSON-parse as the expected auth struct — a corrupted
// admin or vaultCfg that must NOT be silently orphaned.
//
// Length-only discriminators (0x12, 0x13) never return a parse error: there
// is no JSON to parse, so there is no fail-loud path — the length either
// matches auth or it matches storage.
//
// NOTE: the switch matches on the OLD prefix byte (the byte the caller is
// scanning), so the case labels use the storage prefix constants that STILL
// own those bytes (DigestFlags=0x11, Coherence=0x12, VaultWeights=0x13,
// AssocWeightIndex=0x14). The case for 0x12 uses prefix.Coherence (not
// prefix.APIKey, which == 0x43 after Task 2's relocation) — using the new
// auth constant here would never match and silently skip every API key.
func isAuthKey(pfx byte, key, val []byte) (bool, error) {
	switch pfx {
	case prefix.DigestFlags: // old 0x11 — admin was colocated here
		// [RT-FIX RT2] Value-shape gate. Storage DigestFlags value is a 1-byte
		// uint8 bitfield; an admin user's value is multi-byte JSON (never 1B).
		// Without the value gate, a 16-char username admin (key 1+16=17B)
		// collides with storage's 1+16-byte-ULID key (also 17B) and is silently
		// orphaned at 0x11 — invisible to AdminExists post-migration → Bootstrap
		// creates default root/password → silent lockout + security regression.
		//
		// Exact length is tighter than a JSON-marker byte: a 1-byte value is
		// structurally impossible for any admin JSON ('{"...":...}' is many B).
		if len(key) == 17 && len(val) == 1 {
			return false, nil // storage DigestFlags (1 + 16 ULID, 1-byte bitfield) — leave
		}
		var u auth.AdminUser
		if err := json.Unmarshal(val, &u); err != nil {
			return false, err // corrupted admin — fail loud
		}
		return string(key[1:]) == u.Username, nil
	case prefix.Coherence: // old 0x12 — apiKey was colocated here
		// Length-only: auth APIKey is 1+16=17; storage Coherence is 1+8=9.
		return len(key) == prefix.MinAPIKeyLen, nil
	case prefix.VaultWeights: // old 0x13 — apiKeyVIdx was colocated here
		// Length-only: auth APIKeyVaultIdx is 1+vault+0x00+8 >= 10; storage VaultWeights is 1+8=9.
		return len(key) >= prefix.MinAPIKeyVIdxLen, nil
	case prefix.AssocWeightIndex: // old 0x14 — vaultCfg was colocated here
		// [RT-FIX RT2] Value-shape gate. Storage AssocWeightIndex value is a
		// 4-byte big-endian float32 weight (association.go:410-412); a vault
		// config's value is multi-byte JSON (never 4B).
		// Without the value gate, a 40-char vault name (key 1+40=41B) collides
		// with storage's 41B key and is silently orphaned at 0x14 — invisible
		// post-migration, loses the vault's plasticity/public config.
		//
		// Truth check: encodeAssocValue writes to AssocFwdKey (0x03)/AssocRevKey
		// (0x04), NOT to AssocWeightIndex (0x14). The earlier comment claiming
		// 26-byte 0x14 values confused the 0x03/0x04 struct for the 0x14 weight.
		// Exact length is tighter than a JSON-marker byte: a 4-byte value is
		// structurally impossible for any vaultCfg JSON ('{"...":...}' is many B).
		if len(key) == 41 && len(val) == 4 {
			return false, nil // storage AssocWeightIndex (41B key, 4-byte float32 weight) — leave
		}
		var c auth.VaultConfig
		if err := json.Unmarshal(val, &c); err != nil {
			return false, err // corrupted vaultCfg — fail loud
		}
		return string(key[1:]) == c.Name, nil
	}
	return false, nil
}
