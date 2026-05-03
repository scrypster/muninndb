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
	Trust          uint8  // TrustLevel; 0x00=unset(inferred), 0x01=verified, 0x02=inferred, 0x03=external, 0x04=untrusted
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
	State       uint8  // LifecycleState
	AssocCount  uint16
	EmbedDim    uint8 // EmbedDimension
	MemoryType  uint8
}
