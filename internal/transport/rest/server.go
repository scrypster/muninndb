package rest

import (
	"context"
	"crypto/subtle"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	lru "github.com/hashicorp/golang-lru/v2"
	"github.com/scrypster/muninndb/internal/auth"
	"github.com/scrypster/muninndb/internal/config"
	"github.com/scrypster/muninndb/internal/engine"
	"github.com/scrypster/muninndb/internal/engine/trigger"
	"github.com/scrypster/muninndb/internal/metrics"
	"github.com/scrypster/muninndb/internal/plugin"
	"github.com/scrypster/muninndb/internal/replication"
	mbp "github.com/scrypster/muninndb/internal/transport/mbp"
	"golang.org/x/time/rate"
)

// ctxKeyRequestID is the typed context key used to propagate the request ID
// through the middleware chain to sendError.
type ctxKeyRequestID struct{}

// statusRecorder wraps http.ResponseWriter to capture the HTTP status code
// written by downstream handlers for use in metrics instrumentation.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// Server is an HTTP REST server for the MuninnDB engine.
type Server struct {
	addr          string
	engine        EngineAPI
	authStore     *auth.Store
	sessionSecret []byte   // for admin session validation
	corsOrigins   []string // allowed CORS origins; nil = no cross-origin allowed
	mux           *http.ServeMux
	server        *http.Server
	tlsConfig     *tls.Config // nil = plain TCP

	// Embedder info — set at construction time, static for the lifetime of the server.
	embedProvider string // "ollama", "openai", "voyage", or "none"
	embedModel    string // model name, or "" if none

	// Enrichment info — set at construction time, static for the lifetime of the server.
	enrichProvider string // "ollama", "openai", "anthropic", or ""
	enrichModel    string // model name, or ""

	// MCP info — set at construction time for the /api/admin/mcp-info endpoint.
	mcpAddr     string // MCP listen address, e.g. ":8750"
	mcpHasToken bool   // whether a bearer token is configured

	pluginRegistry *plugin.Registry

	// dataDir is the server's data directory, used for reading/writing plugin_config.json.
	dataDir string

	// coordinatorFactory, when set, is called by enableClusterRuntime to create and
	// start a coordinator from a persisted ClusterConfig. If nil, config is persisted
	// but the coordinator is not started until the next process restart.
	coordinatorFactory func(ctx context.Context, cfg config.ClusterConfig) (*replication.ClusterCoordinator, error)

	// coordinator is the optional cluster coordinator; nil when cluster is disabled.
	coordinator *replication.ClusterCoordinator

	// Health check fields.
	startTime       time.Time
	version         string // set at construction time; empty falls back to "dev"
	dbWritable      atomic.Bool
	subsystemsReady atomic.Bool

	shutdown  chan struct{}
	ready     chan struct{} // closed by Serve after wg.Add(1); guards against Shutdown racing wg.Wait
	wg        sync.WaitGroup
	shutdownM sync.Mutex
}

// EmbedInfo carries static embedder metadata set at server construction time.
type EmbedInfo struct {
	Provider string // "ollama", "openai", "voyage", or "none"
	Model    string // model name, or ""
}

// EnrichInfo carries static enrichment metadata set at server construction time.
type EnrichInfo struct {
	Provider string // "ollama", "openai", "anthropic", or ""
	Model    string // model name, or ""
}

// MCPInfo carries static MCP server metadata set at server construction time.
type MCPInfo struct {
	Addr     string // MCP listen address, e.g. ":8750"
	HasToken bool   // whether a bearer token is configured
}

