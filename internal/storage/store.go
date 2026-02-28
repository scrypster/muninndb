package storage

import (
	"context"
	"time"
)

// AssocWeightUpdate represents a single association weight update for batching.
type AssocWeightUpdate struct {
	WS     [8]byte
	Src    ULID
	Dst    ULID
	Weight float32
}

// EngineStore is the storage interface for the MuninnDB engine.
// Implemented by the Pebble-backed store. All operations are vault-scoped
// via the vault prefix in the key construction.
type EngineStore interface {
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
	UpdateAssocWeight(ctx context.Context, wsPrefix [8]byte, a, b ULID, weight float32) error

	// DecayAssocWeights multiplies all association weights for wsPrefix by decayFactor,
	// deleting entries that fall below minWeight. Returns count deleted.
	DecayAssocWeights(ctx context.Context, wsPrefix [8]byte, decayFactor float64, minWeight float32) (int, error)

	// UpdateAssocWeightBatch atomically updates multiple association weights in a single batch.
	UpdateAssocWeightBatch(ctx context.Context, updates []AssocWeightUpdate) error

	// GetConfidence reads the confidence value from 0x02 metadata for an engram.
	GetConfidence(ctx context.Context, wsPrefix [8]byte, id ULID) (float32, error)

	// UpdateConfidence updates the confidence in 0x02 metadata (and 0x01 full engram).
	UpdateConfidence(ctx context.Context, wsPrefix [8]byte, id ULID, confidence float32) error

	// GetConceptAssociations returns up to maxN neighbor IDs for spreading activation.
	GetConceptAssociations(ctx context.Context, wsPrefix [8]byte, id ULID, maxN int) ([]ULID, error)

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

	// Close flushes all pending writes and closes the Pebble database.
	Close() error
}
