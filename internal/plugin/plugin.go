package plugin

import (
	"context"
	"github.com/scrypster/muninndb/internal/storage"
)

// Engram is a local alias for use in plugin interfaces.
type Engram = storage.Engram

// ULID is a local alias for use in plugin interfaces.
type ULID = storage.ULID

// Plugin is the interface all plugins implement.
// The engine discovers and manages plugins at startup or via runtime API.
type Plugin interface {
	// Name returns the plugin identifier (e.g., "embed-ollama", "enrich-openai").
	// Must be unique across all registered plugins.
	Name() string

	// Tier returns the plugin tier: TierEmbed (2) or TierEnrich (3).
	Tier() PluginTier

	// Init validates configuration and external connectivity.
	// Called once at startup or when added at runtime.
	// Return an error to prevent registration with a clear diagnostic message.
	Init(ctx context.Context, cfg PluginConfig) error

	// Close releases external connections on graceful shutdown.
	Close() error
}

// EmbedPlugin generates vector embeddings for engrams.
// Exactly one embed plugin may be active at a time.
type EmbedPlugin interface {
	Plugin

	// Embed converts one or more texts to a flat vector.
	// For single text: returns a single embedding (len = Dimension()).
	// For batch: returns concatenated embeddings (len = len(texts) * Dimension()).
	// Called on the write path (new engrams) and the ACTIVATE query path (context embedding).
	Embed(ctx context.Context, texts []string) ([]float32, error)

	// Dimension returns the embedding vector dimension (384, 768, 1024, 1536).
	// Detected at Init time by sending a probe text to the provider.
	Dimension() int

	// MaxBatchSize returns the maximum number of texts per Embed call.
	// The retroactive processor uses this to size micro-batches, so embeddings
	// are generated at the provider's optimal batch size rather than a
	// hardcoded constant.
	MaxBatchSize() int
}

// EnrichPlugin generates summaries, entities, and relationships via LLM.
// Exactly one enrich plugin may be active at a time.
type EnrichPlugin interface {
	Plugin

	// Enrich processes one engram and returns enrichment data.
	// Called asynchronously after write ACK. Never blocks the write path.
	Enrich(ctx context.Context, eng *Engram) (*EnrichmentResult, error)
}
