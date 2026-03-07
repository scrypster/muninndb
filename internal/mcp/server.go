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
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// MCPServer serves the MCP JSON-RPC 2.0 protocol on a single HTTP mux.
type MCPServer struct {
	engine    EngineInterface
	token     string // required Bearer token; empty = no auth
	limiter   *rate.Limiter
	srv       *http.Server
	tlsConfig *tls.Config // nil = plain TCP

	sseSessionsMu sync.Mutex
	sseSessions   map[string]*sseSession // sessionID → session
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
	ch        chan []byte
	authToken string // bearer token used to establish the session
}

// New creates an MCPServer. addr is the listen address (e.g., ":8750").
// token is the required Bearer token; pass "" to disable auth.
// tlsConfig, if non-nil, enables TLS on the listener.
func New(addr string, eng EngineInterface, token string, tlsConfig *tls.Config) *MCPServer {
	s := &MCPServer{
		engine:      eng,
		token:       token,
		limiter:     rate.NewLimiter(rate.Limit(100), 200),
		sseSessions: make(map[string]*sseSession),
		tlsConfig:   tlsConfig,
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

// withMiddleware wraps a handler with: body size limit → rate limiter → auth check.
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

		if !s.limiter.Allow() {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`{"jsonrpc":"2.0","error":{"code":-32000,"message":"rate limited"}}`))
			return
		}
		auth := authFromRequest(r, s.token)
		if !auth.Authorized {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"jsonrpc":"2.0","error":{"code":-32001,"message":"unauthorized"}}`))
			return
		}
		next(w, r)
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
		sendResult(w, req.ID, map[string]any{"tools": allToolDefinitions()})
	case req.Method == "tools/call":
		s.dispatchToolCall(ctx, w, &req)
	case req.Method == "":
		sendError(w, req.ID, -32601, "method not found: method is required")
	default:
		sendError(w, req.ID, -32601, "method not found: "+req.Method)
	}
}

func (s *MCPServer) dispatchToolCall(ctx context.Context, w http.ResponseWriter, req *JSONRPCRequest) {
	if req.Params == nil {
		sendError(w, req.ID, -32602, "invalid params: params required")
		return
	}

	args := req.Params.Arguments
	if args == nil {
		args = make(map[string]any)
	}

	vault, errMsg := resolveVault(nil, args)
	if errMsg != "" {
		sendError(w, req.ID, -32602, "invalid params: "+errMsg)
		return
	}

	handlers := map[string]func(context.Context, http.ResponseWriter, json.RawMessage, string, map[string]any){
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
		"muninn_restore":      s.handleRestore,
		"muninn_traverse":     s.handleTraverse,
		"muninn_explain":      s.handleExplain,
		"muninn_state":        s.handleState,
		"muninn_list_deleted": s.handleListDeleted,
		"muninn_retry_enrich": s.handleRetryEnrich,
		"muninn_guide":        s.handleGuide,
		// Hierarchical memory tools
		"muninn_where_left_off": s.handleWhereLeftOff,

		"muninn_remember_tree": s.handleRememberTree,
		"muninn_recall_tree":   s.handleRecallTree,
		"muninn_add_child":     s.handleAddChild,

		// Entity reverse index
		"muninn_find_by_entity": s.handleFindByEntity,

		// Entity lifecycle state
		"muninn_entity_state": s.handleEntityState,

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
	}

	handler, found := handlers[req.Params.Name]
	if !found {
		sendError(w, req.ID, -32602, "unknown tool: "+req.Params.Name)
		return
	}
	handler(ctx, w, req.ID, vault, args)
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
	auth := authFromRequest(r, s.token)
	if !auth.Authorized {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	// Generate session
	idBytes := make([]byte, 16)
	rand.Read(idBytes)
	sessionID := hex.EncodeToString(idBytes)
	ch := make(chan []byte, 64)

	s.sseSessionsMu.Lock()
	s.sseSessions[sessionID] = &sseSession{ch: ch, authToken: auth.Token}
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
	for {
		select {
		case <-ctx.Done():
			slog.Info("mcp: SSE stream closed", "session", sessionID[:8])
			return
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

	s.sseSessionsMu.Lock()
	sess, exists := s.sseSessions[sessionID]
	s.sseSessionsMu.Unlock()
	if !exists {
		http.Error(w, "unknown session", http.StatusNotFound)
		return
	}

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

	if !s.limiter.Allow() {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"jsonrpc":"2.0","error":{"code":-32000,"message":"rate limited"}}`))
		return
	}

	auth := authFromRequest(r, s.token)
	if !auth.Authorized {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"jsonrpc":"2.0","error":{"code":-32001,"message":"unauthorized"}}`))
		return
	}

	// If the client also has SSE streams open, route through the async
	// SSE handler so the response is pushed to ALL matching event streams
	// (some clients read from SSE even when they POST to the base URL).
	sseChannels := s.findSSEChannelsByToken(auth.Token)
	if len(sseChannels) > 0 {
		s.processAndPushSSE(w, r, sseChannels, "streamable")
		return
	}

	// No SSE stream — pure POST, return response in body.
	s.handleRPC(w, r)
}

// findSSEChannelsByToken returns all SSE channels matching the given auth token.
func (s *MCPServer) findSSEChannelsByToken(token string) []chan []byte {
	s.sseSessionsMu.Lock()
	defer s.sseSessionsMu.Unlock()
	var channels []chan []byte
	for _, sess := range s.sseSessions {
		if sess.authToken == token {
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

	var buf bytes.Buffer
	recorder := &responseCapture{header: http.Header{}, buf: &buf}

	switch {
	case req.Method == "initialize":
		s.handleInitialize(recorder, &req)
	case req.Method == "ping":
		sendResult(recorder, req.ID, map[string]any{})
	case req.Method == "tools/list":
		sendResult(recorder, req.ID, map[string]any{"tools": allToolDefinitions()})
	case req.Method == "tools/call":
		s.dispatchToolCall(ctx, recorder, &req)
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
	})
}

func (s *MCPServer) handleListTools(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"tools": allToolDefinitions()})
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