// NewServer creates a new REST server.
//
// sessionSecret is used to validate admin session cookies on /api/admin/* routes.
// corsOrigins is the set of allowed CORS origins; nil disables cross-origin access.
func NewServer(addr string, engine EngineAPI, authStore *auth.Store, sessionSecret []byte, corsOrigins []string, embedInfo EmbedInfo, enrichInfo EnrichInfo, pluginRegistry *plugin.Registry, dataDir string, tlsConfig *tls.Config, mcpInfo ...MCPInfo) *Server {
	mux := http.NewServeMux()
	s := &Server{
		addr:           addr,
		engine:         engine,
		authStore:      authStore,
		sessionSecret:  sessionSecret,
		corsOrigins:    corsOrigins,
		mux:            mux,
		embedProvider:  embedInfo.Provider,
		embedModel:     embedInfo.Model,
		enrichProvider: enrichInfo.Provider,
		enrichModel:    enrichInfo.Model,
		pluginRegistry: pluginRegistry,
		dataDir:        dataDir,
		tlsConfig:      tlsConfig,
		startTime:      time.Now(),
		shutdown:       make(chan struct{}),
		ready:          make(chan struct{}),
	}
	// Subsystems are considered ready immediately unless explicitly marked otherwise.
	s.subsystemsReady.Store(true)
	if len(mcpInfo) > 0 {
		s.mcpAddr = mcpInfo[0].Addr
		s.mcpHasToken = mcpInfo[0].HasToken
	}
	// Start background DB writability probe (non-blocking, avoids probing on every request).
	go s.probeDBWritability()

	// Replication routes — cluster auth required when cluster is active.
	mux.HandleFunc("GET /v1/replication/status", s.withClusterAuthMiddleware(s.withPublicMiddleware(s.handleReplicationStatus)))
	mux.HandleFunc("GET /v1/replication/lag", s.withClusterAuthMiddleware(s.withPublicMiddleware(s.handleReplicationLag)))
	mux.HandleFunc("POST /v1/replication/promote", s.withClusterAuthMiddleware(s.withPublicMiddleware(s.handleReplicationPromote)))

	// Cluster routes — cluster auth required when cluster is active.
	mux.HandleFunc("GET /v1/cluster/info", s.withClusterAuthMiddleware(s.withPublicMiddleware(s.handleClusterInfo)))
	mux.HandleFunc("GET /v1/cluster/health", s.withClusterAuthMiddleware(s.withPublicMiddleware(s.handleClusterHealth)))
	mux.HandleFunc("GET /v1/cluster/nodes", s.withClusterAuthMiddleware(s.withPublicMiddleware(s.handleClusterNodes)))
	mux.HandleFunc("GET /v1/cluster/cognitive/consistency", s.withClusterAuthMiddleware(s.withPublicMiddleware(s.handleCognitiveConsistency)))

	// Public routes — no auth, no body size limit (health/auth handshake).
	mux.HandleFunc("GET /api/openapi.yaml", s.withPublicMiddleware(s.handleOpenAPISpec))
	mux.HandleFunc("POST /api/hello", s.withPublicMiddleware(s.handleHello))
	mux.HandleFunc("GET /api/health", s.withPublicMiddleware(s.handleHealth))
	mux.HandleFunc("GET /api/ready", s.withPublicMiddleware(s.handleReady))
	mux.HandleFunc("GET /api/workers", s.withPublicMiddleware(s.handleWorkerStats))

	// Authenticated vault routes — require Bearer API key.
	mux.HandleFunc("POST /api/engrams/batch", s.withMiddleware(s.handleBatchCreate))
	mux.HandleFunc("POST /api/engrams", s.withMiddleware(s.handleCreateEngram))
	mux.HandleFunc("GET /api/engrams/{id}", s.withMiddleware(s.handleGetEngram))
	mux.HandleFunc("DELETE /api/engrams/{id}", s.withMiddleware(s.handleDeleteEngram))
	mux.HandleFunc("POST /api/activate", s.withMiddleware(s.handleActivate))
	mux.HandleFunc("POST /api/link", s.withMiddleware(s.handleLink))
	mux.HandleFunc("GET /api/stats", s.withMiddleware(s.handleStats))
	mux.HandleFunc("GET /api/engrams", s.withMiddleware(s.handleListEngrams))
	mux.HandleFunc("GET /api/engrams/{id}/links", s.withMiddleware(s.handleGetEngramLinks))
	mux.HandleFunc("POST /api/engrams/links/batch", s.withMiddleware(s.handleBatchGetEngramLinks))
	mux.HandleFunc("GET /api/vaults", s.withMiddleware(s.handleListVaults))
	mux.HandleFunc("GET /api/vaults/stats", s.withAdminMiddleware(s.handleVaultStats()))
	mux.HandleFunc("GET /api/session", s.withMiddleware(s.handleGetSession))
	// SSE subscribe — long-lived; bypasses write timeout via ResponseController.
	mux.HandleFunc("GET /api/subscribe", s.withMiddleware(s.handleSubscribe))

	// Extended vault routes — operations that were previously MCP-only.
	mux.HandleFunc("POST /api/engrams/{id}/evolve", s.withMiddleware(s.handleEvolve))
	mux.HandleFunc("POST /api/consolidate", s.withMiddleware(s.handleConsolidateEngrams))
	mux.HandleFunc("POST /api/decide", s.withMiddleware(s.handleDecide))
	mux.HandleFunc("POST /api/engrams/{id}/restore", s.withMiddleware(s.handleRestore))
	mux.HandleFunc("POST /api/traverse", s.withMiddleware(s.handleTraverse))
	mux.HandleFunc("POST /api/explain", s.withMiddleware(s.handleExplain))
	mux.HandleFunc("PUT /api/engrams/{id}/state", s.withMiddleware(s.handleSetState))
	mux.HandleFunc("PUT /api/engrams/{id}/tags", s.withMiddleware(s.handleUpdateTags))
	mux.HandleFunc("GET /api/deleted", s.withMiddleware(s.handleListDeleted))
	mux.HandleFunc("POST /api/engrams/{id}/retry-enrich", s.withMiddleware(s.handleRetryEnrich))
	mux.HandleFunc("GET /api/contradictions", s.withMiddleware(s.handleContradictions))
	mux.HandleFunc("GET /api/guide", s.withMiddleware(s.handleGuide))

	// Admin routes — require valid admin session cookie, return JSON 401 on failure.
	mux.HandleFunc("POST /api/admin/keys", s.withAdminMiddleware(s.handleCreateAPIKey(authStore)))
	mux.HandleFunc("GET /api/admin/keys", s.withAdminMiddleware(s.handleListAPIKeys(authStore)))
	mux.HandleFunc("DELETE /api/admin/keys/{id}", s.withAdminMiddleware(s.handleRevokeAPIKey(authStore)))
	mux.HandleFunc("PUT /api/admin/vaults/config", s.withAdminMiddleware(s.handleSetVaultConfig(authStore)))
	mux.HandleFunc("PUT /api/admin/password", s.withAdminMiddleware(s.handleChangeAdminPassword(authStore)))
	mux.HandleFunc("GET /api/admin/embed/status", s.withAdminMiddleware(s.handleEmbedStatus))
	mux.HandleFunc("GET /api/admin/mcp-info", s.withAdminMiddleware(s.handleMCPInfo))
	mux.HandleFunc("GET /api/admin/plugins", s.withAdminMiddleware(s.handlePlugins))
	mux.HandleFunc("GET /api/admin/vault/{name}/plasticity", s.withAdminMiddleware(s.handleGetVaultPlasticity(authStore)))
	mux.HandleFunc("PUT /api/admin/vault/{name}/plasticity", s.withAdminMiddleware(s.handlePutVaultPlasticity(authStore)))
	mux.HandleFunc("GET /api/admin/plugin-config", s.withAdminMiddleware(s.handleGetPluginConfig))
	mux.HandleFunc("PUT /api/admin/plugin-config", s.withAdminMiddleware(s.handlePutPluginConfig))
	mux.HandleFunc("DELETE /api/admin/vaults/{name}", s.withAdminMiddleware(s.handleDeleteVault))
	mux.HandleFunc("POST /api/admin/vaults/{name}/clear", s.withAdminMiddleware(s.handleClearVault))
	mux.HandleFunc("POST /api/admin/vaults/{name}/clone", s.withAdminMiddleware(s.handleCloneVault))
	mux.HandleFunc("POST /api/admin/vaults/{name}/merge-into", s.withAdminMiddleware(s.handleMergeVault))
	mux.HandleFunc("GET /api/admin/vaults/{name}/job-status", s.withAdminMiddleware(s.handleVaultJobStatus))
	mux.HandleFunc("GET /api/admin/vaults/{name}/export", s.withAdminMiddleware(s.handleExportVault))
	mux.HandleFunc("GET /api/admin/vaults/{name}/export-markdown", s.withAdminMiddleware(s.handleExportVaultMarkdown))
	mux.HandleFunc("POST /api/admin/vaults/import", s.withAdminMiddleware(s.withLargeBody(s.handleImportVault)))
	mux.HandleFunc("POST /api/admin/vaults/{name}/reindex-fts", s.withAdminMiddleware(s.handleReindexFTSVault))
	mux.HandleFunc("POST /api/admin/vaults/{name}/reembed", s.withAdminMiddleware(s.handleReembedVault))
	mux.HandleFunc("POST /api/admin/vaults/{name}/rename", s.withAdminMiddleware(s.handleRenameVault))
	mux.HandleFunc("POST /api/admin/backup", s.withAdminMiddleware(s.handleBackup))
	mux.HandleFunc("GET /api/admin/observability", s.withAdminMiddleware(s.handleObservability))
	mux.HandleFunc("POST /api/admin/contradictions/resolve", s.withAdminMiddleware(s.handleResolveContradiction))

	// Cluster management — session auth required
	mux.HandleFunc("GET /api/admin/cluster/token", s.withAdminMiddleware(s.handleAdminClusterToken))
	mux.HandleFunc("POST /api/admin/cluster/token/regenerate", s.withAdminMiddleware(s.handleAdminClusterRegenerateToken))
	mux.HandleFunc("POST /api/admin/cluster/enable", s.withAdminMiddleware(s.handleAdminClusterEnable))
	mux.HandleFunc("POST /api/admin/cluster/disable", s.withAdminMiddleware(s.handleAdminClusterDisable))
	mux.HandleFunc("POST /api/admin/cluster/nodes", s.withAdminMiddleware(s.handleAdminClusterAddNode))
	mux.HandleFunc("DELETE /api/admin/cluster/nodes/{id}", s.withAdminMiddleware(s.handleAdminClusterRemoveNode))
	mux.HandleFunc("POST /api/admin/cluster/failover", s.withAdminMiddleware(s.handleAdminClusterFailover))
	mux.HandleFunc("POST /api/admin/cluster/tls/rotate", s.withAdminMiddleware(s.handleAdminClusterRotateTLS))
	mux.HandleFunc("PUT /api/admin/cluster/settings", s.withAdminMiddleware(s.handleAdminClusterSettings))
	mux.HandleFunc("POST /api/admin/cluster/nodes/test", s.withAdminMiddleware(s.handleAdminClusterTestNode))
	mux.HandleFunc("GET /api/admin/cluster/events", s.withAdminMiddleware(s.handleAdminClusterEvents))

	// Build the global and per-IP rate limiters from env vars with fallback defaults.
	globalRPS := envIntDefault("MUNINN_RATE_LIMIT_GLOBAL_RPS", 1000)
	perIPRPS := envIntDefault("MUNINN_RATE_LIMIT_PER_IP_RPS", 100)
	globalLimiter := rate.NewLimiter(rate.Limit(globalRPS), globalRPS*2)
	ipCache, _ := lru.New[string, *rate.Limiter](50_000)

	s.server = &http.Server{
		Addr:           addr,
		Handler:        newRateLimitMiddleware(globalLimiter, ipCache, perIPRPS)(s.corsMiddleware(mux)),
		ReadTimeout:    15 * time.Second,
		WriteTimeout:   15 * time.Second,
		IdleTimeout:    60 * time.Second,
		MaxHeaderBytes: 1 << 16, // 64 KB
	}

	return s
}

