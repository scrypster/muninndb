package storage

import (
	"context"
	"fmt"
	"time"

	"github.com/cockroachdb/pebble"
	"github.com/scrypster/muninndb/internal/storage/erf"
	"github.com/scrypster/muninndb/internal/storage/keys"
	"github.com/vmihailenco/msgpack/v5"
)

// EntityRecord is a named entity stored at the global 0x1F key prefix.
// Records are vault-agnostic; entity-engram links are vault-scoped at 0x20.
type EntityRecord struct {
	Name       string  `msgpack:"name"`
	Type       string  `msgpack:"type"`
	Confidence float32 `msgpack:"confidence"`
	Source     string  `msgpack:"source"`     // "inline", "plugin:enrich", etc.
	UpdatedAt  int64   `msgpack:"updated_at"` // Unix nanos
}

// RelationshipRecord is a typed entity-to-entity relationship extracted from a specific engram.
// Stored at the vault-scoped 0x21 key prefix.
type RelationshipRecord struct {
	FromEntity string  `msgpack:"from_entity"`
	ToEntity   string  `msgpack:"to_entity"`
	RelType    string  `msgpack:"rel_type"`
	Weight     float32 `msgpack:"weight"`
	Source     string  `msgpack:"source"`
	UpdatedAt  int64   `msgpack:"updated_at"`
}

// UpsertEntityRecord stores or updates a global entity record at 0x1F|nameHash.
func (ps *PebbleStore) UpsertEntityRecord(ctx context.Context, record EntityRecord, source string) error {
	nameHash := keys.EntityNameHash(record.Name)
	record.Source = source
	record.UpdatedAt = time.Now().UnixNano()
	val, err := msgpack.Marshal(record)
	if err != nil {
		return fmt.Errorf("entity record marshal: %w", err)
	}
	key := keys.EntityKey(nameHash)
	return ps.db.Set(key, val, pebble.NoSync)
}

// GetEntityRecord reads a global entity record by name. Returns nil, nil if not found.
func (ps *PebbleStore) GetEntityRecord(ctx context.Context, name string) (*EntityRecord, error) {
	nameHash := keys.EntityNameHash(name)
	key := keys.EntityKey(nameHash)
	val, err := Get(ps.db, key)
	if err != nil {
		return nil, fmt.Errorf("get entity record: %w", err)
	}
	if val == nil {
		return nil, nil
	}
	var record EntityRecord
	if err := msgpack.Unmarshal(val, &record); err != nil {
		return nil, fmt.Errorf("decode entity record: %w", err)
	}
	return &record, nil
}

// WriteEntityEngramLink writes a vault-scoped engram→entity link at 0x20.
// Value is the canonical entity name (UTF-8).
func (ps *PebbleStore) WriteEntityEngramLink(ctx context.Context, ws [8]byte, engramID ULID, entityName string) error {
	nameHash := keys.EntityNameHash(entityName)
	key := keys.EntityEngramLinkKey(ws, [16]byte(engramID), nameHash)
	return ps.db.Set(key, []byte(entityName), pebble.NoSync)
}

// UpsertRelationshipRecord writes a vault-scoped relationship record at 0x21.
func (ps *PebbleStore) UpsertRelationshipRecord(ctx context.Context, ws [8]byte, engramID ULID, record RelationshipRecord) error {
	record.UpdatedAt = time.Now().UnixNano()
	val, err := msgpack.Marshal(record)
	if err != nil {
		return fmt.Errorf("relationship record marshal: %w", err)
	}
	fromHash := keys.EntityNameHash(record.FromEntity)
	toHash := keys.EntityNameHash(record.ToEntity)
	relTypeByte := relTypeByteFromString(record.RelType)
	key := keys.RelationshipKey(ws, [16]byte(engramID), fromHash, relTypeByte, toHash)
	return ps.db.Set(key, val, pebble.NoSync)
}

// UpdateDigest updates the summary, key points, and memory type fields on an
// existing engram identified by id. The engram's vault prefix is resolved via
// FindVaultPrefix. Both 0x01 (full engram) and 0x02 (meta slice) keys are
// updated atomically, and the L1/meta caches are invalidated.
func (ps *PebbleStore) UpdateDigest(ctx context.Context, id ULID, summary string, keyPoints []string, memoryType string) error {
	ws, ok := ps.FindVaultPrefix(id)
	if !ok {
		return fmt.Errorf("UpdateDigest: engram %s not found", id.String())
	}

	eng, err := ps.GetEngram(ctx, ws, id)
	if err != nil {
		return fmt.Errorf("UpdateDigest: get engram: %w", err)
	}

	// Only overwrite fields that were provided (non-empty).
	if summary != "" {
		eng.Summary = summary
	}
	if len(keyPoints) > 0 {
		eng.KeyPoints = keyPoints
	}
	if memoryType != "" {
		if mt, ok := ParseMemoryType(memoryType); ok {
			eng.MemoryType = mt
		}
	}
	eng.UpdatedAt = time.Now()

	erfEng := toERFEngram(eng)
	erfBytes, err := erf.EncodeV2(erfEng)
	if err != nil {
		return fmt.Errorf("UpdateDigest: encode engram: %w", err)
	}

	batch := ps.db.NewBatch()
	defer batch.Close()

	engramKey := keys.EngramKey(ws, [16]byte(id))
	batch.Set(engramKey, erfBytes, nil)

	metaKey := keys.MetaKey(ws, [16]byte(id))
	metaSlice := erfBytes
	if len(metaSlice) > erf.MetaKeySize {
		metaSlice = metaSlice[:erf.MetaKeySize]
	}
	batch.Set(metaKey, metaSlice, nil)

	// Invalidate caches before commit — cached structs are stale.
	ps.cache.Delete(ws, id)
	ps.metaCache.Remove([16]byte(id))

	if err := batch.Commit(pebble.NoSync); err != nil {
		return fmt.Errorf("UpdateDigest: commit: %w", err)
	}

	return nil
}

// relTypeByteFromString maps a relationship type string to a 1-byte discriminant.
func relTypeByteFromString(relType string) uint8 {
	m := map[string]uint8{
		"manages":     0x01,
		"uses":        0x02,
		"depends_on":  0x03,
		"implements":  0x04,
		"created_by":  0x05,
		"part_of":     0x06,
		"causes":      0x07,
		"contradicts": 0x08,
		"supports":    0x09,
	}
	if b, ok := m[relType]; ok {
		return b
	}
	return 0xFF
}
