package storage

import (
	"context"
	"time"
)

// AssocWeightUpdate represents a single association weight update for batching.
type AssocWeightUpdate struct {
	WS         [8]byte
	Src        ULID
	Dst        ULID
	Weight     float32
	CountDelta uint32 // Hebbian co-activation increment to add to CoActivationCount
}

// OrdinalEntry is a (childID, ordinal) pair returned by ListChildOrdinals.
type OrdinalEntry struct {
	ChildID ULID
	Ordinal int32
}

// StoreBatch is a write-only handle for atomic multi-write operations.
// Callers must call Commit or Discard exactly once.
type StoreBatch interface {
	// WriteEngram queues an engram write into the batch.
	WriteEngram(ctx context.Context, wsPrefix [8]byte, eng *Engram) error
	// WriteAssociation queues association forward (0x03), reverse (0x04) keys into the batch.
	WriteAssociation(ctx context.Context, wsPrefix [8]byte, src, dst ULID, assoc *Association) error
	// WriteOrdinal queues the ordinal key for (parentID, childID) into the batch.
	WriteOrdinal(ctx context.Context, wsPrefix [8]byte, parentID, childID ULID, ordinal int32) error
	// UpdateEngramState queues a state update for an existing engram into the batch.
	// Reads the current engram from the underlying store, sets its state, and queues
	// updated 0x01 and 0x02 key writes.
	UpdateEngramState(ctx context.Context, ws [8]byte, id ULID, newState LifecycleState) error
	// Commit atomically commits all queued writes.
	Commit() error
	// Discard releases the batch without writing anything.
	// Safe to call after Commit (idempotent).
	Discard()
}