// Handler returns the HTTP handler for the REST API, so it can be mounted on another mux.
func (s *Server) Handler() http.Handler {
	return s.server.Handler
}

// SetCoordinator wires in the cluster coordinator. Pass nil to disable cluster endpoints.
// Must be called before Serve.
func (s *Server) SetCoordinator(coord *replication.ClusterCoordinator) {
	s.coordinator = coord
}

// DisableCluster clears the active coordinator.
// Safe to call when coordinator is nil (no-op).
func (s *Server) DisableCluster() {
	s.coordinator = nil
}

// ActiveCoordinator returns the current coordinator, or nil if cluster mode is off.
func (s *Server) ActiveCoordinator() *replication.ClusterCoordinator {
	return s.coordinator
}

// SetDataDir sets the data directory used for persisting cluster configuration.
func (s *Server) SetDataDir(dir string) { s.dataDir = dir }

// SetVersion sets the version string reported by the health endpoint.
func (s *Server) SetVersion(v string) { s.version = v }

// probeDBWritability runs a periodic background check of data directory writability.
// It writes and deletes a small sentinel file every 30 seconds rather than on every
// health request, so load-balancer probes (fired 2-10x/sec) never touch the disk.
func (s *Server) probeDBWritability() {
	probe := func() {
		if s.dataDir == "" {
			s.dbWritable.Store(true) // no data dir configured; treat as writable
			return
		}
		testFile := s.dataDir + "/.health_probe"
		err := os.WriteFile(testFile, []byte("1"), 0600)
		if err == nil {
			os.Remove(testFile)
		}
		s.dbWritable.Store(err == nil)
	}
	probe() // run immediately on startup
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			probe()
		case <-s.shutdown:
			return
		}
	}
}

// SetCoordinatorFactory wires in a factory function that creates and starts a
// ClusterCoordinator from a ClusterConfig. Must be called before Serve.
func (s *Server) SetCoordinatorFactory(f func(context.Context, config.ClusterConfig) (*replication.ClusterCoordinator, error)) {
	s.coordinatorFactory = f
}

// enableClusterRuntime persists the config and, if a coordinatorFactory is wired,
// starts the coordinator. For Phase 1, if no factory is present, config is persisted
// and the server reports success (coordinator starts on next restart).
func (s *Server) enableClusterRuntime(ctx context.Context, cfg config.ClusterConfig) error {
	if s.dataDir != "" {
		if err := config.SaveClusterConfig(s.dataDir, cfg); err != nil {
			return fmt.Errorf("persist config: %w", err)
		}
	}
	if s.coordinatorFactory != nil {
		coord, err := s.coordinatorFactory(ctx, cfg)
		if err != nil {
			return err
		}
		s.SetCoordinator(coord)
	}
	return nil
}

// persistClusterDisabled writes Enabled=false to the cluster config file.
func (s *Server) persistClusterDisabled() error {
	if s.dataDir == "" {
		return nil
	}
	existing, err := config.LoadClusterConfig(s.dataDir)
	if err != nil {
		existing = config.ClusterConfig{}
	}
	existing.Enabled = false
	return config.SaveClusterConfig(s.dataDir, existing)
}

// applyAndPersistSettings merges settings into the cluster config file.
func (s *Server) applyAndPersistSettings(req clusterSettingsRequest) error {
	if s.dataDir == "" {
		return nil
	}
	cfg, err := config.LoadClusterConfig(s.dataDir)
	if err != nil {
		return err
	}
	if req.HeartbeatMS != nil {
		cfg.HeartbeatMS = *req.HeartbeatMS
	}
	return config.SaveClusterConfig(s.dataDir, cfg)
}

// Serve starts listening and blocks until context is cancelled or Shutdown is called.
func (s *Server) Serve(ctx context.Context) error {
	listener, err := net.Listen("tcp", s.addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", s.addr, err)
	}
	if s.tlsConfig != nil {
		listener = tls.NewListener(listener, s.tlsConfig)
		slog.Info("rest: TLS enabled", "addr", listener.Addr().String())
	}
	slog.Info("rest: listening", "addr", listener.Addr().String())

	s.wg.Add(1)
	close(s.ready) // signal that wg.Add(1) has run; Shutdown may now call wg.Wait safely
	go func() {
		defer s.wg.Done()
		if err := s.server.Serve(listener); err != nil && err != http.ErrServerClosed {
			slog.Error("server error", "error", err)
		}
	}()

	// Wait for shutdown signal
	<-s.shutdown

	// Graceful shutdown
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s.server.Shutdown(shutdownCtx)
	s.wg.Wait()

	return nil
}

// Shutdown gracefully stops the server.
func (s *Server) Shutdown(ctx context.Context) error {
	s.shutdownM.Lock()
	defer s.shutdownM.Unlock()

	select {
	case <-s.shutdown:
		return nil // Already shut down
	default:
	}

	close(s.shutdown)

	// Wait for Serve to have called wg.Add(1) before we call wg.Wait; without this,
	// a Shutdown that races with Serve startup would see a zero-count wg and return
	// before the goroutine is even launched (DATA RACE on wg internals).
	select {
	case <-s.ready:
	case <-ctx.Done():
		return ctx.Err()
	}

	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Middleware

// withPublicMiddleware applies observability middleware + a 64 KB body size limit
// (no auth). Use for health checks, readiness probes, and the HELLO handshake.
// MaxBytesReader is a no-op on GET requests (no body), so it is safe to apply
// universally across all public routes.
func (s *Server) withPublicMiddleware(handler http.HandlerFunc) http.HandlerFunc {
	return s.recoveryMiddleware(s.requestIDMiddleware(s.loggingMiddleware(s.publicBodySizeMiddleware(handler))))
}

// publicBodySizeMiddleware limits request bodies to 64 KB on public (unauthenticated)
// routes. This is smaller than the 4 MB limit used on authenticated routes because
// public endpoints (health, ready, hello) never legitimately need large payloads.
func (s *Server) publicBodySizeMiddleware(next http.HandlerFunc) http.HandlerFunc {
	const maxBody = 64 * 1024 // 64 KB
	return func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, maxBody)
		next(w, r)
	}
}

// withMiddleware applies the full chain: observability + body size limit + vault auth.
// All vault-scoped data routes use this.
// Admin session cookies bypass vault locking so the Web UI can access any vault.
// If authStore is nil (e.g. in tests), vault auth is skipped.
func (s *Server) withMiddleware(handler http.HandlerFunc) http.HandlerFunc {
	if s.authStore == nil {
		return s.withPublicMiddleware(s.bodySizeMiddleware(handler))
	}
	return s.withPublicMiddleware(s.bodySizeMiddleware(s.authStore.VaultAuthWithAdminBypass(s.sessionSecret, handler)))
}

