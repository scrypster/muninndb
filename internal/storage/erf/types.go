package erf

import "time"

// Engram is the erf-package local representation of a stored memory.
// Uses raw primitive types to avoid circular imports with the storage package.
type Engram struct {
	ID             [16]byte // ULID raw bytes
	CreatedAt      time.Time
	UpdatedAt      time.Time
	LastAccess     time.Time
	Confidence     float32
	Relevance      float32
	Stability      float32
	AccessCount    uint32
	State          uint8  // LifecycleState
	EmbedDim       uint8  // EmbedDimension
	Concept        string // max 512 bytes
	CreatedBy      string // max 64 bytes
	Content        string // max 16KB
	Tags           []string
	Associations   []Association
	Embedding      []float32
	Summary        string
	KeyPoints      []string
	MemoryType     uint8
	TypeLabel      string // free-form label, e.g. "architectural_decision"
	Classification uint16
	Trust          uint8 // TrustLevel; 0x00=unset(inferred), 0x01=verified, 0x02=inferred, 0x03=external, 0x04=untrusted

	// Valid-time (application-time) axis. Half-open [ValidFrom, ValidUntil).
	// On disk both live in the formerly-reserved metadata area with a zero
	// default: raw 0 at OffsetValidFrom decodes to CreatedAt ("valid from
	// creation"), raw 0 at OffsetValidUntil decodes to the zero time ("open /
	// still current"). Legacy records therefore decode as "valid from creation,
	// still true" with no version bump or rewrite.
	ValidFrom  time.Time
	ValidUntil time.Time
	Importance float32 // 0 = unset (legacy default); OffsetImportance
}

// EngramMeta is the erf-package local representation of the 100-byte fixed metadata section.
type EngramMeta struct {
	ID          [16]byte // ULID raw bytes
	CreatedAt   time.Time
	UpdatedAt   time.Time
	LastAccess  time.Time
	Confidence  float32
	Relevance   float32
	Stability   float32
	AccessCount uint32
	State       uint8 // LifecycleState
	AssocCount  uint16
	EmbedDim    uint8 // EmbedDimension
	MemoryType  uint8
	ValidFrom   time.Time // decode default: CreatedAt when raw bytes are zero
	ValidUntil  time.Time // zero = open / "current"
	Importance  float32   // 0 = unset
}
