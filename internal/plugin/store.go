package plugin

import "context"

// PluginStore is the storage interface the plugin system needs.
// A subset of EngineStore — plugins never see the full store surface.
type PluginStore interface {
	// CountWithoutFlag returns the number of engrams missing the given digest flag.
	// skipFlags causes engrams that have any of those bits set to be excluded
	// from the count (e.g. pass DigestEmbedFailed to exclude permanently-failed engrams).
	// Used by RetroactiveProcessor to calculate total work.
	CountWithoutFlag(ctx context.Context, flag, skipFlags uint16) (int64, error)

	// ScanWithoutFlag returns an iterator over engrams missing the given digest flag.
	// skipFlags causes engrams that have any of those bits set to be skipped
	// (e.g. pass DigestEmbedFailed to skip permanently-failed engrams).
	// Iterates in ULID order (oldest first). Must be resumable: if the server
	// restarts, calling ScanWithoutFlag again yields only unprocessed engrams.
	ScanWithoutFlag(ctx context.Context, flag, skipFlags uint16) EngramIterator

	// SetDigestFlag sets a digest flag bit on an engram's metadata.
	// Atomic: uses Pebble Merge to set the bit without read-modify-write.
	SetDigestFlag(ctx context.Context, id ULID, flag uint16) error

	// GetDigestFlags returns the current digest flags byte for an engram.
	GetDigestFlags(ctx context.Context, id ULID) (uint16, error)

	// UpdateEmbedding stores an embedding vector for an engram.
	// Also updates the EmbedDim field in ERF metadata.
	UpdateEmbedding(ctx context.Context, id ULID, vec []float32) error

	// UpdateDigest updates digest fields (summary, key_points, memory_type,
	// type_label/topic classification) on an existing engram. Called by enrich.
	UpdateDigest(ctx context.Context, id ULID, result *EnrichmentResult) error

	// UpsertEntity creates or updates a lightweight entity record in the vault
	// that contains the given engram. Entity records are vault-scoped
	// (0x1F | ws | hash(name)) since #683, so the vault must be resolved from
	// the engram exactly as IncrementEntityCoOccurrence already does.
	UpsertEntity(ctx context.Context, engramID ULID, entity ExtractedEntity) error

	// LinkEngramToEntity creates an association between an engram and an entity.
	LinkEngramToEntity(ctx context.Context, engramID ULID, entityName string) error

	// IncrementEntityCoOccurrence increments the co-occurrence count for a pair
	// of entity names within the vault that contains the given engram.
	// The vault is resolved via the engramID lookup.
	IncrementEntityCoOccurrence(ctx context.Context, engramID ULID, nameA, nameB string) error

	// UpsertRelationship stores a typed relationship in the association graph.
	// Maps to the standard 0x03/0x04 forward/reverse association keys.
	UpsertRelationship(ctx context.Context, engramID ULID, rel ExtractedRelation) error

	// HNSWInsert inserts a vector into the HNSW index.
	HNSWInsert(ctx context.Context, id ULID, vec []float32) error

	// CheckEmbedDim reports whether an embedding of the given dimension would
	// be accepted for the vault that contains the engram (issue #582). Returns
	// nil when the vault has no established dimension yet. Used by the
	// retroactive processor to skip engrams of a mismatched vault BEFORE
	// paying for inference — the mismatch is a vault-level configuration
	// condition, so the engrams stay pending (not failure-flagged) and embed
	// normally once the configuration or the vault is fixed.
	CheckEmbedDim(ctx context.Context, id ULID, dim int) error

	// AutoLinkByEmbedding finds the top-K nearest neighbors by embedding and
	// creates RELATES_TO associations with weight = similarity * 0.8.
	// K = 5 (hardcoded, matching the design doc).
	AutoLinkByEmbedding(ctx context.Context, id ULID, vec []float32) error
}

// EngramIterator is a forward-only iterator over engrams.
type EngramIterator interface {
	// Next advances to the next engram. Returns false when exhausted.
	Next() bool

	// Engram returns the current engram. Only valid after Next() returns true.
	Engram() *Engram

	// CurrentWS returns the vault workspace prefix of the current engram.
	// Only valid after Next() returns true.
	CurrentWS() [8]byte

	// Close releases the underlying Pebble iterator.
	Close() error
}
