package mcp

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/scrypster/muninndb/internal/auth"
)

// MCPServer serves the MCP JSON-RPC 2.0 protocol on a single HTTP mux.
type MCPServer struct {
	engine           EngineInterface
	token            string              // required Bearer token (mdb_ static token); empty = no auth
	authKeys         apiKeyValidator     // optional: enables mk_ vault API key auth; nil = disabled
	capKeys          capabilityValidator // optional: enables cap_ capability auth (RFC #597); nil = disabled
	authStore        *auth.Store         // privileged: SetVaultConfig + GenerateCapability (create-workflow-vault); nil = tool disabled
	agentVaultCreate bool                // opt-in: MUNINN_AGENT_VAULT_CREATE (default off, secure-by-default)
	srv              *http.Server
	// writeGate is the cluster single-writer gate (#596); nil = standalone.
	writeGate func() error
	tlsConfig *tls.Config // nil = plain TCP

	sseSessionsMu sync.RWMutex
	sseSessions   map[string]*sseSession // sessionID → session

	// THE PUSH (prospective memory) — opt-in via MUNINN_PROSPECTIVE=1.
	// When false, the notices path on recall/remember is fully inert
	// (muninn_intend still arms durable intentions). noticeSeen tracks
	// per-session delivered-notice dedup keys (see prospective.go).
	prospective bool
	noticeMu    sync.Mutex
	noticeSeen  map[string]map[string]struct{} // sessionKey → delivered dedup keys
	// NOTE: idempotencyLocks grows by one entry per unique op_id seen during the
	// process lifetime. In practice op_id cardinality is low (client-generated,
	// not per-request UUIDs), so growth is bounded by usage patterns. The
	// canonical exactly-once guarantee lives in Pebble; the in-memory lock only
	// prevents the concurrent check→write TOCTOU race during the narrow window
	// before a receipt is written. Disk accumulation is addressed by
	// runIdempotencySweep (see engine.go).
	idempotencyLocks sync.Map
}

// getIdempotencyLock returns (or lazily creates) a per-op_id mutex. This is
// used by handleRemember to prevent TOCTOU races when two concurrent requests
// arrive with the same op_id: only one goroutine at a time can execute the
// check→write→store-receipt flow for a given op_id.
func (s *MCPServer) getIdempotencyLock(opID string) *sync.Mutex {
	v, _ := s.idempotencyLocks.LoadOrStore(opID, &sync.Mutex{})
	return v.(*sync.Mutex)
}

type sseSession struct {
	ch   chan []byte
	auth AuthContext // auth context established when the SSE stream was opened
}

// New creates an MCPServer. addr is the listen address (e.g., ":8750").
// token is the required static Bearer token (mdb_ style); pass "" to disable auth.
// keyAuth, if non-nil, enables mk_ vault API key authentication with automatic vault pinning.
// capAuth, if non-nil, enables cap_ capability token authentication (RFC #597).
// tlsConfig, if non-nil, enables TLS on the listener.
func New(addr string, eng EngineInterface, token string, keyAuth apiKeyValidator, capAuth capabilityValidator, tlsConfig *tls.Config) *MCPServer {
	// muninn_create_workflow_vault opt-in: default off (secure-by-default).
	agentVaultCreate := os.Getenv("MUNINN_AGENT_VAULT_CREATE") == "1"
	s := &MCPServer{
		engine:           eng,
		token:            token,
		authKeys:         keyAuth,
		capKeys:          capAuth,
		agentVaultCreate: agentVaultCreate,
		sseSessions:      make(map[string]*sseSession),
		prospective:      os.Getenv("MUNINN_PROSPECTIVE") == "1",
		noticeSeen:       make(map[string]map[string]struct{}),
		tlsConfig:        tlsConfig,
	}
	// The create-workflow-vault handler needs the concrete *auth.Store for
	// SetVaultConfig + GenerateCapability. capAuth is typed capabilityValidator
	// for testability (stub cap_ stores in tests), but in production it holds a
	// real *auth.Store. Derive authStore via type assertion so the constructor
	// signature stays unchanged; stub-based tests get authStore == nil (the
	// recursion guard still fires, the handler returns "not configured").
	if as, ok := capAuth.(*auth.Store); ok {
		s.authStore = as
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		slog.Info("mcp: request", "method", r.Method, "path", r.URL.String(), "auth", r.Header.Get("Authorization") != "")
		switch r.Method {
		case http.MethodPost:
			s.handleStreamablePost(w, r)
		case http.MethodGet:
			s.handleSSE(w, r)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})
	mux.HandleFunc("/mcp/message", func(w http.ResponseWriter, r *http.Request) {
		slog.Info("mcp: SSE message", "method", r.Method, "path", r.URL.String(), "auth", r.Header.Get("Authorization") != "")
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		s.handleSSEMessage(w, r)
	})
	mux.HandleFunc("/mcp/tools", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			s.withMiddleware(s.handleListTools)(w, r)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})
	mux.HandleFunc("/mcp/health", s.handleHealth)
	s.srv = &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	return s
}

