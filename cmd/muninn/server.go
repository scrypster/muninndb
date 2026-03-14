package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"log/slog"
	"net"
	"net/http"
	neturl "net/url"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/scrypster/muninndb/internal/auth"
	"github.com/scrypster/muninndb/internal/backup"
	"github.com/scrypster/muninndb/internal/cognitive"
	plugincfg "github.com/scrypster/muninndb/internal/config"
	"github.com/scrypster/muninndb/internal/engine"
	"github.com/scrypster/muninndb/internal/engine/activation"
	"github.com/scrypster/muninndb/internal/engine/trigger"
	"github.com/scrypster/muninndb/internal/index/fts"
	hnswpkg "github.com/scrypster/muninndb/internal/index/hnsw"
	"github.com/scrypster/muninndb/internal/logging"
	"github.com/scrypster/muninndb/internal/mcp"
	"github.com/scrypster/muninndb/internal/metrics"
	"github.com/scrypster/muninndb/internal/metrics/latency"
	"github.com/scrypster/muninndb/internal/plugin"
	embedpkg "github.com/scrypster/muninndb/internal/plugin/embed"
	enrichpkg "github.com/scrypster/muninndb/internal/plugin/enrich"
	"github.com/scrypster/muninndb/internal/replication"
	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/storage/migrate"
	grpcpkg "github.com/scrypster/muninndb/internal/transport/grpc"
	"github.com/scrypster/muninndb/internal/transport/mbp"
	"github.com/scrypster/muninndb/internal/transport/rest"
	"github.com/scrypster/muninndb/internal/ui"
	"github.com/scrypster/muninndb/internal/wal"
	webui "github.com/scrypster/muninndb/web"
)

const defaultMCPPort = "8750"
const defaultOpenAIEmbedProviderURL = "openai://text-embedding-3-small"

const vaultUpgradeWarning = `
================================================================
NOTICE: Vault access is now fail-closed by default.

This server has existing data, but no vault access policy has
been configured. All vaults now require an API key unless
explicitly set to public.

To allow unauthenticated access to the default vault:

  curl -X POST http://HOST:PORT/api/admin/set-vault-config \
    -H "Content-Type: application/json" \
    -d '{"name":"default","public":true}'

Or generate an API key:

  muninn api-key create --vault default --label mykey

================================================================
`

// resolveEmbedInfo reads env vars and the saved plugin config to determine the
// active embed provider + model without side-effects (no network calls).
// Priority: env vars → plugin_config.json → local bundled → none.
func resolveEmbedInfo(cfg plugincfg.PluginConfig) rest.EmbedInfo {
	openAIOverride := strings.TrimSpace(os.Getenv("MUNINN_OPENAI_URL"))
	openAIOverrideValid := true
	if openAIOverride != "" {
		if _, err := resolveOpenAIEmbedProviderURL(openAIOverride); err != nil {
			openAIOverrideValid = false
		}
	}

	if rawURL := os.Getenv("MUNINN_OLLAMA_URL"); rawURL != "" {
		if provCfg, err := plugin.ParseProviderURL(rawURL); err == nil {
			return rest.EmbedInfo{Provider: "ollama", Model: provCfg.Model}
		}
		return rest.EmbedInfo{Provider: "ollama", Model: ""}
	}
	if os.Getenv("MUNINN_OPENAI_KEY") != "" {
		if openAIOverrideValid {
			if providerURL, err := resolveOpenAIEmbedProviderURL(openAIOverride); err == nil {
				if provCfg, parseErr := plugin.ParseProviderURL(providerURL); parseErr == nil {
					return rest.EmbedInfo{Provider: "openai", Model: provCfg.Model}
				}
			}
			return rest.EmbedInfo{Provider: "openai", Model: "text-embedding-3-small"}
		}
		// Explicit invalid override disables OpenAI so we don't accidentally
		// send traffic to the default OpenAI endpoint.
	}
	if os.Getenv("MUNINN_VOYAGE_KEY") != "" {
		return rest.EmbedInfo{Provider: "voyage", Model: "voyage-3"}
	}
	if os.Getenv("MUNINN_COHERE_KEY") != "" {
		return rest.EmbedInfo{Provider: "cohere", Model: "embed-v4"}
	}
	if os.Getenv("MUNINN_GOOGLE_KEY") != "" {
		return rest.EmbedInfo{Provider: "google", Model: "text-embedding-004"}
	}
	if os.Getenv("MUNINN_JINA_KEY") != "" {
		return rest.EmbedInfo{Provider: "jina", Model: "jina-embeddings-v3"}
	}
	if os.Getenv("MUNINN_MISTRAL_KEY") != "" {
		return rest.EmbedInfo{Provider: "mistral", Model: "mistral-embed"}
	}
	// Saved config fallback (env vars above take precedence).
	switch cfg.EmbedProvider {
	case "ollama":
		if cfg.EmbedURL != "" {
			if provCfg, err := plugin.ParseProviderURL(cfg.EmbedURL); err == nil {
				return rest.EmbedInfo{Provider: "ollama", Model: provCfg.Model}
			}
			return rest.EmbedInfo{Provider: "ollama", Model: ""}
		}
	case "openai":
		if !openAIOverrideValid {
			break
		}
		openaiSource := cfg.EmbedURL
		if openAIOverride != "" {
			openaiSource = openAIOverride
		}
		if providerURL, err := resolveOpenAIEmbedProviderURL(openaiSource); err == nil {
			if provCfg, parseErr := plugin.ParseProviderURL(providerURL); parseErr == nil {
				return rest.EmbedInfo{Provider: "openai", Model: provCfg.Model}
			}
			return rest.EmbedInfo{Provider: "openai", Model: "text-embedding-3-small"}
		}
		// Explicit invalid override in saved config disables OpenAI.
		if strings.TrimSpace(openaiSource) == "" {
			return rest.EmbedInfo{Provider: "openai", Model: "text-embedding-3-small"}
		}
	case "voyage":
		return rest.EmbedInfo{Provider: "voyage", Model: "voyage-3"}
	case "cohere":
		return rest.EmbedInfo{Provider: "cohere", Model: "embed-v4"}
	case "google":
		return rest.EmbedInfo{Provider: "google", Model: "text-embedding-004"}
	case "jina":
		return rest.EmbedInfo{Provider: "jina", Model: "jina-embeddings-v3"}
	case "mistral":
		return rest.EmbedInfo{Provider: "mistral", Model: "mistral-embed"}
	case "none":
		return rest.EmbedInfo{Provider: "none", Model: ""}
	}
	// Bundled local embedder: on by default. Opt out with MUNINN_LOCAL_EMBED=0.
	if os.Getenv("MUNINN_LOCAL_EMBED") != "0" && embedpkg.LocalAvailable() {
		return rest.EmbedInfo{Provider: "local", Model: "bge-small-en-v1.5"}
	}
	return rest.EmbedInfo{Provider: "none", Model: ""}
}

// resolveEnrichInfo determines the LLM enrichment provider and model from
// env variables and saved plugin config. Order: env vars → saved config.
func resolveEnrichInfo(cfg plugincfg.PluginConfig) rest.EnrichInfo {
	// Env var takes precedence.
	if enrichURL := os.Getenv("MUNINN_ENRICH_URL"); enrichURL != "" {
		if provCfg, err := plugin.ParseProviderURL(enrichURL); err == nil {
			return rest.EnrichInfo{Provider: string(provCfg.Scheme), Model: provCfg.Model}
		}
		return rest.EnrichInfo{Provider: "unknown", Model: ""}
	}
	// Saved config fallback.
	if cfg.EnrichProvider != "" && cfg.EnrichProvider != "none" {
		if cfg.EnrichURL != "" {
			if provCfg, err := plugin.ParseProviderURL(cfg.EnrichURL); err == nil {
				return rest.EnrichInfo{Provider: string(provCfg.Scheme), Model: provCfg.Model}
			}
		}
		return rest.EnrichInfo{Provider: cfg.EnrichProvider, Model: ""}
	}
	return rest.EnrichInfo{}
}

