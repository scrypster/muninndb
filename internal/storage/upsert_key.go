package storage

import (
	"context"
	"fmt"

	"github.com/cockroachdb/pebble"
	"github.com/scrypster/muninndb/internal/storage/keys"
)

// GetUpsertKey looks up the engram ID pinned to a given upsert key (the sha256
// of the caller's idempotent_id) within a vault. Returns (ULID{}, nil) when no
// mapping exists — the caller then creates a fresh engram. A present-but-malformed
// value is an error, never a silent miss (a corrupted index entry must fail loud).
//
// The durable forward index itself is populated by PutUpsertKey on a create and
// re-pointed atomically with the evolve on a content change (StoreBatch.RepointUpsertKey
// inside EvolveAt's batch). Issue #556.
func (ps *PebbleStore) GetUpsertKey(ctx context.Context, wsPrefix [8]byte, keyHash [32]byte) (ULID, error) {
	key := keys.UpsertKeyKey(wsPrefix, keyHash)
	val, err := Get(ps.db, key)
	if err != nil {
		return ULID{}, fmt.Errorf("get upsert key: %w", err)
	}
	if val == nil {
		return ULID{}, nil
	}
	if len(val) != 16 {
		return ULID{}, fmt.Errorf("upsert key value has unexpected length %d", len(val))
	}
	var id ULID
	copy(id[:], val)
	return id, nil
}

// PutUpsertKey stores an upsert key → engram ID mapping for a vault. Used by
// the upsert orchestrator (Engine.writeUpsert) to pin a freshly created engram
// to sha256(idempotent_id) after the default Write path lands it. Mirrors
// PutContentHash (content_hash.go) in shape — a simple, non-batched Set of the
// 0x2F pointer. The re-point on a content-change evolve is queued into the
// evolve's own batch via StoreBatch.RepointUpsertKey so it commits atomically
// with the successor; this method is only for the create / stale-recreate path
// where the engram has just been written under the default Write path's batch.
// Issue #556.
func (ps *PebbleStore) PutUpsertKey(ctx context.Context, wsPrefix [8]byte, keyHash [32]byte, id ULID) error {
	key := keys.UpsertKeyKey(wsPrefix, keyHash)
	if err := ps.db.Set(key, id[:], pebble.NoSync); err != nil {
		return fmt.Errorf("put upsert key: %w", err)
	}
	return nil
}