// EngineStore is the storage interface for the MuninnDB engine.
// Implemented by the Pebble-backed store. All operations are vault-scoped
// via the vault prefix in the key construction.
type EngineStore interface {
	// NewBatch returns a StoreBatch for atomic multi-engram writes.
	// The caller must call Commit or Discard exactly once on the returned batch.
	NewBatch() StoreBatch
	// WriteEngram atomically writes the full engram record (0x01 key) and
	// the metadata-only copy (0x02 key) in a single Pebble batch.
	// Also writes association forward/reverse keys (0x03/0x04) and secondary
	// index entries (0x0B/0x0C/0x0D) in the same batch.
	// Returns the assigned ULID.
	WriteEngram(ctx context.Context, wsPrefix [8]byte, eng *Engram) (ULID, error)

	// GetEngram reads a full engram record by ID from the 0x01 key prefix.
	GetEngram(ctx context.Context, wsPrefix [8]byte, id ULID) (*Engram, error)

	// GetEngrams batch-reads full engram records.
	GetEngrams(ctx context.Context, wsPrefix [8]byte, ids []ULID) ([]*Engram, error)

	// GetMetadata reads only the 100-byte fixed metadata from the 0x02 key prefix.
	GetMetadata(ctx context.Context, wsPrefix [8]byte, ids []ULID) ([]*EngramMeta, error)

	// UpdateMetadata writes only the metadata fields that changed (state, confidence,
	// relevance_bucket, access count, timestamps). Updates both 0x01 and 0x02 keys.
	UpdateMetadata(ctx context.Context, wsPrefix [8]byte, id ULID, meta *EngramMeta) error

	// UpdateRelevance updates the relevance and stability of an engram.
	// Moves the relevance bucket key (0x10) from the old bucket to the new bucket,
	// and updates the metadata (0x01 and 0x02 keys) with the new values.
	UpdateRelevance(ctx context.Context, wsPrefix [8]byte, id ULID, relevance, stability float32) error

	// DeleteEngram performs a hard delete: removes 0x01, 0x02, and all association keys.
	DeleteEngram(ctx context.Context, wsPrefix [8]byte, id ULID) error

	// SoftDelete sets state to StateSoftDeleted and FlagSoftDeleted in the record.
	SoftDelete(ctx context.Context, wsPrefix [8]byte, id ULID) error

	// WriteAssociation writes forward (0x03) and reverse (0x04) association keys.
	WriteAssociation(ctx context.Context, wsPrefix [8]byte, src, dst ULID, assoc *Association) error

	// GetAssociations returns forward associations for a set of source IDs,
	// weight-sorted descending, up to maxPerNode per source.
	GetAssociations(ctx context.Context, wsPrefix [8]byte, ids []ULID, maxPerNode int) (map[ULID][]Association, error)

	// RecentActive returns up to topK engram IDs with the highest relevance
	// in the vault. Uses the 0x10 relevance bucket index for O(k) scanning.
	RecentActive(ctx context.Context, wsPrefix [8]byte, topK int) ([]ULID, error)

	// GetAssocWeight reads the weight of a forward association (0x03 key) for pair (a,b).
	// Returns 0.0 if no association exists.
	GetAssocWeight(ctx context.Context, wsPrefix [8]byte, a, b ULID) (float32, error)

	// UpdateAssocWeight writes/updates the 0x03 and 0x04 association keys for pair (a,b).
	// countDelta is added to the existing CoActivationCount (saturating at MaxUint32).
	UpdateAssocWeight(ctx context.Context, wsPrefix [8]byte, a, b ULID, weight float32, countDelta uint32) error

	// DecayAssocWeights multiplies all association weights for wsPrefix by decayFactor,
	// deleting entries that fall below minWeight. Returns count deleted.
	// archiveThreshold > 0 enables moving strong floor-hit edges to the 0x25 archive namespace.
	DecayAssocWeights(ctx context.Context, wsPrefix [8]byte, decayFactor float64, minWeight float32, archiveThreshold float64) (int, error)

	// UpdateAssocWeightBatch atomically updates multiple association weights in a single batch.
	UpdateAssocWeightBatch(ctx context.Context, updates []AssocWeightUpdate) error

	// GetConfidence reads the confidence value from 0x02 metadata for an engram.
	GetConfidence(ctx context.Context, wsPrefix [8]byte, id ULID) (float32, error)

	// UpdateConfidence updates the confidence in 0x02 metadata (and 0x01 full engram).
	UpdateConfidence(ctx context.Context, wsPrefix [8]byte, id ULID, confidence float32) error

	// GetConceptAssociations returns up to maxN neighbor IDs for spreading activation.
	GetConceptAssociations(ctx context.Context, wsPrefix [8]byte, id ULID, maxN int) ([]ULID, error)

	// GetChildrenByParent returns IDs of all engrams that have an is_part_of
	// association pointing to parentID. Scans the 0x04 reverse index.
	GetChildrenByParent(ctx context.Context, wsPrefix [8]byte, parentID ULID) ([]ULID, error)

	// FlagContradiction writes the 0x0A contradiction key for pair (a,b).
	FlagContradiction(ctx context.Context, wsPrefix [8]byte, a, b ULID) error

	// GetContradictions returns all contradiction pairs in the vault by scanning the 0x0A prefix.
	GetContradictions(ctx context.Context, wsPrefix [8]byte) ([][2]ULID, error)

	// ResolveContradiction deletes the contradiction marker(s) for the pair (a,b).
	// Both directions are removed (the pair is stored bidirectionally).
	ResolveContradiction(ctx context.Context, wsPrefix [8]byte, a, b ULID) error

	// ListByState returns up to limit engram IDs whose lifecycle state matches,
	// using the 0x0B state secondary index.
	ListByState(ctx context.Context, wsPrefix [8]byte, state LifecycleState, limit int) ([]ULID, error)

	// VaultPrefix computes the 8-byte SipHash prefix for a vault name.
	VaultPrefix(vault string) [8]byte

	// DiskSize returns the total on-disk size of all database files in bytes.
	DiskSize() int64

	// WriteVaultName persists the human-readable vault name so ListVaultNames
	// can return it. Safe to call on every write (idempotent, cheap).
	WriteVaultName(wsPrefix [8]byte, name string) error

	// ResolveVaultPrefix returns the actual workspace prefix for a vault name,
	// using the stored forward index if available, otherwise computing SipHash.
	ResolveVaultPrefix(name string) [8]byte

	// ListVaultNames returns all vault names that have been persisted.
	ListVaultNames() ([]string, error)

	// EngramsByCreatedSince returns engrams created at or after since, ordered
	// by creation time (ascending), with offset/limit for pagination.
	EngramsByCreatedSince(ctx context.Context, wsPrefix [8]byte, since time.Time, offset, limit int) ([]*Engram, error)

	// WriteOrdinal atomically writes the ordinal for childID within parentID.
	// Overwrites any existing value.
	WriteOrdinal(ctx context.Context, wsPrefix [8]byte, parentID, childID ULID, ordinal int32) error

	// ReadOrdinal reads the ordinal for (parentID, childID).
	// Returns found=false if the key does not exist.
	ReadOrdinal(ctx context.Context, wsPrefix [8]byte, parentID, childID ULID) (ordinal int32, found bool, err error)

	// DeleteOrdinal removes the ordinal key for (parentID, childID). No-op if absent.
	DeleteOrdinal(ctx context.Context, wsPrefix [8]byte, parentID, childID ULID) error

	// DeleteEngramOrdinal removes the ordinal key for (parentID, childID).
	// Called by the engram delete hook to clean up tree membership when a child
	// engram is deleted. No-op if the key does not exist.
	DeleteEngramOrdinal(ctx context.Context, wsPrefix [8]byte, parentID, childID ULID) error

	// ListChildOrdinals returns all (childID, ordinal) pairs for parentID,
	// sorted by ordinal ascending.
	ListChildOrdinals(ctx context.Context, wsPrefix [8]byte, parentID ULID) ([]OrdinalEntry, error)

	// UpsertEntityRecord stores or updates a global entity record.
	UpsertEntityRecord(ctx context.Context, record EntityRecord, source string) error

	// GetEntityRecord reads a global entity record by canonical name. Returns nil, nil if not found.
	GetEntityRecord(ctx context.Context, name string) (*EntityRecord, error)

	// WriteEntityEngramLink writes a vault-scoped engram→entity link.
	WriteEntityEngramLink(ctx context.Context, ws [8]byte, engramID ULID, entityName string) error

	// ScanEntityEngrams scans the 0x23 reverse index for all vault-scoped (ws, engramID)
	// pairs that mention the given entity name. Calls fn for each pair until fn returns
	// a non-nil error or the index is exhausted.
	ScanEntityEngrams(ctx context.Context, entityName string, fn func(ws [8]byte, engramID ULID) error) error

	// ScanEngramEntities scans the 0x20 forward index for all entities mentioned
	// by the given engram in vault ws. Calls fn for each entity name.
	ScanEngramEntities(ctx context.Context, ws [8]byte, engramID ULID, fn func(entityName string) error) error

	// ScanVaultEntityNames scans the 0x20 forward index for all distinct entity names
	// in a vault. fn is called exactly once per unique name.
	ScanVaultEntityNames(ctx context.Context, ws [8]byte, fn func(name string) error) error

	// UpsertRelationshipRecord writes a vault-scoped relationship record.
	UpsertRelationshipRecord(ctx context.Context, ws [8]byte, engramID ULID, record RelationshipRecord) error

	// ScanRelationships scans all vault-scoped relationship records at the 0x21 prefix.
	// Calls fn for each RelationshipRecord until fn returns a non-nil error or the scan is exhausted.
	ScanRelationships(ctx context.Context, ws [8]byte, fn func(record RelationshipRecord) error) error

	// IncrementEntityCoOccurrence increments the co-occurrence count for two entity names
	// within a vault. Uses the 0x24 index. Pair is stored in canonical (hashA <= hashB) order.
	IncrementEntityCoOccurrence(ctx context.Context, ws [8]byte, nameA, nameB string) error

	// ScanEntityClusters scans the 0x24 co-occurrence index for the given vault and calls
	// fn for each pair with count >= minCount.
	ScanEntityClusters(ctx context.Context, ws [8]byte, minCount int, fn func(nameA, nameB string, count int) error) error

	// WriteLastAccessEntry writes/updates the 0x22 LastAccess index entry.
	// prevMillis is the old LastAccess unix-millis (0 if first write).
	// newMillis is the new LastAccess unix-millis.
	WriteLastAccessEntry(ctx context.Context, ws [8]byte, id ULID, prevMillis, newMillis int64) error

	// ScanLastAccessDesc scans the 0x22 index in descending LastAccess order
	// (ascending byte scan due to inverted millis encoding).
	ScanLastAccessDesc(ctx context.Context, ws [8]byte, fn func(id ULID, lastAccessMillis int64) error) error

	// DeleteLastAccessEntry removes the 0x22 index entry for a deleted engram.
	DeleteLastAccessEntry(ctx context.Context, ws [8]byte, id ULID, lastAccessMillis int64) error

	// CheckIdempotency looks up an op_id receipt. Returns nil, nil if not found.
	CheckIdempotency(ctx context.Context, opID string) (*IdempotencyReceipt, error)

	// WriteIdempotency stores an idempotency receipt (op_id → engramID).
	WriteIdempotency(ctx context.Context, opID, engramID string) error

	// Close flushes all pending writes and closes the Pebble database.
	Close() error
}