// withAdminMiddleware applies observability + body size limit + admin session auth.
// Returns JSON 401 (not a redirect) on auth failure — suitable for REST API callers.
// If authStore is nil or sessionSecret is empty, admin auth is skipped (e.g. in tests).
func (s *Server) withAdminMiddleware(handler http.HandlerFunc) http.HandlerFunc {
	if s.authStore == nil || len(s.sessionSecret) == 0 {
		return s.withPublicMiddleware(s.bodySizeMiddleware(handler))
	}
	return s.withPublicMiddleware(s.bodySizeMiddleware(s.authStore.AdminAPIMiddleware(s.sessionSecret, handler)))
}

// bodySizeMiddleware limits request bodies to 4 MB to prevent resource exhaustion.
func (s *Server) bodySizeMiddleware(next http.HandlerFunc) http.HandlerFunc {
	const maxBody = 4 << 20 // 4 MB
	return func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, maxBody)
		next(w, r)
	}
}

// withLargeBody replaces the body size limit with 512 MB for bulk import operations.
func (s *Server) withLargeBody(next http.HandlerFunc) http.HandlerFunc {
	const maxBody = 512 << 20 // 512 MB
	return func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, maxBody)
		next(w, r)
	}
}

func (s *Server) recoveryMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if err := recover(); err != nil {
				// Re-panic for ErrAbortHandler so Go's HTTP server can close the
				// connection cleanly. This is used by the export handler to signal
				// a truncated stream when streaming has already begun.
				if err == http.ErrAbortHandler {
					panic(err)
				}
				slog.Error("panic", "error", err, "path", r.URL.Path, "stack", string(debug.Stack()))
				s.sendError(r, w, http.StatusInternalServerError, ErrInternal, "internal server error")
			}
		}()
		next(w, r)
	}
}

func (s *Server) requestIDMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := r.Header.Get("X-Request-ID")
		if requestID == "" {
			requestID = uuid.New().String()
		}
		r.Header.Set("X-Request-ID", requestID)
		ctx := context.WithValue(r.Context(), ctxKeyRequestID{}, requestID)
		next(w, r.WithContext(ctx))
	}
}

func (s *Server) loggingMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()
		next(rec, r)
		duration := time.Since(start).Seconds()
		elapsed := time.Duration(duration * float64(time.Second))

		// Use the route pattern (set by Go 1.22+ ServeMux) to avoid high-cardinality
		// path labels from per-resource IDs.
		path := r.Pattern
		if path == "" {
			path = "unknown"
		}

		statusClass := fmt.Sprintf("%dxx", rec.status/100)
		metrics.RESTRequestDuration.WithLabelValues(r.Method, path, statusClass).Observe(duration)

		slog.Info("request", "method", r.Method, "path", r.URL.Path, "duration_ms", elapsed.Milliseconds())
	}
}

// Handlers

func (s *Server) handleHello(w http.ResponseWriter, r *http.Request) {
	var req HelloRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.sendError(r, w, http.StatusBadRequest, ErrInvalidEngram, "invalid request body")
		return
	}
	resp, err := s.engine.Hello(r.Context(), &req)
	if err != nil {
		s.sendError(r, w, http.StatusUnauthorized, ErrAuthFailed, err.Error())
		return
	}
	s.sendJSON(w, http.StatusOK, resp)
}

func (s *Server) handleCreateEngram(w http.ResponseWriter, r *http.Request) {
	var req WriteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.sendError(r, w, http.StatusBadRequest, ErrInvalidEngram, "invalid request body")
		return
	}
	if req.Vault == "" {
		req.Vault = ctxVault(r)
	}
	resp, err := s.engine.Write(r.Context(), &req)
	if err != nil {
		s.sendError(r, w, http.StatusInternalServerError, ErrStorageError, err.Error())
		return
	}
	s.sendJSON(w, http.StatusCreated, resp)
}

