package storage

// Entity Graph Key Space Design
//
// MuninnDB maintains a two-layer entity graph: a global entity registry and a
// vault-scoped relationship graph. Both are stored in Pebble using the following
// key prefixes. All writes in this file use pebble.NoSync + walSyncer group-commit
// (≤10ms durability window). See storage/wal_syncer.go for the durability contract.
//
// ┌─────────────────────────────────────────────────────────────────────────────┐
// │ Prefix │ Scope  │ Key Layout                              │ Value           │
// ├────────┼────────┼─────────────────────────────────────────┼─────────────────┤
// │ 0x1F   │ Global │ 0x1F | nameHash(8)                      │ msgpack(Entity) │
// │ 0x20   │ Vault  │ 0x20 | ws(8) | engramID(16) | hash(8)  │ entityName(str) │
// │ 0x21   │ Vault  │ 0x21 | ws(8) | engramID(16) | fromH(8) │                 │
// │        │        │        | relTypeByte(1) | toH(8)        │ msgpack(Rel)    │
// │ 0x23   │ Cross  │ 0x23 | nameHash(8) | ws(8) | engramID  │ empty           │
// │ 0x24   │ Vault  │ 0x24 | ws(8) | hashA(8) | hashB(8)     │ msgpack(CoOcc)  │
// └─────────────────────────────────────────────────────────────────────────────┘
//
// Prefix 0x1F — Global Entity Registry
//   Key:   0x1F | SipHash(NFKC-normalized entity name)(8 bytes)
//   Value: msgpack-encoded EntityRecord (name, type, confidence, source, timestamps,
//          mentionCount, state, mergedInto)
//   Scope: Global (no vault isolation) — entity identity is cross-vault
//   Mutex: Per-entity lock via getEntityLock(nameHash) prevents TOCTOU in UpsertEntityRecord
//   Merge: Confidence-preserving: max(existing, new); other fields are last-writer-wins
//
// Prefix 0x20 — Engram→Entity Forward Link Index (Vault-Scoped)
//   Key:   0x20 | ws(8) | engramID(16) | entityNameHash(8)  [33 bytes total]
//   Value: Raw entity name string
//   Query: "Which entities does engram X mention?" — scan prefix 0x20|ws|engramID
//   Write: WriteEntityEngramLink — also writes the 0x23 reverse key atomically
//
// Prefix 0x21 — Entity Relationship Records (Vault-Scoped)
//   Key:   0x21 | ws(8) | engramID(16) | fromHash(8) | relTypeByte(1) | toHash(8)
//          [42 bytes total]
//   Value: msgpack-encoded RelationshipRecord (fromEntity, toEntity, relType, weight, source)
//   Semantics: Per-engram relationship assertion — each engram that describes a relationship
//              writes its own 0x21 key. ExportGraph deduplicates by max-weight per triple.
//   RelType mapping: see relTypeBytes map (0x01=supports, ..., 0x0A=co_occurs_with, 0xFF=unknown)
//
// Prefix 0x23 — Entity→Engram Reverse Index (Cross-Vault)
//   Key:   0x23 | entityNameHash(8) | ws(8) | engramID(16)  [33 bytes total]
//   Value: Empty (key encodes all data)
//   Query: "Which engrams mention entity Y, across all vaults?" — scan prefix 0x23|nameHash
//   Written atomically with the 0x20 forward key in WriteEntityEngramLink
//
// Prefix 0x24 — Entity Co-Occurrence Index (Vault-Scoped)
//   Key:   0x24 | ws(8) | hashA(8) | hashB(8)  [25 bytes total]
//          Canonical order: hashA ≤ hashB byte-by-byte (ensures (A,B) == (B,A))
//   Value: msgpack-encoded coOccurrenceRecord (nameA, nameB, count uint32)
//   Written by IncrementEntityCoOccurrence after each engram write with ≥2 entities
//   Mutex: Per-pair lock via getCoOccurrenceLock prevents TOCTOU on increment

