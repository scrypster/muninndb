// Package prefix is the single source of truth for Pebble key-prefix byte
// allocations across storage, auth, and capability. Every key constructor in
// internal/storage/keys and internal/auth MUST reference these constants;
// never inline a raw byte. [RT-FIX RT3] The length invariants below are load-
// bearing for the v3 migration discriminator. NOTE: replication's schemaVersionKey
// (0x19,0x03,... in internal/replication/schema_version.go) bypasses this registry
// and overlaps storage's 0x19 Idempotency — a pre-existing instance of the same
// bug class, flagged as a follow-up (NOT fixed here).
package prefix

// Source-of-truth prefix bytes. Storage unchanged; auth RELOCATED 0x11–0x14 → 0x42–0x45.
const (
	// Storage (0x01–0x2A)
	Engram             byte = 0x01
	Meta               byte = 0x02
	AssocFwd           byte = 0x03
	AssocRev           byte = 0x04
	FTSPosting         byte = 0x05
	Trigram            byte = 0x06
	HNSWNode           byte = 0x07
	FTSStats           byte = 0x08
	TermStats          byte = 0x09
	Contradiction      byte = 0x0A
	StateIndex         byte = 0x0B
	TagIndex           byte = 0x0C
	CreatorIndex       byte = 0x0D
	VaultMeta          byte = 0x0E
	VaultNameIndex     byte = 0x0F
	RelevanceBucket    byte = 0x10
	DigestFlags        byte = 0x11
	Coherence          byte = 0x12
	VaultWeights       byte = 0x13
	AssocWeightIndex   byte = 0x14
	VaultCount         byte = 0x15
	Provenance         byte = 0x16
	BucketMigration    byte = 0x17
	Embedding          byte = 0x18
	Idempotency        byte = 0x19
	Episode            byte = 0x1A
	FTSVersion         byte = 0x1B
	Transition         byte = 0x1C
	EmbedModel         byte = 0x1D
	Ordinal            byte = 0x1E
	Entity             byte = 0x1F
	EntityEngramLink   byte = 0x20
	Relationship       byte = 0x21
	LastAccess         byte = 0x22
	EntityReverseIndex byte = 0x23
	CoOccurrence       byte = 0x24
	ArchiveAssoc       byte = 0x25
	RelEntityIndex     byte = 0x26
	DreamState         byte = 0x27
	ContentHash        byte = 0x28
	Lease              byte = 0x2A
	// Capability (0x40/0x41 — clean since #612)
	Capability         byte = 0x40
	CapabilityVaultIdx byte = 0x41
	// Auth (RELOCATED 0x42–0x45 by #611)
	AdminUser      byte = 0x42
	APIKey         byte = 0x43
	APIKeyVaultIdx byte = 0x44
	VaultConfig    byte = 0x45

	// [RT-FIX RT3] length invariants the v3 discriminator depends on (no magic numbers).
	MinAPIKeyLen     = 17 // APIKey key = 1 + 16-byte hash
	MinAPIKeyVIdxLen = 10 // APIKeyVaultIdx key = 1 + vault + 0x00 + 8 (>= 10)
	CoherenceKeyLen  = 9  // Coherence/VaultWeights key = 1 + 8-byte wsPrefix
)

type Entry struct {
	Byte  byte
	Owner string // "storage" | "auth" | "capability"
	Name  string
	Cat   string // category for the per-list partition guards (Task 5)
}

func All() []Entry { return registry }

// Category returns the partition category for a prefix (Task 5's per-list guards).
func Category(b byte) string {
	for _, e := range registry {
		if e.Byte == b {
			return e.Cat
		}
	}
	return ""
}

var registry = []Entry{
	{Engram, "storage", "Engram", "vault-scoped-data"},
	{Meta, "storage", "Meta", "vault-scoped-data"},
	{AssocFwd, "storage", "AssocFwd", "vault-scoped-data"},
	{AssocRev, "storage", "AssocRev", "vault-scoped-data"},
	{FTSPosting, "storage", "FTSPosting", "vault-scoped-data"},
	{Trigram, "storage", "Trigram", "vault-scoped-data"},
	{HNSWNode, "storage", "HNSWNode", "vault-scoped-data"},
	{FTSStats, "storage", "FTSStats", "vault-scoped-data"},
	{TermStats, "storage", "TermStats", "vault-scoped-data"},
	{Contradiction, "storage", "Contradiction", "vault-scoped-data"},
	{StateIndex, "storage", "StateIndex", "vault-scoped-data"},
	{TagIndex, "storage", "TagIndex", "vault-scoped-data"},
	{CreatorIndex, "storage", "CreatorIndex", "vault-scoped-data"},
	{VaultMeta, "storage", "VaultMeta", "name-key"},
	{VaultNameIndex, "storage", "VaultNameIndex", "name-key"},
	{RelevanceBucket, "storage", "RelevanceBucket", "vault-scoped-data"},
	{DigestFlags, "storage", "DigestFlags", "global-by-ulid"},
	{Coherence, "storage", "Coherence", "vault-scoped-data"},
	{VaultWeights, "storage", "VaultWeights", "vault-scoped-data"},
	{AssocWeightIndex, "storage", "AssocWeightIndex", "vault-scoped-data"},
	{VaultCount, "storage", "VaultCount", "vault-scoped-data"},
	{Provenance, "storage", "Provenance", "vault-scoped-data"},
	{BucketMigration, "storage", "BucketMigration", "vault-scoped-data"},
	{Embedding, "storage", "Embedding", "vault-scoped-data"},
	{Idempotency, "storage", "Idempotency", "global-by-ulid"},
	{Episode, "storage", "Episode", "vault-scoped-data"},
	{FTSVersion, "storage", "FTSVersion", "vault-scoped-data"},
	{Transition, "storage", "Transition", "vault-scoped-data"},
	{EmbedModel, "storage", "EmbedModel", "vault-scoped-data"},
	{Ordinal, "storage", "Ordinal", "vault-scoped-data"},
	{Entity, "storage", "Entity", "global-by-ulid"},
	{EntityEngramLink, "storage", "EntityEngramLink", "vault-scoped-data"},
	{Relationship, "storage", "Relationship", "vault-scoped-data"},
	{LastAccess, "storage", "LastAccess", "vault-scoped-data"},
	{EntityReverseIndex, "storage", "EntityReverseIndex", "global-by-ulid"},
	{CoOccurrence, "storage", "CoOccurrence", "vault-scoped-data"},
	{ArchiveAssoc, "storage", "ArchiveAssoc", "vault-scoped-data"},
	{RelEntityIndex, "storage", "RelEntityIndex", "vault-scoped-data"},
	{DreamState, "storage", "DreamState", "vault-scoped-data"},
	{ContentHash, "storage", "ContentHash", "vault-scoped-data"},
	{Lease, "storage", "Lease", "lease"},
	{Capability, "capability", "Capability", "capability"},
	{CapabilityVaultIdx, "capability", "CapabilityVaultIdx", "capability"},
	{AdminUser, "auth", "AdminUser", "auth"},
	{APIKey, "auth", "APIKey", "auth"},
	{APIKeyVaultIdx, "auth", "APIKeyVaultIdx", "auth"},
	{VaultConfig, "auth", "VaultConfig", "auth"},
}