func (s *Server) handleBatchCreate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Engrams []WriteRequest `json:"engrams"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		s.sendError(r, w, http.StatusBadRequest, ErrInvalidEngram, "invalid request body")
		return
	}
	if len(body.Engrams) == 0 {
		s.sendError(r, w, http.StatusBadRequest, ErrInvalidEngram, "'engrams' array is required and must not be empty")
		return
	}
	if len(body.Engrams) > 50 {
		s.sendError(r, w, http.StatusBadRequest, ErrInvalidEngram, "'engrams' exceeds maximum batch size of 50")
		return
	}

	reqs := make([]*WriteRequest, len(body.Engrams))
	for i := range body.Engrams {
		if body.Engrams[i].Vault == "" {
			body.Engrams[i].Vault = ctxVault(r)
		}
		reqs[i] = &body.Engrams[i]
	}

	responses, errs := s.engine.WriteBatch(r.Context(), reqs)

	type batchItemResult struct {
		Index  int    `json:"index"`
		ID     string `json:"id,omitempty"`
		Status string `json:"status"`
		Error  string `json:"error,omitempty"`
	}
	results := make([]batchItemResult, len(reqs))
	for i := range reqs {
		if errs[i] != nil {
			results[i] = batchItemResult{Index: i, Status: "error", Error: errs[i].Error()}
		} else {
			results[i] = batchItemResult{Index: i, ID: responses[i].ID, Status: "ok"}
		}
	}

	s.sendJSON(w, http.StatusCreated, map[string]any{"results": results})
}

func (s *Server) handleGetEngram(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		s.sendError(r, w, http.StatusBadRequest, ErrInvalidEngram, "missing engram id")
		return
	}
	resp, err := s.engine.Read(r.Context(), &ReadRequest{ID: id, Vault: ctxVault(r)})
	if err != nil {
		s.sendError(r, w, http.StatusNotFound, ErrEngramNotFound, err.Error())
		return
	}
	s.sendJSON(w, http.StatusOK, resp)
}

func (s *Server) handleDeleteEngram(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		s.sendError(r, w, http.StatusBadRequest, ErrInvalidEngram, "missing engram id")
		return
	}
	resp, err := s.engine.Forget(r.Context(), &ForgetRequest{ID: id, Vault: ctxVault(r)})
	if err != nil {
		s.sendError(r, w, http.StatusInternalServerError, ErrStorageError, err.Error())
		return
	}
	s.sendJSON(w, http.StatusOK, resp)
}

// activateTimeout is read once at startup from MUNINN_ACTIVATE_TIMEOUT (seconds).
// Default: 30s. The context deadline ensures deep BFS traversals cannot run unbounded.
var activateTimeout = func() time.Duration {
	secs := envIntDefault("MUNINN_ACTIVATE_TIMEOUT", 30)
	return time.Duration(secs) * time.Second
}()

func (s *Server) handleActivate(w http.ResponseWriter, r *http.Request) {
	var req ActivateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.sendError(r, w, http.StatusBadRequest, ErrInvalidEngram, "invalid request body")
		return
	}
	if req.Vault == "" {
		req.Vault = ctxVault(r)
	}
	// Apply recall mode preset if provided.
	if req.Mode != "" {
		preset, err := auth.LookupRecallMode(req.Mode)
		if err != nil {
			s.sendError(r, w, http.StatusBadRequest, ErrInvalidEngram, err.Error())
			return
		}
		applyRecallModePreset(&req, preset)
	}
	// Apply a hard activation timeout so deep BFS traversals on large vaults
	// cannot run unbounded. MUNINN_ACTIVATE_TIMEOUT (default 30s) is capped
	// to the outer WriteTimeout so we never wait longer than the HTTP server allows.
	ctx, cancel := context.WithTimeout(r.Context(), activateTimeout)
	defer cancel()
	resp, err := s.engine.Activate(ctx, &req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			s.sendError(r, w, http.StatusGatewayTimeout, ErrIndexError, "activation timeout: query took too long")
			return
		}
		s.sendError(r, w, http.StatusInternalServerError, ErrIndexError, err.Error())
		return
	}
	s.sendJSON(w, http.StatusOK, resp)
}

// applyRecallModePreset applies non-zero recall mode preset fields to an ActivateRequest,
// only when the caller has not already set the corresponding field.
func applyRecallModePreset(req *ActivateRequest, preset auth.RecallModePreset) {
	if preset.Threshold > 0 && req.Threshold == 0 {
		req.Threshold = preset.Threshold
	}
	if preset.MaxHops > 0 && req.MaxHops == 0 {
		req.MaxHops = preset.MaxHops
	}
	if preset.SemanticSimilarity > 0 || preset.FullTextRelevance > 0 || preset.Recency > 0 || preset.DisableACTR {
		if req.Weights == nil {
			req.Weights = &mbp.Weights{}
		}
		if preset.SemanticSimilarity > 0 && req.Weights.SemanticSimilarity == 0 {
			req.Weights.SemanticSimilarity = preset.SemanticSimilarity
		}
		if preset.FullTextRelevance > 0 && req.Weights.FullTextRelevance == 0 {
			req.Weights.FullTextRelevance = preset.FullTextRelevance
		}
		if preset.Recency > 0 && req.Weights.Recency == 0 {
			req.Weights.Recency = preset.Recency
		}
		if preset.DisableACTR {
			req.Weights.DisableACTR = true
		}
	}
}

func (s *Server) handleLink(w http.ResponseWriter, r *http.Request) {
	var req LinkRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.sendError(r, w, http.StatusBadRequest, ErrInvalidEngram, "invalid request body")
		return
	}
	if req.Vault == "" {
		req.Vault = ctxVault(r)
	}
	mbpReq := &mbp.LinkRequest{
		SourceID: req.SourceID,
		TargetID: req.TargetID,
		RelType:  req.RelType,
		Weight:   req.Weight,
		Vault:    req.Vault,
	}
	resp, err := s.engine.Link(r.Context(), mbpReq)
	if err != nil {
		if errors.Is(err, engine.ErrEngramNotFound) {
			s.sendError(r, w, http.StatusNotFound, ErrEngramNotFound, err.Error())
			return
		}
		if errors.Is(err, engine.ErrEngramSoftDeleted) {
			s.sendError(r, w, http.StatusConflict, ErrInvalidAssociation, err.Error())
			return
		}
		s.sendError(r, w, http.StatusInternalServerError, ErrInvalidAssociation, err.Error())
		return
	}
	s.sendJSON(w, http.StatusOK, resp)
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	resp, err := s.engine.Stat(r.Context(), &StatRequest{
		Vault: ctxVault(r),
	})
	if err != nil {
		s.sendError(r, w, http.StatusInternalServerError, ErrStorageError, err.Error())
		return
	}
	s.sendJSON(w, http.StatusOK, resp)
}

func (s *Server) handleWorkerStats(w http.ResponseWriter, r *http.Request) {
	stats := s.engine.WorkerStats()
	s.sendJSON(w, http.StatusOK, stats)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	ver := s.version
	if ver == "" {
		ver = "dev"
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(HealthResponse{
		Status:        "ok",
		Version:       ver,
		UptimeSeconds: int64(time.Since(s.startTime).Seconds()),
		DBWritable:    s.dbWritable.Load(),
	})
}

func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	if !s.subsystemsReady.Load() {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(map[string]string{"status": "initializing"})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(ReadyResponse{Status: "ready"})
}

// ctxVault returns the vault name resolved by the auth middleware for this request.
// The middleware always sets a non-empty vault in context (defaulting to "default");
// this helper ensures handlers never pass an empty vault name to the engine.
func ctxVault(r *http.Request) string {
	if v, ok := r.Context().Value(auth.ContextVault).(string); ok && v != "" {
		return v
	}
	return "default"
}

// Utility methods

func (s *Server) sendJSON(w http.ResponseWriter, statusCode int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	if err := json.NewEncoder(w).Encode(data); err != nil {
		slog.Error("rest: failed to encode response", "err", err)
	}
}

func (s *Server) sendError(r *http.Request, w http.ResponseWriter, statusCode int, code ErrorCode, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)

	displayMsg := message
	if statusCode >= 500 {
		slog.Error("rest: internal error", "code", code, "message", message, "status", statusCode)
		displayMsg = "an internal error occurred"
	}

	var requestID string
	if r != nil {
		if id, ok := r.Context().Value(ctxKeyRequestID{}).(string); ok {
			requestID = id
		}
	}

	json.NewEncoder(w).Encode(ErrorResponse{
		Error: ErrorDetail{
			Code:      code,
			Message:   displayMsg,
			RequestID: requestID,
		},
	})
}

// corsMiddleware adds CORS headers when the request Origin is in s.corsOrigins.
// If corsOrigins is empty, no cross-origin access is allowed (no ACAO header set).
// OPTIONS preflight always returns 204 so browsers can probe the policy.
func (s *Server) corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" && len(s.corsOrigins) > 0 {
			for _, allowed := range s.corsOrigins {
				if origin == allowed {
					w.Header().Set("Access-Control-Allow-Origin", origin)
					w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
					w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Request-ID, X-Allow-Default")
					w.Header().Set("Vary", "Origin")
					break
				}
			}
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// envIntDefault reads an integer environment variable by key. If the variable is
// unset, empty, non-numeric, or outside [1, 100000], the provided default is
// returned and a warning is logged for invalid (non-empty) values.
func envIntDefault(key string, def int) int {
	s := os.Getenv(key)
	if s == "" {
		return def
	}
	v, err := strconv.Atoi(s)
	if err != nil || v < 1 || v > 100000 {
		slog.Warn("invalid env var value, using default", "key", key, "value", s, "default", def)
		return def
	}
	return v
}

// newRateLimitMiddleware returns an http.Handler middleware that enforces both a
// global token-bucket limiter and a per-IP token-bucket limiter. Requests that
// exceed either limit receive an immediate 429 JSON response and never enter the
// downstream recovery/logging chain.
//
// The client IP is taken from the direct TCP connection (r.RemoteAddr) only;
// proxy headers are not trusted.
func newRateLimitMiddleware(global *rate.Limiter, ipCache *lru.Cache[string, *rate.Limiter], perIPRPS int) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Check global limiter first.
			if !global.Allow() {
				res := global.Reserve()
				delay := res.Delay()
				res.Cancel()
				retryAfter := int(delay.Seconds()) + 1
				w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
				metrics.RateLimitRejections.WithLabelValues("global").Inc()
				writeError(w, http.StatusTooManyRequests, "rate_limit_exceeded", "rate limit exceeded")
				return
			}

			// Resolve the client IP.
			ip := clientIP(r)

			// Look up or create the per-IP limiter.
			limiter, ok := ipCache.Get(ip)
			if !ok {
				limiter = rate.NewLimiter(rate.Limit(perIPRPS), perIPRPS*2)
				ipCache.Add(ip, limiter)
			}

			if !limiter.Allow() {
				res := limiter.Reserve()
				delay := res.Delay()
				res.Cancel()
				retryAfter := int(delay.Seconds()) + 1
				w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
				metrics.RateLimitRejections.WithLabelValues("per_ip").Inc()
				writeError(w, http.StatusTooManyRequests, "rate_limit_exceeded", "rate limit exceeded")
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

// clientIP returns the direct TCP peer IP from r.RemoteAddr with the port
// stripped. Proxy headers (X-Forwarded-For, X-Real-IP) are intentionally
// ignored because they are attacker-controlled without a trusted proxy list.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// clusterAuthWarnOnce ensures the misconfiguration warning is logged only once.
var clusterAuthWarnOnce sync.Once

// withClusterAuth returns a per-handler middleware that gates access to cluster
// and replication routes based on whether the cluster is active and whether a
// shared secret has been configured.
//
// Behaviour:
//   - cluster inactive (coordinator == nil): pass through, no auth required.
//   - cluster active + secret empty: reject all requests with 403; log once.
//   - cluster active + secret non-empty: require "Authorization: Bearer <token>";
//     reject with 401 on mismatch (constant-time compare).
func withClusterAuth(secret string, clusterActive bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !clusterActive {
				// Cluster is disabled; no auth required.
				next.ServeHTTP(w, r)
				return
			}

			if secret == "" {
				// Cluster is active but no secret is configured — this is a
				// misconfiguration. Reject all callers and warn once.
				clusterAuthWarnOnce.Do(func() {
					slog.Warn("rest: cluster is active but no cluster secret is set; all replication/cluster requests will be rejected with 403")
				})
				writeError(w, http.StatusForbidden, "cluster_auth_misconfigured", "cluster auth is not configured")
				return
			}

			// Extract Bearer token.
			authHeader := r.Header.Get("Authorization")
			const prefix = "Bearer "
			if !strings.HasPrefix(authHeader, prefix) {
				writeError(w, http.StatusUnauthorized, "cluster_auth_required", "cluster authorization required")
				return
			}
			token := authHeader[len(prefix):]

			// Constant-time comparison to prevent timing attacks.
			if subtle.ConstantTimeCompare([]byte(token), []byte(secret)) != 1 {
				writeError(w, http.StatusUnauthorized, "cluster_auth_failed", "invalid cluster token")
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

// withClusterAuthMiddleware wraps a handler with cluster auth using the server's
// current coordinator state. The coordinator state is evaluated per-request so
// that routes added at construction time respect coordinator changes made later
// via SetCoordinator / DisableCluster.
func (s *Server) withClusterAuthMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		clusterActive := s.coordinator != nil
		var secret string
		if clusterActive {
			secret = s.coordinator.ClusterSecret()
		}
		withClusterAuth(secret, clusterActive)(next).ServeHTTP(w, r)
	}
}

func (s *Server) handleListEngrams(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	vault := ctxVault(r)

	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	offset, _ := strconv.Atoi(q.Get("offset"))
	if offset < 0 {
		offset = 0
	}

	sortBy := q.Get("sort") // "created" or "accessed"

	var tags []string
	if raw := q.Get("tags"); raw != "" {
		for _, t := range strings.Split(raw, ",") {
			if t = strings.TrimSpace(t); t != "" {
				tags = append(tags, t)
			}
		}
	}

	state := q.Get("state")

	var minConf, maxConf float64
	if v := q.Get("min_confidence"); v != "" {
		parsed, err := strconv.ParseFloat(v, 64)
		if err != nil {
			http.Error(w, fmt.Sprintf("invalid min_confidence: %s", v), http.StatusBadRequest)
			return
		}
		minConf = parsed
	}
	if v := q.Get("max_confidence"); v != "" {
		parsed, err := strconv.ParseFloat(v, 64)
		if err != nil {
			http.Error(w, fmt.Sprintf("invalid max_confidence: %s", v), http.StatusBadRequest)
			return
		}
		maxConf = parsed
	}

	since := q.Get("since")
	before := q.Get("before")

	req := &ListEngramsRequest{
		Vault:   vault,
		Limit:   limit,
		Offset:  offset,
		Sort:    sortBy,
		Tags:    tags,
		State:   state,
		MinConf: float32(minConf),
		MaxConf: float32(maxConf),
		Since:   since,
		Before:  before,
	}

	resp, err := s.engine.ListEngrams(r.Context(), req)
	if err != nil {
		s.sendError(r, w, http.StatusInternalServerError, ErrStorageError, err.Error())
		return
	}
	s.sendJSON(w, http.StatusOK, resp)
}

func (s *Server) handleGetEngramLinks(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		s.sendError(r, w, http.StatusBadRequest, ErrInvalidEngram, "missing engram id")
		return
	}
	vault := ctxVault(r)
	resp, err := s.engine.GetEngramLinks(r.Context(), &GetEngramLinksRequest{ID: id, Vault: vault})
	if err != nil {
		s.sendError(r, w, http.StatusInternalServerError, ErrStorageError, err.Error())
		return
	}
	s.sendJSON(w, http.StatusOK, resp)
}

// handleBatchGetEngramLinks returns associations for multiple engrams in one call.
// POST /api/engrams/links/batch
// Body: {"ids": ["ulid1", ...], "vault": "x", "max_per_node": 50}
// Response 200: {"links": {"id1": [{target_id, rel_type, weight, co_activation_count}], ...}}
func (s *Server) handleBatchGetEngramLinks(w http.ResponseWriter, r *http.Request) {
	var req BatchGetEngramLinksRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.sendError(r, w, http.StatusBadRequest, ErrInvalidEngram, "invalid request body")
		return
	}
	if len(req.IDs) == 0 {
		s.sendError(r, w, http.StatusBadRequest, ErrInvalidEngram, "'ids' array is required and must not be empty")
		return
	}
	if len(req.IDs) > 200 {
		s.sendError(r, w, http.StatusBadRequest, ErrInvalidEngram, "'ids' exceeds maximum batch size of 200")
		return
	}
	if req.Vault == "" {
		req.Vault = ctxVault(r)
	}
	resp, err := s.engine.GetBatchEngramLinks(r.Context(), &req)
	if err != nil {
		s.sendError(r, w, http.StatusInternalServerError, ErrStorageError, err.Error())
		return
	}
	s.sendJSON(w, http.StatusOK, resp)
}

func (s *Server) handleListVaults(w http.ResponseWriter, r *http.Request) {
	vaults, err := s.engine.ListVaults(r.Context())
	if err != nil {
		s.sendError(r, w, http.StatusInternalServerError, ErrStorageError, err.Error())
		return
	}

	// Merge in vaults that exist in the auth config but haven't had an engram
	// written yet (or a Hello call pre-fix). This ensures newly created vaults
	// appear in the dropdown immediately after creation.
	if s.authStore != nil {
		cfgs, cfgErr := s.authStore.ListVaultConfigs()
		if cfgErr == nil {
			seen := make(map[string]struct{}, len(vaults))
			for _, v := range vaults {
				seen[v] = struct{}{}
			}
			for _, cfg := range cfgs {
				if cfg.Name != "" {
					if _, ok := seen[cfg.Name]; !ok {
						vaults = append(vaults, cfg.Name)
					}
				}
			}
		}
	}

	s.sendJSON(w, http.StatusOK, vaults)
}

func (s *Server) handleVaultStats() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Get all vaults using the same merge pattern as handleListVaults.
		if _, err := s.engine.ListVaults(r.Context()); err != nil {
			s.sendError(r, w, http.StatusInternalServerError, ErrStorageError, err.Error())
			return
		}
		vaultNames := s.collectVaultNames(r, s.authStore)

		type vaultStat struct {
			Name        string `json:"name"`
			EngramCount int64  `json:"engram_count"`
		}

		result := make([]vaultStat, 0, len(vaultNames))
		for _, name := range vaultNames {
			req := &mbp.StatRequest{Vault: name}
			stat, err := s.engine.Stat(r.Context(), req)
			if err != nil {
				stat = &mbp.StatResponse{}
			}
			result = append(result, vaultStat{Name: name, EngramCount: stat.EngramCount})
		}
		s.sendJSON(w, http.StatusOK, result)
	}
}

func (s *Server) handleGetSession(w http.ResponseWriter, r *http.Request) {
	vault := ctxVault(r)
	sinceStr := r.URL.Query().Get("since")
	since := time.Now().Add(-24 * time.Hour)
	if sinceStr != "" {
		if t, err := time.Parse(time.RFC3339, sinceStr); err == nil {
			since = t
		}
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	if offset < 0 {
		offset = 0
	}
	resp, err := s.engine.GetSession(r.Context(), &GetSessionRequest{Vault: vault, Since: since, Limit: limit, Offset: offset})
	if err != nil {
		s.sendError(r, w, http.StatusInternalServerError, ErrStorageError, err.Error())
		return
	}
	s.sendJSON(w, http.StatusOK, resp)
}

func (s *Server) handleEvolve(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		s.sendError(r, w, http.StatusBadRequest, ErrInvalidEngram, "missing engram id")
		return
	}
	var body struct {
		NewContent string `json:"new_content"`
		Reason     string `json:"reason"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		s.sendError(r, w, http.StatusBadRequest, ErrInvalidEngram, "invalid request body")
		return
	}
	if body.NewContent == "" || body.Reason == "" {
		s.sendError(r, w, http.StatusBadRequest, ErrInvalidEngram, "'new_content' and 'reason' are required")
		return
	}
	resp, err := s.engine.Evolve(r.Context(), ctxVault(r), id, body.NewContent, body.Reason)
	if err != nil {
		s.sendError(r, w, http.StatusInternalServerError, ErrStorageError, err.Error())
		return
	}
	s.sendJSON(w, http.StatusOK, resp)
}