import (
	"context"
	"errors"
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
		// Preserve MergedInto only when the entity remains merged.
		// A caller explicitly transitioning to a non-merged state (e.g. "active")
		// must clear MergedInto — preserving it would cause the validation below to reject the write.
		if record.MergedInto == "" && record.State == "merged" {
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

// getCoOccurrenceLock returns a per-pair mutex for the given canonical hash pair.
// hashA and hashB must already be canonicalized (hashA <= hashB).
func (ps *PebbleStore) getCoOccurrenceLock(hashA, hashB [8]byte) *sync.Mutex {
	// Create a string key from the two 8-byte hashes (16 bytes total)
	key := string(hashA[:]) + string(hashB[:])
	m, _ := ps.coOccurrenceLocks.LoadOrStore(key, &sync.Mutex{})
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

// coOccurrenceRecord is the msgpack value stored at each 0x24 co-occurrence key.
type coOccurrenceRecord struct {
	NameA string `msgpack:"a"`
	NameB string `msgpack:"b"`
	Count uint32 `msgpack:"n"`
}

// IncrementEntityCoOccurrence increments the co-occurrence count for a pair of
// entity names within a vault. The pair is stored in canonical order
// (nameHashA <= nameHashB byte-by-byte) so that (A,B) and (B,A) share the same key.
// On first call the count is initialised to 1; subsequent calls increment by 1.
// Safe for concurrent calls — uses per-pair locking to prevent TOCTOU races.
func (ps *PebbleStore) IncrementEntityCoOccurrence(ctx context.Context, ws [8]byte, nameA, nameB string) error {
	hashA := keys.EntityNameHash(nameA)
	hashB := keys.EntityNameHash(nameB)

	// Canonicalize pair order: ensure hashA <= hashB byte-by-byte.
	canonA, canonB := nameA, nameB
	for i := 0; i < 8; i++ {
		if hashA[i] < hashB[i] {
			break
		}
		if hashA[i] > hashB[i] {
			// Swap so that the smaller hash comes first.
			hashA, hashB = hashB, hashA
			canonA, canonB = nameB, nameA
			break
		}
	}

	// Acquire per-pair mutex to prevent concurrent TOCTOU races.
	mu := ps.getCoOccurrenceLock(hashA, hashB)
	mu.Lock()
	defer mu.Unlock()

	key := keys.CoOccurrenceKey(ws, hashA, hashB)

	// Read-before-write: load existing count.
	existing, err := Get(ps.db, key)
	if err != nil {
		return fmt.Errorf("co-occurrence read: %w", err)
	}

	var rec coOccurrenceRecord
	if existing != nil {
		if err := msgpack.Unmarshal(existing, &rec); err != nil {
			return fmt.Errorf("co-occurrence unmarshal: %w", err)
		}
	} else {
		rec.NameA = canonA
		rec.NameB = canonB
	}
	rec.Count++

	val, err := msgpack.Marshal(rec)
	if err != nil {
		return fmt.Errorf("co-occurrence marshal: %w", err)
	}
	return ps.db.Set(key, val, pebble.NoSync)
}

// ScanEntityClusters scans the 0x24 co-occurrence index for a vault and calls fn
// for each pair whose count >= minCount. The pairs are not sorted; callers should
// sort the results themselves if ordering is required.
func (ps *PebbleStore) ScanEntityClusters(ctx context.Context, ws [8]byte, minCount int, fn func(nameA, nameB string, count int) error) error {
	prefix := keys.CoOccurrencePrefix(ws)
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
		return fmt.Errorf("scan entity clusters: iter: %w", err)
	}
	defer iter.Close()

	for valid := iter.First(); valid; valid = iter.Next() {
		val := iter.Value()
		var rec coOccurrenceRecord
		if err := msgpack.Unmarshal(val, &rec); err != nil {
			continue
		}
		if int(rec.Count) < minCount {
			continue
		}
		if err := fn(rec.NameA, rec.NameB, int(rec.Count)); err != nil {
			return err
		}
	}
	return nil
}

// ScanRelationships scans all vault-scoped relationship records at the 0x21 prefix.
// Calls fn for each RelationshipRecord until fn returns a non-nil error or the
// scan is exhausted.
func (ps *PebbleStore) ScanRelationships(ctx context.Context, ws [8]byte, fn func(record RelationshipRecord) error) error {
	prefix := keys.RelationshipPrefix(ws)
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
		return fmt.Errorf("scan relationships: iter: %w", err)
	}
	defer iter.Close()

	for valid := iter.First(); valid; valid = iter.Next() {
		val := iter.Value()
		var rec RelationshipRecord
		if err := msgpack.Unmarshal(val, &rec); err != nil {
			continue
		}
		if err := fn(rec); err != nil {
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

const (
	// Keep these values aligned with plugin.DigestClassified and plugin.DigestSummarized.
	digestClassifiedFlag uint8 = 0x20
	digestSummarizedFlag uint8 = 0x40
)

// UpdateDigest updates the summary, key points, memory type, and type label on an
// existing engram identified by id. The engram's vault prefix is resolved via
// FindVaultPrefix. Both 0x01 (full engram) and 0x02 (meta slice) keys are
// updated atomically, and the L1/meta caches are invalidated.
func (ps *PebbleStore) UpdateDigest(ctx context.Context, id ULID, summary string, keyPoints []string, memoryType string, typeLabel string) error {
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
	if typeLabel != "" {
		eng.TypeLabel = typeLabel
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

	flags, flagsErr := ps.getDigestFlagsRaw([16]byte(id))
	if flagsErr != nil {
		if !errors.Is(flagsErr, pebble.ErrNotFound) {
			return fmt.Errorf("UpdateDigest: read digest flags: %w", flagsErr)
		}
		flags = 0
	}
	if summary != "" || len(keyPoints) > 0 {
		flags |= digestSummarizedFlag
	}
	if memoryType != "" || typeLabel != "" {
		flags |= digestClassifiedFlag
	}
	batch.Set(keys.DigestFlagsKey([16]byte(id)), []byte{flags}, nil)

	// Invalidate caches before commit — cached structs are stale.
	ps.cache.Delete(ws, id)
	ps.metaCache.Remove([16]byte(id))

	if err := batch.Commit(pebble.NoSync); err != nil {
		return fmt.Errorf("UpdateDigest: commit: %w", err)
	}

	return nil
}

// DecrementEntityMentionCount decrements the MentionCount on the global entity
// record for the given name, floored at 0. No-ops if the record does not exist.
// When the count reaches 0, the 0x23 reverse index is scanned to confirm no live
// engrams still reference the entity (counts can be stale-high after a crash);
// if none are found, the 0x1F entity record is deleted.
// Safe for concurrent calls — uses per-entity locking.
func (ps *PebbleStore) DecrementEntityMentionCount(ctx context.Context, name string) error {
	mu := ps.getEntityLock(name)
	mu.Lock()
	defer mu.Unlock()

	existing, err := ps.GetEntityRecord(ctx, name)
	if err != nil {
		return fmt.Errorf("decrement entity mention count: %w", err)
	}
	if existing == nil {
		return nil
	}

	existing.MentionCount--
	if existing.MentionCount < 0 {
		existing.MentionCount = 0
	}

	nameHash := keys.EntityNameHash(name)

	// When count reaches 0, verify via the 0x23 reverse index — the ground truth
	// for which engrams still reference this entity. Counts can be stale-high
	// after a crash, so we don't trust the count alone. If no reverse links exist,
	// the entity is genuinely orphaned and the 0x1F record is deleted.
	if existing.MentionCount == 0 {
		orphaned := true
		revPrefix := keys.EntityReverseIndexPrefix(nameHash)
		iter, iterErr := PrefixIterator(ps.db, revPrefix)
		if iterErr == nil {
			if iter.First() {
				orphaned = false
			}
			iter.Close()
		}
		if orphaned {
			return ps.db.Delete(keys.EntityKey(nameHash), pebble.NoSync)
		}
	}

	existing.UpdatedAt = time.Now().UnixNano()
	val, err := msgpack.Marshal(existing)
	if err != nil {
		return fmt.Errorf("decrement entity mention count: marshal: %w", err)
	}
	return ps.db.Set(keys.EntityKey(nameHash), val, pebble.NoSync)
}

// DecrementEntityCoOccurrence decrements the co-occurrence count for a pair of
// entity names within a vault. Deletes the 0x24 key when the count reaches 0.
// Canonicalises pair order (hashA <= hashB) to match IncrementEntityCoOccurrence.
// Safe for concurrent calls — uses per-pair locking.
func (ps *PebbleStore) DecrementEntityCoOccurrence(ctx context.Context, ws [8]byte, nameA, nameB string) error {
	hashA := keys.EntityNameHash(nameA)
	hashB := keys.EntityNameHash(nameB)

	// Canonicalize pair order: ensure hashA <= hashB byte-by-byte.
	for i := 0; i < 8; i++ {
		if hashA[i] < hashB[i] {
			break
		}
		if hashA[i] > hashB[i] {
			hashA, hashB = hashB, hashA
			nameA, nameB = nameB, nameA
			break
		}
	}

	mu := ps.getCoOccurrenceLock(hashA, hashB)
	mu.Lock()
	defer mu.Unlock()

	key := keys.CoOccurrenceKey(ws, hashA, hashB)

	existing, err := Get(ps.db, key)
	if err != nil {
		return fmt.Errorf("co-occurrence decrement read: %w", err)
	}
	if existing == nil {
		return nil
	}

	var rec coOccurrenceRecord
	if err := msgpack.Unmarshal(existing, &rec); err != nil {
		return fmt.Errorf("co-occurrence decrement unmarshal: %w", err)
	}

	if rec.Count <= 1 {
		return ps.db.Delete(key, pebble.NoSync)
	}

	rec.Count--
	val, err := msgpack.Marshal(rec)
	if err != nil {
		return fmt.Errorf("co-occurrence decrement marshal: %w", err)
	}
	return ps.db.Set(key, val, pebble.NoSync)
}

// deleteEntityLinks scans the 0x20 forward index and 0x21 relationship index for
// the given engram, adding delete operations to batch for the 0x20 forward keys,
// their corresponding 0x23 reverse keys, and all 0x21 relationship keys.
// Returns the deduplicated entity names whose MentionCount should be decremented
// post-commit. Callers are responsible for calling DecrementEntityMentionCount
// for each name returned.
// Only called from DeleteEngram (hard delete). SoftDelete intentionally preserves
// entity links so that Restore can return the engram with associations intact.
func (ps *PebbleStore) deleteEntityLinks(ws [8]byte, engramID [16]byte, batch *pebble.Batch) ([]string, error) {
	seen := make(map[string]struct{})

	// Scan 0x20 forward links: engram → entity.
	iter, err := PrefixIterator(ps.db, keys.EntityEngramLinkPrefix(ws, engramID))
	if err != nil {
		return nil, fmt.Errorf("delete entity links: fwd iter: %w", err)
	}
	for valid := iter.First(); valid; valid = iter.Next() {
		k := iter.Key()
		entityName := string(iter.Value())
		// Delete 0x20 forward key.
		keyCopy := make([]byte, len(k))
		copy(keyCopy, k)
		batch.Delete(keyCopy, nil)
		// Delete 0x23 reverse key.
		if entityName != "" {
			nameHash := keys.EntityNameHash(entityName)
			batch.Delete(keys.EntityReverseIndexKey(nameHash, ws, engramID), nil)
			seen[entityName] = struct{}{}
		}
	}
	if err := iter.Close(); err != nil {
		return nil, fmt.Errorf("delete entity links: fwd iter close: %w", err)
	}

	// Scan 0x21 relationship keys sourced from this engram.
	relIter, err := PrefixIterator(ps.db, keys.RelationshipEngramPrefix(ws, engramID))
	if err != nil {
		// Return what we have — 0x20/0x23 are already queued in the batch.
		entityNames := make([]string, 0, len(seen))
		for name := range seen {
			entityNames = append(entityNames, name)
		}
		return entityNames, fmt.Errorf("delete entity links: rel iter: %w", err)
	}
	for valid := relIter.First(); valid; valid = relIter.Next() {
		k := relIter.Key()
		keyCopy := make([]byte, len(k))
		copy(keyCopy, k)
		batch.Delete(keyCopy, nil)
	}
	if err := relIter.Close(); err != nil {
		return nil, fmt.Errorf("delete entity links: rel iter close: %w", err)
	}

	entityNames := make([]string, 0, len(seen))
	for name := range seen {
		entityNames = append(entityNames, name)
	}
	return entityNames, nil
}

// ScanVaultEntityNames scans the 0x20 forward index for all distinct entity names
// in a vault. The same entity name may appear multiple times (once per engram-link);
// fn is called exactly once per unique name.
func (ps *PebbleStore) ScanVaultEntityNames(ctx context.Context, ws [8]byte, fn func(name string) error) error {
	prefix := make([]byte, 1+8)
	prefix[0] = 0x20
	copy(prefix[1:9], ws[:])

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
		return fmt.Errorf("scan vault entity names: iter: %w", err)
	}
	defer iter.Close()

	seen := make(map[string]struct{})
	for valid := iter.First(); valid; valid = iter.Next() {
		val := iter.Value()
		name := string(val)
		if name == "" {
			continue
		}
		if _, already := seen[name]; already {
			continue
		}
		seen[name] = struct{}{}
		if err := fn(name); err != nil {
			return err
		}
	}
	return nil
}

// relTypeBytes maps relationship type strings to 1-byte discriminants for the 0x21 key.
var relTypeBytes = map[string]uint8{
	"manages": 0x01, "uses": 0x02, "depends_on": 0x03,
	"implements": 0x04, "created_by": 0x05, "part_of": 0x06,
	"causes": 0x07, "contradicts": 0x08, "supports": 0x09,
	"co_occurs_with": 0x0A, "caches_with": 0x0B,
}

func relTypeByteFromString(relType string) uint8 {
	if b, ok := relTypeBytes[relType]; ok {
		return b
	}
	return 0xFF
}