// Serve starts listening. Blocks until the server is stopped.
func (s *MCPServer) Serve() error {
	ln, err := net.Listen("tcp", s.srv.Addr)
	if err != nil {
		return err
	}
	if s.tlsConfig != nil {
		ln = tls.NewListener(ln, s.tlsConfig)
		slog.Info("mcp: TLS enabled", "addr", ln.Addr().String())
	}
	return s.srv.Serve(ln)
}

// Shutdown gracefully stops the server.
func (s *MCPServer) Shutdown(ctx context.Context) error { return s.srv.Shutdown(ctx) }

// withMiddleware wraps a handler with: body size limit → auth check.
func (s *MCPServer) withMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Enforce 1MB body limit before any processing.
		if r.ContentLength > 1<<20 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			w.Write([]byte(`{"jsonrpc":"2.0","error":{"code":-32700,"message":"request body too large"}}`))
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)

		a := authFromRequest(r, s.token, s.authKeys, s.capKeys)
		if !a.Authorized {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"jsonrpc":"2.0","error":{"code":-32001,"message":"unauthorized"}}`))
			return
		}
		next(w, r.WithContext(contextWithAuth(r.Context(), a)))
	}
}

func (s *MCPServer) handleRPC(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	var req JSONRPCRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendError(w, nil, -32700, "parse error")
		return
	}
	if req.JSONRPC != "2.0" {
		sendError(w, req.ID, -32600, "invalid request: jsonrpc must be '2.0'")
		return
	}

	a := authFromContext(r.Context())
	switch {
	case req.Method == "initialize":
		s.handleInitialize(w, &req)
	case strings.HasPrefix(req.Method, "notifications/"):
		// MCP Streamable HTTP spec: notifications are fire-and-forget; respond
		// with 202 Accepted and no body.  200 OK breaks strict clients (e.g. Codex).
		w.WriteHeader(http.StatusAccepted)
	case req.Method == "ping":
		sendResult(w, req.ID, map[string]any{})
	case req.Method == "tools/list":
		sendResult(w, req.ID, map[string]any{"tools": exposedToolDefinitions(r)})
	case req.Method == "tools/call":
		s.dispatchToolCall(ctx, w, &req, a)
	case req.Method == "":
		sendError(w, req.ID, -32601, "method not found: method is required")
	default:
		sendError(w, req.ID, -32601, "method not found: "+req.Method)
	}
}

// SetWriteGate installs the cluster single-writer gate (#596). cmd/muninn wires
// it to the ClusterCoordinator when cluster mode is enabled.
func (s *MCPServer) SetWriteGate(g func() error) { s.writeGate = g }

func (s *MCPServer) dispatchToolCall(ctx context.Context, w http.ResponseWriter, req *JSONRPCRequest, a AuthContext) {
	if req.Params == nil {
		sendError(w, req.ID, -32602, "invalid params: params required")
		return
	}

	args := req.Params.Arguments
	if args == nil {
		args = make(map[string]any)
	}

	// Resolve vault — pinned to key's vault when authenticated via mk_ API key.
	vault, errMsg := resolveVault(a.Vault, args)
	if errMsg != "" {
		sendError(w, req.ID, -32602, "invalid params: "+errMsg)
		return
	}

	// Mode enforcement for mk_ vault keys and cap_ capability tokens (RFC #597).
	// Fail-closed: unknown tools (not in either classification list) are blocked
	// for both observe and write modes. Only full-mode credentials bypass this
	// check. The body switches on a.Mode, which is populated for both credential
	// types, so only the guard condition needed to include capabilities.
	if a.IsAPIKey || a.IsCapability {
		toolName := req.Params.Name
		switch a.Mode {
		case auth.ModeObserve:
			if !isReadOnlyTool(toolName) {
				sendError(w, req.ID, -32001, "forbidden: observe-mode key cannot call this tool")
				return
			}
		case auth.ModeWrite:
			if !isMutatingTool(toolName) {
				sendError(w, req.ID, -32001, "forbidden: write-mode key cannot call this tool")
				return
			}
		case auth.ModeAppend:
			// Append: read + create-new only. Destructive/modifying mutating
			// tools (forget/evolve/trust/merge/…) are refused. The engine also
			// refuses Evolve/Forget for append-mode as a transport-agnostic
			// backstop, so this is defense in depth, not the only gate.
			if !isReadOnlyTool(toolName) && !isAdditiveTool(toolName) {
				sendError(w, req.ID, -32001, "forbidden: append-mode key cannot call this tool (create-new and read only)")
				return
			}
		case auth.ModeFull:
			// full mode: no tool restriction within the pinned vault.
		default:
			// Unknown mode — fail-closed.
			sendError(w, req.ID, -32001, "forbidden: unrecognized key mode")
			return
		}
	}

	// Cluster single-writer gate (#596). Every mutating tool is refused on a
	// non-Cortex node with the leader named, rather than being accepted locally
	// and silently never replicated. isMutatingTool is the same classification
	// the write-mode credential check above uses, so a new mutating tool is
	// covered the moment it is classified — there is no second list.
	//
	// Read tools are untouched: a Lobe serving reads is the entire point of a
	// Lobe. This runs AFTER credential checks so an unauthenticated caller
	// cannot use it to learn cluster topology.
	if s.writeGate != nil && isMutatingTool(req.Params.Name) {
		if err := s.writeGate(); err != nil {
			sendError(w, req.ID, -32002, err.Error())
			return
		}
	}

	// muninn_create_workflow_vault is privileged: it requires an mk_ full-mode
	// key AND the opt-in flag. A cap_ bearer (IsCapability, not IsAPIKey) is
	// rejected here — this is the structural recursion guard. No capability can
	// mint further capabilities, because capabilities do not satisfy IsAPIKey.
	// This check runs BEFORE handler dispatch (handlers do not receive
	// AuthContext) and AFTER mode enforcement. Write-mode keys pass mode
	// enforcement (the tool is mutating) but fail here because Mode != ModeFull.
	// NOTE: the guard deliberately checks IsAPIKey, NOT IsCapability — this is
	// the security-critical invariant (see TestCreateWorkflowVault_RecursionGuard).
	if req.Params.Name == "muninn_create_workflow_vault" {
		if !s.agentVaultCreate {
			sendError(w, req.ID, -32001, "forbidden: agent vault creation disabled (set MUNINN_AGENT_VAULT_CREATE=1)")
			return
		}
		if !(a.IsAPIKey && a.Mode == auth.ModeFull) {
			sendError(w, req.ID, -32001, "forbidden: muninn_create_workflow_vault requires a full-mode mk_ key")
			return
		}
	}

	handler, found := s.toolHandlers()[req.Params.Name]
	if !found {
		sendError(w, req.ID, -32602, "unknown tool: "+req.Params.Name)
		return
	}

	// COG-11: inject the credential's mode into ctx so engine-layer code (e.g.
	// engine.go:2005's auth.ObserveFromContext(ctx)) can see it. gRPC
	// (internal/transport/grpc/server.go:172) and REST (internal/auth/middleware.go:49)
	// already do this; the MCP surface must match so observe-mode credentials get
	// ReadOnly=true and skip Hebbian/PAS/activation-log side effects.
	//
	// An authorized session with no explicit mode — the static mdb_ token and the
	// open-server (zero-config) deployment, which both have full tool access — is
	// mapped to ModeFull, matching REST's public path (internal/auth/middleware.go:74).
	// Without this, resolveTrust (SEC-14) would reject trust=verified on the default
	// deployment even though the caller has full access. mk_/cap_ sessions carry
	// their real key/cap Mode and are left untouched, so an observe key still cannot
	// stamp verified.
	mode := a.Mode
	if mode == "" && a.Authorized {
		mode = auth.ModeFull
	}
	ctx = context.WithValue(ctx, auth.ContextMode, mode)

	handler(ctx, w, req.ID, vault, args)
}

// toolHandlerFunc is the signature every MCP tool handler shares.
type toolHandlerFunc func(context.Context, http.ResponseWriter, json.RawMessage, string, map[string]any)

// toolHandlers is the tool dispatch table: the single authoritative list of
// which tool names this server serves. It was a map literal inside
// dispatchToolCall until #731, with registeredToolNames() a hand-maintained
// mirror of it kept in sync by a comment. A tool present here but missing from
// that mirror was invisible to every classification test, so the mirror is now
// derived from these keys instead (see registeredToolNames).
func (s *MCPServer) toolHandlers() map[string]toolHandlerFunc {
	return map[string]toolHandlerFunc{
		"muninn_remember":       s.handleRemember,
		"muninn_remember_batch": s.handleRememberBatch,
		"muninn_recall":         s.handleRecall,
		"muninn_read":           s.handleRead,
		"muninn_forget":         s.handleForget,
		"muninn_link":           s.handleLink,
		"muninn_contradictions": s.handleContradictions,
		"muninn_status":         s.handleStatus,
		"muninn_evolve":         s.handleEvolve,
		"muninn_consolidate":    s.handleConsolidate,
		"muninn_session":        s.handleSession,
		"muninn_decide":         s.handleDecide,
		// Epic 18: tools 12-17
		"muninn_restore":                   s.handleRestore,
		"muninn_traverse":                  s.handleTraverse,
		"muninn_explain":                   s.handleExplain,
		"muninn_state":                     s.handleState,
		"muninn_compare_and_set":           s.handleCompareAndSet,
		"muninn_claim":                     s.handleClaim,
		"muninn_release":                   s.handleRelease,
		"muninn_list_deleted":              s.handleListDeleted,
		"muninn_retry_enrich":              s.handleRetryEnrich,
		"muninn_get_enrichment_candidates": s.handleGetEnrichmentCandidates,
		"muninn_apply_enrichment":          s.handleApplyEnrichment,
		"muninn_guide":                     s.handleGuide,
		// Hierarchical memory tools
		"muninn_where_left_off": s.handleWhereLeftOff,

		"muninn_remember_tree": s.handleRememberTree,
		"muninn_recall_tree":   s.handleRecallTree,
		"muninn_add_child":     s.handleAddChild,

		// Entity reverse index
		"muninn_find_by_entity": s.handleFindByEntity,

		// Entity lifecycle state
		"muninn_entity_state":       s.handleEntityState,
		"muninn_entity_state_batch": s.handleEntityStateBatch,

		// Entity cluster detection
		"muninn_entity_clusters": s.handleEntityClusters,

		// Knowledge graph export
		"muninn_export_graph": s.handleExportGraph,

		// Entity similarity detection and merge
		"muninn_similar_entities": s.handleSimilarEntities,
		"muninn_merge_entity":     s.handleMergeEntity,

		// Entity timeline
		"muninn_entity_timeline": s.handleEntityTimeline,

		// Enrichment replay
		"muninn_replay_enrichment": s.handleReplayEnrichment,

		// Provenance audit trail
		"muninn_provenance": s.handleProvenance,

		// SGD learning loop feedback
		"muninn_feedback": s.handleFeedback,

		// Entity aggregate view
		"muninn_entity":   s.handleEntity,
		"muninn_entities": s.handleEntities,

		// Trust label
		"muninn_trust": s.handleSetTrust,

		// In-place retag (#720)
		"muninn_update_tags": s.handleUpdateTags,

		// RFC #597: privileged workflow-vault creation (recursion-guarded in
		// dispatchToolCall).
		"muninn_create_workflow_vault": s.handleCreateWorkflowVault,

		// THE PUSH: prospective memory (arm an intention on entity cues).
		"muninn_intend": s.handleIntend,
	}
}

// registeredToolNames returns, sorted, the tool names registered in the handler
// dispatch map. Tests use it to verify that the classification tables cover
// every registered tool. It is DERIVED from toolHandlers() rather than
// hand-maintained, so a tool cannot be registered without the classification
// tests seeing it (#731).
//
// The zero-value receiver is never invoked — only the method VALUES are built,
// to take their keys — so no handler runs and nothing dereferences it.
func registeredToolNames() []string {
	handlers := new(MCPServer).toolHandlers()
	names := make([]string, 0, len(handlers))
	for name := range handlers {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// handleSSE establishes an SSE stream per the MCP SSE transport spec.
// Sends an "endpoint" event with the POST URL, then streams responses.
func (s *MCPServer) handleSSE(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	// Auth check
	a := authFromRequest(r, s.token, s.authKeys, s.capKeys)
	if !a.Authorized {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	// Generate session
	idBytes := make([]byte, 16)
	rand.Read(idBytes)
	sessionID := hex.EncodeToString(idBytes)
	ch := make(chan []byte, 64)

	s.sseSessionsMu.Lock()
	s.sseSessions[sessionID] = &sseSession{ch: ch, auth: a}
	s.sseSessionsMu.Unlock()

	defer func() {
		s.sseSessionsMu.Lock()
		delete(s.sseSessions, sessionID)
		s.sseSessionsMu.Unlock()
	}()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	// Send the endpoint event — tells the client where to POST messages
	msgEndpoint := fmt.Sprintf("/mcp/message?sessionId=%s", sessionID)
	fmt.Fprintf(w, "event: endpoint\ndata: %s\n\n", msgEndpoint)
	flusher.Flush()

	slog.Info("mcp: SSE stream open", "session", sessionID[:8])
	ctx := r.Context()
	keepalive := time.NewTicker(30 * time.Second)
	defer keepalive.Stop()
	for {
		select {
		case <-ctx.Done():
			slog.Info("mcp: SSE stream closed", "session", sessionID[:8])
			return
		case <-keepalive.C:
			fmt.Fprintf(w, ": keepalive\n\n")
			flusher.Flush()
		case data, ok := <-ch:
			if !ok {
				slog.Info("mcp: SSE channel closed", "session", sessionID[:8])
				return
			}
			fmt.Fprintf(w, "event: message\ndata: %s\n\n", data)
			flusher.Flush()
			slog.Info("mcp: SSE flushed event", "session", sessionID[:8], "bytes", len(data))
		}
	}
}

// handleSSEMessage handles POST requests from SSE clients, processes the RPC,
// and pushes the response to the client's SSE stream.
func (s *MCPServer) handleSSEMessage(w http.ResponseWriter, r *http.Request) {
	sessionID := r.URL.Query().Get("sessionId")
	if sessionID == "" {
		http.Error(w, "missing sessionId", http.StatusBadRequest)
		return
	}

	// Re-validate auth on every POST — defense in depth against session ID leakage.
	// Run whenever any auth mechanism is active (static token, mk_ key store, or
	// cap_ capability store), not just when a static token is configured.
	if s.token != "" || s.authKeys != nil || s.capKeys != nil {
		a := authFromRequest(r, s.token, s.authKeys, s.capKeys)
		if !a.Authorized {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"jsonrpc":"2.0","error":{"code":-32001,"message":"unauthorized"}}`))
			return
		}
	}

	s.sseSessionsMu.RLock()
	sess, exists := s.sseSessions[sessionID]
	s.sseSessionsMu.RUnlock()
	if !exists {
		http.Error(w, "unknown session", http.StatusNotFound)
		return
	}

	// Re-validate the cached session credential before dispatch (RedTeam finding
	// CRITICAL #2). The POST's authFromRequest above only checks .Authorized; the
	// dispatch context below is set to sess.auth (cached at SSE-open). Without
	// this re-validation, a cap_ token's TTL expiry or revocation is NEVER
	// re-checked for an active SSE session — the TTL/revocation mitigation is
	// defeated on /mcp/message. We keep dispatching on sess.auth (the SSE session
	// model depends on it for vault pinning) — we just refuse to dispatch if the
	// cached credential is no longer valid.
	if sess.auth.IsCapability && s.capKeys != nil {
		if _, err := s.capKeys.ValidateCapability(sess.auth.Token); err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"jsonrpc":"2.0","error":{"code":-32001,"message":"session credential no longer valid (expired or revoked)"}}`))
			return
		}
	}

	// Re-validate mk_ API keys too (issue #615): same confused-deputy shape as
	// the cap_ branch above. A revoked mk_ key whose AuthContext was cached at
	// SSE-open would otherwise keep dispatching on sess.auth until the session
	// times out. auth.Store.ValidateAPIKey fails-closed on a revoked (deleted)
	// key, so this is the mk_ analogue of the cap_ re-validation added for #612.
	if sess.auth.IsAPIKey && s.authKeys != nil {
		if _, err := s.authKeys.ValidateAPIKey(sess.auth.Token); err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"jsonrpc":"2.0","error":{"code":-32001,"message":"session credential no longer valid (expired or revoked)"}}`))
			return
		}
	}

	// Thread the auth context established at SSE stream open time into the request.
	// The session auth is authoritative for vault pinning and mode enforcement;
	// the POST auth check above ensures the caller is still authenticated.
	r = r.WithContext(contextWithAuth(r.Context(), sess.auth))
	// SSE transport: the SSE session ID is the notice-session identity.
	r = r.WithContext(withNoticeSession(r.Context(), sessionID))
	s.processAndPushSSE(w, r, []chan []byte{sess.ch}, sessionID)
}