func (s *Server) handleConsolidateEngrams(w http.ResponseWriter, r *http.Request) {
	var body ConsolidateRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		s.sendError(r, w, http.StatusBadRequest, ErrInvalidEngram, "invalid request body")
		return
	}
	if len(body.IDs) < 2 {
		s.sendError(r, w, http.StatusBadRequest, ErrInvalidEngram, "'ids' must contain at least 2 engram IDs")
		return
	}
	if len(body.IDs) > 50 {
		s.sendError(r, w, http.StatusBadRequest, ErrInvalidEngram, "'ids' exceeds maximum of 50")
		return
	}
	if body.MergedContent == "" {
		s.sendError(r, w, http.StatusBadRequest, ErrInvalidEngram, "'merged_content' is required")
		return
	}
	vault := body.Vault
	if vault == "" {
		vault = ctxVault(r)
	}
	resp, err := s.engine.Consolidate(r.Context(), vault, body.IDs, body.MergedContent)
	if err != nil {
		s.sendError(r, w, http.StatusInternalServerError, ErrStorageError, err.Error())
		return
	}
	s.sendJSON(w, http.StatusOK, resp)
}

func (s *Server) handleDecide(w http.ResponseWriter, r *http.Request) {
	var body DecideRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		s.sendError(r, w, http.StatusBadRequest, ErrInvalidEngram, "invalid request body")
		return
	}
	if body.Decision == "" || body.Rationale == "" {
		s.sendError(r, w, http.StatusBadRequest, ErrInvalidEngram, "'decision' and 'rationale' are required")
		return
	}
	vault := body.Vault
	if vault == "" {
		vault = ctxVault(r)
	}
	resp, err := s.engine.Decide(r.Context(), vault, body.Decision, body.Rationale, body.Alternatives, body.EvidenceIDs)
	if err != nil {
		s.sendError(r, w, http.StatusInternalServerError, ErrStorageError, err.Error())
		return
	}
	s.sendJSON(w, http.StatusCreated, resp)
}

