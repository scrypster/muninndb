package plugin

import "time"

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
}

// EnrichmentResult is what the enrich plugin returns for one engram.
type EnrichmentResult struct {
	Summary        string              // abstractive summary (replaces extractive)
	KeyPoints      []string            // semantic key points (replaces IDF-based)
	MemoryType     string              // nuanced classification (e.g., "architectural_decision")
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

// DigestFlags tracks which processing stages have been applied to an engram.
// Stored in the ERF metadata Reserved section at offset 68 (first byte of Reserved).
const (
	DigestCore   uint8 = 0x01 // extractive, rule-based (always set on write)
	DigestEmbed  uint8 = 0x02 // embedding vector generated and stored
	DigestEnrich uint8 = 0x04 // LLM-enriched: full pipeline complete

	// Per-stage completion flags (set individually by UpdateDigest).
	DigestEntities      uint8 = 0x08 // entity extraction complete
	DigestRelationships uint8 = 0x10 // relationship extraction complete
	DigestClassified    uint8 = 0x20 // classification complete
	DigestSummarized    uint8 = 0x40 // summarization complete
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
	Status     string    `json:"status"`     // "running", "complete", "paused", "failed"
	Processed  int64     `json:"processed"`
	Total      int64     `json:"total"`
	RatePerSec float64   `json:"rate_per_sec"`
	ETASeconds int64     `json:"eta_seconds"`
	StartedAt  time.Time `json:"started_at"`
	Errors     int64     `json:"errors"` // count of skipped engrams
}
