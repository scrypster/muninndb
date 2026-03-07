package embed

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/scrypster/muninndb/internal/plugin"
)

// Provider is the internal interface for provider HTTP clients.
type Provider interface {
	// Name returns the provider identifier for logging.
	Name() string

	// Init validates connectivity and detects dimension.
	// Returns the detected embedding dimension or an error.
	Init(ctx context.Context, cfg ProviderHTTPConfig) (dimension int, err error)

	// EmbedBatch sends a batch of texts and returns embeddings.
	// Returns a flat []float32 of length len(texts) * dimension.
	EmbedBatch(ctx context.Context, texts []string) ([]float32, error)

	// MaxBatchSize returns the maximum texts per API call for this provider.
	MaxBatchSize() int

	// Close releases HTTP connections.
	Close() error
}

// ProviderHTTPConfig is the resolved configuration for an HTTP provider.
type ProviderHTTPConfig struct {
	BaseURL string // "http://localhost:11434" or "https://api.openai.com"
	Model   string // "nomic-embed-text" or "text-embedding-3-small"
	APIKey  string // empty for Ollama, required for cloud providers
	DataDir string // local data directory for asset extraction (local provider only)
}

// EmbedService implements plugin.EmbedPlugin.
type EmbedService struct {
	provider Provider
	cfg      plugin.PluginConfig
	provCfg  *plugin.ProviderConfig
	dim      int    // detected at Init time
	name     string // "embed-ollama", "embed-openai", "embed-voyage"
	batcher  *BatchEmbedder
	limiter  *TokenBucketLimiter
	mu       sync.Mutex
	closed   bool
}

// NewEmbedService creates an EmbedService from a parsed provider URL.
// Auto-detects the provider type from the URL scheme.
func NewEmbedService(providerURL string) (*EmbedService, error) {
	provCfg, err := plugin.ParseProviderURL(providerURL)
	if err != nil {
		return nil, fmt.Errorf("failed to parse provider URL: %w", err)
	}

	var prov Provider
	switch provCfg.Scheme {
	case plugin.SchemeLocal:
		prov = &LocalProvider{}
	case plugin.SchemeOllama:
		prov = &OllamaProvider{}
	case plugin.SchemeOpenAI:
		prov = &OpenAIProvider{}
	case plugin.SchemeVoyage:
		prov = &VoyageProvider{}
	case plugin.SchemeCohere:
		prov = &CohereProvider{}
	case plugin.SchemeGoogle:
		prov = &GoogleProvider{}
	case plugin.SchemeJina:
		prov = &JinaProvider{}
	case plugin.SchemeMistral:
		prov = &MistralProvider{}
	default:
		return nil, fmt.Errorf("unsupported embed provider scheme: %q", provCfg.Scheme)
	}

	name := fmt.Sprintf("embed-%s", provCfg.Scheme)

	es := &EmbedService{
		provider: prov,
		provCfg:  provCfg,
		name:     name,
	}

	return es, nil
}

// Name returns the plugin identifier.
func (s *EmbedService) Name() string {
	return s.name
}

// Tier returns the plugin tier.
func (s *EmbedService) Tier() plugin.PluginTier {
	return plugin.TierEmbed
}

// Init validates configuration and external connectivity.
func (s *EmbedService) Init(ctx context.Context, cfg plugin.PluginConfig) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.cfg = cfg

	provHTTPCfg := ProviderHTTPConfig{
		BaseURL: s.provCfg.BaseURL,
		Model:   s.provCfg.Model,
		APIKey:  cfg.APIKey,
		DataDir: cfg.DataDir,
	}

	slog.Info("initializing embed provider",
		"name", s.name,
		"base_url", provHTTPCfg.BaseURL,
		"model", provHTTPCfg.Model,
	)

	// Initialize provider and detect dimension
	dim, err := s.provider.Init(ctx, provHTTPCfg)
	if err != nil {
		return fmt.Errorf("provider init failed: %w", err)
	}

	s.dim = dim

	// Validate dimension
	validDims := map[int]bool{384: true, 768: true, 1024: true, 1536: true}
	if !validDims[s.dim] {
		slog.Warn("detected embedding dimension not in standard set",
			"dimension", s.dim,
			"standard_dimensions", []int{384, 768, 1024, 1536},
		)
	}

	slog.Info("embed provider initialized",
		"name", s.name,
		"dimension", s.dim,
		"max_batch_size", s.provider.MaxBatchSize(),
	)

	// Set up rate limiter based on provider
	s.limiter = s.createRateLimiter(s.provCfg.Scheme, cfg.Options)

	// Set up batch embedder
	s.batcher = NewBatchEmbedder(s.provider, s.limiter)

	return nil
}

// Embed converts texts to embeddings.
func (s *EmbedService) Embed(ctx context.Context, texts []string) ([]float32, error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, fmt.Errorf("embed service is closed")
	}
	batcher := s.batcher
	s.mu.Unlock()

	if batcher == nil {
		return nil, fmt.Errorf("embed service not initialized")
	}

	return batcher.Embed(ctx, texts)
}

// Dimension returns the embedding vector dimension.
func (s *EmbedService) Dimension() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dim
}

// MaxBatchSize delegates to the underlying provider's optimal batch size.
func (s *EmbedService) MaxBatchSize() int {
	return s.provider.MaxBatchSize()
}

// Close releases external connections.
func (s *EmbedService) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return nil
	}
	s.closed = true

	if s.provider != nil {
		if err := s.provider.Close(); err != nil {
			slog.Error("error closing embed provider", "error", err)
			return err
		}
	}

	return nil
}

// createRateLimiter creates a rate limiter appropriate for the provider.
func (s *EmbedService) createRateLimiter(scheme plugin.ProviderScheme, options map[string]string) *TokenBucketLimiter {
	// Ollama is local, no rate limiting
	if scheme == plugin.SchemeOllama {
		return nil
	}

	// Default rates from spec
	var ratePerSec float64
	switch scheme {
	case plugin.SchemeOpenAI:
		ratePerSec = 50.0 // 3000 RPM / 60
	case plugin.SchemeVoyage:
		ratePerSec = 5.0 // 300 RPM / 60
	case plugin.SchemeCohere:
		ratePerSec = 16.0 // ~1000 RPM / 60
	case plugin.SchemeGoogle:
		ratePerSec = 25.0 // 1500 RPM / 60
	case plugin.SchemeJina:
		ratePerSec = 8.0 // 500 RPM / 60
	case plugin.SchemeMistral:
		ratePerSec = 10.0 // ~600 RPM / 60
	default:
		ratePerSec = 10.0 // conservative default
	}

	// Allow override via options
	if options != nil {
		if customRate, ok := options["rate_per_sec"]; ok {
			if parsedRate, err := time.ParseDuration(customRate); err == nil {
				ratePerSec = parsedRate.Seconds()
			}
		}
	}

	return NewTokenBucketLimiter(ratePerSec, ratePerSec*10) // max burst = 10 seconds
}
