package storage

import (
	"context"
	"crypto/sha256"
	"fmt"

	"github.com/cockroachdb/pebble"
	"github.com/scrypster/muninndb/internal/storage/keys"
)

// ContentHash computes the SHA-256 hash of a content string.
func ContentHash(content string) [32]byte {
	return sha256.Sum256([]byte(content))
}

// GetContentHash looks up an existing engram ID by content hash within a vault.
// Returns (ULID{}, nil) if no mapping exists.
func (ps *PebbleStore) GetContentHash(ctx context.Context, wsPrefix [8]byte, hash [32]byte) (ULID, error) {
	key := keys.ContentHashKey(wsPrefix, hash)
	val, err := Get(ps.db, key)
	if err != nil {
		return ULID{}, fmt.Errorf("get content hash: %w", err)
	}
	if val == nil {
		return ULID{}, nil
	}
	if len(val) != 16 {
		return ULID{}, fmt.Errorf("content hash value has unexpected length %d", len(val))
	}
	var id ULID
	copy(id[:], val)
	return id, nil
}

// PutContentHash stores a content hash → engram ID mapping for a vault.
func (ps *PebbleStore) PutContentHash(ctx context.Context, wsPrefix [8]byte, hash [32]byte, id ULID) error {
	key := keys.ContentHashKey(wsPrefix, hash)
	if err := ps.db.Set(key, id[:], pebble.NoSync); err != nil {
		return fmt.Errorf("put content hash: %w", err)
	}
	return nil
}

// DeleteContentHash removes a content hash mapping for a vault.
// Used when an engram is hard-deleted so the hash slot becomes available.
func (ps *PebbleStore) DeleteContentHash(ctx context.Context, wsPrefix [8]byte, hash [32]byte) error {
	key := keys.ContentHashKey(wsPrefix, hash)
	if err := ps.db.Delete(key, pebble.NoSync); err != nil {
		return fmt.Errorf("delete content hash: %w", err)
	}
	return nil
}
