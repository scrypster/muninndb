package storage

import (
	"fmt"

	"github.com/cockroachdb/pebble"
	"github.com/scrypster/muninndb/internal/storage/keys"
)

// WriteVaultName persists the vault name under two keys:
//
//	0x0E | wsPrefix → name   (prefix → name, for listing)
//	0x0F | siphash(name) → wsPrefix  (name → prefix, for resolution)
//
// Idempotent — safe to call on every write.
func (ps *PebbleStore) WriteVaultName(wsPrefix [8]byte, name string) error {
	// Fast path: already written this session — skip Pebble existence check.
	if _, ok := ps.vaultNameWritten.Load(wsPrefix); ok {
		return nil
	}
	metaKey := keys.VaultMetaKey(wsPrefix)
	// Only write if key doesn't already exist.
	_, closer, err := ps.db.Get(metaKey)
	if err == nil {
		closer.Close()
		// Mark as written so future calls skip this check.
		ps.vaultNameWritten.Store(wsPrefix, struct{}{})
		ps.vaultPrefixCache.Add(name, wsPrefix)
		return nil
	}
	batch := ps.db.NewBatch()
	defer batch.Close()
	batch.Set(metaKey, []byte(name), nil)
	idxKey := keys.VaultNameIndexKey(name)
	batch.Set(idxKey, wsPrefix[:], nil)
	if err := batch.Commit(nil); err != nil {
		return err
	}
	ps.vaultNameWritten.Store(wsPrefix, struct{}{})
	ps.vaultPrefixCache.Add(name, wsPrefix)
	return nil
}

// ResolveVaultPrefix looks up the actual workspace prefix for a vault name.
// Uses an in-memory cache to avoid Pebble reads on the common hot path.
func (ps *PebbleStore) ResolveVaultPrefix(name string) [8]byte {
	// Hot path: in-memory cache (populated by WriteVaultName and prior lookups).
	if ws, ok := ps.vaultPrefixCache.Get(name); ok {
		return ws
	}
	// Cold path: read from Pebble (once per vault name per process lifetime).
	idxKey := keys.VaultNameIndexKey(name)
	val, closer, err := ps.db.Get(idxKey)
	if err == nil {
		defer closer.Close()
		if len(val) == 8 {
			var ws [8]byte
			copy(ws[:], val)
			ps.vaultPrefixCache.Add(name, ws)
			return ws
		}
	}
	// Fall back to computing the SipHash and cache it.
	ws := keys.VaultPrefix(name)
	ps.vaultPrefixCache.Add(name, ws)
	return ws
}

// BackfillVaultNames scans all 0x01 engram keys, finds vault prefixes that have
// no 0x0E meta key, and writes a placeholder name for each. Called once on startup
// so that legacy data (written before vault-name persistence) is discoverable.
func (ps *PebbleStore) BackfillVaultNames() error {
	// Collect unique vault prefixes from 0x01 keys.
	seen := make(map[[8]byte]struct{})
	iter, err := ps.db.NewIter(&pebble.IterOptions{
		LowerBound: []byte{0x01},
		UpperBound: []byte{0x02},
	})
	if err != nil {
		return err
	}
	for valid := iter.First(); valid; valid = iter.Next() {
		k := iter.Key()
		if len(k) >= 25 {
			var ws [8]byte
			copy(ws[:], k[1:9])
			seen[ws] = struct{}{}
		}
	}
	iter.Close()

	// For each vault prefix, ensure both 0x0E (name) and 0x0F (forward index) are written.
	for ws := range seen {
		metaKey := keys.VaultMetaKey(ws)
		var name string

		// Determine the name (existing or new placeholder).
		val, closer, getErr := ps.db.Get(metaKey)
		if getErr == nil {
			name = string(val)
			closer.Close()
		} else {
			name = fmt.Sprintf("vault-%x", ws[:4]) // 4-byte abbreviation
		}

		// Check if forward index key exists.
		idxKey := keys.VaultNameIndexKey(name)
		_, idxCloser, idxErr := ps.db.Get(idxKey)
		if idxErr == nil {
			idxCloser.Close()
			continue // both keys already exist
		}

		// Write whichever keys are missing.
		batch := ps.db.NewBatch()
		if getErr != nil {
			batch.Set(metaKey, []byte(name), nil)
		}
		batch.Set(idxKey, ws[:], nil)
		if commitErr := batch.Commit(nil); commitErr != nil {
			batch.Close()
			return commitErr
		}
		batch.Close()
	}
	return nil
}

// VaultNameExists returns true if a vault with the given name is registered
// (i.e. a 0x0F index key exists for the name).
func (ps *PebbleStore) VaultNameExists(name string) bool {
	idxKey := keys.VaultNameIndexKey(name)
	_, closer, err := ps.db.Get(idxKey)
	if err == nil {
		closer.Close()
		return true
	}
	return false
}

// RenameVault atomically renames a vault by updating the two index keys
// (0x0E prefix→name and 0x0F nameHash→prefix). No engram data changes.
// Returns an error if oldName doesn't match the stored name for ws, or if
// newName already exists (collision).
func (ps *PebbleStore) RenameVault(ws [8]byte, oldName, newName string) error {
	// Verify 0x0E meta key exists and matches oldName.
	metaKey := keys.VaultMetaKey(ws)
	val, closer, err := ps.db.Get(metaKey)
	if err != nil {
		return fmt.Errorf("vault prefix not found: %w", err)
	}
	storedName := string(val)
	closer.Close()
	if storedName != oldName {
		return fmt.Errorf("vault name mismatch: stored %q, expected %q", storedName, oldName)
	}

	// Verify newName doesn't already exist (collision check).
	if ps.VaultNameExists(newName) {
		return fmt.Errorf("vault name %q already exists", newName)
	}

	// Atomic batch: update 0x0E, set new 0x0F, delete old 0x0F.
	batch := ps.db.NewBatch()
	defer batch.Close()
	batch.Set(metaKey, []byte(newName), nil)
	batch.Set(keys.VaultNameIndexKey(newName), ws[:], nil)
	batch.Delete(keys.VaultNameIndexKey(oldName), nil)
	if err := batch.Commit(nil); err != nil {
		return fmt.Errorf("rename vault commit: %w", err)
	}

	// Update cache: evict old, insert new.
	ps.vaultPrefixCache.Remove(oldName)
	ps.vaultPrefixCache.Add(newName, ws)
	// Clear the written flag so WriteVaultName re-checks if called with the old name.
	ps.vaultNameWritten.Delete(ws)

	return nil
}

// ListVaultNames scans the 0x0E prefix and returns all known vault names.
func (ps *PebbleStore) ListVaultNames() ([]string, error) {
	iter, err := ps.db.NewIter(&pebble.IterOptions{
		LowerBound: []byte{0x0E},
		UpperBound: []byte{0x0F},
	})
	if err != nil {
		return nil, err
	}
	defer iter.Close()

	var names []string
	for valid := iter.First(); valid; valid = iter.Next() {
		if len(iter.Key()) == 9 && iter.Key()[0] == 0x0E {
			val := make([]byte, len(iter.Value()))
			copy(val, iter.Value())
			names = append(names, string(val))
		}
	}
	return names, nil
}