func (s *Server) handleRestore(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		s.sendError(r, w, http.StatusBadRequest, ErrInvalidEngram, "missing engram id")
		return
	}
	resp, err := s.engine.Restore(r.Context(), ctxVault(r), id)
	if err != nil {
		s.sendError(r, w, http.StatusInternalServerError, ErrStorageError, err.Error())
		return
	}
	s.sendJSON(w, http.StatusOK, resp)
}

func (s *Server) handleTraverse(w http.ResponseWriter, r *http.Request) {
	var body TraverseRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		s.sendError(r, w, http.StatusBadRequest, ErrInvalidEngram, "invalid request body")
		return
	}
	if body.StartID == "" {
		s.sendError(r, w, http.StatusBadRequest, ErrInvalidEngram, "'start_id' is required")
		return
	}
	if body.MaxHops <= 0 {
		body.MaxHops = 3
	}
	if body.MaxHops > 5 {
		body.MaxHops = 5
	}
	if body.MaxNodes <= 0 {
		body.MaxNodes = 50
	}
	if body.MaxNodes > 100 {
		body.MaxNodes = 100
	}
	vault := body.Vault
	if vault == "" {
		vault = ctxVault(r)
	}
	resp, err := s.engine.Traverse(r.Context(), vault, &body)
	if err != nil {
		s.sendError(r, w, http.StatusInternalServerError, ErrStorageError, err.Error())
		return
	}
	s.sendJSON(w, http.StatusOK, resp)
}

func (s *Server) handleExplain(w http.ResponseWriter, r *http.Request) {
	var body ExplainRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		s.sendError(r, w, http.StatusBadRequest, ErrInvalidEngram, "invalid request body")
		return
	}
	if body.EngramID == "" {
		s.sendError(r, w, http.StatusBadRequest, ErrInvalidEngram, "'engram_id' is required")
		return
	}
	vault := body.Vault
	if vault == "" {
		vault = ctxVault(r)
	}
	resp, err := s.engine.Explain(r.Context(), vault, &body)
	if err != nil {
		s.sendError(r, w, http.StatusInternalServerError, ErrStorageError, err.Error())
		return
	}
	s.sendJSON(w, http.StatusOK, resp)
}

func (s *Server) handleSetState(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		s.sendError(r, w, http.StatusBadRequest, ErrInvalidEngram, "missing engram id")
		return
	}
	var body SetStateRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		s.sendError(r, w, http.StatusBadRequest, ErrInvalidEngram, "invalid request body")
		return
	}
	validStates := map[string]bool{
		"planning": true, "active": true, "paused": true, "blocked": true,
		"completed": true, "cancelled": true, "archived": true,
	}
	if !validStates[body.State] {
		s.sendError(r, w, http.StatusBadRequest, ErrInvalidEngram,
			"'state' must be one of: planning, active, paused, blocked, completed, cancelled, archived")
		return
	}
	vault := body.Vault
	if vault == "" {
		vault = ctxVault(r)
	}
	if err := s.engine.UpdateState(r.Context(), vault, id, body.State, body.Reason); err != nil {
		s.sendError(r, w, http.StatusInternalServerError, ErrStorageError, err.Error())
		return
	}
	s.sendJSON(w, http.StatusOK, SetStateResponse{
		ID:      id,
		State:   body.State,
		Updated: true,
	})
}

func (s *Server) handleUpdateTags(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		s.sendError(r, w, http.StatusBadRequest, ErrInvalidEngram, "missing engram id")
		return
	}
	var body UpdateTagsRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		s.sendError(r, w, http.StatusBadRequest, ErrInvalidEngram, "invalid request body")
		return
	}
	vault := body.Vault
	if vault == "" {
		vault = ctxVault(r)
	}
	if body.Tags == nil {
		body.Tags = []string{}
	}
	if err := s.engine.UpdateTags(r.Context(), vault, id, body.Tags); err != nil {
		s.sendError(r, w, http.StatusInternalServerError, ErrStorageError, err.Error())
		return
	}
	s.sendJSON(w, http.StatusOK, UpdateTagsResponse{
		ID:   id,
		Tags: body.Tags,
	})
}