// resolveOpenAIEmbedProviderURL resolves an OpenAI embed URL override into a
// provider URL that ParseProviderURL can handle. Supports both:
//   - openai://text-embedding-3-small?base_url=http://localhost:8080
//   - http://localhost:8080 (treated as base_url with default model)
func resolveOpenAIEmbedProviderURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return defaultOpenAIEmbedProviderURL, nil
	}
	if strings.HasPrefix(strings.ToLower(raw), "openai://") {
		if _, err := plugin.ParseProviderURL(raw); err != nil {
			return "", err
		}
		return raw, nil
	}
	providerURL := defaultOpenAIEmbedProviderURL + "?base_url=" + neturl.QueryEscape(raw)
	if _, err := plugin.ParseProviderURL(providerURL); err != nil {
		return "", err
	}
	return providerURL, nil
}

func sanitizeProviderURLForLog(providerURL string) string {
	parsed, err := neturl.Parse(providerURL)
	if err != nil {
		return providerURL
	}
	if strings.EqualFold(parsed.Scheme, "openai") {
		parsed.RawQuery = ""
		return parsed.String()
	}
	return providerURL
}

func openAIEmbedLogAttrs(providerURL string) []any {
	provCfg, err := plugin.ParseProviderURL(providerURL)
	if err != nil {
		return []any{"url", sanitizeProviderURLForLog(providerURL)}
	}
	return []any{"model", provCfg.Model, "custom_base_url", provCfg.BaseURL != "https://api.openai.com"}
}

// buildEmbedder constructs an embedder. Priority (highest → lowest):
//  1. Environment variables (MUNINN_OLLAMA_URL, MUNINN_OPENAI_KEY (+ optional MUNINN_OPENAI_URL), MUNINN_VOYAGE_KEY, MUNINN_COHERE_KEY, MUNINN_GOOGLE_KEY, MUNINN_JINA_KEY, MUNINN_MISTRAL_KEY)
//  2. Saved plugin_config.json (cfg parameter)
//  3. Bundled local ONNX model — enabled by default when the binary was built
//     with embedded assets. Disable with MUNINN_LOCAL_EMBED=0.
//  4. Noop
//
// Returns both the activation.Embedder (for query embedding) and the underlying
// plugin.EmbedPlugin (for the RetroactiveProcessor), or nil for the plugin if noop.
func buildEmbedder(ctx context.Context, cfg plugincfg.PluginConfig, dataDir string) (activation.Embedder, plugin.EmbedPlugin, error) {
	const (
		ollamaURL  = "MUNINN_OLLAMA_URL"
		openaiKey  = "MUNINN_OPENAI_KEY"
		openaiURL  = "MUNINN_OPENAI_URL"
		voyageKey  = "MUNINN_VOYAGE_KEY"
		cohereKey  = "MUNINN_COHERE_KEY"
		googleKey  = "MUNINN_GOOGLE_KEY"
		jinaKey    = "MUNINN_JINA_KEY"
		mistralKey = "MUNINN_MISTRAL_KEY"
		localEmbed = "MUNINN_LOCAL_EMBED"
	)

	openAIEnvOverride := strings.TrimSpace(os.Getenv(openaiURL))
	openAIEnvOverrideInvalid := false
	if openAIEnvOverride != "" {
		if _, err := resolveOpenAIEmbedProviderURL(openAIEnvOverride); err != nil {
			openAIEnvOverrideInvalid = true
			slog.Warn("invalid OpenAI URL override detected, OpenAI embedder disabled", "env_var", openaiURL, "error", err)
		}
	}

	tryEmbedService := func(providerURL string, pcfg plugin.PluginConfig) *embedpkg.EmbedService {
		logURL := sanitizeProviderURLForLog(providerURL)
		svc, err := embedpkg.NewEmbedService(providerURL)
		if err != nil {
			slog.Warn("embedder service creation failed", "url", logURL, "error", err)
			return nil
		}
		if err := svc.Init(ctx, pcfg); err != nil {
			slog.Warn("embedder init failed, trying next provider", "url", logURL, "error", err)
			_ = svc.Close()
			return nil
		}
		return svc
	}

	// 1. Env var: Ollama
	if url := os.Getenv(ollamaURL); url != "" {
		slog.Info("initializing Ollama embedder", "url", url)
		if svc := tryEmbedService(url, plugin.PluginConfig{}); svc != nil {
			return embedpkg.NewEmbedServiceAdapter(svc), svc, nil
		}
	}

	// 1. Env var: OpenAI
	if key := os.Getenv(openaiKey); key != "" {
		if !openAIEnvOverrideInvalid {
			openaiProviderURL, err := resolveOpenAIEmbedProviderURL(openAIEnvOverride)
			if err != nil {
				slog.Warn("failed to resolve OpenAI provider URL, skipping OpenAI embedder", "error", err)
			} else {
				slog.Info("initializing OpenAI embedder", openAIEmbedLogAttrs(openaiProviderURL)...)
				if svc := tryEmbedService(openaiProviderURL, plugin.PluginConfig{APIKey: key}); svc != nil {
					return embedpkg.NewEmbedServiceAdapter(svc), svc, nil
				}
			}
		}
	}

	// 1. Env var: Voyage
	if key := os.Getenv(voyageKey); key != "" {
		slog.Info("initializing Voyage embedder")
		if svc := tryEmbedService("voyage://voyage-3", plugin.PluginConfig{APIKey: key}); svc != nil {
			return embedpkg.NewEmbedServiceAdapter(svc), svc, nil
		}
	}

	// 1. Env var: Cohere
	if key := os.Getenv(cohereKey); key != "" {
		slog.Info("initializing Cohere embedder")
		if svc := tryEmbedService("cohere://embed-v4", plugin.PluginConfig{APIKey: key}); svc != nil {
			return embedpkg.NewEmbedServiceAdapter(svc), svc, nil
		}
	}

	// 1. Env var: Google
	if key := os.Getenv(googleKey); key != "" {
		slog.Info("initializing Google embedder")
		if svc := tryEmbedService("google://text-embedding-004", plugin.PluginConfig{APIKey: key}); svc != nil {
			return embedpkg.NewEmbedServiceAdapter(svc), svc, nil
		}
	}

	// 1. Env var: Jina
	if key := os.Getenv(jinaKey); key != "" {
		slog.Info("initializing Jina embedder")
		if svc := tryEmbedService("jina://jina-embeddings-v3", plugin.PluginConfig{APIKey: key}); svc != nil {
			return embedpkg.NewEmbedServiceAdapter(svc), svc, nil
		}
	}

	// 1. Env var: Mistral
	if key := os.Getenv(mistralKey); key != "" {
		slog.Info("initializing Mistral embedder")
		if svc := tryEmbedService("mistral://mistral-embed", plugin.PluginConfig{APIKey: key}); svc != nil {
			return embedpkg.NewEmbedServiceAdapter(svc), svc, nil
		}
	}

	// 2. Saved config fallback
	if cfg.EmbedProvider != "" && cfg.EmbedProvider != "none" && cfg.EmbedProvider != "local" {
		switch cfg.EmbedProvider {
		case "ollama":
			if cfg.EmbedURL != "" {
				slog.Info("initializing Ollama embedder from saved config", "url", cfg.EmbedURL)
				if svc := tryEmbedService(cfg.EmbedURL, plugin.PluginConfig{}); svc != nil {
					return embedpkg.NewEmbedServiceAdapter(svc), svc, nil
				}
			}
		case "openai":
			if openAIEnvOverrideInvalid {
				slog.Warn("invalid OpenAI URL override is set, skipping OpenAI embedder from saved config", "env_var", openaiURL)
				break
			}
			openaiSource := cfg.EmbedURL
			if openAIEnvOverride != "" {
				openaiSource = openAIEnvOverride
			}
			openaiProviderURL, err := resolveOpenAIEmbedProviderURL(openaiSource)
			if err != nil {
				if strings.TrimSpace(openaiSource) != "" {
					slog.Warn("invalid OpenAI URL in saved config, skipping OpenAI embedder", "error", err)
				} else {
					slog.Warn("failed to resolve OpenAI provider URL from saved config, skipping OpenAI embedder", "error", err)
				}
				break
			}
			slog.Info("initializing OpenAI embedder from saved config", openAIEmbedLogAttrs(openaiProviderURL)...)
			if svc := tryEmbedService(openaiProviderURL, plugin.PluginConfig{APIKey: cfg.EmbedAPIKey}); svc != nil {
				return embedpkg.NewEmbedServiceAdapter(svc), svc, nil
			}
		case "voyage":
			slog.Info("initializing Voyage embedder from saved config")
			if svc := tryEmbedService("voyage://voyage-3", plugin.PluginConfig{APIKey: cfg.EmbedAPIKey}); svc != nil {
				return embedpkg.NewEmbedServiceAdapter(svc), svc, nil
			}
		case "cohere":
			slog.Info("initializing Cohere embedder from saved config")
			if svc := tryEmbedService("cohere://embed-v4", plugin.PluginConfig{APIKey: cfg.EmbedAPIKey}); svc != nil {
				return embedpkg.NewEmbedServiceAdapter(svc), svc, nil
			}
		case "google":
			slog.Info("initializing Google embedder from saved config")
			if svc := tryEmbedService("google://text-embedding-004", plugin.PluginConfig{APIKey: cfg.EmbedAPIKey}); svc != nil {
				return embedpkg.NewEmbedServiceAdapter(svc), svc, nil
			}
		case "jina":
			slog.Info("initializing Jina embedder from saved config")
			if svc := tryEmbedService("jina://jina-embeddings-v3", plugin.PluginConfig{APIKey: cfg.EmbedAPIKey}); svc != nil {
				return embedpkg.NewEmbedServiceAdapter(svc), svc, nil
			}
		case "mistral":
			slog.Info("initializing Mistral embedder from saved config")
			if svc := tryEmbedService("mistral://mistral-embed", plugin.PluginConfig{APIKey: cfg.EmbedAPIKey}); svc != nil {
				return embedpkg.NewEmbedServiceAdapter(svc), svc, nil
			}
		}
	}

	// 3. Bundled local ONNX model — on by default when embedded at build time.
	// Skip only if the user explicitly opts out (MUNINN_LOCAL_EMBED=0) or chose
	// "none" as their provider.
	if cfg.EmbedProvider != "none" && os.Getenv(localEmbed) != "0" && embedpkg.LocalAvailable() {
		slog.Info("initializing bundled local ONNX embedder", "data_dir", dataDir)
		if svc := tryEmbedService("local://all-MiniLM-L6-v2", plugin.PluginConfig{DataDir: dataDir}); svc != nil {
			return embedpkg.NewEmbedServiceAdapter(svc), svc, nil
		}
		slog.Warn("bundled local embedder init failed, falling back to noop")
	}

	// 4. Noop
	slog.Warn("no embedder configured, semantic similarity disabled")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "  ⚠  No embedder configured — semantic search disabled.")
	fmt.Fprintln(os.Stderr, "     To use a cloud embedder: set MUNINN_OPENAI_KEY (optionally MUNINN_OPENAI_URL), MUNINN_COHERE_KEY, MUNINN_GOOGLE_KEY, etc.")
	fmt.Fprintln(os.Stderr, "     To disable this warning: set MUNINN_LOCAL_EMBED=0")
	fmt.Fprintln(os.Stderr, "")
	return activation.NewNoopEmbedder(), nil, nil
}

