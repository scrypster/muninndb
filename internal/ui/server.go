package ui

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/scrypster/muninndb/internal/auth"
	"github.com/scrypster/muninndb/internal/logging"
	"github.com/scrypster/muninndb/internal/transport/rest"
)

// Server is the web UI HTTP server.
type Server struct {
	engine        rest.EngineAPI
	webFS         fs.FS
	tmplFS        fs.FS
	staticFS      fs.FS
	hub           *sseHub
	mux           *http.ServeMux
	server        *http.Server
	authStore     *auth.Store
	sessionSecret []byte
	ring          *logging.RingBuffer
	tlsConfig     *tls.Config // nil = plain TCP
	corsOrigins   []string
	ln            net.Listener
}

// sseHub manages connected SSE clients.
type sseHub struct {
	mu      sync.Mutex
	clients map[chan []byte]struct{}
}

func newHub() *sseHub {
	return &sseHub{clients: make(map[chan []byte]struct{})}
}

func (h *sseHub) subscribe() chan []byte {
	ch := make(chan []byte, 16)
	h.mu.Lock()
	h.clients[ch] = struct{}{}
	h.mu.Unlock()
	return ch
}

func (h *sseHub) unsubscribe(ch chan []byte) {
	h.mu.Lock()
	delete(h.clients, ch)
	h.mu.Unlock()
	close(ch)
}

func (h *sseHub) broadcast(data []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.clients {
		select {
		case ch <- data:
		default:
			// drop if client buffer full
		}
	}
}

// NewServer creates a new UI server using the provided embedded FS, engine, and API handler.
// apiHandler is mounted at /api/ so the SPA can make same-origin API calls.
// authStore and sessionSecret are used to handle admin login/logout via cookie sessions.
// tlsConfig, if non-nil, enables TLS on the listener.
// corsOrigins is the list of allowed CORS origins for the SSE endpoint.
func NewServer(webFS fs.FS, engine rest.EngineAPI, apiHandler http.Handler, authStore *auth.Store, sessionSecret []byte, ring *logging.RingBuffer, tlsConfig *tls.Config, corsOrigins []string) (*Server, error) {
	staticFS, err := fs.Sub(webFS, "static")
	if err != nil {
		return nil, err
	}
	tmplFS, err := fs.Sub(webFS, "templates")
	if err != nil {
		return nil, err
	}

	s := &Server{
		engine:        engine,
		webFS:         webFS,
		tmplFS:        tmplFS,
		staticFS:      staticFS,
		hub:           newHub(),
		authStore:     authStore,
		sessionSecret: sessionSecret,
		ring:          ring,
		tlsConfig:     tlsConfig,
		corsOrigins:   corsOrigins,
	}

	mux := http.NewServeMux()
	mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.FS(staticFS))))
	// Login/logout are handled by the UI server itself (cookie sessions).
	// These must be registered before the /api/ catch-all so they take precedence.
	mux.HandleFunc("POST /api/auth/login", s.handleAdminLogin)
	mux.HandleFunc("POST /api/auth/logout", s.handleAdminLogout)
	authCheckHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	})
	if s.authStore != nil && len(s.sessionSecret) > 0 {
		mux.HandleFunc("GET /api/auth/check", s.authStore.AdminAPIMiddleware(s.sessionSecret, authCheckHandler))
		mux.HandleFunc("GET /logs", s.authStore.AdminAPIMiddleware(s.sessionSecret, s.handleLogs))
		mux.HandleFunc("/events", s.authStore.AdminAPIMiddleware(s.sessionSecret, s.handleSSE))
	} else {
		mux.HandleFunc("GET /api/auth/check", authCheckHandler)
		mux.HandleFunc("GET /logs", s.handleLogs)
		mux.HandleFunc("/events", s.handleSSE)
	}
	mux.Handle("/api/", apiHandler)
	mux.HandleFunc("/", s.handleSPA)

	s.mux = mux
	s.server = &http.Server{
		Handler:      mux,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 0, // SSE needs long-lived connections
		IdleTimeout:  60 * time.Second,
	}

	return s, nil
}

// ServeHTTP implements http.Handler, exposing the mux for testing.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

// Start begins listening on addr and starts the broadcaster goroutine. Non-blocking.
func (s *Server) Start(ctx context.Context, addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	s.ln = ln // store raw listener for Addr(); TLS wrapper (if any) shares the same addr
	s.server.Addr = ln.Addr().String()
	if s.tlsConfig != nil {
		ln = tls.NewListener(ln, s.tlsConfig)
		slog.Info("ui: TLS enabled", "addr", ln.Addr().String())
	}

	go func() {
		if err := s.server.Serve(ln); err != nil && err != http.ErrServerClosed {
			slog.Error("ui server error", "err", err)
		}
	}()

	go s.broadcaster(ctx)

	return nil
}

// Addr returns the server's resolved listening address after Start has been called.
// Safe to call from the same goroutine as Start; no concurrent access is expected.
func (s *Server) Addr() string {
	if s.ln != nil {
		return s.ln.Addr().String()
	}
	return s.server.Addr
}

// Stop gracefully shuts down the UI server.
func (s *Server) Stop(ctx context.Context) error {
	return s.server.Shutdown(ctx)
}