// handleStreamablePost handles POST /mcp requests. Supports both standalone
// JSON-RPC (response in POST body) and the Streamable HTTP pattern where the
// client also has an SSE connection open and expects responses on that stream.
func (s *MCPServer) handleStreamablePost(w http.ResponseWriter, r *http.Request) {
	if r.ContentLength > 1<<20 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		w.Write([]byte(`{"jsonrpc":"2.0","error":{"code":-32700,"message":"request body too large"}}`))
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)

	a := authFromRequest(r, s.token, s.authKeys, s.capKeys)
	if !a.Authorized {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"jsonrpc":"2.0","error":{"code":-32001,"message":"unauthorized"}}`))
		return
	}
	r = r.WithContext(contextWithAuth(r.Context(), a))
	// Notice-session identity for prospective-memory dedup: Streamable HTTP
	// clients echo the Mcp-Session-Id header minted at initialize.
	r = r.WithContext(withNoticeSession(r.Context(), r.Header.Get(mcpSessionHeader)))

	// If the client also has SSE streams open, route through the async
	// SSE handler so the response is pushed to ALL matching event streams
	// (some clients read from SSE even when they POST to the base URL).
	sseChannels := s.findSSEChannelsByToken(a.Token)
	if len(sseChannels) > 0 {
		s.processAndPushSSE(w, r, sseChannels, "streamable")
		return
	}

	// No SSE stream — pure POST, return response in body.
	s.handleRPC(w, r)
}

// findSSEChannelsByToken returns all SSE channels matching the given auth token.
// Returns nil for empty tokens to prevent cross-session contamination on open
// (no-auth) servers where every session has Token == "".
func (s *MCPServer) findSSEChannelsByToken(token string) []chan []byte {
	if token == "" {
		return nil
	}
	s.sseSessionsMu.RLock()
	defer s.sseSessionsMu.RUnlock()
	var channels []chan []byte
	for _, sess := range s.sseSessions {
		if sess.auth.Token == token {
			channels = append(channels, sess.ch)
		}
	}
	return channels
}

// processAndPushSSE processes a JSON-RPC request, writes the response to the
// POST body (primary delivery) AND broadcasts it to SSE channels (secondary).
// Uses a detached context for tool calls so POST connection close cannot cancel
// the operation.
func (s *MCPServer) processAndPushSSE(w http.ResponseWriter, r *http.Request, channels []chan []byte, label string) {
	var req JSONRPCRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		broadcastSSE(channels, nil, -32700, "parse error")
		w.WriteHeader(http.StatusAccepted)
		return
	}
	if req.JSONRPC != "2.0" {
		broadcastSSE(channels, req.ID, -32600, "invalid request: jsonrpc must be '2.0'")
		w.WriteHeader(http.StatusAccepted)
		return
	}

	slog.Info("mcp: dispatch", "via", label, "method", req.Method, "id", string(req.ID))

	if strings.HasPrefix(req.Method, "notifications/") {
		w.WriteHeader(http.StatusAccepted)
		return
	}

	// Use a detached context so the POST connection closing won't cancel
	// the tool call. This is critical — Claude Code may close the POST
	// before a slow tool call completes.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// The detached context loses request values — re-carry the notice-session
	// identity so prospective-memory dedup survives the detach.
	if key, ok := r.Context().Value(noticeSessionCtxKey{}).(string); ok {
		ctx = withNoticeSession(ctx, key)
	}

	var buf bytes.Buffer
	recorder := &responseCapture{header: http.Header{}, buf: &buf}

	switch {
	case req.Method == "initialize":
		s.handleInitialize(recorder, &req)
	case req.Method == "ping":
		sendResult(recorder, req.ID, map[string]any{})
	case req.Method == "tools/list":
		sendResult(recorder, req.ID, map[string]any{"tools": exposedToolDefinitions(r)})
	case req.Method == "tools/call":
		s.dispatchToolCall(ctx, recorder, &req, authFromContext(r.Context()))
	case req.Method == "":
		sendError(recorder, req.ID, -32601, "method not found: method is required")
	default:
		sendError(recorder, req.ID, -32601, "method not found: "+req.Method)
	}

	if buf.Len() > 0 {
		responseBytes := make([]byte, buf.Len())
		copy(responseBytes, buf.Bytes())

		slog.Info("mcp: response ready", "via", label, "method", req.Method, "bytes", len(responseBytes), "streams", len(channels))

		// Primary delivery: write response in POST body.
		copyResponseHeaders(w.Header(), recorder.header)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if _, err := w.Write(responseBytes); err != nil {
			slog.Warn("mcp: POST body write failed", "via", label, "err", err)
		}

		// Secondary delivery: push to all SSE streams.
		pushToAll(channels, responseBytes, label)
	} else {
		w.WriteHeader(http.StatusAccepted)
	}
}

// pushToAll sends data to all SSE channels without blocking.
func pushToAll(channels []chan []byte, data []byte, label string) {
	for _, ch := range channels {
		select {
		case ch <- data:
		default:
			slog.Warn("mcp: SSE channel full, dropping", "via", label)
		}
	}
}

// broadcastSSE sends an error response to all SSE channels.
func broadcastSSE(channels []chan []byte, id json.RawMessage, code int, message string) {
	resp := JSONRPCResponse{
		JSONRPC: "2.0",
		ID:      id,
		Error:   &JSONRPCError{Code: code, Message: message},
	}
	b, _ := json.Marshal(resp)
	for _, ch := range channels {
		select {
		case ch <- b:
		default:
		}
	}
}

// responseCapture captures HTTP response body without writing to a real connection.
type responseCapture struct {
	header http.Header
	buf    *bytes.Buffer
	code   int
}

func (r *responseCapture) Header() http.Header         { return r.header }
func (r *responseCapture) WriteHeader(code int)        { r.code = code }
func (r *responseCapture) Write(b []byte) (int, error) { return r.buf.Write(b) }

func (s *MCPServer) handleInitialize(w http.ResponseWriter, req *JSONRPCRequest) {
	// Streamable HTTP clients may require a session ID from initialize even if the
	// server does not enforce session state on subsequent unary POST requests.
	idBytes := make([]byte, 16)
	if _, err := rand.Read(idBytes); err == nil {
		w.Header().Set(mcpSessionHeader, hex.EncodeToString(idBytes))
	}
	sendResult(w, req.ID, map[string]any{
		"protocolVersion": "2024-11-05",
		"capabilities": map[string]any{
			"tools": map[string]any{},
		},
		"serverInfo": map[string]any{
			"name":    "muninn",
			"version": "1.0.0",
		},
		"instructions": mcpInstructions,
	})
}

func (s *MCPServer) handleListTools(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"tools": exposedToolDefinitions(r)})
}

func (s *MCPServer) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{"status":"ok"}`))
}

// sendResult writes a successful JSON-RPC response.
func sendResult(w http.ResponseWriter, id json.RawMessage, result any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(JSONRPCResponse{
		JSONRPC: "2.0",
		ID:      id,
		Result:  result,
	})
}

// sendError writes a JSON-RPC error response.
func sendError(w http.ResponseWriter, id json.RawMessage, code int, message string) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(JSONRPCResponse{
		JSONRPC: "2.0",
		ID:      id,
		Error:   &JSONRPCError{Code: code, Message: message},
	})
}

func copyResponseHeaders(dst, src http.Header) {
	for k, values := range src {
		for _, v := range values {
			dst.Add(k, v)
		}
	}
}

// mustJSON marshals v to JSON.
// On marshal failure it logs the error and returns an empty JSON object
// rather than panicking — marshal errors are caused by non-serialisable types
// in dynamic handler data, not programmer bugs in static schema definitions.
func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		slog.Error("mcp: mustJSON marshal failed", "error", err)
		return "{}"
	}
	return string(b)
}