func (s *Server) handleListDeleted(w http.ResponseWriter, r *http.Request) {
	vault := ctxVault(r)
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	resp, err := s.engine.ListDeleted(r.Context(), vault, limit)
	if err != nil {
		s.sendError(r, w, http.StatusInternalServerError, ErrStorageError, err.Error())
		return
	}
	s.sendJSON(w, http.StatusOK, resp)
}

func (s *Server) handleRetryEnrich(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		s.sendError(r, w, http.StatusBadRequest, ErrInvalidEngram, "missing engram id")
		return
	}
	resp, err := s.engine.RetryEnrich(r.Context(), ctxVault(r), id)
	if err != nil {
		s.sendError(r, w, http.StatusInternalServerError, ErrEnrichmentError, err.Error())
		return
	}
	s.sendJSON(w, http.StatusOK, resp)
}

func (s *Server) handleContradictions(w http.ResponseWriter, r *http.Request) {
	resp, err := s.engine.GetContradictions(r.Context(), ctxVault(r))
	if err != nil {
		s.sendError(r, w, http.StatusInternalServerError, ErrStorageError, err.Error())
		return
	}
	s.sendJSON(w, http.StatusOK, resp)
}

func (s *Server) handleResolveContradiction(w http.ResponseWriter, r *http.Request) {
	var req ResolveContradictionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.sendError(r, w, http.StatusBadRequest, ErrInvalidEngram, err.Error())
		return
	}
	if req.IDA == "" || req.IDB == "" {
		s.sendError(r, w, http.StatusBadRequest, ErrInvalidEngram, "id_a and id_b are required")
		return
	}
	vault := req.Vault
	if vault == "" {
		vault = ctxVault(r)
	}
	if err := s.engine.ResolveContradiction(r.Context(), vault, req.IDA, req.IDB); err != nil {
		s.sendError(r, w, http.StatusInternalServerError, ErrStorageError, err.Error())
		return
	}
	s.sendJSON(w, http.StatusOK, ResolveContradictionResponse{Resolved: true})
}

func (s *Server) handleGuide(w http.ResponseWriter, r *http.Request) {
	guide, err := s.engine.GetGuide(r.Context(), ctxVault(r))
	if err != nil {
		s.sendError(r, w, http.StatusInternalServerError, ErrStorageError, err.Error())
		return
	}
	s.sendJSON(w, http.StatusOK, GuideResponse{Guide: guide})
}

// handleSubscribe opens a long-lived SSE connection. The client receives
// ActivationPush events as JSON-encoded server-sent events.
//
// Delivery is best-effort: if the client reads too slowly, pushes are dropped
// (the subscription stays alive). The connection sends a keepalive ping every
// 30 seconds so reverse proxies do not close idle streams.
//
// Query params:
//
//	vault     — vault name (default: "default")
//	context   — (repeatable) subscription context strings for semantic matching
//	threshold — float32 score threshold, default 0.5
//	on_write  — "true"|"1" to receive a push on every qualifying write
//	ttl       — subscription TTL in seconds, 0 = no expiry
//	rate      — max pushes/sec, default 10
func (s *Server) handleSubscribe(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	vault := q.Get("vault")
	if vault == "" {
		vault = "default"
	}
	contextStrs := q["context"]

	threshold := float32(0.5)
	if v := q.Get("threshold"); v != "" {
		if f, err := strconv.ParseFloat(v, 32); err == nil {
			threshold = float32(f)
		}
	}
	if threshold < 0 || threshold > 1 {
		threshold = 0.5
	}
	ttl := 0
	if v := q.Get("ttl"); v != "" {
		if i, err := strconv.Atoi(v); err == nil {
			ttl = i
		}
	}
	if ttl < 0 {
		ttl = 0
	}
	rateLimit := 10
	if v := q.Get("rate"); v != "" {
		if i, err := strconv.Atoi(v); err == nil {
			rateLimit = i
		}
	}
	if rateLimit < 1 {
		rateLimit = 1
	} else if rateLimit > 1000 {
		rateLimit = 1000
	}
	pushOnWrite := q.Get("on_write") == "true" || q.Get("on_write") == "1"

	// Set SSE headers before any write to the body.
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // disable nginx buffering

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	// Clear the server write deadline so long-lived SSE streams are not killed
	// by the REST server's 15-second WriteTimeout.
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Time{})

	// Buffered channel for pushes. The deliver func is non-blocking: it drops
	// pushes when the channel is full rather than blocking the trigger worker.
	pushCh := make(chan *trigger.ActivationPush, 32)

	// T6: local drop counter — tracks consecutive drops for this connection.
	// We use a plain int64 because deliver and the SSE loop are sequential
	// (deliver puts into pushCh; the SSE loop drains it), but deliver may be
	// called from any goroutine, so we use an atomic for safety.
	var consecutiveDrops int64

	deliver := func(ctx context.Context, push *trigger.ActivationPush) error {
		select {
		case pushCh <- push:
			// Successful delivery — reset consecutive drop counter.
			atomic.StoreInt64(&consecutiveDrops, 0)
		default:
			// Client too slow — drop this push, keep subscription alive.
			atomic.AddInt64(&consecutiveDrops, 1)
		}
		return nil
	}

	req := &mbp.SubscribeRequest{
		Context:     contextStrs,
		Threshold:   threshold,
		Vault:       vault,
		TTL:         ttl,
		RateLimit:   rateLimit,
		PushOnWrite: pushOnWrite,
	}

	subID, err := s.engine.SubscribeWithDeliver(r.Context(), req, deliver)
	if err != nil {
		// SSE headers have already been written (200 OK, text/event-stream), so
		// we cannot change the status code. Send an SSE error event instead.
		var errMsg string
		if errors.Is(err, trigger.ErrVaultSubscriptionLimitReached) || errors.Is(err, trigger.ErrGlobalSubscriptionLimitReached) {
			errMsg = "subscription limit reached"
		} else {
			errMsg = err.Error()
		}
		fmt.Fprintf(w, "event: error\ndata: {\"error\":%q}\n\n", errMsg)
		flusher.Flush()
		return
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		s.engine.Unsubscribe(ctx, subID)
	}()

	// Confirm subscription to client.
	fmt.Fprintf(w, "event: subscribed\ndata: {\"id\":%q}\n\n", subID)
	flusher.Flush()

	ping := time.NewTicker(30 * time.Second)
	defer ping.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-ping.C:
			fmt.Fprintf(w, "event: ping\ndata: {}\n\n")
			flusher.Flush()
		case push, ok := <-pushCh:
			if !ok {
				return
			}
			// T6: Check whether the deliver func has been dropping pushes for this
			// connection. If consecutiveDrops reached 50, the client is too slow and
			// we terminate the stream so the worker goroutine isn't blocked indefinitely.
			if atomic.LoadInt64(&consecutiveDrops) >= 50 {
				slog.Warn("SSE: slow subscriber disconnected", "sub_id", subID,
					"consecutive_drops", atomic.LoadInt64(&consecutiveDrops))
				return
			}
			data, err := json.Marshal(map[string]interface{}{
				"subscription_id": push.SubscriptionID,
				"trigger":         string(push.Trigger),
				"score":           push.Score,
				"push_number":     push.PushNumber,
				"at":              push.At.UnixNano(),
				"engram": func() interface{} {
					if push.Engram == nil {
						return nil
					}
					return map[string]interface{}{
						"id":      push.Engram.ID.String(),
						"concept": push.Engram.Concept,
						"content": push.Engram.Content,
					}
				}(),
				"why": push.Why,
			})
			if err != nil {
				continue
			}
			fmt.Fprintf(w, "event: push\ndata: %s\n\n", data)
			flusher.Flush()
		}
	}
}