// Broadcast sends data to all connected SSE clients. Used for log tail wiring.
func (s *Server) Broadcast(data []byte) {
	s.hub.broadcast(data)
}

func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if s.ring == nil {
		json.NewEncoder(w).Encode([]struct{}{})
		return
	}
	entries := s.ring.Snapshot()
	type logResponse struct {
		Level string            `json:"level"`
		Time  string            `json:"time"`
		Msg   string            `json:"msg"`
		Attrs map[string]string `json:"attrs"`
	}
	out := make([]logResponse, len(entries))
	for i, e := range entries {
		out[i] = logResponse{
			Level: e.Level,
			Time:  e.Time.Format(time.RFC3339Nano),
			Msg:   e.Msg,
			Attrs: e.Attrs,
		}
	}
	json.NewEncoder(w).Encode(out)
}

// setCORSIfAllowed sets Access-Control-Allow-Origin + Vary: Origin if the
// request origin is in s.corsOrigins. Also handles OPTIONS preflight with 204.
// Returns true if the request was a preflight (caller should return early).
func (s *Server) setCORSIfAllowed(w http.ResponseWriter, r *http.Request) bool {
	origin := r.Header.Get("Origin")
	matched := false
	if origin != "" && len(s.corsOrigins) > 0 {
		for _, allowed := range s.corsOrigins {
			if origin == allowed {
				w.Header().Set("Access-Control-Allow-Origin", origin)
				w.Header().Set("Access-Control-Allow-Credentials", "true")
				matched = true
				break
			}
		}
		// Vary must be set unconditionally when this endpoint is CORS-aware
		// so caches don't serve one origin's response to another origin.
		w.Header().Set("Vary", "Origin")
	}
	if r.Method == http.MethodOptions {
		if matched {
			w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
		}
		w.WriteHeader(http.StatusNoContent)
		return true
	}
	return false
}

func (s *Server) handleSSE(w http.ResponseWriter, r *http.Request) {
	if s.setCORSIfAllowed(w, r) {
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // disable nginx buffering
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ch := s.hub.subscribe()
	defer s.hub.unsubscribe(ch)

	// Keep connection alive with 20s heartbeat comment.
	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case data, open := <-ch:
			if !open {
				return
			}
			_, _ = fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
		case <-ticker.C:
			_, _ = fmt.Fprintf(w, ": heartbeat\n\n")
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

func (s *Server) handleAdminLogin(w http.ResponseWriter, r *http.Request) {
	if s.authStore == nil {
		http.Error(w, "authentication not configured", http.StatusServiceUnavailable)
		return
	}
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	if err := s.authStore.ValidateAdmin(req.Username, req.Password); err != nil {
		http.Error(w, "invalid credentials", http.StatusUnauthorized)
		return
	}
	token, err := auth.NewSessionToken(req.Username, s.sessionSecret)
	if err != nil {
		http.Error(w, "session error", http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     "muninn_session",
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   s.tlsConfig != nil,
		MaxAge:   86400,
	})
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func (s *Server) handleAdminLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:   "muninn_session",
		Value:  "",
		Path:   "/",
		MaxAge: -1,
	})
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func (s *Server) handleSPA(w http.ResponseWriter, r *http.Request) {
	f, err := s.tmplFS.Open("index.html")
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = io.Copy(w, f)
}

type statsMsg struct {
	Type string      `json:"type"`
	Data interface{} `json:"data"`
}

// broadcaster polls engine.Stat every 5s and pushes stats_update to all SSE clients.
// It also does count-diff to detect new engrams and push memory_added.
func (s *Server) broadcaster(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	var prevCount int64

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.broadcastStats(&prevCount)
		}
	}
}

func (s *Server) broadcastStats(prevCount *int64) {
	resp, err := s.engine.Stat(context.Background(), &rest.StatRequest{})
	if err != nil {
		return
	}

	msg := statsMsg{
		Type: "stats_update",
		Data: map[string]interface{}{
			"engramCount":  resp.EngramCount,
			"vaultCount":   resp.VaultCount,
			"indexSize":    resp.IndexSize,
			"storageBytes": resp.StorageBytes,
		},
	}
	data, err := json.Marshal(msg)
	if err != nil {
		return
	}
	s.hub.broadcast(data)

	// Count-diff: push memory_added if new engrams appeared.
	if *prevCount > 0 && resp.EngramCount > *prevCount {
		s.broadcastNewestEngram()
	}
	*prevCount = resp.EngramCount
}

func (s *Server) broadcastNewestEngram() {
	resp, err := s.engine.ListEngrams(context.Background(), &rest.ListEngramsRequest{
		Vault:  "default",
		Limit:  1,
		Offset: 0,
	})
	if err != nil || len(resp.Engrams) == 0 {
		return
	}
	e := resp.Engrams[0]
	msg := statsMsg{
		Type: "memory_added",
		Data: map[string]interface{}{
			"id":        e.ID,
			"concept":   e.Concept,
			"vault":     e.Vault,
			"createdAt": e.CreatedAt,
		},
	}
	data, err := json.Marshal(msg)
	if err != nil {
		return
	}
	s.hub.broadcast(data)
}