// buildEnricher constructs an EnrichService from environment variables.
// Reads MUNINN_ENRICH_URL to select provider and model. Supported schemes:
//
//	ollama://localhost:11434/llama3.2          (local, no key required)
//	openai://gpt-4o-mini                       (MUNINN_ENRICH_API_KEY required)
//	anthropic://claude-haiku-4-5-20251001      (MUNINN_ANTHROPIC_KEY or MUNINN_ENRICH_API_KEY)
//
// Returns nil without error if MUNINN_ENRICH_URL is not set — LLM enrichment
// is optional. Logs a warning on init failure so the server starts without
// enrichment rather than refusing to start.
// buildEnricher constructs an EnrichService. Priority:
//  1. MUNINN_ENRICH_URL env var
//  2. Saved plugin_config.json (cfg parameter)
//
// Returns nil (no error) if neither is set — LLM enrichment is optional.
func buildEnricher(ctx context.Context, cfg plugincfg.PluginConfig) plugin.EnrichPlugin {
	enrichURL := os.Getenv("MUNINN_ENRICH_URL")

	// Fall back to saved config if env var is not set.
	if enrichURL == "" && cfg.EnrichURL != "" {
		enrichURL = cfg.EnrichURL
	}

	if enrichURL == "" {
		slog.Info("no enrich plugin configured, LLM enrichment disabled")
		return nil
	}

	slog.Info("initializing enrich plugin", "url", enrichURL)
	svc, err := enrichpkg.NewEnrichService(enrichURL)
	if err != nil {
		slog.Warn("enrich plugin URL parse failed, LLM enrichment disabled", "err", err)
		return nil
	}

	// MUNINN_ANTHROPIC_KEY is an alias for MUNINN_ENRICH_API_KEY when using Anthropic.
	apiKey := os.Getenv("MUNINN_ENRICH_API_KEY")
	if apiKey == "" {
		apiKey = os.Getenv("MUNINN_ANTHROPIC_KEY")
	}
	if apiKey == "" {
		apiKey = cfg.EnrichAPIKey // saved config fallback
	}
	if err := svc.Init(ctx, plugin.PluginConfig{APIKey: apiKey}); err != nil {
		slog.Warn("enrich plugin init failed (LLM provider may be down), LLM enrichment disabled", "err", err)
		return nil
	}

	slog.Info("enrich plugin initialized", "url", enrichURL)
	return svc
}

// parseCORSOrigins splits a comma-separated MUNINN_CORS_ORIGINS env var into a slice.
// Returns nil if the string is empty — no cross-origin access allowed.
func parseCORSOrigins(env string) []string {
	if env == "" {
		return nil
	}
	var origins []string
	for _, o := range strings.Split(env, ",") {
		if o = strings.TrimSpace(o); o != "" {
			origins = append(origins, o)
		}
	}
	return origins
}

// applyMemoryLimits sets GOMEMLIMIT and GOGC for the server process.
// GOMEMLIMIT prevents unbounded heap growth; GOGC controls GC frequency.
// Configure with MUNINN_MEM_LIMIT_GB (default 4) and MUNINN_GC_PERCENT (default 200).
func applyMemoryLimits() {
	const defaultMemGB = 4
	const defaultGCPercent = 200

	memGB := defaultMemGB
	if s := os.Getenv("MUNINN_MEM_LIMIT_GB"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			memGB = n
		}
	}

	gcPct := defaultGCPercent
	if s := os.Getenv("MUNINN_GC_PERCENT"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			gcPct = n
		}
	}

	debug.SetMemoryLimit(int64(memGB) * 1024 * 1024 * 1024)
	debug.SetGCPercent(gcPct)
	slog.Info("memory limits applied",
		"mem_limit_gb", memGB,
		"gc_percent", gcPct,
	)
}

