package storage

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/cockroachdb/pebble"
	"github.com/scrypster/muninndb/internal/storage/erf"
	"github.com/scrypster/muninndb/internal/storage/keys"
	"github.com/vmihailenco/msgpack/v5"
	"golang.org/x/text/unicode/norm"
)

// validEntityStates is the set of allowed lifecycle state values.
var validEntityStates = map[string]bool{
	"active": true, "deprecated": true, "merged": true, "resolved": true,
}

// EntityRecord is a named entity stored at the global 0x1F key prefix.
// Records are vault-agnostic; entity-engram links are vault-scoped at 0x20.
type EntityRecord struct {
	Name         string  `msgpack:"name"`
	Type         string  `msgpack:"type"`
	Confidence   float32 `msgpack:"confidence"`
	Source       string  `msgpack:"source"`        // "inline", "plugin:enrich", etc.
	UpdatedAt    int64   `msgpack:"updated_at"`    // Unix nanos
	FirstSeen    int64   `msgpack:"first_seen"`    // Unix nanos, set once on first upsert
	MentionCount int32   `msgpack:"mention_count"` // incremented on every upsert
	State        string  `msgpack:"state"`         // "active", "deprecated", "merged", "resolved"
	MergedInto   string  `msgpack:"merged_into"`   // set when State == "merged"
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
// Applies confidence-preserving merge: if an existing record has higher confidence,
// the existing confidence is preserved (last-writer-wins on all other fields).
// Safe for concurrent calls — uses per-entity locking to prevent TOCTOU races.
func (ps *PebbleStore) UpsertEntityRecord(ctx context.Context, record EntityRecord, source string) error {
	mu := ps.getEntityLock(record.Name)
	mu.Lock()
	defer mu.Unlock()

	nameHash := keys.EntityNameHash(record.Name)
	key := keys.EntityKey(nameHash)

	// Read existing record for confidence-preserving merge.
	existing, err := ps.GetEntityRecord(ctx, record.Name)
	if err != nil {
		return fmt.Errorf("entity record read-before-write: %w", err)
	}

	if existing != nil {
		// Preserve FirstSeen (set once, never overwritten).
		if existing.FirstSeen != 0 {
			record.FirstSeen = existing.FirstSeen
		}
		// Increment mention count.
		record.MentionCount = existing.MentionCount + 1
		// Preserve lifecycle state unless caller explicitly set it.
		if record.State == "" {
			record.State = existing.State
		}
		if record.MergedInto == "" {
			record.MergedInto = existing.MergedInto
		}
		// Preserve higher confidence.
		if existing.Confidence > record.Confidence {
			record.Confidence = existing.Confidence
		}
	} else {
		// First write.
		record.FirstSeen = time.Now().UnixNano()
		record.MentionCount = 1
		if record.State == "" {
			record.State = "active"
		}
	}

	record.Source = source
	record.UpdatedAt = time.Now().UnixNano()

	// Validate state — default to "active" if empty, error if unrecognized.
	if record.State == "" {
		record.State = "active"
	}
	if !validEntityStates[record.State] {
		return fmt.Errorf("upsert entity: invalid state %q (allowed: active, deprecated, merged, resolved)", record.State)
	}

	// MergedInto is only valid when State == "merged".
	if record.MergedInto != "" && record.State != "merged" {
		return fmt.Errorf("upsert entity: MergedInto requires State=merged, got State=%q", record.State)
	}

	val, err := msgpack.Marshal(record)
	if err != nil {
		return fmt.Errorf("entity record marshal: %w", err)
	}
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

// getEntityLock returns a per-entity mutex for the given entity name.
// Uses the same NFKC normalization as EntityNameHash for consistent keying.
func (ps *PebbleStore) getEntityLock(name string) *sync.Mutex {
	normalized := strings.ToLower(strings.TrimSpace(norm.NFKC.String(name)))
	m, _ := ps.entityLocks.LoadOrStore(normalized, &sync.Mutex{})
	return m.(*sync.Mutex)
}

// WriteEntityEngramLink writes a vault-scoped engram→entity link at 0x20
// and the corresponding entity→engram reverse index entry at 0x23.
// Both writes are committed atomically in a single Pebble batch.
// Callers MUST call UpsertEntityRecord first — this method does not verify
// the entity record exists.
func (ps *PebbleStore) WriteEntityEngramLink(ctx context.Context, ws [8]byte, engramID ULID, entityName string) error {
	nameHash := keys.EntityNameHash(entityName)
	fwdKey := keys.EntityEngramLinkKey(ws, [16]byte(engramID), nameHash)
	revKey := keys.EntityReverseIndexKey(nameHash, ws, [16]byte(engramID))

	batch := ps.db.NewBatch()
	defer batch.Close()
	if err := batch.Set(fwdKey, []byte(entityName), nil); err != nil {
		return fmt.Errorf("write entity link fwd: %w", err)
	}
	if err := batch.Set(revKey, nil, nil); err != nil {
		return fmt.Errorf("write entity link rev: %w", err)
	}
	return batch.Commit(pebble.NoSync)
}

// ScanEntityEngrams scans the 0x23 reverse index for all vault-scoped engrams
// that mention the given entity name. Calls fn for each (ws, engramID) pair.
func (ps *PebbleStore) ScanEntityEngrams(ctx context.Context, entityName string, fn func(ws [8]byte, engramID ULID) error) error {
	nameHash := keys.EntityNameHash(entityName)
	prefix := keys.EntityReverseIndexPrefix(nameHash)
	upperBound := make([]byte, len(prefix))
	copy(upperBound, prefix)
	for i := len(upperBound) - 1; i >= 0; i-- {
		upperBound[i]++
		if upperBound[i] != 0 {
			break
		}
	}

	iter, err := ps.db.NewIter(&pebble.IterOptions{LowerBound: prefix, UpperBound: upperBound})
	if err != nil {
		return fmt.Errorf("scan entity engrams: iter: %w", err)
	}
	defer iter.Close()

	for valid := iter.First(); valid; valid = iter.Next() {
		k := iter.Key()
		if len(k) != 33 { // 1 + 8 + 8 + 16
			continue
		}
		var ws [8]byte
		copy(ws[:], k[9:17])
		var idBytes [16]byte
		copy(idBytes[:], k[17:33])
		id := ULID(idBytes)
		if err := fn(ws, id); err != nil {
			return err
		}
	}
	return nil
}

// ScanEngramEntities scans the 0x20 forward index for all entities mentioned
// by the given engram in vault ws. Calls fn for each entity name found.
// Uses the EntityEngramLinkPrefix (0x20|ws|engramID) as the scan prefix;
// the value stored at each key is the raw entity name string.
func (ps *PebbleStore) ScanEngramEntities(ctx context.Context, ws [8]byte, engramID ULID, fn func(entityName string) error) error {
	prefix := keys.EntityEngramLinkPrefix(ws, [16]byte(engramID))
	upperBound := make([]byte, len(prefix))
	copy(upperBound, prefix)
	for i := len(upperBound) - 1; i >= 0; i-- {
		upperBound[i]++
		if upperBound[i] != 0 {
			break
		}
	}

	iter, err := ps.db.NewIter(&pebble.IterOptions{LowerBound: prefix, UpperBound: upperBound})
	if err != nil {
		return fmt.Errorf("scan engram entities: iter: %w", err)
	}
	defer iter.Close()

	for valid := iter.First(); valid; valid = iter.Next() {
		val := iter.Value()
		entityName := string(val)
		if entityName == "" {
			continue
		}
		if err := fn(entityName); err != nil {
			return err
		}
	}
	return nil
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

// relTypeBytes maps relationship type strings to 1-byte discriminants for the 0x21 key.
var relTypeBytes = map[string]uint8{
	"manages": 0x01, "uses": 0x02, "depends_on": 0x03,
	"implements": 0x04, "created_by": 0x05, "part_of": 0x06,
	"causes": 0x07, "contradicts": 0x08, "supports": 0x09,
}

func relTypeByteFromString(relType string) uint8 {
	if b, ok := relTypeBytes[relType]; ok {
		return b
	}
	return 0xFF
}
