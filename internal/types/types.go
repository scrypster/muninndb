// Package types defines the shared data types used across all MuninnDB packages.
// It has no internal dependencies, breaking circular import chains.
package types

import (
	"bytes"
	"crypto/rand"
	"fmt"
	"time"

	"github.com/oklog/ulid/v2"
)

// ULID is a 16-byte Universally Unique Lexicographically Sortable Identifier.
// Stored as raw bytes internally, converted to 26-char Crockford base32 for APIs.
type ULID [16]byte

// NewULID generates a new ULID using the current timestamp and crypto/rand entropy.
func NewULID() ULID {
	entropy := ulid.Monotonic(rand.Reader, 0)
	id := ulid.MustNew(ulid.Timestamp(time.Now()), entropy)
	var u ULID
	copy(u[:], id[:])
	return u
}

// String returns the 26-character Crockford base32 string representation.
func (u ULID) String() string {
	var id ulid.ULID
	copy(id[:], u[:])
	return id.String()
}

// ParseULID parses a 26-character string into a ULID.
func ParseULID(s string) (ULID, error) {
	id, err := ulid.Parse(s)
	if err != nil {
		return ULID{}, fmt.Errorf("parse ulid: %w", err)
	}
	var u ULID
	copy(u[:], id[:])
	return u, nil
}

// CompareULIDs returns -1, 0, or 1 for lexicographic comparison.
func CompareULIDs(a, b ULID) int {
	return bytes.Compare(a[:], b[:])
}

// Engram is the full in-memory representation of a stored memory.
type Engram struct {
	ID             ULID
	CreatedAt      time.Time
	UpdatedAt      time.Time
	LastAccess     time.Time
	Confidence     float32 // 0.0-1.0
	Relevance      float32 // current Ebbinghaus score (computed at read time in ACTIVATE)
	Stability      float32 // decay resistance (days)
	AccessCount    uint32
	State          LifecycleState
	EmbedDim       EmbedDimension
	Concept        string // required, max 512 bytes
	CreatedBy      string // max 64 bytes
	Content        string // required, max 16KB
	Tags           []string
	Associations   []Association
	Embedding      []float32 // nil if no embedding
	Summary        string    // extractive first 2 sentences
	KeyPoints      []string  // top 5 sentences by IDF rarity
	MemoryType     MemoryType
	Classification uint16 // concept-cluster ID
}

// EngramMeta is the 100-byte fixed metadata section.
// Used by decay worker, activation scoring, and any path that does not need
// the full content/embedding.
type EngramMeta struct {
	ID          ULID
	CreatedAt   time.Time
	UpdatedAt   time.Time
	LastAccess  time.Time
	Confidence  float32
	Relevance   float32
	Stability   float32
	AccessCount uint32
	State       LifecycleState
	AssocCount  uint16
	EmbedDim    EmbedDimension
}

// Association represents a directed, weighted link between two engrams.
// Fixed-size: 40 bytes on disk.
type Association struct {
	TargetID      ULID
	RelType       RelType
	Weight        float32 // 0.0-1.0, Hebbian-adjustable
	Confidence    float32 // 0.0-1.0
	CreatedAt     time.Time
	LastActivated int32 // Unix seconds (not nanoseconds; int32 is sufficient)
}

// LifecycleState is the engram state machine (uint8 on disk).
type LifecycleState uint8

const (
	StatePlanning    LifecycleState = 0x00
	StateActive      LifecycleState = 0x01 // default on write
	StatePaused      LifecycleState = 0x02
	StateBlocked     LifecycleState = 0x03
	StateCompleted   LifecycleState = 0x04
	StateCancelled   LifecycleState = 0x05
	StateArchived    LifecycleState = 0x06
	StateSoftDeleted LifecycleState = 0x7F
)

// RelType is the association relationship type (uint16 on disk).
type RelType uint16

const (
	RelSupports         RelType = 0x0001
	RelContradicts      RelType = 0x0002
	RelDependsOn        RelType = 0x0003
	RelSupersedes       RelType = 0x0004
	RelRelatesTo        RelType = 0x0005
	RelIsPartOf         RelType = 0x0006
	RelCauses           RelType = 0x0007
	RelPrecededBy       RelType = 0x0008
	RelFollowedBy       RelType = 0x0009
	RelCreatedByPerson  RelType = 0x000A
	RelBelongsToProject RelType = 0x000B
	RelReferences       RelType = 0x000C
	RelImplements       RelType = 0x000D
	RelBlocks           RelType = 0x000E
	RelResolves         RelType = 0x000F
	RelUserDefined      RelType = 0x8000
)

// EmbedDimension encodes embedding dimensionality (uint8 on disk).
type EmbedDimension uint8

const (
	EmbedNone  EmbedDimension = 0
	Embed384   EmbedDimension = 1
	Embed768   EmbedDimension = 2
	Embed1536  EmbedDimension = 3
	Embed3072  EmbedDimension = 4
	EmbedOther EmbedDimension = 255 // embedded, dimension not in known enum
)

// MemoryType is the rule-based classification.
type MemoryType uint8

const (
	TypeFact        MemoryType = 0  // factual information
	TypeDecision    MemoryType = 1  // choices made with rationale
	TypeObservation MemoryType = 2  // something noticed, insight
	TypePreference  MemoryType = 3  // opinions, personal choices
	TypeIssue       MemoryType = 4  // bugs, problems, defects (renamed from TypeBugfix)
	TypeTask        MemoryType = 5  // action items, to-dos
	TypeProcedure   MemoryType = 6  // how-to, workflows, processes
	TypeEvent       MemoryType = 7  // something that happened, temporal
	TypeGoal        MemoryType = 8  // objectives, targets, intentions
	TypeConstraint  MemoryType = 9  // rules, limitations, requirements
	TypeIdentity    MemoryType = 10 // about a person, role, entity
	TypeReference   MemoryType = 11 // documentation, specifications
)

// TypeBugfix is a backward-compatible alias for TypeIssue.
const TypeBugfix = TypeIssue

// ERF flags byte (offset 5 in the record).
const (
	FlagHasEmbedding      uint8 = 1 << 0
	FlagContentCompressed uint8 = 1 << 1
	FlagEmbedQuantized    uint8 = 1 << 2
	FlagDormant           uint8 = 1 << 3
	FlagSoftDeleted       uint8 = 1 << 4
	FlagDirty             uint8 = 1 << 5
)