// runStartupMigrations runs all idempotent storage migrations on startup.
// It enumerates every known vault and calls MigrateBuckets for each one.
// Migration errors are non-fatal: a warning is logged and startup continues.
func runStartupMigrations(ctx context.Context, store *storage.PebbleStore) {
	names, err := store.ListVaultNames()
	if err != nil {
		slog.Warn("startup migration: failed to list vault names", "err", err)
		return
	}
	for _, name := range names {
		prefix := store.ResolveVaultPrefix(name)
		if err := store.MigrateBuckets(ctx, prefix); err != nil {
			slog.Warn("startup migration: MigrateBuckets failed", "vault", name, "err", err)
		}
	}
	slog.Info("startup migration complete", "vaults", len(names))
}

// handleClusterConn reads MBP frames from an incoming cluster TCP connection
// and dispatches them to the coordinator. Exits when the connection is closed.
func handleClusterConn(conn net.Conn, coord *replication.ClusterCoordinator) {
	defer conn.Close()
	for {
		frame, err := mbp.ReadFrame(conn)
		if err != nil {
			return // connection closed or error
		}
		fromNodeID := conn.RemoteAddr().String()
		if err := coord.HandleIncomingFrame(fromNodeID, frame.Type, frame.Payload); err != nil {
			log.Printf("[cluster] frame error from %s: %v", fromNodeID, err)
		}
	}
}

// validateServerFlags checks that each addr is a valid host:port pair with a
// port number in the range 1-65535. Returns the first validation error found.
func validateServerFlags(addrs ...string) error {
	for _, addr := range addrs {
		host, portStr, err := net.SplitHostPort(addr)
		if err != nil {
			return fmt.Errorf("invalid address %q: %w", addr, err)
		}
		_ = host
		port, err := strconv.Atoi(portStr)
		if err != nil || port < 1 || port > 65535 {
			return fmt.Errorf("invalid port in address %q: port must be 1-65535", addr)
		}
	}
	return nil
}

// parseListenHost extracts the --listen-host value from args, falling back to
// envVal and then "127.0.0.1". It is a pure function so it can be tested
// without parsing the real flag set.
func parseListenHost(args []string, envVal string) string {
	host := "127.0.0.1"
	if envVal != "" {
		host = envVal
	}
	for i, arg := range args {
		if (arg == "--listen-host" || arg == "-listen-host") && i+1 < len(args) {
			host = args[i+1]
			break
		}
		if after, ok := strings.CutPrefix(arg, "--listen-host="); ok {
			host = after
			break
		}
		if after, ok := strings.CutPrefix(arg, "-listen-host="); ok {
			host = after
			break
		}
	}
	return host
}

