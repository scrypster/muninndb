package plugin

import (
	"errors"
	"time"
)

// PluginTier identifies the tier level of a plugin.
type PluginTier int

const (
	TierEmbed  PluginTier = 2
	TierEnrich PluginTier = 3
)

// PluginConfig holds configuration passed to plugins at Init().
type PluginConfig struct {
	ProviderURL string            // "ollama://localhost:11434/nomic-embed-text"
	APIKey      string            // for cloud providers (OpenAI, Anthropic, Voyage)
	Options     map[string]string // provider-specific options (e.g., "batch_size": "32")
	DataDir     string            // plugin-specific data directory under the main data dir

	// User-supplied local ONNX model overrides (local embed provider only,
	// issue #583). Both paths set together; dimension is probed at init.
	LocalModelPath     string // path to a user-supplied .onnx model file
	LocalTokenizerPath string // path to its HuggingFace tokenizer.json
	LocalPooling       string // "cls" (default) or "mean"
	LocalMaxTokens     int    // max sequence length; 0 = provider default
}

// EnrichmentResult is what the enrich plugin returns for one engram.
type EnrichmentResult struct {
	Summary        string              // abstractive summary (replaces extractive)
	KeyPoints      []string            // semantic key points (replaces IDF-based)
	MemoryType     string              // canonical memory type (e.g., "decision")
	TypeLabel      string              // nuanced classification label (e.g., "architectural_decision")
	Classification string              // topic category (e.g., "infrastructure/databases")
	Entities       []ExtractedEntity   // people, projects, tools, organizations
	Relationships  []ExtractedRelation // typed relationships between entities
}

// ExtractedEntity represents a named entity extracted by the enrich plugin.
type ExtractedEntity struct {
	Name       string  // "payment-service", "MJ", "PostgreSQL"
	Type       string  // "person", "organization", "project", "tool", "framework", "language", "database", "service"
	Confidence float32 // 0.0-1.0
}

// ExtractedRelation represents a typed relationship between two entities.
type ExtractedRelation struct {
	FromEntity string  // entity name (must match an ExtractedEntity.Name)
	ToEntity   string  // entity name
	RelType    string  // "manages", "uses", "depends_on", "implements", "created_by"
	Weight     float32 // 0.0-1.0 confidence in this relationship
}

// ErrNothingToEnrich is returned when all pipeline stages are skipped because
// the engram already has inline data (e.g., Summary set by caller during Write).
// This is distinct from a real failure where LLM/network errors caused stages to fail.
var ErrNothingToEnrich = errors.New("enrich: nothing to enrich")

// DigestFlags tracks which processing stages have been applied to an engram.
// Stored standalone under the 0x11 DigestFlagsKey keyspace (NOT in the ERF
// record itself — see PebbleStore.SetDigestFlag / getDigestFlagsRaw).
//
// The type is untyped-constant by design (no explicit uint8/uint16), so it
// converts to whatever the flags value is stored/compared as without an
// explicit cast at every call site — this is what let the bit-split below
// (#605) widen the underlying storage from 1 byte to 2 without touching the
// ~30 call sites that only ever read/write named constants.
const (
	DigestCore   = 0x01 // extractive, rule-based (always set on write)
	DigestEmbed  = 0x02 // embedding vector generated and stored
	DigestEnrich = 0x04 // LLM-enriched: full pipeline complete

	// Per-stage completion flags (set individually by UpdateDigest).
	DigestEntities      = 0x08 // entity extraction complete
	DigestRelationships = 0x10 // relationship extraction complete
	DigestClassified    = 0x20 // classification complete
	DigestSummarized    = 0x40 // summarization complete

	// DigestEmbedFailed is set when an embed batch permanently fails for an
	// engram. Engrams with this flag are skipped by the embed retroactive
	// processor so they are not retried indefinitely.
	DigestEmbedFailed = 0x80

	// DigestEnrichFailed is set when LLM enrichment permanently fails for an
	// engram (e.g. the LLM returns unparseable output). Engrams with this flag
	// are skipped by the enrich retroactive processor to prevent infinite
	// retry loops that trip the circuit breaker and block enrichment for all
	// other memories.
	//
	// Distinct bit from DigestEmbedFailed (#605): before this fix the digest
	// byte was a fully-allocated uint8 and DigestEnrichFailed literally
	// aliased DigestEmbedFailed (0x80), so a transient embed failure also
	// permanently skipped that engram from the *enrich* pass, and vice versa.
	// The flags value now widens to a second byte (0x100) the first time it is
	// written with this bit; see getDigestFlagsRaw's tolerant decode for what
	// happens to engrams that already persisted the collided 0x80 value.
	DigestEnrichFailed = 0x100
)

// PluginStatus represents the runtime state of a registered plugin.
type PluginStatus struct {
	Name      string     `json:"name"`
	Tier      PluginTier `json:"tier"`
	Healthy   bool       `json:"healthy"`
	LastCheck time.Time  `json:"last_check"`
	Error     string     `json:"error,omitempty"` // last health check error
}

// RetroactiveStats is the progress of a retroactive processor.
type RetroactiveStats struct {
	PluginName string    `json:"plugin_name"`
	Status     string    `json:"status"` // "running", "complete", "paused", "failed"
	Processed  int64     `json:"processed"`
	Total      int64     `json:"total"`
	RatePerSec float64   `json:"rate_per_sec"`
	ETASeconds int64     `json:"eta_seconds"`
	StartedAt  time.Time `json:"started_at"`
	Errors     int64     `json:"errors"` // count of skipped engrams
}

// HardwareAwarePlugin is implemented by providers that can report
// whether they are running with hardware acceleration (e.g., GPU).
type HardwareAwarePlugin interface {
	HardwareAccelerated() bool
}