func runServer() {
	loadEnvFile()

	// Apply memory limits before any significant allocations.
	applyMemoryLimits()

	// Pre-scan os.Args for --listen-host so we can use it as the default host
	// for all --*-addr flags. Explicit --*-addr flags will still override it.
	listenHost := parseListenHost(os.Args[1:], os.Getenv("MUNINN_LISTEN_HOST"))

	// Flags
	dataDir := flag.String("data", "./muninn-data", "data directory")
	_ = flag.String("listen-host", listenHost, `host to bind all servers to (default "127.0.0.1"; use 0.0.0.0 for LAN/remote access)`)
	mbpAddr := flag.String("mbp-addr", listenHost+":8474", "MBP TCP listen address")
	restAddr := flag.String("rest-addr", listenHost+":8475", "REST HTTP listen address")
	mcpAddr := flag.String("mcp-addr", listenHost+":"+defaultMCPPort, "MCP JSON-RPC listen address")
	grpcAddr := flag.String("grpc-addr", listenHost+":8477", "gRPC listen address")
	metricsAddr := flag.String("metrics-addr", "", "Prometheus /metrics listen address (empty = disabled)")
	uiAddrDefault := listenHost + ":8476"
	if v := os.Getenv("MUNINN_UI_ADDR"); v != "" {
		uiAddrDefault = v
	}
	uiAddr := flag.String("ui-addr", uiAddrDefault, "Web UI HTTP listen address")
	mcpToken := flag.String("mcp-token", "", "Bearer token override for MCP auth (leave empty to read from ~/.muninn/mcp.token)")
	dev := flag.Bool("dev", false, "serve web assets from ./web directory (development mode)")
	backupInterval := flag.String("backup-interval", "", "Automated backup interval (e.g. 6h, 30m); empty = disabled")
	backupDir := flag.String("backup-dir", "", "Directory to write automated backups into")
	backupRetain := flag.Int("backup-retain", 5, "Number of automated backups to keep")
	tlsCert := flag.String("tls-cert", "", "Path to TLS certificate file (PEM)")
	tlsKey := flag.String("tls-key", "", "Path to TLS private key file (PEM)")
	corsOriginsDefault := os.Getenv("MUNINN_CORS_ORIGINS")
	corsOriginsFlag := flag.String("cors-origins", corsOriginsDefault, "Comma-separated allowed CORS origins for browser clients (e.g. http://myapp.local:3000); overrides MUNINN_CORS_ORIGINS")
	var logLevelStr string
	flag.StringVar(&logLevelStr, "log-level", "info", "Log level: debug, info, warn, error")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage of muninndb:\n")
		flag.PrintDefaults()
		fmt.Fprintf(os.Stderr, "\nEnvironment variables (primary configuration; see docs for full list):\n")
		fmt.Fprintf(os.Stderr, "  MUNINN_OLLAMA_URL            Ollama embedder base URL (e.g. http://localhost:11434)\n")
		fmt.Fprintf(os.Stderr, "  MUNINN_OPENAI_KEY            OpenAI embedder API key\n")
		fmt.Fprintf(os.Stderr, "  MUNINN_OPENAI_URL            Optional OpenAI base URL or provider URL override\n")
		fmt.Fprintf(os.Stderr, "  MUNINN_VOYAGE_KEY            Voyage embedder API key\n")
		fmt.Fprintf(os.Stderr, "  MUNINN_COHERE_KEY            Cohere embedder API key\n")
		fmt.Fprintf(os.Stderr, "  MUNINN_GOOGLE_KEY            Google Gemini embedder API key\n")
		fmt.Fprintf(os.Stderr, "  MUNINN_JINA_KEY              Jina embedder API key\n")
		fmt.Fprintf(os.Stderr, "  MUNINN_MISTRAL_KEY           Mistral embedder API key\n")
		fmt.Fprintf(os.Stderr, "  MUNINN_LOCAL_EMBED           Set to \"0\" to disable bundled ONNX embedder\n")
		fmt.Fprintf(os.Stderr, "  MUNINN_ENRICH_URL            LLM enrichment endpoint URL (optional)\n")
		fmt.Fprintf(os.Stderr, "  MUNINN_ENRICH_API_KEY        API key for enrichment (or MUNINN_ANTHROPIC_KEY)\n")
		fmt.Fprintf(os.Stderr, "  MUNINN_ENRICH_TIMEOUT        Per-engram LLM timeout for replay_enrichment (e.g. 60s, 2m; default: no extra timeout)\n")
		fmt.Fprintf(os.Stderr, "  MUNINN_HNSW_WARN_THRESHOLD_MB  Emit a warning when HNSW in-memory vector bytes exceed N MB (optional)\n")
		fmt.Fprintf(os.Stderr, "  MUNINN_HNSW_MAX_MB             Skip HNSW insert (keep Pebble write) when memory exceeds N MB (optional)\n")
		fmt.Fprintf(os.Stderr, "  MUNINN_LISTEN_HOST           Host to bind all servers to (e.g. 0.0.0.0 for LAN access)\n")
		fmt.Fprintf(os.Stderr, "  MUNINN_CORS_ORIGINS          Comma-separated CORS allowed origins\n")
		fmt.Fprintf(os.Stderr, "  MUNINN_MEM_LIMIT_GB          Memory limit in GB (default: 4)\n")
		fmt.Fprintf(os.Stderr, "  MUNINN_GC_PERCENT            Go GC target percentage (default: 200)\n")
		fmt.Fprintf(os.Stderr, "  MUNINN_RATE_LIMIT_GLOBAL_RPS Global rate limit requests/sec (default: 1000)\n")
		fmt.Fprintf(os.Stderr, "  MUNINN_RATE_LIMIT_PER_IP_RPS Per-IP rate limit requests/sec (default: 100)\n")
		fmt.Fprintf(os.Stderr, "  MUNINN_BACKUP_INTERVAL        Automated backup interval (e.g. 6h, 30m); empty = disabled\n")
		fmt.Fprintf(os.Stderr, "  MUNINN_BACKUP_DIR             Directory to write automated backups into\n")
		fmt.Fprintf(os.Stderr, "  MUNINN_BACKUP_RETAIN          Number of automated backups to keep (default: 5)\n")
	}
	flag.Parse()

	// Persist actual bound addresses so 'muninn status' and the startup health poll
	// can probe the correct ports when non-default --*-addr flags are used.
	_ = writeAddrsFile(*dataDir, daemonAddrs{
		RestAddr: *restAddr,
		MCPAddr:  *mcpAddr,
		UIAddr:   *uiAddr,
	})

	// MCP token: --mcp-token flag is an explicit override (tests, container entrypoints).
	// Default path reads from ~/.muninn/mcp.token so the token never appears in `ps` output.
	if *mcpToken == "" {
		*mcpToken = readTokenFile()
	}

	// TLS env fallbacks — flags take priority; env vars are the fallback.
	if *tlsCert == "" {
		*tlsCert = os.Getenv("MUNINN_TLS_CERT")
	}
	if *tlsKey == "" {
		*tlsKey = os.Getenv("MUNINN_TLS_KEY")
	}

	// Backup env fallbacks — flags take priority; env vars are the fallback.
	if *backupInterval == "" {
		*backupInterval = os.Getenv("MUNINN_BACKUP_INTERVAL")
	}
	if *backupDir == "" {
		*backupDir = os.Getenv("MUNINN_BACKUP_DIR")
	}
	if *backupRetain == 5 {
		if s := os.Getenv("MUNINN_BACKUP_RETAIN"); s != "" {
			if n, err := strconv.Atoi(s); err == nil && n > 0 {
				*backupRetain = n
			}
		}
	}

	// Parse and validate backup configuration.
	var backupIntervalDur time.Duration
	if *backupInterval != "" {
		d, err := time.ParseDuration(*backupInterval)
		if err != nil {
			slog.Error("invalid --backup-interval", "value", *backupInterval, "err", err)
			os.Exit(1)
		}
		backupIntervalDur = d
	}
	if (backupIntervalDur > 0) != (*backupDir != "") {
		slog.Error("backup: --backup-interval and --backup-dir must both be set or both be empty")
		os.Exit(1)
	}

	// Validate: both cert and key must be provided together, or neither.
	if (*tlsCert == "") != (*tlsKey == "") {
		slog.Error("tls: --tls-cert and --tls-key must both be set (or neither)")
		os.Exit(1)
	}

	// Load TLS configuration if cert/key pair is provided.
	var clientTLS *tls.Config
	if *tlsCert != "" {
		cert, err := tls.LoadX509KeyPair(*tlsCert, *tlsKey)
		if err != nil {
			slog.Error("tls: failed to load certificate", "cert", *tlsCert, "err", err)
			os.Exit(1)
		}
		clientTLS = &tls.Config{
			Certificates: []tls.Certificate{cert},
			MinVersion:   tls.VersionTLS12,
		}
		slog.Info("tls: client-facing TLS enabled", "cert", *tlsCert)
	}

	// Validate address flags early so misconfigurations are caught before any
	// resources are allocated. metricsAddr is optional (empty = disabled).
	addrsToValidate := []string{*mbpAddr, *restAddr, *mcpAddr, *grpcAddr, *uiAddr}
	if *metricsAddr != "" {
		addrsToValidate = append(addrsToValidate, *metricsAddr)
	}
	if err := validateServerFlags(addrsToValidate...); err != nil {
		slog.Error("invalid server address flag", "err", err)
		os.Exit(1)
	}

	if listenHost == "0.0.0.0" {
		slog.Warn("all services bound to 0.0.0.0 — ensure firewall rules are in place")
	}

	var logLevel slog.Level
	if err := logLevel.UnmarshalText([]byte(logLevelStr)); err != nil {
		fmt.Fprintf(os.Stderr, "invalid --log-level %q: must be debug, info, warn, or error\n", logLevelStr)
		os.Exit(1)
	}
	// Create ring buffer — onAdd wired after uiSrv is constructed.
	ring := logging.NewRingBuffer(1000, nil)
	baseHandler := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: logLevel})
	slog.SetDefault(slog.New(logging.NewRingHandler(baseHandler, ring)))

	// Resolve web FS (embedded by default, filesystem in dev mode)
	var webFS fs.FS = webui.FS
	if *dev {
		// Try to find web directory relative to binary location first
		webDir := filepath.Join(filepath.Dir(os.Args[0]), "web")
		if _, err := os.Stat(webDir); err != nil {
			// Fallback: check current working directory
			webDir = "web"
		}
		webFS = os.DirFS(webDir)
		slog.Info("dev mode: serving web assets from filesystem", "dir", webDir)
	}

	// Open Pebble
	// Use 0700 so other local users cannot list or access the database directory.
	dbPath := filepath.Join(*dataDir, "pebble")
	if err := os.MkdirAll(dbPath, 0700); err != nil {
		slog.Error("create data dir", "err", err)
		os.Exit(1)
	}

	// Verify the data directory is writable before opening the DB.
	testFile := filepath.Join(dbPath, ".write-test")
	if err := os.WriteFile(testFile, []byte("ok"), 0600); err != nil {
		slog.Error("data directory is not writable", "path", dbPath, "err", err)
		os.Exit(1)
	}
	os.Remove(testFile)

	db, err := storage.OpenPebble(dbPath, storage.DefaultOptions())
	if err != nil {
		slog.Error("open pebble", "err", err)
		os.Exit(1)
	}
	// NOTE: db.Close() is NOT deferred here because store.Close() (called
	// during the ordered shutdown sequence) internally closes the Pebble DB
	// after flushing its own background workers.

	if err := replication.CheckAndSetSchemaVersion(db); err != nil {
		slog.Error("schema version check", "err", err)
		os.Exit(1)
	}

	// Run versioned schema migrations before the storage layer is built.
	migRunner := migrate.NewRunner(db)
	migRunner.Register(migrate.Migration{
		Version:     1,
		Description: "backfill embed_dim in ERF records for existing embeddings",
		Up:          migrate.BackfillEmbedDim,
	})
	migRunner.Register(migrate.Migration{
		Version:     2,
		Description: "backfill relationship entity index (0x26) for GetEntityAggregate optimisation",
		Up:          migrate.BackfillRelEntityIndex,
	})
	if applied, err := migRunner.Run(); err != nil {
		slog.Error("migration failed", "err", err)
		db.Close()
		os.Exit(1)
	} else if applied > 0 {
		slog.Info("migrations applied", "count", applied)
	}

	// Load cluster config (disabled by default; enabled via muninn.yaml or cluster.yaml).
	clusterCfg, err := plugincfg.LoadClusterConfig(*dataDir)
	if err != nil {
		slog.Error("load cluster config", "err", err)
		os.Exit(1)
	}
	if err := clusterCfg.Validate(); err != nil {
		slog.Error("invalid cluster config", "err", err)
		os.Exit(1)
	}

	// Wire ClusterCoordinator when cluster mode is enabled.
	var coordinator *replication.ClusterCoordinator
	if clusterCfg.Enabled {
		repLog := replication.NewReplicationLog(db)
		applier := replication.NewApplier(db)
		epochStore, err := replication.NewEpochStore(db)
		if err != nil {
			slog.Error("create epoch store", "err", err)
			os.Exit(1)
		}
		coordinator = replication.NewClusterCoordinator(&clusterCfg, repLog, applier, epochStore)

		// Role change callbacks are wired after engine creation (below).
	}

	authStore := auth.NewStore(db)
	secretPath := filepath.Join(*dataDir, "auth_secret")
	sessionSecret, err := auth.Bootstrap(authStore, secretPath)
	if err != nil {
		slog.Error("auth bootstrap failed", "err", err)
		os.Exit(1)
	}

	// Open MOL (Write-Ahead Log)
	walPath := filepath.Join(*dataDir, "wal")
	mol, err := wal.Open(walPath)
	if err != nil {
		slog.Error("open wal", "err", err)
		os.Exit(1)
	}
	defer mol.Close()

	// Recover MOL: replay sealed segments to reconcile sequence tracking.
	// Crash recovery of engram data is handled by Pebble's internal WAL.
	// The MOL replay ensures replication sequence continuity.
	lastSeq := wal.LoadLastSeq(db)
	var replayedCount int
	err = mol.Recover(db, func(e *wal.MOLEntry) error {
		if e.SeqNum <= lastSeq {
			return nil // already committed
		}
		replayedCount++
		return nil
	})
	if err != nil {
		slog.Error("recover wal", "err", err)
		os.Exit(1)
	}
	if replayedCount > 0 {
		slog.Info("wal recovery", "replayed_entries", replayedCount, "last_committed_seq", lastSeq)
	}

	// Build storage layer
	store := storage.NewPebbleStore(db, storage.PebbleStoreConfig{CacheSize: 10000})

	// Run startup migrations before the engine is built.
	runStartupMigrations(context.Background(), store)

	// Create GroupCommitter
	gc := wal.NewGroupCommitter(mol, db)

	// Set WAL on store
	store.SetWAL(mol, gc)

	// Wire MOL into coordinator for periodic SafePrune.
	if coordinator != nil {
		coordinator.SetMOL(mol)
	}

	// Build indexes
	ftsIndex := fts.New(db)

	// Load saved plugin config (env vars always override these values).
	savedPluginCfg, err := plugincfg.LoadPluginConfig(*dataDir)
	if err != nil {
		slog.Warn("failed to load plugin config, using defaults", "err", err)
	}

	// Build embedder: env vars → saved config → local bundled → noop.
	initCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	embedder, embedPlugin, err := buildEmbedder(initCtx, savedPluginCfg, *dataDir)
	cancel()
	if err != nil {
		slog.Error("embedder build failed", "err", err)
		os.Exit(1)
	}

	// Determine embedder provider and model for the status endpoint.
	embedInfo := resolveEmbedInfo(savedPluginCfg)
	// Wire hardware acceleration flag once at startup (captured before first request).
	if embedPlugin != nil {
		if h, ok := embedPlugin.(plugin.HardwareAwarePlugin); ok {
			v := h.HardwareAccelerated()
			embedInfo.HardwareAccelerated = &v
		}
	}

	// Build enrich plugin (optional): env vars → saved config.
	enrichCtx, enrichCancel := context.WithTimeout(context.Background(), 30*time.Second)
	enrichPlugin := buildEnricher(enrichCtx, savedPluginCfg)
	enrichCancel()

	// Build HNSW registry (multi-vault, lazy-loading)
	hnswRegistry := hnswpkg.NewRegistry(db)

	// Apply optional memory thresholds from environment variables.
	//
	//   MUNINN_HNSW_WARN_THRESHOLD_MB  – emit a throttled slog.Warn when total
	//                                    in-memory vector bytes exceed this value
	//                                    (no insert penalty; default: disabled).
	//   MUNINN_HNSW_MAX_MB             – skip graph insert when total bytes meet
	//                                    or exceed this value (vector still stored
	//                                    in Pebble; FTS unaffected; default: disabled).
	if warnMBStr := os.Getenv("MUNINN_HNSW_WARN_THRESHOLD_MB"); warnMBStr != "" {
		if warnMB, err := strconv.ParseInt(warnMBStr, 10, 64); err != nil || warnMB <= 0 {
			slog.Warn("invalid MUNINN_HNSW_WARN_THRESHOLD_MB, ignoring", "value", warnMBStr)
		} else {
			hnswRegistry.SetWarnThresholdBytes(warnMB << 20)
			slog.Info("hnsw: memory warn threshold configured", "warn_threshold_mb", warnMB)
		}
	}
	if maxMBStr := os.Getenv("MUNINN_HNSW_MAX_MB"); maxMBStr != "" {
		if maxMB, err := strconv.ParseInt(maxMBStr, 10, 64); err != nil || maxMB <= 0 {
			slog.Warn("invalid MUNINN_HNSW_MAX_MB, ignoring", "value", maxMBStr)
		} else {
			hnswRegistry.SetMaxBytes(maxMB << 20)
			slog.Info("hnsw: hard memory limit configured", "max_mb", maxMB)
		}
	}

	// Build activation engine
	actEngine := activation.New(store, activation.NewFTSAdapter(ftsIndex), activation.NewHNSWAdapter(hnswRegistry), embedder)

	// Build trigger system
	trigSystem := trigger.New(store, trigger.NewFTSAdapter(ftsIndex), trigger.NewHNSWAdapter(hnswRegistry), embedder)

	// Signal handling context — created early so workers can inherit it for graceful shutdown.
	ctx, cancel := context.WithCancel(context.Background())

	// Create cognitive workers with storage adapters
	hebbianWorkerImpl := cognitive.NewHebbianWorker(cognitive.NewHebbianStoreAdapter(store))
	contradictWorkerImpl := cognitive.NewContradictWorker(cognitive.NewContradictStoreAdapter(store))
	confidenceWorkerImpl := cognitive.NewConfidenceWorker(cognitive.NewConfidenceStoreAdapter(store))

	// Create PAS transition worker, wiring it to the TransitionCache.
	transitionWorkerImpl := cognitive.NewTransitionWorker(ctx, store.TransitionCache())
	actEngine.SetTransitionStore(store.TransitionCache())

	// Build engine API - pass the full worker implementations
	eng := engine.NewEngine(engine.EngineConfig{
		Store:            store,
		AuthStore:        authStore,
		FTSIndex:         ftsIndex,
		ActivationEngine: actEngine,
		TriggerSystem:    trigSystem,
		HebbianWorker:    hebbianWorkerImpl,
		ContradictWorker: contradictWorkerImpl.Worker,
		ConfidenceWorker: confidenceWorkerImpl.Worker,
		Embedder:         embedder,
		HNSWRegistry:     hnswRegistry,
	})

	eng.SetTransitionWorker(transitionWorkerImpl)

	latTracker := latency.New()
	eng.SetLatencyTracker(latTracker)

	// Wire cluster role change callbacks now that the engine exists.
	if coordinator != nil {
		hebbianStore := cognitive.NewHebbianStoreAdapter(store)
		contradictStore := cognitive.NewContradictStoreAdapter(store)
		confidenceStore := cognitive.NewConfidenceStoreAdapter(store)

		var cogCancel context.CancelFunc
		var cogHeb *cognitive.HebbianWorker
		var cogTransition *cognitive.TransitionWorker

		coordinator.OnBecameCortex = func(epoch uint64) {
			slog.Info("[cluster] node promoted to Cortex — starting cognitive workers", "epoch", epoch)
			cogHeb = cognitive.NewHebbianWorker(hebbianStore)
			contra := cognitive.NewContradictWorker(contradictStore)
			conf := cognitive.NewConfidenceWorker(confidenceStore)
			eng.SetCognitiveWorkers(cogHeb, contra.Worker, conf.Worker)

			cogTransition = cognitive.NewTransitionWorker(ctx, store.TransitionCache())
			eng.SetTransitionWorker(cogTransition)

			var cogCtx context.Context
			cogCtx, cogCancel = context.WithCancel(context.Background())
			go contra.Worker.Run(cogCtx)
			go conf.Worker.Run(cogCtx)
		}
		coordinator.OnBecameLobe = func() {
			slog.Info("[cluster] node demoted to Lobe — stopping cognitive workers")
			if cogCancel != nil {
				cogCancel()
				cogCancel = nil
			}
			if cogHeb != nil {
				cogHeb.Stop()
				cogHeb = nil
			}
			if cogTransition != nil {
				cogTransition.Stop()
				cogTransition = nil
			}
			eng.ClearCognitiveWorkers()
		}
	}

	// Create wrapper for REST that handles the context
	restWrapper := rest.NewEngineWrapper(eng, hnswRegistry)

	// Build plugin registry and register active plugins.
	pluginRegistry := plugin.NewRegistry()
	if embedPlugin != nil {
		if err := pluginRegistry.Register(embedPlugin); err != nil {
			slog.Warn("failed to register embed plugin in registry", "err", err)
		}
	}
	if enrichPlugin != nil {
		if err := pluginRegistry.Register(enrichPlugin); err != nil {
			slog.Warn("failed to register enrich plugin in registry", "err", err)
		}
		eng.SetEnrichPlugin(enrichPlugin)
		if timeoutStr := os.Getenv("MUNINN_ENRICH_TIMEOUT"); timeoutStr != "" {
			if d, err := time.ParseDuration(timeoutStr); err == nil && d > 0 {
				eng.SetReplayEnrichTimeout(d)
				slog.Info("replay enrichment per-engram timeout configured", "timeout", d)
			} else if err != nil {
				slog.Warn("MUNINN_ENRICH_TIMEOUT invalid, ignoring", "value", timeoutStr, "err", err)
			}
		}
		if rew, ok := restWrapper.(*rest.RESTEngineWrapper); ok {
			rew.SetEnricher(enrichPlugin)
		}
		// Wire circuit-breaker state-change hook so transitions emit structured
		// log lines and update the plugin registry health status.
		if es, ok := enrichPlugin.(interface {
			SetBreakerStateChangeHook(interface {
				SetHealthy(name string, healthy bool)
				SetUnhealthy(name string, err error)
			})
		}); ok {
			es.SetBreakerStateChangeHook(pluginRegistry)
		}
	}

	// Build transport servers
	mbpServer := mbp.NewServer(*mbpAddr, eng, authStore, clientTLS)
	corsOrigins := parseCORSOrigins(*corsOriginsFlag)
	enrichInfo := resolveEnrichInfo(savedPluginCfg)
	restServer := rest.NewServer(*restAddr, restWrapper, authStore, sessionSecret, corsOrigins, embedInfo, enrichInfo, pluginRegistry, *dataDir, clientTLS, rest.MCPInfo{
		Addr:     *mcpAddr,
		HasToken: *mcpToken != "",
	})
	restServer.SetVersion(muninnVersion())

	// Shared plugin store — bridges raw storage to the plugin.PluginStore interface.
	// Created here so it can be shared by the MCP adapter (RetryEnrich) and both
	// retroactive processors (embed + enrich) which are started below.
	pStore := plugin.NewStoreAdapter(store, hnswRegistry)

	// Build MCP server
	mcpAdapter := mcp.NewEngineAdapter(eng, enrichPlugin, pStore)
	mcpServer := mcp.New(*mcpAddr, mcpAdapter, *mcpToken, clientTLS)

	// Build gRPC server
	grpcAdapter := grpcpkg.NewEngineAdapter(eng)
	grpcServer := grpcpkg.NewServer(*grpcAddr, grpcAdapter, authStore, clientTLS)

	// Signal handling
	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-sigCh
		slog.Info("shutdown signal received — starting graceful shutdown")
		cancel()
		<-sigCh
		slog.Error("second signal received — forcing immediate exit")
		os.Exit(1)
	}()

	// Start Prometheus metrics server (if configured).
	// Register per-vault engram count collector and start the HTTP listener.
	if *metricsAddr != "" {
		prometheus.MustRegister(metrics.NewVaultEngramCollector(store))
		metrics.Serve(ctx, *metricsAddr)
		slog.Info("metrics server starting", "addr", *metricsAddr)
	}

	// startCoordinator starts the TCP listener and coordinator.Run goroutines
	// for the given coordinator. Captures server-lifetime ctx from outer scope.
	startCoordinator := func(coord *replication.ClusterCoordinator, bindAddr string) {
		go func() {
			ln, err := net.Listen("tcp", bindAddr)
			if err != nil {
				log.Printf("[cluster] failed to listen on %s: %v", bindAddr, err)
				return
			}
			defer ln.Close()
			slog.Info("[cluster] TCP listener started", "addr", bindAddr)
			for {
				conn, err := ln.Accept()
				if err != nil {
					select {
					case <-ctx.Done():
						return
					default:
						log.Printf("[cluster] accept error: %v", err)
						time.Sleep(10 * time.Millisecond)
						continue
					}
				}
				go handleClusterConn(conn, coord)
			}
		}()
		go func() {
			if err := coord.Run(ctx); err != nil && err != context.Canceled {
				slog.Error("[cluster] coordinator exited", "err", err)
			}
		}()
	}

	// Start cluster coordinator when enabled at startup.
	if coordinator != nil {
		startCoordinator(coordinator, clusterCfg.BindAddr)
	}

	// Wire coordinator to REST server so admin cluster endpoints work.
	if coordinator != nil {
		restServer.SetCoordinator(coordinator)
	}

	// Wire coordinator factory so the admin enable endpoint can start cluster
	// at runtime (without a restart) when cluster.yaml is written via the UI/CLI.
	restServer.SetCoordinatorFactory(func(_ context.Context, cfg plugincfg.ClusterConfig) (*replication.ClusterCoordinator, error) {
		repLog := replication.NewReplicationLog(db)
		applier := replication.NewApplier(db)
		epochStore, err := replication.NewEpochStore(db)
		if err != nil {
			return nil, fmt.Errorf("create epoch store: %w", err)
		}
		coord := replication.NewClusterCoordinator(&cfg, repLog, applier, epochStore)
		coord.OnBecameCortex = func(epoch uint64) {
			log.Printf("[cluster] node promoted to Cortex at epoch %d", epoch)
		}
		coord.OnBecameLobe = func() {
			log.Printf("[cluster] node demoted to Lobe")
		}
		startCoordinator(coord, cfg.BindAddr)
		return coord, nil
	})

	// Start GroupCommitter
	go gc.Run(ctx)

	// Start trigger system event loop (must start before engines begin writing).
	trigSystem.Start(ctx)

	// Start cognitive workers.
	// HebbianWorker auto-starts its own goroutine in NewHebbianWorker; do NOT call Run again.
	go contradictWorkerImpl.Worker.Run(ctx)
	go confidenceWorkerImpl.Worker.Run(ctx)

	// Start retroactive embed processor if a real embedder is configured.
	// It runs continuously, picking up newly written engrams via Notify() or its poll ticker.
	var retroProcessor *plugin.RetroactiveProcessor
	if embedPlugin != nil {
		retroProcessor = plugin.NewRetroactiveProcessor(pStore, embedPlugin, plugin.DigestEmbed)
		retroProcessor.Start(ctx)
		slog.Info("retroactive embed processor started")
	}

	// Start retroactive enrich processor if an enrich plugin is configured.
	// Without this processor, auto-enrichment never fires (issue #113 bug 1).
	var enrichProcessor *plugin.RetroactiveProcessor
	if enrichPlugin != nil {
		enrichProcessor = plugin.NewRetroactiveProcessor(pStore, enrichPlugin, plugin.DigestEnrich)
		enrichProcessor.Start(ctx)
		slog.Info("retroactive enrich processor started")
	}

	// Wire engine → processors: chain Notify calls so every Write wakes all active workers.
	switch {
	case retroProcessor != nil && enrichProcessor != nil:
		embedNotify := retroProcessor.Notify
		enrichNotify := enrichProcessor.Notify
		eng.SetOnWrite(func() { embedNotify(); enrichNotify() })
	case retroProcessor != nil:
		eng.SetOnWrite(retroProcessor.Notify)
	case enrichProcessor != nil:
		eng.SetOnWrite(enrichProcessor.Notify)
	}

	// Wire processors into engine for observability stats.
	var obsProcs []*plugin.RetroactiveProcessor
	if retroProcessor != nil {
		obsProcs = append(obsProcs, retroProcessor)
	}
	if enrichProcessor != nil {
		obsProcs = append(obsProcs, enrichProcessor)
	}
	eng.SetRetroactiveProcessors(obsProcs...)

	// Start servers
	errCh := make(chan error, 3)

	go func() {
		slog.Info("MBP server starting", "addr", *mbpAddr)
		if err := mbpServer.Serve(ctx); err != nil {
			errCh <- err
		}
	}()

	go func() {
		slog.Info("REST server starting", "addr", *restAddr)
		if err := restServer.Serve(ctx); err != nil {
			errCh <- err
		}
	}()

	go func() {
		slog.Info("gRPC server starting", "addr", *grpcAddr)
		if err := grpcServer.Serve(ctx); err != nil {
			slog.Error("gRPC server error", "err", err)
		}
	}()

	go func() {
		slog.Info("mcp listening", "addr", *mcpAddr)
		if err := mcpServer.Serve(); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
	}()

	// Start UI server
	uiSrv, err := ui.NewServer(webFS, restWrapper, restServer.Handler(), authStore, sessionSecret, ring, clientTLS, corsOrigins)
	if err != nil {
		slog.Error("create ui server", "err", err)
		os.Exit(1)
	}
	// Wire broadcast callback now that uiSrv is available.
	ring.SetOnAdd(func(e logging.LogEntry) {
		data, _ := json.Marshal(map[string]any{
			"type":  "log_entry",
			"level": e.Level,
			"time":  e.Time.Format(time.RFC3339),
			"msg":   e.Msg,
			"attrs": e.Attrs,
		})
		uiSrv.Broadcast(data)
	})
	if err := uiSrv.Start(ctx, *uiAddr); err != nil {
		slog.Error("start ui server", "err", err)
		os.Exit(1)
	}
	slog.Info("UI server listening", "addr", *uiAddr)

	slog.Info("vault fail-closed: unconfigured vaults require an API key; use muninn api-key create to grant access")

	// Upgrade notice: warn operators if data exists but no vault configs are set.
	// This detects the scenario where an operator upgraded from a version that
	// defaulted vaults to public, and now all vaults are locked (fail-closed).
	if authStore.AdminExists() {
		cfgs, err := authStore.ListVaultConfigs()
		if err == nil && len(cfgs) == 0 {
			fmt.Fprint(os.Stderr, vaultUpgradeWarning)
		}
	}

	slog.Info("MuninnDB started")

	// Start automated backup scheduler if both interval and directory are configured.
	if backupIntervalDur > 0 && *backupDir != "" {
		sched := backup.New(backup.Config{
			Interval:  backupIntervalDur,
			BackupDir: *backupDir,
			Retain:    *backupRetain,
			DataDir:   *dataDir,
		}, eng)
		sched.Start(ctx)
		slog.Info("backup scheduler started",
			"interval", backupIntervalDur,
			"dir", *backupDir,
			"retain", *backupRetain,
		)
	}

	select {
	case <-ctx.Done():
	case err := <-errCh:
		slog.Error("server error", "err", err)
		cancel()
	}

	slog.Info("shutting down")
	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		if retroProcessor != nil {
			retroProcessor.Stop()
		}
		if enrichProcessor != nil {
			enrichProcessor.Stop()
		}
		if enrichPlugin != nil {
			if closer, ok := enrichPlugin.(interface{ Close() error }); ok {
				_ = closer.Close()
			}
		}
		netShutCtx, netShutCancel := context.WithTimeout(context.Background(), 25*time.Second)
		defer netShutCancel()
		mbpServer.Shutdown(netShutCtx)
		restServer.Shutdown(netShutCtx)
		if err := grpcServer.Shutdown(netShutCtx); err != nil {
			slog.Error("gRPC shutdown error", "err", err)
		}
		if err := mcpServer.Shutdown(netShutCtx); err != nil {
			slog.Error("mcp shutdown error", "err", err)
		}
		if err := uiSrv.Stop(netShutCtx); err != nil {
			slog.Error("ui server shutdown error", "err", err)
		}
		// Stop cluster coordinator before closing the DB (coordinator holds DB references).
		if coordinator != nil {
			if err := coordinator.Stop(); err != nil {
				slog.Error("[cluster] coordinator stop error", "err", err)
			}
		}
		// Stop cognitive workers: eng.Stop() flushes the coherence counters (final flush) and
		// stops the autoAssoc worker. HebbianWorker must be stopped AFTER eng.Stop() so any
		// buffered Hebbian writes enqueued by the engine are not lost.
		//
		// contradictWorkerImpl and confidenceWorkerImpl are Worker[T] types started
		// with `go worker.Run(ctx)`. They exit when ctx is cancelled (signal handler
		// or errCh path above). No explicit Stop() is needed — the 30s shutdown
		// timeout below provides the hard deadline if they stall.
		eng.Stop()
		hebbianWorkerImpl.Stop()
		transitionWorkerImpl.Stop()
		// Close the storage layer (flushes TransitionCache, counter flush,
		// provenance worker, WAL sync, and then closes Pebble). Must happen
		// after cognitive workers have stopped writing, but before the
		// GroupCommitter is torn down.
		if err := store.Close(); err != nil {
			slog.Error("store close error", "err", err)
		}
		gc.Stop()
	}()
	select {
	case <-shutdownDone:
		slog.Info("shutdown complete")
	case <-time.After(30 * time.Second):
		slog.Error("shutdown timed out after 30s; forcing exit")
		os.Exit(1)
	}
}
